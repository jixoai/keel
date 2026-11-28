package superagentstore_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/pluginstore"
	"github.com/jixoai/keel/internal/pluginstore/conformance"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/superagentplugin"
	"github.com/jixoai/keel/internal/superagentstore"
)

func openTestStore(t *testing.T) *superagentstore.Store {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn, MaxConns: 4, MinConns: 1})
	if err != nil {
		t.Fatalf("open keel store: %v", err)
	}
	t.Cleanup(st.Close)
	return superagentstore.New(st.Pool())
}

func TestBoardLinksTenantIsolationAndIdempotency(t *testing.T) {
	adapter := openTestStore(t)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	linkA, err := adapter.PutBoardLink(ctx, superagentstore.BoardLink{
		TenantID: tenantA, Source: "superagent", SourceID: "ticket-a", Kind: superagentstore.BoardLinkTicket, BoardTaskID: 7,
	})
	if err != nil {
		t.Fatalf("put A: %v", err)
	}
	if linkA.BoardTaskID != 7 {
		t.Fatalf("task = %d", linkA.BoardTaskID)
	}
	again, err := adapter.PutBoardLink(ctx, superagentstore.BoardLink{
		TenantID: tenantA, Source: "superagent", SourceID: "ticket-a", Kind: superagentstore.BoardLinkTicket, BoardTaskID: 7,
	})
	if err != nil || again.BoardTaskID != 7 {
		t.Fatalf("repeat put = %+v err=%v", again, err)
	}
	if _, err := adapter.PutBoardLink(ctx, superagentstore.BoardLink{
		TenantID: tenantA, Source: "superagent", SourceID: "ticket-a", Kind: superagentstore.BoardLinkTicket, BoardTaskID: 8,
	}); err == nil {
		t.Fatal("a second task for one source key was accepted")
	}
	if _, err := adapter.PutBoardLink(ctx, superagentstore.BoardLink{
		TenantID: tenantB, Source: "superagent", SourceID: "ticket-a", Kind: superagentstore.BoardLinkTicket, BoardTaskID: 7,
	}); err != nil {
		t.Fatalf("put B: %v", err)
	}
	if _, found, err := adapter.GetBoardLink(ctx, tenantA, "superagent", "ticket-b", superagentstore.BoardLinkTicket); err != nil || found {
		t.Fatalf("missing link found=%v err=%v", found, err)
	}
	if err := adapter.SetBoardLinkClaimEpoch(ctx, tenantA, "superagent", "ticket-a", superagentstore.BoardLinkTicket, 4); err != nil {
		t.Fatalf("set epoch: %v", err)
	}
	updated, _, err := adapter.GetBoardLink(ctx, tenantA, "superagent", "ticket-a", superagentstore.BoardLinkTicket)
	if err != nil || updated.ClaimEpoch != 4 {
		t.Fatalf("epoch = %d err=%v", updated.ClaimEpoch, err)
	}
	if err := adapter.SetBoardLinkClaimEpoch(ctx, tenantB, "superagent", "missing", superagentstore.BoardLinkTicket, 1); !errors.Is(err, superagentstore.ErrNotFound) {
		t.Fatalf("missing epoch update = %v, want ErrNotFound", err)
	}
}

func TestBoardLinksRLSIsForced(t *testing.T) {
	adapter := openTestStore(t)
	ctx := context.Background()
	var enabled, forced bool
	if err := adapter.Pool().QueryRow(ctx, `
		SELECT relrowsecurity, relforcerowsecurity
		  FROM pg_class
		 WHERE relname = 'superagent_board_links' AND relnamespace = current_schema()::regnamespace`).
		Scan(&enabled, &forced); err != nil {
		t.Fatalf("read superagent_board_links rls flags: %v", err)
	}
	if !enabled || !forced {
		t.Fatalf("superagent_board_links rls enabled=%v forced=%v", enabled, forced)
	}
	var policies int
	if err := adapter.Pool().QueryRow(ctx, `
		SELECT count(*) FROM pg_policies
		 WHERE schemaname = current_schema() AND tablename = 'superagent_board_links' AND policyname = 'tenant_isolation'`).
		Scan(&policies); err != nil {
		t.Fatalf("read superagent_board_links policy: %v", err)
	}
	if policies != 1 {
		t.Fatalf("superagent_board_links tenant_isolation policies = %d, want 1", policies)
	}
}

func TestSuperagentDomainConformance(t *testing.T) {
	adapter := openTestStore(t)
	conformance.RunDomain(t, func(t *testing.T) pluginstore.DomainStore {
		return superagentplugin.DomainStore{Store: adapter}
	}, "superagent")
}
