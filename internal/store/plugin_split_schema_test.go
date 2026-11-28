package store

import (
	"context"
	"os"
	"testing"
)

// TestPluginSplitSchemaIsBreaking pins the change's storage cutover: the old
// collab_* / board_links names are gone, the superagent plugin owns
// superagent_*; the platform owns plugin_state/config/dependency. There is no
// compatibility view and no dual-write table.
func TestPluginSplitSchemaIsBreaking(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, Config{DSN: dsn, MaxConns: 4, MinConns: 1})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	for _, legacy := range []string{
		"collab_work_items", "collab_assignments", "collab_worker_reports",
		"collab_attentions", "collab_links", "collab_subscriptions",
		"collab_source_cursors", "collab_source_events", "collab_outbox",
		"collab_session_index", "board_links",
	} {
		var present bool
		if err := st.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, legacy).Scan(&present); err != nil {
			t.Fatalf("lookup %s: %v", legacy, err)
		}
		if present {
			t.Fatalf("legacy table %s still exists", legacy)
		}
	}
	for _, owned := range []string{
		"superagent_work_items", "superagent_assignments", "superagent_worker_reports",
		"superagent_attentions", "superagent_links", "superagent_subscriptions",
		"superagent_source_cursors", "superagent_source_events", "superagent_outbox",
		"superagent_session_index", "superagent_board_links",
		"plugin_state", "plugin_config", "plugin_dependency",
	} {
		var present bool
		if err := st.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, owned).Scan(&present); err != nil {
			t.Fatalf("lookup %s: %v", owned, err)
		}
		if !present {
			t.Fatalf("owned table %s is missing", owned)
		}
	}
	listed := make(map[string]bool, len(rlsTenantTables))
	for _, table := range rlsTenantTables {
		listed[table] = true
	}
	for _, table := range []string{
		"superagent_work_items", "superagent_board_links",
		"plugin_state", "plugin_config", "plugin_dependency",
	} {
		if !listed[table] {
			t.Fatalf("%s is missing from rlsTenantTables", table)
		}
	}
}
