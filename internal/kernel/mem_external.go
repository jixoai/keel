// MemStore's external-session and run-pin seams. They mirror the PostgreSQL
// semantics of external_pg.go and pin_pg.go:
//
//   - an external session is a session row with marks and NO run row, so it can
//     never be claimed;
//   - a pristine native session (no events, no run, no submissions) may be
//     promoted to external; a native session with history is refused;
//   - marking a backend unavailable only touches non-closed external sessions
//     of that backend and writes no event;
//   - the loadout pin is a fenced write under the session lock.
package kernel

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// memExternal is the mark set of an external session.
type memExternal struct {
	marks     ExternalMarks
	createdAt time.Time
	updatedAt time.Time
}

// isExternal reports whether the session is marked external.
func (s *memSession) isExternal() bool { return s.external != nil }

// UpsertExternalSession creates or refreshes an external session.
func (m *MemStore) UpsertExternalSession(ctx context.Context, scope Scope, marks ExternalMarks) (ExternalSession, error) {
	if err := ctx.Err(); err != nil {
		return ExternalSession{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	s, ok := m.sessions[scope]
	if !ok {
		s = &memSession{dedupe: map[string]int64{}, nextSeq: 1}
		m.sessions[scope] = s
		m.order = append(m.order, scope)
	} else if !s.isExternal() {
		// A native session may only be promoted while it is pristine: no
		// events, no run row. MemStore cannot represent submissions (they live
		// in the store layer and are written only after a committed event), so
		// the events check covers the same history; PG additionally probes
		// submissions for legacy or partially-written rows.
		if len(s.events) > 0 || s.run != nil {
			return ExternalSession{}, fmt.Errorf("memstore: session %s has native history: %w", scope, ErrExternalConflict)
		}
	} else {
		// An existing external session may only be refreshed idempotently by
		// its own handle: a different backend or reference is a conflict (the
		// marks are the single fact of "who owns this session"), and closed is
		// terminal. Mirrors the PostgreSQL rules.
		cur := s.external.marks
		if cur.Status == ExternalStatusClosed {
			return ExternalSession{}, fmt.Errorf("memstore: external session %s is closed: %w", scope, ErrExternalConflict)
		}
		if marks.Backend != cur.Backend {
			return ExternalSession{}, fmt.Errorf("memstore: external session %s belongs to backend %q, not %q: %w",
				scope, cur.Backend, marks.Backend, ErrExternalConflict)
		}
		if marks.Ref == "" {
			marks.Ref = cur.Ref // an unspecified ref never erases a known one
		}
		if cur.Ref != "" && marks.Ref != cur.Ref {
			return ExternalSession{}, fmt.Errorf("memstore: external session %s is bound to ref %q, not %q: %w",
				scope, cur.Ref, marks.Ref, ErrExternalConflict)
		}
	}
	if s.external == nil {
		s.external = &memExternal{createdAt: now}
	}
	s.external.marks = marks
	s.external.updatedAt = now
	return s.externalSession(scope), nil
}

// SetExternalStatus updates the external_status mark.
func (m *MemStore) SetExternalStatus(ctx context.Context, scope Scope, status string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[scope]
	if !ok {
		return fmt.Errorf("memstore: session %s: %w", scope, ErrNotFound)
	}
	if !s.isExternal() {
		return fmt.Errorf("memstore: session %s is not external: %w", scope, ErrExternalConflict)
	}
	if s.external.marks.Status == ExternalStatusClosed && status != ExternalStatusClosed {
		return fmt.Errorf("memstore: session %s is closed: %w", scope, ErrExternalConflict)
	}
	s.external.marks.Status = status
	s.external.updatedAt = m.now()
	return nil
}

// MarkBackendUnavailable marks every open session of a backend unavailable.
func (m *MemStore) MarkBackendUnavailable(ctx context.Context, tenantID uuid.UUID, backend string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, scope := range m.order {
		if scope.TenantID != tenantID {
			continue
		}
		s := m.sessions[scope]
		if s == nil || !s.isExternal() {
			continue
		}
		if s.external.marks.Backend != backend {
			continue
		}
		switch s.external.marks.Status {
		case ExternalStatusClosed, ExternalStatusUnavailable:
			continue
		}
		s.external.marks.Status = ExternalStatusUnavailable
		s.external.updatedAt = m.now()
		n++
	}
	return n, nil
}

// GetExternalSession reads one session's marks.
func (m *MemStore) GetExternalSession(ctx context.Context, scope Scope) (ExternalSession, bool, error) {
	if err := ctx.Err(); err != nil {
		return ExternalSession{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[scope]
	if !ok {
		return ExternalSession{}, false, nil
	}
	return s.externalSession(scope), true, nil
}

// ListExternalSessions lists a tenant's external sessions, newest first.
func (m *MemStore) ListExternalSessions(ctx context.Context, tenantID uuid.UUID) ([]ExternalSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ExternalSession
	for i := len(m.order) - 1; i >= 0; i-- {
		scope := m.order[i]
		if scope.TenantID != tenantID {
			continue
		}
		s := m.sessions[scope]
		if s == nil || !s.isExternal() {
			continue
		}
		out = append(out, s.externalSession(scope))
	}
	return out, nil
}

// externalSession builds the read model of one session row. A native session
// has empty marks (PG parity: the session's external_* columns are NULL and the
// scan coalesces them to ""); only an external row carries a status/metering.
func (s *memSession) externalSession(scope Scope) ExternalSession {
	out := ExternalSession{Scope: scope, ExternalMarks: ExternalMarks{
		StateOwner: StateOwnerKeel,
	}}
	if s.external == nil {
		return out
	}
	out.ExternalMarks = s.external.marks
	out.CreatedAt = s.external.createdAt
	out.UpdatedAt = s.external.updatedAt
	return out
}

// PinRunLoadout writes the loadout pin of a claimed run.
func (m *MemStore) PinRunLoadout(ctx context.Context, lease Lease, loadoutID string, revision int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[lease.Scope]
	if !ok {
		return fmt.Errorf("memstore: session %s: %w", lease.Scope, ErrNotFound)
	}
	if err := checkToken(m.leaseStateLocked(lease.Scope, s), lease.Token); err != nil {
		return fmt.Errorf("memstore: pin: %w", err)
	}
	if s.run == nil {
		return fmt.Errorf("memstore: pin: %w", ErrNoRunnable)
	}
	s.run.loadoutID = loadoutID
	s.run.loadoutRevision = revision
	return nil
}

// RecordRead writes the attempt's load high-water mark (monotonic, fenced).
func (m *MemStore) RecordRead(ctx context.Context, lease Lease, watermark int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[lease.Scope]
	if !ok {
		return fmt.Errorf("memstore: session %s: %w", lease.Scope, ErrNotFound)
	}
	if err := checkToken(m.leaseStateLocked(lease.Scope, s), lease.Token); err != nil {
		return fmt.Errorf("memstore: record read: %w", err)
	}
	if s.run == nil {
		return fmt.Errorf("memstore: record read: %w", ErrNoRunnable)
	}
	if watermark > s.run.readWatermark {
		s.run.readWatermark = watermark
	}
	return nil
}

// ReadRunPin answers the run row's pin.
func (m *MemStore) ReadRunPin(ctx context.Context, scope Scope) (RunPin, bool, error) {
	if err := ctx.Err(); err != nil {
		return RunPin{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[scope]
	if !ok {
		return RunPin{}, false, fmt.Errorf("memstore: session %s: %w", scope, ErrNotFound)
	}
	if s.run == nil {
		return RunPin{}, false, nil
	}
	state := m.leaseStateLocked(scope, s)
	pin := RunPin{
		Scope:             scope,
		Status:            s.run.status,
		Attempts:          s.run.attempts,
		Cursor:            s.run.cursor,
		LoadoutID:         s.run.loadoutID,
		LoadoutRevision:   s.run.loadoutRevision,
		LoadoutGeneration: s.run.loadoutGeneration,
		LoadoutOwnerEpoch: s.run.loadoutOwnerEpoch,
		PendingInput:      s.run.pendingInput,
		ReadWatermark:     s.run.readWatermark,
	}
	// Owner/epoch mirror the runs row: a claim sets them, a settle clears them
	// (PG parity), while the session authority keeps the last token.
	if state.Found {
		pin.Owner = state.Lease.Token.Owner
		pin.Epoch = state.Lease.Token.Epoch
	}
	return pin, true, nil
}
