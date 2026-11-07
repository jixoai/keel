package agentlooptest

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
)

// caseToolCallResults
// PI: it("should handle tool calls and results") (packages/agent/test/agent-loop.test.ts)
//
// The call is executed once, the result is paired by id, the model sees the
// result on the next request, and the produced transcript holds exactly the
// prompt, the call, its result and the final reply.
func caseToolCallResults(t T, e *env) {
	tool := &echoTool{}
	prov := e.scripted(
		assistantToolUse(toolCall("call-1", "echo", `{"value":"hello"}`)),
		assistantText("done", agentcore.StopReasonEndTurn),
	)
	in := promptInput("echo something")
	in.Context.Tools = []agentcore.AgentTool{tool}
	in.Context.ConvertToLlm = dropSystemMessages

	events, result := runLoop(t, e.loop(prov), in)

	checkComplete(t, "tool call results", events, result, true)
	if got := tool.executions(); got != 1 {
		t.Fatalf("echo executions = %d, want 1", got)
	}
	var end *agentcore.ToolExecutionEndEvent
	for i := range events {
		if ev, ok := events[i].(agentcore.ToolExecutionEndEvent); ok {
			end = &ev
		}
	}
	if end == nil {
		t.Fatalf("no tool_execution_end event; events = %v", piEventTypes(events))
	}
	if end.ToolCallID != "call-1" || end.IsError {
		t.Fatalf("tool_execution_end = %+v, want a successful call-1", end)
	}
	assertShapesEqual(t, "produced transcript", nonSystemShapes(result), []msgShape{
		{Role: agentcore.RoleUser, Text: "echo something"},
		{Role: agentcore.RoleAssistant, StopReason: agentcore.StopReasonToolUse,
			Calls: []string{`call-1|echo|{"value":"hello"}`}},
		{Role: agentcore.RoleToolResult, Text: "echoed: hello", ToolCallID: "call-1", ToolName: "echo"},
		{Role: agentcore.RoleAssistant, Text: "done", StopReason: agentcore.StopReasonEndTurn},
	})

	reqs := prov.seenRequests()
	if len(reqs) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(reqs))
	}
	for i, req := range reqs {
		for _, m := range req.Messages {
			if m.Role() == agentcore.RoleSystem {
				t.Fatalf("request %d carried a transcript system message despite ConvertToLlm: %v", i, shapesOf(req.Messages))
			}
		}
	}
	assertShapesEqual(t, "first request transcript", shapesOf(reqs[0].Messages), []msgShape{
		{Role: agentcore.RoleUser, Text: "echo something"},
	})
	assertShapesEqual(t, "second request transcript", shapesOf(reqs[1].Messages), []msgShape{
		{Role: agentcore.RoleUser, Text: "echo something"},
		{Role: agentcore.RoleAssistant, StopReason: agentcore.StopReasonToolUse,
			Calls: []string{`call-1|echo|{"value":"hello"}`}},
		{Role: agentcore.RoleToolResult, Text: "echoed: hello", ToolCallID: "call-1", ToolName: "echo"},
	})
	// The executable tool reached the provider as the model-facing declaration.
	if got := reqs[1].Tools; len(got) != 1 || got[0].Name() != "echo" {
		t.Fatalf("second request tools = %v, want the echo declaration", got)
	}
}

// caseToolCompletionOrder
// PI: it("should emit tool_execution_end in completion order but persist tool results in source order")
// (packages/agent/test/agent-loop.test.ts)
//
// Parallel tools finish out of source order, but the persisted transcript keeps
// the assistant's call order.
func caseToolCompletionOrder(t T, e *env) {
	var firstFinished, parallelObserved atomic.Bool
	release := make(chan struct{})
	var releaseOnce sync.Once
	tool := funcTool{name: "echo", schema: echoSchema, mode: agentcore.ToolExecutionParallel, run: func(_ context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
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
		return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("echoed: " + in.Value)}}, nil
	}}
	prov := e.scripted(
		assistantToolUse(
			toolCall("tool-1", "echo", `{"value":"first"}`),
			toolCall("tool-2", "echo", `{"value":"second"}`),
		),
		assistantText("done", agentcore.StopReasonEndTurn),
	)
	in := promptInput("echo both")
	in.Context.Tools = []agentcore.AgentTool{tool}
	in.Context.ConvertToLlm = dropSystemMessages

	ctx, cancel := context.WithTimeout(context.Background(), caseTimeout)
	defer cancel()
	stream, err := e.loop(prov).Run(ctx, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var events []agentcore.AgentEvent
	for ev := range stream.Events() {
		events = append(events, ev)
		if end, ok := ev.(agentcore.ToolExecutionEndEvent); ok && end.ToolCallID == "tool-2" {
			releaseOnce.Do(func() { close(release) })
		}
	}
	result, err := stream.Result(ctx)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}

	checkComplete(t, "tool completion order", events, result, true)
	if !parallelObserved.Load() {
		t.Fatalf("the second tool did not run before the first finished; the batch was not parallel")
	}
	var endIDs []string
	for _, ev := range events {
		if end, ok := ev.(agentcore.ToolExecutionEndEvent); ok {
			endIDs = append(endIDs, end.ToolCallID)
		}
	}
	if want := []string{"tool-2", "tool-1"}; !reflect.DeepEqual(endIDs, want) {
		t.Fatalf("tool_execution_end order = %v, want completion order %v", endIDs, want)
	}
	var persisted []string
	for _, m := range result {
		if tr, ok := m.(agentcore.ToolResultMessage); ok {
			persisted = append(persisted, tr.ToolCallID)
		}
	}
	if want := []string{"tool-1", "tool-2"}; !reflect.DeepEqual(persisted, want) {
		t.Fatalf("persisted tool results = %v, want source order %v", persisted, want)
	}
	var turnResultIDs []string
	for _, ev := range events {
		if end, ok := ev.(agentcore.TurnEndEvent); ok && len(end.ToolResults) > 0 {
			for _, tr := range end.ToolResults {
				turnResultIDs = append(turnResultIDs, tr.ToolCallID)
			}
		}
	}
	if want := []string{"tool-1", "tool-2"}; !reflect.DeepEqual(turnResultIDs, want) {
		t.Fatalf("turn_end tool results = %v, want source order %v", turnResultIDs, want)
	}
}

// caseToolTruncatedAssistant
// PI: it("should not execute tool calls from a length-truncated assistant message")
// (packages/agent/test/agent-loop.test.ts)
//
// A truncated response never executes its call; the call resolves to an error
// result and the loop continues so the model can re-issue it.
func caseToolTruncatedAssistant(t T, e *env) {
	tool := &echoTool{}
	truncated := assistantToolUse(toolCall("tool-1", "echo", `{"value":"hel"}`))
	truncated.StopReason = agentcore.StopReasonLength
	prov := e.scripted(truncated, assistantText("done", agentcore.StopReasonEndTurn))
	in := promptInput("echo something")
	in.Context.Tools = []agentcore.AgentTool{tool}
	in.Context.ConvertToLlm = dropSystemMessages

	events, result := runLoop(t, e.loop(prov), in)

	checkComplete(t, "truncated assistant", events, result, true)
	if got := tool.executions(); got != 0 {
		t.Fatalf("tool executions = %d, want 0 (the truncated call must not run)", got)
	}
	var end *agentcore.ToolExecutionEndEvent
	for i := range events {
		if ev, ok := events[i].(agentcore.ToolExecutionEndEvent); ok {
			end = &ev
		}
	}
	if end == nil || !end.IsError {
		t.Fatalf("tool_execution_end = %+v, want an error end for the truncated call", end)
	}
	var failed *agentcore.ToolResultMessage
	for _, m := range result {
		if tr, ok := m.(agentcore.ToolResultMessage); ok {
			failed = &tr
		}
	}
	if failed == nil || !failed.IsError || failed.ToolCallID != "tool-1" {
		t.Fatalf("persisted tool result = %+v, want an error result for tool-1", failed)
	}
	if got := prov.callCount(); got != 2 {
		t.Fatalf("provider calls = %d, want 2 (the loop continues after truncation)", got)
	}
	shapes := nonSystemShapes(result)
	if len(shapes) != 4 {
		t.Fatalf("produced transcript = %v, want prompt, truncated call, error result, reply", shapes)
	}
	if shapes[1].Role != agentcore.RoleAssistant || shapes[1].StopReason != agentcore.StopReasonLength {
		t.Fatalf("truncated assistant shape = %+v, want stopReason length", shapes[1])
	}
	if shapes[3].Text != "done" || shapes[3].StopReason != agentcore.StopReasonEndTurn {
		t.Fatalf("final reply = %+v, want done/end_turn", shapes[3])
	}
}

// caseToolMutatedArgs
// PI: it("should execute mutated beforeToolCall args without revalidation")
// (packages/agent/test/agent-loop.test.ts)
//
// The hook runs after validation: rewriting a schema-valid string into a
// schema-invalid number must reach execute() untouched.
func caseToolMutatedArgs(t T, e *env) {
	var mu sync.Mutex
	var executed []json.RawMessage
	tool := funcTool{name: "echo", schema: echoSchema, mode: agentcore.ToolExecutionParallel, run: func(_ context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		mu.Lock()
		executed = append(executed, append(json.RawMessage{}, args...))
		mu.Unlock()
		return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("echoed")}}, nil
	}}
	rewritten := json.RawMessage(`{"value":123}`)
	prov := e.scripted(
		assistantToolUse(toolCall("tool-1", "echo", `{"value":"hello"}`)),
		assistantText("done", agentcore.StopReasonEndTurn),
	)
	in := promptInput("echo something")
	in.Context.Tools = []agentcore.AgentTool{tool}
	in.Context.ConvertToLlm = dropSystemMessages
	in.Batch = agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{
		Registry: registryOf(t, tool),
		BeforeToolCall: func(context.Context, agentcore.AgentToolCall) *agentcore.BeforeToolCallDecision {
			return &agentcore.BeforeToolCallDecision{UpdatedInput: rewritten}
		},
	}}

	events, result := runLoop(t, e.loop(prov), in)

	checkComplete(t, "mutated args", events, result, true)
	mu.Lock()
	gotExecuted := append([]json.RawMessage{}, executed...)
	mu.Unlock()
	if want := []json.RawMessage{rewritten}; !reflect.DeepEqual(gotExecuted, want) {
		t.Fatalf("executed args = %v, want %v (the rewrite was re-validated)", gotExecuted, want)
	}
	for _, m := range result {
		if tr, ok := m.(agentcore.ToolResultMessage); ok && tr.IsError {
			t.Fatalf("tool result is an error (%q); the rewrite was rejected", agentcore.ContentToText(tr.Content))
		}
	}
}

// caseToolDurationExcludesHooks
// PI: it("records how long execute() took on the tool result, excluding hooks")
// (packages/agent/test/agent-loop.test.ts)
//
// DurationMs measures execute() only; a blocked call never ran and has none.
func caseToolDurationExcludesHooks(t T, e *env) {
	tool := funcTool{name: "echo", schema: echoSchema, mode: agentcore.ToolExecutionParallel, run: func(_ context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		var in struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return agentcore.AgentToolResult{}, err
		}
		time.Sleep(30 * time.Millisecond)
		return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent(in.Value)}}, nil
	}}
	blocked := agentcore.ContentList{agentcore.NewTextContent("no")}
	prov := e.scripted(
		assistantToolUse(
			toolCall("ran", "echo", `{"value":"a"}`),
			toolCall("blocked", "echo", `{"value":"b"}`),
		),
		assistantText("done", agentcore.StopReasonEndTurn),
	)
	in := promptInput("go")
	in.Context.Tools = []agentcore.AgentTool{tool}
	in.Context.ConvertToLlm = dropSystemMessages
	in.Batch = agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{
		Registry: registryOf(t, tool),
		BeforeToolCall: func(_ context.Context, call agentcore.AgentToolCall) *agentcore.BeforeToolCallDecision {
			time.Sleep(200 * time.Millisecond)
			if call.ID == "blocked" {
				return &agentcore.BeforeToolCallDecision{Block: true, Content: &blocked}
			}
			return nil
		},
	}}

	events, result := runLoop(t, e.loop(prov), in)
	checkComplete(t, "tool duration", events, result, true)

	byID := map[string]agentcore.ToolResultMessage{}
	for _, m := range result {
		if tr, ok := m.(agentcore.ToolResultMessage); ok {
			byID[tr.ToolCallID] = tr
		}
	}
	ran, ok := byID["ran"]
	if !ok {
		t.Fatalf("missing tool result for ran: %v", shapesOf(result))
	}
	if ran.DurationMs == nil {
		t.Fatalf("ran tool result has no durationMs")
	}
	if got := *ran.DurationMs; got < 20 || got >= 180 {
		t.Fatalf("ran durationMs = %d, want [20,180) — execute only, not the 200ms hook", got)
	}
	blockedResult, ok := byID["blocked"]
	if !ok {
		t.Fatalf("missing tool result for blocked: %v", shapesOf(result))
	}
	if !blockedResult.IsError {
		t.Fatalf("blocked tool result IsError = false, want true")
	}
	if blockedResult.DurationMs != nil {
		t.Fatalf("blocked tool result has durationMs = %d, want none (execute never ran)", *blockedResult.DurationMs)
	}
}

// caseTerminateWholeBatch
// PI: it("should stop after a tool batch when every tool result sets terminate=true")
// (packages/agent/test/agent-loop.test.ts)
func caseTerminateWholeBatch(t T, e *env) {
	terminate := true
	tool := funcTool{name: "echo", schema: echoSchema, mode: agentcore.ToolExecutionParallel, run: func(_ context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		return agentcore.AgentToolResult{
			Content:   agentcore.ContentList{agentcore.NewTextContent("echoed")},
			Terminate: &terminate,
		}, nil
	}}
	prov := e.scripted(assistantToolUse(toolCall("tool-1", "echo", `{"value":"hello"}`)))
	in := promptInput("echo something")
	in.Context.Tools = []agentcore.AgentTool{tool}
	in.Context.ConvertToLlm = dropSystemMessages

	events, result := runLoop(t, e.loop(prov), in)

	checkComplete(t, "terminate whole batch", events, result, true)
	if got := prov.callCount(); got != 1 {
		t.Fatalf("provider calls = %d, want 1 (the batch terminated the run)", got)
	}
	if got := countEvents(events, agentcore.EventTurnEnd); got != 1 {
		t.Fatalf("turn_end events = %d, want 1", got)
	}
	shapes := nonSystemShapes(result)
	if got, want := len(shapes), 3; got != want {
		t.Fatalf("produced transcript = %v, want assistant call + tool result (no follow-up turn)", shapes)
	}
	if shapes[2].Role != agentcore.RoleToolResult || shapes[2].IsError {
		t.Fatalf("tool result = %+v, want a successful result", shapes[2])
	}
}

// caseTerminateBlockedSingle
// PI: it("should stop after a blocked tool call when beforeToolCall sets terminate=true")
// (packages/agent/test/agent-loop.test.ts)
func caseTerminateBlockedSingle(t T, e *env) {
	var executed atomic.Bool
	tool := funcTool{name: "echo", schema: echoSchema, mode: agentcore.ToolExecutionParallel, run: func(_ context.Context, _ string, _ json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		executed.Store(true)
		return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("should not execute")}}, nil
	}}
	blocked := agentcore.ContentList{agentcore.NewTextContent("Blocked by policy")}
	prov := e.scripted(assistantToolUse(toolCall("tool-1", "echo", `{"value":"hello"}`)))
	in := promptInput("echo something")
	in.Context.Tools = []agentcore.AgentTool{tool}
	in.Context.ConvertToLlm = dropSystemMessages
	in.Batch = agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{
		Registry: registryOf(t, tool),
		BeforeToolCall: func(context.Context, agentcore.AgentToolCall) *agentcore.BeforeToolCallDecision {
			return &agentcore.BeforeToolCallDecision{Block: true, Content: &blocked, Terminate: true}
		},
	}}

	events, result := runLoop(t, e.loop(prov), in)

	checkComplete(t, "terminate blocked single", events, result, true)
	if executed.Load() {
		t.Fatalf("the blocked tool executed")
	}
	if got := prov.callCount(); got != 1 {
		t.Fatalf("provider calls = %d, want 1 (the blocked call terminated the run)", got)
	}
	var toolResult *agentcore.ToolResultMessage
	for _, m := range result {
		if tr, ok := m.(agentcore.ToolResultMessage); ok {
			toolResult = &tr
		}
	}
	if toolResult == nil {
		t.Fatalf("no tool result in %v", shapesOf(result))
	}
	if !toolResult.IsError {
		t.Fatalf("blocked tool result IsError = false, want true")
	}
	if text := agentcore.ContentToText(toolResult.Content); text != "Blocked by policy" {
		t.Fatalf("blocked reason = %q, want %q", text, "Blocked by policy")
	}
}

// caseTerminateMixedBatch
// PI: it("should continue after a mixed batch with one terminating blocked call")
// (packages/agent/test/agent-loop.test.ts)
func caseTerminateMixedBatch(t T, e *env) {
	var mu sync.Mutex
	executed := make([]string, 0, 2)
	tool := funcTool{name: "echo", schema: echoSchema, mode: agentcore.ToolExecutionParallel, run: func(_ context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		var in struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return agentcore.AgentToolResult{}, err
		}
		mu.Lock()
		executed = append(executed, in.Value)
		mu.Unlock()
		return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("echoed: " + in.Value)}}, nil
	}}
	blocked := agentcore.ContentList{agentcore.NewTextContent("Blocked first")}
	prov := e.scripted(
		assistantToolUse(
			toolCall("tool-1", "echo", `{"value":"first"}`),
			toolCall("tool-2", "echo", `{"value":"second"}`),
		),
		assistantText("done", agentcore.StopReasonEndTurn),
	)
	in := promptInput("echo both")
	in.Context.Tools = []agentcore.AgentTool{tool}
	in.Context.ConvertToLlm = dropSystemMessages
	in.Batch = agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{
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
	}}

	events, result := runLoop(t, e.loop(prov), in)

	checkComplete(t, "terminate mixed batch", events, result, true)
	mu.Lock()
	gotExecuted := append([]string{}, executed...)
	mu.Unlock()
	if want := []string{"second"}; !reflect.DeepEqual(gotExecuted, want) {
		t.Fatalf("executed = %v, want %v", gotExecuted, want)
	}
	if got := prov.callCount(); got != 2 {
		t.Fatalf("provider calls = %d, want 2 (the mixed batch continues)", got)
	}
	shapes := nonSystemShapes(result)
	if len(shapes) != 5 {
		t.Fatalf("produced transcript = %v, want prompt, call batch, two results, reply", shapes)
	}
	if shapes[2].ToolCallID != "tool-1" || !shapes[2].IsError {
		t.Fatalf("first (blocked) result = %+v, want an error for tool-1", shapes[2])
	}
	if shapes[3].ToolCallID != "tool-2" || shapes[3].IsError {
		t.Fatalf("second (executed) result = %+v, want success for tool-2", shapes[3])
	}
}

// caseTerminatePartialBatch
// PI: it("should continue after parallel tool calls when not all tool results terminate")
// (packages/agent/test/agent-loop.test.ts)
func caseTerminatePartialBatch(t T, e *env) {
	tool := funcTool{name: "echo", schema: echoSchema, mode: agentcore.ToolExecutionParallel, run: func(_ context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		var in struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return agentcore.AgentToolResult{}, err
		}
		terminate := in.Value == "first"
		return agentcore.AgentToolResult{
			Content:   agentcore.ContentList{agentcore.NewTextContent("echoed: " + in.Value)},
			Terminate: &terminate,
		}, nil
	}}
	prov := e.scripted(
		assistantToolUse(
			toolCall("tool-1", "echo", `{"value":"first"}`),
			toolCall("tool-2", "echo", `{"value":"second"}`),
		),
		assistantText("done", agentcore.StopReasonEndTurn),
	)
	in := promptInput("echo both")
	in.Context.Tools = []agentcore.AgentTool{tool}
	in.Context.ConvertToLlm = dropSystemMessages

	events, result := runLoop(t, e.loop(prov), in)

	checkComplete(t, "terminate partial batch", events, result, true)
	if got := prov.callCount(); got != 2 {
		t.Fatalf("provider calls = %d, want 2 (not every result terminated)", got)
	}
	if roles := messageRoles(nonSystemMessages(result)); !reflect.DeepEqual(roles,
		[]string{agentcore.RoleUser, agentcore.RoleAssistant, agentcore.RoleToolResult, agentcore.RoleToolResult, agentcore.RoleAssistant}) {
		t.Fatalf("result roles = %v", roles)
	}
}

// nonSystemMessages returns the produced transcript without tool-loadout
// system declarations.
func nonSystemMessages(msgs []agentcore.AgentMessage) []agentcore.AgentMessage {
	out := make([]agentcore.AgentMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.Role() == agentcore.RoleSystem {
			continue
		}
		out = append(out, m)
	}
	return out
}
