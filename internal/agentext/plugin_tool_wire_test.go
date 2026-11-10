package agentext

// Wire-safe publication names (change keel-plugin-core, spec: 发布名必须是 wire
// 安全的 `<a-zA-Z0-9_->{1,64}`).
//
// Two behaviours are pinned here, and they are the two the spec asks for:
//
//	one call, closed loop  a sidecar's local tool is published as a name a real
//	                       model can call, the model-facing registry carries it,
//	                       and a dispatch by that name resolves back to the local
//	                       tool and really executes it on the sidecar.
//	negative control       a package whose published name cannot satisfy the
//	                       wire grammar is refused at install with a field error
//	                       (fail-closed), instead of failing at call time.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/pluginpkg"
)

// TestPublishedToolNameClosedLoop drives the whole path with the real sidecar
// process: publish (`demo.sidecar` + `greet` → `demo_sidecar_greet`) → the
// model's tool registry → dispatch by the published name → the sidecar executes
// its local tool → the result comes back.
func TestPublishedToolNameClosedLoop(t *testing.T) {
	ctx := context.Background()
	t.Setenv("KEEL_TEST_SIDECAR_TOOLS", "greet")
	root := t.TempDir()
	harness := newSidecarHarness(t, root, newTestSidecarLoader(t, SidecarOptions{}))

	source := sidecarPackage(t, t.TempDir(),
		sidecarManifest(t, "demo.sidecar", "1.0.0", []string{"greet"}, []string{"greet"})).Dir
	view, err := harness.manager.Install(ctx, InstallRequest{Path: source}, AuditContext{Actor: "test"})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if view.State != LifecycleInstalled {
		t.Fatalf("installed view = %+v", view)
	}
	view, err = harness.manager.Enable(ctx, "demo.sidecar", AuditContext{Actor: "test"})
	if err != nil {
		t.Fatalf("enable: %v", err)
	}

	// 1. Published: the model's surface carries the wire-safe name, and the old
	// dotted spelling never appears (it is not callable).
	surface := harness.tools.list()
	const published = "demo_sidecar_greet"
	if !contains(surface, published) {
		t.Fatalf("model tool surface = %v, want %q", surface, published)
	}
	if contains(surface, "demo.sidecar.greet") {
		t.Fatalf("the dotted name reached the model: %v", surface)
	}
	if !pluginpkg.ValidPublishedToolName(published) {
		t.Fatalf("%q is not wire safe", published)
	}
	if !contains(view.Tools, published) {
		t.Fatalf("view tools = %v, want the published name", view.Tools)
	}
	// 2. Visible in the manifest projection too: declared ∪ published.
	if !contains(view.Provides.Tools, "greet") || !contains(view.Provides.Tools, published) {
		t.Fatalf("view provides.tools = %v, want the declared and published names", view.Provides.Tools)
	}

	// 3. Dispatch by the published name resolves back to the local tool and
	// executes it. The fixture sidecar echoes the tool it received, so the
	// assertion proves the LOCAL name crossed the wire, not the published one.
	raw, err := harness.manager.InvokePluginTool(ctx, "demo.sidecar", published, json.RawMessage(`{"who":"world"}`))
	if err != nil {
		t.Fatalf("invoke by published name: %v", err)
	}
	if !strings.Contains(string(raw), "answered greet") || !strings.Contains(string(raw), "world") {
		t.Fatalf("result = %s", raw)
	}

	// 4. The declared spellings still resolve (the host never stopped accepting
	// them), so an existing client cannot be broken by the publication name.
	for _, spelling := range []string{"greet", "demo.sidecar.greet"} {
		raw, err := harness.manager.InvokePluginTool(ctx, "demo.sidecar", spelling, json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("invoke by declared spelling %q: %v", spelling, err)
		}
		if !strings.Contains(string(raw), "answered greet") {
			t.Fatalf("result for %q = %s", spelling, raw)
		}
	}

	// 5. Disable withdraws the published name from the surface.
	if _, err := harness.manager.Disable(ctx, "demo.sidecar", AuditContext{Actor: "test"}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if contains(harness.tools.list(), published) {
		t.Fatalf("the disabled plugin's tool is still published: %v", harness.tools.list())
	}
}

// TestInstallRefusesUnpublishableToolName is the negative control: a package
// whose tool would publish a name outside `^[A-Za-z0-9_-]{1,64}$` is refused at
// install with a field-level error, and nothing of it is adopted (no row, no
// managed directory).
func TestInstallRefusesUnpublishableToolName(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	harness := newSidecarHarness(t, root, newTestSidecarLoader(t, SidecarOptions{}))

	// 60-character id + 7-character tool = 68 characters once the host joins
	// `<id>.<tool>`; normalization cannot shorten that, so install must refuse.
	id := "a" + strings.Repeat("b", 59)
	source := writePluginPackage(t, t.TempDir(), map[string]string{
		"plugin.yaml": "id: " + id + "\nversion: 1.0.0\nkind: sidecar\ndescription: over long\n" +
			"provides:\n  tools:\n    - toolong\n" +
			"entry:\n  command: [node, backend.mjs]\n",
	})

	_, err := harness.manager.Install(ctx, InstallRequest{Path: source}, AuditContext{Actor: "test"})
	if err == nil {
		t.Fatal("an unpublishable tool name was installed")
	}
	if !errors.Is(err, ErrPluginInvalid) {
		t.Fatalf("error = %v, want ErrPluginInvalid", err)
	}
	var pluginErr *PluginError
	if !errors.As(err, &pluginErr) {
		t.Fatalf("error = %T, want *PluginError", err)
	}
	if len(pluginErr.Fields) == 0 || pluginErr.Fields[0].Field != "provides.tools[0]" {
		t.Fatalf("fields = %+v, want the published-name field error", pluginErr.Fields)
	}
	if !strings.Contains(pluginErr.Fields[0].Message, "must match") {
		t.Fatalf("field message = %q", pluginErr.Fields[0].Message)
	}
	for _, view := range harness.manager.Snapshot().Plugins {
		if view.ID == id {
			t.Fatalf("the refused package is in the inventory: %+v", view)
		}
	}
	if _, statErr := os.Stat(filepath.Join(root, id)); !os.IsNotExist(statErr) {
		t.Fatalf("the refused package reached the managed root: %v", statErr)
	}
	if containsAction(harness.store.auditActions(t), "install:ok") {
		t.Fatalf("the refused install was audited as a success: %v", harness.store.auditActions(t))
	}
}

// TestRuntimePublicationNormalizesOnlyNonCompliantNames pins the compatibility
// red line at the assembly level: an already wire-safe plugin tool keeps its
// exact name, and only a name carrying punctuation is rewritten.
func TestRuntimePublicationNormalizesOnlyNonCompliantNames(t *testing.T) {
	recorder := &toolRecorder{}
	runtime := NewRuntime(RuntimeOptions{Tools: recorder.sink, Logger: testLogger()})
	t.Cleanup(func() { runtime.Close(context.Background()) })
	plugin := &Plugin{
		ID: "compat.probe",
		Tools: []ToolSpec{
			{Name: "example_status", Schema: json.RawMessage(`{"type":"object"}`)},
			{Name: "spawn_subagent", Schema: json.RawMessage(`{"type":"object"}`)},
			{Name: "demo.tool:v1", Schema: json.RawMessage(`{"type":"object"}`)},
		},
	}
	if err := runtime.ApplyPlugins(context.Background(), []*Plugin{plugin}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	want := []string{"example_status", "spawn_subagent", "demo_tool_v1"}
	got := recorder.list()
	if len(got) != len(want) {
		t.Fatalf("published tools = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("published tools = %v, want %v", got, want)
		}
	}
	// The caller's plugin value is untouched: publication copies, it does not
	// rewrite the declarative surface.
	if plugin.Tools[2].Name != "demo.tool:v1" {
		t.Fatalf("publication mutated the plugin's tool name: %q", plugin.Tools[2].Name)
	}
	snapshot := runtime.Snapshot()
	if !contains(snapshot.Tools, "example_status") || !contains(snapshot.Tools, "demo_tool_v1") {
		t.Fatalf("snapshot tools = %v", snapshot.Tools)
	}
}
