package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestMigrateKeepsCrossSessionRequestIDs pins the invariant the reverse
// migration block used to break: re-applying schema.sql (which happens on every
// boot) must not delete a second session's submission that reuses the same
// requestId. The tail migration widens the key (2 columns -> 3) without losing
// rows; it must never narrow it back and deduplicate tenants-wide.
func TestMigrateKeepsCrossSessionRequestIDs(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	sessionA, sessionB := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "migrate-scope"); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []uuid.UUID{sessionA, sessionB} {
		if err := st.CreateSession(ctx, tenant, sid); err != nil {
			t.Fatal(err)
		}
	}
	for i, sid := range []uuid.UUID{sessionA, sessionB} {
		row := SubmissionRow{TenantID: tenant, SessionID: sid, SubmissionID: uuid.New(),
			RequestID: "shared-request", Type: "input", Status: "queued"}
		created, isNew, err := st.CreateSubmission(ctx, row)
		if err != nil || !isNew || created.SubmissionID != row.SubmissionID {
			t.Fatalf("session %d: created=%+v new=%v err=%v", i, created, isNew, err)
		}
	}
	// A restart re-applies the schema.
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	for i, sid := range []uuid.UUID{sessionA, sessionB} {
		if _, err := st.GetSubmissionByRequest(ctx, tenant, sid, "shared-request"); err != nil {
			t.Fatalf("session %d lost its submission after re-migrate: %v", i, err)
		}
	}
	listed, err := st.ListSubmissions(ctx, tenant, sessionA)
	if err != nil || len(listed) != 1 {
		t.Fatalf("session A list = %+v err=%v", listed, err)
	}
}
