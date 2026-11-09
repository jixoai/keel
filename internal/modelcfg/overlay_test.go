package modelcfg

import (
	"strings"
	"testing"
)

func TestLegacyFlatConfigNormalization(t *testing.T) {
	cat := EmbeddedBuiltin()
	tests := []struct {
		name           string
		legacy         LegacyAgent
		wantProviders  int
		wantProviderID string
		wantModelID    string
		wantAPIType    string
		wantBaseURL    string
		wantEnv        string
		wantRef        string
	}{
		{
			name: "explicit openai protocol",
			legacy: LegacyAgent{
				Model:     "deepseek-v4.1-flash",
				BaseURL:   "https://legacy.example/v1",
				Protocol:  "openai",
				APIKeyRef: "secret:LEGACY_KEY",
			},
			wantProviders:  1,
			wantProviderID: "legacy",
			wantModelID:    "deepseek-v4.1-flash",
			wantAPIType:    APITypeOpenAIChatCompletions,
			wantBaseURL:    "https://legacy.example/v1",
			wantRef:        "secret:LEGACY_KEY",
		},
		{
			name: "anthropic protocol",
			legacy: LegacyAgent{
				Model:     "claude-test",
				BaseURL:   "https://proxy.example",
				Protocol:  "anthropic",
				APIKeyRef: "env:ANTHROPIC_KEY",
			},
			wantProviders:  1,
			wantProviderID: "legacy",
			wantModelID:    "claude-test",
			wantAPIType:    APITypeAnthropicMessages,
			wantBaseURL:    "https://proxy.example",
			wantRef:        "env:ANTHROPIC_KEY",
		},
		{
			name: "responses protocol",
			legacy: LegacyAgent{
				Model:    "deepseek-v4.1-flash",
				BaseURL:  "https://proxy.example/v1",
				Protocol: "openai/resp_api",
			},
			wantProviders:  1,
			wantProviderID: "legacy",
			wantModelID:    "deepseek-v4.1-flash",
			wantAPIType:    APITypeOpenAIResponses,
			wantBaseURL:    "https://proxy.example/v1",
			wantEnv:        "DEEPSEEK_API_KEY",
		},
		{
			name: "provider defaults from catalog",
			legacy: LegacyAgent{
				Provider: "deepseek",
				Model:    "deepseek-chat",
			},
			wantProviders:  1,
			wantProviderID: "deepseek",
			wantModelID:    "deepseek-chat",
			wantAPIType:    APITypeOpenAIChatCompletions,
			wantBaseURL:    "https://api.deepseek.com/v1",
			wantEnv:        "DEEPSEEK_API_KEY",
		},
		{
			name: "provider id is normalized",
			legacy: LegacyAgent{
				Provider: "DeepSeek",
				Model:    "deepseek-chat",
			},
			wantProviders:  1,
			wantProviderID: "deepseek",
			wantModelID:    "deepseek-chat",
			wantAPIType:    APITypeOpenAIChatCompletions,
			wantBaseURL:    "https://api.deepseek.com/v1",
			wantEnv:        "DEEPSEEK_API_KEY",
		},
		{
			name: "model defaults from catalog",
			legacy: LegacyAgent{
				Model: "claude-sonnet-4-5",
			},
			wantProviders:  1,
			wantProviderID: "legacy",
			wantModelID:    "claude-sonnet-4-5",
			wantAPIType:    APITypeAnthropicMessages,
			wantBaseURL:    "https://api.anthropic.com",
			wantEnv:        "ANTHROPIC_API_KEY",
		},
		{
			name: "unknown provider derives env",
			legacy: LegacyAgent{
				Provider: "custom-gateway",
				Model:    "custom-model",
				BaseURL:  "https://custom.example/v1",
				Protocol: "openai",
			},
			wantProviders:  1,
			wantProviderID: "custom-gateway",
			wantModelID:    "custom-model",
			wantAPIType:    APITypeOpenAIChatCompletions,
			wantBaseURL:    "https://custom.example/v1",
			wantEnv:        "CUSTOM_GATEWAY_API_KEY",
		},
		{
			name:          "empty legacy leaves catalog untouched",
			legacy:        LegacyAgent{},
			wantProviders: 7,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			overlay, err := OverlayFromDocument(nil, test.legacy)
			if err != nil {
				t.Fatalf("OverlayFromDocument: %v", err)
			}
			resolution := resolveFrom(t, cat, overlay)
			// The embedded catalog stays present; a legacy provider with a new id
			// is appended, one whose id matches a catalog entry is a sparse patch.
			base := resolveFrom(t, cat, Overlay{})
			want := len(base.Providers)
			known := false
			for _, provider := range base.Providers {
				if provider.ProviderID == test.wantProviderID {
					known = true
				}
			}
			if test.wantProviderID != "" && !known {
				want++
			}
			if len(resolution.Providers) != want {
				t.Fatalf("providers = %d, want %d", len(resolution.Providers), want)
			}
			if test.wantProviderID == "" {
				if selection := overlay.Selection(); selection.ModelID != "" {
					t.Fatalf("empty legacy produced selection %#v", selection)
				}
				return
			}
			provider := mustFindProvider(t, resolution, test.wantProviderID)
			if provider.API.Type != test.wantAPIType {
				t.Fatalf("api.type = %q, want %q", provider.API.Type, test.wantAPIType)
			}
			if provider.API.BaseURL != test.wantBaseURL {
				t.Fatalf("baseUrl = %q, want %q", provider.API.BaseURL, test.wantBaseURL)
			}
			if provider.Access.Env != test.wantEnv {
				t.Fatalf("access.env = %q, want %q", provider.Access.Env, test.wantEnv)
			}
			if provider.Access.Ref != test.wantRef {
				t.Fatalf("access.ref = %q, want %q", provider.Access.Ref, test.wantRef)
			}
			if !provider.Executable {
				t.Fatalf("legacy provider must be executable, issues: %v", issueMessages(provider.Issues))
			}
			model := mustFindModel(t, resolution, test.wantProviderID, test.wantModelID)
			if !model.Selectable {
				t.Fatalf("legacy model must be selectable, issues: %v", issueMessages(model.Issues))
			}
			selection := overlay.Selection()
			if selection.ProviderID != test.wantProviderID || selection.ModelID != test.wantModelID {
				t.Fatalf("selection = %#v", selection)
			}

			factory := mustFactory(t, resolution)
			active, err := factory.Resolve(selection)
			if err != nil {
				t.Fatalf("factory resolve legacy selection: %v", err)
			}
			if active.API.BaseURL != test.wantBaseURL || active.API.Type != test.wantAPIType {
				t.Fatalf("active api = %#v", active.API)
			}
		})
	}
}

func TestOverlaySelectionAcceptsFlatAndNestedShape(t *testing.T) {
	overlay := mustOverlay(t, map[string]any{
		"selection": map[string]any{
			"providerId": "s2api",
			"modelId":    "deepseek-v4.1-flash",
			"options":    map[string]any{"reasoningLevel": "high"},
		},
	}, LegacyAgent{})
	selection := overlay.Selection()
	if selection.ProviderID != "s2api" || selection.ModelID != "deepseek-v4.1-flash" || selection.ReasoningLevel != "high" {
		t.Fatalf("selection = %#v", selection)
	}
}

func TestDocumentProvidersSuppressLegacyNormalization(t *testing.T) {
	cat := EmbeddedBuiltin()
	overlay := mustOverlay(t, map[string]any{
		"providers": []any{
			testProvider("custom", APITypeOpenAIChatCompletions, "https://custom.example/v1", "CUSTOM_KEY", "custom-model"),
		},
	}, LegacyAgent{Provider: "deepseek", Model: "deepseek-chat"})
	resolution := resolveFrom(t, cat, overlay)
	if custom := mustFindProvider(t, resolution, "custom"); custom.API.BaseURL != "https://custom.example/v1" {
		t.Fatalf("tenant provider = %#v", custom)
	}
	// A tenant provider document suppresses legacy normalization: the catalog's
	// deepseek entry must keep its own endpoint instead of being patched by the
	// legacy flat fields.
	if deepseek := mustFindProvider(t, resolution, "deepseek"); deepseek.API.BaseURL != "https://api.deepseek.com/v1" {
		t.Fatalf("legacy normalization ran despite a tenant provider document: %q", deepseek.API.BaseURL)
	}
}

// TestDocumentRulesKeepLegacyNormalization: rule sets alone do not suppress the
// legacy path. A rules-only document (no providers) still normalizes the flat
// agent fields into one provider and the default selection — the documented
// incremental-adoption path — and the tenant rule applies on top.
func TestDocumentRulesKeepLegacyNormalization(t *testing.T) {
	cat := EmbeddedBuiltin()
	overlay := mustOverlay(t, map[string]any{
		"providerModelRules": []any{
			map[string]any{
				"providerId": "s2api",
				"modelId":    "deepseek-v4.1-flash",
				"config":     map[string]any{"enabled": false},
			},
		},
	}, LegacyAgent{Model: "deepseek-v4.1-flash"})
	resolution := resolveFrom(t, cat, overlay)
	if len(resolution.Providers) != 8 {
		t.Fatalf("providers = %d, want the catalog providers plus the legacy provider", len(resolution.Providers))
	}
	legacy := mustFindProvider(t, resolution, "legacy")
	if legacy.API.BaseURL == "" || legacy.Access.Env == "" {
		t.Fatalf("legacy provider = %#v, want the flat fields normalized in", legacy)
	}
	if overlay.Selection().ModelID != "deepseek-v4.1-flash" {
		t.Fatalf("selection = %#v, want the flat model as the default selection", overlay.Selection())
	}
	model := mustFindModel(t, resolution, "s2api", "deepseek-v4.1-flash")
	if model.Enabled {
		t.Fatalf("overlay rule must apply")
	}
}

// TestDocumentProvidersSuppressLegacyNormalizationOnly pins the complementary
// half: a document that declares providers keeps the flat fields out, and a
// rules-only document keeps them in.
func TestDocumentProvidersSuppressLegacyNormalizationOnly(t *testing.T) {
	doc := map[string]any{
		"providers": []any{
			testProvider("custom", APITypeOpenAIChatCompletions, "https://custom.example/v1", "CUSTOM_KEY", "custom-model"),
		},
	}
	if !documentDeclaresProviders(doc) {
		t.Fatal("a document with providers must suppress the legacy normalization")
	}
	if documentDeclaresProviders(map[string]any{"modelRules": []any{map[string]any{"match": "*", "config": map[string]any{}}}}) {
		t.Fatal("rule sets alone must not suppress the legacy normalization")
	}
}

func TestOverlayFromDocumentErrors(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "invalid JSON", raw: "{", wantErr: "unexpected EOF"},
		{name: "not an object", raw: "[]", wantErr: "must be a JSON object"},
		{name: "bad schema version", raw: `{"schemaVersion":2}`, wantErr: "schemaVersion must be 1"},
		{
			name:    "bad provider shape",
			raw:     `{"providers":[{"id":"p","api":"https://x"}]}`,
			wantErr: "providers[0]",
		},
		{
			name:    "bad rule config",
			raw:     `{"modelRules":[{"match":"*","config":{"properties":{"contextWindow":"large"}}}]}`,
			wantErr: "contextWindow",
		},
		{
			name:    "duplicate provider",
			raw:     `{"providers":[{"id":"p"},{"id":"p"}]}`,
			wantErr: "duplicated",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := OverlayFromDocument([]byte(test.raw), LegacyAgent{})
			if err == nil {
				t.Fatalf("OverlayFromDocument must fail")
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), test.wantErr)
			}
		})
	}
}

func TestOverlayFromLegacyConvenience(t *testing.T) {
	overlay, err := OverlayFromLegacy(LegacyAgent{Model: "deepseek-v4.1-flash", BaseURL: "https://x.example/v1", Protocol: "openai"})
	if err != nil {
		t.Fatalf("OverlayFromLegacy: %v", err)
	}
	if overlay.Selection().ModelID != "deepseek-v4.1-flash" {
		t.Fatalf("selection = %#v", overlay.Selection())
	}
}

// TestModelsDocumentConfiguredIsSemantic pins the boundary of the static
// compatibility path: only a document that declares something activates the
// resolver. Bytes like `{ }` or a schema-version-only body declare nothing.
func TestModelsDocumentConfiguredIsSemantic(t *testing.T) {
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
		{name: "null providers", raw: `{"providers":null}`, want: false},
		{name: "empty providers", raw: `{"providers":[]}`, want: true},
		{name: "selection", raw: `{"selection":{"modelId":"m"}}`, want: true},
		{name: "rules only", raw: `{"modelRules":[{"match":"*","config":{}}]}`, want: true},
		{name: "providers", raw: `{"providers":[{"id":"x"}]}`, want: true},
		{name: "not an object", raw: `[1]`, want: true},
		{name: "malformed", raw: `{"providers":`, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ModelsDocumentConfigured([]byte(test.raw)); got != test.want {
				t.Fatalf("ModelsDocumentConfigured(%q) = %v, want %v", test.raw, got, test.want)
			}
		})
	}
}

// TestSpacedEmptyDocumentKeepsTheFlatSelection: a `{ }` document is the same
// deployment as no document at all — flat fields normalize into the default
// selection exactly as they do on the static path.
func TestSpacedEmptyDocumentKeepsTheFlatSelection(t *testing.T) {
	legacy := LegacyAgent{Provider: "a6api", Model: "deepseek-v4.1-flash", BaseURL: "https://api.a6api.com/v1"}
	for _, raw := range []string{"", `{}`, "{ }", "{\n}"} {
		overlay, err := OverlayFromDocument([]byte(raw), legacy)
		if err != nil {
			t.Fatalf("OverlayFromDocument(%q): %v", raw, err)
		}
		selection := overlay.Selection()
		if selection.ProviderID != "a6api" || selection.ModelID != "deepseek-v4.1-flash" {
			t.Fatalf("OverlayFromDocument(%q) selection = %#v, want the flat default", raw, selection)
		}
	}
}

// TestRuleOnlyDocumentKeepsItsSelection: a document with rules and a selection
// but no providers and no flat fields resolves through the catalog exactly as
// declared — the rules customize the catalog entry the selection names.
func TestRuleOnlyDocumentKeepsItsSelection(t *testing.T) {
	overlay := mustOverlay(t, map[string]any{
		"modelRules": []any{modelRuleForWindow("deepseek-v4.1-flash", 65536)},
		"selection":  map[string]any{"providerId": "deepseek", "modelId": "deepseek-v4.1-flash"},
	}, LegacyAgent{})
	resolution := resolveFrom(t, EmbeddedBuiltin(), overlay)
	factory := mustFactory(t, resolution)

	active, err := factory.Resolve(overlay.Selection())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if active.ProviderID != "deepseek" || active.ModelID != "deepseek-v4.1-flash" {
		t.Fatalf("identity = %s/%s", active.ProviderID, active.ModelID)
	}
	if active.Properties.ContextWindow != 65536 {
		t.Fatalf("contextWindow = %d, want the tenant rule's window", active.Properties.ContextWindow)
	}
}
