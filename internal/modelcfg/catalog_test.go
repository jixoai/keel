package modelcfg

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/host"
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

// TestReasoningLevelsMatchAgentcore pins the unified vocabulary against the
// runtime's level constants in both directions, so the model layer and the
// execution layer can never drift apart.
func TestReasoningLevelsMatchAgentcore(t *testing.T) {
	want := []string{
		string(agentcore.ThinkingOff),
		string(agentcore.ThinkingMinimal),
		string(agentcore.ThinkingLow),
		string(agentcore.ThinkingMedium),
		string(agentcore.ThinkingHigh),
		string(agentcore.ThinkingXHigh),
		string(agentcore.ThinkingMax),
	}
	if !reflect.DeepEqual(reasoningLevels, want) {
		t.Fatalf("reasoningLevels = %v, want the agentcore levels %v", reasoningLevels, want)
	}
}

// TestSecretRefVocabularyMatchesHost pins provider access.ref to the host's
// tenant-document vocabulary: a value the host accepts for agent.apiKeyRef must
// be legal here, and a value it refuses must be refused here too. The empty
// string is the "no reference declared" case and is compared only for
// completeness.
func TestSecretRefVocabularyMatchesHost(t *testing.T) {
	samples := []string{
		"env:NAME", "env:_under", "secret:NAME", "${NAME}", "${A1_B}",
		"hunter2", "sk-1234", "pk-live", "Bearer abc", "basic xyz",
		"env:", "secret:", "${}", "${NAME", "NAME}", "env:1BAD", "env:BAD-NAME",
		"ENV:NAME", " name", "name ", "   ", "a/b",
		// The store's secret names are capped at 64 characters; the reference
		// vocabulary carries the same bound, so a name that cannot be stored
		// cannot be referenced either (the two vocabularies are one).
		"env:" + strings.Repeat("A", 64), "env:" + strings.Repeat("A", 65),
		"secret:" + strings.Repeat("A", 65), "${" + strings.Repeat("A", 65) + "}",
	}
	for _, sample := range samples {
		hostErr := host.TenantConfig{Agent: host.AgentConfig{APIKeyRef: sample}}.Validate()
		refErr := validateSecretRef(sample)
		if (hostErr == nil) != (refErr == nil) {
			t.Errorf("vocabulary drift for %q: host err=%v, modelcfg err=%v", sample, hostErr, refErr)
		}
	}
	if err := validateSecretRef(""); err != nil {
		t.Fatalf("the empty ref is 'no reference declared': %v", err)
	}
}

// TestParseSecretRef pins the three accepted shapes and the names they yield.
func TestParseSecretRef(t *testing.T) {
	valid := map[string]string{
		"env:NAME":    "NAME",
		"secret:NAME": "NAME",
		"${NAME}":     "NAME",
		" env:NAME ":  "NAME",
		"${A1_B}":     "A1_B",
	}
	for ref, want := range valid {
		name, ok := ParseSecretRef(ref)
		if !ok || name != want {
			t.Errorf("ParseSecretRef(%q) = %q, %v; want %q, true", ref, name, ok, want)
		}
	}
	for _, ref := range []string{"", "hunter2", "sk-1234", "env:", "env:BAD-NAME", "${NAME", "NAME}", "NAME"} {
		if name, ok := ParseSecretRef(ref); ok {
			t.Errorf("ParseSecretRef(%q) accepted as %q; literal values must not parse", ref, name)
		}
	}
}

// TestProviderRefMustBeAReference: a literal credential in access.ref is
// refused at validation (naming the field), whatever shape it has.
func TestProviderRefMustBeAReference(t *testing.T) {
	provider := testProvider("gw", APITypeOpenAIChatCompletions, "https://gw.example/v1", "", "model")
	provider["access"] = map[string]any{"type": "api-key", "ref": "hunter2-literal-secret"}
	doc := testCatalogDoc([]any{provider})
	_, err := LoadCatalog(marshalJSON(t, doc))
	if err == nil {
		t.Fatal("a literal credential in access.ref must be refused")
	}
	if !strings.Contains(err.Error(), "access.ref") || !strings.Contains(err.Error(), "secret reference") {
		t.Fatalf("ref refusal must name the field and the vocabulary: %v", err)
	}

	// The same check runs for a tenant patch after the overlay is merged.
	overlay := mustOverlay(t, map[string]any{
		"providers": []any{map[string]any{
			"id":     "chat",
			"access": map[string]any{"type": "api-key", "ref": "hunter2-literal-secret"},
		}},
	}, LegacyAgent{})
	if _, err := NewResolver(loadTestCatalog(t, testCatalogDoc(baseProviders())), overlay); err == nil {
		t.Fatal("a tenant patch with a literal ref must be refused")
	}
}

// TestValidateSelectionFact covers the strict gate: exactly the documented
// vocabulary, both spellings of the reasoning level, and no duplicates.
func TestValidateSelectionFact(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "empty", raw: ""},
		{name: "null", raw: "null"},
		{name: "full selection", raw: `{"providerId":"a","modelId":"m","reasoningLevel":"high"}`},
		{name: "documented alias", raw: `{"modelId":"m","options":{"reasoningLevel":"low"}}`},
		{name: "level only", raw: `{"reasoningLevel":"low"}`},
		{name: "options null", raw: `{"modelId":"m","options":null}`},
		{name: "unknown key", raw: `{"modelId":"m","baseUrl":"https://evil.example"}`, wantErr: `unknown key "baseUrl"`},
		{name: "unknown nested key", raw: `{"modelId":"m","options":{"temperature":1}}`, wantErr: `options: unknown key "temperature"`},
		{name: "duplicate key", raw: `{"modelId":"m","modelId":"n"}`, wantErr: `duplicate key "modelId"`},
		{name: "duplicate nested key", raw: `{"modelId":"m","options":{"reasoningLevel":"a","reasoningLevel":"b"}}`, wantErr: `options: duplicate key "reasoningLevel"`},
		{name: "wrong value type", raw: `{"modelId":7}`, wantErr: "selection:"},
		{name: "not an object", raw: `["modelId"]`, wantErr: "must be a JSON object"},
		{name: "trailing content", raw: `{"modelId":"m"} {}`, wantErr: "selection:"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateSelectionFact([]byte(test.raw))
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateSelectionFact(%q) = %v", test.raw, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateSelectionFact(%q) = nil, want %q", test.raw, test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

// TestProviderTypeValidation pins the provider family token. A declared family
// (including a plugin family this package does not know) is accepted; a value
// that is not a token is refused at load time, and a tenant patch obeys the
// same grammar.
func TestProviderTypeValidation(t *testing.T) {
	doc := validCatalogDocument()
	doc["providers"].([]any)[0].(map[string]any)["type"] = "realtime"
	if _, err := LoadCatalog(marshalJSON(t, doc)); err != nil {
		t.Fatalf("a declared provider type must be accepted: %v", err)
	}

	for _, invalid := range []any{"realtime audio", " llm", 7, "real/time"} {
		doc := validCatalogDocument()
		doc["providers"].([]any)[0].(map[string]any)["type"] = invalid
		if _, err := LoadCatalog(marshalJSON(t, doc)); err == nil {
			t.Fatalf("provider type %#v must be refused", invalid)
		}
	}

	for _, tenantDoc := range []string{
		`{"providers":[{"id":"p","type":"realtime audio"}]}`,
		`{"providers":[{"id":"p","type":7}]}`,
	} {
		if _, err := OverlayFromDocument([]byte(tenantDoc), LegacyAgent{}); err == nil {
			t.Fatalf("tenant provider patch %s must be refused", tenantDoc)
		}
	}
}

// TestOutputFormatAcceptsAudioAndMedia pins the MediaOut vocabulary extension:
// audio is a declared output (realtime/tts), image and video are declared
// outputs (media generation), and an unknown key is still refused so a typo
// cannot silently declare a capability.
func TestOutputFormatAcceptsAudioAndMedia(t *testing.T) {
	for _, output := range []map[string]any{
		{"text": true, "audio": true},
		{"audio": true},
		{"text": true, "image": true, "video": true, "audio": true},
	} {
		doc := validCatalogDocument()
		doc["modelRules"] = []any{
			map[string]any{
				"match":  "*",
				"config": map[string]any{"properties": map[string]any{"outputFormat": output}},
			},
		}
		if _, err := LoadCatalog(marshalJSON(t, doc)); err != nil {
			t.Fatalf("outputFormat %v rejected: %v", output, err)
		}
	}

	doc := validCatalogDocument()
	doc["modelRules"] = []any{
		map[string]any{
			"match": "*",
			"config": map[string]any{
				"properties": map[string]any{"outputFormat": map[string]any{"text": true, "hologram": true}},
			},
		},
	}
	if _, err := LoadCatalog(marshalJSON(t, doc)); err == nil {
		t.Fatal("an unknown outputFormat key must be refused")
	}
}

// TestParseSelectionSurfaceHints pins the picker hint vocabulary: the hints are
// accepted by the strict fact gate (so the picker can record the surface), are
// normalized to lowercase tokens, and a non-token value is refused instead of
// stored.
func TestParseSelectionSurfaceHints(t *testing.T) {
	fact := `{"providerId":"omni","modelId":"qwen-omni","providerType":"Realtime","surfaceKind":"Realtime"}`
	if err := ValidateSelectionFact([]byte(fact)); err != nil {
		t.Fatalf("surface hints must be accepted: %v", err)
	}
	selection, err := ParseSelection([]byte(fact))
	if err != nil {
		t.Fatalf("ParseSelection: %v", err)
	}
	if selection.ProviderType != ProviderTypeRealtime || selection.SurfaceKind != SurfaceKindRealtime {
		t.Fatalf("selection hints = %s/%s, want realtime/realtime", selection.ProviderType, selection.SurfaceKind)
	}
	if err := ValidateSelectionFact([]byte(`{"modelId":"m","surfaceKind":"realtime audio"}`)); err == nil {
		t.Fatal("a non-token surfaceKind must be refused")
	}
	if err := ValidateSelectionFact([]byte(`{"modelId":"m","providerType":" "}`)); err == nil {
		t.Fatal("a blank-but-present providerType must be refused")
	}
}
