// External sessions: the durable mark set of a session whose state a backend
// owns (external-gateway spec: 外部会话数据模型).
//
// Keel records the marks and, when it chooses to, high-level messages; it does
// NOT run these sessions. An external session has no runs row, so ClaimLease
// and ClaimScope can never return it, and store.EnqueueRun refuses to create
// one: "外部会话 MUST NOT 进入原生 run/lease 作业流" is enforced structurally
// rather than by convention.
//
// The vocabulary below is pinned against internal/gateway's wire marks by
// gateway's cross-check test; the strings are the durable contract.
package kernel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// State owners. keel is the default for every existing session.
const (
	StateOwnerKeel     = "keel"
	StateOwnerExternal = "external"
)

// external_status marks. backend-unavailable is a queryable terminal state
// (the external-gateway spec's "backend 崩溃" scenario): Keel records it and
// never claims a native recovery for the session.
const (
	ExternalStatusOpening     = "opening"
	ExternalStatusActive      = "active"
	ExternalStatusUnavailable = "backend-unavailable"
	ExternalStatusClosed      = "closed"
)

// Metering marks. An external session is metering=external when the backend
// accounts for its own usage, and metering=none when its usage cannot be
// measured at all; a missing usage record is never recorded as zero usage.
const (
	MeteringExternal = "external"
	MeteringNone     = "none"
)

// validExternalStatus reports whether status is a registered external_status.
func validExternalStatus(status string) bool {
	switch status {
	case ExternalStatusOpening, ExternalStatusActive, ExternalStatusUnavailable, ExternalStatusClosed:
		return true
	}
	return false
}

// validMetering reports whether mark is a registered metering mark.
func validMetering(mark string) bool {
	return mark == MeteringExternal || mark == MeteringNone
}

// ExternalMarks is the mark set persisted for an external session. The zero
// value normalizes to the safe reading: state_owner=external, status=active,
// metering=none.
type ExternalMarks struct {
	// StateOwner is keel or external. The zero value (or "external") marks an
	// external session; "keel" is refused here (a native session is created by
	// the edge, not by a backend).
	StateOwner string
	// Backend is the backend name from the gateway's initialize (required).
	Backend string
	// Ref is the backend's own session handle. It is opaque to Keel and
	// meaningless outside the backend that issued it.
	Ref string
	// Status is the external_status mark; "" means active.
	Status string
	// Metering is external or none; "" means none.
	Metering string
	// Recovery is the backend's declared recovery mode for the session, if any.
	Recovery string
}

// normalize fills the safe defaults and validates the marks.
func (m ExternalMarks) normalize() (ExternalMarks, error) {
	if m.StateOwner == "" {
		m.StateOwner = StateOwnerExternal
	}
	if m.StateOwner != StateOwnerExternal {
		return ExternalMarks{}, fmt.Errorf("state owner %q: an external session must be marked external", m.StateOwner)
	}
	if m.Backend == "" {
		return ExternalMarks{}, errors.New("external_backend is required")
	}
	if m.Status == "" {
		m.Status = ExternalStatusActive
	}
	if !validExternalStatus(m.Status) {
		return ExternalMarks{}, fmt.Errorf("unknown external status %q", m.Status)
	}
	if m.Metering == "" {
		m.Metering = MeteringNone
	}
	if !validMetering(m.Metering) {
		return ExternalMarks{}, fmt.Errorf("unknown metering mark %q", m.Metering)
	}
	return m, nil
}

// ExternalSession is one session's persisted marks plus its scope.
type ExternalSession struct {
	Scope Scope
	ExternalMarks
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CompatMode reports whether the session is an external (compatibility)
// session: Keel does not promise native kill-9 recovery or fencing handoff for
// it.
func (s ExternalSession) CompatMode() bool { return s.StateOwner != StateOwnerKeel }

// ErrExternalConflict reports that a session cannot be used as an external
// session: it is a native session that already has history (events, a run row
// or submissions), so promoting it would hide where its state lives.
var ErrExternalConflict = errors.New("kernel: session is already a native session")

// ErrNoExternalStore reports that the kernel's log store does not implement
// external-session persistence.
var ErrNoExternalStore = errors.New("kernel: store does not support external sessions")

// ExternalStore is the persistence seam of external-session marks. MemStore
// and PGStore implement it; Kernel exposes the operations with the scope and
// vocabulary validated.
type ExternalStore interface {
	// UpsertExternalSession creates an external session or refreshes the marks
	// of an existing one. A pristine native session (no events, no run row, no
	// submissions) may be promoted; a native session with history answers
	// ErrExternalConflict.
	UpsertExternalSession(ctx context.Context, scope Scope, marks ExternalMarks) (ExternalSession, error)
	// SetExternalStatus updates the external_status of an external session.
	SetExternalStatus(ctx context.Context, scope Scope, status string) error
	// MarkBackendUnavailable marks every non-closed external session of a
	// backend backend-unavailable and reports how many rows changed.
	MarkBackendUnavailable(ctx context.Context, tenantID uuid.UUID, backend string) (int64, error)
	// GetExternalSession reads one session's marks (nil, false for absent).
	GetExternalSession(ctx context.Context, scope Scope) (ExternalSession, bool, error)
	// ListExternalSessions lists a tenant's external sessions, newest first.
	ListExternalSessions(ctx context.Context, tenantID uuid.UUID) ([]ExternalSession, error)
}

// externalStore returns the seam, or an error when the store has none.
func (k *Kernel) externalStore() (ExternalStore, error) {
	store, ok := k.log.(ExternalStore)
	if !ok {
		return nil, &Error{Reason: ReasonInvalidRequest, Op: OpExternal, Detail: ErrNoExternalStore.Error(), Err: ErrNoExternalStore}
	}
	return store, nil
}

// OpenExternalSession creates (or refreshes) an external session: the marks
// are persisted, no run is enqueued and the session never enters the native
// claim flow. A native session with history is refused with ErrExternalConflict.
func (k *Kernel) OpenExternalSession(ctx context.Context, scope Scope, marks ExternalMarks) (ExternalSession, error) {
	if scope.IsZero() {
		return ExternalSession{}, &Error{Reason: ReasonInvalidRequest, Op: OpExternal, Scope: scope, Detail: "tenant and session are required", Err: ErrInvalidRequest}
	}
	normalized, err := marks.normalize()
	if err != nil {
		return ExternalSession{}, &Error{Reason: ReasonInvalidRequest, Op: OpExternal, Scope: scope, Detail: err.Error(), Err: fmt.Errorf("%w: %s", ErrInvalidRequest, err)}
	}
	store, err := k.externalStore()
	if err != nil {
		return ExternalSession{}, err
	}
	session, err := store.UpsertExternalSession(ctx, scope, normalized)
	if err != nil {
		return ExternalSession{}, classifyExternalStoreError(OpExternal, scope, err)
	}
	return session, nil
}

// SetExternalStatus updates the external_status mark of an external session.
// A closed session is terminal: reopening it is a new session on the backend.
func (k *Kernel) SetExternalStatus(ctx context.Context, scope Scope, status string) error {
	if scope.IsZero() {
		return &Error{Reason: ReasonInvalidRequest, Op: OpExternal, Scope: scope, Detail: "tenant and session are required", Err: ErrInvalidRequest}
	}
	if !validExternalStatus(status) {
		return &Error{Reason: ReasonInvalidRequest, Op: OpExternal, Scope: scope, Detail: fmt.Sprintf("unknown external status %q", status), Err: ErrInvalidRequest}
	}
	store, err := k.externalStore()
	if err != nil {
		return err
	}
	if err := store.SetExternalStatus(ctx, scope, status); err != nil {
		return classifyExternalStoreError(OpExternal, scope, err)
	}
	return nil
}

// MarkBackendUnavailable marks every open session of a backend unavailable.
// It is the crash path of the external-gateway spec: the sessions become a
// queryable terminal state, no event is written and no native recovery is
// claimed.
func (k *Kernel) MarkBackendUnavailable(ctx context.Context, tenantID uuid.UUID, backend string) (int64, error) {
	if tenantID == uuid.Nil {
		return 0, &Error{Reason: ReasonInvalidRequest, Op: OpExternal, Detail: "tenant is required", Err: ErrInvalidRequest}
	}
	if backend == "" {
		return 0, &Error{Reason: ReasonInvalidRequest, Op: OpExternal, Detail: "backend name is required", Err: ErrInvalidRequest}
	}
	store, err := k.externalStore()
	if err != nil {
		return 0, err
	}
	n, err := store.MarkBackendUnavailable(ctx, tenantID, backend)
	if err != nil {
		return 0, classifyExternalStoreError(OpExternal, Scope{TenantID: tenantID}, err)
	}
	return n, nil
}

// ExternalSession reads one session's marks. A native session answers with
// StateOwner=keel (so callers can tell the two apart); an absent session
// answers ErrNotFound, and another tenant's session answers identically.
func (k *Kernel) ExternalSession(ctx context.Context, scope Scope) (ExternalSession, error) {
	if scope.IsZero() {
		return ExternalSession{}, &Error{Reason: ReasonInvalidRequest, Op: OpExternal, Scope: scope, Detail: "tenant and session are required", Err: ErrInvalidRequest}
	}
	store, err := k.externalStore()
	if err != nil {
		return ExternalSession{}, err
	}
	session, found, err := store.GetExternalSession(ctx, scope)
	if err != nil {
		return ExternalSession{}, classifyExternalStoreError(OpExternal, scope, err)
	}
	if !found {
		return ExternalSession{}, &Error{Reason: ReasonNotFound, Op: OpExternal, Scope: scope, Err: ErrNotFound}
	}
	return session, nil
}

// ListExternalSessions lists a tenant's external sessions, newest first.
func (k *Kernel) ListExternalSessions(ctx context.Context, tenantID uuid.UUID) ([]ExternalSession, error) {
	if tenantID == uuid.Nil {
		return nil, &Error{Reason: ReasonInvalidRequest, Op: OpExternal, Detail: "tenant is required", Err: ErrInvalidRequest}
	}
	store, err := k.externalStore()
	if err != nil {
		return nil, err
	}
	sessions, err := store.ListExternalSessions(ctx, tenantID)
	if err != nil {
		return nil, classifyExternalStoreError(OpExternal, Scope{TenantID: tenantID}, err)
	}
	return sessions, nil
}

// classifyExternalStoreError maps the seam's sentinels onto kernel rejections.
func classifyExternalStoreError(op string, scope Scope, err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return &Error{Reason: ReasonNotFound, Op: op, Scope: scope, Err: ErrNotFound}
	case errors.Is(err, ErrExternalConflict):
		return &Error{Reason: ReasonExternalConflict, Op: op, Scope: scope, Detail: err.Error(), Err: fmt.Errorf("%w: %s", ErrExternalConflict, err)}
	default:
		return &Error{Reason: ReasonStoreFailure, Op: op, Scope: scope, Detail: err.Error(), Err: fmt.Errorf("%w: %w", ErrStoreFailure, err)}
	}
}
