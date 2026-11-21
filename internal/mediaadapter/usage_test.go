package mediaadapter

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestUsageHasNoPricingFields pins the usage contract: objective counts only.
// Pricing belongs to the contract side, so a struct field named cost/price/
// currency is a regression, not a future feature.
func TestUsageHasNoPricingFields(t *testing.T) {
	usageType := reflect.TypeOf(Usage{})
	for i := 0; i < usageType.NumField(); i++ {
		name := strings.ToLower(usageType.Field(i).Name)
		jsonName := strings.ToLower(usageType.Field(i).Tag.Get("json"))
		for _, forbidden := range []string{"cost", "price", "currency", "usd", "billing"} {
			if strings.Contains(name, forbidden) || strings.Contains(jsonName, forbidden) {
				t.Fatalf("Usage carries pricing field %s (%s)", usageType.Field(i).Name, jsonName)
			}
		}
	}

	raw, err := json.Marshal(Usage{
		InputTokens: 1, OutputTokens: 2, Questions: 3, Images: 4, Videos: 5,
		InputCharacters: 6, TranscriptCharacters: 7, AudioBytes: 8, AudioMillis: 9, Bytes: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"cost", "price", "currency", "usd"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("usage JSON leaked %q: %s", forbidden, raw)
		}
	}
	t.Logf("usage JSON=%s", raw)
}

func TestDecisionUsageDropsUpstreamCost(t *testing.T) {
	response := systemOneResponse{
		Model:   "jev-1.14",
		Answers: map[string]json.RawMessage{"ok": json.RawMessage(`{"type":"noul","noul":0.5}`)},
	}
	response.Usage.InputTokens = 12
	response.Usage.OutputTokens = 3
	usage := decisionUsage(response, 1)
	if usage.InputTokens != 12 || usage.OutputTokens != 3 || usage.Questions != 1 {
		t.Fatalf("usage = %+v", usage)
	}
	raw, _ := json.Marshal(usage)
	if strings.Contains(string(raw), "cost") {
		t.Fatalf("upstream cost leaked: %s", raw)
	}
}
