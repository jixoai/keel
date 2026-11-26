package superagent_test

// Stop/terminal boundary and lease/scan regressions from the fifth review.
//
// The queue must never deliver input after a human stop, while a cancel is
// still propagating, or after the ticket is terminal. Retry is a delivery
// action and is refused (409); dismiss stays available to settle the row. The
// startup compensation scan must re-arm after a failed pass, and Run must stop
// writing when its single-writer lease loses its connection.

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

func setTicketStatusForTest(t *testing.T, env *testEnv, ticketID uuid.UUID, status collab.WorkItemStatus) {
	t.Helper()
	item, err := env.store.GetWorkItem(context.Background(), env.tenant, ticketID)
	if err != nil {
		t.Fatalf("get work item: %v", err)
	}
	item.Status = status
	if _, err := env.store.UpdateWorkItem(context.Background(), item); err != nil {
		t.Fatalf("set status %s: %v", status, err)
	}
}

func revokeMandateForTest(t *testing.T, env *testEnv, ticketID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	item, err := env.store.GetWorkItem(ctx, env.tenant, ticketID)
	if err != nil {
		t.Fatalf("get work item: %v", err)
	}
	doc := superagent.TicketFromWorkItem(item)
	now := *env.clock
	doc.Mandate.RevokedAt = &now
	doc.Mandate.RevokedBy = "alice"
	doc.Mandate.RevocationCause = "human stop"
	if _, err := env.store.UpdateWorkItem(ctx, doc.Record()); err != nil {
		t.Fatalf("revoke mandate: %v", err)
	}
}

func appendPendingForTest(t *testing.T, env *testEnv, ticketID uuid.UUID, entry superagent.PendingFollowUp) {
	t.Helper()
	ctx := context.Background()
	item, err := env.store.GetWorkItem(ctx, env.tenant, ticketID)
	if err != nil {
		t.Fatalf("get work item: %v", err)
	}
	doc := superagent.TicketFromWorkItem(item)
	doc.Pending = append(doc.Pending, entry)
	if _, err := env.store.UpdateWorkItem(ctx, doc.Record()); err != nil {
		t.Fatalf("append pending: %v", err)
	}
}

// TestClosedTicketNeverDeliversPendingInput is the P0 regression: a revoked
// mandate, a cancelling ticket (a failed Host.Cancel leaves the live assignment
// behind) and terminal tickets must refuse delivery at every entry point.
// Retry returns ErrWorkItemClosed (HTTP 409); the pending row stays untouched
// (no attempt, no Failed mark) and only dismiss can settle it.
func TestClosedTicketNeverDeliversPendingInput(t *testing.T) {
	cases := []struct {
		name   string
		status collab.WorkItemStatus
		revoke bool
	}{
		{name: "cancelling_with_live_worker", status: collab.WorkItemCancelling, revoke: true},
		{name: "cancelled", status: collab.WorkItemCancelled, revoke: true},
		{name: "done", status: collab.WorkItemDone},
		{name: "failed", status: collab.WorkItemFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t, envOpts{})
			ctx := context.Background()
			acceptEvent(t, env, issueEvent("e1", "issue#1"))
			ticket := ticketByThread(t, env, "issue#1")
			attention := askAndBlock(t, env, ticket, collab.AttentionRequest{
				Prompt: "Approve?", Options: []collab.AttentionOption{{ID: "approve"}},
			})
			answerEntry := superagent.PendingFollowUp{
				ID: "attention:" + attention.ID.String(), Kind: superagent.PendingAnswer,
				Text: "Human selected: approve", AttentionID: attention.ID,
				OptionID: "approve", AnswerActor: "alice", Trusted: true, AllowDispatch: true,
				CreatedAt: *env.clock,
			}
			followUp := superagent.PendingFollowUp{
				ID: "fixture:stopped-1", Kind: superagent.PendingFollowUpInput,
				Text: "thread context", Trusted: true, AllowDispatch: true, CreatedAt: *env.clock,
			}
			appendPendingForTest(t, env, ticket.ID, answerEntry)
			appendPendingForTest(t, env, ticket.ID, followUp)
			if tc.revoke {
				revokeMandateForTest(t, env, ticket.ID)
			}
			setTicketStatusForTest(t, env, ticket.ID, tc.status)

			launches := env.launcher.count()
			// Housekeeping must skip the ticket entirely.
			if err := env.orchestrator.Housekeeping(ctx); err != nil {
				t.Fatalf("housekeeping: %v", err)
			}
			// An answer retry is refused before touching anything.
			if _, err := env.orchestrator.AnswerAttention(ctx, env.tenant, attention.ID, "approve", "", "alice"); !errors.Is(err, collab.ErrWorkItemClosed) {
				t.Fatalf("answer retry on %s = %v, want ErrWorkItemClosed", tc.name, err)
			}
			// Both retry shapes are refused with the closed sentinel.
			for _, id := range []string{answerEntry.ID, followUp.ID} {
				if _, err := env.orchestrator.RetryPending(ctx, env.tenant, ticket.ID, id, "alice"); !errors.Is(err, collab.ErrWorkItemClosed) {
					t.Fatalf("retry %s on %s = %v, want ErrWorkItemClosed", id, tc.name, err)
				}
			}
			if env.sink.count() != 0 {
				t.Fatalf("%s delivered input: %+v", tc.name, env.sink.delivered)
			}
			if env.launcher.count() != launches {
				t.Fatalf("%s dispatched a worker: %d -> %d", tc.name, launches, env.launcher.count())
			}
			closed := ticketByThread(t, env, "issue#1")
			if len(closed.Pending) != 2 {
				t.Fatalf("%s pending = %+v, want both rows untouched", tc.name, closed.Pending)
			}
			for _, entry := range closed.Pending {
				if entry.Failed || entry.Attempts != 0 {
					t.Fatalf("%s mutated a closed pending row: %+v", tc.name, entry)
				}
			}
			// Dismiss is the settlement action and stays available.
			if _, err := env.orchestrator.DismissPending(ctx, env.tenant, ticket.ID, followUp.ID, "alice"); err != nil {
				t.Fatalf("%s dismiss: %v", tc.name, err)
			}
			if after := ticketByThread(t, env, "issue#1"); len(after.Pending) != 1 {
				t.Fatalf("%s pending after dismiss = %+v", tc.name, after.Pending)
			}
			if env.sink.count() != 0 {
				t.Fatalf("%s delivered during dismiss: %+v", tc.name, env.sink.delivered)
			}
		})
	}
}

// flakyAnswerHost fails the first AnswerAttention calls with injected errors and
// then delegates to the real Host.
type flakyAnswerHost struct {
	superagent.Host
	mu   sync.Mutex
	errs []error
}

func (h *flakyAnswerHost) AnswerAttention(ctx context.Context, tenantID, workItemID, attentionID uuid.UUID, req collab.AttentionAnswerRequest) (collab.Attention, error) {
	h.mu.Lock()
	if len(h.errs) > 0 {
		err := h.errs[0]
		h.errs = h.errs[1:]
		h.mu.Unlock()
		return collab.Attention{}, err
	}
	h.mu.Unlock()
	return h.Host.AnswerAttention(ctx, tenantID, workItemID, attentionID, req)
}

// TestTransientAnswerWriteIsRecordedPending is the P1-2 regression: a durable
// intent whose Attention write fails transiently is reported as a recorded,
// completion-pending answer (202 semantics), the board is invalidated, and the
// pending pump completes the decision on a later tick. It is never a 4xx that
// drops the answer.
func TestTransientAnswerWriteIsRecordedPending(t *testing.T) {
	flaky := &flakyAnswerHost{errs: []error{errors.New("audit store is down")}}
	env := newEnv(t, envOpts{host: func(host superagent.Host) superagent.Host {
		flaky.Host = host
		return flaky
	}})
	ctx := context.Background()
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	ticket := ticketByThread(t, env, "issue#1")
	attention := askAndBlock(t, env, ticket, collab.AttentionRequest{
		Prompt: "Approve?", Options: []collab.AttentionOption{{ID: "approve"}},
	})
	before := env.changes.count()
	if _, err := env.orchestrator.AnswerAttention(ctx, env.tenant, attention.ID, "approve", "", "alice"); !errors.Is(err, superagent.ErrAnswerDeliveryPending) {
		t.Fatalf("transient answer write = %v, want ErrAnswerDeliveryPending", err)
	}
	if env.changes.count() == before {
		t.Fatal("a recorded-pending answer must invalidate the board (SSE)")
	}
	stored, err := env.store.GetAttention(ctx, env.tenant, attention.ID)
	if err != nil || stored.Status != collab.AttentionOpen {
		t.Fatalf("attention = %+v err=%v, want still open", stored, err)
	}
	pending := ticketByThread(t, env, "issue#1").Pending
	if len(pending) != 1 || pending[0].Kind != superagent.PendingAnswer || pending[0].Failed {
		t.Fatalf("intent = %+v, want one retryable answer intent", pending)
	}

	*env.clock = env.clock.Add(2 * time.Second)
	if err := env.orchestrator.Housekeeping(ctx); err != nil {
		t.Fatalf("housekeeping: %v", err)
	}
	stored, _ = env.store.GetAttention(ctx, env.tenant, attention.ID)
	if stored.Status != collab.AttentionAnswered {
		t.Fatalf("attention after retry = %s, want answered", stored.Status)
	}
	if env.sink.count() != 1 {
		t.Fatalf("deliveries = %+v, want one", env.sink.delivered)
	}
	after := ticketByThread(t, env, "issue#1")
	if after.AnswerReceiptFor(attention.ID) == nil || len(after.Pending) != 0 {
		t.Fatalf("after completion: receipt=%+v pending=%+v", after.AnswerReceiptFor(attention.ID), after.Pending)
	}
}

// TestAnswerRepairScanFailureIsRetriedByHousekeeping is the P1-1 regression: a
// compensation scan that fails once must stay armed; the next housekeeping tick
// repairs the answered Attention instead of leaving it orphaned forever.
func TestAnswerRepairScanFailureIsRetriedByHousekeeping(t *testing.T) {
	fault := &faultStore{failAttentionList: 1}
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
	// Simulate the crash: answered directly, no intent/receipt.
	if _, err := env.host.AnswerAttention(ctx, env.tenant, ticket.ID, attention.ID, collab.AttentionAnswerRequest{
		OptionID: "approve", Actor: "alice",
	}); err != nil {
		t.Fatalf("direct answer: %v", err)
	}
	if _, err := env.orchestrator.Recover(ctx); err != nil {
		t.Fatalf("recover must not fail on one compensation error: %v", err)
	}
	if env.sink.count() != 0 {
		t.Fatalf("the failed scan delivered: %+v", env.sink.delivered)
	}
	// The failure scheduled a 1s retry; the next due tick repairs it.
	*env.clock = env.clock.Add(2 * time.Second)
	if err := env.orchestrator.Housekeeping(ctx); err != nil {
		t.Fatalf("housekeeping: %v", err)
	}
	if env.sink.count() != 1 {
		t.Fatalf("repair deliveries = %+v, want one", env.sink.delivered)
	}
	after := ticketByThread(t, env, "issue#1")
	if after.AnswerReceiptFor(attention.ID) == nil || len(after.Pending) != 0 {
		t.Fatalf("after repair: receipt=%+v pending=%+v", after.AnswerReceiptFor(attention.ID), after.Pending)
	}
}

// TestUntrustedContextWithoutALiveWorkerIsKept: untrusted thread context is
// durable and waits; it is never dropped, never delivered to a dead worker and
// never dispatched.
func TestUntrustedContextWithoutALiveWorkerIsKept(t *testing.T) {
	env := newEnv(t, envOpts{})
	ctx := context.Background()
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	ticket := ticketByThread(t, env, "issue#1")
	assignment := *ticket.ActiveAssignment
	assignment.Status = collab.AssignmentFailed
	if _, err := env.store.UpdateAssignment(ctx, assignment); err != nil {
		t.Fatalf("fail assignment: %v", err)
	}
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
	if len(pending) != 1 || pending[0].AllowDispatch || pending[0].Failed {
		t.Fatalf("pending = %+v, want one waiting non-dispatch entry", pending)
	}
	if env.sink.count() != 0 || env.launcher.count() != 1 {
		t.Fatalf("sink=%d launches=%d, want no delivery/dispatch", env.sink.count(), env.launcher.count())
	}
	*env.clock = env.clock.Add(31 * time.Second)
	if err := env.orchestrator.Housekeeping(ctx); err != nil {
		t.Fatalf("housekeeping: %v", err)
	}
	if env.sink.count() != 0 || env.launcher.count() != 1 {
		t.Fatalf("housekeeping delivered/dispatched untrusted context")
	}
	if kept := ticketByThread(t, env, "issue#1"); len(kept.Pending) != 1 {
		t.Fatalf("untrusted context was dropped: %+v", kept.Pending)
	}
}

// TestObserveMandateCannotBeResumedForDispatch: observe is tracking authority,
// never work. Neither accept nor a human resume may start a Worker.
func TestObserveMandateCannotBeResumedForDispatch(t *testing.T) {
	env := newEnv(t, envOpts{policy: fixedPolicy{verdict: superagent.Verdict{
		Action: superagent.ActionAccept, Strength: superagent.MandateObserve, BotID: "worker",
	}}})
	ctx := context.Background()
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	ticket := ticketByThread(t, env, "issue#1")
	if ticket.Status != collab.WorkItemBlocked {
		t.Fatalf("observe ticket status = %s, want blocked", ticket.Status)
	}
	if env.launcher.count() != 0 {
		t.Fatalf("observe accept dispatched a worker: %d", env.launcher.count())
	}
	if _, err := env.orchestrator.ResumeTicket(ctx, env.tenant, ticket.ID, "alice", "please work"); !errors.Is(err, collab.ErrInvalidRequest) {
		t.Fatalf("observe resume = %v, want refused", err)
	}
	if env.launcher.count() != 0 {
		t.Fatalf("observe resume dispatched a worker: %d", env.launcher.count())
	}
	if dispatched, err := env.orchestrator.DispatchQueued(ctx); err != nil || dispatched != 0 {
		t.Fatalf("dispatch queued = %d, %v", dispatched, err)
	}
}

type fakeWriterLease struct {
	mu         sync.Mutex
	healthyErr error
	releases   int
}

func (l *fakeWriterLease) Healthy(context.Context) error { return l.healthyErr }

func (l *fakeWriterLease) Release(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.releases++
	return nil
}

// TestRunStopsWhenTheWriterLeaseIsUnhealthy is the P1-3 boundary at the loop:
// a lease that cannot prove it still owns the tenant stops Run before any
// recovery or poll write.
func TestRunStopsWhenTheWriterLeaseIsUnhealthy(t *testing.T) {
	lease := &fakeWriterLease{healthyErr: errors.New("lease connection lost")}
	env := newEnv(t, envOpts{lease: lease})
	done := make(chan struct{})
	go func() {
		env.orchestrator.Run(context.Background(), time.Hour)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run kept running with an unhealthy writer lease")
	}
	lease.mu.Lock()
	releases := lease.releases
	lease.mu.Unlock()
	if releases != 1 {
		t.Fatalf("lease releases = %d, want 1", releases)
	}
	if env.changes.count() != 0 {
		t.Fatalf("an unhealthy lease wrote during recovery: %d changes", env.changes.count())
	}
}

// TestPendingAuditRecordsTheOutcome: a refused retry writes failed; a dismiss
// (the terminal settlement) writes accepted.
func TestPendingAuditRecordsTheOutcome(t *testing.T) {
	env := newEnv(t, envOpts{})
	ctx := context.Background()
	acceptEvent(t, env, issueEvent("e1", "issue#1"))
	ticket := ticketByThread(t, env, "issue#1")
	followUp := superagent.PendingFollowUp{
		ID: "fixture:settle-1", Kind: superagent.PendingFollowUpInput,
		Text: "context", Trusted: true, AllowDispatch: true, CreatedAt: *env.clock,
	}
	appendPendingForTest(t, env, ticket.ID, followUp)
	setTicketStatusForTest(t, env, ticket.ID, collab.WorkItemDone)

	if _, err := env.orchestrator.RetryPending(ctx, env.tenant, ticket.ID, followUp.ID, "alice"); !errors.Is(err, collab.ErrWorkItemClosed) {
		t.Fatalf("retry = %v, want closed", err)
	}
	if got := lastAuditResult(t, env, superagent.AuditPendingRetried); got != "failed" {
		t.Fatalf("retry audit result = %q, want failed", got)
	}
	if _, err := env.orchestrator.DismissPending(ctx, env.tenant, ticket.ID, followUp.ID, "alice"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	if got := lastAuditResult(t, env, superagent.AuditPendingDismissed); got != "accepted" {
		t.Fatalf("dismiss audit result = %q, want accepted", got)
	}
	if !strings.Contains(lastAuditDetail(t, env, superagent.AuditPendingDismissed), followUp.ID) {
		t.Fatalf("dismiss audit detail does not name the row: %q", lastAuditDetail(t, env, superagent.AuditPendingDismissed))
	}
}

func lastAuditResult(t *testing.T, env *testEnv, kind string) string {
	t.Helper()
	env.audit.mu.Lock()
	defer env.audit.mu.Unlock()
	for index := len(env.audit.events) - 1; index >= 0; index-- {
		if env.audit.events[index].Kind == kind {
			return env.audit.events[index].Result
		}
	}
	t.Fatalf("no audit row %q", kind)
	return ""
}

func lastAuditDetail(t *testing.T, env *testEnv, kind string) string {
	t.Helper()
	env.audit.mu.Lock()
	defer env.audit.mu.Unlock()
	for index := len(env.audit.events) - 1; index >= 0; index-- {
		if env.audit.events[index].Kind == kind {
			return env.audit.events[index].Detail
		}
	}
	t.Fatalf("no audit row %q", kind)
	return ""
}
