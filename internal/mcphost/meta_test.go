package mcphost

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParseResourceMetaFullBlock pins the typed projection of `_meta.ui`.
func TestParseResourceMetaFullBlock(t *testing.T) {
	raw := json.RawMessage(`{
		"ui": {
			"csp": {
				"connectDomains": ["https://api.example.com"],
				"resourceDomains": ["https://cdn.example.com"],
				"frameDomains": ["https://embed.example.com"],
				"baseUriDomains": ["https://base.example.com"]
			},
			"permissions": {"clipboardWrite": true, "camera": false},
			"domain": "Example.COM",
			"prefersBorder": true
		}
	}`)
	meta, diagnostics := parseResourceMeta(raw)
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
	if meta.CSP == nil || len(meta.CSP.ConnectDomains) != 1 || meta.CSP.ConnectDomains[0] != "https://api.example.com" {
		t.Fatalf("csp = %+v", meta.CSP)
	}
	if meta.CSP.ResourceDomains[0] != "https://cdn.example.com" ||
		meta.CSP.FrameDomains[0] != "https://embed.example.com" ||
		meta.CSP.BaseURIDomains[0] != "https://base.example.com" {
		t.Fatalf("csp domains = %+v", meta.CSP)
	}
	if !meta.Permissions["clipboardWrite"] || meta.Permissions["camera"] {
		t.Fatalf("permissions = %v", meta.Permissions)
	}
	if meta.Domain != "example.com" {
		t.Fatalf("domain = %q (want lower-case)", meta.Domain)
	}
	if meta.PrefersBorder == nil || !*meta.PrefersBorder {
		t.Fatalf("prefersBorder = %v", meta.PrefersBorder)
	}
}

// TestParseResourceMetaDropsInvalidMembers pins the degradation rule: a broken
// member is diagnosed and dropped, and the rest of the block still applies.
func TestParseResourceMetaDropsInvalidMembers(t *testing.T) {
	raw := json.RawMessage(`{
		"ui": {
			"csp": {
				"scriptSrc": "'unsafe-eval'",
				"connectDomains": ["https://ok.example.com", 7, ""]
			},
			"permissions": {"fingerprint": true, "camera": "yes", "microphone": true},
			"domain": "https://not-a-host",
			"prefersBorder": "true",
			"extra": {"ignored": true}
		}
	}`)
	meta, diagnostics := parseResourceMeta(raw)
	if len(diagnostics) < 6 {
		t.Fatalf("diagnostics = %v, want one per dropped member", diagnostics)
	}
	if meta.CSP == nil || len(meta.CSP.ConnectDomains) != 1 || meta.CSP.ConnectDomains[0] != "https://ok.example.com" {
		t.Fatalf("valid connect domain lost: %+v", meta.CSP)
	}
	if len(meta.CSP.ResourceDomains) != 0 {
		t.Fatalf("unknown csp key produced a directive input: %+v", meta.CSP.ResourceDomains)
	}
	if meta.Permissions["fingerprint"] || meta.Permissions["camera"] || !meta.Permissions["microphone"] {
		t.Fatalf("permissions = %v", meta.Permissions)
	}
	if meta.Domain != "" {
		t.Fatalf("domain = %q, want dropped", meta.Domain)
	}
	if meta.PrefersBorder != nil {
		t.Fatalf("prefersBorder = %v, want dropped", meta.PrefersBorder)
	}
	joined := strings.Join(diagnostics, "\n")
	for _, want := range []string{"scriptSrc", "fingerprint", "not a bare hostname"} {
		if !strings.Contains(joined, want) {
			t.Errorf("diagnostics do not name %q: %s", want, joined)
		}
	}
}

// TestParseResourceMetaAbsent pins the no-metadata case: strict defaults apply
// and nothing is diagnosed.
func TestParseResourceMetaAbsent(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`{}`), json.RawMessage(`{"ui": null}`)} {
		meta, diagnostics := parseResourceMeta(raw)
		if len(diagnostics) != 0 {
			t.Errorf("%s: diagnostics = %v", raw, diagnostics)
		}
		if meta.CSP != nil || meta.Domain != "" || meta.Permissions != nil || meta.PrefersBorder != nil {
			t.Errorf("%s: meta = %+v, want zero", raw, meta)
		}
	}
}
