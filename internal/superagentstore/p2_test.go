// P2 collab persistence moved with the superagent domain: cross-WorkItem
// assignment rebinding and the natural link key are adapter properties, not
// core-store ones. These tests follow the storage into superagentstore so the
// guard does not disappear with the old collab_* table names.

package superagentstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/collab"
)

func newP2WorkItem(t *testing.T, adapter interface {
	CreateWorkItem(context.Context, collab.WorkItem) (collab.WorkItem, error)
}, tenant uuid.UUID, title string) collab.WorkItem {
	t.Helper()
	item := collab.WorkItem{
		ID: uuid.New(), TenantID: tenant, ModeID: collab.ModeSuperagent,
		Title: title, Status: collab.WorkItemOpen,
	}
	stored, err := adapter.CreateWorkItem(context.Background(), item)
	if err != nil {
		t.Fatalf("CreateWorkItem(%s): %v", title, err)
	}
	return stored
}

// TestCollabAssignmentBySessionAcrossWorkItems pins the durable half of the
// rebinding guard: one session's assignments are visible across WorkItems, and
// another tenant sees none.
func TestCollabAssignmentBySessionAcrossWorkItems(t *testing.T) {
	adapter := openTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	first := newP2WorkItem(t, adapter, tenant, "first")
	second := newP2WorkItem(t, adapter, tenant, "second")
	worker := uuid.New()
	now := time.Now().UTC()
	for index, item := range []collab.WorkItem{first, second} {
		if _, err := adapter.CreateAssignment(ctx, collab.Assignment{
			ID: uuid.New(), TenantID: tenant, WorkItemID: item.ID, ModeID: item.ModeID,
			BotID: "worker", SessionID: worker, CallerSessionID: uuid.New(),
			Status: collab.AssignmentActive, Title: "binding", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreateAssignment %d: %v", index, err)
		}
	}
	assignments, err := adapter.ListAssignmentsBySession(ctx, tenant, worker)
	if err != nil || len(assignments) != 2 {
		t.Fatalf("ListAssignmentsBySession = %d (%v), want 2", len(assignments), err)
	}
	if assignments[0].WorkItemID == assignments[1].WorkItemID {
		t.Fatalf("assignments = %+v, want one per WorkItem", assignments)
	}
	if foreign, err := adapter.ListAssignmentsBySession(ctx, uuid.New(), worker); err != nil || len(foreign) != 0 {
		t.Fatalf("foreign tenant assignments = %+v, %v, want none", foreign, err)
	}
}

// TestCollabLinkNaturalDedupe pins the PG half of AddLink: the deterministic
// relation is stored once, readable from either WorkItem endpoint, and an
// explicit id naming a different relation is refused.
func TestCollabLinkNaturalDedupe(t *testing.T) {
	adapter := openTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	first := newP2WorkItem(t, adapter, tenant, "first")
	second := newP2WorkItem(t, adapter, tenant, "second")
	link := collab.Link{
		ID:        uuid.New(),
		TenantID:  tenant,
		Kind:      collab.LinkReview,
		From:      collab.LinkEndpoint{Type: collab.LinkEndpointWorkItem, ID: first.ID.String()},
		To:        collab.LinkEndpoint{Type: collab.LinkEndpointWorkItem, ID: second.ID.String()},
		CreatedAt: time.Now().UTC(),
	}
	stored, err := adapter.AddLink(ctx, link)
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}
	again, err := adapter.AddLink(ctx, collab.Link{
		ID: uuid.New(), TenantID: tenant, Kind: collab.LinkReview,
		From: link.From, To: link.To, CreatedAt: time.Now().UTC(),
	})
	if err != nil || again.ID != stored.ID {
		t.Fatalf("dedupe = %+v, %v, want %s", again, err, stored.ID)
	}
	for _, item := range []collab.WorkItem{first, second} {
		links, err := adapter.ListLinks(ctx, tenant, item.ID)
		if err != nil || len(links) != 1 || links[0].ID != stored.ID {
			t.Fatalf("ListLinks(%s) = %+v, %v", item.ID, links, err)
		}
	}
	if got, err := adapter.GetLink(ctx, tenant, stored.ID); err != nil || got.ID != stored.ID {
		t.Fatalf("GetLink = %+v, %v", got, err)
	}
	if _, err := adapter.GetLink(ctx, uuid.New(), stored.ID); !errors.Is(err, collab.ErrLinkNotFound) {
		t.Fatalf("foreign GetLink = %v, want ErrLinkNotFound", err)
	}
	if _, err := adapter.AddLink(ctx, collab.Link{
		ID: stored.ID, TenantID: tenant, Kind: collab.LinkDelegation,
		From: link.From, To: link.To, CreatedAt: time.Now().UTC(),
	}); !errors.Is(err, collab.ErrLinkConflict) {
		t.Fatalf("explicit id conflict = %v, want ErrLinkConflict", err)
	}
}
