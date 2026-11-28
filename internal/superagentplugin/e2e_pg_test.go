package superagentplugin_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	boardlog "github.com/jixoai/keel-board/log"
	boardstore "github.com/jixoai/keel-board/store"

	"github.com/jixoai/keel/internal/boardplugin"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/superagent"
	"github.com/jixoai/keel/internal/superagentplugin"
	"github.com/jixoai/keel/internal/superagentstore"
)

// TestBoardTaskToTicketToBoardWriteback is the change's E2E:
//
//	board plugin task -> superagent ticket + assignment -> board write-back.
//
// The board substrate is the board plugin's Service (memory log here; its
// PostgreSQL adapter is the board module's own, covered by the board repo's
// storage conformance). The ticket/assignment rows are the real PostgreSQL
// superagent_* tables. No board table is read: every board operation goes
// through the Service interface.
func TestBoardTaskToTicketToBoardWriteback(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	keelStore, err := store.Open(ctx, store.Config{DSN: dsn, MaxConns: 4, MinConns: 1})
	if err != nil {
		t.Fatalf("open keel store: %v", err)
	}
	t.Cleanup(keelStore.Close)
	domain := superagentstore.New(keelStore.Pool())
	tenantID := uuid.New()

	clock := func() time.Time { return time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC) }
	board := &boardplugin.LogBoard{
		Board: boardlog.New(boardlog.Options{Store: boardstore.NewMemory(), Now: clock}),
		Now:   clock,
	}
	task, err := board.EnsureTicketThread(ctx, tenantID, boardplugin.TicketThread{
		TicketID: "board-task-1", Channel: "ops", Title: "Fix the build",
		Request: "The CI job fails.", Actor: boardplugin.ServiceActor(tenantID),
	})
	if err != nil {
		t.Fatalf("board task: %v", err)
	}

	ticket, created, err := superagentplugin.ImportBoardTask(ctx, superagentplugin.ImportOptions{
		Store: domain, Links: domain, TenantID: tenantID,
		Requester: superagent.Actor{ID: "board:" + tenantID.String(), Display: "Board"},
		WorkerBot: "worker", Now: clock,
	}, task)
	if err != nil || !created {
		t.Fatalf("import board task: created=%v err=%v", created, err)
	}
	if ticket.ID == uuid.Nil || ticket.Title != task.Text {
		t.Fatalf("ticket = %+v", ticket)
	}
	// Repeat import is a no-op (the link is the identity map).
	if _, again, err := superagentplugin.ImportBoardTask(ctx, superagentplugin.ImportOptions{
		Store: domain, Links: domain, TenantID: tenantID, WorkerBot: "worker", Now: clock,
	}, task); err != nil || again {
		t.Fatalf("repeat import: created=%v err=%v", again, err)
	}
	assignments, err := domain.ListAssignments(ctx, tenantID, ticket.ID)
	if err != nil || len(assignments) != 1 {
		t.Fatalf("assignments = %+v err=%v", assignments, err)
	}
	assignment := assignments[0]
	assignment.Status = collab.AssignmentDone
	if _, err := domain.UpdateAssignment(ctx, assignment); err != nil {
		t.Fatalf("complete assignment: %v", err)
	}
	item, err := domain.GetWorkItem(ctx, tenantID, ticket.ID)
	if err != nil {
		t.Fatalf("read ticket: %v", err)
	}
	item.Status = collab.WorkItemDone
	if _, err := domain.UpdateWorkItem(ctx, item); err != nil {
		t.Fatalf("complete ticket: %v", err)
	}

	tokens, err := superagentplugin.NewTokenSource("test-secret")
	if err != nil {
		t.Fatalf("token source: %v", err)
	}
	writer, err := superagentplugin.NewWriter(&superagentplugin.Writer{
		TenantID: tenantID,
		Store:    domain,
		Board:    board,
		Links:    domain,
		Config: superagentplugin.StaticConfigReader{Config: superagentplugin.SuperagentConfig{
			Mirror: true, Channel: "ops",
		}},
		Secret: tokens,
		Now:    clock,
	})
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	report, err := writer.SyncTicket(ctx, ticket.ID)
	if err != nil {
		t.Fatalf("sync ticket: %v", err)
	}
	if report.Completed != 1 {
		t.Fatalf("sync report = %+v, want one completed write-back", report)
	}
	done, err := board.Task(ctx, tenantID, task.ID)
	if err != nil {
		t.Fatalf("board task after write-back: %v", err)
	}
	if done.State != boardplugin.StateCompleted {
		t.Fatalf("board task state = %s, want completed", done.State)
	}
}
