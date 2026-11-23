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
	// ReleaseRun drops an unstarted reservation (pending or queued, no unread
	// input), then promotes the tenant's FIFO queue heads while capacity
	// permits. tenantLimit is the resolved tenant quota (0 = unlimited, a
	// negative value means "could not resolve": release still happens, but no
	// promotion is attempted from this call).
	ReleaseRun(ctx context.Context, tenantID, sessionID uuid.UUID, tenantLimit int) error
	// QueueState reads one session's durable queue entry (Queued=false when the
	// run is not waiting).
	QueueState(ctx context.Context, tenantID, sessionID uuid.UUID) (QueueState, error)
	// PromoteQueued promotes FIFO queue heads for one tenant, one per free slot,
	// while slots, the memory budget and the tenant quota all permit.
	PromoteQueued(ctx context.Context, tenantID uuid.UUID, tenantLimit int) ([]Promotion, error)
	// ObserveRunDuration records one observed attempt duration: the only source
	// of the queue ETA average (there is no built-in duration).
	ObserveRunDuration(ctx context.Context, tenantID, groupID uuid.UUID, duration time.Duration) error
	// GroupMemoryUsage reports the active-run memory estimate per group (the
	// pack strategy's headroom input).
	GroupMemoryUsage(ctx context.Context, tenantID uuid.UUID) (map[uuid.UUID]int64, error)
	// QueuedTenants lists the tenants that currently have queued runs (the
	// crash-recovery sweep's work list).
	QueuedTenants(ctx context.Context) ([]uuid.UUID, error)
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
// memoryEstimate/memoryKnown carry the resolved memory estimate; a memory-
// bounded group refuses an unknown estimate with *UnsupportedError instead of
// inventing one.
type admitWrite struct {
	tenantID       uuid.UUID
	sessionID      uuid.UUID
	groupID        *uuid.UUID
	tenantLimit    int
	memoryEstimate int64
	memoryKnown    bool
}

// Run status values placed on the runs row. They mirror store.Run* on purpose:
// the runs table is the one active-run account, so the placement package reads
// and writes the same strings, never a second ledger. "queued" is a waiting
// run: it is durable, it holds no slot, and the kernel's claim predicate never
// matches it, so only promotion (queued -> pending) can make it runnable.
const (
	RunStatusQueued  = "queued"
	RunStatusPending = "pending"
	RunStatusRunning = "running"
	RunStatusDone    = "done"
	RunStatusFailed  = "failed"
)

// runActive reports whether a run status occupies a slot (group or tenant).
// Queued rows deliberately do not: the queue is waiting, not running.
func runActive(status string) bool {
	return status == RunStatusPending || status == RunStatusRunning
}

// etcWait derives the explainable ETA of one queue position. It is only called
// with an observed average (samples > 0); the wait is the number of full
// slot-waves a run must wait, times the average attempt duration. limit 0 means
// the group has no slot limit (the row is queued only until the next sweep) and
// yields the one-wave wait.
func etcWait(position, limit int, avg time.Duration) time.Duration {
	if position <= 0 || avg <= 0 {
		return 0
	}
	if limit <= 0 {
		limit = 1
	}
	waves := (position + limit - 1) / limit
	return avg * time.Duration(waves)
}

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
