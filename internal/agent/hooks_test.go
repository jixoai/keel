package agent

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/hooks"
)

func TestHookDenyBlocksTool(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	ctx := context.Background()
	r, k := newTestRunner(t, st)
	registry := hooks.NewRegistry()
	registry.On(hooks.EventToolCall, func(context.Context, hooks.Event) (hooks.Decision, error) {
		return hooks.Deny("denied by test"), nil
	})
	r.Hooks = registry
	if _, err := st.AppendEvent(ctx, tenant, session, "user.message", map[string]string{"text": "echo hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err := execRun(t, k, r, tenant, session); err != nil {
		t.Fatal(err)
	}
	events := eventsOf(t, st, tenant, session)
	found := false
	for _, ev := range events {
		if ev.Kind == "tool.result" && strings.Contains(string(ev.Payload), "denied by test") {
			found = true
		}
	}
	if !found {
		t.Fatalf("denied tool result not persisted: %+v", events)
	}
}

func TestHookTimeoutDoesNotHangRun(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	st := testStore(t)
	tenant, session := newSession(t, st)
	ctx := context.Background()
	r, k := newTestRunner(t, st)
	registry := hooks.NewRegistry()
	if _, err := registry.RegisterCommandHooks([]hooks.CommandHookConfig{{
		Name:          "slow",
		Event:         hooks.EventToolCall,
		Command:       []string{"sh", "-c", "sleep 2"},
		Timeout:       "50ms",
		FailurePolicy: hooks.FailureContinue,
	}}); err != nil {
		t.Fatal(err)
	}
	r.Hooks = registry
	if _, err := st.AppendEvent(ctx, tenant, session, "user.message", map[string]string{"text": "echo hello"}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := execRun(t, k, r, tenant, session); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("hook timeout blocked the run for %s", elapsed)
	}
}

func TestHookSessionBeforeCompactCanCancel(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	ctx := context.Background()
	r, k := newTestRunner(t, st)
	registry := hooks.NewRegistry()
	registry.On(hooks.EventSessionBeforeCompact, func(context.Context, hooks.Event) (hooks.Decision, error) {
		return hooks.Deny("no compaction in this test"), nil
	})
	r.Hooks = registry
	r.Compaction = &CompactionOptions{
		ContextWindow: 1,
		Settings: compaction.CompactionSettings{
			Enabled:          true,
			ReserveTokens:    0,
			KeepRecentTokens: 1,
		},
	}
	if _, err := st.AppendEvent(ctx, tenant, session, "user.message", map[string]string{"text": "hello there, this should be over a one-token window"}); err != nil {
		t.Fatal(err)
	}
	if _, err := execRun(t, k, r, tenant, session); err != nil {
		t.Fatal(err)
	}
	events := eventsOf(t, st, tenant, session)
	if n := countKind(events, "compaction"); n != 0 {
		t.Fatalf("compaction ran despite deny hook: %d events", n)
	}
}

func TestHookBeforeToolCallModifiesInput(t *testing.T) {
	registry := hooks.NewRegistry()
	registry.On(hooks.EventToolCall, func(context.Context, hooks.Event) (hooks.Decision, error) {
		return hooks.ModifyInput(json.RawMessage(`{"x":"rewritten"}`)), nil
	})
	r := &Runner{Hooks: registry}
	decision := r.hookBeforeToolCall(nil)(context.Background(), agentcore.AgentToolCall{
		ID: "c1", Name: "echo", Arguments: json.RawMessage(`{"x":"original"}`),
	})
	if decision == nil || string(decision.UpdatedInput) != `{"x":"rewritten"}` {
		t.Fatalf("hook did not rewrite input: %+v", decision)
	}
}

func TestHookAfterToolCallPatchesContent(t *testing.T) {
	registry := hooks.NewRegistry()
	registry.On(hooks.EventToolResult, func(context.Context, hooks.Event) (hooks.Decision, error) {
		content := agentcore.ContentList{agentcore.NewTextContent("redacted")}
		return hooks.ModifyResult(hooks.ToolResultPatch{Content: &content}), nil
	})
	r := &Runner{Hooks: registry}
	out := r.hookAfterToolCall()(context.Background(), agentcore.AgentToolCall{ID: "c1", Name: "echo"},
		agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("secret")}}, false)
	if out == nil || out.Content == nil || ContentText(*out.Content) != "redacted" {
		t.Fatalf("hook did not patch result: %+v", out)
	}
}

func TestBeforeAgentStartRewritesPrompt(t *testing.T) {
	registry := hooks.NewRegistry()
	registry.On(hooks.EventBeforeAgentStart, func(context.Context, hooks.Event) (hooks.Decision, error) {
		return hooks.SetSystemPrompt("hooked prompt"), nil
	})
	r := &Runner{Hooks: registry, Logger: discardLog()}
	if got := r.applyBeforeAgentStart(context.Background(), "base prompt", "hello"); got != "hooked prompt" {
		t.Fatalf("prompt=%q", got)
	}
}
