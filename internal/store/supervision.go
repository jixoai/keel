package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Supervisor read/write surface (change keel-harness-v2, W12 recovery).
//
// These methods are the persistence seam of internal/supervisor. They add no
// table and no second source of truth: the run row is the queue's truth and the
// event log is the recovery truth (`resume.attempt`, `supervision.escalated`).
// The only write, ParkRun, reuses the existing resumable `failed` status so the
// explicit resume endpoint (RequeueRun) continues a parked run unchanged.

// SupervisorRun is one run row as the supervisor reads it.
type SupervisorRun struct {
	TenantID   uuid.UUID
	SessionID  uuid.UUID
	Status     string
	Attempts   int
	LeaseUntil *time.Time
	CursorSeq  int64
	LastError  string
	UpdatedAt  time.Time
}

// SupervisorCursor is the keyset continuation of a supervisor scan. It matches
// the (updated_at, tenant_id, session_id) order, so pages cannot skip or repeat
// a row whose timestamp ties another.
type SupervisorCursor struct {
	UpdatedAt time.Time
	TenantID  uuid.UUID
	SessionID uuid.UUID
}

// supervisorFailedLookback bounds automatic resume of already-failed runs to
// recent history. Non-terminal runs are always scanned; an old failed run is
// not silently retried months later by a restart.
const supervisorFailedLookback = 24 * time.Hour

// ListSupervisorRuns returns one page of the runs a supervisor scan may act on:
// every non-terminal row (pending/running) and failed rows that changed within
// the lookback. The scan is cross-tenant by design (it is the run queue's
// global reader, like the kernel's claim), so it uses the operator pool with
// its explicit tenant column. after=nil starts at the beginning; a full page is
// continued by passing the last row's SupervisorCursor.
func (s *Store) ListSupervisorRuns(ctx context.Context, limit int, after *SupervisorCursor) ([]SupervisorRun, error) {
	if limit <= 0 {
		limit = 200
	}
	var afterAt any
	var afterTenant, afterSession any
	if after != nil {
		afterAt = after.UpdatedAt
		afterTenant = after.TenantID
		afterSession = after.SessionID
	}
	rows, err := s.pool.Query(ctx, `
		SELECT tenant_id, session_id, status, attempts, lease_until, cursor_seq,
		       COALESCE(last_error, ''), updated_at
		  FROM runs
		 WHERE (status IN ('pending', 'running')
		    OR (status = 'failed' AND updated_at >= now() - $2::interval))
		   AND ($3::timestamptz IS NULL
		        OR (updated_at, tenant_id, session_id) > ($3::timestamptz, $4::uuid, $5::uuid))
		 ORDER BY updated_at ASC, tenant_id, session_id
		 LIMIT $1`, limit, supervisorFailedLookback.String(), afterAt, afterTenant, afterSession)
	if err != nil {
		return nil, fmt.Errorf("store: list supervisor runs: %w", err)
	}
	defer rows.Close()
	var out []SupervisorRun
	for rows.Next() {
		var run SupervisorRun
		if err := rows.Scan(&run.TenantID, &run.SessionID, &run.Status, &run.Attempts,
			&run.LeaseUntil, &run.CursorSeq, &run.LastError, &run.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan supervisor run: %w", err)
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

// ParkRun moves a pending or expired-running row to the resumable failed status
// without settling an attempt. It is the durable "pending recovery" state for
// resume.mode=manual|never: no worker claims the row, and the explicit resume
// endpoint (RequeueRun accepts failed) continues it.
//
// A live lease is never touched: another process may be executing the run, and
// a policy flag must not kill work that is already in flight.
func (s *Store) ParkRun(ctx context.Context, tenantID, sessionID uuid.UUID, reason string) (bool, error) {
	if tenantID == uuid.Nil || sessionID == uuid.Nil {
		return false, fmt.Errorf("store: park run: tenant and session are required")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE runs
		   SET status = 'failed', lease_until = NULL, last_error = $3, updated_at = now()
		 WHERE tenant_id = $1 AND session_id = $2
		   AND (status = 'pending'
		        OR (status = 'running' AND (lease_until IS NULL OR lease_until < now())))`,
		tenantID, sessionID, reason)
	if err != nil {
		return false, fmt.Errorf("store: park run: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ResumeAttemptCount counts every committed resume.attempt fact of a session.
// It mints the supervisor's idempotency ordinal; the kernel's dedupe index
// makes a concurrent scan with the same ordinal a no-op.
func (s *Store) ResumeAttemptCount(ctx context.Context, tenantID, sessionID uuid.UUID) (int, error) {
	return s.countSessionEvents(ctx, tenantID, sessionID, `
		kind = 'resume.attempt'`)
}

// AutoResumeCount counts the supervisor automatic resumes charged to the retry
// budget. Operator resumes are deliberately excluded: an explicit operator
// decision restarts the budget rather than consuming it.
func (s *Store) AutoResumeCount(ctx context.Context, tenantID, sessionID uuid.UUID) (int, error) {
	return s.countSessionEvents(ctx, tenantID, sessionID, `
		kind = 'resume.attempt'
		AND COALESCE(payload->>'actor', '') = 'supervisor'
		AND COALESCE(payload->>'counted', 'false') = 'true'`)
}

// LastAutoResume returns the newest counted automatic resume time.
func (s *Store) LastAutoResume(ctx context.Context, tenantID, sessionID uuid.UUID) (time.Time, bool, error) {
	var at *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT max(created_at)
		  FROM events
		 WHERE tenant_id = $1 AND session_id = $2
		   AND kind = 'resume.attempt'
		   AND COALESCE(payload->>'actor', '') = 'supervisor'
		   AND COALESCE(payload->>'counted', 'false') = 'true'`,
		tenantID, sessionID).Scan(&at)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: last supervisor resume: %w", err)
	}
	if at == nil {
		return time.Time{}, false, nil
	}
	return *at, true, nil
}

// EscalationCount counts committed supervision.escalated facts.
func (s *Store) EscalationCount(ctx context.Context, tenantID, sessionID uuid.UUID) (int, error) {
	return s.countSessionEvents(ctx, tenantID, sessionID, `
		kind = 'supervision.escalated'`)
}

// EscalationsSince counts escalations at or after one failure epoch's start.
func (s *Store) EscalationsSince(ctx context.Context, tenantID, sessionID uuid.UUID, since time.Time) (int, error) {
	var count int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM events
		 WHERE tenant_id = $1 AND session_id = $2
		   AND kind = 'supervision.escalated'
		   AND created_at >= $3`,
		tenantID, sessionID, since).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: escalations since: %w", err)
	}
	return count, nil
}

// LatestSelection returns the newest selection fact (the model identity a run
// resolves) or nil. It backs the providerFallback decision without replaying a
// long log.
func (s *Store) LatestSelection(ctx context.Context, tenantID, sessionID uuid.UUID) (json.RawMessage, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT payload->'selection'
		  FROM events
		 WHERE tenant_id = $1 AND session_id = $2
		   AND kind IN ('user.message', 'resume.attempt')
		   AND payload->'selection' IS NOT NULL
		   AND payload->'selection' <> 'null'::jsonb
		 ORDER BY seq DESC
		 LIMIT 1`, tenantID, sessionID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: latest selection: %w", err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	return json.RawMessage(raw), nil
}

func (s *Store) countSessionEvents(ctx context.Context, tenantID, sessionID uuid.UUID, predicate string) (int, error) {
	var count int
	query := `SELECT count(*) FROM events WHERE tenant_id = $1 AND session_id = $2 AND ` + predicate
	if err := s.pool.QueryRow(ctx, query, tenantID, sessionID).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count session events: %w", err)
	}
	return count, nil
}
