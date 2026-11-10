package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/agentext"
)

// Plugin UI hosting (change keel-plugin-core, W1): /plugins/<id>/* serves an
// ENABLED plugin's UI package and nothing else.

// writePluginPackage materializes a minimal webview package under root/<id>.
func writePluginPackage(t *testing.T, root, id, entry string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "id: " + id + "\nversion: 1.0.0\nkind: webview\ndescription: test panel\n" +
		"provides:\n  ui:\n    entry: " + entry + "\n    title: test panel\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// pluginUITestManager builds a real manager over a temp plugins root holding one
// enabled webview plugin.
func pluginUITestManager(t *testing.T, mutate func(root string)) (*agentext.Manager, string) {
	t.Helper()
	root := t.TempDir()
	writePluginPackage(t, root, "vendor.panel", "ui/index.html", map[string]string{
		"ui/index.html":           "<!doctype html><title>panel</title><p>panel-home</p>",
		"ui/assets/app-C3x9f2.js": "console.log('panel')",
		"ui/sub/index.html":       "<p>sub</p>",
	})
	if mutate != nil {
		mutate(root)
	}
	manager, err := agentext.NewPluginManager(agentext.ManagerOptions{
		Root: root,
		Runtime: agentext.NewRuntime(agentext.RuntimeOptions{
			Logger: discardLogger(),
		}),
		KeelVersion: "0.2.0",
		Logger:      discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.Snapshot() })
	if err := manager.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	return manager, root
}

func TestPluginUIHostsEnabledPluginOnly(t *testing.T) {
	manager, _ := pluginUITestManager(t, nil)
	handler := newPluginUIHandler(manager, discardLogger())
	if !handler.available() {
		t.Fatal("handler should be available")
	}
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// Installed but not enabled: the operator asked for it to be off.
	if rec := get("/plugins/vendor.panel/"); rec.Code != http.StatusForbidden {
		t.Fatalf("disabled plugin = %d %s", rec.Code, rec.Body.String())
	}
	if rec := get("/plugins/vendor.panel/index.html"); rec.Code != http.StatusForbidden {
		t.Fatalf("disabled plugin asset = %d", rec.Code)
	}

	// Enable it: a UI-only webview plugin has no runtime surface, and its UI is
	// exactly what the enablement gates.
	if _, err := manager.Enable(context.Background(), "vendor.panel", agentext.AuditContext{Actor: "test"}); err != nil {
		t.Fatalf("enable: %v", err)
	}

	// /plugins/<id> redirects to the directory form so relative URLs resolve.
	if rec := get("/plugins/vendor.panel"); rec.Code != http.StatusMovedPermanently ||
		rec.Header().Get("Location") != "/plugins/vendor.panel/" {
		t.Fatalf("slash redirect = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec := get("/plugins/vendor.panel/")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "panel-home") {
		t.Fatalf("index = %d %q", rec.Code, rec.Body.String())
	}
	if contentType := rec.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
		t.Fatalf("index content type = %q", contentType)
	}
	if rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("index cache = %q", rec.Header().Get("Cache-Control"))
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("security headers missing: %+v", rec.Header())
	}
	// Assets: same origin, hashed names are immutable.
	rec = get("/plugins/vendor.panel/assets/app-C3x9f2.js")
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset = %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if contentType := rec.Header().Get("Content-Type"); !strings.Contains(contentType, "javascript") {
		t.Fatalf("asset content type = %q", contentType)
	}
	// A directory with an index.html is served by it.
	if rec := get("/plugins/vendor.panel/sub/"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "sub") {
		t.Fatalf("nested index = %d %q", rec.Code, rec.Body.String())
	}
	// The manifest is not part of the UI even when the package root is the root.
	if rec := get("/plugins/vendor.panel/plugin.yaml"); rec.Code != http.StatusForbidden {
		t.Fatalf("manifest = %d", rec.Code)
	}
	// Unknown ids and unknown files are 404s.
	if rec := get("/plugins/other.plugin/"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown plugin = %d", rec.Code)
	}
	if rec := get("/plugins/vendor.panel/missing.js"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing file = %d", rec.Code)
	}
	// Only reads are served.
	req := httptest.NewRequest(http.MethodPost, "/plugins/vendor.panel/", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post = %d", rec.Code)
	}
}

func TestPluginUIRefusesTraversalAndSymlinkEscape(t *testing.T) {
	manager, root := pluginUITestManager(t, func(root string) {
		// A symlink planted inside the package pointing at the host filesystem.
		if err := os.MkdirAll(filepath.Join(root, "vendor.panel", "ui"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/etc/passwd", filepath.Join(root, "vendor.panel", "ui", "escape.txt")); err != nil {
			t.Fatal(err)
		}
	})
	handler := newPluginUIHandler(manager, discardLogger())
	if _, err := manager.Enable(context.Background(), "vendor.panel", agentext.AuditContext{Actor: "test"}); err != nil {
		// The escaping symlink may make the package invalid, which is itself a
		// refusal — but then the handler must never serve it either.
		t.Logf("enable refused by the package validator: %v", err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	for _, path := range []string{
		"/plugins/vendor.panel/../../etc/passwd",
		"/plugins/vendor.panel/%2e%2e/%2e%2e/etc/passwd",
		"/plugins/vendor.panel/ui/../../plugin.yaml",
	} {
		rec := get(path)
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
			t.Fatalf("%s = %d %q", path, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "root:") {
			t.Fatalf("%s served a system file", path)
		}
	}
	// The symlink is refused even when the package itself was accepted.
	if rec := get("/plugins/vendor.panel/ui/escape.txt"); rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "root:") {
		t.Fatalf("the escaping symlink served a system file: %d %q", rec.Code, rec.Body.String())
	}
	_ = root
}

// TestPluginUIServeFileContainment drives the serving core directly: a symlink
// that resolves outside the UI root is refused even when the request path is
// perfectly inside it.
func TestPluginUIServeFileContainment(t *testing.T) {
	packageDir := t.TempDir()
	uiDir := filepath.Join(packageDir, "ui")
	if err := os.MkdirAll(uiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uiDir, "index.html"), []byte("<p>ok</p>"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("host-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(uiDir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	handler := &pluginUIHandler{log: discardLogger()}
	entry := filepath.Join(uiDir, "index.html")

	// The honest file is served.
	rec := httptest.NewRecorder()
	handler.serveFile(rec, httptest.NewRequest(http.MethodGet, "/plugins/vendor.panel/", nil), entry, uiDir, entry)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("entry = %d %q", rec.Code, rec.Body.String())
	}
	// The link is not: it resolves outside the root.
	rec = httptest.NewRecorder()
	handler.serveFile(rec, httptest.NewRequest(http.MethodGet, "/plugins/vendor.panel/link.txt", nil),
		filepath.Join(uiDir, "link.txt"), uiDir, entry)
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "host-secret") {
		t.Fatalf("symlink escape = %d %q", rec.Code, rec.Body.String())
	}
}

func TestPluginUIContentTypeMap(t *testing.T) {
	cases := map[string]string{
		"index.html":  "text/html; charset=utf-8",
		"app.js":      "text/javascript; charset=utf-8",
		"app.mjs":     "text/javascript; charset=utf-8",
		"style.css":   "text/css; charset=utf-8",
		"theme.json":  "application/json; charset=utf-8",
		"logo.svg":    "image/svg+xml",
		"module.wasm": "application/wasm",
	}
	for name, want := range cases {
		if got := pluginUIContentType(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	// An unknown extension falls through to the platform table (or sniffing).
	if got := pluginUIContentType("data.bin"); strings.HasPrefix(got, "text/html") {
		t.Fatalf("unknown extension = %q", got)
	}
}

// TestPluginUIUnwired: a deployment without a plugin manager answers a JSON 404
// (the same shape the web build's 404 uses).
func TestPluginUIUnwired(t *testing.T) {
	handler := newPluginUIHandler(nil, discardLogger())
	if handler.available() {
		t.Fatal("a handler without a manager is not available")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/plugins/vendor.panel/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unwired = %d", rec.Code)
	}
}

// TestLoadHostTheme: the bridge's theme tokens come from the web build, and a
// missing or broken document is not fatal.
func TestLoadHostTheme(t *testing.T) {
	dir := t.TempDir()
	if theme := loadHostTheme(dir, discardLogger()); theme != nil {
		t.Fatalf("missing theme must be nil: %s", theme)
	}
	if theme := loadHostTheme("", discardLogger()); theme != nil {
		t.Fatalf("no root must be nil: %s", theme)
	}
	document := `{"name":"Keel Solid Glass","cssVars":{"light":{"--bg":"oklch(1 0 0)"}}}`
	if err := os.WriteFile(filepath.Join(dir, "theme.json"), []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
	theme := loadHostTheme(dir, discardLogger())
	var parsed map[string]any
	if err := json.Unmarshal(theme, &parsed); err != nil || parsed["name"] != "Keel Solid Glass" {
		t.Fatalf("theme = %s (%v)", theme, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "theme.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if theme := loadHostTheme(dir, discardLogger()); theme != nil {
		t.Fatalf("broken theme must be nil: %s", theme)
	}
}
