package agentapi

// The demo provider through the real registry projections: /v1/models and
// /v1/providers must present it as one ordinary provider group whose six models
// carry the family and surface the client's surface registry reads. These tests
// drive the actual modelcfg resolution, so a change to the demo catalog or to the
// projection is caught here rather than in a hand-built stub.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/demo"
	"github.com/jixoai/keel/internal/modelcfg"
)

// demoCatalogFrom builds the demo-aware catalog with an explicit switch, so the
// test never depends on the developer's shell.
func demoCatalogFrom(t *testing.T, enabled bool) *modelcfg.Catalog {
	t.Helper()
	return modelcfg.DemoCatalog(enabled)
}

// demoResolution resolves the demo-aware catalog into the projection pair the
// endpoints serve.
func demoResolution(t *testing.T, enabled bool) ([]modelcfg.ProviderView, []modelcfg.ModelView) {
	t.Helper()
	resolver, err := modelcfg.NewResolver(demoCatalogFrom(t, enabled), modelcfg.Overlay{})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	resolution := resolver.Resolve()
	return resolution.Settings(), resolution.Models()
}

// demoServer serves the AgentAPI over the demo resolution.
func demoServer(t *testing.T, providers []modelcfg.ProviderView, models []modelcfg.ModelView) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(New(Config{
		Models: stubCatalog{providers: providers, models: models},
		Logger: testLogger(),
	}))
	t.Cleanup(server.Close)
	return server
}

// TestDemoProviderProjectsSixModels pins the picker projection when the provider is
// enabled: providerId "demo", one model per family, each with the providerType and
// surfaceKind the client's registry maps onto a panel.
func TestDemoProviderProjectsSixModels(t *testing.T) {
	providers, models := demoResolution(t, true)
	server := demoServer(t, providers, models)
	tenant := uuid.New()

	var raw json.RawMessage
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/models", tenant, nil, &raw); code != 200 {
		t.Fatalf("GET /v1/models = %d %s", code, raw)
	}
	var list modelsResponse
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	picked := make(map[string]modelcfg.ModelView, len(list.Models))
	for _, model := range list.Models {
		if model.ProviderID == demo.ProviderID {
			picked[model.ModelID] = model
		}
	}
	if len(picked) != len(demo.ModelIDs()) {
		t.Fatalf("demo models in the picker = %d, want %d: %+v", len(picked), len(demo.ModelIDs()), picked)
	}
	want := map[string]struct {
		providerType, surfaceKind string
		selectable                bool
	}{
		demo.ModelLLM:      {"llm", "llm", true},
		demo.ModelImage:    {"image", "image", true},
		demo.ModelDecision: {"decision", "generic", false},
		demo.ModelRealtime: {"realtime", "realtime", true},
		demo.ModelTTS:      {"tts", "speech", true},
		demo.ModelSTT:      {"stt", "speech", true},
	}
	for modelID, expected := range want {
		model, ok := picked[modelID]
		if !ok {
			t.Fatalf("%s is missing from the picker projection", modelID)
		}
		if model.ProviderType != expected.providerType || model.SurfaceKind != expected.surfaceKind {
			t.Fatalf("%s providerType/surfaceKind = %s/%s, want %s/%s",
				modelID, model.ProviderType, model.SurfaceKind, expected.providerType, expected.surfaceKind)
		}
		if !model.Executable {
			t.Fatalf("%s must stay executable, issues: %v", modelID, model.Issues)
		}
		// The decision model travels on the wire (the settings page lists this
		// projection) but is never a session choice: selecting it would open the
		// generic "no dedicated panel" surface, a dead end.
		if model.Selectable != expected.selectable {
			t.Fatalf("%s selectable = %v, want %v", modelID, model.Selectable, expected.selectable)
		}
		if !expected.selectable && len(model.Issues) == 0 {
			t.Fatalf("%s must carry the issue that explains why it cannot be chosen", modelID)
		}
	}

	// The wire carries the facts the client branches on, with no demo-specific
	// field: the client's existing surface registry is all it needs.
	for _, want := range []string{`"providerId":"demo"`, `"providerType":"realtime"`, `"surfaceKind":"speech"`, `"audio":true`, `"image":true`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("model list missing %s: %s", want, raw)
		}
	}
}

// TestDemoProviderProjectsAsOneProviderGroup pins the settings projection: one
// visible, selectable provider named by its display name, with all six member
// models and no demo-mode field anywhere.
func TestDemoProviderProjectsAsOneProviderGroup(t *testing.T) {
	providers, models := demoResolution(t, true)
	server := demoServer(t, providers, models)
	tenant := uuid.New()

	var raw json.RawMessage
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/providers", tenant, nil, &raw); code != 200 {
		t.Fatalf("GET /v1/providers = %d %s", code, raw)
	}
	var body providersResponse
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	var found *modelcfg.ProviderView
	for i := range body.Providers {
		if body.Providers[i].ProviderID == demo.ProviderID {
			found = &body.Providers[i]
		}
	}
	if found == nil {
		t.Fatal("the demo provider is missing from the settings projection")
	}
	if found.Name != demo.ProviderName {
		t.Fatalf("name = %q, want %q", found.Name, demo.ProviderName)
	}
	if found.Visibility != modelcfg.VisibilityVisible {
		t.Fatalf("visibility = %q, want visible", found.Visibility)
	}
	if !found.Executable || !found.Selectable {
		t.Fatalf("the demo provider must be complete and selectable")
	}
	if len(found.Models) != len(demo.ModelIDs()) {
		t.Fatalf("provider models = %d, want %d", len(found.Models), len(demo.ModelIDs()))
	}
	// No credential is required, and the provider must not claim one: an access.env
	// on a credential-free provider would send the client hunting for a secret.
	if strings.TrimSpace(found.Access.Env) != "" || strings.TrimSpace(found.Access.Ref) != "" {
		t.Fatalf("demo access = %+v, want no credential reference", found.Access)
	}
	for _, ban := range []string{`"demoMode"`, `"isDemo"`, `"preview"`} {
		if strings.Contains(string(raw), ban) {
			t.Fatalf("the settings projection carries a demo-only field %s: %s", ban, raw)
		}
	}
}

// TestDemoProviderAbsentFromBothProjections pins the zero-impact half: with the
// switch off, neither endpoint mentions the demo provider or any of its models.
func TestDemoProviderAbsentFromBothProjections(t *testing.T) {
	providers, models := demoResolution(t, false)
	server := demoServer(t, providers, models)
	tenant := uuid.New()

	for _, path := range []string{"/v1/models", "/v1/providers"} {
		var raw json.RawMessage
		if code := doJSON(t, server.Client(), http.MethodGet, server.URL+path, tenant, nil, &raw); code != 200 {
			t.Fatalf("GET %s = %d %s", path, code, raw)
		}
		if strings.Contains(string(raw), `"providerId":"demo"`) {
			t.Fatalf("%s names the demo provider while it is disabled: %s", path, raw)
		}
		for _, modelID := range demo.ModelIDs() {
			if strings.Contains(string(raw), `"`+modelID+`"`) {
				t.Fatalf("%s lists %s while the demo provider is disabled: %s", path, modelID, raw)
			}
		}
	}

	// The two projections agree: the flat list is exactly the selectable models of
	// the visible providers, so a disabled demo model cannot leak through either.
	var list modelsResponse
	var raw json.RawMessage
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/models", tenant, nil, &raw); code != 200 {
		t.Fatalf("GET /v1/models = %d", code)
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Models) != len(models) {
		t.Fatalf("models = %d, want %d", len(list.Models), len(models))
	}
}
