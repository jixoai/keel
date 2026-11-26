package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/superagent"
)

func TestCollabSuperagentSchemaAndRLS(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tables := []string{
		"collab_work_items", "collab_assignments", "collab_worker_reports", "collab_attentions", "collab_links",
		"collab_subscriptions", "collab_source_cursors", "collab_source_events",
		"collab_outbox", "collab_session_index",
	}
	for _, table := range tables {
		var enabled, forced bool
		if err := st.pool.QueryRow(ctx,
			`SELECT c.relrowsecurity, c.relforcerowsecurity
			   FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			  WHERE n.nspname = current_schema() AND c.relname = $1`, table).
			Scan(&enabled, &forced); err != nil {
			t.Fatalf("%s catalog row: %v", table, err)
		}
		if !enabled || !forced {
			t.Fatalf("%s row security: enabled=%v forced=%v", table, enabled, forced)
		}
		found := false
		for _, candidate := range rlsTenantTables {
			if candidate == table {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s is missing from rlsTenantTables", table)
		}
	}
}

func TestCollabSuperagentStoreRoundTrip(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	other := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "collab-"+tenant.String()[:8]); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	item := collab.WorkItem{
		ID: uuid.New(), TenantID: tenant, ModeID: collab.ModeSuperagent,
		Title: "Fix issue", Status: collab.WorkItemOpen, IdempotencyKey: "github/issue:1",
		Source: collab.SourceRef{Kind: "github", ID: "acme/repo#1"},
		Input:  []byte(`{"mandate":{"requester":{"login":"alice"}}}`),
	}
	created, err := st.CreateWorkItem(ctx, item)
	if err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}
	duplicate, err := st.CreateWorkItem(ctx, item)
	if err != nil {
		t.Fatalf("idempotent CreateWorkItem: %v", err)
	}
	if duplicate.ID != created.ID {
		t.Fatalf("duplicate id = %s, want %s", duplicate.ID, created.ID)
	}
	if _, err := st.GetWorkItem(ctx, other, created.ID); !errors.Is(err, collab.ErrWorkItemNotFound) {
		t.Fatalf("foreign GetWorkItem err = %v, want not found", err)
	}
	if _, err := st.GetWorkItem(ctx, tenant, created.ID); err != nil {
		t.Fatalf("GetWorkItem: %v", err)
	}

	assignment := collab.Assignment{
		ID: uuid.New(), TenantID: tenant, WorkItemID: created.ID, ModeID: collab.ModeSuperagent,
		BotID: "worker", Status: collab.AssignmentOpen, IdempotencyKey: "assignment:1",
	}
	if _, err := st.CreateAssignment(ctx, assignment); err != nil {
		t.Fatalf("CreateAssignment: %v", err)
	}
	if _, err := st.CreateAssignment(ctx, assignment); err != nil {
		t.Fatalf("idempotent CreateAssignment: %v", err)
	}
	assignments, err := st.ListAssignments(ctx, tenant, created.ID)
	if err != nil || len(assignments) != 1 {
		t.Fatalf("assignments = %d, err=%v", len(assignments), err)
	}

	attention := collab.Attention{
		ID: uuid.New(), TenantID: tenant, WorkItemID: created.ID, ModeID: collab.ModeSuperagent,
		RequestID: "q1", Kind: collab.AttentionDecision, Prompt: "Which?",
		Options: []collab.AttentionOption{{ID: "a", Label: "A"}},
		Route:   collab.AttentionRouteChannel, Channel: "acme/repo#1",
		Status: collab.AttentionOpen, Deadline: created.CreatedAt.Add(5 * time.Minute),
	}
	if _, err := st.CreateAttention(ctx, attention); err != nil {
		t.Fatalf("CreateAttention: %v", err)
	}
	if _, err := st.CreateAttention(ctx, attention); err != nil {
		t.Fatalf("idempotent CreateAttention: %v", err)
	}
	open, err := st.ListOpenAttentions(ctx, tenant)
	if err != nil || len(open) != 1 {
		t.Fatalf("open attentions = %d, err=%v", len(open), err)
	}

	subscription := superagent.Subscription{
		TenantID: tenant, WorkItemID: created.ID, Active: true,
		Thread: superagent.ThreadRef{Source: "github", Kind: "issue", ID: "acme/repo#1"},
	}
	if _, err := st.CreateSubscription(ctx, subscription); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	if _, err := st.CreateSubscription(ctx, subscription); err != nil {
		t.Fatalf("idempotent CreateSubscription: %v", err)
	}
	subs, err := st.ListThreadSubscriptions(ctx, tenant, subscription.Thread)
	if err != nil || len(subs) != 1 {
		t.Fatalf("subscriptions = %d, err=%v", len(subs), err)
	}

	if err := st.SetSourceCursor(ctx, tenant, "github", "2026-11-25T00:00:00Z"); err != nil {
		t.Fatalf("SetSourceCursor: %v", err)
	}
	cursor, err := st.GetSourceCursor(ctx, tenant, "github")
	if err != nil || cursor != "2026-11-25T00:00:00Z" {
		t.Fatalf("cursor = %q, err=%v", cursor, err)
	}
	if err := st.RecordSourceEvent(ctx, superagent.SeenEvent{
		TenantID: tenant, SourceID: "github", EventID: "issue:1", Decision: "accepted",
		WorkItemID: created.ID, CreatedAt: created.CreatedAt,
	}); err != nil {
		t.Fatalf("RecordSourceEvent: %v", err)
	}
	seen, err := st.SourceEventSeen(ctx, tenant, "github", "issue:1")
	if err != nil || !seen {
		t.Fatalf("seen = %v, err=%v", seen, err)
	}

	message := superagent.OutboxMessage{
		TenantID: tenant, WorkItemID: created.ID, ChannelID: "acme/repo#1",
		Text: "accepted", IdempotencyKey: "ticket:" + created.ID.String() + ":accepted",
	}
	if _, err := st.EnqueueOutbox(ctx, message); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}
	if _, err := st.EnqueueOutbox(ctx, message); err != nil {
		t.Fatalf("idempotent EnqueueOutbox: %v", err)
	}
	outbox, err := st.ListOutbox(ctx, tenant, superagent.OutboxPending, 10)
	if err != nil || len(outbox) != 1 {
		t.Fatalf("outbox = %d, err=%v", len(outbox), err)
	}

	sessionID := uuid.New()
	ref := collab.SessionRef{
		TenantID: tenant, SessionID: sessionID, ParentSessionID: uuid.New(),
		BotID: "worker", ModeID: collab.ModeSuperagent, WorkItemID: created.ID,
		Depth: 1, State: collab.SessionActive, CreatedAt: created.CreatedAt,
	}
	if err := st.Put(ctx, ref); err != nil {
		t.Fatalf("Put session: %v", err)
	}
	if err := st.Put(ctx, ref); !errors.Is(err, collab.ErrSessionExists) {
		t.Fatalf("duplicate Put err = %v, want session exists", err)
	}
	stored, err := st.Session(ctx, tenant, sessionID)
	if err != nil || stored.WorkItemID != created.ID || stored.Depth != 1 {
		t.Fatalf("Session = %+v, err=%v", stored, err)
	}
	if err := st.SetState(ctx, tenant, sessionID, collab.SessionDone); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	children, err := st.ActiveChildren(ctx, tenant, ref.ParentSessionID)
	if err != nil || children != 0 {
		t.Fatalf("active children = %d, err=%v", children, err)
	}

	report := superagent.WorkerReport{
		ID: uuid.New(), TenantID: tenant, WorkItemID: created.ID, AssignmentID: assignment.ID,
		SessionID: sessionID, RequestID: "report:1", State: superagent.ReportProgress,
		Summary: "halfway", CreatedAt: created.CreatedAt,
	}
	storedReport, createdReport, err := st.RecordWorkerReport(ctx, report)
	if err != nil || !createdReport || storedReport.ID != report.ID {
		t.Fatalf("RecordWorkerReport = %+v created=%v err=%v", storedReport, createdReport, err)
	}
	duplicateReport, createdReport, err := st.RecordWorkerReport(ctx, report)
	if err != nil || createdReport || duplicateReport.ID != report.ID {
		t.Fatalf("idempotent RecordWorkerReport = %+v created=%v err=%v", duplicateReport, createdReport, err)
	}
	loadedReport, err := st.GetWorkerReport(ctx, tenant, created.ID, "report:1")
	if err != nil || loadedReport.State != superagent.ReportProgress || loadedReport.SessionID != sessionID {
		t.Fatalf("GetWorkerReport = %+v err=%v", loadedReport, err)
	}
	if _, err := st.GetWorkerReport(ctx, other, created.ID, "report:1"); !errors.Is(err, superagent.ErrReportNotFound) {
		t.Fatalf("foreign GetWorkerReport err = %v, want report not found", err)
	}
}

type pgFakeLauncher struct{}

func (pgFakeLauncher) Launch(_ context.Context, req collab.LaunchRequest) (collab.SessionRef, error) {
	return collab.SessionRef{
		TenantID: req.TenantID, SessionID: uuid.New(), ParentSessionID: req.ParentSessionID,
		BotID: req.BotID, ModeID: req.ModeID, WorkItemID: req.WorkItemID, Depth: req.Depth,
		State: collab.SessionActive, CreatedAt: time.Now().UTC(),
	}, nil
}

func (pgFakeLauncher) CancelSession(context.Context, uuid.UUID, uuid.UUID, string) error { return nil }

type pgFakePermissions struct{}

func (pgFakePermissions) CheckBotInvoke(context.Context, uuid.UUID, collab.SessionRef, string) error {
	return nil
}

type pgFakeAudit struct{}

func (pgFakeAudit) Record(context.Context, collab.AuditEvent) error { return nil }

func pgModeRegistry(t *testing.T, tenant uuid.UUID) *collab.ModeRegistry {
	t.Helper()
	registry, err := collab.NewModeRegistry(tenant, collab.Limits{
		Depth: collab.LimitedDepth(8), MaxChildren: 32, MaxConcurrent: 16,
		MaxTurns: 100, MaxTokens: 100_000, MaxDurationMs: 3_600_000,
	})
	if err != nil {
		t.Fatalf("NewModeRegistry: %v", err)
	}
	if err := registry.Register(collab.SuperagentMode()); err != nil {
		t.Fatalf("Register superagent: %v", err)
	}
	if err := registry.SetEnabled(collab.ModeSuperagent, true); err != nil {
		t.Fatalf("enable superagent: %v", err)
	}
	return registry
}

func pgHost(t *testing.T, st *Store, registry *collab.ModeRegistry) *collab.Coordinator {
	t.Helper()
	host, err := collab.NewHost(collab.HostOptions{
		TenantID: registry.TenantID(), Modes: registry, Work: st, Sessions: st,
		Permissions: pgFakePermissions{}, Audit: pgFakeAudit{}, Launcher: pgFakeLauncher{},
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	return host
}

// TestCollabOwnerSessionSurvivesPGRestart pins P0-1: OwnerSessionID is a
// durable WorkItem column, so the owner can Assign after a store reopen. A
// memory-only field would have made the first PG Assign permission-denied.
func TestCollabOwnerSessionSurvivesPGRestart(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "owner-"+tenant.String()[:8]); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	registry := pgModeRegistry(t, tenant)
	owner := collab.SessionRef{
		TenantID: tenant, SessionID: uuid.New(), BotID: "orchestrator",
		State: collab.SessionActive, CreatedAt: time.Now().UTC(),
	}
	if err := st.Put(ctx, owner); err != nil {
		t.Fatalf("Put owner: %v", err)
	}
	item := collab.WorkItem{
		ID: uuid.New(), TenantID: tenant, ModeID: collab.ModeSuperagent,
		Title: "owner membership", Status: collab.WorkItemOpen, OwnerSessionID: owner.SessionID,
		IdempotencyKey: "owner-pg", CreatedAt: time.Now().UTC(),
	}
	if _, err := st.CreateWorkItem(ctx, item); err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}
	host := pgHost(t, st, registry)
	if _, err := host.Assign(ctx, tenant, item.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: owner.SessionID, IdempotencyKey: "owner-assign-1",
	}); err != nil {
		t.Fatalf("first Assign: %v", err)
	}

	// Reopen the store (same database) and re-read the work item. The owner is
	// still the persisted membership bootstrap.
	dsn := os.Getenv("KEEL_TEST_DSN")
	reopened, err := Open(ctx, Config{DSN: dsn})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(reopened.Close)
	stored, err := reopened.GetWorkItem(ctx, tenant, item.ID)
	if err != nil {
		t.Fatalf("GetWorkItem after reopen: %v", err)
	}
	if stored.OwnerSessionID != owner.SessionID {
		t.Fatalf("owner session = %s, want %s", stored.OwnerSessionID, owner.SessionID)
	}
	host2 := pgHost(t, reopened, registry)
	if _, err := host2.Assign(ctx, tenant, item.ID, "worker", collab.AssignmentRequest{
		CallerSessionID: owner.SessionID, IdempotencyKey: "owner-assign-2",
	}); err != nil {
		t.Fatalf("Assign after reopen: %v", err)
	}
}

// TestCollabRestartInterruptsStaleWorkerAndRedispatch pins P0-2: boot marks the
// previous process's active worker rows cancelled, and Recover fails the old
// assignment with a recovery note, dispatches a new one and releases the slot.
func TestCollabRestartInterruptsStaleWorkerAndRedispatch(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "restart-"+tenant.String()[:8]); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	registry := pgModeRegistry(t, tenant)
	owner := collab.SessionRef{
		TenantID: tenant, SessionID: uuid.New(), BotID: "orchestrator",
		State: collab.SessionActive, CreatedAt: time.Now().UTC(),
	}
	if err := st.Put(ctx, owner); err != nil {
		t.Fatalf("Put owner: %v", err)
	}
	item := collab.WorkItem{
		ID: uuid.New(), TenantID: tenant, ModeID: collab.ModeSuperagent,
		Title: "restart ticket", Status: collab.WorkItemActive, OwnerSessionID: owner.SessionID,
		IdempotencyKey: "restart-pg", CreatedAt: time.Now().UTC(),
	}
	if _, err := st.CreateWorkItem(ctx, item); err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}
	workerID := uuid.New()
	if err := st.Put(ctx, collab.SessionRef{
		TenantID: tenant, SessionID: workerID, ParentSessionID: owner.SessionID,
		BotID: "worker", ModeID: collab.ModeSuperagent, WorkItemID: item.ID,
		Depth: 1, State: collab.SessionActive, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Put worker: %v", err)
	}
	old, err := st.CreateAssignment(ctx, collab.Assignment{
		ID: uuid.New(), TenantID: tenant, WorkItemID: item.ID, ModeID: collab.ModeSuperagent,
		BotID: "worker", SessionID: workerID, CallerSessionID: owner.SessionID,
		Status: collab.AssignmentActive, IdempotencyKey: "old-assignment",
	})
	if err != nil {
		t.Fatalf("CreateAssignment: %v", err)
	}
	interrupted, err := st.InterruptActiveCollabSessions(ctx, tenant, collab.ModeSuperagent, owner.SessionID)
	if err != nil || interrupted != 1 {
		t.Fatalf("InterruptActiveCollabSessions = %d, err=%v", interrupted, err)
	}
	host := pgHost(t, st, registry)
	orchestrator, err := superagent.NewOrchestrator(superagent.Config{
		TenantID: tenant, Store: st, Host: host, Policy: &superagent.AllowlistPolicy{},
		Inputs: superagent.NoopInputSink{}, Sessions: st, Audit: pgFakeAudit{},
		OrchestratorSession: owner.SessionID, OrchestratorBot: "orchestrator", WorkerBot: "worker",
	})
	if err != nil {
		t.Fatalf("NewOrchestrator: %v", err)
	}
	if _, err := orchestrator.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	assignments, err := st.ListAssignments(ctx, tenant, item.ID)
	if err != nil {
		t.Fatalf("ListAssignments: %v", err)
	}
	var oldRow, replacement *collab.Assignment
	for i := range assignments {
		if assignments[i].ID == old.ID {
			oldRow = &assignments[i]
		} else {
			replacement = &assignments[i]
		}
	}
	if oldRow == nil || oldRow.Status != collab.AssignmentFailed || oldRow.Error == "" {
		t.Fatalf("old assignment = %+v, want failed with recovery note", oldRow)
	}
	if replacement == nil || replacement.Status != collab.AssignmentActive {
		t.Fatalf("replacement = %+v, want active", replacement)
	}
	active, err := st.ListAssignmentsByStatus(ctx, tenant, collab.AssignmentActive)
	if err != nil || len(active) != 1 {
		t.Fatalf("active slots = %d, err=%v", len(active), err)
	}
}
