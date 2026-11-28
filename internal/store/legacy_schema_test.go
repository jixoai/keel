package store

// Legacy schema retirement (change keel-bot-identity-access, A4.2b3a).
//
// Alpha zero compatibility (L19): agents / agent_teams / api_keys are not kept
// as a second truth and no migration path is written. An empty leftover table is
// dropped; a table that still holds rows refuses startup with a rebuild
// instruction. These tests run against the real test database.

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestLegacyTablesAreRetired pins the applied schema: none of the retired tables
// exists after a migration.
func TestLegacyTablesAreRetired(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	for _, table := range []string{"agents", "agent_teams", "api_keys"} {
		var exists bool
		if err := st.Pool().QueryRow(ctx,
			`SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatalf("retired table %s still exists", table)
		}
	}
}

// TestLegacyRowsRefuseStartup pins the loud refusal: a table that still holds
// rows makes Migrate fail with the rebuild instruction instead of dropping the
// operator's data silently. The error is asserted before the cleanup, so a
// regression cannot pass by dropping the evidence.
func TestLegacyRowsRefuseStartup(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	for _, tc := range []struct {
		table string
		ddl   string
		want  string
	}{
		{
			table: "agents",
			ddl:   `CREATE TABLE public.agents (tenant_id uuid NOT NULL)`,
			want:  "legacy agent registry still holds rows",
		},
		{
			table: "agent_teams",
			ddl:   `CREATE TABLE public.agent_teams (tenant_id uuid NOT NULL)`,
			want:  "legacy agent-team table still holds rows",
		},
		{
			table: "api_keys",
			ddl: `CREATE TABLE public.api_keys (
			            tenant_id uuid NOT NULL,
			            key_id uuid NOT NULL
			        )`,
			want: "legacy tenant key table still holds rows",
		},
	} {
		t.Run(tc.table, func(t *testing.T) {
			if _, err := st.Pool().Exec(ctx, tc.ddl); err != nil {
				t.Fatalf("create legacy table: %v", err)
			}
			t.Cleanup(func() {
				_, _ = st.Pool().Exec(context.Background(), `DROP TABLE IF EXISTS public.`+tc.table+` CASCADE`)
			})
			if tc.table == "api_keys" {
				if _, err := st.Pool().Exec(ctx,
					`INSERT INTO public.api_keys (tenant_id, key_id) VALUES ($1, $2)`,
					uuid.New(), uuid.New()); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := st.Pool().Exec(ctx,
					`INSERT INTO public.`+tc.table+` (tenant_id) VALUES ($1)`, uuid.New()); err != nil {
					t.Fatal(err)
				}
			}
			err := st.Migrate(ctx)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Migrate with legacy %s rows = %v, want %q", tc.table, err, tc.want)
			}
			// The refusal leaves the table (and its rows) in place for the operator.
			var exists bool
			if err := st.Pool().QueryRow(ctx,
				`SELECT to_regclass('public.' || $1) IS NOT NULL`, tc.table).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if !exists {
				t.Fatalf("the refused migration dropped %s", tc.table)
			}
		})
	}
}

// TestLegacyEmptyTableIsDropped pins the other half: an empty leftover with the
// old shape is removed, not kept as an unreadable table.
func TestLegacyEmptyTableIsDropped(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	if _, err := st.Pool().Exec(ctx, `CREATE TABLE public.agents (tenant_id uuid NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DROP TABLE IF EXISTS public.agents CASCADE`)
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate with an empty legacy table: %v", err)
	}
	var exists bool
	if err := st.Pool().QueryRow(ctx, `SELECT to_regclass('public.agents') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("an empty legacy agents table survived the migration")
	}
}

// TestLegacyPluginRowsRefuseStartup is the plugin-split half of the loud
// refusal: the retired collab_*/board_links tables are dropped only when empty.
// A table with rows stops the migration with an export instruction instead of
// silently discarding the old supervisor/board-link truth.
func TestLegacyPluginRowsRefuseStartup(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	if _, err := st.Pool().Exec(ctx, `CREATE TABLE public.board_links (tenant_id uuid NOT NULL)`); err != nil {
		t.Fatalf("create legacy board_links: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DROP TABLE IF EXISTS public.board_links CASCADE`)
	})
	if _, err := st.Pool().Exec(ctx, `INSERT INTO public.board_links (tenant_id) VALUES ($1)`, uuid.New()); err != nil {
		t.Fatal(err)
	}
	err := st.Migrate(ctx)
	if err == nil || !strings.Contains(err.Error(), "board_links") || !strings.Contains(err.Error(), "still holds") {
		t.Fatalf("Migrate with legacy board_links rows = %v, want the export instruction", err)
	}
	var exists bool
	if err := st.Pool().QueryRow(ctx, `SELECT to_regclass('public.board_links') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("the refused migration dropped board_links")
	}
}

// TestLegacyEmptyPluginTableIsDropped pins the other half: an empty old
// collab table is retired so it cannot become an unreadable second truth.
func TestLegacyEmptyPluginTableIsDropped(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	if _, err := st.Pool().Exec(ctx, `CREATE TABLE public.collab_work_items (tenant_id uuid NOT NULL)`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate with an empty legacy collab table: %v", err)
	}
	var exists bool
	if err := st.Pool().QueryRow(ctx, `SELECT to_regclass('public.collab_work_items') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("an empty legacy collab_work_items table survived the migration")
	}
}
