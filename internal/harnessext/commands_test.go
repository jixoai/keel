package harnessext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/pluginpkg"
)

// commandBuiltin is a compiled-in plugin with one command and its backend tool.
func commandBuiltin(defaultEnabled bool, startErr error) Builtin {
	manifest := &pluginpkg.Manifest{
		ID:          "keel.commands",
		Version:     "1.0.0",
		Kind:        pluginpkg.KindInProcess,
		Description: "command provider",
		Provides: pluginpkg.Provides{
			Tools: []string{"commands_refresh"},
			Commands: []pluginpkg.CommandDecl{{
				Name:       "refresh",
				Summary:    "Refresh the thing.",
				Tool:       "commands_refresh",
				Danger:     pluginpkg.DangerWrite,
				Idempotent: true,
			}},
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
					Name:        "commands_refresh",
					Description: "refresh",
					Schema:      json.RawMessage(`{"type":"object"}`),
					Execute: func(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
						return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("ok")}}, nil
					},
				}},
				Start: func(context.Context, *PluginEnv) error { return startErr },
			}
		},
	}
}

func TestManagerListCommandsFollowsLifecycle(t *testing.T) {
	harness := newManagerHarness(t, "", []Builtin{commandBuiltin(true, nil)})
	if err := harness.manager.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	registry := harness.manager.ListCommands()
	if registry.Generation != harness.runtime.Generation() {
		t.Fatalf("generation = %d, runtime = %d", registry.Generation, harness.runtime.Generation())
	}
	if len(registry.Commands) != 1 {
		t.Fatalf("commands = %+v", registry.Commands)
	}
	command := registry.Commands[0]
	if command.Published != "keel.commands:refresh" || command.Tool != "commands_refresh" ||
		command.PublishedTool != "commands_refresh" || !command.Available || command.Reason != "" {
		t.Fatalf("command = %+v", command)
	}
	if command.Decl.Name != "refresh" || command.Decl.Danger != pluginpkg.DangerWrite || !command.Decl.Idempotent {
		t.Fatalf("declaration = %+v", command.Decl)
	}

	if _, err := harness.manager.Disable(context.Background(), "keel.commands", AuditContext{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if got := harness.manager.ListCommands().Commands; len(got) != 0 {
		t.Fatalf("disabled plugin still lists commands: %+v", got)
	}

	if _, err := harness.manager.Enable(context.Background(), "keel.commands", AuditContext{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, found := harness.manager.FindCommand("keel.commands:refresh"); !found {
		t.Fatal("FindCommand did not resolve the re-enabled command")
	}
	if _, found := harness.manager.FindCommand("keel.commands:missing"); found {
		t.Fatal("FindCommand resolved an unknown command")
	}
}

func TestManagerListCommandsReportsFailedPlugin(t *testing.T) {
	harness := newManagerHarness(t, "", []Builtin{commandBuiltin(false, errors.New("activation exploded"))})
	if err := harness.manager.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.manager.Enable(context.Background(), "keel.commands", AuditContext{Actor: "test"}); err == nil {
		t.Fatal("enable of a broken plugin succeeded")
	}
	view := harness.manager.Snapshot().Plugins[0]
	if view.ID != "keel.commands" || view.State != LifecycleFailed {
		t.Fatalf("view = %+v, want failed", view)
	}
	registry := harness.manager.ListCommands()
	if len(registry.Commands) != 1 {
		t.Fatalf("commands = %+v", registry.Commands)
	}
	if command := registry.Commands[0]; command.Available || command.Reason != CommandReasonPluginFailed {
		t.Fatalf("failed plugin command = %+v", command)
	}
}

func TestManagerListCommandsInstalledPackage(t *testing.T) {
	root := t.TempDir()
	harness := newManagerHarness(t, root, nil)
	manifest := fmt.Sprintf(`id: vendor.commands
version: 1.0.0
kind: webview
description: command package
provides:
  tools: [vendor.commands.run]
  commands:
    - name: refresh
      summary: Refresh from the package.
      tool: vendor.commands.run
      danger: write
      targets: [web, native]
  ui:
    entry: ui/index.html
entry:
  command: [node, backend.js]
`)
	dir := writePluginPackage(t, root+"/pkg", map[string]string{"plugin.yaml": manifest, "ui/index.html": "<html></html>"})
	if _, err := harness.manager.Install(context.Background(), InstallRequest{Path: dir}, AuditContext{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	// Installed but not enabled: no commands.
	if got := harness.manager.ListCommands().Commands; len(got) != 0 {
		t.Fatalf("installed plugin lists commands: %+v", got)
	}
	if _, err := harness.manager.Enable(context.Background(), "vendor.commands", AuditContext{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	registry := harness.manager.ListCommands()
	if len(registry.Commands) != 1 {
		t.Fatalf("commands = %+v", registry.Commands)
	}
	command := registry.Commands[0]
	if command.Published != "vendor.commands:refresh" || command.Tool != "run" ||
		command.PublishedTool != "vendor_commands_run" || len(command.Decl.EffectiveTargets()) != 2 {
		t.Fatalf("command = %+v", command)
	}
	if _, err := harness.manager.Disable(context.Background(), "vendor.commands", AuditContext{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.manager.Uninstall(context.Background(), "vendor.commands", AuditContext{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if got := harness.manager.ListCommands().Commands; len(got) != 0 {
		t.Fatalf("uninstalled plugin lists commands: %+v", got)
	}
}

func TestManagerListCommandsFollowsUpdate(t *testing.T) {
	root := t.TempDir()
	harness := newManagerHarness(t, root, nil)
	manifest := func(version, commands string) string {
		return fmt.Sprintf(`id: vendor.commands
version: %s
kind: webview
description: command package
provides:
  tools: [vendor.commands.run]
  commands:
%s
  ui:
    entry: ui/index.html
entry:
  command: [node, backend.js]
`, version, commands)
	}
	firstDir := writePluginPackage(t, root+"/pkg-v1", map[string]string{
		"plugin.yaml":   manifest("1.0.0", "    - name: refresh\n      summary: Refresh.\n      tool: vendor.commands.run\n      danger: write\n"),
		"ui/index.html": "<html></html>",
	})
	if _, err := harness.manager.Install(context.Background(), InstallRequest{Path: firstDir}, AuditContext{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.manager.Enable(context.Background(), "vendor.commands", AuditContext{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	secondDir := writePluginPackage(t, root+"/pkg-v2", map[string]string{
		"plugin.yaml":   manifest("1.0.1", "    - name: refresh\n      summary: Refresh.\n      tool: vendor.commands.run\n      danger: write\n    - name: prune\n      summary: Prune.\n      tool: vendor.commands.run\n      danger: destructive\n"),
		"ui/index.html": "<html></html>",
	})
	if _, err := harness.manager.Update(context.Background(), "vendor.commands", UpdateRequest{Path: secondDir}, AuditContext{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	registry := harness.manager.ListCommands()
	if len(registry.Commands) != 2 {
		t.Fatalf("updated commands = %+v", registry.Commands)
	}
	if registry.Commands[0].Published != "vendor.commands:refresh" || registry.Commands[1].Published != "vendor.commands:prune" {
		t.Fatalf("updated commands = %+v", registry.Commands)
	}
}
