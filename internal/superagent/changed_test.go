package superagent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/superagent"
)

// TestChangedPublishesOnlySuccessfulWrites pins the SSE invalidation contract:
// a successful ticket/Attention write publishes once; a refused or failed
// write publishes nothing.
func TestChangedPublishesOnlySuccessfulWrites(t *testing.T) {
	env := newEnv(t, envOpts{})
	ctx := context.Background()

	// A failed accept must not publish.
	env.audit.err = errors.New("audit store is down")
	if _, err := env.orchestrator.HandleSourceEvent(ctx, fixtureEvent(t, issueEvent("fail-1", "issue#fail"))); err == nil {
		t.Fatal("accept should fail when the audit sink is down")
	}
	if env.changes.count() != 0 {
		t.Fatalf("failed accept published %d changes", env.changes.count())
	}
	env.audit.err = nil

	// Accept creates a ticket -> one invalidation.
	if _, err := env.orchestrator.HandleSourceEvent(ctx, fixtureEvent(t, issueEvent("e1", "issue#1"))); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if env.changes.count() != 1 || env.changes.last() != env.tenant {
		t.Fatalf("accept changes = %d last=%s", env.changes.count(), env.changes.last())
	}
	board, err := env.orchestrator.Board(ctx, env.tenant, superagent.BoardFilter{})
	if err != nil || len(board.Tickets) != 1 || board.Tickets[0].ActiveAssignment == nil {
		t.Fatalf("board = %+v err=%v", board, err)
	}
	ticket := board.Tickets[0]
	sessionID := ticket.ActiveAssignment.SessionID

	// A successful report publishes.
	if _, err := env.orchestrator.ReportFromWorker(ctx, env.tenant, sessionID, superagent.WorkerReportInput{
		State: superagent.ReportProgress, Summary: "working", RequestID: "change:progress",
	}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if env.changes.count() != 2 {
		t.Fatalf("report changes = %d, want 2", env.changes.count())
	}

	// A successful ask publishes.
	attention, _, err := env.orchestrator.AskFromWorker(ctx, env.tenant, sessionID, collab.AttentionRequest{
		RequestID: "change:ask", Prompt: "Proceed?", AllowFreeText: true,
	})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if env.changes.count() != 3 {
		t.Fatalf("ask changes = %d, want 3", env.changes.count())
	}

	// A successful answer publishes.
	if _, err := env.orchestrator.AnswerAttention(ctx, env.tenant, attention.ID, "", "yes", "alice"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if env.changes.count() != 4 {
		t.Fatalf("answer changes = %d, want 4", env.changes.count())
	}

	// A successful ready report, resume and stop each publish.
	if _, err := env.orchestrator.ReportFromWorker(ctx, env.tenant, sessionID, superagent.WorkerReportInput{
		State: superagent.ReportReady, Summary: "done", RequestID: "change:ready",
	}); err != nil {
		t.Fatalf("report ready: %v", err)
	}
	if env.changes.count() != 5 {
		t.Fatalf("ready changes = %d, want 5", env.changes.count())
	}
	if _, err := env.orchestrator.ResumeTicket(ctx, env.tenant, ticket.ID, "alice", "again"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if env.changes.count() != 6 {
		t.Fatalf("resume changes = %d, want 6", env.changes.count())
	}
	if err := env.orchestrator.StopTicket(ctx, env.tenant, ticket.ID, "alice", "stop"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if env.changes.count() != 7 {
		t.Fatalf("stop changes = %d, want 7", env.changes.count())
	}

	// A refused report after the stop publishes nothing.
	before := env.changes.count()
	if _, err := env.orchestrator.ReportFromWorker(ctx, env.tenant, sessionID, superagent.WorkerReportInput{
		State: superagent.ReportProgress, RequestID: "change:refused",
	}); err == nil {
		t.Fatal("report on a cancelled ticket should be refused")
	}
	if env.changes.count() != before {
		t.Fatalf("refused report published %d changes", env.changes.count()-before)
	}
}
