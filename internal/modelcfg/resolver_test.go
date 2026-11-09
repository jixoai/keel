package modelcfg

import (
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
