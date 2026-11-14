package modelcfg

import (
	"encoding/json"
	"strings"
	"testing"
)

func appendModelRules(doc map[string]any, rules ...map[string]any) {
	existing := doc["modelRules"].([]any)
	for _, rule := range rules {
		existing = append(existing, rule)
	}
	doc["modelRules"] = existing
}

func appendModelAPIRules(doc map[string]any, rules ...map[string]any) {
	existing := doc["modelApiRules"].([]any)
	for _, rule := range rules {
		existing = append(existing, rule)
	}
	doc["modelApiRules"] = existing
}

func appendSiteRules(doc map[string]any, rules ...map[string]any) {
	existing := doc["siteRules"].([]any)
	for _, rule := range rules {
		existing = append(existing, rule)
	}
	doc["siteRules"] = existing
}

func appendProviderModelRules(doc map[string]any, rules ...map[string]any) {
	existing := doc["providerModelRules"].([]any)
	for _, rule := range rules {
		existing = append(existing, rule)
	}
	doc["providerModelRules"] = existing
}

func contextWindowConfig(window int) map[string]any {
	return map[string]any{
		"config": map[string]any{
			"properties": map[string]any{"contextWindow": window},
		},
	}
}

func modelRuleForWindow(match string, window int) map[string]any {
	rule := contextWindowConfig(window)
	rule["match"] = match
	return rule
}

func modelAPIRuleForWindow(match, apiType string, window int) map[string]any {
	rule := contextWindowConfig(window)
	rule["match"] = match
	rule["apiType"] = apiType
	return rule
}

func siteRuleForWindow(match, baseURL string, window int) map[string]any {
	rule := contextWindowConfig(window)
	rule["match"] = match
	rule["baseUrl"] = baseURL
	return rule
}

func providerModelRuleForWindow(providerID, modelID string, window int) map[string]any {
	rule := contextWindowConfig(window)
	rule["providerId"] = providerID
	rule["modelId"] = modelID
	return rule
}

func TestRulePrecedence(t *testing.T) {
	tests := []struct {
		name    string
		modelID string
		mutate  func(doc map[string]any)
		window  int
		exact   bool
	}{
		{
			name:    "later rule in the same layer wins",
			modelID: "deepseek-v4.1-flash",
			mutate: func(doc map[string]any) {
				appendModelRules(doc,
					modelRuleForWindow("*deepseek*", 64000),
					modelRuleForWindow("*deepseek-v4.1*", 96000),
				)
			},
			window: 96000,
		},
		{
			name:    "specific variant overrides family baseline",
			modelID: "deepseek-v4.1-flash",
			mutate: func(doc map[string]any) {
				appendModelRules(doc,
					modelRuleForWindow("*deepseek*", 64000),
					modelRuleForWindow("deepseek-v4.1-flash", 128000),
				)
			},
			window: 128000,
			exact:  true,
		},
		{
			name:    "api layer applies after model layer",
			modelID: "deepseek-v4.1-flash",
			mutate: func(doc map[string]any) {
				appendModelRules(doc, modelRuleForWindow("deepseek-v4.1-flash", 128000))
				appendModelAPIRules(doc, modelAPIRuleForWindow("deepseek-v4.1-flash", APITypeOpenAIChatCompletions, 100000))
			},
			window: 100000,
			exact:  true,
		},
		{
			name:    "site rule matches normalized scheme host and trailing slash",
			modelID: "deepseek-v4.1-flash",
			mutate: func(doc map[string]any) {
				appendModelAPIRules(doc, modelAPIRuleForWindow("deepseek-v4.1-flash", APITypeOpenAIChatCompletions, 100000))
				appendSiteRules(doc, siteRuleForWindow("*", "HTTPS://Chat.Example/v1/", 150000))
			},
			window: 150000,
			exact:  true,
		},
		{
			name:    "site rule does not match a different path",
			modelID: "deepseek-v4.1-flash",
			mutate: func(doc map[string]any) {
				appendModelAPIRules(doc, modelAPIRuleForWindow("deepseek-v4.1-flash", APITypeOpenAIChatCompletions, 100000))
				appendSiteRules(doc, siteRuleForWindow("*", "https://chat.example/v2", 150000))
			},
			window: 100000,
			exact:  true,
		},
		{
			name:    "provider model rule applies last",
			modelID: "deepseek-v4.1-flash",
			mutate: func(doc map[string]any) {
				appendModelRules(doc, modelRuleForWindow("*deepseek*", 64000))
				appendModelAPIRules(doc, modelAPIRuleForWindow("deepseek-v4.1-flash", APITypeOpenAIChatCompletions, 100000))
				appendSiteRules(doc, siteRuleForWindow("*", "https://chat.example/v1", 150000))
				appendProviderModelRules(doc, providerModelRuleForWindow("chat", "deepseek-v4.1-flash", 200000))
			},
			window: 200000,
			exact:  true,
		},
		{
			name:    "globs are case-insensitive and whole string",
			modelID: "DEEPSEEK-V4.1-FLASH",
			mutate: func(doc map[string]any) {
				for _, provider := range doc["providers"].([]any) {
					p := provider.(map[string]any)
					if p["id"] == "chat" {
						p["modelIds"] = anyStrings("DEEPSEEK-V4.1-FLASH")
					}
				}
				appendModelRules(doc,
					modelRuleForWindow("deepseek-v4.1-flash", 130000),
					modelRuleForWindow("deepseek-v4.1-flash-extra", 140000),
				)
			},
			window: 130000,
			exact:  true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cat := newTestCatalog(t, test.mutate)
			resolution := resolveFrom(t, cat, Overlay{})
			model := mustFindModel(t, resolution, "chat", test.modelID)
			if !model.Executable {
				t.Fatalf("model should be executable, issues: %v", issueMessages(model.Issues))
			}
			if got := model.Properties.ContextWindow; got != test.window {
				t.Fatalf("contextWindow = %d, want %d", got, test.window)
			}
			baseline := hasIssue(model.Issues, "properties", "baseline-derived capabilities; override with a model rule")
			if baseline == test.exact {
				t.Fatalf("baseline issue = %v, want exact=%v (issues: %v)", baseline, test.exact, issueMessages(model.Issues))
			}
		})
	}
}

func TestSameModelDifferentProviders(t *testing.T) {
	cat := newTestCatalog(t, nil)
	resolution := resolveFrom(t, cat, Overlay{})

	chat := mustFindModel(t, resolution, "chat", "deepseek-v4.1-flash")
	resp := mustFindModel(t, resolution, "resp", "deepseek-v4.1-flash")
	if chat.API.Type != APITypeOpenAIChatCompletions {
		t.Fatalf("chat api type = %q", chat.API.Type)
	}
	if resp.API.Type != APITypeOpenAIResponses {
		t.Fatalf("resp api type = %q", resp.API.Type)
	}
	if chat.API.BaseURL == resp.API.BaseURL {
		t.Fatalf("same model id under two providers must keep distinct base urls")
	}
	if !chat.Selectable || !resp.Selectable {
		t.Fatalf("both identities should be selectable: chat=%v resp=%v", chat.Selectable, resp.Selectable)
	}

	factory := mustFactory(t, resolution)
	chatModel, err := factory.Resolve(Selection{ProviderID: "chat", ModelID: "deepseek-v4.1-flash", ReasoningLevel: "high"})
	if err != nil {
		t.Fatalf("resolve chat: %v", err)
	}
	respModel, err := factory.Resolve(Selection{ProviderID: "resp", ModelID: "deepseek-v4.1-flash", ReasoningLevel: "high"})
	if err != nil {
		t.Fatalf("resolve resp: %v", err)
	}
	if patchString(t, chatModel.RequestPatch, "reasoning_effort") != "high" {
		t.Fatalf("chat patch = %#v", chatModel.RequestPatch)
	}
	if patchInt(t, chatModel.RequestPatch, "max_tokens") != 8192 {
		t.Fatalf("chat patch = %#v", chatModel.RequestPatch)
	}
	reasoning := patchObject(t, respModel.RequestPatch, "reasoning")
	if patchString(t, reasoning, "effort") != "high" {
		t.Fatalf("resp patch = %#v", respModel.RequestPatch)
	}
	if patchInt(t, respModel.RequestPatch, "max_output_tokens") != 8192 {
		t.Fatalf("resp patch = %#v", respModel.RequestPatch)
	}
	if _, ok := respModel.RequestPatch["max_tokens"]; ok {
		t.Fatalf("responses protocol must not receive max_tokens: %#v", respModel.RequestPatch)
	}
}

func TestTenantBaseURLOverrideKeepsCapabilities(t *testing.T) {
	cat := newTestCatalog(t, nil)
	overlay := mustOverlay(t, map[string]any{
		"providers": []any{
			map[string]any{"id": "chat", "api": map[string]any{"baseUrl": "https://new.example/v1"}},
		},
	}, LegacyAgent{})
	resolution := resolveFrom(t, cat, overlay)

	if len(resolution.Providers) != 3 {
		t.Fatalf("providers = %d, want the 3 catalog providers (sparse patch merges by id)", len(resolution.Providers))
	}
	model := mustFindModel(t, resolution, "chat", "deepseek-v4.1-flash")
	if model.API.BaseURL != "https://new.example/v1" {
		t.Fatalf("baseUrl = %q", model.API.BaseURL)
	}
	if model.Properties.ContextWindow != 128000 {
		t.Fatalf("capabilities must be inherited from catalog rules, contextWindow = %d", model.Properties.ContextWindow)
	}
	if model.Options.MaxOutputTokens.Max != 8192 {
		t.Fatalf("maxOutputTokens = %d", model.Options.MaxOutputTokens.Max)
	}
	if len(model.Options.ReasoningLevel.Values) != 4 {
		t.Fatalf("reasoning levels = %v", model.Options.ReasoningLevel.Values)
	}
	if !model.Executable || !model.Selectable {
		t.Fatalf("model should remain executable/selectable, issues: %v", issueMessages(model.Issues))
	}
}

func TestNullClearsField(t *testing.T) {
	cat := newTestCatalog(t, nil)
	overlay := mustOverlay(t, map[string]any{
		"providerModelRules": []any{
			map[string]any{
				"providerId": "chat",
				"modelId":    "deepseek-v4.1-flash",
				"config": map[string]any{
					"properties": map[string]any{
						"contextWindow":    nil,
						"supportsToolCall": nil,
					},
				},
			},
		},
	}, LegacyAgent{})
	resolution := resolveFrom(t, cat, overlay)
	model := mustFindModel(t, resolution, "chat", "deepseek-v4.1-flash")
	if model.Properties.ContextWindow != 0 {
		t.Fatalf("null must clear contextWindow, got %d", model.Properties.ContextWindow)
	}
	if model.Properties.SupportsToolCall {
		t.Fatalf("null must clear supportsToolCall")
	}
	if model.Executable {
		t.Fatalf("model without a context window must not be executable")
	}
	requireIssue(t, model.Issues, "properties.contextWindow", "contextWindow must be greater than zero")
	if model.Selectable {
		t.Fatalf("incomplete model must not be selectable")
	}
}

func TestIncompleteProviderAppearsInSettingsOnly(t *testing.T) {
	cat := newTestCatalog(t, nil)
	overlay := mustOverlay(t, map[string]any{
		"providers": []any{
			map[string]any{
				"id":       "broken",
				"modelIds": anyStrings("mystery-model"),
			},
		},
	}, LegacyAgent{})
	resolution := resolveFrom(t, cat, overlay)

	provider := mustFindProvider(t, resolution, "broken")
	if provider.Executable {
		t.Fatalf("incomplete provider must not be executable")
	}
	requireIssue(t, provider.Issues, "api.type", "")
	requireIssue(t, provider.Issues, "api.baseUrl", "base URL is required")
	requireIssue(t, provider.Issues, "access.type", "")
	requireIssue(t, provider.Issues, "access", "")
	if len(provider.Models) != 1 {
		t.Fatalf("settings view must retain the model entry, got %d", len(provider.Models))
	}
	if provider.Models[0].Selectable {
		t.Fatalf("model of an incomplete provider must not be selectable")
	}
	for _, model := range resolution.Models() {
		if model.ProviderID == "broken" {
			t.Fatalf("Models() must exclude the incomplete provider, got %#v", model)
		}
	}
	if len(resolution.Models()) != 3 {
		t.Fatalf("Models() = %d catalog models, want 3", len(resolution.Models()))
	}
	settings := resolution.Settings()
	if len(settings) != 4 {
		t.Fatalf("Settings() = %d providers, want 4 (3 catalog + broken)", len(settings))
	}
	if broken := mustFindProvider(t, resolution, "broken"); broken.Executable {
		t.Fatalf("broken provider must stay non-executable in settings")
	}
}

func TestDisabledModelInSettingsNotModels(t *testing.T) {
	cat := newTestCatalog(t, nil)
	overlay := mustOverlay(t, map[string]any{
		"providerModelRules": []any{
			map[string]any{
				"providerId": "chat",
				"modelId":    "deepseek-v4.1-flash",
				"config":     map[string]any{"enabled": false},
			},
		},
	}, LegacyAgent{})
	resolution := resolveFrom(t, cat, overlay)
	model := mustFindModel(t, resolution, "chat", "deepseek-v4.1-flash")
	if model.Enabled {
		t.Fatalf("model must be disabled")
	}
	if !model.Executable {
		t.Fatalf("disabled model should still be complete, issues: %v", issueMessages(model.Issues))
	}
	requireIssue(t, model.Issues, "enabled", "model is disabled")
	if model.Selectable {
		t.Fatalf("disabled model must not be selectable")
	}
	for _, listed := range resolution.Models() {
		if listed.ProviderID == "chat" && listed.ModelID == "deepseek-v4.1-flash" {
			t.Fatalf("Models() must exclude disabled models")
		}
	}

	factory := mustFactory(t, resolution)
	if _, err := factory.Resolve(Selection{ProviderID: "chat", ModelID: "deepseek-v4.1-flash"}); err == nil {
		t.Fatalf("factory must refuse a disabled model")
	}
}

func TestResolutionOrderIsCatalogOrder(t *testing.T) {
	doc := testCatalogDoc([]any{
		testProvider("z-provider", APITypeOpenAIChatCompletions, "https://z.example/v1", "Z_KEY", "m2", "m1"),
		testProvider("a-provider", APITypeOpenAIChatCompletions, "https://a.example/v1", "A_KEY", "m3"),
	})
	cat := loadTestCatalog(t, doc)
	resolution := resolveFrom(t, cat, Overlay{})
	if len(resolution.Providers) != 2 {
		t.Fatalf("providers = %d", len(resolution.Providers))
	}
	if resolution.Providers[0].ProviderID != "z-provider" || resolution.Providers[1].ProviderID != "a-provider" {
		t.Fatalf("provider order changed: %s, %s", resolution.Providers[0].ProviderID, resolution.Providers[1].ProviderID)
	}
	models := resolution.Providers[0].Models
	if len(models) != 2 || models[0].ModelID != "m2" || models[1].ModelID != "m1" {
		t.Fatalf("model order changed: %#v", models)
	}
	flat := resolution.Models()
	if len(flat) != 3 || flat[0].ModelID != "m2" || flat[1].ModelID != "m1" || flat[2].ModelID != "m3" {
		t.Fatalf("flat model order changed: %#v", flat)
	}
}

func TestOverlayReplacesRuleArraysWholesale(t *testing.T) {
	cat := newTestCatalog(t, func(doc map[string]any) {
		appendModelRules(doc, modelRuleForWindow("*deepseek*", 64000))
	})
	replacement := baselineModelRule()
	replacement["config"] = map[string]any{
		"properties": map[string]any{"contextWindow": 32000},
	}
	overlay := mustOverlay(t, map[string]any{
		"modelRules": []any{replacement},
	}, LegacyAgent{})
	resolution := resolveFrom(t, cat, overlay)
	model := mustFindModel(t, resolution, "chat", "deepseek-v4.1-flash")
	if model.Properties.ContextWindow != 32000 {
		t.Fatalf("tenant modelRules must replace the catalog array, contextWindow = %d", model.Properties.ContextWindow)
	}
}

func TestOverlayNullAddsNothing(t *testing.T) {
	// Tenant arrays are additions; null means "no addition" and must not remove
	// the shipped catalog rules.
	cat := newTestCatalog(t, nil)
	overlay := mustOverlay(t, map[string]any{"modelRules": nil, "providers": nil}, LegacyAgent{})
	resolution := resolveFrom(t, cat, overlay)
	model := mustFindModel(t, resolution, "chat", "deepseek-v4.1-flash")
	if !model.Executable || !model.Selectable {
		t.Fatalf("null tenant arrays must leave the catalog intact, issues: %v", issueMessages(model.Issues))
	}
}

func TestProviderModelRuleIntroducesUnknownModel(t *testing.T) {
	cat := newTestCatalog(t, func(doc map[string]any) {
		appendProviderModelRules(doc, map[string]any{
			"providerId": "chat",
			"modelId":    "brand-new-model",
			"config":     baselineModelRule()["config"],
		})
	})
	resolution := resolveFrom(t, cat, Overlay{})
	model := mustFindModel(t, resolution, "chat", "brand-new-model")
	if !model.Executable || !model.Selectable {
		t.Fatalf("unknown model with a complete exact rule must be executable, issues: %v", issueMessages(model.Issues))
	}
	if hasIssue(model.Issues, "properties", "baseline-derived capabilities; override with a model rule") {
		t.Fatalf("an exact provider model rule must clear the baseline-derived issue: %v", issueMessages(model.Issues))
	}
	factory := mustFactory(t, resolution)
	if _, err := factory.Resolve(Selection{ProviderID: "chat", ModelID: "brand-new-model"}); err != nil {
		t.Fatalf("factory resolve introduced model: %v", err)
	}
}

func TestNewResolverDoesNotMutateCatalog(t *testing.T) {
	cat := newTestCatalog(t, nil)
	overlay := mustOverlay(t, map[string]any{
		"providers": []any{
			testProvider("solo", APITypeOpenAIChatCompletions, "https://solo.example/v1", "SOLO_KEY", "solo-model"),
		},
	}, LegacyAgent{})
	if resolution := resolveFrom(t, cat, overlay); len(resolution.Providers) != 4 {
		t.Fatalf("overlay resolution providers = %d, want 4 (3 catalog + solo)", len(resolution.Providers))
	}
	resolution := resolveFrom(t, cat, Overlay{})
	if len(resolution.Providers) != 3 {
		t.Fatalf("catalog was mutated by overlay resolution: %d providers", len(resolution.Providers))
	}
	mustFindModel(t, resolution, "chat", "deepseek-v4.1-flash")
}

func TestCaseInsensitiveUnambiguousProviderFallback(t *testing.T) {
	doc := testCatalogDoc([]any{
		testProvider("solo", APITypeOpenAIChatCompletions, "https://solo.example/v1", "SOLO_KEY", "only-model"),
	})
	cat := loadTestCatalog(t, doc)
	resolution := resolveFrom(t, cat, Overlay{})
	factory := mustFactory(t, resolution)
	active, err := factory.Resolve(Selection{ModelID: "ONLY-MODEL"})
	if err != nil {
		t.Fatalf("unique model id must resolve without providerId: %v", err)
	}
	if active.ProviderID != "solo" || active.ModelID != "only-model" {
		t.Fatalf("active model = %#v", active)
	}
}

// hiddenProviderCatalog is a catalog with one visible and one hidden provider
// that share a model id, so both the identity path and the ambiguity path can
// be exercised against the visibility boundary.
func hiddenProviderCatalog(t *testing.T) *Catalog {
	t.Helper()
	hidden := testProvider("hidden-gw", APITypeOpenAIChatCompletions, "https://hidden.example/v1", "HIDDEN_KEY", "secret-model", "shared-model")
	hidden["visibility"] = VisibilityHidden
	return loadTestCatalog(t, testCatalogDoc([]any{
		testProvider("visible-gw", APITypeOpenAIChatCompletions, "https://visible.example/v1", "VISIBLE_KEY", "shared-model"),
		hidden,
	}))
}

// TestHiddenProviderIsNotResolvedButIsRefused: a hidden provider is not part of
// the resolution at all, so it cannot execute and its identity cannot leak.
// Both entry paths fail closed.
func TestHiddenProviderIsNotResolvedButIsRefused(t *testing.T) {
	resolution := resolveFrom(t, hiddenProviderCatalog(t), Overlay{})
	for _, provider := range resolution.Providers {
		if provider.ProviderID == "hidden-gw" {
			t.Fatalf("hidden provider leaked into the resolution: %#v", provider)
		}
	}
	factory := mustFactory(t, resolution)

	if _, err := factory.Resolve(Selection{ProviderID: "hidden-gw", ModelID: "secret-model"}); err == nil {
		t.Fatal("a hidden provider must not resolve")
	} else if !strings.Contains(err.Error(), "hidden-gw") || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("hidden refusal must name the unknown provider: %v", err)
	}
	if _, err := factory.Resolve(Selection{ProviderID: "hidden-gw"}); err == nil {
		t.Fatal("a hidden provider id alone must not resolve")
	}
	if _, ok := resolution.Find("hidden-gw", "secret-model"); ok {
		t.Fatal("Find must not see a hidden provider")
	}
	if _, err := factory.Resolve(Selection{ModelID: "secret-model"}); err == nil {
		t.Fatal("a model that only a hidden provider declares must be unknown")
	}
}

// TestAmbiguityDoesNotEnumerateHiddenProviders: the ambiguity diagnostic lists
// what a caller may choose. A hidden provider is not a choice, so a model id
// that is visible in one provider and hidden in another resolves to the visible
// one; a hidden-only id is simply unknown.
func TestAmbiguityDoesNotEnumerateHiddenProviders(t *testing.T) {
	resolution := resolveFrom(t, hiddenProviderCatalog(t), Overlay{})
	factory := mustFactory(t, resolution)

	active, err := factory.Resolve(Selection{ModelID: "shared-model"})
	if err != nil {
		t.Fatalf("hidden duplicates must not create an ambiguity: %v", err)
	}
	if active.ProviderID != "visible-gw" {
		t.Fatalf("resolved provider = %q, want the visible one", active.ProviderID)
	}

	_, err = factory.Resolve(Selection{ModelID: "secret-model"})
	if err == nil {
		t.Fatal("a hidden-only model id must not resolve")
	}
	if strings.Contains(err.Error(), "hidden-gw") {
		t.Fatalf("a diagnostic must never name a hidden provider: %v", err)
	}
}

// TestBaselineFlagMarksUndescribedModels: Baseline reports that no rule named
// the model, which is what servicehost uses to keep an invented window out of
// the compaction path.
func TestBaselineFlagMarksUndescribedModels(t *testing.T) {
	doc := testCatalogDoc([]any{
		testProvider("gw", APITypeOpenAIChatCompletions, "https://gw.example/v1", "GW_KEY", "described", "undescribed"),
	})
	doc["providerModelRules"] = []any{providerModelRuleForWindow("gw", "described", 65536)}
	cat := loadTestCatalog(t, doc)
	resolution := resolveFrom(t, cat, Overlay{})

	if described := mustFindModel(t, resolution, "gw", "described"); described.Baseline {
		t.Fatalf("a model named by a rule is not baseline-derived: %#v", described)
	}
	if undescribed := mustFindModel(t, resolution, "gw", "undescribed"); !undescribed.Baseline {
		t.Fatalf("a model nobody named must be baseline-derived: %#v", undescribed)
	}
}

// TestStaticHeadersMakeAProviderUnexecutable: no runtime driver can carry
// api.headers today, so a provider that declares them must not present itself
// as executable (the settings view shows the issue instead).
func TestStaticHeadersMakeAProviderUnexecutable(t *testing.T) {
	provider := testProvider("gw", APITypeOpenAIChatCompletions, "https://gw.example/v1", "GW_KEY", "model")
	provider["api"] = map[string]any{
		"type":    APITypeOpenAIChatCompletions,
		"baseUrl": "https://gw.example/v1",
		"headers": map[string]any{"x-api-version": "2024-10-01"},
	}
	cat := loadTestCatalog(t, testCatalogDoc([]any{provider}))
	resolution := resolveFrom(t, cat, Overlay{})

	view := mustFindProvider(t, resolution, "gw")
	if view.Executable {
		t.Fatalf("a header-carrying provider must not be executable: %#v", view)
	}
	requireIssue(t, view.Issues, "api.headers", "")
	if model := mustFindModel(t, resolution, "gw", "model"); model.Selectable {
		t.Fatalf("no model of an unexecutable provider may be selectable: %#v", model)
	}
}

// TestReasoningValuesOutsideTheRuntimeVocabulary: a model whose reasoning
// vocabulary the runtime cannot interpret is not executable, and the issue
// names the offender.
func TestReasoningValuesOutsideTheRuntimeVocabulary(t *testing.T) {
	doc := testCatalogDoc([]any{
		testProvider("gw", APITypeOpenAIChatCompletions, "https://gw.example/v1", "GW_KEY", "model"),
	})
	doc["providerModelRules"] = []any{
		map[string]any{
			"providerId": "gw",
			"modelId":    "model",
			"config": map[string]any{
				"options": map[string]any{
					"reasoningLevel": map[string]any{
						"values": anyStrings("off", "turbo"),
						"map": map[string]any{
							"off":   map[string]any{},
							"turbo": map[string]any{"reasoning_effort": "turbo"},
						},
					},
				},
			},
		},
	}
	cat := loadTestCatalog(t, doc)
	resolution := resolveFrom(t, cat, Overlay{})

	model := mustFindModel(t, resolution, "gw", "model")
	if model.Executable || model.Selectable {
		t.Fatalf("a model with a non-runtime reasoning level must not execute: %#v", model)
	}
	requireIssue(t, model.Issues, "options.reasoningLevel.values", `reasoning level "turbo" is not a runtime level (want off|minimal|low|medium|high|xhigh|max)`)
}

// TestSurfaceKindForType pins the provider family → session surface mapping.
// Only the families with a dedicated panel are mapped; everything else is the
// generic surface, never a silent llm.
func TestSurfaceKindForType(t *testing.T) {
	tests := map[string]string{
		"":          SurfaceKindLLM, // legacy: absent type is llm
		"llm":       SurfaceKindLLM,
		" LLM ":     SurfaceKindLLM,
		"realtime":  SurfaceKindRealtime,
		"Realtime":  SurfaceKindRealtime,
		"image":     SurfaceKindImage,
		"video":     SurfaceKindVideo,
		"tts":       SurfaceKindSpeech,
		"stt":       SurfaceKindSpeech,
		"generic":   SurfaceKindGeneric,
		"embedding": SurfaceKindGeneric, // a plugin family with no panel
	}
	for providerType, want := range tests {
		if got := SurfaceKindForType(providerType); got != want {
			t.Fatalf("SurfaceKindForType(%q) = %q, want %q", providerType, got, want)
		}
	}
}

// TestLegacyProviderDefaultsToLLM pins the upgrade promise: a document written
// before provider types existed resolves as an llm provider on both views, so
// every existing deployment keeps its picker and session surface unchanged.
func TestLegacyProviderDefaultsToLLM(t *testing.T) {
	doc := testCatalogDoc([]any{
		testProvider("legacy", APITypeOpenAIChatCompletions, "https://legacy.example/v1", "LEGACY_KEY", "legacy-model"),
	})
	resolution := resolveFrom(t, loadTestCatalog(t, doc), Overlay{})

	provider := mustFindProvider(t, resolution, "legacy")
	if provider.ProviderType != ProviderTypeLLM || provider.SurfaceKind != SurfaceKindLLM {
		t.Fatalf("legacy provider = %s/%s, want llm/llm", provider.ProviderType, provider.SurfaceKind)
	}
	model := mustFindModel(t, resolution, "legacy", "legacy-model")
	if model.ProviderType != ProviderTypeLLM || model.SurfaceKind != SurfaceKindLLM {
		t.Fatalf("legacy model = %s/%s, want llm/llm", model.ProviderType, model.SurfaceKind)
	}
}

// TestRealtimeProviderReachesThePicker pins the blocker: a provider declared
// with type realtime, and a model that declares audio output, resolves to a
// selectable model whose views carry providerType/surfaceKind on the wire.
func TestRealtimeProviderReachesThePicker(t *testing.T) {
	doc := testCatalogDoc([]any{
		testProviderWithType("omni", ProviderTypeRealtime,
			APITypeOpenAIChatCompletions, "https://omni.example/v1", "OMNI_KEY", "qwen-omni"),
	})
	doc["providerModelRules"] = []any{
		map[string]any{
			"providerId": "omni",
			"modelId":    "qwen-omni",
			"config": map[string]any{
				"properties": map[string]any{
					"inputFormat":  map[string]any{"text": true, "audio": true},
					"outputFormat": map[string]any{"text": true, "audio": true},
				},
			},
		},
	}
	resolution := resolveFrom(t, loadTestCatalog(t, doc), Overlay{})

	provider := mustFindProvider(t, resolution, "omni")
	if provider.ProviderType != ProviderTypeRealtime || provider.SurfaceKind != SurfaceKindRealtime {
		t.Fatalf("realtime provider = %s/%s, want realtime/realtime", provider.ProviderType, provider.SurfaceKind)
	}
	model := mustFindModel(t, resolution, "omni", "qwen-omni")
	if model.ProviderType != ProviderTypeRealtime || model.SurfaceKind != SurfaceKindRealtime {
		t.Fatalf("realtime model = %s/%s, want realtime/realtime", model.ProviderType, model.SurfaceKind)
	}
	if !model.Properties.OutputFormat.Text || !model.Properties.OutputFormat.Audio {
		t.Fatalf("realtime outputFormat = %+v, want text and audio", model.Properties.OutputFormat)
	}
	if !model.Properties.InputFormat.Audio {
		t.Fatalf("realtime inputFormat = %+v, want audio input", model.Properties.InputFormat)
	}
	if !model.Executable || !model.Selectable {
		t.Fatalf("a complete realtime model must be selectable, issues: %v", issueMessages(model.Issues))
	}

	picker := resolution.Models()
	found := false
	for _, candidate := range picker {
		if candidate.ProviderID == "omni" && candidate.ModelID == "qwen-omni" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the realtime model is missing from the picker projection: %+v", picker)
	}

	wire, err := json.Marshal(model)
	if err != nil {
		t.Fatalf("marshal model view: %v", err)
	}
	for _, want := range []string{`"providerType":"realtime"`, `"surfaceKind":"realtime"`, `"audio":true`} {
		if !strings.Contains(string(wire), want) {
			t.Fatalf("model view JSON missing %s: %s", want, wire)
		}
	}
}

// TestUnknownProviderTypeFloorsToGeneric pins the documented policy: the family
// vocabulary is open, so an unrecognized type is accepted (not rejected) and
// resolves to the generic surface, which the client renders as "no dedicated
// panel" instead of a fake chat model.
func TestUnknownProviderTypeFloorsToGeneric(t *testing.T) {
	doc := testCatalogDoc([]any{
		testProviderWithType("holo", "hologram",
			APITypeOpenAIChatCompletions, "https://holo.example/v1", "HOLO_KEY", "holo-model"),
	})
	resolution := resolveFrom(t, loadTestCatalog(t, doc), Overlay{})

	provider := mustFindProvider(t, resolution, "holo")
	if provider.ProviderType != "hologram" || provider.SurfaceKind != SurfaceKindGeneric {
		t.Fatalf("unknown family = %s/%s, want hologram/generic", provider.ProviderType, provider.SurfaceKind)
	}
	model := mustFindModel(t, resolution, "holo", "holo-model")
	if model.ProviderType != "hologram" || model.SurfaceKind != SurfaceKindGeneric {
		t.Fatalf("unknown family model = %s/%s, want hologram/generic", model.ProviderType, model.SurfaceKind)
	}
}
