package store

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

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
	first, inserted, err := st.ClaimCommandInvocation(ctx, row, time.Minute)
	if err != nil || !inserted || first.RequestID != "req-1" {
		t.Fatalf("first claim = %+v inserted=%v err=%v", first, inserted, err)
	}
	again, inserted, err := st.ClaimCommandInvocation(ctx, row, time.Minute)
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
			_, owned, err := st.ClaimCommandInvocation(ctx, candidate, time.Minute)
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
	first.DurationMs = 42
	first.EventSeq = 3
	first.Result = result
	first.Status = CommandStatusOK
	if ok, err := st.TransitionCommandInvocation(ctx, first, CommandStatusRunning); err != nil || !ok {
		t.Fatalf("transition terminal = %v err=%v", ok, err)
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

func TestCommandInvocationsIsInRLS(t *testing.T) {
	for _, table := range rlsTenantTables {
		if table == "command_invocations" {
			return
		}
	}
	t.Fatal("command_invocations is missing from rlsTenantTables")
}

func TestPostgresCommandDecisionCAS(t *testing.T) {
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
	if err := st.CreateTenant(ctx, tenant, "cmd-cas-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	session := uuid.New()
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM command_invocations WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM sessions WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM tenants WHERE tenant_id = $1`, tenant)
	})

	row := CommandInvocation{
		TenantID: tenant, SessionID: session, RequestID: "cas-1",
		Command: "vendor:purge", Source: "plugin", Status: CommandStatusRunning,
		ArgsHash: "sha256:x", ArgsBytes: 2,
	}
	claimed, owned, err := st.ClaimCommandInvocation(ctx, row, time.Minute)
	if err != nil || !owned {
		t.Fatalf("claim = owned=%v err=%v", owned, err)
	}
	claimed.Status = CommandStatusPendingDecision
	claimed.DecisionID = uuid.NewString()
	if ok, err := st.TransitionCommandInvocation(ctx, claimed, CommandStatusRunning); err != nil || !ok {
		t.Fatalf("pending transition = %v err=%v", ok, err)
	}

	var wg sync.WaitGroup
	owners := 0
	var mu sync.Mutex
	for index := 0; index < 4; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, got, err := st.ClaimDecisionResult(ctx, tenant, session, "cas-1", time.Minute)
			if err != nil {
				t.Errorf("claim decision result: %v", err)
				return
			}
			if got {
				mu.Lock()
				owners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if owners != 1 {
		t.Fatalf("decision CAS owners = %d, want 1", owners)
	}

	// The terminal write is conditional: a stale pending transition is refused.
	stale := claimed
	stale.Status = CommandStatusRefused
	if ok, err := st.TransitionCommandInvocation(ctx, stale, CommandStatusPendingDecision); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("a stale pending transition overwrote the running owner")
	}
}

func TestPostgresCommandLeaseReclaim(t *testing.T) {
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
	if err := st.CreateTenant(ctx, tenant, "cmd-lease-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	session := uuid.New()
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM command_invocations WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM sessions WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM tenants WHERE tenant_id = $1`, tenant)
	})
	row := CommandInvocation{
		TenantID: tenant, SessionID: session, RequestID: "lease-1",
		Command: "compact", Source: "builtin", Status: CommandStatusRunning,
	}
	if _, owned, err := st.ClaimCommandInvocation(ctx, row, 50*time.Millisecond); err != nil || !owned {
		t.Fatalf("claim = owned=%v err=%v", owned, err)
	}
	time.Sleep(120 * time.Millisecond)
	reclaimed, owned, err := st.ClaimCommandInvocation(ctx, row, time.Minute)
	if err != nil || !owned {
		t.Fatalf("reclaim = owned=%v err=%v", owned, err)
	}
	if reclaimed.LeaseUntil == nil || !reclaimed.LeaseUntil.After(time.Now()) {
		t.Fatalf("reclaimed lease = %v", reclaimed.LeaseUntil)
	}
	// A renewed lease is not stealable.
	if _, owned, err := st.ClaimCommandInvocation(ctx, row, time.Minute); err != nil || owned {
		t.Fatalf("live lease stolen = owned=%v err=%v", owned, err)
	}
}

func TestPostgresCommandInvocationsRLSPolicy(t *testing.T) {
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
	var enabled, forced bool
	if err := st.Pool().QueryRow(ctx,
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = 'command_invocations'::regclass`).
		Scan(&enabled, &forced); err != nil {
		t.Fatalf("pg_class: %v", err)
	}
	if !enabled || !forced {
		t.Fatalf("command_invocations rls enabled=%v forced=%v", enabled, forced)
	}
	var policies int
	if err := st.Pool().QueryRow(ctx,
		`SELECT count(*) FROM pg_policies WHERE schemaname='public' AND tablename='command_invocations' AND policyname='tenant_isolation'`).
		Scan(&policies); err != nil {
		t.Fatalf("pg_policies: %v", err)
	}
	if policies != 1 {
		t.Fatalf("tenant_isolation policies = %d", policies)
	}
}
