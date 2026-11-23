// Package placement implements the harness placement model frozen in
// openspec/changes/keel-harness-v2/specs/harness-core/spec.md:
//
//   - a Group is the placement and collaboration unit (its own concurrent-run
//     slots and memory budget; one worker process per group in this change);
//   - a session's membership is a lease (epoch + lease_until), never a static
//     binding: an expired lease is adoptable, a live one is not;
//   - placement is chosen by strategy: pack (default, density), user (one group
//     per user), pin (an explicit group);
//   - group slots and the tenant run quota are counted in the database, so two
//     groups cannot sell the same tenant slot and two workers cannot oversell a
//     group. A full group queues the run as a durable FIFO entry (status
//     `queued`, one per session, ordered by a database sequence) and answers
//     an explainable position + ETA; the tenant quota stays a hard refusal
//     (429 + Retry-After), so the queue can never become an oversell channel.
//     A queued row is never claimable by the kernel: promotion is the single
//     guarded transition queued -> pending, and it runs FIFO on ReleaseRun,
//     on a terminal attempt settlement, and from a recovery sweep (so a crash
//     converges the queue).
//
// The package owns no kernel state: it reads and writes the `groups` /
// `session_placement` rows and the existing `runs` table (which is the only
// active-run account; no second ledger). The kernel never imports this package;
// the runtime wires it in as the admission gate (Gate + GatedCommitter in
// gate.go / committer.go), and the kernel's process-local run-concurrency
// counter is delegated to the database gate (quota.Limits.EventsOnly) so one
// run is never counted twice. The events-per-minute bucket stays in process:
// it is a rate limit of the process that executes the work, not an accounting
// fact.
package placement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Strategy is the placement policy. It is a Harness concern, never a kernel
// one (the kernel sees only scope and lease tokens).
type Strategy string

const (
	// Pack places sessions into the tenant's pack groups by memory budget. One
	// group may carry sessions of more than one user; on this runtime the
	// tenancy model still equates user with tenant, so "cross-user" currently
	// means cross-session within a tenant (a deployment-scoped project group
	// needs a membership table and is deferred).
	Pack Strategy = "pack"
	// User pins every user to one group. With the current model (user ==
	// tenant) that is one group per tenant: the enterprise-isolation SKU.
	User Strategy = "user"
	// Pin uses the group named by the caller (heavy session, debugging,
	// canary).
	Pin Strategy = "pin"
)

// Group status values. A draining group takes no new placements (its sessions
// either finish or move); a retired group keeps its row for audit.
const (
	StatusActive   = "active"
	StatusDraining = "draining"
	StatusRetired  = "retired"
)

// Group names created by the automatic strategies.
const (
	PackDefaultGroupName = "pack-default"
	UserGroupName        = "user"
)

// Group is one placement container.
type Group struct {
	TenantID          uuid.UUID `json:"tenantId"`
	GroupID           uuid.UUID `json:"groupId"`
	Name              string    `json:"name"`
	MaxConcurrentRuns int       `json:"maxConcurrentRuns"`
	MemoryLimitBytes  int64     `json:"memoryLimitBytes"`
	Status            string    `json:"status"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

// Placement is a session's current membership fact. Epoch is monotonic and
// LeaseUntil bounds the named worker's authority: the worker may act for the
// session only while its epoch is current and its lease has not expired.
type Placement struct {
	TenantID   uuid.UUID `json:"tenantId"`
	SessionID  uuid.UUID `json:"sessionId"`
	GroupID    uuid.UUID `json:"groupId"`
	WorkerID   string    `json:"workerId"`
	Epoch      int64     `json:"epoch"`
	LeaseUntil time.Time `json:"leaseUntil"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// ConflictReason classifies a typed placement conflict (HTTP 409 at the edge).
type ConflictReason string

const (
	// ReasonEpochMismatch: the caller's expected epoch is not the stored one;
	// a newer claim won.
	ReasonEpochMismatch ConflictReason = "epoch_mismatch"
	// ReasonLeaseHeld: another worker holds a live lease for the session.
	ReasonLeaseHeld ConflictReason = "lease_held"
	// ReasonGroupDraining: the target group takes no new placements.
	ReasonGroupDraining ConflictReason = "group_draining"
	// ReasonGroupUnavailable: the group does not exist, is retired, or belongs
	// to another tenant.
	ReasonGroupUnavailable ConflictReason = "group_unavailable"
	// ReasonPinRequiresGroup: the pin strategy was selected without a group.
	ReasonPinRequiresGroup ConflictReason = "pin_requires_group"
	// ReasonAlreadyPlaced: an explicit join named a different group than the
	// current placement; a move must go through Seek/drain + claim.
	ReasonAlreadyPlaced ConflictReason = "already_placed"
	// ReasonPlacementNotFound: the session has no placement row.
	ReasonPlacementNotFound ConflictReason = "placement_not_found"
)

// Conflict is the typed refusal of a placement write. Callers branch on it
// with errors.As; HTTP maps it to 409. The loser of a claim must not start a
// second run for the session.
type Conflict struct {
	Reason        ConflictReason `json:"reason"`
	TenantID      uuid.UUID      `json:"tenantId"`
	SessionID     uuid.UUID      `json:"sessionId"`
	GroupID       uuid.UUID      `json:"groupId"`
	ExpectedEpoch int64          `json:"expectedEpoch"`
	CurrentEpoch  int64          `json:"currentEpoch"`
	Holder        string         `json:"holder,omitempty"`
	LeaseUntil    time.Time      `json:"leaseUntil,omitempty"`
	Detail        string         `json:"detail,omitempty"`
}

func (c *Conflict) Error() string {
	base := fmt.Sprintf("placement: %s (tenant %s session %s group %s)",
		c.Reason, c.TenantID, c.SessionID, c.GroupID)
	if c.ExpectedEpoch > 0 || c.CurrentEpoch > 0 {
		base += fmt.Sprintf(" expected epoch %d, current %d", c.ExpectedEpoch, c.CurrentEpoch)
	}
	if c.Detail != "" {
		base += ": " + c.Detail
	}
	return base
}

// IsConflict reports whether err is a typed placement conflict.
func IsConflict(err error) bool {
	var conflict *Conflict
	return errors.As(err, &conflict)
}

// CapacityReason classifies a capacity refusal.
type CapacityReason string

const (
	// CapacityGroup: the target group has no free run slot. A new run is queued
	// (not refused) when this is the only limit involved; the reason explains a
	// queued admission and a promotion that is still waiting for a slot.
	CapacityGroup CapacityReason = "group_slots_full"
	// CapacityGroupMemory: the target group's memory budget has no headroom for
	// the run's estimate. Memory is a hard ceiling at admission (a run that does
	// not fit is refused, not queued behind an estimate nothing can honour);
	// promotion re-checks it, so a queued run whose group lost headroom waits.
	CapacityGroupMemory CapacityReason = "group_memory_full"
	// CapacityTenant: the tenant's run quota is spent across every group. The
	// number is aggregated from the runs table, so grouping cannot sell the
	// same tenant slot twice. This stays a hard refusal: the queue never
	// bypasses the tenant quota, and a queued run waits at promotion time until
	// the quota has room again.
	CapacityTenant CapacityReason = "tenant_quota_exceeded"
)

// CapacityError is the typed "not now" refusal of a run admission. It is not a
// conflict (nothing is inconsistent); the client gets 429 + Retry-After, and
// the run must not be silently dropped. A full group is not reported this way
// anymore: it is a queued admission (Admission.Queued).
type CapacityError struct {
	Reason CapacityReason `json:"reason"`
	// Resource is "group", "group_memory" or "tenant" (diagnostics; it mirrors
	// Reason).
	Resource   string        `json:"resource"`
	TenantID   uuid.UUID     `json:"tenantId"`
	SessionID  uuid.UUID     `json:"sessionId"`
	GroupID    uuid.UUID     `json:"groupId,omitempty"`
	Limit      int           `json:"limit"`
	Active     int           `json:"active"`
	RetryAfter time.Duration `json:"-"`
	// LimitBytes/UsedBytes explain a memory-budget refusal (CapacityGroupMemory);
	// they are zero for slot/quota refusals.
	LimitBytes int64 `json:"limitBytes,omitempty"`
	UsedBytes  int64 `json:"usedBytes,omitempty"`
}

func (e *CapacityError) Error() string {
	return fmt.Sprintf("placement: %s %d/%d for tenant %s session %s; retry after %s",
		e.Reason, e.Active, e.Limit, e.TenantID, e.SessionID, e.RetryAfter)
}

// IsCapacity reports whether err is a typed capacity refusal.
func IsCapacity(err error) bool {
	var capacity *CapacityError
	return errors.As(err, &capacity)
}

// Sentinel errors for lookups that are not conflicts.
var (
	// ErrPlacementNotFound means the session has no placement row.
	ErrPlacementNotFound = errors.New("placement: session has no placement")
	// ErrGroupNotFound means the group does not exist for the tenant.
	ErrGroupNotFound = errors.New("placement: group not found")
)

// ClaimRequest is one epoch-fenced placement claim. ExpectedEpoch is the epoch
// the caller believes is current:
//
//   - 0 means "the session has no placement; create it";
//   - a positive value is a compare-and-set: the claim wins only when the
//     stored epoch is exactly that value, then writes epoch+1.
//
// GroupID nil keeps the current group (or resolves one by Strategy when the
// session has no placement). A live lease in another group cannot be moved by
// a plain claim: the old group must drain/expire first (see Seek).
type ClaimRequest struct {
	TenantID      uuid.UUID
	SessionID     uuid.UUID
	GroupID       *uuid.UUID
	Strategy      Strategy
	WorkerID      string
	ExpectedEpoch int64
	TTL           time.Duration
}

// AdoptRequest is the runtime gate's claim: the kernel has already issued the
// fencing epoch (lease.Token.Epoch), and the placement mirrors it. Epoch must
// be monotonically non-decreasing; a lower epoch is a stale writer and is
// refused. Adopt can take over an expired placement, and (because the kernel
// claim is the sole authority for run execution) it may follow a group change
// only when the old placement lease has expired.
type AdoptRequest struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	GroupID   *uuid.UUID
	Strategy  Strategy
	WorkerID  string
	Epoch     int64
	TTL       time.Duration
}

// JoinRequest is an explicit join (the session is placed and the worker takes
// the lease in one step). Joining the current group again is idempotent for the
// same worker; joining a different group while the current lease is live is a
// conflict.
type JoinRequest struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	GroupID   *uuid.UUID
	Strategy  Strategy
	WorkerID  string
	TTL       time.Duration
}

// SeekRequest asks for a migration plan to another group. It does not move
// anything: the plan names the drain and rebuild steps so the caller can drive
// them (old group drains, new group rebuilds from the ledger).
type SeekRequest struct {
	TenantID      uuid.UUID
	SessionID     uuid.UUID
	ToGroupID     uuid.UUID
	ExpectedEpoch int64
}

// MigrationPlan is the interface semantics of a placement move: no process
// state is copied, the new group rebuilds the session from the event ledger.
type MigrationPlan struct {
	From             Placement `json:"from"`
	FromGroup        Group     `json:"fromGroup"`
	To               Group     `json:"to"`
	ExpectedEpoch    int64     `json:"expectedEpoch"`
	NextEpoch        int64     `json:"nextEpoch"`
	DrainRequired    bool      `json:"drainRequired"`
	RebuildFromSeq   int64     `json:"rebuildFromSeq"`
	CopyProcessState bool      `json:"copyProcessState"`
}

// LeaveRequest removes a session's placement fact. The epoch is a
// compare-and-set guard: a stale caller cannot delete a newer placement.
type LeaveRequest struct {
	TenantID      uuid.UUID
	SessionID     uuid.UUID
	WorkerID      string
	ExpectedEpoch int64
}

// AdmitRequest asks for one run slot for an already-placed session. The
// placement row decides the group; GroupID, when set, is only a consistency
// check (a caller that believes the session is in another group is refused
// instead of silently running in the stored one).
//
// The service resolves TenantLimit (the same quota document the kernel's
// admission uses) and the store enforces both limits in one database
// transaction, so two groups of one tenant cannot oversell the tenant quota.
type AdmitRequest struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	GroupID   *uuid.UUID
}

// Admission is the outcome of a run admission: either a granted slot or a
// durable position in the group's FIFO queue. ActiveGroup/ActiveTenant count
// active (pending+running) rows and include this session's own row; Limit 0
// means unlimited. A queued admission holds no slot: the queued row is the
// durable queue entry, it is excluded from every active count, and promotion
// (FIFO, exactly once) turns it into a pending run when a slot frees.
type Admission struct {
	TenantID     uuid.UUID `json:"tenantId"`
	SessionID    uuid.UUID `json:"sessionId"`
	GroupID      uuid.UUID `json:"groupId"`
	ActiveGroup  int       `json:"activeGroup"`
	LimitGroup   int       `json:"limitGroup"`
	ActiveTenant int       `json:"activeTenant"`
	LimitTenant  int       `json:"limitTenant"`
	// AlreadyActive means the session already had an active (pending/running)
	// row in this group before this call; the call reserved no extra slot.
	AlreadyActive bool `json:"alreadyActive"`
	// Created means this call wrote the run reservation/queue entry. It is what
	// a failed commit releases; a re-admission of an existing row is not.
	Created bool `json:"created"`
	// Queued means the run waits in the group's FIFO queue. The input commit
	// still proceeds (the message is durable); the run starts on promotion.
	Queued        bool     `json:"queued"`
	QueuePosition int      `json:"queuePosition,omitempty"`
	ETA           QueueETA `json:"eta"`
	// MemoryBytes is the estimate recorded on the run row (0 when the group has
	// no memory budget or the estimate is unknown).
	MemoryBytes int64 `json:"memoryBytes,omitempty"`
	// RetryAfter is the explainable wait a hard capacity refusal advertises.
	RetryAfter time.Duration `json:"-"`
}

// QueueETA is the explainable wait of a queued run. Known=false is the
// documented fallback when the group (and its tenant) have no observed run
// durations yet: the wait is unknown, never a fabricated zero. Wait is derived
// only from observed attempts: avgDuration * ceil(position/limit).
type QueueETA struct {
	Known   bool          `json:"known"`
	Wait    time.Duration `json:"-"`
	Samples int           `json:"samples"`
	// Basis is "group" or "tenant": which rolling window produced the average.
	Basis string `json:"basis,omitempty"`
}

// QueueState is the durable queue fact for one session, read after a commit by
// the HTTP edge so a queued admission can answer 202 with its position and ETA.
type QueueState struct {
	TenantID  uuid.UUID `json:"tenantId"`
	SessionID uuid.UUID `json:"sessionId"`
	GroupID   uuid.UUID `json:"groupId"`
	Queued    bool      `json:"queued"`
	Position  int       `json:"position"`
	Limit     int       `json:"limit"`
	Active    int       `json:"active"`
	ETA       QueueETA  `json:"eta"`
}

// Promotion is one queued run that became pending.
type Promotion struct {
	TenantID  uuid.UUID `json:"tenantId"`
	GroupID   uuid.UUID `json:"groupId"`
	SessionID uuid.UUID `json:"sessionId"`
}

// UnsupportedReason classifies a request the current configuration cannot
// answer honestly.
type UnsupportedReason string

const (
	// ReasonMemoryEstimateUnavailable: the group has a memory budget but no
	// memory estimate is available for the session. Packing must not invent a
	// number (an invented estimate would make a hard ceiling look exact), so
	// the request is refused with this typed reason instead.
	ReasonMemoryEstimateUnavailable UnsupportedReason = "memory_estimate_unavailable"
)

// UnsupportedError is the typed "the current configuration cannot decide this"
// failure (HTTP 501 at the edge). It is distinct from a capacity refusal: no
// retry will help until configuration supplies the missing input.
type UnsupportedError struct {
	Reason    UnsupportedReason `json:"reason"`
	TenantID  uuid.UUID         `json:"tenantId"`
	SessionID uuid.UUID         `json:"sessionId"`
	GroupID   uuid.UUID         `json:"groupId,omitempty"`
	Detail    string            `json:"detail,omitempty"`
}

func (e *UnsupportedError) Error() string {
	base := fmt.Sprintf("placement: unsupported %s (tenant %s session %s)", e.Reason, e.TenantID, e.SessionID)
	if e.GroupID != uuid.Nil {
		base += fmt.Sprintf(" group %s", e.GroupID)
	}
	if e.Detail != "" {
		base += ": " + e.Detail
	}
	return base
}

// IsUnsupported reports whether err is a typed unsupported/configuration
// failure.
func IsUnsupported(err error) bool {
	var unsupported *UnsupportedError
	return errors.As(err, &unsupported)
}

// GroupSpec is one group creation request. Name is a per-tenant handle; the
// strategies use it to upsert their automatic groups.
type GroupSpec struct {
	TenantID          uuid.UUID
	GroupID           uuid.UUID
	Name              string
	MaxConcurrentRuns int
	MemoryLimitBytes  int64
}

// Options wires the service.
type Options struct {
	// Store is the placement store (required): *PGStore in production,
	// MemStore in unit tests.
	Store Store
	// Strategy is the default policy when a request names none.
	Strategy Strategy
	// PinGroupID is the configured default target for the pin strategy. A pin
	// request may still name a group explicitly; when neither is present the
	// claim is refused (the strategy never guesses).
	PinGroupID *uuid.UUID
	// DefaultMaxConcurrentRuns / DefaultMemoryLimitBytes configure the groups
	// the strategies create on demand. Zero means unlimited.
	DefaultMaxConcurrentRuns int
	DefaultMemoryLimitBytes  int64
	// MemoryEstimator resolves the memory budget a session's next run is
	// expected to consume. It is the only source of that number: there is no
	// built-in default, because an invented estimate would make a hard memory
	// ceiling look exact. A nil estimator (or a non-positive answer) means
	// "unknown": using a memory-bounded group then fails with
	// *UnsupportedError{Reason: ReasonMemoryEstimateUnavailable}.
	MemoryEstimator MemoryEstimator
	// TenantRunLimit resolves the tenant's concurrent-run quota from the same
	// configuration vocabulary the kernel admission uses (quota.Limits.Quota).
	// Nil means no tenant-level limit; an error is fail-closed.
	TenantRunLimit func(ctx context.Context, tenantID uuid.UUID) (int, error)
	// NewID mints group ids (tests make them predictable). Defaults to uuid.New.
	NewID func() uuid.UUID
}

// MemoryEstimator answers the memory estimate for one session's next run. A
// non-positive value means "no estimate"; an error is propagated (fail closed).
type MemoryEstimator func(ctx context.Context, tenantID, sessionID uuid.UUID) (int64, error)

// ETA observation bounds: how many recent attempts an ETA average uses per
// group, and how many samples are retained per group.
const (
	ETASampleWindow    = 20
	ETASampleRetention = 50
)

// DefaultLeaseTTL bounds a placement lease when a request carries no TTL.
const DefaultLeaseTTL = 30 * time.Second

// DefaultRetryAfter is the minimum Retry-After a capacity refusal advertises
// when the store cannot derive a nearer lease expiry.
const DefaultRetryAfter = time.Second
