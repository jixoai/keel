// MCP Apps tool metadata: the `_meta.ui` block a server attaches to a tool
// (SEP-1865, extension io.modelcontextprotocol/ui). This file parses the block
// into a typed value the rest of the package can trust, keeps the deprecated
// flat key `_meta["ui/resourceUri"]` readable, and derives the host-side app
// identity (appId) from the server name and the resource URI.
//
// The model-facing tool surface is NOT affected by any naming rule here: v2
// exposes MCP only through the aggregate `mcp` tool (docs/PI-PARITY.md §4.2),
// so the app catalogue below is an internal binding table, never a source of
// model-visible tool names.

package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// Tool visibility values of `_meta.ui.visibility` (apps.mdx §Visibility).
const (
	VisibilityModel = "model"
	VisibilityApp   = "app"
)

// uiScheme is the only URI scheme a UI resource may use (apps.mdx §UI Resource
// Format).
const uiScheme = "ui://"

// maxUIResourceURIBytes bounds a declared UI resource URI. The URI reaches the
// system prompt and, from P2, cache paths and HTTP responses; a server must not
// be able to grow those with an unbounded string.
const maxUIResourceURIBytes = 256

// AppMIMEType is the only media type an MCP app document may declare.
const AppMIMEType = "text/html;profile=mcp-app"

// DefaultToolVisibility applies when a tool declares no `_meta.ui.visibility`:
// the tool is both a model tool and an app tool.
var DefaultToolVisibility = []string{VisibilityModel, VisibilityApp}

// ToolUI is the parsed `_meta.ui` block of a tool. A nil Visibility means the
// block declared none (the default applies); a non-nil empty slice means the
// server explicitly declared an empty visibility list.
type ToolUI struct {
	ResourceURI string   `json:"resourceUri,omitempty"`
	Visibility  []string `json:"visibility,omitempty"`
}

// VisibilityList returns the effective visibility. The caller gets a copy: the
// default slice is package state and must not be mutable by callers.
func (ui ToolUI) VisibilityList() []string {
	if ui.Visibility == nil {
		return append([]string(nil), DefaultToolVisibility...)
	}
	return append([]string(nil), ui.Visibility...)
}

// Allows reports whether the tool is visible to one audience ("model" or
// "app").
func (ui ToolUI) Allows(audience string) bool {
	for _, value := range ui.VisibilityList() {
		if value == audience {
			return true
		}
	}
	return false
}

// MCPAppBinding is the app identity a tool result carries in its Details, so a
// client can mount the app from the ledger alone (design §2.4).
type MCPAppBinding struct {
	AppID       string   `json:"appId"`
	ResourceURI string   `json:"resourceUri"`
	Visibility  []string `json:"visibility"`
}

// MCPToolDetails is the Details payload of the aggregate `mcp` tool's call
// action. It keeps the PI-era fields (server/tool/isError/fullOutputPath) and
// adds the two MCP Apps fields: the app binding and the bounded structured
// content. Details travel in tool.result and never enter the model context.
type MCPToolDetails struct {
	Server            string          `json:"server"`
	Tool              string          `json:"tool"`
	IsError           bool            `json:"isError,omitempty"`
	FullOutputPath    string          `json:"fullOutputPath,omitempty"`
	App               *MCPAppBinding  `json:"app,omitempty"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	// StructuredContentTruncated marks a structured result that exceeded
	// StructuredContentMaxBytes; the field then holds only the bounded marker
	// document, never a silently mangled JSON value.
	StructuredContentTruncated bool `json:"structuredContentTruncated,omitempty"`
}

// StructuredContentMaxBytes bounds the structured content copied into Details.
// The full CallToolResult stays available in-process (and, from P3, over the
// app bridge); the ledger copy is capped so one server cannot grow every
// tool.result without bound.
const StructuredContentMaxBytes = 256 * 1024

// BoundStructuredContent returns the Details copy of a structuredContent value.
// Values within max are returned verbatim. A larger value becomes a bounded
// marker document `{"_meta":{"truncated":true,"originalBytes":N}}` and
// truncated is true: the JSON structure is never silently mangled into
// something that still parses.
//
// The marker shape is NOT authoritative: it is ordinary JSON a server could
// have produced itself. Consumers decide "truncated" from the boolean field
// (MCPToolDetails.StructuredContentTruncated), never by inspecting the marker.
func BoundStructuredContent(raw json.RawMessage, max int) (json.RawMessage, bool) {
	if len(raw) == 0 || max <= 0 || len(raw) <= max {
		return raw, false
	}
	marker, err := json.Marshal(map[string]any{
		"_meta": map[string]any{
			"truncated":     true,
			"originalBytes": len(raw),
		},
	})
	if err != nil {
		return nil, true
	}
	return marker, true
}

// AppIDFor derives the host-side app identity: appId is never a server-supplied
// string (design §3.1). The shape mirrors the tool-name hash suffix style.
func AppIDFor(server, resourceURI string) string {
	sum := sha256.Sum256([]byte(server + "\x00" + resourceURI))
	return "mcpapp_" + hex.EncodeToString(sum[:8])
}

// parseToolUI reads the `_meta.ui` block of a tool. The deprecated flat key
// `_meta["ui/resourceUri"]` stays readable; when both keys are present and the
// structured one is valid it wins. Malformed shapes are reported as diagnostics
// and never fail the catalogue: a broken app declaration must not take a
// server's tools down.
func parseToolUI(meta json.RawMessage) (ToolUI, []string) {
	var ui ToolUI
	if len(meta) == 0 || isJSONNull(meta) {
		return ui, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(meta, &fields); err != nil || fields == nil {
		return ui, []string{"_meta is not an object"}
	}

	var diagnostics []string
	var structured map[string]json.RawMessage
	structuredValid := false
	if raw, ok := fields["ui"]; ok && !isJSONNull(raw) {
		if err := json.Unmarshal(raw, &structured); err != nil || structured == nil {
			diagnostics = append(diagnostics, "_meta.ui is not an object")
		} else {
			structuredValid = true
		}
	}

	if structuredValid {
		if raw, ok := structured["resourceUri"]; ok && !isJSONNull(raw) {
			var uri string
			if err := json.Unmarshal(raw, &uri); err != nil {
				diagnostics = append(diagnostics, "_meta.ui.resourceUri is not a string")
			} else if reason := uiResourceURIProblem(uri); reason != "" {
				diagnostics = append(diagnostics, "_meta.ui.resourceUri "+reason)
			} else {
				ui.ResourceURI = uri
			}
		}
		if raw, ok := structured["visibility"]; ok && !isJSONNull(raw) {
			visibility, reason := parseVisibility(raw)
			if reason != "" {
				diagnostics = append(diagnostics, "_meta.ui.visibility "+reason)
			} else {
				ui.Visibility = visibility
			}
		}
	}

	// Deprecated flat key. It only carries a resource URI; visibility stays at
	// the default. It is consulted when the structured block did not produce a
	// usable URI.
	if ui.ResourceURI == "" {
		if raw, ok := fields["ui/resourceUri"]; ok && !isJSONNull(raw) {
			var uri string
			if err := json.Unmarshal(raw, &uri); err != nil {
				diagnostics = append(diagnostics, `_meta["ui/resourceUri"] is not a string`)
			} else if reason := uiResourceURIProblem(uri); reason != "" {
				diagnostics = append(diagnostics, `_meta["ui/resourceUri"] `+reason)
			} else {
				ui.ResourceURI = uri
				diagnostics = append(diagnostics, `deprecated _meta["ui/resourceUri"] used`)
			}
		}
	}
	return ui, diagnostics
}

// uiResourceURIProblem names why a declared resource URI is not usable, or ""
// when it is. A URI is untrusted server input that ends up in the system prompt:
// control characters (C0/C1, line/paragraph separators) could forge prompt
// structure, and an unbounded length could push every run's prompt around.
func uiResourceURIProblem(uri string) string {
	if !strings.HasPrefix(uri, uiScheme) {
		return fmt.Sprintf("%q does not use the ui:// scheme", uri)
	}
	if len(uri) == len(uiScheme) {
		return "is empty after ui://"
	}
	if len(uri) > maxUIResourceURIBytes {
		return fmt.Sprintf("is longer than %d bytes", maxUIResourceURIBytes)
	}
	for _, r := range uri {
		if isPromptUnsafeRune(r) {
			return "contains control characters"
		}
	}
	return ""
}

// isPromptUnsafeRune reports runes that can break or forge prompt structure.
// Beyond C0/C1 controls (unicode.IsControl) the Unicode line and paragraph
// separators count: they are line breaks to many renderers even though they are
// not Cc.
func isPromptUnsafeRune(r rune) bool {
	return unicode.IsControl(r) || r == '\u2028' || r == '\u2029'
}

// parseVisibility validates `_meta.ui.visibility`. The whole value falls back
// to the default when any entry is not a known audience: a partially understood
// list must not silently hide a tool from the model.
func parseVisibility(raw json.RawMessage) ([]string, string) {
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, "is not an array of strings"
	}
	if values == nil {
		return nil, "is not an array of strings"
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		switch value {
		case VisibilityModel, VisibilityApp:
			if !seen[value] {
				seen[value] = true
				out = append(out, value)
			}
		default:
			return nil, fmt.Sprintf("has unknown value %q", value)
		}
	}
	return out, ""
}
