package main

// The P2 assembly proof (change feat-mcp-apps-events): the MCP half (registry →
// prompt sections + catalogue) and the plugin half (harnessext runtime → tools +
// sections) are published through separate sinks and composed on every
// republish. The test drives the production wiring — a real MCP stdio fixture,
// the real plugin layer, the real registry — and asserts the two directions:
//
//   - a plugin enable/disable keeps the MCP sections and the builtin `mcp`
//     tool (R2, direction A);
//   - an MCP republish (reconnect) keeps the plugin tools and prompt sections
//     (R2, direction B).
//
// It then serves the registry through the production HarnessAPI and asserts the
// app document contract end to end: catalogue identity, content hash, CSP and
// the four security headers.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/harnessapi"
	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/loadout"
	"github.com/jixoai/keel/internal/mcp"
	"github.com/jixoai/keel/internal/mcphost"
	"github.com/jixoai/keel/internal/servicehost"
	"github.com/jixoai/keel/internal/testkit"
)

// mcpSurfaceFixtureHTML is the app document the MCP fixture serves; the
// catalogue hash and the view body are asserted against it.
const mcpSurfaceFixtureHTML = "<html><body><h1>fixture panel</h1></body></html>"

type mcpSurfaceHarness struct {
	rt       *servicehost.Runtime
	registry *mcphost.Registry
	plugins  *pluginSurface
	tenant   uuid.UUID
}

func newMCPSurfaceHarness(t *testing.T) *mcpSurfaceHarness {
	t.Helper()
	ctx := context.Background()
	log := discardLogger()
	mem := kernel.NewMemStore()
	k := kernel.New(mem, kernel.Options{})
	tenant := uuid.New()

	publication := newDynamicSurface(log)
	registry := mcphost.New(mcphost.Options{TenantID: tenant, Logger: log, Sinks: publication.sinks()})
	entry := mcp.ServerEntry{
		Name: "promptfixture",
		Config: map[string]any{
			"command":     executablePath(t),
			"args":        []any{"-test.run=^TestPluginPromptMCPHelper$"},
			"env":         map[string]any{pluginPromptMCPHelperEnv: "1"},
			"description": "prompt fixture",
		},
	}
	rt, err := servicehost.Boot(ctx, servicehost.Config{
		Seams: servicehost.Seams{
			Committer: k, Reader: k, Leases: k, External: k,
			Runs: pluginPromptMirror{}, Logger: log,
		},
		Tenant: host.TenantConfig{
			Agent: host.AgentConfig{
				SystemPrompt: "You are the P2 surface test agent.",
				Sections:     []host.PromptSection{{Name: "tenant_rules", Content: "Tenant rule text."}},
			},
			MCP: host.MCPConfig{Servers: map[string]host.MCPServerConfig{
				"promptfixture": {Command: executablePath(t)},
			}},
		},
		Options:   loadout.Options{Revision: 1},
		RuntimeID: "mcp-surface-test",
		MCP: servicehost.MCPOptions{
			Entries:  []mcp.ServerEntry{entry},
			Registry: registry,
			ConnectOptions: mcp.ConnectOptions{
				ClientName:    "keel-mcp-surface-test",
				ClientVersion: "1.0.0",
				Stderr:        io.Discard,
				Logger:        log,
			},
		},
	})
	if err != nil {
		t.Fatalf("boot runtime: %v", err)
	}
	t.Cleanup(func() {
		if err := rt.Close(context.Background()); err != nil {
			t.Errorf("close runtime: %v", err)
		}
	})
	publication.attach(rt)

	hooks := harnessext.NewHookBus(harnessext.HookBusOptions{Logger: log})
	decisions := harnessext.NewDecisionBroker(harnessext.DecisionBrokerOptions{Logger: log})
	partials := harnessext.NewPartialHub()
	triggers := harnessext.NewTriggerEngine(harnessext.TriggerEngineOptions{Hooks: hooks, Logger: log})
	subagents := harnessext.NewSubagentRunner(harnessext.SubagentOptions{Hooks: hooks, Logger: log})
	t.Cleanup(func() {
		triggers.Close()
		decisions.Close()
		hooks.Close()
	})
	surface, err := newPluginSurface(ctx, rt, nil, serveOptions{
		TenantID:    tenant,
		PluginsRoot: t.TempDir(),
	}, hooks, triggers, decisions, nil, nil, subagents, partials, nil, log)
	if err != nil {
		t.Fatalf("plugin surface: %v", err)
	}
	if _, err := surface.manager.Enable(ctx, testkit.PluginID, harnessext.AuditContext{Actor: "test"}); err != nil {
		t.Fatalf("enable the testkit plugin: %v", err)
	}
	t.Cleanup(func() { surface.runtime.Close(context.Background()) })
	return &mcpSurfaceHarness{rt: rt, registry: registry, plugins: surface, tenant: tenant}
}

// executablePath is the test binary itself: the stdio fixture is a child of it.
func executablePath(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	return path
}

// mustContainMCPHalf asserts the MCP half is live in the assembly the model
// would see: both sections and the builtin aggregate tool.
func (h *mcpSurfaceHarness) mustContainMCPHalf(t *testing.T, step string) {
	t.Helper()
	prompt := h.rt.Assembly().SystemPrompt()
	for _, want := range []string{"<mcp_servers>", "- promptfixture:", "<mcp_apps>", "- promptfixture/fixture_echo: ui://fixture/panel"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("%s: the MCP half is missing %q from the prompt: %q", step, want, prompt)
		}
	}
	if _, ok := h.rt.Assembly().Tools().Get("mcp"); !ok {
		t.Fatalf("%s: the builtin mcp tool is missing", step)
	}
	if _, ok := h.rt.Assembly().Tools().Get("clock"); !ok {
		t.Fatalf("%s: the plugin tool is missing", step)
	}
	if !strings.Contains(prompt, "<plugin_standard>") || !strings.Contains(prompt, harnessext.CreationPrompt()) {
		t.Fatalf("%s: the plugin prompt section is missing: %q", step, prompt)
	}
}

// TestMCPHalfSurvivesPluginRefresh is R2 direction A: a plugin enable/disable
// republishes the plugin half and must keep the MCP sections and tool.
func TestMCPHalfSurvivesPluginRefresh(t *testing.T) {
	harness := newMCPSurfaceHarness(t)
	harness.mustContainMCPHalf(t, "boot")

	if _, err := harness.plugins.manager.Disable(context.Background(), harnessext.CreationPluginID, harnessext.AuditContext{Actor: "test"}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	prompt := harness.rt.Assembly().SystemPrompt()
	if !strings.Contains(prompt, "<mcp_servers>") || !strings.Contains(prompt, "<mcp_apps>") {
		t.Fatalf("a plugin refresh dropped the MCP sections: %q", prompt)
	}
	if mcpTool, ok := harness.rt.Assembly().Tools().Get("mcp"); !ok {
		t.Fatal("a plugin refresh dropped the builtin mcp tool")
	} else if _, err := mcpTool.Execute(context.Background(), "list-after-disable", json.RawMessage(`{"action":"list"}`), nil); err != nil {
		t.Fatalf("the mcp tool stopped working after a plugin refresh: %v", err)
	}

	if _, err := harness.plugins.manager.Enable(context.Background(), harnessext.CreationPluginID, harnessext.AuditContext{Actor: "test"}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	harness.mustContainMCPHalf(t, "after re-enable")
}

// TestPluginHalfSurvivesMCPRepublish is R2 direction B: an MCP reconnect
// republishes the MCP half (new registry generation) and must keep the plugin
// tools and prompt sections.
func TestPluginHalfSurvivesMCPRepublish(t *testing.T) {
	harness := newMCPSurfaceHarness(t)
	harness.mustContainMCPHalf(t, "boot")
	before := harness.registry.Generation()

	if err := harness.rt.ReconnectMCP(context.Background(), "promptfixture"); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if harness.registry.Generation() <= before {
		t.Fatalf("the MCP republish did not advance the generation: %d -> %d", before, harness.registry.Generation())
	}
	harness.mustContainMCPHalf(t, "after MCP republish")
}

// TestMCPAppsHTTPEndToEnd drives the registry through the production HarnessAPI:
// the catalogue identity, the document hash/URL, the CSP and the four security
// headers, with the tenant boundary in front.
func TestMCPAppsHTTPEndToEnd(t *testing.T) {
	harness := newMCPSurfaceHarness(t)
	theme := json.RawMessage(`{"cssVars":{"theme":{"radius":"12px"},"light":{"card":"#fff","foreground":"#111"},"dark":{"card":"#111","foreground":"#eee"}}}`)
	server := httptest.NewServer(harnessapi.New(harnessapi.Config{
		MCPApps:      harness.registry,
		MCPAppsTheme: theme,
		Logger:       discardLogger(),
	}))
	t.Cleanup(server.Close)

	tenant := harness.tenant
	get := func(path string, tenantID uuid.UUID) *http.Response {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tenantID != uuid.Nil {
			request.Header.Set("X-Tenant-ID", tenantID.String())
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { response.Body.Close() })
		return response
	}

	catalogResponse := get("/v1/mcp/apps", tenant)
	var catalog struct {
		Generation int64                        `json:"generation"`
		Apps       []mcphost.AppSpec            `json:"apps"`
		Theme      map[string]map[string]string `json:"theme"`
	}
	if code := catalogResponse.StatusCode; code != http.StatusOK {
		body, _ := io.ReadAll(catalogResponse.Body)
		t.Fatalf("catalog = %d: %s", code, body)
	}
	if err := json.NewDecoder(catalogResponse.Body).Decode(&catalog); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if catalog.Generation < 1 || len(catalog.Apps) != 1 {
		t.Fatalf("catalog = %+v", catalog)
	}
	app := catalog.Apps[0]
	expectedID := mcp.AppIDFor("promptfixture", "ui://fixture/panel")
	if app.AppID != expectedID || app.ResourceURI != "ui://fixture/panel" || app.Server != "promptfixture" {
		t.Fatalf("app identity = %+v, want %s", app, expectedID)
	}
	sum := sha256.Sum256([]byte(mcpSurfaceFixtureHTML))
	expectedHash := hex.EncodeToString(sum[:])
	if app.Hash != expectedHash {
		t.Fatalf("hash = %q, want %q", app.Hash, expectedHash)
	}
	if app.DocumentURL != "/v1/mcp/apps/"+expectedID+"/view?v="+expectedHash {
		t.Fatalf("documentUrl = %q", app.DocumentURL)
	}
	if !strings.Contains(app.CSP, "default-src 'none'") ||
		!strings.Contains(app.CSP, "connect-src 'self' https://api.fixture.test") ||
		!strings.Contains(app.CSP, "frame-ancestors 'self'") {
		t.Fatalf("csp = %q", app.CSP)
	}
	if app.Permissions["clipboardWrite"] != true || app.PrefersBorder == nil || !*app.PrefersBorder {
		t.Fatalf("permissions/prefersBorder = %+v / %v", app.Permissions, app.PrefersBorder)
	}
	if catalog.Theme["light"]["--color-background-primary"] != "#fff" || catalog.Theme["light"]["--color-border-primary"] != "" {
		t.Fatalf("theme mapping = %+v", catalog.Theme)
	}

	view := get(app.DocumentURL, tenant)
	if view.StatusCode != http.StatusOK {
		t.Fatalf("view = %d", view.StatusCode)
	}
	for header, want := range map[string]string{
		"Content-Type":           "text/html; charset=utf-8",
		"X-MCP-App-Mime":         "text/html;profile=mcp-app",
		"X-MCP-App-Hash":         expectedHash,
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"Cache-Control":          "public, max-age=31536000, immutable",
	} {
		if got := view.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if got := view.Header.Get("Content-Security-Policy"); got != app.CSP {
		t.Fatalf("the served CSP differs from the catalogue's: %q vs %q", got, app.CSP)
	}
	body, err := io.ReadAll(view.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != mcpSurfaceFixtureHTML {
		t.Fatalf("body = %q", body)
	}

	if unknown := get("/v1/mcp/apps/mcpapp_0000000000000000/view", tenant); unknown.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown app = %d, want 404", unknown.StatusCode)
	}
	if foreign := get("/v1/mcp/apps", uuid.New()); foreign.StatusCode != http.StatusOK {
		t.Fatalf("foreign catalogue = %d", foreign.StatusCode)
	} else {
		raw, _ := io.ReadAll(foreign.Body)
		if strings.Contains(string(raw), "promptfixture") {
			t.Fatalf("a foreign tenant saw the catalogue: %s", raw)
		}
	}

	capabilitiesResponse := get("/v1/capabilities", tenant)
	var capabilities struct {
		Features map[string]bool `json:"features"`
	}
	if err := json.NewDecoder(capabilitiesResponse.Body).Decode(&capabilities); err != nil {
		t.Fatal(err)
	}
	if !capabilities.Features["mcp.apps"] {
		t.Fatal("mcp.apps is false with a wired registry")
	}
	if capabilities.Features["mcp.apps.sandboxOrigin"] || capabilities.Features["mcp.events"] {
		t.Fatalf("capability bits = %+v", capabilities.Features)
	}
}
