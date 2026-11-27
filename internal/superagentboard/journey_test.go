package superagentboard

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/superagent"
)

// TestBoardCarriesTheTicketAnswerCompletionJourney is the 11.6 acceptance
// check: the board queue/thread is a complete task face, not a second view that
// still needs the legacy /superagent page. The legacy route stays marked for
// deletion rather than gaining a compatibility path.
func TestBoardCarriesTheTicketAnswerCompletionJourney(t *testing.T) {
	mirror, store, board, links, _, tenantID := mirrorFixture(t)
	ctx := context.Background()
	ticket := seedTicket(t, store, tenantID, "Ship the fix", "Body", "manual", "journey-1", "evt-journey")
	assignment := addAssignment(t, store, ticket, collab.AssignmentActive, "researcher", uuid.New())
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemActive)

	report, err := mirror.SyncOnce(ctx)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if report.TasksCreated != 1 || report.Claims != 1 {
		t.Fatalf("journey start = %+v", report)
	}
	link := mustLink(t, links, tenantID, ticket.ID.String(), storeTicketKind())
	task := mustTask(t, board, tenantID, link.BoardTaskID)
	if task.State != StateClaimed {
		t.Fatalf("state = %q, want claimed", task.State)
	}
	if task.Labels["source"] != BoardSource || task.Labels["ticket"] != ticket.ID.String() {
		t.Fatalf("labels = %v", task.Labels)
	}

	// A human decision is part of the same thread: blocked, asked, answered.
	attention := addAttention(t, store, ticket, assignment, collab.AttentionClarify, collab.AttentionOpen)
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemBlocked)
	if _, err := mirror.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if mustTask(t, board, tenantID, link.BoardTaskID).State != StateBlocked {
		t.Fatal("the open attention did not block the board task")
	}
	answerAttention(t, store, attention, "yes")
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemActive)
	if _, err := mirror.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	unblocked := mustTask(t, board, tenantID, link.BoardTaskID)
	if unblocked.State != StateClaimed || unblocked.Replies != 1 {
		t.Fatalf("after answer = %+v", unblocked)
	}

	// The worker report is the thread's work log, and the completion is the
	// task's terminal state.
	addAppliedReport(t, store, ticket, assignment, superagent.ReportReady, "tests pass")
	if _, err := mirror.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if mustTask(t, board, tenantID, link.BoardTaskID).Replies != 2 {
		t.Fatalf("replies = %d, want 2", mustTask(t, board, tenantID, link.BoardTaskID).Replies)
	}
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemDone)
	if _, err := mirror.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	done := mustTask(t, board, tenantID, link.BoardTaskID)
	if done.State != StateCompleted {
		t.Fatalf("final state = %q", done.State)
	}
	view, err := board.TaskView(ctx, tenantID, link.BoardTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Task.Result == nil || *view.Task.Result != "tests pass" {
		t.Fatalf("result = %#v", view.Task.Result)
	}
	if !view.NoSync {
		t.Fatal("the mirrored journey must not be pushed back through sync")
	}

	// The legacy board is explicitly frozen; the board is no longer a second
	// optional entry. The actual deletion is Phase 2 15.7.
	if superagent.DeprecatedBoardRoute != "/superagent" {
		t.Fatalf("deprecated route = %q", superagent.DeprecatedBoardRoute)
	}
	if superagent.LegacyBoardRetirement != "15.7" {
		t.Fatalf("retirement task = %q", superagent.LegacyBoardRetirement)
	}
}

// TestMirrorNeverRewritesCollabRecords proves the one-way direction outside
// the archive case: a normal pass may write board events and links, but every
// collab_* snapshot stays byte-identical.
func TestMirrorNeverRewritesCollabRecords(t *testing.T) {
	mirror, store, board, links, _, tenantID := mirrorFixture(t)
	ctx := context.Background()
	ticket := seedTicket(t, store, tenantID, "One way", "Body", "manual", "one-way-1", "evt-oneway")
	assignment := addAssignment(t, store, ticket, collab.AssignmentActive, "researcher", uuid.New())
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemActive)
	addAttention(t, store, ticket, assignment, collab.AttentionClarify, collab.AttentionOpen)
	updateTicketStatus(t, store, tenantID, ticket.ID, collab.WorkItemBlocked)
	addAppliedReport(t, store, ticket, assignment, superagent.ReportProgress, "halfway")

	before := ticketSnapshot(t, store, tenantID)
	if _, err := mirror.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if after := ticketSnapshot(t, store, tenantID); after != before {
		t.Fatal("the mirror changed a collab record")
	}
	link := mustLink(t, links, tenantID, ticket.ID.String(), storeTicketKind())
	if link.BoardTaskID == 0 {
		t.Fatal("no board task")
	}
	if head := boardHead(t, board, tenantID); head == 0 {
		t.Fatal("no board events")
	}
}
