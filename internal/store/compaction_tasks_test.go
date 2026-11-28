package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestCompactionTasksSchemaAndRLS pins the two non-obvious integration points of
// the durable compaction task row: it exists after the ordinary migration, is
// part of the readiness contract, and is a tenant table (ENABLE + FORCE row
// level security with the tenant_isolation policy). A table added to
// schema.sql without rlsTenantTables would silently be cross-tenant readable.
func TestCompactionTasksSchemaAndRLS(t *testing.T) {
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
		  WHERE n.nspname = current_schema() AND c.relname = 'compaction_tasks'`).
		Scan(&enabled, &forced); err != nil {
		t.Fatalf("compaction_tasks catalog row: %v", err)
	}
	if !enabled || !forced {
		t.Fatalf("compaction_tasks row security: enabled=%v forced=%v, want both true", enabled, forced)
	}
	var policies int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_policies WHERE schemaname = current_schema()
		    AND tablename = 'compaction_tasks' AND policyname = 'tenant_isolation'`).Scan(&policies); err != nil {
		t.Fatalf("compaction_tasks policies: %v", err)
	}
	if policies != 1 {
		t.Fatalf("compaction_tasks tenant_isolation policies = %d, want 1", policies)
	}
	found := false
	for _, table := range rlsTenantTables {
		if table == "compaction_tasks" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("compaction_tasks is missing from rlsTenantTables")
	}
	required := false
	for _, table := range requiredTables {
		if table == "compaction_tasks" {
			required = true
			break
		}
	}
	if !required {
		t.Fatal("compaction_tasks is missing from requiredTables (readiness)")
	}
}
