package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RequeueOutcome is the durable run state after an explicit resume request.
type RequeueOutcome struct {
	// Status is "requeued", "pending", "running", "done" or "no_run".
	Status   string
	Attempts int
}

// RequeueRun re-enqueues an existing session's run for an explicit resume. It
// never creates a session, an input or a card: the run row is the same row, and
// the ledger the next attempt rebuilds from is the same ledger.
//
// The update runs under the run row lock, so a resume that races a live attempt
// observes "running" and does not duplicate it. A terminal (done) run is
// reported as "done", never silently reopened: resume continues an interrupted
// task, it does not restart a completed one. A failed run's attempt budget is
// reset because an explicit resume is a new operator decision; the historical
// attempts stay in the ledger (run.finished facts), only the retry budget
// restarts.
func (s *Store) RequeueRun(ctx context.Context, tenantID, sessionID uuid.UUID) (RequeueOutcome, error) {
	if tenantID == uuid.Nil || sessionID == uuid.Nil {
		return RequeueOutcome{}, errors.New("store: requeue needs tenant and session")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RequeueOutcome{}, fmt.Errorf("store: requeue begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var (
		status     string
		attempts   int
		leaseUntil *time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT status, attempts, lease_until FROM runs
		 WHERE tenant_id = $1 AND session_id = $2
		 FOR UPDATE`, tenantID, sessionID).Scan(&status, &attempts, &leaseUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return RequeueOutcome{Status: "no_run"}, nil
	}
	if err != nil {
		return RequeueOutcome{}, fmt.Errorf("store: requeue read: %w", err)
	}
	switch status {
	case RunRunning:
		if leaseUntil != nil && leaseUntil.After(time.Now()) {
			return RequeueOutcome{Status: "running", Attempts: attempts}, nil
		}
		// A running row whose lease expired is a crashed attempt: requeue it.
	case RunPending:
		return RequeueOutcome{Status: "pending", Attempts: attempts}, nil
	case RunQueued:
		// A placement-queued run is already waiting for a slot; a resume must
		// not bypass the queue by making it pending. "pending" is the resume
		// vocabulary's "already enqueued, nothing to do".
		return RequeueOutcome{Status: "pending", Attempts: attempts}, nil
	case RunDone:
		return RequeueOutcome{Status: "done", Attempts: attempts}, nil
	case RunFailed:
		// Resumable.
	default:
		return RequeueOutcome{}, fmt.Errorf("store: requeue unknown run status %q", status)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE runs
		   SET status = 'pending', attempts = 0, pending_input = true,
		       lease_until = NULL, last_error = NULL, updated_at = now()
		 WHERE tenant_id = $1 AND session_id = $2`,
		tenantID, sessionID); err != nil {
		return RequeueOutcome{}, fmt.Errorf("store: requeue update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RequeueOutcome{}, fmt.Errorf("store: requeue commit: %w", err)
	}
	return RequeueOutcome{Status: "requeued", Attempts: 0}, nil
}
