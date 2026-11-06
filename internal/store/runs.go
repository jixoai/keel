package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RunStatus values.
const (
	RunPending = "pending"
	RunRunning = "running"
	RunDone    = "done"
	RunFailed  = "failed"
)

// Run is a session's execution state. One row per session; the event log remains
// the truth, the run only tracks who is executing and how far it has read.
type Run struct {
	TenantID   uuid.UUID
	SessionID  uuid.UUID
	Status     string
	LeaseUntil *time.Time
	Attempts   int
	CursorSeq  int64
	LastError  string
}

// EnqueueRun marks a session as having work. A run already executing is left
// alone; its finish path re-enqueues if newer user events arrived meanwhile.
func (s *Store) EnqueueRun(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO runs (tenant_id, session_id, status) VALUES ($1, $2, 'pending')
		 ON CONFLICT (tenant_id, session_id) DO UPDATE
		 SET status = CASE WHEN runs.status = 'running' THEN 'running' ELSE 'pending' END,
		     updated_at = now()`, tenantID, sessionID)
	if err != nil {
		return fmt.Errorf("store: enqueue run: %w", err)
	}
	return nil
}

// ClaimRun atomically leases one runnable session. The lease bounds how long a
// dead process can block the session before another worker takes over.
func (s *Store) ClaimRun(ctx context.Context, lease time.Duration) (Run, error) {
	var r Run
	err := s.pool.QueryRow(ctx,
		`UPDATE runs SET status = 'running',
		                lease_until = now() + $1::interval,
		                attempts = attempts + 1,
		                updated_at = now()
		 WHERE (tenant_id, session_id) IN (
		   SELECT tenant_id, session_id FROM runs
		   WHERE status = 'pending'
		      OR (status = 'running' AND (lease_until IS NULL OR lease_until < now()))
		   ORDER BY updated_at
		   LIMIT 1
		   FOR UPDATE SKIP LOCKED)
		 RETURNING tenant_id, session_id, status, lease_until, attempts, cursor_seq,
		           COALESCE(last_error, '')`,
		lease.String()).
		Scan(&r.TenantID, &r.SessionID, &r.Status, &r.LeaseUntil, &r.Attempts, &r.CursorSeq, &r.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNoRun
	}
	if err != nil {
		return Run{}, fmt.Errorf("store: claim run: %w", err)
	}
	return r, nil
}

// ErrNoRun means there is no claimable run right now.
var ErrNoRun = errors.New("store: no claimable run")

// RenewLease extends a running run's lease.
func (s *Store) RenewLease(ctx context.Context, tenantID, sessionID uuid.UUID, lease time.Duration) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE runs SET lease_until = now() + $3::interval, updated_at = now()
		 WHERE tenant_id = $1 AND session_id = $2 AND status = 'running'`,
		tenantID, sessionID, lease.String())
	if err != nil {
		return fmt.Errorf("store: renew lease: %w", err)
	}
	return nil
}

// FinishRun records the outcome. If newer user events arrived while the run was
// executing, the row goes back to pending so the worker picks them up.
func (s *Store) FinishRun(ctx context.Context, tenantID, sessionID uuid.UUID, cursor int64, runErr error, maxAttempts int) error {
	status := RunDone
	lastError := ""
	if runErr != nil {
		lastError = runErr.Error()
		status = RunPending
		if maxAttempts > 0 {
			var attempts int
			if err := s.pool.QueryRow(ctx,
				`SELECT attempts FROM runs WHERE tenant_id=$1 AND session_id=$2`,
				tenantID, sessionID).Scan(&attempts); err == nil && attempts >= maxAttempts {
				status = RunFailed
			}
		}
	} else {
		var more bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM events
			                WHERE tenant_id=$1 AND session_id=$2 AND seq > $3 AND kind='user.message')`,
			tenantID, sessionID, cursor).Scan(&more); err != nil {
			return fmt.Errorf("store: check pending input: %w", err)
		}
		if more {
			status = RunPending
		}
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE runs SET status=$3, cursor_seq=GREATEST(cursor_seq, $4),
		                last_error=NULLIF($5,''), lease_until=NULL, updated_at=now()
		 WHERE tenant_id=$1 AND session_id=$2`,
		tenantID, sessionID, status, cursor, lastError)
	if err != nil {
		return fmt.Errorf("store: finish run: %w", err)
	}
	return nil
}

// GetRun reads a session's run row (for tests and status endpoints).
func (s *Store) GetRun(ctx context.Context, tenantID, sessionID uuid.UUID) (Run, error) {
	var r Run
	err := s.pool.QueryRow(ctx,
		`SELECT tenant_id, session_id, status, lease_until, attempts, cursor_seq, COALESCE(last_error,'')
		 FROM runs WHERE tenant_id=$1 AND session_id=$2`, tenantID, sessionID).
		Scan(&r.TenantID, &r.SessionID, &r.Status, &r.LeaseUntil, &r.Attempts, &r.CursorSeq, &r.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNoRun
	}
	if err != nil {
		return Run{}, fmt.Errorf("store: get run: %w", err)
	}
	return r, nil
}
