package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/pluginpkg"
	"github.com/jixoai/keel/internal/store"
)

// fakePluginManager is the API-side double: it records the calls and answers
// canned views/errors, so the HTTP contract (status codes, bodies, audit
// attribution) is tested without a plugins root.
type fakePluginManager struct {
	mu sync.Mutex

	inventory agentext.PluginInventory
	audit     []store.PluginAuditEntry

	installErr error
	enableErr  error
	disableErr error
	removeErr  error
	updateErr  error

	calls   []string
	audits  []agentext.AuditContext
	reqs    []agentext.InstallRequest
	updates []agentext.UpdateRequest
}

func (f *fakePluginManager) record(call string, audit agentext.AuditContext) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	f.audits = append(f.audits, audit)
}

func (f *fakePluginManager) Snapshot() agentext.PluginInventory {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inventory
}

func (f *fakePluginManager) Refresh(context.Context) error { return nil }

func (f *fakePluginManager) Install(_ context.Context, req agentext.InstallRequest, audit agentext.AuditContext) (agentext.PluginView, error) {
	f.record("install:"+req.Path, audit)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.installErr != nil {
		return agentext.PluginView{}, f.installErr
	}
	return agentext.PluginView{ID: "vendor.panel", Version: "1.0.0", State: agentext.LifecycleInstalled}, nil
}

func (f *fakePluginManager) Enable(_ context.Context, id string, audit agentext.AuditContext) (agentext.PluginView, error) {
	f.record("enable:"+id, audit)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.enableErr != nil {
		return agentext.PluginView{}, f.enableErr
	}
	return agentext.PluginView{ID: id, State: agentext.LifecycleEnabled}, nil
}

func (f *fakePluginManager) Disable(_ context.Context, id string, audit agentext.AuditContext) (agentext.PluginView, error) {
	f.record("disable:"+id, audit)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.disableErr != nil {
		return agentext.PluginView{}, f.disableErr
	}
	return agentext.PluginView{ID: id, State: agentext.LifecycleDisabled}, nil
}

func (f *fakePluginManager) Uninstall(_ context.Context, id string, audit agentext.AuditContext) (agentext.PluginView, error) {
	f.record("uninstall:"+id, audit)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return agentext.PluginView{}, f.removeErr
	}
	return agentext.PluginView{ID: id, State: agentext.LifecycleUninstalled}, nil
}

func (f *fakePluginManager) Update(_ context.Context, id string, req agentext.UpdateRequest, audit agentext.AuditContext) (agentext.PluginView, error) {
	f.record("update:"+id, audit)
	f.mu.Lock()
	f.updates = append(f.updates, req)
	defer f.mu.Unlock()
	if f.updateErr != nil {
		return agentext.PluginView{}, f.updateErr
	}
	return agentext.PluginView{ID: id, Version: "2.0.0", State: agentext.LifecycleEnabled}, nil
}

func (f *fakePluginManager) Audit(_ context.Context, tenantID *uuid.UUID, pluginID string, after int64, limit int) ([]store.PluginAuditEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.PluginAuditEntry, 0)
	for _, entry := range f.audit {
		if entry.AuditID <= after || (pluginID != "" && entry.PluginID != pluginID) {
			continue
		}
		out = append(out, entry)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakePluginManager) lastAudit(t *testing.T) agentext.AuditContext {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.audits) == 0 {
		t.Fatal("no audit context was recorded")
	}
	return f.audits[len(f.audits)-1]
}

// fakePluginAuditWriter records the audit rows the API writes for lifecycle
// writes that never reached the manager (approval refusals).
type fakePluginAuditWriter struct {
	mu      sync.Mutex
	entries []store.PluginAuditEntry
	err     error
}

func (f *fakePluginAuditWriter) RecordPluginAudit(_ context.Context, entry store.PluginAuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.entries = append(f.entries, entry)
	return nil
}

func (f *fakePluginAuditWriter) list() []store.PluginAuditEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.PluginAuditEntry(nil), f.entries...)
}

// pluginAPITest is the plugin-surface harness: the server, its fake manager and
// the pieces the durable approval flow needs (the real decision broker over an
// in-memory store, plus the audit writer).
type pluginAPITest struct {
	server  *httptest.Server
	manager *fakePluginManager
	audit   *fakePluginAuditWriter
	broker  *agentext.DecisionBroker
}

func newPluginAPITest(t *testing.T, manager PluginManager, tune ...func(*Config)) *pluginAPITest {
	t.Helper()
	broker := agentext.NewDecisionBroker(agentext.DecisionBrokerOptions{Logger: testLogger()})
	t.Cleanup(broker.Close)
	audit := &fakePluginAuditWriter{}
	cfg := Config{
		Plugins:           agentext.NewRuntime(agentext.RuntimeOptions{Logger: testLogger()}),
		PluginManager:     manager,
		PluginAuditWriter: audit,
		Decisions:         broker,
		Logger:            testLogger(),
	}
	for _, fn := range tune {
		fn(&cfg)
	}
	server := httptest.NewServer(New(cfg))
	t.Cleanup(server.Close)
	harness := &pluginAPITest{server: server, audit: audit, broker: broker}
	if fake, ok := manager.(*fakePluginManager); ok {
		harness.manager = fake
	}
	return harness
}

// newPluginAPIServer keeps the older call shape for tests that only need a
// server.
func newPluginAPIServer(t *testing.T, manager PluginManager) *httptest.Server {
	t.Helper()
	return newPluginAPITest(t, manager).server
}

// write performs one lifecycle write exactly as a client would, without any
// approval evidence. path is relative to the test server.
func (h *pluginAPITest) write(t *testing.T, method, path string, tenant uuid.UUID, body any, out any) int {
	t.Helper()
	return doJSON(t, h.server.Client(), method, h.server.URL+path, tenant, body, out)
}

// approveWrite runs the whole two-phase protocol for one lifecycle write: ask
// (202 with the question), answer it, and repeat the write with the answered
// decision. It returns the final request's status and body.
func (h *pluginAPITest) approveWrite(t *testing.T, method, path string, tenant uuid.UUID, body map[string]any, option string) (int, map[string]any) {
	t.Helper()
	return h.approveWriteAs(t, method, path, tenant, body, option, nil)
}

// approveWriteAs is approveWrite with an observer that sees the answered
// decision (used to exercise the audit row of a refusal).
func (h *pluginAPITest) approveWriteAs(t *testing.T, method, path string, tenant uuid.UUID, body map[string]any, option string, answered func(agentext.Decision)) (int, map[string]any) {
	t.Helper()
	var asked struct {
		ApprovalRequired bool              `json:"approvalRequired"`
		Decision         agentext.Decision `json:"decision"`
		AnswerURL        string            `json:"answerUrl"`
	}
	if code := h.write(t, method, path, tenant, body, &asked); code != http.StatusAccepted {
		t.Fatalf("%s %s without a decision = %d, want 202 (%+v)", method, path, code, asked)
	}
	if !asked.ApprovalRequired || asked.Decision.DecisionID == "" || asked.AnswerURL == "" {
		t.Fatalf("the 202 must carry the question: %+v", asked)
	}
	var answer struct {
		Decision agentext.Decision `json:"decision"`
	}
	if code := doJSON(t, h.server.Client(), http.MethodPost,
		h.server.URL+"/v1/plugin-approvals/"+asked.Decision.DecisionID, tenant,
		map[string]any{"optionId": option}, &answer); code != http.StatusOK {
		t.Fatalf("answer %s = %d", option, code)
	}
	if answered != nil {
		answered(answer.Decision)
	}
	withDecision := map[string]any{"decisionId": asked.Decision.DecisionID}
	for key, value := range body {
		withDecision[key] = value
	}
	var out map[string]any
	code := h.write(t, method, path, tenant, withDecision, &out)
	return code, out
}

func TestPluginAPILifecycleRoutesAndAuditAttribution(t *testing.T) {
	manager := &fakePluginManager{inventory: agentext.PluginInventory{
		Root: "/var/keel/plugins", Revision: 3, Generation: 7,
		Plugins: []agentext.PluginView{{ID: "keel.example", Version: "1.0.0", Kind: "in-process", State: agentext.LifecycleEnabled, Bundled: true,
			Source: pluginpkg.Source{Type: pluginpkg.SourceBuiltin}, Provides: pluginpkg.Provides{Tools: []string{"example_status"}}}},
		Tools:   []string{"example_status"},
		Enabled: []string{"keel.example"},
	}}
	server := newPluginAPITest(t, manager)
	tenant := uuid.New()

	// GET /v1/plugins: the managed inventory, not the boot snapshot.
	var inventory struct {
		Revision   int64                 `json:"revision"`
		Generation int64                 `json:"generation"`
		Lifecycle  string                `json:"lifecycle"`
		Plugins    []agentext.PluginView `json:"plugins"`
		Enabled    []string              `json:"enabled"`
		Tools      []string              `json:"tools"`
	}
	code := doJSON(t, server.server.Client(), http.MethodGet, server.server.URL+"/v1/plugins", tenant, nil, &inventory)
	if code != 200 || inventory.Revision != 3 || inventory.Lifecycle != "dynamic" || len(inventory.Plugins) != 1 {
		t.Fatalf("GET /v1/plugins = %d %+v", code, inventory)
	}
	if inventory.Plugins[0].Source.Type != pluginpkg.SourceBuiltin || !inventory.Plugins[0].Bundled {
		t.Fatalf("plugin row = %+v", inventory.Plugins[0])
	}

	// Install: the approval protocol runs first, then the action; asking does not
	// touch the manager (a separate test asserts that).
	if code, body := server.approveWrite(t, http.MethodPost, "/v1/plugins/install", tenant,
		map[string]any{"path": "/tmp/task-site/package"}, pluginApprovalAllow); code != http.StatusCreated {
		t.Fatalf("install = %d %+v", code, body)
	}
	// Enable, with the actor header: the audit attribution travels on the manager
	// call behind the approval.
	var asked struct {
		Decision agentext.Decision `json:"decision"`
	}
	if code := doJSON(t, server.server.Client(), http.MethodPost, server.server.URL+"/v1/plugins/vendor.panel/enable", tenant, nil, &asked); code != http.StatusAccepted {
		t.Fatalf("enable without a decision = %d", code)
	}
	req, err := http.NewRequest(http.MethodPost, server.server.URL+"/v1/plugins/vendor.panel/enable",
		strings.NewReader(`{"decisionId":"`+asked.Decision.DecisionID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", tenant.String())
	req.Header.Set("X-Keel-Actor", "owner@example.com")
	req.Header.Set("Content-Type", "application/json")
	if code := doJSON(t, server.server.Client(), http.MethodPost,
		server.server.URL+"/v1/plugin-approvals/"+asked.Decision.DecisionID, tenant, map[string]any{"optionId": pluginApprovalAllow}, nil); code != 200 {
		t.Fatalf("approve = %d", code)
	}
	res, err := server.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("enable = %d", res.StatusCode)
	}
	audit := manager.lastAudit(t)
	if audit.Actor != "owner@example.com" || audit.TenantID == nil || *audit.TenantID != tenant {
		t.Fatalf("audit context = %+v", audit)
	}
	for _, call := range []string{"install:/tmp/task-site/package", "enable:vendor.panel"} {
		if !containsCall(manager, call) {
			t.Fatalf("calls = %+v", manager.calls)
		}
	}
	// The states/requests travel through unchanged.
	if code := doJSON(t, server.server.Client(), http.MethodPost, server.server.URL+"/v1/plugins/vendor.panel/disable", tenant, nil, nil); code != 200 {
		t.Fatalf("disable = %d", code)
	}
	if code, body := server.approveWrite(t, http.MethodPost, "/v1/plugins/vendor.panel/update", tenant,
		map[string]any{"path": "/tmp/task-site/package-2"}, pluginApprovalAllow); code != 200 {
		t.Fatalf("update = %d %+v", code, body)
	}
	if code := doJSON(t, server.server.Client(), http.MethodPost, server.server.URL+"/v1/plugins/vendor.panel/uninstall", tenant, nil, nil); code != 200 {
		t.Fatalf("uninstall = %d", code)
	}
	manager.mu.Lock()
	if len(manager.updates) != 1 || manager.updates[0].Path != "/tmp/task-site/package-2" {
		t.Fatalf("update requests = %+v", manager.updates)
	}
	manager.mu.Unlock()
}

func TestPluginAPIErrorMapping(t *testing.T) {
	manager := &fakePluginManager{}
	harness := newPluginAPITest(t, manager)
	tenant := uuid.New()
	cases := []struct {
		name       string
		set        func()
		method     string
		path       string
		body       any
		wantStatus int
		wantField  string
		// approved marks the cases that must complete the durable approval first:
		// the manager failure is what the status maps, and it can only be reached
		// through an allowed write.
		approved bool
	}{
		{name: "not found", set: func() {
			manager.enableErr = pluginError("enable", "x.y", agentext.ErrPluginNotFound)
		},
			method: http.MethodPost, path: "/v1/plugins/x.y/enable", wantStatus: 404, approved: true},
		{name: "already enabled", set: func() {
			manager.enableErr = pluginError("enable", "x.y", agentext.ErrPluginState)
		},
			method: http.MethodPost, path: "/v1/plugins/x.y/enable", wantStatus: 409, approved: true},
		{name: "missing dependency", set: func() {
			failure := pluginError("enable", "x.y", agentext.ErrPluginDependency)
			failure.Missing = []string{"keel.fs"}
			manager.enableErr = failure
		}, method: http.MethodPost, path: "/v1/plugins/x.y/enable", wantStatus: 409, approved: true},
		{name: "dependents", set: func() {
			failure := pluginError("disable", "x.y", agentext.ErrPluginDependency)
			failure.Dependents = []string{"x.z"}
			manager.disableErr = failure
		}, method: http.MethodPost, path: "/v1/plugins/x.y/disable", wantStatus: 409},
		{name: "version gate", set: func() {
			failure := pluginError("install", "x.y", agentext.ErrPluginGate)
			failure.Fields = []pluginpkg.FieldError{{Field: "engines.keel", Message: "no"}}
			manager.installErr = failure
		}, method: http.MethodPost, path: "/v1/plugins/install", body: map[string]any{"path": "/tmp/x"}, wantStatus: 409, wantField: "engines.keel", approved: true},
		{name: "invalid package on install", set: func() {
			failure := pluginError("install", "", agentext.ErrPluginInvalid)
			failure.Fields = []pluginpkg.FieldError{{Field: "id", Message: "id is required"}}
			manager.installErr = failure
		}, method: http.MethodPost, path: "/v1/plugins/install", body: map[string]any{"path": "/tmp/x"}, wantStatus: 400, wantField: "id", approved: true},
		{name: "invalid package on enable", set: func() {
			manager.enableErr = pluginError("enable", "x.y", agentext.ErrPluginInvalid)
		}, method: http.MethodPost, path: "/v1/plugins/x.y/enable", wantStatus: 409, approved: true},
		{name: "kind without loader", set: func() {
			manager.enableErr = pluginError("enable", "x.y", agentext.ErrPluginKind)
		}, method: http.MethodPost, path: "/v1/plugins/x.y/enable", wantStatus: 409, approved: true},
		{name: "bundled", set: func() {
			manager.removeErr = pluginError("uninstall", "keel.example", agentext.ErrPluginBundled)
		}, method: http.MethodPost, path: "/v1/plugins/keel.example/uninstall", wantStatus: 409},
		{name: "path outside", set: func() {
			failure := pluginError("install", "", agentext.ErrPluginPath)
			failure.Fields = []pluginpkg.FieldError{{Field: "path", Message: "path is required"}}
			manager.installErr = failure
		}, method: http.MethodPost, path: "/v1/plugins/install", body: map[string]any{"path": ""}, wantStatus: 400, wantField: "path"},
		{name: "malformed id", set: func() {}, method: http.MethodPost, path: "/v1/plugins/NOT-AN-ID/enable", wantStatus: 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager.enableErr, manager.disableErr, manager.installErr, manager.removeErr = nil, nil, nil, nil
			tc.set()
			var body map[string]any
			var status int
			if tc.approved {
				fields, _ := tc.body.(map[string]any)
				status, body = harness.approveWrite(t, tc.method, tc.path, tenant, fields, pluginApprovalAllow)
			} else {
				status = harness.write(t, tc.method, tc.path, tenant, tc.body, &body)
			}
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%v)", status, tc.wantStatus, body)
			}
			if body["error"] == nil || body["error"] == "" {
				t.Fatalf("body = %v", body)
			}
			if tc.wantField != "" {
				fields, ok := body["fields"].([]any)
				if !ok || len(fields) == 0 {
					t.Fatalf("fields = %v", body["fields"])
				}
				first, _ := fields[0].(map[string]any)
				if first["field"] != tc.wantField {
					t.Fatalf("field = %v, want %s", first["field"], tc.wantField)
				}
			}
			if tc.name == "missing dependency" {
				missing, ok := body["missing"].([]any)
				if !ok || len(missing) != 1 || missing[0] != "keel.fs" {
					t.Fatalf("missing = %v", body["missing"])
				}
			}
			if tc.name == "dependents" {
				dependents, ok := body["dependents"].([]any)
				if !ok || len(dependents) != 1 {
					t.Fatalf("dependents = %v", body["dependents"])
				}
			}
		})
	}
}

func TestPluginAPIRequiresTenantAndManager(t *testing.T) {
	// No manager: the read route falls back to the assembled snapshot and the
	// write routes answer 503.
	server := httptest.NewServer(New(Config{
		Plugins: agentext.NewRuntime(agentext.RuntimeOptions{Logger: testLogger()}),
		Logger:  testLogger(),
	}))
	defer server.Close()
	tenant := uuid.New()
	var body struct {
		Plugins   []agentext.PluginStatus `json:"plugins"`
		Lifecycle string                  `json:"lifecycle"`
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/plugins", tenant, nil, &body); code != 200 {
		t.Fatalf("snapshot fallback = %d", code)
	}
	if body.Lifecycle != "" || body.Plugins == nil {
		t.Fatalf("snapshot fallback body = %+v", body)
	}
	if code := doJSON(t, server.Client(), http.MethodPost, server.URL+"/v1/plugins/x.y/enable", tenant, nil, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("enable without a manager = %d", code)
	}
	// Every plugin route needs a tenant (the auth middleware's header).
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/plugins", uuid.Nil, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("missing tenant = %d", code)
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/plugins/audit", uuid.Nil, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("audit without tenant = %d", code)
	}
}

func TestPluginAPIAuditPaginationAndCapabilities(t *testing.T) {
	manager := &fakePluginManager{audit: []store.PluginAuditEntry{
		{AuditID: 1, TS: time.Unix(100, 0).UTC(), PluginID: "vendor.panel", Version: "1.0.0", Action: "install", Result: "ok", Actor: "api"},
		{AuditID: 2, TS: time.Unix(101, 0).UTC(), PluginID: "vendor.panel", Version: "1.0.0", Action: "enable", Result: "ok", Actor: "api"},
		{AuditID: 3, TS: time.Unix(102, 0).UTC(), PluginID: "other.plugin", Version: "2.0.0", Action: "disable", Result: "ok", Actor: "api"},
	}}
	manager.inventory = agentext.PluginInventory{Revision: 9, Enabled: []string{"vendor.panel"}}
	server := newPluginAPIServer(t, manager)
	tenant := uuid.New()

	var page struct {
		Entries []store.PluginAuditEntry `json:"entries"`
		Next    int64                    `json:"next"`
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/plugins/audit?limit=2", tenant, nil, &page); code != 200 {
		t.Fatalf("audit = %d", code)
	}
	if len(page.Entries) != 2 || page.Next != 2 || page.Entries[0].Action != "install" {
		t.Fatalf("audit page = %+v", page)
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/plugins/audit?after=2&limit=10", tenant, nil, &page); code != 200 {
		t.Fatalf("audit after = %d", code)
	}
	if len(page.Entries) != 1 || page.Entries[0].AuditID != 3 {
		t.Fatalf("audit after page = %+v", page)
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/plugins/audit?plugin=vendor.panel", tenant, nil, &page); code != 200 || len(page.Entries) != 2 {
		t.Fatalf("audit filter = %d %+v", code, page)
	}
	for _, bad := range []string{"?after=abc", "?after=-1", "?limit=0", "?limit=1000", "?plugin=NOT-AN-ID"} {
		if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/plugins/audit"+bad, tenant, nil, nil); code != http.StatusBadRequest {
			t.Fatalf("audit%s = %d, want 400", bad, code)
		}
	}

	// capabilities reflects the managed inventory: enabled ids + revision.
	var caps Capabilities
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/capabilities", tenant, nil, &caps); code != 200 {
		t.Fatalf("capabilities = %d", code)
	}
	if !caps.Features["plugins.manage"] || !caps.Features["plugins.audit"] {
		t.Fatalf("features = %+v", caps.Features)
	}
	if caps.PluginRevision != 9 || len(caps.Plugins) != 1 || caps.Plugins[0] != "vendor.panel" {
		t.Fatalf("plugins = %+v revision %d", caps.Plugins, caps.PluginRevision)
	}
}

func TestPluginAPIInstallRejectsUnknownFields(t *testing.T) {
	manager := &fakePluginManager{}
	server := newPluginAPIServer(t, manager)
	tenant := uuid.New()
	raw := strings.NewReader(`{"path":"/tmp/x","source":"git"}`)
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/plugins/install", raw)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", tenant.String())
	req.Header.Set("Content-Type", "application/json")
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body["error"].(string), "source") {
		t.Fatalf("error = %v", body["error"])
	}
}

// pluginError builds a manager failure the API maps to a status; the detail
// fields (fields/missing/dependents) are set by the caller on the returned
// value.
func pluginError(op, id string, cause error) *agentext.PluginError {
	return agentext.NewPluginError(op, id, cause, "refused")
}

func containsCall(manager *fakePluginManager, call string) bool {
	return manager.count(call) > 0
}

// count reports how many times a call was recorded.
func (f *fakePluginManager) count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := 0
	for _, recorded := range f.calls {
		if recorded == call {
			seen++
		}
	}
	return seen
}

// TestPluginAPIPluginErrorsAreNotWrapped guards the errors.As path: a manager
// failure that is not a *PluginError must still map to a 4xx/5xx without
// panicking.
func TestPluginAPIPluginErrorsAreNotWrapped(t *testing.T) {
	manager := &fakePluginManager{enableErr: errors.New("boom")}
	harness := newPluginAPITest(t, manager)
	if code, body := harness.approveWrite(t, http.MethodPost, "/v1/plugins/x.y/enable", uuid.New(), nil, pluginApprovalAllow); code != http.StatusInternalServerError {
		t.Fatalf("plain error = %d %+v", code, body)
	}
}
