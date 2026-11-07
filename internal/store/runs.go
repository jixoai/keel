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
	// Reclaimed is true when this claim took over a row whose lease had
	// expired (the previous owner crashed or lost its heartbeat).
	Reclaimed bool
}

// EnqueueRun marks a session as having work. A run already executing is left
// alone; its finish path re-enqueues if newer user events arrived meanwhile.
//
// The session row is locked first (lock order sessions -> runs), so this
// statement serializes with CreateExternalSession/UpsertExternalSession, whose
// promotion transaction holds the same row: without the lock the insert below
// reads the pre-promotion snapshot (MVCC) and queues a run for a session that
// is becoming external. An external session is refused before anything is
// written; a missing session returns ErrNoSession.
func (s *Store) EnqueueRun(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	err := retryTxBudget(ctx, 10, func() error {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		var stateOwner string
		err = tx.QueryRow(ctx,
			`SELECT COALESCE(state_owner, 'keel') FROM sessions
			 WHERE tenant_id = $1 AND session_id = $2 FOR UPDATE`,
			tenantID, sessionID).Scan(&stateOwner)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoSession
		}
		if err != nil {
			return err
		}
		if stateOwner == StateOwnerExternal {
			return ErrExternalSession
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO runs (tenant_id, session_id, status) VALUES ($1, $2, 'pending')
			 ON CONFLICT (tenant_id, session_id) DO UPDATE
			 SET status = CASE WHEN runs.status = 'running' THEN 'running' ELSE 'pending' END,
			     updated_at = now()`,
			tenantID, sessionID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
	if err != nil {
		return fmt.Errorf("store: enqueue run: %w", err)
	}
	return nil
}

// ClaimRun atomically leases one runnable session. The lease bounds how long a
// dead process can block the session before another worker takes over.
//
// Deprecated: retired from the production path (contract-kernel W1: the worker
// claims through kernel.LeaseManager instead). This method does not take the
// session row lock and issues no fencing token, so a claim made here is
// invisible to owner-scoped commits. It remains for legacy readers and tests
// that only need the queue semantics; do not add new callers.
func (s *Store) ClaimRun(ctx context.Context, lease time.Duration) (Run, error) {
	var (
		r              Run
		previousStatus string
	)
	err := retryTxBudget(ctx, 6, func() error {
		r = Run{}
		previousStatus = ""
		return s.pool.QueryRow(ctx,
			`WITH candidate AS (
			   SELECT tenant_id, session_id, status AS previous_status
			   FROM runs r
			   WHERE (r.status = 'pending'
			      OR (r.status = 'running' AND (r.lease_until IS NULL OR r.lease_until < now())))
			     -- Fail closed for external sessions without locking their rows: this
			     -- legacy path only reads the mark, and the EXISTS avoids adding a
			     -- second table to the FOR UPDATE SKIP LOCKED lock set.
			     AND EXISTS (SELECT 1 FROM sessions s
			                 WHERE s.tenant_id = r.tenant_id AND s.session_id = r.session_id
			                   AND s.state_owner <> 'external')
			   ORDER BY r.updated_at
			   LIMIT 1
			   FOR UPDATE SKIP LOCKED)
			 UPDATE runs SET status = 'running',
			                lease_until = now() + $1::interval,
			                attempts = attempts + 1,
			                updated_at = now()
			 FROM candidate
			 WHERE runs.tenant_id = candidate.tenant_id
			   AND runs.session_id = candidate.session_id
			 RETURNING runs.tenant_id, runs.session_id, runs.status, runs.lease_until,
			           runs.attempts, runs.cursor_seq, COALESCE(runs.last_error, ''),
			           candidate.previous_status`,
			lease.String()).
			Scan(&r.TenantID, &r.SessionID, &r.Status, &r.LeaseUntil, &r.Attempts, &r.CursorSeq, &r.LastError, &previousStatus)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNoRun
	}
	if err != nil {
		return Run{}, fmt.Errorf("store: claim run: %w", err)
	}
	r.Reclaimed = previousStatus == RunRunning
	return r, nil
}

// ErrNoRun means there is no claimable run right now.
var ErrNoRun = errors.New("store: no claimable run")

// ErrStaleRun means FinishRun's expected attempt no longer owns the row: a
// newer attempt has taken over after lease expiry, or the run already reached a
// terminal state. The caller must not treat this as a run outcome.
var ErrStaleRun = errors.New("store: stale run attempt")

// RenewLease extends a running run's lease.
//
// Deprecated: retired from the production path (kernel.LeaseManager.Renew owns
// lease renewal, and the lease authority lives on the session row). Kept for
// legacy callers and tests only.
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

// FinishRun records the outcome and returns the committed status. If newer user
// events arrived while the run was executing, or the error is retryable and the
// attempt budget is not exhausted, the row goes back to pending so a worker
// picks it up again. Callers must use the returned status for terminal metrics:
// a failed attempt that stays pending is not a finished run.
//
// The "newer input" probe uses the reported cursor as the read-through
// watermark. Callers that know the load high-water mark (Runner.RunWithWatermark)
// must use FinishRunAfter instead: a run's own commits advance the cursor past
// input that arrived mid-run and was never read, and probing on the cursor
// would strand that input.
//
// expectedAttempts is the attempts value returned by ClaimRun. The UPDATE is
// guarded by status='running' AND attempts=expectedAttempts, so a zombie attempt
// whose lease was taken over (or whose run already finished) cannot overwrite a
// newer state; that case returns ErrStaleRun.
//
// Deprecated: retired from the production path (kernel.LeaseManager.Finish
// settles a run under the session lock and rejects stale fencing tokens; the
// outcome policy is mirrored by kernel.finishStatus). Kept for legacy callers
// and tests only.
func (s *Store) FinishRun(ctx context.Context, tenantID, sessionID uuid.UUID, cursor int64, runErr error, maxAttempts, expectedAttempts int) (string, error) {
	return s.FinishRunAfter(ctx, tenantID, sessionID, cursor, cursor, runErr, maxAttempts, expectedAttempts)
}

// FinishRunAfter is FinishRun with an explicit read-through watermark: the
// highest committed sequence the run's transcript was built from. Input
// committed after it (a user.message with a higher seq) was not seen by the run
// and forces the row back to pending, even though the run's own later commits
// pushed the cursor further. readThrough <= 0 (or > cursor) falls back to the
// cursor, which is the legacy behavior.
func (s *Store) FinishRunAfter(ctx context.Context, tenantID, sessionID uuid.UUID, cursor, readThrough int64, runErr error, maxAttempts, expectedAttempts int) (string, error) {
	if readThrough <= 0 || readThrough > cursor {
		readThrough = cursor
	}
	status := RunDone
	lastError := ""
	if runErr != nil {
		lastError = runErr.Error()
		status = RunPending
		if maxAttempts > 0 && expectedAttempts >= maxAttempts {
			status = RunFailed
		}
	} else {
		var more bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM events
			                WHERE tenant_id=$1 AND session_id=$2 AND seq > $3 AND kind='user.message')`,
			tenantID, sessionID, readThrough).Scan(&more); err != nil {
			return "", fmt.Errorf("store: check pending input: %w", err)
		}
		if more {
			status = RunPending
		}
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE runs SET status=$3, cursor_seq=GREATEST(cursor_seq, $4),
		                last_error=NULLIF($5,''), lease_until=NULL, updated_at=now()
		 WHERE tenant_id=$1 AND session_id=$2
		   AND status='running' AND attempts=$6`,
		tenantID, sessionID, status, cursor, lastError, expectedAttempts)
	if err != nil {
		return "", fmt.Errorf("store: finish run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var currentStatus string
		var currentAttempts int
		err := s.pool.QueryRow(ctx,
			`SELECT status, attempts FROM runs WHERE tenant_id=$1 AND session_id=$2`,
			tenantID, sessionID).Scan(&currentStatus, &currentAttempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("%w: run not found", ErrStaleRun)
		}
		if err != nil {
			return "", fmt.Errorf("store: finish run: inspect stale row: %w", err)
		}
		return "", fmt.Errorf("%w: have status=%s attempts=%d, want running attempts=%d",
			ErrStaleRun, currentStatus, currentAttempts, expectedAttempts)
	}
	return status, nil
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
