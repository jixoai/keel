// Submission bookkeeping: the requestId idempotency scope is the session, the
// same scope the event log's dedupe key uses.

package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestSubmitIsScopedToSession pins the M3 fix at the storage seam: the key is
// (tenant, session, requestId), so the same requestId in two sessions yields
// two independent submissions with their own seq, while a replay in the same
// session stays idempotent.
func TestSubmitIsScopedToSession(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant, sessionA, sessionB := uuid.New(), uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "submissions-test"); err != nil {
		t.Fatal(err)
	}
	for _, session := range []uuid.UUID{sessionA, sessionB} {
		if err := st.CreateSession(ctx, tenant, session); err != nil {
			t.Fatal(err)
		}
	}

	firstA, created, err := st.Submit(ctx, tenant, "dup-1", sessionA, uuid.New())
	if err != nil || !created {
		t.Fatalf("first submit A = %+v created=%v err=%v", firstA, created, err)
	}
	replayA, created, err := st.Submit(ctx, tenant, "dup-1", sessionA, uuid.New())
	if err != nil || created || replayA.ID != firstA.ID {
		t.Fatalf("same-session replay = %+v created=%v err=%v, want the stored row", replayA, created, err)
	}

	firstB, created, err := st.Submit(ctx, tenant, "dup-1", sessionB, uuid.New())
	if err != nil || !created {
		t.Fatalf("submit B with the same requestId = %+v created=%v err=%v", firstB, created, err)
	}
	if firstB.ID == firstA.ID {
		t.Fatal("session B received session A's submission id")
	}

	// Settlement is session-scoped too: committing A must not change B's row.
	if err := st.SetSubmissionStatus(ctx, tenant, sessionA, "dup-1", "committed", 7); err != nil {
		t.Fatal(err)
	}
	settledA, _, err := st.Submit(ctx, tenant, "dup-1", sessionA, uuid.New())
	if err != nil || settledA.Status != "committed" || settledA.Seq == nil || *settledA.Seq != 7 {
		t.Fatalf("settled A = %+v err=%v", settledA, err)
	}
	pendingB, _, err := st.Submit(ctx, tenant, "dup-1", sessionB, uuid.New())
	if err != nil || pendingB.Status != "pending" || pendingB.Seq != nil {
		t.Fatalf("B was changed by A's settlement: %+v err=%v", pendingB, err)
	}
}
