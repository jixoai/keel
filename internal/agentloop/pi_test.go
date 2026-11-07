package agentloop_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentloop"
	"github.com/jixoai/keel/internal/agenttool"
)

// TestPiLoopTextRunEmitsPiSequence pins the default loop's text behavior
// through the service interface: PI's event order, the produced-message result
// and the request the provider saw.
func TestPiLoopTextRunEmitsPiSequence(t *testing.T) {
	prov := newFakeProvider(assistantText("Hi there!", agentcore.StopReasonEndTurn))
	loop := agentloop.NewPiLoop(prov.stream)
	in := baseInput()

	events, result := runLoopOnce(t, loop, in)

	want := []string{
		agentcore.EventAgentStart,
		agentcore.EventMessageStart, agentcore.EventMessageEnd, // the prompt's lifecycle
		agentcore.EventTurnStart,
		agentcore.EventMessageStart, agentcore.EventMessageUpdate, agentcore.EventMessageEnd,
		agentcore.EventTurnEnd,
		agentcore.EventAgentEnd,
	}
	if got := piEventTypes(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("PI event sequence = %v, want %v", got, want)
	}
	assertShapesEqual(t, "produced messages", shapesOf(result), []msgShape{
		{Role: agentcore.RoleUser, Text: "hello"},
		{Role: agentcore.RoleAssistant, Text: "Hi there!", StopReason: agentcore.StopReasonEndTurn},
	})
	if prov.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1", prov.callCount())
	}
	reqs := prov.seenRequests()
	if len(reqs) != 1 {
		t.Fatalf("recorded requests = %d, want 1", len(reqs))
	}
	if reqs[0].SystemPrompt != "you are a test agent" {
		t.Fatalf("system prompt = %q", reqs[0].SystemPrompt)
	}
	var roles []string
	for _, m := range reqs[0].Messages {
		roles = append(roles, m.Role())
	}
	if want := []string{"user", "user"}; !reflect.DeepEqual(roles, want) {
		t.Fatalf("request message roles = %v, want %v (history + prompt)", roles, want)
	}
}

// TestPiLoopToolRoundTrip pins tool-call ↔ result pairing and ordering: the
// call is executed once, its result is announced to the model and appears in
// the produced messages, and the run continues to the natural end.
func TestPiLoopToolRoundTrip(t *testing.T) {
	tool := &echoTool{}
	prov := newFakeProvider(
		assistantToolUse(toolCall("call-1", "echo", `{"value":"hello"}`)),
		assistantText("done", agentcore.StopReasonEndTurn),
	)
	loop := agentloop.NewPiLoop(prov.stream)
	in := baseInput()
	in.Context.Tools = []agentcore.AgentTool{tool}
	// The coordinator's provider-boundary filter: transcript tool declarations
	// (system messages) never reach the provider wire.
	in.Context.ConvertToLlm = func(msgs agentcore.MessageList) agentcore.MessageList {
		out := make(agentcore.MessageList, 0, len(msgs))
		for _, m := range msgs {
			if m.Role() != agentcore.RoleSystem {
				out = append(out, m)
			}
		}
		return out
	}

	events, result := runLoopOnce(t, loop, in)

	want := []string{
		agentcore.EventAgentStart,
		agentcore.EventMessageStart, agentcore.EventMessageEnd, // tool-loadout declaration
		agentcore.EventMessageStart, agentcore.EventMessageEnd, // prompt
		agentcore.EventTurnStart,
		agentcore.EventMessageStart, agentcore.EventMessageUpdate, agentcore.EventMessageEnd,
		agentcore.EventToolExecutionStart, agentcore.EventToolExecutionEnd,
		agentcore.EventMessageStart, agentcore.EventMessageEnd, // tool result
		agentcore.EventTurnEnd,
		agentcore.EventTurnStart,
		agentcore.EventMessageStart, agentcore.EventMessageUpdate, agentcore.EventMessageEnd,
		agentcore.EventTurnEnd,
		agentcore.EventAgentEnd,
	}
	if got := piEventTypes(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("PI event sequence = %v, want %v", got, want)
	}
	assertShapesEqual(t, "produced messages", shapesOf(result), []msgShape{
		{Role: agentcore.RoleSystem, DeclaredTools: []string{"echo"}},
		{Role: agentcore.RoleUser, Text: "hello"},
		{Role: agentcore.RoleAssistant, StopReason: agentcore.StopReasonToolUse, Calls: []string{`call-1|echo|{"value":"hello"}`}},
		{Role: agentcore.RoleToolResult, Text: "echoed: hello", ToolCallID: "call-1", ToolName: "echo"},
		{Role: agentcore.RoleAssistant, Text: "done", StopReason: agentcore.StopReasonEndTurn},
	})
	if got := tool.executed.Load(); got != 1 {
		t.Fatalf("echo tool executions = %d, want 1", got)
	}
	reqs := prov.seenRequests()
	if len(reqs) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(reqs))
	}
	// The provider never sees the transcript's system messages.
	for i, req := range reqs {
		for _, m := range req.Messages {
			if m.Role() == agentcore.RoleSystem {
				t.Fatalf("request %d carried a system message despite ConvertToLlm: %v", i, m)
			}
		}
	}
	last := reqs[1].Messages[len(reqs[1].Messages)-1]
	tr, ok := last.(agentcore.ToolResultMessage)
	if !ok {
		t.Fatalf("second request tail = %T, want the tool result", last)
	}
	if tr.ToolCallID != "call-1" {
		t.Fatalf("second request tool result = %q, want call-1 (pairing)", tr.ToolCallID)
	}
}

// TestPiLoopCancelSettlesRun pins the cancellation semantics the adapter must
// preserve: cancelling settles the run, the stream closes, the result is
// returned, and the deterministic prefix before the blocked call is intact.
func TestPiLoopCancelSettlesRun(t *testing.T) {
	tool := &echoTool{}
	prov := newFakeProvider(assistantToolUse(toolCall("call-1", "echo", `{"value":"hello"}`)))
	entered := prov.blockFromCall(2)
	loop := agentloop.NewPiLoop(prov.stream)
	in := baseInput()
	in.Context.Tools = []agentcore.AgentTool{tool}
	in.Limits.EventBuffer = 32

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := loop.Run(ctx, in)
	if err != nil {
		t.Fatalf("loop run: %v", err)
	}
	var events []agentcore.AgentEvent
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range stream.Events() {
			events = append(events, ev)
		}
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the second provider call never started")
	}
	stream.Cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not settle after Cancel")
	}
	result, err := stream.Result(context.Background())
	if err != nil {
		t.Fatalf("result after cancel: %v", err)
	}

	// Everything before the blocked call is deterministic; the torn tail after
	// the cancel is best-effort (emits race the cancelled context).
	if got := piEventTypes(prefixThroughTurnStart(events, 2)); !reflect.DeepEqual(got, toolTurnPrefix) {
		t.Fatalf("deterministic prefix = %v, want %v", got, toolTurnPrefix)
	}
	if len(result) < len(toolTurnFirstTurn) {
		t.Fatalf("produced messages = %v, want at least the first turn", shapesOf(result))
	}
	assertShapesEqual(t, "deterministic produced prefix", shapesOf(result[:len(toolTurnFirstTurn)]), toolTurnFirstTurn)
	assertToolResultsPaired(t, "pi-loop", result)
	if len(result) > len(toolTurnFirstTurn) {
		// The only acceptable tail is the aborted terminal message of the
		// blocked turn; it must not carry unpaired tool calls.
		last := shapesOf(result)[len(result)-1]
		if last.Role != agentcore.RoleAssistant || last.StopReason != agentcore.StopReasonAborted {
			t.Fatalf("cancelled run tail = %+v, want an aborted assistant message", last)
		}
		if len(last.Calls) != 0 {
			t.Fatalf("cancelled run tail declares tool calls: %+v", last)
		}
	}
	if got := tool.executed.Load(); got != 1 {
		t.Fatalf("echo tool executions = %d, want 1", got)
	}
}

// TestPiLoopTurnCap pins the run limit: Limits.MaxTurns is enforced where the
// PI loop consults ShouldStopAfterTurn, and the zero value selects
// DefaultMaxTurns (not a cap of one).
func TestPiLoopTurnCap(t *testing.T) {
	script := func() *fakeProvider {
		return newFakeProvider(
			assistantToolUse(toolCall("call-1", "echo", `{"value":"one"}`)),
			assistantToolUse(toolCall("call-2", "echo", `{"value":"two"}`)),
			assistantText("done", agentcore.StopReasonEndTurn),
		)
	}
	tool := &echoTool{}

	t.Run("cap stops the run", func(t *testing.T) {
		prov := script()
		loop := agentloop.NewPiLoop(prov.stream)
		in := baseInput()
		in.Context.Tools = []agentcore.AgentTool{tool}
		in.Limits.MaxTurns = 2
		events, result := runLoopOnce(t, loop, in)
		if got := prov.callCount(); got != 2 {
			t.Fatalf("provider calls = %d, want 2 (capped)", got)
		}
		if _, ok := events[len(events)-1].(agentcore.AgentEndEvent); !ok {
			t.Fatalf("last event = %T, want agent_end", events[len(events)-1])
		}
		assertToolResultsPaired(t, "capped run", result)
	})

	t.Run("zero selects the default cap", func(t *testing.T) {
		prov := script()
		loop := agentloop.NewPiLoop(prov.stream)
		in := baseInput()
		in.Context.Tools = []agentcore.AgentTool{tool}
		in.Limits.MaxTurns = 0
		_, result := runLoopOnce(t, loop, in)
		if got := prov.callCount(); got != 3 {
			t.Fatalf("provider calls = %d, want 3 (DefaultMaxTurns is 8, not 1)", got)
		}
		if got := result[len(result)-1].(agentcore.AssistantMessage).StopReason; got != agentcore.StopReasonEndTurn {
			t.Fatalf("final stop reason = %q, want end_turn", got)
		}
	})
}

// TestPiLoopRejectsInvalidRuns pins the pre-run validation: configuration
// problems surface as errors, never as a half-started run.
func TestPiLoopRejectsInvalidRuns(t *testing.T) {
	prov := newFakeProvider(assistantText("never", agentcore.StopReasonEndTurn))
	loop := agentloop.NewPiLoop(prov.stream)

	cases := []struct {
		name string
		in   agentloop.Input
		want error
	}{
		{"empty transcript", agentloop.Input{SessionID: "s"}, agentloop.ErrNothingToAnswer},
		{"system only", func() agentloop.Input {
			in := baseInput()
			in.Context.Messages = agentcore.MessageList{agentcore.SystemMessage{RoleField: agentcore.RoleSystem, Content: "boot"}}
			in.Context.PromptCount = 0
			return in
		}(), agentloop.ErrNothingToAnswer},
		{"assistant tail", func() agentloop.Input {
			in := baseInput()
			in.Context.Messages = agentcore.MessageList{userMsg("hello"), assistantText("answered", agentcore.StopReasonEndTurn)}
			in.Context.PromptCount = 0
			return in
		}(), agentloop.ErrNothingToAnswer},
		{"prompt count out of range", func() agentloop.Input {
			in := baseInput()
			in.Context.PromptCount = 3
			return in
		}(), agentloop.ErrInvalidInput},
		{"duplicate tools", func() agentloop.Input {
			in := baseInput()
			in.Context.Tools = []agentcore.AgentTool{&echoTool{}, &echoTool{}}
			return in
		}(), agentloop.ErrInvalidInput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream, err := loop.Run(context.Background(), tc.in)
			if stream != nil {
				t.Fatalf("Run returned a stream for an invalid input")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}

	t.Run("no stream", func(t *testing.T) {
		if _, err := agentloop.NewPiLoop(nil).Run(context.Background(), baseInput()); !errors.Is(err, agentloop.ErrNoStream) {
			t.Fatalf("err = %v, want ErrNoStream", err)
		}
	})
	if prov.callCount() != 0 {
		t.Fatalf("provider calls = %d, want 0 (rejected runs never start)", prov.callCount())
	}
}

// TestPiLoopUsesProvidedRegistry: a coordinator that already owns a registry
// passes it in and its tools are declared to the model.
func TestPiLoopUsesProvidedRegistry(t *testing.T) {
	prov := newFakeProvider(assistantText("ok", agentcore.StopReasonEndTurn))
	loop := agentloop.NewPiLoop(prov.stream)
	registry := agenttool.NewToolRegistry()
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	in := baseInput()
	in.Batch = agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: registry}}

	events, _ := runLoopOnce(t, loop, in)

	// The registry's tools are declared to the model.
	reqs := prov.seenRequests()
	if len(reqs) != 1 || len(reqs[0].Tools) != 1 || reqs[0].Tools[0].Name() != "echo" {
		t.Fatalf("request tools = %v, want the provided echo tool", reqs[0].Tools)
	}
	// The tool-loadout declaration appears in the transcript events.
	if got := piEventTypes(events)[1:3]; !reflect.DeepEqual(got, []string{agentcore.EventMessageStart, agentcore.EventMessageEnd}) {
		t.Fatalf("declaration events = %v", got)
	}
}

// TestPiLoopDoesNotMutateInputTranscript: the loop copies the transcript, so
// the coordinator's slice is not disturbed by a run.
func TestPiLoopDoesNotMutateInputTranscript(t *testing.T) {
	prov := newFakeProvider(assistantText("hi", agentcore.StopReasonEndTurn))
	loop := agentloop.NewPiLoop(prov.stream)
	in := baseInput()
	before := shapesOf(in.Context.Messages)

	_, result := runLoopOnce(t, loop, in)

	assertShapesEqual(t, "input transcript after the run", shapesOf(in.Context.Messages), before)
	if len(result) != 2 {
		t.Fatalf("produced = %d messages, want the prompt + reply only", len(result))
	}
}

// TestPiLoopResultIsStableAcrossReads: the stream result is read repeatedly
// without consuming it (the coordinator may read it after iterating events).
func TestPiLoopResultIsStableAcrossReads(t *testing.T) {
	prov := newFakeProvider(assistantText("stable", agentcore.StopReasonEndTurn))
	loop := agentloop.NewPiLoop(prov.stream)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := loop.Run(ctx, baseInput())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for range stream.Events() {
	}
	first, err := stream.Result(ctx)
	if err != nil {
		t.Fatalf("first result: %v", err)
	}
	second, err := stream.Result(ctx)
	if err != nil {
		t.Fatalf("second result: %v", err)
	}
	assertShapesEqual(t, "repeated result reads", shapesOf(second), shapesOf(first))
}

// prefixThroughTurnStart returns the events up to and including the nth
// turn_start: the longest prefix that is deterministic before the loop blocks
// on a provider call.
func prefixThroughTurnStart(events []agentcore.AgentEvent, n int) []agentcore.AgentEvent {
	seen := 0
	for i, ev := range events {
		if _, ok := ev.(agentcore.TurnStartEvent); ok {
			seen++
			if seen == n {
				return events[:i+1]
			}
		}
	}
	return events
}

// assertToolResultsPaired fails when a tool result has no matching tool call
// earlier in the same message list (orphan results are a ledger-level
// rejection, so a loop must never produce one).
func assertToolResultsPaired(t *testing.T, name string, msgs []agentcore.AgentMessage) {
	t.Helper()
	declared := map[string]bool{}
	for _, m := range msgs {
		switch msg := m.(type) {
		case agentcore.AssistantMessage:
			for _, c := range msg.ToolCalls() {
				declared[c.ID] = true
			}
		case agentcore.ToolResultMessage:
			if !declared[msg.ToolCallID] {
				t.Fatalf("%s: tool result %q has no matching call (%s)", name, msg.ToolCallID, fmt.Sprintf("%v", shapesOf(msgs)))
			}
		}
	}
}
