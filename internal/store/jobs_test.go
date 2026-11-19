package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestBashJobsSchemaAndRLS pins the two non-obvious integration points of the
// W2.2 job table: it exists after the ordinary migration, and it is a tenant
// table (ENABLE + FORCE row level security with the tenant_isolation policy).
// A table added to schema.sql but not to rlsTenantTables would silently be
// readable across tenants; this test is the guard for that mistake.
func TestBashJobsSchemaAndRLS(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := Open(ctx, Config{DSN: dsn, MaxConns: 4, MinConns: 1})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	var enabled, forced bool
	if err := st.pool.QueryRow(ctx,
		`SELECT c.relrowsecurity, c.relforcerowsecurity
		   FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = current_schema() AND c.relname = 'bash_jobs'`).
		Scan(&enabled, &forced); err != nil {
		t.Fatalf("bash_jobs catalog row: %v", err)
	}
	if !enabled || !forced {
		t.Fatalf("bash_jobs row security: enabled=%v forced=%v, want both true", enabled, forced)
	}
	var policies int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_policies WHERE schemaname = current_schema()
		    AND tablename = 'bash_jobs' AND policyname = 'tenant_isolation'`).Scan(&policies); err != nil {
		t.Fatalf("bash_jobs policies: %v", err)
	}
	if policies != 1 {
		t.Fatalf("bash_jobs tenant_isolation policies = %d, want 1", policies)
	}
	// The table must be in the migration list that produced that policy.
	found := false
	for _, table := range rlsTenantTables {
		if table == "bash_jobs" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("bash_jobs is missing from rlsTenantTables")
	}
}
