package store

import (
	"context"
	"errors"
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
		"collab_work_items", "collab_assignments", "collab_attentions", "collab_links",
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
}
