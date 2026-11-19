package mcphost

// Resource `_meta.ui` parsing (apps.mdx §UI Resource Format). The block is
// untrusted server input that ends up in a response header (CSP), in an iframe
// permission policy (P3) and in the catalogue: every member is validated, and
// an unusable member is dropped with a diagnostic while the rest of the block
// still applies. Dropping (never guessing) is what keeps "只收紧不放宽" true.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// maxDomainBytes bounds the resource `_meta.ui.domain` value. It names the
// app's own domain (display only in P2); an unbounded value is a prompt/audit
// growth vector.
const maxDomainBytes = 253

// parseResourceMeta reads the resource-level `_meta` into the typed UI block.
// A nil/garbage `_meta` or a non-object `ui` yields the zero value plus a
// diagnostic: the app still renders under the strict defaults.
func parseResourceMeta(raw json.RawMessage) (UIResourceMeta, []string) {
	var meta UIResourceMeta
	if len(raw) == 0 || isJSONNull(raw) {
		return meta, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return meta, []string{"_meta is not an object"}
	}
	uiRaw, ok := fields["ui"]
	if !ok || isJSONNull(uiRaw) {
		return meta, nil
	}
	var ui map[string]json.RawMessage
	if err := json.Unmarshal(uiRaw, &ui); err != nil || ui == nil {
		return meta, []string{"_meta.ui is not an object"}
	}

	var diagnostics []string
	for key := range ui {
		switch key {
		case "csp", "permissions", "domain", "prefersBorder":
		default:
			diagnostics = append(diagnostics, fmt.Sprintf("_meta.ui.%s is not a known key; ignored", safeName(key)))
		}
	}

	if cspRaw, ok := ui["csp"]; ok && !isJSONNull(cspRaw) {
		csp, notes := parseCSPBlock(cspRaw)
		diagnostics = append(diagnostics, notes...)
		meta.CSP = csp
	}
	if permissionsRaw, ok := ui["permissions"]; ok && !isJSONNull(permissionsRaw) {
		permissions, notes := parsePermissions(permissionsRaw)
		diagnostics = append(diagnostics, notes...)
		meta.Permissions = permissions
	}
	if domainRaw, ok := ui["domain"]; ok && !isJSONNull(domainRaw) {
		var domain string
		if err := json.Unmarshal(domainRaw, &domain); err != nil {
			diagnostics = append(diagnostics, "_meta.ui.domain is not a string; ignored")
		} else if reason := domainProblem(domain); reason != "" {
			diagnostics = append(diagnostics, "_meta.ui.domain "+reason+"; ignored")
		} else {
			meta.Domain = strings.ToLower(domain)
		}
	}
	if borderRaw, ok := ui["prefersBorder"]; ok && !isJSONNull(borderRaw) {
		var prefers bool
		if err := json.Unmarshal(borderRaw, &prefers); err != nil {
			diagnostics = append(diagnostics, "_meta.ui.prefersBorder is not a boolean; ignored")
		} else {
			meta.PrefersBorder = &prefers
		}
	}
	return meta, diagnostics
}

// parseCSPBlock reads the four domain lists. An unknown key is ignored with a
// diagnostic: there is no path from server input to a directive.
func parseCSPBlock(raw json.RawMessage) (*CSPMeta, []string) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, []string{"_meta.ui.csp is not an object; using the strict default"}
	}
	var diagnostics []string
	for key := range fields {
		switch key {
		case "connectDomains", "resourceDomains", "frameDomains", "baseUriDomains":
		default:
			diagnostics = append(diagnostics, fmt.Sprintf("_meta.ui.csp.%s is not a known key; ignored", safeName(key)))
		}
	}
	csp := &CSPMeta{}
	for key, target := range map[string]*[]string{
		"connectDomains":  &csp.ConnectDomains,
		"resourceDomains": &csp.ResourceDomains,
		"frameDomains":    &csp.FrameDomains,
		"baseUriDomains":  &csp.BaseURIDomains,
	} {
		rawList, ok := fields[key]
		if !ok || isJSONNull(rawList) {
			continue
		}
		values, notes := parseStringList(rawList, "_meta.ui.csp."+key)
		diagnostics = append(diagnostics, notes...)
		*target = values
	}
	return csp, diagnostics
}

// parsePermissions reads the declared browser permissions. Only the four
// spec-defined names are understood; a non-boolean or unknown entry is dropped
// with a diagnostic (and, from P3, never granted).
func parsePermissions(raw json.RawMessage) (map[string]bool, []string) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, []string{"_meta.ui.permissions is not an object; ignored"}
	}
	var diagnostics []string
	permissions := make(map[string]bool, len(fields))
	for key, rawValue := range fields {
		if !knownPermissions[key] {
			diagnostics = append(diagnostics, fmt.Sprintf("_meta.ui.permissions.%s is not a known permission; ignored", safeName(key)))
			continue
		}
		var granted bool
		if err := json.Unmarshal(rawValue, &granted); err != nil {
			diagnostics = append(diagnostics, fmt.Sprintf("_meta.ui.permissions.%s is not a boolean; ignored", safeName(key)))
			continue
		}
		permissions[key] = granted
	}
	if len(permissions) == 0 {
		return nil, diagnostics
	}
	return permissions, diagnostics
}

// parseStringList decodes a JSON array of strings, dropping non-string and
// empty entries with a diagnostic.
func parseStringList(raw json.RawMessage, field string) ([]string, []string) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
		return nil, []string{field + " is not an array; ignored"}
	}
	var diagnostics []string
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		var value string
		if err := json.Unmarshal(entry, &value); err != nil {
			diagnostics = append(diagnostics, field+" has a non-string entry; dropped")
			continue
		}
		if strings.TrimSpace(value) == "" {
			diagnostics = append(diagnostics, field+" has an empty entry; dropped")
			continue
		}
		out = append(out, value)
	}
	return out, diagnostics
}

// domainProblem names why `_meta.ui.domain` is unusable: it is a display
// hostname, so a scheme, a path, a port, whitespace or an overlong value is
// rejected.
func domainProblem(domain string) string {
	if domain == "" {
		return "is empty"
	}
	if len(domain) > maxDomainBytes {
		return fmt.Sprintf("is longer than %d bytes", maxDomainBytes)
	}
	for _, r := range domain {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.', r == '-':
		default:
			return "is not a bare hostname"
		}
	}
	return ""
}

// safeName renders an untrusted object key inside a diagnostic: controls and
// line separators removed, bounded to 48 bytes.
func safeName(value string) string { return safeSnippetBounded(value, 48) }

func safeSnippetBounded(value string, max int) string {
	var builder strings.Builder
	count := 0
	for _, r := range value {
		if r < 0x20 || r == 0x7f || r == '\u2028' || r == '\u2029' {
			continue
		}
		size := len(string(r))
		if count+size > max {
			builder.WriteString("…")
			break
		}
		builder.WriteRune(r)
		count += size
	}
	return builder.String()
}

// isJSONNull mirrors the mcp package's helper for local use.
func isJSONNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}
