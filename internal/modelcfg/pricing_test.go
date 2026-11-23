package modelcfg

// Pricing is the model's declared price list (per 1M tokens) for display and
// estimates. These tests pin the vocabulary (currency + four token rates + the
// free marker + notes), the additive merge behaviour, the projection into
// ModelView / ActiveModel-adjacent views, and the boundary that matters most:
// pricing is metadata only — it never reaches the resolved request patch and a
// model without a price stays executable.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func priceConfig(values map[string]any) map[string]any {
	pricing := map[string]any{}
	for key, value := range values {
		pricing[key] = value
	}
	return map[string]any{"pricing": pricing}
}

func pricingRule(match string, values map[string]any) map[string]any {
	return map[string]any{"match": match, "config": priceConfig(values)}
}

func providerPricingRule(providerID, modelID string, values map[string]any) map[string]any {
	return map[string]any{"providerId": providerID, "modelId": modelID, "config": priceConfig(values)}
}

// TestPricingVocabularyRoundTrips: a complete price block survives
// catalog → resolve → ModelView untouched, per-field pointers included, and
// exports through the JSON projection the API serves.
func TestPricingVocabularyRoundTrips(t *testing.T) {
	doc := map[string]any{
		"schemaVersion": 1,
		"providers":     baseProviders(),
		"modelRules": []any{
			baselineModelRule(),
			pricingRule("deepseek-v4.1-flash", map[string]any{
				"currency":   "USD",
				"input":      0.27,
				"output":     1.10,
				"cacheRead":  0.07,
				"cacheWrite": 0.27,
				"free":       false,
				"notes":      "list price, 2026-11",
			}),
		},
		"modelApiRules":      protocolModelAPIRules(),
		"siteRules":          []any{},
		"providerModelRules": []any{},
	}
	cat := loadTestCatalog(t, doc)
	model := mustFindModel(t, resolveFrom(t, cat, Overlay{}), "chat", "deepseek-v4.1-flash")

	pricing := model.Pricing
	if pricing == nil {
		t.Fatal("the resolved model must carry its declared pricing")
	}
	if pricing.Currency != "USD" || pricing.Free || pricing.Notes != "list price, 2026-11" {
		t.Fatalf("pricing scalars = %+v", pricing)
	}
	for name, got := range map[string]*float64{
		"input":      pricing.Input,
		"output":     pricing.Output,
		"cacheRead":  pricing.CacheRead,
		"cacheWrite": pricing.CacheWrite,
	} {
		if got == nil {
			t.Fatalf("pricing.%s is nil, want the declared number", name)
		}
	}
	if *pricing.Input != 0.27 || *pricing.Output != 1.10 || *pricing.CacheRead != 0.07 || *pricing.CacheWrite != 0.27 {
		t.Fatalf("pricing rates = %+v", pricing)
	}

	raw, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	var projected map[string]any
	if err := json.Unmarshal(raw, &projected); err != nil {
		t.Fatal(err)
	}
	block, ok := projected["pricing"].(map[string]any)
	if !ok {
		t.Fatalf("the ModelView JSON projection lost the pricing block: %s", raw)
	}
	for key, want := range map[string]float64{
		"input": 0.27, "output": 1.1, "cacheRead": 0.07, "cacheWrite": 0.27,
	} {
		if got, _ := block[key].(float64); got != want {
			t.Fatalf("projected pricing.%s = %v, want %v", key, block[key], want)
		}
	}
}

// TestPricingIsMetadataOnly: a model with no price is executable, a priced
// model's request patch carries no price key, and the factory's ActiveModel
// still exposes the display facts through the view (not the patch).
func TestPricingIsMetadataOnly(t *testing.T) {
	doc := map[string]any{
		"schemaVersion": 1,
		"providers":     baseProviders(),
		"modelRules": []any{
			baselineModelRule(),
			pricingRule("deepseek-v4.1-flash", map[string]any{"input": 1.5}),
		},
		"modelApiRules":      protocolModelAPIRules(),
		"siteRules":          []any{},
		"providerModelRules": []any{},
	}
	cat := loadTestCatalog(t, doc)
	resolution := resolveFrom(t, cat, Overlay{})
	priced := mustFindModel(t, resolution, "chat", "deepseek-v4.1-flash")
	unpriced := mustFindModel(t, resolution, "anth", "claude-test")

	if !priced.Executable {
		t.Fatalf("a priced model must stay executable: %v", issueMessages(priced.Issues))
	}
	if unpriced.Pricing != nil {
		t.Fatalf("an unpriced model must report no pricing block, got %+v", unpriced.Pricing)
	}
	if !unpriced.Executable {
		t.Fatalf("an unpriced model must be executable: %v", issueMessages(unpriced.Issues))
	}

	factory := mustFactory(t, resolution)
	active, err := factory.Resolve(Selection{ProviderID: "chat", ModelID: "deepseek-v4.1-flash", ReasoningLevel: "high"})
	if err != nil {
		t.Fatalf("resolve priced model: %v", err)
	}
	raw, err := json.Marshal(active.RequestPatch)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"pricing", "price", "cost", "currency"} {
		if strings.Contains(strings.ToLower(string(raw)), forbidden) {
			t.Fatalf("the request patch carried %q: %s", forbidden, raw)
		}
	}
}

// TestPricingAdditiveMergeAndUnits: tenant/model/provider-model rules each
// contribute fields, a later rule wins per field, and null clears a block.
func TestPricingAdditiveMergeAndUnits(t *testing.T) {
	doc := map[string]any{
		"schemaVersion": 1,
		"providers":     baseProviders(),
		"modelRules": []any{
			baselineModelRule(),
			pricingRule("*", map[string]any{"currency": "USD", "input": 1.0}),
			pricingRule("*deepseek*", map[string]any{"input": 2.0}),
		},
		"modelApiRules": protocolModelAPIRules(),
		"siteRules":     []any{},
		"providerModelRules": []any{
			providerPricingRule("chat", "deepseek-v4.1-flash", map[string]any{"output": 8.0}),
		},
	}
	cat := loadTestCatalog(t, doc)
	model := mustFindModel(t, resolveFrom(t, cat, Overlay{}), "chat", "deepseek-v4.1-flash")
	if model.Pricing == nil || model.Pricing.Input == nil || *model.Pricing.Input != 2.0 {
		t.Fatalf("the later model rule must win per field: %+v", model.Pricing)
	}
	if model.Pricing.Output == nil || *model.Pricing.Output != 8.0 {
		t.Fatalf("a provider-model rule contributes its own field: %+v", model.Pricing)
	}
	if model.Pricing.Currency != "USD" {
		t.Fatalf("currency inherited from the wildcard rule = %q", model.Pricing.Currency)
	}
}

// TestPricingNullClearsBlocks: transitioning from a priced model to an
// unpriced one is expressible (null clears the block, JSON Merge Patch style).
func TestPricingNullClearsBlocks(t *testing.T) {
	doc := map[string]any{
		"schemaVersion": 1,
		"providers":     baseProviders(),
		"modelRules": []any{
			baselineModelRule(),
			pricingRule("*", map[string]any{"input": 1.0}),
			map[string]any{"match": "deepseek-v4.1-flash", "config": map[string]any{"pricing": nil}},
		},
		"modelApiRules":      protocolModelAPIRules(),
		"siteRules":          []any{},
		"providerModelRules": []any{},
	}
	cat := loadTestCatalog(t, doc)
	model := mustFindModel(t, resolveFrom(t, cat, Overlay{}), "chat", "deepseek-v4.1-flash")
	if model.Pricing != nil {
		t.Fatalf("a null must clear the pricing block, got %+v", model.Pricing)
	}
	if !model.Executable {
		t.Fatalf("clearing a price must not disable the model: %v", issueMessages(model.Issues))
	}
}

// TestPricingValidationRefusesUnknownAndBadValues: the vocabulary is closed and
// the value domains are checked with field paths.
func TestPricingValidationRefusesUnknownAndBadValues(t *testing.T) {
	tests := []struct {
		name    string
		config  map[string]any
		wantErr string
	}{
		{
			name:    "unknown pricing key",
			config:  map[string]any{"pricing": map[string]any{"inputPrice": 1.0}},
			wantErr: `unknown field "inputPrice"`,
		},
		{
			name:    "unknown model config key",
			config:  map[string]any{"cost": map[string]any{"input": 1.0}},
			wantErr: `unknown field "cost"`,
		},
		{
			name:    "negative price",
			config:  map[string]any{"pricing": map[string]any{"input": -1.0}},
			wantErr: "input must be a non-negative number",
		},
		{
			name:    "non-string price",
			config:  map[string]any{"pricing": map[string]any{"output": "cheap"}},
			wantErr: "output must be a non-negative number",
		},
		{
			name:    "bad currency shape",
			config:  map[string]any{"pricing": map[string]any{"currency": "usd"}},
			wantErr: "currency must be a three-letter",
		},
		{
			name:    "free not a boolean",
			config:  map[string]any{"pricing": map[string]any{"free": "yes"}},
			wantErr: "free must be a boolean",
		},
		{
			name:    "notes with a line break",
			config:  map[string]any{"pricing": map[string]any{"notes": "a\nb"}},
			wantErr: "notes must not contain line breaks",
		},
		{
			name:    "pricing not an object",
			config:  map[string]any{"pricing": []any{1.0}},
			wantErr: "pricing must be an object",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateModelConfigMap(test.config)
			if err == nil {
				t.Fatal("the configuration was accepted")
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), test.wantErr)
			}
		})
	}
}

// TestPricingFreeMarkerOnly: a block of only `free: true` (the local-model /
// free-tier shape) is legal and survives the projection — the marker is what
// makes "$0" readable as a published free model rather than a missing price.
func TestPricingFreeMarkerOnly(t *testing.T) {
	config := priceConfig(map[string]any{"free": true})
	if err := validateModelConfigMap(config); err != nil {
		t.Fatalf("a free-only block was refused: %v", err)
	}
	decoded, err := decodeModelConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Pricing == nil || !decoded.Pricing.Free {
		t.Fatalf("decoded pricing = %+v", decoded.Pricing)
	}
	if decoded.Pricing.Input != nil || decoded.Pricing.Output != nil {
		t.Fatal("a free-only block must not invent a rate")
	}
}

// TestPricingCloneIsDeep: a settings read must not be able to mutate the
// resolution a generation displays (a shared *float64 would).
func TestPricingCloneIsDeep(t *testing.T) {
	resolution := Resolution{Providers: []ProviderView{{
		ProviderID: "p",
		Models: []ModelView{{
			ProviderID: "p",
			ModelID:    "m",
			Pricing:    &Pricing{Input: ptrFloat(1.25), Currency: "USD"},
		}},
	}}}
	read := resolution.Settings()
	if read[0].Models[0].Pricing == nil {
		t.Fatal("the read dropped the pricing block")
	}
	*read[0].Models[0].Pricing.Input = 99.0
	if *resolution.Providers[0].Models[0].Pricing.Input != 1.25 {
		t.Fatal("mutating a settings read changed the resolution")
	}
}

func ptrFloat(value float64) *float64 { return &value }

// TestPricingUnknownKeyRejectedThroughTheDocumentPath pins the same refusal at
// the rule layer: a rule config carrying an unknown model key never loads.
func TestPricingUnknownKeyRejectedThroughTheDocumentPath(t *testing.T) {
	raw := `{"schemaVersion":1,"providers":[` +
		`{"id":"chat","api":{"type":"openai-chat-completions","baseUrl":"https://chat.example/v1"},` +
		`"access":{"type":"api-key","env":"CHAT_KEY"},"modelIds":["deepseek-v4.1-flash"]}],` +
		`"modelRules":[{"match":"*","config":{"pricing":{"input":1,"cache-read":2}}}]}`
	if _, err := LoadCatalog([]byte(raw)); err == nil {
		t.Fatal("an unknown pricing key loaded")
	} else if !strings.Contains(err.Error(), `unknown field "cache-read"`) {
		t.Fatalf("error = %v, want the unknown key named", err)
	}
	// Round-trip sanity for the valid shape used by the same builder.
	valid := priceConfig(map[string]any{"currency": "USD", "cacheRead": 0.5})
	if err := validateModelConfigMap(valid); err != nil {
		t.Fatalf("the valid shape was refused: %v", err)
	}
	if !reflect.DeepEqual(map[string]any{"currency": "USD", "cacheRead": 0.5}, valid["pricing"]) {
		t.Fatal("the builder is not identity")
	}
}

// TestPricingThroughChainRule: a tenant overlay's rule (the settings document's
// shape) reaches the resolved model.
func TestPricingThroughChainRule(t *testing.T) {
	cat := newTestCatalog(t, nil)
	overlay := mustOverlay(t, map[string]any{
		"modelRules": []any{
			pricingRule("deepseek-v4.1-flash", map[string]any{"currency": "CNY", "input": 0.5, "free": false}),
		},
	}, LegacyAgent{})
	model := mustFindModel(t, resolveFrom(t, cat, overlay), "chat", "deepseek-v4.1-flash")
	if model.Pricing == nil || model.Pricing.Currency != "CNY" || model.Pricing.Input == nil || *model.Pricing.Input != 0.5 {
		t.Fatalf("tenant pricing = %+v", model.Pricing)
	}
}
