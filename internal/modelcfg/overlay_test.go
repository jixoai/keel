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

func TestDocumentRulesSuppressLegacyNormalization(t *testing.T) {
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
	if len(resolution.Providers) != 7 {
		t.Fatalf("providers = %d, want the catalog providers", len(resolution.Providers))
	}
	model := mustFindModel(t, resolution, "s2api", "deepseek-v4.1-flash")
	if model.Enabled {
		t.Fatalf("overlay rule must apply instead of legacy normalization")
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
