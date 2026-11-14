package mediagw

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestConfigFromEnvDefaultsAndFallbacks(t *testing.T) {
	env := map[string]string{
		EnvFallbackBaseURL: "http://127.0.0.1:18899/",
		EnvFallbackAPIKey:  "fallback-key",
	}
	lookup := func(name string) string { return env[name] }
	cfg := ConfigFromEnv(lookup)
	if cfg.BaseURL != "http://127.0.0.1:18899" {
		t.Fatalf("base url = %q", cfg.BaseURL)
	}
	if cfg.APIKey != "fallback-key" {
		t.Fatalf("api key = %q", cfg.APIKey)
	}
	if cfg.ModelLLM != DefaultModelLLM || cfg.ModelImage != DefaultModelImage ||
		cfg.ModelDecision != DefaultModelDecision {
		t.Fatalf("models = %+v", cfg)
	}
	if !cfg.Configured() {
		t.Fatal("fallback config should be configured")
	}

	env[EnvBaseURL] = "https://media.example.test"
	env[EnvAPIKey] = "media-key"
	env[EnvModelLLM] = "custom/llm"
	env[EnvModelImage] = "custom/image"
	env[EnvModelDecision] = "custom/decision"
	cfg = ConfigFromEnv(lookup)
	if cfg.BaseURL != "https://media.example.test" || cfg.APIKey != "media-key" {
		t.Fatalf("media env did not win: %+v", cfg)
	}
	if cfg.ModelLLM != "custom/llm" || cfg.ModelImage != "custom/image" || cfg.ModelDecision != "custom/decision" {
		t.Fatalf("model env did not win: %+v", cfg)
	}

	cfg = ConfigFromEnv(func(string) string { return "" })
	if cfg.Configured() {
		t.Fatal("empty env must be unconfigured")
	}
	if cfg.BaseURL != "" || cfg.APIKey != "" {
		t.Fatalf("empty env leaked values: %+v", cfg)
	}
}

func TestCapabilitiesUnconfigured(t *testing.T) {
	g := New(Config{})
	caps := g.Capabilities()
	if caps.Configured || caps.LLM || caps.Image || caps.Decision ||
		caps.Video || caps.TTS || caps.STT || caps.Realtime {
		t.Fatalf("capabilities = %+v", caps)
	}
	if caps.Models.LLM != DefaultModelLLM {
		t.Fatalf("models = %+v", caps.Models)
	}
	if _, err := g.Generate(context.Background(), ImageRequest{Prompt: "x"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Generate err = %v", err)
	}
	if _, err := g.Edit(context.Background(), ImageEditRequest{Prompt: "x", Image: "data:image/png;base64,AAAA"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Edit err = %v", err)
	}
	if _, err := g.Decision(context.Background(), DecisionRequest{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Decision err = %v", err)
	}
	if _, err := g.Chat(context.Background(), ChatRequest{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Chat err = %v", err)
	}
}

func TestGatewayTimeout(t *testing.T) {
	block := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer provider.Close()
	defer close(block)

	g := New(Config{BaseURL: provider.URL, APIKey: "k", Timeout: 30 * time.Millisecond})
	_, err := g.Generate(context.Background(), ImageRequest{Prompt: "x"})
	var gwErr *Error
	if !errors.As(err, &gwErr) || gwErr.Status != http.StatusGatewayTimeout {
		t.Fatalf("err = %#v", err)
	}
}

func TestGatewayTransportErrorDoesNotLeakURL(t *testing.T) {
	g := New(Config{BaseURL: "http://127.0.0.1:1", APIKey: "super-secret", Timeout: 200 * time.Millisecond})
	_, err := g.Generate(context.Background(), ImageRequest{Prompt: "x"})
	var gwErr *Error
	if !errors.As(err, &gwErr) || gwErr.Status != http.StatusBadGateway {
		t.Fatalf("err = %#v", err)
	}
	if strings.Contains(gwErr.Message, "super-secret") || strings.Contains(gwErr.Message, "127.0.0.1") {
		t.Fatalf("message leaked internals: %q", gwErr.Message)
	}
}

func TestProviderErrorMessageSanitized(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request with key super-secret\nsecond line"}}`))
	}))
	defer provider.Close()

	g := New(Config{BaseURL: provider.URL, APIKey: "super-secret"})
	_, err := g.Generate(context.Background(), ImageRequest{Prompt: "x"})
	var gwErr *Error
	if !errors.As(err, &gwErr) {
		t.Fatalf("err = %#v", err)
	}
	if gwErr.Status != http.StatusBadRequest {
		t.Fatalf("status = %d", gwErr.Status)
	}
	if strings.Contains(gwErr.Message, "super-secret") {
		t.Fatalf("message leaked the key: %q", gwErr.Message)
	}
	if strings.ContainsAny(gwErr.Message, "\n\r") {
		t.Fatalf("message kept control characters: %q", gwErr.Message)
	}
	if !strings.Contains(gwErr.Message, "[redacted]") {
		t.Fatalf("message not redacted: %q", gwErr.Message)
	}
}

func TestSuccessBodyScrubbedOfKey(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"m","data":[{"url":"https://cdn.test/x.png?token=leaky-key"}]}`))
	}))
	defer provider.Close()

	g := New(Config{BaseURL: provider.URL, APIKey: "leaky-key"})
	result, err := g.Generate(context.Background(), ImageRequest{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Assets[0].URL, "leaky-key") {
		t.Fatalf("asset leaked the key: %q", result.Assets[0].URL)
	}
}

func TestGenerateValidation(t *testing.T) {
	g := New(Config{BaseURL: "http://127.0.0.1:1", APIKey: "k"})
	cases := []struct {
		name string
		req  ImageRequest
		want string
	}{
		{"empty prompt", ImageRequest{Prompt: "  "}, "prompt is required"},
		{"n too small", ImageRequest{Prompt: "x", N: -1}, "n must be between 1 and 4"},
		{"n too large", ImageRequest{Prompt: "x", N: 5}, "n must be between 1 and 4"},
		{"size", ImageRequest{Prompt: "x", Size: "800x800"}, "size must be one of"},
		{"model", ImageRequest{Prompt: "x", Model: "other"}, "is not allowed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := g.Generate(context.Background(), tc.req)
			var gwErr *Error
			if !errors.As(err, &gwErr) || gwErr.Status != http.StatusBadRequest {
				t.Fatalf("err = %#v", err)
			}
			if !strings.Contains(gwErr.Message, tc.want) {
				t.Fatalf("message = %q, want %q", gwErr.Message, tc.want)
			}
		})
	}
}

func TestDecisionValidation(t *testing.T) {
	g := New(Config{BaseURL: "http://127.0.0.1:1", APIKey: "k"})
	cases := []struct {
		name string
		req  DecisionRequest
		want string
	}{
		{"no input", DecisionRequest{Questions: []Question{{Type: "predicate"}}}, "input is required"},
		{"no questions", DecisionRequest{Input: json.RawMessage(`"x"`)}, "between 1 and 400"},
		{"bad type", DecisionRequest{
			Input:     json.RawMessage(`"x"`),
			Questions: []Question{{Type: "rank"}},
		}, "is not supported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := g.Decision(context.Background(), tc.req)
			var gwErr *Error
			if !errors.As(err, &gwErr) || gwErr.Status != http.StatusBadRequest {
				t.Fatalf("err = %#v", err)
			}
			if !strings.Contains(gwErr.Message, tc.want) {
				t.Fatalf("message = %q, want %q", gwErr.Message, tc.want)
			}
		})
	}
}

func TestDecisionNormalization(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/decisions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("auth = %q", got)
		}
		_, _ = w.Write([]byte(`{"results":[{"answer":"a"}],"usage":{"total_tokens":7}}`))
	}))
	defer provider.Close()

	g := New(Config{BaseURL: provider.URL, APIKey: "k", ModelDecision: "decision-x"})
	result, err := g.Decision(context.Background(), DecisionRequest{
		Input:     json.RawMessage(`{"text":"hello"}`),
		Questions: []Question{{Type: "choice", Predicate: "pick"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Answers) != `[{"answer":"a"}]` {
		t.Fatalf("answers = %s", result.Answers)
	}
	if result.Model != "decision-x" {
		t.Fatalf("model = %q", result.Model)
	}
	if string(result.Usage) != `{"total_tokens":7}` {
		t.Fatalf("usage = %s", result.Usage)
	}
}

func TestEditReferenceValidation(t *testing.T) {
	g := New(Config{BaseURL: "http://127.0.0.1:1", APIKey: "edit-key-123456"})
	cases := []struct {
		name  string
		image string
		want  string
	}{
		{"empty", "", "image is required"},
		{"data url", "data:image/png;base64,AAAA", "data URLs are not supported"},
		{"relative", "images/a.png", "must be an http(s) URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := g.Edit(context.Background(), ImageEditRequest{Prompt: "x", Image: tc.image})
			var gwErr *Error
			if !errors.As(err, &gwErr) || gwErr.Status != http.StatusBadRequest {
				t.Fatalf("err = %#v", err)
			}
			if !strings.Contains(gwErr.Message, tc.want) {
				t.Fatalf("message = %q, want %q", gwErr.Message, tc.want)
			}
		})
	}
}
