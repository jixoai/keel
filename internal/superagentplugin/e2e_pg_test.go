package superagentplugin_test

import (
	"context"
	"os"
	"sync"
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
	if err != nil || len(assignments) != 0 {
		t.Fatalf("assignments after import = %+v err=%v, want none (dispatch owns the worker session)", assignments, err)
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

// TestWriterImportDoesNotReimportItsOwnProjection is the P1-1 regression: a
// ticket that the writer projected into the board becomes a board task, and the
// next SyncOnce's Import pass used to treat that task as a new source task. The
// second ticket then collided with the one-ticket-per-board-task index and the
// import failed forever. The production loop must be stable across passes.
func TestWriterImportDoesNotReimportItsOwnProjection(t *testing.T) {
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
	// A ticket that did not come from the board: its projection is the task the
	// next import pass sees.
	item, err := domain.CreateWorkItem(ctx, collab.WorkItem{
		ID: uuid.New(), TenantID: tenantID, ModeID: superagent.DefaultModeID,
		Title: "source ticket", Status: collab.WorkItemOpen, CreatedAt: clock(),
	})
	if err != nil {
		t.Fatalf("create source ticket: %v", err)
	}
	tokens, err := superagentplugin.NewTokenSource("test-secret")
	if err != nil {
		t.Fatalf("token source: %v", err)
	}
	importOptions := superagentplugin.ImportOptions{
		Store: domain, Links: domain, TenantID: tenantID, WorkerBot: "worker", Now: clock,
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
		Import: func(ctx context.Context) error {
			_, err := superagentplugin.ImportChannel(ctx, board, importOptions, "ops", 200)
			return err
		},
		Now: clock,
	})
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	first, err := writer.SyncOnce(ctx)
	if err != nil {
		t.Fatalf("first SyncOnce: %v", err)
	}
	if first.TasksCreated != 1 {
		t.Fatalf("first pass report = %+v, want one projected task", first)
	}
	// The next passes run Import over the board channel, which now contains the
	// projection. It must skip it by board_task_id (and by the source label).
	for pass := 2; pass <= 3; pass++ {
		report, err := writer.SyncOnce(ctx)
		if err != nil {
			t.Fatalf("SyncOnce pass %d: %v", pass, err)
		}
		if report.TasksCreated != 0 {
			t.Fatalf("pass %d created %d tasks, want 0", pass, report.TasksCreated)
		}
	}
	items, err := domain.ListWorkItems(ctx, tenantID, collab.WorkFilter{ModeID: superagent.DefaultModeID})
	if err != nil {
		t.Fatalf("list tickets: %v", err)
	}
	if len(items) != 1 || items[0].ID != item.ID {
		t.Fatalf("tickets after two import passes = %+v, want only %s", items, item.ID)
	}
	tasks, err := board.Tasks(ctx, tenantID, boardplugin.TaskQuery{Channel: "ops"})
	if err != nil {
		t.Fatalf("list board tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("board tasks = %d, want 1 (no duplicate projection)", len(tasks))
	}
	if link, found, err := domain.GetBoardLinkByTask(ctx, tenantID, superagentplugin.BoardSource, superagentstore.BoardLinkTicket, tasks[0].ID); err != nil || !found || link.SourceID != item.ID.String() {
		t.Fatalf("board link for projected task = %+v found=%v err=%v", link, found, err)
	}
}

type e2eLauncher struct {
	mu   sync.Mutex
	last collab.LaunchRequest
}

func (l *e2eLauncher) Launch(_ context.Context, req collab.LaunchRequest) (collab.SessionRef, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.last = req
	return collab.SessionRef{
		TenantID: req.TenantID, SessionID: uuid.New(), ParentSessionID: req.ParentSessionID,
		BotID: req.BotID, ModeID: req.ModeID, WorkItemID: req.WorkItemID,
		Depth: req.Depth, State: collab.SessionActive, CreatedAt: time.Now().UTC(),
	}, nil
}

func (l *e2eLauncher) CancelSession(context.Context, uuid.UUID, uuid.UUID, string) error { return nil }

func (l *e2eLauncher) lastLaunch() collab.LaunchRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

type e2ePermissions struct{}

func (e2ePermissions) CheckBotInvoke(context.Context, uuid.UUID, collab.SessionRef, string) error {
	return nil
}

type e2eAudit struct{}

func (e2eAudit) Record(context.Context, collab.AuditEvent) error { return nil }

// TestImportedBoardTaskDispatchesWorkerSession is the P1-2/P2-5 end-to-end
// path: a board task is imported with WorkerBot, the normal Host dispatch binds
// an active worker session whose Bot is that WorkerBot, and the production
// Writer loop runs twice without re-importing its own projection.
func TestImportedBoardTaskDispatchesWorkerSession(t *testing.T) {
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
		TicketID: "board-task-worker", Channel: "ops", Title: "Dispatch me",
		Request: "Create one worker session.", Actor: boardplugin.ServiceActor(tenantID),
	})
	if err != nil {
		t.Fatalf("board task: %v", err)
	}

	registry, err := collab.NewModeRegistry(tenantID, collab.Limits{
		Depth: collab.LimitedDepth(8), MaxChildren: 8, MaxConcurrent: 8,
		MaxTurns: 100, MaxTokens: 100_000, MaxDurationMs: 600_000,
	})
	if err != nil {
		t.Fatalf("mode registry: %v", err)
	}
	if err := registry.Register(collab.SuperagentMode()); err != nil {
		t.Fatalf("register mode: %v", err)
	}
	if err := registry.SetEnabled(collab.ModeSuperagent, true); err != nil {
		t.Fatalf("enable mode: %v", err)
	}
	launcher := &e2eLauncher{}
	host, err := collab.NewHost(collab.HostOptions{
		TenantID: tenantID, Modes: registry, Work: domain, Sessions: domain,
		Permissions: e2ePermissions{}, Audit: e2eAudit{}, Launcher: launcher,
		Now: clock,
	})
	if err != nil {
		t.Fatalf("collab host: %v", err)
	}
	orchestratorSession := uuid.New()
	if err := domain.Put(ctx, collab.SessionRef{
		TenantID: tenantID, SessionID: orchestratorSession, BotID: "orchestrator",
		State: collab.SessionActive, CreatedAt: clock(),
	}); err != nil {
		t.Fatalf("orchestrator session: %v", err)
	}
	orchestrator, err := superagent.NewOrchestrator(superagent.Config{
		TenantID: tenantID, ModeID: collab.ModeSuperagent, Store: domain, Host: host,
		Policy:   &superagent.AllowlistPolicy{WorkerBot: "worker-bot", PolicyVersion: "e2e"},
		Sessions: domain, Audit: e2eAudit{},
		OrchestratorSession: orchestratorSession, OrchestratorBot: "orchestrator",
		WorkerBot: "worker-bot", Now: clock,
	})
	if err != nil {
		t.Fatalf("orchestrator: %v", err)
	}

	importOptions := superagentplugin.ImportOptions{
		Store: domain, Links: domain, TenantID: tenantID, WorkerBot: "worker-bot",
		OwnerSessionID: orchestratorSession, Now: clock,
		Dispatch: func(ctx context.Context, ticketID uuid.UUID) error {
			_, err := orchestrator.DispatchTicket(ctx, tenantID, ticketID)
			return err
		},
	}
	if _, created, err := superagentplugin.ImportBoardTask(ctx, importOptions, task); err != nil || !created {
		t.Fatalf("import: created=%v err=%v", created, err)
	}
	ticketRef, found, err := domain.GetBoardLinkByTask(ctx, tenantID, superagentplugin.BoardSource, superagentstore.BoardLinkTicket, task.ID)
	if err != nil || !found {
		t.Fatalf("ticket link: found=%v err=%v", found, err)
	}
	ticketID, err := uuid.Parse(ticketRef.SourceID)
	if err != nil {
		t.Fatalf("ticket id: %v", err)
	}
	assignments, err := domain.ListAssignments(ctx, tenantID, ticketID)
	if err != nil || len(assignments) != 1 {
		t.Fatalf("assignments = %+v err=%v, want one", assignments, err)
	}
	assignment := assignments[0]
	if assignment.SessionID == uuid.Nil || assignment.Status != collab.AssignmentActive {
		t.Fatalf("assignment = %+v, want an active worker session", assignment)
	}
	if assignment.BotID != "worker-bot" {
		t.Fatalf("assignment BotID = %q, want worker-bot", assignment.BotID)
	}
	if launch := launcher.lastLaunch(); launch.BotID != "worker-bot" || launch.WorkItemID != ticketID {
		t.Fatalf("launch = %+v, want worker-bot on %s", launch, ticketID)
	}

	tokens, err := superagentplugin.NewTokenSource("test-secret")
	if err != nil {
		t.Fatalf("token source: %v", err)
	}
	writer, err := superagentplugin.NewWriter(&superagentplugin.Writer{
		TenantID: tenantID, Store: domain, Board: board, Links: domain,
		Config: superagentplugin.StaticConfigReader{Config: superagentplugin.SuperagentConfig{
			Mirror: true, Channel: "ops",
		}},
		Secret: tokens, Now: clock,
		Import: func(ctx context.Context) error {
			_, err := superagentplugin.ImportChannel(ctx, board, importOptions, "ops", 200)
			return err
		},
	})
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	for pass := 1; pass <= 2; pass++ {
		report, err := writer.SyncOnce(ctx)
		if err != nil {
			t.Fatalf("SyncOnce pass %d: %v", pass, err)
		}
		if report.Claims != 1 && pass == 1 {
			t.Fatalf("first pass report = %+v, want the assignment mirrored as one claim", report)
		}
	}
	claimed, err := board.Task(ctx, tenantID, task.ID)
	if err != nil {
		t.Fatalf("board task: %v", err)
	}
	if claimed.State != boardplugin.StateClaimed {
		t.Fatalf("board task state = %s, want claimed", claimed.State)
	}
	if items, err := domain.ListWorkItems(ctx, tenantID, collab.WorkFilter{ModeID: superagent.DefaultModeID}); err != nil || len(items) != 1 {
		t.Fatalf("tickets = %d err=%v, want one (no re-import)", len(items), err)
	}
}
