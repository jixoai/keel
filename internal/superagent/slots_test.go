package superagent_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/superagent"
)

func TestSlotsReserveCapacityForHumanFollowUp(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()
	store := superagent.NewMemoryStore(func() time.Time { return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) })
	slots := superagent.NewSlots(store, superagent.SlotPolicy{Max: 1, HumanReserved: 1})
	item := collab.WorkItem{ID: uuid.New(), TenantID: tenant, ModeID: collab.ModeSuperagent, Status: collab.WorkItemOpen}
	if _, err := store.CreateWorkItem(ctx, item); err != nil {
		t.Fatalf("CreateWorkItem: %v", err)
	}
	active := collab.Assignment{
		ID: uuid.New(), TenantID: tenant, WorkItemID: item.ID, ModeID: collab.ModeSuperagent,
		BotID: "worker", Status: collab.AssignmentActive,
	}
	if _, err := store.CreateAssignment(ctx, active); err != nil {
		t.Fatalf("CreateAssignment: %v", err)
	}
	if err := slots.Acquire(ctx, tenant, false); !errors.Is(err, collab.ErrConcurrencyExceeded) {
		t.Fatalf("normal acquire err = %v, want concurrency exceeded", err)
	}
	if err := slots.Acquire(ctx, tenant, true); err != nil {
		t.Fatalf("human reserve acquire: %v", err)
	}
	human := collab.Assignment{
		ID: uuid.New(), TenantID: tenant, WorkItemID: item.ID, ModeID: collab.ModeSuperagent,
		BotID: "worker", Status: collab.AssignmentActive, Input: []byte(`{"humanFollowUp":true}`),
	}
	if _, err := store.CreateAssignment(ctx, human); err != nil {
		t.Fatalf("CreateAssignment human: %v", err)
	}
	if err := slots.Acquire(ctx, tenant, true); !errors.Is(err, collab.ErrConcurrencyExceeded) {
		t.Fatalf("human reserve exhausted err = %v, want concurrency exceeded", err)
	}
}
