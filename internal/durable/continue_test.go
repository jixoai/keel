package durable

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
)

// Ported from packages/durable/test/harness-continue.test.ts.

func answerTextOf(t *testing.T, h *Harness, conv *Conversation, settled SubmissionRecord) string {
	t.Helper()
	entry, ok := entryByID(h, conv, settled.AnswerID)
	if !ok || len(entry.Model) == 0 {
		t.Fatalf("no answer entry for %+v", settled)
	}
	msg, ok := entry.Model[0].(agentcore.AssistantMessage)
	if !ok {
		t.Fatalf("answer is %T", entry.Model[0])
	}
	return textOfMessage(msg)
}

func userEntryCount(h *Harness, conv *Conversation) int {
	n := 0
	for _, e := range h.Entries(conv) {
		if e.Kind == "pi.user" {
			n++
		}
	}
	return n
}

func TestContinue_AnswersARunThatFailedWithoutANewUserMessage(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	var seen [][]string
	faux.setResponses(
		errorMessage("Invalid request"),
		func(_ context.Context, req Request) (agentcore.AssistantMessage, error) {
			seen = append(seen, rolesOf(req.Messages))
			return textMessage("recovered"), nil
		},
	)
	h, root := openChat(t, nil, Options{Provider: faux})
	first, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("Analyze this")})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := first.Wait(ctx); err != nil || settled.Reason != ReasonModelError {
		t.Fatalf("first = %+v, %v", settled, err)
	}
	if failure := liveOf(h, root).RunFailure; failure == nil || failure.Reason != ReasonModelError {
		t.Fatalf("failure = %+v", failure)
	}
	resumed, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionContinue})
	if err != nil {
		t.Fatal(err)
	}
	if rec, _ := resumed.Status(); rec.Status != SubmissionPlaced {
		t.Fatalf("continue = %+v", rec)
	}
	settled, err := resumed.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Type != SubmissionContinue || answerTextOf(t, h, root, settled) != "recovered" {
		t.Fatalf("settled = %+v", settled)
	}
	if len(seen) == 0 || seen[len(seen)-1][len(seen[len(seen)-1])-1] != "user" {
		t.Fatalf("request roles = %v", seen)
	}
	if rec, _ := first.Status(); rec.Status != SubmissionUnanswered {
		t.Fatalf("first status = %+v", rec)
	}
	if n := userEntryCount(h, root); n != 1 {
		t.Fatalf("user entries = %d", n)
	}
	if failure := liveOf(h, root).RunFailure; failure != nil {
		t.Fatalf("failure not cleared: %+v", failure)
	}
}

func TestContinue_AnswersTheToolResultsOfARoundAbortedMidway(t *testing.T) {
	ctx := ctxT(t)
	started := newDeferred()
	waitTool := &Tool{
		Name: "wait", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			started.resolve()
			<-ctx.Done()
			return ToolResult{}, ctx.Err()
		},
	}
	faux := &fauxProvider{}
	var seen [][]string
	faux.setResponses(
		calls(callSpec{name: "wait", args: map[string]any{}, id: "c1"}),
		func(_ context.Context, req Request) (agentcore.AssistantMessage, error) {
			seen = append(seen, rolesOf(req.Messages))
			return textMessage("after the abort"), nil
		},
	)
	h, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{waitTool}})
	first, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	<-started.ch
	if err := root.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	if settled, err := first.Wait(ctx); err != nil || settled.Reason != ReasonAborted {
		t.Fatalf("first = %+v, %v", settled, err)
	}
	settled, err := func() (SubmissionRecord, error) {
		sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionContinue})
		if err != nil {
			return SubmissionRecord{}, err
		}
		return sub.Wait(ctx)
	}()
	if err != nil {
		t.Fatal(err)
	}
	if got := answerTextOf(t, h, root, settled); got != "after the abort" {
		t.Fatalf("answer = %q", got)
	}
	if last := seen[len(seen)-1]; last[len(last)-1] != "toolResult" {
		t.Fatalf("request roles = %v", seen)
	}
}

func TestContinue_SkipsSystemMessagesWhenDecidingThereIsSomethingToAnswer(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	var seen [][]string
	faux.setResponses(
		textMessage("first answer"),
		errorMessage("Invalid request"),
		func(_ context.Context, req Request) (agentcore.AssistantMessage, error) {
			seen = append(seen, rolesOf(req.Messages))
			return textMessage("resumed"), nil
		},
	)
	h, root := openChat(t, nil, Options{Provider: faux})
	if err := root.SetInstructions(ctx, "Be thorough."); err != nil {
		t.Fatal(err)
	}
	one, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("one")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = one.Wait(ctx)
	if err := root.SetInstructions(ctx, "Be brief."); err != nil {
		t.Fatal(err)
	}
	two, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("two")})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := two.Wait(ctx); err != nil || settled.Reason != ReasonModelError {
		t.Fatalf("two = %+v, %v", settled, err)
	}
	roles := rolesOf(root.Context())
	if len(roles) == 0 || roles[len(roles)-1] != "system" {
		t.Fatalf("context roles = %v", roles)
	}
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionContinue})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := sub.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := answerTextOf(t, h, root, settled); got != "resumed" {
		t.Fatalf("answer = %q", got)
	}
}

func TestContinue_IsRejectedWhileBusyAfterRequestIDDeduplication(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	busyStep, reached, release := gated(textMessage("later"))
	faux.setResponses(errorMessage("Invalid request"), busyStep)
	_, root := openChat(t, nil, Options{Provider: faux})
	first, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = first.Wait(ctx)
	resumed, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionContinue, RequestID: "resume-1"})
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	if _, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionContinue}); err == nil {
		t.Fatalf("continue while busy was accepted")
	} else if _, ok := err.(*ConversationBusy); !ok {
		t.Fatalf("error = %v", err)
	}
	again, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionContinue, RequestID: "resume-1"})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID() != resumed.ID() {
		t.Fatalf("request id not deduplicated")
	}
	release.resolve()
	_, _ = resumed.Wait(ctx)
}

func TestContinue_EndsWithoutARequestWhenThereIsNothingToAnswer(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	faux.setResponses(textMessage("answered"))
	h, root := openChat(t, nil, Options{Provider: faux})
	empty, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionContinue})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := empty.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Status != SubmissionUnanswered || settled.Reason != ReasonNothingToAnswer || settled.Type != SubmissionContinue {
		t.Fatalf("empty = %+v", settled)
	}
	hi, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("hi")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = hi.Wait(ctx)
	calls := faux.callCount()
	after, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionContinue})
	if err != nil {
		t.Fatal(err)
	}
	settled, err = after.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Status != SubmissionUnanswered || settled.Reason != ReasonNothingToAnswer {
		t.Fatalf("after = %+v", settled)
	}
	if faux.callCount() != calls {
		t.Fatalf("continue after an answer made a request")
	}
	if failure := liveOf(h, root).RunFailure; failure == nil || failure.Reason != ReasonNothingToAnswer {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestContinue_ContinuesAnAnswerWithToolCallsCutOffAtItsOutputLimit(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	var seen [][]string
	emptyArgs := json.RawMessage(`{}`)
	cut := agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList{agentcore.NewTextContent("calling"), agentcore.NewToolCallContent("c1", "echo", emptyArgs)},
		StopReason: agentcore.StopReasonLength,
	}
	faux.setResponses(cut, func(_ context.Context, req Request) (agentcore.AssistantMessage, error) {
		seen = append(seen, rolesOf(req.Messages))
		return textMessage("resumed"), nil
	})
	h, root := openChat(t, nil, Options{Provider: faux})
	first, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := first.Wait(ctx); err != nil || settled.Status != SubmissionDone {
		t.Fatalf("first = %+v, %v", settled, err)
	}
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionContinue})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := sub.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := answerTextOf(t, h, root, settled); got != "resumed" {
		t.Fatalf("answer = %q", got)
	}
	last := seen[len(seen)-1]
	if len(last) < 2 || last[len(last)-2] != "assistant" || last[len(last)-1] != "toolResult" {
		t.Fatalf("request roles = %v", seen)
	}
}

func TestContinue_DoesNotContinueATextAnswerCutOffAtItsOutputLimit(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	cut := textMessage("cut")
	cut.StopReason = agentcore.StopReasonLength
	faux.setResponses(cut)
	_, root := openChat(t, nil, Options{Provider: faux})
	first, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("write a lot")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = first.Wait(ctx)
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionContinue})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := sub.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Status != SubmissionUnanswered || settled.Reason != ReasonNothingToAnswer {
		t.Fatalf("settled = %+v", settled)
	}
	if faux.callCount() != 1 {
		t.Fatalf("calls = %d", faux.callCount())
	}
}

func TestContinue_AnswersARunInterruptedByAHostThatDiedWhileTheModelAnswered(t *testing.T) {
	ctx := ctxT(t)
	store := NewMemStore()
	faux1 := &fauxProvider{}
	step, reached, _ := gated(textMessage("never"))
	faux1.setResponses(step)
	h1, root1 := openChat(t, store, Options{Provider: faux1})
	sub, err := root1.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("hi")})
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	id := sub.ID()
	h1.Kill()

	faux2 := &fauxProvider{}
	var seen [][]string
	faux2.setResponses(func(_ context.Context, req Request) (agentcore.AssistantMessage, error) {
		seen = append(seen, rolesOf(req.Messages))
		return textMessage("after the crash"), nil
	})
	h2, root2 := openChat(t, store, Options{Provider: faux2, Settings: Settings{Retry: &RetryPolicy{Enabled: false, MaxRetries: 0}}})
	h2.Recover()
	reopened, ok := h2.Submission(id)
	if !ok {
		t.Fatalf("submission not reacquired")
	}
	interrupted, err := reopened.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.Status != SubmissionUnanswered || interrupted.Reason != ReasonInterrupted {
		t.Fatalf("interrupted = %+v", interrupted)
	}
	resumed, err := root2.Submit(ctx, SubmissionDraft{Type: SubmissionContinue})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := resumed.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := answerTextOf(t, h2, root2, settled); got != "after the crash" {
		t.Fatalf("answer = %q", got)
	}
	if last := seen[len(seen)-1]; last[len(last)-1] != "user" {
		t.Fatalf("request roles = %v", seen)
	}
}

func TestContinue_PlacesAResetAFailedRunKeptAndFindsNothingToAnswer(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	step, reached, release := gated(errorMessage("Invalid request"))
	faux.setResponses(step)
	h, root := openChat(t, nil, Options{Provider: faux})
	first, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("first")})
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	if err := root.Reset(ctx, ""); err != nil {
		t.Fatal(err)
	}
	release.resolve()
	if settled, err := first.Wait(ctx); err != nil || settled.Reason != ReasonModelError {
		t.Fatalf("first = %+v, %v", settled, err)
	}
	if items := h.Inbox(root); len(items) != 1 {
		t.Fatalf("inbox = %+v", items)
	}
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionContinue})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := sub.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Status != SubmissionUnanswered || settled.Reason != ReasonNothingToAnswer {
		t.Fatalf("continue = %+v", settled)
	}
	if items := h.Inbox(root); len(items) != 0 {
		t.Fatalf("inbox after continue = %+v", items)
	}
	msgs := root.Context()
	for _, m := range msgs {
		if m.Role != "system" {
			t.Fatalf("context not empty: %v", textMessages(msgs))
		}
	}
	if faux.callCount() != 1 {
		t.Fatalf("calls = %d", faux.callCount())
	}
}

func TestContinue_StartsTheInputsQueuedBehindAContinueWithNothingToAnswerUnderAfterFailedRunContinue(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	faux.setResponses(textMessage("answered"), textMessage("second answered"))
	h, root := openChat(t, nil, Options{Provider: faux, Settings: Settings{AfterFailedRun: AfterFailedContinue}})
	one, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("one")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = one.Wait(ctx)
	resumed, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionContinue})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("two"), WhenBusy: "followUp"})
	if err != nil {
		t.Fatal(err)
	}
	if rec, _ := queued.Status(); rec.Status != SubmissionQueued {
		t.Fatalf("queued = %+v", rec)
	}
	if settled, err := resumed.Wait(ctx); err != nil || settled.Reason != ReasonNothingToAnswer {
		t.Fatalf("resumed = %+v, %v", settled, err)
	}
	settled, err := queued.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := answerTextOf(t, h, root, settled); got != "second answered" {
		t.Fatalf("answer = %q", got)
	}
}

func TestContinue_PlacesInputsAFailedRunLeftQueuedFirstAndTheyJoinItsRun(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	step, reached, release := gated(errorMessage("Invalid request"))
	faux.setResponses(step, textMessage("both handled"))
	h, root := openChat(t, nil, Options{Provider: faux})
	first, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("first")})
	if err != nil {
		t.Fatal(err)
	}
	_ = first
	<-reached
	queued, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("second"), WhenBusy: "followUp"})
	if err != nil {
		t.Fatal(err)
	}
	release.resolve()
	if err := h.WaitForIdle(ctx, root); err != nil {
		t.Fatal(err)
	}
	if rec, _ := queued.Status(); rec.Status != SubmissionQueued {
		t.Fatalf("queued = %+v", rec)
	}
	resumed, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionContinue})
	if err != nil {
		t.Fatal(err)
	}
	live := h.Live(root)
	if live.Run == nil || len(live.Run.Inputs) != 2 || live.Run.Inputs[0] != queued.ID() || live.Run.Inputs[1] != resumed.ID() {
		t.Fatalf("run = %+v", live.Run)
	}
	byQueued, err := queued.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byContinue, err := resumed.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := answerTextOf(t, h, root, byContinue); got != "both handled" {
		t.Fatalf("answer = %q", got)
	}
	if byQueued.Status != SubmissionDone || byQueued.AnswerID != byContinue.AnswerID {
		t.Fatalf("queued = %+v", byQueued)
	}
}
