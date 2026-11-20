// Store is the placement persistence contract. The service owns policy
// (strategy, quota resolution, typed errors); the store owns the atomic
// compare-and-set writes that make the policy safe under concurrency.
//
// The interface's write requests are unexported on purpose: only this package
// can implement or call them, so an external package cannot bypass the
// service's group resolution and quota checks.
package placement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Store persists groups, placement facts and run-slot accounting.
type Store interface {
	// CreateGroup inserts one group. A duplicate (tenant, name) is
	// ErrGroupExists: explicit creation must not silently adopt an existing
	// group with different limits.
	CreateGroup(ctx context.Context, spec GroupSpec) (Group, error)
	// EnsureGroup returns the group with the spec's name, creating it when
	// absent. The automatic strategies (pack/user) use it; an existing group's
	// limits are kept, because an administrator may have tuned them.
	EnsureGroup(ctx context.Context, spec GroupSpec) (Group, error)
	GetGroup(ctx context.Context, tenantID, groupID uuid.UUID) (Group, error)
	ListGroups(ctx context.Context, tenantID uuid.UUID) ([]Group, error)
	// SetGroupStatus moves a group through active -> draining -> retired.
	SetGroupStatus(ctx context.Context, tenantID, groupID uuid.UUID, status string) (Group, error)

	GetPlacement(ctx context.Context, tenantID, sessionID uuid.UUID) (Placement, error)
	ClaimPlacement(ctx context.Context, req claimWrite) (Placement, error)
	AdoptPlacement(ctx context.Context, req adoptWrite) (Placement, error)
	ReleasePlacement(ctx context.Context, tenantID, sessionID uuid.UUID, workerID string, expectedEpoch int64) error

	AdmitRun(ctx context.Context, req admitWrite) (Admission, error)
	ReleaseRun(ctx context.Context, tenantID, sessionID uuid.UUID) error
}

// claimWrite is a compare-and-set placement claim after the service resolved
// the target group (nil keeps the current group).
type claimWrite struct {
	tenantID      uuid.UUID
	sessionID     uuid.UUID
	groupID       *uuid.UUID
	expectedEpoch int64
	workerID      string
	ttl           time.Duration
	// moveReason is the conflict reason when the session already holds a live
	// lease in another group: ReasonLeaseHeld for a generic claim,
	// ReasonAlreadyPlaced for an explicit join.
	moveReason ConflictReason
}

// adoptWrite mirrors a kernel-issued epoch. Epoch is the kernel's authority;
// the placement row follows it and never invents a lower one.
type adoptWrite struct {
	tenantID  uuid.UUID
	sessionID uuid.UUID
	groupID   *uuid.UUID
	workerID  string
	epoch     int64
	ttl       time.Duration
}

// admitWrite is one run-slot admission. tenantLimit is already resolved by the
// service (0 = unlimited, an error there is fail-closed before this call).
type admitWrite struct {
	tenantID    uuid.UUID
	sessionID   uuid.UUID
	groupID     *uuid.UUID
	tenantLimit int
}

// Run status values placed on the runs row. They mirror store.Run* on purpose:
// the runs table is the one active-run account, so the placement package reads
// and writes the same strings, never a second ledger.
const (
	RunStatusPending = "pending"
	RunStatusRunning = "running"
	RunStatusDone    = "done"
)

var (
	// ErrGroupExists reports a duplicate (tenant, name) group creation.
	ErrGroupExists = errors.New("placement: group name already exists")
	// ErrInvalidGroup reports a group spec the store refuses (empty name,
	// negative limits).
	ErrInvalidGroup = errors.New("placement: invalid group")
	// ErrNoRun reports that an explicit SetRunStatus aid found no run row.
	ErrNoRun = errors.New("placement: no admitted run")
)

func validateGroupSpec(spec GroupSpec) error {
	switch {
	case spec.TenantID == uuid.Nil:
		return fmt.Errorf("%w: tenant id is required", ErrInvalidGroup)
	case spec.GroupID == uuid.Nil:
		return fmt.Errorf("%w: group id is required", ErrInvalidGroup)
	case spec.Name == "":
		return fmt.Errorf("%w: name is required", ErrInvalidGroup)
	case spec.MaxConcurrentRuns < 0:
		return fmt.Errorf("%w: max_concurrent_runs must be >= 0", ErrInvalidGroup)
	case spec.MemoryLimitBytes < 0:
		return fmt.Errorf("%w: memory_limit_bytes must be >= 0", ErrInvalidGroup)
	}
	return nil
}

// ValidateGroupStatus reports whether status is a known group status.
func ValidateGroupStatus(status string) error {
	switch status {
	case StatusActive, StatusDraining, StatusRetired:
		return nil
	}
	return fmt.Errorf("%w: unknown status %q", ErrInvalidGroup, status)
}
