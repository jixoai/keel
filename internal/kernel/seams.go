package kernel

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// This file declares the narrow interfaces a service (or the runtime's own
// worker and HTTP edge) may depend on. *Kernel implements all three; nothing
// else is ever handed to a service, so a plugin cannot reach a table handle,
// allocate a sequence or bypass the tenant/lease checks (see README.md, and
// the change's requirement "事件只能经内核提交").
//
// The runtime wiring depends on these interfaces rather than on *Kernel so the
// loadout can substitute a test double and so the dependency is visible in the
// constructor signatures.

// Committer is the write seam: commit one validated event, or ask whether an
// idempotency key already has a committed fact (the "reuse the existing
// generation instead of calling the model again" query).
type Committer interface {
	Commit(ctx context.Context, req CommitRequest) (Receipt, error)
	Lookup(ctx context.Context, scope Scope, key KeyRef) (Receipt, bool, error)
}

// Reader is the read seam. Both methods carry the tenant boundary: a foreign
// or missing session answers ErrNotFound, never an empty page.
type Reader interface {
	LoadEvents(ctx context.Context, scope Scope, afterSeq int64, limit int) ([]Event, error)
	UnresolvedToolCalls(ctx context.Context, scope Scope) ([]ToolCallRef, error)
}

// Checkpoint is one compaction projection: the summary of a successful
// compaction (keyed by the compaction event's seq) plus the seq of the first
// event the compaction retained, the token accounting and the policy identity
// it was produced under. It is derived state; the event log stays the only
// truth and is never shortened.
type Checkpoint struct {
	// UptoSeq is the seq of the compaction event that produced this checkpoint.
	UptoSeq int64
	// KeptFromSeq is the seq of the first event retained by the compaction
	// (the tail that must be replayed after Summary). Zero means the host has
	// not anchored the tail yet; a replay then falls back to the event log
	// rather than guessing.
	KeptFromSeq int64
	Summary     string
	// TokensBefore is the context-token estimate before the compaction.
	TokensBefore int
	// TokensAfter is the context-token estimate after the projection
	// (summary + retained tail).
	TokensAfter int
	// PolicyVersion is the effective CheckpointPolicy identity at generation
	// time. It explains differences between checkpoints; it never enables a
	// replay on its own.
	PolicyVersion string
	// SummaryHash is sha256(summary), hex encoded. It is the projection's
	// integrity fact: recovery refuses a row whose summary no longer matches it.
	// Rows written before the column existed carry "" and are not treated as
	// verified.
	SummaryHash string
	// RequestID is the manual-compaction requestId, empty for automatic
	// compactions. It is the idempotency key of the manual surface.
	RequestID string
	// CreatedAt is when the projection row was written (the TTL clock).
	CreatedAt time.Time
}

// CheckpointRecord is the durable input of one checkpoint save. UptoSeq is the
// compaction event's sequence and therefore the idempotency boundary of an
// automatic compaction; RequestID carries the manual surface's idempotency.
type CheckpointRecord struct {
	UptoSeq       int64
	KeptFromSeq   int64
	Summary       string
	TokensBefore  int
	TokensAfter   int
	PolicyVersion string
	// SummaryHash may be supplied by the caller; an empty value is computed
	// from Summary by the kernel before the write. A supplied value that does
	// not match Summary is refused.
	SummaryHash string
	RequestID   string
}

// PruneRequest bounds the checkpoint projections one session keeps. Both
// dimensions are projections only: pruning never deletes or rewrites events,
// and the newest checkpoint is always retained so a running session can still
// replay from it until a newer one replaces it.
type PruneRequest struct {
	// MaxCheckpoints keeps at most this many newest projections. Zero means no
	// count limit.
	MaxCheckpoints int
	// TTL deletes projections older than this. Zero means no expiry. The newest
	// projection is exempt: it is the only recovery anchor of the session.
	TTL time.Duration
}

// CheckpointStore is the compaction-projection seam. Saving is best-effort by
// contract: a failure must never roll back or hide the committed compaction
// event, and the caller logs it instead of pretending the checkpoint exists.
// Loading is tenant-bound and returns found=false when no checkpoint exists.
type CheckpointStore interface {
	SaveCheckpoint(ctx context.Context, scope Scope, rec CheckpointRecord) error
	LoadCheckpoint(ctx context.Context, scope Scope) (Checkpoint, bool, error)
}

// CheckpointPruner is the retention half of the checkpoint seam. It deletes
// projections only; the event log is untouched and stays readable. The newest
// checkpoint survives every pruning pass.
type CheckpointPruner interface {
	PruneCheckpoints(ctx context.Context, scope Scope, req PruneRequest) (int64, error)
}

// CheckpointRequestPruneRequest bounds the manual-compaction idempotency ledger
// independently of the projection: a projection can be pruned while the
// requestId still replays, but the ledger itself must not grow without bound.
// Both bounds are optional; at least one must be positive.
type CheckpointRequestPruneRequest struct {
	// MaxRequests keeps at most this many newest request rows per session.
	MaxRequests int
	// TTL deletes request rows older than this. Zero means no expiry.
	TTL time.Duration
}

// CheckpointRequestPruner is the retention half of the idempotency ledger. It
// never touches `checkpoints` or `events`.
type CheckpointRequestPruner interface {
	PruneCheckpointRequests(ctx context.Context, scope Scope, req CheckpointRequestPruneRequest) (int64, error)
}

// CheckpointRequestLookup resolves the manual-compaction idempotency key. It is
// separate from CheckpointStore so a host that only implements the automatic
// projection path keeps compiling; a missing implementation means the caller
// cannot replay a requestId and must not guess.
type CheckpointRequestLookup interface {
	CheckpointByRequest(ctx context.Context, scope Scope, requestID string) (Checkpoint, bool, error)
}

// Compaction task phases mirror PI's CompactionCheckpoint phases
// (packages/durable/src/harness/compaction.ts): a task starts at select, pins
// the range it will summarize, and may sleep in retry. The task checkpoint is
// independent of the compaction projection (`checkpoints`): it records
// recovery state for an interrupted run, never a replayable transcript.
const (
	CompactionPhaseSelect    = "select"
	CompactionPhaseSummarize = "summarize"
	CompactionPhaseRetry     = "retry"
)

// CompactionTaskCheckpoint is the durable recovery state of one compaction
// task. It is keyed by the task's request ID (manual surface) or a derived
// `auto:<seq>` identity for automatic compaction.
//
// Phase select has no range yet. Phase summarize pins FirstKeptSeq (the seq of
// the first committed event the summary will retain) and TailSeq (the last
// committed seq covered by the selection at pin time); those two facts let a
// restarted task re-run the summary without re-selecting a different range.
// Phase retry adds RetryUntil.
type CompactionTaskCheckpoint struct {
	RequestID     string
	Phase         string
	FirstKeptSeq  int64
	TailSeq       int64
	TokensBefore  int
	PolicyVersion string
	Attempt       int
	RetryUntil    time.Time
}

// CompactionTaskStore is the compaction-task seam. Saving is an upsert keyed
// by request ID; deleting a finished task never touches the event log or the
// checkpoint projection. Every method carries the tenant boundary.
type CompactionTaskStore interface {
	SaveCompactionTask(ctx context.Context, scope Scope, checkpoint CompactionTaskCheckpoint) error
	LoadCompactionTask(ctx context.Context, scope Scope, requestID string) (CompactionTaskCheckpoint, bool, error)
	ListCompactionTasks(ctx context.Context, scope Scope) ([]CompactionTaskCheckpoint, error)
	DeleteCompactionTask(ctx context.Context, scope Scope, requestID string) error
}

// ReadMarker is the durable read-point seam. A run records where its transcript
// ended immediately after loading the log, before the first model call: a
// process killed between then and Finish must still leave the fact behind, or
// the successor cannot tell which input the attempt never saw (adversarial
// review N1/N2).
type ReadMarker interface {
	RecordRead(ctx context.Context, lease Lease, watermark int64) error
}

// LeaseManager is the lease seam: claim, keep alive, settle and hand back run
// leases. Every verb is fenced by the owner token issued by Claim/ClaimScope.
type LeaseManager interface {
	Claim(ctx context.Context, owner string, ttl time.Duration) (Lease, error)
	ClaimScope(ctx context.Context, scope Scope, owner string, ttl time.Duration) (Lease, error)
	Renew(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error)
	Finish(ctx context.Context, req FinishRequest) error
	Release(ctx context.Context, lease Lease) error
}

// ExternalSessions is the external-session seam: the marks of sessions whose
// state an external backend owns (external-gateway spec: 外部会话数据模型).
// Every method carries the tenant boundary, and the operations can never create
// a run row or a lease: an external session is structurally outside the native
// flow.
type ExternalSessions interface {
	OpenExternalSession(ctx context.Context, scope Scope, marks ExternalMarks) (ExternalSession, error)
	SetExternalStatus(ctx context.Context, scope Scope, status string) error
	MarkBackendUnavailable(ctx context.Context, tenantID uuid.UUID, backend string) (int64, error)
	ExternalSession(ctx context.Context, scope Scope) (ExternalSession, error)
	ListExternalSessions(ctx context.Context, tenantID uuid.UUID) ([]ExternalSession, error)
}

var (
	_ Committer           = (*Kernel)(nil)
	_ Reader              = (*Kernel)(nil)
	_ CheckpointStore     = (*Kernel)(nil)
	_ CompactionTaskStore = (*Kernel)(nil)
	_ ReadMarker          = (*Kernel)(nil)
	_ LeaseManager        = (*Kernel)(nil)
	_ ExternalSessions    = (*Kernel)(nil)
)
