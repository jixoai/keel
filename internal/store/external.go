// External sessions at the store layer (external-gateway spec: 外部会话数据模型).
//
// internal/kernel.PGStore owns the kernel-side seam of the same columns (the
// runtime path); this file is the thin store-level surface for callers that
// hold a *store.Store — admin listings, the gateway wiring — and it pins the
// same rules:
//
//   - an external session never gets a runs row, so store.EnqueueRun refuses it
//     (ErrExternalSession) and no native claim can return it;
//   - a pristine native session (no events, no run row, no submission) may be
//     promoted; a native session with history is refused (ErrExternalConflict);
//   - marking a backend unavailable only touches non-closed external sessions
//     of that backend and writes no event: no native recovery is fabricated.
//
// external_store_test.go runs both layers against the same database and asserts
// they agree, so the two implementations cannot drift silently.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// State owners (pinned against internal/kernel by a test).
const (
	StateOwnerKeel     = "keel"
	StateOwnerExternal = "external"
)

// External statuses.
const (
	ExternalStatusOpening     = "opening"
	ExternalStatusActive      = "active"
	ExternalStatusUnavailable = "backend-unavailable"
	ExternalStatusClosed      = "closed"
)

// Metering marks.
const (
	MeteringExternal = "external"
	MeteringNone     = "none"
)

// ErrExternalConflict reports that a session cannot be used as an external
// session: it is a native session with history.
var ErrExternalConflict = errors.New("store: session is already a native session")

// ErrExternalSession reports a native queue/run operation against a session
// marked external.
var ErrExternalSession = errors.New("store: session is an external session")

// ExternalMarks is the persisted mark set of an external session. The zero
// value normalizes to state_owner=external, status=active, metering=none.
type ExternalMarks struct {
	StateOwner string
	Backend    string
	Ref        string
	Status     string
	Metering   string
	Recovery   string
}

func (m ExternalMarks) normalize() (ExternalMarks, error) {
	if m.StateOwner == "" {
		m.StateOwner = StateOwnerExternal
	}
	if m.StateOwner != StateOwnerExternal {
		return ExternalMarks{}, fmt.Errorf("external sessions must be marked external, got state owner %q", m.StateOwner)
	}
	if m.Backend == "" {
		return ExternalMarks{}, errors.New("external_backend is required")
	}
	if m.Status == "" {
		m.Status = ExternalStatusActive
	}
	switch m.Status {
	case ExternalStatusOpening, ExternalStatusActive, ExternalStatusUnavailable, ExternalStatusClosed:
	default:
		return ExternalMarks{}, fmt.Errorf("unknown external status %q", m.Status)
	}
	if m.Metering == "" {
		m.Metering = MeteringNone
	}
	if m.Metering != MeteringExternal && m.Metering != MeteringNone {
		return ExternalMarks{}, fmt.Errorf("unknown metering mark %q", m.Metering)
	}
	return m, nil
}

// ExternalSession is one external session's persisted marks.
type ExternalSession struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	ExternalMarks
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CompatMode reports whether the session is an external (compatibility)
// session, for which Keel claims no native recovery.
func (s ExternalSession) CompatMode() bool { return s.StateOwner != StateOwnerKeel }

const externalColumns = `tenant_id, session_id, state_owner,
	COALESCE(external_backend, ''), COALESCE(external_ref, ''),
	COALESCE(external_status, ''), COALESCE(metering, ''), COALESCE(external_recovery, ''),
	created_at, COALESCE(external_updated_at, updated_at)`

func scanExternalSession(row pgx.Row) (ExternalSession, error) {
	var out ExternalSession
	err := row.Scan(&out.TenantID, &out.SessionID, &out.StateOwner, &out.Backend, &out.Ref,
		&out.Status, &out.Metering, &out.Recovery, &out.CreatedAt, &out.UpdatedAt)
	return out, err
}

// CreateExternalSession creates or refreshes an external session. It never
// creates a runs row.
func (s *Store) CreateExternalSession(ctx context.Context, tenantID, sessionID uuid.UUID, marks ExternalMarks) (ExternalSession, error) {
	normalized, err := marks.normalize()
	if err != nil {
		return ExternalSession{}, fmt.Errorf("store: external session: %w", err)
	}
	var out ExternalSession
	err = retryTx(ctx, func() error {
		var err error
		out, err = s.createExternalSessionOnce(ctx, tenantID, sessionID, normalized)
		return err
	})
	if err != nil {
		return ExternalSession{}, err
	}
	return out, nil
}

func (s *Store) createExternalSessionOnce(ctx context.Context, tenantID, sessionID uuid.UUID, marks ExternalMarks) (ExternalSession, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ExternalSession{}, fmt.Errorf("store: external begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var (
		stateOwner string
		nextSeq    int64
		curBackend *string
		curRef     *string
		curStatus  *string
	)
	err = tx.QueryRow(ctx,
		`SELECT state_owner, next_seq, external_backend, external_ref, external_status FROM sessions
		 WHERE tenant_id = $1 AND session_id = $2 FOR UPDATE`,
		tenantID, sessionID).Scan(&stateOwner, &nextSeq, &curBackend, &curRef, &curStatus)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := tx.Exec(ctx,
			`INSERT INTO sessions (tenant_id, session_id, state_owner, external_backend,
			                       external_ref, external_status, metering, external_recovery,
			                       external_updated_at)
			 VALUES ($1, $2, 'external', $3, NULLIF($4, ''), $5, $6, NULLIF($7, ''), now())`,
			tenantID, sessionID, marks.Backend, marks.Ref, marks.Status, marks.Metering, marks.Recovery); err != nil {
			return ExternalSession{}, fmt.Errorf("store: external insert: %w", err)
		}
	case err != nil:
		return ExternalSession{}, fmt.Errorf("store: external lock session: %w", err)
	case stateOwner == StateOwnerExternal:
		// Idempotent refresh only, pinned to the kernel's rules (the parity
		// test runs both layers against the same database): a different
		// backend or reference would strand the previous backend session and
		// silently rewrite who owns the log; closed is terminal.
		var curBackendV, curRefV, curStatusV string
		if curBackend != nil {
			curBackendV = *curBackend
		}
		if curRef != nil {
			curRefV = *curRef
		}
		if curStatus != nil {
			curStatusV = *curStatus
		}
		if curStatusV == ExternalStatusClosed {
			return ExternalSession{}, fmt.Errorf("store: external session %s/%s is closed: %w", tenantID, sessionID, ErrExternalConflict)
		}
		if marks.Backend != curBackendV {
			return ExternalSession{}, fmt.Errorf("store: external session %s/%s belongs to backend %q, not %q: %w",
				tenantID, sessionID, curBackendV, marks.Backend, ErrExternalConflict)
		}
		ref := marks.Ref
		if ref == "" {
			ref = curRefV // an unspecified ref never erases a known one
		}
		if curRefV != "" && ref != curRefV {
			return ExternalSession{}, fmt.Errorf("store: external session %s/%s is bound to ref %q, not %q: %w",
				tenantID, sessionID, curRefV, ref, ErrExternalConflict)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE sessions SET external_ref = NULLIF($3, ''), external_status = $4,
			                     metering = $5, external_recovery = NULLIF($6, ''),
			                     external_updated_at = now(), updated_at = now()
			 WHERE tenant_id = $1 AND session_id = $2 AND state_owner = 'external'`,
			tenantID, sessionID, ref, marks.Status, marks.Metering, marks.Recovery); err != nil {
			return ExternalSession{}, fmt.Errorf("store: external update: %w", err)
		}
	default:
		var history bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM sessions
			                WHERE tenant_id = $1 AND session_id = $2 AND next_seq > 1)
			        OR EXISTS (SELECT 1 FROM runs WHERE tenant_id = $1 AND session_id = $2)
			        OR EXISTS (SELECT 1 FROM submissions WHERE tenant_id = $1 AND session_id = $2)`,
			tenantID, sessionID).Scan(&history); err != nil {
			return ExternalSession{}, fmt.Errorf("store: external history probe: %w", err)
		}
		if history {
			return ExternalSession{}, fmt.Errorf("store: session %s/%s has native history: %w", tenantID, sessionID, ErrExternalConflict)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE sessions SET state_owner = 'external', external_backend = $3,
			                     external_ref = NULLIF($4, ''), external_status = $5,
			                     metering = $6, external_recovery = NULLIF($7, ''),
			                     external_updated_at = now(), updated_at = now()
			 WHERE tenant_id = $1 AND session_id = $2`,
			tenantID, sessionID, marks.Backend, marks.Ref, marks.Status, marks.Metering, marks.Recovery); err != nil {
			return ExternalSession{}, fmt.Errorf("store: external update: %w", err)
		}
	}

	out, err := scanExternalSession(tx.QueryRow(ctx,
		`SELECT `+externalColumns+` FROM sessions WHERE tenant_id = $1 AND session_id = $2`,
		tenantID, sessionID))
	if err != nil {
		return ExternalSession{}, fmt.Errorf("store: external read back: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ExternalSession{}, fmt.Errorf("store: external commit: %w", err)
	}
	return out, nil
}

// SetExternalStatus updates the external_status mark of an external session.
func (s *Store) SetExternalStatus(ctx context.Context, tenantID, sessionID uuid.UUID, status string) error {
	return retryTx(ctx, func() error {
		tag, err := s.pool.Exec(ctx,
			`UPDATE sessions SET external_status = $3, external_updated_at = now(), updated_at = now()
			 WHERE tenant_id = $1 AND session_id = $2
			   AND state_owner = 'external'
			   AND (external_status IS NULL OR external_status <> 'closed' OR $3 = 'closed')`,
			tenantID, sessionID, status)
		if err != nil {
			return fmt.Errorf("store: external status: %w", err)
		}
		if tag.RowsAffected() == 0 {
			var owner string
			var current *string
			err := s.pool.QueryRow(ctx,
				`SELECT state_owner, external_status FROM sessions
				 WHERE tenant_id = $1 AND session_id = $2`, tenantID, sessionID).Scan(&owner, &current)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("store: external status: %w", ErrNoSession)
			}
			if err != nil {
				return fmt.Errorf("store: external status probe: %w", err)
			}
			if owner != StateOwnerExternal {
				return fmt.Errorf("store: session %s/%s is not external: %w", tenantID, sessionID, ErrExternalConflict)
			}
			return fmt.Errorf("store: session %s/%s is closed: %w", tenantID, sessionID, ErrExternalConflict)
		}
		return nil
	})
}

// MarkBackendUnavailable marks every open session of a backend unavailable and
// reports how many rows changed.
func (s *Store) MarkBackendUnavailable(ctx context.Context, tenantID uuid.UUID, backend string) (int64, error) {
	var n int64
	err := retryTx(ctx, func() error {
		tag, err := s.pool.Exec(ctx,
			`UPDATE sessions SET external_status = 'backend-unavailable',
			                     external_updated_at = now(), updated_at = now()
			 WHERE tenant_id = $1 AND state_owner = 'external' AND external_backend = $2
			   AND external_status IS DISTINCT FROM 'closed'
			   AND external_status IS DISTINCT FROM 'backend-unavailable'`,
			tenantID, backend)
		if err != nil {
			return fmt.Errorf("store: mark backend unavailable: %w", err)
		}
		n = tag.RowsAffected()
		return nil
	})
	return n, err
}

// GetExternalSession reads one session's marks. found=false means the session
// row does not exist (including another tenant's).
func (s *Store) GetExternalSession(ctx context.Context, tenantID, sessionID uuid.UUID) (ExternalSession, bool, error) {
	out, err := scanExternalSession(s.pool.QueryRow(ctx,
		`SELECT `+externalColumns+` FROM sessions WHERE tenant_id = $1 AND session_id = $2`,
		tenantID, sessionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ExternalSession{}, false, nil
	}
	if err != nil {
		return ExternalSession{}, false, fmt.Errorf("store: external read: %w", err)
	}
	return out, true, nil
}

// SessionOwner reads the session's state_owner. found=false means the session
// row does not exist (including another tenant's). The edge uses it as a
// pre-commit class check: an external session must reject native input before
// the kernel appends anything, instead of committing and then failing to queue
// a run.
func (s *Store) SessionOwner(ctx context.Context, tenantID, sessionID uuid.UUID) (string, bool, error) {
	var owner string
	err := s.pool.QueryRow(ctx,
		`SELECT state_owner FROM sessions WHERE tenant_id = $1 AND session_id = $2`,
		tenantID, sessionID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: session owner: %w", err)
	}
	return owner, true, nil
}

// ListExternalSessions lists a tenant's external sessions, newest first.
func (s *Store) ListExternalSessions(ctx context.Context, tenantID uuid.UUID) ([]ExternalSession, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+externalColumns+` FROM sessions
		 WHERE tenant_id = $1 AND state_owner = 'external'
		 ORDER BY COALESCE(external_updated_at, updated_at) DESC, session_id`,
		tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: list external sessions: %w", err)
	}
	defer rows.Close()
	var out []ExternalSession
	for rows.Next() {
		session, err := scanExternalSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan external session: %w", err)
		}
		out = append(out, session)
	}
	return out, rows.Err()
}

// retryTx replays a whole transaction on transient failures (deadlock,
// serialization failure, lock unavailable), mirroring the kernel's budget.
func retryTx(ctx context.Context, fn func() error) error {
	return retryTxBudget(ctx, 6, fn)
}

// retryTxBudget is retryTx with an explicit attempt budget. The queue-wake
// path uses a longer one: a concurrent migration (ALTER TABLE) holds ACCESS
// EXCLUSIVE on runs/sessions, and the wake statement's own lock order can
// deadlock with it, so the survivor has to outlast the migration's own retry
// loop.
func retryTxBudget(ctx context.Context, budget int, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt < budget; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		if !isRetryableTx(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(30+attempt*80) * time.Millisecond):
		}
	}
	return lastErr
}
