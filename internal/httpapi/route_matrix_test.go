package httpapi_test

import (
	"sort"
	"testing"

	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/accessgate"
	"github.com/jixoai/keel/internal/harnessapi"
	"github.com/jixoai/keel/internal/httpapi"
)

// TestEveryV1RouteHasPermissionDeclaration enumerates both muxes (the runtime
// edge and the nested product API) plus the A2 auth surface, and asserts the
// single route permission matrix covers every /v1 route. cmd/keel runs the same
// check at startup; this test is the CI half.
func TestEveryV1RouteHasPermissionDeclaration(t *testing.T) {
	product := harnessapi.New(harnessapi.Config{})
	server := httpapi.New(httpapi.Config{Product: product})
	routes := append(server.Routes(), product.Routes()...)
	routes = append(routes, accessgate.AuthRoutes()...)
	sort.Strings(routes)

	if err := accessgate.CheckRoutes(routes); err != nil {
		t.Fatal(err)
	}
	if unused := accessgate.UnusedRules(routes); len(unused) > 0 {
		t.Fatalf("matrix declares routes no mux registers (drift): %v", unused)
	}
	if accessgate.RuleCount() < len(routes) {
		t.Fatalf("matrix has %d rules for %d routes", accessgate.RuleCount(), len(routes))
	}
	t.Logf("route permission matrix: %d declarations, %d registered routes", accessgate.RuleCount(), len(routes))
}

// TestAdminAndOperatorSurfacesAreExplicit pins the routes that must never be
// mistaken for tenant-key routes. They are operator-gated by the handler
// (KEEL_ADMIN_KEY / runtime admin API); the matrix records that explicitly.
func TestAdminAndOperatorSurfacesAreExplicit(t *testing.T) {
	for _, probe := range []struct {
		method string
		path   string
	}{
		{"GET", "/v1/runtime/status"},
		{"POST", "/v1/runtime/drain"},
		{"POST", "/v1/tenants"},
		{"GET", "/v1/tenants/00000000-0000-0000-0000-000000000000/config"},
		{"POST", "/v1/admin/tenants"},
	} {
		match, ok := accessgate.Match(probe.method, probe.path)
		if !ok {
			t.Fatalf("%s %s: no matrix rule", probe.method, probe.path)
		}
		if match.Rule.Mode != accessgate.ModeOperator {
			t.Fatalf("%s %s: mode %q, want operator", probe.method, probe.path, match.Rule.Mode)
		}
	}
}

// TestMatchSelectsTheDeclaredRuleForEveryRoute synthesizes a request path for
// every registered pattern and asserts the gate matches the same declaration.
// This catches a precedence bug (a wildcard rule shadowing a literal one)
// that a pure coverage check would miss.
func TestMatchSelectsTheDeclaredRuleForEveryRoute(t *testing.T) {
	product := harnessapi.New(harnessapi.Config{})
	server := httpapi.New(httpapi.Config{Product: product})
	routes := append(server.Routes(), product.Routes()...)
	routes = append(routes, accessgate.AuthRoutes()...)
	for _, pattern := range routes {
		method, path := splitRoute(pattern)
		if path == "/v1/" || len(path) < 4 || path[:4] != "/v1/" {
			continue
		}
		sample := sampleRoutePath(path)
		match, ok := accessgate.Match(method, sample)
		if !ok {
			t.Fatalf("%s %s: no match for sample %q", method, path, sample)
		}
		if match.Rule.Pattern != path {
			t.Fatalf("%s %s: matched rule %q instead", method, path, match.Rule.Pattern)
		}
		if method != "" && match.Rule.Method != "" && match.Rule.Method != method {
			t.Fatalf("%s %s: matched method %q instead", method, path, match.Rule.Method)
		}
	}
}

func splitRoute(pattern string) (method, path string) {
	if len(pattern) > 0 && pattern[0] == '/' {
		return "GET", pattern
	}
	for i := 0; i < len(pattern); i++ {
		if pattern[i] == ' ' {
			return pattern[:i], pattern[i+1:]
		}
	}
	return "", pattern
}

func sampleRoutePath(path string) string {
	out := make([]byte, 0, len(path))
	for i := 0; i < len(path); i++ {
		if path[i] == '{' {
			out = append(out, 'x')
			for i < len(path) && path[i] != '}' {
				i++
			}
			continue
		}
		out = append(out, path[i])
	}
	return string(out)
}

// TestAttachmentPartUsesTheSessionReadCapability pins the projection's
// permission model: it is a read of the session's committed message.
func TestAttachmentPartUsesTheSessionReadCapability(t *testing.T) {
	match, ok := accessgate.Match("GET", "/v1/sessions/00000000-0000-0000-0000-000000000000/attachments/11111111-1111-1111-1111-111111111111/part")
	if !ok {
		t.Fatal("no matrix rule for the attachment projection")
	}
	if match.Rule.Key != access.KeySessionRead {
		t.Fatalf("attachment part key = %q, want %q", match.Rule.Key, access.KeySessionRead)
	}
}
