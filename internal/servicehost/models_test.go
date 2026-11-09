package servicehost

// Model service wiring: the tenant document's models slice must resolve to the
// attempt's execution identity, the views must stay consistent with it, and a
// document the resolver cannot use must fail boot/reassembly instead of being
// adopted silently.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/loadout"
	"github.com/jixoai/keel/internal/modelcfg"
)

// modelsDocument is one tenant's provider configuration: an override provider
// (tenant-gw), a hidden provider that must never surface, and a catalog
// provider reached through the builtin entry.
const modelsDocument = `{
  "providers": [
    {"id": "tenant-gw", "name": "tenant-gw",
     "api": {"type": "openai-chat-completions", "baseUrl": "https://gw.example.invalid/v1"},
     "access": {"type": "api-key", "env": "TENANT_GW_KEY"},
     "modelIds": ["deepseek-v4.1-flash"]},
    {"id": "hidden-gw", "name": "hidden-gw", "visibility": "hidden",
     "api": {"type": "openai-chat-completions", "baseUrl": "https://hidden.example.invalid/v1"},
     "access": {"type": "api-key", "env": "HIDDEN_GW_KEY"},
     "modelIds": ["secret-model"]},
    {"id": "anthropic",
     "api": {"type": "anthropic-messages", "baseUrl": "https://api.anthropic.com"},
     "access": {"type": "api-key", "env": "ANTHROPIC_API_KEY"},
     "modelIds": ["claude-sonnet-4-5"]}
  ],
  "selection": {"providerId": "tenant-gw", "modelId": "deepseek-v4.1-flash", "reasoningLevel": "high"}
}`

func testLLM(lookup map[string]string, flatKey string) LLMOptions {
	return LLMOptions{
		APIKey: flatKey,
		LookupEnv: func(name string) (string, bool) {
			value, ok := lookup[name]
			return value, ok
		},
	}
}

func newTestModels(t *testing.T, tenant host.TenantConfig, llm LLMOptions) *modelsService {
	t.Helper()
	service, err := newModelsService(tenant, llm, nil)
	if err != nil {
		t.Fatalf("newModelsService: %v", err)
	}
	return service
}

// TestModelServiceResolvesTheDocumentSelection pins the identity round trip:
// the tenant default selection resolves to the configured provider/model with
// the access environment variable as the credential.
func TestModelServiceResolvesTheDocumentSelection(t *testing.T) {
	tenant := host.TenantConfig{Models: json.RawMessage(modelsDocument)}
	service := newTestModels(t, tenant, testLLM(map[string]string{"TENANT_GW_KEY": "env-key"}, "flat-key"))

	active, err := service.Resolve(context.Background(), kernel.Scope{}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if active.ProviderID != "tenant-gw" || active.Model != "deepseek-v4.1-flash" {
		t.Fatalf("identity = %s/%s, want tenant-gw/deepseek-v4.1-flash", active.ProviderID, active.Model)
	}
	if active.APIKey != "env-key" {
		t.Fatalf("APIKey = %q, want the provider's access env value", active.APIKey)
	}
	if active.Stream == nil {
		t.Fatal("Resolve must carry the wire driver")
	}
	if active.ContextWindow <= 0 {
		t.Fatalf("ContextWindow = %d, want the resolved model budget", active.ContextWindow)
	}
	if active.ThinkingLevel != agentcore.ThinkingHigh {
		t.Fatalf("ThinkingLevel = %q, want high (the selection's reasoning level)", active.ThinkingLevel)
	}
	if active.RequestPatch["max_tokens"] == nil || active.RequestPatch["reasoning_effort"] != "high" {
		t.Fatalf("RequestPatch = %v, want the resolved request overlay", active.RequestPatch)
	}
}

// TestModelServicePerInputSelectionWins: the fact carried by the input is what
// the attempt executes (the tenant selection only applies when the fact names
// no model).
func TestModelServicePerInputSelectionWins(t *testing.T) {
	tenant := host.TenantConfig{Models: json.RawMessage(modelsDocument)}
	service := newTestModels(t, tenant, testLLM(map[string]string{
		"ANTHROPIC_API_KEY": "a-key",
		"TENANT_GW_KEY":     "gw-key",
	}, ""))

	active, err := service.Resolve(context.Background(), kernel.Scope{},
		json.RawMessage(`{"providerId":"anthropic","modelId":"claude-sonnet-4-5"}`))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if active.ProviderID != "anthropic" || active.Model != "claude-sonnet-4-5" {
		t.Fatalf("identity = %s/%s, want the input's selection", active.ProviderID, active.Model)
	}
	if active.APIKey != "a-key" {
		t.Fatalf("APIKey = %q, want the anthropic access env value", active.APIKey)
	}
	// An empty selection object falls back to the tenant default; the wire
	// request is the same one a nil fact produces.
	def, err := service.Resolve(context.Background(), kernel.Scope{}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Resolve empty object: %v", err)
	}
	if def.ProviderID != "tenant-gw" || def.Model != "deepseek-v4.1-flash" {
		t.Fatalf("empty selection = %s/%s, want the tenant default", def.ProviderID, def.Model)
	}
}

// TestModelServiceFailsClosedOnUnknownModel: an unresolvable selection is an
// error carrying modelcfg's sentinel, never a guess.
func TestModelServiceFailsClosedOnUnknownModel(t *testing.T) {
	tenant := host.TenantConfig{Models: json.RawMessage(modelsDocument)}
	service := newTestModels(t, tenant, testLLM(nil, "flat-key"))

	_, err := service.Resolve(context.Background(), kernel.Scope{}, json.RawMessage(`{"providerId":"tenant-gw","modelId":"nope"}`))
	if !errors.Is(err, modelcfg.ErrNotExecutable) {
		t.Fatalf("Resolve unknown model = %v, want modelcfg.ErrNotExecutable", err)
	}
	if err := service.ValidateSelection(json.RawMessage(`{"providerId":"tenant-gw","modelId":"nope"}`)); !errors.Is(err, modelcfg.ErrNotExecutable) {
		t.Fatalf("ValidateSelection unknown model = %v, want modelcfg.ErrNotExecutable", err)
	}
	if err := service.ValidateSelection(json.RawMessage(`"not an object"`)); err == nil {
		t.Fatal("ValidateSelection must reject a non-object selection")
	}
	if err := service.ValidateSelection(json.RawMessage(`{"providerId":"tenant-gw","modelId":"deepseek-v4.1-flash"}`)); err != nil {
		t.Fatalf("ValidateSelection of a configured model = %v", err)
	}
}

// TestModelServiceRequiresACredential: a provider without an access env value
// and without the hosting process's flat key fails at resolve time; the error
// names the access field and never a credential value.
func TestModelServiceRequiresACredential(t *testing.T) {
	tenant := host.TenantConfig{Models: json.RawMessage(modelsDocument)}
	service := newTestModels(t, tenant, testLLM(nil, ""))

	_, err := service.Resolve(context.Background(), kernel.Scope{}, nil)
	if err == nil {
		t.Fatal("Resolve without any credential must fail")
	}
	if !strings.Contains(err.Error(), "access.env") || !strings.Contains(err.Error(), "TENANT_GW_KEY") {
		t.Fatalf("error must name the access field and variable: %v", err)
	}

	// The hosting process's flat key is the fallback when the env var is absent.
	fallback := newTestModels(t, tenant, testLLM(nil, "flat-key"))
	active, err := fallback.Resolve(context.Background(), kernel.Scope{}, nil)
	if err != nil {
		t.Fatalf("Resolve with the flat key: %v", err)
	}
	if active.APIKey != "flat-key" {
		t.Fatalf("APIKey = %q, want the flat fallback", active.APIKey)
	}
}

// TestModelServiceViewsHideHiddenProviders: the API projections and the
// executable registry come from one resolution, and a hidden provider never
// leaks into either.
func TestModelServiceViewsHideHiddenProviders(t *testing.T) {
	tenant := host.TenantConfig{Models: json.RawMessage(modelsDocument)}
	service := newTestModels(t, tenant, testLLM(nil, "flat-key"))

	for _, view := range service.ProviderViews() {
		if view.ProviderID == "hidden-gw" {
			t.Fatalf("a hidden provider surfaced in the settings view: %+v", view)
		}
		if view.Visibility != modelcfg.VisibilityVisible {
			t.Fatalf("provider %s has visibility %q", view.ProviderID, view.Visibility)
		}
	}
	for _, model := range service.ModelViews() {
		if model.ModelID == "secret-model" {
			t.Fatalf("a hidden provider's model surfaced in the picker list: %+v", model)
		}
		if !model.Selectable {
			t.Fatalf("ModelViews must only carry selectable models: %+v", model)
		}
	}
	found := false
	for _, model := range service.ModelViews() {
		if model.ProviderID == "tenant-gw" && model.ModelID == "deepseek-v4.1-flash" {
			found = true
		}
	}
	if !found {
		t.Fatal("the configured model is missing from ModelViews")
	}
}

// TestModelServiceLegacyOnlyTenantKeepsTheWireModel: a tenant that only has the
// flat agent configuration resolves to exactly the model and endpoint it had
// before provider configuration existed.
func TestModelServiceLegacyOnlyTenantKeepsTheWireModel(t *testing.T) {
	tenant := host.TenantConfig{Agent: host.AgentConfig{
		Provider: "a6api",
		Model:    "deepseek-v4.1-flash",
		BaseURL:  "https://api.a6api.com/v1",
		Protocol: "openai",
	}}
	service := newTestModels(t, tenant, testLLM(nil, "flat-key"))

	active, err := service.resolveActive(nil)
	if err != nil {
		t.Fatalf("resolve legacy tenant: %v", err)
	}
	if active.ProviderID != "a6api" || active.ModelID != "deepseek-v4.1-flash" {
		t.Fatalf("identity = %s/%s, want a6api/deepseek-v4.1-flash", active.ProviderID, active.ModelID)
	}
	if active.API.Type != modelcfg.APITypeOpenAIChatCompletions {
		t.Fatalf("API type = %q, want the legacy protocol", active.API.Type)
	}
	if active.API.BaseURL != "https://api.a6api.com/v1" {
		t.Fatalf("base URL = %q, want the legacy endpoint", active.API.BaseURL)
	}
	if active.Access.Env != "A6API_KEY" {
		t.Fatalf("access env = %q, want the catalog's environment variable", active.Access.Env)
	}

	resolved, err := service.Resolve(context.Background(), kernel.Scope{}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Model != "deepseek-v4.1-flash" {
		t.Fatalf("wire model = %q, want the legacy model id (a provider name here is model_not_found)", resolved.Model)
	}
}

// TestBootWiresModelsOnlyWhenConfigured: the runner gets the resolver exactly
// when the tenant declares a provider configuration, and a document the
// resolver cannot use fails boot.
func TestBootWiresModelsOnlyWhenConfigured(t *testing.T) {
	t.Run("models document", func(t *testing.T) {
		rt := bootWithLLM(t, host.TenantConfig{Models: json.RawMessage(modelsDocument)}, testLLM(nil, "flat-key"))
		t.Cleanup(func() { rt.Close(context.Background()) })
		if rt.Models() == nil {
			t.Fatal("runtime must expose the model service")
		}
		bound, err := rt.Binding().Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		defer bound.Release(context.Background())
		if bound.Runner().Models == nil {
			t.Fatal("the runner must resolve its execution identity from the model service")
		}
		active, err := bound.Runner().Models.Resolve(context.Background(), kernel.Scope{}, nil)
		if err != nil {
			t.Fatalf("runner resolve: %v", err)
		}
		if active.Model != "deepseek-v4.1-flash" {
			t.Fatalf("resolved model = %q", active.Model)
		}
	})

	t.Run("legacy model only", func(t *testing.T) {
		// The flat configuration predates the document, so it must keep the
		// static path: resolving it through the new vocabulary would change the
		// thinking level and the request shape of a deployment that never asked
		// for either.
		tenant := host.TenantConfig{Agent: host.AgentConfig{
			Model: "deepseek-v4.1-flash", Protocol: "openai", BaseURL: "https://api.a6api.com/v1",
		}}
		rt := bootWithLLM(t, tenant, testLLM(nil, "flat-key"))
		t.Cleanup(func() { rt.Close(context.Background()) })
		if rt.Models() != nil {
			t.Fatal("a flat configuration is not a provider document")
		}
		bound, err := rt.Binding().Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		defer bound.Release(context.Background())
		if bound.Runner().Models != nil {
			t.Fatal("the flat path must keep the runner's static identity")
		}
		if got := bound.Runner().Model; got != "deepseek-v4.1-flash" {
			t.Fatalf("static wire model = %q, want the flat model id", got)
		}
	})

	t.Run("nothing configured", func(t *testing.T) {
		rt := bootWithLLM(t, host.TenantConfig{}, testLLM(nil, "flat-key"))
		t.Cleanup(func() { rt.Close(context.Background()) })
		if rt.Models() != nil {
			t.Fatal("an empty tenant must expose no model service")
		}
		bound, err := rt.Binding().Acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		defer bound.Release(context.Background())
		if bound.Runner().Models != nil {
			t.Fatal("the fake path must keep the runner's static fields")
		}
	})

	t.Run("malformed document fails boot", func(t *testing.T) {
		tenant := host.TenantConfig{Models: json.RawMessage(`{"providers":[{"id":"x","api":{"type":"carrier-pigeon"}}]}`)}
		st := kernel.NewMemStore()
		k := kernel.New(st, kernel.Options{})
		rt, err := Boot(context.Background(), Config{
			Seams:     Seams{Committer: k, Reader: k, Leases: k, External: k, Runs: fakeMirror{}, Logger: discardLogger()},
			Tenant:    tenant,
			RuntimeID: "test-runtime",
		})
		if err == nil {
			rt.Close(context.Background())
			t.Fatal("a document the resolver cannot use must fail boot")
		}
		if !strings.Contains(err.Error(), "api.type") {
			t.Fatalf("boot error must carry the resolver's diagnostic: %v", err)
		}
	})
}

// TestReloadRebuildsTheModelServiceOnDocumentChange covers the next-run rule
// for provider configuration: a changed models document produces a new model
// service (the llm descriptor's version is derived from it), an unchanged one
// is shared, and a broken one is a reassembly error.
func TestReloadRebuildsTheModelServiceOnDocumentChange(t *testing.T) {
	rt := bootWithLLM(t, host.TenantConfig{Models: json.RawMessage(modelsDocument)}, testLLM(nil, "flat-key"))
	t.Cleanup(func() { rt.Close(context.Background()) })

	first, err := rt.Binding().Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer first.Release(context.Background())
	llmBefore, _ := rt.engine.Lookup(loadout.ServiceLLM)

	// The same document must not churn the run path.
	if _, err := rt.Reload(context.Background(), host.TenantConfig{Models: json.RawMessage(modelsDocument)}, loadout.Options{Revision: 2}); err != nil {
		t.Fatalf("reload unchanged: %v", err)
	}
	if llmAfter, _ := rt.engine.Lookup(loadout.ServiceLLM); llmAfter != llmBefore {
		t.Fatal("an unchanged models document must share the llm instance")
	}

	// A changed document rebuilds it: the old pin keeps its configuration, the
	// new generation resolves the new one.
	next := strings.Replace(modelsDocument, "https://gw.example.invalid/v1", "https://gw2.example.invalid/v1", 1)
	next = strings.Replace(next, `"selection": {"providerId": "tenant-gw", "modelId": "deepseek-v4.1-flash", "reasoningLevel": "high"}`,
		`"selection": {"providerId": "tenant-gw", "modelId": "deepseek-v4.1-flash", "reasoningLevel": "low"}`, 1)
	if _, err := rt.Reload(context.Background(), host.TenantConfig{Models: json.RawMessage(next)}, loadout.Options{Revision: 3}); err != nil {
		t.Fatalf("reload changed: %v", err)
	}
	if llmAfter, _ := rt.engine.Lookup(loadout.ServiceLLM); llmAfter == llmBefore {
		t.Fatal("a changed models document must produce a new llm instance")
	}
	if got := first.Runner().Models.(*modelsService); got == rt.Models() {
		t.Fatal("the pinned generation must keep its own model service")
	}
	oldActive, err := first.Runner().Models.Resolve(context.Background(), kernel.Scope{}, nil)
	if err != nil {
		t.Fatalf("pinned resolve: %v", err)
	}
	if oldActive.ThinkingLevel != agentcore.ThinkingHigh {
		t.Fatalf("the pinned run's configuration changed under it: %q", oldActive.ThinkingLevel)
	}
	newActive, err := rt.Models().Resolve(context.Background(), kernel.Scope{}, nil)
	if err != nil {
		t.Fatalf("new resolve: %v", err)
	}
	if newActive.ThinkingLevel != agentcore.ThinkingLow {
		t.Fatalf("the new generation must resolve the new document: %q", newActive.ThinkingLevel)
	}

	// A broken document is rejected before it can be adopted.
	bad := host.TenantConfig{Models: json.RawMessage(`{"providers":[{"id":"x","api":{"type":"carrier-pigeon"}}]}`)}
	if _, err := rt.Reload(context.Background(), bad, loadout.Options{Revision: 4}); err == nil {
		t.Fatal("a broken models document must be a reassembly error")
	}
}

// bootWithLLM boots a runtime with the given tenant and llm options.
func bootWithLLM(t *testing.T, tenant host.TenantConfig, llm LLMOptions) *Runtime {
	t.Helper()
	st := kernel.NewMemStore()
	k := kernel.New(st, kernel.Options{})
	rt, err := Boot(context.Background(), Config{
		Seams:     Seams{Committer: k, Reader: k, Leases: k, External: k, Runs: fakeMirror{}, Logger: discardLogger()},
		Tenant:    tenant,
		LLM:       llm,
		RuntimeID: "test-runtime",
	})
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	return rt
}
