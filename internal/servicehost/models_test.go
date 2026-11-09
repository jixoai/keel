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
	// Nobody named this model in a rule, so its context window is the catalog
	// baseline's 128k invention: the attempt withholds it so the runner keeps
	// the tenant's compaction window authoritative.
	if active.ContextWindow != 0 {
		t.Fatalf("ContextWindow = %d, want 0 for a baseline-derived model", active.ContextWindow)
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

// TestModelServiceRequiresACredential: the tenant default must be executable at
// construction, and a provider without an access env value (and without the
// hosting process's flat key) fails boot; the error names the access field and
// never a credential value.
func TestModelServiceRequiresACredential(t *testing.T) {
	tenant := host.TenantConfig{Models: json.RawMessage(modelsDocument)}
	_, err := newModelsService(tenant, testLLM(nil, ""), nil)
	if err == nil {
		t.Fatal("a default selection without a resolvable credential must fail construction")
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

// TestModelServiceSetButEmptyEnvFailsClosed: an access.env variable that is set
// but empty is a deliberate empty value, not a missing one: the attempt must
// refuse it instead of silently sending the process's other key.
func TestModelServiceSetButEmptyEnvFailsClosed(t *testing.T) {
	doc := json.RawMessage(`{"providers":[{"id":"e-gw","api":{"type":"openai-chat-completions","baseUrl":"https://e.example/v1"},
	"access":{"type":"api-key","env":"EMPTY_KEY"},"modelIds":["m1"]}]}`)
	selection := json.RawMessage(`{"providerId":"e-gw","modelId":"m1"}`)

	service := newTestModels(t, host.TenantConfig{Models: doc}, testLLM(map[string]string{"EMPTY_KEY": ""}, "flat-key"))
	_, err := service.Resolve(context.Background(), kernel.Scope{}, selection)
	if err == nil {
		t.Fatal("a set-but-empty access.env must not fall back to another credential")
	}
	if !strings.Contains(err.Error(), "EMPTY_KEY") || !strings.Contains(err.Error(), "set but empty") {
		t.Fatalf("error must name the variable and the reason: %v", err)
	}
	if strings.Contains(err.Error(), "flat-key") {
		t.Fatalf("the error must never carry a credential value: %v", err)
	}
	if err := service.ValidateSelection(selection); err == nil {
		t.Fatal("ValidateSelection must refuse the same shape")
	}

	// The same provider with the variable unset falls back to the process key:
	// missing is not the same as deliberately empty.
	fallback := newTestModels(t, host.TenantConfig{Models: doc}, testLLM(nil, "flat-key"))
	active, err := fallback.Resolve(context.Background(), kernel.Scope{}, selection)
	if err != nil {
		t.Fatalf("Resolve with the process key: %v", err)
	}
	if active.APIKey != "flat-key" {
		t.Fatalf("APIKey = %q, want the process key", active.APIKey)
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

// TestModelsConfiguredIsSemantic pins the static-path boundary at the service
// level: a document that only differs from nothing by its bytes must not switch
// a flat deployment onto the resolver path.
func TestModelsConfiguredIsSemantic(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "empty bytes", raw: "", want: false},
		{name: "empty object", raw: `{}`, want: false},
		{name: "spaced object", raw: `{ }`, want: false},
		{name: "newline object", raw: "{\n}", want: false},
		{name: "null", raw: `null`, want: false},
		{name: "schema version only", raw: `{"schemaVersion":1}`, want: false},
		{name: "null selection", raw: `{"selection":null}`, want: false},
		{name: "empty providers", raw: `{"providers":[]}`, want: true},
		{name: "selection", raw: `{"selection":{"modelId":"m"}}`, want: true},
		{name: "rules", raw: `{"modelRules":[{"match":"*","config":{}}]}`, want: true},
		{name: "providers", raw: `{"providers":[{"id":"x"}]}`, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := modelsConfigured(host.TenantConfig{Models: json.RawMessage(test.raw)})
			if got != test.want {
				t.Fatalf("modelsConfigured(%q) = %v, want %v", test.raw, got, test.want)
			}
		})
	}
}

// TestSpacedEmptyDocumentKeepsTheStaticPath: `{ }` must behave like no document
// at all — no model service, no request overlay, the runner keeps its static
// identity.
func TestSpacedEmptyDocumentKeepsTheStaticPath(t *testing.T) {
	tenant := host.TenantConfig{
		Agent:  host.AgentConfig{Model: "deepseek-v4.1-flash", BaseURL: "https://api.a6api.com/v1", Protocol: "openai"},
		Models: json.RawMessage(`{ }`),
	}
	rt := bootWithLLM(t, tenant, testLLM(nil, "flat-key"))
	t.Cleanup(func() { rt.Close(context.Background()) })
	if rt.Models() != nil {
		t.Fatal("a `{ }` document must not activate the resolver path")
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
		t.Fatalf("static wire model = %q", got)
	}
}

// TestModelServiceHiddenProviderIsRefused: the API surface refuses a hidden
// provider's identity at submit time and at attempt time, and the refusal never
// mentions another hidden provider.
func TestModelServiceHiddenProviderIsRefused(t *testing.T) {
	tenant := host.TenantConfig{Models: json.RawMessage(modelsDocument)}
	service := newTestModels(t, tenant, testLLM(map[string]string{"HIDDEN_GW_KEY": "h-key"}, "flat-key"))

	selection := json.RawMessage(`{"providerId":"hidden-gw","modelId":"secret-model"}`)
	if err := service.ValidateSelection(selection); err == nil {
		t.Fatal("ValidateSelection must refuse a hidden provider")
	}
	if _, err := service.Resolve(context.Background(), kernel.Scope{}, selection); err == nil {
		t.Fatal("Resolve must refuse a hidden provider")
	}
	if _, err := service.Resolve(context.Background(), kernel.Scope{}, json.RawMessage(`{"modelId":"shared-model"}`)); err == nil {
		t.Fatal("a hidden-only model id must not resolve")
	}
}

// TestModelServiceRefCredentials: access.ref is resolved through the injected
// lookup with the host's vocabulary, and an explicit ref never falls back to
// another credential.
func TestModelServiceRefCredentials(t *testing.T) {
	docFor := func(ref string) string {
		return `{"providers":[{"id":"ref-gw","api":{"type":"openai-chat-completions","baseUrl":"https://ref.example/v1"},
		"access":{"type":"api-key","ref":"` + ref + `"},"modelIds":["m1"]}]}`
	}
	selection := json.RawMessage(`{"providerId":"ref-gw","modelId":"m1"}`)
	for _, ref := range []string{"env:REF_KEY", "${REF_KEY}", "secret:REF_KEY"} {
		t.Run(ref, func(t *testing.T) {
			service := newTestModels(t, host.TenantConfig{Models: json.RawMessage(docFor(ref))},
				testLLM(map[string]string{"REF_KEY": "ref-value"}, "flat-key"))
			active, err := service.Resolve(context.Background(), kernel.Scope{}, selection)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if active.APIKey != "ref-value" {
				t.Fatalf("APIKey = %q, want the reference's value", active.APIKey)
			}
		})
	}

	t.Run("unresolvable ref never falls back", func(t *testing.T) {
		service := newTestModels(t, host.TenantConfig{Models: json.RawMessage(docFor("env:MISSING_REF"))},
			testLLM(nil, "flat-key"))
		_, err := service.Resolve(context.Background(), kernel.Scope{}, selection)
		if err == nil {
			t.Fatal("an explicit ref that cannot resolve must fail, not use the flat key")
		}
		if !strings.Contains(err.Error(), "access.ref") || !strings.Contains(err.Error(), "MISSING_REF") {
			t.Fatalf("error must name the reference: %v", err)
		}
	})

	t.Run("literal ref fails construction", func(t *testing.T) {
		_, err := newModelsService(host.TenantConfig{Models: json.RawMessage(docFor("hunter2-literal-secret"))},
			testLLM(nil, "flat-key"), nil)
		if err == nil {
			t.Fatal("a literal credential in access.ref must fail construction")
		}
		if !strings.Contains(err.Error(), "access.ref") || !strings.Contains(err.Error(), "secret reference") {
			t.Fatalf("error must name the field and the vocabulary: %v", err)
		}
	})
}

// TestModelServiceProviderWithoutModelIsRefused: a fact that names a provider
// but no model is refused instead of silently executing the tenant default.
func TestModelServiceProviderWithoutModelIsRefused(t *testing.T) {
	tenant := host.TenantConfig{Models: json.RawMessage(modelsDocument)}
	service := newTestModels(t, tenant, testLLM(nil, "flat-key"))

	selection := json.RawMessage(`{"providerId":"anthropic"}`)
	if err := service.ValidateSelection(selection); err == nil {
		t.Fatal("providerId without modelId must be refused")
	} else if !strings.Contains(err.Error(), "modelId") {
		t.Fatalf("refusal must name modelId: %v", err)
	}
	if _, err := service.Resolve(context.Background(), kernel.Scope{}, selection); err == nil {
		t.Fatal("Resolve must refuse providerId without modelId")
	}
}

// TestModelServicePartialSelectionMergesTheLevel: a fact that only narrows the
// reasoning level keeps the default model and takes the level.
func TestModelServicePartialSelectionMergesTheLevel(t *testing.T) {
	tenant := host.TenantConfig{Models: json.RawMessage(modelsDocument)}
	service := newTestModels(t, tenant, testLLM(nil, "flat-key"))

	active, err := service.Resolve(context.Background(), kernel.Scope{}, json.RawMessage(`{"reasoningLevel":"off"}`))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if active.ProviderID != "tenant-gw" || active.Model != "deepseek-v4.1-flash" {
		t.Fatalf("identity = %s/%s, want the tenant default", active.ProviderID, active.Model)
	}
	if active.ThinkingLevel != agentcore.ThinkingOff {
		t.Fatalf("ThinkingLevel = %q, want the fact's level", active.ThinkingLevel)
	}

	// The alias spelling widens the same way.
	aliased, err := service.Resolve(context.Background(), kernel.Scope{}, json.RawMessage(`{"options":{"reasoningLevel":"low"}}`))
	if err != nil {
		t.Fatalf("Resolve alias: %v", err)
	}
	if aliased.ThinkingLevel != agentcore.ThinkingLow {
		t.Fatalf("alias ThinkingLevel = %q, want low", aliased.ThinkingLevel)
	}
}

// TestModelServiceValidateSelectionMatchesExecution: every shape the settings
// view would present as executable must pass ValidateSelection, and every shape
// an attempt denies must be refused at submit time.
func TestModelServiceValidateSelectionMatchesExecution(t *testing.T) {
	headerDoc := `{"providers":[{"id":"hdr-gw","api":{"type":"openai-chat-completions","baseUrl":"https://hdr.example/v1",
	"headers":{"x-api-version":"2024-10-01"}},"access":{"type":"api-key","env":"HDR_KEY"},"modelIds":["m1"]}]}`
	service := newTestModels(t, host.TenantConfig{Models: json.RawMessage(headerDoc)}, testLLM(map[string]string{"HDR_KEY": "k"}, ""))
	selection := json.RawMessage(`{"providerId":"hdr-gw","modelId":"m1"}`)
	if err := service.ValidateSelection(selection); err == nil {
		t.Fatal("ValidateSelection accepted a provider whose driver cannot carry api.headers")
	}
	if _, err := service.Resolve(context.Background(), kernel.Scope{}, selection); err == nil {
		t.Fatal("the attempt must deny the same selection")
	}
	for _, view := range service.ProviderViews() {
		if view.ProviderID == "hdr-gw" && view.Executable {
			t.Fatalf("the settings view must not present a header provider as executable: %#v", view)
		}
	}
	for _, model := range service.ModelViews() {
		if model.ProviderID == "hdr-gw" {
			t.Fatalf("the picker must not offer a model that cannot run: %#v", model)
		}
	}

	turboDoc := `{"providers":[{"id":"t-gw","api":{"type":"openai-chat-completions","baseUrl":"https://t.example/v1"},
	"access":{"type":"api-key","env":"T_KEY"},"modelIds":["m1"]}],
	"providerModelRules":[{"providerId":"t-gw","modelId":"m1","config":{"options":{"reasoningLevel":
	{"values":["off","turbo"],"map":{"off":{},"turbo":{"reasoning_effort":"turbo"}}}}}}]}`
	turbo := newTestModels(t, host.TenantConfig{Models: json.RawMessage(turboDoc)}, testLLM(map[string]string{"T_KEY": "k"}, ""))
	if err := turbo.ValidateSelection(json.RawMessage(`{"providerId":"t-gw","modelId":"m1","reasoningLevel":"turbo"}`)); err == nil {
		t.Fatal("ValidateSelection accepted a reasoning level the runtime cannot interpret")
	}
	if err := turbo.ValidateSelection(json.RawMessage(`{"providerId":"t-gw","modelId":"m1","reasoningLevel":"off"}`)); err == nil {
		t.Fatal("the model itself declared a non-runtime level, so no level of it may execute")
	}
	for _, model := range turbo.ModelViews() {
		if model.ProviderID == "t-gw" {
			t.Fatalf("the picker must not offer a model with a non-runtime level: %#v", model)
		}
	}
}

// TestModelServiceValidatesTheDefaultSelectionAtBoot: a default selection this
// generation cannot execute fails construction, not the first attempt.
func TestModelServiceValidatesTheDefaultSelectionAtBoot(t *testing.T) {
	docFor := func(selection string) json.RawMessage {
		return json.RawMessage(`{"providers":[{"id":"d-gw","api":{"type":"openai-chat-completions","baseUrl":"https://d.example/v1"},
		"access":{"type":"api-key","env":"D_KEY"},"modelIds":["m1","disabled-model"]}],
		"providerModelRules":[{"providerId":"d-gw","modelId":"disabled-model","config":{"enabled":false}}],
		"selection":` + selection + `}`)
	}
	llm := testLLM(map[string]string{"D_KEY": "k"}, "")
	for _, test := range []struct {
		name      string
		selection string
		wantErr   string
	}{
		{name: "typo'd model", selection: `{"providerId":"d-gw","modelId":"typo"}`, wantErr: "typo"},
		{name: "disabled model", selection: `{"providerId":"d-gw","modelId":"disabled-model"}`, wantErr: "disabled"},
		{name: "provider without model", selection: `{"providerId":"d-gw"}`, wantErr: "modelId"},
		{name: "unknown provider", selection: `{"providerId":"nope","modelId":"m1"}`, wantErr: "unknown provider"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := newModelsService(host.TenantConfig{Models: docFor(test.selection)}, llm, nil)
			if err == nil {
				t.Fatalf("default selection %s must fail construction", test.selection)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, test.wantErr)
			}
		})
	}

	t.Run("legal default", func(t *testing.T) {
		if _, err := newModelsService(host.TenantConfig{Models: docFor(`{"providerId":"d-gw","modelId":"m1"}`)}, llm, nil); err != nil {
			t.Fatalf("a legal default selection must boot: %v", err)
		}
	})
}

// TestModelServiceBaselineWindowIsWithheld: the catalog baseline invents a
// context window for models nobody described; the attempt must not hand that
// number to the compaction path, while a declared window still travels.
func TestModelServiceBaselineWindowIsWithheld(t *testing.T) {
	doc := `{"providers":[{"id":"w-gw","api":{"type":"openai-chat-completions","baseUrl":"https://w.example/v1"},
	"access":{"type":"api-key","env":"W_KEY"},"modelIds":["described","undescribed"]}],
	"providerModelRules":[{"providerId":"w-gw","modelId":"described","config":{"properties":{"contextWindow":65536}}}]}`
	service := newTestModels(t, host.TenantConfig{Models: json.RawMessage(doc)}, testLLM(map[string]string{"W_KEY": "k"}, ""))

	described, err := service.Resolve(context.Background(), kernel.Scope{}, json.RawMessage(`{"providerId":"w-gw","modelId":"described"}`))
	if err != nil {
		t.Fatalf("Resolve described: %v", err)
	}
	if described.ContextWindow != 65536 {
		t.Fatalf("ContextWindow = %d, want the declared 65536", described.ContextWindow)
	}
	undescribed, err := service.Resolve(context.Background(), kernel.Scope{}, json.RawMessage(`{"providerId":"w-gw","modelId":"undescribed"}`))
	if err != nil {
		t.Fatalf("Resolve undescribed: %v", err)
	}
	if undescribed.ContextWindow != 0 {
		t.Fatalf("ContextWindow = %d, want 0 for a baseline-derived model", undescribed.ContextWindow)
	}
}

// TestModelServicePickerRequiresAResolvableCredential: the picker list only
// offers what this process can authenticate; the settings view keeps the
// provider with an issue explaining why it cannot be chosen.
func TestModelServicePickerRequiresAResolvableCredential(t *testing.T) {
	doc := json.RawMessage(`{"providers":[{"id":"n-gw","api":{"type":"openai-chat-completions","baseUrl":"https://n.example/v1"},
	"access":{"type":"api-key","env":"NOT_SET_ANYWHERE"},"modelIds":["m1"]}]}`)

	t.Run("unresolvable", func(t *testing.T) {
		service := newTestModels(t, host.TenantConfig{Models: doc}, testLLM(nil, ""))
		if models := service.ModelViews(); len(models) != 0 {
			t.Fatalf("the picker must offer nothing without a credential: %#v", models)
		}
		found := false
		for _, view := range service.ProviderViews() {
			if view.ProviderID != "n-gw" {
				continue
			}
			found = true
			if len(view.Issues) == 0 {
				t.Fatalf("the settings view must explain why the provider cannot be chosen: %#v", view)
			}
		}
		if !found {
			t.Fatal("the settings view must keep the provider visible")
		}
	})

	t.Run("env resolvable", func(t *testing.T) {
		service := newTestModels(t, host.TenantConfig{Models: doc}, testLLM(map[string]string{"NOT_SET_ANYWHERE": "k"}, ""))
		found := false
		for _, model := range service.ModelViews() {
			if model.ProviderID == "n-gw" && model.ModelID == "m1" {
				found = true
			}
		}
		if !found {
			t.Fatal("a resolvable environment variable must put the model in the picker")
		}
	})

	t.Run("flat key fallback", func(t *testing.T) {
		service := newTestModels(t, host.TenantConfig{Models: doc}, testLLM(nil, "flat-key"))
		found := false
		for _, model := range service.ModelViews() {
			if model.ProviderID == "n-gw" && model.ModelID == "m1" {
				found = true
			}
		}
		if !found {
			t.Fatal("an unset variable with a process key present must put the model in the picker")
		}
	})
}

// TestModelServiceAllHiddenProvidersGetTheirOwnDiagnostic: a configuration
// whose providers are all hidden is not "no provider configuration is loaded".
func TestModelServiceAllHiddenProvidersGetTheirOwnDiagnostic(t *testing.T) {
	hidden := make([]string, 0, 7)
	for _, id := range []string{"s2api", "a6api", "deepseek", "anthropic", "openai", "openrouter", "ollama"} {
		hidden = append(hidden, `{"id":"`+id+`","visibility":"hidden"}`)
	}
	doc := json.RawMessage(`{"providers":[` + strings.Join(hidden, ",") + `]}`)
	service, err := newModelsService(host.TenantConfig{Models: doc}, testLLM(nil, ""), nil)
	if err != nil {
		t.Fatalf("newModelsService: %v", err)
	}
	if views := service.ProviderViews(); len(views) != 0 {
		t.Fatalf("hidden providers must not surface: %d views", len(views))
	}
	if models := service.ModelViews(); len(models) != 0 {
		t.Fatalf("hidden providers must not offer models: %d models", len(models))
	}
	issues := service.ResolutionIssues()
	if len(issues) == 0 {
		t.Fatal("an all-hidden configuration must carry its own diagnostic")
	}
	if !strings.Contains(issues[0].Message, "hidden") {
		t.Fatalf("diagnostic must say what happened: %#v", issues)
	}
}

// TestModelServiceUnknownSelectionKeysRefused: the submit gate keeps unknown
// and duplicate keys out of the stored fact, and accepts both documented
// spellings of the reasoning level.
func TestModelServiceUnknownSelectionKeysRefused(t *testing.T) {
	tenant := host.TenantConfig{Models: json.RawMessage(modelsDocument)}
	service := newTestModels(t, tenant, testLLM(nil, "flat-key"))

	for _, raw := range []string{
		`{"providerId":"tenant-gw","modelId":"deepseek-v4.1-flash","baseUrl":"https://evil.example"}`,
		`{"modelId":"deepseek-v4.1-flash","modelId":"deepseek-v4.1-flash"}`,
		`{"modelId":"deepseek-v4.1-flash","options":{"temperature":1}}`,
	} {
		if err := service.ValidateSelection(json.RawMessage(raw)); err == nil {
			t.Fatalf("ValidateSelection(%s) must be refused", raw)
		}
	}
	for _, raw := range []string{
		`{"providerId":"tenant-gw","modelId":"deepseek-v4.1-flash","reasoningLevel":"low"}`,
		`{"providerId":"tenant-gw","modelId":"deepseek-v4.1-flash","options":{"reasoningLevel":"low"}}`,
		`{"reasoningLevel":"low"}`,
	} {
		if err := service.ValidateSelection(json.RawMessage(raw)); err != nil {
			t.Fatalf("ValidateSelection(%s) must be accepted: %v", raw, err)
		}
	}
}

// TestModelServiceAdvertisesTheDocumentIdentity: a document-driven deployment
// must not present itself as the fake provider in its logs, telemetry and
// loadout summary.
func TestModelServiceAdvertisesTheDocumentIdentity(t *testing.T) {
	rt := bootWithLLM(t, host.TenantConfig{Models: json.RawMessage(modelsDocument)},
		testLLM(map[string]string{"TENANT_GW_KEY": "k"}, ""))
	t.Cleanup(func() { rt.Close(context.Background()) })

	if got := rt.ProviderName(); got != "tenant-gw" {
		t.Fatalf("advertised provider = %q, want the document's default selection", got)
	}
	if got := rt.Model(); got != "deepseek-v4.1-flash" {
		t.Fatalf("advertised model = %q, want the document's default selection", got)
	}
}

// TestModelServiceUsesOnlyTheInjectedLookup: the service must never read the
// process environment behind the host's back — a nil lookup resolves nothing,
// whatever the real environment says.
func TestModelServiceUsesOnlyTheInjectedLookup(t *testing.T) {
	t.Setenv("TENANT_GW_KEY", "from-the-real-environment")
	tenant := host.TenantConfig{Models: json.RawMessage(modelsDocument)}

	if _, err := newModelsService(tenant, LLMOptions{}, nil); err == nil {
		t.Fatal("a nil lookup must resolve nothing even when the process environment has the variable")
	}

	// The flat key is not a lookup either: it is an explicit injection.
	service := newTestModels(t, tenant, testLLM(nil, "flat-key"))
	active, err := service.Resolve(context.Background(), kernel.Scope{}, nil)
	if err != nil {
		t.Fatalf("Resolve with the injected flat key: %v", err)
	}
	if active.APIKey != "flat-key" {
		t.Fatalf("APIKey = %q, want the injected flat key", active.APIKey)
	}
}
