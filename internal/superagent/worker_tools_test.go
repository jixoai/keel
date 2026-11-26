package superagent_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/superagent"
	"github.com/jixoai/keel/internal/toolsurface"
)

// The domain refusal codes are rendered by the tool surface, and the tool
// surface does not import this package. Pin the spelling here so a rename
// cannot silently turn a typed refusal into a generic backend error.
func TestWorkerToolCodesMatchToolSurface(t *testing.T) {
	pairs := map[superagent.WorkerToolCode]string{
		superagent.WorkerToolUnavailable:      toolsurface.CodeSuperagentUnavailable,
		superagent.WorkerToolScopeRequired:    toolsurface.CodeSuperagentScopeRequired,
		superagent.WorkerToolNotWorker:        toolsurface.CodeSuperagentNotWorker,
		superagent.WorkerToolTicketBlocked:    toolsurface.CodeSuperagentTicketBlocked,
		superagent.WorkerToolTicketClosed:     toolsurface.CodeSuperagentTicketClosed,
		superagent.WorkerToolInvalidRequest:   toolsurface.CodeSuperagentInvalid,
		superagent.WorkerToolAttentionInvalid: toolsurface.CodeSuperagentAttention,
	}
	for code, surface := range pairs {
		if string(code) != surface {
			t.Fatalf("worker tool code %q != tool-surface code %q", code, surface)
		}
	}
}

// activeTicket polls one event and returns the accepted ticket with its live
// assignment. The worker session index row is the fact resolveWorker reads.
func activeTicket(t *testing.T, env *testEnv, eventID, threadID string) superagent.TicketView {
	t.Helper()
	if _, err := env.orchestrator.HandleSourceEvent(context.Background(), fixtureEvent(t, issueEvent(eventID, threadID))); err != nil {
		t.Fatalf("accept %s: %v", eventID, err)
	}
	board, err := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	for _, ticket := range board.Tickets {
		if ticket.Thread.ID == threadID {
			if ticket.ActiveAssignment == nil || ticket.ActiveAssignment.SessionID == uuid.Nil {
				t.Fatalf("ticket %s has no active assignment: %+v", threadID, ticket.ActiveAssignment)
			}
			return ticket
		}
	}
	t.Fatalf("ticket for thread %s not found", threadID)
	return superagent.TicketView{}
}

func boardTicket(t *testing.T, env *testEnv, ticketID uuid.UUID) superagent.TicketView {
	t.Helper()
	board, err := env.orchestrator.Board(context.Background(), env.tenant, superagent.BoardFilter{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	for _, ticket := range board.Tickets {
		if ticket.ID == ticketID {
			return ticket
		}
	}
	t.Fatalf("ticket %s not found", ticketID)
	return superagent.TicketView{}
}

func lastAudit(t *testing.T, env *testEnv, kind string) collab.AuditEvent {
	t.Helper()
	env.audit.mu.Lock()
	defer env.audit.mu.Unlock()
	for index := len(env.audit.events) - 1; index >= 0; index-- {
		if env.audit.events[index].Kind == kind {
			return env.audit.events[index]
		}
	}
	t.Fatalf("no audit event %q", kind)
	return collab.AuditEvent{}
}

func workerToolCode(t *testing.T, err error) string {
	t.Helper()
	var refusal *superagent.WorkerToolError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want *superagent.WorkerToolError", err)
	}
	return refusal.ToolCode()
}

func TestWorkerReportResolvesSessionTicketAndIsIdempotent(t *testing.T) {
	env := newEnv(t, envOpts{})
	ticket := activeTicket(t, env, "e1", "issue#1")
	sessionID := ticket.ActiveAssignment.SessionID

	outcome, err := env.orchestrator.ReportFromWorker(context.Background(), env.tenant, sessionID, superagent.WorkerReportInput{
		State: superagent.ReportProgress, Summary: "reading the repository", RequestID: "report:1",
	})
	if err != nil {
		t.Fatalf("ReportFromWorker: %v", err)
	}
	if outcome.Ticket.ID != ticket.ID || outcome.Ticket.Status != collab.WorkItemActive {
		t.Fatalf("outcome ticket = %+v, want the worker's active ticket", outcome.Ticket)
	}
	if outcome.Assignment.ID != ticket.ActiveAssignment.ID || outcome.Assignment.Status != collab.AssignmentActive {
		t.Fatalf("outcome assignment = %+v", outcome.Assignment)
	}
	audit := lastAudit(t, env, superagent.AuditReport)
	if audit.TenantID != env.tenant || audit.WorkItemID != ticket.ID || audit.AssignmentID != ticket.ActiveAssignment.ID {
		t.Fatalf("report audit scope = %+v", audit)
	}
	if audit.SessionID != sessionID || audit.BotID != "worker" {
		t.Fatalf("report audit actor = %+v, want the worker session/bot", audit)
	}
	if strings.Contains(audit.Detail, "reading the repository") || strings.Contains(audit.Reason, "reading the repository") {
		t.Fatalf("report audit leaked the report body: %+v", audit)
	}
	if audit.Detail != "state=progress" {
		t.Fatalf("report audit detail = %q, want state=progress", audit.Detail)
	}

	// A retried call with the same request id is a no-op: no second audit, no
	// state change, and the durable report row is the original one.
	reportsBefore := env.audit.count(superagent.AuditReport)
	again, err := env.orchestrator.ReportFromWorker(context.Background(), env.tenant, sessionID, superagent.WorkerReportInput{
		State: superagent.ReportProgress, Summary: "different text", RequestID: "report:1",
	})
	if err != nil {
		t.Fatalf("idempotent ReportFromWorker: %v", err)
	}
	if again.Ticket.Status != collab.WorkItemActive || env.audit.count(superagent.AuditReport) != reportsBefore {
		t.Fatalf("retried report changed state or audited twice: %+v", again.Ticket)
	}
	if again.Created || again.Report.State != superagent.ReportProgress || again.Report.Summary != "reading the repository" {
		t.Fatalf("retried report fact = %+v, want the stored first report", again)
	}
	recorded, err := env.store.GetWorkerReport(context.Background(), env.tenant, ticket.ID, ticket.ActiveAssignment.ID, sessionID, "report:1")
	if err != nil {
		t.Fatalf("GetWorkerReport: %v", err)
	}
	if recorded.State != superagent.ReportProgress || recorded.SessionID != sessionID || recorded.AppliedAt == nil || recorded.AuditedAt == nil {
		t.Fatalf("recorded report = %+v", recorded)
	}

	// Ready finishes the assignment and the ticket.
	done, err := env.orchestrator.ReportFromWorker(context.Background(), env.tenant, sessionID, superagent.WorkerReportInput{
		State: superagent.ReportReady, Summary: "shipped", RequestID: "report:done",
	})
	if err != nil {
		t.Fatalf("Report ready: %v", err)
	}
	if done.Ticket.Status != collab.WorkItemDone || done.Assignment.Status != collab.AssignmentDone {
		t.Fatalf("ready outcome = %+v / %+v", done.Ticket, done.Assignment)
	}
	if _, err := env.orchestrator.ReportFromWorker(context.Background(), env.tenant, sessionID, superagent.WorkerReportInput{
		State: superagent.ReportProgress, RequestID: "report:after",
	}); workerToolCode(t, err) != string(superagent.WorkerToolTicketClosed) {
		t.Fatalf("report after ready err = %v, want ticket_closed", err)
	}
}

func TestWorkerAskIsScopedAndReusesAttention(t *testing.T) {
	env := newEnv(t, envOpts{})
	ticket := activeTicket(t, env, "e1", "issue#1")
	sessionID := ticket.ActiveAssignment.SessionID

	attention, askTicket, err := env.orchestrator.AskFromWorker(context.Background(), env.tenant, sessionID, collab.AttentionRequest{
		RequestID: "q1",
		Prompt:    "Which branch should I target?",
		Options:   []collab.AttentionOption{{ID: "main", Label: "main"}, {ID: "next", Label: "next"}},
	})
	if err != nil {
		t.Fatalf("AskFromWorker: %v", err)
	}
	if askTicket.ID != ticket.ID || attention.WorkItemID != ticket.ID {
		t.Fatalf("attention ticket = %s / %s, want %s", attention.WorkItemID, askTicket.ID, ticket.ID)
	}
	if attention.SessionID != sessionID || attention.AssignmentID != ticket.ActiveAssignment.ID {
		t.Fatalf("attention actor = %+v", attention)
	}
	if attention.Route != collab.AttentionRouteChannel || attention.Channel != "issue#1" {
		t.Fatalf("attention route = %+v", attention)
	}
	audit := lastAudit(t, env, superagent.AuditAttentionOpen)
	if audit.SessionID != sessionID || audit.AssignmentID != ticket.ActiveAssignment.ID || audit.WorkItemID != ticket.ID {
		t.Fatalf("attention audit = %+v", audit)
	}
	if strings.Contains(audit.Detail, "Which branch") || strings.Contains(audit.Reason, "Which branch") {
		t.Fatalf("attention audit leaked the prompt: %+v", audit)
	}

	again, _, err := env.orchestrator.AskFromWorker(context.Background(), env.tenant, sessionID, collab.AttentionRequest{
		RequestID: "q1",
		Prompt:    "Which branch should I target?",
		Options:   []collab.AttentionOption{{ID: "main", Label: "main"}, {ID: "next", Label: "next"}},
	})
	if err != nil {
		t.Fatalf("idempotent AskFromWorker: %v", err)
	}
	if again.ID != attention.ID {
		t.Fatalf("retried ask id = %s, want %s", again.ID, attention.ID)
	}
	attentions, err := env.store.ListAttentions(context.Background(), env.tenant, ticket.ID)
	if err != nil || len(attentions) != 1 {
		t.Fatalf("attentions = %d, err=%v; want one", len(attentions), err)
	}

	// A waiting report binds the ticket to that Attention.
	if _, err := env.orchestrator.ReportFromWorker(context.Background(), env.tenant, sessionID, superagent.WorkerReportInput{
		State: superagent.ReportWaiting, Summary: "waiting for a branch decision", AttentionID: attention.ID, RequestID: "wait:1",
	}); err != nil {
		t.Fatalf("Report waiting: %v", err)
	}
	if status := boardTicket(t, env, ticket.ID).Status; status != collab.WorkItemBlocked {
		t.Fatalf("status after waiting = %s, want blocked", status)
	}
	if _, _, err := env.orchestrator.AskFromWorker(context.Background(), env.tenant, sessionID, collab.AttentionRequest{
		RequestID: "q2", Prompt: "Another question?", AllowFreeText: true,
	}); workerToolCode(t, err) != string(superagent.WorkerToolTicketBlocked) {
		t.Fatalf("ask on blocked ticket err = %v, want ticket_blocked", err)
	}

	// The human answer wakes the same ticket; worker tools work again.
	if _, err := env.orchestrator.AnswerAttention(context.Background(), env.tenant, attention.ID, "next", "", "alice"); err != nil {
		t.Fatalf("AnswerAttention: %v", err)
	}
	if status := boardTicket(t, env, ticket.ID).Status; status != collab.WorkItemActive {
		t.Fatalf("status after answer = %s, want active", status)
	}
	if _, err := env.orchestrator.ReportFromWorker(context.Background(), env.tenant, sessionID, superagent.WorkerReportInput{
		State: superagent.ReportProgress, RequestID: "progress:after-answer",
	}); err != nil {
		t.Fatalf("progress after answer: %v", err)
	}
}

func TestWorkerReportRefusesBlockedTicket(t *testing.T) {
	env := newEnv(t, envOpts{})
	ticket := activeTicket(t, env, "e1", "issue#1")
	sessionID := ticket.ActiveAssignment.SessionID

	attention, _, err := env.orchestrator.AskFromWorker(context.Background(), env.tenant, sessionID, collab.AttentionRequest{
		RequestID: "q1", Prompt: "Proceed?", AllowFreeText: true,
	})
	if err != nil {
		t.Fatalf("AskFromWorker: %v", err)
	}
	if _, err := env.orchestrator.ReportFromWorker(context.Background(), env.tenant, sessionID, superagent.WorkerReportInput{
		State: superagent.ReportWaiting, AttentionID: attention.ID, RequestID: "wait:1",
	}); err != nil {
		t.Fatalf("Report waiting: %v", err)
	}
	_, err = env.orchestrator.ReportFromWorker(context.Background(), env.tenant, sessionID, superagent.WorkerReportInput{
		State: superagent.ReportProgress, RequestID: "progress:blocked",
	})
	if code := workerToolCode(t, err); code != string(superagent.WorkerToolTicketBlocked) {
		t.Fatalf("blocked report code = %q", code)
	}
	// A retried waiting report with the original request id still succeeds: the
	// idempotency fact predates the blocked guard.
	if _, err := env.orchestrator.ReportFromWorker(context.Background(), env.tenant, sessionID, superagent.WorkerReportInput{
		State: superagent.ReportWaiting, AttentionID: attention.ID, RequestID: "wait:1",
	}); err != nil {
		t.Fatalf("idempotent waiting report on blocked ticket: %v", err)
	}
}

func TestWorkerToolsRejectNonWorkerSessions(t *testing.T) {
	env := newEnv(t, envOpts{})
	ticket := activeTicket(t, env, "e1", "issue#1")
	ctx := context.Background()

	// A random session is not in the index at all.
	_, err := env.orchestrator.ReportFromWorker(ctx, env.tenant, uuid.New(), superagent.WorkerReportInput{
		State: superagent.ReportProgress, RequestID: "n1",
	})
	if code := workerToolCode(t, err); code != string(superagent.WorkerToolNotWorker) {
		t.Fatalf("unknown session code = %q", code)
	}

	// The orchestrator root session is a super-agent role but not a worker.
	_, err = env.orchestrator.ReportFromWorker(ctx, env.tenant, env.orchestratorSession.SessionID, superagent.WorkerReportInput{
		State: superagent.ReportProgress, RequestID: "n2",
	})
	if code := workerToolCode(t, err); code != string(superagent.WorkerToolNotWorker) {
		t.Fatalf("orchestrator session code = %q", code)
	}

	// A sub-agent child of the worker has no assignment binding.
	childID := uuid.New()
	if err := env.sessions.Put(ctx, collab.SessionRef{
		TenantID: env.tenant, SessionID: childID, ParentSessionID: ticket.ActiveAssignment.SessionID,
		BotID: "worker", ModeID: collab.ModeSubagent, WorkItemID: ticket.ID,
		State: collab.SessionActive,
	}); err != nil {
		t.Fatalf("put child: %v", err)
	}
	_, _, err = env.orchestrator.AskFromWorker(ctx, env.tenant, childID, collab.AttentionRequest{
		RequestID: "n3", Prompt: "Question?", AllowFreeText: true,
	})
	if code := workerToolCode(t, err); code != string(superagent.WorkerToolNotWorker) {
		t.Fatalf("sub-agent code = %q", code)
	}

	// A bare session id is the scope error, not a silent success.
	_, err = env.orchestrator.ReportFromWorker(ctx, env.tenant, uuid.Nil, superagent.WorkerReportInput{
		State: superagent.ReportProgress, RequestID: "n4",
	})
	if code := workerToolCode(t, err); code != string(superagent.WorkerToolScopeRequired) {
		t.Fatalf("empty session code = %q", code)
	}

	// Another tenant cannot use this orchestrator, even with a real session id.
	_, err = env.orchestrator.ReportFromWorker(ctx, uuid.New(), ticket.ActiveAssignment.SessionID, superagent.WorkerReportInput{
		State: superagent.ReportProgress, RequestID: "n5",
	})
	if code := workerToolCode(t, err); code != string(superagent.WorkerToolScopeRequired) {
		t.Fatalf("foreign tenant code = %q", code)
	}
}

func TestStaleWorkerAssignmentCannotFinishTheTicket(t *testing.T) {
	env := newEnv(t, envOpts{})
	ticket := activeTicket(t, env, "e1", "issue#1")
	oldSession := ticket.ActiveAssignment.SessionID
	oldAssignment := ticket.ActiveAssignment.ID

	// Dropping the assignment releases the ticket to the queue, which dispatches
	// a replacement worker in the same pass.
	if _, err := env.orchestrator.ReportFromWorker(context.Background(), env.tenant, oldSession, superagent.WorkerReportInput{
		State: superagent.ReportDropped, Summary: "handing back", RequestID: "drop:1",
	}); err != nil {
		t.Fatalf("drop report: %v", err)
	}
	board := boardTicket(t, env, ticket.ID)
	if board.Status != collab.WorkItemActive || board.ActiveAssignment == nil {
		t.Fatalf("ticket after drop = %+v", board)
	}
	if board.ActiveAssignment.SessionID == oldSession || board.ActiveAssignment.ID == oldAssignment {
		t.Fatalf("the replacement worker reused the old assignment: %+v", board.ActiveAssignment)
	}

	// The replaced worker's session still exists in the index, but its
	// assignment is terminal: it must not finish the ticket the new worker owns.
	_, err := env.orchestrator.ReportFromWorker(context.Background(), env.tenant, oldSession, superagent.WorkerReportInput{
		State: superagent.ReportReady, Summary: "stale completion", RequestID: "stale:ready",
	})
	if code := workerToolCode(t, err); code != string(superagent.WorkerToolTicketClosed) {
		t.Fatalf("stale ready code = %q, want ticket_closed", code)
	}
	if _, _, err := env.orchestrator.AskFromWorker(context.Background(), env.tenant, oldSession, collab.AttentionRequest{
		RequestID: "stale:ask", Prompt: "Still mine?", AllowFreeText: true,
	}); workerToolCode(t, err) != string(superagent.WorkerToolTicketClosed) {
		t.Fatalf("stale ask err = %v, want ticket_closed", err)
	}
	after := boardTicket(t, env, ticket.ID)
	if after.Status != collab.WorkItemActive || after.ActiveAssignment == nil || after.ActiveAssignment.ID != board.ActiveAssignment.ID {
		t.Fatalf("the stale worker changed the ticket: %+v", after)
	}
}

func TestReportClaimResumesAfterAuditFailureWithoutDoubleAudit(t *testing.T) {
	env := newEnv(t, envOpts{})
	ticket := activeTicket(t, env, "e1", "issue#1")
	sessionID := ticket.ActiveAssignment.SessionID

	env.audit.err = errors.New("audit store is down")
	if _, err := env.orchestrator.ReportFromWorker(context.Background(), env.tenant, sessionID, superagent.WorkerReportInput{
		State: superagent.ReportProgress, Summary: "first attempt", RequestID: "retry:1",
	}); err == nil {
		t.Fatal("report should fail while the audit sink is down")
	}
	stored, err := env.store.GetWorkerReport(context.Background(), env.tenant, ticket.ID, ticket.ActiveAssignment.ID, sessionID, "retry:1")
	if err != nil {
		t.Fatalf("claim was not written before the audit: %v", err)
	}
	if stored.AuditedAt != nil || stored.AppliedAt != nil {
		t.Fatalf("failed audit left a completed claim: %+v", stored)
	}
	if got := env.audit.count(superagent.AuditReport); got != 0 {
		t.Fatalf("audit count after failure = %d, want 0", got)
	}

	env.audit.err = nil
	outcome, err := env.orchestrator.ReportFromWorker(context.Background(), env.tenant, sessionID, superagent.WorkerReportInput{
		State: superagent.ReportProgress, Summary: "second attempt", RequestID: "retry:1",
	})
	if err != nil {
		t.Fatalf("retry after audit recovery: %v", err)
	}
	if outcome.Created || outcome.Report.State != superagent.ReportProgress || outcome.Report.ID != stored.ID {
		t.Fatalf("retry outcome = %+v", outcome)
	}
	if got := env.audit.count(superagent.AuditReport); got != 1 {
		t.Fatalf("audit count after retry = %d, want exactly 1", got)
	}
	stored, err = env.store.GetWorkerReport(context.Background(), env.tenant, ticket.ID, ticket.ActiveAssignment.ID, sessionID, "retry:1")
	if err != nil || stored.AuditedAt == nil || stored.AppliedAt == nil {
		t.Fatalf("claim after retry = %+v err=%v", stored, err)
	}

	// A completed claim is authoritative: a retry with a different payload
	// returns the stored state and does not apply the new one.
	again, err := env.orchestrator.ReportFromWorker(context.Background(), env.tenant, sessionID, superagent.WorkerReportInput{
		State: superagent.ReportReady, Summary: "different request", RequestID: "retry:1",
	})
	if err != nil {
		t.Fatalf("completed claim retry: %v", err)
	}
	if again.Created || again.Report.State != superagent.ReportProgress {
		t.Fatalf("completed claim retry = %+v", again)
	}
	if status := boardTicket(t, env, ticket.ID).Status; status != collab.WorkItemActive {
		t.Fatalf("a completed claim retry changed the ticket to %s", status)
	}
	if got := env.audit.count(superagent.AuditReport); got != 1 {
		t.Fatalf("completed claim retry audited again: %d", got)
	}
}

func TestWaitingReportRequiresOpenAttentionOwnedByWorker(t *testing.T) {
	env := newEnv(t, envOpts{})
	ticket := activeTicket(t, env, "e1", "issue#1")
	sessionID := ticket.ActiveAssignment.SessionID
	ctx := context.Background()

	attention, _, err := env.orchestrator.AskFromWorker(ctx, env.tenant, sessionID, collab.AttentionRequest{
		RequestID: "q:answered", Prompt: "Proceed?", AllowFreeText: true,
	})
	if err != nil {
		t.Fatalf("AskFromWorker: %v", err)
	}
	if _, err := env.orchestrator.AnswerAttention(ctx, env.tenant, attention.ID, "", "yes", "alice"); err != nil {
		t.Fatalf("AnswerAttention: %v", err)
	}
	_, err = env.orchestrator.ReportFromWorker(ctx, env.tenant, sessionID, superagent.WorkerReportInput{
		State: superagent.ReportWaiting, AttentionID: attention.ID, RequestID: "wait:answered",
	})
	if code := workerToolCode(t, err); code != string(superagent.WorkerToolAttentionInvalid) {
		t.Fatalf("answered attention report code = %q", code)
	}
	if _, err := env.store.GetWorkerReport(ctx, env.tenant, ticket.ID, ticket.ActiveAssignment.ID, sessionID, "wait:answered"); !errors.Is(err, superagent.ErrReportNotFound) {
		t.Fatalf("refused report left a claim: %v", err)
	}

	// An expired attention is equally unusable.
	expiring, _, err := env.orchestrator.AskFromWorker(ctx, env.tenant, sessionID, collab.AttentionRequest{
		RequestID: "q:expiring", Prompt: "Still there?", AllowFreeText: true, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("AskFromWorker expiring: %v", err)
	}
	*env.clock = env.clock.Add(2 * time.Minute)
	if _, err := env.orchestrator.ExpireAttentions(ctx); err != nil {
		t.Fatalf("ExpireAttentions: %v", err)
	}
	_, err = env.orchestrator.ReportFromWorker(ctx, env.tenant, sessionID, superagent.WorkerReportInput{
		State: superagent.ReportWaiting, AttentionID: expiring.ID, RequestID: "wait:expired",
	})
	if code := workerToolCode(t, err); code != string(superagent.WorkerToolAttentionInvalid) {
		t.Fatalf("expired attention report code = %q", code)
	}
}

func TestWaitingReportRejectsAnotherAssignmentsAttention(t *testing.T) {
	env := newEnv(t, envOpts{})
	ticket := activeTicket(t, env, "e1", "issue#1")
	oldSession := ticket.ActiveAssignment.SessionID
	oldAssignment := ticket.ActiveAssignment.ID
	ctx := context.Background()

	attention, _, err := env.orchestrator.AskFromWorker(ctx, env.tenant, oldSession, collab.AttentionRequest{
		RequestID: "q:old-worker", Prompt: "Old question?", AllowFreeText: true,
	})
	if err != nil {
		t.Fatalf("AskFromWorker: %v", err)
	}
	if _, err := env.orchestrator.ReportFromWorker(ctx, env.tenant, oldSession, superagent.WorkerReportInput{
		State: superagent.ReportDropped, Summary: "hand back", RequestID: "drop:old",
	}); err != nil {
		t.Fatalf("drop: %v", err)
	}
	replacement := boardTicket(t, env, ticket.ID).ActiveAssignment
	if replacement == nil || replacement.ID == oldAssignment || replacement.SessionID == oldSession {
		t.Fatalf("replacement assignment = %+v", replacement)
	}
	_, err = env.orchestrator.ReportFromWorker(ctx, env.tenant, replacement.SessionID, superagent.WorkerReportInput{
		State: superagent.ReportWaiting, AttentionID: attention.ID, RequestID: "wait:stolen",
	})
	if code := workerToolCode(t, err); code != string(superagent.WorkerToolAttentionInvalid) {
		t.Fatalf("foreign attention report code = %q", code)
	}
}

func TestReportRequestIDIsAssignmentScoped(t *testing.T) {
	env := newEnv(t, envOpts{})
	ticket := activeTicket(t, env, "e1", "issue#1")
	oldSession := ticket.ActiveAssignment.SessionID
	oldAssignment := ticket.ActiveAssignment.ID
	ctx := context.Background()

	first, err := env.orchestrator.ReportFromWorker(ctx, env.tenant, oldSession, superagent.WorkerReportInput{
		State: superagent.ReportProgress, Summary: "old worker progress", RequestID: "shared:1",
	})
	if err != nil || !first.Created {
		t.Fatalf("old worker report = %+v err=%v", first, err)
	}
	if _, err := env.orchestrator.ReportFromWorker(ctx, env.tenant, oldSession, superagent.WorkerReportInput{
		State: superagent.ReportDropped, Summary: "hand back", RequestID: "drop:1",
	}); err != nil {
		t.Fatalf("drop: %v", err)
	}
	replacement := boardTicket(t, env, ticket.ID).ActiveAssignment
	if replacement == nil || replacement.ID == oldAssignment {
		t.Fatalf("replacement assignment = %+v", replacement)
	}
	second, err := env.orchestrator.ReportFromWorker(ctx, env.tenant, replacement.SessionID, superagent.WorkerReportInput{
		State: superagent.ReportProgress, Summary: "new worker progress", RequestID: "shared:1",
	})
	if err != nil || !second.Created {
		t.Fatalf("replacement report = %+v err=%v; the old request id must not swallow it", second, err)
	}
	if second.Report.AssignmentID != replacement.ID || second.Report.Summary != "new worker progress" {
		t.Fatalf("replacement report fact = %+v", second.Report)
	}
	oldStored, err := env.store.GetWorkerReport(ctx, env.tenant, ticket.ID, oldAssignment, oldSession, "shared:1")
	if err != nil || oldStored.Summary != "old worker progress" {
		t.Fatalf("old claim changed: %+v err=%v", oldStored, err)
	}
}

func TestAskRequestIDIsAssignmentScoped(t *testing.T) {
	env := newEnv(t, envOpts{})
	ticket := activeTicket(t, env, "e1", "issue#1")
	oldSession := ticket.ActiveAssignment.SessionID
	oldAssignment := ticket.ActiveAssignment.ID
	ctx := context.Background()

	oldAttention, _, err := env.orchestrator.AskFromWorker(ctx, env.tenant, oldSession, collab.AttentionRequest{
		RequestID: "question:1", Prompt: "Old worker question?", AllowFreeText: true,
	})
	if err != nil {
		t.Fatalf("old ask: %v", err)
	}
	// A same-assignment retry returns the original Attention (idempotent).
	retried, _, err := env.orchestrator.AskFromWorker(ctx, env.tenant, oldSession, collab.AttentionRequest{
		RequestID: "question:1", Prompt: "Old worker question?", AllowFreeText: true,
	})
	if err != nil || retried.ID != oldAttention.ID {
		t.Fatalf("old ask retry = %+v err=%v", retried, err)
	}
	if _, err := env.orchestrator.ReportFromWorker(ctx, env.tenant, oldSession, superagent.WorkerReportInput{
		State: superagent.ReportDropped, Summary: "hand back", RequestID: "drop:ask",
	}); err != nil {
		t.Fatalf("drop: %v", err)
	}
	replacement := boardTicket(t, env, ticket.ID).ActiveAssignment
	if replacement == nil || replacement.ID == oldAssignment {
		t.Fatalf("replacement assignment = %+v", replacement)
	}
	// The replacement worker may not reuse the old request id and must not
	// receive its predecessor's Attention.
	_, _, err = env.orchestrator.AskFromWorker(ctx, env.tenant, replacement.SessionID, collab.AttentionRequest{
		RequestID: "question:1", Prompt: "New worker question?", AllowFreeText: true,
	})
	if code := workerToolCode(t, err); code != string(superagent.WorkerToolInvalidRequest) {
		t.Fatalf("replacement ask code = %q", code)
	}
	attentions, err := env.store.ListAttentions(ctx, env.tenant, ticket.ID)
	if err != nil || len(attentions) != 1 {
		t.Fatalf("attentions = %d err=%v, want only the old one", len(attentions), err)
	}
	// The HTTP-shaped path gets the conflict error, not the old Attention.
	if _, err := env.orchestrator.Ask(ctx, ticket.ID, replacement.ID, collab.AttentionRequest{
		RequestID: "question:1", Prompt: "New worker question?", CallerSessionID: replacement.SessionID, AllowFreeText: true,
	}); !errors.Is(err, collab.ErrAttentionConflict) {
		t.Fatalf("http-shaped scoped ask err = %v, want attention conflict", err)
	}
}

func TestWorkerToolCannotTargetAnotherTicket(t *testing.T) {
	env := newEnv(t, envOpts{})
	first := activeTicket(t, env, "e1", "issue#1")
	second := activeTicket(t, env, "e2", "issue#2")

	// The model-facing path has no ticket argument: the report lands on the
	// caller's own ticket.
	outcome, err := env.orchestrator.ReportFromWorker(context.Background(), env.tenant, first.ActiveAssignment.SessionID, superagent.WorkerReportInput{
		State: superagent.ReportProgress, RequestID: "own-ticket",
	})
	if err != nil {
		t.Fatalf("ReportFromWorker: %v", err)
	}
	if outcome.Ticket.ID != first.ID {
		t.Fatalf("report landed on %s, want %s", outcome.Ticket.ID, first.ID)
	}
	if status := boardTicket(t, env, second.ID).Status; status != collab.WorkItemActive {
		t.Fatalf("the other ticket changed to %s", status)
	}

	// The HTTP-shaped entry point still rejects a mismatched caller session:
	// a worker cannot name another ticket's assignment.
	err = env.orchestrator.Report(context.Background(), superagent.ReportRequest{
		TenantID: env.tenant, WorkItemID: second.ID, AssignmentID: second.ActiveAssignment.ID,
		CallerSessionID: first.ActiveAssignment.SessionID,
		State:           superagent.ReportProgress, RequestID: "cross-ticket",
	})
	if !errors.Is(err, collab.ErrPermissionDenied) {
		t.Fatalf("cross-ticket report err = %v, want permission denied", err)
	}
	// Nor can the worker ask a question on the other ticket.
	if _, err := env.orchestrator.Ask(context.Background(), second.ID, second.ActiveAssignment.ID, collab.AttentionRequest{
		CallerSessionID: first.ActiveAssignment.SessionID, RequestID: "cross-ask", Prompt: "Question?", AllowFreeText: true,
	}); !errors.Is(err, collab.ErrPermissionDenied) {
		t.Fatalf("cross-ticket ask err = %v, want permission denied", err)
	}
	if status := boardTicket(t, env, second.ID).Status; status != collab.WorkItemActive {
		t.Fatalf("the other ticket changed to %s", status)
	}
}
