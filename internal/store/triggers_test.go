package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestTriggerStateSchemaAndRLS pins the two non-obvious integration points of
// the W10 catch-up cursor: the table exists after the ordinary migration, and it
// is a tenant table (ENABLE + FORCE row level security with the
// tenant_isolation policy). A table added to schema.sql but not to
// rlsTenantTables would silently be readable across tenants.
func TestTriggerStateSchemaAndRLS(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()

	var enabled, forced bool
	if err := st.pool.QueryRow(ctx,
		`SELECT c.relrowsecurity, c.relforcerowsecurity
		   FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = current_schema() AND c.relname = 'trigger_state'`).
		Scan(&enabled, &forced); err != nil {
		t.Fatalf("trigger_state catalog row: %v", err)
	}
	if !enabled || !forced {
		t.Fatalf("trigger_state row security: enabled=%v forced=%v, want both true", enabled, forced)
	}
	var policies int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_policies WHERE schemaname = current_schema()
		    AND tablename = 'trigger_state' AND policyname = 'tenant_isolation'`).Scan(&policies); err != nil {
		t.Fatalf("trigger_state policies: %v", err)
	}
	if policies != 1 {
		t.Fatalf("trigger_state tenant_isolation policies = %d, want 1", policies)
	}
	found := false
	for _, table := range rlsTenantTables {
		if table == "trigger_state" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("trigger_state is missing from rlsTenantTables")
	}
}

// TestTriggerStateRoundTrip pins the cursor contract: upsert, read, the
// never-go-backwards guarantee that keeps two processes from replaying an older
// window, and tenant isolation.
func TestTriggerStateRoundTrip(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "trigger-state-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	triggerID := "test.tick"
	base := time.Now().UTC().Truncate(time.Microsecond)
	if _, ok, err := st.LoadTriggerState(ctx, tenant, triggerID); err != nil || ok {
		t.Fatalf("fresh load: ok=%v err=%v", ok, err)
	}
	first := TriggerState{
		TenantID:        tenant,
		TriggerID:       triggerID,
		LastScheduledAt: base,
		LastFiredAt:     base.Add(time.Second),
		LastStatus:      "fired",
	}
	if err := st.SaveTriggerState(ctx, first); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := st.LoadTriggerState(ctx, tenant, triggerID)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if !loaded.LastScheduledAt.Equal(first.LastScheduledAt) || loaded.LastStatus != "fired" {
		t.Fatalf("loaded = %+v", loaded)
	}

	// A late writer carrying an older window must not move the cursor back.
	older := first
	older.LastScheduledAt = base.Add(-time.Minute)
	older.LastStatus = "skipped"
	if err := st.SaveTriggerState(ctx, older); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err = st.LoadTriggerState(ctx, tenant, triggerID)
	if err != nil || !ok || !loaded.LastScheduledAt.Equal(base) || loaded.LastStatus != "fired" {
		t.Fatalf("monotonic cursor = %+v ok=%v err=%v", loaded, ok, err)
	}

	// A newer window advances both values.
	newer := first
	newer.LastScheduledAt = base.Add(time.Minute)
	newer.LastFiredAt = base.Add(2 * time.Minute)
	newer.LastStatus = "deduped"
	if err := st.SaveTriggerState(ctx, newer); err != nil {
		t.Fatal(err)
	}
	loaded, _, err = st.LoadTriggerState(ctx, tenant, triggerID)
	if err != nil || !loaded.LastScheduledAt.Equal(newer.LastScheduledAt) || loaded.LastStatus != "deduped" {
		t.Fatalf("advanced cursor = %+v err=%v", loaded, err)
	}

	// Another tenant sees no row: the cursor is tenant-scoped.
	if _, ok, err := st.LoadTriggerState(ctx, uuid.New(), triggerID); err != nil || ok {
		t.Fatalf("cross-tenant load: ok=%v err=%v", ok, err)
	}
}
