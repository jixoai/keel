package mcphost

// CSP construction (spec「CSP 构造：只收紧不放宽」, design §3.7). The host owns
// every directive; the resource `_meta.ui.csp` block contributes domain lists
// only. A value that is not a plain http(s) origin is dropped (with a
// diagnostic), so a server cannot widen a directive with `*`, `data:`,
// `'unsafe-eval'` or an injected `; directive` of its own.

import (
	"fmt"
	"net/url"
	"strings"
)

// CSP directive limits. They bound the response header (a server must not be
// able to grow every view response with thousands of domains) and the
// diagnostic size.
const (
	maxCSPDomainsPerDirective = 16
	maxCSPDomainBytes         = 256
)

// Permissions an app may declare (apps.mdx §UI Resource Format). The host
// grants them only after user confirmation (P3); P2 parses and publishes them.
var knownPermissions = map[string]bool{
	"camera":         true,
	"microphone":     true,
	"geolocation":    true,
	"clipboardWrite": true,
}

// BuildCSP constructs the Content-Security-Policy value of one app document.
//
// The output is the spec's directive set in a fixed order, plus the
// `sandbox allow-scripts` directive that keeps a directly-navigated document in
// an opaque origin:
//
//	default-src 'none'; script-src 'self' 'unsafe-inline' <resourceDomains>;
//	style-src 'self' 'unsafe-inline' <resourceDomains>;
//	img-src 'self' data: <resourceDomains>;
//	media-src 'self' data: <resourceDomains>;
//	font-src 'self' <resourceDomains>;
//	connect-src 'self' <connectDomains>;
//	frame-src <frameDomains|'none'>; object-src 'none';
//	base-uri <baseUriDomains|'self'>; frame-ancestors 'self'
//
// A nil meta (no `_meta.ui.csp`) yields the strict default: no external
// connection, no frames, base-uri 'self'. Diagnostics name every dropped
// domain; dropping is the收紧 direction, never a silent widening.
func BuildCSP(meta *CSPMeta) (string, []string) {
	var declared CSPMeta
	if meta != nil {
		declared = *meta
	}
	var diagnostics []string
	resource := sanitizeCSPDomains(declared.ResourceDomains, "resourceDomains", &diagnostics)
	connect := sanitizeCSPDomains(declared.ConnectDomains, "connectDomains", &diagnostics)
	frames := sanitizeCSPDomains(declared.FrameDomains, "frameDomains", &diagnostics)
	base := sanitizeCSPDomains(declared.BaseURIDomains, "baseUriDomains", &diagnostics)

	directives := []string{
		// The document is server-controlled HTML served from the host origin.
		// The CSP sandbox makes it an opaque origin even when a user navigates
		// to the URL directly (the iframe attribute covers the embedded case):
		// without it, a direct visit would run a server's scripts on the host
		// origin with the user's cookies. allow-scripts is the only grant, so
		// forms, popups, top navigation and storage stay closed.
		"sandbox allow-scripts",
		"default-src 'none'",
		directive("script-src", append([]string{"'self'", "'unsafe-inline'"}, resource...)),
		directive("style-src", append([]string{"'self'", "'unsafe-inline'"}, resource...)),
		directive("img-src", append([]string{"'self'", "data:"}, resource...)),
		directive("media-src", append([]string{"'self'", "data:"}, resource...)),
		directive("font-src", append([]string{"'self'"}, resource...)),
		directive("connect-src", append([]string{"'self'"}, connect...)),
		withValue("frame-src", orNone(frames)),
		"object-src 'none'",
		withValue("base-uri", orSelf(base)),
		// The host is the only embedder: a leaked document URL must not become
		// an embeddable surface on another site.
		"frame-ancestors 'self'",
	}
	return strings.Join(directives, "; "), diagnostics
}

func directive(name string, values []string) string {
	return name + " " + strings.Join(values, " ")
}

func withValue(name, value string) string { return name + " " + value }

func orNone(values []string) string {
	if len(values) == 0 {
		return "'none'"
	}
	return strings.Join(values, " ")
}

func orSelf(values []string) string {
	if len(values) == 0 {
		return "'self'"
	}
	return strings.Join(values, " ")
}

// sanitizeCSPDomains validates and normalizes one declared domain list. The
// accepted shape is a plain http(s) origin, optionally with a wildcard
// subdomain (`https://*.example.com`): a scheme, a host, an optional port, and
// nothing else. Everything else is dropped with a diagnostic.
func sanitizeCSPDomains(values []string, field string, diagnostics *[]string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, raw := range values {
		if len(out) >= maxCSPDomainsPerDirective {
			*diagnostics = append(*diagnostics, fmt.Sprintf("%s: more than %d domains; the rest were dropped",
				field, maxCSPDomainsPerDirective))
			break
		}
		origin, reason := normalizeCSPDomain(raw)
		if reason != "" {
			*diagnostics = append(*diagnostics, fmt.Sprintf("%s: %s", field, reason))
			continue
		}
		if seen[origin] {
			continue
		}
		seen[origin] = true
		out = append(out, origin)
	}
	return out
}

// normalizeCSPDomain returns the normalized origin, or a reason it is unusable.
// The returned origin is safe to concatenate into the header: it contains only
// the scheme separator, host characters and an optional port.
func normalizeCSPDomain(raw string) (string, string) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", "empty domain dropped"
	}
	if len(value) > maxCSPDomainBytes {
		return "", fmt.Sprintf("a domain longer than %d bytes was dropped", maxCSPDomainBytes)
	}
	// Reject the source expressions that would widen a directive instead of
	// naming an origin.
	if strings.HasPrefix(value, "'") || value == "*" || strings.HasPrefix(value, "*.") {
		return "", fmt.Sprintf("%q is not an http(s) origin; dropped", safeSnippet(value))
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Sprintf("%q is not a valid origin; dropped", safeSnippet(value))
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Sprintf("%q does not use http(s); dropped", safeSnippet(value))
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", fmt.Sprintf("%q is not a plain origin; dropped", safeSnippet(value))
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Sprintf("%q carries a path; dropped", safeSnippet(value))
	}
	if parsed.Host == "" || parsed.Hostname() == "" {
		return "", fmt.Sprintf("%q has no host; dropped", safeSnippet(value))
	}
	if !validCSPHost(parsed.Host) {
		return "", fmt.Sprintf("%q has an unexpected host shape; dropped", safeSnippet(value))
	}
	return parsed.Scheme + "://" + parsed.Host, ""
}

// validCSPHost checks the characters that may appear in a CSP host: letters,
// digits, dot, dash, colon (port), brackets (IPv6) and one leading wildcard
// label. Quotes, semicolons, commas and spaces would allow injecting another
// directive and are rejected.
func validCSPHost(host string) bool {
	host = strings.TrimPrefix(host, "*.")
	if host == "" {
		return false
	}
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.', r == '-', r == ':', r == '[', r == ']':
		default:
			return false
		}
	}
	return true
}

// safeSnippet renders an untrusted value inside a diagnostic: controls and
// line separators removed, bounded to 64 bytes, so a server cannot forge log
// lines or grow the diagnostic without bound.
func safeSnippet(value string) string {
	var builder strings.Builder
	count := 0
	for _, r := range value {
		if r < 0x20 || r == 0x7f || r == '\u2028' || r == '\u2029' {
			continue
		}
		if count+len(string(r)) > 64 {
			builder.WriteString("…")
			break
		}
		builder.WriteRune(r)
		count += len(string(r))
	}
	return builder.String()
}
