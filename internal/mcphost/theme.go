package mcphost

// Theme mapping: the host's design tokens (web/theme.json) projected onto the
// MCP Apps standard CSS variables (design §3.4). The View keeps its own
// defaults for variables the host does not provide: an unknown token is
// omitted, never invented.

import (
	"encoding/json"
	"strings"
)

// themeVariableMap is the explicit, one-directional mapping table from Keel
// tokens to MCP Apps variables. The order is the table order (it is only used
// for documentation and tests); the result is a map.
//
// design §3.4: --color-background-success/--color-background-warning fall back
// to accent when the theme has no success/warning token; --shadow-hairline uses
// the glass-edge family and is omitted when absent.
var themeVariableMap = []struct {
	Variable string
	Tokens   []string
}{
	{"--color-background-primary", []string{"card"}},
	{"--color-background-secondary", []string{"panel"}},
	{"--color-background-tertiary", []string{"muted"}},
	{"--color-text-primary", []string{"foreground"}},
	{"--color-text-secondary", []string{"muted-foreground"}},
	{"--color-border-primary", []string{"border"}},
	{"--color-border-secondary", []string{"header-border"}},
	{"--color-background-danger", []string{"destructive"}},
	{"--color-background-success", []string{"success", "accent"}},
	{"--color-background-warning", []string{"warning", "accent"}},
	{"--color-ring-primary", []string{"ring"}},
	{"--font-sans", []string{"font-sans"}},
	{"--font-mono", []string{"font-mono"}},
	{"--border-radius-sm", []string{"radius-control"}},
	{"--border-radius-md", []string{"radius"}},
	{"--border-radius-lg", []string{"radius-card"}},
	{"--border-radius-full", []string{"radius-pill"}},
	{"--shadow-hairline", []string{"glass-edge"}},
}

// ThemeVariables maps one mode's tokens. The mode map is consulted first, then
// the theme-wide map (radius/font tokens live there), matching how the web
// build applies cssVars.
func ThemeVariables(theme json.RawMessage, dark bool) map[string]string {
	mode, shared, ok := themeTokenMaps(theme, dark)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(themeVariableMap))
	for _, mapping := range themeVariableMap {
		for _, token := range mapping.Tokens {
			value, present := mode[token]
			if !present {
				value, present = shared[token]
			}
			if !present || strings.TrimSpace(value) == "" {
				continue
			}
			out[mapping.Variable] = value
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ThemePalettes maps both modes, the shape the app catalogue serves: the host
// process does not know which mode the client renders in, so it publishes both
// and the View picks the one its own host context reports.
func ThemePalettes(theme json.RawMessage) map[string]map[string]string {
	light := ThemeVariables(theme, false)
	dark := ThemeVariables(theme, true)
	if light == nil && dark == nil {
		return nil
	}
	out := map[string]map[string]string{}
	if light != nil {
		out["light"] = light
	}
	if dark != nil {
		out["dark"] = dark
	}
	return out
}

// themeTokenMaps decodes `cssVars.<mode>` and `cssVars.theme`. A document that
// is not an object, or carries no cssVars object, is unusable (ok=false).
func themeTokenMaps(theme json.RawMessage, dark bool) (map[string]string, map[string]string, bool) {
	if len(theme) == 0 {
		return nil, nil, false
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(theme, &document); err != nil || document == nil {
		return nil, nil, false
	}
	cssVarsRaw, ok := document["cssVars"]
	if !ok {
		return nil, nil, false
	}
	var cssVars map[string]json.RawMessage
	if err := json.Unmarshal(cssVarsRaw, &cssVars); err != nil || cssVars == nil {
		return nil, nil, false
	}
	mode := modeTokens(cssVars, dark)
	shared := decodeStringMap(cssVars["theme"])
	if mode == nil && shared == nil {
		return nil, nil, false
	}
	return mode, shared, true
}

// modeTokens decodes cssVars.light or cssVars.dark.
func modeTokens(cssVars map[string]json.RawMessage, dark bool) map[string]string {
	if dark {
		return decodeStringMap(cssVars["dark"])
	}
	return decodeStringMap(cssVars["light"])
}

// decodeStringMap decodes a flat token object. Non-string values are skipped:
// a theme document is host-owned, but the mapping must never publish a
// non-CSS-string value.
func decodeStringMap(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil
	}
	out := make(map[string]string, len(fields))
	for key, rawValue := range fields {
		var value string
		if err := json.Unmarshal(rawValue, &value); err != nil {
			continue
		}
		out[key] = value
	}
	return out
}
