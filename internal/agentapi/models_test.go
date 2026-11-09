package agentapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/modelcfg"
)

// stubCatalog is the ModelCatalog stand-in for the hosting process: it returns
// resolved views, and deliberately includes a hidden provider (and a model of
// that provider) so the handler's own visibility boundary is exercised too.
type stubCatalog struct {
	providers        []modelcfg.ProviderView
	models           []modelcfg.ModelView
	resolutionIssues []modelcfg.Issue
	defaultSelection modelcfg.Selection
}

func (s stubCatalog) ProviderViews() []modelcfg.ProviderView { return s.providers }
func (s stubCatalog) ModelViews() []modelcfg.ModelView       { return s.models }
func (s stubCatalog) ResolutionIssues() []modelcfg.Issue     { return s.resolutionIssues }
func (s stubCatalog) DefaultSelection() modelcfg.Selection   { return s.defaultSelection }

// testViews builds one visible provider (a complete model plus an incomplete
// one that must stay in the settings view) and one hidden provider.
func testViews() ([]modelcfg.ProviderView, []modelcfg.ModelView, modelcfg.ModelView) {
	complete := modelcfg.ModelView{
		ProviderID: "s2api",
		ModelID:    "deepseek-v4.1-flash",
		API: modelcfg.API{
			Type:    modelcfg.APITypeOpenAIChatCompletions,
			BaseURL: "https://api.s2.example.test/v1",
		},
		Properties: modelcfg.Properties{
			ContextWindow:    131072,
			InputFormat:      modelcfg.MediaIn{Text: true, Image: true},
			OutputFormat:     modelcfg.MediaOut{Text: true},
			SupportsToolCall: true,
		},
		Options: modelcfg.OptionSpecs{
			ReasoningLevel: modelcfg.EnumSpec{
				Values: []string{"low", "high"},
				Map: map[string]map[string]any{
					"low":  {"reasoning_effort": "low"},
					"high": {"reasoning_effort": "high"},
				},
			},
			MaxOutputTokens: modelcfg.LimitSpec{
				Max: 8192,
				Map: map[string]map[string]any{"": {"max_tokens": float64(8192)}},
			},
		},
		Enabled:    true,
		Executable: true,
		Selectable: true,
	}
	incomplete := modelcfg.ModelView{
		ProviderID: "s2api",
		ModelID:    "half-configured",
		Enabled:    true,
		Issues: []modelcfg.Issue{{
			Field:   "options.reasoningLevel.values",
			Message: "reasoning level values must be non-empty",
		}},
	}
	hiddenModel := modelcfg.ModelView{
		ProviderID: "internal",
		ModelID:    "secret-model",
		Enabled:    true,
		Executable: true,
		Selectable: true,
	}
	providers := []modelcfg.ProviderView{
		{
			ProviderID: "s2api",
			Name:       "S2API",
			Visibility: modelcfg.VisibilityVisible,
			Access:     modelcfg.Access{Type: modelcfg.AccessTypeAPIKey, Env: "S2API_KEY"},
			Models:     []modelcfg.ModelView{complete, incomplete},
			Executable: true,
			Selectable: true,
		},
		{
			ProviderID: "internal",
			Visibility: modelcfg.VisibilityHidden,
			Models:     []modelcfg.ModelView{hiddenModel},
			Executable: true,
			Selectable: true,
		},
	}
	return providers, []modelcfg.ModelView{complete, hiddenModel}, complete
}

// TestProviderSettingsAndModelList pins the two projections: /v1/providers is
// the settings view (incomplete entries and their issues included, hidden
// providers omitted), /v1/models is the flat picker list, and neither is a
// credential carrier — the only access fact on the wire is the environment
// variable *name*.
func TestProviderSettingsAndModelList(t *testing.T) {
	providers, models, complete := testViews()
	server := httptest.NewServer(New(Config{
		Models: stubCatalog{providers: providers, models: models},
		Logger: testLogger(),
	}))
	defer server.Close()
	tenant := uuid.New()

	var raw json.RawMessage
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/providers", tenant, nil, &raw); code != 200 {
		t.Fatalf("GET /v1/providers = %d %s", code, raw)
	}
	if strings.Contains(string(raw), "internal") || strings.Contains(string(raw), "secret-model") {
		t.Fatalf("hidden provider leaked: %s", raw)
	}
	if strings.Contains(string(raw), "sk-") {
		t.Fatalf("credential-shaped value in the settings view: %s", raw)
	}
	if !strings.Contains(string(raw), `"S2API_KEY"`) {
		t.Fatalf("access env name missing from the settings view: %s", raw)
	}
	if !strings.Contains(string(raw), `"issues":[]`) {
		t.Fatalf("resolution-level issues must render as []: %s", raw)
	}
	var settings providersResponse
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if len(settings.Providers) != 1 || settings.Providers[0].ProviderID != "s2api" {
		t.Fatalf("providers = %+v", settings.Providers)
	}
	// The settings view is not a picker: the incomplete model stays, carrying
	// the issue the seam attached to it, unchanged.
	if len(settings.Providers[0].Models) != 2 {
		t.Fatalf("settings kept %d models, want both", len(settings.Providers[0].Models))
	}
	incomplete := settings.Providers[0].Models[1]
	if !reflect.DeepEqual(incomplete.Issues, []modelcfg.Issue{{
		Field:   "options.reasoningLevel.values",
		Message: "reasoning level values must be non-empty",
	}}) {
		t.Fatalf("model issues were not passed through: %+v", incomplete.Issues)
	}
	if !reflect.DeepEqual(settings.Providers[0].Models[0], complete) {
		t.Fatalf("complete model view changed on the wire:\ngot  %+v\nwant %+v", settings.Providers[0].Models[0], complete)
	}

	raw = nil
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/models", tenant, nil, &raw); code != 200 {
		t.Fatalf("GET /v1/models = %d %s", code, raw)
	}
	if strings.Contains(string(raw), "internal") || strings.Contains(string(raw), "secret-model") {
		t.Fatalf("a model of a hidden provider leaked: %s", raw)
	}
	if strings.Contains(string(raw), "sk-") {
		t.Fatalf("credential-shaped value in the model list: %s", raw)
	}
	var list modelsResponse
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Models) != 1 || list.Models[0].ModelID != "deepseek-v4.1-flash" {
		t.Fatalf("models = %+v", list.Models)
	}
	if !reflect.DeepEqual(list.Models[0], complete) {
		t.Fatalf("model view changed on the wire:\ngot  %+v\nwant %+v", list.Models[0], complete)
	}
}

// TestModelCatalogUnwiredAnswers501: without the seam both endpoints say so
// instead of pretending the configuration is empty.
func TestModelCatalogUnwiredAnswers501(t *testing.T) {
	server := httptest.NewServer(New(Config{Logger: testLogger()}))
	defer server.Close()
	for _, path := range []string{"/v1/providers", "/v1/models"} {
		var body map[string]any
		if code := doJSON(t, server.Client(), http.MethodGet, server.URL+path, uuid.New(), nil, &body); code != 501 {
			t.Fatalf("GET %s unwired = %d %v, want 501", path, code, body)
		}
		if body["status"] != "not_wired" {
			t.Fatalf("GET %s body = %v", path, body)
		}
		if message, _ := body["error"].(string); !strings.Contains(message, "not wired") {
			t.Fatalf("GET %s must name the unwired capability, got %v", path, body["error"])
		}
	}
}

// TestModelCatalogEmptyDiagnostics: an empty resolution is not an empty answer
// — the response says why it is empty and keeps the arrays as [] (a client
// iterating null would break).
func TestModelCatalogEmptyDiagnostics(t *testing.T) {
	server := httptest.NewServer(New(Config{Models: stubCatalog{}, Logger: testLogger()}))
	defer server.Close()
	tenant := uuid.New()

	var raw json.RawMessage
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/providers", tenant, nil, &raw); code != 200 {
		t.Fatalf("GET /v1/providers = %d %s", code, raw)
	}
	if !strings.Contains(string(raw), `"providers":[]`) {
		t.Fatalf("providers must render as []: %s", raw)
	}
	var settings providersResponse
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	want := modelcfg.Issue{Field: "providers", Message: "no provider configuration is loaded"}
	if len(settings.Issues) != 1 || settings.Issues[0] != want {
		t.Fatalf("providers issues = %+v, want %+v", settings.Issues, want)
	}

	raw = nil
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/models", tenant, nil, &raw); code != 200 {
		t.Fatalf("GET /v1/models = %d %s", code, raw)
	}
	if !strings.Contains(string(raw), `"models":[]`) {
		t.Fatalf("models must render as []: %s", raw)
	}
	var list modelsResponse
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Issues) != 1 || list.Issues[0] != want {
		t.Fatalf("model-list issues = %+v, want %+v", list.Issues, want)
	}
}

// TestProviderRoutesRequireTenantHeader: the two read-only endpoints sit behind
// the same tenant header as the rest of /v1; an unauthenticated call is
// rejected exactly like an existing route.
func TestProviderRoutesRequireTenantHeader(t *testing.T) {
	providers, models, _ := testViews()
	server := httptest.NewServer(New(Config{
		Models: stubCatalog{providers: providers, models: models},
		Logger: testLogger(),
	}))
	defer server.Close()
	for _, path := range []string{"/v1/providers", "/v1/models", "/v1/sessions"} {
		var body map[string]any
		code := doJSON(t, server.Client(), http.MethodGet, server.URL+path, uuid.Nil, nil, &body)
		if code != http.StatusUnauthorized {
			t.Fatalf("GET %s without X-Tenant-ID = %d %v, want 401", path, code, body)
		}
	}
}

// TestModelCatalogAllHiddenDiagnostic: a resolution whose providers are all
// hidden explains itself differently from a deployment with no provider
// configuration at all.
func TestModelCatalogAllHiddenDiagnostic(t *testing.T) {
	hidden := modelcfg.Issue{Field: "providers", Message: "every configured provider is hidden; no model can be selected"}
	server := httptest.NewServer(New(Config{
		Models: stubCatalog{resolutionIssues: []modelcfg.Issue{hidden}},
		Logger: testLogger(),
	}))
	defer server.Close()
	tenant := uuid.New()

	var raw json.RawMessage
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/providers", tenant, nil, &raw); code != 200 {
		t.Fatalf("GET /v1/providers = %d %s", code, raw)
	}
	if strings.Contains(string(raw), "no provider configuration is loaded") {
		t.Fatalf("an all-hidden configuration must not read as an unloaded one: %s", raw)
	}
	var settings providersResponse
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if len(settings.Issues) != 1 || settings.Issues[0] != hidden {
		t.Fatalf("providers issues = %+v, want the hidden diagnostic", settings.Issues)
	}

	raw = nil
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/models", tenant, nil, &raw); code != 200 {
		t.Fatalf("GET /v1/models = %d %s", code, raw)
	}
	var list modelsResponse
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Issues) != 1 || list.Issues[0] != hidden {
		t.Fatalf("model-list issues = %+v, want the hidden diagnostic", list.Issues)
	}
}

// TestModelsResponsePublishesTheEnforcementLedger pins the round-2 blocker: a
// declared capability the server does not enforce must not read like a promise,
// so the response carries the enforcement answer explicitly.
func TestModelsResponsePublishesTheEnforcementLedger(t *testing.T) {
	server := httptest.NewServer(New(Config{Models: stubCatalog{}, Logger: testLogger()}))
	defer server.Close()
	var raw json.RawMessage
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/models", uuid.New(), nil, &raw); code != 200 {
		t.Fatalf("GET /v1/models = %d %s", code, raw)
	}
	var body modelsResponse
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if !body.Enforcement.ToolCall {
		t.Fatal("the tool gate is enforced and the ledger must say so")
	}
	if body.Enforcement.Media {
		t.Fatal("the media gate is a recorded deferral; claiming enforcement would be a lie")
	}
}
