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

// fakeModelAPI answers the endpoints the CLI consumes: the two read
// projections, and the models document write path (GET/PUT
// /v1/settings/models) the provider management subset edits.
type fakeModelAPI struct {
	mu        sync.Mutex
	password  string
	providers []modelcfg.ProviderView
	models    []modelcfg.ModelView
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
	t.Setenv("KEEL_SERVE_PASSWORD", fake.password)

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
	t.Setenv("KEEL_SERVE_PASSWORD", fake.password)

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
	t.Setenv("KEEL_SERVE_PASSWORD", fake.password)

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
	t.Setenv("KEEL_SERVE_PASSWORD", fake.password)

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

// TestModelProviderTestProbe pins `keel model provider test`: the report comes
// from the server's resolved projections (never an upstream call), the exit
// status follows executability, and a provider the server does not serve is a
// readable miss.
func TestModelProviderTestProbe(t *testing.T) {
	fake := fakeModelRegistry()
	server := httptest.NewServer(fake)
	defer server.Close()
	t.Setenv("KEEL_SERVE_PASSWORD", fake.password)

	stdout, stderr, code := runModel("provider", "test", "s2api", "--server", server.URL)
	if code != 0 {
		t.Fatalf("an executable provider must exit 0, got %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"provider s2api (llm)", "executable yes", "issues —", "deepseek-v4.1-flash", "gpt-image-canvas", "ok:"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("probe output lacks %q:\n%s", want, stdout)
		}
	}
	// The probe really reads both projections and never writes.
	var sawProviders, sawModels, wrote bool
	for _, request := range fake.requests {
		switch {
		case request.Path == "/v1/providers" && request.Method == http.MethodGet:
			sawProviders = true
		case request.Path == "/v1/models" && request.Method == http.MethodGet:
			sawModels = true
		case request.Method != http.MethodGet && request.Path != "/v1/auth/login":
			wrote = true
		}
	}
	if !sawProviders || !sawModels {
		t.Fatalf("the probe must read /v1/providers and /v1/models (providers=%v models=%v)", sawProviders, sawModels)
	}
	if wrote {
		t.Fatal("a probe must not write anything")
	}

	// A provider that cannot execute fails the probe with its state on screen.
	stdout, _, code = runModel("provider", "test", "a6api", "--server", server.URL)
	if code != 1 {
		t.Fatalf("a non-executable provider must exit 1, got %d", code)
	}
	if !strings.Contains(stdout, "executable no") || !strings.Contains(stdout, "not ok:") {
		t.Fatalf("the failure report does not say why:\n%s", stdout)
	}

	// JSON output carries the same facts for a script.
	stdout, stderr, code = runModel("provider", "test", "s2api", "--json", "--server", server.URL)
	if code != 0 {
		t.Fatalf("JSON probe exit = %d, stderr: %s", code, stderr)
	}
	var probe providerProbe
	if err := json.Unmarshal([]byte(stdout), &probe); err != nil {
		t.Fatalf("probe JSON does not decode: %v\n%s", err, stdout)
	}
	if probe.ProviderID != "s2api" || !probe.Executable || !probe.OK || len(probe.Models) != 2 {
		t.Fatalf("probe = %+v", probe)
	}

	// An id this server does not serve is a miss (exit 1), never an empty
	// success.
	_, stderr, code = runModel("provider", "test", "nope", "--server", server.URL)
	if code != 1 || !strings.Contains(stderr, "does not serve a provider") {
		t.Fatalf("unknown provider = %d, stderr: %s", code, stderr)
	}
}

// TestModelProviderUsage covers the group's dispatch surface.
func TestModelProviderUsage(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := runModelCmd([]string{"provider"}, &out, &errBuf); code != 2 {
		t.Fatalf("bare provider exit = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "keel model provider add") ||
		!strings.Contains(errBuf.String(), "keel model provider remove") {
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
