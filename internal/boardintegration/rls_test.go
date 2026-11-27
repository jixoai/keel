package boardintegration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// assertBoardRLS proves the board tables carry the board's own
// (keel.tenant_id) FORCE RLS and that a restricted role sees nothing without a
// tenant setting. It also proves keel's app.tenant_id policy was NOT applied to
// these tables: two policies with different settings would fail closed against
// each other, which is the architecture decision this change records.
func assertBoardRLS(t *testing.T, ctx context.Context, dsn, boardSchema string, tenantA, tenantB uuid.UUID) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	schemaIdent := quoteIdent(boardSchema)
	if _, err := conn.Exec(ctx, "SET search_path TO "+schemaIdent); err != nil {
		t.Fatalf("set search_path: %v", err)
	}

	// Every board table is ENABLE+FORCE.
	rows, err := conn.Query(ctx, `
		SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname LIKE 'board\_%' AND c.relkind = 'r'
		ORDER BY c.relname`, boardSchema)
	if err != nil {
		t.Fatalf("read RLS flags: %v", err)
	}
	tables := 0
	for rows.Next() {
		var name string
		var enabled, forced bool
		if err := rows.Scan(&name, &enabled, &forced); err != nil {
			rows.Close()
			t.Fatalf("scan RLS flags: %v", err)
		}
		if !enabled || !forced {
			rows.Close()
			t.Fatalf("%s: rls=%v force=%v", name, enabled, forced)
		}
		tables++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("RLS rows: %v", err)
	}
	if tables == 0 {
		t.Fatal("no board tables were found in the test schema")
	}

	// The policy must read keel.tenant_id, never app.tenant_id.
	policies, err := conn.Query(ctx, `
		SELECT tablename, qual, with_check
		FROM pg_policies WHERE schemaname = $1 AND tablename LIKE 'board\_%'`, boardSchema)
	if err != nil {
		t.Fatalf("read policies: %v", err)
	}
	policyTables := 0
	seen := make(map[string]bool)
	for policies.Next() {
		var table string
		var qual, check *string
		if err := policies.Scan(&table, &qual, &check); err != nil {
			policies.Close()
			t.Fatalf("scan policy: %v", err)
		}
		seen[table] = true
		policyTables++
		text := ""
		if qual != nil {
			text += *qual
		}
		if check != nil {
			text += *check
		}
		if strings.Contains(text, "app.tenant_id") {
			policies.Close()
			t.Fatalf("%s carries keel's app.tenant_id policy as well: %s", table, text)
		}
		if !strings.Contains(text, "keel.tenant_id") {
			policies.Close()
			t.Fatalf("%s policy does not read keel.tenant_id: %s", table, text)
		}
	}
	policies.Close()
	if err := policies.Err(); err != nil {
		t.Fatalf("policy rows: %v", err)
	}
	if policyTables == 0 || len(seen) != tables {
		t.Fatalf("policy coverage = %d policies on %d tables, want every board table", policyTables, tables)
	}

	// A restricted role proves the boundary is real DB enforcement, not only
	// the query predicates: unset setting -> zero rows; tenant A -> only A; a
	// forged tenant B insert is refused.
	var currentUser string
	if err := conn.QueryRow(ctx, `SELECT current_user`).Scan(&currentUser); err != nil {
		t.Fatalf("current_user: %v", err)
	}
	role := fmt.Sprintf("keel_board_rls_%d", time.Now().UnixNano())
	roleIdent := quoteIdent(role)
	userIdent := quoteIdent(currentUser)
	if _, err := conn.Exec(ctx, `CREATE ROLE `+roleIdent+` NOLOGIN`); err != nil {
		t.Fatalf("cannot create a restricted role in this database (CI must be able to prove RLS): %v", err)
	}
	if _, err := conn.Exec(ctx, `GRANT `+roleIdent+` TO `+userIdent); err != nil {
		_, _ = conn.Exec(ctx, `DROP ROLE `+roleIdent)
		t.Fatalf("grant restricted role: %v", err)
	}
	t.Cleanup(func() {
		// The test connection is closed by the defer above before cleanups run;
		// the role cleanup needs its own connection.
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		admin, err := pgx.Connect(cleanup, dsn)
		if err != nil {
			return
		}
		defer admin.Close(context.Background())
		_, _ = admin.Exec(cleanup, `DROP OWNED BY `+roleIdent)
		_, _ = admin.Exec(cleanup, `REVOKE `+roleIdent+` FROM `+userIdent)
		_, _ = admin.Exec(cleanup, `DROP ROLE `+roleIdent)
	})
	if _, err := conn.Exec(ctx, `GRANT USAGE ON SCHEMA `+schemaIdent+` TO `+roleIdent); err != nil {
		t.Fatalf("grant schema usage: %v", err)
	}
	if _, err := conn.Exec(ctx, `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA `+schemaIdent+` TO `+roleIdent); err != nil {
		t.Fatalf("grant table access: %v", err)
	}
	if _, err := conn.Exec(ctx, `GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA `+schemaIdent+` TO `+roleIdent); err != nil {
		t.Fatalf("grant sequence access: %v", err)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+roleIdent); err != nil {
		tx.Rollback(ctx)
		t.Fatalf("set role: %v", err)
	}
	var unset int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM board_events`).Scan(&unset); err != nil {
		tx.Rollback(ctx)
		t.Fatalf("count without a tenant: %v", err)
	}
	tx.Rollback(ctx)
	if unset != 0 {
		t.Fatalf("unset tenant saw %d board events, want 0", unset)
	}

	tx, err = conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+roleIdent); err != nil {
		tx.Rollback(ctx)
		t.Fatalf("set role A: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('keel.tenant_id', $1, true)`, tenantA.String()); err != nil {
		tx.Rollback(ctx)
		t.Fatalf("set tenant A: %v", err)
	}
	var visibleA int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM board_events`).Scan(&visibleA); err != nil {
		tx.Rollback(ctx)
		t.Fatalf("count tenant A: %v", err)
	}
	if visibleA == 0 {
		tx.Rollback(ctx)
		t.Fatal("tenant A saw no board events")
	}
	_, insertErr := tx.Exec(ctx, `
		INSERT INTO board_events (tenant_id, seq, kind, payload)
		VALUES ($1, 999999, 'forged', '{}'::jsonb)`, tenantB.String())
	if insertErr == nil {
		tx.Rollback(ctx)
		t.Fatal("a cross-tenant board_events insert succeeded under tenant A's setting")
	}
	tx.Rollback(ctx)

	tx, err = conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin B: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+roleIdent); err != nil {
		t.Fatalf("set role B: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('keel.tenant_id', $1, true)`, tenantB.String()); err != nil {
		t.Fatalf("set tenant B: %v", err)
	}
	var visibleB int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM board_events`).Scan(&visibleB); err != nil {
		t.Fatalf("count tenant B: %v", err)
	}
	if visibleB != 0 {
		t.Fatalf("tenant B saw %d tenant A board events", visibleB)
	}
}
