package servicehost

// End-to-end modality dispatch through the *assembled* runtime (change
// keel-harness-v2, modality-input lane): a committed attachment travels through
// Boot → generation → runner → real resolver → real encoder → mock vendor. The
// mock asserts the request body, and the model catalog document decides whether
// the attachment is native or metadata text.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
)

// runAssembledAttachment boots the runtime for the given models document, commits
// one user.message carrying the attachment, runs one attempt, and returns the
// body the mock vendor received (nil when the vendor was never called).
func runAssembledAttachment(t *testing.T, doc string, blobs *stubBlobs, attachment map[string]any, vendor http.HandlerFunc) map[string]any {
	t.Helper()
	var seen map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &seen)
		vendor(w, r)
	}))
	t.Cleanup(server.Close)

	resolvedDoc := strings.ReplaceAll(doc, "@SERVER@", server.URL)
	llm := LLMOptions{
		LookupEnv: func(name string) (string, bool) {
			if name == "MOCK_KEY" {
				return "mock-key", true
			}
			return "", false
		},
		Images: blobs,
	}
	st := kernel.NewMemStore()
	k := kernel.New(st, kernel.Options{})
	rt, err := Boot(context.Background(), Config{
		Seams:     Seams{Committer: k, Reader: k, Leases: k, External: k, Runs: fakeMirror{}, Logger: discardLogger()},
		Tenant:    host.TenantConfig{Models: json.RawMessage(resolvedDoc)},
		LLM:       llm,
		RuntimeID: "test-runtime",
	})
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	defer rt.Close(context.Background())

	bound, err := rt.Binding().Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer bound.Release(context.Background())
	runner := bound.Runner()

	tenant := uuid.New()
	session := uuid.New()
	st.CreateSession(tenant, session)
	payload, _ := json.Marshal(map[string]any{
		"text":        "summarize the attachment",
		"requestId":   "req-attach",
		"attachments": []map[string]any{attachment},
	})
	if _, err := k.Commit(context.Background(), kernel.CommitRequest{
		Scope:      kernel.Scope{TenantID: tenant, SessionID: session},
		Kind:       kernel.KindUserMessage,
		RequestID:  "req-attach",
		Payload:    payload,
		NativeEdge: true,
	}); err != nil {
		t.Fatalf("commit input: %v", err)
	}
	lease, err := k.ClaimScope(context.Background(), kernel.Scope{TenantID: tenant, SessionID: session}, "test-owner", time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := runner.RunAttempt(context.Background(), lease); err != nil {
		t.Fatalf("RunAttempt: %v", err)
	}
	return seen
}

// chatVendorSSE answers like a Chat Completions endpoint.
func chatVendorSSE(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, openAISSEBody("seen"))
}

// openAISSEBody is a minimal chat completion SSE stream.
func openAISSEBody(text string) string {
	return "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"" + text + "\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n" +
		"data: [DONE]\n\n"
}

// TestAssembledRuntimeCarriesANativePDF pins the whole chain: a PDF attachment
// on a model that declares PDF input arrives at the chat vendor as a `file`
// part with a data-URL file_data.
func TestAssembledRuntimeCarriesANativePDF(t *testing.T) {
	doc := `{
	  "providers": [{"id": "mock", "api": {"type": "openai-chat-completions", "baseUrl": "@SERVER@"},
	                 "access": {"type": "api-key", "env": "MOCK_KEY"}, "modelIds": ["mock-pdf"]}],
	  "modelRules": [{"match": "mock-pdf", "config": {"properties": {"inputFormat": {"text": true, "pdf": true}}}}],
	  "selection": {"providerId": "mock", "modelId": "mock-pdf"}
	}`
	blobs := &stubBlobs{data: []byte("%PDF-1.4 payload"), mime: "application/pdf"}
	seen := runAssembledAttachment(t, doc, blobs, map[string]any{
		"fileId": uuid.NewString(), "name": "report.pdf", "mime": "application/pdf", "size": 16,
	}, chatVendorSSE)
	if seen == nil {
		t.Fatal("the mock vendor saw no body")
	}
	messages, _ := seen["messages"].([]any)
	var filePart map[string]any
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		parts, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, part := range parts {
			block, _ := part.(map[string]any)
			if block["type"] == "file" {
				filePart, _ = block["file"].(map[string]any)
			}
		}
	}
	if filePart == nil {
		t.Fatalf("vendor body = %#v, want a native file part", seen["messages"])
	}
	if filePart["file_data"] != "data:application/pdf;base64,JVBERi0xLjQgcGF5bG9hZA==" {
		t.Fatalf("file_data = %#v, want the resolved PDF as a data URL", filePart["file_data"])
	}
	if filePart["filename"] != "report.pdf" {
		t.Fatalf("filename = %#v, want the attachment's name", filePart["filename"])
	}
}

// TestAssembledRuntimeDegradesPDFOnATextOnlyModel pins the fallback through the
// assembled runtime: the vendor receives the metadata text block (blob id, name,
// size) and no file part.
func TestAssembledRuntimeDegradesPDFOnATextOnlyModel(t *testing.T) {
	doc := `{
	  "providers": [{"id": "mock", "api": {"type": "openai-chat-completions", "baseUrl": "@SERVER@"},
	                 "access": {"type": "api-key", "env": "MOCK_KEY"}, "modelIds": ["mock-text"]}],
	  "modelRules": [{"match": "mock-text", "config": {"properties": {"inputFormat": {"text": true, "pdf": false}}}}],
	  "selection": {"providerId": "mock", "modelId": "mock-text"}
	}`
	blobID := uuid.NewString()
	blobs := &stubBlobs{data: []byte("%PDF-1.4 payload"), mime: "application/pdf"}
	seen := runAssembledAttachment(t, doc, blobs, map[string]any{
		"fileId": blobID, "name": "report.pdf", "mime": "application/pdf", "size": 16,
	}, chatVendorSSE)
	if seen == nil {
		t.Fatal("the mock vendor saw no body")
	}
	raw, _ := json.Marshal(seen["messages"])
	if strings.Contains(string(raw), "\"file\"") {
		t.Fatalf("vendor body = %s, want no native file part for a text-only model", raw)
	}
	text := string(raw)
	for _, want := range []string{"report.pdf", "application/pdf", blobID, "does not declare PDF"} {
		if !strings.Contains(text, want) {
			t.Fatalf("vendor body = %s, want the metadata fallback to contain %q", text, want)
		}
	}
}

// TestAssembledRuntimeModelPropertiesCarryEveryModality pins the product layer's
// projection: the resolved identity hands the runner every declared modality and
// the wire's media caps.
func TestAssembledRuntimeModelPropertiesCarryEveryModality(t *testing.T) {
	doc := `{
	  "providers": [{"id": "mock", "api": {"type": "openai-chat-completions", "baseUrl": "https://mock.example/v1"},
	                 "access": {"type": "api-key", "env": "MOCK_KEY"}, "modelIds": ["mock-multi"]}],
	  "modelRules": [{"match": "mock-multi", "config": {"properties": {"inputFormat":
	    {"text": true, "image": true, "video": true, "audio": true, "pdf": true}}}}],
	  "selection": {"providerId": "mock", "modelId": "mock-multi"}
	}`
	service := ungatedModels(t, doc, secretLLM(map[string]string{"MOCK_KEY": "k"}, nil, ""))
	active, err := service.Resolve(context.Background(), kernel.Scope{}, json.RawMessage(`{"providerId":"mock","modelId":"mock-multi"}`))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	props := active.Properties
	if props == nil {
		t.Fatal("the resolved identity must carry its capability facts")
	}
	if !props.TextInput || !props.ImageInput || !props.VideoInput || !props.AudioInput || !props.PDFInput {
		t.Fatalf("properties = %+v, want every declared modality", props)
	}
	if active.APIType != "openai-chat-completions" {
		t.Fatalf("api type = %q, want the resolved protocol token", active.APIType)
	}
	caps := active.MediaCaps
	if !caps.Audio.SupportsInline || !caps.File.SupportsMime("application/pdf") || !caps.Image.SupportsImageURL {
		t.Fatalf("media caps = %+v, want the chat wire's full declaration", caps)
	}
	if active.ImageCaps.SupportsImageURL != caps.Image.SupportsImageURL {
		t.Fatalf("legacy ImageCaps = %+v, want it to mirror MediaCaps.Image", active.ImageCaps)
	}
}

// TestAssembledRuntimeCarriesNativeAudio pins the second native modality through
// the assembled runtime: a model that declares audio input on the chat wire sends
// the WAV inline as input_audio (base64 plus the wire format token).
func TestAssembledRuntimeCarriesNativeAudio(t *testing.T) {
	doc := `{
	  "providers": [{"id": "mock", "api": {"type": "openai-chat-completions", "baseUrl": "@SERVER@"},
	                 "access": {"type": "api-key", "env": "MOCK_KEY"}, "modelIds": ["mock-audio"]}],
	  "modelRules": [{"match": "mock-audio", "config": {"properties": {"inputFormat": {"text": true, "audio": true}}}}],
	  "selection": {"providerId": "mock", "modelId": "mock-audio"}
	}`
	blobs := &stubBlobs{data: []byte("RIFF\x24\x00\x00\x00WAVEfmt "), mime: "audio/wav"}
	seen := runAssembledAttachment(t, doc, blobs, map[string]any{
		"fileId": uuid.NewString(), "name": "memo.wav", "mime": "audio/wav", "size": 16,
	}, chatVendorSSE)
	if seen == nil {
		t.Fatal("the mock vendor saw no body")
	}
	var audioPart map[string]any
	for _, part := range chatUserParts(t, seen) {
		if part["type"] == "input_audio" {
			audioPart, _ = part["input_audio"].(map[string]any)
		}
	}
	if audioPart == nil {
		t.Fatalf("vendor body = %#v, want a native input_audio part", seen["messages"])
	}
	if audioPart["format"] != "wav" {
		t.Fatalf("input_audio = %#v, want the wire format token", audioPart)
	}
	want := base64.StdEncoding.EncodeToString([]byte("RIFF\x24\x00\x00\x00WAVEfmt "))
	if audioPart["data"] != want {
		t.Fatalf("input_audio data = %#v, want the blob's base64", audioPart["data"])
	}
}

// TestAssembledRuntimeUndeclaredModalityDegrades pins the product projection's
// consequence: attemptModel always declares what the resolved document says, and
// a model that declares no input modalities (an empty config, or the catalog's
// text-only wildcard) receives the metadata fallback — never bytes it did not
// declare it accepts. The model-catalog lane owns declaring vision/PDF models.
func TestAssembledRuntimeUndeclaredModalityDegrades(t *testing.T) {
	doc := `{
	  "providers": [{"id": "mock", "api": {"type": "openai-chat-completions", "baseUrl": "@SERVER@"},
	                 "access": {"type": "api-key", "env": "MOCK_KEY"}, "modelIds": ["mock-undeclared"]}],
	  "modelRules": [{"match": "mock-undeclared", "config": {}}],
	  "selection": {"providerId": "mock", "modelId": "mock-undeclared"}
	}`
	blobID := uuid.NewString()
	blobs := &stubBlobs{data: []byte("%PDF-1.4 payload"), mime: "application/pdf"}
	seen := runAssembledAttachment(t, doc, blobs, map[string]any{
		"fileId": blobID, "name": "report.pdf", "mime": "application/pdf", "size": 16,
	}, chatVendorSSE)
	if seen == nil {
		t.Fatal("the mock vendor saw no body")
	}
	raw, _ := json.Marshal(seen["messages"])
	if strings.Contains(string(raw), `"file"`) {
		t.Fatalf("vendor body = %s, want no native file part for an undeclared modality", raw)
	}
	for _, want := range []string{"report.pdf", "does not declare PDF", blobID} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("vendor body = %s, want the metadata fallback to contain %q", raw, want)
		}
	}
}

// chatUserParts pulls the content parts of the first user message out of a chat
// body.
func chatUserParts(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if message["role"] != "user" {
			continue
		}
		parts, ok := message["content"].([]any)
		if !ok {
			continue
		}
		out := make([]map[string]any, 0, len(parts))
		for _, part := range parts {
			block, _ := part.(map[string]any)
			out = append(out, block)
		}
		return out
	}
	t.Fatalf("no user content parts in %#v", body["messages"])
	return nil
}

// TestMediaCapsForAPIUnknownDeclaresNothing is the resolver table's negative
// case, kept next to the assembled projection because both read the same function.
func TestMediaCapsForAPIUnknownDeclaresNothing(t *testing.T) {
	if caps := provider.MediaCapsForAPI("demo"); !caps.IsEmpty() {
		t.Fatalf("demo caps = %+v, want the empty declaration", caps)
	}
}
