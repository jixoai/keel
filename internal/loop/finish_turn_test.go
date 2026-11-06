// Conformance tests ported from pi's packages/agent/test/agent-loop.test.ts
// around finishTurn and declareToolChanges (pin 55a5c69, MIT License). Titles
// keep pi's it(...) wording.

package loop

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/provider"
)

// runObserved drives a run and hands every event to observe while collecting it.
func runObserved(t *testing.T, cfg RunConfig, agentCtx *agentcore.AgentContext, observe func(agentcore.AgentEvent)) ([]agentcore.AgentEvent, []agentcore.AgentMessage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := StartRun(ctx, agentCtx, cfg)
	var events []agentcore.AgentEvent
	for ev := range stream.Events() {
		events = append(events, ev)
		if observe != nil {
			observe(ev)
		}
	}
	result, err := stream.Result(ctx)
	if err != nil {
		t.Fatalf("stream result: %v", err)
	}
	return events, result
}

// PI: it("runs finishTurn after tool-result messages and before turn_end")
func TestConformanceRunsFinishTurnAfterToolResultMessagesAndBeforeTurnEnd(t *testing.T) {
	tool := funcTool{name: "echo", schema: echoSchema, run: func(_ context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		return agentcore.AgentToolResult{Content: textContent("echoed")}, nil
	}}
	var (
		mu       sync.Mutex
		ordering []string
	)
	cfg := RunConfig{
		LoopConfig: LoopConfig{Model: "mock", Provider: "mock"},
		Batch: agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{
			Registry: registryOf(t, tool),
		}},
		FinishTurn: func(_ context.Context, turn AgentTurnContext) *AgentTurnDecision {
			mu.Lock()
			ordering = append(ordering, "finishTurn")
			mu.Unlock()
			if len(turn.ToolResults) != 1 {
				t.Errorf("finishTurn tool results = %d, want 1", len(turn.ToolResults))
			}
			if turn.Context.Messages[len(turn.Context.Messages)-1].Role() != agentcore.RoleToolResult {
				t.Errorf("finishTurn context tail = %q, want toolResult", turn.Context.Messages[len(turn.Context.Messages)-1].Role())
			}
			return &AgentTurnDecision{Action: TurnActionEnd}
		},
		// Unbuffered: the loop advances only when the observer consumed an event,
		// so hook/event interleaving matches the on-stream order.
		EventBuffer: 0,
	}
	streamFn, _ := scriptedStream(t, toolUseAssistant(call("tool-1", "echo", `{"value":"hello"}`)))
	cfg.LoopConfig.Stream = streamFn
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("echo")}}

	events, _ := runObserved(t, cfg, agentCtx, func(ev agentcore.AgentEvent) {
		mu.Lock()
		switch e := ev.(type) {
		case agentcore.MessageEndEvent:
			ordering = append(ordering, "message_end:"+e.Message.Role())
		case agentcore.TurnEndEvent:
			ordering = append(ordering, "turn_end")
		}
		mu.Unlock()
	})

	mu.Lock()
	defer mu.Unlock()
	if !containsString(ordering, "finishTurn") {
		t.Fatal("finishTurn was not called")
	}
	// The stream order pins the contract: the tool-result message_end precedes
	// turn_end, and finishTurn runs after the tool result entered the turn
	// context (the hook asserts that above).
	toolResultAt, turnEndAt := -1, -1
	for i, ev := range events {
		if e, ok := ev.(agentcore.MessageEndEvent); ok && e.Message.Role() == agentcore.RoleToolResult {
			toolResultAt = i
		}
		if _, ok := ev.(agentcore.TurnEndEvent); ok {
			turnEndAt = i
		}
	}
	if toolResultAt < 0 || turnEndAt < 0 || toolResultAt > turnEndAt {
		t.Fatalf("tool-result message_end index = %d, turn_end index = %d", toolResultAt, turnEndAt)
	}
}

// PI: it.each(["error", "aborted"])("runs finishTurn for a %s assistant before
// turn_end without changing the hard exit")
func TestConformanceRunsFinishTurnForTerminalAssistantBeforeTurnEnd(t *testing.T) {
	for _, reason := range []string{agentcore.StopReasonError, agentcore.StopReasonAborted} {
		t.Run(reason, func(t *testing.T) {
			var (
				mu            sync.Mutex
				ordering      []string
				providerCalls int
				steeringPolls int
				followUpPolls int
			)
			cfg := RunConfig{
				LoopConfig: LoopConfig{Model: "mock", Provider: "mock"},
				FinishTurn: func(_ context.Context, turn AgentTurnContext) *AgentTurnDecision {
					if turn.Message.StopReason != reason {
						t.Errorf("finishTurn stopReason = %q, want %q", turn.Message.StopReason, reason)
					}
					mu.Lock()
					ordering = append(ordering, "finishTurn")
					mu.Unlock()
					return &AgentTurnDecision{Action: TurnActionContinue}
				},
				GetSteeringMessages: func(context.Context) []agentcore.AgentMessage {
					mu.Lock()
					steeringPolls++
					mu.Unlock()
					return nil
				},
				GetFollowUpMessages: func(context.Context, *agentcore.AgentContext) []agentcore.AgentMessage {
					mu.Lock()
					followUpPolls++
					mu.Unlock()
					return []agentcore.AgentMessage{userMessage("queued")}
				},
				EventBuffer: 8,
			}
			streamFn, _ := scriptedStream(t, agentcore.AssistantMessage{
				RoleField: agentcore.RoleAssistant, StopReason: reason, ErrorMessage: reason,
			})
			cfg.LoopConfig.Stream = func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
				providerCalls++
				return streamFn(ctx, model, llm, sc)
			}
			agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("run")}}

			_, _ = runObserved(t, cfg, agentCtx, func(ev agentcore.AgentEvent) {
				if ev.EventType() == agentcore.EventTurnEnd {
					mu.Lock()
					ordering = append(ordering, "turn_end")
					mu.Unlock()
				}
			})

			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(ordering, []string{"finishTurn", "turn_end"}) {
				t.Fatalf("ordering = %v", ordering)
			}
			if providerCalls != 1 {
				t.Fatalf("provider calls = %d, want 1", providerCalls)
			}
			if steeringPolls != 1 {
				t.Fatalf("steering polls = %d, want 1", steeringPolls)
			}
			if followUpPolls != 0 {
				t.Fatalf("follow-up polls = %d, want 0", followUpPolls)
			}
		})
	}
}

// PI: it("action:end skips queue polling and next-turn preparation")
func TestConformanceActionEndSkipsQueuePollingAndNextTurnPreparation(t *testing.T) {
	tool := funcTool{name: "noop", schema: `{"type":"object"}`, run: func(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		return agentcore.AgentToolResult{Content: textContent("done")}, nil
	}}
	var (
		mu               sync.Mutex
		providerCalls    int
		steeringPolls    int
		followUpPolls    int
		prepareNextCalls int
	)
	cfg := RunConfig{
		LoopConfig: LoopConfig{Model: "mock", Provider: "mock"},
		Batch: agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{
			Registry: registryOf(t, tool),
		}},
		FinishTurn: func(context.Context, AgentTurnContext) *AgentTurnDecision {
			return &AgentTurnDecision{Action: TurnActionEnd}
		},
		PrepareNextTurn: func(context.Context, *agentcore.AgentContext) *TurnUpdate {
			mu.Lock()
			prepareNextCalls++
			mu.Unlock()
			return nil
		},
		GetSteeringMessages: func(context.Context) []agentcore.AgentMessage {
			mu.Lock()
			steeringPolls++
			mu.Unlock()
			return nil
		},
		GetFollowUpMessages: func(context.Context, *agentcore.AgentContext) []agentcore.AgentMessage {
			mu.Lock()
			followUpPolls++
			mu.Unlock()
			return []agentcore.AgentMessage{userMessage("queued")}
		},
		EventBuffer: 8,
	}
	streamFn, _ := scriptedStream(t, toolUseAssistant(call("tool-1", "noop", `{}`)))
	cfg.LoopConfig.Stream = func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		providerCalls++
		return streamFn(ctx, model, llm, sc)
	}
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("run")}}

	_, _ = runObserved(t, cfg, agentCtx, nil)

	mu.Lock()
	defer mu.Unlock()
	if providerCalls != 1 {
		t.Fatalf("provider calls = %d, want 1", providerCalls)
	}
	if steeringPolls != 1 {
		t.Fatalf("steering polls = %d, want 1 (startup only)", steeringPolls)
	}
	if followUpPolls != 0 {
		t.Fatalf("follow-up polls = %d, want 0", followUpPolls)
	}
	if prepareNextCalls != 0 {
		t.Fatalf("prepareNextTurn calls = %d, want 0", prepareNextCalls)
	}
}

// PI: it("makes exactly one context-only request when no natural request
// satisfies continuation")
func TestConformanceMakesExactlyOneContextOnlyRequestForContinuation(t *testing.T) {
	var (
		mu            sync.Mutex
		providerCalls int
		finishCalls   int
	)
	cfg := RunConfig{
		LoopConfig: LoopConfig{Model: "mock", Provider: "mock"},
		FinishTurn: func(context.Context, AgentTurnContext) *AgentTurnDecision {
			mu.Lock()
			defer mu.Unlock()
			finishCalls++
			if finishCalls == 1 {
				return &AgentTurnDecision{Action: TurnActionContinue}
			}
			return nil
		},
		EventBuffer: 8,
	}
	streamFn, _ := scriptedStream(t,
		textAssistant("response 1", agentcore.StopReasonEndTurn),
		textAssistant("response 2", agentcore.StopReasonEndTurn),
	)
	cfg.LoopConfig.Stream = func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		mu.Lock()
		providerCalls++
		mu.Unlock()
		return streamFn(ctx, model, llm, sc)
	}
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("run")}}

	_, _ = runObserved(t, cfg, agentCtx, nil)

	mu.Lock()
	defer mu.Unlock()
	if providerCalls != 2 {
		t.Fatalf("provider calls = %d, want 2", providerCalls)
	}
	if finishCalls != 2 {
		t.Fatalf("finishTurn calls = %d, want 2", finishCalls)
	}
}

// PI: it("lets a natural tool-result request satisfy continuation")
func TestConformanceLetsNaturalToolResultRequestSatisfyContinuation(t *testing.T) {
	tool := funcTool{name: "noop", schema: `{"type":"object"}`, run: func(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		return agentcore.AgentToolResult{Content: textContent("done")}, nil
	}}
	var (
		mu            sync.Mutex
		providerCalls int
		finishCalls   int
	)
	cfg := RunConfig{
		LoopConfig: LoopConfig{Model: "mock", Provider: "mock"},
		Batch: agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{
			Registry: registryOf(t, tool),
		}},
		FinishTurn: func(context.Context, AgentTurnContext) *AgentTurnDecision {
			mu.Lock()
			defer mu.Unlock()
			finishCalls++
			if finishCalls == 1 {
				return &AgentTurnDecision{Action: TurnActionContinue}
			}
			return nil
		},
		EventBuffer: 8,
	}
	streamFn, _ := scriptedStream(t,
		toolUseAssistant(call("tool-1", "noop", `{}`)),
		textAssistant("done", agentcore.StopReasonEndTurn),
	)
	cfg.LoopConfig.Stream = func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		mu.Lock()
		providerCalls++
		mu.Unlock()
		return streamFn(ctx, model, llm, sc)
	}
	agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("run")}}

	_, _ = runObserved(t, cfg, agentCtx, nil)

	mu.Lock()
	defer mu.Unlock()
	if providerCalls != 2 {
		t.Fatalf("provider calls = %d, want 2", providerCalls)
	}
	if finishCalls != 2 {
		t.Fatalf("finishTurn calls = %d, want 2", finishCalls)
	}
}

// PI: it.each(["steering", "follow-up"])("lets a natural %s request satisfy continuation")
func TestConformanceLetsNaturalQueueRequestSatisfyContinuation(t *testing.T) {
	for _, kind := range []string{"steering", "follow-up"} {
		t.Run(kind, func(t *testing.T) {
			var (
				mu            sync.Mutex
				providerCalls int
				finishCalls   int
				steeringPolls int
				delivered     bool
			)
			queued := userMessage(kind)
			cfg := RunConfig{
				LoopConfig: LoopConfig{Model: "mock", Provider: "mock"},
				FinishTurn: func(context.Context, AgentTurnContext) *AgentTurnDecision {
					mu.Lock()
					defer mu.Unlock()
					finishCalls++
					if finishCalls == 1 {
						return &AgentTurnDecision{Action: TurnActionContinue}
					}
					return nil
				},
				GetSteeringMessages: func(context.Context) []agentcore.AgentMessage {
					mu.Lock()
					defer mu.Unlock()
					steeringPolls++
					if kind == "steering" && steeringPolls == 2 {
						return []agentcore.AgentMessage{queued}
					}
					return nil
				},
				GetFollowUpMessages: func(context.Context, *agentcore.AgentContext) []agentcore.AgentMessage {
					mu.Lock()
					defer mu.Unlock()
					if kind != "follow-up" || delivered {
						return nil
					}
					delivered = true
					return []agentcore.AgentMessage{queued}
				},
				EventBuffer: 8,
			}
			var secondUsers []string
			streamFn, requests := scriptedStream(t,
				textAssistant("one", agentcore.StopReasonEndTurn),
				textAssistant("two", agentcore.StopReasonEndTurn),
			)
			cfg.LoopConfig.Stream = func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
				mu.Lock()
				providerCalls++
				callNo := providerCalls
				mu.Unlock()
				if callNo == 2 {
					secondUsers = requestUserTexts(llm)
				}
				return streamFn(ctx, model, llm, sc)
			}
			agentCtx := &agentcore.AgentContext{Messages: agentcore.MessageList{userMessage("run")}}

			_, _ = runObserved(t, cfg, agentCtx, nil)

			mu.Lock()
			defer mu.Unlock()
			if providerCalls != 2 {
				t.Fatalf("provider calls = %d, want 2", providerCalls)
			}
			if finishCalls != 2 {
				t.Fatalf("finishTurn calls = %d, want 2", finishCalls)
			}
			if len(requests()) != 2 {
				t.Fatalf("requests = %d, want 2", len(requests()))
			}
			if !reflect.DeepEqual(secondUsers, []string{"run", kind}) {
				t.Fatalf("second request users = %v, want [run %s]", secondUsers, kind)
			}
		})
	}
}

// PI: it("action:end receives finalized turn context and stops before queue polling")
func TestConformanceActionEndReceivesFinalizedTurnContextAndStopsBeforeQueuePolling(t *testing.T) {
	tool := funcTool{name: "echo", schema: echoSchema, run: func(_ context.Context, _ string, _ json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		return agentcore.AgentToolResult{Content: textContent("echoed")}, nil
	}}
	var (
		mu              sync.Mutex
		llmCalls        int
		steeringPolls   int
		followUpPolls   int
		callbackToolIDs []string
		callbackRoles   []string
	)
	cfg := RunConfig{
		LoopConfig: LoopConfig{Model: "mock", Provider: "mock"},
		Batch: agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{
			Registry: registryOf(t, tool),
		}},
		FinishTurn: func(_ context.Context, turn AgentTurnContext) *AgentTurnDecision {
			callbackToolIDs = toolResultIDs(turn.ToolResults)
			callbackRoles = messageRoles(turn.Context.Messages)
			return &AgentTurnDecision{Action: TurnActionEnd}
		},
		GetSteeringMessages: func(context.Context) []agentcore.AgentMessage {
			mu.Lock()
			steeringPolls++
			mu.Unlock()
			return nil
		},
		GetFollowUpMessages: func(context.Context, *agentcore.AgentContext) []agentcore.AgentMessage {
			mu.Lock()
			followUpPolls++
			mu.Unlock()
			return []agentcore.AgentMessage{userMessage("follow up should stay queued")}
		},
		EventBuffer: 8,
		PromptCount: 1,
	}
	streamFn, _ := scriptedStream(t, toolUseAssistant(call("tool-1", "echo", `{"value":"hello"}`)))
	cfg.LoopConfig.Stream = func(ctx context.Context, model string, llm provider.LlmContext, sc provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		mu.Lock()
		llmCalls++
		mu.Unlock()
		return streamFn(ctx, model, llm, sc)
	}
	agentCtx := &agentcore.AgentContext{
		Messages: agentcore.MessageList{userMessage("echo something")},
		Tools:    registryTools(t, tool),
	}

	events, result := runObserved(t, cfg, agentCtx, nil)

	mu.Lock()
	defer mu.Unlock()
	if llmCalls != 1 {
		t.Fatalf("provider calls = %d, want 1", llmCalls)
	}
	if steeringPolls != 1 || followUpPolls != 0 {
		t.Fatalf("polls = steering:%d followUp:%d, want 1/0", steeringPolls, followUpPolls)
	}
	if !reflect.DeepEqual(callbackToolIDs, []string{"tool-1"}) {
		t.Fatalf("callback tool ids = %v", callbackToolIDs)
	}
	// The context declares its tools, so the leading system declaration is
	// present before the prompt (pi's declareToolChanges).
	if want := []string{"system", "user", "assistant", "toolResult"}; !reflect.DeepEqual(callbackRoles, want) {
		t.Fatalf("callback context roles = %v, want %v", callbackRoles, want)
	}
	if roles := messageRoles(result); !reflect.DeepEqual(roles, []string{"system", "user", "assistant", "toolResult"}) {
		t.Fatalf("result roles = %v", roles)
	}
	var types []string
	for _, ev := range events {
		if ev.EventType() == agentcore.EventTelemetry {
			continue
		}
		types = append(types, ev.EventType())
	}
	want := []string{
		"agent_start",
		"message_start", "message_end", // system tool declaration (prompt tail)
		"message_start", "message_end", // user prompt
		"turn_start",
		"message_start", "message_update", "message_end", // assistant
		"tool_execution_start", "tool_execution_end",
		"message_start", "message_end", // tool result
		"turn_end",
		"agent_end",
	}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("event sequence = %v, want %v", types, want)
	}
}

// PI: it("should build provider context exclusively from transcript messages")
// Adapted: the tool declaration lives in the transcript, so the provider sees a
// leading system message carrying toolsAdded before the user prompt.
func TestConformanceDeclaresToolLoadoutBeforeFirstRequest(t *testing.T) {
	tool := funcTool{name: "echo", schema: echoSchema, run: func(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		return agentcore.AgentToolResult{Content: textContent("ok")}, nil
	}}
	streamFn, requests := scriptedStream(t, textAssistant("done", agentcore.StopReasonEndTurn))
	cfg := RunConfig{
		LoopConfig:  LoopConfig{Model: "mock", Provider: "mock", Stream: streamFn},
		EventBuffer: 8,
		PromptCount: 1,
	}
	agentCtx := &agentcore.AgentContext{
		Messages: agentcore.MessageList{userMessage("Hello")},
		Tools:    registryTools(t, tool),
	}
	events, result := runObserved(t, cfg, agentCtx, nil)

	reqs := requests()
	if len(reqs) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(reqs))
	}
	if roles := messageRoles(reqs[0].LLM.Messages); !reflect.DeepEqual(roles, []string{"system", "user"}) {
		t.Fatalf("request roles = %v, want [system user]", roles)
	}
	sys, ok := reqs[0].LLM.Messages[0].(agentcore.SystemMessage)
	if !ok || len(sys.ToolsAdded) != 1 || sys.ToolsAdded[0].Name != "echo" {
		t.Fatalf("declaration = %#v", reqs[0].LLM.Messages[0])
	}
	if roles := messageRoles(result); !reflect.DeepEqual(roles, []string{"system", "user", "assistant"}) {
		t.Fatalf("result roles = %v, want the declaration + prompt + assistant", roles)
	}
	var sawDeclaration bool
	for _, ev := range events {
		if e, ok := ev.(agentcore.MessageEndEvent); ok && e.Message.Role() == agentcore.RoleSystem {
			sawDeclaration = true
		}
	}
	if !sawDeclaration {
		t.Fatal("no message_end for the tool declaration")
	}
}

// registryTools lists a registry's tools in deterministic order.
func registryTools(t *testing.T, tools ...agentcore.AgentTool) []agentcore.AgentTool {
	t.Helper()
	reg := registryOf(t, tools...)
	return reg.List()
}
