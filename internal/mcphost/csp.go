package mcphost

// CSP construction (spec「CSP 构造：只收紧不放宽」, design §3.7). The host owns
// every directive; the resource `_meta.ui.csp` block contributes domain lists
// only. A value that is not a plain http(s) origin is dropped (with a
// diagnostic), so a server cannot widen a directive with `*`, `data:`,
// `'unsafe-eval'` or an injected `; directive` of its own.
//
// Two host rules keep a declared "domain" from becoming a blanket grant:
//
//   - a wildcard must keep at least two labels (`https://*.example.com` is a
//     subdomain grant; `https://*.com` would be the whole TLD) and may not name
//     loopback (`*.localhost`) or an IP;
//   - loopback and private/link-local addresses are refused unless the
//     deployment explicitly opts in (AllowPrivateDomains), so a document
//     cannot point the browser at the host's own services.

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// CSP directive limits. They bound the response header (a server must not be
// able to grow every view response with thousands of domains) and the
// diagnostic size.
const (
	maxCSPDomainsPerDirective = 16
	maxCSPDomainBytes         = 256
)

// CSPOptions is the host's domain policy for one app document. The zero value
// is the production policy: public http(s) origins only.
type CSPOptions struct {
	// AllowPrivateDomains admits loopback and private/link-local origins. It
	// exists for dev fixtures (a local MCP server panel) and is off by default:
	// a View is browser JavaScript, and a private origin gives it a path to the
	// host's own network services.
	AllowPrivateDomains bool
}

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
// domain; dropping is the收紧 direction, never a silent widening. The list is
// bounded (see maxAppDiagnostics): a hostile block cannot amplify into an
// unbounded diagnostic set.
func BuildCSP(meta *CSPMeta, options CSPOptions) (string, []string) {
	var declared CSPMeta
	if meta != nil {
		declared = *meta
	}
	diagnostics := newDiagCollector(maxAppDiagnostics)
	resource := sanitizeCSPDomains(declared.ResourceDomains, "resourceDomains", options, diagnostics)
	connect := sanitizeCSPDomains(declared.ConnectDomains, "connectDomains", options, diagnostics)
	frames := sanitizeCSPDomains(declared.FrameDomains, "frameDomains", options, diagnostics)
	base := sanitizeCSPDomains(declared.BaseURIDomains, "baseUriDomains", options, diagnostics)

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
	return strings.Join(directives, "; "), diagnostics.finish()
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
//
// The scan is bounded twice: at most maxCSPDomainsPerDirective accepted
// origins, and at most maxAppDiagnostics inspected failures (once the
// diagnostic budget is spent the remaining entries are counted and dropped —
// fail closed, never widen).
func sanitizeCSPDomains(values []string, field string, options CSPOptions, diagnostics *diagCollector) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for index, raw := range values {
		if len(out) >= maxCSPDomainsPerDirective {
			diagnostics.addDropped(len(values) - index)
			diagnostics.addf("%s: more than %d domains; the rest were dropped", field, maxCSPDomainsPerDirective)
			break
		}
		if diagnostics.full() {
			diagnostics.addDropped(len(values) - index)
			break
		}
		origin, reason := normalizeCSPDomain(raw, options)
		if reason != "" {
			diagnostics.addf("%s: %s", field, reason)
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
// The returned origin is safe to concatenate into the header: a lowercase host
// (DNS is case-insensitive) and an optional port, with IPv6 bracketed.
func normalizeCSPDomain(raw string, options CSPOptions) (string, string) {
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
	hostname := parsed.Hostname()
	if parsed.Host == "" || hostname == "" {
		return "", fmt.Sprintf("%q has no host; dropped", safeSnippet(value))
	}
	if !validCSPHostname(hostname) {
		return "", fmt.Sprintf("%q has an unexpected host shape; dropped", safeSnippet(value))
	}
	port := parsed.Port()
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", fmt.Sprintf("%q has an invalid port; dropped", safeSnippet(value))
		}
	}

	host := strings.TrimRight(hostname, ".")
	if host == "" {
		return "", fmt.Sprintf("%q has an empty host; dropped", safeSnippet(value))
	}
	// A trailing dot is the FQDN form; it is normalized away above so the
	// wildcard checks below cannot be bypassed with `*.com.`.
	if strings.HasPrefix(host, "*.") {
		base := host[2:]
		if base == "" || !strings.Contains(base, ".") {
			return "", fmt.Sprintf("%q is a wildcard over a single label; dropped", safeSnippet(value))
		}
		if strings.EqualFold(base, "localhost") {
			return "", fmt.Sprintf("%q is a wildcard over loopback; dropped", safeSnippet(value))
		}
		if net.ParseIP(base) != nil {
			return "", fmt.Sprintf("%q is a wildcard over an IP address; dropped", safeSnippet(value))
		}
	}
	if problem := domainPolicyProblem(host, options); problem != "" {
		return "", fmt.Sprintf("%q %s; dropped", safeSnippet(value), problem)
	}

	out := strings.ToLower(host)
	if strings.Contains(out, ":") {
		out = "[" + out + "]"
	}
	if port != "" {
		out += ":" + port
	}
	return parsed.Scheme + "://" + out, ""
}

// validCSPHostname accepts a DNS name (a leading `*.` label allowed) or an IP
// literal. Quotes, semicolons, commas and spaces would allow injecting another
// directive and are rejected; a `*` anywhere but the leading label is rejected
// because it is not a subgroup grant.
func validCSPHostname(name string) bool {
	if net.ParseIP(name) != nil {
		return true
	}
	if strings.HasPrefix(name, "*.") {
		name = name[2:]
	}
	if name == "" || strings.Contains(name, "*") {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.', r == '-':
		default:
			return false
		}
	}
	return true
}

// domainPolicyProblem names why a host is not an acceptable CSP domain: the
// loopback/private policy and the numeric-host rule (browsers may read an
// all-numeric host as an IP, so `2130706433` must not slip past the IP checks).
func domainPolicyProblem(host string, options CSPOptions) string {
	name := strings.ToLower(host)
	base := strings.TrimPrefix(name, "*.")
	if base == "localhost" || strings.HasSuffix(base, ".localhost") {
		if !options.AllowPrivateDomains {
			return "is a loopback host"
		}
	}
	if ip := net.ParseIP(base); ip != nil {
		if !options.AllowPrivateDomains && privateIP(ip) {
			return "is a loopback/private address"
		}
		return ""
	}
	if allNumericLabels(base) {
		return "is a numeric host"
	}
	return ""
}

// privateIP reports the address classes the default policy refuses: loopback,
// RFC1918/ULA, link-local, and the unspecified address.
func privateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// allNumericLabels reports whether every dot-separated label is decimal digits
// (the wide-integer form of an IPv4 address).
func allNumericLabels(host string) bool {
	if host == "" {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" {
			return false
		}
		for _, r := range label {
			if r < '0' || r > '9' {
				return false
			}
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
