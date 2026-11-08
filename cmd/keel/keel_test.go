package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestVersionCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "keel "+version) || !strings.Contains(out, "agent-api: v1") {
		t.Fatalf("version output:\n%s", out)
	}
	if code := run([]string{"nope"}, &stdout, &stderr); code != 2 {
		t.Fatalf("unknown command exit = %d", code)
	}
}

func TestAuthMiddlewarePasswordAndTenantInjection(t *testing.T) {
	tenantID := uuid.New()
	var seenTenant string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenTenant = r.Header.Get("X-Tenant-ID")
		w.WriteHeader(http.StatusOK)
	})
	mux := http.NewServeMux()
	handler := newAuthHandler(authConfig{Password: "s3cret", TenantID: tenantID}, mux, noopLog{})
	handler.register(mux)
	mux.Handle("/", next)
	server := httptest.NewServer(handler)
	defer server.Close()

	// Unauthenticated API request: 401, never a default tenant.
	res, err := server.Client().Get(server.URL + "/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("unauthenticated: %d", res.StatusCode)
	}

	// Wrong passphrase.
	body, _ := json.Marshal(map[string]string{"password": "nope"})
	res, err = server.Client().Post(server.URL+"/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("wrong passphrase: %d", res.StatusCode)
	}

	// Login sets an HttpOnly cookie; the cookie authenticates and the client's
	// own X-Tenant-ID is replaced by the server's tenant.
	body, _ = json.Marshal(map[string]string{"password": "s3cret"})
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	res, err = client.Post(server.URL+"/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	cookies := res.Cookies()
	res.Body.Close()
	if res.StatusCode != 200 || len(cookies) != 1 {
		t.Fatalf("login: %d cookies=%d", res.StatusCode, len(cookies))
	}
	if !cookies[0].HttpOnly || cookies[0].Name != authCookieName {
		t.Fatalf("cookie = %+v", cookies[0])
	}
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/sessions", nil)
	request.Header.Set("X-Tenant-ID", uuid.NewString())
	res, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("cookie auth: %d", res.StatusCode)
	}
	if seenTenant != tenantID.String() {
		t.Fatalf("tenant not injected: %q", seenTenant)
	}

	// Bearer token (CLI/TUI) works without the cookie jar.
	bearer, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/sessions", nil)
	bearer.Header.Set("Authorization", "Bearer s3cret")
	res, err = http.DefaultClient.Do(bearer)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("bearer auth: %d", res.StatusCode)
	}

	// Probes and the login surface are exempt.
	for _, path := range []string{"/v1/health", "/v1/auth/session"} {
		res, err = server.Client().Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("%s must be exempt, got %d", path, res.StatusCode)
		}
	}

	// A tampered cookie is rejected.
	tampered, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/sessions", nil)
	tampered.Header.Set("Cookie", authCookieName+"=AAAA.BBBB")
	res, err = http.DefaultClient.Do(tampered)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("tampered cookie: %d", res.StatusCode)
	}
}

func TestAuthCookieExpiryAndDisabledMode(t *testing.T) {
	tenantID := uuid.New()
	now := time.Now().UTC()
	cfg := authConfig{Password: "p", TenantID: tenantID, SessionTTL: time.Minute, Now: func() time.Time { return now }}
	handler := newAuthHandler(cfg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }), noopLog{})
	login := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewBufferString(`{"password":"p"}`))
	loginReq.ContentLength = int64(len(`{"password":"p"}`))
	handler.login(login, loginReq)
	if login.Code != 200 {
		t.Fatalf("login: %d %s", login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %+v", cookies)
	}
	// Move the clock past the TTL: the same cookie is now invalid.
	cfg.Now = func() time.Time { return now.Add(2 * time.Minute) }
	expired := newAuthHandler(cfg, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }), noopLog{})
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	req.AddCookie(cookies[0])
	rec := httptest.NewRecorder()
	expired.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("expired cookie: %d", rec.Code)
	}

	// Disabled mode (no passphrase) passes through and does not inject a tenant.
	var seen string
	disabled := newAuthHandler(authConfig{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-Tenant-ID")
		w.WriteHeader(200)
	}), noopLog{})
	req = httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	req.Header.Set("X-Tenant-ID", tenantID.String())
	rec = httptest.NewRecorder()
	disabled.ServeHTTP(rec, req)
	if rec.Code != 200 || seen != tenantID.String() {
		t.Fatalf("disabled mode: %d tenant=%q", rec.Code, seen)
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
	err := runServe(context.Background(), serveOptions{DSN: "postgres://ignored", Password: "p"})
	if err == nil || !strings.Contains(err.Error(), "KEEL_SERVE_TENANT") {
		t.Fatalf("password without tenant: %v", err)
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
