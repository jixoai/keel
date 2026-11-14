package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

const testMediaKey = "sk-test-media-key-do-not-leak"

// setMediaEnv points the media gateway at base and clears the fallback pair so
// a developer shell's KEEL_BASE_URL / KEEL_API_KEY can never leak into a test.
func setMediaEnv(t *testing.T, base string) {
	t.Helper()
	t.Setenv("KEEL_MEDIA_BASE_URL", base)
	t.Setenv("KEEL_BASE_URL", "")
	t.Setenv("KEEL_MEDIA_API_KEY", testMediaKey)
	t.Setenv("KEEL_API_KEY", "")
	t.Setenv("KEEL_MEDIA_MODEL_LLM", "")
	t.Setenv("KEEL_MEDIA_MODEL_IMAGE", "")
	t.Setenv("KEEL_MEDIA_MODEL_DECISION", "")
}

func clearMediaEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"KEEL_MEDIA_BASE_URL", "KEEL_BASE_URL",
		"KEEL_MEDIA_API_KEY", "KEEL_API_KEY",
		"KEEL_MEDIA_MODEL_LLM", "KEEL_MEDIA_MODEL_IMAGE", "KEEL_MEDIA_MODEL_DECISION",
	} {
		t.Setenv(name, "")
	}
}

func newMediaServer(t *testing.T, logw io.Writer) *httptest.Server {
	t.Helper()
	if logw == nil {
		logw = io.Discard
	}
	srv := New(Config{Logger: slog.New(slog.NewTextHandler(logw, nil))})
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	return hs
}

func doMedia(t *testing.T, hs *httptest.Server, method, path string, body any, tenant string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, hs.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tenant != "" {
		req.Header.Set("X-Tenant-ID", tenant)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

type mediaCall struct {
	path        string
	auth        string
	contentType string
	body        []byte
}

type fakeMediaProvider struct {
	mu    sync.Mutex
	calls []mediaCall
}

func (f *fakeMediaProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, mediaCall{
		path:        r.URL.Path,
		auth:        r.Header.Get("Authorization"),
		contentType: r.Header.Get("Content-Type"),
		body:        body,
	})
	f.mu.Unlock()

	if r.Header.Get("Authorization") != "Bearer "+testMediaKey {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad credential"}}`))
		return
	}
	switch r.URL.Path {
	case "/images/generations":
		var req struct {
			Model          string `json:"model"`
			Prompt         string `json:"prompt"`
			Size           string `json:"size"`
			N              int    `json:"n"`
			NegativePrompt string `json:"negative_prompt"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Prompt == "provider-fail" {
			// A hostile/buggy provider echoing the credential must never reach
			// the client or the log.
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited: Bearer ` + testMediaKey + `"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"model":"bailian-token-plan/wan2.7-image","data":[` +
			`{"url":"https://cdn.example.test/img-1.png","width":1024,"height":1024}]}`))
	case "/images/edits":
		_, _ = w.Write([]byte(`{"model":"bailian-token-plan/wan2.7-image","data":[` +
			`{"url":"https://cdn.example.test/edited-json.png","width":1024,"height":1024}]}`))
	case "/decisions":
		_, _ = w.Write([]byte(`{"model":"bailian-token-plan/decision-model-preview",` +
			`"answers":[{"question":0,"answer":"yes"}],"usage":{"total_tokens":12}}`))
	case "/chat/completions":
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","model":"bailian-token-plan/qwen3.8-flash",` +
			`"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"total_tokens":3}}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"no such provider route"}}`))
	}
}

func (f *fakeMediaProvider) callCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, call := range f.calls {
		if call.path == path {
			n++
		}
	}
	return n
}

func (f *fakeMediaProvider) callsFor(path string) []mediaCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []mediaCall
	for _, call := range f.calls {
		if call.path == path {
			out = append(out, call)
		}
	}
	return out
}

func newFakeProvider(t *testing.T) (*fakeMediaProvider, *httptest.Server) {
	t.Helper()
	fake := &fakeMediaProvider{}
	hs := httptest.NewServer(fake)
	t.Cleanup(hs.Close)
	return fake, hs
}

func TestMediaGenerateHappyPath(t *testing.T) {
	fake, provider := newFakeProvider(t)
	setMediaEnv(t, provider.URL)
	hs := newMediaServer(t, nil)
	tenant := uuid.NewString()

	status, body := doMedia(t, hs, http.MethodPost, "/v1/media/images/generate", map[string]any{
		"prompt":         "a lighthouse at dusk",
		"negativePrompt": "blurry",
		"size":           "1024x1536",
		"n":              2,
	}, tenant)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	var out struct {
		Assets []struct {
			URL    string `json:"url"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
			Model  string `json:"model"`
		} `json:"assets"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("bad response: %v (%s)", err, body)
	}
	if len(out.Assets) != 1 || out.Assets[0].URL != "https://cdn.example.test/img-1.png" {
		t.Fatalf("assets = %+v", out.Assets)
	}
	if out.Model != "bailian-token-plan/wan2.7-image" {
		t.Fatalf("model = %q", out.Model)
	}
	calls := fake.callsFor("/images/generations")
	if len(calls) != 1 {
		t.Fatalf("provider calls = %d", len(calls))
	}
	if calls[0].auth != "Bearer "+testMediaKey {
		t.Fatalf("provider auth = %q", calls[0].auth)
	}
	var forwarded struct {
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		Size           string `json:"size"`
		N              int    `json:"n"`
		NegativePrompt string `json:"negative_prompt"`
	}
	if err := json.Unmarshal(calls[0].body, &forwarded); err != nil {
		t.Fatal(err)
	}
	if forwarded.Prompt != "a lighthouse at dusk" || forwarded.N != 2 ||
		forwarded.Size != "1024x1536" || forwarded.NegativePrompt != "blurry" ||
		forwarded.Model != "bailian-token-plan/wan2.7-image" {
		t.Fatalf("forwarded = %+v", forwarded)
	}
	if strings.Contains(body, testMediaKey) {
		t.Fatalf("response leaked the key: %s", body)
	}
}

func TestMediaEditJSONHappyPath(t *testing.T) {
	fake, provider := newFakeProvider(t)
	setMediaEnv(t, provider.URL)
	hs := newMediaServer(t, nil)

	ref := "https://images.example.test/reference.png"
	status, body := doMedia(t, hs, http.MethodPost, "/v1/media/images/edit", map[string]any{
		"prompt": "make it night",
		"image":  ref,
	}, uuid.NewString())
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, "edited-json.png") {
		t.Fatalf("unexpected body: %s", body)
	}
	calls := fake.callsFor("/images/edits")
	if len(calls) != 1 {
		t.Fatalf("provider calls = %d", len(calls))
	}
	var forwarded struct {
		Prompt string `json:"prompt"`
		Image  string `json:"image"`
		Model  string `json:"model"`
	}
	if err := json.Unmarshal(calls[0].body, &forwarded); err != nil {
		t.Fatal(err)
	}
	if forwarded.Prompt != "make it night" || forwarded.Image != ref ||
		forwarded.Model != "bailian-token-plan/wan2.7-image" {
		t.Fatalf("forwarded = %+v", forwarded)
	}
}

func TestMediaEditRejectsDataURL(t *testing.T) {
	fake, provider := newFakeProvider(t)
	setMediaEnv(t, provider.URL)
	hs := newMediaServer(t, nil)

	status, body := doMedia(t, hs, http.MethodPost, "/v1/media/images/edit", map[string]any{
		"prompt": "add a hat",
		"image":  "data:image/png;base64,AAAA",
	}, uuid.NewString())
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, "data URLs are not supported") ||
		!strings.Contains(body, "http(s) URL") {
		t.Fatalf("body = %s", body)
	}
	if fake.callCount("/images/edits") != 0 {
		t.Fatal("a rejected data URL must not reach the provider")
	}
}

func TestMediaDecisionHappyPath(t *testing.T) {
	_, provider := newFakeProvider(t)
	setMediaEnv(t, provider.URL)
	hs := newMediaServer(t, nil)

	status, body := doMedia(t, hs, http.MethodPost, "/v1/media/decision", map[string]any{
		"input": "The user wants a red theme.",
		"questions": []map[string]any{{
			"type":      "choice",
			"predicate": "Which theme?",
			"choices":   []map[string]any{{"value": "red", "description": "warm"}},
		}},
	}, uuid.NewString())
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	var out struct {
		Answers []map[string]any `json:"answers"`
		Model   string           `json:"model"`
		Usage   map[string]any   `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("bad response: %v (%s)", err, body)
	}
	if len(out.Answers) != 1 || out.Answers[0]["answer"] != "yes" {
		t.Fatalf("answers = %+v", out.Answers)
	}
	if out.Model != "bailian-token-plan/decision-model-preview" {
		t.Fatalf("model = %q", out.Model)
	}
	if out.Usage == nil || out.Usage["total_tokens"] != float64(12) {
		t.Fatalf("usage = %+v", out.Usage)
	}
}

func TestMediaDecisionValidation(t *testing.T) {
	fake, provider := newFakeProvider(t)
	setMediaEnv(t, provider.URL)
	hs := newMediaServer(t, nil)
	tenant := uuid.NewString()

	cases := []struct {
		name string
		body map[string]any
	}{
		{"no questions", map[string]any{"input": "x", "questions": []any{}}},
		{"too many questions", func() map[string]any {
			questions := make([]map[string]any, 401)
			for i := range questions {
				questions[i] = map[string]any{"type": "predicate", "predicate": "p"}
			}
			return map[string]any{"input": "x", "questions": questions}
		}()},
		{"unknown type", map[string]any{
			"input":     "x",
			"questions": []map[string]any{{"type": "ranking", "predicate": "p"}},
		}},
		{"missing input", map[string]any{
			"questions": []map[string]any{{"type": "predicate", "predicate": "p"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := doMedia(t, hs, http.MethodPost, "/v1/media/decision", tc.body, tenant)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", status, body)
			}
		})
	}
	if got := fake.callCount("/decisions"); got != 0 {
		t.Fatalf("validation failures must not reach the provider, got %d calls", got)
	}
}

func TestMediaCapabilitiesConfigured(t *testing.T) {
	_, provider := newFakeProvider(t)
	setMediaEnv(t, provider.URL)
	hs := newMediaServer(t, nil)

	status, body := doMedia(t, hs, http.MethodGet, "/v1/media/capabilities", nil, uuid.NewString())
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	var out struct {
		Configured bool `json:"configured"`
		LLM        bool `json:"llm"`
		Image      bool `json:"image"`
		Decision   bool `json:"decision"`
		Video      bool `json:"video"`
		TTS        bool `json:"tts"`
		STT        bool `json:"stt"`
		Realtime   bool `json:"realtime"`
		Models     struct {
			LLM      string `json:"llm"`
			Image    string `json:"image"`
			Decision string `json:"decision"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("bad response: %v (%s)", err, body)
	}
	if !out.Configured || !out.LLM || !out.Image || !out.Decision {
		t.Fatalf("capabilities = %+v", out)
	}
	if out.Video || out.TTS || out.STT || out.Realtime {
		t.Fatalf("unconfigured transports must be false: %+v", out)
	}
	if out.Models.LLM != "bailian-token-plan/qwen3.8-flash" ||
		out.Models.Image != "bailian-token-plan/wan2.7-image" ||
		out.Models.Decision != "bailian-token-plan/decision-model-preview" {
		t.Fatalf("models = %+v", out.Models)
	}
	if strings.Contains(body, testMediaKey) || strings.Contains(body, provider.URL) {
		t.Fatalf("capabilities leaked configuration: %s", body)
	}
}

func TestMediaUnconfigured503(t *testing.T) {
	clearMediaEnv(t)
	hs := newMediaServer(t, nil)
	tenant := uuid.NewString()

	posts := []struct {
		path string
		body map[string]any
	}{
		{"/v1/media/images/generate", map[string]any{"prompt": "x"}},
		{"/v1/media/images/edit", map[string]any{"prompt": "x", "image": "https://example.test/a.png"}},
		{"/v1/media/decision", map[string]any{
			"input":     "x",
			"questions": []map[string]any{{"type": "predicate", "predicate": "p"}},
		}},
		{"/v1/media/llm/chat", map[string]any{"messages": []map[string]any{{"role": "user", "content": "hi"}}}},
	}
	for _, tc := range posts {
		t.Run(tc.path, func(t *testing.T) {
			status, body := doMedia(t, hs, http.MethodPost, tc.path, tc.body, tenant)
			if status != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, body = %s", status, body)
			}
			if body != `{"error":"media provider is not configured"}` {
				t.Fatalf("body = %s", body)
			}
		})
	}

	status, body := doMedia(t, hs, http.MethodGet, "/v1/media/capabilities", nil, tenant)
	if status != http.StatusOK {
		t.Fatalf("capabilities status = %d, body = %s", status, body)
	}
	var caps struct {
		Configured bool `json:"configured"`
		LLM        bool `json:"llm"`
		Image      bool `json:"image"`
		Decision   bool `json:"decision"`
	}
	if err := json.Unmarshal([]byte(body), &caps); err != nil {
		t.Fatal(err)
	}
	if caps.Configured || caps.LLM || caps.Image || caps.Decision {
		t.Fatalf("capabilities = %+v", caps)
	}
}

func TestMediaModelNotAllowed(t *testing.T) {
	fake, provider := newFakeProvider(t)
	setMediaEnv(t, provider.URL)
	hs := newMediaServer(t, nil)
	tenant := uuid.NewString()

	status, body := doMedia(t, hs, http.MethodPost, "/v1/media/images/generate", map[string]any{
		"prompt": "x",
		"model":  "some-other/image-model",
	}, tenant)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, "is not allowed") {
		t.Fatalf("body = %s", body)
	}

	status, body = doMedia(t, hs, http.MethodPost, "/v1/media/decision", map[string]any{
		"input":     "x",
		"model":     "some-other/decision-model",
		"questions": []map[string]any{{"type": "score", "predicate": "p"}},
	}, tenant)
	if status != http.StatusBadRequest {
		t.Fatalf("decision status = %d, body = %s", status, body)
	}
	if fake.callCount("/images/generations") != 0 || fake.callCount("/decisions") != 0 {
		t.Fatal("model validation must run before the provider call")
	}
}

func TestMediaEmptyPrompt(t *testing.T) {
	fake, provider := newFakeProvider(t)
	setMediaEnv(t, provider.URL)
	hs := newMediaServer(t, nil)

	for _, path := range []string{"/v1/media/images/generate", "/v1/media/images/edit"} {
		status, body := doMedia(t, hs, http.MethodPost, path, map[string]any{
			"prompt": "   ",
			"image":  "https://example.test/a.png",
		}, uuid.NewString())
		if status != http.StatusBadRequest || !strings.Contains(body, "prompt is required") {
			t.Fatalf("%s: status = %d, body = %s", path, status, body)
		}
	}
	if fake.callCount("/images/generations") != 0 || fake.callCount("/images/edits") != 0 {
		t.Fatal("empty prompt must not reach the provider")
	}
}

func TestMediaImageValidation(t *testing.T) {
	fake, provider := newFakeProvider(t)
	setMediaEnv(t, provider.URL)
	hs := newMediaServer(t, nil)
	tenant := uuid.NewString()

	cases := []struct {
		name string
		body map[string]any
	}{
		{"n zero", map[string]any{"prompt": "x", "n": 0}},
		{"n too large", map[string]any{"prompt": "x", "n": 5}},
		{"size not allowed", map[string]any{"prompt": "x", "size": "512x512"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := doMedia(t, hs, http.MethodPost, "/v1/media/images/generate", tc.body, tenant)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", status, body)
			}
		})
	}
	if fake.callCount("/images/generations") != 0 {
		t.Fatal("invalid image requests must not reach the provider")
	}
}

func TestMediaMissingTenant(t *testing.T) {
	_, provider := newFakeProvider(t)
	setMediaEnv(t, provider.URL)
	hs := newMediaServer(t, nil)

	status, body := doMedia(t, hs, http.MethodPost, "/v1/media/images/generate",
		map[string]any{"prompt": "x"}, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", status, body)
	}
}

func TestMediaRequestBodyCap(t *testing.T) {
	_, provider := newFakeProvider(t)
	setMediaEnv(t, provider.URL)
	hs := newMediaServer(t, nil)

	status, body := doMedia(t, hs, http.MethodPost, "/v1/media/images/generate",
		map[string]any{"prompt": strings.Repeat("a", mediaMaxBody+1)}, uuid.NewString())
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", status, body)
	}
}

func TestMediaNoKeyOrBodyInResponseOrLog(t *testing.T) {
	_, provider := newFakeProvider(t)
	setMediaEnv(t, provider.URL)
	var logs bytes.Buffer
	hs := newMediaServer(t, &logs)
	tenant := uuid.NewString()
	const secretPrompt = "SECRET-PROMPT-DO-NOT-LOG"

	status, body := doMedia(t, hs, http.MethodPost, "/v1/media/images/generate",
		map[string]any{"prompt": secretPrompt}, tenant)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	status, body = doMedia(t, hs, http.MethodPost, "/v1/media/images/generate",
		map[string]any{"prompt": "provider-fail"}, tenant)
	if status != http.StatusTooManyRequests {
		t.Fatalf("provider error status = %d, body = %s", status, body)
	}
	if strings.Contains(body, testMediaKey) {
		t.Fatalf("provider error leaked the key: %s", body)
	}
	if !strings.Contains(body, "[redacted]") {
		t.Fatalf("provider error was not marked redacted: %s", body)
	}
	logged := logs.String()
	if strings.Contains(logged, testMediaKey) {
		t.Fatalf("log leaked the key: %s", logged)
	}
	if strings.Contains(logged, secretPrompt) {
		t.Fatalf("log leaked the request body: %s", logged)
	}
}

func TestMediaChatProxy(t *testing.T) {
	fake, provider := newFakeProvider(t)
	setMediaEnv(t, provider.URL)
	hs := newMediaServer(t, nil)

	status, body := doMedia(t, hs, http.MethodPost, "/v1/media/llm/chat", map[string]any{
		"messages":   []map[string]any{{"role": "user", "content": "ping"}},
		"max_tokens": 8,
	}, uuid.NewString())
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, "chatcmpl-test") {
		t.Fatalf("unexpected body: %s", body)
	}
	calls := fake.callsFor("/chat/completions")
	if len(calls) != 1 {
		t.Fatalf("provider calls = %d", len(calls))
	}
	var forwarded struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
	}
	if err := json.Unmarshal(calls[0].body, &forwarded); err != nil {
		t.Fatal(err)
	}
	if forwarded.Model != "bailian-token-plan/qwen3.8-flash" || forwarded.MaxTokens != 8 {
		t.Fatalf("forwarded = %+v", forwarded)
	}
}
