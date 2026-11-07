package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func runTestSession(t *testing.T, st *Store) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tenant, session := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "runs-test"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	return tenant, session
}

// markRunning gives the test's run row a lease without going through ClaimRun.
// ClaimRun is a global queue shared by parallel test packages; driving
// FinishRun directly keeps these tests deterministic.
func markRunning(t *testing.T, st *Store, tenant, session uuid.UUID, attempts int) {
	t.Helper()
	if _, err := st.Pool().Exec(context.Background(),
		`UPDATE runs SET status = 'running', attempts = $3,
		                lease_until = now() + interval '30 seconds', updated_at = now()
		 WHERE tenant_id = $1 AND session_id = $2`, tenant, session, attempts); err != nil {
		t.Fatal(err)
	}
}

// TestEnqueueRunRequiresExistingSession: a run row without its session can
// never be claimed (the claim joins sessions) and would inflate the queue depth
// forever, so enqueueing a missing or foreign session creates nothing.
func TestEnqueueRunRequiresExistingSession(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "runs-missing"); err != nil {
		t.Fatal(err)
	}
	session := uuid.New()
	if err := st.EnqueueRun(ctx, tenant, session); !errors.Is(err, ErrNoSession) {
		t.Fatalf("enqueue missing session error = %v, want ErrNoSession", err)
	}
	if _, err := st.GetRun(ctx, tenant, session); !errors.Is(err, ErrNoRun) {
		t.Fatalf("orphan run row was created: %v", err)
	}
	owner, otherSession := runTestSession(t, st)
	if err := st.EnqueueRun(ctx, tenant, otherSession); !errors.Is(err, ErrNoSession) {
		t.Fatalf("cross-tenant enqueue error = %v, want ErrNoSession", err)
	}
	if _, err := st.GetRun(ctx, owner, otherSession); !errors.Is(err, ErrNoRun) {
		t.Fatalf("cross-tenant enqueue created a run row: %v", err)
	}
	if err := st.EnqueueRun(ctx, owner, otherSession); err != nil {
		t.Fatalf("owning tenant enqueue: %v", err)
	}
	if row, err := st.GetRun(ctx, owner, otherSession); err != nil || row.Status != RunPending {
		t.Fatalf("enqueued row = %+v, %v", row, err)
	}
}

// TestEnqueueRunRefusesExternalSession: an existing external session is a
// native-queue refusal (409 at the edge), and no run row is created.
func TestEnqueueRunRefusesExternalSession(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant, session := runTestSession(t, st)
	if _, err := st.CreateExternalSession(ctx, tenant, session, ExternalMarks{Backend: "dsh"}); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueRun(ctx, tenant, session); !errors.Is(err, ErrExternalSession) {
		t.Fatalf("enqueue external session = %v, want ErrExternalSession", err)
	}
	if _, err := st.GetRun(ctx, tenant, session); !errors.Is(err, ErrNoRun) {
		t.Fatalf("external session got a run row: %v", err)
	}
}

// TestEnqueueRunSerializesWithExternalPromotion is the M5 race: the promotion
// holds the session row lock; EnqueueRun must wait for it and then read the
// committed state_owner. Without the FOR UPDATE it reads the pre-promotion
// snapshot (MVCC) and queues a claimable run for an external session.
func TestEnqueueRunSerializesWithExternalPromotion(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant, session := runTestSession(t, st)
	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`UPDATE sessions SET state_owner = 'external', external_backend = 'dsh',
		                     external_status = 'active', metering = 'none',
		                     external_updated_at = now(), updated_at = now()
		 WHERE tenant_id = $1 AND session_id = $2`,
		tenant, session); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- st.EnqueueRun(ctx, tenant, session) }()
	time.Sleep(200 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("EnqueueRun returned before the promotion committed (race): %v", err)
	default:
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrExternalSession) {
			t.Fatalf("enqueue after promotion = %v, want ErrExternalSession", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("EnqueueRun did not resume after the promotion committed")
	}
	if _, err := st.GetRun(ctx, tenant, session); !errors.Is(err, ErrNoRun) {
		t.Fatalf("external session got a run row: %v", err)
	}
}

func TestFinishRunReturnsPendingThenDone(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant, session := runTestSession(t, st)
	if err := st.EnqueueRun(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	markRunning(t, st, tenant, session, 1)
	status, err := st.FinishRun(ctx, tenant, session, 0, errors.New("model stream failed"), 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if status != RunPending {
		t.Fatalf("first failure status = %q, want pending", status)
	}
	row, err := st.GetRun(ctx, tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != RunPending || row.LastError == "" {
		t.Fatalf("row after failed attempt = %+v", row)
	}

	markRunning(t, st, tenant, session, 2)
	status, err = st.FinishRun(ctx, tenant, session, 0, nil, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if status != RunDone {
		t.Fatalf("retry success status = %q, want done", status)
	}
	row, err = st.GetRun(ctx, tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != RunDone {
		t.Fatalf("terminal row = %+v", row)
	}
}

func TestFinishRunExhaustsAttempts(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant, session := runTestSession(t, st)
	if err := st.EnqueueRun(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	markRunning(t, st, tenant, session, 2)
	status, err := st.FinishRun(ctx, tenant, session, 0, errors.New("always fails"), 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if status != RunFailed {
		t.Fatalf("exhausted status = %q, want failed", status)
	}
	row, err := st.GetRun(ctx, tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != RunFailed {
		t.Fatalf("row = %+v", row)
	}
}

func TestFinishRunReturnsPendingWhenNewInputArrived(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant, session := runTestSession(t, st)
	first, err := st.AppendEvent(ctx, tenant, session, EventUserMessage, map[string]string{"text": "one"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueRun(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	markRunning(t, st, tenant, session, 1)
	// The run consumed seq 1; a newer user message arrived while it ran.
	if _, err := st.AppendEvent(ctx, tenant, session, EventUserMessage, map[string]string{"text": "two"}); err != nil {
		t.Fatal(err)
	}
	status, err := st.FinishRun(ctx, tenant, session, first, nil, 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if status != RunPending {
		t.Fatalf("status = %q, want pending for new input", status)
	}
}

func TestFinishRunRejectsZombieAfterDone(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant, session := runTestSession(t, st)
	if err := st.EnqueueRun(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	markRunning(t, st, tenant, session, 1)
	status, err := st.FinishRun(ctx, tenant, session, 0, nil, 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if status != RunDone {
		t.Fatalf("status = %q, want done", status)
	}
	// A late attempt that still believes it owns attempt 1 must not move the
	// terminal row back to pending/failed.
	if _, err := st.FinishRun(ctx, tenant, session, 0, errors.New("zombie"), 3, 1); !errors.Is(err, ErrStaleRun) {
		t.Fatalf("zombie FinishRun error = %v, want ErrStaleRun", err)
	}
	row, err := st.GetRun(ctx, tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != RunDone {
		t.Fatalf("terminal row overwritten: %+v", row)
	}
	if row.LastError != "" {
		t.Fatalf("zombie wrote last_error: %q", row.LastError)
	}
}

func TestFinishRunRejectsSupersededAttempt(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant, session := runTestSession(t, st)
	if err := st.EnqueueRun(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	// Attempt 1 claims. Its lease expires; attempt 2 takes over.
	markRunning(t, st, tenant, session, 1)
	markRunning(t, st, tenant, session, 2)
	if _, err := st.FinishRun(ctx, tenant, session, 0, nil, 3, 1); !errors.Is(err, ErrStaleRun) {
		t.Fatalf("stale attempt error = %v, want ErrStaleRun", err)
	}
	row, err := st.GetRun(ctx, tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != RunRunning || row.Attempts != 2 {
		t.Fatalf("row after stale finish = %+v", row)
	}
	status, err := st.FinishRun(ctx, tenant, session, 0, nil, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if status != RunDone {
		t.Fatalf("current attempt status = %q, want done", status)
	}
}
