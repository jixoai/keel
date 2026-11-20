package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestPlacementSchemaMigratesWithRLS pins the harness placement DDL: both
// tables exist after Migrate, RLS is enabled and forced, the tenant policy is
// present, and the runs row carries the admitting group.
func TestPlacementSchemaMigratesWithRLS(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()

	for _, table := range []string{"groups", "session_placement"} {
		var exists bool
		if err := st.Pool().QueryRow(ctx,
			`SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatalf("probe %s: %v", table, err)
		}
		if !exists {
			t.Fatalf("table %s missing after migrate", table)
		}
		if !st.RLSEnabled() {
			continue
		}
		var enabled, forced bool
		var policies int
		if err := st.Pool().QueryRow(ctx,
			`SELECT c.relrowsecurity, c.relforcerowsecurity,
			        (SELECT count(*) FROM pg_policy p
			          WHERE p.polrelid = c.oid AND p.polname = 'tenant_isolation')
			   FROM pg_class c
			  WHERE c.oid = to_regclass($1)`, table).
			Scan(&enabled, &forced, &policies); err != nil {
			t.Fatalf("probe rls on %s: %v", table, err)
		}
		if !enabled || !forced || policies != 1 {
			t.Fatalf("%s rls enabled=%v forced=%v policies=%d, want true/true/1",
				table, enabled, forced, policies)
		}
	}

	var groupColumn string
	if err := st.Pool().QueryRow(ctx,
		`SELECT data_type FROM information_schema.columns
		  WHERE table_schema = current_schema() AND table_name = 'runs' AND column_name = 'group_id'`).
		Scan(&groupColumn); err != nil {
		t.Fatalf("runs.group_id missing: %v", err)
	}
	if groupColumn != "uuid" {
		t.Fatalf("runs.group_id type = %q, want uuid", groupColumn)
	}
	var fkCount int
	if err := st.Pool().QueryRow(ctx,
		`SELECT count(*) FROM pg_constraint
		  WHERE conname = 'runs_group_fk' AND conrelid = 'runs'::regclass`).Scan(&fkCount); err != nil {
		t.Fatal(err)
	}
	if fkCount != 1 {
		t.Fatalf("runs_group_fk count = %d, want 1", fkCount)
	}
	for _, index := range []string{"runs_active_group", "runs_active_tenant", "groups_name"} {
		var exists bool
		if err := st.Pool().QueryRow(ctx,
			`SELECT to_regclass($1) IS NOT NULL`, index).Scan(&exists); err != nil {
			t.Fatalf("probe index %s: %v", index, err)
		}
		if !exists {
			t.Fatalf("index %s missing", index)
		}
	}

	// The FK is tenant-consistent: a runs row cannot name another tenant's
	// group, even when both ids are valid on their own.
	tenantA, tenantB := uuid.New(), uuid.New()
	groupB := uuid.New()
	for _, tenant := range []uuid.UUID{tenantA, tenantB} {
		if err := st.CreateTenant(ctx, tenant, "placement-fk-"+tenant.String()[:8]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO groups (tenant_id, group_id, name) VALUES ($1, $2, 'fk-other')`,
		tenantB, groupB); err != nil {
		t.Fatalf("insert group B: %v", err)
	}
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO runs (tenant_id, session_id, status, group_id) VALUES ($1, $2, 'pending', $3)`,
		tenantA, uuid.New(), groupB); err == nil {
		t.Fatal("a run admitted into another tenant's group was accepted")
	}
}

// TestPlacementRLSIsolatesTenants proves the policies on the new tables behave
// like every other tenant table: a tenant transaction sees exactly its own
// rows (even without a predicate) and cannot write a row tagged with another
// tenant.
func TestPlacementRLSIsolatesTenants(t *testing.T) {
	st := tenancyStore(t)
	if !st.RLSEnabled() {
		t.Skip("RLS is disabled for this store (KEEL_TEST_DSN cluster cannot create roles)")
	}
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	groupA, groupB := uuid.New(), uuid.New()
	sessionA, sessionB := uuid.New(), uuid.New()
	for _, tenant := range []uuid.UUID{tenantA, tenantB} {
		if err := st.CreateTenant(ctx, tenant, "placement-rls-"+tenant.String()[:8]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO groups (tenant_id, group_id, name, max_concurrent_runs)
		 VALUES ($1, $2, 'rls-a', 2), ($3, $4, 'rls-b', 2)`,
		tenantA, groupA, tenantB, groupB); err != nil {
		t.Fatalf("insert groups: %v", err)
	}
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO session_placement (tenant_id, session_id, group_id, epoch, lease_until)
		 VALUES ($1, $2, $3, 3, now() + interval '1 hour'),
		        ($4, $5, $6, 3, now() + interval '1 hour')`,
		tenantA, sessionA, groupA, tenantB, sessionB, groupB); err != nil {
		t.Fatalf("insert placements: %v", err)
	}

	if err := st.TenantScoped(ctx, tenantA, func(tx TenantTx) error {
		var groups, placements int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM groups`).Scan(&groups); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM session_placement`).Scan(&placements); err != nil {
			return err
		}
		if groups != 1 || placements != 1 {
			t.Fatalf("tenant A sees groups=%d placements=%d, want 1/1", groups, placements)
		}
		var foreign int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM session_placement WHERE session_id = $1`, sessionB).Scan(&foreign); err != nil {
			return err
		}
		if foreign != 0 {
			t.Fatalf("tenant A sees tenant B's placement")
		}
		return nil
	}); err != nil {
		t.Fatalf("tenant A scope: %v", err)
	}

	if err := st.TenantScoped(ctx, uuid.New(), func(tx TenantTx) error {
		var groups, placements int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM groups`).Scan(&groups); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM session_placement`).Scan(&placements); err != nil {
			return err
		}
		if groups != 0 || placements != 0 {
			t.Fatalf("unrelated tenant sees groups=%d placements=%d", groups, placements)
		}
		return nil
	}); err != nil {
		t.Fatalf("unrelated scope: %v", err)
	}

	err := st.TenantScoped(ctx, tenantA, func(tx TenantTx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO groups (tenant_id, group_id, name) VALUES ($1, $2, 'cross-tenant')`,
			tenantB, uuid.New())
		return err
	})
	if err == nil {
		t.Fatal("a group write tagged with another tenant was accepted")
	}
}
