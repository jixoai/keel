package mcphost

import (
	"fmt"
	"strings"
	"testing"
)

// cspDirective extracts one directive's value from the built header.
func cspDirective(header, name string) string {
	for _, directive := range strings.Split(header, "; ") {
		if strings.HasPrefix(directive, name+" ") {
			return strings.TrimPrefix(directive, name+" ")
		}
	}
	return "\x00missing"
}

// cspValues returns the accepted origins of one directive, without the fixed
// keyword sources.
func cspValues(header, name string) []string {
	value := cspDirective(header, name)
	if value == "\x00missing" {
		return nil
	}
	var out []string
	for _, token := range strings.Fields(value) {
		if strings.HasPrefix(token, "'") || token == "data:" {
			continue
		}
		out = append(out, token)
	}
	return out
}

// TestBuildCSPStrictDefault pins the missing-`csp` default: every directive is
// present with exactly the spec's strict value, and no external connection is
// allowed.
func TestBuildCSPStrictDefault(t *testing.T) {
	header, diagnostics := BuildCSP(nil, CSPOptions{})
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
	want := map[string]string{
		"sandbox":         "allow-scripts",
		"default-src":     "'none'",
		"script-src":      "'self' 'unsafe-inline'",
		"style-src":       "'self' 'unsafe-inline'",
		"img-src":         "'self' data:",
		"media-src":       "'self' data:",
		"font-src":        "'self'",
		"connect-src":     "'self'",
		"frame-src":       "'none'",
		"object-src":      "'none'",
		"base-uri":        "'self'",
		"frame-ancestors": "'self'",
	}
	for name, value := range want {
		if got := cspDirective(header, name); got != value {
			t.Errorf("%s = %q, want %q (header: %s)", name, got, value, header)
		}
	}
	// No directive beyond the spec's set: the host never adds one.
	if count := len(strings.Split(header, "; ")); count != len(want) {
		t.Fatalf("header has %d directives, want %d: %s", count, len(want), header)
	}
}

// TestBuildCSPDeclaredDomains pins each directive's domain routing: a
// resource domain reaches script/style/img/media/font, a connect domain reaches
// connect only, a frame domain reaches frame only.
func TestBuildCSPDeclaredDomains(t *testing.T) {
	header, diagnostics := BuildCSP(&CSPMeta{
		ResourceDomains: []string{"https://cdn.example.com", "https://cdn.example.com"},
		ConnectDomains:  []string{"https://api.example.com"},
		FrameDomains:    []string{"https://embed.example.com"},
		BaseURIDomains:  []string{"https://base.example.com"},
	}, CSPOptions{})
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
	want := map[string]string{
		"sandbox":         "allow-scripts",
		"default-src":     "'none'",
		"script-src":      "'self' 'unsafe-inline' https://cdn.example.com",
		"style-src":       "'self' 'unsafe-inline' https://cdn.example.com",
		"img-src":         "'self' data: https://cdn.example.com",
		"media-src":       "'self' data: https://cdn.example.com",
		"font-src":        "'self' https://cdn.example.com",
		"connect-src":     "'self' https://api.example.com",
		"frame-src":       "https://embed.example.com",
		"object-src":      "'none'",
		"base-uri":        "https://base.example.com",
		"frame-ancestors": "'self'",
	}
	for name, value := range want {
		if got := cspDirective(header, name); got != value {
			t.Errorf("%s = %q, want %q (header: %s)", name, got, value, header)
		}
	}
	if cspDirective(header, "script-src") == cspDirective(header, "connect-src") {
		t.Fatal("resource and connect domains were merged")
	}
}

// TestBuildCSPTightensOnly is the negative case the spec calls for: a server
// can never widen a directive. Non-origin source expressions, injected
// directives, loopback and wildcard-TLD hosts are dropped, and
// default-src/object-src stay 'none' whatever the block says.
func TestBuildCSPTightensOnly(t *testing.T) {
	header, diagnostics := BuildCSP(&CSPMeta{
		ResourceDomains: []string{
			"*",
			"'unsafe-eval'",
			"data:",
			"https://ok.example.com; default-src *",
			"https://ok.example.com",
		},
		ConnectDomains: []string{"javascript:alert(1)", "http://localhost:3000"},
	}, CSPOptions{})
	if len(diagnostics) != 6 {
		t.Fatalf("diagnostics = %d (%v), want 6 dropped entries", len(diagnostics), diagnostics)
	}
	if got := cspDirective(header, "default-src"); got != "'none'" {
		t.Fatalf("default-src = %q, a server widened it", got)
	}
	if got := cspDirective(header, "object-src"); got != "'none'" {
		t.Fatalf("object-src = %q", got)
	}
	script := cspDirective(header, "script-src")
	if strings.Contains(script, "*") || strings.Contains(script, "data:") || strings.Contains(script, "unsafe-eval") {
		t.Fatalf("script-src was widened: %q", script)
	}
	if !strings.Contains(script, "https://ok.example.com") {
		t.Fatalf("the valid resource origin was dropped: %q", script)
	}
	if strings.Contains(header, "default-src *") {
		t.Fatalf("a server injected a directive: %q", header)
	}
	if got := cspDirective(header, "connect-src"); got != "'self'" {
		t.Fatalf("connect-src = %q, a loopback origin was accepted by default", got)
	}
}

// TestBuildCSPWildcardRequiresTwoLabels pins the P1-A fix: a wildcard over a
// single label (`*.com`) grants the whole TLD and is refused, with or without
// the FQDN trailing dot; `*.example.com` stays an accepted subgroup grant.
func TestBuildCSPWildcardRequiresTwoLabels(t *testing.T) {
	for _, raw := range []string{
		"https://*.com",
		"https://*.net",
		"https://*.localhost",
		"https://*.com.",
		"https://*.x",
		"https://*",
		"https://*.127.0.0.1",
	} {
		header, diagnostics := BuildCSP(&CSPMeta{ConnectDomains: []string{raw}}, CSPOptions{})
		if len(diagnostics) != 1 {
			t.Errorf("%s: diagnostics = %v, want one drop", raw, diagnostics)
		}
		if got := cspValues(header, "connect-src"); len(got) != 0 {
			t.Errorf("%s was accepted: connect-src = %v", raw, got)
		}
	}

	header, diagnostics := BuildCSP(&CSPMeta{ConnectDomains: []string{"https://*.example.com"}}, CSPOptions{})
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
	if got := cspValues(header, "connect-src"); len(got) != 1 || got[0] != "https://*.example.com" {
		t.Fatalf("connect-src = %v, want the wildcard subgroup grant", got)
	}

	// The FQDN trailing dot is normalized away, not a bypass.
	header, diagnostics = BuildCSP(&CSPMeta{ConnectDomains: []string{"https://*.Example.com."}}, CSPOptions{})
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
	if got := cspValues(header, "connect-src"); len(got) != 1 || got[0] != "https://*.example.com" {
		t.Fatalf("connect-src = %v, want the normalized wildcard", got)
	}
}

// TestBuildCSPPrivateDomains pins P2-1: loopback and private/link-local
// addresses are refused by default and admitted only with the explicit dev
// option. Numeric host forms of IPv4 stay refused either way.
func TestBuildCSPPrivateDomains(t *testing.T) {
	private := []string{
		"http://127.0.0.1",
		"http://127.0.0.1:8080",
		"http://[::1]:8080",
		"http://10.1.2.3",
		"http://172.16.0.9",
		"http://192.168.1.10",
		"http://169.254.1.1",
		"http://localhost",
		"http://localhost:3000",
		"https://sub.localhost",
		"http://2130706433",
		"http://0.0.0.0",
	}
	for _, raw := range private {
		header, diagnostics := BuildCSP(&CSPMeta{ConnectDomains: []string{raw}}, CSPOptions{})
		if len(diagnostics) != 1 {
			t.Errorf("%s: diagnostics = %v, want a default-policy drop", raw, diagnostics)
		}
		if got := cspValues(header, "connect-src"); len(got) != 0 {
			t.Errorf("%s was accepted by default: %v", raw, got)
		}
	}

	// The dev fixture opts in: loopback/private origins are admitted.
	allow := CSPOptions{AllowPrivateDomains: true}
	for _, raw := range []string{"http://127.0.0.1:8080", "http://[::1]:8080", "http://localhost:3000", "http://10.1.2.3"} {
		header, diagnostics := BuildCSP(&CSPMeta{ConnectDomains: []string{raw}}, allow)
		if len(diagnostics) != 0 {
			t.Errorf("%s: diagnostics = %v with the dev option", raw, diagnostics)
		}
		if got := cspValues(header, "connect-src"); len(got) != 1 {
			t.Errorf("%s: connect-src = %v with the dev option", raw, got)
		}
	}
	// The option is about address policy, not origin shape: an all-numeric
	// host is still not an origin.
	if _, diagnostics := BuildCSP(&CSPMeta{ConnectDomains: []string{"http://2130706433"}}, allow); len(diagnostics) != 1 {
		t.Fatalf("a numeric host was accepted with the dev option: %v", diagnostics)
	}
	// A public origin is accepted under both policies.
	for _, options := range []CSPOptions{{}, allow} {
		if _, diagnostics := BuildCSP(&CSPMeta{ConnectDomains: []string{"https://api.example.com"}}, options); len(diagnostics) != 0 {
			t.Fatalf("a public origin was refused: %v", diagnostics)
		}
	}
}

// TestBuildCSPPortRange pins P2-6: a port outside 1..65535 is not an origin.
func TestBuildCSPPortRange(t *testing.T) {
	for _, raw := range []string{"https://api.example.com:0", "https://api.example.com:65536", "https://api.example.com:999999"} {
		header, diagnostics := BuildCSP(&CSPMeta{ConnectDomains: []string{raw}}, CSPOptions{})
		if len(diagnostics) != 1 {
			t.Errorf("%s: diagnostics = %v, want a drop", raw, diagnostics)
		}
		if got := cspValues(header, "connect-src"); len(got) != 0 {
			t.Errorf("%s was accepted: %v", raw, got)
		}
	}
	header, diagnostics := BuildCSP(&CSPMeta{ConnectDomains: []string{"https://api.example.com:65535", "http://api2.example.com:80"}}, CSPOptions{})
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
	if got := cspValues(header, "connect-src"); len(got) != 2 {
		t.Fatalf("connect-src = %v, want both in-range ports", got)
	}
}

// TestBuildCSPDomainBounds pins the count bound: the header stays bounded even
// when a server declares thousands of domains.
func TestBuildCSPDomainBounds(t *testing.T) {
	domains := make([]string, 0, 40)
	for index := 0; index < 40; index++ {
		domains = append(domains, fmt.Sprintf("https://host%d.example.com", index))
	}
	header, diagnostics := BuildCSP(&CSPMeta{ResourceDomains: domains}, CSPOptions{})
	script := cspDirective(header, "script-src")
	if got := len(strings.Fields(script)); got > maxCSPDomainsPerDirective+2 {
		t.Fatalf("script-src carries %d sources, want at most %d", got, maxCSPDomainsPerDirective+2)
	}
	found := false
	for _, diagnostic := range diagnostics {
		if strings.Contains(diagnostic, "more than") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the dropped domains were not diagnosed: %v", diagnostics)
	}

	long := "https://" + strings.Repeat("a", 300) + ".example.com"
	if _, diagnostics := BuildCSP(&CSPMeta{ResourceDomains: []string{long}}, CSPOptions{}); len(diagnostics) != 1 {
		t.Fatalf("an overlong domain was not dropped: %v", diagnostics)
	}
}

// TestBuildCSPDiagnosticsAreBounded pins the amplification fix: thousands of
// invalid entries produce at most the per-app budget plus one summary line, and
// the scan stops instead of validating every entry.
func TestBuildCSPDiagnosticsAreBounded(t *testing.T) {
	invalid := make([]string, 5000)
	for index := range invalid {
		invalid[index] = fmt.Sprintf("'bad-%d'", index)
	}
	_, diagnostics := BuildCSP(&CSPMeta{
		ConnectDomains:  invalid,
		ResourceDomains: invalid,
		FrameDomains:    invalid,
	}, CSPOptions{})
	if len(diagnostics) > maxAppDiagnostics+1 {
		t.Fatalf("diagnostics = %d, want at most %d", len(diagnostics), maxAppDiagnostics+1)
	}
	if len(diagnostics) == 0 || !strings.Contains(diagnostics[len(diagnostics)-1], "more dropped") {
		t.Fatalf("the diagnostic list has no drop summary: %v", diagnostics)
	}
}
