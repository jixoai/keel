package superagent_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/superagent"
	"github.com/jixoai/keel/internal/superagentstore"
)

// TestIssueUpdateSnapshotIsDurableAcrossRestart is the P1 regression for the
// live-GitHub echo: the "already delivered" baseline must survive a process
// restart, so a comment bump after a real edit does not re-deliver the edited
// body. It uses the real PostgreSQL store (skipped without KEEL_TEST_DSN) and
// reads the snapshot through a second Store handle.
func TestIssueUpdateSnapshotIsDurableAcrossRestart(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	tenant := uuid.New()
	ownerSession := uuid.New()

	openStore := func() *store.Store {
		t.Helper()
		st, err := store.Open(ctx, store.Config{DSN: dsn})
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		return st
	}
	build := func(st *store.Store) (*superagent.Orchestrator, *recordingSink, *superagent.FixtureSource) {
		t.Helper()
		domain := superagentstore.New(st.Pool())
		registry := e2eRegistry(t, tenant)
		source := superagent.NewFixtureSource(func() time.Time { return time.Now().UTC() })
		outbox, err := superagent.NewOutbox(superagent.OutboxOptions{Store: domain, Channel: source, Audit: &fakeAudit{}})
		if err != nil {
			t.Fatalf("NewOutbox: %v", err)
		}
		host := e2eHost(t, domain, registry, outbox)
		inputs := &recordingSink{}
		orch, err := superagent.NewOrchestrator(superagent.Config{
			TenantID: tenant, ModeID: collab.ModeSuperagent, Store: domain, Host: host,
			Source: source, Channel: source, Inputs: inputs, Sessions: domain,
			Policy: &superagent.AllowlistPolicy{Actors: map[string]bool{"alice": true}, WorkerBot: "worker", PolicyVersion: "pg-snapshot-v1"},
			Audit:  &fakeAudit{}, Outbox: outbox, Operator: allowOperator{},
			OrchestratorSession: ownerSession, OrchestratorBot: "orchestrator",
			WorkerBot: "worker", MaxPoll: 20,
		})
		if err != nil {
			t.Fatalf("NewOrchestrator: %v", err)
		}
		return orch, inputs, source
	}

	// First process: accept the ticket and deliver one real edit.
	st1 := openStore()
	d1 := superagentstore.New(st1.Pool())
	if err := st1.CreateTenant(ctx, tenant, "snapshot-"+tenant.String()[:8]); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if err := d1.Put(ctx, collab.SessionRef{
		TenantID: tenant, SessionID: ownerSession, BotID: "orchestrator",
		State: collab.SessionActive, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Put owner: %v", err)
	}
	orch1, sink1, source1 := build(st1)
	if err := source1.PushEvent(tenant, issueEvent("e1", "issue#1")); err != nil {
		t.Fatalf("push accept: %v", err)
	}
	if _, err := orch1.PollOnce(ctx); err != nil {
		t.Fatalf("accept poll: %v", err)
	}
	edited := superagent.Event{
		SourceID: "fixture", EventID: "e2", Kind: superagent.EventIssueUpdated,
		Actor: alice(), Thread: superagent.ThreadRef{Source: "fixture", Kind: "issue", ID: "issue#1"},
		Title: "Fix issue#1", Body: "Please fix it. Also update the tests.",
	}
	if err := source1.PushEvent(tenant, edited); err != nil {
		t.Fatalf("push edit: %v", err)
	}
	if _, err := orch1.PollOnce(ctx); err != nil {
		t.Fatalf("edit poll: %v", err)
	}
	if sink1.count() != 1 {
		t.Fatalf("first process deliveries = %d, want 1", sink1.count())
	}
	st1.Close()

	// Second process: same database, fresh Store/Orchestrator. The cursor is
	// reset so the new in-memory fixture starts at zero; the durable seen-event
	// log makes any replay harmless and the new event ids are what matter.
	st2 := openStore()
	d2 := superagentstore.New(st2.Pool())
	t.Cleanup(st2.Close)
	if err := d2.SetSourceCursor(ctx, tenant, "fixture", ""); err != nil {
		t.Fatalf("reset cursor: %v", err)
	}
	orch2, sink2, source2 := build(st2)

	bump := edited
	bump.EventID = "e3"
	if err := source2.PushEvent(tenant, bump); err != nil {
		t.Fatalf("push bump: %v", err)
	}
	if _, err := orch2.PollOnce(ctx); err != nil {
		t.Fatalf("bump poll: %v", err)
	}
	if sink2.count() != 0 {
		t.Fatalf("restart deliveries = %d, want 0: the edited body was re-delivered from a lost snapshot", sink2.count())
	}

	// A real title edit after the restart is still delivered exactly once.
	titled := bump
	titled.EventID = "e4"
	titled.Title = "Fix issue#1 (P1)"
	if err := source2.PushEvent(tenant, titled); err != nil {
		t.Fatalf("push title edit: %v", err)
	}
	if _, err := orch2.PollOnce(ctx); err != nil {
		t.Fatalf("title poll: %v", err)
	}
	if sink2.count() != 1 {
		t.Fatalf("restart deliveries after a title edit = %d, want 1", sink2.count())
	}

	items, err := d2.ListWorkItems(ctx, tenant, collab.WorkFilter{ModeID: collab.ModeSuperagent})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("work items = %d, want 1", len(items))
	}
	delivered := superagent.TicketFromWorkItem(items[0]).LastDelivered()
	if delivered.Title != "Fix issue#1 (P1)" || delivered.Body != "Please fix it. Also update the tests." {
		t.Fatalf("persisted snapshot = %+v", delivered)
	}
}

// TestPendingFollowUpSurvivesRestart is the P1 durability check: a follow-up
// whose dispatch failed is stored in the Ticket document, survives a process
// restart (new Store + new Orchestrator), and is delivered by housekeeping
// before the snapshot advances.
func TestPendingFollowUpSurvivesRestart(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	tenant := uuid.New()
	ownerSession := uuid.New()

	openStore := func() *store.Store {
		t.Helper()
		st, err := store.Open(ctx, store.Config{DSN: dsn})
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		return st
	}
	build := func(st *store.Store, launcher *fakeLauncher, now func() time.Time) (*superagent.Orchestrator, *recordingSink, *superagent.FixtureSource) {
		t.Helper()
		domain := superagentstore.New(st.Pool())
		registry := e2eRegistry(t, tenant)
		source := superagent.NewFixtureSource(now)
		outbox, err := superagent.NewOutbox(superagent.OutboxOptions{Store: domain, Channel: source, Audit: &fakeAudit{}})
		if err != nil {
			t.Fatalf("NewOutbox: %v", err)
		}
		host, err := collab.NewHost(collab.HostOptions{
			TenantID: tenant, Modes: registry, Work: domain, Sessions: domain,
			Permissions: fakePermissions{}, Audit: &fakeAudit{}, Launcher: launcher,
			Attention: outbox,
		})
		if err != nil {
			t.Fatalf("NewHost: %v", err)
		}
		inputs := &recordingSink{}
		orch, err := superagent.NewOrchestrator(superagent.Config{
			TenantID: tenant, ModeID: collab.ModeSuperagent, Store: domain, Host: host,
			Source: source, Channel: source, Inputs: inputs, Sessions: domain,
			Policy: &superagent.AllowlistPolicy{Actors: map[string]bool{"alice": true}, WorkerBot: "worker", PolicyVersion: "pg-pending-v1"},
			Audit:  &fakeAudit{}, Outbox: outbox, Operator: allowOperator{},
			OrchestratorSession: ownerSession, OrchestratorBot: "orchestrator",
			WorkerBot: "worker", MaxPoll: 20, Now: now,
		})
		if err != nil {
			t.Fatalf("NewOrchestrator: %v", err)
		}
		return orch, inputs, source
	}

	st1 := openStore()
	d1 := superagentstore.New(st1.Pool())
	if err := st1.CreateTenant(ctx, tenant, "pending-"+tenant.String()[:8]); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if err := d1.Put(ctx, collab.SessionRef{
		TenantID: tenant, SessionID: ownerSession, BotID: "orchestrator",
		State: collab.SessionActive, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Put owner: %v", err)
	}
	launcher1 := &fakeLauncher{}
	orch1, _, source1 := build(st1, launcher1, func() time.Time { return time.Now().UTC() })
	if err := source1.PushEvent(tenant, issueEvent("e1", "issue#1")); err != nil {
		t.Fatalf("push accept: %v", err)
	}
	if _, err := orch1.PollOnce(ctx); err != nil {
		t.Fatalf("accept poll: %v", err)
	}
	board, _ := orch1.Board(ctx, tenant, superagent.BoardFilter{})
	ticket := board.Tickets[0]
	attention, err := orch1.Ask(ctx, ticket.ID, ticket.ActiveAssignment.ID, collab.AttentionRequest{Prompt: "Approve?", AllowFreeText: true})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if err := orch1.Report(ctx, superagent.ReportRequest{
		TenantID: tenant, WorkItemID: ticket.ID, AssignmentID: ticket.ActiveAssignment.ID,
		State: superagent.ReportWaiting, Summary: "waiting", AttentionID: attention.ID,
	}); err != nil {
		t.Fatalf("Report waiting: %v", err)
	}
	assignment := *ticket.ActiveAssignment
	assignment.Status = collab.AssignmentFailed
	if _, err := d1.UpdateAssignment(ctx, assignment); err != nil {
		t.Fatalf("fail assignment: %v", err)
	}
	launcher1.launchErr = errors.New("launch down")
	edited := superagent.Event{
		SourceID: "fixture", EventID: "u1", Kind: superagent.EventIssueUpdated,
		Actor: alice(), Thread: superagent.ThreadRef{Source: "fixture", Kind: "issue", ID: "issue#1"},
		Title: "Fix issue#1", Body: "Please fix it. Also update the tests.",
	}
	if _, err := orch1.HandleSourceEvent(ctx, fixtureEvent(t, edited)); err != nil {
		t.Fatalf("failed dispatch must not fail the page: %v", err)
	}
	items, err := d1.ListWorkItems(ctx, tenant, collab.WorkFilter{ModeID: collab.ModeSuperagent})
	if err != nil || len(items) != 1 {
		t.Fatalf("work items = %d, err=%v", len(items), err)
	}
	stored := superagent.TicketFromWorkItem(items[0])
	if len(stored.Pending) != 1 || stored.Delivered == nil || stored.Delivered.Body != "Please fix it." {
		t.Fatalf("stored pending/snapshot = %+v / %+v", stored.Pending, stored.Delivered)
	}
	st1.Close()

	// Restart: new store handle, new orchestrator, launcher recovered. The
	// pending entry must still be in the ticket document and deliverable.
	st2 := openStore()
	d2 := superagentstore.New(st2.Pool())
	t.Cleanup(st2.Close)
	later := time.Now().UTC().Add(2 * time.Second)
	launcher2 := &fakeLauncher{}
	orch2, _, _ := build(st2, launcher2, func() time.Time { return later })
	if err := orch2.Housekeeping(ctx); err != nil {
		t.Fatalf("housekeeping after restart: %v", err)
	}
	if launch := launcher2.lastLaunch(); !strings.Contains(launch.Task, "Also update the tests") {
		t.Fatalf("restart delivery task = %q, want the pending text", launch.Task)
	}
	items, err = d2.ListWorkItems(ctx, tenant, collab.WorkFilter{ModeID: collab.ModeSuperagent})
	if err != nil || len(items) != 1 {
		t.Fatalf("work items after restart = %d, err=%v", len(items), err)
	}
	delivered := superagent.TicketFromWorkItem(items[0])
	if len(delivered.Pending) != 0 || delivered.Delivered == nil || delivered.Delivered.Body != "Please fix it. Also update the tests." {
		t.Fatalf("pending after restart delivery = %+v, snapshot = %+v", delivered.Pending, delivered.Delivered)
	}
}
