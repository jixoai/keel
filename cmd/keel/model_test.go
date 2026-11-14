package main

// CLI conformance tests for `keel model` — the declared surface of the
// keel.model plugin (internal/pluginpkg/builtin/keel.model.yaml). Every
// command the manifest declares is exercised here against an httptest fake of
// the AgentAPI, so the declaration and the implementation are checked from both
// sides (the declaration side is TestCapabilityPluginsDeclareTheirCLI in
// internal/agentext).
//
// The passphrase assertion mirrors `keel agent`: it is exchanged at
// /v1/auth/login and never appears in any output.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jixoai/keel/internal/modelcfg"
)

// fakeModelAPI answers the two read endpoints the CLI consumes.
type fakeModelAPI struct {
	mu        sync.Mutex
	password  string
	providers []modelcfg.ProviderView
	models    []modelcfg.ModelView
	requests  []recordedRequest
}

func (f *fakeModelAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{Method: r.Method, Path: r.URL.Path, Body: raw})
	f.mu.Unlock()

	if r.Method == http.MethodPost && r.URL.Path == "/v1/auth/login" {
		var body struct {
			Password string `json:"password"`
		}
		_ = json.Unmarshal(raw, &body)
		if strings.TrimSpace(body.Password) != f.password {
			writeFakeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid passphrase"})
			return
		}
		http.SetCookie(w, &http.Cookie{Name: authCookieName, Value: "fake-session", Path: "/"})
		writeFakeJSON(w, http.StatusOK, map[string]any{"authenticated": true})
		return
	}
	if cookie, err := r.Cookie(authCookieName); err != nil || cookie.Value != "fake-session" {
		writeFakeJSON(w, http.StatusUnauthorized, map[string]any{"error": "authentication required"})
		return
	}
	switch {
	case r.URL.Path == "/v1/providers" && r.Method == http.MethodGet:
		writeFakeJSON(w, http.StatusOK, map[string]any{"providers": f.providers, "issues": []modelcfg.Issue{}})
	case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
		writeFakeJSON(w, http.StatusOK, map[string]any{"models": f.models, "issues": []modelcfg.Issue{}})
	default:
		writeFakeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

// fakeModelRegistry builds the fixture: two providers, one of them without a
// name and not executable (the failure state a table must still show), and a
// model id that both providers carry (the ambiguity case of `show`).
func fakeModelRegistry() *fakeModelAPI {
	shared := "deepseek-v4.1-flash"
	model := func(provider string) modelcfg.ModelView {
		return modelcfg.ModelView{
			ProviderID: provider, ModelID: shared,
			ProviderType: "llm", SurfaceKind: "llm",
			Enabled: true, Executable: true, Selectable: true,
		}
	}
	return &fakeModelAPI{
		password: "model-test-pass",
		providers: []modelcfg.ProviderView{
			{ProviderID: "s2api", Name: "S2API", ProviderType: "llm", SurfaceKind: "llm", Executable: true, Selectable: true, Models: []modelcfg.ModelView{model("s2api")}},
			{ProviderID: "a6api", ProviderType: "llm", SurfaceKind: "llm", Executable: false, Selectable: false, Models: []modelcfg.ModelView{model("a6api")}},
		},
		models: []modelcfg.ModelView{
			model("s2api"),
			func() modelcfg.ModelView {
				view := model("a6api")
				view.Selectable = false
				view.Executable = false
				return view
			}(),
			{ProviderID: "s2api", ModelID: "gpt-image-canvas", ProviderType: "image", SurfaceKind: "image", Enabled: true, Executable: true, Selectable: true},
		},
	}
}

func runModel(args ...string) (stdout, stderr string, code int) {
	var out, errBuf bytes.Buffer
	code = runModelCmd(args, &out, &errBuf)
	return out.String(), errBuf.String(), code
}

func TestModelProvidersTable(t *testing.T) {
	fake := fakeModelRegistry()
	server := httptest.NewServer(fake)
	defer server.Close()
	t.Setenv("KEEL_SERVE_PASSWORD", fake.password)

	stdout, stderr, code := runModel("providers", "--server", server.URL)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	for _, want := range []string{"ID", "NAME", "TYPE", "MODELS", "EXECUTABLE", "s2api", "S2API", "a6api", "yes", "no"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("table lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout+stderr, fake.password) {
		t.Fatal("the passphrase leaked into the output")
	}
	// The login really happened (the endpoints refuse without the cookie).
	loggedIn := false
	for _, request := range fake.requests {
		if request.Path == "/v1/auth/login" {
			loggedIn = true
		}
	}
	if !loggedIn {
		t.Fatal("no /v1/auth/login request was made")
	}
}

func TestModelProvidersJSON(t *testing.T) {
	fake := fakeModelRegistry()
	server := httptest.NewServer(fake)
	defer server.Close()
	t.Setenv("KEEL_SERVE_PASSWORD", fake.password)

	stdout, stderr, code := runModel("providers", "--server", server.URL, "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var body struct {
		Providers []modelcfg.ProviderView `json:"providers"`
	}
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("output is not the raw JSON body: %v\n%s", err, stdout)
	}
	if len(body.Providers) != 2 || body.Providers[0].ProviderID != "s2api" {
		t.Fatalf("providers = %+v", body.Providers)
	}
}

func TestModelListTable(t *testing.T) {
	fake := fakeModelRegistry()
	server := httptest.NewServer(fake)
	defer server.Close()
	t.Setenv("KEEL_SERVE_PASSWORD", fake.password)

	stdout, stderr, code := runModel("list", "--server", server.URL)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	for _, want := range []string{"PROVIDER/MODEL", "SURFACE", "s2api/deepseek-v4.1-flash", "s2api/gpt-image-canvas", "image"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("table lacks %q:\n%s", want, stdout)
		}
	}
}

func TestModelShowQualified(t *testing.T) {
	fake := fakeModelRegistry()
	server := httptest.NewServer(fake)
	defer server.Close()
	t.Setenv("KEEL_SERVE_PASSWORD", fake.password)

	stdout, stderr, code := runModel("show", "s2api/deepseek-v4.1-flash", "--server", server.URL)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var view modelcfg.ModelView
	if err := json.Unmarshal([]byte(stdout), &view); err != nil {
		t.Fatalf("show did not print the model JSON: %v\n%s", err, stdout)
	}
	if view.ProviderID != "s2api" || view.ModelID != "deepseek-v4.1-flash" || view.SurfaceKind != "llm" {
		t.Fatalf("view = %+v", view)
	}
}

func TestModelShowBareIDAmbiguousNamesCandidates(t *testing.T) {
	fake := fakeModelRegistry()
	server := httptest.NewServer(fake)
	defer server.Close()
	t.Setenv("KEEL_SERVE_PASSWORD", fake.password)

	stdout, stderr, code := runModel("show", "deepseek-v4.1-flash", "--server", server.URL)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (ambiguous), stdout: %s, stderr: %s", code, stdout, stderr)
	}
	for _, want := range []string{"s2api/deepseek-v4.1-flash", "a6api/deepseek-v4.1-flash"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("candidates lack %q:\n%s", want, stderr)
		}
	}
}

func TestModelShowNotFound(t *testing.T) {
	fake := fakeModelRegistry()
	server := httptest.NewServer(fake)
	defer server.Close()
	t.Setenv("KEEL_SERVE_PASSWORD", fake.password)

	_, stderr, code := runModel("show", "s2api/nope", "--server", server.URL)
	if code != 1 {
		t.Fatalf("exit = %d, want 1, stderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "does not name a model") {
		t.Fatalf("stderr does not explain the miss: %s", stderr)
	}
}

func TestModelUsageAndUnknownCommand(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := runModelCmd(nil, &out, &errBuf); code != 2 {
		t.Fatalf("no args exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "keel model providers") {
		t.Fatalf("usage does not name the commands: %s", errBuf.String())
	}
	out.Reset()
	errBuf.Reset()
	if code := runModelCmd([]string{"frobnicate"}, &out, &errBuf); code != 2 {
		t.Fatalf("unknown command exit = %d, want 2", code)
	}
}
