// Conformance tests ported from PI's official agent-loop test contract.
//
// Upstream: earendil-works/pi @ 55a5c6914570e2ae31ca36297654483bbbf46883
//   - packages/agent/test/agent-loop.test.ts
//     ("agentLoop with AgentMessage", "agentLoopContinue with AgentMessage")
//   - packages/agent/src/agent-loop.ts (runLoop control flow)
//   - packages/coding-agent/docs/extensions.md
//     (agent_start / agent_end / agent_before_settle / agent_settled ordering)
//
// Every test keeps PI's it(...) title (prefixed with TestConformance) so the two
// suites can be diffed line by line. Where Go cannot express PI's shape the
// adaptation is stated on the test:
//   - prompts are pre-appended to the context by the caller (Keel's loop does not
//     emit prompt message events, see the package docs);
//   - custom message kinds implement agentcore.Message instead of TypeScript
//     declaration merging, so the custom-kind case uses the non-LLM
//     CompactionMessage role;
//   - AgentLoopConfig hooks become RunConfig funcs with the same semantics.
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/provider"
)

// echoSchema is the `obj({ value: str() })` schema every tool test in PI uses.
const echoSchema = `{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`

// recordedRequest is one provider call captured by scriptedStream.
type recordedRequest struct {
	Model  string
	LLM    provider.LlmContext
	Config provider.StreamConfig
}

// scriptedStream is the conformance fake provider: it hands out the
// pre-programmed responses in order (PI's createAssistantMessageEventStream +
// streamFn) and records every request so a test can assert what the model would
// have seen.
func scriptedStream(t *testing.T, responses ...agentcore.AssistantMessage) (provider.StreamFn, func() []recordedRequest) {
	t.Helper()
	var (
		mu   sync.Mutex
		next int
		reqs []recordedRequest
	)
	fn := func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		mu.Lock()
		i := next
		next++
		reqs = append(reqs, recordedRequest{
			Model: model,
			LLM: provider.LlmContext{
				SystemPrompt: llm.SystemPrompt,
				Messages:     append(agentcore.MessageList{}, llm.Messages...),
				Tools:        llm.Tools,
			},
			Config: cfg,
		})
		mu.Unlock()
		if i >= len(responses) {
			return nil, errors.New("conformance: unexpected provider call")
		}
		msg := responses[i]
		s := provider.NewAssistantMessageEventStream(8)
		go func() {
			defer s.Close()
			_ = s.Emit(ctx, provider.StreamStartEvent{Partial: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}})
			_ = s.Emit(ctx, provider.StreamTextEvent{Partial: msg})
			_ = s.Emit(ctx, provider.StreamDoneEvent{Message: msg})
		}()
		return s, nil
	}
	requests := func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		out := make([]recordedRequest, len(reqs))
		copy(out, reqs)
		return out
	}
	return fn, requests
}

// funcTool is a scriptable AgentTool: PI's `{ name, label, description,
// parameters, execute }` literal, adapted to Go's interface.
type funcTool struct {
	name   string
	schema string
	mode   agentcore.ToolExecutionMode
	run    func(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error)
}

func (f funcTool) Name() string                               { return f.name }
func (f funcTool) Description() string                        { return f.name + " tool" }
func (f funcTool) Schema() json.RawMessage                    { return json.RawMessage(f.schema) }
func (f funcTool) ExecutionMode() agentcore.ToolExecutionMode { return f.mode }
func (f funcTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	return f.run(ctx, id, args, onUpdate)
}

// registryOf builds a tool registry from the scenario's tools.
func registryOf(t *testing.T, tools ...agentcore.AgentTool) *agenttool.ToolRegistry {
	t.Helper()
	reg := agenttool.NewToolRegistry()
	for _, tool := range tools {
		if err := reg.Register(tool); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	return reg
}

func textContent(text string) agentcore.ContentList {
	return agentcore.ContentList{agentcore.NewTextContent(text)}
}

func textAssistant(text, stop string) agentcore.AssistantMessage {
	return agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    textContent(text),
		StopReason: stop,
	}
}

func toolUseAssistant(calls ...agentcore.ToolCallContent) agentcore.AssistantMessage {
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

func userMessage(text string) agentcore.UserMessage {
	return agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: textContent(text)}
}

func call(id, name, args string) agentcore.ToolCallContent {
	return agentcore.NewToolCallContent(id, name, json.RawMessage(args))
}

// runScenario drives a fresh run and returns the events a subscriber saw, in
// order, plus the run result.
func runScenario(t *testing.T, cfg RunConfig, agentCtx *agentcore.AgentContext) ([]agentcore.AgentEvent, []agentcore.AgentMessage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
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

// piEventTypes maps the emitted stream onto PI's AgentEvent discriminants,
// dropping Keel's additive extension events (telemetry/compaction/progress) so a
// sequence assertion compares exactly the PI contract.
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

func messageRoles(msgs []agentcore.AgentMessage) []string {
	roles := make([]string, 0, len(msgs))
	for _, m := range msgs {
		roles = append(roles, m.Role())
	}
	return roles
}

func toolResultIDs(results []agentcore.ToolResultMessage) []string {
	ids := make([]string, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.ToolCallID)
	}
	return ids
}

func requestUserTexts(llm provider.LlmContext) []string {
	var out []string
	for _, m := range llm.Messages {
		u, ok := m.(agentcore.UserMessage)
		if !ok {
			continue
		}
		out = append(out, agentcore.ContentToText(u.Content))
	}
	return out
}

// ---------------------------------------------------------------------------
// 1. Basic turn event sequence.
// PI: it("should emit events with AgentMessage types")
// ---------------------------------------------------------------------------

func TestConformanceShouldEmitEventsWithAgentMessageTypes(t *testing.T) {
	streamFn, requests := scriptedStream(t, textAssistant("Hi there!", agentcore.StopReasonEndTurn))
	cfg := RunConfig{
		LoopConfig:  LoopConfig{Model: "mock", Provider: "mock", Stream: streamFn},
		EventBuffer: 8,
		// Host-level settle events (pi's coding-agent session layer, not the core
		// AgentEvent set). Opted in here to assert the full host lifecycle; the
		// kernel default (off) is covered by the lifecycle test.
		EmitSettleEvents: true,
	}
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("Hello")}}

	events, result := runScenario(t, cfg, agentCtx)

	// A prompt run returns the messages produced by this run: the assistant
	// reply only (the prompt was appended by the caller, as PI's runAgentLoop
	// does not return the prompt in newMessages either).
	if len(result) != 1 {
		t.Fatalf("result messages = %d, want 1: %v", len(result), messageRoles(result))
	}
	if got := result[0].Role(); got != agentcore.RoleAssistant {
		t.Fatalf("result[0] role = %q, want assistant", got)
	}
	if len(agentCtx.Messages) != 2 {
		t.Fatalf("context length = %d, want 2 (user + assistant)", len(agentCtx.Messages))
	}

	// Subscribers see the events in exactly this order: PI's core AgentEvent
	// sequence, then (opt-in) the host-level settle pair.
	want := []string{
		agentcore.EventAgentStart,
		agentcore.EventTurnStart,
		agentcore.EventMessageStart,
		agentcore.EventMessageUpdate,
		agentcore.EventMessageEnd,
		agentcore.EventTurnEnd,
		agentcore.EventAgentEnd,
		agentcore.EventAgentBeforeSettle,
		agentcore.EventAgentSettled,
	}
	if got := piEventTypes(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("event sequence = %v, want %v", got, want)
	}

	// The provider received the transcript (not a separate prompt field), as in
	// PI's "should build provider context exclusively from transcript messages".
	if got := requests(); len(got) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(got))
	} else if roles := messageRoles(got[0].LLM.Messages); !reflect.DeepEqual(roles, []string{"user"}) {
		t.Fatalf("request messages = %v, want [user]", roles)
	}
}

// ---------------------------------------------------------------------------
// 2. tool_execution_end completion order vs persisted result order.
// PI: it("should emit tool_execution_end in completion order but persist tool results in source order")
// ---------------------------------------------------------------------------

func TestConformanceToolExecutionEndCompletionOrderPersistSourceOrder(t *testing.T) {
	var firstFinished atomic.Bool
	var parallelObserved atomic.Bool
	release := make(chan struct{})
	var releaseOnce sync.Once

	tool := funcTool{name: "echo", schema: echoSchema, run: func(_ context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		var in struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return agentcore.AgentToolResult{}, err
		}
		switch in.Value {
		case "first":
			<-release
			firstFinished.Store(true)
		case "second":
			if !firstFinished.Load() {
				parallelObserved.Store(true)
			}
		}
		return agentcore.AgentToolResult{Content: textContent("echoed: " + in.Value)}, nil
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
		Batch: agenttool.BatchConfig{
			ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: registryOf(t, tool)},
		},
		EventBuffer: 8,
	}
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("echo both")}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := StartRun(ctx, agentCtx, cfg)
	var events []agentcore.AgentEvent
	for ev := range stream.Events() {
		events = append(events, ev)
		// Release the first tool only after the second tool's end event has been
		// delivered, which pins the completion order deterministically.
		if e, ok := ev.(agentcore.ToolExecutionEndEvent); ok && e.ToolCallID == "tool-2" {
			releaseOnce.Do(func() { close(release) })
		}
	}
	result, err := stream.Result(ctx)
	if err != nil {
		t.Fatalf("stream result: %v", err)
	}

	if !parallelObserved.Load() {
		t.Fatal("second tool did not run before the first finished; batch was not parallel")
	}

	var endIDs []string
	for _, ev := range events {
		if e, ok := ev.(agentcore.ToolExecutionEndEvent); ok {
			endIDs = append(endIDs, e.ToolCallID)
		}
	}
	if want := []string{"tool-2", "tool-1"}; !reflect.DeepEqual(endIDs, want) {
		t.Fatalf("tool_execution_end order = %v, want %v", endIDs, want)
	}

	var turnResultIDs []string
	for _, ev := range events {
		if e, ok := ev.(agentcore.TurnEndEvent); ok && len(e.ToolResults) > 0 {
			turnResultIDs = toolResultIDs(e.ToolResults)
		}
	}
	if want := []string{"tool-1", "tool-2"}; !reflect.DeepEqual(turnResultIDs, want) {
		t.Fatalf("turn_end tool results = %v, want source order %v", turnResultIDs, want)
	}

	// Persisted (context + run result) tool results keep assistant source order.
	var persisted []string
	for _, m := range result {
		if tr, ok := m.(agentcore.ToolResultMessage); ok {
			persisted = append(persisted, tr.ToolCallID)
		}
	}
	if want := []string{"tool-1", "tool-2"}; !reflect.DeepEqual(persisted, want) {
		t.Fatalf("persisted tool results = %v, want %v", persisted, want)
	}
	if roles := messageRoles(result); !reflect.DeepEqual(roles, []string{"assistant", "toolResult", "toolResult", "assistant"}) {
		t.Fatalf("result roles = %v", roles)
	}
}

// ---------------------------------------------------------------------------
// 3. length-truncated assistant: no tool executes; every call fails.
// PI: it("should not execute tool calls from a length-truncated assistant message")
// ---------------------------------------------------------------------------

func TestConformanceLengthTruncatedAssistantFailsToolCalls(t *testing.T) {
	var executed atomic.Int32
	tool := funcTool{name: "echo", schema: echoSchema, run: func(_ context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		executed.Add(1)
		return agentcore.AgentToolResult{Content: textContent("echoed")}, nil
	}}
	truncated := toolUseAssistant(call("tool-1", "echo", `{"value":"hel"}`))
	truncated.StopReason = agentcore.StopReasonLength

	streamFn, requests := scriptedStream(t,
		truncated,
		textAssistant("done", agentcore.StopReasonEndTurn),
	)
	cfg := RunConfig{
		LoopConfig: LoopConfig{Model: "mock", Provider: "mock", Stream: streamFn},
		Batch: agenttool.BatchConfig{
			ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: registryOf(t, tool)},
		},
		EventBuffer: 8,
	}
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("echo something")}}

	events, result := runScenario(t, cfg, agentCtx)

	if got := executed.Load(); got != 0 {
		t.Fatalf("tool executed %d times from a truncated message, want 0", got)
	}

	var end *agentcore.ToolExecutionEndEvent
	for i := range events {
		if e, ok := events[i].(agentcore.ToolExecutionEndEvent); ok {
			end = &e
		}
	}
	if end == nil {
		t.Fatal("no tool_execution_end event for the truncated tool call")
	}
	if !end.IsError {
		t.Fatal("tool_execution_end.IsError = false, want true")
	}
	if text := agentcore.ContentToText(end.Result.Content); !strings.Contains(text, "output token limit") {
		t.Fatalf("truncation message = %q, want it to mention the output token limit", text)
	}

	// The loop continues so the model can re-issue the call.
	if got := len(requests()); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
	if last := result[len(result)-1]; last.Role() != agentcore.RoleAssistant {
		t.Fatalf("last result message role = %q, want assistant", last.Role())
	}
	var failed agentcore.ToolResultMessage
	for _, m := range result {
		if tr, ok := m.(agentcore.ToolResultMessage); ok {
			failed = tr
		}
	}
	if !failed.IsError {
		t.Fatal("persisted tool result IsError = false, want true")
	}
}

// ---------------------------------------------------------------------------
// 4. beforeToolCall argument rewrite is not re-validated.
// PI: it("should execute mutated beforeToolCall args without revalidation")
// ---------------------------------------------------------------------------

func TestConformanceMutatedBeforeToolCallArgsWithoutRevalidation(t *testing.T) {
	var executed []string
	tool := funcTool{name: "echo", schema: echoSchema, run: func(_ context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		executed = append(executed, string(args))
		return agentcore.AgentToolResult{Content: textContent("echoed")}, nil
	}}
	rewritten := json.RawMessage(`{"value":123}`)
	cfg := RunConfig{
		LoopConfig: LoopConfig{Model: "mock", Provider: "mock"},
		Batch: agenttool.BatchConfig{
			ToolExecutorConfig: agenttool.ToolExecutorConfig{
				Registry: registryOf(t, tool),
				// The hook runs after validation: rewriting a schema-valid string
				// into a schema-invalid number must reach execute() untouched.
				BeforeToolCall: func(_ context.Context, _ agentcore.AgentToolCall) *agentcore.BeforeToolCallDecision {
					return &agentcore.BeforeToolCallDecision{UpdatedInput: rewritten}
				},
			},
		},
		EventBuffer: 8,
	}
	streamFn, _ := scriptedStream(t,
		toolUseAssistant(call("tool-1", "echo", `{"value":"hello"}`)),
		textAssistant("done", agentcore.StopReasonEndTurn),
	)
	cfg.LoopConfig.Stream = streamFn
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("echo something")}}

	_, result := runScenario(t, cfg, agentCtx)

	if want := []string{`{"value":123}`}; !reflect.DeepEqual(executed, want) {
		t.Fatalf("executed args = %v, want %v", executed, want)
	}
	// No validation-error tool result: the call ran.
	for _, m := range result {
		if tr, ok := m.(agentcore.ToolResultMessage); ok && tr.IsError {
			t.Fatalf("tool result is an error (%q); the rewrite was rejected",
				agentcore.ContentToText(tr.Content))
		}
	}
}

// ---------------------------------------------------------------------------
// 5. terminate semantics: the whole batch must ask to stop.
// PI: it("should stop after a tool batch when every tool result sets terminate=true")
//     it("should continue after parallel tool calls when not all tool results terminate")
//     it("should stop after a blocked tool call when beforeToolCall sets terminate=true")
//     it("should continue after a mixed batch with one terminating blocked call")
// ---------------------------------------------------------------------------

func TestConformanceTerminateRequiresWholeBatch(t *testing.T) {
	t.Run("should stop after a tool batch when every tool result sets terminate=true", func(t *testing.T) {
		tool := funcTool{name: "echo", schema: echoSchema, run: func(_ context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
			terminate := true
			return agentcore.AgentToolResult{Content: textContent("echoed"), Terminate: &terminate}, nil
		}}
		streamFn, requests := scriptedStream(t,
			toolUseAssistant(call("tool-1", "echo", `{"value":"hello"}`)),
		)
		cfg := RunConfig{
			LoopConfig: LoopConfig{Model: "mock", Provider: "mock", Stream: streamFn},
			Batch: agenttool.BatchConfig{
				ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: registryOf(t, tool)},
			},
			EventBuffer: 8,
		}
		agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("echo something")}}

		events, result := runScenario(t, cfg, agentCtx)

		if got := len(requests()); got != 1 {
			t.Fatalf("provider calls = %d, want 1", got)
		}
		if roles := messageRoles(result); !reflect.DeepEqual(roles, []string{"assistant", "toolResult"}) {
			t.Fatalf("result roles = %v", roles)
		}
		var turnEnds int
		for _, ev := range events {
			if _, ok := ev.(agentcore.TurnEndEvent); ok {
				turnEnds++
			}
		}
		if turnEnds != 1 {
			t.Fatalf("turn_end events = %d, want 1", turnEnds)
		}
	})

	t.Run("should continue after parallel tool calls when not all tool results terminate", func(t *testing.T) {
		tool := funcTool{name: "echo", schema: echoSchema, run: func(_ context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
			var in struct {
				Value string `json:"value"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return agentcore.AgentToolResult{}, err
			}
			terminate := in.Value == "first"
			return agentcore.AgentToolResult{Content: textContent("echoed: " + in.Value), Terminate: &terminate}, nil
		}}
		streamFn, requests := scriptedStream(t,
			toolUseAssistant(
				call("tool-1", "echo", `{"value":"first"}`),
				call("tool-2", "echo", `{"value":"second"}`),
			),
			textAssistant("done", agentcore.StopReasonEndTurn),
		)
		cfg := RunConfig{
			LoopConfig: LoopConfig{Model: "mock", Provider: "mock", Stream: streamFn},
			Batch: agenttool.BatchConfig{
				ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: registryOf(t, tool)},
			},
			EventBuffer: 8,
		}
		agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("echo both")}}

		_, result := runScenario(t, cfg, agentCtx)

		if got := len(requests()); got != 2 {
			t.Fatalf("provider calls = %d, want 2", got)
		}
		want := []string{"assistant", "toolResult", "toolResult", "assistant"}
		if roles := messageRoles(result); !reflect.DeepEqual(roles, want) {
			t.Fatalf("result roles = %v, want %v", roles, want)
		}
	})

	t.Run("should stop after a blocked tool call when beforeToolCall sets terminate=true", func(t *testing.T) {
		var executed atomic.Bool
		tool := funcTool{name: "echo", schema: echoSchema, run: func(_ context.Context, _ string, _ json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
			executed.Store(true)
			return agentcore.AgentToolResult{Content: textContent("should not execute")}, nil
		}}
		blocked := textContent("Blocked by policy")
		streamFn, requests := scriptedStream(t,
			toolUseAssistant(call("tool-1", "echo", `{"value":"hello"}`)),
		)
		cfg := RunConfig{
			LoopConfig: LoopConfig{Model: "mock", Provider: "mock", Stream: streamFn},
			Batch: agenttool.BatchConfig{
				ToolExecutorConfig: agenttool.ToolExecutorConfig{
					Registry: registryOf(t, tool),
					BeforeToolCall: func(_ context.Context, _ agentcore.AgentToolCall) *agentcore.BeforeToolCallDecision {
						return &agentcore.BeforeToolCallDecision{Block: true, Content: &blocked, Terminate: true}
					},
				},
			},
			EventBuffer: 8,
		}
		agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("echo something")}}

		_, result := runScenario(t, cfg, agentCtx)

		if executed.Load() {
			t.Fatal("blocked tool executed")
		}
		if got := len(requests()); got != 1 {
			t.Fatalf("provider calls = %d, want 1", got)
		}
		var toolResult agentcore.ToolResultMessage
		for _, m := range result {
			if tr, ok := m.(agentcore.ToolResultMessage); ok {
				toolResult = tr
			}
		}
		if !toolResult.IsError {
			t.Fatal("blocked tool result IsError = false, want true")
		}
		if text := agentcore.ContentToText(toolResult.Content); text != "Blocked by policy" {
			t.Fatalf("blocked reason = %q, want %q", text, "Blocked by policy")
		}
	})

	t.Run("should continue after a mixed batch with one terminating blocked call", func(t *testing.T) {
		executed := make([]string, 0, 2)
		tool := funcTool{name: "echo", schema: echoSchema, run: func(_ context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
			var in struct {
				Value string `json:"value"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return agentcore.AgentToolResult{}, err
			}
			executed = append(executed, in.Value)
			return agentcore.AgentToolResult{Content: textContent("echoed: " + in.Value)}, nil
		}}
		blocked := textContent("Blocked first")
		streamFn, requests := scriptedStream(t,
			toolUseAssistant(
				call("tool-1", "echo", `{"value":"first"}`),
				call("tool-2", "echo", `{"value":"second"}`),
			),
			textAssistant("done", agentcore.StopReasonEndTurn),
		)
		cfg := RunConfig{
			LoopConfig: LoopConfig{Model: "mock", Provider: "mock", Stream: streamFn},
			Batch: agenttool.BatchConfig{
				ToolExecutorConfig: agenttool.ToolExecutorConfig{
					Registry: registryOf(t, tool),
					BeforeToolCall: func(_ context.Context, call agentcore.AgentToolCall) *agentcore.BeforeToolCallDecision {
						var in struct {
							Value string `json:"value"`
						}
						if err := json.Unmarshal(call.Arguments, &in); err == nil && in.Value == "first" {
							return &agentcore.BeforeToolCallDecision{Block: true, Content: &blocked, Terminate: true}
						}
						return nil
					},
				},
			},
			EventBuffer: 8,
		}
		agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("echo both")}}

		_, _ = runScenario(t, cfg, agentCtx)

		if want := []string{"second"}; !reflect.DeepEqual(executed, want) {
			t.Fatalf("executed = %v, want %v", executed, want)
		}
		if got := len(requests()); got != 2 {
			t.Fatalf("provider calls = %d, want 2", got)
		}
	})
}

// ---------------------------------------------------------------------------
// 6. agentLoopContinue: resume without a prompt; reject empty/assistant tails.
// PI describe("agentLoopContinue with AgentMessage")
// ---------------------------------------------------------------------------

func TestConformanceAgentLoopContinue(t *testing.T) {
	t.Run("should throw when context has no messages", func(t *testing.T) {
		streamFn, _ := scriptedStream(t)
		_, err := StartContinue(context.Background(), &agentcore.AgentContext{}, RunConfig{
			LoopConfig: LoopConfig{Model: "mock", Provider: "mock", Stream: streamFn},
		})
		if err == nil {
			t.Fatal("StartContinue on an empty context returned no error")
		}
		if !errors.Is(err, ErrNoMessagesToContinue) {
			t.Fatalf("error = %v, want ErrNoMessagesToContinue", err)
		}
		if err.Error() != "Cannot continue: no messages in context" {
			t.Fatalf("error text = %q, want pi's wording", err.Error())
		}
	})

	t.Run("should continue from existing context without emitting user message events", func(t *testing.T) {
		streamFn, requests := scriptedStream(t, textAssistant("Response", agentcore.StopReasonEndTurn))
		cfg := RunConfig{
			LoopConfig:  LoopConfig{Model: "mock", Provider: "mock", Stream: streamFn},
			EventBuffer: 8,
		}
		agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("Hello")}}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stream, err := StartContinue(ctx, agentCtx, cfg)
		if err != nil {
			t.Fatalf("StartContinue: %v", err)
		}
		var events []agentcore.AgentEvent
		for ev := range stream.Events() {
			events = append(events, ev)
		}
		result, err := stream.Result(ctx)
		if err != nil {
			t.Fatalf("stream result: %v", err)
		}

		// Only the new assistant message is returned; the pre-existing user
		// message is not part of this run's newMessages.
		if len(result) != 1 {
			t.Fatalf("result messages = %d, want 1: %v", len(result), messageRoles(result))
		}
		if got := result[0].Role(); got != agentcore.RoleAssistant {
			t.Fatalf("result[0] role = %q, want assistant", got)
		}

		// No user message lifecycle events: that is the key difference from a
		// prompt run.
		var messageEnds int
		for _, ev := range events {
			switch e := ev.(type) {
			case agentcore.MessageStartEvent:
				if e.Message.Role() == agentcore.RoleUser {
					t.Fatalf("continuation emitted message_start for a user message")
				}
			case agentcore.MessageEndEvent:
				messageEnds++
				if e.Message.Role() == agentcore.RoleUser {
					t.Fatalf("continuation emitted message_end for a user message")
				}
			}
		}
		if messageEnds != 1 {
			t.Fatalf("message_end events = %d, want 1 (assistant)", messageEnds)
		}
		if got := len(requests()); got != 1 {
			t.Fatalf("provider calls = %d, want 1", got)
		}
	})

	t.Run("should reject a context whose last message is an assistant message", func(t *testing.T) {
		streamFn, _ := scriptedStream(t)
		agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
			userMessage("Hello"),
			textAssistant("hi", agentcore.StopReasonEndTurn),
		}}
		_, err := StartContinue(context.Background(), agentCtx, RunConfig{
			LoopConfig: LoopConfig{Model: "mock", Provider: "mock", Stream: streamFn},
		})
		if !errors.Is(err, ErrContinueFromAssistant) {
			t.Fatalf("error = %v, want ErrContinueFromAssistant", err)
		}
	})
}

// ---------------------------------------------------------------------------
// 7. steering / follow-up queue drain points.
// PI: it("should inject queued messages after all tool calls complete")
// ---------------------------------------------------------------------------

func TestConformanceSteeringInjectedAfterAllToolCallsComplete(t *testing.T) {
	var mu sync.Mutex
	executed := make([]string, 0, 2)
	tool := funcTool{name: "echo", schema: echoSchema, run: func(_ context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		var in struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return agentcore.AgentToolResult{}, err
		}
		mu.Lock()
		executed = append(executed, in.Value)
		mu.Unlock()
		return agentcore.AgentToolResult{Content: textContent("ok:" + in.Value)}, nil
	}}

	var (
		pollMu         sync.Mutex
		steeringPolls  []int // number of executed tools at each poll
		steeringDelive bool
	)
	cfg := RunConfig{
		LoopConfig: LoopConfig{Model: "mock", Provider: "mock"},
		Batch: agenttool.BatchConfig{
			ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: registryOf(t, tool)},
			ForceSequential:    true,
		},
		GetSteeringMessages: func(context.Context) []agentcore.AgentMessage {
			mu.Lock()
			n := len(executed)
			mu.Unlock()
			pollMu.Lock()
			steeringPolls = append(steeringPolls, n)
			deliver := n >= 2 && !steeringDelive
			if deliver {
				steeringDelive = true
			}
			pollMu.Unlock()
			if deliver {
				return []agentcore.AgentMessage{userMessage("interrupt")}
			}
			return nil
		},
		EventBuffer: 8,
	}
	streamFn, requests := scriptedStream(t,
		toolUseAssistant(
			call("tool-1", "echo", `{"value":"first"}`),
			call("tool-2", "echo", `{"value":"second"}`),
		),
		textAssistant("done", agentcore.StopReasonEndTurn),
	)
	cfg.LoopConfig.Stream = streamFn
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("start")}}

	events, _ := runScenario(t, cfg, agentCtx)

	mu.Lock()
	gotExecuted := append([]string{}, executed...)
	mu.Unlock()
	if want := []string{"first", "second"}; !reflect.DeepEqual(gotExecuted, want) {
		t.Fatalf("executed = %v, want %v (both tools before steering)", gotExecuted, want)
	}

	var toolEnds int
	for _, ev := range events {
		if e, ok := ev.(agentcore.ToolExecutionEndEvent); ok {
			if e.IsError {
				t.Fatalf("tool_execution_end for %s is an error", e.ToolCallID)
			}
			toolEnds++
		}
	}
	if toolEnds != 2 {
		t.Fatalf("tool_execution_end events = %d, want 2", toolEnds)
	}

	// The steering message is delivered before the next request, after both tool
	// results (PI asserts the same via message_start events; Keel's loop does not
	// emit message events for steering input, so the request transcript is the
	// observable contract).
	reqs := requests()
	if len(reqs) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(reqs))
	}
	if roles := messageRoles(reqs[1].LLM.Messages); !reflect.DeepEqual(roles,
		[]string{"user", "assistant", "toolResult", "toolResult", "user"}) {
		t.Fatalf("second request roles = %v", roles)
	}
	if users := requestUserTexts(reqs[1].LLM); !reflect.DeepEqual(users, []string{"start", "interrupt"}) {
		t.Fatalf("second request users = %v, want [start interrupt]", users)
	}
	if users := requestUserTexts(reqs[0].LLM); len(users) != 1 || users[0] != "start" {
		t.Fatalf("first request users = %v, want [start] (steering not yet available)", users)
	}
}

// PI: it("should continue after parallel tool calls when not all tool results terminate")
// covers follow-up consumption: it is polled only once the inner loop settles.
func TestConformanceFollowUpConsumedAfterInnerLoop(t *testing.T) {
	var (
		mu          sync.Mutex
		followPolls int
		delivered   bool
		polledAt    []string
	)
	cfg := RunConfig{
		LoopConfig: LoopConfig{Model: "mock", Provider: "mock"},
		GetFollowUpMessages: func(_ context.Context, agentCtx *agentcore.AgentContext) []agentcore.AgentMessage {
			mu.Lock()
			defer mu.Unlock()
			followPolls++
			if followPolls == 1 {
				polledAt = messageRoles(agentCtx.Messages)
			}
			if delivered {
				return nil
			}
			delivered = true
			return []agentcore.AgentMessage{userMessage("follow up")}
		},
		EventBuffer: 8,
	}
	streamFn, requests := scriptedStream(t,
		textAssistant("first answer", agentcore.StopReasonEndTurn),
		textAssistant("second answer", agentcore.StopReasonEndTurn),
	)
	cfg.LoopConfig.Stream = streamFn
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("run")}}

	_, result := runScenario(t, cfg, agentCtx)

	if got := len(requests()); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if followPolls != 2 {
		t.Fatalf("follow-up polls = %d, want 2 (deliver, then empty)", followPolls)
	}
	// The first poll happens after the inner loop ended: the transcript already
	// holds the settled assistant answer.
	if want := []string{"user", "assistant"}; !reflect.DeepEqual(polledAt, want) {
		t.Fatalf("first follow-up poll saw roles %v, want %v", polledAt, want)
	}
	if users := requestUserTexts(requests()[1].LLM); !reflect.DeepEqual(users, []string{"run", "follow up"}) {
		t.Fatalf("second request users = %v, want [run follow up]", users)
	}
	if roles := messageRoles(result); !reflect.DeepEqual(roles, []string{"assistant", "user", "assistant"}) {
		t.Fatalf("result roles = %v", roles)
	}
}

// ---------------------------------------------------------------------------
// 8. prepareNextTurn: a snapshot + appended messages affect the next turn.
// PI: it("should use prepareNextTurn snapshot before continuing")
// ---------------------------------------------------------------------------

func TestConformancePrepareNextTurnSnapshotBeforeContinuing(t *testing.T) {
	tool := funcTool{name: "echo", schema: echoSchema, run: func(_ context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		return agentcore.AgentToolResult{Content: textContent("echoed: " + string(args))}, nil
	}}
	var (
		mu           sync.Mutex
		prepareCalls int
		prepared     bool
	)
	cfg := RunConfig{
		LoopConfig: LoopConfig{Model: "mock", Provider: "mock"},
		Batch: agenttool.BatchConfig{
			ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: registryOf(t, tool)},
		},
		PrepareNextTurn: func(_ context.Context, agentCtx *agentcore.AgentContext) *TurnUpdate {
			mu.Lock()
			defer mu.Unlock()
			prepareCalls++
			if prepared {
				return nil
			}
			prepared = true
			snapshot := append(agentcore.MessageList{}, agentCtx.Messages...)
			return &TurnUpdate{
				Messages:       &snapshot,
				AppendMessages: []agentcore.AgentMessage{userMessage("updated guidance")},
			}
		},
		EventBuffer: 8,
	}
	streamFn, requests := scriptedStream(t,
		toolUseAssistant(call("tool-1", "echo", `{"value":"hello"}`)),
		textAssistant("done", agentcore.StopReasonEndTurn),
	)
	cfg.LoopConfig.Stream = streamFn
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("echo something")}}

	_, _ = runScenario(t, cfg, agentCtx)

	mu.Lock()
	defer mu.Unlock()
	if prepareCalls != 1 {
		t.Fatalf("prepareNextTurn calls = %d, want 1 (not before the first turn)", prepareCalls)
	}
	reqs := requests()
	if len(reqs) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(reqs))
	}
	if !contextHasUserText(reqs[1].LLM.Messages, "updated guidance") {
		t.Fatalf("second request is missing the appended guidance: %v", messageRoles(reqs[1].LLM.Messages))
	}
	// The snapshot kept the turn's tool result, so the model still sees it.
	if roles := messageRoles(reqs[1].LLM.Messages); !reflect.DeepEqual(roles,
		[]string{"user", "assistant", "toolResult", "user"}) {
		t.Fatalf("second request roles = %v", roles)
	}
}

// ---------------------------------------------------------------------------
// 9. prepareRequest.
// PI: it("prepares the initial request after pending messages and can replace request state")
//     it("does not poll steering after prepareRequest")
// ---------------------------------------------------------------------------

func TestConformancePrepareRequest(t *testing.T) {
	t.Run("prepares the initial request after pending messages and can replace request state", func(t *testing.T) {
		replacementModel := "replacement"
		high := agentcore.ThinkingHigh
		canonical := userMessage("canonical projection")

		var (
			mu                sync.Mutex
			steeringDelivered bool
			prepareCalls      int
		)
		cfg := RunConfig{
			LoopConfig: LoopConfig{Model: "mock", Provider: "mock"},
			GetSteeringMessages: func(context.Context) []agentcore.AgentMessage {
				mu.Lock()
				defer mu.Unlock()
				if steeringDelivered {
					return nil
				}
				steeringDelivered = true
				return []agentcore.AgentMessage{userMessage("steering")}
			},
			PrepareRequest: func(_ context.Context, req PrepareRequestContext) *RequestUpdate {
				mu.Lock()
				prepareCalls++
				mu.Unlock()

				// Pending steering is already appended when the hook runs (PI also
				// emits message events for it; Keel's loop does not emit message
				// events for steering input, so the context is the contract).
				if !contextHasUserText(req.Context.Messages, "steering") {
					t.Errorf("prepareRequest context is missing the steering message: %v", messageRoles(req.Context.Messages))
				}
				return &RequestUpdate{
					Context:       &agentcore.AgentContext{Messages: agentcore.MessageList{canonical}},
					Model:         &replacementModel,
					ThinkingLevel: &high,
				}
			},
			EventBuffer: 8,
		}
		streamFn, requests := scriptedStream(t, textAssistant("done", agentcore.StopReasonEndTurn))
		cfg.LoopConfig.Stream = func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
			// Assert what the provider would have seen, then delegate.
			if model != replacementModel {
				t.Errorf("request model = %q, want %q", model, replacementModel)
			}
			if roles := messageRoles(llm.Messages); !reflect.DeepEqual(roles, []string{"user"}) || !contextHasUserText(llm.Messages, "canonical projection") {
				t.Errorf("request messages = %v, want the canonical projection only", roles)
			}
			if sc.ThinkingLevel != agentcore.ThinkingHigh {
				t.Errorf("request thinking level = %q, want high", sc.ThinkingLevel)
			}
			return streamFn(ctx, model, llm, sc)
		}
		agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("prompt")}}

		_, _ = runScenario(t, cfg, agentCtx)

		mu.Lock()
		defer mu.Unlock()
		if prepareCalls != 1 {
			t.Fatalf("prepareRequest calls = %d, want 1", prepareCalls)
		}
		if got := len(requests()); got != 1 {
			t.Fatalf("provider calls = %d, want 1", got)
		}
	})

	t.Run("does not poll steering after prepareRequest", func(t *testing.T) {
		var (
			mu                  sync.Mutex
			queued              []agentcore.AgentMessage
			lateSteering        = userMessage("late steering")
			requestPreparations int
			steeringPolls       int
		)
		cfg := RunConfig{
			LoopConfig: LoopConfig{Model: "mock", Provider: "mock"},
			GetSteeringMessages: func(context.Context) []agentcore.AgentMessage {
				mu.Lock()
				defer mu.Unlock()
				steeringPolls++
				out := queued
				queued = nil
				return out
			},
			PrepareRequest: func(context.Context, PrepareRequestContext) *RequestUpdate {
				mu.Lock()
				defer mu.Unlock()
				requestPreparations++
				if requestPreparations == 1 {
					queued = append(queued, lateSteering)
				}
				return nil
			},
			EventBuffer: 8,
		}
		var sawLate []bool
		streamFn, _ := scriptedStream(t,
			textAssistant("first", agentcore.StopReasonEndTurn),
			textAssistant("second", agentcore.StopReasonEndTurn),
		)
		cfg.LoopConfig.Stream = func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
			mu.Lock()
			sawLate = append(sawLate, contextHasUserText(llm.Messages, "late steering"))
			mu.Unlock()
			return streamFn(ctx, model, llm, sc)
		}
		agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("run")}}

		_, _ = runScenario(t, cfg, agentCtx)

		mu.Lock()
		defer mu.Unlock()
		if want := []bool{false, true}; !reflect.DeepEqual(sawLate, want) {
			t.Fatalf("requests including late steering = %v, want %v", sawLate, want)
		}
		if requestPreparations != 2 {
			t.Fatalf("prepareRequest calls = %d, want 2", requestPreparations)
		}
		// Startup poll, post-turn-1 delivery, post-turn-2 natural-stop check.
		if steeringPolls != 3 {
			t.Fatalf("steering polls = %d, want 3", steeringPolls)
		}
	})
}

func contextHasUserText(msgs []agentcore.AgentMessage, want string) bool {
	for _, m := range msgs {
		if u, ok := m.(agentcore.UserMessage); ok && agentcore.ContentToText(u.Content) == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 10. execute() duration excludes hook time.
// PI: it("records how long execute() took on the tool result, excluding hooks")
// ---------------------------------------------------------------------------

func TestConformanceToolDurationExcludesHooks(t *testing.T) {
	tool := funcTool{name: "echo", schema: echoSchema, run: func(_ context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		var in struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return agentcore.AgentToolResult{}, err
		}
		time.Sleep(30 * time.Millisecond)
		return agentcore.AgentToolResult{Content: textContent(in.Value)}, nil
	}}
	cfg := RunConfig{
		LoopConfig: LoopConfig{Model: "mock", Provider: "mock"},
		Batch: agenttool.BatchConfig{
			ToolExecutorConfig: agenttool.ToolExecutorConfig{
				Registry: registryOf(t, tool),
				BeforeToolCall: func(_ context.Context, call agentcore.AgentToolCall) *agentcore.BeforeToolCallDecision {
					time.Sleep(200 * time.Millisecond)
					if call.ID == "blocked" {
						blocked := textContent("no")
						return &agentcore.BeforeToolCallDecision{Block: true, Content: &blocked}
					}
					return nil
				},
			},
		},
		EventBuffer: 8,
	}
	streamFn, _ := scriptedStream(t,
		toolUseAssistant(
			call("ran", "echo", `{"value":"a"}`),
			call("blocked", "echo", `{"value":"b"}`),
		),
		textAssistant("done", agentcore.StopReasonEndTurn),
	)
	cfg.LoopConfig.Stream = streamFn
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("go")}}

	_, result := runScenario(t, cfg, agentCtx)

	byID := make(map[string]agentcore.ToolResultMessage)
	for _, m := range result {
		if tr, ok := m.(agentcore.ToolResultMessage); ok {
			byID[tr.ToolCallID] = tr
		}
	}
	ran, ok := byID["ran"]
	if !ok {
		t.Fatal("missing tool result for ran")
	}
	if ran.DurationMs == nil {
		t.Fatal("ran tool result has no durationMs")
	}
	if got := *ran.DurationMs; got < 25 || got >= 150 {
		t.Fatalf("ran durationMs = %d, want [25,150) — execute only, no hook time (hook sleeps 200ms)", got)
	}
	blocked, ok := byID["blocked"]
	if !ok {
		t.Fatal("missing tool result for blocked")
	}
	if !blocked.IsError {
		t.Fatal("blocked tool result IsError = false, want true")
	}
	if blocked.DurationMs != nil {
		t.Fatalf("blocked tool result has durationMs = %d, want none (execute never ran)", *blocked.DurationMs)
	}
}

// ---------------------------------------------------------------------------
// 11. transformContext runs before convertToLlm.
// PI: it("should apply transformContext before convertToLlm")
// ---------------------------------------------------------------------------

func TestConformanceApplyTransformContextBeforeConvertToLlm(t *testing.T) {
	cfg := RunConfig{
		LoopConfig:  LoopConfig{Model: "mock", Provider: "mock"},
		EventBuffer: 8,
	}
	var (
		transformedLen int
		convertedLen   int
	)
	cfg.TransformContext = func(_ context.Context, msgs agentcore.MessageList) agentcore.MessageList {
		// Keep only the last two messages (prune old ones).
		transformedLen = 2
		out := make(agentcore.MessageList, 0, 2)
		out = append(out, msgs[len(msgs)-2:]...)
		return out
	}
	cfg.ConvertToLlm = func(msgs agentcore.MessageList) agentcore.MessageList {
		convertedLen = len(msgs)
		return msgs
	}
	streamFn, requests := scriptedStream(t, textAssistant("Response", agentcore.StopReasonEndTurn))
	cfg.LoopConfig.Stream = streamFn
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
		userMessage("old message 1"),
		textAssistant("old response 1", agentcore.StopReasonEndTurn),
		userMessage("old message 2"),
		textAssistant("old response 2", agentcore.StopReasonEndTurn),
		userMessage("new message"),
	}}

	_, _ = runScenario(t, cfg, agentCtx)

	// transformContext should have been called first, keeping only the last 2.
	if transformedLen != 2 {
		t.Fatalf("transformContext produced %d messages, want 2", transformedLen)
	}
	// Then convertToLlm receives the pruned messages.
	if convertedLen != 2 {
		t.Fatalf("convertToLlm saw %d messages, want the 2 pruned ones", convertedLen)
	}
	reqs := requests()
	if len(reqs) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(reqs))
	}
	if got := messageRoles(reqs[0].LLM.Messages); !reflect.DeepEqual(got, []string{"assistant", "user"}) {
		t.Fatalf("provider message roles = %v, want the pruned tail [assistant user]", got)
	}
	if got := requestUserTexts(reqs[0].LLM); !reflect.DeepEqual(got, []string{"new message"}) {
		t.Fatalf("provider user messages = %v, want [new message]", got)
	}
}

// ---------------------------------------------------------------------------
// 12. custom (non-LLM) message kinds go through convertToLlm.
// PI: it("should handle custom message types via convertToLlm")
//
// Adaptation: PI's test injects a `notification` role via TS declaration
// merging. Go's Message interface is sealed inside agentcore, so the non-LLM
// kind available to callers is CompactionMessage (role "compaction") — the same
// contract: a message kind that is not LLM-bound must be handled by
// convertToLlm, never sent to the provider raw.
// ---------------------------------------------------------------------------

func TestConformanceCustomMessageTypesViaConvertToLlm(t *testing.T) {
	compaction := agentcore.CompactionMessage{
		RoleField: agentcore.RoleCompaction,
		Summary:   "older history",
	}
	run := func(t *testing.T, convert func(agentcore.MessageList) agentcore.MessageList) ([]recordedRequest, int) {
		t.Helper()
		var convertedCount int
		cfg := RunConfig{
			LoopConfig:  LoopConfig{Model: "mock", Provider: "mock"},
			EventBuffer: 8,
		}
		if convert != nil {
			cfg.ConvertToLlm = func(msgs agentcore.MessageList) agentcore.MessageList {
				convertedCount = len(msgs)
				return convert(msgs)
			}
		}
		streamFn, requests := scriptedStream(t, textAssistant("Response", agentcore.StopReasonEndTurn))
		cfg.LoopConfig.Stream = streamFn
		agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{
			compaction,
			userMessage("Hello"),
		}}
		_, _ = runScenario(t, cfg, agentCtx)
		return requests(), convertedCount
	}

	requests, converted := run(t, func(msgs agentcore.MessageList) agentcore.MessageList {
		out := make(agentcore.MessageList, 0, len(msgs))
		for _, m := range msgs {
			if cm, ok := m.(agentcore.CompactionMessage); ok {
				out = append(out, cm.AsUserMessage())
				continue
			}
			out = append(out, m)
		}
		return out
	})
	if len(requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(requests))
	}
	if roles := messageRoles(requests[0].LLM.Messages); !reflect.DeepEqual(roles, []string{"user", "user"}) {
		t.Fatalf("request roles = %v, want both messages converted to user", roles)
	}
	if converted != 2 {
		t.Fatalf("convertToLlm saw %d messages, want 2 (it handles the custom kind)", converted)
	}
	if text := agentcore.ContentToText(requests[0].LLM.Messages[0].(agentcore.UserMessage).Content); !strings.Contains(text, "older history") {
		t.Fatalf("converted compaction text = %q", text)
	}

	// Control: without convertToLlm the custom kind reaches the provider raw, so
	// the hook — not the loop — is what makes custom kinds safe.
	raw, _ := run(t, nil)
	if len(raw) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(raw))
	}
	if roles := messageRoles(raw[0].LLM.Messages); !reflect.DeepEqual(roles, []string{agentcore.RoleCompaction, agentcore.RoleUser}) {
		t.Fatalf("identity request roles = %v, want the custom kind passed through", roles)
	}
}

// ---------------------------------------------------------------------------
// 13. Host-level settle lifecycle: agent_before_settle / agent_settled.
//
// PI: packages/coding-agent (session + extension layer), docs/extensions.md
//     "agent_start / agent_end / agent_before_settle / agent_settled".
// These are NOT part of PI's core AgentEvent (packages/agent/src/types.ts ends
// at agent_end); the loop emits them only when RunConfig.EmitSettleEvents is
// true. The default-off case is asserted below.
// ---------------------------------------------------------------------------

func TestConformanceAgentLifecycleSettleEvents(t *testing.T) {
	t.Run("the kernel default does not emit settle events", func(t *testing.T) {
		streamFn, _ := scriptedStream(t, textAssistant("done", agentcore.StopReasonEndTurn))
		cfg := RunConfig{
			LoopConfig:  LoopConfig{Model: "mock", Provider: "mock", Stream: streamFn},
			EventBuffer: 8,
		}
		agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("hi")}}

		events, _ := runScenario(t, cfg, agentCtx)

		want := []string{
			"agent_start", "turn_start", "message_start", "message_update", "message_end",
			"turn_end", "agent_end",
		}
		if got := piEventTypes(events); !reflect.DeepEqual(got, want) {
			t.Fatalf("default event sequence = %v, want PI core %v", got, want)
		}
	})

	t.Run("emits agent_end then agent_before_settle then agent_settled", func(t *testing.T) {
		streamFn, _ := scriptedStream(t, textAssistant("done", agentcore.StopReasonEndTurn))
		cfg := RunConfig{
			LoopConfig:       LoopConfig{Model: "mock", Provider: "mock", Stream: streamFn},
			EventBuffer:      8,
			EmitSettleEvents: true,
		}
		agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("hi")}}

		events, _ := runScenario(t, cfg, agentCtx)

		var tail []agentcore.AgentEvent
		for _, ev := range events {
			switch ev.(type) {
			case agentcore.AgentEndEvent, agentcore.AgentBeforeSettleEvent, agentcore.AgentSettledEvent:
				tail = append(tail, ev)
			}
		}
		if len(tail) != 3 {
			t.Fatalf("settle-related events = %d, want 3: %v", len(tail), piEventTypes(tail))
		}
		if got, want := tail[0].EventType(), "agent_end"; got != want {
			t.Fatalf("first settle event = %q, want %q", got, want)
		}
		before, ok := tail[1].(agentcore.AgentBeforeSettleEvent)
		if !ok {
			t.Fatalf("second settle event = %T, want AgentBeforeSettleEvent", tail[1])
		}
		if before.EventType() != "agent_before_settle" {
			t.Fatalf("event type = %q, want agent_before_settle", before.EventType())
		}
		if before.Outcome != agentcore.SettleOutcomeCompleted {
			t.Fatalf("outcome = %q, want completed", before.Outcome)
		}
		settled, ok := tail[2].(agentcore.AgentSettledEvent)
		if !ok {
			t.Fatalf("third settle event = %T, want AgentSettledEvent", tail[2])
		}
		if settled.EventType() != "agent_settled" {
			t.Fatalf("event type = %q, want agent_settled", settled.EventType())
		}
		if settled.Aborted {
			t.Fatal("settled.Aborted = true, want false")
		}
	})

	t.Run("an aborted run skips agent_before_settle", func(t *testing.T) {
		aborted := textAssistant("", agentcore.StopReasonAborted)
		streamFn, _ := scriptedStream(t, aborted)
		cfg := RunConfig{
			LoopConfig:       LoopConfig{Model: "mock", Provider: "mock", Stream: streamFn},
			EventBuffer:      8,
			EmitSettleEvents: true,
		}
		agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("hi")}}

		events, _ := runScenario(t, cfg, agentCtx)

		var sawEnd, sawBefore, sawSettled bool
		for _, ev := range events {
			switch e := ev.(type) {
			case agentcore.AgentEndEvent:
				sawEnd = true
			case agentcore.AgentBeforeSettleEvent:
				sawBefore = true
			case agentcore.AgentSettledEvent:
				sawSettled = true
				if !e.Aborted {
					t.Fatal("agent_settled.Aborted = false, want true")
				}
			}
		}
		if !sawEnd || !sawSettled {
			t.Fatalf("agent_end/agent_settled = %v/%v, want both", sawEnd, sawSettled)
		}
		if sawBefore {
			t.Fatal("aborted run emitted agent_before_settle, want it skipped")
		}
	})

	t.Run("the boundary can request one more turn", func(t *testing.T) {
		var boundaries atomic.Int32
		cfg := RunConfig{
			LoopConfig: LoopConfig{Model: "mock", Provider: "mock"},
			OnBeforeSettle: func(_ context.Context, agentCtx *agentcore.AgentContext) bool {
				if boundaries.Add(1) > 1 {
					return false
				}
				agentCtx.Messages = append(agentCtx.Messages, userMessage("continue"))
				return true
			},
			EventBuffer:      8,
			EmitSettleEvents: true,
		}
		streamFn, requests := scriptedStream(t,
			textAssistant("one", agentcore.StopReasonEndTurn),
			textAssistant("two", agentcore.StopReasonEndTurn),
		)
		cfg.LoopConfig.Stream = streamFn
		agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("run")}}

		events, result := runScenario(t, cfg, agentCtx)

		if got := len(requests()); got != 2 {
			t.Fatalf("provider calls = %d, want 2 (one continuation)", got)
		}
		if users := requestUserTexts(requests()[1].LLM); !reflect.DeepEqual(users, []string{"run", "continue"}) {
			t.Fatalf("second request users = %v, want [run continue]", users)
		}
		if got := boundaries.Load(); got != 2 {
			t.Fatalf("settle boundaries = %d, want 2", got)
		}
		types := piEventTypes(events)
		// Two low-level runs: each ends with agent_end + agent_before_settle, and
		// only the last settles.
		want := []string{
			"agent_start", "turn_start", "message_start", "message_update", "message_end", "turn_end",
			"agent_end", "agent_before_settle",
			"agent_start", "turn_start", "message_start", "message_update", "message_end", "turn_end",
			"agent_end", "agent_before_settle", "agent_settled",
		}
		if !reflect.DeepEqual(types, want) {
			t.Fatalf("event sequence = %v, want %v", types, want)
		}
		if roles := messageRoles(result); !reflect.DeepEqual(roles, []string{"assistant", "user", "assistant"}) {
			t.Fatalf("result roles = %v", roles)
		}
	})
}
