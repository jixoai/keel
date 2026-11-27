package store

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
)

func boardLinksTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	st, err := Open(context.Background(), Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func TestBoardLinksTenantIsolationAndIdempotency(t *testing.T) {
	st := boardLinksTestStore(t)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	for _, tenant := range []uuid.UUID{tenantA, tenantB} {
		if err := st.CreateTenant(ctx, tenant, "board-link-"+tenant.String()[:8]); err != nil {
			t.Fatalf("create tenant: %v", err)
		}
	}
	linkA, err := st.PutBoardLink(ctx, BoardLink{
		TenantID: tenantA, Source: "superagent", SourceID: "ticket-a", Kind: BoardLinkTicket, BoardTaskID: 7,
	})
	if err != nil {
		t.Fatalf("put A: %v", err)
	}
	if linkA.BoardTaskID != 7 {
		t.Fatalf("task = %d", linkA.BoardTaskID)
	}
	// The same key with the same task is idempotent.
	again, err := st.PutBoardLink(ctx, BoardLink{
		TenantID: tenantA, Source: "superagent", SourceID: "ticket-a", Kind: BoardLinkTicket, BoardTaskID: 7,
	})
	if err != nil || again.BoardTaskID != 7 {
		t.Fatalf("repeat put = %+v err=%v", again, err)
	}
	// The same key with another task is a conflict: one source record cannot
	// map to two board threads.
	if _, err := st.PutBoardLink(ctx, BoardLink{
		TenantID: tenantA, Source: "superagent", SourceID: "ticket-a", Kind: BoardLinkTicket, BoardTaskID: 8,
	}); err == nil {
		t.Fatal("a second task for one source key was accepted")
	}

	// Tenant B has its own namespace.
	if _, err := st.PutBoardLink(ctx, BoardLink{
		TenantID: tenantB, Source: "superagent", SourceID: "ticket-a", Kind: BoardLinkTicket, BoardTaskID: 7,
	}); err != nil {
		t.Fatalf("put B: %v", err)
	}
	gotB, found, err := st.GetBoardLink(ctx, tenantB, "superagent", "ticket-a", BoardLinkTicket)
	if err != nil || !found || gotB.TenantID != tenantB {
		t.Fatalf("get B = %+v found=%v err=%v", gotB, found, err)
	}
	if _, found, err := st.GetBoardLink(ctx, tenantA, "superagent", "ticket-b", BoardLinkTicket); err != nil || found {
		t.Fatalf("missing link found=%v err=%v", found, err)
	}
	if _, found, err := st.GetBoardLinkByTask(ctx, tenantA, "superagent", BoardLinkTicket, 7); err != nil || !found {
		t.Fatalf("by task found=%v err=%v", found, err)
	}
	if err := st.SetBoardLinkClaimEpoch(ctx, tenantA, "superagent", "ticket-a", BoardLinkTicket, 4); err != nil {
		t.Fatalf("set epoch: %v", err)
	}
	updated, _, err := st.GetBoardLink(ctx, tenantA, "superagent", "ticket-a", BoardLinkTicket)
	if err != nil || updated.ClaimEpoch != 4 {
		t.Fatalf("epoch = %d err=%v", updated.ClaimEpoch, err)
	}
	if err := st.SetBoardLinkClaimEpoch(ctx, tenantB, "superagent", "missing", BoardLinkTicket, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing epoch update = %v, want ErrNotFound", err)
	}
}

func TestBoardLinksRLSIsForced(t *testing.T) {
	st := boardLinksTestStore(t)
	ctx := context.Background()
	var enabled, forced bool
	if err := st.Pool().QueryRow(ctx, `
		SELECT relrowsecurity, relforcerowsecurity
		  FROM pg_class
		 WHERE relname = 'board_links' AND relnamespace = current_schema()::regnamespace`).
		Scan(&enabled, &forced); err != nil {
		t.Fatalf("read board_links rls flags: %v", err)
	}
	if !enabled || !forced {
		t.Fatalf("board_links rls enabled=%v forced=%v", enabled, forced)
	}
	var policies int
	if err := st.Pool().QueryRow(ctx, `
		SELECT count(*) FROM pg_policies
		 WHERE schemaname = current_schema() AND tablename = 'board_links' AND policyname = 'tenant_isolation'`).
		Scan(&policies); err != nil {
		t.Fatalf("read board_links policy: %v", err)
	}
	if policies != 1 {
		t.Fatalf("board_links tenant_isolation policies = %d, want 1", policies)
	}
}

func TestBoardLinksRLSReadIsTenantScoped(t *testing.T) {
	st := boardLinksTestStore(t)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	for _, tenant := range []uuid.UUID{tenantA, tenantB} {
		if err := st.CreateTenant(ctx, tenant, "rls-"+tenant.String()[:8]); err != nil {
			t.Fatalf("create tenant: %v", err)
		}
	}
	for _, pair := range []struct {
		tenant uuid.UUID
		task   uint64
	}{{tenantA, 11}, {tenantB, 22}} {
		if _, err := st.PutBoardLink(ctx, BoardLink{
			TenantID: pair.tenant, Source: "superagent", SourceID: "ticket", Kind: BoardLinkTicket, BoardTaskID: pair.task,
		}); err != nil {
			t.Fatalf("put link: %v", err)
		}
	}
	err := st.TenantScoped(ctx, tenantA, func(tx TenantTx) error {
		var own, other int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM board_links`).Scan(&own); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM board_links WHERE tenant_id = $1`, tenantB).Scan(&other); err != nil {
			return err
		}
		if own != 1 {
			t.Fatalf("tenant A sees %d rows, want 1", own)
		}
		if other != 0 {
			t.Fatalf("tenant A sees %d tenant B rows, want 0", other)
		}
		return nil
	})
	if errors.Is(err, ErrRLSUnavailable) {
		t.Skip("RLS role is unavailable on this test cluster")
	}
	if err != nil {
		t.Fatalf("tenant-scoped read: %v", err)
	}
}
