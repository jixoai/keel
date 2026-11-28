package superagent_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/superagent"
	"github.com/jixoai/keel/internal/superagentstore"
)

// openPgStore is the shared real-PostgreSQL handle for the durability tests.
func openPgStore(t *testing.T, ctx context.Context, dsn string) *store.Store {
	t.Helper()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// TestAnswerProtocolIsDurableInPostgres runs the repaired answer protocol
// against the real store: an Attention answered without delivery evidence is
// re-armed and delivered by a retry, and the receipt is visible through a
// second Store handle (a separate process). It is not skipped when
// KEEL_TEST_DSN is set.
func TestAnswerProtocolIsDurableInPostgres(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	tenant := uuid.New()
	ownerSession := uuid.New()
	st := openPgStore(t, ctx, dsn)
	domain := superagentstore.New(st.Pool())
	if err := st.CreateTenant(ctx, tenant, "answer-"+tenant.String()[:8]); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if err := domain.Put(ctx, collab.SessionRef{
		TenantID: tenant, SessionID: ownerSession, BotID: "orchestrator",
		State: collab.SessionActive, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Put owner: %v", err)
	}
	registry := e2eRegistry(t, tenant)
	source := superagent.NewFixtureSource(func() time.Time { return time.Now().UTC() })
	outbox, err := superagent.NewOutbox(superagent.OutboxOptions{Store: domain, Channel: source, Audit: &fakeAudit{}})
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	host := e2eHost(t, domain, registry, outbox)
	sink := &recordingSink{}
	orch, err := superagent.NewOrchestrator(superagent.Config{
		TenantID: tenant, ModeID: collab.ModeSuperagent, Store: domain, Host: host,
		Source: source, Channel: source, Inputs: sink, Sessions: domain,
		Policy: &superagent.AllowlistPolicy{Actors: map[string]bool{"alice": true}, WorkerBot: "worker", PolicyVersion: "pg-answer-v1"},
		Audit:  &fakeAudit{}, Outbox: outbox, Operator: allowOperator{},
		OrchestratorSession: ownerSession, OrchestratorBot: "orchestrator",
		WorkerBot: "worker", MaxPoll: 20,
	})
	if err != nil {
		t.Fatalf("NewOrchestrator: %v", err)
	}
	if err := source.PushEvent(tenant, issueEvent("e1", "issue#1")); err != nil {
		t.Fatalf("push accept: %v", err)
	}
	if _, err := orch.PollOnce(ctx); err != nil {
		t.Fatalf("accept poll: %v", err)
	}
	board, err := orch.Board(ctx, tenant, superagent.BoardFilter{})
	if err != nil || len(board.Tickets) != 1 || board.Tickets[0].ActiveAssignment == nil {
		t.Fatalf("board = %+v err=%v", board, err)
	}
	ticket := board.Tickets[0]
	attention, err := orch.Ask(ctx, ticket.ID, ticket.ActiveAssignment.ID, collab.AttentionRequest{
		Prompt: "Approve?", Options: []collab.AttentionOption{{ID: "approve"}},
	})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if err := orch.Report(ctx, superagent.ReportRequest{
		TenantID: tenant, WorkItemID: ticket.ID, AssignmentID: ticket.ActiveAssignment.ID,
		State: superagent.ReportWaiting, Summary: "waiting", AttentionID: attention.ID,
	}); err != nil {
		t.Fatalf("report waiting: %v", err)
	}

	// Simulate the old crash window: the Attention row is answered through the
	// Host while the Ticket has no intent and no receipt.
	if _, err := host.AnswerAttention(ctx, tenant, ticket.ID, attention.ID, collab.AttentionAnswerRequest{
		OptionID: "approve", Actor: "alice",
	}); err != nil {
		t.Fatalf("direct attention answer: %v", err)
	}
	if _, err := orch.AnswerAttention(ctx, tenant, attention.ID, "approve", "", "alice"); err != nil {
		t.Fatalf("retry must deliver: %v", err)
	}
	if sink.count() != 1 {
		t.Fatalf("deliveries = %+v", sink.delivered)
	}

	// A second store handle is the restart/other-process read.
	other := openPgStore(t, ctx, dsn)
	otherDomain := superagentstore.New(other.Pool())
	item, err := otherDomain.GetWorkItem(ctx, tenant, ticket.ID)
	if err != nil {
		t.Fatalf("second handle read: %v", err)
	}
	stored := superagent.TicketFromWorkItem(item)
	if len(stored.Pending) != 0 {
		t.Fatalf("pending after delivery = %+v", stored.Pending)
	}
	receipt := stored.AnswerReceiptFor(attention.ID)
	if receipt == nil || receipt.DeliveredAt.IsZero() || !receipt.Delivered() {
		t.Fatalf("receipt = %+v, want durable delivery evidence", receipt)
	}
	answered, err := otherDomain.ListAttentionsByStatus(ctx, tenant, collab.ModeSuperagent, 10, collab.AttentionAnswered)
	if err != nil {
		t.Fatalf("list answered attentions: %v", err)
	}
	found := false
	for _, row := range answered {
		if row.ID == attention.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("answered attention %s not listed: %+v", attention.ID, answered)
	}
}

// TestWriterLeaseRejectsASecondProcess proves the single-writer guard with two
// independent pools: the second lease is refused until the first is released.
func TestWriterLeaseRejectsASecondProcess(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	tenant := uuid.New()
	first := openPgStore(t, ctx, dsn)
	second := openPgStore(t, ctx, dsn)
	firstDomain := superagentstore.New(first.Pool())
	secondDomain := superagentstore.New(second.Pool())
	lease, err := firstDomain.AcquireWriterLease(ctx, tenant)
	if err != nil {
		t.Fatalf("first lease: %v", err)
	}
	if _, err := secondDomain.AcquireWriterLease(ctx, tenant); !errors.Is(err, superagent.ErrWriterLeaseHeld) {
		t.Fatalf("second lease = %v, want ErrWriterLeaseHeld", err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	next, err := secondDomain.AcquireWriterLease(ctx, tenant)
	if err != nil {
		t.Fatalf("lease after release: %v", err)
	}
	if err := next.Release(ctx); err != nil {
		t.Fatalf("second release: %v", err)
	}
}
