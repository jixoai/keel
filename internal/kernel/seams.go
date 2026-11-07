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
	_ Committer        = (*Kernel)(nil)
	_ Reader           = (*Kernel)(nil)
	_ ReadMarker       = (*Kernel)(nil)
	_ LeaseManager     = (*Kernel)(nil)
	_ ExternalSessions = (*Kernel)(nil)
)
