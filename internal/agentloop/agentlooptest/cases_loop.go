package agentlooptest

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentloop"
)

// caseAgentMessageEvents
// PI: it("should emit events with AgentMessage types")
// (packages/agent/test/agent-loop.test.ts, describe "agentLoop with AgentMessage")
//
// The prompt run emits PI's core lifecycle in order, the stream result is the
// prompt it echoed plus the reply, and the provider request is built from the
// transcript.
func caseAgentMessageEvents(t T, e *env) {
	prov := e.scripted(assistantText("Hi there!", agentcore.StopReasonEndTurn))
	in := promptInput("Hello")
	events, result := runLoop(t, e.loop(prov), in)

	checkComplete(t, "agent message events", events, result, true)
	promptLifecycle(t, "agent message events", events, userMsg("Hello"))
	if got := countEvents(events, agentcore.EventTurnStart); got != 1 {
		t.Fatalf("turn_start events = %d, want 1 (a text reply ends after one turn)", got)
	}
	assertShapesEqual(t, "produced messages", shapesOf(result), []msgShape{
		{Role: agentcore.RoleUser, Text: "Hello"},
		{Role: agentcore.RoleAssistant, Text: "Hi there!", StopReason: agentcore.StopReasonEndTurn},
	})

	reqs := prov.seenRequests()
	if len(reqs) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(reqs))
	}
	assertShapesEqual(t, "provider request transcript", shapesOf(reqs[0].Messages), []msgShape{
		{Role: agentcore.RoleUser, Text: "Hello"},
	})
	if reqs[0].SystemPrompt != systemPrompt {
		t.Fatalf("provider system prompt = %q, want %q", reqs[0].SystemPrompt, systemPrompt)
	}
	if reqs[0].Model != in.Model.ID {
		t.Fatalf("provider model = %q, want %q", reqs[0].Model, in.Model.ID)
	}
}

// caseProviderContextFromTranscript
// PI: it("should build provider context exclusively from transcript messages")
// (packages/agent/test/agent-loop.test.ts)
//
// The provider sees the transcript (system message included) and the run is
// replay-deterministic: the same committed input and script produce the same
// event types, transcript and requests.
func caseProviderContextFromTranscript(t T, e *env) {
	newInput := func() agentloop.Input {
		in := baseInput()
		in.Context.Messages = agentcore.MessageList{systemMsg("Transcript prompt"), userMsg("Hello")}
		in.Context.PromptCount = 1
		return in
	}
	run := func() ([]agentcore.AgentEvent, []agentcore.AgentMessage, *scriptedProvider) {
		prov := e.scripted(assistantText("done", agentcore.StopReasonEndTurn))
		events, result := runLoop(t, e.loop(prov), newInput())
		return events, result, prov
	}
	eventsA, resultA, provA := run()
	eventsB, resultB, provB := run()

	checkComplete(t, "provider context", eventsA, resultA, true)
	reqs := provA.seenRequests()
	if len(reqs) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(reqs))
	}
	assertShapesEqual(t, "provider context", shapesOf(reqs[0].Messages), []msgShape{
		{Role: agentcore.RoleSystem, Text: "Transcript prompt"},
		{Role: agentcore.RoleUser, Text: "Hello"},
	})
	if reqs[0].SystemPrompt != systemPrompt {
		t.Fatalf("provider system prompt = %q, want the host-assembled %q", reqs[0].SystemPrompt, systemPrompt)
	}
	// The transcript system message is history: it is not part of what this run
	// produced (only the prompt and the reply are).
	assertShapesEqual(t, "produced messages", shapesOf(resultA), []msgShape{
		{Role: agentcore.RoleUser, Text: "Hello"},
		{Role: agentcore.RoleAssistant, Text: "done", StopReason: agentcore.StopReasonEndTurn},
	})

	// Deterministic replay: identical input and script, identical observable run.
	if got, want := piEventTypes(eventsB), piEventTypes(eventsA); !reflect.DeepEqual(got, want) {
		t.Fatalf("replay event sequence = %v, want %v", got, want)
	}
	assertShapesEqual(t, "replay transcript", shapesOf(resultB), shapesOf(resultA))
	reqsB := provB.seenRequests()
	if len(reqsB) != 1 {
		t.Fatalf("replay provider calls = %d, want 1", len(reqsB))
	}
	assertShapesEqual(t, "replay provider context", shapesOf(reqsB[0].Messages), shapesOf(reqs[0].Messages))
}

// caseContinueExistingContext
// PI: it("should continue from existing context without emitting user message events")
// (packages/agent/test/agent-loop.test.ts, describe "agentLoopContinue with AgentMessage")
//
// PromptCount == 0 is the continuation run: the loop answers the transcript
// tail without announcing a prompt, and returns only the new assistant message.
func caseContinueExistingContext(t T, e *env) {
	prov := e.scripted(assistantText("Response", agentcore.StopReasonEndTurn))
	in := continuationInput(userMsg("Hello"))
	events, result := runLoop(t, e.loop(prov), in)

	checkComplete(t, "continuation", events, result, true)
	for _, ev := range events {
		switch m := ev.(type) {
		case agentcore.MessageStartEvent:
			if m.Message.Role() == agentcore.RoleUser {
				t.Fatalf("continuation emitted message_start for a user message: %v", piEventTypes(events))
			}
		case agentcore.MessageEndEvent:
			if m.Message.Role() == agentcore.RoleUser {
				t.Fatalf("continuation emitted message_end for a user message: %v", piEventTypes(events))
			}
		}
	}
	if got := countEvents(events, agentcore.EventMessageEnd); got != 1 {
		t.Fatalf("message_end events = %d, want 1 (assistant)", got)
	}
	assertShapesEqual(t, "produced messages", shapesOf(result), []msgShape{
		{Role: agentcore.RoleAssistant, Text: "Response", StopReason: agentcore.StopReasonEndTurn},
	})
	if got := prov.callCount(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

// caseContinueNoMessages
// PI: it("should throw when context has no messages")
// (packages/agent/test/agent-loop.test.ts, describe "agentLoopContinue with AgentMessage")
//
// Configuration problems are returned by Run before any event is emitted; an
// empty transcript must not start a run.
func caseContinueNoMessages(t T, e *env) {
	prov := e.scripted(assistantText("never", agentcore.StopReasonEndTurn))
	in := baseInput()
	stream, err := e.loop(prov).Run(context.Background(), in)
	if stream != nil {
		t.Fatalf("Run returned a stream for an empty transcript")
	}
	if !errors.Is(err, agentloop.ErrNothingToAnswer) {
		t.Fatalf("err = %v, want ErrNothingToAnswer", err)
	}
	if got := prov.callCount(); got != 0 {
		t.Fatalf("provider calls = %d, want 0 (a rejected run never starts)", got)
	}
}

// caseProviderErrorTerminal
// PI: it("emits an error instead of hanging when the stream ends without a terminal event")
// (packages/agent/test/proxy.test.ts)
//
// Both an early StreamFn failure and a stream that ends without done/error must
// settle the run with a terminal error assistant message — never hang, never
// skip the lifecycle.
func caseProviderErrorTerminal(t T, e *env) {
	assertTerminalError := func(label string, events []agentcore.AgentEvent, result []agentcore.AgentMessage, wantMessage string) {
		t.Helper()
		checkComplete(t, label, events, result, true)
		if len(result) == 0 {
			t.Fatalf("%s: run produced no messages", label)
		}
		last, ok := result[len(result)-1].(agentcore.AssistantMessage)
		if !ok {
			t.Fatalf("%s: last produced message = %T, want a terminal assistant message", label, result[len(result)-1])
		}
		if last.StopReason != agentcore.StopReasonError {
			t.Fatalf("%s: terminal stop reason = %q, want %q", label, last.StopReason, agentcore.StopReasonError)
		}
		if last.ErrorMessage == "" {
			t.Fatalf("%s: terminal error message is empty", label)
		}
		if wantMessage != "" && last.ErrorMessage != wantMessage {
			t.Fatalf("%s: error message = %q, want %q", label, last.ErrorMessage, wantMessage)
		}
	}

	{
		prov := e.scripted()
		prov.truncateAtCall(1)
		events, result := runLoop(t, e.loop(prov), promptInput("Hello"))
		assertTerminalError("truncated stream", events, result, "")
		if got := prov.callCount(); got != 1 {
			t.Fatalf("provider calls = %d, want 1", got)
		}
	}
	{
		prov := e.scripted()
		prov.failAtCall(1, errors.New("provider exploded"))
		events, result := runLoop(t, e.loop(prov), promptInput("Hello"))
		assertTerminalError("failed stream", events, result, "provider exploded")
		if got := prov.callCount(); got != 1 {
			t.Fatalf("provider calls = %d, want 1", got)
		}
	}
}

// caseAbortDuringStreaming
// PI: it("handles abort during streaming") (packages/agent/test/e2e.test.ts)
//
// Cancelling the run (RunStream.Cancel) settles it: the event stream closes,
// Result returns, and a provider call that was already in flight leaves an
// aborted terminal assistant message. Everything before the blocked call is
// deterministic; the tail after the cancel is best-effort.
func caseAbortDuringStreaming(t T, e *env) {
	prov := e.scripted(assistantText("never", agentcore.StopReasonEndTurn))
	entered := prov.blockAtCall(1)
	in := promptInput("Hello")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := e.loop(prov).Run(ctx, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	run := collectRun(stream.Events())

	select {
	case <-entered:
	case <-time.After(caseTimeout):
		t.Fatalf("the provider call never started")
	}
	stream.Cancel()
	stream.Cancel() // idempotent
	run.wait(t, "the cancelled run")

	resultCtx, resultCancel := context.WithTimeout(context.Background(), caseTimeout)
	defer resultCancel()
	result, err := stream.Result(resultCtx)
	if err != nil {
		t.Fatalf("Result after Cancel: %v", err)
	}

	// The deterministic prefix: agent_start, the prompt lifecycle, turn_start.
	prefix := piEventTypes(run.snapshot())
	wantPrefix := []string{
		agentcore.EventAgentStart,
		agentcore.EventMessageStart, agentcore.EventMessageEnd,
		agentcore.EventTurnStart,
	}
	if len(prefix) < len(wantPrefix) || !reflect.DeepEqual(prefix[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("cancelled prefix = %v, want %v", prefix, wantPrefix)
	}
	if len(result) == 0 {
		t.Fatalf("cancelled run produced no result")
	}
	assertShapesEqual(t, "produced prompt", shapesOf(result[:1]), []msgShape{
		{Role: agentcore.RoleUser, Text: "Hello"},
	})
	// The tail after the cancel is best-effort (emits race the cancelled
	// context, exactly like pi's default signal handling): when a terminal
	// message survives it must be an aborted assistant message without tool
	// calls. What is not optional is that the producer settles and Result
	// returns — asserted above.
	if len(result) > 1 {
		last, ok := result[len(result)-1].(agentcore.AssistantMessage)
		if !ok {
			t.Fatalf("cancelled run tail = %T, want a terminal assistant message", result[len(result)-1])
		}
		if last.StopReason != agentcore.StopReasonAborted {
			t.Fatalf("cancelled terminal stop reason = %q, want %q", last.StopReason, agentcore.StopReasonAborted)
		}
		if len(last.ToolCalls()) != 0 {
			t.Fatalf("cancelled terminal message declares tool calls: %+v", last.ToolCalls())
		}
	}
	checkToolPairing(t, "cancelled run", shapesOf(result), false)
	if got := prov.callCount(); got != 1 {
		t.Fatalf("provider calls = %d, want 1 (the blocked call was in flight)", got)
	}
}

// caseAbortController
// PI: it("should handle abort controller") (packages/agent/test/agent.test.ts)
//
// Cancelling a settled run is a no-op: Cancel is idempotent, the stream result
// is unchanged, and no panic is observable.
func caseAbortController(t T, e *env) {
	prov := e.scripted(assistantText("done", agentcore.StopReasonEndTurn))
	ctx, cancel := context.WithTimeout(context.Background(), caseTimeout)
	defer cancel()
	stream, err := e.loop(prov).Run(ctx, promptInput("Hello"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for range stream.Events() {
	}
	first, err := stream.Result(ctx)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	stream.Cancel()
	stream.Cancel()
	second, err := stream.Result(context.Background())
	if err != nil {
		t.Fatalf("Result after Cancel: %v", err)
	}
	assertShapesEqual(t, "result after cancel", shapesOf(second), shapesOf(first))
}
