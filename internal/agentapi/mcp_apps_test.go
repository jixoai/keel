package agentapi

// The MCP Apps HTTP contract (P2.4/P2.5): tenant boundary first, unknown app
// ids indistinguishable from foreign ones, the four document security headers,
// content-hash cache identity, and the capability bits.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/mcphost"
)

// fakeMCPApps is the read model the routes consume.
type fakeMCPApps struct {
	tenant      uuid.UUID
	published   bool
	surface     mcphost.Surface
	documents   map[string]mcphost.Document
	diagnostics []mcphost.Diagnostic
}

func (f *fakeMCPApps) Visible(tenantID uuid.UUID) bool {
	return f.tenant == uuid.Nil || f.tenant == tenantID
}

func (f *fakeMCPApps) Surface() (mcphost.Surface, bool) {
	return f.surface, f.published
}

func (f *fakeMCPApps) Document(appID, hash string) (mcphost.Document, bool) {
	for key, document := range f.documents {
		if !strings.HasPrefix(key, appID+"\x00") {
			continue
		}
		if hash != "" && document.Hash != hash {
			return mcphost.Document{}, false
		}
		return document, true
	}
	return mcphost.Document{}, false
}

func (f *fakeMCPApps) Diagnostics() []mcphost.Diagnostic { return f.diagnostics }

func (f *fakeMCPApps) Limits() (int64, int64) { return 2 << 20, 64 << 20 }

const (
	testAppID = "mcpapp_0123456789abcdef"
	testHTML  = "<html><body>dashboard</body></html>"
)

func testCSP(t *testing.T) string {
	t.Helper()
	header, _ := mcphost.BuildCSP(&mcphost.CSPMeta{ConnectDomains: []string{"https://api.example.com"}})
	return header
}

func testSurface(t *testing.T) mcphost.Surface {
	t.Helper()
	csp := testCSP(t)
	hash := hashFor(testHTML)
	return mcphost.Surface{
		Generation: 7,
		Servers: []mcphost.ServerState{{
			Name: "weather", Transport: "streamable-http", Era: "legacy", Connected: true, Tools: 2, Apps: 1,
		}},
		Apps: []mcphost.AppSpec{{
			AppID:       testAppID,
			Server:      "weather",
			ResourceURI: "ui://weather/dashboard",
			Title:       "Weather",
			MIMEType:    "text/html;profile=mcp-app",
			Visibility:  []string{"model", "app"},
			Tools:       []mcphost.ToolRef{{Tool: "dashboard", Visibility: []string{"model", "app"}}},
			CSP:         csp,
			Permissions: map[string]bool{"clipboardWrite": true},
			Hash:        hash,
			FetchedAt:   time.Unix(1_700_000_000, 0),
			DocumentURL: "/v1/mcp/apps/" + testAppID + "/view?v=" + hash,
		}},
	}
}

func testDocument(t *testing.T) mcphost.Document {
	t.Helper()
	return mcphost.Document{
		AppID:    testAppID,
		Server:   "weather",
		Hash:     hashFor(testHTML),
		MIMEType: "text/html;profile=mcp-app",
		CSP:      testCSP(t),
		Bytes:    []byte(testHTML),
	}
}

// hashFor mirrors the registry's sha256 content identity for the fixture.
func hashFor(value string) string {
	// The route only needs a well-formed 64-hex identity that matches the
	// document the fake serves; the registry itself hashes with sha256.
	return strings.Repeat("ab", 32)
}

func testAgentAPI(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(New(cfg))
	t.Cleanup(server.Close)
	return server
}

// rawGet issues a request and returns the response (headers included).
func rawGet(t *testing.T, server *httptest.Server, path string, tenant uuid.UUID) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tenant != uuid.Nil {
		req.Header.Set("X-Tenant-ID", tenant.String())
	}
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func readBody(t *testing.T, res *http.Response) string {
	t.Helper()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestMCPAppsRoutesRequireTenant pins the tenant boundary first: no catalogue
// byte and no document byte is read before the tenant predicate passes.
func TestMCPAppsRoutesRequireTenant(t *testing.T) {
	apps := &fakeMCPApps{tenant: uuid.New(), published: true, surface: testSurface(t),
		documents: map[string]mcphost.Document{testAppID + "\x00" + hashFor(testHTML): testDocument(t)}}
	server := testAgentAPI(t, Config{MCPApps: apps})
	for _, path := range []string{
		"/v1/mcp/servers",
		"/v1/mcp/apps",
		"/v1/mcp/apps/" + testAppID + "/view?v=" + hashFor(testHTML),
	} {
		res := rawGet(t, server, path, uuid.Nil)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a tenant = %d, want 401", path, res.StatusCode)
		}
		if strings.Contains(readBody(t, res), "weather") {
			t.Errorf("%s leaked the catalogue before the tenant check", path)
		}
	}
}

// TestMCPAppsCatalogIsTenantScoped pins the catalogue answer and the isolation:
// a foreign tenant sees no app and no server name.
func TestMCPAppsCatalogIsTenantScoped(t *testing.T) {
	tenant := uuid.New()
	apps := &fakeMCPApps{tenant: tenant, published: true, surface: testSurface(t),
		documents: map[string]mcphost.Document{testAppID + "\x00" + hashFor(testHTML): testDocument(t)}}
	theme := json.RawMessage(`{"cssVars":{"light":{"card":"#fff"},"dark":{"card":"#111"}}}`)
	server := testAgentAPI(t, Config{MCPApps: apps, MCPAppsSandboxOrigin: "https://sandbox.example.com", MCPAppsTheme: theme})

	var body mcpAppsResponse
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/mcp/apps", tenant, nil, &body); code != 200 {
		t.Fatalf("catalog = %d", code)
	}
	if body.Generation != 7 || len(body.Apps) != 1 || body.Apps[0].AppID != testAppID {
		t.Fatalf("catalog = %+v", body)
	}
	if body.Apps[0].DocumentURL != "/v1/mcp/apps/"+testAppID+"/view?v="+hashFor(testHTML) {
		t.Fatalf("documentUrl = %q", body.Apps[0].DocumentURL)
	}
	if body.SandboxOrigin != "https://sandbox.example.com" {
		t.Fatalf("sandboxOrigin = %q", body.SandboxOrigin)
	}
	if body.Theme["light"]["--color-background-primary"] != "#fff" || body.Theme["dark"]["--color-background-primary"] != "#111" {
		t.Fatalf("theme = %+v", body.Theme)
	}

	foreign := rawGet(t, server, "/v1/mcp/apps", uuid.New())
	raw := readBody(t, foreign)
	if foreign.StatusCode != 200 {
		t.Fatalf("foreign tenant catalog = %d", foreign.StatusCode)
	}
	var foreignBody mcpAppsResponse
	if err := json.Unmarshal([]byte(raw), &foreignBody); err != nil {
		t.Fatal(err)
	}
	if len(foreignBody.Apps) != 0 || strings.Contains(raw, "weather") || strings.Contains(raw, "ui://") {
		t.Fatalf("a foreign tenant saw the catalogue: %s", raw)
	}
	// The servers projection is tenant-scoped too.
	servers := rawGet(t, server, "/v1/mcp/servers", tenant)
	var serverBody mcpServersResponse
	if err := json.Unmarshal([]byte(readBody(t, servers)), &serverBody); err != nil {
		t.Fatal(err)
	}
	if len(serverBody.Servers) != 1 || serverBody.Servers[0].Name != "weather" || serverBody.Generation != 7 {
		t.Fatalf("servers = %+v", serverBody)
	}
	foreignServers := readBody(t, rawGet(t, server, "/v1/mcp/servers", uuid.New()))
	if strings.Contains(foreignServers, "weather") {
		t.Fatalf("a foreign tenant saw the server list: %s", foreignServers)
	}
}

// TestMCPAppViewSecurityHeaders pins the document response: the four required
// headers, the immutable hash identity and the exact bytes.
func TestMCPAppViewSecurityHeaders(t *testing.T) {
	tenant := uuid.New()
	apps := &fakeMCPApps{tenant: tenant, published: true, surface: testSurface(t),
		documents: map[string]mcphost.Document{testAppID + "\x00" + hashFor(testHTML): testDocument(t)}}
	server := testAgentAPI(t, Config{MCPApps: apps})

	res := rawGet(t, server, "/v1/mcp/apps/"+testAppID+"/view?v="+hashFor(testHTML), tenant)
	if res.StatusCode != 200 {
		t.Fatalf("view = %d", res.StatusCode)
	}
	checks := map[string]string{
		"Content-Type":            "text/html; charset=utf-8",
		"X-MCP-App-Mime":          "text/html;profile=mcp-app",
		"X-MCP-App-Hash":          hashFor(testHTML),
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"Cache-Control":           "public, max-age=31536000, immutable",
		"Content-Security-Policy": testCSP(t),
	}
	for header, want := range checks {
		if got := res.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	csp := res.Header.Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'none'", "connect-src 'self' https://api.example.com", "frame-ancestors 'self'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP is missing %q: %s", directive, csp)
		}
	}
	if body := readBody(t, res); body != testHTML {
		t.Fatalf("body = %q", body)
	}

	// Bare URL: still a valid document, but never cached by an intermediary
	// (the content moves under an URL without a content identity).
	plain := rawGet(t, server, "/v1/mcp/apps/"+testAppID+"/view", tenant)
	if plain.StatusCode != 200 || plain.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("plain view = %d / %q", plain.StatusCode, plain.Header.Get("Cache-Control"))
	}
	if plain.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("the bare view dropped a security header")
	}
}

// TestMCPAppViewUnknownIdentity pins the one 404: unknown id, wrong shape,
// stale hash and foreign tenant are indistinguishable, and none of them lists
// the servers.
func TestMCPAppViewUnknownIdentity(t *testing.T) {
	tenant := uuid.New()
	apps := &fakeMCPApps{tenant: tenant, published: true, surface: testSurface(t),
		documents: map[string]mcphost.Document{testAppID + "\x00" + hashFor(testHTML): testDocument(t)}}
	server := testAgentAPI(t, Config{MCPApps: apps})
	stale := strings.Repeat("cd", 32)
	cases := []struct {
		name   string
		path   string
		tenant uuid.UUID
	}{
		{"unknown app", "/v1/mcp/apps/mcpapp_0000000000000000/view", tenant},
		{"wrong shape", "/v1/mcp/apps/not-an-app/view", tenant},
		{"stale hash", "/v1/mcp/apps/" + testAppID + "/view?v=" + stale, tenant},
		{"malformed hash", "/v1/mcp/apps/" + testAppID + "/view?v=abc", tenant},
		{"foreign tenant", "/v1/mcp/apps/" + testAppID + "/view?v=" + hashFor(testHTML), uuid.New()},
	}
	for _, testCase := range cases {
		res := rawGet(t, server, testCase.path, testCase.tenant)
		body := readBody(t, res)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", testCase.name, res.StatusCode)
		}
		if strings.Contains(body, "weather") || strings.Contains(body, "ui://") || strings.Contains(body, "dashboard") {
			t.Errorf("%s leaked the catalogue: %s", testCase.name, body)
		}
	}
}

// TestMCPAppsUnwired pins the explicit 501: a deployment without the registry
// serves no app surface, and the capability bits say so.
func TestMCPAppsUnwired(t *testing.T) {
	server := testAgentAPI(t, Config{})
	tenant := uuid.New()
	for _, path := range []string{"/v1/mcp/servers", "/v1/mcp/apps", "/v1/mcp/apps/" + testAppID + "/view"} {
		res := rawGet(t, server, path, tenant)
		if res.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s = %d, want 501", path, res.StatusCode)
		}
	}
	var capabilities Capabilities
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/capabilities", tenant, nil, &capabilities); code != 200 {
		t.Fatalf("capabilities = %d", code)
	}
	if capabilities.Features["mcp.apps"] || capabilities.Features["mcp.apps.sandboxOrigin"] {
		t.Fatalf("features = %+v", capabilities.Features)
	}
	if capabilities.Features["mcp.events"] {
		t.Fatal("mcp.events is true without a dispatcher")
	}
	if capabilities.MCPApps != nil {
		t.Fatalf("mcpApps block = %+v", capabilities.MCPApps)
	}
}

// TestCapabilitiesMCPBits pins the wired bits: mcp.apps follows the registry,
// the sandbox-origin bit follows the configured origin, and mcp.events follows
// the dispatcher (P4).
func TestCapabilitiesMCPBits(t *testing.T) {
	tenant := uuid.New()
	apps := &fakeMCPApps{tenant: tenant, published: true, surface: testSurface(t)}
	server := testAgentAPI(t, Config{
		MCPApps:              apps,
		MCPAppsSandboxOrigin: "https://sandbox.example.com",
	})
	var capabilities Capabilities
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/capabilities", tenant, nil, &capabilities); code != 200 {
		t.Fatalf("capabilities = %d", code)
	}
	if !capabilities.Features["mcp.apps"] || !capabilities.Features["mcp.apps.sandboxOrigin"] {
		t.Fatalf("features = %+v", capabilities.Features)
	}
	if capabilities.MCPApps == nil || capabilities.MCPApps.SandboxOrigin != "https://sandbox.example.com" ||
		capabilities.MCPApps.Generation != 7 || capabilities.MCPApps.Apps != 1 {
		t.Fatalf("mcpApps block = %+v", capabilities.MCPApps)
	}

	wired := testAgentAPI(t, Config{MCPApps: apps, MCPEvents: true})
	if code := doJSON(t, wired.Client(), http.MethodGet, wired.URL+"/v1/capabilities", tenant, nil, &capabilities); code != 200 {
		t.Fatalf("capabilities = %d", code)
	}
	if !capabilities.Features["mcp.events"] {
		t.Fatal("mcp.events did not follow the wired flag")
	}
}

// TestMCPAppViewRefusesIncompleteDocument pins the last line of defense: a
// document without its CSP or with a foreign media type cannot be served (a
// body must never go out without the required headers).
func TestMCPAppViewRefusesIncompleteDocument(t *testing.T) {
	tenant := uuid.New()
	for name, mutate := range map[string]func(*mcphost.Document){
		"no csp":       func(doc *mcphost.Document) { doc.CSP = "" },
		"foreign mime": func(doc *mcphost.Document) { doc.MIMEType = "application/json" },
		"empty body":   func(doc *mcphost.Document) { doc.Bytes = nil },
	} {
		document := testDocument(t)
		mutate(&document)
		apps := &fakeMCPApps{tenant: tenant, published: true, surface: testSurface(t),
			documents: map[string]mcphost.Document{testAppID + "\x00" + hashFor(testHTML): document}}
		server := testAgentAPI(t, Config{MCPApps: apps})
		res := rawGet(t, server, "/v1/mcp/apps/"+testAppID+"/view?v="+hashFor(testHTML), tenant)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: view = %d, want 404", name, res.StatusCode)
		}
		if res.Header.Get("Content-Security-Policy") != "" {
			t.Errorf("%s: an incomplete document shipped a body", name)
		}
	}
}
