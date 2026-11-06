// Conformance tests for pi's parallel tool preflight and queue message
// lifecycle (packages/agent/src/agent-loop.ts, pin 55a5c69).

package loop

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
)

// PI: executeToolCallsParallel emits tool_execution_start sequentially in
// assistant source order during preflight, before any execution begins.
func TestConformanceParallelToolExecutionStartSourceOrder(t *testing.T) {
	tool := funcTool{name: "echo", schema: echoSchema, run: func(_ context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		return agentcore.AgentToolResult{Content: textContent(string(args))}, nil
	}}
	streamFn, _ := scriptedStream(t,
		toolUseAssistant(
			call("tool-1", "echo", `{"value":"first"}`),
			call("tool-2", "echo", `{"value":"second"}`),
		),
		textAssistant("done", agentcore.StopReasonEndTurn),
	)
	cfg := RunConfig{
		LoopConfig: LoopConfig{Model: "mock", Provider: "mock", Stream: streamFn},
		Batch: agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{
			Registry: registryOf(t, tool),
		}},
		EventBuffer: 16,
	}
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("echo both")}}

	events, _ := runScenario(t, cfg, agentCtx)

	var startIDs []string
	for _, ev := range events {
		if e, ok := ev.(agentcore.ToolExecutionStartEvent); ok {
			startIDs = append(startIDs, e.ToolCallID)
		}
	}
	if want := []string{"tool-1", "tool-2"}; !reflect.DeepEqual(startIDs, want) {
		t.Fatalf("tool_execution_start order = %v, want %v", startIDs, want)
	}
}

// PI: it("should inject queued messages after all tool calls complete") also
// asserts the steering message's message_start lifecycle. Keel emits it.
func TestConformanceSteeringMessagesEmitLifecycleEvents(t *testing.T) {
	tool := funcTool{name: "echo", schema: echoSchema, run: func(_ context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		return agentcore.AgentToolResult{Content: textContent("ok")}, nil
	}}
	var steeringDelivered bool
	cfg := RunConfig{
		LoopConfig: LoopConfig{Model: "mock", Provider: "mock"},
		Batch: agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{
			Registry: registryOf(t, tool),
		}},
		GetSteeringMessages: func(context.Context) []agentcore.AgentMessage {
			if steeringDelivered {
				return nil
			}
			steeringDelivered = true
			return []agentcore.AgentMessage{userMessage("interrupt")}
		},
		EventBuffer: 16,
	}
	streamFn, _ := scriptedStream(t,
		toolUseAssistant(call("tool-1", "echo", `{"value":"first"}`)),
		textAssistant("done", agentcore.StopReasonEndTurn),
	)
	cfg.LoopConfig.Stream = streamFn
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("start")}}

	events, result := runScenario(t, cfg, agentCtx)

	var starts, ends int
	for _, ev := range events {
		if e, ok := ev.(agentcore.MessageEndEvent); ok && e.Message.Role() == agentcore.RoleUser &&
			agentcore.ContentToText(e.Message.(agentcore.UserMessage).Content) == "interrupt" {
			ends++
		}
		if e, ok := ev.(agentcore.MessageStartEvent); ok && e.Message.Role() == agentcore.RoleUser {
			if u, ok := e.Message.(agentcore.UserMessage); ok && agentcore.ContentToText(u.Content) == "interrupt" {
				starts++
			}
		}
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("steering message lifecycle = start:%d end:%d, want 1/1", starts, ends)
	}
	found := false
	for _, m := range result {
		if u, ok := m.(agentcore.UserMessage); ok && agentcore.ContentToText(u.Content) == "interrupt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("steering message missing from the run result: %v", messageRoles(result))
	}
}
