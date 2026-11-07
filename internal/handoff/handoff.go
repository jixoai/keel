package handoff

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
)

// Mode is a drain mode. The strings are the durable audit values.
type Mode string

const (
	// ModeShutdown is the SIGTERM / deployment drain: stop claiming, reject new
	// writes, close SSE; in-flight runs may be abandoned and the lease expires.
	ModeShutdown Mode = "shutdown"
	// ModeHandoff is the upgrade drain: stop claiming, keep accepting
	// edge-scoped input, drain in-flight runs to a safe point or the deadline.
	ModeHandoff Mode = "handoff"
)

// Valid reports whether m is a registered drain mode.
func (m Mode) Valid() bool { return m == ModeShutdown || m == ModeHandoff }

// AdmitsEdgeWrites reports whether the drain mode keeps accepting edge-scoped
// input (user.message). Only handoff does; shutdown answers 503 and the client
// retries against the new runtime.
func (m Mode) AdmitsEdgeWrites() bool { return m == ModeHandoff }

// String renders the mode.
func (m Mode) String() string { return string(m) }

// Identity is one runtime instance: its persistent id (used as the claim
// owner) and the loadout it runs under (pinned on every claim).
type Identity struct {
	// ID is the runtime id. It is persisted in runtime_instances and written
	// into every claim as the owner.
	ID string
	// LoadoutID and Revision are the loadout pin recorded with each claim.
	LoadoutID string
	Revision  int64
}

// Status is the runtime's state snapshot (the admin/liveness answer).
type Status struct {
	Identity
	Draining     bool
	Mode         Mode
	Deadline     time.Time
	InFlight     int
	Claiming     bool
	StartedAt    time.Time
	ReadyAt      time.Time
	DrainStarted time.Time
	StoppedAt    time.Time
	Reason       string
}

// Runtime status values (the runtime_instances.status vocabulary).
const (
	RuntimeStarting = "starting"
	RuntimeReady    = "ready"
	RuntimeDraining = "draining"
	RuntimeStopped  = "stopped"
)

// Handoff audit statuses.
const (
	HandoffStarted   = "started"
	HandoffCompleted = "completed"
	HandoffTimedOut  = "timed-out"
)

// Registry errors.
var (
	// ErrNoRegistry: the coordinator has no registry to persist to.
	ErrNoRegistry = errors.New("handoff: no registry configured")
	// ErrNoKernel: the coordinator has no kernel slice to drive.
	ErrNoKernel = errors.New("handoff: no kernel configured")
	// ErrAlreadyDraining: a drain is already in progress.
	ErrAlreadyDraining = errors.New("handoff: a drain is already in progress")
	// ErrNotDraining: no drain is in progress.
	ErrNotDraining = errors.New("handoff: no drain in progress")
	// ErrNothingToAdopt: the session has no claimable run (settled or absent).
	ErrNothingToAdopt = errors.New("handoff: nothing to adopt")
	// ErrUnknownRuntime: the registry has no such runtime row.
	ErrUnknownRuntime = errors.New("handoff: unknown runtime")
	// ErrUnknownHandoff: the registry has no such handoff row.
	ErrUnknownHandoff = errors.New("handoff: unknown handoff")
)

// Kernel is the slice of the contract kernel the handoff protocol drives.
// *kernel.Kernel satisfies it; tests can supply a wrapper to inject failures.
type Kernel interface {
	Claim(ctx context.Context, owner string, ttl time.Duration) (kernel.Lease, error)
	ClaimScope(ctx context.Context, scope kernel.Scope, owner string, ttl time.Duration) (kernel.Lease, error)
	Renew(ctx context.Context, lease kernel.Lease, ttl time.Duration) (kernel.Lease, error)
	Finish(ctx context.Context, req kernel.FinishRequest) error
	Release(ctx context.Context, lease kernel.Lease) error
	LeaseState(ctx context.Context, scope kernel.Scope) (kernel.LeaseState, error)
	Commit(ctx context.Context, req kernel.CommitRequest) (kernel.Receipt, error)
	LoadEvents(ctx context.Context, scope kernel.Scope, afterSeq int64, limit int) ([]kernel.Event, error)
	UnresolvedToolCalls(ctx context.Context, scope kernel.Scope) ([]kernel.ToolCallRef, error)
	PinRunLoadout(ctx context.Context, lease kernel.Lease, loadoutID string, revision int64) error
	RunPin(ctx context.Context, scope kernel.Scope) (kernel.RunPin, error)
}

var _ Kernel = (*kernel.Kernel)(nil)

// RuntimeRecord is one runtime_instances row: a runtime's identity, its
// loadout pin and its drain lifecycle. Every transition is durable so an
// operator can answer "which runtime drained when, in which mode, and how many
// runs were in flight".
type RuntimeRecord struct {
	RuntimeID string
	LoadoutID string
	Revision  int64
	Status    string
	Mode      Mode
	Deadline  time.Time
	InFlight  int
	StartedAt time.Time
	ReadyAt   time.Time
	DrainAt   time.Time
	StoppedAt time.Time
	Reason    string
}

// HandoffRecord is one handoffs row: a runtime-level drain or a per-session
// adoption. SessionID/TenantID are zero for a runtime-level drain. The record
// answers the spec's audit scenario: old/new runtime, loadout revision, epochs,
// start/end times, in-flight runs.
type HandoffRecord struct {
	ID          string
	TenantID    uuid.UUID
	SessionID   uuid.UUID
	Mode        Mode
	FromRuntime string
	ToRuntime   string
	FromEpoch   int64
	ToEpoch     int64
	LoadoutID   string
	Revision    int64
	Status      string
	InFlight    int
	StartedAt   time.Time
	FinishedAt  time.Time
	Detail      string
}

// HandoffQuery filters handoff audit reads. Zero fields match everything.
type HandoffQuery struct {
	TenantID    uuid.UUID
	SessionID   uuid.UUID
	FromRuntime string
	ToRuntime   string
	Limit       int
}

// Registry is the durable handoff audit seam (runtime_instances + handoffs).
// MemRegistry and PGRegistry implement it.
type Registry interface {
	// SaveRuntime upserts one runtime row.
	SaveRuntime(ctx context.Context, rec RuntimeRecord) error
	// Runtime reads one runtime row; ErrUnknownRuntime when absent.
	Runtime(ctx context.Context, runtimeID string) (RuntimeRecord, error)
	// Runtimes lists runtime rows, newest first.
	Runtimes(ctx context.Context, limit int) ([]RuntimeRecord, error)

	// SaveHandoff inserts one handoff row.
	SaveHandoff(ctx context.Context, rec HandoffRecord) error
	// FinishHandoff closes a started handoff row with its final status.
	FinishHandoff(ctx context.Context, id, status string, inFlight int, detail string, at time.Time) error
	// Handoff reads one row; ErrUnknownHandoff when absent.
	Handoff(ctx context.Context, id string) (HandoffRecord, error)
	// ListHandoffs lists rows matching the query, newest first.
	ListHandoffs(ctx context.Context, q HandoffQuery) ([]HandoffRecord, error)
}

// Adoption is what a successor runtime learns when it takes a session over.
type Adoption struct {
	// Lease is the new claim (owner = this runtime, epoch = previous+1).
	Lease kernel.Lease
	// Previous is the token of the owner this runtime took over from (zero
	// when the session was free, e.g. a first claim after a crash).
	Previous kernel.OwnerToken
	// FirstClaim reports whether there was no previous live claim.
	FirstClaim bool
	// Rebuild is the resume state reconstructed from the event log.
	Rebuild Rebuild
	// HandoffID is the audit row recorded for this takeover ("" when no
	// handoff was recorded because the session was already free and owned by
	// this runtime).
	HandoffID string
	// Waited is how long the adoption waited for the previous lease to clear.
	Waited time.Duration
}

// Rebuild is the resume state a successor computes from the committed log. It
// is deliberately derived only from committed events: an uncommitted model
// partial is invisible here, which is exactly the "不出现半条消息" guarantee.
type Rebuild struct {
	// Cursor is the last committed sequence.
	Cursor int64
	// Events is how many events were replayed.
	Events int
	// LastKind is the kind of the last committed event ("" when empty).
	LastKind kernel.EventKind
	// LastInputSeq is the seq of the newest committed user.message (0 when the
	// session has no input yet).
	LastInputSeq int64
	// ReadWatermark is the run row's last proven read point.
	ReadWatermark int64
	// Unresolved lists assistant tool calls with no committed result yet: the
	// calls the successor must re-execute (at-least-once) before continuing.
	Unresolved []kernel.ToolCallRef
	// PendingUserInput reports whether the last committed event is an
	// unanswered user message.
	PendingUserInput bool
}

// NeedsModelTurn reports whether the successor must ask the model for another
// step: either input arrived that no assistant message answered, or tool calls
// are still unresolved.
func (r Rebuild) NeedsRun() bool {
	return r.PendingUserInput || len(r.Unresolved) > 0
}

// String renders the rebuild for logs.
func (r Rebuild) String() string {
	return fmt.Sprintf("cursor=%d events=%d unresolved=%d pending_input=%t",
		r.Cursor, r.Events, len(r.Unresolved), r.PendingUserInput)
}

// edgeKind reports whether a committed event kind is client input that keeps
// landing during a handoff drain.
func edgeKind(kind kernel.EventKind) bool {
	for _, k := range kernel.EdgeKinds() {
		if k == kind {
			return true
		}
	}
	return false
}
