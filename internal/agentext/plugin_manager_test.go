package agentext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/pluginpkg"
	"github.com/jixoai/keel/internal/store"
)

// --- fakes ---------------------------------------------------------------

// fakePluginStore is the in-memory PluginPersistence: rows plus the audit
// trail, with the same shapes the PG store uses.
type fakePluginStore struct {
	mu    sync.Mutex
	rows  map[string]store.PluginRecord
	audit []store.PluginAuditEntry
}

func newFakePluginStore() *fakePluginStore {
	return &fakePluginStore{rows: map[string]store.PluginRecord{}}
}

func (f *fakePluginStore) UpsertPlugin(_ context.Context, record store.PluginRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[record.ID] = record
	return nil
}

func (f *fakePluginStore) GetPlugin(_ context.Context, id string) (store.PluginRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[id]
	return row, ok, nil
}

func (f *fakePluginStore) ListPlugins(context.Context) ([]store.PluginRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.PluginRecord, 0, len(f.rows))
	for _, row := range f.rows {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakePluginStore) DeletePlugin(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.rows[id]
	delete(f.rows, id)
	return ok, nil
}

func (f *fakePluginStore) AppendPluginAudit(_ context.Context, entry store.PluginAuditEntry) (store.PluginAuditEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry.AuditID = int64(len(f.audit) + 1)
	f.audit = append(f.audit, entry)
	return entry, nil
}

func (f *fakePluginStore) ListPluginAudit(_ context.Context, tenantID *uuid.UUID, pluginID string, after int64, limit int) ([]store.PluginAuditEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.PluginAuditEntry, 0)
	for _, entry := range f.audit {
		if entry.AuditID <= after {
			continue
		}
		if tenantID != nil && entry.TenantID != nil && *entry.TenantID != *tenantID {
			continue
		}
		if pluginID != "" && entry.PluginID != pluginID {
			continue
		}
		out = append(out, entry)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakePluginStore) row(t *testing.T, id string) store.PluginRecord {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[id]
	if !ok {
		t.Fatalf("no plugin row for %s", id)
	}
	return row
}

func (f *fakePluginStore) auditActions(t *testing.T) []string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.audit))
	for _, entry := range f.audit {
		out = append(out, entry.Action+":"+entry.Result)
	}
	return out
}

// fakeLoader materializes packages of one kind. loadErr, when set, fails every
// load; perVersion fails specific versions (update rollback tests).
type fakeLoader struct {
	mu         sync.Mutex
	loadErr    error
	perVersion map[string]error
	calls      []string
}

func (l *fakeLoader) Load(_ context.Context, pkg pluginpkg.Package, manifest *pluginpkg.Manifest) (*Plugin, error) {
	l.mu.Lock()
	l.calls = append(l.calls, manifest.ID+"@"+manifest.Version)
	err := l.loadErr
	if specific, ok := l.perVersion[manifest.Version]; ok {
		err = specific
	}
	l.mu.Unlock()
	if err != nil {
		return nil, err
	}
	version := manifest.Version
	return &Plugin{
		ID:      manifest.ID,
		Version: version,
		Tools: []ToolSpec{{
			Name:        strings.ReplaceAll(manifest.ID, ".", "_") + "_run",
			Description: "loaded tool",
			Schema:      json.RawMessage(`{"type":"object"}`),
			Execute: func(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
				return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("ok")}}, nil
			},
		}},
	}, nil
}

// managerHarness owns the pieces a manager test needs.
type managerHarness struct {
	manager *Manager
	runtime *Runtime
	store   *fakePluginStore
	loader  *fakeLoader
	root    string
	hooks   *HookBus
	tools   *toolRecorder
	clock   *testClock
}

type toolRecorder struct {
	mu    sync.Mutex
	tools []string
	calls int
}

func (r *toolRecorder) sink(tools []agentcore.AgentTool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.tools = r.tools[:0]
	for _, tool := range tools {
		r.tools = append(r.tools, tool.Name())
	}
	return nil
}

func (r *toolRecorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.tools...)
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) tick() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Millisecond)
	return c.now
}

// exampleBuiltin is the builtin plugin every harness registers: one tool, one
// hook subscription, one webhook trigger, one effect, Start/Stop.
func exampleBuiltin(defaultEnabled bool, events *[]string, mu *sync.Mutex) Builtin {
	manifest := &pluginpkg.Manifest{
		ID:          "keel.example",
		Version:     "1.0.0",
		Kind:        pluginpkg.KindInProcess,
		Description: "example",
		Engines:     pluginpkg.Engines{Keel: ">=0.1.0 <1.0.0"},
		Provides: pluginpkg.Provides{
			Tools:    []string{"example_status"},
			Hooks:    []string{"turn.started"},
			Triggers: []string{"keel.example.ping"},
		},
	}
	return Builtin{
		Manifest:       manifest,
		DefaultEnabled: defaultEnabled,
		Factory: func() *Plugin {
			return &Plugin{
				ID:      manifest.ID,
				Version: manifest.Version,
				Tools: []ToolSpec{{
					Name:        "example_status",
					Description: "status",
					Schema:      json.RawMessage(`{"type":"object"}`),
					Execute: func(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
						return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("ok")}}, nil
					},
				}},
				Hooks: []HookSpec{{Event: HookTurnStarted, Handler: func(context.Context, HookPayload) error {
					mu.Lock()
					*events = append(*events, "hook")
					mu.Unlock()
					return nil
				}}},
				Triggers: []TriggerSpec{{ID: "keel.example.ping", Kind: TriggerWebhook, Prompt: "p", Enabled: true}},
				Effects:  []Effect{{Name: "seal", Stop: func(context.Context) error { return nil }}},
				Start: func(context.Context, *PluginEnv) error {
					mu.Lock()
					*events = append(*events, "start")
					mu.Unlock()
					return nil
				},
				Stop: func(context.Context) error {
					mu.Lock()
					*events = append(*events, "stop")
					mu.Unlock()
					return nil
				},
			}
		},
	}
}

func newManagerHarness(t *testing.T, root string, builtins []Builtin) *managerHarness {
	t.Helper()
	hooks := NewHookBus(HookBusOptions{Logger: testLogger(), Queue: 64, Workers: 1})
	triggers := NewTriggerEngine(TriggerEngineOptions{Starter: &countingStarter{}, Logger: testLogger()})
	recorder := &toolRecorder{}
	clock := &testClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	runtime := NewRuntime(RuntimeOptions{
		Hooks:    hooks,
		Triggers: triggers,
		Tools:    recorder.sink,
		Logger:   testLogger(),
		Now:      clock.tick,
	})
	loader := &fakeLoader{perVersion: map[string]error{}}
	persistence := newFakePluginStore()
	manager, err := NewPluginManager(ManagerOptions{
		Root:        root,
		Runtime:     runtime,
		Persistence: persistence,
		Builtins:    builtins,
		Loaders:     map[pluginpkg.Kind]PluginLoader{pluginpkg.KindWebview: loader},
		KeelVersion: "0.1.0",
		Logger:      testLogger(),
		Now:         clock.tick,
	})
	if err != nil {
		t.Fatal(err)
	}
	harness := &managerHarness{manager: manager, runtime: runtime, store: persistence, loader: loader, root: root, hooks: hooks, tools: recorder, clock: clock}
	t.Cleanup(func() {
		runtime.Close(context.Background())
		triggers.Close()
		hooks.Close()
	})
	return harness
}

// writePluginPackage drops a package into a directory.
func writePluginPackage(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	path, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		target := filepath.Join(path, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func webviewManifest(id, version string, requires []string, backend bool) string {
	requiresLine := ""
	if len(requires) > 0 {
		requiresLine = "requires:\n" + strings.Join(prefixEach(requires, "  - "), "\n") + "\n"
	}
	entry := ""
	if backend {
		entry = "entry:\n  command: [node, backend.js]\n"
	}
	return fmt.Sprintf("id: %s\nversion: %s\nkind: webview\ndescription: ui package\nengines:\n  keel: \">=0.1.0 <1.0.0\"\n%sprovides:\n  ui:\n    entry: ui/index.html\n    title: Panel\n%s", id, version, requiresLine, entry)
}

func prefixEach(values []string, prefix string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, prefix+value)
	}
	return out
}

// --- tests ---------------------------------------------------------------

func TestPluginManagerBootDisableEnableRoundTrip(t *testing.T) {
	var (
		mu     sync.Mutex
		events []string
	)
	harness := newManagerHarness(t, t.TempDir(), []Builtin{exampleBuiltin(true, &events, &mu)})
	ctx := context.Background()
	if err := harness.manager.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	inventory := harness.manager.Snapshot()
	if len(inventory.Plugins) != 1 || inventory.Plugins[0].State != LifecycleEnabled {
		t.Fatalf("boot inventory = %+v", inventory.Plugins)
	}
	if inventory.Plugins[0].Runtime == nil || inventory.Plugins[0].Runtime.State != PluginActive {
		t.Fatalf("runtime view = %+v", inventory.Plugins[0].Runtime)
	}
	if got := harness.tools.list(); len(got) != 1 || got[0] != "example_status" {
		t.Fatalf("published tools = %v", got)
	}
	if row := harness.store.row(t, "keel.example"); row.State != store.PluginStateEnabled || !row.Bundled {
		t.Fatalf("row = %+v", row)
	}

	// Disable revokes every registration: tool, hook, trigger and effect.
	view, err := harness.manager.Disable(ctx, "keel.example", AuditContext{Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if view.State != LifecycleDisabled || view.Runtime != nil {
		t.Fatalf("disabled view = %+v", view)
	}
	if got := harness.tools.list(); len(got) != 0 {
		t.Fatalf("tools after disable = %v", got)
	}
	if got := harness.runtime.Snapshot().Plugins; len(got) != 0 {
		t.Fatalf("runtime plugins after disable = %+v", got)
	}
	if len(harness.runtime.triggers.List()) != 0 {
		t.Fatalf("triggers after disable = %+v", harness.runtime.triggers.List())
	}
	mu.Lock()
	before := len(events)
	mu.Unlock()
	harness.hooks.Emit(HookPayload{Event: HookTurnStarted, TenantID: "t"})
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	after := len(events)
	mu.Unlock()
	if after != before {
		t.Fatalf("a disabled plugin must not receive hooks: events %v", events)
	}

	// Enable again: the plugin comes back cleanly (a fresh instance, so its own
	// state restarts rather than resuming a sealed one).
	view, err = harness.manager.Enable(ctx, "keel.example", AuditContext{Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if view.State != LifecycleEnabled || view.EnabledAt == nil {
		t.Fatalf("re-enabled view = %+v", view)
	}
	if got := harness.tools.list(); len(got) != 1 {
		t.Fatalf("tools after re-enable = %v", got)
	}
	if got := harness.runtime.triggers.List(); len(got) != 1 {
		t.Fatalf("triggers after re-enable = %+v", got)
	}
	actions := harness.store.auditActions(t)
	want := []string{"discover:ok", "disable:ok", "enable:ok"}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Fatalf("audit = %v, want %v", actions, want)
	}
}

func TestPluginManagerInstallEnableDisableUninstall(t *testing.T) {
	root := t.TempDir()
	harness := newManagerHarness(t, root, nil)
	ctx := context.Background()
	if err := harness.manager.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	source := writePluginPackage(t, filepath.Join(t.TempDir(), "panel-package"), map[string]string{
		"plugin.yaml":   webviewManifest("vendor.panel", "1.0.0", nil, false),
		"ui/index.html": "<html>panel</html>",
	})

	view, err := harness.manager.Install(ctx, InstallRequest{Path: source}, AuditContext{Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if view.State != LifecycleInstalled || view.Bundled || view.ContentHash == "" {
		t.Fatalf("installed view = %+v", view)
	}
	// The managed copy lives under the root and never points at the source.
	if !pluginpkg.ManagedDir(root, view.Path) {
		t.Fatalf("managed path = %q (root %q)", view.Path, root)
	}
	if strings.HasPrefix(view.Path, source) {
		t.Fatalf("the stored plugin must be a copy, not the source: %q", view.Path)
	}
	if _, err := os.Stat(filepath.Join(view.Path, "ui", "index.html")); err != nil {
		t.Fatalf("copied package is incomplete: %v", err)
	}
	// A UI-only webview plugin has no runtime surface: enabling it is a
	// lifecycle fact and the runtime stays out of it.
	view, err = harness.manager.Enable(ctx, "vendor.panel", AuditContext{Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if view.State != LifecycleEnabled || view.Runtime != nil {
		t.Fatalf("enabled view = %+v", view)
	}
	// A second install of the same id is a conflict.
	if _, err := harness.manager.Install(ctx, InstallRequest{Path: source}, AuditContext{Actor: "tester"}); !errors.Is(err, ErrPluginExists) {
		t.Fatalf("duplicate install err = %v", err)
	}
	// Uninstall of an enabled plugin is refused: disable first.
	if _, err := harness.manager.Uninstall(ctx, "vendor.panel", AuditContext{Actor: "tester"}); !errors.Is(err, ErrPluginState) {
		t.Fatalf("uninstall while enabled err = %v", err)
	}
	if _, err := harness.manager.Disable(ctx, "vendor.panel", AuditContext{Actor: "tester"}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.manager.Uninstall(ctx, "vendor.panel", AuditContext{Actor: "tester"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pluginpkg.PackageDirFor(root, "vendor.panel")); !os.IsNotExist(err) {
		t.Fatal("the managed directory must be removed on uninstall")
	}
	if harness.manager.Snapshot().Plugins == nil || len(harness.manager.Snapshot().Plugins) != 0 {
		t.Fatalf("inventory after uninstall = %+v", harness.manager.Snapshot().Plugins)
	}
	actions := harness.store.auditActions(t)
	want := []string{"install:ok", "enable:ok", "install:refused", "uninstall:refused", "disable:ok", "uninstall:ok"}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Fatalf("audit = %v, want %v", actions, want)
	}
	// The audit trail is queryable by plugin and cursor.
	entries, err := harness.manager.Audit(ctx, nil, "vendor.panel", 4, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Action != store.PluginActionDisable {
		t.Fatalf("audit page = %+v", entries)
	}
	for _, entry := range entries {
		if entry.AuditID == 0 || entry.TS.IsZero() || entry.Actor != "tester" || entry.Version != "1.0.0" {
			t.Fatalf("audit entry = %+v", entry)
		}
	}
}

func TestPluginManagerDependenciesAndKindLoader(t *testing.T) {
	root := t.TempDir()
	var (
		mu     sync.Mutex
		events []string
	)
	harness := newManagerHarness(t, root, []Builtin{exampleBuiltin(false, &events, &mu)})
	ctx := context.Background()
	// A provider builtin that is not enabled by default, plus a package whose
	// webview backend requires it.
	writePluginPackage(t, filepath.Join(root, "vendor.consumer"), map[string]string{
		"plugin.yaml":   webviewManifest("vendor.consumer", "1.0.0", []string{"keel.example"}, true),
		"ui/index.html": "<html></html>",
		"backend.js":    "//",
	})
	if err := harness.manager.Boot(ctx); err != nil {
		t.Fatal(err)
	}

	// The dependency is not enabled: B stays disabled-ish (installed) with the
	// missing requirement named, and nothing is half-loaded.
	_, err := harness.manager.Enable(ctx, "vendor.consumer", AuditContext{Actor: "tester"})
	if !errors.Is(err, ErrPluginDependency) {
		t.Fatalf("enable without dependency err = %v", err)
	}
	var pluginErr *PluginError
	if !errors.As(err, &pluginErr) || len(pluginErr.Missing) != 1 || pluginErr.Missing[0] != "keel.example" {
		t.Fatalf("missing list = %+v", pluginErr)
	}
	view := harness.manager.Snapshot().Plugins[1]
	if view.ID != "vendor.consumer" || view.State != LifecycleInstalled {
		t.Fatalf("state after refused enable = %+v", view)
	}
	if harness.runtime.Snapshot().Tools == nil {
		t.Fatal("a refused enable must not publish a tool surface")
	}
	if len(harness.tools.list()) != 0 {
		t.Fatalf("tools = %v", harness.tools.list())
	}

	// Enable the provider, then the consumer: the loader materializes it and its
	// tool joins the surface.
	if _, err := harness.manager.Enable(ctx, "keel.example", AuditContext{Actor: "tester"}); err != nil {
		t.Fatal(err)
	}
	view, err = harness.manager.Enable(ctx, "vendor.consumer", AuditContext{Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if view.State != LifecycleEnabled || view.Runtime == nil || view.Runtime.State != PluginActive {
		t.Fatalf("consumer view = %+v", view)
	}
	if got := strings.Join(harness.tools.list(), ","); got != "example_status,vendor_consumer_run" {
		t.Fatalf("tools = %q", got)
	}
	// Disabling the provider while the consumer needs it is refused with the
	// dependent named.
	_, err = harness.manager.Disable(ctx, "keel.example", AuditContext{Actor: "tester"})
	if !errors.Is(err, ErrPluginDependency) {
		t.Fatalf("disable provider err = %v", err)
	}
	if !errors.As(err, &pluginErr) || len(pluginErr.Dependents) != 1 || pluginErr.Dependents[0] != "vendor.consumer" {
		t.Fatalf("dependents = %+v", pluginErr)
	}
	if _, err := harness.manager.Disable(ctx, "vendor.consumer", AuditContext{Actor: "tester"}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.manager.Disable(ctx, "keel.example", AuditContext{Actor: "tester"}); err != nil {
		t.Fatal(err)
	}
	if harness.runtime.Generation() < 5 {
		t.Fatalf("every mutation is an assembly generation: %d", harness.runtime.Generation())
	}
}

func TestPluginManagerUpdateSwapsAndRollsBack(t *testing.T) {
	root := t.TempDir()
	harness := newManagerHarness(t, root, nil)
	ctx := context.Background()
	if err := harness.manager.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	source := writePluginPackage(t, filepath.Join(t.TempDir(), "pkg-1"), map[string]string{
		"plugin.yaml":   webviewManifest("vendor.panel", "1.0.0", nil, true),
		"ui/index.html": "<html>1</html>",
		"backend.js":    "//",
	})
	if _, err := harness.manager.Install(ctx, InstallRequest{Path: source}, AuditContext{Actor: "tester"}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.manager.Enable(ctx, "vendor.panel", AuditContext{Actor: "tester"}); err != nil {
		t.Fatal(err)
	}
	firstHash := harness.store.row(t, "vendor.panel").ContentHash

	// A failing loader must leave the previous version installed and enabled.
	upgrade := writePluginPackage(t, filepath.Join(t.TempDir(), "pkg-2"), map[string]string{
		"plugin.yaml":   webviewManifest("vendor.panel", "2.0.0", nil, true),
		"ui/index.html": "<html>2</html>",
		"backend.js":    "//",
	})
	harness.loader.mu.Lock()
	harness.loader.perVersion["2.0.0"] = errors.New("backend refused to start")
	harness.loader.mu.Unlock()
	if _, err := harness.manager.Update(ctx, "vendor.panel", UpdateRequest{Path: upgrade}, AuditContext{Actor: "tester"}); err == nil {
		t.Fatal("a failed update must report an error")
	}
	entry := harness.store.row(t, "vendor.panel")
	if entry.Version != "1.0.0" || entry.State != store.PluginStateEnabled || entry.ContentHash != firstHash {
		t.Fatalf("rollback row = %+v", entry)
	}
	if data, err := os.ReadFile(filepath.Join(root, "vendor.panel", "ui", "index.html")); err != nil || string(data) != "<html>1</html>" {
		t.Fatalf("rollback package = %q, %v", data, err)
	}
	if got := strings.Join(harness.tools.list(), ","); got != "vendor_panel_run" {
		t.Fatalf("tools after rollback = %q", got)
	}

	// A good update replaces the version, archives the old one and keeps it
	// enabled.
	harness.loader.mu.Lock()
	delete(harness.loader.perVersion, "2.0.0")
	harness.loader.mu.Unlock()
	view, err := harness.manager.Update(ctx, "vendor.panel", UpdateRequest{Path: upgrade}, AuditContext{Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if view.Version != "2.0.0" || view.State != LifecycleEnabled {
		t.Fatalf("updated view = %+v", view)
	}
	if data, err := os.ReadFile(filepath.Join(root, "vendor.panel", "ui", "index.html")); err != nil || string(data) != "<html>2</html>" {
		t.Fatalf("updated package = %q, %v", data, err)
	}
	archives, err := os.ReadDir(filepath.Join(root, ".versions", "vendor.panel"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("archived versions = %v, %v", archives, err)
	}
	actions := harness.store.auditActions(t)
	if !containsAction(actions, "update:failed") || !containsAction(actions, "update:ok") {
		t.Fatalf("audit = %v", actions)
	}

	// Updating without a path picks the highest other version under the root:
	// here there is none, so it is refused with a readable reason.
	if _, err := harness.manager.Update(ctx, "vendor.panel", UpdateRequest{}, AuditContext{Actor: "tester"}); err == nil {
		t.Fatal("update without a candidate version must fail")
	}
	// The multi-version layout is also honoured: dropping 3.0.0 next to the
	// managed directory lets update() with no path pick it.
	writePluginPackage(t, filepath.Join(root, "vendor.panel-3.0.0"), map[string]string{
		"plugin.yaml":   webviewManifest("vendor.panel", "3.0.0", nil, true),
		"ui/index.html": "<html>3</html>",
		"backend.js":    "//",
	})
	view, err = harness.manager.Update(ctx, "vendor.panel", UpdateRequest{}, AuditContext{Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if view.Version != "3.0.0" || view.State != LifecycleEnabled {
		t.Fatalf("discovered update view = %+v", view)
	}
	// Uninstall removes the plugin *and* the versions archived by updates.
	if _, err := harness.manager.Disable(ctx, "vendor.panel", AuditContext{Actor: "tester"}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.manager.Uninstall(ctx, "vendor.panel", AuditContext{Actor: "tester"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".versions", "vendor.panel")); !os.IsNotExist(err) {
		t.Fatal("archived versions must be removed with the plugin")
	}
}

func TestPluginManagerIsolatesInvalidPackages(t *testing.T) {
	root := t.TempDir()
	writePluginPackage(t, filepath.Join(root, "vendor.good"), map[string]string{
		"plugin.yaml":   webviewManifest("vendor.good", "1.0.0", nil, false),
		"ui/index.html": "<html></html>",
	})
	writePluginPackage(t, filepath.Join(root, "vendor.broken"), map[string]string{"plugin.yaml": "id: vendor.broken\nversion: nonsense\n"})
	writePluginPackage(t, filepath.Join(root, "vendor.gate"), map[string]string{
		"plugin.yaml":   "id: vendor.gate\nversion: 1.0.0\nkind: webview\ndescription: d\nengines:\n  keel: \">=9.0.0\"\nprovides:\n  ui:\n    entry: ui/index.html\n",
		"ui/index.html": "<html></html>",
	})
	harness := newManagerHarness(t, root, nil)
	if err := harness.manager.Boot(context.Background()); err != nil {
		t.Fatalf("a broken package must not fail the boot: %v", err)
	}
	byID := map[string]PluginView{}
	for _, view := range harness.manager.Snapshot().Plugins {
		byID[view.ID] = view
	}
	if byID["vendor.good"].State != LifecycleInstalled || byID["vendor.good"].Problem != nil {
		t.Fatalf("good = %+v", byID["vendor.good"])
	}
	broken := byID["vendor.broken"]
	if broken.State != LifecycleInvalid || broken.Problem == nil || !strings.Contains(broken.Problem.Reason, "version") {
		t.Fatalf("broken = %+v", broken)
	}
	// A well-formed package this host is too old for is not "invalid": it is
	// gate-blocked, and the reason is visible without failing anything else.
	gate := byID["vendor.gate"]
	if gate.State != LifecycleInstalled || gate.Problem != nil {
		t.Fatalf("gate = %+v", gate)
	}
	if len(gate.Diagnostics) == 0 || !strings.Contains(gate.Diagnostics[0], "version gate") {
		t.Fatalf("gate diagnostics = %+v", gate.Diagnostics)
	}
	// Enabling an invalid package is refused with its reason, and the version
	// gate is enforced on the enable path too.
	if _, err := harness.manager.Enable(context.Background(), "vendor.broken", AuditContext{Actor: "t"}); !errors.Is(err, ErrPluginInvalid) {
		t.Fatalf("enabling an invalid package must be refused: %v", err)
	}
	if _, err := harness.manager.Enable(context.Background(), "vendor.gate", AuditContext{Actor: "t"}); !errors.Is(err, ErrPluginGate) {
		t.Fatalf("the version gate must refuse enable: %v", err)
	}
}

func TestPluginManagerGateBlocksInstall(t *testing.T) {
	root := t.TempDir()
	harness := newManagerHarness(t, root, nil)
	if err := harness.manager.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	source := writePluginPackage(t, filepath.Join(t.TempDir(), "future"), map[string]string{
		"plugin.yaml":   "id: vendor.future\nversion: 1.0.0\nkind: webview\ndescription: d\nengines:\n  keel: \">=0.2.0 <0.3.0\"\nprovides:\n  ui:\n    entry: ui/index.html\n",
		"ui/index.html": "<html></html>",
	})
	_, err := harness.manager.Install(context.Background(), InstallRequest{Path: source}, AuditContext{Actor: "t"})
	if !errors.Is(err, ErrPluginGate) {
		t.Fatalf("install err = %v", err)
	}
	var pluginErr *PluginError
	if !errors.As(err, &pluginErr) || len(pluginErr.Fields) == 0 || pluginErr.Fields[0].Field != "engines.keel" {
		t.Fatalf("gate fields = %+v", pluginErr)
	}
	if _, statErr := os.Stat(pluginpkg.PackageDirFor(root, "vendor.future")); !os.IsNotExist(statErr) {
		t.Fatal("a refused install must not leave a package behind")
	}
}

func TestPluginManagerDisableSurvivesBoot(t *testing.T) {
	root := t.TempDir()
	var (
		mu     sync.Mutex
		events []string
	)
	harness := newManagerHarness(t, root, []Builtin{exampleBuiltin(true, &events, &mu)})
	ctx := context.Background()
	if err := harness.manager.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.manager.Disable(ctx, "keel.example", AuditContext{Actor: "tester"}); err != nil {
		t.Fatal(err)
	}
	// A restart on the same persistence must not re-enable it.
	second := newManagerHarnessWithStore(t, root, []Builtin{exampleBuiltin(true, &events, &mu)}, harness.store)
	if err := second.manager.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	view := second.manager.Snapshot().Plugins[0]
	if view.State != LifecycleDisabled || view.Runtime != nil {
		t.Fatalf("state after restart = %+v", view)
	}
	if got := second.tools.list(); len(got) != 0 {
		t.Fatalf("tools after restart = %v", got)
	}
	// And enabling works again, proving the persisted intent is not a dead end.
	if _, err := second.manager.Enable(ctx, "keel.example", AuditContext{Actor: "tester"}); err != nil {
		t.Fatal(err)
	}
	if got := second.tools.list(); len(got) != 1 {
		t.Fatalf("tools after re-enable = %v", got)
	}
}

func TestPluginManagerBundledPluginsAreNotUninstallable(t *testing.T) {
	var (
		mu     sync.Mutex
		events []string
	)
	harness := newManagerHarness(t, t.TempDir(), []Builtin{exampleBuiltin(true, &events, &mu)})
	ctx := context.Background()
	if err := harness.manager.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.manager.Disable(ctx, "keel.example", AuditContext{Actor: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.manager.Uninstall(ctx, "keel.example", AuditContext{Actor: "t"}); !errors.Is(err, ErrPluginBundled) {
		t.Fatalf("uninstall err = %v", err)
	}
	if _, err := harness.manager.Update(ctx, "keel.example", UpdateRequest{Path: t.TempDir()}, AuditContext{Actor: "t"}); !errors.Is(err, ErrPluginBundled) {
		t.Fatalf("update err = %v", err)
	}
	if _, err := harness.manager.Enable(ctx, "no.such.plugin", AuditContext{Actor: "t"}); !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("enable unknown err = %v", err)
	}
	if _, err := harness.manager.Disable(ctx, "keel.example", AuditContext{Actor: "t"}); !errors.Is(err, ErrPluginState) {
		t.Fatalf("disable twice err = %v", err)
	}
}

// newManagerHarnessWithStore reuses an existing persistence (a "restart").
func newManagerHarnessWithStore(t *testing.T, root string, builtins []Builtin, persistence PluginPersistence) *managerHarness {
	t.Helper()
	hooks := NewHookBus(HookBusOptions{Logger: testLogger(), Queue: 64, Workers: 1})
	triggers := NewTriggerEngine(TriggerEngineOptions{Starter: &countingStarter{}, Logger: testLogger()})
	recorder := &toolRecorder{}
	clock := &testClock{now: time.Date(2026, 11, 6, 13, 0, 0, 0, time.UTC)}
	runtime := NewRuntime(RuntimeOptions{Hooks: hooks, Triggers: triggers, Tools: recorder.sink, Logger: testLogger(), Now: clock.tick})
	loader := &fakeLoader{perVersion: map[string]error{}}
	manager, err := NewPluginManager(ManagerOptions{
		Root: root, Runtime: runtime, Persistence: persistence, Builtins: builtins,
		Loaders:     map[pluginpkg.Kind]PluginLoader{pluginpkg.KindWebview: loader},
		KeelVersion: "0.1.0", Logger: testLogger(), Now: clock.tick,
	})
	if err != nil {
		t.Fatal(err)
	}
	harness := &managerHarness{manager: manager, runtime: runtime, store: persistence.(*fakePluginStore), loader: loader, root: root, hooks: hooks, tools: recorder, clock: clock}
	t.Cleanup(func() {
		runtime.Close(context.Background())
		triggers.Close()
		hooks.Close()
	})
	return harness
}

func containsAction(actions []string, want string) bool {
	for _, action := range actions {
		if action == want {
			return true
		}
	}
	return false
}
