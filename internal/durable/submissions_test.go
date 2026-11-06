package durable

import (
	"context"
	"testing"
)

// Ported from packages/durable/test/harness-submissions.test.ts
// (upstream earendil-works/pi @ 55a5c69). Test names keep PI's it(...) titles.

func TestSubmissions_AppendsAnIdleWriteAndSettlesItDoneWithoutATurn(t *testing.T) {
	ctx := ctxT(t)
	h, root := openChat(t, nil, Options{Provider: &fauxProvider{}})
	draft := SubmissionDraft{Type: SubmissionWrite, Write: &EntryDraft{
		Kind: "note", Data: raw(map[string]string{"text": "x"}),
	}}
	sub, err := root.Submit(ctx, draft)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := sub.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != SubmissionDone || rec.EntryID == 0 {
		t.Fatalf("settled = %+v", rec)
	}
	entries := h.Entries(root)
	if len(entries) != 1 || entries[0].Kind != "note" || entries[0].ID != rec.EntryID {
		t.Fatalf("entries = %+v", entries)
	}
	if live := h.Live(root); live.Run != nil {
		t.Fatalf("write started a run: %+v", live.Run)
	}
	if tasks := h.Tasks(root); len(tasks) != 0 {
		t.Fatalf("tasks = %+v", tasks)
	}
}

func TestSubmissions_PlacesIdleInputAndRejectsBusyInputWithWhenBusyRejectWithoutWriting(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	step, reached, release := gated(textMessage("late"))
	faux.setResponses(step)
	h, root := openChat(t, nil, Options{Provider: faux})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("hi")})
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	rec, err := sub.Status()
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != SubmissionPlaced || rec.EntryID == 0 {
		t.Fatalf("record = %+v", rec)
	}
	entry := h.Entries(root)[0]
	if got := textMessages(root.Context())[0]; got != "user:hi" {
		t.Fatalf("context = %q", got)
	}
	live := h.Live(root)
	if live.Run == nil || len(live.Run.Inputs) != 1 || live.Run.Inputs[0] != sub.ID() {
		t.Fatalf("live run = %+v", live.Run)
	}
	task, ok := h.Task(live.Run.TaskID)
	if !ok || task.Kind != GenerationTaskKind {
		t.Fatalf("generation task = %+v", task)
	}
	before := len(h.Store().ListSubmissions(root.ID()))
	_, err = root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("again"), WhenBusy: "reject"})
	if _, ok := err.(*ConversationBusy); !ok {
		t.Fatalf("error = %v, want ConversationBusy", err)
	}
	if after := len(h.Store().ListSubmissions(root.ID())); after != before {
		t.Fatalf("rejected input wrote a submission: %d -> %d", before, after)
	}
	_ = entry
	release.resolve()
	if _, err := sub.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSubmissions_DeduplicatesRequestIDsPerConversationBeforeAnyWrite(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	step, reached, release := gated(textMessage("late"))
	faux.setResponses(step)
	h, root := openChat(t, nil, Options{Provider: faux})
	first, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("hi"), RequestID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	before := len(h.Store().ListSubmissions(""))
	again, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("different"), RequestID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID() != first.ID() {
		t.Fatalf("dedupe ids differ: %s vs %s", again.ID(), first.ID())
	}
	if after := len(h.Store().ListSubmissions("")); after != before {
		t.Fatalf("dedupe wrote a submission: %d -> %d", before, after)
	}
	_, err = root.Submit(ctx, SubmissionDraft{Type: SubmissionWrite, Write: &EntryDraft{Kind: "note"}, RequestID: "r1"})
	if err == nil || err.Error() != "Request r1 already identifies a submission of type input" {
		t.Fatalf("error = %v", err)
	}
	rec, _ := first.Status()
	if rec.RequestID != "r1" || rec.Status != SubmissionPlaced {
		t.Fatalf("record = %+v", rec)
	}
	other := h.CreateConversation()
	write, err := other.Submit(ctx, SubmissionDraft{Type: SubmissionWrite, Write: &EntryDraft{Kind: "note"}, RequestID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if write.ID() == first.ID() {
		t.Fatalf("other conversation reused the submission")
	}
	againWrite, err := other.Submit(ctx, SubmissionDraft{Type: SubmissionWrite, Write: &EntryDraft{Kind: "note"}, RequestID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if againWrite.ID() != write.ID() {
		t.Fatalf("other conversation dedupe failed")
	}
	release.resolve()
	_, _ = first.Wait(ctx)
}

func TestSubmissions_ReportsAbortResultsAndLooksSubmissionsUpByConversation(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	step, _, release := gated(textMessage("answer"))
	faux.setResponses(step)
	h, root := openChat(t, nil, Options{Provider: faux})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("hi")})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := sub.Abort(ctx); err != nil || got != AbortAlreadyPlaced {
		t.Fatalf("abort = %v, %v", got, err)
	}
	if got, err := h.abortSubmission(ctx, sub.ID(), root.ID()); err != nil || got != AbortAlreadyPlaced {
		t.Fatalf("abort by conversation = %v, %v", got, err)
	}
	other := h.CreateConversation()
	if got, err := h.abortSubmission(ctx, sub.ID(), other.ID()); err != nil || got != AbortNotFound {
		t.Fatalf("abort other conversation = %v, %v", got, err)
	}
	if got, err := h.abortSubmission(ctx, "missing", ""); err != nil || got != AbortNotFound {
		t.Fatalf("abort missing = %v, %v", got, err)
	}
	if _, ok := h.Submission("missing"); ok {
		t.Fatalf("missing submission found")
	}
	release.resolve()
	if _, err := sub.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := sub.Abort(ctx); err != nil || got != AbortSettled {
		t.Fatalf("abort settled = %v, %v", got, err)
	}
}

func TestSubmissions_CancelsOnlyAWait(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	step, reached, release := gated(textMessage("late"))
	faux.setResponses(step)
	h, root := openChat(t, nil, Options{Provider: faux})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("hi")})
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	waitCtx, cancel := context.WithCancel(ctx)
	cancelled := make(chan error, 1)
	go func() {
		_, err := sub.Wait(waitCtx)
		cancelled <- err
	}()
	cancel()
	if err := <-cancelled; err == nil {
		t.Fatalf("cancelled wait succeeded")
	}
	rec, _ := sub.Status()
	if rec.Status != SubmissionPlaced {
		t.Fatalf("status after cancel = %s", rec.Status)
	}
	pending := make(chan error, 1)
	go func() {
		_, err := sub.Wait(ctx)
		pending <- err
	}()
	h.Close()
	if err := <-pending; err == nil {
		t.Fatalf("wait after close did not fail")
	}
	release.resolve()
}

func TestSubmissions_ReacquiresASubmissionAfterReopenAndSettlesItDurably(t *testing.T) {
	ctx := ctxT(t)
	store := NewMemStore()
	faux1 := &fauxProvider{}
	step, reached, _ := gated(textMessage("never"))
	faux1.setResponses(step)
	h1, root1 := openChat(t, store, Options{Provider: faux1})
	sub, err := root1.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("hi"), RequestID: "print"})
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	h1.Close()

	faux2 := &fauxProvider{}
	faux2.setResponses(textMessage("after reopen"))
	h2, root2 := openChat(t, store, Options{Provider: faux2})
	h2.Recover()
	reopened, ok := h2.Submission(sub.ID())
	if !ok {
		t.Fatalf("submission not reacquired")
	}
	if rec, _ := reopened.Status(); rec.Status != SubmissionPlaced {
		t.Fatalf("status after reopen = %s", rec.Status)
	}
	settled, err := reopened.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Status != SubmissionDone || settled.AnswerID == 0 {
		t.Fatalf("settled = %+v", settled)
	}
	again, err := root2.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("hi"), RequestID: "print"})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID() != sub.ID() {
		t.Fatalf("request id not deduplicated after reopen")
	}
}

func TestSubmissions_SettlesSubmissionsByTheirCurrentRecord(t *testing.T) {
	// PI settles a submission created in the caller's own commit, and a second
	// settlement leaves the first. The Go transaction is the Store update itself.
	h, root := openChat(t, nil, Options{Provider: &fauxProvider{}})
	entry := h.Store().AppendEntry(root.ID(), EntryRecord{Kind: "note"})
	rec := h.Store().CreateSubmission(SubmissionRecord{
		ConversationID: root.ID(), Type: SubmissionInput, Status: SubmissionQueued, EntryID: entry.ID,
	})
	h.mu.Lock()
	h.settleSubmissionLocked(rec.ID, SubmissionDone, "", "", 0, entry.ID)
	h.settleSubmissionLocked(rec.ID, SubmissionUnanswered, "late", "", 0, 0)
	h.mu.Unlock()
	got, _ := h.Store().GetSubmission(rec.ID)
	if got.Status != SubmissionDone || got.AnswerID != entry.ID || got.Reason != "" {
		t.Fatalf("settled = %+v", got)
	}
}
