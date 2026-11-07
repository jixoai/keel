// Package-level test doubles and shape helpers for the agentloop service
// contract tests. Everything here is deterministic: scripts are fixed, tool
// call ids come from the script and no test depends on the clock (the W1
// test-infrastructure rule: no UnixNano).
package agentloop_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentloop"
	"github.com/jixoai/keel/internal/provider"
)

// ---------------------------------------------------------------------------
// Fake provider
// ---------------------------------------------------------------------------

// fakeProvider is the scripted provider double: it serves the responses in
// order, counts calls, records the requests it saw and can block a call until
// the run context ends (the cancellation scenario). It never touches the
// network or the clock.
type fakeProvider struct {
	mu          sync.Mutex
	responses   []agentcore.AssistantMessage
	requests    []provider.LlmContext
	calls       int
	blockFrom   int
	entered     chan struct{}
	enteredOnce sync.Once
}

func newFakeProvider(responses ...agentcore.AssistantMessage) *fakeProvider {
	return &fakeProvider{responses: responses}
}

// blockFromCall makes every provider call with a 1-based index >= n block
// until the run context ends (a client that goes away). It returns a channel
// closed when the first blocking call is entered.
func (f *fakeProvider) blockFromCall(n int) <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blockFrom = n
	f.entered = make(chan struct{})
	return f.entered
}

func (f *fakeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeProvider) seenRequests() []provider.LlmContext {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]provider.LlmContext{}, f.requests...)
}

// stream is the provider.StreamFn handed to a loop.
func (f *fakeProvider) stream(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
	f.mu.Lock()
	f.calls++
	index := f.calls
	f.requests = append(f.requests, provider.LlmContext{
		SystemPrompt: llm.SystemPrompt,
		Messages:     append(agentcore.MessageList{}, llm.Messages...),
		Tools:        llm.Tools,
	})
	blocking := f.blockFrom > 0 && index >= f.blockFrom
	if blocking {
		f.enteredOnce.Do(func() { close(f.entered) })
	}
	var response agentcore.AssistantMessage
	if index <= len(f.responses) {
		response = f.responses[index-1]
	}
	f.mu.Unlock()

	if blocking {
		// The request abandons when the client (the run context) goes away:
		// the stream closes without a done event, like a real disconnect.
		<-ctx.Done()
		stream := provider.NewAssistantMessageEventStream(1)
		stream.Close()
		return stream, nil
	}
	if index > len(f.responses) {
		return nil, fmt.Errorf("fake provider: unexpected call %d (script has %d responses)", index, len(f.responses))
	}
	stream := provider.NewAssistantMessageEventStream(4)
	go func() {
		defer stream.Close()
		partial := agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}
		_ = stream.Emit(ctx, provider.StreamStartEvent{Partial: partial})
		_ = stream.Emit(ctx, provider.StreamTextEvent{Partial: response})
		_ = stream.Emit(ctx, provider.StreamDoneEvent{Message: response})
	}()
	return stream, nil
}

// streamBinding is a StreamProvider-shaped binding for the descriptor tests
// (the shape W2's llm service instance is expected to have).
type streamBinding struct{ stream provider.StreamFn }

func (b streamBinding) StreamFn() provider.StreamFn { return b.stream }

// ---------------------------------------------------------------------------
// Messages, tools, scenarios
// ---------------------------------------------------------------------------

func userMsg(text string) agentcore.UserMessage {
	return agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewTextContent(text)},
	}
}

func assistantText(text, stop string) agentcore.AssistantMessage {
	return agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList{agentcore.NewTextContent(text)},
		StopReason: stop,
	}
}

func assistantToolUse(calls ...agentcore.ToolCallContent) agentcore.AssistantMessage {
	content := make(agentcore.ContentList, 0, len(calls))
	for _, c := range calls {
		content = append(content, c)
	}
	return agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    content,
		StopReason: agentcore.StopReasonToolUse,
	}
}

func toolCall(id, name, args string) agentcore.ToolCallContent {
	return agentcore.NewToolCallContent(id, name, json.RawMessage(args))
}

// echoTool is the deterministic tool every tool scenario executes.
type echoTool struct{ executed atomic.Int64 }

var echoSchema = json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`)

func (t *echoTool) Name() string            { return "echo" }
func (t *echoTool) Description() string     { return "echo tool" }
func (t *echoTool) Schema() json.RawMessage { return echoSchema }
func (t *echoTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}
func (t *echoTool) Execute(_ context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	var in struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return agentcore.AgentToolResult{}, err
	}
	t.executed.Add(1)
	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent("echoed: " + in.Value)},
	}, nil
}

// baseInput is the canonical scenario: one prior user turn, one new prompt.
func baseInput() agentloop.Input {
	return agentloop.Input{
		SessionID: "session-1",
		Context: agentloop.Context{
			SystemPrompt: "you are a test agent",
			Messages:     agentcore.MessageList{userMsg("old history"), userMsg("hello")},
			PromptCount:  1,
		},
		Model: agentloop.ModelConfig{
			ID:            "mock",
			Provider:      "mock",
			APIKey:        "test-key",
			ThinkingLevel: agentcore.ThinkingLow,
		},
		Limits: agentloop.Limits{MaxTurns: 8},
	}
}

// runLoopOnce drives one loop run to completion and returns the observed
// events and the produced messages.
func runLoopOnce(t *testing.T, l agentloop.Loop, in agentloop.Input) ([]agentcore.AgentEvent, []agentcore.AgentMessage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := l.Run(ctx, in)
	if err != nil {
		t.Fatalf("loop run: %v", err)
	}
	var events []agentcore.AgentEvent
	for ev := range stream.Events() {
		events = append(events, ev)
	}
	result, err := stream.Result(ctx)
	if err != nil {
		t.Fatalf("loop result: %v", err)
	}
	return events, result
}

// ---------------------------------------------------------------------------
// Shapes: comparable projections with the volatile fields removed (timestamps,
// durations, provider metadata) — the whitelist the change allows.
// ---------------------------------------------------------------------------

type msgShape struct {
	Role          string
	Text          string
	StopReason    string
	ToolCallID    string
	ToolName      string
	IsError       bool
	Calls         []string
	DeclaredTools []string
	RemovedTools  []string
}

func shapeOf(m agentcore.AgentMessage) msgShape {
	sh := msgShape{Role: m.Role()}
	switch msg := m.(type) {
	case agentcore.AssistantMessage:
		sh.Text = agentcore.ContentToText(msg.Content)
		sh.StopReason = msg.StopReason
		for _, c := range msg.ToolCalls() {
			sh.Calls = append(sh.Calls, c.ID+"|"+c.Name+"|"+string(c.Arguments))
		}
	case agentcore.ToolResultMessage:
		sh.Text = agentcore.ContentToText(msg.Content)
		sh.ToolCallID = msg.ToolCallID
		sh.ToolName = msg.ToolName
		sh.IsError = msg.IsError
	case agentcore.UserMessage:
		sh.Text = agentcore.ContentToText(msg.Content)
	case agentcore.SystemMessage:
		sh.Text = msg.Content
		for _, d := range msg.ToolsAdded {
			sh.DeclaredTools = append(sh.DeclaredTools, d.Name)
		}
		for _, r := range msg.ToolsRemoved {
			sh.RemovedTools = append(sh.RemovedTools, r.Name)
		}
	case agentcore.CompactionMessage:
		sh.Text = msg.Summary
	}
	return sh
}

func shapesOf(msgs []agentcore.AgentMessage) []msgShape {
	out := make([]msgShape, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, shapeOf(m))
	}
	return out
}

func eventTypes(events []agentcore.AgentEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.EventType())
	}
	return out
}

func toolNames(tools []agentcore.AgentTool) []string {
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		out = append(out, tool.Name())
	}
	return out
}

// piEventTypes filters Keel's additive observability events so a sequence
// assertion compares exactly PI's core AgentEvent set.
func piEventTypes(events []agentcore.AgentEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		switch ev.(type) {
		case agentcore.TelemetryEvent, agentcore.CompactionEvent,
			agentcore.CompactionStartEvent, agentcore.SubAgentProgressEvent:
			continue
		}
		out = append(out, ev.EventType())
	}
	return out
}

func assertShapesEqual(t *testing.T, label string, got, want []msgShape) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s:\n got  %v\n want %v", label, got, want)
	}
}
