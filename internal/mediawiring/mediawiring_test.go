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
		case "/audio/speech":
			w.Header().Set("Content-Type", "audio/wav")
			w.Header().Set("X-Audio-Duration-Ms", "1500")
			_, _ = w.Write([]byte("wav-bytes"))
		case "/audio/transcriptions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"text":"hello world","language":"en","model":"whisper-1","duration":1.2,`+
				`"segments":[{"start":0,"end":0.5,"text":"hello"},{"start":0.5,"end":1.2,"text":"world"}],`+
				`"usage":{"input_tokens":2,"output_tokens":1,"cost":0.1}}`)
		case "/v1/systemone":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"decision-req-1","model":"judge-1","answers":{"urgent":{"type":"noul","noul":0.91}},`+
				`"usage":{"input_tokens":5,"output_tokens":1,"cost":1.25}}`)
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

const speechDocumentTemplate = `{
  "providers": [
    {"id": "speech", "type": "tts",
     "api": {"type": "openai-chat-completions", "baseUrl": "SERVER"},
     "access": {"type": "api-key", "env": "SPEECH_KEY"},
     "modelIds": ["tts-1"]},
    {"id": "ears", "type": "stt",
     "api": {"type": "openai-chat-completions", "baseUrl": "SERVER"},
     "access": {"type": "api-key", "env": "SPEECH_KEY"},
     "modelIds": ["whisper-1"]}
  ]
}`

func speechDocument(serverURL string) json.RawMessage {
	return json.RawMessage(strings.ReplaceAll(speechDocumentTemplate, "SERVER", serverURL))
}

const decisionDocumentTemplate = `{
  "providers": [
    {"id": "judge", "type": "decision",
     "api": {"type": "openai-chat-completions", "baseUrl": "SERVER"},
     "access": {"type": "api-key", "env": "DECISION_KEY"},
     "modelIds": ["judge-1"],
     "decision": {"adapter": "typesafe", "binding": {
       "state": {"ticket": "customer is angry"},
       "questions": {"urgent": {"type": "noul", "instructions": "is this urgent?"}},
       "mode": "MODE"}}}
  ]
}`

func decisionDocument(serverURL, mode string) json.RawMessage {
	document := strings.ReplaceAll(decisionDocumentTemplate, "SERVER", serverURL)
	return json.RawMessage(strings.ReplaceAll(document, "MODE", mode))
}

func speechAndDecisionDocument(serverURL string) json.RawMessage {
	combined := `{"providers": [` +
		`{"id": "speech", "type": "tts", "api": {"type": "openai-chat-completions", "baseUrl": "` + serverURL + `"}, "access": {"type": "api-key", "env": "SPEECH_KEY"}, "modelIds": ["tts-1"]},` +
		`{"id": "ears", "type": "stt", "api": {"type": "openai-chat-completions", "baseUrl": "` + serverURL + `"}, "access": {"type": "api-key", "env": "SPEECH_KEY"}, "modelIds": ["whisper-1"]},` +
		`{"id": "judge", "type": "decision", "api": {"type": "openai-chat-completions", "baseUrl": "` + serverURL + `"}, "access": {"type": "api-key", "env": "DECISION_KEY"}, "modelIds": ["judge-1"], "decision": {"adapter": "typesafe", "binding": {"state": {"ticket": "customer is angry"}, "questions": {"urgent": {"type": "noul", "instructions": "is this urgent?"}}, "mode": "enforce"}}}` +
		`]}`
	return json.RawMessage(combined)
}

func findTool(t *testing.T, tools []agentcore.AgentTool, name string) agentcore.AgentTool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name() == name {
			return tool
		}
	}
	return nil
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

func TestBuildWiresSpeechToolsAndLayerFSReferences(t *testing.T) {
	server := newMediaServer(t)
	store := openArtifactStore(t)
	result := Build(Options{
		Models:     speechDocument(server.server.URL),
		Credential: credentialLookup(map[string]string{"SPEECH_KEY": "speech-key"}),
		Artifacts:  store,
		HTTPClient: server.server.Client(),
	})
	for _, family := range []string{FamilyTTS, FamilySTT} {
		if state := result.Families[family]; !state.Configured || !state.Wired {
			t.Fatalf("%s state = %+v, want configured/wired", family, state)
		}
	}
	if len(result.Tools) != 2 {
		t.Fatalf("tools = %d, want speech_tts and speech_stt", len(result.Tools))
	}

	ttsOutcome := runTool(t, result.Tools, "speech_tts",
		`{"text":"hello world","voice":"alloy","language":"en","format":"wav"}`, "session-tts")
	if ttsOutcome.IsError {
		t.Fatalf("speech_tts failed: %s", contentText(t, ttsOutcome.Result))
	}
	var tts mediaadapter.SpeechTTSToolOutput
	if err := json.Unmarshal([]byte(contentText(t, ttsOutcome.Result)), &tts); err != nil {
		t.Fatal(err)
	}
	if !tts.Success || tts.Audio.BlobID == "" || tts.Audio.Mime != "audio/wav" {
		t.Fatalf("speech_tts output = %+v", tts)
	}
	if tts.Usage.InputCharacters != 11 || tts.Usage.AudioBytes != int64(len("wav-bytes")) || tts.Usage.AudioMillis != 1500 {
		t.Fatalf("speech_tts usage = %+v", tts.Usage)
	}
	if got := string(readArtifact(t, store, tts.Audio.BlobID)); got != "wav-bytes" {
		t.Fatalf("tts artifact = %q", got)
	}

	ttsRequest := findRequest(t, server.snapshot(), "/audio/speech")
	if ttsRequest.Auth != "Bearer speech-key" {
		t.Fatalf("tts auth = %q", ttsRequest.Auth)
	}
	for _, want := range []string{`"model":"tts-1"`, `"input":"hello world"`, `"voice":"alloy"`, `"language":"en"`, `"response_format":"wav"`} {
		if !strings.Contains(ttsRequest.Body, want) {
			t.Fatalf("tts wire body missing %s: %s", want, ttsRequest.Body)
		}
	}

	sttOutcome := runTool(t, result.Tools, "speech_stt",
		`{"blob_id":"`+tts.Audio.BlobID+`","mime":"audio/wav","language":"en"}`, "session-stt")
	if sttOutcome.IsError {
		t.Fatalf("speech_stt failed: %s", contentText(t, sttOutcome.Result))
	}
	var stt mediaadapter.SpeechSTTToolOutput
	if err := json.Unmarshal([]byte(contentText(t, sttOutcome.Result)), &stt); err != nil {
		t.Fatal(err)
	}
	if !stt.Success || stt.Transcript.Text != "hello world" || len(stt.Transcript.Segments) != 2 {
		t.Fatalf("speech_stt output = %+v", stt)
	}
	if stt.Transcript.Segments[1].StartMillis != 500 || stt.Transcript.Segments[1].EndMillis != 1200 {
		t.Fatalf("segments = %+v", stt.Transcript.Segments)
	}
	if stt.Audio == nil || stt.Audio.BlobID != tts.Audio.BlobID {
		t.Fatalf("transcript input reference = %+v", stt.Audio)
	}
	if stt.Transcript.Usage.TranscriptCharacters != 11 || stt.Transcript.Usage.AudioBytes != int64(len("wav-bytes")) {
		t.Fatalf("stt usage = %+v", stt.Transcript.Usage)
	}
	sttRequest := findRequest(t, server.snapshot(), "/audio/transcriptions")
	if sttRequest.Auth != "Bearer speech-key" {
		t.Fatalf("stt auth = %q", sttRequest.Auth)
	}
	if !strings.Contains(sttRequest.Body, `name="file"`) || !strings.Contains(sttRequest.Body, "wav-bytes") {
		t.Fatalf("stt multipart body missing the audio: %s", sttRequest.Body)
	}
	for _, raw := range []string{contentText(t, ttsOutcome.Result), contentText(t, sttOutcome.Result)} {
		for _, forbidden := range []string{"cost", "0.1", "1.25", "speech-key", server.server.URL} {
			if strings.Contains(raw, forbidden) {
				t.Fatalf("speech result leaked %q: %s", forbidden, raw)
			}
		}
	}
}

func TestBuildWiresDecisionToolWithSavedBindingAndPolicy(t *testing.T) {
	server := newMediaServer(t)
	result := Build(Options{
		Models:     decisionDocument(server.server.URL, "enforce"),
		Credential: credentialLookup(map[string]string{"DECISION_KEY": "decision-key"}),
		HTTPClient: server.server.Client(),
	})
	if state := result.Families[FamilyDecision]; !state.Configured || !state.Wired {
		t.Fatalf("decision state = %+v", state)
	}
	tool := findTool(t, result.Tools, "decision_ask")
	if tool == nil {
		t.Fatalf("decision_ask is not in the tool registry: %v", result.Tools)
	}

	// No arguments: the saved binding supplies state and questions.
	outcome := runTool(t, result.Tools, "decision_ask", `{}`, "session-decision")
	if outcome.IsError {
		t.Fatalf("decision_ask failed: %s", contentText(t, outcome.Result))
	}
	var output mediaadapter.DecisionToolOutput
	if err := json.Unmarshal([]byte(contentText(t, outcome.Result)), &output); err != nil {
		t.Fatal(err)
	}
	if !output.Success || !output.Answered || output.Mode != "enforce" || output.Shadow {
		t.Fatalf("decision output = %+v", output)
	}
	if len(output.Answers) != 1 || !strings.Contains(string(output.Answers["urgent"]), `"noul":0.91`) {
		t.Fatalf("answers = %+v", output.Answers)
	}
	if strings.Contains(string(output.Answers["urgent"]), "confidence") {
		t.Fatalf("the adapter added confidence to a noul answer: %s", output.Answers["urgent"])
	}
	if output.Usage.InputTokens != 5 || output.Usage.OutputTokens != 1 || output.Usage.Questions != 1 {
		t.Fatalf("usage = %+v", output.Usage)
	}

	request := findRequest(t, server.snapshot(), "/v1/systemone")
	if request.Auth != "Bearer decision-key" {
		t.Fatalf("decision auth = %q", request.Auth)
	}
	for _, want := range []string{`"state":{"ticket":"customer is angry"}`, `"questions":{"urgent"`, `"model":"judge-1"`} {
		if !strings.Contains(request.Body, want) {
			t.Fatalf("decision wire body missing %s: %s", want, request.Body)
		}
	}
	raw, _ := json.Marshal(output)
	for _, forbidden := range []string{"cost", "1.25", "decision-key", server.server.URL} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("decision output leaked %q: %s", forbidden, raw)
		}
	}
}

func TestBuildSpeechAndDecisionKeepMissingCredentialFailClosed(t *testing.T) {
	server := newMediaServer(t)
	result := Build(Options{
		Models:     speechAndDecisionDocument(server.server.URL),
		Credential: credentialLookup(map[string]string{}),
		Artifacts:  openArtifactStore(t),
		HTTPClient: server.server.Client(),
	})
	for _, family := range []string{FamilyTTS, FamilySTT, FamilyDecision} {
		if state := result.Families[family]; !state.Wired {
			t.Fatalf("%s state = %+v, want a wired fail-closed tool", family, state)
		}
	}

	tts := runTool(t, result.Tools, "speech_tts", `{"text":"hello"}`, "session-tts")
	if !tts.IsError || !strings.Contains(contentText(t, tts.Result), "missing_credentials") {
		t.Fatalf("speech_tts missing credential = %s", contentText(t, tts.Result))
	}
	decision := runTool(t, result.Tools, "decision_ask", `{}`, "session-decision")
	if !decision.IsError || !strings.Contains(contentText(t, decision.Result), "missing_credentials") {
		t.Fatalf("decision_ask missing credential = %s", contentText(t, decision.Result))
	}
	if got := server.count("/audio/speech") + server.count("/audio/transcriptions") + server.count("/v1/systemone"); got != 0 {
		t.Fatalf("a provider without a credential was called %d times", got)
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
