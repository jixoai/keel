package fileplugin_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/fileplugin"
	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/pluginpkg/builtin"
	"github.com/jixoai/keel/internal/toolsurface"
)

// TestReadWriteEditThroughTheKeelFilePlugin is the wiring proof: the constant
// tools drive the plugin's internal handlers, the plugin gate is the manager,
// and disabling the plugin yields plugin_disabled without changing the surface.
func TestReadWriteEditThroughTheKeelFilePlugin(t *testing.T) {
	h := newHarness(t)
	manifest, ok := builtin.Manifest(harnessext.FilePluginID)
	if !ok {
		t.Fatal("keel.file manifest is not embedded")
	}
	runtime := harnessext.NewRuntime(harnessext.RuntimeOptions{})
	t.Cleanup(func() { runtime.Close(context.Background()) })
	manager, err := harnessext.NewPluginManager(harnessext.ManagerOptions{
		TenantID:    h.scope.TenantID,
		Runtime:     runtime,
		KeelVersion: "0.1.0",
		Builtins: []harnessext.Builtin{{
			Manifest:       manifest,
			DefaultEnabled: true,
			Factory: func() *harnessext.Plugin {
				return harnessext.NewFilePlugin(harnessext.FilePluginOptions{Service: h.service})
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	registry := map[string]agentcore.AgentTool{}
	for _, tool := range toolsurface.BuiltinTools(nil, toolsurface.Backends{
		Files: harnessext.FileBackend{Manager: func() *harnessext.Manager { return manager }},
	}) {
		registry[tool.Name()] = tool
	}
	ctx := harnessext.WithRunScope(context.Background(), harnessext.RunScope{
		TenantID: h.scope.TenantID, SessionID: h.scope.SessionID, RunID: "run-1",
	})

	write, err := registry[toolsurface.Write].Execute(ctx, "call-w1",
		json.RawMessage(`{"path":"a.txt","content":"hello\n"}`), nil)
	if err != nil || (write.IsError != nil && *write.IsError) {
		t.Fatalf("write = %#v err=%v", write, err)
	}
	var written fileplugin.WriteResult
	if err := json.Unmarshal([]byte(agentcore.ContentToText(write.Content)), &written); err != nil {
		t.Fatal(err)
	}
	if written.Path != "a.txt" || !written.Created {
		t.Fatalf("write result = %+v", written)
	}

	read, err := registry[toolsurface.Read].Execute(ctx, "call-r1",
		json.RawMessage(`{"path":"a.txt"}`), nil)
	if err != nil || (read.IsError != nil && *read.IsError) {
		t.Fatalf("read = %#v err=%v", read, err)
	}
	text := agentcore.ContentToText(read.Content)
	if !strings.Contains(text, `<file path="a.txt"`) || !strings.Contains(text, "hello") || !strings.Contains(text, `totalLines="1"`) {
		t.Fatalf("read text = %q", text)
	}
	structured, ok := read.StructuredContent.(fileplugin.ReadResult)
	if !ok || structured.Hash != written.Hash {
		t.Fatalf("read structured = %#v", read.StructuredContent)
	}

	edit, err := registry[toolsurface.Edit].Execute(ctx, "call-e1",
		json.RawMessage(`{"path":"a.txt","oldText":"hello","newText":"bye","expectedHash":"`+written.Hash+`"}`), nil)
	if err != nil || (edit.IsError != nil && *edit.IsError) {
		t.Fatalf("edit = %#v err=%v", edit, err)
	}
	if got := agentcore.ContentToText(edit.Content); !strings.Contains(got, `"replacements":1`) {
		t.Fatalf("edit result = %s", got)
	}

	// The surface never changes: disabling keel.file makes the actions answer
	// plugin_disabled, it does not remove read/write/edit.
	if _, err := manager.Disable(context.Background(), harnessext.FilePluginID, harnessext.AuditContext{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	disabled, err := registry[toolsurface.Write].Execute(ctx, "call-w2",
		json.RawMessage(`{"path":"b.txt","content":"x"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.IsError == nil || !*disabled.IsError {
		t.Fatalf("disabled write = %#v", disabled)
	}
	document, ok := disabled.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("disabled structured = %#v", disabled.StructuredContent)
	}
	problem := document["error"].(map[string]any)
	if problem["code"] != toolsurface.CodePluginDisabled {
		t.Fatalf("disabled code = %v", problem["code"])
	}
	for _, name := range []string{toolsurface.Read, toolsurface.Write, toolsurface.Edit} {
		if _, ok := registry[name]; !ok {
			t.Fatalf("%s disappeared from the surface", name)
		}
	}
}
