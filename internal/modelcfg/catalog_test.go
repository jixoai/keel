package modelcfg

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEmbeddedCatalogResolves(t *testing.T) {
	cat := EmbeddedBuiltin()
	resolution := resolveFrom(t, cat, Overlay{})

	wantProviders := []string{"s2api", "a6api", "deepseek", "anthropic", "openai", "openrouter", "ollama"}
	if len(resolution.Providers) != len(wantProviders) {
		t.Fatalf("providers = %d, want %d", len(resolution.Providers), len(wantProviders))
	}
	for i, id := range wantProviders {
		provider := resolution.Providers[i]
		if provider.ProviderID != id {
			t.Fatalf("providers[%d] = %q, want %q", i, provider.ProviderID, id)
		}
		if !provider.Executable {
			t.Fatalf("builtin provider %q must be executable, issues: %v", id, issueMessages(provider.Issues))
		}
		if !provider.Selectable {
			t.Fatalf("builtin provider %q must be selectable", id)
		}
	}

	model := mustFindModel(t, resolution, "s2api", "deepseek-v4.1-flash")
	if model.API.Type != APITypeOpenAIChatCompletions || model.API.BaseURL != "https://s2api.top/v1" {
		t.Fatalf("api = %#v", model.API)
	}
	if model.Properties.ContextWindow != 128000 {
		t.Fatalf("contextWindow = %d", model.Properties.ContextWindow)
	}
	if !model.Properties.InputFormat.Text || model.Properties.InputFormat.Image {
		t.Fatalf("inputFormat = %#v", model.Properties.InputFormat)
	}
	if !model.Properties.OutputFormat.Text {
		t.Fatalf("outputFormat = %#v", model.Properties.OutputFormat)
	}
	if !model.Properties.SupportsToolCall || !model.Properties.SupportsJSONSchemaOutput || model.Properties.SupportsNativeWebSearch {
		t.Fatalf("supports flags = %#v", model.Properties)
	}
	if got := strings.Join(model.Options.ReasoningLevel.Values, ","); got != "off,low,medium,high" {
		t.Fatalf("reasoning levels = %q", got)
	}
	if model.Options.MaxOutputTokens.Max != 8192 {
		t.Fatalf("maxOutputTokens = %d", model.Options.MaxOutputTokens.Max)
	}
	requireIssue(t, model.Issues, "properties", "baseline-derived capabilities; override with a model rule")
	if !model.Executable || !model.Selectable {
		t.Fatalf("baseline model must be executable and selectable, issues: %v", issueMessages(model.Issues))
	}

	models := resolution.Models()
	if len(models) != 9 {
		t.Fatalf("selectable models = %d, want 9", len(models))
	}
	for _, listed := range models {
		if !listed.Selectable {
			t.Fatalf("Models() returned a non-selectable model: %s/%s", listed.ProviderID, listed.ModelID)
		}
	}
}

func TestBuiltinCatalogHasNoSecretValues(t *testing.T) {
	doc, err := decodeDocument(EmbeddedBuiltin().doc)
	if err != nil {
		t.Fatalf("decode embedded catalog: %v", err)
	}
	for _, provider := range doc.providers {
		if provider.Access.Ref != "" {
			t.Fatalf("builtin provider %q must not carry a secret reference", provider.ID)
		}
		if provider.Access.Env == "" || !envNamePattern.MatchString(provider.Access.Env) {
			t.Fatalf("builtin provider %q access.env = %q, want an env var name", provider.ID, provider.Access.Env)
		}
		for name, value := range provider.API.Headers {
			lower := strings.ToLower(name)
			if (lower == "authorization" || lower == "x-api-key") && value != "" {
				t.Fatalf("builtin provider %q carries a credential header", provider.ID)
			}
		}
	}
}

func validCatalogDocument() map[string]any {
	return map[string]any{
		"schemaVersion": 1,
		"providers": []any{
			map[string]any{
				"id":       "p",
				"name":     "P",
				"api":      map[string]any{"type": APITypeOpenAIChatCompletions, "baseUrl": "https://p.example/v1"},
				"access":   map[string]any{"type": "api-key", "env": "P_KEY"},
				"modelIds": anyStrings("m"),
			},
		},
		"modelRules": []any{baselineModelRule()},
	}
}

func marshalJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func TestLoadCatalogValidation(t *testing.T) {
	tests := []struct {
		name    string
		doc     func() map[string]any
		raw     string
		wantErr string
	}{
		{
			name:    "invalid JSON",
			raw:     "{",
			wantErr: "unexpected EOF",
		},
		{
			name:    "document must be an object",
			raw:     "[]",
			wantErr: "must be a JSON object",
		},
		{
			name: "missing schema version",
			doc: func() map[string]any {
				doc := validCatalogDocument()
				delete(doc, "schemaVersion")
				return doc
			},
			wantErr: "schemaVersion is required",
		},
		{
			name: "wrong schema version",
			doc: func() map[string]any {
				doc := validCatalogDocument()
				doc["schemaVersion"] = 2
				return doc
			},
			wantErr: "schemaVersion must be 1",
		},
		{
			name: "unknown top-level field",
			doc: func() map[string]any {
				doc := validCatalogDocument()
				doc["revision"] = 3
				return doc
			},
			wantErr: `unknown field "revision"`,
		},
		{
			name: "duplicate provider id",
			doc: func() map[string]any {
				doc := validCatalogDocument()
				providers := doc["providers"].([]any)
				doc["providers"] = append(providers, providers[0])
				return doc
			},
			wantErr: `duplicated`,
		},
		{
			name: "unsupported API type",
			doc: func() map[string]any {
				doc := validCatalogDocument()
				provider := doc["providers"].([]any)[0].(map[string]any)
				provider["api"].(map[string]any)["type"] = "grpc"
				return doc
			},
			wantErr: `api.type "grpc" is not supported`,
		},
		{
			name: "duplicate model id",
			doc: func() map[string]any {
				doc := validCatalogDocument()
				provider := doc["providers"].([]any)[0].(map[string]any)
				provider["modelIds"] = anyStrings("m", "m")
				return doc
			},
			wantErr: `duplicated`,
		},
		{
			name: "invalid base url scheme",
			doc: func() map[string]any {
				doc := validCatalogDocument()
				provider := doc["providers"].([]any)[0].(map[string]any)
				provider["api"].(map[string]any)["baseUrl"] = "ftp://p.example"
				return doc
			},
			wantErr: "must use http or https",
		},
		{
			name: "credentials in base url",
			doc: func() map[string]any {
				doc := validCatalogDocument()
				provider := doc["providers"].([]any)[0].(map[string]any)
				provider["api"].(map[string]any)["baseUrl"] = "https://user:pass@p.example/v1"
				return doc
			},
			wantErr: "must not embed credentials",
		},
		{
			name: "secret value in env",
			doc: func() map[string]any {
				doc := validCatalogDocument()
				provider := doc["providers"].([]any)[0].(map[string]any)
				provider["access"].(map[string]any)["env"] = "sk-live-abc"
				return doc
			},
			wantErr: "must be an environment variable name",
		},
		{
			name: "credential header",
			doc: func() map[string]any {
				doc := validCatalogDocument()
				provider := doc["providers"].([]any)[0].(map[string]any)
				provider["api"].(map[string]any)["headers"] = map[string]any{"Authorization": "Bearer secret"}
				return doc
			},
			wantErr: "must not carry a credential",
		},
		{
			name: "unsupported visibility",
			doc: func() map[string]any {
				doc := validCatalogDocument()
				provider := doc["providers"].([]any)[0].(map[string]any)
				provider["visibility"] = "sometimes"
				return doc
			},
			wantErr: "visibility",
		},
		{
			name: "unknown field in provider",
			doc: func() map[string]any {
				doc := validCatalogDocument()
				provider := doc["providers"].([]any)[0].(map[string]any)
				provider["endpoint"] = "https://x"
				return doc
			},
			wantErr: "unknown field",
		},
		{
			name: "provider model rule references unknown provider",
			doc: func() map[string]any {
				doc := validCatalogDocument()
				doc["providerModelRules"] = []any{
					map[string]any{"providerId": "ghost", "modelId": "m", "config": map[string]any{}},
				}
				return doc
			},
			wantErr: "does not match any provider",
		},
		{
			name: "rule missing match",
			doc: func() map[string]any {
				doc := validCatalogDocument()
				doc["modelRules"] = []any{
					map[string]any{"config": map[string]any{}},
				}
				return doc
			},
			wantErr: "match is required",
		},
		{
			name: "rule config wrong type",
			doc: func() map[string]any {
				doc := validCatalogDocument()
				doc["modelRules"] = []any{
					map[string]any{
						"match": "*",
						"config": map[string]any{
							"properties": map[string]any{"contextWindow": "large"},
						},
					},
				}
				return doc
			},
			wantErr: "contextWindow must be a non-negative integer",
		},
		{
			name: "unknown model config field",
			doc: func() map[string]any {
				doc := validCatalogDocument()
				doc["modelRules"] = []any{
					map[string]any{
						"match":  "*",
						"config": map[string]any{"optionSpecs": map[string]any{}},
					},
				}
				return doc
			},
			wantErr: `unknown field "optionSpecs"`,
		},
		{
			name: "model api rule needs a supported api type",
			doc: func() map[string]any {
				doc := validCatalogDocument()
				doc["modelApiRules"] = []any{
					map[string]any{"match": "*", "apiType": "grpc", "config": map[string]any{}},
				}
				return doc
			},
			wantErr: "apiType",
		},
		{
			name: "site rule base url must be valid",
			doc: func() map[string]any {
				doc := validCatalogDocument()
				doc["siteRules"] = []any{
					map[string]any{"match": "*", "baseUrl": "ftp://x", "config": map[string]any{}},
				}
				return doc
			},
			wantErr: "must use http or https",
		},
		{
			name: "provider model rule needs model id",
			doc: func() map[string]any {
				doc := validCatalogDocument()
				doc["providerModelRules"] = []any{
					map[string]any{"providerId": "p", "config": map[string]any{}},
				}
				return doc
			},
			wantErr: "modelId is required",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var raw []byte
			if test.raw != "" {
				raw = []byte(test.raw)
			} else {
				raw = marshalJSON(t, test.doc())
			}
			_, err := LoadCatalog(raw)
			if err == nil {
				t.Fatalf("LoadCatalog must fail")
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), test.wantErr)
			}
		})
	}
}

func TestLoadCatalogAllowsIncompleteProvider(t *testing.T) {
	doc := validCatalogDocument()
	doc["providers"] = []any{
		map[string]any{"id": "broken", "modelIds": anyStrings("m")},
	}
	doc["modelRules"] = []any{baselineModelRule()}
	cat := loadTestCatalog(t, doc)
	resolution := resolveFrom(t, cat, Overlay{})
	provider := mustFindProvider(t, resolution, "broken")
	if provider.Executable {
		t.Fatalf("incomplete provider must not be executable")
	}
	if len(provider.Issues) == 0 {
		t.Fatalf("incomplete provider must carry issues")
	}
}

func TestCatalogValidationAcceptsCompleteBaseline(t *testing.T) {
	raw := marshalJSON(t, validCatalogDocument())
	if _, err := LoadCatalog(raw); err != nil {
		t.Fatalf("valid catalog rejected: %v", err)
	}
}
