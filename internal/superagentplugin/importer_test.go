package superagentplugin_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/boardplugin"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/superagent"
	"github.com/jixoai/keel/internal/superagentplugin"
	"github.com/jixoai/keel/internal/superagentstore"
)

// memoryLinkStore is the in-memory LinkStore. It mirrors the PostgreSQL
// adapter's two identity rules: (tenant, source, source_id, kind) is unique and
// one board task maps to at most one ticket. The duplicate insert returns the
// existing row instead of an error, exactly like the production adapter.
type memoryLinkStore struct {
	mu    sync.Mutex
	rows  []superagentstore.BoardLink
	epoch map[string]uint64
}

func newMemoryLinkStore() *memoryLinkStore {
	return &memoryLinkStore{epoch: map[string]uint64{}}
}

func (m *memoryLinkStore) PutBoardLink(_ context.Context, link superagentstore.BoardLink) (superagentstore.BoardLink, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range m.rows {
		if row.TenantID == link.TenantID && row.Source == link.Source && row.SourceID == link.SourceID && row.Kind == link.Kind {
			if row.BoardTaskID != link.BoardTaskID {
				return superagentstore.BoardLink{}, fmt.Errorf("link already maps task %d", row.BoardTaskID)
			}
			return row, nil
		}
	}
	if link.Kind == superagentstore.BoardLinkTicket {
		for _, row := range m.rows {
			if row.TenantID == link.TenantID && row.Source == link.Source && row.Kind == link.Kind && row.BoardTaskID == link.BoardTaskID {
				return row, nil
			}
		}
	}
	if link.CreatedAt.IsZero() {
		link.CreatedAt = time.Now().UTC()
	}
	link.UpdatedAt = link.CreatedAt
	m.rows = append(m.rows, link)
	return link, nil
}

func (m *memoryLinkStore) GetBoardLink(_ context.Context, tenantID uuid.UUID, source, sourceID, kind string) (superagentstore.BoardLink, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range m.rows {
		if row.TenantID == tenantID && row.Source == source && row.SourceID == sourceID && row.Kind == kind {
			return row, true, nil
		}
	}
	return superagentstore.BoardLink{}, false, nil
}

func (m *memoryLinkStore) GetBoardLinkByTask(_ context.Context, tenantID uuid.UUID, source, kind string, boardTaskID uint64) (superagentstore.BoardLink, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range m.rows {
		if row.TenantID == tenantID && row.Source == source && row.Kind == kind && row.BoardTaskID == boardTaskID {
			return row, true, nil
		}
	}
	return superagentstore.BoardLink{}, false, nil
}

func (m *memoryLinkStore) SetBoardLinkClaimEpoch(_ context.Context, tenantID uuid.UUID, source, sourceID, kind string, epoch uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range m.rows {
		if row.TenantID == tenantID && row.Source == source && row.SourceID == sourceID && row.Kind == kind {
			m.epoch[sourceID] = epoch
			return nil
		}
	}
	return superagentstore.ErrNotFound
}

func importedTickets(t *testing.T, store superagent.Store, tenantID uuid.UUID) []collab.WorkItem {
	t.Helper()
	items, err := store.ListWorkItems(context.Background(), tenantID, collab.WorkFilter{ModeID: superagent.DefaultModeID})
	if err != nil {
		t.Fatalf("list work items: %v", err)
	}
	return items
}

// TestImportOnlyCreatesExecutableTickets pins the board-history rule and the
// projected-task rule: completed/archived tasks and the writer's own
// projections never become a second open ticket.
func TestImportOnlyCreatesExecutableTickets(t *testing.T) {
	ctx := context.Background()
	tenantID := uuid.New()
	clock := func() time.Time { return time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC) }
	store := superagent.NewMemoryStore(clock)
	links := newMemoryLinkStore()
	ownerSession := uuid.New()
	dispatched := make([]uuid.UUID, 0, 1)
	opts := superagentplugin.ImportOptions{
		Store: store, Links: links, TenantID: tenantID, WorkerBot: "worker-bot",
		OwnerSessionID: ownerSession,
		Dispatch: func(_ context.Context, ticketID uuid.UUID) error {
			dispatched = append(dispatched, ticketID)
			return nil
		},
		Now: clock,
	}
	projected := boardTaskAt(1, "ready", map[string]string{"source": superagentplugin.BoardSource, "ticket": uuid.NewString()})
	if _, created, err := superagentplugin.ImportBoardTask(ctx, opts, projected); err != nil || created {
		t.Fatalf("projected task imported: created=%v err=%v", created, err)
	}
	terminal := boardTaskAt(2, "completed", nil)
	if _, created, err := superagentplugin.ImportBoardTask(ctx, opts, terminal); err != nil || created {
		t.Fatalf("completed task imported: created=%v err=%v", created, err)
	}
	archived := boardTaskAt(3, "ready", map[string]string{superagentplugin.ArchivedLabel: "true"})
	if _, created, err := superagentplugin.ImportBoardTask(ctx, opts, archived); err != nil || created {
		t.Fatalf("archived task imported: created=%v err=%v", created, err)
	}
	executable := boardTaskAt(4, "ready", nil)
	ticket, created, err := superagentplugin.ImportBoardTask(ctx, opts, executable)
	if err != nil || !created {
		t.Fatalf("executable import: created=%v err=%v", created, err)
	}
	if ticket.WorkerBot != "worker-bot" {
		t.Fatalf("ticket WorkerBot = %q, want worker-bot", ticket.WorkerBot)
	}
	if ticket.OwnerSessionID != ownerSession {
		t.Fatalf("ticket OwnerSessionID = %s, want %s", ticket.OwnerSessionID, ownerSession)
	}
	if len(dispatched) != 1 || dispatched[0] != ticket.ID {
		t.Fatalf("dispatched = %v, want the imported ticket %s", dispatched, ticket.ID)
	}
	// The importer must not write a sessionless OPEN assignment: dispatch sees
	// it as an existing assignment and would skip the ticket forever.
	assignments, err := store.ListAssignments(ctx, tenantID, ticket.ID)
	if err != nil || len(assignments) != 0 {
		t.Fatalf("assignments after import = %+v, err=%v, want none", assignments, err)
	}
	// A repeat import finds the link by board task and repairs dispatch without
	// creating a second ticket.
	if _, again, err := superagentplugin.ImportBoardTask(ctx, opts, executable); err != nil || again {
		t.Fatalf("repeat import: created=%v err=%v", again, err)
	}
	if len(dispatched) != 2 {
		t.Fatalf("repeat import did not re-run dispatch: %v", dispatched)
	}
	if items := importedTickets(t, store, tenantID); len(items) != 1 {
		t.Fatalf("tickets = %d, want 1", len(items))
	}
}

func boardTaskAt(id uint64, state string, labels map[string]string) boardplugin.TaskInfo {
	return boardplugin.TaskInfo{ID: id, State: state, Labels: labels}
}
