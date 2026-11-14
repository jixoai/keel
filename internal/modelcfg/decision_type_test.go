package modelcfg

import (
	"strings"
	"testing"
)

// TestDecisionProviderTypeResolvesToGenericSurface pins the decision family's
// configuration contract: `type: decision` is a legal provider family, it does
// not inherit the llm surface, and it resolves to the generic surface because
// the client surface registry (web/src/surfaces/registry.ts) has no dedicated
// decision panel yet. A future decision panel must add a surface kind here and
// in the registry together, not silently re-label this family as llm.
func TestDecisionProviderTypeResolvesToGenericSurface(t *testing.T) {
	const decisionType = "decision"
	doc := testCatalogDoc([]any{
		testProviderWithType("judge", decisionType,
			APITypeOpenAIChatCompletions, "https://decision.example/v1",
			"DECISION_KEY", "decision-model-preview"),
	})
	resolution := resolveFrom(t, loadTestCatalog(t, doc), Overlay{})

	provider := mustFindProvider(t, resolution, "judge")
	if provider.ProviderType != decisionType || provider.SurfaceKind != SurfaceKindGeneric {
		t.Fatalf("decision provider = %s/%s, want decision/generic", provider.ProviderType, provider.SurfaceKind)
	}
	if provider.SurfaceKind == SurfaceKindLLM {
		t.Fatal("a decision provider must not be forced onto the llm surface")
	}

	model := mustFindModel(t, resolution, "judge", "decision-model-preview")
	if model.ProviderType != decisionType || model.SurfaceKind != SurfaceKindGeneric {
		t.Fatalf("decision model = %s/%s, want decision/generic", model.ProviderType, model.SurfaceKind)
	}
}

// TestOnlySessionFamiliesAreSelectable pins the selectability contract: a
// decision model is a capability endpoint (POST /v1/decisions/ask,
// POST /v1/media/decision), not a conversation, so it is executable but never
// selectable — picking it would open the generic "no dedicated panel"
// placeholder, a dead end. Every conversation-capable family keeps working, so
// the guard cannot silently shrink the picker.
func TestOnlySessionFamiliesAreSelectable(t *testing.T) {
	families := []string{
		ProviderTypeLLM,
		ProviderTypeImage,
		ProviderTypeRealtime,
		ProviderTypeTTS,
		ProviderTypeSTT,
		ProviderTypeDecision,
	}
	providers := make([]any, 0, len(families))
	for _, family := range families {
		providers = append(providers, testProviderWithType(
			"gw-"+family, family,
			APITypeOpenAIChatCompletions, "https://"+family+".example/v1",
			strings.ToUpper(family)+"_KEY", "m",
		))
	}
	resolution := resolveFrom(t, loadTestCatalog(t, testCatalogDoc(providers)), Overlay{})

	for _, family := range families {
		model := mustFindModel(t, resolution, "gw-"+family, "m")
		if !model.Executable {
			t.Fatalf("%s model must stay executable, issues: %v", family, issueMessages(model.Issues))
		}
		wantSelectable := family != ProviderTypeDecision
		if model.Selectable != wantSelectable {
			t.Fatalf("%s model selectable = %v, want %v", family, model.Selectable, wantSelectable)
		}
		if !wantSelectable {
			requireIssue(t, model.Issues, "providerType",
				"decision models are capability endpoints called through /v1/decisions/ask (or /v1/media/decision), not session models: the picker does not offer them as a conversation")
		} else if hasIssue(model.Issues, "providerType", "") {
			t.Fatalf("%s model must not carry a providerType issue: %v", family, issueMessages(model.Issues))
		}
	}

	// The model-level family override is what the demo provider uses (a generic
	// provider declaring one family per model), so it must be the effective
	// family the guard reads: inheriting the provider's llm family here would
	// silently re-enable the dead end.
	doc := testCatalogDoc([]any{
		testProviderWithType("multi", ProviderTypeLLM,
			APITypeOpenAIChatCompletions, "https://multi.example/v1", "MULTI_KEY", "chat-model", "judge-model"),
	})
	doc["providerModelRules"] = []any{
		map[string]any{
			"providerId": "multi",
			"modelId":    "judge-model",
			"config":     map[string]any{"providerType": ProviderTypeDecision},
		},
	}
	overridden := resolveFrom(t, loadTestCatalog(t, doc), Overlay{})

	chat := mustFindModel(t, overridden, "multi", "chat-model")
	if !chat.Selectable {
		t.Fatalf("the sibling llm model must stay selectable, issues: %v", issueMessages(chat.Issues))
	}
	judge := mustFindModel(t, overridden, "multi", "judge-model")
	if judge.ProviderType != ProviderTypeDecision || judge.Selectable {
		t.Fatalf("model-level decision override = %s/selectable=%v, want decision/false",
			judge.ProviderType, judge.Selectable)
	}
	if !judge.Executable {
		t.Fatalf("the overridden decision model must stay executable, issues: %v", issueMessages(judge.Issues))
	}
}
