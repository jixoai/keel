package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/modelcfg"
	"github.com/jixoai/keel/internal/store"
)

// --- fakes ---------------------------------------------------------------

// fakeModelSettingsStore is an in-memory ModelSettingsStore with the store's
// revision semantics: a monotonic lifecycle revision per tenant, the expected
// revision guard, and the two sentinels the handlers map onto 409/404.
type fakeModelSettingsStore struct {
	mu       sync.Mutex
	rows     map[uuid.UUID]json.RawMessage
	revision map[uuid.UUID]int64
	at       map[uuid.UUID]time.Time
}

func newFakeModelSettingsStore() *fakeModelSettingsStore {
	return &fakeModelSettingsStore{
		rows:     map[uuid.UUID]json.RawMessage{},
		revision: map[uuid.UUID]int64{},
		at:       map[uuid.UUID]time.Time{},
	}
}

func (f *fakeModelSettingsStore) GetModelSettings(_ context.Context, tenantID uuid.UUID) (json.RawMessage, int64, time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	document, found := f.rows[tenantID]
	if !found {
		return nil, f.revision[tenantID], f.at[tenantID], false, nil
	}
	return document, f.revision[tenantID], f.at[tenantID], true, nil
}

func (f *fakeModelSettingsStore) SetModelSettings(_ context.Context, tenantID uuid.UUID, expectedRevision int64, document json.RawMessage) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	current := f.revision[tenantID]
	if expectedRevision > 0 && expectedRevision != current {
		return current, &store.ModelSettingsConflictError{Current: current}
	}
	f.rows[tenantID] = document
	f.revision[tenantID] = current + 1
	f.at[tenantID] = time.Now().UTC()
	return f.revision[tenantID], nil
}

func (f *fakeModelSettingsStore) DeleteModelSettings(_ context.Context, tenantID uuid.UUID, expectedRevision int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, found := f.rows[tenantID]; !found {
		return f.revision[tenantID], store.ErrNoModelSettings
	}
	current := f.revision[tenantID]
	if expectedRevision > 0 && expectedRevision != current {
		return current, &store.ModelSettingsConflictError{Current: current}
	}
	delete(f.rows, tenantID)
	f.revision[tenantID] = current + 1
	f.at[tenantID] = time.Now().UTC()
	return f.revision[tenantID], nil
}

// fakeSettingsRuntime is the ModelSettingsRuntime stand-in: it records what was
// applied and can be told to reject or to fail the apply.
type fakeSettingsRuntime struct {
	deployment  json.RawMessage
	candidate   ModelCandidate
	issues      []modelcfg.Issue
	validateErr error
	applyErr    error

	// rejected is the stored override this process refused at boot, if any.
	rejectedRevision int64
	rejectedReason   string
	rejected         bool

	mu      sync.Mutex
	applied []json.RawMessage
}

func (f *fakeSettingsRuntime) DeploymentModels() json.RawMessage { return f.deployment }

func (f *fakeSettingsRuntime) RejectedModelsOverride() (int64, string, bool) {
	return f.rejectedRevision, f.rejectedReason, f.rejected
}

func (f *fakeSettingsRuntime) ValidateModelsDocument(document json.RawMessage) (ModelCandidate, []modelcfg.Issue, error) {
	if f.validateErr != nil {
		return ModelCandidate{}, nil, f.validateErr
	}
	if len(f.issues) > 0 {
		return ModelCandidate{}, f.issues, nil
	}
	return f.candidate, nil, nil
}

func (f *fakeSettingsRuntime) ApplyModels(_ context.Context, document json.RawMessage) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, document)
	return nil
}

func (f *fakeSettingsRuntime) appliedDocuments() []json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]json.RawMessage(nil), f.applied...)
}

type fakeSecretStore struct {
	mu      sync.Mutex
	values  map[uuid.UUID]map[string][]byte
	created map[string]time.Time
}

func newFakeSecretStore() *fakeSecretStore {
	return &fakeSecretStore{values: map[uuid.UUID]map[string][]byte{}, created: map[string]time.Time{}}
}

func (f *fakeSecretStore) List(_ context.Context, tenantID uuid.UUID) ([]SecretMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SecretMeta, 0, len(f.values[tenantID]))
	for name := range f.values[tenantID] {
		key := tenantID.String() + "/" + name
		out = append(out, SecretMeta{Name: name, CreatedAt: f.created[key], UpdatedAt: time.Now().UTC()})
	}
	return out, nil
}

func (f *fakeSecretStore) Put(_ context.Context, tenantID uuid.UUID, name string, value []byte) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.values[tenantID] == nil {
		f.values[tenantID] = map[string][]byte{}
	}
	key := tenantID.String() + "/" + name
	if _, exists := f.values[tenantID][name]; !exists {
		f.created[key] = time.Now().UTC()
	}
	f.values[tenantID][name] = append([]byte(nil), value...)
	return time.Now().UTC(), nil
}

func (f *fakeSecretStore) Delete(_ context.Context, tenantID uuid.UUID, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, found := f.values[tenantID][name]; !found {
		return false, nil
	}
	delete(f.values[tenantID], name)
	return true, nil
}

func settingsServer(t *testing.T, mutate func(*Config)) *httptest.Server {
	t.Helper()
	cfg := Config{Logger: testLogger()}
	if mutate != nil {
		mutate(&cfg)
	}
	server := httptest.NewServer(New(cfg))
	t.Cleanup(server.Close)
	return server
}

func doSettingsJSON(t *testing.T, client *http.Client, method, url string, tenant uuid.UUID, body any, out any) int {
	t.Helper()
	return doJSON(t, client, method, url, tenant, body, out)
}

// --- model settings ------------------------------------------------------

// TestModelSettingsRoutes pins the GET/PUT/DELETE contract and the
// source/revision lifecycle: a deployment document until a write stores an
// override, the stored revision while it is in place, and the deployment
// document again after the reset.
func TestModelSettingsRoutes(t *testing.T) {
	settings := newFakeModelSettingsStore()
	runtime := &fakeSettingsRuntime{
		deployment: json.RawMessage(`{"selection":{"providerId":"s2api","modelId":"deepseek-v4.1-flash"}}`),
		candidate: ModelCandidate{
			Selection:           modelcfg.Selection{ProviderID: "s2api", ModelID: "claude-sonnet-5", ReasoningLevel: "high"},
			ProvidersExecutable: 2,
			ModelsSelectable:    5,
		},
	}
	server := settingsServer(t, func(cfg *Config) {
		cfg.ModelSettingsStore = settings
		cfg.ModelSettingsRuntime = runtime
		cfg.Models = stubCatalog{defaultSelection: modelcfg.Selection{
			ProviderID: "s2api", ModelID: "deepseek-v4.1-flash", ReasoningLevel: "high",
		}}
	})
	client := server.Client()
	tenant := uuid.New()

	var got modelSettingsResponse
	// GET before any write: the deployment document at revision 0.
	if code := doSettingsJSON(t, client, http.MethodGet, server.URL+"/v1/settings/models", tenant, nil, &got); code != 200 {
		t.Fatalf("GET deployment = %d", code)
	}
	if got.Source != "deployment" || got.Revision != 0 {
		t.Fatalf("deployment read = %+v", got)
	}
	if strings.Contains(string(got.Document), "claude-sonnet-5") {
		t.Fatalf("the deployment document leaked into the stored read: %s", got.Document)
	}
	if got.Issues == nil {
		t.Fatal("issues must render as [] when there are none")
	}
	// The effective default selection travels with the read, so a page can show
	// it even when the document itself names none.
	if got.Selection.ProviderID != "s2api" || got.Selection.ModelID != "deepseek-v4.1-flash" ||
		got.Selection.ReasoningLevel != "high" {
		t.Fatalf("reported default selection = %+v", got.Selection)
	}

	document := map[string]any{
		"providers": []any{map[string]any{"id": "s2api", "modelIds": []string{"claude-sonnet-5"}}},
		"selection": map[string]any{"providerId": "s2api", "modelId": "claude-sonnet-5", "reasoningLevel": "high"},
	}
	var written modelSettingsWriteResponse
	if code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/models", tenant, document, &written); code != 200 {
		t.Fatalf("PUT = %d", code)
	}
	if written.Revision != 1 || written.Source != "stored" {
		t.Fatalf("PUT summary = %+v", written)
	}
	if written.Selection.ProviderID != "s2api" || written.Selection.ModelID != "claude-sonnet-5" || written.Selection.ReasoningLevel != "high" {
		t.Fatalf("PUT selection = %+v", written.Selection)
	}
	if written.ProvidersExecutable != 2 || written.ModelsSelectable != 5 {
		t.Fatalf("PUT counts = %d/%d", written.ProvidersExecutable, written.ModelsSelectable)
	}
	applied := runtime.appliedDocuments()
	if len(applied) != 1 {
		t.Fatalf("applied documents = %d, want 1", len(applied))
	}
	var appliedDocument map[string]any
	if err := json.Unmarshal(applied[0], &appliedDocument); err != nil {
		t.Fatal(err)
	}
	if _, ok := appliedDocument["expectedRevision"]; ok {
		t.Fatal("expectedRevision must never be persisted or applied")
	}

	// The stored read reflects the write immediately.
	got = modelSettingsResponse{}
	if code := doSettingsJSON(t, client, http.MethodGet, server.URL+"/v1/settings/models", tenant, nil, &got); code != 200 {
		t.Fatalf("GET stored = %d", code)
	}
	if got.Source != "stored" || got.Revision != 1 {
		t.Fatalf("stored read = %+v", got)
	}
	if !strings.Contains(string(got.Document), "claude-sonnet-5") {
		t.Fatalf("stored document = %s", got.Document)
	}

	// A guarded write with the current revision lands at the next revision.
	if id := settingsRow(t, settings, tenant); id != 1 {
		t.Fatalf("store revision = %d, want 1", id)
	}
	guarded := map[string]any{
		"selection":        map[string]any{"providerId": "s2api", "modelId": "claude-sonnet-5"},
		"expectedRevision": 1,
	}
	written = modelSettingsWriteResponse{}
	if code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/models", tenant, guarded, &written); code != 200 || written.Revision != 2 {
		t.Fatalf("guarded PUT = %d %+v", code, written)
	}

	// Reset: 200 with source=deployment, the runtime goes back to the deployment
	// document (nil), and the read reports the new revision with no override.
	var reset modelSettingsResetResponse
	if code := doSettingsJSON(t, client, http.MethodDelete, server.URL+"/v1/settings/models?expectedRevision=2", tenant, nil, &reset); code != 200 {
		t.Fatalf("DELETE = %d", code)
	}
	if reset.Revision != 3 || reset.Source != "deployment" {
		t.Fatalf("DELETE response = %+v", reset)
	}
	if applied = runtime.appliedDocuments(); len(applied) != 3 || applied[2] != nil {
		t.Fatalf("reset must apply the deployment document (nil): %q", applied)
	}
	got = modelSettingsResponse{}
	if code := doSettingsJSON(t, client, http.MethodGet, server.URL+"/v1/settings/models", tenant, nil, &got); code != 200 {
		t.Fatalf("GET after reset = %d", code)
	}
	if got.Source != "deployment" || got.Revision != 3 {
		t.Fatalf("read after reset = %+v", got)
	}
	// A repeated reset is idempotent and writes nothing.
	before := settingsRow(t, settings, tenant)
	reset = modelSettingsResetResponse{}
	if code := doSettingsJSON(t, client, http.MethodDelete, server.URL+"/v1/settings/models", tenant, nil, &reset); code != 200 {
		t.Fatalf("idempotent DELETE = %d", code)
	}
	if reset.Revision != before || reset.Source != "deployment" {
		t.Fatalf("idempotent DELETE response = %+v (store revision %d)", reset, before)
	}
	if after := settingsRow(t, settings, tenant); after != before {
		t.Fatalf("an idempotent reset wrote a revision: %d -> %d", before, after)
	}
}

// settingsRow reports the store's lifecycle revision for a tenant.
func settingsRow(t *testing.T, store *fakeModelSettingsStore, tenantID uuid.UUID) int64 {
	t.Helper()
	_, revision, _, _, err := store.GetModelSettings(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("read store revision: %v", err)
	}
	return revision
}

// TestModelSettingsValidationAndConflicts pins the rejection shapes: a document
// the runtime refuses is 400 with field-path issues, a revision guard failure
// is 409 carrying the revision that won, and neither leaves a trace.
func TestModelSettingsValidationAndConflicts(t *testing.T) {
	settings := newFakeModelSettingsStore()
	runtime := &fakeSettingsRuntime{deployment: json.RawMessage(`{}`)}
	server := settingsServer(t, func(cfg *Config) {
		cfg.ModelSettingsStore = settings
		cfg.ModelSettingsRuntime = runtime
	})
	client := server.Client()
	tenant := uuid.New()

	runtime.issues = []modelcfg.Issue{{Field: "selection.modelId", Message: `unknown model "claude-sonnet-9"`}}
	var body struct {
		Error  string           `json:"error"`
		Issues []modelcfg.Issue `json:"issues"`
	}
	code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/models", tenant,
		map[string]any{"selection": map[string]any{"modelId": "claude-sonnet-9"}}, &body)
	if code != 400 {
		t.Fatalf("rejected document = %d, want 400", code)
	}
	if len(body.Issues) != 1 || body.Issues[0].Field != "selection.modelId" || !strings.Contains(body.Error, "claude-sonnet-9") {
		t.Fatalf("400 body = %+v", body)
	}
	if revision := settingsRow(t, settings, tenant); revision != 0 {
		t.Fatalf("a rejected document must not write: revision %d", revision)
	}
	if applied := runtime.appliedDocuments(); len(applied) != 0 {
		t.Fatalf("a rejected document must not apply: %q", applied)
	}

	// A valid write, then a stale guard.
	runtime.issues = nil
	if code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/models", tenant,
		map[string]any{"selection": map[string]any{"modelId": "m"}}, nil); code != 200 {
		t.Fatalf("seed write = %d", code)
	}
	body.Issues = nil
	var conflict struct {
		Error    string `json:"error"`
		Revision int64  `json:"revision"`
	}
	code = doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/models", tenant,
		map[string]any{"selection": map[string]any{"modelId": "m"}, "expectedRevision": 99}, &conflict)
	if code != 409 {
		t.Fatalf("stale guard = %d, want 409", code)
	}
	if conflict.Revision != 1 {
		t.Fatalf("conflict must carry the current revision: %+v", conflict)
	}
	code = doSettingsJSON(t, client, http.MethodDelete, server.URL+"/v1/settings/models?expectedRevision=7", tenant, nil, &conflict)
	if code != 409 || conflict.Revision != 1 {
		t.Fatalf("stale reset guard = %d %+v, want 409 carrying revision 1", code, conflict)
	}

	// A body that names a tenant is refused: scoping comes from the session.
	code = doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/models", tenant,
		map[string]any{"tenantId": uuid.NewString(), "selection": map[string]any{"modelId": "m"}}, &body)
	if code != 400 || !strings.Contains(body.Error, "tenantId") {
		t.Fatalf("tenantId in the body = %d %+v, want 400", code, body)
	}
}

// TestModelSettingsReloadFailureIsCompensated: a document that validates but
// cannot be assembled rejects the write (422) AND restores the stored state, so
// the store never claims an override the runtime is not serving.
func TestModelSettingsReloadFailureIsCompensated(t *testing.T) {
	settings := newFakeModelSettingsStore()
	runtime := &fakeSettingsRuntime{
		deployment: json.RawMessage(`{"selection":{"providerId":"s2api","modelId":"deepseek-v4.1-flash"}}`),
		candidate:  ModelCandidate{Selection: modelcfg.Selection{ProviderID: "s2api", ModelID: "claude-sonnet-5"}},
	}
	server := settingsServer(t, func(cfg *Config) {
		cfg.ModelSettingsStore = settings
		cfg.ModelSettingsRuntime = runtime
	})
	client := server.Client()
	tenant := uuid.New()

	// First failure with no previous override: the row must be gone afterwards.
	runtime.applyErr = errors.New("model configuration: assembly refused")
	var body struct {
		Error string `json:"error"`
	}
	code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/models", tenant,
		map[string]any{"selection": map[string]any{"modelId": "claude-sonnet-5"}}, &body)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("reload failure = %d, want 422", code)
	}
	if !strings.Contains(body.Error, "assembly refused") {
		t.Fatalf("reload failure body = %+v", body)
	}
	// The compensation is itself an audited transition: revision 1 stored the
	// document, revision 2 cleared it again. What matters is that no override is
	// left stored.
	_, revision, _, found, err := settings.GetModelSettings(context.Background(), tenant)
	if err != nil || found || revision != 2 {
		t.Fatalf("compensated store = (revision %d, found %v, err %v), want no override after the compensating reset", revision, found, err)
	}

	// With a previous override: the write restores it instead of clearing it.
	runtime.applyErr = nil
	if code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/models", tenant,
		map[string]any{"selection": map[string]any{"modelId": "first"}}, nil); code != 200 {
		t.Fatalf("seed write = %d", code)
	}
	seedRevision := settingsRow(t, settings, tenant)
	runtime.applyErr = errors.New("model configuration: assembly refused")
	if code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/models", tenant,
		map[string]any{"selection": map[string]any{"modelId": "second"}}, &body); code != http.StatusUnprocessableEntity {
		t.Fatalf("second reload failure = %d, want 422", code)
	}
	// The refused write took a revision, the compensating restore took the next.
	document, revision, _, found, err := settings.GetModelSettings(context.Background(), tenant)
	if err != nil || !found || revision != seedRevision+2 {
		t.Fatalf("restored store = (found %v, revision %d, err %v), want the previous override at revision %d",
			found, revision, err, seedRevision+2)
	}
	if !strings.Contains(string(document), "first") {
		t.Fatalf("the previous override was not restored: %s", document)
	}

	// The read endpoint agrees: the restored document is what is stored.
	var got modelSettingsResponse
	if code := doSettingsJSON(t, client, http.MethodGet, server.URL+"/v1/settings/models", tenant, nil, &got); code != 200 {
		t.Fatalf("GET after compensation = %d", code)
	}
	if got.Source != "stored" || got.Revision != seedRevision+2 {
		t.Fatalf("read after compensation = %+v (want revision %d)", got, seedRevision+2)
	}
}

// TestModelSettingsTenantScoping: the tenant is the session's and only the
// session's; one tenant's writes and reads never touch another's.
func TestModelSettingsTenantScoping(t *testing.T) {
	settings := newFakeModelSettingsStore()
	runtime := &fakeSettingsRuntime{deployment: json.RawMessage(`{}`)}
	server := settingsServer(t, func(cfg *Config) {
		cfg.ModelSettingsStore = settings
		cfg.ModelSettingsRuntime = runtime
	})
	client := server.Client()
	a, b := uuid.New(), uuid.New()

	if code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/models", a,
		map[string]any{"selection": map[string]any{"modelId": "a-model"}}, nil); code != 200 {
		t.Fatalf("write a = %d", code)
	}
	var got modelSettingsResponse
	if code := doSettingsJSON(t, client, http.MethodGet, server.URL+"/v1/settings/models", b, nil, &got); code != 200 {
		t.Fatalf("read b = %d", code)
	}
	if got.Source != "deployment" || strings.Contains(string(got.Document), "a-model") {
		t.Fatalf("tenant b saw tenant a's override: %+v", got)
	}
	// Without the injected tenant there is no implicit scope.
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		var body map[string]any
		if code := doSettingsJSON(t, client, method, server.URL+"/v1/settings/models", uuid.Nil, nil, &body); code != 401 {
			t.Fatalf("%s without X-Tenant-ID = %d, want 401", method, code)
		}
	}
}

// TestModelSettingsUnwired: without both seams the routes say so instead of
// pretending the surface exists.
func TestModelSettingsUnwired(t *testing.T) {
	server := settingsServer(t, nil)
	client := server.Client()
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/settings/models"},
		{http.MethodPut, "/v1/settings/models"},
		{http.MethodDelete, "/v1/settings/models"},
	} {
		var body map[string]any
		code := doSettingsJSON(t, client, tc.method, server.URL+tc.path, uuid.New(), nil, &body)
		if code != 501 || body["status"] != "not_wired" {
			t.Fatalf("%s %s = %d %v, want 501 not_wired", tc.method, tc.path, code, body)
		}
	}
}

// TestModelCatalogLiveSeam: the registry endpoints read the catalog per request
// — a settings write that installs configuration must be visible to the next
// call, and a runtime without any must answer 501 exactly like a boot without a
// seam.
func TestModelCatalogLiveSeam(t *testing.T) {
	var current stubCatalog
	installed := false
	server := settingsServer(t, func(cfg *Config) {
		cfg.ModelsLive = func() ModelCatalog {
			if !installed {
				return nil
			}
			return current
		}
	})
	client := server.Client()
	tenant := uuid.New()

	var body map[string]any
	if code := doSettingsJSON(t, client, http.MethodGet, server.URL+"/v1/models", tenant, nil, &body); code != 501 {
		t.Fatalf("live seam resolving nil = %d, want 501", code)
	}
	providers, models, _ := testViews()
	current = stubCatalog{providers: providers, models: models}
	installed = true
	var list modelsResponse
	if code := doSettingsJSON(t, client, http.MethodGet, server.URL+"/v1/models", tenant, nil, &list); code != 200 {
		t.Fatalf("live seam after install = %d", code)
	}
	if len(list.Models) != 1 || list.Models[0].ModelID != "deepseek-v4.1-flash" {
		t.Fatalf("live models = %+v", list.Models)
	}
}

// --- secrets -------------------------------------------------------------

// TestSettingsSecretsRoutes pins the secret surface: names and timestamps only,
// one-way values, idempotent deletes, and tenant scoping.
func TestSettingsSecretsRoutes(t *testing.T) {
	store := newFakeSecretStore()
	server := settingsServer(t, func(cfg *Config) { cfg.SecretStore = store })
	client := server.Client()
	tenant := uuid.New()

	var list secretsResponse
	if code := doSettingsJSON(t, client, http.MethodGet, server.URL+"/v1/settings/secrets", tenant, nil, &list); code != 200 {
		t.Fatalf("list = %d", code)
	}
	if !list.Available || list.Secrets == nil || len(list.Secrets) != 0 {
		t.Fatalf("empty list = %+v", list)
	}

	const secretValue = "sk-live-should-never-come-back"
	var raw json.RawMessage
	var written secretWriteResponse
	if code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/secrets/s2api", tenant,
		map[string]string{"value": secretValue}, &written); code != 200 {
		t.Fatalf("put = %d", code)
	}
	if written.Name != "s2api" || written.UpdatedAt.IsZero() {
		t.Fatalf("put response = %+v", written)
	}
	if code := doSettingsJSON(t, client, http.MethodGet, server.URL+"/v1/settings/secrets", tenant, nil, &raw); code != 200 {
		t.Fatalf("list after put = %d", code)
	}
	if strings.Contains(string(raw), secretValue) || strings.Contains(string(raw), "sk-") {
		t.Fatalf("the list leaked the value or a value-shaped field: %s", raw)
	}
	if !strings.Contains(string(raw), `"s2api"`) {
		t.Fatalf("the list must name the secret: %s", raw)
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if list.Secrets[0].CreatedAt.IsZero() || list.Secrets[0].UpdatedAt.IsZero() {
		t.Fatalf("the list must carry the timestamps: %+v", list.Secrets[0])
	}

	// Overwrite is a 200 with a fresh timestamp; deletes are idempotent.
	if code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/secrets/s2api", tenant,
		map[string]string{"value": "rotated"}, &written); code != 200 {
		t.Fatalf("overwrite = %d", code)
	}
	var deleted secretDeleteResponse
	if code := doSettingsJSON(t, client, http.MethodDelete, server.URL+"/v1/settings/secrets/s2api", tenant, nil, &deleted); code != 200 || !deleted.Deleted {
		t.Fatalf("delete = %d %+v", code, deleted)
	}
	deleted = secretDeleteResponse{}
	if code := doSettingsJSON(t, client, http.MethodDelete, server.URL+"/v1/settings/secrets/s2api", tenant, nil, &deleted); code != 200 || deleted.Deleted {
		t.Fatalf("second delete = %d %+v, want an idempotent 200", code, deleted)
	}

	// Tenant scoping: b never sees a's secret, and a's name grammar is enforced.
	if code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/secrets/other", tenant,
		map[string]string{"value": "v"}, nil); code != 200 {
		t.Fatalf("put other = %d", code)
	}
	if value, found := store.values[uuid.New()]["other"]; found && value != nil {
		t.Fatal("a secret landed under a foreign tenant")
	}
	for _, name := range []string{"has space", "bad!name", strings.Repeat("x", 65)} {
		var body map[string]any
		code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/secrets/"+url.PathEscape(name), tenant,
			map[string]string{"value": "v"}, &body)
		if code != 400 {
			t.Fatalf("name %q = %d %v, want 400", name, code, body)
		}
	}
	// A name that would need a second path segment is not routable at all.
	if code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/secrets/bad/slash", tenant,
		map[string]string{"value": "v"}, nil); code != 404 {
		t.Fatalf("a multi-segment name = %d, want 404", code)
	}
	var body map[string]any
	if code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/secrets/empty", tenant,
		map[string]string{"value": ""}, &body); code != 400 {
		t.Fatalf("empty value = %d %v, want 400", code, body)
	}
	if code := doSettingsJSON(t, client, http.MethodGet, server.URL+"/v1/settings/secrets", uuid.Nil, nil, &body); code != 401 {
		t.Fatalf("list without a tenant = %d, want 401", code)
	}
}

// TestSettingsSecretsUnavailable: without a secret store the list explains
// itself (200 + available=false + reason) and every write is refused — never a
// plaintext fallback.
func TestSettingsSecretsUnavailable(t *testing.T) {
	server := settingsServer(t, nil)
	client := server.Client()
	tenant := uuid.New()

	var list secretsResponse
	if code := doSettingsJSON(t, client, http.MethodGet, server.URL+"/v1/settings/secrets", tenant, nil, &list); code != 200 {
		t.Fatalf("unavailable list = %d", code)
	}
	if list.Available || list.Reason == "" || !strings.Contains(list.Reason, "KEEL_SECRET_KEY") {
		t.Fatalf("unavailable list = %+v", list)
	}
	if list.Secrets == nil {
		t.Fatal("the list must render as [], not null")
	}
	var body map[string]any
	if code := doSettingsJSON(t, client, http.MethodPut, server.URL+"/v1/settings/secrets/s2api", tenant,
		map[string]string{"value": "v"}, &body); code != 503 {
		t.Fatalf("put without a store = %d %v, want 503", code, body)
	}
	if message, _ := body["error"].(string); !strings.Contains(message, "KEEL_SECRET_KEY") {
		t.Fatalf("503 must explain the missing configuration: %v", body)
	}
	if code := doSettingsJSON(t, client, http.MethodDelete, server.URL+"/v1/settings/secrets/s2api", tenant, nil, &body); code != 503 {
		t.Fatalf("delete without a store = %d, want 503", code)
	}
}

// --- plugins -------------------------------------------------------------

// TestSettingsPluginsInventory: the settings plugin section is the assembled
// snapshot plus the trigger list, and it states the boot-time lifecycle instead
// of implying plugin management.
func TestSettingsPluginsInventory(t *testing.T) {
	starter := &countingStarter{}
	hooks := agentext.NewHookBus(agentext.HookBusOptions{Logger: testLogger()})
	defer hooks.Close()
	triggers := agentext.NewTriggerEngine(agentext.TriggerEngineOptions{Starter: starter, Hooks: hooks, Logger: testLogger()})
	defer triggers.Close()
	plugins := agentext.NewRuntime(agentext.RuntimeOptions{
		Plugins: []*agentext.Plugin{{
			ID:       "test.plugin",
			Version:  "1.0.0",
			Triggers: []agentext.TriggerSpec{{ID: "test.webhook", Kind: agentext.TriggerWebhook, Prompt: "go", Enabled: true}},
		}},
		Hooks: hooks, Triggers: triggers, Logger: testLogger(),
	})
	if err := plugins.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer plugins.Close(context.Background())
	server := settingsServer(t, func(cfg *Config) {
		cfg.Plugins = plugins
		cfg.Hooks = hooks
		cfg.Triggers = triggers
	})
	client := server.Client()

	var raw json.RawMessage
	if code := doSettingsJSON(t, client, http.MethodGet, server.URL+"/v1/settings/plugins", uuid.New(), nil, &raw); code != 200 {
		t.Fatalf("plugins inventory = %d", code)
	}
	var body settingsPluginsResponse
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.Lifecycle != "boot" || !strings.Contains(body.Description, "boot") {
		t.Fatalf("the inventory must state the lifecycle: %+v", body)
	}
	if body.Generation != 1 || len(body.Plugins) != 1 || body.Plugins[0].ID != "test.plugin" {
		t.Fatalf("the inventory must carry the snapshot: %+v", body.Plugins)
	}
	if len(body.Triggers) != 1 || body.Triggers[0].ID != "test.webhook" {
		t.Fatalf("the inventory must carry the triggers: %+v", body.Triggers)
	}
	if body.Tools == nil {
		t.Fatalf("empty lists must render as []: %s", raw)
	}

	// The fire action is the existing endpoint, and its result is the whole
	// answer the page needs.
	var fire struct {
		Result agentext.FireResult `json:"result"`
	}
	if code := doSettingsJSON(t, client, http.MethodPost, server.URL+"/v1/triggers/test.webhook/fire", uuid.New(),
		map[string]any{"idempotencyKey": "settings-ui"}, &fire); code != 200 {
		t.Fatalf("fire = %d", code)
	}
	if fire.Result.Status != "fired" || fire.Result.TriggerID != "test.webhook" || fire.Result.SessionID == "" {
		t.Fatalf("fire result = %+v", fire.Result)
	}
}

func TestSettingsPluginsUnwired(t *testing.T) {
	server := settingsServer(t, nil)
	var body map[string]any
	if code := doSettingsJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/settings/plugins", uuid.New(), nil, &body); code != 501 {
		t.Fatalf("unwired plugins = %d %v, want 501", code, body)
	}
}

// TestSettingsCapabilitiesDiscoverable: the settings surface is discoverable
// per section, so a client can render exactly the sections this deployment
// serves.
func TestSettingsCapabilitiesDiscoverable(t *testing.T) {
	settings := newFakeModelSettingsStore()
	server := settingsServer(t, func(cfg *Config) {
		cfg.ModelSettingsStore = settings
		cfg.ModelSettingsRuntime = &fakeSettingsRuntime{}
		cfg.SecretStore = newFakeSecretStore()
	})
	var caps Capabilities
	if code := doSettingsJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/capabilities", uuid.New(), nil, &caps); code != 200 {
		t.Fatalf("capabilities = %d", code)
	}
	if !caps.Features["settings.models"] || !caps.Features["settings.secrets"] {
		t.Fatalf("settings features = %v", caps.Features)
	}

	bare := settingsServer(t, nil)
	caps = Capabilities{}
	if code := doSettingsJSON(t, bare.Client(), http.MethodGet, bare.URL+"/v1/capabilities", uuid.New(), nil, &caps); code != 200 {
		t.Fatalf("bare capabilities = %d", code)
	}
	if caps.Features["settings.models"] || caps.Features["settings.secrets"] {
		t.Fatalf("a deployment without the seams must not advertise them: %v", caps.Features)
	}
}

// TestSecretNameGrammarMatchesReferences pins the grammar to the one a models
// document can reference: a stored secret nobody can reference is a footgun,
// so the write edge refuses it (e.g. a hyphenated name).
func TestSecretNameGrammarMatchesReferences(t *testing.T) {
	for _, name := range []string{"s2api", "_k", "A1_b2"} {
		if !secretNamePattern.MatchString(name) {
			t.Fatalf("%q must be accepted (a models document can reference it)", name)
		}
	}
	for _, name := range []string{"e2e-key", "a.b", "9lead", "", "a b", "a/b"} {
		if secretNamePattern.MatchString(name) {
			t.Fatalf("%q must be refused (no models document could reference it)", name)
		}
	}
}

// TestModelSettingsReadNamesARejectedOverride pins the boot fallback's read
// side: when the process refused a stored override at boot, the read reports
// what is really serving (the deployment document) and names the override it
// refused, so the settings page can point the operator at the row to fix or
// delete instead of showing a document that is not in effect.
func TestModelSettingsReadNamesARejectedOverride(t *testing.T) {
	settings := newFakeModelSettingsStore()
	runtime := &fakeSettingsRuntime{
		deployment:       json.RawMessage(`{"selection":{"providerId":"s2api","modelId":"deepseek-v4.1-flash"}}`),
		rejected:         true,
		rejectedRevision: 1,
		rejectedReason:   `providers[0].access.ref: secret "gone" is not stored for this tenant`,
	}
	server := settingsServer(t, func(cfg *Config) {
		cfg.ModelSettingsStore = settings
		cfg.ModelSettingsRuntime = runtime
		cfg.Models = stubCatalog{defaultSelection: modelcfg.Selection{ProviderID: "s2api", ModelID: "deepseek-v4.1-flash"}}
	})
	client := server.Client()
	tenant := uuid.New()
	if _, err := settings.SetModelSettings(context.Background(), tenant, 0,
		json.RawMessage(`{"selection":{"providerId":"gone-gw","modelId":"x"}}`)); err != nil {
		t.Fatalf("plant override: %v", err)
	}

	var view struct {
		Source   string           `json:"source"`
		Revision int64            `json:"revision"`
		Issues   []map[string]any `json:"issues"`
	}
	if code := doSettingsJSON(t, client, http.MethodGet, server.URL+"/v1/settings/models", tenant, nil, &view); code != 200 {
		t.Fatalf("GET: %d", code)
	}
	if view.Source != "deployment" {
		t.Fatalf("source = %q, want deployment (the override is not in effect)", view.Source)
	}
	if view.Revision != 1 {
		t.Fatalf("revision = %d, want the stored row's revision 1", view.Revision)
	}
	named := false
	for _, issue := range view.Issues {
		if issue["field"] == "storedOverride" {
			named = true
			if reason, _ := issue["message"].(string); !strings.Contains(reason, "revision 1") {
				t.Fatalf("issue message does not name the revision: %q", reason)
			}
		}
	}
	if !named {
		t.Fatalf("issues do not name the rejected override: %+v", view.Issues)
	}
}

// TestSecretDeleteRefusesWhileReferenced pins the guard that keeps a delete
// from creating the boot problem in the first place: a secret a stored models
// document references is not deletable until that document stops referencing it.
func TestSecretDeleteRefusesWhileReferenced(t *testing.T) {
	settings := newFakeModelSettingsStore()
	secrets := newFakeSecretStore()
	server := settingsServer(t, func(cfg *Config) {
		cfg.ModelSettingsStore = settings
		cfg.ModelSettingsRuntime = &fakeSettingsRuntime{deployment: json.RawMessage(`{}`)}
		cfg.SecretStore = secrets
	})
	client := server.Client()
	tenant := uuid.New()
	if _, err := secrets.Put(context.Background(), tenant, "gw_key", []byte("sk-value")); err != nil {
		t.Fatalf("plant secret: %v", err)
	}
	if _, err := settings.SetModelSettings(context.Background(), tenant, 0, json.RawMessage(
		`{"providers":[{"id":"gw","access":{"type":"api-key","ref":"secret:gw_key"}}],"selection":{"providerId":"gw","modelId":"m"}}`)); err != nil {
		t.Fatalf("plant override: %v", err)
	}

	var out map[string]any
	if code := doSettingsJSON(t, client, http.MethodDelete, server.URL+"/v1/settings/secrets/gw_key", tenant, nil, &out); code != http.StatusConflict {
		t.Fatalf("DELETE referenced secret: %d (%+v), want 409", code, out)
	}
	holds := false
	if list, err := secrets.List(context.Background(), tenant); err == nil {
		for _, meta := range list {
			if meta.Name == "gw_key" {
				holds = true
			}
		}
	}
	if !holds {
		t.Fatal("the referenced secret was deleted anyway")
	}

	// Once the stored document stops referencing it, the delete goes through.
	if _, err := settings.SetModelSettings(context.Background(), tenant, 0, json.RawMessage(
		`{"providers":[{"id":"gw","access":{"type":"api-key","env":"S2API_KEY"}}],"selection":{"providerId":"gw","modelId":"m"}}`)); err != nil {
		t.Fatalf("rewrite override: %v", err)
	}
	if code := doSettingsJSON(t, client, http.MethodDelete, server.URL+"/v1/settings/secrets/gw_key", tenant, nil, &out); code != 200 {
		t.Fatalf("DELETE unreferenced secret: %d (%+v), want 200", code, out)
	}
}
