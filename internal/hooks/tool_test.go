package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
)

func TestWrapToolEventAndHookOrder(t *testing.T) {
	reg, _ := newTestRegistry(t)
	var log eventLog
	reg.On(EventToolExecutionStart, func(context.Context, Event) (Decision, error) {
		log.add("execution_start")
		return Allow(), nil
	})
	reg.On(EventToolCall, func(context.Context, Event) (Decision, error) {
		log.add("tool_call")
		return Allow(), nil
	})
	reg.On(EventToolExecutionUpdate, func(context.Context, Event) (Decision, error) {
		log.add("execution_update")
		return Allow(), nil
	})
	reg.On(EventToolResult, func(context.Context, Event) (Decision, error) {
		log.add("tool_result")
		return Allow(), nil
	})
	reg.On(EventToolExecutionEnd, func(context.Context, Event) (Decision, error) {
		log.add("execution_end")
		return Allow(), nil
	})

	tool := &testTool{
		name: "demo",
		fn: func(_ context.Context, _ string, _ json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
			onUpdate(agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("partial")}})
			return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("done")}}, nil
		},
	}
	wrapped := WrapTool(reg, tool)
	result, err := wrapped.Execute(context.Background(), "call-1", json.RawMessage(`{"x":1}`), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if agentcore.ContentToText(result.Content) != "done" {
		t.Fatalf("result = %+v", result)
	}
	assertStrings(t, log.snapshot(), []string{
		"execution_start",
		"tool_call",
		"execution_update",
		"tool_result",
		"execution_end",
	})
}

func TestWrapToolDenyBlocksExecution(t *testing.T) {
	reg, rec := newTestRegistry(t)
	var endEvent *ToolExecutionEndEvent
	reg.On(EventToolCall, func(context.Context, Event) (Decision, error) {
		return Deny("dangerous command"), nil
	}, WithName("guard"))
	reg.On(EventToolExecutionEnd, func(_ context.Context, event Event) (Decision, error) {
		endEvent = event.(*ToolExecutionEndEvent)
		return Allow(), nil
	})

	tool := &testTool{name: "bash"}
	wrapped := WrapTool(reg, tool)
	result, err := wrapped.Execute(context.Background(), "call-1", json.RawMessage(`{"command":"rm -rf /"}`), nil)
	if !IsDenied(err) {
		t.Fatalf("error = %v, want denial", err)
	}
	var denied *DeniedError
	if !errors.As(err, &denied) || denied.Reason != "dangerous command" {
		t.Fatalf("denied = %+v", denied)
	}
	if tool.callCount() != 0 {
		t.Fatalf("tool executed %d times", tool.callCount())
	}
	if agentcore.ContentToText(result.Content) != "dangerous command" {
		t.Fatalf("result = %+v", result)
	}
	if endEvent == nil || !endEvent.IsError || endEvent.DurationMs != nil {
		t.Fatalf("end event = %+v", endEvent)
	}
	audits := rec.auditRecords()
	if len(audits) != 2 {
		t.Fatalf("audits = %+v", audits)
	}
	if audits[0].Decision != ActionDeny || audits[0].Hook != "guard" {
		t.Fatalf("audit[0] = %+v", audits[0])
	}
}

func TestWrapToolModifyInputIsUsed(t *testing.T) {
	reg, _ := newTestRegistry(t)
	reg.On(EventToolCall, func(_ context.Context, event Event) (Decision, error) {
		call := event.(*ToolCallEvent)
		if string(call.Input) != `{"x":"base"}` {
			t.Fatalf("handler saw %s", call.Input)
		}
		return ModifyInput(json.RawMessage(`{"x":"modified"}`)), nil
	})

	tool := &testTool{name: "demo"}
	wrapped := WrapTool(reg, tool)
	if _, err := wrapped.Execute(context.Background(), "call-1", json.RawMessage(`{"x":"base"}`), nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if string(tool.args()) != `{"x":"modified"}` {
		t.Fatalf("tool args = %s", tool.args())
	}
}

func TestWrapToolResultPatchIsApplied(t *testing.T) {
	reg, _ := newTestRegistry(t)
	reg.On(EventToolResult, func(_ context.Context, event Event) (Decision, error) {
		result := event.(*ToolResultEvent)
		if agentcore.ContentToText(result.Content) != "secret" {
			t.Fatalf("handler saw %q", agentcore.ContentToText(result.Content))
		}
		content := agentcore.ContentList{agentcore.NewTextContent("[redacted]")}
		isError := false
		return ModifyResult(ToolResultPatch{Content: &content, IsError: &isError}), nil
	})

	tool := &testTool{
		name: "demo",
		fn: func(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
			return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("secret")}}, nil
		},
	}
	wrapped := WrapTool(reg, tool)
	result, err := wrapped.Execute(context.Background(), "call-1", json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if agentcore.ContentToText(result.Content) != "[redacted]" {
		t.Fatalf("result = %+v", result)
	}
}

func TestWrapToolForwardsUpdates(t *testing.T) {
	reg, _ := newTestRegistry(t)
	var observed []string
	reg.On(EventToolExecutionUpdate, func(_ context.Context, event Event) (Decision, error) {
		update := event.(*ToolExecutionUpdateEvent)
		observed = append(observed, agentcore.ContentToText(update.PartialResult.Content))
		return Allow(), nil
	})

	tool := &testTool{
		name: "demo",
		fn: func(_ context.Context, _ string, _ json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
			onUpdate(agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("step 1")}})
			onUpdate(agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("step 2")}})
			return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("done")}}, nil
		},
	}
	wrapped := WrapTool(reg, tool)
	var hostUpdates []string
	_, err := wrapped.Execute(context.Background(), "call-1", json.RawMessage(`{}`), func(partial agentcore.AgentToolResult) {
		hostUpdates = append(hostUpdates, agentcore.ContentToText(partial.Content))
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	assertStrings(t, observed, []string{"step 1", "step 2"})
	assertStrings(t, hostUpdates, []string{"step 1", "step 2"})
}

func TestWrapToolPreservesToolIdentity(t *testing.T) {
	reg := NewRegistry()
	tool := &testTool{
		name:        "demo",
		description: "demo tool",
		schema:      json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
		mode:        agentcore.ToolExecutionSequential,
	}
	wrapped := WrapTool(reg, tool)
	if wrapped.Name() != tool.Name() || wrapped.Description() != tool.Description() {
		t.Fatalf("identity changed: %s/%s", wrapped.Name(), wrapped.Description())
	}
	if string(wrapped.Schema()) != string(tool.Schema()) {
		t.Fatalf("schema changed: %s", wrapped.Schema())
	}
	if wrapped.ExecutionMode() != agentcore.ToolExecutionSequential {
		t.Fatalf("execution mode = %q", wrapped.ExecutionMode())
	}
}

func TestWrapToolCanSkipExecutionEvents(t *testing.T) {
	reg, _ := newTestRegistry(t)
	emitted := false
	reg.On(EventToolExecutionStart, func(context.Context, Event) (Decision, error) {
		emitted = true
		return Allow(), nil
	})
	tool := &testTool{name: "demo"}
	wrapped := WrapToolWithOptions(reg, tool, ToolWrapOptions{EmitExecutionEvents: false})
	if _, err := wrapped.Execute(context.Background(), "call-1", json.RawMessage(`{}`), nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if emitted {
		t.Fatal("execution event emitted when disabled")
	}
}

func TestWrapToolFailureStillRunsToolResult(t *testing.T) {
	reg, _ := newTestRegistry(t)
	sawError := false
	reg.On(EventToolResult, func(_ context.Context, event Event) (Decision, error) {
		result := event.(*ToolResultEvent)
		sawError = result.IsError
		return Allow(), nil
	})
	tool := &testTool{
		name: "demo",
		fn: func(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
			return agentcore.AgentToolResult{}, errors.New("boom")
		},
	}
	wrapped := WrapTool(reg, tool)
	_, err := wrapped.Execute(context.Background(), "call-1", json.RawMessage(`{}`), nil)
	if err == nil || err.Error() != "boom" {
		t.Fatalf("error = %v", err)
	}
	if !sawError {
		t.Fatal("tool_result did not observe the error result")
	}
}
