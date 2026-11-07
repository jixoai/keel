// PGStore's external-session and run-pin seams.
//
// External-session writes never create a runs row, so an external session is
// structurally unclaimable; a pristine native session may be promoted, while a
// native session with history is refused (ErrExternalConflict). Marking a
// backend unavailable touches only non-closed external sessions of that backend
// and writes no event: Keel never fabricates a recovery for them.
package kernel

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// externalColumns is the shared select list of the external marks.
const externalColumns = `tenant_id, session_id, state_owner,
	COALESCE(external_backend, ''), COALESCE(external_ref, ''),
	COALESCE(external_status, ''), COALESCE(metering, ''), COALESCE(external_recovery, ''),
	created_at, COALESCE(external_updated_at, updated_at)`

// scanExternal reads one external marks row.
func scanExternal(row pgx.Row) (ExternalSession, error) {
	var out ExternalSession
	if err := row.Scan(&out.Scope.TenantID, &out.Scope.SessionID, &out.ExternalMarks.StateOwner,
		&out.ExternalMarks.Backend, &out.ExternalMarks.Ref, &out.ExternalMarks.Status,
		&out.ExternalMarks.Metering, &out.ExternalMarks.Recovery,
		&out.CreatedAt, &out.UpdatedAt); err != nil {
		return ExternalSession{}, err
	}
	return out, nil
}

// UpsertExternalSession creates an external session or refreshes its marks.
// The whole check runs in one transaction that locks the session row, so two
// concurrent opens cannot race the pristine check.
func (s *PGStore) UpsertExternalSession(ctx context.Context, scope Scope, marks ExternalMarks) (ExternalSession, error) {
	var out ExternalSession
	err := s.retryTx(ctx, func() error {
		var err error
		out, err = s.upsertExternalSession(ctx, scope, marks)
		return err
	})
	if err != nil {
		return ExternalSession{}, err
	}
	return out, nil
}

func (s *PGStore) upsertExternalSession(ctx context.Context, scope Scope, marks ExternalMarks) (ExternalSession, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ExternalSession{}, fmt.Errorf("kernel: external begin: %w", pgFail(err))
	}
	defer tx.Rollback(ctx)

	var (
		stateOwner string
		nextSeq    int64
	)
	err = tx.QueryRow(ctx,
		`SELECT state_owner, next_seq FROM sessions
		 WHERE tenant_id = $1 AND session_id = $2 FOR UPDATE`,
		scope.TenantID, scope.SessionID).Scan(&stateOwner, &nextSeq)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := tx.Exec(ctx,
			`INSERT INTO sessions (tenant_id, session_id, state_owner, external_backend,
			                       external_ref, external_status, metering, external_recovery,
			                       external_updated_at)
			 VALUES ($1, $2, 'external', $3, NULLIF($4, ''), $5, $6, NULLIF($7, ''), now())`,
			scope.TenantID, scope.SessionID, marks.Backend, marks.Ref,
			marks.Status, marks.Metering, marks.Recovery); err != nil {
			return ExternalSession{}, fmt.Errorf("kernel: external insert: %w", pgFail(err))
		}
	case err != nil:
		return ExternalSession{}, fmt.Errorf("kernel: external lock session: %w", pgFail(err))
	case stateOwner != StateOwnerExternal:
		// A native session may only be promoted while it is pristine: nothing
		// in the log, no run row, no submission. Anything else is already owned
		// by the native flow and must not be dressed up as external.
		var history bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM sessions
			                WHERE tenant_id = $1 AND session_id = $2 AND next_seq > 1)
			        OR EXISTS (SELECT 1 FROM runs WHERE tenant_id = $1 AND session_id = $2)
			        OR EXISTS (SELECT 1 FROM submissions WHERE tenant_id = $1 AND session_id = $2)`,
			scope.TenantID, scope.SessionID).Scan(&history); err != nil {
			return ExternalSession{}, fmt.Errorf("kernel: external history probe: %w", pgFail(err))
		}
		if history {
			return ExternalSession{}, fmt.Errorf("kernel: session %s is a native session with history: %w", scope, ErrExternalConflict)
		}
		fallthrough
	default:
		if _, err := tx.Exec(ctx,
			`UPDATE sessions SET state_owner = 'external', external_backend = $3,
			                     external_ref = NULLIF($4, ''), external_status = $5,
			                     metering = $6, external_recovery = NULLIF($7, ''),
			                     external_updated_at = now(), updated_at = now()
			 WHERE tenant_id = $1 AND session_id = $2`,
			scope.TenantID, scope.SessionID, marks.Backend, marks.Ref,
			marks.Status, marks.Metering, marks.Recovery); err != nil {
			return ExternalSession{}, fmt.Errorf("kernel: external update: %w", pgFail(err))
		}
	}

	row := tx.QueryRow(ctx,
		`SELECT `+externalColumns+` FROM sessions WHERE tenant_id = $1 AND session_id = $2`,
		scope.TenantID, scope.SessionID)
	out, err := scanExternal(row)
	if err != nil {
		return ExternalSession{}, fmt.Errorf("kernel: external read back: %w", pgFail(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return ExternalSession{}, fmt.Errorf("kernel: external commit: %w", pgFail(err))
	}
	return out, nil
}

// SetExternalStatus updates the external_status of an external session. A
// closed session stays closed (reopening is a new session on the backend).
func (s *PGStore) SetExternalStatus(ctx context.Context, scope Scope, status string) error {
	return s.retryTx(ctx, func() error {
		tag, err := s.pool.Exec(ctx,
			`UPDATE sessions SET external_status = $3, external_updated_at = now(), updated_at = now()
			 WHERE tenant_id = $1 AND session_id = $2
			   AND state_owner = 'external'
			   AND (external_status IS NULL OR external_status <> 'closed' OR $3 = 'closed')`,
			scope.TenantID, scope.SessionID, status)
		if err != nil {
			return fmt.Errorf("kernel: external status: %w", pgFail(err))
		}
		if tag.RowsAffected() == 0 {
			// Explain: absent session, native session, or a closed session.
			var owner string
			var current *string
			err := s.pool.QueryRow(ctx,
				`SELECT state_owner, external_status FROM sessions
				 WHERE tenant_id = $1 AND session_id = $2`,
				scope.TenantID, scope.SessionID).Scan(&owner, &current)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("kernel: external status: %w", ErrNotFound)
			}
			if err != nil {
				return fmt.Errorf("kernel: external status probe: %w", pgFail(err))
			}
			if owner != StateOwnerExternal {
				return fmt.Errorf("kernel: session %s is not external: %w", scope, ErrExternalConflict)
			}
			return fmt.Errorf("kernel: session %s is closed: %w", scope, ErrExternalConflict)
		}
		return nil
	})
}

// MarkBackendUnavailable marks every open session of a backend unavailable and
// reports how many rows changed. No event is written; the status is the fact.
func (s *PGStore) MarkBackendUnavailable(ctx context.Context, tenantID uuid.UUID, backend string) (int64, error) {
	var n int64
	err := s.retryTx(ctx, func() error {
		tag, err := s.pool.Exec(ctx,
			`UPDATE sessions SET external_status = 'backend-unavailable',
			                     external_updated_at = now(), updated_at = now()
			 WHERE tenant_id = $1 AND state_owner = 'external' AND external_backend = $2
			   AND external_status IS DISTINCT FROM 'closed'
			   AND external_status IS DISTINCT FROM 'backend-unavailable'`,
			tenantID, backend)
		if err != nil {
			return fmt.Errorf("kernel: mark backend unavailable: %w", pgFail(err))
		}
		n = tag.RowsAffected()
		return nil
	})
	return n, err
}

// GetExternalSession reads one session's marks. found=false means the session
// row does not exist (including another tenant's).
func (s *PGStore) GetExternalSession(ctx context.Context, scope Scope) (ExternalSession, bool, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+externalColumns+` FROM sessions WHERE tenant_id = $1 AND session_id = $2`,
		scope.TenantID, scope.SessionID)
	out, err := scanExternal(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return ExternalSession{}, false, nil
	}
	if err != nil {
		return ExternalSession{}, false, fmt.Errorf("kernel: external read: %w", pgFail(err))
	}
	return out, true, nil
}

// ListExternalSessions lists a tenant's external sessions, newest first.
func (s *PGStore) ListExternalSessions(ctx context.Context, tenantID uuid.UUID) ([]ExternalSession, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+externalColumns+` FROM sessions
		 WHERE tenant_id = $1 AND state_owner = 'external'
		 ORDER BY COALESCE(external_updated_at, updated_at) DESC, session_id`,
		tenantID)
	if err != nil {
		return nil, fmt.Errorf("kernel: list external sessions: %w", pgFail(err))
	}
	defer rows.Close()
	var out []ExternalSession
	for rows.Next() {
		session, err := scanExternal(rows)
		if err != nil {
			return nil, fmt.Errorf("kernel: scan external session: %w", pgFail(err))
		}
		out = append(out, session)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// PinRunLoadout writes the loadout pin under the session lock, only for the
// current claim. A stale owner cannot re-pin a run the new owner has claimed.
func (s *PGStore) PinRunLoadout(ctx context.Context, lease Lease, loadoutID string, revision int64) error {
	return s.retryTx(ctx, func() error {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("kernel: pin begin: %w", pgFail(err))
		}
		defer tx.Rollback(ctx)
		state, ok, err := lockSessionLease(ctx, tx, lease.Scope)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("kernel: pin: %w", ErrNotFound)
		}
		if err := checkToken(state, lease.Token); err != nil {
			return fmt.Errorf("kernel: pin: %w", err)
		}
		tag, err := tx.Exec(ctx,
			`UPDATE runs SET loadout_id = NULLIF($3, ''), loadout_revision = $4, updated_at = now()
			 WHERE tenant_id = $1 AND session_id = $2`,
			lease.Scope.TenantID, lease.Scope.SessionID, loadoutID, revision)
		if err != nil {
			return fmt.Errorf("kernel: pin write: %w", pgFail(err))
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("kernel: pin: %w", ErrNoRunnable)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("kernel: pin commit: %w", pgFail(err))
		}
		return nil
	})
}

// RecordRead writes the attempt's load high-water mark under the session lock,
// fenced by the claim and monotonic (GREATEST): a killed attempt still leaves
// its read point behind, and a delayed lower write cannot rewind it.
func (s *PGStore) RecordRead(ctx context.Context, lease Lease, watermark int64) error {
	return s.retryTx(ctx, func() error {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("kernel: record read begin: %w", pgFail(err))
		}
		defer tx.Rollback(ctx)
		state, ok, err := lockSessionLease(ctx, tx, lease.Scope)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("kernel: record read: %w", ErrNotFound)
		}
		if err := checkToken(state, lease.Token); err != nil {
			return fmt.Errorf("kernel: record read: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE runs SET read_watermark = GREATEST(read_watermark, $3), updated_at = now()
			 WHERE tenant_id = $1 AND session_id = $2`,
			lease.Scope.TenantID, lease.Scope.SessionID, watermark); err != nil {
			return fmt.Errorf("kernel: record read: %w", pgFail(err))
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("kernel: record read commit: %w", pgFail(err))
		}
		return nil
	})
}

// ReadRunPin answers the run row's pin.
func (s *PGStore) ReadRunPin(ctx context.Context, scope Scope) (RunPin, bool, error) {
	var (
		out     RunPin
		owner   *string
		loadout *string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(status, ''), owner, owner_epoch, attempts, cursor_seq,
		        loadout_id, loadout_revision,
		        COALESCE(loadout_generation, 0), COALESCE(loadout_owner_epoch, 0),
		        COALESCE(pending_input, false), COALESCE(read_watermark, 0)
		 FROM runs WHERE tenant_id = $1 AND session_id = $2`,
		scope.TenantID, scope.SessionID).
		Scan(&out.Status, &owner, &out.Epoch, &out.Attempts, &out.Cursor, &loadout,
			&out.LoadoutRevision, &out.LoadoutGeneration, &out.LoadoutOwnerEpoch,
			&out.PendingInput, &out.ReadWatermark)
	if errors.Is(err, pgx.ErrNoRows) {
		// Distinguish "no run row" from "no session at all".
		exists, probeErr := s.SessionExists(ctx, scope.TenantID, scope.SessionID)
		if probeErr != nil {
			return RunPin{}, false, probeErr
		}
		if !exists {
			return RunPin{}, false, fmt.Errorf("kernel: run pin: %w", ErrNotFound)
		}
		return RunPin{}, false, nil
	}
	if err != nil {
		return RunPin{}, false, fmt.Errorf("kernel: run pin: %w", pgFail(err))
	}
	if owner != nil {
		out.Owner = *owner
	}
	if loadout != nil {
		out.LoadoutID = *loadout
	}
	out.Scope = scope
	return out, true, nil
}
