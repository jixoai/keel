package store

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// Command invocation rows against real PostgreSQL: the requestId claim is
// exactly-once under concurrency, a foreign tenant sees no row, and a terminal
// update round-trips its result. Skipped without KEEL_TEST_DSN.
func TestPostgresCommandInvocations(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "commands-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	session := uuid.New()
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM command_invocations WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM events WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM sessions WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM tenants WHERE tenant_id = $1`, tenant)
	})

	row := CommandInvocation{
		TenantID: tenant, SessionID: session, RequestID: "req-1",
		Command: "compact", Source: "builtin", Actor: "account-1",
		Status: CommandStatusRunning, ArgsHash: "sha256:abc", ArgsBytes: 2,
	}
	first, inserted, err := st.ClaimCommandInvocation(ctx, row)
	if err != nil || !inserted || first.RequestID != "req-1" {
		t.Fatalf("first claim = %+v inserted=%v err=%v", first, inserted, err)
	}
	again, inserted, err := st.ClaimCommandInvocation(ctx, row)
	if err != nil || inserted || again.RequestID != "req-1" {
		t.Fatalf("repeat claim = %+v inserted=%v err=%v", again, inserted, err)
	}

	// Concurrent claims: exactly one owner.
	const workers = 8
	var wg sync.WaitGroup
	owners := 0
	var mu sync.Mutex
	for index := 0; index < workers; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			candidate := row
			candidate.RequestID = "req-concurrent"
			_, owned, err := st.ClaimCommandInvocation(ctx, candidate)
			if err != nil {
				t.Errorf("concurrent claim: %v", err)
				return
			}
			if owned {
				mu.Lock()
				owners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if owners != 1 {
		t.Fatalf("concurrent owners = %d, want 1", owners)
	}

	result := json.RawMessage(`{"checkpointId":9}`)
	first.Status = CommandStatusOK
	first.DurationMs = 42
	first.EventSeq = 3
	first.Result = result
	if err := st.UpdateCommandInvocation(ctx, first); err != nil {
		t.Fatal(err)
	}
	stored, found, err := st.GetCommandInvocation(ctx, tenant, session, "req-1")
	if err != nil || !found {
		t.Fatalf("get = found=%v err=%v", found, err)
	}
	if stored.Status != CommandStatusOK || stored.DurationMs != 42 || stored.EventSeq != 3 ||
		stored.ArgsHash != "sha256:abc" {
		t.Fatalf("stored = %+v", stored)
	}
	var storedResult map[string]any
	if err := json.Unmarshal(stored.Result, &storedResult); err != nil || storedResult["checkpointId"] != float64(9) {
		t.Fatalf("stored result = %s (%v)", stored.Result, err)
	}
	if _, found, err := st.GetCommandInvocation(ctx, uuid.New(), session, "req-1"); err != nil || found {
		t.Fatalf("foreign tenant get = found=%v err=%v", found, err)
	}
	if _, found, _ := st.GetCommandInvocation(ctx, tenant, uuid.New(), "req-1"); found {
		t.Fatal("foreign session get found the row")
	}
}
