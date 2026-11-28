package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/collab"
)

func newCollabP2WorkItem(t *testing.T, st *Store, tenant uuid.UUID, title string) collab.WorkItem {
	t.Helper()
	item := collab.WorkItem{
		ID: uuid.New(), TenantID: tenant, ModeID: collab.ModeSuperagent,
		Title: title, Status: collab.WorkItemOpen,
	}
	stored, err := st.CreateWorkItem(context.Background(), item)
	if err != nil {
		t.Fatalf("CreateWorkItem(%s): %v", title, err)
	}
	return stored
}

// TestCollabAssignmentBySessionAcrossWorkItems pins the durable half of the
// rebinding guard: one session's assignments are visible across WorkItems, and
// another tenant sees none.
func TestCollabAssignmentBySessionAcrossWorkItems(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "p2-assign-"+tenant.String()[:8]); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	first := newCollabP2WorkItem(t, st, tenant, "first")
	second := newCollabP2WorkItem(t, st, tenant, "second")
	worker := uuid.New()
	now := time.Now().UTC()
	for index, item := range []collab.WorkItem{first, second} {
		if _, err := st.CreateAssignment(ctx, collab.Assignment{
			ID: uuid.New(), TenantID: tenant, WorkItemID: item.ID, ModeID: item.ModeID,
			BotID: "worker", SessionID: worker, CallerSessionID: uuid.New(),
			Status: collab.AssignmentActive, Title: "binding", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreateAssignment %d: %v", index, err)
		}
	}
	assignments, err := st.ListAssignmentsBySession(ctx, tenant, worker)
	if err != nil || len(assignments) != 2 {
		t.Fatalf("ListAssignmentsBySession = %d (%v), want 2", len(assignments), err)
	}
	if assignments[0].WorkItemID == assignments[1].WorkItemID {
		t.Fatalf("assignments = %+v, want one per WorkItem", assignments)
	}
	if foreign, err := st.ListAssignmentsBySession(ctx, uuid.New(), worker); err != nil || len(foreign) != 0 {
		t.Fatalf("foreign tenant assignments = %+v, %v, want none", foreign, err)
	}
}

// TestCollabLinkNaturalDedupe pins the PG half of AddLink: the deterministic
// relation is stored once, readable from either WorkItem endpoint, and an
// explicit id naming a different relation is refused.
func TestCollabLinkNaturalDedupe(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "p2-link-"+tenant.String()[:8]); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	first := newCollabP2WorkItem(t, st, tenant, "first")
	second := newCollabP2WorkItem(t, st, tenant, "second")
	link := collab.Link{
		ID:        uuid.New(),
		TenantID:  tenant,
		Kind:      collab.LinkReview,
		From:      collab.LinkEndpoint{Type: collab.LinkEndpointWorkItem, ID: first.ID.String()},
		To:        collab.LinkEndpoint{Type: collab.LinkEndpointWorkItem, ID: second.ID.String()},
		CreatedAt: time.Now().UTC(),
	}
	stored, err := st.AddLink(ctx, link)
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}
	// The same natural relation under another id resolves to the first row.
	again, err := st.AddLink(ctx, collab.Link{
		ID: uuid.New(), TenantID: tenant, Kind: collab.LinkReview,
		From: link.From, To: link.To, CreatedAt: time.Now().UTC(),
	})
	if err != nil || again.ID != stored.ID {
		t.Fatalf("dedupe = %+v, %v, want %s", again, err, stored.ID)
	}
	for _, item := range []collab.WorkItem{first, second} {
		links, err := st.ListLinks(ctx, tenant, item.ID)
		if err != nil || len(links) != 1 || links[0].ID != stored.ID {
			t.Fatalf("ListLinks(%s) = %+v, %v", item.ID, links, err)
		}
	}
	if got, err := st.GetLink(ctx, tenant, stored.ID); err != nil || got.ID != stored.ID {
		t.Fatalf("GetLink = %+v, %v", got, err)
	}
	if _, err := st.GetLink(ctx, uuid.New(), stored.ID); !errors.Is(err, collab.ErrLinkNotFound) {
		t.Fatalf("foreign GetLink = %v, want ErrLinkNotFound", err)
	}
	if _, err := st.AddLink(ctx, collab.Link{
		ID: stored.ID, TenantID: tenant, Kind: collab.LinkDelegation,
		From: link.From, To: link.To, CreatedAt: time.Now().UTC(),
	}); !errors.Is(err, collab.ErrLinkConflict) {
		t.Fatalf("explicit id conflict = %v, want ErrLinkConflict", err)
	}
}
