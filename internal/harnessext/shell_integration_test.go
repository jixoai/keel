package harnessext

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/bashbridge"
	"github.com/jixoai/keel/internal/blob"
	"github.com/jixoai/keel/internal/jobqueue"
	"github.com/jixoai/keel/internal/pluginpkg/builtin"
	"github.com/jixoai/keel/internal/store"
)

// TestShellPluginEndToEnd drives the exact production chain: the bash
// aggregate happens through the plugin manager's InvokeToolResult, resolves the
// keel.shell internal tool, and reaches the gbash bridge + JobManager. It is
// the test that would catch a tool-name normalization drift (the failure would
// otherwise only appear at runtime). Skipped without KEEL_TEST_DSN because the
// job facts are Postgres rows.
func TestShellPluginEndToEnd(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st, err := store.Open(ctx, store.Config{DSN: dsn, MaxConns: 4, MinConns: 1})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	tenant := uuid.New()
	jobManager, err := jobqueue.New(jobqueue.Options{
		Store:  st,
		Bridge: bashbridge.New(bashbridge.Config{}),
		Uploader: &blob.Uploader{
			Store:       st,
			CASDir:      t.TempDir(),
			TempDir:     t.TempDir(),
			PGThreshold: 1 << 20,
			MaxBytes:    64 << 20,
		},
		TenantID:      tenant,
		MaxGlobal:     4,
		MaxPerGroup:   2,
		SweepInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("job manager: %v", err)
	}
	if err := jobManager.Start(ctx); err != nil {
		t.Fatalf("start job manager: %v", err)
	}
	defer jobManager.Close()

	manifest, ok := builtin.Manifest(ShellPluginID)
	if !ok {
		t.Fatalf("no embedded manifest for %s", ShellPluginID)
	}
	hooks := NewHookBus(HookBusOptions{Logger: testLogger()})
	triggers := NewTriggerEngine(TriggerEngineOptions{Hooks: hooks, Logger: testLogger()})
	defer func() {
		triggers.Close()
		hooks.Close()
	}()
	runtime := NewRuntime(RuntimeOptions{Hooks: hooks, Triggers: triggers, Logger: testLogger()})
	defer runtime.Close(context.Background())
	manager, err := NewPluginManager(ManagerOptions{
		TenantID:    tenant,
		Root:        t.TempDir(),
		Runtime:     runtime,
		Persistence: newFakePluginStore(),
		Builtins: []Builtin{{
			Manifest: manifest,
			Factory: func() *Plugin {
				return NewShellPlugin(ShellOptions{
					Service:  func() *jobqueue.Manager { return jobManager },
					TenantID: tenant,
				})
			},
			DefaultEnabled: true,
		}},
		KeelVersion: "0.1.0",
		Logger:      testLogger(),
	})
	if err != nil {
		t.Fatalf("plugin manager: %v", err)
	}
	if err := manager.Boot(ctx); err != nil {
		t.Fatalf("boot: %v", err)
	}

	backend := ShellBackend{Manager: func() *Manager { return manager }}
	args := json.RawMessage(`{"action":"run","command":"echo end-to-end; printf 'err-line\n' >&2"}`)
	result, err := backend.Invoke(ctx, "run", args)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if result.IsError != nil && *result.IsError {
		t.Fatalf("bash run failed: %s", agentcore.ContentToText(result.Content))
	}
	document, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content = %#v", result.StructuredContent)
	}
	if document["status"] != "exited" {
		t.Fatalf("status = %v (%s)", document["status"], agentcore.ContentToText(result.Content))
	}
	if document["exitCode"] != 0 {
		t.Fatalf("exitCode = %v", document["exitCode"])
	}
	text := agentcore.ContentToText(result.Content)
	if !strings.Contains(text, "end-to-end") || !strings.Contains(text, "err-line") {
		t.Fatalf("result text = %q", text)
	}

	// Disabling the plugin must retract the backend: the aggregate tool stays,
	// but the plugin manager answers ErrPluginState (toolsurface maps that to
	// plugin_disabled).
	if _, err := manager.Disable(ctx, ShellPluginID, AuditContext{Actor: "test"}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := backend.Invoke(ctx, "run", args); !errors.Is(err, ErrPluginState) {
		t.Fatalf("disabled invoke error = %v, want ErrPluginState", err)
	}
}
