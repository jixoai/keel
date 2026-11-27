package harnessext

// Real-PostgreSQL coverage for the plugin command registry (change
// keel-plugin-commands, task 5.2): enable/disable/failed are reflected in the
// live catalog while the lifecycle intent is persisted in the store. Skipped
// without KEEL_TEST_DSN.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/pluginpkg"
	"github.com/jixoai/keel/internal/store"
)

func TestPluginCommandRegistryWithPostgresPersistence(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "cmd-registry-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM plugin_audit WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM plugins WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM tenants WHERE tenant_id = $1`, tenant)
	})

	hooks := NewHookBus(HookBusOptions{Logger: testLogger(), Queue: 16, Workers: 1})
	t.Cleanup(hooks.Close)
	runtime := NewRuntime(RuntimeOptions{Hooks: hooks, Logger: testLogger()})
	t.Cleanup(func() { runtime.Close(context.Background()) })
	manager, err := NewPluginManager(ManagerOptions{
		TenantID: tenant, Runtime: runtime, Persistence: st,
		Builtins:    []Builtin{commandBuiltin(false, nil), commandBuiltinFail(false)},
		KeelVersion: "0.1.0", Logger: testLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Boot(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.Enable(ctx, "keel.commands", AuditContext{TenantID: &tenant, Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if got := manager.ListCommands().Commands; len(got) != 1 || !got[0].Available {
		t.Fatalf("enabled commands = %+v", got)
	}
	if _, err := manager.Disable(ctx, "keel.commands", AuditContext{TenantID: &tenant, Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if got := manager.ListCommands().Commands; len(got) != 0 {
		t.Fatalf("disabled plugin still lists commands: %+v", got)
	}

	if _, err := manager.Enable(ctx, "keel.commands-failed", AuditContext{TenantID: &tenant, Actor: "test"}); err == nil {
		t.Fatal("enable of the broken plugin succeeded")
	}
	commands := manager.ListCommands().Commands
	if len(commands) != 1 || commands[0].PluginID != "keel.commands-failed" ||
		commands[0].Available || commands[0].Reason != CommandReasonPluginFailed {
		t.Fatalf("failed plugin commands = %+v", commands)
	}

	// A fresh manager over the same persisted intent sees the same surface.
	reopened, err := NewPluginManager(ManagerOptions{
		TenantID: tenant, Runtime: runtime, Persistence: st,
		Builtins:    []Builtin{commandBuiltin(false, nil), commandBuiltinFail(false)},
		KeelVersion: "0.1.0", Logger: testLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	if got := reopened.ListCommands().Commands; len(got) != 1 || got[0].PluginID != "keel.commands-failed" {
		t.Fatalf("reopened commands = %+v", got)
	}
}

// commandBuiltinFail is a second compiled-in plugin whose commands must report
// plugin_failed once its activation fails.
func commandBuiltinFail(defaultEnabled bool) Builtin {
	builtin := commandBuiltin(defaultEnabled, nil)
	builtin.Manifest.ID = "keel.commands-failed"
	builtin.Manifest.Provides.Tools = []string{"commands_failed_run"}
	builtin.Manifest.Provides.Commands = []pluginpkg.CommandDecl{{
		Name: "refresh", Summary: "Refresh.", Tool: "commands_failed_run", Danger: pluginpkg.DangerWrite,
	}}
	builtin.Factory = func() *Plugin {
		return &Plugin{
			ID: builtin.Manifest.ID, Version: builtin.Manifest.Version,
			Tools: []ToolSpec{{
				Name: "commands_failed_run", Description: "refresh", Schema: json.RawMessage(`{"type":"object"}`),
				Execute: func(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
					return agentcore.AgentToolResult{}, nil
				},
			}},
			Start: func(context.Context, *PluginEnv) error { return errCommandActivation },
		}
	}
	return builtin
}

var errCommandActivation = fmt.Errorf("activation failed")
