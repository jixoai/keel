package mcphost

import (
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

// TestBuildCSPStrictDefault pins the missing-`csp` default: every directive is
// present with exactly the spec's strict value, and no external connection is
// allowed.
func TestBuildCSPStrictDefault(t *testing.T) {
	header, diagnostics := BuildCSP(nil)
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
	})
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
	if strings.Contains(header, "https://api.example.com https://api") {
		t.Fatalf("a connect domain leaked a second time: %s", header)
	}
	if cspDirective(header, "script-src") == cspDirective(header, "connect-src") {
		t.Fatal("resource and connect domains were merged")
	}
}

// TestBuildCSPTightensOnly is the negative case the spec calls for: a server
// can never widen a directive. Non-origin source expressions, injected
// directives and wildcard hosts are dropped, and default-src/object-src stay
// 'none' whatever the block says.
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
	})
	if len(diagnostics) != 5 {
		t.Fatalf("diagnostics = %d (%v), want 5 dropped entries", len(diagnostics), diagnostics)
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
	if got := cspDirective(header, "connect-src"); got != "'self' http://localhost:3000" {
		t.Fatalf("connect-src = %q (loopback http origin should be accepted)", got)
	}
}

// TestBuildCSPDomainBounds pins the count and length bounds: the header stays
// bounded even when a server declares thousands of domains.
func TestBuildCSPDomainBounds(t *testing.T) {
	domains := make([]string, 0, 40)
	for index := 0; index < 40; index++ {
		domains = append(domains, "https://host"+string(rune('a'+index%26))+".example.com")
	}
	header, diagnostics := BuildCSP(&CSPMeta{ResourceDomains: domains})
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
	if _, diagnostics := BuildCSP(&CSPMeta{ResourceDomains: []string{long}}); len(diagnostics) != 1 {
		t.Fatalf("an overlong domain was not dropped: %v", diagnostics)
	}
}
