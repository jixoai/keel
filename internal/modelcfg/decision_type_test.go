package modelcfg

import (
	"encoding/json"
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

// TestDecisionProviderRequiresAnAdapter pins the config contract: a provider
// declared as the decision family is not complete until it names its System
// One adapter. The binding is carried through resolution verbatim so the
// settings preflight sees exactly what was saved.
func TestDecisionProviderRequiresAnAdapter(t *testing.T) {
	doc := testCatalogDoc([]any{
		testProviderWithType("judge", ProviderTypeDecision,
			APITypeOpenAIChatCompletions, "https://decision.example/v1",
			"DECISION_KEY", "decision-model-preview"),
	})
	resolution := resolveFrom(t, loadTestCatalog(t, doc), Overlay{})

	provider := mustFindProvider(t, resolution, "judge")
	if provider.Executable {
		t.Fatal("a decision provider without an adapter must not be executable")
	}
	requireIssue(t, provider.Issues, "decision.adapter",
		"decision provider must declare an adapter (typesafe|cloudflare|openrouter|openjev)")

	// Declaring the adapter completes the provider and the binding survives.
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var withAdapter map[string]any
	if err := json.Unmarshal(raw, &withAdapter); err != nil {
		t.Fatal(err)
	}
	providers := withAdapter["providers"].([]any)
	providers[0].(map[string]any)["decision"] = map[string]any{
		"adapter":         "typesafe",
		"protocolVersion": "0.2.0",
		"binding": map[string]any{
			"state":     map[string]any{"text": "hello"},
			"questions": map[string]any{"urgent": map[string]any{"type": "noul", "instructions": "is it urgent?"}},
			"mode":      "enforce",
		},
	}
	loaded := loadTestCatalog(t, withAdapter)
	resolved := resolveFrom(t, loaded, Overlay{})
	provider = mustFindProvider(t, resolved, "judge")
	if !provider.Executable {
		t.Fatalf("a decision provider with an adapter must be executable, issues: %v", issueMessages(provider.Issues))
	}
	if provider.Decision == nil || provider.Decision.Adapter != "typesafe" ||
		provider.Decision.Binding == nil || provider.Decision.Binding.Mode != "enforce" {
		t.Fatalf("decision config did not survive resolution: %+v", provider.Decision)
	}
}

// TestDecisionBindingShapeIsValidated: malformed JSON and out-of-vocabulary
// policy values are configuration errors, not something a save may store and
// the runtime discover later.
func TestDecisionBindingShapeIsValidated(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "binding state is not JSON",
			raw:  `{"id":"judge","type":"decision","api":{"type":"openai-chat-completions","baseUrl":"https://d.example/v1"},"access":{"type":"api-key","env":"K"},"modelIds":["m"],"decision":{"adapter":"typesafe","binding":{"state":{"bad":`,
		},
		{
			name: "binding mode is unknown",
			raw:  `{"id":"judge","type":"decision","api":{"type":"openai-chat-completions","baseUrl":"https://d.example/v1"},"access":{"type":"api-key","env":"K"},"modelIds":["m"],"decision":{"adapter":"typesafe","binding":{"mode":"advisory"}}}`,
		},
		{
			name: "threshold out of range",
			raw:  `{"id":"judge","type":"decision","api":{"type":"openai-chat-completions","baseUrl":"https://d.example/v1"},"access":{"type":"api-key","env":"K"},"modelIds":["m"],"decision":{"adapter":"typesafe","binding":{"threshold":1.5}}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := `{"schemaVersion":1,"providers":[` + tc.raw + `],"modelRules":[],"modelApiRules":[],"siteRules":[],"providerModelRules":[]}`
			if _, err := LoadCatalog([]byte(doc)); err == nil {
				t.Fatal("a malformed decision binding was accepted")
			}
		})
	}
}
