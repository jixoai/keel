package superagent

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
)

// SlotPolicy is the concurrency budget of the super-agent queue. Max is the
// total number of active assignments a tenant may run for this mode.
// HumanReserved is an additional allowance for human follow-up work, so a
// queue full of fresh tickets cannot starve a person already talking to a
// worker.
type SlotPolicy struct {
	Max           int
	HumanReserved int
}

// Slots computes whether one more assignment may start. The assignment rows are
// the durable slot truth: a process restart recounts them, so no separate slot
// table can drift from reality.
type Slots struct {
	store  Store
	policy SlotPolicy
}

// NewSlots builds the counter. Max 0 means unbounded (tests/degraded mode).
func NewSlots(store Store, policy SlotPolicy) *Slots {
	return &Slots{store: store, policy: policy}
}

// Acquire checks the budget before a new assignment is created. It is a
// check-then-act guard, not a distributed lock: two processes can both pass and
// exceed the cap by one. The guarded Host's per-mode MaxConcurrent is the hard
// ceiling behind it; this counter is the product policy above that ceiling.
func (s *Slots) Acquire(ctx context.Context, tenantID uuid.UUID, humanFollowUp bool) error {
	if s == nil || s.policy.Max <= 0 {
		return nil
	}
	active, err := s.store.ListAssignmentsByStatus(ctx, tenantID, collab.AssignmentOpen, collab.AssignmentActive)
	if err != nil {
		return err
	}
	total := len(active)
	if total < s.policy.Max {
		return nil
	}
	if !humanFollowUp {
		return fmt.Errorf("%w: all %d worker slots are busy", collab.ErrConcurrencyExceeded, s.policy.Max)
	}
	usedReserved := 0
	for _, assignment := range active {
		if assignmentDocOf(assignment).HumanFollowUp {
			usedReserved++
		}
	}
	if usedReserved >= s.policy.HumanReserved {
		return fmt.Errorf("%w: all %d human follow-up slots are busy", collab.ErrConcurrencyExceeded, s.policy.HumanReserved)
	}
	return nil
}
