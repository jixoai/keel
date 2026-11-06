// Tests ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi), packages/mcp/test/input-schema.test.ts
// (MIT License). Each subtest keeps the title of the pi test it ports.

package mcp

import (
	"encoding/json"
	"reflect"
	"testing"
)

// assertJSONEqual compares two JSON documents semantically, since Go maps
// marshal with sorted keys while pi's object spread preserves insertion order.
func assertJSONEqual(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("result is not JSON: %v (raw %q)", err, got)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("want is not JSON: %v", err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("schema = %s, want %s", got, want)
	}
}

func TestNormalizeInputSchema(t *testing.T) {
	t.Run("adds type object and empty properties when missing", func(t *testing.T) {
		tests := []struct{ name, raw, want string }{
			{name: "empty schema", raw: `{}`, want: `{"type":"object","properties":{}}`},
			{name: "type only", raw: `{"type":"object"}`, want: `{"type":"object","properties":{}}`},
			// Supplementary: pi uses ??, so a null type also falls back.
			{name: "null type", raw: `{"type":null}`, want: `{"type":"object","properties":{}}`},
			// Supplementary: a null schema spreads to an empty object in pi.
			{name: "null schema", raw: `null`, want: `{"type":"object","properties":{}}`},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				got, err := NormalizeInputSchema(json.RawMessage(test.raw))
				if err != nil {
					t.Fatalf("NormalizeInputSchema(%s): %v", test.raw, err)
				}
				assertJSONEqual(t, got, test.want)
			})
		}
	})

	t.Run("keeps the schema's own keywords", func(t *testing.T) {
		tests := []struct{ name, raw, want string }{
			{
				name: "full schema",
				raw:  `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`,
				want: `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`,
			},
			{
				name: "$defs",
				raw:  `{"properties":{"a":{}},"$defs":{"x":{}}}`,
				want: `{"type":"object","properties":{"a":{}},"$defs":{"x":{}}}`,
			},
			{
				// Supplementary: a present null properties is kept, as pi only
				// adds the key when it is undefined.
				name: "null properties",
				raw:  `{"properties":null}`,
				want: `{"type":"object","properties":null}`,
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				got, err := NormalizeInputSchema(json.RawMessage(test.raw))
				if err != nil {
					t.Fatalf("NormalizeInputSchema(%s): %v", test.raw, err)
				}
				assertJSONEqual(t, got, test.want)
			})
		}
	})

	t.Run("rejects invalid JSON", func(t *testing.T) {
		for _, raw := range []string{``, `   `, `{`, `{"type":`} {
			if _, err := NormalizeInputSchema(json.RawMessage(raw)); err == nil {
				t.Errorf("NormalizeInputSchema(%q) = nil error, want one", raw)
			}
		}
	})

	t.Run("rejects JSON that is not an object", func(t *testing.T) {
		// Supplementary: pi's Record<string, unknown> parameter makes these
		// unrepresentable at the type level, so the Go port rejects them.
		for _, raw := range []string{`[]`, `"object"`, `5`, `true`} {
			if _, err := NormalizeInputSchema(json.RawMessage(raw)); err == nil {
				t.Errorf("NormalizeInputSchema(%q) = nil error, want one", raw)
			}
		}
	})
}
