package mediagw

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jixoai/keel/internal/demo"
)

// demoGateway builds a gateway with no vendor configuration and an explicit demo
// switch, so the test never depends on the developer's shell.
func demoGateway(enabled bool) *Gateway {
	value := ""
	if enabled {
		value = "1"
	}
	return New(Config{Getenv: func(name string) string {
		if name == demo.Env {
			return value
		}
		return ""
	}})
}

// TestDemoGatewayOffKeepsNotConfigured pins the zero-impact state: without the
// switch every operation is the historical ErrNotConfigured and the capabilities
// report stays empty.
func TestDemoGatewayOffKeepsNotConfigured(t *testing.T) {
	g := demoGateway(false)
	if _, err := g.Generate(context.Background(), ImageRequest{Prompt: "x"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Generate err = %v, want ErrNotConfigured", err)
	}
	if _, err := g.Edit(context.Background(), ImageEditRequest{Prompt: "x", Image: "https://example.test/a.png"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Edit err = %v, want ErrNotConfigured", err)
	}
	if _, err := g.Decision(context.Background(), DecisionRequest{
		Input:     json.RawMessage(`"x"`),
		Questions: []Question{{Type: "predicate"}},
	}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Decision err = %v, want ErrNotConfigured", err)
	}
	if _, err := g.Chat(context.Background(), ChatRequest{Messages: json.RawMessage(`[{"role":"user","content":"hi"}]`)}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Chat err = %v, want ErrNotConfigured", err)
	}
	caps := g.Capabilities()
	if caps.Configured || caps.LLM || caps.Image || caps.Decision {
		t.Fatalf("capabilities = %+v, want an unconfigured report", caps)
	}
}

// TestDemoVendorConfigurationWins pins default routing: with vendor credentials
// present, an UNNAMED request goes to the vendor even when the demo switch is on,
// so enabling the demo in a working deployment changes nothing about what its own
// credentials serve.
//
// An explicit demo-* model is a different case (see
// TestDemoExplicitModelWinsOverVendor): that model is listed in the catalog, so it
// must be servable.
func TestDemoVendorConfigurationWins(t *testing.T) {
	g := New(Config{
		BaseURL: "http://127.0.0.1:1",
		APIKey:  "vendor-key",
		Getenv:  func(string) string { return "1" },
	})
	caps := g.Capabilities()
	if !caps.Configured || caps.Models.Image != DefaultModelImage {
		t.Fatalf("capabilities = %+v, want the vendor report", caps)
	}
	// The vendor endpoint is unreachable, so the call fails as a gateway error —
	// never as a demo answer.
	_, err := g.Generate(context.Background(), ImageRequest{Prompt: "x"})
	if err == nil {
		t.Fatal("Generate against the vendor must not be answered by the demo")
	}
	if errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want a gateway failure, not the not-configured state", err)
	}
}

// vendorRecorder is a minimal provider that records the paths it was called on and
// answers a fixed JSON body per path, so a test can prove the vendor was (or was
// not) reached without any network.
type vendorRecorder struct {
	mu    sync.Mutex
	paths []string
}

func (v *vendorRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	v.paths = append(v.paths, r.URL.Path)
	v.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/images/generations":
		_, _ = w.Write([]byte(`{"model":"vendor-image","data":[{"url":"https://cdn.example.test/vendor.png","width":1024,"height":1024}]}`))
	case "/images/edits":
		_, _ = w.Write([]byte(`{"model":"vendor-image","data":[{"url":"https://cdn.example.test/vendor-edit.png"}]}`))
	case "/decisions":
		_, _ = w.Write([]byte(`{"model":"vendor-decision","answers":[{"type":"predicate","probability":0.11}]}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"no such provider route"}}`))
	}
}

func (v *vendorRecorder) called() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.paths...)
}

// vendorAndDemoGateway is a gateway with BOTH a reachable vendor endpoint and the
// demo switch on: the coexistence state the catalog registers demo models in.
func vendorAndDemoGateway(t *testing.T) (*Gateway, *vendorRecorder) {
	t.Helper()
	recorder := &vendorRecorder{}
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)
	return New(Config{
		BaseURL: server.URL,
		APIKey:  "vendor-key",
		Getenv:  func(string) string { return "1" },
	}), recorder
}

// TestDemoExplicitModelWinsOverVendor pins the fix for a model the client can
// select but could not run: with vendor credentials present AND the demo switch on,
// naming a demo-* model is answered by the in-process demo provider.
func TestDemoExplicitModelWinsOverVendor(t *testing.T) {
	g, recorder := vendorAndDemoGateway(t)

	image, err := g.Generate(context.Background(), ImageRequest{
		Prompt: "x", Size: "1024x1024", N: 2, Model: demo.ModelImage,
	})
	if err != nil {
		t.Fatalf("Generate(demo-image): %v", err)
	}
	if image.Model != demo.ModelImage {
		t.Fatalf("model = %q, want %q", image.Model, demo.ModelImage)
	}
	if len(image.Assets) != 2 {
		t.Fatalf("assets = %d, want 2", len(image.Assets))
	}
	for i, asset := range image.Assets {
		// The tile repeats but the URL does not: `?i=<index>` keeps the n assets
		// addressable one by one.
		want := demo.AssetURL(demo.AssetSquare) + "?i=" + strconv.Itoa(i)
		if asset.URL != want {
			t.Fatalf("asset %d URL = %q, want %q", i, asset.URL, want)
		}
		if asset.Model != demo.ModelImage {
			t.Fatalf("asset model = %q, want %q", asset.Model, demo.ModelImage)
		}
	}

	edit, err := g.Edit(context.Background(), ImageEditRequest{
		Prompt: "warmer", Image: demo.AssetURL(demo.AssetSquare), Model: demo.ModelImage,
	})
	if err != nil {
		t.Fatalf("Edit(demo-image): %v", err)
	}
	if edit.Model != demo.ModelImage || len(edit.Assets) != 1 {
		t.Fatalf("edit = %+v, want one demo asset", edit)
	}
	if !strings.HasPrefix(edit.Assets[0].URL, "/v1/demo/assets/") {
		t.Fatalf("edit asset URL = %q, want a hosted demo asset", edit.Assets[0].URL)
	}

	decision, err := g.Decision(context.Background(), DecisionRequest{
		Input:     json.RawMessage(`"x"`),
		Questions: []Question{{Type: "predicate", Predicate: "p"}},
		Model:     demo.ModelDecision,
	})
	if err != nil {
		t.Fatalf("Decision(demo-decision): %v", err)
	}
	if decision.Model != demo.ModelDecision {
		t.Fatalf("decision model = %q, want %q", decision.Model, demo.ModelDecision)
	}
	var answers []map[string]any
	if err := json.Unmarshal(decision.Answers, &answers); err != nil {
		t.Fatalf("answers are not JSON: %v (%s)", err, decision.Answers)
	}
	if len(answers) != 1 || answers[0]["type"] != "predicate" || answers[0]["probability"] != 0.5 {
		t.Fatalf("answers = %+v, want one demo predicate answer", answers)
	}

	chat, err := g.Chat(context.Background(), ChatRequest{
		Messages: json.RawMessage(`[{"role":"user","content":"hi"}]`),
		Model:    demo.ModelLLM,
	})
	if err != nil {
		t.Fatalf("Chat(demo-llm): %v", err)
	}
	if !strings.Contains(string(chat), demo.ResponseID) {
		t.Fatalf("chat body = %s, want the fixed demo completion", chat)
	}

	if calls := recorder.called(); len(calls) != 0 {
		t.Fatalf("vendor calls = %v, want none for explicit demo models", calls)
	}
}

// TestDemoUnnamedRequestStaysVendor pins the other half of the rule: without a
// model, the vendor answers even though the demo switch is on, so the demo cannot
// hijack a deployment's default model.
func TestDemoUnnamedRequestStaysVendor(t *testing.T) {
	g, recorder := vendorAndDemoGateway(t)

	image, err := g.Generate(context.Background(), ImageRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if image.Model != "vendor-image" {
		t.Fatalf("model = %q, want the vendor model", image.Model)
	}
	if len(image.Assets) != 1 || image.Assets[0].URL != "https://cdn.example.test/vendor.png" {
		t.Fatalf("assets = %+v, want the vendor asset", image.Assets)
	}

	decision, err := g.Decision(context.Background(), DecisionRequest{
		Input:     json.RawMessage(`"x"`),
		Questions: []Question{{Type: "predicate", Predicate: "p"}},
	})
	if err != nil {
		t.Fatalf("Decision: %v", err)
	}
	if decision.Model != "vendor-decision" {
		t.Fatalf("decision model = %q, want the vendor model", decision.Model)
	}

	if calls := recorder.called(); len(calls) != 2 {
		t.Fatalf("vendor calls = %v, want the generation and decision calls", calls)
	}
}

// TestDemoModelRefusedWhenSwitchOff pins that the new routing does not widen
// access: with the switch off, a demo model id is never served.
func TestDemoModelRefusedWhenSwitchOff(t *testing.T) {
	recorder := &vendorRecorder{}
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)
	vendor := New(Config{
		BaseURL: server.URL,
		APIKey:  "vendor-key",
		Getenv:  func(string) string { return "" },
	})

	// Vendor configured: the demo id is not in the vendor allowlist, so the vendor
	// path answers its historical 400 and never calls upstream.
	_, err := vendor.Generate(context.Background(), ImageRequest{Prompt: "x", Model: demo.ModelImage})
	var gwErr *Error
	if !errors.As(err, &gwErr) || gwErr.Status != http.StatusBadRequest {
		t.Fatalf("Generate(demo-image, switch off, vendor configured) err = %#v, want a 400", err)
	}
	if !strings.Contains(gwErr.Message, "is not allowed") {
		t.Fatalf("message = %q, want the allowlist rejection", gwErr.Message)
	}
	if calls := recorder.called(); len(calls) != 0 {
		t.Fatalf("vendor calls = %v, want none for a rejected model", calls)
	}

	// No vendor configured: the historical not-configured state, not a demo answer.
	unconfigured := New(Config{Getenv: func(string) string { return "" }})
	if _, err := unconfigured.Generate(context.Background(), ImageRequest{Prompt: "x", Model: demo.ModelImage}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Generate(demo-image, switch off, no vendor) err = %v, want ErrNotConfigured", err)
	}
	if _, err := unconfigured.Decision(context.Background(), DecisionRequest{
		Input:     json.RawMessage(`"x"`),
		Questions: []Question{{Type: "predicate"}},
		Model:     demo.ModelDecision,
	}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Decision(demo-decision, switch off, no vendor) err = %v, want ErrNotConfigured", err)
	}
}

// TestDemoVendorModelStillRejectedWithDemoOn pins that naming a vendor model in a
// demo-only deployment is refused instead of being served by the demo: the demo
// answers only its own ids.
func TestDemoVendorModelStillRejectedWithDemoOn(t *testing.T) {
	g := demoGateway(true)
	_, err := g.Generate(context.Background(), ImageRequest{Prompt: "x", Model: DefaultModelImage})
	var gwErr *Error
	if !errors.As(err, &gwErr) || gwErr.Status != http.StatusBadRequest {
		t.Fatalf("Generate(vendor model, demo only) err = %#v, want a 400", err)
	}
	if !strings.Contains(gwErr.Message, "is not allowed") {
		t.Fatalf("message = %q, want the allowlist rejection", gwErr.Message)
	}
}

// TestDemoGenerateAnswersHostedAssets pins the image answer: one asset per
// requested image, a server-hosted URL chosen by the size, and the demo model id.
func TestDemoGenerateAnswersHostedAssets(t *testing.T) {
	g := demoGateway(true)
	result, err := g.Generate(context.Background(), ImageRequest{Prompt: "x", Size: "1024x1536", N: 2})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if result.Model != demo.ModelImage {
		t.Fatalf("model = %q, want %q", result.Model, demo.ModelImage)
	}
	if len(result.Assets) != 2 {
		t.Fatalf("assets = %d, want 2", len(result.Assets))
	}
	for i, asset := range result.Assets {
		// One tile per request, one URL per asset: `?i=<index>` distinguishes the n
		// members of the batch.
		want := demo.AssetURL(demo.AssetPortrait) + "?i=" + strconv.Itoa(i)
		if asset.URL != want {
			t.Fatalf("asset %d URL = %q, want %q", i, asset.URL, want)
		}
		if asset.Model != demo.ModelImage {
			t.Fatalf("asset model = %q, want %q", asset.Model, demo.ModelImage)
		}
		if asset.Width <= 0 || asset.Height <= 0 {
			t.Fatalf("asset size = %dx%d, want a positive box", asset.Width, asset.Height)
		}
	}

	// Validation is the vendor validation: a bad size or an over-large n is a 400,
	// not a silently normalized demo answer.
	for _, tc := range []struct {
		name string
		req  ImageRequest
		want string
	}{
		{"empty prompt", ImageRequest{Prompt: "  "}, "prompt is required"},
		{"bad n", ImageRequest{Prompt: "x", N: 9}, "n must be between 1 and 4"},
		{"bad size", ImageRequest{Prompt: "x", Size: "800x800"}, "size must be one of"},
		{"foreign model", ImageRequest{Prompt: "x", Model: "other-image"}, "is not allowed"},
	} {
		_, err := g.Generate(context.Background(), tc.req)
		var gwErr *Error
		if !errors.As(err, &gwErr) || gwErr.Status != http.StatusBadRequest {
			t.Fatalf("%s: err = %#v, want a 400", tc.name, err)
		}
		if !strings.Contains(gwErr.Message, tc.want) {
			t.Fatalf("%s: message = %q, want %q", tc.name, gwErr.Message, tc.want)
		}
	}
}

// TestDemoEditAnswersADifferentTile pins the edit answer: a reference to a hosted
// demo asset yields a different tile than a plain reference, so a multi-turn chain
// visibly changes image.
func TestDemoEditAnswersADifferentTile(t *testing.T) {
	g := demoGateway(true)
	fromAsset, err := g.Edit(context.Background(), ImageEditRequest{
		Prompt: "make it warmer",
		Image:  demo.AssetURL(demo.AssetSquare),
	})
	if err != nil {
		t.Fatalf("Edit(hosted asset): %v", err)
	}
	if len(fromAsset.Assets) != 1 || fromAsset.Assets[0].URL != demo.AssetURL(demo.AssetPortrait) {
		t.Fatalf("hosted-asset edit = %+v, want the portrait tile", fromAsset.Assets)
	}
	fromForeign, err := g.Edit(context.Background(), ImageEditRequest{
		Prompt: "make it warmer",
		Image:  "https://cdn.example.test/a.png",
	})
	if err != nil {
		t.Fatalf("Edit(foreign URL): %v", err)
	}
	if fromForeign.Assets[0].URL != demo.AssetURL(demo.AssetSquare) {
		t.Fatalf("foreign-reference edit = %+v, want the square tile", fromForeign.Assets)
	}
	// A data URL and a relative path are refused exactly as the vendor path does.
	for _, image := range []string{"data:image/png;base64,AAAA", "images/a.png"} {
		_, err := g.Edit(context.Background(), ImageEditRequest{Prompt: "x", Image: image})
		var gwErr *Error
		if !errors.As(err, &gwErr) || gwErr.Status != http.StatusBadRequest {
			t.Fatalf("Edit(%q) err = %#v, want a 400", image, err)
		}
	}
}

// TestDemoDecisionAnswersShape pins the three decision answers and their
// determinism.
func TestDemoDecisionAnswersShape(t *testing.T) {
	g := demoGateway(true)
	request := DecisionRequest{
		Input: json.RawMessage(`{"text":"hello"}`),
		Questions: []Question{
			{Type: "choice", Predicate: "pick", Choices: []Choice{{Value: "a"}, {Value: "b"}}},
			{Type: "predicate", Predicate: "notify?"},
			{Type: "score", Predicate: "severity", Criteria: json.RawMessage(`["low","medium","high"]`)},
		},
	}
	first, err := g.Decision(context.Background(), request)
	if err != nil {
		t.Fatalf("Decision: %v", err)
	}
	if first.Model != demo.ModelDecision {
		t.Fatalf("model = %q, want %q", first.Model, demo.ModelDecision)
	}
	var answers []map[string]any
	if err := json.Unmarshal(first.Answers, &answers); err != nil {
		t.Fatalf("answers are not JSON: %v (%s)", err, first.Answers)
	}
	if len(answers) != 3 {
		t.Fatalf("answers = %d, want 3", len(answers))
	}
	if answers[0]["type"] != "choice" || answers[0]["choice"] != "a" {
		t.Fatalf("choice answer = %+v", answers[0])
	}
	if answers[1]["type"] != "predicate" || answers[1]["probability"] != 0.5 {
		t.Fatalf("predicate answer = %+v", answers[1])
	}
	if answers[2]["type"] != "score" || answers[2]["legend"].(map[string]any)["2"] != "high" {
		t.Fatalf("score answer = %+v", answers[2])
	}

	second, err := g.Decision(context.Background(), request)
	if err != nil {
		t.Fatalf("second Decision: %v", err)
	}
	if string(first.Answers) != string(second.Answers) {
		t.Fatalf("the demo decision is not deterministic:\n%s\n%s", first.Answers, second.Answers)
	}
	if string(first.Usage) != string(second.Usage) {
		t.Fatalf("usage differs: %s vs %s", first.Usage, second.Usage)
	}
}

// TestDemoCapabilitiesReportSpeechStaysOff pins the split between the two layers:
// the gateway owns image/decision/llm, so it reports those; speech is the HTTP
// layer's projection and stays false here.
func TestDemoCapabilitiesReportSpeechStaysOff(t *testing.T) {
	caps := demoGateway(true).Capabilities()
	if !caps.Configured || !caps.LLM || !caps.Image || !caps.Decision {
		t.Fatalf("capabilities = %+v, want the demo media capabilities", caps)
	}
	if caps.Video || caps.TTS || caps.STT || caps.Realtime {
		t.Fatalf("capabilities = %+v, want the unsupported transports off", caps)
	}
	if caps.Models.Image != demo.ModelImage || caps.Models.Decision != demo.ModelDecision ||
		caps.Models.LLM != demo.ModelLLM {
		t.Fatalf("models = %+v, want the demo model names", caps.Models)
	}
}

// TestDemoChatAnswersTheFixedCompletion pins the diagnostics proxy.
func TestDemoChatAnswersTheFixedCompletion(t *testing.T) {
	g := demoGateway(true)
	raw, err := g.Chat(context.Background(), ChatRequest{
		Messages: json.RawMessage(`[{"role":"user","content":"hi"}]`),
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var body struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("chat body is not JSON: %v", err)
	}
	if body.Object != "chat.completion" || body.Model != demo.ModelLLM {
		t.Fatalf("chat body = %+v", body)
	}
	if len(body.Choices) != 1 || body.Choices[0].Message.Content != demo.ReplyText {
		t.Fatalf("chat content = %+v, want the fixed reply", body.Choices)
	}
}
