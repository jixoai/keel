// MCP event dispatcher tests (P4.2–P4.4). The fake fleet implements the two
// seams the dispatcher reads (a server list and RefreshServer); the registry is
// a real one with a scripted document reader. The debounce timer is injected,
// so no test depends on wall-clock sleeps.

package mcphost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/mcp"
)

// eventFleet is a mutable fleet: RefreshServer installs the scripted tool list
// or returns the scripted error, mimicking mcp.Manager's keep-old-on-failure
// rule.
type eventFleet struct {
	mu           sync.Mutex
	servers      []*mcp.Server
	apps         []mcp.App
	refreshErr   map[string]error
	replacement  map[string][]mcp.Tool
	refreshCalls int
}

func (f *eventFleet) Servers() []*mcp.Server {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*mcp.Server(nil), f.servers...)
}

func (f *eventFleet) DeclaredApps() []mcp.App {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mcp.App(nil), f.apps...)
}
func (f *eventFleet) PromptSection() string { return "- fixture: one server" }
func (f *eventFleet) AppsPromptSection() string {
	return ""
}

func (f *eventFleet) RefreshServer(_ context.Context, name string) (mcp.ToolDiff, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshCalls++
	if err := f.refreshErr[name]; err != nil {
		// Keep the old catalogue and expose the failure on the server value.
		for _, server := range f.servers {
			if server.Entry.Name == name {
				server.CatalogueError = err.Error()
			}
		}
		return mcp.ToolDiff{}, err
	}
	for _, server := range f.servers {
		if server.Entry.Name != name {
			continue
		}
		next, scripted := f.replacement[name]
		if !scripted {
			return mcp.ToolDiff{}, nil
		}
		previous := make(map[string]bool, len(server.Tools))
		for _, tool := range server.Tools {
			previous[tool.Name] = true
		}
		changed := make(map[string]bool, len(next))
		for _, tool := range next {
			changed[tool.Name] = true
		}
		diff := mcp.ToolDiff{}
		for _, tool := range next {
			if !previous[tool.Name] {
				diff.Added = append(diff.Added, tool.Name)
			}
		}
		for _, tool := range server.Tools {
			if !changed[tool.Name] {
				diff.Removed = append(diff.Removed, tool.Name)
			}
		}
		server.Tools = append([]mcp.Tool(nil), next...)
		return diff, nil
	}
	return mcp.ToolDiff{}, fmt.Errorf("unknown server %q", name)
}

func (f *eventFleet) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshCalls
}

// eventFixture assembles the dispatcher over one connected server and a real
// registry (no apps unless the test declares one).
type eventFixture struct {
	events   *Events
	fleet    *eventFleet
	registry *Registry
	timers   chan time.Time
	changes  chan SurfaceChange
	auditor  *fakeAuditor
	logs     *bytes.Buffer
}

func newEventFixture(t *testing.T, options EventsOptions, apps []mcp.App, readApp func(context.Context, *mcp.Server, string) (mcp.AppResource, error)) *eventFixture {
	t.Helper()
	server := &mcp.Server{
		Entry:  mcp.ServerEntry{Name: "fixture", Config: map[string]any{"url": "https://mcp.example/mcp"}},
		Client: mcp.NewClient(mcp.ClientOptions{}),
		Tools:  []mcp.Tool{{Name: "old"}},
	}
	fleet := &eventFleet{
		servers:     []*mcp.Server{server},
		apps:        apps,
		refreshErr:  map[string]error{},
		replacement: map[string][]mcp.Tool{},
	}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	auditor := &fakeAuditor{}
	registry := newTestRegistry(t, Options{
		Logger:  logger,
		Auditor: auditor,
		ReadApp: readApp,
	})
	timers := make(chan time.Time, 4)
	changes := make(chan SurfaceChange, 8)
	options.Fleet = fleet
	options.Registry = registry
	options.Logger = logger
	options.Auditor = auditor
	options.After = func(time.Duration) <-chan time.Time { return timers }
	options.OnSurfaceChanged = func(change SurfaceChange) { changes <- change }
	events := NewEvents(options)
	return &eventFixture{
		events: events, fleet: fleet, registry: registry,
		timers: timers, changes: changes, auditor: auditor, logs: logs,
	}
}

func (f *eventFixture) fireDebounce(t *testing.T) {
	t.Helper()
	select {
	case f.timers <- time.Now():
	case <-time.After(time.Second):
		t.Fatal("no debounce timer was armed")
	}
}

func (f *eventFixture) awaitChange(t *testing.T) SurfaceChange {
	t.Helper()
	select {
	case change := <-f.changes:
		return change
	case <-time.After(2 * time.Second):
		t.Fatal("no surface change was published")
		return SurfaceChange{}
	}
}

func auditResults(auditor *fakeAuditor, action string) []string {
	var out []string
	for _, event := range auditor.snapshot() {
		if event.Action == action {
			out = append(out, event.Result)
		}
	}
	return out
}

// TestEventsTransientNotificationsNeverPublish: progress, logs, unknown
// methods and cancellations are not catalogue changes and produce no
// generation.
func TestEventsTransientNotificationsNeverPublish(t *testing.T) {
	fixture := newEventFixture(t, EventsOptions{}, nil, nil)
	for i := 0; i < 50; i++ {
		fixture.events.Notification(context.Background(), "fixture", "notifications/progress",
			json.RawMessage(fmt.Sprintf(`{"progressToken":"t","progress":%d}`, i)))
	}
	fixture.events.Notification(context.Background(), "fixture", "notifications/message",
		json.RawMessage(`{"level":"error","data":{"message":"boom"}}`))
	fixture.events.Notification(context.Background(), "fixture", "notifications/cancelled",
		json.RawMessage(`{"requestId":1}`))
	fixture.events.Notification(context.Background(), "fixture", "notifications/something/new", nil)
	fixture.events.Flush(context.Background())

	if fixture.registry.Generation() != 0 {
		t.Fatalf("generation = %d, want 0", fixture.registry.Generation())
	}
	if fixture.fleet.calls() != 0 {
		t.Fatalf("tool refreshes = %d, want 0", fixture.fleet.calls())
	}
	if _, published := fixture.registry.Surface(); published {
		t.Fatal("the registry published a surface for transient notifications")
	}
	select {
	case change := <-fixture.changes:
		t.Fatalf("unexpected surface change: %+v", change)
	default:
	}
	// The server log is a structured log row, not a catalogue or ledger fact.
	if !strings.Contains(fixture.logs.String(), "mcp server log") {
		t.Fatalf("server log missing: %s", fixture.logs.String())
	}
}

// TestEventsToolsListChangedDebouncesAndRepublishes pins the P4.4 contract: a
// burst collapses into one tools/list refresh, one generation and one hook.
func TestEventsToolsListChangedDebouncesAndRepublishes(t *testing.T) {
	fixture := newEventFixture(t, EventsOptions{}, nil, nil)
	fixture.fleet.replacement["fixture"] = []mcp.Tool{{Name: "old"}, {Name: "new"}}

	for i := 0; i < 20; i++ {
		fixture.events.Notification(context.Background(), "fixture", "notifications/tools/list_changed", nil)
	}
	if fixture.fleet.calls() != 0 {
		t.Fatalf("refresh started before the debounce window: %d", fixture.fleet.calls())
	}
	if fixture.registry.Generation() != 0 {
		t.Fatalf("generation moved before the debounce window: %d", fixture.registry.Generation())
	}
	fixture.fireDebounce(t)
	change := fixture.awaitChange(t)
	if fixture.fleet.calls() != 1 {
		t.Fatalf("tool refreshes = %d, want 1 (burst coalesced)", fixture.fleet.calls())
	}
	if change.Generation != 1 {
		t.Fatalf("generation = %d, want 1", change.Generation)
	}
	if len(change.Added) != 1 || change.Added[0] != "new" {
		t.Fatalf("added = %v, want [new]", change.Added)
	}
	if change.Servers[0] != "fixture" || change.Kinds[0] != "tools" {
		t.Fatalf("change = %+v", change)
	}
	// The audit trail carries the republish (and never a message body).
	results := auditResults(fixture.auditor, "catalogue.refresh")
	if len(results) != 1 || results[0] != "ok" {
		t.Fatalf("audit results = %v", results)
	}
}

// TestEventsPromptsAndResourcesDoNotRelistTools: only the tools category costs a
// tools/list; prompts/resources still republish the whole surface once.
func TestEventsPromptsAndResourcesDoNotRelistTools(t *testing.T) {
	fixture := newEventFixture(t, EventsOptions{}, nil, nil)
	fixture.events.Notification(context.Background(), "fixture", "notifications/prompts/list_changed", nil)
	fixture.events.Notification(context.Background(), "fixture", "notifications/resources/list_changed", nil)
	fixture.events.Flush(context.Background())
	if fixture.fleet.calls() != 0 {
		t.Fatalf("tools/list ran for a non-tools change: %d", fixture.fleet.calls())
	}
	if fixture.registry.Generation() != 1 {
		t.Fatalf("generation = %d, want 1", fixture.registry.Generation())
	}
}

// TestEventsRefreshFailureKeepsOldSurface: a failed recompute publishes the
// previous catalogue (same tools, same app hash), reports the error and audits
// it, and does not emit the change hook.
func TestEventsRefreshFailureKeepsOldSurface(t *testing.T) {
	html := "<html><body>v1</body></html>"
	readCalls := 0
	apps := []mcp.App{{
		AppID: mcp.AppIDFor("fixture", "ui://fixture/panel"), Server: "fixture", ResourceURI: "ui://fixture/panel",
		Tools: []mcp.AppTool{{Server: "fixture", Tool: "old", Visibility: []string{"model", "app"}}},
	}}
	fixture := newEventFixture(t, EventsOptions{}, apps, func(context.Context, *mcp.Server, string) (mcp.AppResource, error) {
		readCalls++
		return mcp.AppResource{URI: "ui://fixture/panel", MIMEType: mcp.AppMIMEType, Text: html}, nil
	})
	// First publish: one app, hash.
	if _, err := fixture.registry.Refresh(context.Background(), fixture.fleet); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}
	before, _ := fixture.registry.Surface()
	if len(before.Apps) != 1 {
		t.Fatalf("apps = %d", len(before.Apps))
	}
	fixture.fleet.mu.Lock()
	fixture.fleet.refreshErr["fixture"] = errors.New("connection reset")
	fixture.fleet.mu.Unlock()

	fixture.events.Notification(context.Background(), "fixture", "notifications/tools/list_changed", nil)
	fixture.events.Flush(context.Background())

	after, published := fixture.registry.Surface()
	if !published {
		t.Fatal("surface disappeared on a failed refresh")
	}
	if len(after.Apps) != 1 || after.Apps[0].Hash != before.Apps[0].Hash {
		t.Fatalf("app surface changed on a failed refresh: %+v", after.Apps)
	}
	servers := fixture.fleet.Servers()
	if len(servers[0].Tools) != 1 || servers[0].Tools[0].Name != "old" {
		t.Fatalf("tool catalogue changed on a failed refresh: %+v", servers[0].Tools)
	}
	if servers[0].CatalogueError == "" {
		t.Fatal("server projection does not carry the catalogue error")
	}
	// /v1/mcp/servers reads Surface.Servers: the same error must be there.
	surface, _ := fixture.registry.Surface()
	found := false
	for _, state := range surface.Servers {
		if state.Name == "fixture" {
			found = true
			if state.CatalogueError == "" {
				t.Fatal("Surface.ServerState.CatalogueError is empty after a failed refresh")
			}
			if state.Tools != 1 {
				t.Fatalf("server state tools = %d, want the old 1", state.Tools)
			}
		}
	}
	if !found {
		t.Fatal("the server disappeared from the published state")
	}
	if errors := fixture.events.Errors(); errors["fixture"] == "" {
		t.Fatalf("dispatcher errors = %v", errors)
	}
	if results := auditResults(fixture.auditor, "catalogue.refresh"); len(results) == 0 || results[len(results)-1] != "error" {
		t.Fatalf("audit results = %v", results)
	}
	select {
	case change := <-fixture.changes:
		t.Fatalf("failed refresh emitted a change hook: %+v", change)
	default:
	}
	if readCalls == 0 {
		t.Fatal("app document was never read")
	}
}

// TestEventsResourcesUpdatedInvalidatesTTLCache: a resources/updated frame
// forces the next republish to re-read a document whose TTL would otherwise
// reuse the cached version.
func TestEventsResourcesUpdatedInvalidatesTTLCache(t *testing.T) {
	version := 1
	var reads int
	apps := []mcp.App{{
		AppID: mcp.AppIDFor("fixture", "ui://fixture/panel"), Server: "fixture", ResourceURI: "ui://fixture/panel",
		Tools: []mcp.AppTool{{Server: "fixture", Tool: "old", Visibility: []string{"model", "app"}}},
	}}
	fixture := newEventFixture(t, EventsOptions{}, apps, func(context.Context, *mcp.Server, string) (mcp.AppResource, error) {
		reads++
		return mcp.AppResource{
			URI: "ui://fixture/panel", MIMEType: mcp.AppMIMEType,
			Text:  fmt.Sprintf("<html><body>v%d</body></html>", version),
			TTLMs: ptrInt64(3_600_000),
		}, nil
	})
	first, err := fixture.registry.Refresh(context.Background(), fixture.fleet)
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	// A TTL hit reuses the document without a read.
	if _, err := fixture.registry.Refresh(context.Background(), fixture.fleet); err != nil {
		t.Fatalf("TTL refresh: %v", err)
	}
	if reads != 1 {
		t.Fatalf("reads = %d, want 1 (TTL reuse)", reads)
	}
	version = 2
	fixture.events.Notification(context.Background(), "fixture", "notifications/resources/updated",
		json.RawMessage(`{"uri":"ui://fixture/panel"}`))
	fixture.events.Flush(context.Background())
	if reads != 2 {
		t.Fatalf("reads = %d, want 2 (invalidation bypassed the TTL)", reads)
	}
	updated, _ := fixture.registry.Surface()
	if len(updated.Apps) != 1 || updated.Apps[0].Hash == first.Apps[0].Hash {
		t.Fatalf("document hash did not change after resources/updated")
	}
}

// TestNotificationStormLeavesLedgerKindSetUnchanged: the dispatcher has no
// ledger seam at all, and the kernel's closed kind set does not gain a
// notification kind (spec mcp-events「账本零新增 kind」).
func TestNotificationStormLeavesLedgerKindSetUnchanged(t *testing.T) {
	before := make(map[kernel.EventKind]bool)
	for _, kind := range kernel.Kinds() {
		before[kind] = true
	}
	fixture := newEventFixture(t, EventsOptions{}, nil, nil)
	for i := 0; i < 100; i++ {
		fixture.events.Notification(context.Background(), "fixture", "notifications/progress",
			json.RawMessage(`{"progressToken":"t","progress":1}`))
		fixture.events.Notification(context.Background(), "fixture", "notifications/message",
			json.RawMessage(`{"level":"info","data":"line"}`))
		if i%10 == 0 {
			fixture.events.Notification(context.Background(), "fixture", "notifications/tools/list_changed", nil)
		}
	}
	fixture.events.Flush(context.Background())
	after := make(map[kernel.EventKind]bool)
	for _, kind := range kernel.Kinds() {
		after[kind] = true
		if strings.Contains(string(kind), "progress") || strings.Contains(string(kind), "mcp.") {
			t.Fatalf("a notification kind entered the ledger set: %s", kind)
		}
	}
	if len(after) != len(before) {
		t.Fatalf("ledger kind count changed: %d -> %d", len(before), len(after))
	}
	for kind := range before {
		if !after[kind] {
			t.Fatalf("ledger kind %s disappeared", kind)
		}
	}
}

func ptrInt64(value int64) *int64 { return &value }

// TestEventsServerLogLevelWhitelistAndBounds pins the notifications/message
// fields: a closed level set, bounded logger/data, and no full body.
func TestEventsServerLogLevelWhitelistAndBounds(t *testing.T) {
	fixture := newEventFixture(t, EventsOptions{}, nil, nil)
	long := strings.Repeat("x", 4096)
	fixture.events.Notification(context.Background(), "fixture", "notifications/message", json.RawMessage(fmt.Sprintf(
		`{"level":"error","logger":%q,"data":{"body":%q}}`, long, long)))
	fixture.events.Notification(context.Background(), "fixture", "notifications/message",
		json.RawMessage(`{"level":"verbose","logger":"l","data":"d"}`))

	type row map[string]any
	var rows []row
	for _, line := range strings.Split(strings.TrimSpace(fixture.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record row
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line is not JSON: %v (%s)", err, line)
		}
		if record["msg"] == "mcp server log" {
			rows = append(rows, record)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("server log rows = %d, want 2: %s", len(rows), fixture.logs.String())
	}
	if rows[0]["level"] != "ERROR" {
		t.Fatalf("level = %v, want ERROR", rows[0]["level"])
	}
	// Unknown levels collapse to info (the closed set).
	if rows[1]["level"] != "INFO" || rows[1]["mcp_level"] != "verbose" {
		t.Fatalf("unknown level row = %v", rows[1])
	}
	loggerName, _ := rows[0]["mcp_logger"].(string)
	if len(loggerName) > 64+len("…") {
		t.Fatalf("logger field is unbounded: %d bytes", len(loggerName))
	}
	data, _ := rows[0]["data"].(string)
	if len(data) > 256+len("…") {
		t.Fatalf("data field is unbounded: %d bytes", len(data))
	}
	if strings.Contains(data, long) {
		t.Fatal("the full server payload reached the log field")
	}
}
