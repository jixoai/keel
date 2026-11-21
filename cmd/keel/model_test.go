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

	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/modelcfg"
)

// fakeModelAPI answers the endpoints the CLI consumes: the two read
// projections, and the models document write path (GET/PUT
// /v1/settings/models) the provider management subset edits.
type fakeModelAPI struct {
	mu        sync.Mutex
	username  string
	password  string
	csrf      string
	providers []modelcfg.ProviderView
	models    []modelcfg.ModelView
	probes    map[string]modelcfg.ProviderProbe
	requests  []recordedRequest

	// The models document the GET serves and the PUT replaces. revision is the
	// store's optimistic-concurrency token; expectedRevisions records what the
	// CLI sent; putRefusal, when set, is answered instead of accepting a write
	// (the refusal shape the server produces, e.g. 409 provider_in_use).
	document          map[string]any
	revision          int64
	expectedRevisions []int64
	putRefusal        *fakeRefusal
}

// fakeRefusal is one canned refusal: status, body.
type fakeRefusal struct {
	status int
	body   map[string]any
}

func (f *fakeModelAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{Method: r.Method, Path: r.URL.Path, Body: raw})
	f.mu.Unlock()

	if r.Method == http.MethodGet && r.URL.Path == "/v1/auth/session" {
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"authenticated": false, "csrfToken": f.csrf,
		})
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/v1/auth/login" {
		if strings.TrimSpace(r.Header.Get("X-Keel-CSRF")) != f.csrf {
			writeFakeJSON(w, http.StatusForbidden, map[string]any{"error": "a valid X-Keel-CSRF header is required for writes"})
			return
		}
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		_ = json.Unmarshal(raw, &body)
		if strings.TrimSpace(body.Username) != f.username || strings.TrimSpace(body.Password) != f.password {
			writeFakeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid username or password"})
			return
		}
		http.SetCookie(w, &http.Cookie{Name: auth.SessionCookieName, Value: "fake-session", Path: "/"})
		writeFakeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "csrfToken": f.csrf})
		return
	}
	if cookie, err := r.Cookie(auth.SessionCookieName); err != nil || cookie.Value != "fake-session" {
		writeFakeJSON(w, http.StatusUnauthorized, map[string]any{"error": "authentication required"})
		return
	}
	if r.Method != http.MethodGet && strings.TrimSpace(r.Header.Get("X-Keel-CSRF")) != f.csrf {
		writeFakeJSON(w, http.StatusForbidden, map[string]any{"error": "a valid X-Keel-CSRF header is required for writes"})
		return
	}
	switch {
	case r.URL.Path == "/v1/providers" && r.Method == http.MethodGet:
		writeFakeJSON(w, http.StatusOK, map[string]any{"providers": f.providers, "issues": []modelcfg.Issue{}})
	case strings.HasPrefix(r.URL.Path, "/v1/providers/") && strings.HasSuffix(r.URL.Path, "/test") && r.Method == http.MethodPost:
		providerID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/providers/"), "/test")
		probe, ok := f.probes[providerID]
		if !ok {
			writeFakeJSON(w, http.StatusNotFound, map[string]any{
				"error": "provider " + providerID + " is not served by this deployment",
			})
			return
		}
		writeFakeJSON(w, http.StatusOK, probe)
	case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
		writeFakeJSON(w, http.StatusOK, map[string]any{"models": f.models, "issues": []modelcfg.Issue{}})
	case r.URL.Path == "/v1/settings/models" && r.Method == http.MethodGet:
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"revision": f.revision, "source": "stored", "document": f.document, "selection": map[string]any{},
		})
	case r.URL.Path == "/v1/settings/models" && r.Method == http.MethodPut:
		f.servePut(w, raw)
	default:
		writeFakeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

// servePut is the fake write path: it refuses with the canned body when one is
// set, otherwise it stores the document minus expectedRevision (the server
// strips it too) and moves the revision.
func (f *fakeModelAPI) servePut(w http.ResponseWriter, raw []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putRefusal != nil {
		writeFakeJSON(w, f.putRefusal.status, f.putRefusal.body)
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		writeFakeJSON(w, http.StatusBadRequest, map[string]any{"error": "the models document must be a JSON object"})
		return
	}
	if revisionRaw, ok := fields["expectedRevision"]; ok {
		var revision int64
		if err := json.Unmarshal(revisionRaw, &revision); err == nil {
			f.expectedRevisions = append(f.expectedRevisions, revision)
		}
		delete(fields, "expectedRevision")
	}
	document := map[string]any{}
	for key, value := range fields {
		var decoded any
		if err := json.Unmarshal(value, &decoded); err == nil {
			document[key] = decoded
		}
	}
	f.document = document
	f.revision++
	writeFakeJSON(w, http.StatusOK, map[string]any{
		"revision": f.revision, "source": "stored", "providersExecutable": 2, "modelsSelectable": 3,
	})
}

// storedDocument returns the document the fake currently holds.
func (f *fakeModelAPI) storedDocument() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.document
}

// storedProviders returns providers[] of the stored document.
func (f *fakeModelAPI) storedProviders() []map[string]any {
	document := f.storedDocument()
	raw, _ := document["providers"].([]any)
	entries := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if entry, ok := item.(map[string]any); ok {
			entries = append(entries, entry)
		}
	}
	return entries
}

// fakeModelRegistry builds the fixture: two providers, one of them without a
// name and not executable (the failure state a table must still show), and a
// model id that both providers carry (the ambiguity case of `show`). The models
// document it holds declares two entries — s2api (which the projection also
// serves, i.e. a catalog-like provider) and magpie (document-only, so removing
// it really removes it) — plus configuration the write subset must send back
// verbatim (modelRules).
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
		username: "admin",
		password: "model-test-pass",
		csrf:     "model-fake-csrf",
		providers: []modelcfg.ProviderView{
			{
				ProviderID: "s2api", Name: "S2API", ProviderType: "llm", SurfaceKind: "llm",
				Access:     modelcfg.Access{Type: modelcfg.AccessTypeAPIKey, Env: "S2API_KEY"},
				Executable: true, Selectable: true,
				Models: []modelcfg.ModelView{
					model("s2api"),
					{ProviderID: "s2api", ModelID: "gpt-image-canvas", ProviderType: "image", SurfaceKind: "image", Enabled: true, Executable: true, Selectable: true},
				},
			},
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
		probes: map[string]modelcfg.ProviderProbe{
			"s2api": {
				ProviderID: "s2api", ProviderType: "llm",
				APIType: "openai-chat-completions", Endpoint: "https://s2api.top/v1",
				OK: true, Status: modelcfg.ProbeStatusReady, LatencyMS: 12,
				ModelCount: 2, Models: []string{"deepseek-v4.1-flash", "gpt-image-canvas"},
			},
			"a6api": {
				ProviderID: "a6api", ProviderType: "llm",
				APIType: "openai-chat-completions", Endpoint: "https://a6api.example.test/v1",
				OK: false, Status: modelcfg.ProbeStatusNeedsAuth,
				Error: "no credential could be resolved; no request was sent",
				Issues: []modelcfg.Issue{{
					Field:   "access.env",
					Message: "access.env (A6API_KEY) is not set in this process",
				}},
			},
		},
		document: map[string]any{
			"providers": []any{
				map[string]any{
					"id": "s2api",
					"api": map[string]any{
						"type":    "openai-chat-completions",
						"baseUrl": "https://s2api.top/v1",
					},
					"access":   map[string]any{"type": "api-key", "env": "S2API_KEY"},
					"modelIds": []any{shared},
				},
				map[string]any{
					"id": "magpie",
					"api": map[string]any{
						"type":    "openai-chat-completions",
						"baseUrl": "http://127.0.0.1:18899",
					},
					"access":   map[string]any{"type": "api-key", "env": "KEEL_API_KEY"},
					"modelIds": []any{"bailian-token-plan/qwen3.8-flash"},
				},
			},
			"selection": map[string]any{"providerId": "magpie", "modelId": "bailian-token-plan/qwen3.8-flash"},
			"modelRules": []any{
				map[string]any{"match": "*", "config": map[string]any{"enabled": true}},
			},
		},
		revision: 1,
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
	setAccountEnv(t, "admin", fake.password)

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
	setAccountEnv(t, "admin", fake.password)

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
	setAccountEnv(t, "admin", fake.password)

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
	setAccountEnv(t, "admin", fake.password)

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
	setAccountEnv(t, "admin", fake.password)

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
	setAccountEnv(t, "admin", fake.password)

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
	if !strings.Contains(errBuf.String(), "keel model providers") ||
		!strings.Contains(errBuf.String(), "keel model catalog") ||
		!strings.Contains(errBuf.String(), "provider list") {
		t.Fatalf("usage does not name the commands: %s", errBuf.String())
	}
	out.Reset()
	errBuf.Reset()
	if code := runModelCmd([]string{"frobnicate"}, &out, &errBuf); code != 2 {
		t.Fatalf("unknown command exit = %d, want 2", code)
	}
}

// --- provider management (declared commands, exercised against the stub) -----

// TestModelProviderAddWritesDocument pins `keel model provider add`: the entry
// is appended to the document's providers[], the flags land on the fields the
// manifest's args_schema names, every other entry and every other top-level key
// (modelRules, selection) travel back verbatim, and the write carries the
// revision the document was read at.
func TestModelProviderAddWritesDocument(t *testing.T) {
	fake := fakeModelRegistry()
	server := httptest.NewServer(fake)
	defer server.Close()
	setAccountEnv(t, "admin", fake.password)

	stdout, stderr, code := runModel(
		"provider", "add", "bailian", "--server", server.URL,
		"--type", "llm", "--name", "百炼", "--api-type", "openai-chat-completions",
		"--base-url", "https://bailian.example.test/v1", "--env", "BAILIAN_API_KEY",
		"--model", "qwen3.8-flash", "--model", "qwen3.8-max",
		"--header", "X-Tier: pro", "--visibility", "visible",
	)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "added provider bailian") {
		t.Fatalf("stdout does not report the add: %s", stdout)
	}
	entries := fake.storedProviders()
	if len(entries) != 3 {
		t.Fatalf("providers = %d, want 3", len(entries))
	}
	added := entries[2]
	if added["id"] != "bailian" || added["name"] != "百炼" || added["type"] != "llm" || added["visibility"] != "visible" {
		t.Fatalf("added entry = %+v", added)
	}
	api, _ := added["api"].(map[string]any)
	if api["type"] != "openai-chat-completions" || api["baseUrl"] != "https://bailian.example.test/v1" {
		t.Fatalf("added api = %+v", api)
	}
	headers, _ := api["headers"].(map[string]any)
	if headers["X-Tier"] != "pro" {
		t.Fatalf("added headers = %+v", headers)
	}
	access, _ := added["access"].(map[string]any)
	if access["type"] != "api-key" || access["env"] != "BAILIAN_API_KEY" {
		t.Fatalf("added access = %+v (a credential reference, never a value)", access)
	}
	models, _ := added["modelIds"].([]any)
	if len(models) != 2 || models[0] != "qwen3.8-flash" || models[1] != "qwen3.8-max" {
		t.Fatalf("added modelIds = %+v", models)
	}
	// The entries the command did not touch are sent back untouched, and the
	// keys this CLI never decodes (modelRules) survive the round trip.
	first := entries[0]
	if first["id"] != "s2api" {
		t.Fatalf("the first entry moved: %+v", first)
	}
	if api, _ := first["api"].(map[string]any); api["baseUrl"] != "https://s2api.top/v1" {
		t.Fatalf("the first entry's api changed: %+v", api)
	}
	document := fake.storedDocument()
	if _, ok := document["modelRules"]; !ok {
		t.Fatalf("modelRules were dropped by the write: %+v", document)
	}
	if selection, _ := document["selection"].(map[string]any); selection["providerId"] != "magpie" {
		t.Fatalf("selection changed: %+v", document["selection"])
	}
	// The write carried the revision it read (the optimistic-concurrency guard).
	if len(fake.expectedRevisions) != 1 || fake.expectedRevisions[0] != 1 {
		t.Fatalf("expectedRevision = %+v, want [1]", fake.expectedRevisions)
	}
	// Declaring the same id twice is refused locally instead of overwriting.
	_, stderr, code = runModel("provider", "add", "magpie", "--server", server.URL, "--base-url", "https://x.test/v1")
	if code != 1 || !strings.Contains(stderr, "already declared") {
		t.Fatalf("duplicate add = %d, stderr: %s", code, stderr)
	}
}

// TestModelProviderUpdateAppliesOnlyPassedFlags pins the read-modify-write
// contract: unnamed fields of the edited entry survive, and an id the document
// does not declare yet (a built-in catalog provider) starts a patch entry of
// the same id.
func TestModelProviderUpdateAppliesOnlyPassedFlags(t *testing.T) {
	fake := fakeModelRegistry()
	server := httptest.NewServer(fake)
	defer server.Close()
	setAccountEnv(t, "admin", fake.password)

	stdout, stderr, code := runModel("provider", "update", "s2api", "--visibility", "hidden", "--server", server.URL)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "updated provider s2api") {
		t.Fatalf("stdout does not report the update: %s", stdout)
	}
	entries := fake.storedProviders()
	if len(entries) != 2 {
		t.Fatalf("providers = %d, want 2 (an update never appends)", len(entries))
	}
	updated := entries[0]
	if updated["visibility"] != "hidden" {
		t.Fatalf("visibility was not applied: %+v", updated)
	}
	if api, _ := updated["api"].(map[string]any); api["baseUrl"] != "https://s2api.top/v1" || api["type"] != "openai-chat-completions" {
		t.Fatalf("an unpassed api field changed: %+v", api)
	}
	if access, _ := updated["access"].(map[string]any); access["env"] != "S2API_KEY" {
		t.Fatalf("an unpassed access field changed: %+v", access)
	}
	if models, _ := updated["modelIds"].([]any); len(models) != 1 || models[0] != "deepseek-v4.1-flash" {
		t.Fatalf("an unpassed modelIds changed: %+v", models)
	}

	// A catalog provider the document does not declare gets a patch entry, and
	// the patch carries exactly what the flags named.
	if _, stderr, code := runModel("provider", "update", "deepseek", "--ref", "secret:deepseek_key", "--server", server.URL); code != 0 {
		t.Fatalf("update of an undeclared provider = %d, stderr: %s", code, stderr)
	}
	entries = fake.storedProviders()
	if len(entries) != 3 || entries[2]["id"] != "deepseek" {
		t.Fatalf("patch entry missing: %+v", entries)
	}
	patch := entries[2]
	if access, _ := patch["access"].(map[string]any); access["ref"] != "secret:deepseek_key" || access["type"] != "api-key" {
		t.Fatalf("patch access = %+v", patch["access"])
	}
	if _, named := patch["api"]; named {
		t.Fatalf("the patch invented fields the flags did not name: %+v", patch)
	}
}

// TestModelProviderRemoveDropsEntryAndReportsCatalogRegistry pins the removal
// contract: the entry is dropped, and when the resolved projection still serves
// the id (a built-in catalog provider) the command says so instead of claiming
// the provider is gone.
func TestModelProviderRemoveDropsEntryAndReportsCatalogRegistry(t *testing.T) {
	fake := fakeModelRegistry()
	server := httptest.NewServer(fake)
	defer server.Close()
	setAccountEnv(t, "admin", fake.password)

	// The document's default selection names magpie: removing it without saying
	// where the selection moves is refused before any write (the command never
	// guesses a replacement).
	_, stderr, code := runModel("provider", "remove", "magpie", "--server", server.URL)
	if code != 2 || !strings.Contains(stderr, "--repoint-selection") {
		t.Fatalf("removal of the selected provider = %d, stderr: %s", code, stderr)
	}

	// magpie is document-only: with the selection repointed, the removal is
	// complete and nothing extra is claimed.
	stdout, stderr, code := runModel(
		"provider", "remove", "magpie", "--repoint-selection", "s2api/deepseek-v4.1-flash", "--server", server.URL)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "removed provider magpie") {
		t.Fatalf("stdout does not report the removal: %s", stdout)
	}
	if strings.Contains(stdout, "still served") {
		t.Fatalf("a document-only provider must not be reported as still served: %s", stdout)
	}
	entries := fake.storedProviders()
	if len(entries) != 1 || entries[0]["id"] != "s2api" {
		t.Fatalf("providers after removal = %+v", entries)
	}
	document := fake.storedDocument()
	if selection, _ := document["selection"].(map[string]any); selection["providerId"] != "s2api" || selection["modelId"] != "deepseek-v4.1-flash" {
		t.Fatalf("the selection was not repointed: %+v", document["selection"])
	}

	// s2api stays in the projection (the catalog ships it): the command lands
	// the write and then says the id is still served.
	stdout, stderr, code = runModel(
		"provider", "remove", "s2api", "--repoint-selection", "a6api/deepseek-v4.1-flash", "--server", server.URL)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "still served") || !strings.Contains(stdout, "built-in catalog") {
		t.Fatalf("the catalog case is not reported: %s", stdout)
	}

	// An id the document does not declare is refused before any write.
	before := len(fake.requests)
	_, stderr, code = runModel("provider", "remove", "nope", "--server", server.URL)
	if code != 1 || !strings.Contains(stderr, "does not declare provider") {
		t.Fatalf("unknown id = %d, stderr: %s", code, stderr)
	}
	for _, request := range fake.requests[before:] {
		if request.Method == http.MethodPut {
			t.Fatal("a refused removal still wrote the document")
		}
	}
}

// TestModelProviderRemovePrintsReferenceRefusal pins the 409 provider_in_use
// rendering: the server's code, field path and the referencing registry rows
// (kind/id/name/field) reach stderr, so the operator learns what to repoint.
func TestModelProviderRemovePrintsReferenceRefusal(t *testing.T) {
	fake := fakeModelRegistry()
	fake.putRefusal = &fakeRefusal{status: http.StatusConflict, body: map[string]any{
		"error": "provider magpie is referenced by agent agent_publication 发布助手 (field defaultModel); repoint those bindings or disable the provider instead of removing it",
		"code":  "provider_in_use",
		"fields": []any{map[string]any{
			"field": "providers[magpie]", "code": "provider_in_use",
			"message": "provider magpie is still referenced by a registry document",
		}},
		"references": []any{map[string]any{
			"providerId": "magpie", "kind": "agent", "id": "agent_publication", "name": "发布助手", "field": "defaultModel",
		}},
	}}
	server := httptest.NewServer(fake)
	defer server.Close()
	setAccountEnv(t, "admin", fake.password)

	_, stderr, code := runModel(
		"provider", "remove", "magpie", "--repoint-selection", "s2api/deepseek-v4.1-flash", "--server", server.URL)
	if code != 1 {
		t.Fatalf("a refused removal must exit 1, got %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{
		"409 provider_in_use",
		"providers[magpie] (provider_in_use)",
		"referencing agent: agent_publication 发布助手 (field defaultModel)",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

// TestModelProviderTestProbe pins `keel model provider test`: the command asks
// the server for one real minimal upstream probe (POST
// /v1/providers/{id}/test), never resolves the credential itself, follows the
// measured status with its exit code, and prints every field path the server
// reported. A provider the server does not serve is a readable miss.
func TestModelProviderTestProbe(t *testing.T) {
	fake := fakeModelRegistry()
	server := httptest.NewServer(fake)
	defer server.Close()
	setAccountEnv(t, "admin", fake.password)
	// A credential-looking value in the environment must never reach the CLI
	// output: the CLI only names the reference, the server resolves it.
	const secret = "probe-secret-value-that-must-not-leak"
	t.Setenv("A6API_KEY", secret)

	stdout, stderr, code := runModel("provider", "test", "s2api", "--server", server.URL)
	if code != 0 {
		t.Fatalf("a reachable provider must exit 0, got %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{
		"provider s2api (llm)", "endpoint https://s2api.top/v1", "status ready",
		"latency 12ms", "models 2", "ok: the provider answered a model-list request",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("probe output lacks %q:\n%s", want, stdout)
		}
	}
	// The probe is one POST with an empty body: the credential stays server-side.
	var probeCalls int
	for _, request := range fake.requests {
		switch {
		case request.Method == http.MethodPost && request.Path == "/v1/providers/s2api/test":
			probeCalls++
			if len(strings.TrimSpace(string(request.Body))) != 0 {
				t.Fatalf("the probe request carried a body: %s", request.Body)
			}
		case request.Method == http.MethodGet && request.Path == "/v1/providers":
			t.Fatal("the probe must call the probe endpoint, not re-read the resolved list")
		}
	}
	if probeCalls != 1 {
		t.Fatalf("probe requests = %d, want 1", probeCalls)
	}
	if strings.Contains(stdout+stderr, secret) {
		t.Fatal("a credential value leaked into the probe output")
	}

	// A provider whose credential cannot be resolved fails with needs_auth and
	// the field path, never a fabricated success.
	stdout, _, code = runModel("provider", "test", "a6api", "--server", server.URL)
	if code != 1 {
		t.Fatalf("a needs_auth provider must exit 1, got %d", code)
	}
	for _, want := range []string{"status needs_auth", "access.env: access.env (A6API_KEY)", "not ok:"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("the needs_auth report lacks %q:\n%s", want, stdout)
		}
	}

	// JSON output carries the same measured facts for a script.
	stdout, stderr, code = runModel("provider", "test", "s2api", "--json", "--server", server.URL)
	if code != 0 {
		t.Fatalf("JSON probe exit = %d, stderr: %s", code, stderr)
	}
	var probe modelcfg.ProviderProbe
	if err := json.Unmarshal([]byte(stdout), &probe); err != nil {
		t.Fatalf("probe JSON does not decode: %v\n%s", err, stdout)
	}
	if probe.ProviderID != "s2api" || !probe.OK || probe.Status != modelcfg.ProbeStatusReady ||
		probe.LatencyMS != 12 || probe.ModelCount != 2 {
		t.Fatalf("probe = %+v", probe)
	}

	// An id this server does not serve is a miss (exit 1), never an empty
	// success; the server's 404 text is rendered verbatim.
	_, stderr, code = runModel("provider", "test", "nope", "--server", server.URL)
	if code != 1 || !strings.Contains(stderr, "is not served by this deployment") {
		t.Fatalf("unknown provider = %d, stderr: %s", code, stderr)
	}
}

// TestModelProviderListAlias pins `keel model provider list`: same resolved
// projection as `keel model providers`, with the family filter applied to the
// printed rows only.
func TestModelProviderListAlias(t *testing.T) {
	fake := fakeModelRegistry()
	server := httptest.NewServer(fake)
	defer server.Close()
	setAccountEnv(t, "admin", fake.password)

	stdout, stderr, code := runModel("provider", "list", "--server", server.URL)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "s2api") || !strings.Contains(stdout, "a6api") {
		t.Fatalf("provider list lacks rows:\n%s", stdout)
	}
	stdout, stderr, code = runModel("provider", "list", "--type", "image", "--server", server.URL)
	if code != 0 {
		t.Fatalf("filtered list exit %d, stderr: %s", code, stderr)
	}
	if strings.Contains(stdout, "s2api") || strings.Contains(stdout, "a6api") {
		t.Fatalf("the --type filter kept llm providers:\n%s", stdout)
	}
	stdout, stderr, code = runModel("provider", "list", "--type", "llm", "--json", "--server", server.URL)
	if code != 0 {
		t.Fatalf("JSON list exit %d, stderr: %s", code, stderr)
	}
	var body providerListBody
	if err := json.Unmarshal([]byte(stdout), &body); err != nil || len(body.Providers) != 2 {
		t.Fatalf("JSON list = %v, body %+v", err, body)
	}
}

// TestModelProviderShow pins `keel model provider show`: the full resolved view
// as JSON, credential references only, and a miss for an unknown id.
func TestModelProviderShow(t *testing.T) {
	fake := fakeModelRegistry()
	server := httptest.NewServer(fake)
	defer server.Close()
	setAccountEnv(t, "admin", fake.password)
	const secret = "provider-show-secret-value-that-must-not-leak"
	t.Setenv("S2API_KEY", secret)

	stdout, stderr, code := runModel("provider", "show", "s2api", "--server", server.URL)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var view modelcfg.ProviderView
	if err := json.Unmarshal([]byte(stdout), &view); err != nil {
		t.Fatalf("show did not print the provider JSON: %v\n%s", err, stdout)
	}
	if view.ProviderID != "s2api" || view.Name != "S2API" || !view.Executable ||
		view.Access.Env != "S2API_KEY" || len(view.Models) != 2 {
		t.Fatalf("view = %+v", view)
	}
	if strings.Contains(stdout, secret) {
		t.Fatal("the credential value leaked into provider show")
	}
	_, stderr, code = runModel("provider", "show", "nope", "--server", server.URL)
	if code != 1 || !strings.Contains(stderr, "does not serve a provider") {
		t.Fatalf("unknown provider show = %d, stderr: %s", code, stderr)
	}
}

// TestModelCatalogResolvedAndRefresh pins `keel model catalog`: the resolved
// projection (optionally one provider) and the live upstream mode, which goes
// through the same probe endpoint instead of inventing a catalog.
func TestModelCatalogResolvedAndRefresh(t *testing.T) {
	fake := fakeModelRegistry()
	server := httptest.NewServer(fake)
	defer server.Close()
	setAccountEnv(t, "admin", fake.password)

	stdout, stderr, code := runModel("catalog", "s2api", "--server", server.URL)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "s2api/deepseek-v4.1-flash") || !strings.Contains(stdout, "s2api/gpt-image-canvas") {
		t.Fatalf("resolved catalog lacks the provider's models:\n%s", stdout)
	}
	if strings.Contains(stdout, "a6api/") {
		t.Fatalf("the provider filter leaked another provider:\n%s", stdout)
	}
	stdout, stderr, code = runModel("catalog", "s2api", "--json", "--server", server.URL)
	if code != 0 {
		t.Fatalf("JSON catalog exit %d, stderr: %s", code, stderr)
	}
	var resolved catalogBody
	if err := json.Unmarshal([]byte(stdout), &resolved); err != nil {
		t.Fatalf("catalog JSON does not decode: %v\n%s", err, stdout)
	}
	if resolved.ProviderID != "s2api" || resolved.Source != "resolved" || len(resolved.Models) != 2 {
		t.Fatalf("resolved catalog = %+v", resolved)
	}

	// --refresh sends the probe and prints its live ids, not the resolved view.
	stdout, stderr, code = runModel("catalog", "s2api", "--refresh", "--json", "--server", server.URL)
	if code != 0 {
		t.Fatalf("refresh exit %d, stderr: %s", code, stderr)
	}
	var refreshed catalogRefreshBody
	if err := json.Unmarshal([]byte(stdout), &refreshed); err != nil {
		t.Fatalf("refresh JSON does not decode: %v\n%s", err, stdout)
	}
	if refreshed.ProviderID != "s2api" || refreshed.Source != "upstream" ||
		len(refreshed.Models) != 2 || refreshed.Models[0] != "deepseek-v4.1-flash" {
		t.Fatalf("refreshed catalog = %+v", refreshed)
	}
	var refreshCalls int
	for _, request := range fake.requests {
		if request.Method == http.MethodPost && request.Path == "/v1/providers/s2api/test" {
			refreshCalls++
		}
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh probe calls = %d, want 1", refreshCalls)
	}

	// A refresh without an explicit provider is refused before any request.
	before := len(fake.requests)
	_, stderr, code = runModel("catalog", "--refresh", "--server", server.URL)
	if code != 2 || !strings.Contains(stderr, "--refresh needs a provider id") {
		t.Fatalf("refresh without id = %d, stderr: %s", code, stderr)
	}
	if len(fake.requests) != before {
		t.Fatal("a refused refresh still sent a request")
	}
}

// TestModelProviderAddRendersValidationIssues pins the server-refusal
// rendering: the settings write path answers field paths as modelcfg issues,
// and the CLI must print them instead of only the top-level sentence.
func TestModelProviderAddRendersValidationIssues(t *testing.T) {
	fake := fakeModelRegistry()
	fake.putRefusal = &fakeRefusal{status: http.StatusBadRequest, body: map[string]any{
		"error": "model configuration: providers[bad].api.type: unsupported API type \"nope\"",
		"issues": []any{map[string]any{
			"field":   "providers[bad].api.type",
			"message": "unsupported API type \"nope\"",
		}},
	}}
	server := httptest.NewServer(fake)
	defer server.Close()
	setAccountEnv(t, "admin", fake.password)

	_, stderr, code := runModel("provider", "add", "bad", "--api-type", "nope", "--server", server.URL)
	if code != 1 {
		t.Fatalf("a rejected write must exit 1, got %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{
		"server refused (400)",
		"providers[bad].api.type: unsupported API type",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

// TestModelProviderUsage covers the group's dispatch surface.
func TestModelProviderUsage(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := runModelCmd([]string{"provider"}, &out, &errBuf); code != 2 {
		t.Fatalf("bare provider exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "keel model provider add") ||
		!strings.Contains(errBuf.String(), "keel model provider remove") ||
		!strings.Contains(errBuf.String(), "keel model provider list") ||
		!strings.Contains(errBuf.String(), "keel model provider show") {
		t.Fatalf("usage does not name the verbs: %s", errBuf.String())
	}
	out.Reset()
	errBuf.Reset()
	if code := runModelCmd([]string{"provider", "frobnicate"}, &out, &errBuf); code != 2 {
		t.Fatalf("unknown verb exit = %d, want 2", code)
	}
	out.Reset()
	errBuf.Reset()
	if code := runModelCmd([]string{"provider", "add"}, &out, &errBuf); code != 2 {
		t.Fatalf("add without an id exit = %d, want 2", code)
	}
}
