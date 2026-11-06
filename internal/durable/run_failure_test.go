package durable

import (
	"context"
	"encoding/json"
	"testing"
)

// Ported from packages/durable/test/harness-run-failure.test.ts.

func liveOf(h *Harness, conv *Conversation) LiveState { return h.Live(conv) }

func TestRunFailure_RecordsAModelErrorWithItsTextAndALaterRunClearsIt(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	faux.setResponses(errorMessage("Invalid request"), textMessage("fine"))
	h, root := openChat(t, nil, Options{Provider: faux})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("hi")})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := sub.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Status != SubmissionUnanswered || settled.Reason != ReasonModelError || settled.Detail != "Invalid request" {
		t.Fatalf("settled = %+v", settled)
	}
	failure := liveOf(h, root).RunFailure
	if failure == nil || failure.Reason != ReasonModelError || failure.Detail != "Invalid request" ||
		len(failure.Inputs) != 1 || failure.Inputs[0] != sub.ID() || failure.TaskID == "" || failure.First == 0 {
		t.Fatalf("failure = %+v", failure)
	}
	next, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("again")})
	if err != nil {
		t.Fatal(err)
	}
	if failure := liveOf(h, root).RunFailure; failure != nil {
		t.Fatalf("failure not cleared: %+v", failure)
	}
	if rec, err := next.Wait(ctx); err != nil || rec.Status != SubmissionDone {
		t.Fatalf("next = %+v, %v", rec, err)
	}
	live := liveOf(h, root)
	if live.Run != nil || live.RunFailure != nil {
		t.Fatalf("live = %+v", live)
	}
}

func TestRunFailure_RecordsAMissingModelWithWhichOne(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	h, root := openChat(t, nil, Options{Provider: faux})
	if err := root.Configure(ctx, AgentState{Model: &ModelRef{Provider: "faux", ModelID: "gone"}}); err != nil {
		t.Fatal(err)
	}
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("hi")})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := sub.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Status != SubmissionUnanswered || settled.Reason != ReasonNoModel || settled.Detail != "" {
		t.Fatalf("settled = %+v", settled)
	}
	failure := liveOf(h, root).RunFailure
	if failure == nil || failure.Reason != ReasonNoModel || failure.Detail != "Model faux/gone is not available" {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestRunFailure_RecordsAnAbort(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	step, reached, release := gated(textMessage("never"))
	faux.setResponses(step)
	h, root := openChat(t, nil, Options{Provider: faux})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("hi")})
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	taskID := liveOf(h, root).Run.TaskID
	_ = release
	if err := root.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	settled, err := sub.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Status != SubmissionUnanswered || settled.Reason != ReasonAborted {
		t.Fatalf("settled = %+v", settled)
	}
	failure := liveOf(h, root).RunFailure
	if failure == nil || failure.TaskID != taskID || failure.Reason != ReasonAborted ||
		len(failure.Inputs) != 1 || failure.Inputs[0] != sub.ID() {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestRunFailure_RecordsAResetThatCutTheRunBeforeAnAnswerKeptByTheRunItsQueuedInputStarts(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	started := newDeferred()
	gate := newDeferred()
	hold := &Tool{
		Name: "hold", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			started.resolve()
			if err := gate.wait(ctx); err != nil {
				return ToolResult{}, err
			}
			return ToolResult{}, nil
		},
	}
	secondStep, secondReached, secondRelease := gated(textMessage("never"))
	faux.setResponses(
		calls(callSpec{name: "hold", args: map[string]any{}, id: "c1"}),
		secondStep,
	)
	h, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{hold}})
	first, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	<-started.ch
	if err := root.Reset(ctx, ""); err != nil {
		t.Fatal(err)
	}
	followUp, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("next"), WhenBusy: "followUp"})
	if err != nil {
		t.Fatal(err)
	}
	gate.resolve()
	if settled, err := first.Wait(ctx); err != nil || settled.Reason != ReasonReset {
		t.Fatalf("first = %+v, %v", settled, err)
	}
	<-secondReached
	state := liveOf(h, root)
	if state.Run == nil || len(state.Run.Inputs) != 1 || state.Run.Inputs[0] != followUp.ID() {
		t.Fatalf("run = %+v", state.Run)
	}
	if state.RunFailure == nil || state.RunFailure.Reason != ReasonReset ||
		len(state.RunFailure.Inputs) != 1 || state.RunFailure.Inputs[0] != first.ID() {
		t.Fatalf("failure = %+v", state.RunFailure)
	}
	if err := root.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	failure := liveOf(h, root).RunFailure
	if failure == nil || failure.Reason != ReasonAborted || len(failure.Inputs) != 1 || failure.Inputs[0] != followUp.ID() {
		t.Fatalf("failure after abort = %+v", failure)
	}
	secondRelease.resolve()
}

func TestRunFailure_KeepsTheFailureWhileARunContinuesTheQueueInTheSameCommitAndReplacesItWithTheNext(t *testing.T) {
	for _, policy := range []string{AfterFailedContinue, AfterFailedKeep} {
		t.Run(policy, func(t *testing.T) {
			ctx := ctxT(t)
			faux := &fauxProvider{}
			failingStep, failingReached, failingRelease := gated(errorMessage("Invalid request"))
			busyStep, busyReached, _ := gated(textMessage("third"))
			faux.setResponses(failingStep, busyStep, textMessage("third"))
			h, root := openChat(t, nil, Options{Provider: faux, Settings: Settings{AfterFailedRun: policy}})
			first, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("first")})
			if err != nil {
				t.Fatal(err)
			}
			<-failingReached
			second, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("second"), WhenBusy: "followUp"})
			if err != nil {
				t.Fatal(err)
			}
			failingRelease.resolve()
			if settled, err := first.Wait(ctx); err != nil || settled.Reason != ReasonModelError {
				t.Fatalf("first = %+v, %v", settled, err)
			}
			if policy == AfterFailedContinue {
				<-busyReached
				state := liveOf(h, root)
				if state.Run == nil || len(state.Run.Inputs) != 1 || state.Run.Inputs[0] != second.ID() {
					t.Fatalf("run = %+v", state.Run)
				}
				if state.RunFailure == nil || state.RunFailure.Reason != ReasonModelError ||
					len(state.RunFailure.Inputs) != 1 || state.RunFailure.Inputs[0] != first.ID() {
					t.Fatalf("failure = %+v", state.RunFailure)
				}
			} else {
				if err := h.WaitForIdle(ctx, root); err != nil {
					t.Fatal(err)
				}
				failure := liveOf(h, root).RunFailure
				if failure == nil || failure.Reason != ReasonModelError || len(failure.Inputs) != 1 || failure.Inputs[0] != first.ID() {
					t.Fatalf("failure = %+v", failure)
				}
			}
		})
	}
}
