package kernel

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// LogStore is the narrow persistence seam of the contract kernel.
//
// Two implementations exist: MemStore (in-memory fake used by tests and by
// nothing else) and PGStore (PostgreSQL, built on the same pool as
// internal/store). The seam deliberately exposes no raw table handle: the only
// way to append an event is Append on a SessionTxn, and the only way to obtain
// one is BeginSession, which takes the session's linearization lock.
//
// The session is the single linearization point: BeginSession, ClaimLease,
// RenewLease, FinishLease and ReleaseLease all take the session row lock before
// touching anything else, which is also the lock sequence allocation takes. The
// implementation MUST keep the fixed lock order sessions -> runs, so a claim
// and a finish can never deadlock.
//
// A plugin never sees a LogStore, a SessionTxn, or store.Store. The service
// graph injects Committer / Reader / LeaseManager instead (see README.md).
type LogStore interface {
	// BeginSession opens the session's write transaction. It fails with
	// ErrNotFound when (tenant, session) does not exist, so a write to another
	// tenant's session is indistinguishable from a write to a missing one.
	//
	// The returned transaction holds the per-session write lock: every read it
	// serves is consistent with the append it authorizes, and concurrent claims
	// or finishes cannot interleave between validation and append.
	BeginSession(ctx context.Context, tenantID, sessionID uuid.UUID) (SessionTxn, error)

	// LoadEvents returns up to limit events with seq > afterSeq, in order.
	// An empty page is checked against the session row: ErrNotFound means the
	// (tenant, session) pair does not exist.
	LoadEvents(ctx context.Context, tenantID, sessionID uuid.UUID, afterSeq int64, limit int) ([]Event, error)

	// FindDedupe looks up one committed event by its namespaced dedupe key
	// without taking the session write lock (the Lookup path is read-only).
	FindDedupe(ctx context.Context, tenantID, sessionID uuid.UUID, key string) (Event, bool, error)

	// SessionExists reports whether the (tenant, session) pair exists.
	SessionExists(ctx context.Context, tenantID, sessionID uuid.UUID) (bool, error)

	// ClaimLease claims any runnable run (runs.status='pending', or 'running'
	// with an expired lease) and issues a new fencing token: it locks the
	// candidate's session row, increments the epoch there, then mirrors the
	// claim onto the run row. ErrNoRunnable when nothing is claimable.
	ClaimLease(ctx context.Context, owner string, ttl time.Duration) (Lease, error)

	// ClaimScope claims one specific session. It fails with ErrNotFound when
	// the session has no run row, ErrLeaseHeld when a live lease is held, and
	// ErrNoRunnable when the run is not claimable (done/failed).
	ClaimScope(ctx context.Context, scope Scope, owner string, ttl time.Duration) (Lease, error)

	// RenewLease extends a live lease with a conditional update on owner+epoch
	// in the session row. Rejections: ErrNotFound, ErrLeaseLost, ErrStaleEpoch
	// (owner or epoch mismatch), ErrLeaseExpired.
	RenewLease(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error)

	// FinishLease settles a claimed run: holding the session row lock it
	// validates the token against the authority, applies the same outcome
	// policy as store.FinishRun (done / re-queued / failed) and clears the
	// lease. A stale owner's finish is rejected so it cannot clear a newer
	// owner's lease or overwrite its run state.
	FinishLease(ctx context.Context, req FinishRequest) error

	// ReleaseLease gives a claim back without settling it: the lease is cleared
	// and the run returns to pending, under the same token guard as Finish.
	ReleaseLease(ctx context.Context, scope Scope, token OwnerToken) error

	// FindLease reads the session's fencing state for audits and handoff
	// queries. Live/Found follow the adapter's clock.
	FindLease(ctx context.Context, scope Scope) (LeaseState, error)
}

// ExternalState is the session's external-session classification as read by a
// session transaction. StateOwner is StateOwnerKeel for a native session;
// Status is the external_status mark ("" for a native session).
type ExternalState struct {
	StateOwner string
	Status     string
}

// SessionTxn is one serialized session transaction. Reads and the staged
// append happen under the session write lock; Commit applies the append and
// releases the lock, Rollback discards it. Rollback is always safe to call.
type SessionTxn interface {
	// Lease reads the session's fencing state, taking the lock that serializes
	// this transaction against Claim / Renew / Finish on the same session.
	Lease(ctx context.Context) (LeaseState, error)

	// ExternalState reads the session's external-session classification: the
	// state_owner mark (StateOwnerKeel for a native session) and the
	// external_status mark ("" for a native session). It is read under the same
	// session lock as the append, so an external-scoped commit cannot race a
	// promotion or a close: one of the two orderings always wins cleanly.
	// NativeEdge commits use the same read for the mirror-image refusal.
	ExternalState(ctx context.Context) (ExternalState, error)

	// Dedupe looks up a committed event by its namespaced dedupe key.
	Dedupe(ctx context.Context, key string) (Event, bool, error)

	// FindToolCall locates the committed assistant tool call with this id,
	// searching the whole session; the newest match wins. It is the pairing
	// check of a tool.result commit and it runs while the session write lock is
	// held, so implementations must keep it cheap: PGStore answers it with one
	// indexed lookup (schema/0002: the assistant-message scan index makes the
	// walk step only over assistant messages, the GIN index is the id-selective
	// alternative on large tables), MemStore with a reverse scan of the
	// in-memory log. See README.md for the recorded cost trade-off.
	FindToolCall(ctx context.Context, callID string) (ToolCallRef, bool, error)

	// Append stages one event. The sequence is allocated by the store and only
	// materializes on Commit; a rolled-back transaction leaves no gap.
	//
	// An edge-scoped user.message also wakes the session's run row in the same
	// transaction: the client's input and the "there is work" signal commit
	// together, so a crash in the HTTP layer's bookkeeping cannot strand the
	// input. The wake keeps a running run running (its finish probe decides
	// whether the input needs another turn) and never touches an external
	// session.
	//
	// Implementations mark transient failures (deadlock, serialization, lock
	// unavailable) with markRetryable so Kernel.Commit can replay them.
	Append(ctx context.Context, kind EventKind, payload json.RawMessage, dedupeKey string) (int64, error)

	// Commit applies the staged append.
	Commit(ctx context.Context) error

	// Rollback discards everything. It is idempotent and a no-op after Commit.
	Rollback(ctx context.Context) error
}
