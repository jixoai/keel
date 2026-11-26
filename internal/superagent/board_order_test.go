package superagent_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/superagent"
)

func TestBoardOrdersByUpdatedAtDescAndLimitTakesHead(t *testing.T) {
	env := newEnv(t, envOpts{})
	ctx := context.Background()
	base := *env.clock

	first := activeTicket(t, env, "e1", "issue#1")
	*env.clock = base.Add(time.Minute)
	second := activeTicket(t, env, "e2", "issue#2")
	*env.clock = base.Add(2 * time.Minute)
	touchTicket(t, env, first.ID, "first updated")

	board, err := env.orchestrator.Board(ctx, env.tenant, superagent.BoardFilter{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if len(board.Tickets) != 2 || board.Tickets[0].ID != first.ID || board.Tickets[1].ID != second.ID {
		t.Fatalf("board order = %v, want %s then %s", ticketIDs(board.Tickets), first.ID, second.ID)
	}
	limited, err := env.orchestrator.Board(ctx, env.tenant, superagent.BoardFilter{Limit: 1})
	if err != nil {
		t.Fatalf("Board limit: %v", err)
	}
	if len(limited.Tickets) != 1 || limited.Tickets[0].ID != first.ID {
		t.Fatalf("board limit = %v, want the most recently updated ticket %s", ticketIDs(limited.Tickets), first.ID)
	}

	// Updating the other ticket moves it to the head; the limit follows.
	*env.clock = base.Add(3 * time.Minute)
	touchTicket(t, env, second.ID, "second updated")
	board, err = env.orchestrator.Board(ctx, env.tenant, superagent.BoardFilter{})
	if err != nil {
		t.Fatalf("Board after second update: %v", err)
	}
	if len(board.Tickets) != 2 || board.Tickets[0].ID != second.ID || board.Tickets[1].ID != first.ID {
		t.Fatalf("board order after second update = %v", ticketIDs(board.Tickets))
	}
	limited, err = env.orchestrator.Board(ctx, env.tenant, superagent.BoardFilter{Limit: 1})
	if err != nil {
		t.Fatalf("Board limit after second update: %v", err)
	}
	if len(limited.Tickets) != 1 || limited.Tickets[0].ID != second.ID {
		t.Fatalf("board limit after second update = %v", ticketIDs(limited.Tickets))
	}
}

func TestWorkItemOrderVocabularyIsClosed(t *testing.T) {
	if !collab.WorkOrder("").Valid() || !collab.WorkOrderCreatedAsc.Valid() || !collab.WorkOrderUpdatedDesc.Valid() {
		t.Fatal("declared work orders must be valid")
	}
	if collab.WorkOrder("bogus").Valid() {
		t.Fatal("an unknown work order must not be valid")
	}
	env := newEnv(t, envOpts{})
	if _, err := env.store.ListWorkItems(context.Background(), env.tenant, collab.WorkFilter{Order: collab.WorkOrder("bogus")}); err == nil {
		t.Fatal("an unknown work order reached the store without an error")
	}
}

func touchTicket(t *testing.T, env *testEnv, ticketID uuid.UUID, title string) {
	t.Helper()
	item, err := env.store.GetWorkItem(context.Background(), env.tenant, ticketID)
	if err != nil {
		t.Fatalf("GetWorkItem: %v", err)
	}
	item.Title = title
	if _, err := env.store.UpdateWorkItem(context.Background(), item); err != nil {
		t.Fatalf("UpdateWorkItem: %v", err)
	}
}

func ticketIDs(tickets []superagent.TicketView) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(tickets))
	for _, ticket := range tickets {
		out = append(out, ticket.ID)
	}
	return out
}
