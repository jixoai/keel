package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandHookDenyBlocksToolExecution(t *testing.T) {
	reg, _ := newTestRegistry(t)
	cfg := helperCommandConfig(t, "guard", "deny")
	if _, err := reg.RegisterCommandHooks([]CommandHookConfig{cfg}); err != nil {
		t.Fatalf("register: %v", err)
	}

	tool := &testTool{name: "bash"}
	wrapped := WrapTool(reg, tool)
	_, err := wrapped.Execute(context.Background(), "call-1", json.RawMessage(`{"command":"rm -rf /"}`), nil)
	if !IsDenied(err) {
		t.Fatalf("error = %v, want denial", err)
	}
	if tool.callCount() != 0 {
		t.Fatalf("tool executed %d times", tool.callCount())
	}
}

func TestCommandHookAllow(t *testing.T) {
	reg, rec := newTestRegistry(t)
	cfg := helperCommandConfig(t, "allow", "allow")
	if _, err := reg.RegisterCommandHooks([]CommandHookConfig{cfg}); err != nil {
		t.Fatalf("register: %v", err)
	}
	decision := reg.EmitToolCall(context.Background(), &ToolCallEvent{ToolName: "bash", Input: json.RawMessage(`{}`)})
	if decision.Action.Normalized() != ActionAllow {
		t.Fatalf("decision = %+v", decision)
	}
	audits := rec.auditRecords()
	if len(audits) != 1 || audits[0].Hook != "allow" || audits[0].Decision != ActionAllow {
		t.Fatalf("audits = %+v", audits)
	}
}

func TestCommandHookModify(t *testing.T) {
	reg, _ := newTestRegistry(t)
	cfg := helperCommandConfig(t, "modify", "modify")
	if _, err := reg.RegisterCommandHooks([]CommandHookConfig{cfg}); err != nil {
		t.Fatalf("register: %v", err)
	}
	event := &ToolCallEvent{ToolName: "bash", Input: json.RawMessage(`{"x":"base"}`)}
	decision := reg.EmitToolCall(context.Background(), event)
	if decision.Action != ActionModify || string(decision.Input) != `{"x":"modified"}` {
		t.Fatalf("decision = %+v", decision)
	}
	if string(event.Input) != `{"x":"modified"}` {
		t.Fatalf("event input = %s", event.Input)
	}
}

func TestCommandHookTimeoutUsesFailurePolicy(t *testing.T) {
	t.Run("continue", func(t *testing.T) {
		reg, rec := newTestRegistry(t)
		cfg := helperCommandConfig(t, "slow", "sleep")
		cfg.Timeout = "50ms"
		cfg.FailurePolicy = FailureContinue
		if _, err := reg.RegisterCommandHooks([]CommandHookConfig{cfg}); err != nil {
			t.Fatalf("register: %v", err)
		}
		tool := &testTool{name: "bash"}
		wrapped := WrapTool(reg, tool)
		if _, err := wrapped.Execute(context.Background(), "call-1", json.RawMessage(`{}`), nil); err != nil {
			t.Fatalf("execute: %v", err)
		}
		if tool.callCount() != 1 {
			t.Fatalf("tool executed %d times", tool.callCount())
		}
		errs := rec.errors()
		if len(errs) != 1 {
			t.Fatalf("errors = %+v", errs)
		}
		var timeout *CommandTimeoutError
		if !errors.As(errs[0].Err, &timeout) {
			t.Fatalf("error = %v, want timeout", errs[0].Err)
		}
	})

	t.Run("deny", func(t *testing.T) {
		reg, rec := newTestRegistry(t)
		cfg := helperCommandConfig(t, "slow", "sleep")
		cfg.Timeout = "50ms"
		cfg.FailurePolicy = FailureDeny
		if _, err := reg.RegisterCommandHooks([]CommandHookConfig{cfg}); err != nil {
			t.Fatalf("register: %v", err)
		}
		tool := &testTool{name: "bash"}
		wrapped := WrapTool(reg, tool)
		_, err := wrapped.Execute(context.Background(), "call-1", json.RawMessage(`{}`), nil)
		if !IsDenied(err) {
			t.Fatalf("error = %v, want denial", err)
		}
		if tool.callCount() != 0 {
			t.Fatalf("tool executed %d times", tool.callCount())
		}
		if len(rec.errors()) != 1 {
			t.Fatalf("errors = %+v", rec.errors())
		}
	})
}

// Claude Code convention: exit code 2 is a blocking error, stderr is the
// reason.
func TestCommandHookExitTwoDeniesWithStderr(t *testing.T) {
	var stderr bytes.Buffer
	rec := &recorder{}
	reg := NewRegistry(WithAuditSink(rec), WithStderrWriter(&stderr))
	reg.OnError(rec.onError)

	cfg := helperCommandConfig(t, "guard", "stderr-deny")
	if _, err := reg.RegisterCommandHooks([]CommandHookConfig{cfg}); err != nil {
		t.Fatalf("register: %v", err)
	}
	decision := reg.EmitToolCall(context.Background(), &ToolCallEvent{ToolName: "bash", Input: json.RawMessage(`{}`)})
	if decision.Action != ActionDeny || !strings.Contains(decision.Reason, "dangerous command") {
		t.Fatalf("decision = %+v", decision)
	}
	if !strings.Contains(stderr.String(), "dangerous command") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestCommandHookStderrLoggedOnSuccess(t *testing.T) {
	var stderr bytes.Buffer
	rec := &recorder{}
	reg := NewRegistry(WithAuditSink(rec), WithStderrWriter(&stderr))
	cfg := helperCommandConfig(t, "inspector", "stderr-allow")
	if _, err := reg.RegisterCommandHooks([]CommandHookConfig{cfg}); err != nil {
		t.Fatalf("register: %v", err)
	}
	decision := reg.EmitToolCall(context.Background(), &ToolCallEvent{ToolName: "bash", Input: json.RawMessage(`{}`)})
	if decision.Action.Normalized() != ActionAllow {
		t.Fatalf("decision = %+v", decision)
	}
	if !strings.Contains(stderr.String(), "note: inspected") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestCommandHookNonZeroExitUsesFailurePolicy(t *testing.T) {
	t.Run("continue", func(t *testing.T) {
		reg, rec := newTestRegistry(t)
		cfg := helperCommandConfig(t, "broken", "exit1")
		cfg.FailurePolicy = FailureContinue
		if _, err := reg.RegisterCommandHooks([]CommandHookConfig{cfg}); err != nil {
			t.Fatalf("register: %v", err)
		}
		decision := reg.EmitToolCall(context.Background(), &ToolCallEvent{ToolName: "bash", Input: json.RawMessage(`{}`)})
		if decision.Action.Normalized() != ActionAllow {
			t.Fatalf("decision = %+v", decision)
		}
		if len(rec.errors()) != 1 {
			t.Fatalf("errors = %+v", rec.errors())
		}
	})
	t.Run("deny", func(t *testing.T) {
		reg, rec := newTestRegistry(t)
		cfg := helperCommandConfig(t, "broken", "exit1")
		cfg.FailurePolicy = FailureDeny
		if _, err := reg.RegisterCommandHooks([]CommandHookConfig{cfg}); err != nil {
			t.Fatalf("register: %v", err)
		}
		decision := reg.EmitToolCall(context.Background(), &ToolCallEvent{ToolName: "bash", Input: json.RawMessage(`{}`)})
		if decision.Action != ActionDeny {
			t.Fatalf("decision = %+v", decision)
		}
		if len(rec.errors()) != 1 {
			t.Fatalf("errors = %+v", rec.errors())
		}
	})
}

func TestCommandHookInvalidOutputUsesFailurePolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy FailurePolicy
		want   Action
	}{
		{name: "continue", policy: FailureContinue, want: ActionAllow},
		{name: "deny", policy: FailureDeny, want: ActionDeny},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg, _ := newTestRegistry(t)
			cfg := helperCommandConfig(t, "invalid", "invalid")
			cfg.FailurePolicy = tc.policy
			if _, err := reg.RegisterCommandHooks([]CommandHookConfig{cfg}); err != nil {
				t.Fatalf("register: %v", err)
			}
			decision := reg.EmitToolCall(context.Background(), &ToolCallEvent{ToolName: "bash", Input: json.RawMessage(`{}`)})
			if decision.Action.Normalized() != tc.want {
				t.Fatalf("decision = %+v, want %s", decision, tc.want)
			}
		})
	}
}

// The stdin envelope carries the PI event name and the event payload.
func TestCommandHookReadsEventEnvelope(t *testing.T) {
	reg, _ := newTestRegistry(t)
	cfg := helperCommandConfig(t, "echo", "echo-event")
	if _, err := reg.RegisterCommandHooks([]CommandHookConfig{cfg}); err != nil {
		t.Fatalf("register: %v", err)
	}
	decision := reg.EmitToolCall(context.Background(), &ToolCallEvent{
		ToolCallID: "42",
		ToolName:   "bash",
		Input:      json.RawMessage(`{"command":"ls"}`),
	})
	if decision.Action != ActionDeny || decision.Reason != "event=tool_call" {
		t.Fatalf("decision = %+v", decision)
	}
}

// Claude Code PreToolUse shape is accepted as an alias.
func TestCommandHookClaudeCodeUpdatedInput(t *testing.T) {
	reg, _ := newTestRegistry(t)
	cfg := helperCommandConfig(t, "cc", "cc-updated-input")
	if _, err := reg.RegisterCommandHooks([]CommandHookConfig{cfg}); err != nil {
		t.Fatalf("register: %v", err)
	}
	event := &ToolCallEvent{ToolName: "bash", Input: json.RawMessage(`{"x":"base"}`)}
	decision := reg.EmitToolCall(context.Background(), event)
	if decision.Action != ActionModify || string(decision.Input) != `{"x":"cc"}` {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestLoadCommandHooksFromConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	config := CommandHooksFile{Hooks: []CommandHookConfig{helperCommandConfig(t, "guard", "deny")}}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	configs, err := LoadCommandHooks(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(configs) != 1 || configs[0].Name != "guard" || configs[0].Event != EventToolCall {
		t.Fatalf("configs = %+v", configs)
	}
	reg, _ := newTestRegistry(t)
	if _, err := reg.RegisterCommandHooks(configs); err != nil {
		t.Fatalf("register: %v", err)
	}
	decision := reg.EmitToolCall(context.Background(), &ToolCallEvent{ToolName: "bash", Input: json.RawMessage(`{}`)})
	if decision.Action != ActionDeny {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestLoadCommandHooksRejectsInvalidConfig(t *testing.T) {
	cases := map[string]string{
		"unknown event": `{"hooks":[{"name":"x","event":"nope","command":["true"]}]}`,
		"no command":    `{"hooks":[{"name":"x","event":"tool_call","command":[]}]}`,
		"bad timeout":   `{"hooks":[{"name":"x","event":"tool_call","command":["true"],"timeout":"soon"}]}`,
		"bad policy":    `{"hooks":[{"name":"x","event":"tool_call","command":["true"],"failurePolicy":"maybe"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCommandHooks(strings.NewReader(body)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestRegisterCommandHooksRollsBackOnError(t *testing.T) {
	reg := NewRegistry()
	good := helperCommandConfig(t, "good", "allow")
	bad := helperCommandConfig(t, "bad", "allow")
	bad.Event = "nope"
	if _, err := reg.RegisterCommandHooks([]CommandHookConfig{good, bad}); err == nil {
		t.Fatal("expected an error")
	}
	if reg.HasHandlers(EventToolCall) {
		t.Fatal("partially registered command hooks were not rolled back")
	}
}

// Ensure the helper process never inherits a hook environment by accident in
// the parent test process.
func TestHelperEnvIsIsolated(t *testing.T) {
	if os.Getenv("GO_WANT_HOOK_HELPER") == "1" {
		t.Skip("helper process")
	}
	cfg := helperCommandConfig(t, "allow", "allow")
	if cfg.Env["GO_WANT_HOOK_HELPER"] != "1" {
		t.Fatal("helper env missing")
	}
}
