package modelcfg

import (
	"encoding/json"
	"reflect"
	"testing"
)

// ---- builders -------------------------------------------------------------

func anyStrings(values ...string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

func testProvider(id, apiType, baseURL, env string, modelIDs ...string) map[string]any {
	return map[string]any{
		"id":         id,
		"name":       id,
		"api":        map[string]any{"type": apiType, "baseUrl": baseURL},
		"access":     map[string]any{"type": "api-key", "env": env},
		"modelIds":   anyStrings(modelIDs...),
		"visibility": "visible",
	}
}

func baselineModelRule() map[string]any {
	return map[string]any{
		"match": "*",
		"config": map[string]any{
			"enabled": true,
			"properties": map[string]any{
				"contextWindow": 128000,
				"inputFormat": map[string]any{
					"text":  true,
					"image": false,
					"video": false,
					"audio": false,
					"pdf":   false,
				},
				"outputFormat":             map[string]any{"text": true},
				"supportsToolCall":         true,
				"supportsJsonSchemaOutput": true,
				"supportsNativeWebSearch":  false,
			},
			"options": map[string]any{
				"reasoningLevel": map[string]any{
					"values": anyStrings("off", "low", "medium", "high"),
					"map": map[string]any{
						"off":    map[string]any{},
						"low":    map[string]any{},
						"medium": map[string]any{},
						"high":   map[string]any{},
					},
				},
				"maxOutputTokens": map[string]any{
					"max": 8192,
					"map": map[string]any{"": map[string]any{}},
				},
			},
		},
	}
}

func protocolModelAPIRules() []any {
	return []any{
		map[string]any{
			"match":   "*",
			"apiType": APITypeOpenAIChatCompletions,
			"config": map[string]any{
				"options": map[string]any{
					"reasoningLevel": map[string]any{
						"map": map[string]any{
							"off":    map[string]any{},
							"low":    map[string]any{"reasoning_effort": "low"},
							"medium": map[string]any{"reasoning_effort": "medium"},
							"high":   map[string]any{"reasoning_effort": "high"},
						},
					},
				},
			},
		},
		map[string]any{
			"match":   "*",
			"apiType": APITypeOpenAIResponses,
			"config": map[string]any{
				"options": map[string]any{
					"reasoningLevel": map[string]any{
						"map": map[string]any{
							"off":    map[string]any{},
							"low":    map[string]any{"reasoning": map[string]any{"effort": "low"}},
							"medium": map[string]any{"reasoning": map[string]any{"effort": "medium"}},
							"high":   map[string]any{"reasoning": map[string]any{"effort": "high"}},
						},
					},
				},
			},
		},
		map[string]any{
			"match":   "*",
			"apiType": APITypeAnthropicMessages,
			"config": map[string]any{
				"options": map[string]any{
					"reasoningLevel": map[string]any{
						"map": map[string]any{
							"off": map[string]any{
								"thinking": map[string]any{"type": "disabled"},
							},
							"low": map[string]any{
								"thinking": map[string]any{"type": "enabled", "budget_tokens": 1024},
							},
							"medium": map[string]any{
								"thinking": map[string]any{"type": "enabled", "budget_tokens": 4096},
							},
							"high": map[string]any{
								"thinking": map[string]any{"type": "enabled", "budget_tokens": 6144},
							},
						},
					},
				},
			},
		},
	}
}

func baseProviders() []any {
	return []any{
		testProvider("chat", APITypeOpenAIChatCompletions, "https://chat.example/v1", "CHAT_KEY", "deepseek-v4.1-flash"),
		testProvider("resp", APITypeOpenAIResponses, "https://resp.example/v1", "RESP_KEY", "deepseek-v4.1-flash"),
		testProvider("anth", APITypeAnthropicMessages, "https://anth.example", "ANTH_KEY", "claude-test"),
	}
}

func testCatalogDoc(providers []any) map[string]any {
	return map[string]any{
		"schemaVersion":      1,
		"providers":          providers,
		"modelRules":         []any{baselineModelRule()},
		"modelApiRules":      protocolModelAPIRules(),
		"siteRules":          []any{},
		"providerModelRules": []any{},
	}
}

// ---- loading --------------------------------------------------------------

func loadTestCatalog(t *testing.T, doc map[string]any) *Catalog {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal catalog: %v", err)
	}
	cat, err := LoadCatalog(raw)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	return cat
}

func loadTestCatalogJSON(t *testing.T, raw string) *Catalog {
	t.Helper()
	cat, err := LoadCatalog([]byte(raw))
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	return cat
}

func newTestCatalog(t *testing.T, mutate func(doc map[string]any)) *Catalog {
	t.Helper()
	doc := testCatalogDoc(baseProviders())
	if mutate != nil {
		mutate(doc)
	}
	return loadTestCatalog(t, doc)
}

func mustOverlay(t *testing.T, doc map[string]any, legacy LegacyAgent) Overlay {
	t.Helper()
	var raw []byte
	if doc != nil {
		var err error
		raw, err = json.Marshal(doc)
		if err != nil {
			t.Fatalf("marshal overlay: %v", err)
		}
	}
	overlay, err := OverlayFromDocument(raw, legacy)
	if err != nil {
		t.Fatalf("OverlayFromDocument: %v", err)
	}
	return overlay
}

func mustResolver(t *testing.T, cat *Catalog, overlay Overlay) *Resolver {
	t.Helper()
	resolver, err := NewResolver(cat, overlay)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return resolver
}

func resolveFrom(t *testing.T, cat *Catalog, overlay Overlay) Resolution {
	t.Helper()
	return mustResolver(t, cat, overlay).Resolve()
}

func mustFactory(t *testing.T, resolution Resolution) *Factory {
	t.Helper()
	factory, err := NewFactory(&resolution)
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	return factory
}

func mustFindModel(t *testing.T, resolution Resolution, providerID, modelID string) ModelView {
	t.Helper()
	model, ok := resolution.Find(providerID, modelID)
	if !ok {
		t.Fatalf("model %s/%s not found", providerID, modelID)
	}
	return model
}

func mustFindProvider(t *testing.T, resolution Resolution, providerID string) ProviderView {
	t.Helper()
	for _, provider := range resolution.Providers {
		if provider.ProviderID == providerID {
			return provider
		}
	}
	t.Fatalf("provider %s not found", providerID)
	return ProviderView{}
}

// ---- assertions -----------------------------------------------------------

func issueMessages(issues []Issue) []string {
	out := make([]string, len(issues))
	for i, issue := range issues {
		out[i] = issue.Field + ": " + issue.Message
	}
	return out
}

func hasIssue(issues []Issue, field, message string) bool {
	for _, issue := range issues {
		if issue.Field != field {
			continue
		}
		if message == "" || issue.Message == message {
			return true
		}
	}
	return false
}

func requireIssue(t *testing.T, issues []Issue, field, message string) {
	t.Helper()
	if !hasIssue(issues, field, message) {
		t.Fatalf("missing issue %s/%q in %v", field, message, issueMessages(issues))
	}
}

func patchObject(t *testing.T, patch map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := patch[key]
	if !ok {
		t.Fatalf("patch missing key %q: %#v", key, patch)
	}
	obj, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("patch[%q] = %#v, want object", key, value)
	}
	return obj
}

func patchInt(t *testing.T, patch map[string]any, key string) int {
	t.Helper()
	value, ok := patch[key]
	if !ok {
		t.Fatalf("patch missing key %q: %#v", key, patch)
	}
	number, ok := intValue(value)
	if !ok {
		t.Fatalf("patch[%q] = %#v, want int", key, value)
	}
	return number
}

func patchString(t *testing.T, patch map[string]any, key string) string {
	t.Helper()
	value, ok := patch[key]
	if !ok {
		t.Fatalf("patch missing key %q: %#v", key, patch)
	}
	text, ok := value.(string)
	if !ok {
		t.Fatalf("patch[%q] = %#v, want string", key, value)
	}
	return text
}

func equalJSON(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("values differ:\n got: %#v\nwant: %#v", got, want)
	}
}
