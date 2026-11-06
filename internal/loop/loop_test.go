package loop

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/provider"
)

// fakeStream returns a StreamFn that hands out the pre-programmed responses in
// order. It is the loop-level equivalent of a deterministic provider: no
// process counters, no network, identical on replay.
func fakeStream(t *testing.T, responses ...agentcore.AssistantMessage) provider.StreamFn {
	t.Helper()
	next := 0
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		if next >= len(responses) {
			t.Fatalf("provider called %d times, only %d responses", next+1, len(responses))
		}
		msg := responses[next]
		next++
		s := provider.NewAssistantMessageEventStream(4)
		go func() {
			defer s.Close()
			partial := agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}
			_ = s.Emit(ctx, provider.StreamStartEvent{Partial: partial})
			_ = s.Emit(ctx, provider.StreamTextEvent{Partial: msg})
			_ = s.Emit(ctx, provider.StreamDoneEvent{Message: msg})
		}()
		return s, nil
	}
}

func assistantText(text, stop string) agentcore.AssistantMessage {
	return agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList{agentcore.NewTextContent(text)},
		StopReason: stop,
	}
}

func runLoopOnce(t *testing.T, cfg RunConfig, msgs ...agentcore.AgentMessage) ([]agentcore.AgentEvent, []agentcore.AgentMessage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	agentCtx := &agentcore.AgentContext{
		SystemPrompt: "you are a test agent",
		Messages:     agentcore.MessageList(append([]agentcore.AgentMessage{}, msgs...)),
	}
	stream := StartRun(ctx, agentCtx, cfg)
	var events []agentcore.AgentEvent
	for ev := range stream.Events() {
		events = append(events, ev)
	}
	result, err := stream.Result(ctx)
	if err != nil {
		t.Fatalf("stream result: %v", err)
	}
	return events, result
}

func TestLoopTextOnly(t *testing.T) {
	cfg := RunConfig{
		LoopConfig:  LoopConfig{Model: "fake", Provider: "fake", Stream: fakeStream(t, assistantText("hello", agentcore.StopReasonEndTurn))},
		EventBuffer: 8,
	}
	events, result := runLoopOnce(t, cfg, agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewTextContent("hi")},
	})
	if len(result) != 1 {
		t.Fatalf("result messages = %d, want 1", len(result))
	}
	msg, ok := result[0].(agentcore.AssistantMessage)
	if !ok {
		t.Fatalf("result[0] is %T", result[0])
	}
	if text := msg.Content[0].(agentcore.TextContent).Text; text != "hello" {
		t.Fatalf("text = %q", text)
	}
	var starts, ends int
	for _, ev := range events {
		switch ev.(type) {
		case agentcore.AgentStartEvent:
			starts++
		case agentcore.AgentEndEvent:
			ends++
		}
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("agent start/end = %d/%d", starts, ends)
	}
}

// echoTool is a deterministic tool used to prove the tool round-trip.
type echoTool struct{}

func (echoTool) Name() string        { return "echo" }
func (echoTool) Description() string { return "echo the argument" }
func (echoTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}`)
}
func (echoTool) ExecutionMode() agentcore.ToolExecutionMode { return agentcore.ToolExecutionParallel }
func (echoTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent("echo:" + string(args))},
	}, nil
}

func TestLoopToolRoundTrip(t *testing.T) {
	reg := agenttool.NewToolRegistry()
	if err := reg.Register(echoTool{}); err != nil {
		t.Fatal(err)
	}
	call := agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList{agentcore.NewToolCallContent("call-1", "echo", json.RawMessage(`{"x":"a"}`))},
		StopReason: agentcore.StopReasonToolUse,
	}
	cfg := RunConfig{
		LoopConfig:  LoopConfig{Model: "fake", Provider: "fake", Stream: fakeStream(t, call, assistantText("done", agentcore.StopReasonEndTurn))},
		Batch:       agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: reg}},
		EventBuffer: 8,
	}
	_, result := runLoopOnce(t, cfg, agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewTextContent("call echo")},
	})
	if len(result) != 3 {
		t.Fatalf("result messages = %d, want 3 (tool call, tool result, final)", len(result))
	}
	tr, ok := result[1].(agentcore.ToolResultMessage)
	if !ok {
		t.Fatalf("result[1] is %T", result[1])
	}
	text := tr.Content[0].(agentcore.TextContent).Text
	if text != `echo:{"x":"a"}` {
		t.Fatalf("tool result = %q", text)
	}
	final := result[2].(agentcore.AssistantMessage)
	if final.Content[0].(agentcore.TextContent).Text != "done" {
		t.Fatal("final answer missing")
	}
}

func TestLoopProviderBuildErrorBecomesTerminalMessage(t *testing.T) {
	cfg := RunConfig{
		LoopConfig: LoopConfig{Model: "fake", Provider: "fake", Stream: func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
			return nil, context.DeadlineExceeded
		}},
		EventBuffer: 4,
	}
	_, result := runLoopOnce(t, cfg, agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewTextContent("hi")},
	})
	if len(result) != 1 {
		t.Fatalf("result messages = %d", len(result))
	}
	msg := result[0].(agentcore.AssistantMessage)
	if msg.StopReason != agentcore.StopReasonError || msg.ErrorMessage == "" {
		t.Fatalf("want error stop reason, got %q / %q", msg.StopReason, msg.ErrorMessage)
	}
}
