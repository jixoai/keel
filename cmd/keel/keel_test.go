package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestVersionCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "keel "+version) || !strings.Contains(out, "harness-api: v1") {
		t.Fatalf("version output:\n%s", out)
	}
	if code := run([]string{"nope"}, &stdout, &stderr); code != 2 {
		t.Fatalf("unknown command exit = %d", code)
	}
}

func TestStaticHandlerSPAFallbackAndCaching(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>app</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app-C3x9f2.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := newStaticHandler(dir, discardLogger())
	if !handler.available() {
		t.Fatal("build should be available")
	}
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := get("/"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "app") || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("index: %d %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Cache-Control"))
	}
	if rec := get("/assets/app-C3x9f2.js"); rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	// Deep link: SPA fallback to index.html.
	if rec := get("/sessions/abc"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "app") {
		t.Fatalf("spa fallback: %d %q", rec.Code, rec.Body.String())
	}
	// Path traversal is refused (never serves outside the build directory).
	if rec := get("/../../etc/passwd"); rec.Code == 200 && strings.Contains(rec.Body.String(), "root:") {
		t.Fatalf("path traversal returned a system file")
	}

	// NSH packages into a nested root: <out>/web/<app>/<App>/index.html.
	nested := t.TempDir()
	nestedRoot := filepath.Join(nested, "web", "keel", "Keel")
	if err := os.MkdirAll(nestedRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nestedRoot, "index.html"), []byte("<html>nested</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	nestedHandler := newStaticHandler(nested, discardLogger())
	if !nestedHandler.available() || nestedHandler.root != nestedRoot {
		t.Fatalf("nested build not resolved: root=%q available=%v", nestedHandler.root, nestedHandler.available())
	}
	rec := httptest.NewRecorder()
	nestedHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "nested") {
		t.Fatalf("nested index: %d %q", rec.Code, rec.Body.String())
	}

	// Missing build: a JSON 404 that names the directory.
	empty := newStaticHandler(filepath.Join(t.TempDir(), "missing"), discardLogger())
	rec = httptest.NewRecorder()
	empty.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "web build not found") {
		t.Fatalf("missing build: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDoctorWithoutDSNFails(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runDoctorCmd([]string{"-dsn", "", "-web-dir", t.TempDir(), "-tenant", uuid.NewString()}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d\n%s", code, stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "keel doctor") || !strings.Contains(out, "[fail]") || !strings.Contains(out, "database") {
		t.Fatalf("doctor output:\n%s", out)
	}

	stdout.Reset()
	code = runDoctorCmd([]string{"-dsn", "", "-tenant", "not-a-uuid"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stdout.String(), "not a UUID") {
		t.Fatalf("invalid tenant: %d\n%s", code, stdout.String())
	}
}

func TestTUIResolutionAndMissingArtifact(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("PATH", tmp) // hide any system keel-tui
	var stdout, stderr bytes.Buffer
	if code := runTUICmd([]string{"--web-dir", tmp}, &stdout, &stderr); code != 3 {
		t.Fatalf("missing artifact exit = %d", code)
	}
	if !strings.Contains(stderr.String(), "KEEL_TUI_BIN") || !strings.Contains(stderr.String(), "nsh package --target tui") {
		t.Fatalf("missing artifact help:\n%s", stderr.String())
	}

	script := filepath.Join(tmp, "keel-tui")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	path, _ := resolveTUI(script, tmp)
	if path == "" {
		t.Fatal("explicit binary not resolved")
	}
	stdout.Reset()
	stderr.Reset()
	if code := runTUICmd([]string{"--bin", script, "--print", "--token", "abcd1234"}, &stdout, &stderr); code != 0 {
		t.Fatalf("print exit = %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), script) || strings.Contains(stdout.String(), "abcd1234") {
		t.Fatalf("print output leaks the token or misses the path:\n%s", stdout.String())
	}
	if code := runTUICmd([]string{"--bin", script}, &stdout, &stderr); code != 0 {
		t.Fatalf("exec exit = %d", code)
	}
}

func TestServeConfigValidation(t *testing.T) {
	if err := runServe(context.Background(), serveOptions{}); err == nil || !strings.Contains(err.Error(), "KEEL_DSN") {
		t.Fatalf("missing dsn: %v", err)
	}
	err := runServe(context.Background(), serveOptions{DSN: "postgres://ignored", AuthMode: authModeRequired})
	if err == nil || !strings.Contains(err.Error(), "KEEL_SERVE_TENANT") {
		t.Fatalf("required auth without tenant: %v", err)
	}
	// local-only and off may not bind a public interface.
	for _, mode := range []authMode{authModeLocalOnly, authModeOff} {
		err := runServe(context.Background(), serveOptions{
			DSN: "postgres://ignored", Addr: "0.0.0.0:13750", AuthMode: mode, Insecure: true,
		})
		if err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Fatalf("%s on 0.0.0.0: %v", mode, err)
		}
	}
	// off needs the explicit unsafe flag.
	err = runServe(context.Background(), serveOptions{
		DSN: "postgres://ignored", Addr: "127.0.0.1:13750", AuthMode: authModeOff,
	})
	if err == nil || !strings.Contains(err.Error(), "--insecure") {
		t.Fatalf("off without --insecure: %v", err)
	}
}

func TestHasherAndCursorHelpers(t *testing.T) {
	// hashedAsset recognizes build-hash names and rejects normal ones.
	cases := map[string]bool{
		"app-C3x9f2.js":    true,
		"index-abc123.css": true,
		"index.html":       false,
		"main.js":          false,
	}
	for name, want := range cases {
		if got := hashedAsset(name); got != want {
			t.Fatalf("hashedAsset(%q) = %v", name, got)
		}
	}
	if splitPaths("") != nil {
		t.Fatal("empty path list must be nil")
	}
	if got := splitPaths("/a" + string(os.PathListSeparator) + "/b"); len(got) != 2 {
		t.Fatalf("splitPaths = %v", got)
	}
}

// noopLog is the auth handler's minimal logger interface.
type noopLog struct{}

func (noopLog) Info(string, ...any) {}
func (noopLog) Warn(string, ...any) {}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
