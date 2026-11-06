// Behaviour ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/src/input-schema.ts (MIT License); implemented independently in Go.
// Contract kept identical; see docs/UPSTREAM.md.

package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// NormalizeInputSchema returns an MCP tool's input schema in the shape LLM tool
// APIs accept: an object schema. Tool parameters must be objects, MCP servers
// may omit "type", and some providers reject object schemas without
// "properties"; every other keyword is kept as it was.
//
// A missing or null "type" becomes "object" (pi uses ??, which also covers
// null), and a missing "properties" becomes {}. A "properties" value that is
// present, even null, is kept: pi only adds the key when it is undefined.
//
// Invalid JSON is an error. So is valid JSON that is not an object; pi's
// Record<string, unknown> parameter makes those unrepresentable at the type
// level, while a Go caller could otherwise pass them silently.
func NormalizeInputSchema(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("mcp: normalize input schema: empty JSON")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return nil, fmt.Errorf("mcp: normalize input schema: %w", err)
	}
	if fields == nil { // JSON null: pi spreads it as an empty object.
		fields = make(map[string]json.RawMessage, 2)
	}
	if typ, ok := fields["type"]; !ok || isJSONNull(typ) {
		fields["type"] = json.RawMessage(`"object"`)
	}
	if _, ok := fields["properties"]; !ok {
		fields["properties"] = json.RawMessage(`{}`)
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("mcp: normalize input schema: %w", err)
	}
	return out, nil
}

// isJSONNull reports whether raw is the JSON literal null.
func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}
