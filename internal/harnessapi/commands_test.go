package harnessapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/pluginpkg"
	"github.com/jixoai/keel/internal/skilllib"
	"github.com/jixoai/keel/internal/skills"
	"github.com/jixoai/keel/internal/store"
)

// --- doubles -------------------------------------------------------------

// memoryCommandStore is the in-memory CommandInvocationStore + event appender.
type memoryCommandStore struct {
	mu     sync.Mutex
	rows   map[string]store.CommandInvocation
	events []memoryCommandEvent
}

type memoryCommandEvent struct {
	kind    string
	payload any
	dedupe  string
	seq     int64
}

func newMemoryCommandStore() *memoryCommandStore {
	return &memoryCommandStore{rows: map[string]store.CommandInvocation{}}
}

func commandRowKey(tenantID, sessionID uuid.UUID, requestID string) string {
	return tenantID.String() + "|" + sessionID.String() + "|" + requestID
}

func (m *memoryCommandStore) ClaimCommandInvocation(_ context.Context, row store.CommandInvocation, lease time.Duration) (store.CommandInvocation, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := commandRowKey(row.TenantID, row.SessionID, row.RequestID)
	now := time.Now().UTC()
	if existing, ok := m.rows[key]; ok {
		if existing.Status == store.CommandStatusRunning &&
			(existing.LeaseUntil == nil || !existing.LeaseUntil.After(now)) {
			expires := now.Add(lease)
			existing.LeaseUntil = &expires
			existing.UpdatedAt = now
			m.rows[key] = existing
			return existing, true, nil
		}
		return existing, false, nil
	}
	expires := now.Add(lease)
	row.CreatedAt = now
	row.UpdatedAt = now
	row.LeaseUntil = &expires
	m.rows[key] = row
	return row, true, nil
}

func (m *memoryCommandStore) ClaimDecisionResult(_ context.Context, tenantID, sessionID uuid.UUID, requestID string, lease time.Duration) (store.CommandInvocation, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := commandRowKey(tenantID, sessionID, requestID)
	row, ok := m.rows[key]
	if !ok {
		return store.CommandInvocation{}, false, nil
	}
	if row.Status != store.CommandStatusPendingDecision {
		return row, false, nil
	}
	now := time.Now().UTC()
	expires := now.Add(lease)
	row.Status = store.CommandStatusRunning
	row.LeaseUntil = &expires
	row.UpdatedAt = now
	m.rows[key] = row
	return row, true, nil
}

func (m *memoryCommandStore) RenewCommandInvocation(_ context.Context, tenantID, sessionID uuid.UUID, requestID string, lease time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := commandRowKey(tenantID, sessionID, requestID)
	row, ok := m.rows[key]
	if !ok || row.Status != store.CommandStatusRunning {
		return false, nil
	}
	now := time.Now().UTC()
	expires := now.Add(lease)
	row.LeaseUntil = &expires
	row.UpdatedAt = now
	m.rows[key] = row
	return true, nil
}

func (m *memoryCommandStore) TransitionCommandInvocation(_ context.Context, row store.CommandInvocation, from string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := commandRowKey(row.TenantID, row.SessionID, row.RequestID)
	existing, ok := m.rows[key]
	if !ok || existing.Status != from {
		return false, nil
	}
	row.UpdatedAt = time.Now().UTC()
	if row.Terminal() {
		row.LeaseUntil = nil
	}
	m.rows[key] = row
	return true, nil
}

func (m *memoryCommandStore) GetCommandInvocation(_ context.Context, tenantID, sessionID uuid.UUID, requestID string) (store.CommandInvocation, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[commandRowKey(tenantID, sessionID, requestID)]
	return row, ok, nil
}

func (m *memoryCommandStore) AppendEventDedup(_ context.Context, _, _ uuid.UUID, kind string, payload any, dedupeKey string) (int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if dedupeKey != "" {
		for _, event := range m.events {
			if event.dedupe == dedupeKey {
				return event.seq, false, nil
			}
		}
	}
	seq := int64(len(m.events) + 1)
	m.events = append(m.events, memoryCommandEvent{kind: kind, payload: payload, dedupe: dedupeKey, seq: seq})
	return seq, true, nil
}

func (m *memoryCommandStore) eventKinds() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.events))
	for _, event := range m.events {
		out = append(out, event.kind)
	}
	return out
}

func (m *memoryCommandStore) eventsOfKind(kind string) []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]map[string]any, 0)
	for _, event := range m.events {
		if event.kind != kind {
			continue
		}
		raw, err := json.Marshal(event.payload)
		if err != nil {
			continue
		}
		var document map[string]any
		if err := json.Unmarshal(raw, &document); err != nil {
			continue
		}
		out = append(out, document)
	}
	return out
}

// commandAccess is an access double that holds every command key at tenant
// scope, so availability reflects explicit test mutations.
func commandAccess() *fakeAccess {
	return &fakeAccess{grants: access.Grants{
		access.KeySessionRead:  access.ScopeTenant,
		access.KeySessionWrite: access.ScopeTenant,
		access.KeyModelInvoke:  access.ScopeTenant,
		access.KeyPluginInvoke: access.ScopeTenant,
		access.KeySkillRead:    access.ScopeTenant,
	}}
}

type fakeCommandPlugins struct {
	registry harnessext.CommandRegistry
	tenantID uuid.UUID
}

func (f *fakeCommandPlugins) ListCommands() harnessext.CommandRegistry { return f.registry }

func (f *fakeCommandPlugins) TenantID() uuid.UUID { return f.tenantID }

type fakeCommandTools struct {
	mu     sync.Mutex
	calls  []string
	args   []json.RawMessage
	result json.RawMessage
	err    error
	delay  time.Duration
}

func (f *fakeCommandTools) InvokePluginTool(_ context.Context, pluginID, tool string, args json.RawMessage) (json.RawMessage, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, pluginID+"/"+tool)
	f.args = append(f.args, args)
	if f.err != nil {
		return nil, f.err
	}
	if f.result == nil {
		return json.RawMessage(`{"ok":true}`), nil
	}
	return f.result, nil
}

func (f *fakeCommandTools) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeCommandSkills struct {
	mu      sync.Mutex
	details map[string]skilllib.Detail
	err     error
}

func (f *fakeCommandSkills) List(uuid.UUID, bool) (skilllib.Snapshot, error) {
	if f.err != nil {
		return skilllib.Snapshot{}, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.details))
	for name := range f.details {
		names = append(names, name)
	}
	records := make([]skilllib.Record, 0, len(names))
	for _, name := range names {
		records = append(records, skilllib.Record{Name: name, Description: "skill " + name})
	}
	return skilllib.Snapshot{Skills: records}, nil
}

func (f *fakeCommandSkills) Show(_ uuid.UUID, name string) (skilllib.Detail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	detail, ok := f.details[name]
	if !ok {
		return skilllib.Detail{}, &skilllib.Error{Code: skilllib.CodeNotFound, Message: "no skill named " + name}
	}
	return detail, nil
}

type fakeCompact struct {
	mu     sync.Mutex
	calls  []string
	result CompactResult
	err    error
	delay  time.Duration
}

func (f *fakeCompact) compact(_ context.Context, _, _ uuid.UUID, requestID string) (CompactResult, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, requestID)
	if f.err != nil {
		return CompactResult{}, f.err
	}
	return f.result, nil
}

func (f *fakeCompact) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// --- harness -------------------------------------------------------------

type commandHarness struct {
	server    *httptest.Server
	store     *memoryCommandStore
	tools     *fakeCommandTools
	plugins   *fakeCommandPlugins
	skills    *fakeCommandSkills
	compact   *fakeCompact
	broker    *harnessext.DecisionBroker
	tenant    uuid.UUID
	session   uuid.UUID
	idle      func() bool
	access    *fakeAccess
	sessionOK func(uuid.UUID, uuid.UUID) bool
}

func newCommandHarness(t *testing.T, mutate func(*Config)) *commandHarness {
	t.Helper()
	plugins := &fakeCommandPlugins{registry: harnessext.CommandRegistry{Generation: 7}}
	tools := &fakeCommandTools{result: json.RawMessage(`{"indexed":42}`)}
	skills := &fakeCommandSkills{details: map[string]skilllib.Detail{
		"alpha": {Record: skilllib.Record{Name: "alpha", Description: "Alpha skill", CurrentHash: "sha256:alpha"}, Body: "Alpha body."},
		"beta":  {Record: skilllib.Record{Name: "beta", Description: "Beta skill", CurrentHash: "sha256:beta"}, Body: "Beta body."},
	}}
	compact := &fakeCompact{result: CompactResult{CheckpointID: 9, Performed: true, TokensBefore: 100, TokensAfter: 10}}
	broker := harnessext.NewDecisionBroker(harnessext.DecisionBrokerOptions{Logger: testLogger()})
	t.Cleanup(broker.Close)
	commandStore := newMemoryCommandStore()
	harness := &commandHarness{
		store: commandStore, tools: tools, plugins: plugins, skills: skills,
		compact: compact, broker: broker, tenant: uuid.New(), session: uuid.New(),
		access: commandAccess(), idle: func() bool { return true },
		sessionOK: func(uuid.UUID, uuid.UUID) bool { return true },
	}
	cfg := Config{
		Access: harness.access,
		Logger: testLogger(),
		SessionExists: func(_ context.Context, tenantID, sessionID uuid.UUID) (bool, error) {
			return harness.sessionOK(tenantID, sessionID), nil
		},
		SessionIdle:    func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return harness.idle(), nil },
		CompactSession: compact.compact,
		Decisions:      broker,
		CommandStore:   commandStore,
		CommandPlugins: plugins,
		CommandTools:   tools,
		CommandSkills:  skills,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	plugins.tenantID = harness.tenant
	harness.server = httptest.NewServer(New(cfg))
	t.Cleanup(harness.server.Close)
	return harness
}

func (h *commandHarness) addPluginCommands(commands ...harnessext.PluginCommand) {
	h.plugins.registry.Commands = commands
}

func pluginCommandFixture(pluginID, name string, danger pluginpkg.Danger, available bool, reason string, targets ...pluginpkg.SlotTarget) harnessext.PluginCommand {
	if len(targets) == 0 {
		targets = pluginpkg.DefaultCommandTargets()
	}
	return harnessext.PluginCommand{
		PluginID:      pluginID,
		Name:          name,
		Published:     pluginID + ":" + name,
		Tool:          name + "_tool",
		DeclaredTool:  name + "_tool",
		PublishedTool: strings.ReplaceAll(pluginID, ".", "_") + "_" + name + "_tool",
		Decl: pluginpkg.CommandDecl{
			Name: name, Summary: "Run " + name + ".", Tool: name + "_tool",
			Danger: danger, Targets: targets,
			ArgsSchema: map[string]any{"type": "object", "properties": map[string]any{
				"secret": map[string]any{"type": "string"},
			}},
		},
		State:     harnessext.LifecycleEnabled,
		Available: available,
		Reason:    reason,
	}
}

func (h *commandHarness) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var out map[string]any
	code := doJSON(t, h.server.Client(), method, h.server.URL+path, h.tenant, body, &out)
	return code, out
}

// postAsync is the goroutine-safe form: it never calls testing.T methods.
func (h *commandHarness) postAsync(path string, body any) (int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequest(http.MethodPost, h.server.URL+path, strings.NewReader(string(raw)))
	if err != nil {
		return 0, err
	}
	request.Header.Set("X-Tenant-ID", h.tenant.String())
	request.Header.Set("Content-Type", "application/json")
	response, err := h.server.Client().Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	return response.StatusCode, nil
}

func (h *commandHarness) commandPath(name string) string {
	return "/v1/sessions/" + h.session.String() + "/commands/" + name
}

func (h *commandHarness) catalog(t *testing.T, query string) (int, CommandCatalog) {
	t.Helper()
	var out CommandCatalog
	code := doJSON(t, h.server.Client(), http.MethodGet,
		h.server.URL+"/v1/sessions/"+h.session.String()+"/commands"+query, h.tenant, nil, &out)
	return code, out
}

// --- tests ---------------------------------------------------------------

func TestCommandCatalogOrderAndAvailability(t *testing.T) {
	harness := newCommandHarness(t, nil)
	harness.addPluginCommands(
		pluginCommandFixture("vendor.one", "refresh", pluginpkg.DangerWrite, true, "", pluginpkg.SlotTargetWeb),
		pluginCommandFixture("vendor.one", "pending", pluginpkg.DangerRead, false, harnessext.CommandReasonPluginPending),
		pluginCommandFixture("vendor.two", "purge", pluginpkg.DangerDestructive, true, ""),
	)
	code, catalog := harness.catalog(t, "")
	if code != http.StatusOK {
		t.Fatalf("catalog status = %d", code)
	}
	names := make([]string, 0, len(catalog.Commands))
	for _, command := range catalog.Commands {
		names = append(names, command.Name)
	}
	want := []string{"compact", "vendor.one:refresh", "vendor.one:pending", "vendor.two:purge", "skill:alpha", "skill:beta"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("catalog order = %v, want %v", names, want)
	}
	if catalog.Generation != 7 || catalog.Truncated {
		t.Fatalf("catalog metadata = %+v", catalog)
	}
	for _, command := range catalog.Commands {
		if command.Name == "learn" || command.Name == "skill:learn" {
			t.Fatalf("learn must never be a command: %+v", command)
		}
	}
	pending := catalog.Commands[2]
	if pending.Available || pending.Reason != harnessext.CommandReasonPluginPending {
		t.Fatalf("pending plugin command = %+v", pending)
	}
	refresh := catalog.Commands[1]
	if refresh.Source != "plugin" || !refresh.Available || refresh.Danger != "write" ||
		refresh.ArgsSchema == nil || refresh.RequiredPermissions[1] != access.KeyPluginInvoke {
		t.Fatalf("refresh command = %+v", refresh)
	}
	skill := catalog.Commands[4]
	if skill.Source != "skill" || skill.RequiredPermissions[1] != access.KeySkillRead || !skill.Available {
		t.Fatalf("skill command = %+v", skill)
	}

	// A TUI client sees the web-only command as unavailable with the reason,
	// never hidden.
	code, catalog = harness.catalog(t, "?target=tui")
	if code != http.StatusOK {
		t.Fatalf("target catalog status = %d", code)
	}
	if got := catalog.Commands[1]; got.Available || got.Reason != CommandReasonTargetUnsupported {
		t.Fatalf("web-only command on tui = %+v", got)
	}
	if code, _ := harness.catalog(t, "?target=android"); code != http.StatusBadRequest {
		t.Fatalf("invalid target status = %d, want 400", code)
	}
}

func TestCommandCatalogMarksMissingPermissions(t *testing.T) {
	harness := newCommandHarness(t, func(cfg *Config) {
		cfg.Access = &fakeAccess{
			grants: access.Grants{
				access.KeySessionRead:  access.ScopeTenant,
				access.KeySessionWrite: access.ScopeTenant,
				access.KeyPluginInvoke: access.ScopeTenant,
				access.KeySkillRead:    access.ScopeTenant,
			},
			decisions: map[string]access.Decision{
				access.KeyModelInvoke: {Allow: false, Key: access.KeyModelInvoke, Reason: access.ReasonMissingGrant},
			},
		}
	})
	harness.addPluginCommands(pluginCommandFixture("vendor.one", "refresh", pluginpkg.DangerWrite, true, ""))
	_, catalog := harness.catalog(t, "")
	compact := catalog.Commands[0]
	if compact.Available || compact.Reason != CommandReasonMissingPermissions ||
		len(compact.MissingPermissions) != 1 || compact.MissingPermissions[0] != access.KeyModelInvoke {
		t.Fatalf("compact without model.invoke = %+v", compact)
	}
}

func TestCommandCatalogCrossTenantAndPermission(t *testing.T) {
	harness := newCommandHarness(t, nil)
	harness.sessionOK = func(uuid.UUID, uuid.UUID) bool { return false }
	if code, _ := harness.catalog(t, ""); code != http.StatusNotFound {
		t.Fatalf("foreign session catalog = %d, want 404", code)
	}
	harness.sessionOK = func(uuid.UUID, uuid.UUID) bool { return true }
	harness.access.decisions = map[string]access.Decision{
		access.KeySessionRead: {Allow: false, Key: access.KeySessionRead, Reason: access.ReasonMissingGrant},
	}
	if code, _ := harness.catalog(t, ""); code != http.StatusForbidden {
		t.Fatalf("catalog without session.read = %d, want 403", code)
	}
}

func TestCommandExecuteCompactDelegatesAndReplays(t *testing.T) {
	harness := newCommandHarness(t, nil)
	code, body := harness.do(t, http.MethodPost, harness.commandPath("compact"), map[string]any{"requestId": "r1", "args": map[string]any{}})
	if code != http.StatusOK {
		t.Fatalf("compact status = %d (%v)", code, body)
	}
	if body["command"] != "compact" || body["status"] != "ok" || body["eventSeq"] == nil {
		t.Fatalf("compact result = %v", body)
	}
	if harness.compact.callCount() != 1 || harness.compact.calls[0] != "r1" {
		t.Fatalf("compact seam calls = %v", harness.compact.calls)
	}
	code, replay := harness.do(t, http.MethodPost, harness.commandPath("compact"), map[string]any{"requestId": "r1", "args": map[string]any{}})
	if code != http.StatusOK || replay["eventSeq"] != body["eventSeq"] {
		t.Fatalf("replay = %d %v", code, replay)
	}
	if harness.compact.callCount() != 1 {
		t.Fatalf("replay re-executed the seam: %d calls", harness.compact.callCount())
	}
	// A reused requestId with a different command is a conflict, not a replay.
	if code, _ := harness.do(t, http.MethodPost, harness.commandPath("skill:alpha"), map[string]any{"requestId": "r1"}); code != http.StatusConflict {
		t.Fatalf("requestId reuse across commands = %d, want 409", code)
	}
	events := harness.store.eventsOfKind(store.EventCommandInvoked)
	if len(events) != 1 {
		t.Fatalf("command.invoked events = %d", len(events))
	}
	if events[0]["command"] != "compact" || events[0]["argsHash"] == "" || events[0]["argsBytes"] == nil {
		t.Fatalf("audit payload = %v", events[0])
	}
	if _, hasArgs := events[0]["args"]; hasArgs {
		t.Fatalf("audit payload carries argument bodies: %v", events[0])
	}
}

func TestCommandExecuteRefusals(t *testing.T) {
	harness := newCommandHarness(t, nil)
	if code, _ := harness.do(t, http.MethodPost, harness.commandPath("learn"), map[string]any{"requestId": "r1"}); code != http.StatusNotFound {
		t.Fatalf("learn execution = %d, want 404", code)
	}
	if code, _ := harness.do(t, http.MethodPost, harness.commandPath("nope"), map[string]any{"requestId": "r1"}); code != http.StatusNotFound {
		t.Fatalf("unknown command = %d, want 404", code)
	}
	if code, _ := harness.do(t, http.MethodPost, harness.commandPath("compact"), map[string]any{}); code != http.StatusBadRequest {
		t.Fatalf("missing requestId = %d, want 400", code)
	}
	if code, _ := harness.do(t, http.MethodPost, harness.commandPath("compact"), map[string]any{
		"requestId": "r-args", "args": map[string]any{"unknown": 1},
	}); code != http.StatusBadRequest {
		t.Fatalf("unknown argument = %d, want 400", code)
	}
	harness.idle = func() bool { return false }
	code, body := harness.do(t, http.MethodPost, harness.commandPath("compact"), map[string]any{"requestId": "r-busy"})
	if code != http.StatusConflict || body["code"] != CommandCodeBusy {
		t.Fatalf("busy = %d %v", code, body)
	}
	if harness.compact.callCount() != 0 {
		t.Fatalf("busy executed the seam: %d", harness.compact.callCount())
	}
	events := harness.store.eventsOfKind(store.EventCommandInvoked)
	if len(events) != 1 || events[0]["status"] != store.CommandStatusRefused || events[0]["errorCode"] != CommandCodeBusy {
		t.Fatalf("busy audit = %v", events)
	}
}

func TestCommandExecuteFailureReplaysFirstFailure(t *testing.T) {
	harness := newCommandHarness(t, nil)
	harness.compact.err = ErrCompactBusy
	code, body := harness.do(t, http.MethodPost, harness.commandPath("compact"), map[string]any{"requestId": "rf"})
	if code != http.StatusConflict || body["code"] != CommandCodeBusy {
		t.Fatalf("failure = %d %v", code, body)
	}
	harness.compact.err = nil
	code, body = harness.do(t, http.MethodPost, harness.commandPath("compact"), map[string]any{"requestId": "rf"})
	if code != http.StatusConflict || body["code"] != CommandCodeBusy {
		t.Fatalf("replayed failure = %d %v", code, body)
	}
	if harness.compact.callCount() != 1 {
		t.Fatalf("failure replay re-executed: %d", harness.compact.callCount())
	}
}

func TestCommandExecutePluginToolAndAudit(t *testing.T) {
	harness := newCommandHarness(t, nil)
	harness.addPluginCommands(pluginCommandFixture("vendor.one", "refresh", pluginpkg.DangerWrite, true, ""))
	code, body := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:refresh"), map[string]any{
		"requestId": "rp",
		"args":      map[string]any{"secret": "do-not-audit"},
	})
	if code != http.StatusOK || body["data"] == nil {
		t.Fatalf("plugin command = %d %v", code, body)
	}
	if harness.tools.callCount() != 1 || harness.tools.calls[0] != "vendor.one/refresh_tool" {
		t.Fatalf("tool calls = %v", harness.tools.calls)
	}
	if string(harness.tools.args[0]) != `{"secret":"do-not-audit"}` {
		t.Fatalf("tool args = %s", harness.tools.args[0])
	}
	events := harness.store.eventsOfKind(store.EventCommandInvoked)
	if len(events) != 1 {
		t.Fatalf("events = %v", events)
	}
	raw, _ := json.Marshal(events[0])
	if strings.Contains(string(raw), "do-not-audit") {
		t.Fatalf("audit event leaked the argument body: %s", raw)
	}
	if events[0]["source"] != "plugin" || events[0]["argsHash"] == "" {
		t.Fatalf("audit event = %v", events[0])
	}
}

func TestCommandExecutePluginUnavailableAndToolErrors(t *testing.T) {
	harness := newCommandHarness(t, nil)
	harness.addPluginCommands(pluginCommandFixture("vendor.one", "broken", pluginpkg.DangerWrite, false, harnessext.CommandReasonPluginFailed))
	if code, body := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:broken"), map[string]any{"requestId": "r1"}); code != http.StatusServiceUnavailable {
		t.Fatalf("failed plugin command = %d %v", code, body)
	}
	harness.addPluginCommands(pluginCommandFixture("vendor.one", "refresh", pluginpkg.DangerWrite, true, ""))
	harness.tools.err = harnessext.ErrPermissionDenied
	if code, body := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:refresh"), map[string]any{"requestId": "r2"}); code != http.StatusForbidden || body["code"] != CommandCodePermissionDenied {
		t.Fatalf("plugin permission denial = %d %v", code, body)
	}
	harness.tools.err = harnessext.ErrPluginState
	if code, body := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:refresh"), map[string]any{"requestId": "r3"}); code != http.StatusServiceUnavailable {
		t.Fatalf("plugin state error = %d %v", code, body)
	}
	harness.tools.err = context.DeadlineExceeded
	if code, body := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:refresh"), map[string]any{"requestId": "r4"}); code != http.StatusBadGateway {
		t.Fatalf("tool error = %d %v", code, body)
	}
}

func TestCommandDestructiveDecisionFlow(t *testing.T) {
	harness := newCommandHarness(t, nil)
	harness.addPluginCommands(pluginCommandFixture("vendor.one", "purge", pluginpkg.DangerDestructive, true, ""))
	code, first := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:purge"), map[string]any{
		"requestId": "rd", "args": map[string]any{},
	})
	if code != http.StatusAccepted || first["status"] != store.CommandStatusPendingDecision || first["decisionId"] == nil {
		t.Fatalf("destructive open = %d %v", code, first)
	}
	decisionID := first["decisionId"].(string)
	if harness.tools.callCount() != 0 {
		t.Fatal("a destructive command ran before approval")
	}
	// Replaying the same requestId returns the same decision, no second question.
	code, again := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:purge"), map[string]any{"requestId": "rd"})
	if code != http.StatusAccepted || again["decisionId"] != decisionID {
		t.Fatalf("pending replay = %d %v", code, again)
	}
	decisions, _ := harness.broker.List(context.Background(), harness.tenant, harness.session)
	if len(decisions) != 1 {
		t.Fatalf("decisions = %d, want one", len(decisions))
	}
	if _, _, err := harness.broker.Answer(context.Background(), harnessext.AnswerDecisionRequest{
		TenantID: harness.tenant, SessionID: harness.session,
		DecisionID: uuid.MustParse(decisionID), OptionID: commandDecisionOptionOK,
	}); err != nil {
		t.Fatal(err)
	}
	code, done := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:purge"), map[string]any{"requestId": "rd"})
	if code != http.StatusOK || done["status"] != store.CommandStatusOK {
		t.Fatalf("approved execution = %d %v", code, done)
	}
	if harness.tools.callCount() != 1 {
		t.Fatalf("approved tool calls = %d", harness.tools.callCount())
	}
	// A terminal result replays without a second tool call.
	if code, _ := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:purge"), map[string]any{"requestId": "rd"}); code != http.StatusOK {
		t.Fatalf("terminal replay = %d", code)
	}
	if harness.tools.callCount() != 1 {
		t.Fatalf("terminal replay re-executed: %d", harness.tools.callCount())
	}
}

func TestCommandDestructiveDecisionRefused(t *testing.T) {
	harness := newCommandHarness(t, nil)
	harness.addPluginCommands(pluginCommandFixture("vendor.one", "purge", pluginpkg.DangerDestructive, true, ""))
	_, first := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:purge"), map[string]any{"requestId": "rd"})
	decisionID := uuid.MustParse(first["decisionId"].(string))
	if _, _, err := harness.broker.Answer(context.Background(), harnessext.AnswerDecisionRequest{
		TenantID: harness.tenant, SessionID: harness.session, DecisionID: decisionID, OptionID: commandDecisionOptionNo,
	}); err != nil {
		t.Fatal(err)
	}
	code, body := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:purge"), map[string]any{"requestId": "rd"})
	if code != http.StatusForbidden || body["code"] != CommandCodeDecisionRefused {
		t.Fatalf("refused = %d %v", code, body)
	}
	if harness.tools.callCount() != 0 {
		t.Fatal("a refused command executed the tool")
	}
	events := harness.store.eventsOfKind(store.EventCommandInvoked)
	if len(events) != 2 || events[1]["status"] != store.CommandStatusRefused ||
		events[1]["errorCode"] != CommandCodeDecisionRefused || events[1]["decisionId"] == nil {
		t.Fatalf("refusal audit = %v", events)
	}
	raw, _ := json.Marshal(events[1])
	if strings.Contains(string(raw), `"args":`) || strings.Contains(string(raw), "do-not-audit") {
		t.Fatalf("refusal audit carries arguments: %s", raw)
	}
	// The refusal replays as the first failure.
	if code, _ := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:purge"), map[string]any{"requestId": "rd"}); code != http.StatusForbidden {
		t.Fatalf("refusal replay = %d", code)
	}
}

func TestCommandDestructiveDecisionTimeout(t *testing.T) {
	harness := newCommandHarness(t, func(cfg *Config) {
		cfg.CommandDecisionTimeout = 20 * time.Millisecond
	})
	harness.addPluginCommands(pluginCommandFixture("vendor.one", "purge", pluginpkg.DangerDestructive, true, ""))
	_, first := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:purge"), map[string]any{"requestId": "rd"})
	if first["decisionId"] == nil {
		t.Fatalf("no decision opened: %v", first)
	}
	time.Sleep(60 * time.Millisecond)
	code, body := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:purge"), map[string]any{"requestId": "rd"})
	if code != http.StatusRequestTimeout || body["code"] != CommandCodeDecisionTimeout {
		t.Fatalf("timeout = %d %v", code, body)
	}
	if harness.tools.callCount() != 0 {
		t.Fatal("a timed-out command executed the tool")
	}
}

func TestCommandSkillLoadsForNextTurnAndReplays(t *testing.T) {
	harness := newCommandHarness(t, nil)
	code, body := harness.do(t, http.MethodPost, harness.commandPath("skill:alpha"), map[string]any{"requestId": "rs"})
	if code != http.StatusOK || body["status"] != store.CommandStatusOK {
		t.Fatalf("skill = %d %v", code, body)
	}
	kinds := harness.store.eventKinds()
	if len(kinds) != 2 || kinds[0] != store.EventSkillLoaded || kinds[1] != store.EventCommandInvoked {
		t.Fatalf("events = %v", kinds)
	}
	loaded := harness.store.eventsOfKind(store.EventSkillLoaded)
	if loaded[0]["name"] != "alpha" || loaded[0]["body"] != "Alpha body." || loaded[0]["hash"] != "sha256:alpha" {
		t.Fatalf("skill.loaded = %v", loaded[0])
	}
	if code, _ := harness.do(t, http.MethodPost, harness.commandPath("skill:alpha"), map[string]any{"requestId": "rs"}); code != http.StatusOK {
		t.Fatalf("skill replay = %d", code)
	}
	if got := len(harness.store.eventsOfKind(store.EventSkillLoaded)); got != 1 {
		t.Fatalf("replay appended a second skill.loaded: %d", got)
	}
	if code, body := harness.do(t, http.MethodPost, harness.commandPath("skill:missing"), map[string]any{"requestId": "rm"}); code != http.StatusNotFound || body["code"] != CommandCodeSkillNotFound {
		t.Fatalf("missing skill = %d %v", code, body)
	}
	if code, _ := harness.do(t, http.MethodPost, harness.commandPath("skill:Bad_Name"), map[string]any{"requestId": "rb"}); code != http.StatusNotFound {
		t.Fatalf("invalid skill name = %d, want 404", code)
	}
}

func TestCommandSkillRequiresSkillRead(t *testing.T) {
	harness := newCommandHarness(t, func(cfg *Config) {
		cfg.Access = &fakeAccess{
			grants: access.Grants{
				access.KeySessionRead: access.ScopeTenant, access.KeySessionWrite: access.ScopeTenant,
				access.KeyModelInvoke: access.ScopeTenant,
			},
			decisions: map[string]access.Decision{
				access.KeySkillRead: {Allow: false, Key: access.KeySkillRead, Reason: access.ReasonMissingGrant},
			},
		}
	})
	if code, _ := harness.do(t, http.MethodPost, harness.commandPath("skill:alpha"), map[string]any{"requestId": "r1"}); code != http.StatusForbidden {
		t.Fatalf("skill without skill.read = %d, want 403", code)
	}
}

func TestCommandConcurrentSameRequestIDSerializes(t *testing.T) {
	harness := newCommandHarness(t, nil)
	harness.compact.delay = 60 * time.Millisecond
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for index := 0; index < 2; index++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			code, err := harness.postAsync(harness.commandPath("compact"), map[string]any{"requestId": "rc"})
			if err != nil {
				codes[slot] = -1
				return
			}
			codes[slot] = code
		}(index)
	}
	wg.Wait()
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Fatalf("concurrent codes = %v", codes)
	}
	if harness.compact.callCount() != 1 {
		t.Fatalf("concurrent same requestId executed %d times", harness.compact.callCount())
	}
}

func TestCommandSessionWriteRequired(t *testing.T) {
	harness := newCommandHarness(t, func(cfg *Config) {
		cfg.Access = &fakeAccess{
			grants: access.Grants{access.KeySessionRead: access.ScopeTenant},
			decisions: map[string]access.Decision{
				access.KeySessionWrite: {Allow: false, Key: access.KeySessionWrite, Reason: access.ReasonMissingGrant},
			},
		}
	})
	if code, _ := harness.do(t, http.MethodPost, harness.commandPath("compact"), map[string]any{"requestId": "r1"}); code != http.StatusForbidden {
		t.Fatalf("without session.write = %d, want 403", code)
	}
	if harness.compact.callCount() != 0 {
		t.Fatal("denied command reached the seam")
	}
}

func TestCommandPluginRegistryIsTenantBound(t *testing.T) {
	harness := newCommandHarness(t, nil)
	harness.addPluginCommands(pluginCommandFixture("vendor.one", "refresh", pluginpkg.DangerWrite, true, ""))
	// Positive: the manager's own tenant sees its command.
	if code, catalog := harness.catalog(t, ""); code != http.StatusOK || len(catalog.Commands) == 0 {
		t.Fatalf("same-tenant catalog = %d %+v", code, catalog.Commands)
	}
	// The same server answering tenant A must not expose a manager bound to B.
	harness.plugins.tenantID = uuid.New()
	code, catalog := harness.catalog(t, "")
	if code != http.StatusOK {
		t.Fatalf("catalog status = %d", code)
	}
	for _, command := range catalog.Commands {
		if command.Source == "plugin" {
			t.Fatalf("foreign-tenant plugin command leaked: %+v", command)
		}
	}
	code, _ = harness.do(t, http.MethodPost, harness.commandPath("vendor.one:refresh"), map[string]any{"requestId": "r1"})
	if code != http.StatusNotFound {
		t.Fatalf("foreign-tenant plugin execution = %d, want 404", code)
	}
}

func TestCommandResultTruncatesAtBound(t *testing.T) {
	harness := newCommandHarness(t, nil)
	harness.addPluginCommands(pluginCommandFixture("vendor.one", "refresh", pluginpkg.DangerWrite, true, ""))
	harness.tools.result = json.RawMessage(`{"blob":"` + strings.Repeat("x", commandResultMaxBytes+1) + `"}`)
	code, body := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:refresh"), map[string]any{"requestId": "rbig"})
	if code != http.StatusOK || body["truncated"] != true {
		t.Fatalf("oversized result = %d %v", code, body)
	}
	data, _ := body["data"].(map[string]any)
	if data["truncated"] != true {
		t.Fatalf("oversized result data = %v", body["data"])
	}
	events := harness.store.eventsOfKind(store.EventCommandInvoked)
	if len(events) == 0 || events[len(events)-1]["truncated"] != true {
		t.Fatalf("command.invoked projection truncated = %v", events)
	}
}

func TestCommandPluginInvokePermissionIsRequired(t *testing.T) {
	harness := newCommandHarness(t, func(cfg *Config) {
		cfg.Access = &fakeAccess{
			grants: access.Grants{
				access.KeySessionRead: access.ScopeTenant, access.KeySessionWrite: access.ScopeTenant,
			},
			decisions: map[string]access.Decision{
				access.KeyPluginInvoke: {Allow: false, Key: access.KeyPluginInvoke, Reason: access.ReasonMissingGrant},
			},
		}
	})
	harness.addPluginCommands(pluginCommandFixture("vendor.one", "refresh", pluginpkg.DangerWrite, true, ""))
	if code, _ := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:refresh"), map[string]any{"requestId": "r1"}); code != http.StatusForbidden {
		t.Fatalf("plugin command without plugin.invoke = %d, want 403", code)
	}
	if harness.tools.callCount() != 0 {
		t.Fatal("a plugin command without plugin.invoke reached the tool")
	}
}

func TestCommandCatalogTruncatesAtBounds(t *testing.T) {
	harness := newCommandHarness(t, nil)
	commands := make([]harnessext.PluginCommand, 0, commandCatalogMax+10)
	for index := 0; index < commandCatalogMax+10; index++ {
		commands = append(commands, pluginCommandFixture("vendor.many", fmt.Sprintf("cmd-%03d", index), pluginpkg.DangerRead, true, ""))
	}
	harness.addPluginCommands(commands...)
	code, catalog := harness.catalog(t, "")
	if code != http.StatusOK {
		t.Fatalf("catalog status = %d", code)
	}
	if len(catalog.Commands) != commandCatalogMax || !catalog.Truncated {
		t.Fatalf("catalog = %d truncated=%v", len(catalog.Commands), catalog.Truncated)
	}
}

func TestCommandCatalogCapsSkillsAtHalf(t *testing.T) {
	harness := newCommandHarness(t, nil)
	details := make(map[string]skilllib.Detail, commandSkillCatalogMax+10)
	for index := 0; index < commandSkillCatalogMax+10; index++ {
		name := fmt.Sprintf("skill-%03d", index)
		details[name] = skilllib.Detail{Record: skilllib.Record{Name: name, Description: name}, Body: "body"}
	}
	harness.skills.details = details
	_, catalog := harness.catalog(t, "")
	skills := 0
	for _, command := range catalog.Commands {
		if command.Source == "skill" {
			skills++
		}
	}
	if skills != commandSkillCatalogMax || !catalog.Truncated {
		t.Fatalf("skills = %d truncated=%v", skills, catalog.Truncated)
	}
}

// TestCommandSkillIgnoresDisableModelInvocation pins the user-authority rule: a
// skill hidden from the model's self-service index is still loadable by an
// explicit `/skill:<name>` command.
func TestCommandSkillIgnoresDisableModelInvocation(t *testing.T) {
	library := skilllib.New(skilllib.Options{Root: t.TempDir()})
	skillDir := t.TempDir()
	document := "---\nname: hidden\ndescription: Hidden from the model index.\ndisable-model-invocation: true\n---\n\nHidden body.\n"
	if err := os.WriteFile(filepath.Join(skillDir, skills.SkillFileName), []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	if _, err := library.Install(tenant, skilllib.InstallRequest{Dir: skillDir, Trust: skilllib.TrustLocal}); err != nil {
		t.Fatalf("install hidden skill: %v", err)
	}
	harness := newCommandHarness(t, func(cfg *Config) { cfg.CommandSkills = library })
	harness.tenant = tenant
	code, body := harness.do(t, http.MethodPost, harness.commandPath("skill:hidden"), map[string]any{"requestId": "rh"})
	if code != http.StatusOK {
		t.Fatalf("hidden skill command = %d %v", code, body)
	}
	loaded := harness.store.eventsOfKind(store.EventSkillLoaded)
	if len(loaded) != 1 || !strings.Contains(loaded[0]["body"].(string), "Hidden body.") {
		t.Fatalf("skill.loaded = %v", loaded)
	}
}

func TestCommandDestructiveDecisionCancelled(t *testing.T) {
	harness := newCommandHarness(t, nil)
	harness.addPluginCommands(pluginCommandFixture("vendor.one", "purge", pluginpkg.DangerDestructive, true, ""))
	_, first := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:purge"), map[string]any{"requestId": "rd"})
	if first["decisionId"] == nil {
		t.Fatalf("no decision opened: %v", first)
	}
	if changed := harness.broker.CancelSession(context.Background(), harness.tenant, harness.session, "test cancel"); changed != 1 {
		t.Fatalf("cancelled decisions = %d", changed)
	}
	code, body := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:purge"), map[string]any{"requestId": "rd"})
	if code != http.StatusConflict || body["code"] != CommandCodeDecisionCancelled {
		t.Fatalf("cancelled = %d %v", code, body)
	}
	if harness.tools.callCount() != 0 {
		t.Fatal("a cancelled command executed the tool")
	}
}

func TestCommandDestructiveApprovedConcurrentReplaysExecuteOnce(t *testing.T) {
	harness := newCommandHarness(t, nil)
	harness.addPluginCommands(pluginCommandFixture("vendor.one", "purge", pluginpkg.DangerDestructive, true, ""))
	harness.tools.delay = 120 * time.Millisecond
	_, first := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:purge"), map[string]any{"requestId": "rd-concurrent"})
	decisionID := uuid.MustParse(first["decisionId"].(string))
	if _, _, err := harness.broker.Answer(context.Background(), harnessext.AnswerDecisionRequest{
		TenantID: harness.tenant, SessionID: harness.session,
		DecisionID: decisionID, OptionID: commandDecisionOptionOK,
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for index := range codes {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			code, err := harness.postAsync(harness.commandPath("vendor.one:purge"), map[string]any{"requestId": "rd-concurrent"})
			if err != nil {
				codes[slot] = -1
				return
			}
			codes[slot] = code
		}(index)
	}
	wg.Wait()
	for index, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("approved replay[%d] = %d, want 200", index, code)
		}
	}
	if harness.tools.callCount() != 1 {
		t.Fatalf("approved concurrent replays executed the tool %d times", harness.tools.callCount())
	}
	if events := harness.store.eventsOfKind(store.EventCommandInvoked); len(events) != 2 {
		t.Fatalf("command.invoked events = %v, want one pending + one result", events)
	}
}

func TestCommandDecisionIdentityNegativeControls(t *testing.T) {
	cases := []struct {
		name   string
		kind   string
		prompt string
	}{
		{"different kind, same prompt", "other.kind", commandDecisionPromptPrefix("vendor.one:purge") + " forged"},
		{"same kind, different prompt", commandDecisionKind, "Approve something else entirely"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness := newCommandHarness(t, nil)
			harness.addPluginCommands(pluginCommandFixture("vendor.one", "purge", pluginpkg.DangerDestructive, true, ""))
			requestID := "forged-" + strings.ReplaceAll(tc.name, " ", "-")
			forged, _, err := harness.broker.Open(context.Background(), harnessext.OpenDecisionRequest{
				TenantID: harness.tenant, SessionID: harness.session,
				RequestID: commandDecisionRequestID + requestID,
				Kind:      tc.kind,
				Prompt:    tc.prompt,
				Options: []harnessext.DecisionOption{
					{ID: commandDecisionOptionOK, Label: "Run"},
					{ID: commandDecisionOptionNo, Label: "Cancel"},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := harness.broker.Answer(context.Background(), harnessext.AnswerDecisionRequest{
				TenantID: harness.tenant, SessionID: harness.session,
				DecisionID: uuid.MustParse(forged.DecisionID), OptionID: commandDecisionOptionOK,
			}); err != nil {
				t.Fatal(err)
			}
			code, body := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:purge"), map[string]any{"requestId": requestID})
			if code != http.StatusForbidden || body["code"] != CommandCodeDecisionRefused {
				t.Fatalf("forged grant = %d %v", code, body)
			}
			if harness.tools.callCount() != 0 {
				t.Fatal("a forged decision executed the tool")
			}
			row, found, err := harness.store.GetCommandInvocation(context.Background(), harness.tenant, harness.session, requestID)
			if err != nil || !found || row.Status != store.CommandStatusRefused {
				t.Fatalf("forged row = %+v found=%v err=%v", row, found, err)
			}
		})
	}
}

func TestCommandDecisionOpenConflictIs409(t *testing.T) {
	harness := newCommandHarness(t, nil)
	harness.addPluginCommands(pluginCommandFixture("vendor.one", "purge", pluginpkg.DangerDestructive, true, ""))
	requestID := "conflict"
	if _, _, err := harness.broker.Open(context.Background(), harnessext.OpenDecisionRequest{
		TenantID: harness.tenant, SessionID: harness.session,
		RequestID: commandDecisionRequestID + requestID,
		Kind:      "other.kind",
		Prompt:    "another question",
		Options:   []harnessext.DecisionOption{{ID: "ok"}, {ID: "no"}},
	}); err != nil {
		t.Fatal(err)
	}
	code, body := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:purge"), map[string]any{"requestId": requestID})
	if code != http.StatusConflict {
		t.Fatalf("decision conflict = %d %v, want 409", code, body)
	}
	if harness.tools.callCount() != 0 {
		t.Fatal("a conflicting decision executed the tool")
	}
}

// TestCommandResumeDecisionIdentityMutationGuards is the mutation-sensitive
// guard test: a pending row whose stored decision was answered allow under a
// different kind (but the derived requestId) or under a different requestId
// (but the command kind) must NOT execute. Deleting either the Kind or the
// RequestID check turns this test red.
func TestCommandResumeDecisionIdentityMutationGuards(t *testing.T) {
	cases := []struct {
		name       string
		decisionID func(t *testing.T, harness *commandHarness, requestID string) string
	}{
		{
			name: "forged kind on the derived requestId",
			decisionID: func(t *testing.T, harness *commandHarness, requestID string) string {
				decision, _, err := harness.broker.Open(context.Background(), harnessext.OpenDecisionRequest{
					TenantID: harness.tenant, SessionID: harness.session,
					RequestID: commandDecisionRequestID + requestID,
					Kind:      "other.kind",
					Prompt:    commandDecisionPromptPrefix("vendor.one:purge") + " forged kind",
					Options: []harnessext.DecisionOption{
						{ID: commandDecisionOptionOK, Label: "Run"},
						{ID: commandDecisionOptionNo, Label: "Cancel"},
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				return decision.DecisionID
			},
		},
		{
			name: "forged requestId with the command kind",
			decisionID: func(t *testing.T, harness *commandHarness, _ string) string {
				decision, _, err := harness.broker.Open(context.Background(), harnessext.OpenDecisionRequest{
					TenantID: harness.tenant, SessionID: harness.session,
					RequestID: "some-other-decision-" + uuid.NewString(),
					Kind:      commandDecisionKind,
					Prompt:    commandDecisionPromptPrefix("vendor.one:purge") + " forged request",
					Options: []harnessext.DecisionOption{
						{ID: commandDecisionOptionOK, Label: "Run"},
						{ID: commandDecisionOptionNo, Label: "Cancel"},
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				return decision.DecisionID
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness := newCommandHarness(t, nil)
			harness.addPluginCommands(pluginCommandFixture("vendor.one", "purge", pluginpkg.DangerDestructive, true, ""))
			requestID := "mutation-" + strings.ReplaceAll(tc.name, " ", "-")
			decisionID := tc.decisionID(t, harness, requestID)
			if _, _, err := harness.broker.Answer(context.Background(), harnessext.AnswerDecisionRequest{
				TenantID: harness.tenant, SessionID: harness.session,
				DecisionID: uuid.MustParse(decisionID), OptionID: commandDecisionOptionOK,
			}); err != nil {
				t.Fatal(err)
			}
			// Seed the pending row as if the decision had been opened, then the
			// decision was replaced by the forged one. This is the resume path.
			invocation, _, err := harness.store.ClaimCommandInvocation(context.Background(), store.CommandInvocation{
				TenantID: harness.tenant, SessionID: harness.session, RequestID: requestID,
				Command: "vendor.one:purge", Source: "plugin", Status: store.CommandStatusRunning,
				ArgsHash: sha256Hex([]byte("{}")), ArgsBytes: 2,
			}, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			invocation.Status = store.CommandStatusPendingDecision
			invocation.DecisionID = decisionID
			if ok, err := harness.store.TransitionCommandInvocation(context.Background(), invocation, store.CommandStatusRunning); err != nil || !ok {
				t.Fatalf("seed pending = %v err=%v", ok, err)
			}
			code, body := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:purge"), map[string]any{"requestId": requestID})
			if code != http.StatusForbidden || body["code"] != CommandCodeDecisionRefused {
				t.Fatalf("forged resume = %d %v", code, body)
			}
			if harness.tools.callCount() != 0 {
				t.Fatalf("a forged decision executed the tool %d times", harness.tools.callCount())
			}
		})
	}
}

func TestCommandSchemaConstraintsEnforced(t *testing.T) {
	harness := newCommandHarness(t, nil)
	command := pluginCommandFixture("vendor.one", "refresh", pluginpkg.DangerWrite, true, "")
	command.Decl.ArgsSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"slug":   map[string]any{"type": "string", "minLength": 3, "maxLength": 8, "pattern": "^[a-z]+$"},
			"count":  map[string]any{"type": "integer", "minimum": 1, "maximum": 5},
			"labels": map[string]any{"type": "array", "minItems": 1, "maxItems": 2, "uniqueItems": true, "items": map[string]any{"type": "string"}},
		},
	}
	harness.addPluginCommands(command)

	cases := []struct {
		name string
		args map[string]any
		want int
	}{
		{"valid", map[string]any{"slug": "abc", "count": 2, "labels": []any{"a", "b"}}, http.StatusOK},
		{"too short", map[string]any{"slug": "ab"}, http.StatusBadRequest},
		{"pattern", map[string]any{"slug": "ABC"}, http.StatusBadRequest},
		{"too long", map[string]any{"slug": "abcdefghi"}, http.StatusBadRequest},
		{"below minimum", map[string]any{"count": 0}, http.StatusBadRequest},
		{"above maximum", map[string]any{"count": 6}, http.StatusBadRequest},
		{"too few items", map[string]any{"labels": []any{}}, http.StatusBadRequest},
		{"too many items", map[string]any{"labels": []any{"a", "b", "c"}}, http.StatusBadRequest},
		{"duplicate items", map[string]any{"labels": []any{"a", "a"}}, http.StatusBadRequest},
	}
	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _ := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:refresh"), map[string]any{
				"requestId": fmt.Sprintf("constraint-%d", index),
				"args":      tc.args,
			})
			if code != tc.want {
				t.Fatalf("status = %d, want %d", code, tc.want)
			}
		})
	}
}

func TestCommandAuditActorUsesBotID(t *testing.T) {
	harness := newCommandHarness(t, nil)
	harness.access.principal = access.Principal{
		TenantID: harness.tenant, AccountID: uuid.New(), BotID: "support-1",
	}
	if code, body := harness.do(t, http.MethodPost, harness.commandPath("compact"), map[string]any{"requestId": "rbot"}); code != http.StatusOK {
		t.Fatalf("bot command = %d %v", code, body)
	}
	events := harness.store.eventsOfKind(store.EventCommandInvoked)
	if len(events) != 1 || events[0]["actor"] != "support-1" {
		t.Fatalf("bot audit actor = %v, want support-1", events)
	}
}

func TestCommandCatalogMarksNotWired(t *testing.T) {
	harness := newCommandHarness(t, func(cfg *Config) {
		cfg.CommandStore = nil
		cfg.SessionIdle = nil
	})
	harness.addPluginCommands(pluginCommandFixture("vendor.one", "refresh", pluginpkg.DangerWrite, true, ""))
	code, catalog := harness.catalog(t, "")
	if code != http.StatusOK {
		t.Fatalf("catalog status = %d", code)
	}
	for _, command := range catalog.Commands {
		if command.Available || command.Reason != CommandReasonNotWired {
			t.Fatalf("unwired command = %+v", command)
		}
	}
}

func TestCommandCatalogDegradesWhenSkillLibraryFails(t *testing.T) {
	harness := newCommandHarness(t, nil)
	harness.addPluginCommands(pluginCommandFixture("vendor.one", "refresh", pluginpkg.DangerWrite, true, ""))
	harness.skills.err = errors.New("skill library down")
	code, catalog := harness.catalog(t, "")
	if code != http.StatusOK {
		t.Fatalf("catalog status = %d, want 200", code)
	}
	if len(catalog.Commands) != 2 || catalog.Commands[0].Name != "compact" || catalog.Commands[1].Name != "vendor.one:refresh" {
		t.Fatalf("degraded catalog = %+v", catalog.Commands)
	}
	found := false
	for _, warning := range catalog.Warnings {
		if warning == "skills_unavailable" {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %v", catalog.Warnings)
	}
}

func TestCommandSkillEventIsBoundedAndReferenced(t *testing.T) {
	harness := newCommandHarness(t, nil)
	body := strings.Repeat("x", commandSkillBodyMax+1024)
	harness.skills.details["big"] = skilllib.Detail{
		Record: skilllib.Record{Name: "big", Description: strings.Repeat("d", 2048), CurrentHash: "sha256:big", Path: "active/big"},
		Body:   body,
	}
	if code, _ := harness.do(t, http.MethodPost, harness.commandPath("skill:big"), map[string]any{"requestId": "rbig"}); code != http.StatusOK {
		t.Fatalf("big skill status = %d", code)
	}
	events := harness.store.eventsOfKind(store.EventSkillLoaded)
	if len(events) != 1 {
		t.Fatalf("events = %v", events)
	}
	storedBody, _ := events[0]["body"].(string)
	if len(storedBody) > commandSkillBodyMax+len("\n[truncated]") {
		t.Fatalf("stored skill body = %d bytes", len(storedBody))
	}
	if !strings.Contains(storedBody, "[truncated]") {
		t.Fatal("bounded skill body is not marked")
	}
	if events[0]["location"] != "active/big" || events[0]["hash"] != "sha256:big" {
		t.Fatalf("skill reference = %v", events[0])
	}
}

func TestCommandDestructivePromptShowsBoundedArguments(t *testing.T) {
	harness := newCommandHarness(t, nil)
	harness.addPluginCommands(pluginCommandFixture("vendor.one", "purge", pluginpkg.DangerDestructive, true, ""))
	_, first := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:purge"), map[string]any{
		"requestId": "rpreview", "args": map[string]any{"secret": "value"},
	})
	decision, found := harness.broker.Get(context.Background(), harness.tenant, harness.session,
		uuid.MustParse(first["decisionId"].(string)))
	if !found {
		t.Fatal("decision not found")
	}
	if !strings.Contains(decision.Prompt, `"secret":"value"`) {
		t.Fatalf("prompt lacks the bounded argument preview: %q", decision.Prompt)
	}
	long := strings.Repeat("x", commandDecisionPreviewMax*2)
	_, second := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:purge"), map[string]any{
		"requestId": "rpreview2", "args": map[string]any{"secret": long},
	})
	decision, _ = harness.broker.Get(context.Background(), harness.tenant, harness.session,
		uuid.MustParse(second["decisionId"].(string)))
	if len(decision.Prompt) > len(commandDecisionPromptPrefix("vendor.one:purge"))+commandDecisionPreviewMax+4096 {
		t.Fatalf("prompt preview is not bounded: %d bytes", len(decision.Prompt))
	}
}

func TestCommandPluginIsErrorIs502(t *testing.T) {
	harness := newCommandHarness(t, nil)
	harness.addPluginCommands(pluginCommandFixture("vendor.one", "refresh", pluginpkg.DangerWrite, true, ""))
	harness.tools.result = json.RawMessage(`{"content":[{"type":"text","text":"boom"}],"isError":true}`)
	code, body := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:refresh"), map[string]any{"requestId": "riserror"})
	if code != http.StatusBadGateway || body["code"] != CommandCodeToolError {
		t.Fatalf("plugin isError = %d %v", code, body)
	}
	row, found, err := harness.store.GetCommandInvocation(context.Background(), harness.tenant, harness.session, "riserror")
	if err != nil || !found || row.Status != store.CommandStatusError || row.ErrorCode != CommandCodeToolError {
		t.Fatalf("isError row = %+v found=%v err=%v", row, found, err)
	}
}

func TestCommandPermissionRunsBeforeBody(t *testing.T) {
	harness := newCommandHarness(t, func(cfg *Config) {
		cfg.Access = &fakeAccess{
			grants: access.Grants{access.KeySessionRead: access.ScopeTenant, access.KeySessionWrite: access.ScopeTenant},
			decisions: map[string]access.Decision{
				access.KeyModelInvoke: {Allow: false, Key: access.KeyModelInvoke, Reason: access.ReasonMissingGrant},
			},
		}
	})
	// The unknown field would be a 400 if the body were read first.
	code, _ := harness.do(t, http.MethodPost, harness.commandPath("compact"), map[string]any{
		"requestId": "rperm", "bogus": true,
	})
	if code != http.StatusForbidden {
		t.Fatalf("unauthorized malformed body = %d, want 403", code)
	}
	if harness.compact.callCount() != 0 {
		t.Fatal("denied command reached the seam")
	}
}

func TestCommandOrphanRunningLeaseIsReclaimed(t *testing.T) {
	harness := newCommandHarness(t, nil)
	past := time.Now().Add(-time.Minute)
	harness.store.rows[commandRowKey(harness.tenant, harness.session, "rorphan")] = store.CommandInvocation{
		TenantID: harness.tenant, SessionID: harness.session, RequestID: "rorphan",
		Command: "compact", Source: "builtin", Status: store.CommandStatusRunning,
		ArgsHash: sha256Hex([]byte("{}")), ArgsBytes: 2, LeaseUntil: &past,
	}
	code, body := harness.do(t, http.MethodPost, harness.commandPath("compact"), map[string]any{"requestId": "rorphan"})
	if code != http.StatusOK || body["status"] != store.CommandStatusOK {
		t.Fatalf("orphan reclaim = %d %v", code, body)
	}
	if harness.compact.callCount() != 1 {
		t.Fatalf("orphan executions = %d", harness.compact.callCount())
	}
}

func TestCommandLiveLeaseIsNotStolen(t *testing.T) {
	harness := newCommandHarness(t, func(cfg *Config) {
		cfg.CommandWaitTimeout = 40 * time.Millisecond
	})
	future := time.Now().Add(time.Minute)
	harness.store.rows[commandRowKey(harness.tenant, harness.session, "rlive")] = store.CommandInvocation{
		TenantID: harness.tenant, SessionID: harness.session, RequestID: "rlive",
		Command: "compact", Source: "builtin", Status: store.CommandStatusRunning,
		ArgsHash: sha256Hex([]byte("{}")), ArgsBytes: 2, LeaseUntil: &future,
	}
	code, body := harness.do(t, http.MethodPost, harness.commandPath("compact"), map[string]any{"requestId": "rlive"})
	if code != http.StatusConflict || body["code"] != CommandCodeInProgress {
		t.Fatalf("live lease = %d %v", code, body)
	}
	if harness.compact.callCount() != 0 {
		t.Fatal("a live lease was stolen")
	}
}

func TestCommandCatalogUsesSessionRelationForAvailability(t *testing.T) {
	harness := newCommandHarness(t, nil)
	harness.access.decisions = map[string]access.Decision{
		access.KeySessionWrite: {Allow: false, Key: access.KeySessionWrite, Reason: access.ReasonMissingRelation},
	}
	_, catalog := harness.catalog(t, "")
	compact := catalog.Commands[0]
	if compact.Available || compact.Reason != CommandReasonMissingPermissions ||
		len(compact.MissingPermissions) == 0 || compact.MissingPermissions[0] != access.KeySessionWrite {
		t.Fatalf("relation-denied catalog entry = %+v", compact)
	}
}

func TestCommandCatalogMarksPluginSeamsNotWired(t *testing.T) {
	t.Run("no tool dispatch", func(t *testing.T) {
		harness := newCommandHarness(t, func(cfg *Config) { cfg.CommandTools = nil })
		harness.addPluginCommands(pluginCommandFixture("vendor.one", "refresh", pluginpkg.DangerWrite, true, ""))
		_, catalog := harness.catalog(t, "")
		for _, command := range catalog.Commands {
			if command.Source == "plugin" && (command.Available || command.Reason != CommandReasonNotWired) {
				t.Fatalf("plugin without dispatch = %+v", command)
			}
		}
	})
	t.Run("no decisions for destructive", func(t *testing.T) {
		harness := newCommandHarness(t, func(cfg *Config) { cfg.Decisions = nil })
		harness.addPluginCommands(pluginCommandFixture("vendor.one", "purge", pluginpkg.DangerDestructive, true, ""))
		_, catalog := harness.catalog(t, "")
		for _, command := range catalog.Commands {
			if command.Source == "plugin" && (command.Available || command.Reason != CommandReasonNotWired) {
				t.Fatalf("destructive without decisions = %+v", command)
			}
		}
	})
}

func TestCommandApprovedReplayWaitsForIdle(t *testing.T) {
	harness := newCommandHarness(t, nil)
	harness.addPluginCommands(pluginCommandFixture("vendor.one", "purge", pluginpkg.DangerDestructive, true, ""))
	_, first := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:purge"), map[string]any{"requestId": "ridle"})
	if _, _, err := harness.broker.Answer(context.Background(), harnessext.AnswerDecisionRequest{
		TenantID: harness.tenant, SessionID: harness.session,
		DecisionID: uuid.MustParse(first["decisionId"].(string)), OptionID: commandDecisionOptionOK,
	}); err != nil {
		t.Fatal(err)
	}
	harness.idle = func() bool { return false }
	code, body := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:purge"), map[string]any{"requestId": "ridle"})
	if code != http.StatusConflict || body["code"] != CommandCodeBusy {
		t.Fatalf("approved while busy = %d %v", code, body)
	}
	if harness.tools.callCount() != 0 {
		t.Fatal("approved command ran while the session was busy")
	}
	row, found, err := harness.store.GetCommandInvocation(context.Background(), harness.tenant, harness.session, "ridle")
	if err != nil || !found || row.Status != store.CommandStatusPendingDecision {
		t.Fatalf("busy left the row = %+v found=%v err=%v", row, found, err)
	}
	harness.idle = func() bool { return true }
	if code, _ := harness.do(t, http.MethodPost, harness.commandPath("vendor.one:purge"), map[string]any{"requestId": "ridle"}); code != http.StatusOK {
		t.Fatalf("idle retry = %d", code)
	}
	if harness.tools.callCount() != 1 {
		t.Fatalf("idle retry tool calls = %d", harness.tools.callCount())
	}
}
