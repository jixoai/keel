package store

// ArchiveSession round trip against the real test database (KEEL_TEST_DSN):
// the mark is set (soft delete), a repeat reports the session still exists, a
// missing/foreign id reports false (the route's 404), and the ledger rows are
// never touched. The list-side filter lives in internal/agentapi's SQL
// (PGSessionLister) and is pinned by its own tests.

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestArchiveSessionStoreRoundTrip(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	session := uuid.New()
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(),
			`DELETE FROM events WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(),
			`DELETE FROM sessions WHERE tenant_id = $1`, tenant)
	})

	// The ledger row an archive must never touch.
	if _, err := st.AppendEvent(ctx, tenant, session, "user.message", map[string]any{"text": "keep me"}); err != nil {
		t.Fatalf("append event: %v", err)
	}

	found, err := st.ArchiveSession(ctx, tenant, session)
	if err != nil || !found {
		t.Fatalf("archive: found=%v err=%v", found, err)
	}
	// Idempotent: the second call still reports the session exists.
	if found, err := st.ArchiveSession(ctx, tenant, session); err != nil || !found {
		t.Fatalf("repeat archive: found=%v err=%v", found, err)
	}
	// A missing id reports false (the route's 404).
	if found, err := st.ArchiveSession(ctx, tenant, uuid.New()); err != nil || found {
		t.Fatalf("missing session must report false: found=%v err=%v", found, err)
	}
	// Cross tenant: the same session id under another tenant is not this
	// tenant's, so it is a 404 for that caller and a no-op for this row.
	if found, err := st.ArchiveSession(ctx, uuid.New(), session); err != nil || found {
		t.Fatalf("foreign tenant must report false: found=%v err=%v", found, err)
	}

	// The ledger row survives; the mark is set.
	var events int
	if err := st.Pool().QueryRow(ctx,
		`SELECT count(*) FROM events WHERE tenant_id = $1 AND session_id = $2`,
		tenant, session).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Fatalf("events = %d, want the ledger row intact", events)
	}
	var archived bool
	if err := st.Pool().QueryRow(ctx,
		`SELECT archived_at IS NOT NULL FROM sessions WHERE tenant_id = $1 AND session_id = $2`,
		tenant, session).Scan(&archived); err != nil {
		t.Fatalf("read archived_at: %v", err)
	}
	if !archived {
		t.Fatal("archived_at is not set")
	}
}
