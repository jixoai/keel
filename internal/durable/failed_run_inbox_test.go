package durable

import (
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
)

// Ported from packages/durable/test/harness-failed-run-inbox.test.ts.

func failingOnce() (fauxStep, <-chan struct{}, *deferred) {
	return gated(errorMessage("Invalid request"))
}

func failWithQueuedInput(t *testing.T, policy string, settings Settings) (*Harness, *Conversation, *Submission) {
	t.Helper()
	ctx := ctxT(t)
	faux := &fauxProvider{}
	step, reached, release := failingOnce()
	faux.setResponses(step, textMessage("second answer"), textMessage("third answer"))
	if policy != "" {
		settings.AfterFailedRun = policy
	}
	h, root := openChat(t, nil, Options{Provider: faux, Settings: settings})
	first, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("Analyze this report")})
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	second, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("Email me the summary"), WhenBusy: "followUp"})
	if err != nil {
		t.Fatal(err)
	}
	release.resolve()
	if settled, err := first.Wait(ctx); err != nil || settled.Reason != ReasonModelError {
		t.Fatalf("first = %+v, %v", settled, err)
	}
	if err := h.WaitForIdle(ctx, root); err != nil {
		t.Fatal(err)
	}
	return h, root, second
}

func TestInputsQueuedBehindAFailedRun_StayQueuedByDefault(t *testing.T) {
	ctx := ctxT(t)
	h, root, second := failWithQueuedInput(t, "", Settings{})
	rec, err := second.Status()
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != SubmissionQueued {
		t.Fatalf("second = %+v", rec)
	}
	third, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("Anything else?")})
	if err != nil {
		t.Fatal(err)
	}
	if rec, err := second.Wait(ctx); err != nil || rec.Status != SubmissionDone {
		t.Fatalf("second settled = %+v, %v", rec, err)
	}
	if rec, err := third.Wait(ctx); err != nil || rec.Status != SubmissionDone {
		t.Fatalf("third settled = %+v, %v", rec, err)
	}
	_ = h
}

func TestInputsQueuedBehindAFailedRun_StartTheNextRunWithContinue(t *testing.T) {
	ctx := ctxT(t)
	h, root, second := failWithQueuedInput(t, AfterFailedContinue, Settings{})
	settled, err := second.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Status != SubmissionDone || settled.Type != SubmissionInput {
		t.Fatalf("settled = %+v", settled)
	}
	answer, ok := entryByID(h, root, settled.AnswerID)
	if !ok || len(answer.Model) == 0 {
		t.Fatalf("answer entry = %+v", answer)
	}
	assistant, ok := answer.Model[0].(agentcore.AssistantMessage)
	if !ok || textOfMessage(assistant) != "second answer" {
		t.Fatalf("answer = %+v", answer.Model)
	}
}

func TestInputsQueuedBehindAFailedRun_PlaceQueuedWritesAndKeepThemWithWithdraw(t *testing.T) {
	for _, policy := range []string{AfterFailedContinue, AfterFailedWithdraw} {
		t.Run(policy, func(t *testing.T) {
			ctx := ctxT(t)
			faux := &fauxProvider{}
			step, reached, release := failingOnce()
			faux.setResponses(step, textMessage("second answer"))
			h, root := openChat(t, nil, Options{Provider: faux, Settings: Settings{AfterFailedRun: policy}})
			first, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("first")})
			if err != nil {
				t.Fatal(err)
			}
			<-reached
			write, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionWrite, Write: &EntryDraft{
				Kind: "test.note",
				Model: []agentcore.Message{agentcore.UserMessage{
					RoleField: agentcore.RoleUser,
					Content:   agentcore.ContentList{agentcore.NewTextContent("a note the model sees")},
					Timestamp: 1,
				}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			if rec, _ := write.Status(); rec.Status != SubmissionQueued {
				t.Fatalf("write = %+v", rec)
			}
			release.resolve()
			if settled, err := first.Wait(ctx); err != nil || settled.Reason != ReasonModelError {
				t.Fatalf("first = %+v, %v", settled, err)
			}
			if err := h.WaitForIdle(ctx, root); err != nil {
				t.Fatal(err)
			}
			rec, _ := write.Status()
			want := SubmissionQueued
			if policy == AfterFailedContinue {
				want = SubmissionDone
			}
			if rec.Status != want {
				t.Fatalf("write after %s = %+v, want %s", policy, rec, want)
			}
		})
	}
}

func TestInputsQueuedBehindAFailedRun_SettleUnansweredWithRunFailedWithWithdraw(t *testing.T) {
	ctx := ctxT(t)
	_, _, second := failWithQueuedInput(t, AfterFailedWithdraw, Settings{})
	settled, err := second.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Status != SubmissionUnanswered || settled.Reason != ReasonRunFailed {
		t.Fatalf("settled = %+v", settled)
	}
}
