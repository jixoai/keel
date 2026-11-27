package superagentboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/superagent"

	boardlog "github.com/jixoai/keel-board/log"
	boardstore "github.com/jixoai/keel-board/store"
)

// testClock is a manually advanced clock. The mirror and LogBoard share it, so
// lease expiry and renewal thresholds are deterministic.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// memoryLinks is the test LinkStore. It implements the same uniqueness rules as
// the PostgreSQL table: one source record maps to one board task.
type memoryLinks struct {
	mu   sync.Mutex
	rows map[string]store.BoardLink
}

func newMemoryLinks() *memoryLinks {
	return &memoryLinks{rows: map[string]store.BoardLink{}}
}

func memoryLinkKey(tenantID uuid.UUID, source, sourceID, kind string) string {
	return tenantID.String() + "\x00" + source + "\x00" + sourceID + "\x00" + kind
}

func (m *memoryLinks) PutBoardLink(_ context.Context, link store.BoardLink) (store.BoardLink, error) {
	if err := link.Validate(); err != nil {
		return store.BoardLink{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memoryLinkKey(link.TenantID, link.Source, link.SourceID, link.Kind)
	if existing, ok := m.rows[key]; ok {
		if existing.BoardTaskID != link.BoardTaskID {
			return store.BoardLink{}, fmt.Errorf("memory links: %s already maps task %d", key, existing.BoardTaskID)
		}
		return existing, nil
	}
	link.CreatedAt = time.Now().UTC()
	link.UpdatedAt = link.CreatedAt
	m.rows[key] = link
	return link, nil
}

func (m *memoryLinks) GetBoardLink(_ context.Context, tenantID uuid.UUID, source, sourceID, kind string) (store.BoardLink, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	link, ok := m.rows[memoryLinkKey(tenantID, source, sourceID, kind)]
	return link, ok, nil
}

func (m *memoryLinks) GetBoardLinkByTask(_ context.Context, tenantID uuid.UUID, source, kind string, boardTaskID uint64) (store.BoardLink, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, link := range m.rows {
		if link.TenantID == tenantID && link.Source == source && link.Kind == kind && link.BoardTaskID == boardTaskID {
			return link, true, nil
		}
	}
	return store.BoardLink{}, false, nil
}

func (m *memoryLinks) SetBoardLinkClaimEpoch(_ context.Context, tenantID uuid.UUID, source, sourceID, kind string, epoch uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memoryLinkKey(tenantID, source, sourceID, kind)
	link, ok := m.rows[key]
	if !ok {
		return store.ErrNotFound
	}
	link.ClaimEpoch = epoch
	link.UpdatedAt = time.Now().UTC()
	m.rows[key] = link
	return nil
}

func (m *memoryLinks) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}

func newTestBoard(clock *testClock) *LogBoard {
	if clock == nil {
		clock = newTestClock()
	}
	return &LogBoard{Board: boardlog.New(boardlog.Options{Store: boardstore.NewMemory(), Now: clock.Now}), Now: clock.Now}
}

// seedTicket creates the supervisor-side Ticket exactly as the orchestrator
// would: a WorkItem whose document carries the Mandate/Thread/Request.
func seedTicket(t *testing.T, store *superagent.MemoryStore, tenantID uuid.UUID, title, body, source, threadID, eventID string) superagent.Ticket {
	t.Helper()
	event := superagent.Event{
		SourceID: source,
		EventID:  eventID,
		Kind:     superagent.EventIssueOpened,
		Actor:    superagent.Actor{ID: "octocat", Login: "octocat", Display: "Octo Cat"},
		Thread:   superagent.ThreadRef{Source: source, Kind: "issue", ID: threadID},
		Title:    title,
		Body:     body,
	}
	ticket := superagent.NewTicket(tenantID, superagent.DefaultModeID, event,
		superagent.Verdict{Action: superagent.ActionAccept, Strength: superagent.MandateWrite},
		"researcher", time.Now().UTC())
	ticket.IdempotencyKey = event.IdempotencyKey()
	item, err := store.CreateWorkItem(context.Background(), ticket.Record())
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	return superagent.TicketFromWorkItem(item)
}

func updateTicketStatus(t *testing.T, store *superagent.MemoryStore, tenantID, ticketID uuid.UUID, status collab.WorkItemStatus) superagent.Ticket {
	t.Helper()
	ctx := context.Background()
	item, err := store.GetWorkItem(ctx, tenantID, ticketID)
	if err != nil {
		t.Fatalf("get ticket: %v", err)
	}
	item.Status = status
	item.UpdatedAt = time.Now().UTC()
	updated, err := store.UpdateWorkItem(ctx, item)
	if err != nil {
		t.Fatalf("update ticket: %v", err)
	}
	return superagent.TicketFromWorkItem(updated)
}

func addAssignment(t *testing.T, store *superagent.MemoryStore, ticket superagent.Ticket, status collab.AssignmentStatus, botID string, workerSession uuid.UUID) collab.Assignment {
	t.Helper()
	assignment, err := store.CreateAssignment(context.Background(), collab.Assignment{
		ID: uuid.New(), TenantID: ticket.TenantID, WorkItemID: ticket.ID,
		ModeID: superagent.DefaultModeID, BotID: botID, SessionID: workerSession,
		Status: status, Title: ticket.Title, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create assignment: %v", err)
	}
	return assignment
}

func addAttention(t *testing.T, store *superagent.MemoryStore, ticket superagent.Ticket, assignment collab.Assignment, kind string, status collab.AttentionStatus) collab.Attention {
	t.Helper()
	attention, err := store.CreateAttention(context.Background(), collab.Attention{
		ID: uuid.New(), TenantID: ticket.TenantID, WorkItemID: ticket.ID,
		AssignmentID: assignment.ID, ModeID: superagent.DefaultModeID,
		SessionID: assignment.SessionID, Kind: kind, Prompt: "Approve the write?",
		Route: collab.AttentionRouteChannel, Status: status,
		CreatedAt: time.Now().UTC(), Deadline: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("create attention: %v", err)
	}
	return attention
}

func answerAttention(t *testing.T, store *superagent.MemoryStore, attention collab.Attention, optionID string) collab.Attention {
	t.Helper()
	attention.Status = collab.AttentionAnswered
	attention.Answer = &collab.AttentionAnswer{OptionID: optionID, Actor: "alice", At: time.Now().UTC()}
	updated, err := store.UpdateAttention(context.Background(), attention)
	if err != nil {
		t.Fatalf("answer attention: %v", err)
	}
	return updated
}

func addAppliedReport(t *testing.T, store *superagent.MemoryStore, ticket superagent.Ticket, assignment collab.Assignment, state superagent.ReportState, summary string) superagent.WorkerReport {
	t.Helper()
	ctx := context.Background()
	report, created, err := store.ClaimWorkerReport(ctx, superagent.WorkerReport{
		ID: uuid.New(), TenantID: ticket.TenantID, WorkItemID: ticket.ID,
		AssignmentID: assignment.ID, SessionID: assignment.SessionID,
		RequestID: uuid.NewString(), State: state, Summary: summary,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil || !created {
		t.Fatalf("claim report: created=%v err=%v", created, err)
	}
	if _, err := store.MarkWorkerReportAudited(ctx, ticket.TenantID, report.ID, time.Now().UTC()); err != nil {
		t.Fatalf("audit report: %v", err)
	}
	if _, err := store.MarkWorkerReportApplied(ctx, ticket.TenantID, report.ID, time.Now().UTC()); err != nil {
		t.Fatalf("apply report: %v", err)
	}
	report.AuditedAt = timePtr(time.Now().UTC())
	report.AppliedAt = timePtr(time.Now().UTC())
	return report
}

func timePtr(value time.Time) *time.Time { return &value }

func newMirrorForTest(t *testing.T, tenantID uuid.UUID, store superagent.Store, board BoardPort, links LinkStore, config SuperagentConfig, clock *testClock) *Mirror {
	t.Helper()
	tokens, err := NewTokenSource("test-secret")
	if err != nil {
		t.Fatalf("token source: %v", err)
	}
	if clock == nil {
		clock = newTestClock()
	}
	mirror, err := NewMirror(&Mirror{
		TenantID: tenantID, Store: store, Board: board, Links: links,
		Config: StaticConfigReader{Config: config}, Secret: tokens,
		Logger: discardLogger(), Now: clock.Now,
		Lease: DefaultLease, RenewBefore: DefaultRenewBefore,
	})
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	return mirror
}

func boardHead(t *testing.T, board *LogBoard, tenantID uuid.UUID) uint64 {
	t.Helper()
	head, err := board.Board.Head(tenantID.String())
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	return head
}

func mustTask(t *testing.T, board *LogBoard, tenantID uuid.UUID, taskID uint64) TaskInfo {
	t.Helper()
	info, err := board.Task(context.Background(), tenantID, taskID)
	if err != nil {
		t.Fatalf("task %d: %v", taskID, err)
	}
	return info
}

func mustLink(t *testing.T, links LinkStore, tenantID uuid.UUID, sourceID, kind string) store.BoardLink {
	t.Helper()
	link, found, err := links.GetBoardLink(context.Background(), tenantID, BoardSource, sourceID, kind)
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if !found {
		t.Fatalf("link %s/%s not found", sourceID, kind)
	}
	return link
}

func requireNoError(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

var errFake = errors.New("fake supervisor rejection")
