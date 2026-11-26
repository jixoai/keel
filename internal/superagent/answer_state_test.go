package superagent_test

// Answer/pending durability regressions.
//
// The answer protocol is ordered: durable intent -> Attention answer ->
// delivery + receipt. These cases pin every interruption the previous rounds
// got wrong:
//
//   - an Attention answered without any delivery evidence is repaired on retry
//     (or explicitly fails when the ticket is closed), never silently accepted;
//   - a failed intent write leaves the Attention open (nothing is recorded);
//   - a transient delivery failure retries forever (30s cap), not 10 times;
//   - untrusted context never starts a worker and never unblocks a ticket;
//   - a repeated issue-edit bump is captured once;
//   - a permanently failed entry is visible on the Board and an operator can
//     retry or dismiss it;
//   - a delivered answer whose Channel comment was never enqueued is repaired by
//     the startup compensation scan.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/superagent"
)

func ticketByThread(t *testing.T, env *testEnv, threadID string) superagent.TicketView {
	t.Helper()
	board, err := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if err != nil {
		t.Fatalf("board: %v", err)
	}
	for _, row := range board.Tickets {
		if row.Thread.ID == threadID {
			return row
		}
	}
	t.Fatalf("ticket for thread %q not found in %+v", threadID, board.Tickets)
	return superagent.TicketView{}
}

func askAndBlock(t *testing.T, env *testEnv, ticket superagent.TicketView, request collab.AttentionRequest) collab.Attention {
	t.Helper()
	ctx := context.Background()
	attention, err := env.orchestrator.Ask(ctx, ticket.ID, ticket.ActiveAssignment.ID, request)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if err := env.orchestrator.Report(ctx, superagent.ReportRequest{
		TenantID: env.tenant, WorkItemID: ticket.ID, AssignmentID: ticket.ActiveAssignment.ID,
		State: superagent.ReportWaiting, Summary: "waiting", AttentionID: attention.ID,
	}); err != nil {
		t.Fatalf("report waiting: %v", err)
	}
	return attention
}

// TestAnsweredAttentionWithoutDeliveryEvidenceIsRepaired is the P0-1 regression:
// the old code could leave the Attention answered before the pending input was
// written (a crash in between). A retry then returned success though no worker
// would ever receive the answer. The retry must now deliver (here to the live
// worker) and record a durable receipt; a second retry is idempotent.
func TestAnsweredAttentionWithoutDeliveryEvidenceIsRepaired(t *testing.T) {
	env := newEnv(t, envOpts{})
	ctx := context.Background()
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	ticket := ticketByThread(t, env, "issue#1")
	attention := askAndBlock(t, env, ticket, collab.AttentionRequest{
		Prompt: "Approve?", Options: []collab.AttentionOption{{ID: "approve"}}, AllowFreeText: true,
	})

	// Simulate the crash window: the Attention row is answered directly through
	// the Host, so the Ticket has neither an intent nor a receipt.
	if _, err := env.host.AnswerAttention(ctx, env.tenant, ticket.ID, attention.ID, collab.AttentionAnswerRequest{
		OptionID: "approve", Actor: "alice",
	}); err != nil {
		t.Fatalf("direct attention answer: %v", err)
	}
	broken := ticketByThread(t, env, "issue#1")
	if len(broken.Pending) != 0 || broken.AnswerReceiptFor(attention.ID) != nil {
		t.Fatalf("precondition: pending=%+v receipt=%+v", broken.Pending, broken.AnswerReceiptFor(attention.ID))
	}

	// The retry must repair the delivery, not report success on the answered row.
	if _, err := env.orchestrator.AnswerAttention(ctx, env.tenant, attention.ID, "approve", "", "alice"); err != nil {
		t.Fatalf("retry must deliver and succeed: %v", err)
	}
	if env.sink.count() != 1 || !strings.Contains(env.sink.last().Text, "Human selected: approve") {
		t.Fatalf("delivery = %+v, want the recovered answer", env.sink.delivered)
	}
	repaired := ticketByThread(t, env, "issue#1")
	if repaired.AnswerReceiptFor(attention.ID) == nil {
		t.Fatal("repaired answer must leave a durable delivery receipt")
	}
	if len(repaired.Pending) != 0 {
		t.Fatalf("pending after repair = %+v", repaired.Pending)
	}
	if repaired.Status != collab.WorkItemActive {
		t.Fatalf("status after repair = %s, want active", repaired.Status)
	}

	// A second retry is idempotent: the receipt is the success fact, no
	// duplicate delivery and no new worker.
	if _, err := env.orchestrator.AnswerAttention(ctx, env.tenant, attention.ID, "approve", "", "alice"); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if env.sink.count() != 1 {
		t.Fatalf("idempotent retry delivered again: %+v", env.sink.delivered)
	}
}

// TestAnsweredAttentionWithoutEvidenceOnAClosedTicketFailsExplicitly is the
// other half of P0-1: when no delivery can be made (the ticket already closed),
// a retry must say so instead of returning success.
func TestAnsweredAttentionWithoutEvidenceOnAClosedTicketFailsExplicitly(t *testing.T) {
	env := newEnv(t, envOpts{})
	ctx := context.Background()
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	ticket := ticketByThread(t, env, "issue#1")
	attention := askAndBlock(t, env, ticket, collab.AttentionRequest{
		Prompt: "Approve?", Options: []collab.AttentionOption{{ID: "approve"}},
	})
	if _, err := env.host.AnswerAttention(ctx, env.tenant, ticket.ID, attention.ID, collab.AttentionAnswerRequest{
		OptionID: "approve", Actor: "alice",
	}); err != nil {
		t.Fatalf("direct attention answer: %v", err)
	}
	item, err := env.store.GetWorkItem(ctx, env.tenant, ticket.ID)
	if err != nil {
		t.Fatalf("get work item: %v", err)
	}
	item.Status = collab.WorkItemDone
	if _, err := env.store.UpdateWorkItem(ctx, item); err != nil {
		t.Fatalf("close ticket: %v", err)
	}
	if _, err := env.orchestrator.AnswerAttention(ctx, env.tenant, attention.ID, "approve", "", "alice"); !errors.Is(err, collab.ErrWorkItemClosed) {
		t.Fatalf("retry on a closed ticket = %v, want ErrWorkItemClosed (409)", err)
	}
	if env.sink.count() != 0 || env.launcher.count() != 1 {
		t.Fatalf("closed-ticket retry delivered input: sink=%d launches=%d", env.sink.count(), env.launcher.count())
	}
}

// TestOpenAttentionWithADurableIntentIsCompletedBeforeDelivery pins the recovery
// half of the write order: an intent that outlived a crash (Attention still
// open) is completed by the pending pump before any worker is told anything.
func TestOpenAttentionWithADurableIntentIsCompletedBeforeDelivery(t *testing.T) {
	env := newEnv(t, envOpts{})
	ctx := context.Background()
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	ticket := ticketByThread(t, env, "issue#1")
	attention := askAndBlock(t, env, ticket, collab.AttentionRequest{
		Prompt: "Approve?", Options: []collab.AttentionOption{{ID: "approve"}},
	})
	item, err := env.store.GetWorkItem(ctx, env.tenant, ticket.ID)
	if err != nil {
		t.Fatalf("get work item: %v", err)
	}
	doc := superagent.TicketFromWorkItem(item)
	doc.Pending = append(doc.Pending, superagent.PendingFollowUp{
		ID: "attention:" + attention.ID.String(), Kind: superagent.PendingAnswer,
		Text: "Human selected: approve", AttentionID: attention.ID,
		OptionID: "approve", AnswerActor: "alice", Trusted: true, AllowDispatch: true,
		CreatedAt: *env.clock,
	})
	if _, err := env.store.UpdateWorkItem(ctx, doc.Record()); err != nil {
		t.Fatalf("write answer intent: %v", err)
	}
	if env.sink.count() != 0 {
		t.Fatalf("no delivery may happen before the decision is recorded: %+v", env.sink.delivered)
	}
	if err := env.orchestrator.Housekeeping(ctx); err != nil {
		t.Fatalf("housekeeping: %v", err)
	}
	stored, err := env.store.GetAttention(ctx, env.tenant, attention.ID)
	if err != nil {
		t.Fatalf("get attention: %v", err)
	}
	if stored.Status != collab.AttentionAnswered || stored.Answer == nil || stored.Answer.OptionID != "approve" {
		t.Fatalf("attention = %+v, want the replayed approve decision", stored)
	}
	if env.sink.count() != 1 || env.sink.last().Kind != "attention_answer" {
		t.Fatalf("deliveries = %+v, want one answer", env.sink.delivered)
	}
	after := ticketByThread(t, env, "issue#1")
	if after.AnswerReceiptFor(attention.ID) == nil || len(after.Pending) != 0 {
		t.Fatalf("after recovery: receipt=%+v pending=%+v", after.AnswerReceiptFor(attention.ID), after.Pending)
	}
}

// TestStaleAnswerIntentIsNotDelivered: the intent says approve but another path
// answered reject. Housekeeping must not deliver the stale intent; it surfaces a
// permanent failure for the operator instead.
func TestStaleAnswerIntentIsNotDelivered(t *testing.T) {
	env := newEnv(t, envOpts{})
	ctx := context.Background()
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	ticket := ticketByThread(t, env, "issue#1")
	attention := askAndBlock(t, env, ticket, collab.AttentionRequest{
		Prompt: "Approve?", Options: []collab.AttentionOption{{ID: "approve"}, {ID: "reject"}},
	})
	item, err := env.store.GetWorkItem(ctx, env.tenant, ticket.ID)
	if err != nil {
		t.Fatalf("get work item: %v", err)
	}
	doc := superagent.TicketFromWorkItem(item)
	doc.Pending = append(doc.Pending, superagent.PendingFollowUp{
		ID: "attention:" + attention.ID.String(), Kind: superagent.PendingAnswer,
		Text: "Human selected: approve", AttentionID: attention.ID,
		OptionID: "approve", AnswerActor: "alice", Trusted: true, AllowDispatch: true,
		CreatedAt: *env.clock,
	})
	if _, err := env.store.UpdateWorkItem(ctx, doc.Record()); err != nil {
		t.Fatalf("write answer intent: %v", err)
	}
	if _, err := env.host.AnswerAttention(ctx, env.tenant, ticket.ID, attention.ID, collab.AttentionAnswerRequest{
		OptionID: "reject", Actor: "alice",
	}); err != nil {
		t.Fatalf("record the competing answer: %v", err)
	}
	if err := env.orchestrator.Housekeeping(ctx); err != nil {
		t.Fatalf("housekeeping: %v", err)
	}
	if env.sink.count() != 0 {
		t.Fatalf("a stale intent was delivered: %+v", env.sink.delivered)
	}
	after := ticketByThread(t, env, "issue#1")
	if len(after.Pending) != 1 || !after.Pending[0].Failed {
		t.Fatalf("pending = %+v, want one failed stale intent", after.Pending)
	}
}

type faultStore struct {
	*superagent.MemoryStore
	failUpdates       int
	failAttentionList int
}

func (s *faultStore) ListAttentionsByStatus(ctx context.Context, tenantID uuid.UUID, modeID string, limit int, statuses ...collab.AttentionStatus) ([]collab.Attention, error) {
	if s.failAttentionList > 0 {
		s.failAttentionList--
		return nil, errors.New("injected attention scan failure")
	}
	return s.MemoryStore.ListAttentionsByStatus(ctx, tenantID, modeID, limit, statuses...)
}

func (s *faultStore) UpdateWorkItem(ctx context.Context, item collab.WorkItem) (collab.WorkItem, error) {
	if s.failUpdates > 0 {
		s.failUpdates--
		return collab.WorkItem{}, errors.New("injected work item write failure")
	}
	return s.MemoryStore.UpdateWorkItem(ctx, item)
}

// TestAnswerIntentWriteFailureLeavesTheAttentionOpen pins the pending-first
// order: when the intent write fails, the Attention is never recorded as
// answered, so a retry can complete the whole protocol. Nothing is silently
// "answered but undeliverable".
func TestAnswerIntentWriteFailureLeavesTheAttentionOpen(t *testing.T) {
	fault := &faultStore{}
	env := newEnv(t, envOpts{store: func(inner *superagent.MemoryStore) superagent.Store {
		fault.MemoryStore = inner
		return fault
	}})
	ctx := context.Background()
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	ticket := ticketByThread(t, env, "issue#1")
	attention := askAndBlock(t, env, ticket, collab.AttentionRequest{
		Prompt: "Approve?", Options: []collab.AttentionOption{{ID: "approve"}},
	})

	fault.failUpdates = 1
	if _, err := env.orchestrator.AnswerAttention(ctx, env.tenant, attention.ID, "approve", "", "alice"); err == nil {
		t.Fatal("an intent write failure must fail the answer")
	}
	stored, err := env.store.GetAttention(ctx, env.tenant, attention.ID)
	if err != nil {
		t.Fatalf("get attention: %v", err)
	}
	if stored.Status != collab.AttentionOpen {
		t.Fatalf("attention status = %s, want open (the Host must not be called first)", stored.Status)
	}
	if env.sink.count() != 0 {
		t.Fatalf("no answer may be delivered before it is recorded: %+v", env.sink.delivered)
	}

	if _, err := env.orchestrator.AnswerAttention(ctx, env.tenant, attention.ID, "approve", "", "alice"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	stored, _ = env.store.GetAttention(ctx, env.tenant, attention.ID)
	if stored.Status != collab.AttentionAnswered {
		t.Fatalf("attention after retry = %s", stored.Status)
	}
	if env.sink.count() != 1 {
		t.Fatalf("retry deliveries = %+v", env.sink.delivered)
	}
}

// TestAnswerPendingSurvivesARestart: a failed delivery attempt leaves the intent
// durable; a new orchestrator process (same store) delivers it on recovery.
func TestAnswerPendingSurvivesARestart(t *testing.T) {
	env := newEnv(t, envOpts{})
	ctx := context.Background()
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	ticket := ticketByThread(t, env, "issue#1")
	attention := askAndBlock(t, env, ticket, collab.AttentionRequest{
		Prompt: "Approve?", Options: []collab.AttentionOption{{ID: "approve"}},
	})
	env.sink.failNext = 1
	if _, err := env.orchestrator.AnswerAttention(ctx, env.tenant, attention.ID, "approve", "", "alice"); !errors.Is(err, superagent.ErrAnswerDeliveryPending) {
		t.Fatalf("first answer = %v, want ErrAnswerDeliveryPending", err)
	}
	if env.sink.count() != 0 {
		t.Fatalf("failed attempt delivered: %+v", env.sink.delivered)
	}

	restarted := env.newOrchestrator(t, envOpts{})
	*env.clock = env.clock.Add(2 * time.Second)
	if _, err := restarted.Recover(ctx); err != nil {
		t.Fatalf("restart recovery: %v", err)
	}
	if env.sink.count() != 1 {
		t.Fatalf("restart deliveries = %+v, want the recovered answer", env.sink.delivered)
	}
	recovered := ticketByThread(t, env, "issue#1")
	if recovered.AnswerReceiptFor(attention.ID) == nil || len(recovered.Pending) != 0 {
		t.Fatalf("after restart: receipt=%+v pending=%+v", recovered.AnswerReceiptFor(attention.ID), recovered.Pending)
	}
}

// TestTransientDispatchFailureStillDeliversAfterTenRetries is the P1-1
// regression: only a permanent refusal may mark an entry Failed. A transient
// dispatch failure keeps retrying (30s cap), so the 11th housekeeping tick after
// the launcher recovers still delivers.
func TestTransientDispatchFailureStillDeliversAfterTenRetries(t *testing.T) {
	env := newEnv(t, envOpts{})
	ctx := context.Background()
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	ticket := ticketByThread(t, env, "issue#1")
	askAndBlock(t, env, ticket, collab.AttentionRequest{Prompt: "Approve?", AllowFreeText: true})
	assignment := *ticket.ActiveAssignment
	assignment.Status = collab.AssignmentFailed
	if _, err := env.store.UpdateAssignment(ctx, assignment); err != nil {
		t.Fatalf("fail assignment: %v", err)
	}
	env.launcher.setLaunchErr(errors.New("launcher down"))
	edited := superagent.Event{
		SourceID: "fixture", EventID: "edit-1", Kind: superagent.EventIssueUpdated,
		Actor: alice(), Thread: superagent.ThreadRef{Source: "fixture", Kind: "issue", ID: "issue#1"},
		Title: "Fix issue#1", Body: "Please fix it. Also update the tests.",
	}
	if _, err := env.orchestrator.HandleSourceEvent(ctx, fixtureEvent(t, edited)); err != nil {
		t.Fatalf("capture edit: %v", err)
	}
	for i := 0; i < 12; i++ {
		*env.clock = env.clock.Add(31 * time.Second)
		if err := env.orchestrator.Housekeeping(ctx); err != nil {
			t.Fatalf("housekeeping %d: %v", i+1, err)
		}
	}
	failed := ticketByThread(t, env, "issue#1")
	if len(failed.Pending) != 1 {
		t.Fatalf("pending = %+v", failed.Pending)
	}
	if failed.Pending[0].Failed {
		t.Fatalf("a transient failure became permanent after %d attempts: %+v", failed.Pending[0].Attempts, failed.Pending[0])
	}
	if failed.Pending[0].Attempts < 12 {
		t.Fatalf("attempts = %d, want at least 12", failed.Pending[0].Attempts)
	}

	env.launcher.setLaunchErr(nil)
	*env.clock = env.clock.Add(31 * time.Second)
	if err := env.orchestrator.Housekeeping(ctx); err != nil {
		t.Fatalf("recovered housekeeping: %v", err)
	}
	if launch := env.launcher.lastLaunch(); !strings.Contains(launch.Task, "Also update the tests") {
		t.Fatalf("launch task = %q, want the edited body", launch.Task)
	}
	if pending := ticketByThread(t, env, "issue#1").Pending; len(pending) != 0 {
		t.Fatalf("pending after recovery = %+v", pending)
	}
}

// TestUntrustedContextNeverStartsAWorker is the P1-2 regression: an untrusted
// comment captured while a worker was live survives as durable context, but
// when that worker ends it must not start a replacement. A trusted follow-up
// may start one; the surviving context is then delivered to it.
func TestUntrustedContextNeverStartsAWorker(t *testing.T) {
	env := newEnv(t, envOpts{})
	ctx := context.Background()
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	ticket := ticketByThread(t, env, "issue#1")
	env.sink.failNext = 1
	untrusted := superagent.Event{
		SourceID: "fixture", EventID: "c-untrusted", Kind: superagent.EventIssueComment,
		Actor:  superagent.Actor{ID: "u2", Login: "mallory"},
		Thread: superagent.ThreadRef{Source: "fixture", Kind: "issue", ID: "issue#1"},
		Body:   "untrusted context",
	}
	if _, err := env.orchestrator.HandleSourceEvent(ctx, fixtureEvent(t, untrusted)); err != nil {
		t.Fatalf("untrusted comment: %v", err)
	}
	pending := ticketByThread(t, env, "issue#1").Pending
	if len(pending) != 1 || pending[0].Failed || pending[0].AllowDispatch {
		t.Fatalf("untrusted pending = %+v, want one retryable non-dispatch entry", pending)
	}

	// The live worker ends.
	assignment := *ticket.ActiveAssignment
	assignment.Status = collab.AssignmentFailed
	if _, err := env.store.UpdateAssignment(ctx, assignment); err != nil {
		t.Fatalf("fail assignment: %v", err)
	}
	launches := env.launcher.count()
	*env.clock = env.clock.Add(31 * time.Second)
	if err := env.orchestrator.Housekeeping(ctx); err != nil {
		t.Fatalf("housekeeping: %v", err)
	}
	if env.launcher.count() != launches {
		t.Fatalf("untrusted context started a worker: %d -> %d", launches, env.launcher.count())
	}
	if env.sink.count() != 0 {
		t.Fatalf("untrusted context was delivered without a live worker: %+v", env.sink.delivered)
	}

	// A trusted follow-up starts the replacement.
	trusted := untrusted
	trusted.EventID = "c-trusted"
	trusted.Actor = alice()
	trusted.Body = "please continue"
	if _, err := env.orchestrator.HandleSourceEvent(ctx, fixtureEvent(t, trusted)); err != nil {
		t.Fatalf("trusted follow-up: %v", err)
	}
	if env.launcher.count() != launches+1 {
		t.Fatalf("launches = %d, want %d after a trusted follow-up", env.launcher.count(), launches+1)
	}
	// The surviving untrusted context is delivered to the new live worker.
	*env.clock = env.clock.Add(31 * time.Second)
	if err := env.orchestrator.Housekeeping(ctx); err != nil {
		t.Fatalf("housekeeping after dispatch: %v", err)
	}
	if env.sink.count() == 0 || !strings.Contains(env.sink.last().Text, "untrusted context") {
		t.Fatalf("surviving context delivery = %+v", env.sink.delivered)
	}
}

// TestUntrustedCommentDoesNotUnblockABlockedTicket is the P1-3 regression: with
// a live assignment and a blocked ticket, an untrusted comment is durable
// context but never turns the ticket active while the Attention is open.
func TestUntrustedCommentDoesNotUnblockABlockedTicket(t *testing.T) {
	env := newEnv(t, envOpts{})
	ctx := context.Background()
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	ticket := ticketByThread(t, env, "issue#1")
	attention := askAndBlock(t, env, ticket, collab.AttentionRequest{
		Prompt: "Which branch?", Options: []collab.AttentionOption{{ID: "main"}, {ID: "next"}},
	})
	untrusted := superagent.Event{
		SourceID: "fixture", EventID: "c-untrusted", Kind: superagent.EventIssueComment,
		Actor:  superagent.Actor{ID: "u2", Login: "mallory"},
		Thread: superagent.ThreadRef{Source: "fixture", Kind: "issue", ID: "issue#1"},
		Body:   "untrusted context",
	}
	if _, err := env.orchestrator.HandleSourceEvent(ctx, fixtureEvent(t, untrusted)); err != nil {
		t.Fatalf("untrusted comment: %v", err)
	}
	for i := 0; i < 2; i++ {
		*env.clock = env.clock.Add(31 * time.Second)
		if err := env.orchestrator.Housekeeping(ctx); err != nil {
			t.Fatalf("housekeeping: %v", err)
		}
		blocked := ticketByThread(t, env, "issue#1")
		if blocked.Status != collab.WorkItemBlocked {
			t.Fatalf("status = %s, want blocked (untrusted context must not unblock)", blocked.Status)
		}
		if len(blocked.Pending) != 1 || blocked.Pending[0].Failed {
			t.Fatalf("pending = %+v, want one deferred entry", blocked.Pending)
		}
		if env.sink.count() != 0 {
			t.Fatalf("untrusted context was delivered to a blocked ticket: %+v", env.sink.delivered)
		}
	}
	stored, err := env.store.GetAttention(ctx, env.tenant, attention.ID)
	if err != nil || stored.Status != collab.AttentionOpen {
		t.Fatalf("attention = %+v err=%v, want still open", stored, err)
	}
}

// TestRepeatedIssueEditBeforeDeliveryIsCapturedOnce is the P2-1 dedupe rule:
// GitHub bumps an issue's updated_at per comment; before the first edit is
// delivered, the pending snapshot is the comparison point, so the same content
// is captured once. A genuinely different edit is still captured.
func TestRepeatedIssueEditBeforeDeliveryIsCapturedOnce(t *testing.T) {
	env := newEnv(t, envOpts{})
	ctx := context.Background()
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	ticket := ticketByThread(t, env, "issue#1")
	askAndBlock(t, env, ticket, collab.AttentionRequest{Prompt: "Approve?", AllowFreeText: true})
	assignment := *ticket.ActiveAssignment
	assignment.Status = collab.AssignmentFailed
	if _, err := env.store.UpdateAssignment(ctx, assignment); err != nil {
		t.Fatalf("fail assignment: %v", err)
	}
	env.launcher.setLaunchErr(errors.New("launcher down"))
	edit := superagent.Event{
		SourceID: "fixture", EventID: "edit-1", Kind: superagent.EventIssueUpdated,
		Actor: alice(), Thread: superagent.ThreadRef{Source: "fixture", Kind: "issue", ID: "issue#1"},
		Title: "Fix issue#1", Body: "body B",
	}
	if _, err := env.orchestrator.HandleSourceEvent(ctx, fixtureEvent(t, edit)); err != nil {
		t.Fatalf("first edit: %v", err)
	}
	bump := edit
	bump.EventID = "bump-2"
	bump.Kind = superagent.EventIssueUpdated
	if _, err := env.orchestrator.HandleSourceEvent(ctx, fixtureEvent(t, bump)); err != nil {
		t.Fatalf("comment bump: %v", err)
	}
	if pending := ticketByThread(t, env, "issue#1").Pending; len(pending) != 1 {
		t.Fatalf("pending after a same-content bump = %+v, want one", pending)
	}
	third := edit
	third.EventID = "edit-3"
	third.Body = "body C"
	if _, err := env.orchestrator.HandleSourceEvent(ctx, fixtureEvent(t, third)); err != nil {
		t.Fatalf("second edit: %v", err)
	}
	if pending := ticketByThread(t, env, "issue#1").Pending; len(pending) != 2 {
		t.Fatalf("pending after a real edit = %+v, want two", pending)
	}
}

// TestFailedAnswerIsVisibleAndRetryable: a permanent delivery refusal (here a
// permission error) marks the answer entry Failed, and a later human retry
// delivers it once the seam recovers.
func TestFailedAnswerIsVisibleAndRetryable(t *testing.T) {
	env := newEnv(t, envOpts{})
	ctx := context.Background()
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	ticket := ticketByThread(t, env, "issue#1")
	attention := askAndBlock(t, env, ticket, collab.AttentionRequest{
		Prompt: "Approve?", Options: []collab.AttentionOption{{ID: "approve"}},
	})
	env.sink.failNext = 1
	env.sink.failWith = collab.ErrPermissionDenied
	if _, err := env.orchestrator.AnswerAttention(ctx, env.tenant, attention.ID, "approve", "", "alice"); !errors.Is(err, superagent.ErrAnswerDeliveryFailed) {
		t.Fatalf("answer = %v, want ErrAnswerDeliveryFailed", err)
	}
	blocked := ticketByThread(t, env, "issue#1")
	if len(blocked.Pending) != 1 || !blocked.Pending[0].Failed || blocked.FailedPendingCount != 1 {
		t.Fatalf("visible failed pending = %+v (count %d)", blocked.Pending, blocked.FailedPendingCount)
	}

	if _, err := env.orchestrator.AnswerAttention(ctx, env.tenant, attention.ID, "approve", "", "alice"); err != nil {
		t.Fatalf("human retry: %v", err)
	}
	if env.sink.count() != 1 {
		t.Fatalf("retry delivery = %+v", env.sink.delivered)
	}
	recovered := ticketByThread(t, env, "issue#1")
	if len(recovered.Pending) != 0 || recovered.AnswerReceiptFor(attention.ID) == nil {
		t.Fatalf("after retry: pending=%+v receipt=%+v", recovered.Pending, recovered.AnswerReceiptFor(attention.ID))
	}
}

// TestFailedPendingIsVisibleAndCanBeDismissed: the Board exposes a permanently
// failed delivery and the operator can dismiss it (clearing the row). Retrying
// without fixing the seam keeps it failed instead of pretending success.
func TestFailedPendingIsVisibleAndCanBeDismissed(t *testing.T) {
	env := newEnv(t, envOpts{inputs: superagent.NoopInputSink{}})
	ctx := context.Background()
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	edited := superagent.Event{
		SourceID: "fixture", EventID: "edit-1", Kind: superagent.EventIssueUpdated,
		Actor: alice(), Thread: superagent.ThreadRef{Source: "fixture", Kind: "issue", ID: "issue#1"},
		Title: "Fix issue#1", Body: "body B",
	}
	if _, err := env.orchestrator.HandleSourceEvent(ctx, fixtureEvent(t, edited)); err != nil {
		t.Fatalf("capture edit: %v", err)
	}
	failed := ticketByThread(t, env, "issue#1")
	if len(failed.Pending) != 1 || !failed.Pending[0].Failed || failed.FailedPendingCount != 1 {
		t.Fatalf("failed pending = %+v (count %d)", failed.Pending, failed.FailedPendingCount)
	}
	entryID := failed.Pending[0].ID
	if _, err := env.orchestrator.RetryPending(ctx, env.tenant, failed.ID, entryID, "alice"); err != nil {
		t.Fatalf("retry pending: %v", err)
	}
	if still := ticketByThread(t, env, "issue#1"); len(still.Pending) != 1 || !still.Pending[0].Failed {
		t.Fatalf("retry without a fixed seam must stay failed: %+v", still.Pending)
	}
	if _, err := env.orchestrator.DismissPending(ctx, env.tenant, failed.ID, entryID, "alice"); err != nil {
		t.Fatalf("dismiss pending: %v", err)
	}
	dismissed := ticketByThread(t, env, "issue#1")
	if len(dismissed.Pending) != 0 || dismissed.FailedPendingCount != 0 {
		t.Fatalf("after dismiss: pending=%+v failed=%d", dismissed.Pending, dismissed.FailedPendingCount)
	}

	noOperator := newEnv(t, envOpts{noOperator: true})
	if _, err := noOperator.orchestrator.DismissPending(ctx, noOperator.tenant, failed.ID, entryID, "alice"); !errors.Is(err, superagent.ErrOperatorUnavailable) {
		t.Fatalf("dismiss without the operator seam = %v", err)
	}
}

type outboxFaultStore struct {
	*superagent.MemoryStore
	mu      sync.Mutex
	failAns bool
}

func (s *outboxFaultStore) EnqueueOutbox(ctx context.Context, message superagent.OutboxMessage) (superagent.OutboxMessage, error) {
	s.mu.Lock()
	fail := s.failAns && strings.Contains(message.IdempotencyKey, ":answer")
	s.mu.Unlock()
	if fail {
		return superagent.OutboxMessage{}, errors.New("injected outbox failure")
	}
	return s.MemoryStore.EnqueueOutbox(ctx, message)
}

func (s *outboxFaultStore) setFailAnswer(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failAns = fail
}

// TestAnswerCommentIsRepairedByTheStartupScan is the P2-3 compensation: the
// worker received the answer but its Channel comment never reached the outbox
// (the enqueue failed). The receipt says so, and the startup scan writes the
// missing idempotent outbox row.
func TestAnswerCommentIsRepairedByTheStartupScan(t *testing.T) {
	fault := &outboxFaultStore{}
	env := newEnv(t, envOpts{store: func(inner *superagent.MemoryStore) superagent.Store {
		fault.MemoryStore = inner
		return fault
	}})
	ctx := context.Background()
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	ticket := ticketByThread(t, env, "issue#1")
	attention := askAndBlock(t, env, ticket, collab.AttentionRequest{
		Prompt: "Approve?", Options: []collab.AttentionOption{{ID: "approve"}},
	})
	fault.setFailAnswer(true)
	if _, err := env.orchestrator.AnswerAttention(ctx, env.tenant, attention.ID, "approve", "", "alice"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	delivered := ticketByThread(t, env, "issue#1")
	receipt := delivered.AnswerReceiptFor(attention.ID)
	if receipt == nil || receipt.CommentQueued {
		t.Fatalf("receipt = %+v, want delivered with the comment still owed", receipt)
	}
	key := "attention:" + attention.ID.String() + ":answer"
	messages, err := env.store.ListOutbox(ctx, env.tenant, "", 100)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	for _, message := range messages {
		if message.IdempotencyKey == key {
			t.Fatalf("the answer comment must not exist before the repair: %+v", message)
		}
	}

	fault.setFailAnswer(false)
	if _, err := env.orchestrator.Recover(ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}
	repaired := ticketByThread(t, env, "issue#1")
	if receipt := repaired.AnswerReceiptFor(attention.ID); receipt == nil || !receipt.CommentQueued {
		t.Fatalf("receipt after repair = %+v, want CommentQueued", receipt)
	}
	messages, _ = env.store.ListOutbox(ctx, env.tenant, "", 100)
	found := 0
	for _, message := range messages {
		if message.IdempotencyKey == key {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("answer outbox rows = %d, want exactly one", found)
	}
}

// TestWriterLeaseIsExclusive pins the single-writer guard: one lease per tenant,
// released explicitly (a crash releases it with the connection in PostgreSQL).
func TestWriterLeaseIsExclusive(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()
	store := superagent.NewMemoryStore(nil)
	lease, err := store.AcquireWriterLease(ctx, tenant)
	if err != nil {
		t.Fatalf("first lease: %v", err)
	}
	if _, err := store.AcquireWriterLease(ctx, tenant); !errors.Is(err, superagent.ErrWriterLeaseHeld) {
		t.Fatalf("second lease = %v, want ErrWriterLeaseHeld", err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	second, err := store.AcquireWriterLease(ctx, tenant)
	if err != nil {
		t.Fatalf("lease after release: %v", err)
	}
	_ = second.Release(ctx)
}
