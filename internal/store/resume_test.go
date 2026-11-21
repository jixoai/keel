package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestRequeueRunResumesOnlyInterruptedRuns pins the resume policy at the run
// row: a failed (or lease-expired running) run is requeued in place with a
// fresh attempt budget, a live attempt is never duplicated, and a completed run
// is terminal.
func TestRequeueRunResumesOnlyInterruptedRuns(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant, session := runTestSession(t, st)
	if err := st.EnqueueRun(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}

	// A queued run is already pending: no duplicate.
	if outcome, err := st.RequeueRun(ctx, tenant, session); err != nil || outcome.Status != "pending" {
		t.Fatalf("pending requeue = %+v err=%v", outcome, err)
	}

	// A failed run is resumable; the attempt budget restarts because an explicit
	// resume is a new operator decision.
	if _, err := st.Pool().Exec(ctx, `
		UPDATE runs SET status = 'failed', attempts = 5, pending_input = false,
		                last_error = 'provider exhausted', lease_until = NULL
		 WHERE tenant_id = $1 AND session_id = $2`, tenant, session); err != nil {
		t.Fatal(err)
	}
	outcome, err := st.RequeueRun(ctx, tenant, session)
	if err != nil || outcome.Status != "requeued" || outcome.Attempts != 0 {
		t.Fatalf("failed requeue = %+v err=%v", outcome, err)
	}
	var (
		status       string
		attempts     int
		pendingInput bool
		lastError    string
	)
	if err := st.Pool().QueryRow(ctx, `
		SELECT status, attempts, pending_input, COALESCE(last_error, '')
		  FROM runs WHERE tenant_id = $1 AND session_id = $2`, tenant, session).Scan(
		&status, &attempts, &pendingInput, &lastError); err != nil {
		t.Fatal(err)
	}
	if status != RunPending || attempts != 0 || !pendingInput || lastError != "" {
		t.Fatalf("requeued row = status=%q attempts=%d pending=%v error=%q", status, attempts, pendingInput, lastError)
	}

	// A live attempt wins: resume must not create a second attempt.
	if _, err := st.Pool().Exec(ctx, `
		UPDATE runs SET status = 'running', attempts = 1,
		                lease_until = now() + interval '1 minute'
		 WHERE tenant_id = $1 AND session_id = $2`, tenant, session); err != nil {
		t.Fatal(err)
	}
	if outcome, err := st.RequeueRun(ctx, tenant, session); err != nil || outcome.Status != "running" {
		t.Fatalf("live requeue = %+v err=%v", outcome, err)
	}

	// A completed run is terminal.
	if _, err := st.Pool().Exec(ctx, `
		UPDATE runs SET status = 'done', lease_until = NULL
		 WHERE tenant_id = $1 AND session_id = $2`, tenant, session); err != nil {
		t.Fatal(err)
	}
	if outcome, err := st.RequeueRun(ctx, tenant, session); err != nil || outcome.Status != "done" {
		t.Fatalf("done requeue = %+v err=%v", outcome, err)
	}

	// A lease-expired running row is a crashed attempt: requeue it.
	if _, err := st.Pool().Exec(ctx, `
		UPDATE runs SET status = 'running', lease_until = now() - interval '1 minute'
		 WHERE tenant_id = $1 AND session_id = $2`, tenant, session); err != nil {
		t.Fatal(err)
	}
	if outcome, err := st.RequeueRun(ctx, tenant, session); err != nil || outcome.Status != "requeued" {
		t.Fatalf("crashed requeue = %+v err=%v", outcome, err)
	}

	// No run row is an explicit outcome, not an error.
	if outcome, err := st.RequeueRun(ctx, tenant, uuid.New()); err != nil || outcome.Status != "no_run" {
		t.Fatalf("missing run requeue = %+v err=%v", outcome, err)
	}
}
