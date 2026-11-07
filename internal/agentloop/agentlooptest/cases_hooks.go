package agentlooptest

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentloop"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/loop"
)

// caseSteeringAfterTools
// PI: it("should inject queued messages after all tool calls complete")
// (packages/agent/test/agent-loop.test.ts)
//
// Steering is polled at PI's drain points: the queued message reaches the next
// request only after every tool result of the current batch was produced.
// Adaptation: Keel does not emit message lifecycle events for steering input
// (only committed messages get them), so the next request transcript is the
// observable contract.
func caseSteeringAfterTools(t T, e *env) {
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
		return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("ok:" + in.Value)}}, nil
	}}
	var delivered bool
	prov := e.scripted(
		assistantToolUse(
			toolCall("tool-1", "echo", `{"value":"first"}`),
			toolCall("tool-2", "echo", `{"value":"second"}`),
		),
		assistantText("done", agentcore.StopReasonEndTurn),
	)
	in := promptInput("start")
	in.Context.Tools = []agentcore.AgentTool{tool}
	in.Context.ConvertToLlm = dropSystemMessages
	in.Batch = agenttool.BatchConfig{
		ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: registryOf(t, tool)},
		ForceSequential:    true,
	}
	in.Hooks.GetSteeringMessages = func(context.Context) []agentcore.AgentMessage {
		mu.Lock()
		defer mu.Unlock()
		if len(executed) >= 2 && !delivered {
			delivered = true
			return []agentcore.AgentMessage{userMsg("interrupt")}
		}
		return nil
	}

	events, result := runLoop(t, e.loop(prov), in)

	checkComplete(t, "steering", events, result, true)
	mu.Lock()
	gotExecuted := append([]string{}, executed...)
	mu.Unlock()
	if want := []string{"first", "second"}; !reflect.DeepEqual(gotExecuted, want) {
		t.Fatalf("executed = %v, want %v (both tools before steering)", gotExecuted, want)
	}
	var toolEnds int
	for _, ev := range events {
		if end, ok := ev.(agentcore.ToolExecutionEndEvent); ok {
			if end.IsError {
				t.Fatalf("tool_execution_end for %s is an error", end.ToolCallID)
			}
			toolEnds++
		}
	}
	if toolEnds != 2 {
		t.Fatalf("tool_execution_end events = %d, want 2", toolEnds)
	}
	reqs := prov.seenRequests()
	if len(reqs) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(reqs))
	}
	if users := requestUserTexts(reqs[0].Messages); !reflect.DeepEqual(users, []string{"start"}) {
		t.Fatalf("first request users = %v, want [start] (steering not yet delivered)", users)
	}
	if users := requestUserTexts(reqs[1].Messages); !reflect.DeepEqual(users, []string{"start", "interrupt"}) {
		t.Fatalf("second request users = %v, want [start interrupt]", users)
	}
}

// caseFollowUpQueue
// PI: it("should support follow-up message queue") (packages/agent/test/agent.test.ts)
//
// Follow-ups are polled only after the inner loop settles; the delivered
// message continues the run as the next input, and the transcript shows it.
func caseFollowUpQueue(t T, e *env) {
	var (
		mu        sync.Mutex
		polls     int
		delivered bool
		pollRoles []string
	)
	prov := e.scripted(
		assistantText("first answer", agentcore.StopReasonEndTurn),
		assistantText("second answer", agentcore.StopReasonEndTurn),
	)
	in := promptInput("run")
	in.Hooks.GetFollowUpMessages = func(_ context.Context, agentCtx *agentcore.AgentContext) []agentcore.AgentMessage {
		mu.Lock()
		defer mu.Unlock()
		polls++
		if polls == 1 {
			pollRoles = messageRoles(agentCtx.Messages)
			if delivered {
				return nil
			}
			delivered = true
			return []agentcore.AgentMessage{userMsg("follow up")}
		}
		return nil
	}

	events, result := runLoop(t, e.loop(prov), in)

	checkComplete(t, "follow-up", events, result, true)
	mu.Lock()
	gotPolls := polls
	gotPollRoles := append([]string{}, pollRoles...)
	mu.Unlock()
	if gotPolls != 2 {
		t.Fatalf("follow-up polls = %d, want 2 (deliver, then empty)", gotPolls)
	}
	if want := []string{agentcore.RoleUser, agentcore.RoleAssistant}; !reflect.DeepEqual(gotPollRoles, want) {
		t.Fatalf("first follow-up poll saw roles %v, want the settled inner loop %v", gotPollRoles, want)
	}
	reqs := prov.seenRequests()
	if len(reqs) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(reqs))
	}
	if users := requestUserTexts(reqs[1].Messages); !reflect.DeepEqual(users, []string{"run", "follow up"}) {
		t.Fatalf("second request users = %v, want [run follow up]", users)
	}
	assertShapesEqual(t, "produced transcript", shapesOf(result), []msgShape{
		{Role: agentcore.RoleUser, Text: "run"},
		{Role: agentcore.RoleAssistant, Text: "first answer", StopReason: agentcore.StopReasonEndTurn},
		{Role: agentcore.RoleUser, Text: "follow up"},
		{Role: agentcore.RoleAssistant, Text: "second answer", StopReason: agentcore.StopReasonEndTurn},
	})
}

// casePrepareNextTurnSnapshot
// PI: it("should use prepareNextTurn snapshot before continuing")
// (packages/agent/test/agent-loop.test.ts)
func casePrepareNextTurnSnapshot(t T, e *env) {
	tool := &echoTool{}
	var (
		mu           sync.Mutex
		prepareCalls int
		prepared     bool
	)
	prov := e.scripted(
		assistantToolUse(toolCall("tool-1", "echo", `{"value":"hello"}`)),
		assistantText("done", agentcore.StopReasonEndTurn),
	)
	in := promptInput("echo something")
	in.Context.Tools = []agentcore.AgentTool{tool}
	in.Context.ConvertToLlm = dropSystemMessages
	in.Hooks.PrepareNextTurn = func(_ context.Context, agentCtx *agentcore.AgentContext) *loop.TurnUpdate {
		mu.Lock()
		defer mu.Unlock()
		prepareCalls++
		if prepared {
			return nil
		}
		prepared = true
		snapshot := append(agentcore.MessageList{}, agentCtx.Messages...)
		return &loop.TurnUpdate{
			Messages:       &snapshot,
			AppendMessages: []agentcore.AgentMessage{userMsg("updated guidance")},
		}
	}

	events, result := runLoop(t, e.loop(prov), in)

	checkComplete(t, "prepare next turn", events, result, true)
	mu.Lock()
	gotCalls := prepareCalls
	mu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("prepareNextTurn calls = %d, want 1 (not before the first turn)", gotCalls)
	}
	reqs := prov.seenRequests()
	if len(reqs) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(reqs))
	}
	if !requestHasUserText(reqs[1].Messages, "updated guidance") {
		t.Fatalf("second request is missing the appended guidance: %v", shapesOf(reqs[1].Messages))
	}
	// The snapshot kept the turn's tool result, so the model still sees it.
	if roles := messageRoles(reqs[1].Messages); !reflect.DeepEqual(roles,
		[]string{agentcore.RoleUser, agentcore.RoleAssistant, agentcore.RoleToolResult, agentcore.RoleUser}) {
		t.Fatalf("second request roles = %v", roles)
	}
}

// casePrepareRequestInitial
// PI: it("prepares the initial request after pending messages and can replace request state")
// (packages/agent/test/agent-loop.test.ts)
func casePrepareRequestInitial(t T, e *env) {
	replacementModel := "replacement"
	high := agentcore.ThinkingHigh
	canonical := userMsg("canonical projection")
	var (
		mu                sync.Mutex
		steeringDelivered bool
		prepareCalls      int
		sawSteering       bool
	)
	prov := e.scripted(assistantText("done", agentcore.StopReasonEndTurn))
	in := promptInput("prompt")
	in.Hooks.GetSteeringMessages = func(context.Context) []agentcore.AgentMessage {
		mu.Lock()
		defer mu.Unlock()
		if steeringDelivered {
			return nil
		}
		steeringDelivered = true
		return []agentcore.AgentMessage{userMsg("steering")}
	}
	in.Hooks.PrepareRequest = func(_ context.Context, req loop.PrepareRequestContext) *loop.RequestUpdate {
		mu.Lock()
		prepareCalls++
		// The hook runs on the run goroutine: record, assert on the test
		// goroutine (t.Fatalf must not be called from a spawned goroutine).
		if requestHasUserText(req.Context.Messages, "steering") {
			sawSteering = true
		}
		mu.Unlock()
		return &loop.RequestUpdate{
			Context:       &agentcore.AgentContext{Messages: agentcore.MessageList{canonical}},
			Model:         &replacementModel,
			ThinkingLevel: &high,
		}
	}

	events, result := runLoop(t, e.loop(prov), in)

	checkComplete(t, "prepare request", events, result, true)
	mu.Lock()
	gotCalls, gotSawSteering := prepareCalls, sawSteering
	mu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("prepareRequest calls = %d, want 1", gotCalls)
	}
	if !gotSawSteering {
		t.Fatalf("prepareRequest context is missing the steering message; pending steering must be appended before the hook runs")
	}
	reqs := prov.seenRequests()
	if len(reqs) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(reqs))
	}
	if reqs[0].Model != replacementModel {
		t.Fatalf("request model = %q, want %q", reqs[0].Model, replacementModel)
	}
	if reqs[0].ThinkingLevel != agentcore.ThinkingHigh {
		t.Fatalf("request thinking level = %q, want high", reqs[0].ThinkingLevel)
	}
	assertShapesEqual(t, "replacement request context", shapesOf(reqs[0].Messages), []msgShape{
		{Role: agentcore.RoleUser, Text: "canonical projection"},
	})
}

// casePrepareRequestNoSteeringPoll
// PI: it("does not poll steering after prepareRequest")
// (packages/agent/test/agent-loop.test.ts)
func casePrepareRequestNoSteeringPoll(t T, e *env) {
	var (
		mu                  sync.Mutex
		queued              []agentcore.AgentMessage
		lateSteering        = userMsg("late steering")
		requestPreparations int
		steeringPolls       int
	)
	prov := e.scripted(
		assistantText("first", agentcore.StopReasonEndTurn),
		assistantText("second", agentcore.StopReasonEndTurn),
	)
	in := promptInput("run")
	in.Hooks.GetSteeringMessages = func(context.Context) []agentcore.AgentMessage {
		mu.Lock()
		defer mu.Unlock()
		steeringPolls++
		out := queued
		queued = nil
		return out
	}
	in.Hooks.PrepareRequest = func(context.Context, loop.PrepareRequestContext) *loop.RequestUpdate {
		mu.Lock()
		defer mu.Unlock()
		requestPreparations++
		if requestPreparations == 1 {
			queued = append(queued, lateSteering)
		}
		return nil
	}

	events, result := runLoop(t, e.loop(prov), in)

	checkComplete(t, "prepare request steering", events, result, true)
	mu.Lock()
	gotPreparations, gotPolls := requestPreparations, steeringPolls
	mu.Unlock()
	if gotPreparations != 2 {
		t.Fatalf("prepareRequest calls = %d, want 2", gotPreparations)
	}
	// Startup poll, post-turn-1 delivery, post-turn-2 natural-stop check.
	if gotPolls != 3 {
		t.Fatalf("steering polls = %d, want 3", gotPolls)
	}
	reqs := prov.seenRequests()
	if len(reqs) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(reqs))
	}
	if requestHasUserText(reqs[0].Messages, "late steering") {
		t.Fatalf("first request included steering queued during prepareRequest: %v", shapesOf(reqs[0].Messages))
	}
	if !requestHasUserText(reqs[1].Messages, "late steering") {
		t.Fatalf("second request is missing the late steering: %v", shapesOf(reqs[1].Messages))
	}
}

// caseTransformBeforeConvert
// PI: it("should apply transformContext before convertToLlm")
// (packages/agent/test/agent-loop.test.ts)
func caseTransformBeforeConvert(t T, e *env) {
	var (
		mu             sync.Mutex
		transformedLen int
		convertedLen   int
	)
	prov := e.scripted(assistantText("Response", agentcore.StopReasonEndTurn))
	in := baseInput()
	in.Context.Messages = agentcore.MessageList{
		userMsg("old message 1"),
		assistantText("old response 1", agentcore.StopReasonEndTurn),
		userMsg("old message 2"),
		assistantText("old response 2", agentcore.StopReasonEndTurn),
		userMsg("new message"),
	}
	in.Context.PromptCount = 1
	in.Context.TransformContext = func(_ context.Context, msgs agentcore.MessageList) agentcore.MessageList {
		mu.Lock()
		defer mu.Unlock()
		transformedLen = 2
		out := make(agentcore.MessageList, 0, 2)
		out = append(out, msgs[len(msgs)-2:]...)
		return out
	}
	in.Context.ConvertToLlm = func(msgs agentcore.MessageList) agentcore.MessageList {
		mu.Lock()
		defer mu.Unlock()
		convertedLen = len(msgs)
		return msgs
	}

	events, result := runLoop(t, e.loop(prov), in)

	checkComplete(t, "transform before convert", events, result, true)
	mu.Lock()
	gotTransformed, gotConverted := transformedLen, convertedLen
	mu.Unlock()
	if gotTransformed != 2 {
		t.Fatalf("transformContext produced %d messages, want 2", gotTransformed)
	}
	if gotConverted != 2 {
		t.Fatalf("convertToLlm saw %d messages, want the 2 pruned ones (transform must run first)", gotConverted)
	}
	reqs := prov.seenRequests()
	if len(reqs) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(reqs))
	}
	if got := messageRoles(reqs[0].Messages); !reflect.DeepEqual(got, []string{agentcore.RoleAssistant, agentcore.RoleUser}) {
		t.Fatalf("provider message roles = %v, want the pruned tail [assistant user]", got)
	}
	if got := requestUserTexts(reqs[0].Messages); !reflect.DeepEqual(got, []string{"new message"}) {
		t.Fatalf("provider user messages = %v, want [new message]", got)
	}
}

// caseCustomMessageConvertToLLM
// PI: it("should handle custom message types via convertToLlm")
// (packages/agent/test/agent-loop.test.ts)
//
// Adaptation: PI injects a `notification` role via TS declaration merging; Go's
// Message interface is sealed inside agentcore, so the non-LLM kind available to
// callers is CompactionMessage — the same contract: a message kind that is not
// LLM-bound must be handled by convertToLlm, never sent to the provider raw.
func caseCustomMessageConvertToLLM(t T, e *env) {
	convert := func(msgs agentcore.MessageList) agentcore.MessageList {
		out := make(agentcore.MessageList, 0, len(msgs))
		for _, m := range msgs {
			if cm, ok := m.(agentcore.CompactionMessage); ok {
				out = append(out, cm.AsUserMessage())
				continue
			}
			out = append(out, m)
		}
		return out
	}
	newInput := func(withConvert bool) agentloop.Input {
		in := baseInput()
		in.Context.Messages = agentcore.MessageList{
			agentcore.CompactionMessage{RoleField: agentcore.RoleCompaction, Summary: "older history"},
			userMsg("Hello"),
		}
		in.Context.PromptCount = 1
		if withConvert {
			in.Context.ConvertToLlm = convert
		}
		return in
	}

	prov := e.scripted(assistantText("Response", agentcore.StopReasonEndTurn))
	events, result := runLoop(t, e.loop(prov), newInput(true))
	checkComplete(t, "custom message convert", events, result, true)
	reqs := prov.seenRequests()
	if len(reqs) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(reqs))
	}
	if roles := messageRoles(reqs[0].Messages); !reflect.DeepEqual(roles, []string{agentcore.RoleUser, agentcore.RoleUser}) {
		t.Fatalf("request roles = %v, want both messages converted to user", roles)
	}
	first, ok := reqs[0].Messages[0].(agentcore.UserMessage)
	if !ok {
		t.Fatalf("converted message = %T, want a user message", reqs[0].Messages[0])
	}
	if text := agentcore.ContentToText(first.Content); !strings.Contains(text, "older history") {
		t.Fatalf("converted compaction text = %q", text)
	}

	// Control: without convertToLlm the custom kind reaches the provider raw, so
	// the hook — not the loop — is what makes custom kinds safe.
	rawProv := e.scripted(assistantText("Response", agentcore.StopReasonEndTurn))
	runLoop(t, e.loop(rawProv), newInput(false))
	rawReqs := rawProv.seenRequests()
	if len(rawReqs) != 1 {
		t.Fatalf("control provider calls = %d, want 1", len(rawReqs))
	}
	if roles := messageRoles(rawReqs[0].Messages); !reflect.DeepEqual(roles,
		[]string{agentcore.RoleCompaction, agentcore.RoleUser}) {
		t.Fatalf("control request roles = %v, want the custom kind passed through", roles)
	}
}
