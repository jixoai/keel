package mediawiring

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/layerfs"
	"github.com/jixoai/keel/internal/mediaadapter"
	"github.com/jixoai/keel/internal/modelcfg"
)

// mediaServer is the recording provider fixture. It answers the two generation
// paths and the asset download path, recording every request so a test can pin
// the wire shape and the credential boundary.
type mediaServer struct {
	mu       sync.Mutex
	requests []recordedRequest
	server   *httptest.Server
}

type recordedRequest struct {
	Method string
	Path   string
	Auth   string
	Body   string
}

func newMediaServer(t *testing.T) *mediaServer {
	t.Helper()
	m := &mediaServer{}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.requests = append(m.requests, recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Auth:   r.Header.Get("Authorization"),
			Body:   string(body),
		})
		m.mu.Unlock()
		switch r.URL.Path {
		case "/images/generations":
			png := base64.StdEncoding.EncodeToString([]byte("png-bytes"))
			_, _ = io.WriteString(w, `{"model":"img-1","data":[{"b64_json":"`+png+`","mime_type":"image/png","width":8,"height":6}],`+
				`"usage":{"input_tokens":3,"output_tokens":4,"cost":9.99}}`)
		case "/videos/generations":
			_, _ = io.WriteString(w, `{"model":"vid-1","data":[{"url":"`+m.server.URL+`/asset.mp4","mime_type":"video/mp4","duration_ms":1000}],`+
				`"usage":{"input_tokens":5}}`)
		case "/asset.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("mp4-bytes"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mediaServer) snapshot() []recordedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]recordedRequest(nil), m.requests...)
}

func (m *mediaServer) count(path string) int {
	count := 0
	for _, request := range m.snapshot() {
		if request.Path == path {
			count++
		}
	}
	return count
}

func openArtifactStore(t *testing.T) *layerfs.Store {
	t.Helper()
	store, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func credentialLookup(values map[string]string) func(modelcfg.Access) (string, string) {
	return func(access modelcfg.Access) (string, string) {
		if access.CredentialFree() {
			return "", ""
		}
		name := strings.TrimSpace(access.Env)
		if name == "" {
			return "", "provider declares no access source"
		}
		value, ok := values[name]
		if !ok || value == "" {
			return "", "access.env (" + name + ") is not set in this process"
		}
		return value, ""
	}
}

const mediaDocumentTemplate = `{
  "providers": [
    {"id": "img", "type": "image",
     "api": {"type": "openai-chat-completions", "baseUrl": "SERVER"},
     "access": {"type": "api-key", "env": "IMG_KEY"},
     "modelIds": ["img-1"]},
    {"id": "vid", "type": "video",
     "api": {"type": "openai-chat-completions", "baseUrl": "SERVER"},
     "access": {"type": "api-key", "env": "VID_KEY"},
     "modelIds": ["vid-1"]}
  ],
  "selection": {"providerId": "img", "modelId": "img-1"}
}`

func mediaDocument(serverURL string) json.RawMessage {
	return json.RawMessage(strings.ReplaceAll(mediaDocumentTemplate, "SERVER", serverURL))
}

func buildForTest(t *testing.T, serverURL string, values map[string]string) (Result, *layerfs.Store) {
	t.Helper()
	store := openArtifactStore(t)
	return Build(Options{
		Models:     mediaDocument(serverURL),
		Credential: credentialLookup(values),
		Artifacts:  store,
		// The fixture server is on loopback; the production fetcher refuses it.
		Fetcher: mediaadapter.NewHTTPFetcherWithOptions(mediaadapter.HTTPFetcherOptions{AllowPrivateHosts: true}),
	}), store
}

func contentText(t *testing.T, result agentcore.AgentToolResult) string {
	t.Helper()
	if len(result.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(result.Content))
	}
	text, ok := result.Content[0].(agentcore.TextContent)
	if !ok {
		t.Fatalf("content = %T, want TextContent", result.Content[0])
	}
	return text.Text
}

func runTool(t *testing.T, tools []agentcore.AgentTool, name, args, session string) agentcore.AgentToolCallOutcome {
	t.Helper()
	ctx := agentcore.WithToolRunScope(context.Background(), agentcore.ToolRunScope{
		TenantID: "tenant-1", SessionID: session,
	})
	return agenttool.RunToolCall(ctx, agentcore.AgentToolCall{
		ID: "call-" + name, Name: name, Arguments: json.RawMessage(args),
	}, agenttool.RunToolCallOptions{Tools: tools})
}

func readArtifact(t *testing.T, store *layerfs.Store, blobID string) []byte {
	t.Helper()
	hash, err := layerfs.ParseHash(blobID)
	if err != nil {
		t.Fatalf("artifact blobId %q: %v", blobID, err)
	}
	object, err := store.Open(hash)
	if err != nil {
		t.Fatalf("open artifact %q: %v", blobID, err)
	}
	defer object.Close()
	data, err := io.ReadAll(object)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestBuildWiresImageAndVideoToolsIntoTheHostSurface(t *testing.T) {
	server := newMediaServer(t)
	result, artifacts := buildForTest(t, server.server.URL, map[string]string{
		"IMG_KEY": "img-key", "VID_KEY": "vid-key",
	})
	if !result.Wired(FamilyImage) || !result.Wired(FamilyVideo) {
		t.Fatalf("families = %+v, issues = %+v", result.Families, result.Issues)
	}
	if len(result.Tools) != 2 {
		t.Fatalf("tools = %d, want image and video", len(result.Tools))
	}
	names := []string{result.Tools[0].Name(), result.Tools[1].Name()}
	if names[0] != "image_generate" || names[1] != "video_generate" {
		t.Fatalf("tool names = %v", names)
	}

	imageOutcome := runTool(t, result.Tools, "image_generate", `{"prompt":"a lighthouse","n":1}`, "session-image")
	if imageOutcome.IsError {
		t.Fatalf("image tool failed: %+v", imageOutcome.Result)
	}
	var imageOutput mediaadapter.ImageToolOutput
	if err := json.Unmarshal([]byte(contentText(t, imageOutcome.Result)), &imageOutput); err != nil {
		t.Fatal(err)
	}
	if !imageOutput.Success || len(imageOutput.Images) != 1 || imageOutput.Images[0].BlobID == "" {
		t.Fatalf("image output = %+v", imageOutput)
	}
	if imageOutput.Usage.Images != 1 || imageOutput.Usage.Bytes != int64(len("png-bytes")) {
		t.Fatalf("image usage = %+v", imageOutput.Usage)
	}
	if got := string(readArtifact(t, artifacts, imageOutput.Images[0].BlobID)); got != "png-bytes" {
		t.Fatalf("image artifact bytes = %q", got)
	}
	imageRequest := findRequest(t, server.snapshot(), "/images/generations")
	if imageRequest.Method != http.MethodPost || imageRequest.Auth != "Bearer img-key" {
		t.Fatalf("image wire = %+v", imageRequest)
	}
	var imageWire map[string]any
	if err := json.Unmarshal([]byte(imageRequest.Body), &imageWire); err != nil {
		t.Fatal(err)
	}
	if imageWire["model"] != "img-1" || imageWire["prompt"] != "a lighthouse" {
		t.Fatalf("image request body = %s", imageRequest.Body)
	}
	if strings.Contains(imageRequest.Body, "price") || strings.Contains(imageRequest.Body, "cost") {
		t.Fatalf("image request carried a price field: %s", imageRequest.Body)
	}

	videoOutcome := runTool(t, result.Tools, "video_generate", `{"prompt":"a paper boat","duration":4}`, "session-video")
	if videoOutcome.IsError {
		t.Fatalf("video tool failed: %+v", videoOutcome.Result)
	}
	var videoOutput mediaadapter.VideoToolOutput
	if err := json.Unmarshal([]byte(contentText(t, videoOutcome.Result)), &videoOutput); err != nil {
		t.Fatal(err)
	}
	if !videoOutput.Success || len(videoOutput.Videos) != 1 || videoOutput.Videos[0].BlobID == "" {
		t.Fatalf("video output = %+v", videoOutput)
	}
	if videoOutput.Usage.Videos != 1 || videoOutput.Usage.Bytes != int64(len("mp4-bytes")) {
		t.Fatalf("video usage = %+v", videoOutput.Usage)
	}
	if got := string(readArtifact(t, artifacts, videoOutput.Videos[0].BlobID)); got != "mp4-bytes" {
		t.Fatalf("video artifact bytes = %q", got)
	}
	videoRequest := findRequest(t, server.snapshot(), "/videos/generations")
	if videoRequest.Auth != "Bearer vid-key" {
		t.Fatalf("video wire = %+v", videoRequest)
	}
	assetRequest := findRequest(t, server.snapshot(), "/asset.mp4")
	if assetRequest.Auth != "" {
		t.Fatalf("asset fetch forwarded the provider credential: %+v", assetRequest)
	}
}

func findRequest(t *testing.T, requests []recordedRequest, path string) recordedRequest {
	t.Helper()
	for _, request := range requests {
		if request.Path == path {
			return request
		}
	}
	t.Fatalf("no request for %s in %+v", path, requests)
	return recordedRequest{}
}

func TestBuildKeepsMissingCredentialFailClosed(t *testing.T) {
	server := newMediaServer(t)
	result := Build(Options{
		Models:     mediaDocument(server.server.URL),
		Credential: credentialLookup(map[string]string{"VID_KEY": "vid-key"}),
		Artifacts:  openArtifactStore(t),
		Fetcher:    mediaadapter.NewHTTPFetcherWithOptions(mediaadapter.HTTPFetcherOptions{AllowPrivateHosts: true}),
	})
	if !result.Wired(FamilyImage) {
		t.Fatalf("a configured image provider must keep its tool, state = %+v", result.Families[FamilyImage])
	}
	outcome := runTool(t, result.Tools, "image_generate", `{"prompt":"a lighthouse"}`, "session-image")
	if !outcome.IsError {
		t.Fatal("missing credential must fail closed")
	}
	text := contentText(t, outcome.Result)
	var failure mediaadapter.ToolFailure
	if err := json.Unmarshal([]byte(text), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.ErrorType != string(mediaadapter.KindMissingCredentials) {
		t.Fatalf("error type = %q, want missing_credentials (body %s)", failure.ErrorType, text)
	}
	if server.count("/images/generations") != 0 {
		t.Fatal("a provider with no credential was called")
	}
	// The input echo keeps the call retryable without carrying a credential.
	if echo, ok := failure.Request.(map[string]any); !ok || echo["prompt"] != "a lighthouse" {
		t.Fatalf("request echo = %#v", failure.Request)
	}
}

func TestBuildOmitsAbsentFamilies(t *testing.T) {
	result := Build(Options{})
	if len(result.Tools) != 0 {
		t.Fatalf("empty document produced tools: %v", result.Tools)
	}
	for _, family := range []string{FamilyImage, FamilyVideo, FamilyTTS, FamilySTT, FamilyDecision} {
		state := result.Families[family]
		if state.Configured || state.Wired {
			t.Fatalf("%s state = %+v, want absent", family, state)
		}
	}
}

func TestSpeechFamiliesAreReportedAsLibraryOnly(t *testing.T) {
	document := json.RawMessage(`{
	  "providers": [
	    {"id": "speech", "type": "tts",
	     "api": {"type": "openai-chat-completions", "baseUrl": "https://speech.example/v1"},
	     "access": {"type": "api-key", "env": "SPEECH_KEY"},
	     "modelIds": ["tts-1"]},
	    {"id": "ears", "type": "stt",
	     "api": {"type": "openai-chat-completions", "baseUrl": "https://speech.example/v1"},
	     "access": {"type": "api-key", "env": "SPEECH_KEY"},
	     "modelIds": ["whisper-1"]}
	  ]
	}`)
	result := Build(Options{Models: document, Credential: credentialLookup(map[string]string{"SPEECH_KEY": "k"})})
	for _, family := range []string{FamilyTTS, FamilySTT} {
		state := result.Families[family]
		if !state.Configured || state.Wired {
			t.Fatalf("%s state = %+v, want configured/not wired", family, state)
		}
		if !strings.Contains(state.Reason, "library-only") {
			t.Fatalf("%s reason = %q", family, state.Reason)
		}
	}
}

func TestDecisionPreflightIsTheSettingsGate(t *testing.T) {
	base := func(adapter, binding string) json.RawMessage {
		return json.RawMessage(`{
		  "providers": [
		    {"id": "judge", "type": "decision",
		     "api": {"type": "openai-chat-completions", "baseUrl": "https://decision.example/v1"},
		     "access": {"type": "api-key", "env": "DECISION_KEY"},
		     "modelIds": ["judge-1"],
		     "decision": {"adapter": "` + adapter + `"` + binding + `}}
		  ],
		  "selection": {"providerId": "judge", "modelId": "judge-1"}
		}`)
	}
	validBinding := `,"binding":{"state":{"text":"a customer is angry"},"questions":{"urgent":{"type":"noul","instructions":"is this urgent?"}},"mode":"enforce"}`

	if issues := ValidateModels(Options{Models: base("typesafe", validBinding)}); len(issues) != 0 {
		t.Fatalf("a valid binding reported issues: %+v", issues)
	}

	// Cloudflare does not support box questions; Preflight must report the field
	// while the document is still being saved.
	cloudflare := base("cloudflare", `,"binding":{"state":{"image_url":"data:image/png;base64,AAAA"},"questions":{"find":{"type":"box","instructions":"locate the label"}}}`)
	issues := ValidateModels(Options{Models: cloudflare})
	if !hasIssue(issues, "providers[judge].decision.binding.questions.find.type") {
		t.Fatalf("cloudflare box was not refused: %+v", issues)
	}

	// A remote image URL is refused before it can reach the provider.
	remote := base("typesafe", `,"binding":{"state":{"image_url":"https://example.com/a.png"},"questions":{"ok":{"type":"noul","instructions":"ok?"}}}`)
	issues = ValidateModels(Options{Models: remote})
	if !hasIssue(issues, "providers[judge].decision.binding.state") {
		t.Fatalf("remote image URL was not refused: %+v", issues)
	}

	// fail_open is legal only in shadow mode.
	badPolicy := base("typesafe", `,"binding":{"state":{"text":"x"},"questions":{"ok":{"type":"noul","instructions":"ok?"}},"mode":"enforce","failPolicy":"fail_open"}`)
	issues = ValidateModels(Options{Models: badPolicy})
	if !hasIssue(issues, "providers[judge].decision.binding.policy.failPolicy") {
		t.Fatalf("fail_open in enforce was not refused: %+v", issues)
	}

	// An unknown adapter is a provider configuration error, not a default.
	issues = ValidateModels(Options{Models: base("mystery", "")})
	if !hasIssue(issues, "providers[judge].decision") {
		t.Fatalf("unknown decision adapter was not refused: %+v", issues)
	}

	// No binding means no provider call and no invented answer: only the
	// capability declaration is checked.
	if issues := ValidateModels(Options{Models: base("typesafe", "")}); len(issues) != 0 {
		t.Fatalf("a decision provider without a saved binding must pass shape checks: %+v", issues)
	}
}

func hasIssue(issues []modelcfg.Issue, field string) bool {
	for _, issue := range issues {
		if issue.Field == field {
			return true
		}
	}
	return false
}

func TestMain(m *testing.M) {
	// The tests never dial a real provider; keep the demo catalog out of the way
	// so a developer's environment cannot change resolution.
	_ = os.Unsetenv("KEEL_DEMO_PROVIDER")
	os.Exit(m.Run())
}

// TestDecisionBindingStateNeverEntersTheProviderProjection pins the privacy
// boundary: the decision binding's state (caller data — a customer message,
// maybe an image) is available to the wiring that validates it, but it never
// appears in the JSON GET /v1/providers serves.
func TestDecisionBindingStateNeverEntersTheProviderProjection(t *testing.T) {
	document := json.RawMessage(`{
	  "providers": [
	    {"id": "judge", "type": "decision",
	     "api": {"type": "openai-chat-completions", "baseUrl": "https://decision.example/v1"},
	     "access": {"type": "api-key", "env": "DECISION_KEY"},
	     "modelIds": ["judge-1"],
	     "decision": {"adapter": "typesafe", "binding": {
	       "state": {"text": "MARKER-CUSTOMER-SECRET"},
	       "questions": {"urgent": {"type": "noul", "instructions": "MARKER-INSTRUCTION"}}}}}
	  ]
	}`)
	resolved, err := resolve(Options{Models: document})
	if err != nil {
		t.Fatal(err)
	}
	provider, ok := findProvider(resolved, "judge")
	if !ok {
		t.Fatal("judge provider missing from the resolution")
	}
	if provider.Decision == nil || provider.Decision.Binding == nil ||
		!strings.Contains(string(provider.Decision.Binding.State), "MARKER-CUSTOMER-SECRET") {
		t.Fatalf("the wiring must see the saved binding: %+v", provider.Decision)
	}
	raw, err := json.Marshal(provider)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"MARKER-CUSTOMER-SECRET", "MARKER-INSTRUCTION", `"binding"`, `"state"`, `"questions"`} {
		if strings.Contains(string(raw), marker) {
			t.Fatalf("provider projection leaked %q: %s", marker, raw)
		}
	}
}

func findProvider(resolved resolved, providerID string) (modelcfg.ProviderView, bool) {
	for _, provider := range resolved.resolution.Providers {
		if provider.ProviderID == providerID {
			return provider, true
		}
	}
	return modelcfg.ProviderView{}, false
}
