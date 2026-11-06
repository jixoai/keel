package loop

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
)

// PI's core AgentEvent includes message_start/message_end for the prompt and
// for every tool result (packages/agent/src/agent-loop.ts:129-130, 956-958).
// Keel emits them too; the host decides which ones to persist.

func TestConformancePromptMessageLifecycle(t *testing.T) {
	fn, _ := scriptedStream(t, textAssistant("hi", agentcore.StopReasonEndTurn))
	cfg := RunConfig{
		LoopConfig:  LoopConfig{Model: "m", Stream: fn},
		PromptCount: 1,
		EventBuffer: 16,
	}
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: textContent("hello")},
	}}
	stream := StartRun(context.Background(), agentCtx, cfg)
	var events []agentcore.AgentEvent
	for event := range stream.Events() {
		events = append(events, event)
	}
	want := []string{"agent_start", "message_start", "message_end", "turn_start"}
	if len(events) < len(want) {
		t.Fatalf("events = %d, want at least %d", len(events), len(want))
	}
	for i, name := range want {
		if events[i].EventType() != name {
			t.Fatalf("event[%d] = %s, want %s", i, events[i].EventType(), name)
		}
	}
	start, ok := events[1].(agentcore.MessageStartEvent)
	if !ok || start.Message.Role() != agentcore.RoleUser {
		t.Fatalf("prompt message_start = %#v", events[1])
	}
	end, ok := events[2].(agentcore.MessageEndEvent)
	if !ok || end.Message.Role() != agentcore.RoleUser {
		t.Fatalf("prompt message_end = %#v", events[2])
	}
}

func TestConformanceToolResultMessageLifecycle(t *testing.T) {
	tool := funcTool{
		name:   "echo",
		schema: echoSchema,
		mode:   agentcore.ToolExecutionParallel,
		run: func(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
			return agentcore.AgentToolResult{Content: textContent("ok")}, nil
		},
	}
	fn, _ := scriptedStream(t,
		toolUseAssistant(agentcore.NewToolCallContent("call-1", "echo", json.RawMessage(`{"value":"x"}`))),
		textAssistant("done", agentcore.StopReasonEndTurn),
	)
	cfg := RunConfig{
		LoopConfig:  LoopConfig{Model: "m", Stream: fn},
		Batch:       agenttoolBatch(registryOf(t, tool)),
		PromptCount: 1,
		EventBuffer: 32,
	}
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: textContent("run")},
	}}
	stream := StartRun(context.Background(), agentCtx, cfg)
	var events []agentcore.AgentEvent
	for event := range stream.Events() {
		events = append(events, event)
	}
	toolEnd := -1
	resultStart := -1
	resultEnd := -1
	for i, event := range events {
		switch e := event.(type) {
		case agentcore.ToolExecutionEndEvent:
			if e.ToolCallID == "call-1" {
				toolEnd = i
			}
		case agentcore.MessageStartEvent:
			if e.Message.Role() == agentcore.RoleToolResult {
				resultStart = i
			}
		case agentcore.MessageEndEvent:
			if e.Message.Role() == agentcore.RoleToolResult {
				resultEnd = i
			}
		}
	}
	if toolEnd < 0 || resultStart < 0 || resultEnd < 0 {
		t.Fatalf("tool_end=%d result_start=%d result_end=%d", toolEnd, resultStart, resultEnd)
	}
	if !(toolEnd < resultStart && resultStart < resultEnd) {
		t.Fatalf("order tool_end=%d result_start=%d result_end=%d", toolEnd, resultStart, resultEnd)
	}
	msg, ok := events[resultEnd].(agentcore.MessageEndEvent)
	if !ok {
		t.Fatalf("result_end = %T", events[resultEnd])
	}
	tr, ok := msg.Message.(agentcore.ToolResultMessage)
	if !ok || tr.ToolCallID != "call-1" {
		t.Fatalf("tool result message = %#v", msg.Message)
	}
}

// agenttoolBatch keeps the test readable without importing BatchConfig twice.
func agenttoolBatch(reg *agenttool.ToolRegistry) agenttool.BatchConfig {
	return agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: reg}}
}
