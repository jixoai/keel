package modelcfg

import (
	"testing"
)

// TestTenantAddsModelToCatalogProvider pins the membership union: a tenant adds
// a model to a shipped provider without restating its endpoint or access.
func TestTenantAddsModelToCatalogProvider(t *testing.T) {
	cat := newTestCatalog(t, nil)
	overlay := mustOverlay(t, map[string]any{
		"providers": []any{
			map[string]any{"id": "chat", "modelIds": []any{"chat-extra-model"}},
		},
		"providerModelRules": []any{
			map[string]any{
				"providerId": "chat",
				"modelId":    "chat-extra-model",
				"config":     baselineModelRule()["config"],
			},
		},
	}, LegacyAgent{})
	resolution := resolveFrom(t, cat, overlay)
	provider := mustFindProvider(t, resolution, "chat")
	ids := make([]string, 0, len(provider.Models))
	for _, model := range provider.Models {
		ids = append(ids, model.ModelID)
	}
	if len(ids) != 2 || ids[0] != "deepseek-v4.1-flash" || ids[1] != "chat-extra-model" {
		t.Fatalf("chat models = %v, want catalog member first then the tenant addition", ids)
	}
	model := mustFindModel(t, resolution, "chat", "chat-extra-model")
	if !model.Executable || !model.Selectable {
		t.Fatalf("tenant-added model must be executable, issues: %v", issueMessages(model.Issues))
	}
}

// TestTenantRuleAppendsAfterCatalogRule pins the priority rule: tenant rules are
// appended, so inside their layer they patch over the shipped rule.
func TestTenantRuleAppendsAfterCatalogRule(t *testing.T) {
	cat := newTestCatalog(t, nil)
	overlay := mustOverlay(t, map[string]any{
		"modelRules": []any{
			map[string]any{
				"match":  "deepseek-v4.1-flash",
				"config": map[string]any{"properties": map[string]any{"contextWindow": 64000}},
			},
		},
	}, LegacyAgent{})
	resolution := resolveFrom(t, cat, overlay)
	model := mustFindModel(t, resolution, "chat", "deepseek-v4.1-flash")
	if model.Properties.ContextWindow != 64000 {
		t.Fatalf("contextWindow = %d, want the tenant rule's 64000", model.Properties.ContextWindow)
	}
	// The tenant rule did not delete the shipped rule: a field it does not
	// mention keeps the catalog value.
	if !model.Properties.SupportsToolCall {
		t.Fatalf("appending a rule must not drop the catalog baseline: %#v", model.Properties)
	}
}

// TestTenantProviderPatchRequiresID pins the shape check on sparse entries.
func TestTenantProviderPatchRequiresID(t *testing.T) {
	if _, err := OverlayFromDocument([]byte(`{"providers":[{"api":{"baseUrl":"https://x"}}]}`), LegacyAgent{}); err == nil {
		t.Fatal("a provider patch without an id must be rejected")
	}
	if _, err := OverlayFromDocument([]byte(`{"providers":[{"id":"chat","api":{"type":"bogus"}}]}`), LegacyAgent{}); err == nil {
		t.Fatal("an unknown api.type must be rejected")
	}
}
