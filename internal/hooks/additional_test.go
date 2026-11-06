package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
)

func TestWithErrorPolicyOverridesFailClosed(t *testing.T) {
	reg, rec := newTestRegistry(t)
	reg.On(EventToolCall, func(context.Context, Event) (Decision, error) {
		return Allow(), errors.New("optional guard failed")
	}, WithName("optional"), WithErrorPolicy(ErrorPolicyFailOpen))

	decision := reg.EmitToolCall(context.Background(), &ToolCallEvent{ToolName: "t", Input: json.RawMessage(`{}`)})
	if decision.Action.Normalized() != ActionAllow {
		t.Fatalf("decision = %+v", decision)
	}
	if len(rec.errors()) != 1 {
		t.Fatalf("errors = %+v", rec.errors())
	}
}

func TestDenyTerminateAndDeniedError(t *testing.T) {
	reg, _ := newTestRegistry(t)
	reg.On(EventToolCall, func(context.Context, Event) (Decision, error) {
		return DenyTerminate("stop the batch"), nil
	})
	tool := &testTool{name: "bash"}
	_, err := WrapTool(reg, tool).Execute(context.Background(), "1", json.RawMessage(`{}`), nil)
	var denied *DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(err.Error(), "stop the batch") {
		t.Fatalf("error text = %q", err.Error())
	}
	if denied.Result.Terminate == nil || !*denied.Result.Terminate {
		t.Fatalf("terminate not propagated: %+v", denied.Result)
	}
}

func TestBeforeAgentStartInPlaceSystemPromptMutation(t *testing.T) {
	reg, _ := newTestRegistry(t)
	reg.On(EventBeforeAgentStart, func(_ context.Context, event Event) (Decision, error) {
		start := event.(*BeforeAgentStartEvent)
		start.SystemPrompt += "\nin-place"
		return Allow(), nil
	})
	result := reg.EmitBeforeAgentStart(context.Background(), &BeforeAgentStartEvent{SystemPrompt: "base"})
	if result.SystemPrompt != "base\nin-place" {
		t.Fatalf("system prompt = %q", result.SystemPrompt)
	}
}

func TestBeforeAgentStartObserverCannotChangePrompt(t *testing.T) {
	reg, rec := newTestRegistry(t)
	reg.On(EventBeforeAgentStart, func(context.Context, Event) (Decision, error) {
		prompt := "observer"
		return Decision{Action: ActionModify, SystemPrompt: &prompt}, errors.New("observer failed")
	}, AsObserver())
	result := reg.EmitBeforeAgentStart(context.Background(), &BeforeAgentStartEvent{SystemPrompt: "base"})
	if result.SystemPrompt != "base" {
		t.Fatalf("observer changed the prompt: %q", result.SystemPrompt)
	}
	if len(rec.errors()) != 1 {
		t.Fatalf("errors = %+v", rec.errors())
	}
}

func TestSessionBeforeCompactFailClosedOnHandlerError(t *testing.T) {
	reg, _ := newTestRegistry(t)
	reg.On(EventSessionBeforeCompact, func(context.Context, Event) (Decision, error) {
		return Allow(), errors.New("summarizer unavailable")
	}, WithErrorPolicy(ErrorPolicyFailClosed))
	result := reg.EmitSessionBeforeCompact(context.Background(), &SessionBeforeCompactEvent{Reason: CompactReasonThreshold})
	if !result.Cancel {
		t.Fatalf("result = %+v", result)
	}
}

func TestToolResultObserverAndErrorAreIsolated(t *testing.T) {
	reg, rec := newTestRegistry(t)
	reg.On(EventToolResult, func(context.Context, Event) (Decision, error) {
		return Deny("observer"), errors.New("observer failed")
	}, WithName("observer"), AsObserver())
	reg.On(EventToolResult, func(context.Context, Event) (Decision, error) {
		return Allow(), errors.New("transformer failed")
	}, WithName("transformer"))
	isError := true
	reg.On(EventToolResult, func(context.Context, Event) (Decision, error) {
		return ModifyResult(ToolResultPatch{IsError: &isError}), nil
	}, WithName("last"))

	patched, modified := reg.EmitToolResult(context.Background(), &ToolResultEvent{
		ToolName: "t",
		Content:  agentcore.ContentList{agentcore.NewTextContent("base")},
	})
	if !modified || !patched.IsError {
		t.Fatalf("patched = %+v modified=%v", patched, modified)
	}
	if len(rec.errors()) != 2 {
		t.Fatalf("errors = %+v", rec.errors())
	}
}

func TestMessageEndHandlerErrorIsIsolated(t *testing.T) {
	reg, rec := newTestRegistry(t)
	reg.On(EventMessageEnd, func(context.Context, Event) (Decision, error) {
		return Allow(), errors.New("renderer failed")
	})
	final, modified := reg.EmitMessageEnd(context.Background(), MessageEndEvent{
		Message: textMessage(agentcore.RoleAssistant, "original"),
	})
	if modified || messageText(final) != "original" {
		t.Fatalf("final = %q modified=%v", messageText(final), modified)
	}
	if len(rec.errors()) != 1 {
		t.Fatalf("errors = %+v", rec.errors())
	}
}

func TestCommandHookStructuredOutputOnExitTwo(t *testing.T) {
	reg, _ := newTestRegistry(t)
	cfg := helperCommandConfig(t, "guard", "exit2-json")
	if _, err := reg.RegisterCommandHooks([]CommandHookConfig{cfg}); err != nil {
		t.Fatalf("register: %v", err)
	}
	decision := reg.EmitToolCall(context.Background(), &ToolCallEvent{ToolName: "bash", Input: json.RawMessage(`{}`)})
	if decision.Action != ActionDeny || decision.Reason != "json deny" {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestLoadCommandHooksBareArrayAndReadError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	body := `[{"name":"guard","event":"tool_call","command":["true"]}]`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	configs, err := LoadCommandHooks(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(configs) != 1 || configs[0].Name != "guard" {
		t.Fatalf("configs = %+v", configs)
	}
	if _, err := LoadCommandHooks(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("expected a read error")
	}
}

func TestWrapToolParentToolCallID(t *testing.T) {
	reg, _ := newTestRegistry(t)
	var start *ToolExecutionStartEvent
	reg.On(EventToolExecutionStart, func(_ context.Context, event Event) (Decision, error) {
		start = event.(*ToolExecutionStartEvent)
		return Allow(), nil
	})
	tool := &testTool{name: "nested"}
	wrapped := WrapToolWithOptions(reg, tool, ToolWrapOptions{
		EmitExecutionEvents: true,
		ParentToolCallID:    "parent/1",
	})
	if _, err := wrapped.Execute(context.Background(), "parent/1/1", json.RawMessage(`{}`), nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if start == nil || start.ParentToolCallID != "parent/1" {
		t.Fatalf("start event = %+v", start)
	}
}

func TestSetAuditSinkReplacesSink(t *testing.T) {
	first := &recorder{}
	second := &recorder{}
	reg := NewRegistry(WithAuditSink(first))
	reg.SetAuditSink(second)
	reg.On(EventAgentStart, func(context.Context, Event) (Decision, error) { return Allow(), nil })
	reg.Emit(context.Background(), &AgentStartEvent{})
	if len(first.auditRecords()) != 0 {
		t.Fatal("old sink received a record")
	}
	if len(second.auditRecords()) != 1 {
		t.Fatal("new sink did not receive the record")
	}
}

// PI applies result fields by presence, not by a separate action flag.
func TestModificationsApplyByPresence(t *testing.T) {
	t.Run("before_agent_start guidance", func(t *testing.T) {
		reg, _ := newTestRegistry(t)
		reg.On(EventBeforeAgentStart, func(context.Context, Event) (Decision, error) {
			return Decision{Guidance: []GuidanceMessage{{CustomType: "hint", Content: "hint"}}}, nil
		})
		result := reg.EmitBeforeAgentStart(context.Background(), &BeforeAgentStartEvent{SystemPrompt: "base"})
		if len(result.Guidance) != 1 || result.Guidance[0].CustomType != "hint" {
			t.Fatalf("guidance = %+v", result.Guidance)
		}
	})

	t.Run("tool_call input", func(t *testing.T) {
		reg, _ := newTestRegistry(t)
		reg.On(EventToolCall, func(context.Context, Event) (Decision, error) {
			return Decision{Input: json.RawMessage(`{"x":"presence"}`)}, nil
		})
		event := &ToolCallEvent{ToolName: "t", Input: json.RawMessage(`{}`)}
		decision := reg.EmitToolCall(context.Background(), event)
		if decision.Action != ActionModify || string(event.Input) != `{"x":"presence"}` {
			t.Fatalf("decision = %+v input = %s", decision, event.Input)
		}
	})
}

func TestBoundaryContinueCanBeSuppressedByLaterHandler(t *testing.T) {
	reg, _ := newTestRegistry(t)
	reg.On(EventAgentBeforeSettle, func(context.Context, Event) (Decision, error) {
		return ContinueBoundary(), nil
	})
	reg.On(EventAgentBeforeSettle, func(context.Context, Event) (Decision, error) {
		return SetContinue(false), nil
	})
	result := reg.EmitAgentBeforeSettle(context.Background(), AgentBeforeSettleEvent{
		BoundaryState: BoundaryState{Outcome: "completed"},
	}, nil)
	if result.Continue {
		t.Fatalf("continuation was not suppressed: %+v", result)
	}
}

func TestAsyncNotificationHookDoesNotBlockDispatch(t *testing.T) {
	reg, rec := newTestRegistry(t)
	started := make(chan struct{})
	release := make(chan struct{})
	reg.On(EventAgentEnd, func(context.Context, Event) (Decision, error) {
		close(started)
		<-release
		return Allow(), nil
	}, WithName("slow-observer"), WithAsync())

	begin := time.Now()
	reg.Emit(context.Background(), &AgentEndEvent{})
	if elapsed := time.Since(begin); elapsed > 100*time.Millisecond {
		t.Fatalf("async dispatch blocked for %s", elapsed)
	}
	<-started
	close(release)

	deadline := time.Now().Add(2 * time.Second)
	for len(rec.auditRecords()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	audits := rec.auditRecords()
	if len(audits) != 1 || audits[0].Hook != "slow-observer" {
		t.Fatalf("audits = %+v", audits)
	}
}

func TestAsyncRejectedForGateEvents(t *testing.T) {
	reg := NewRegistry()
	_, err := reg.Add(Registration{
		Event:   EventToolCall,
		Handler: func(context.Context, Event) (Decision, error) { return Allow(), nil },
		Async:   true,
	})
	if err == nil {
		t.Fatal("expected an error for an async gate hook")
	}
}

// PI: "calls error listeners when handler throws" (context event).
func TestContextHandlerErrorIsReportedAndDoesNotChangeMessages(t *testing.T) {
	reg, rec := newTestRegistry(t)
	handlerErr := errors.New("Handler error!")
	reg.On(EventContext, func(context.Context, Event) (Decision, error) {
		return Allow(), handlerErr
	}, WithName("throwing"))

	input := []agentcore.Message{textMessage(agentcore.RoleUser, "hi")}
	output := reg.EmitContext(context.Background(), input)
	if len(output) != 1 || messageText(output[0]) != "hi" {
		t.Fatalf("output = %+v", output)
	}
	errs := rec.errors()
	if len(errs) != 1 || errs[0].Event != EventContext || !errors.Is(errs[0].Err, handlerErr) {
		t.Fatalf("errors = %+v", errs)
	}
	audits := rec.auditRecords()
	if len(audits) != 1 || !strings.Contains(audits[0].Error, "Handler error!") {
		t.Fatalf("audits = %+v", audits)
	}
}
