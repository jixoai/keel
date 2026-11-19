package store

// The MCP app audit table (P3.7): schema, tenant isolation and the bounded
// detail. The table is the durable half of the app bridge; a table added to
// schema.sql but not to rlsTenantTables would silently be cross-tenant
// readable, which is the mistake this test guards against.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestMCPAppAuditSchemaAndRLS pins the table, the FORCE row security and the
// tenant policy.
func TestMCPAppAuditSchemaAndRLS(t *testing.T) {
	st := tenancyStore(t)
	ctx := context.Background()

	var enabled, forced bool
	if err := st.pool.QueryRow(ctx,
		`SELECT c.relrowsecurity, c.relforcerowsecurity
		   FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = current_schema() AND c.relname = 'mcp_app_audit'`).
		Scan(&enabled, &forced); err != nil {
		t.Fatalf("mcp_app_audit catalog row: %v", err)
	}
	if !enabled || !forced {
		t.Fatalf("mcp_app_audit row security: enabled=%v forced=%v, want both true", enabled, forced)
	}
	var policies int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_policies WHERE schemaname = current_schema()
		    AND tablename = 'mcp_app_audit' AND policyname = 'tenant_isolation'`).Scan(&policies); err != nil {
		t.Fatalf("mcp_app_audit policies: %v", err)
	}
	if policies != 1 {
		t.Fatalf("mcp_app_audit tenant_isolation policies = %d, want 1", policies)
	}
	found := false
	for _, table := range rlsTenantTables {
		if table == "mcp_app_audit" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("mcp_app_audit is missing from rlsTenantTables")
	}
}

// TestMCPAppAuditAppendAndList pins the write/read contract, the filters, the
// keyset cursor and the detail bound.
func TestMCPAppAuditAppendAndList(t *testing.T) {
	st := tenancyStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "mcp-audit-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	server := "audit-fixture-" + uuid.NewString()
	session := uuid.New()

	base := time.Now().UTC().Truncate(time.Microsecond)
	first, err := st.AppendMCPAppAudit(ctx, MCPAppAuditEntry{
		TS: base, TenantID: &tenant, SessionID: &session,
		Server: server, AppID: "mcpapp_0123456789abcdef",
		Action: "tools/call", Result: "ok", Detail: "tool=refresh",
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if first.AuditID == 0 {
		t.Fatal("append did not return a cursor")
	}
	second, err := st.AppendMCPAppAudit(ctx, MCPAppAuditEntry{
		TS: base.Add(time.Millisecond), TenantID: &tenant,
		Server: server, AppID: "mcpapp_0123456789abcdef",
		Action: "ui/message", Result: "ok", Detail: "branch=discard",
	})
	if err != nil {
		t.Fatalf("append second: %v", err)
	}

	entries, err := st.ListMCPAppAudit(ctx, &tenant, "", "", first.AuditID, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 || entries[0].AuditID != second.AuditID {
		t.Fatalf("after-cursor list = %+v, want only %d", entries, second.AuditID)
	}
	byServer := listAuditByServer(t, st, &tenant, server, 10)
	if len(byServer) != 2 {
		t.Fatalf("server rows = %d, want 2", len(byServer))
	}
	byAction := listAuditByAction(t, st, &tenant, server, "ui/message", 10)
	if len(byAction) != 1 || byAction[0].Action != "ui/message" {
		t.Fatalf("action filter = %+v", byAction)
	}

	// The detail is bounded and control-stripped even when a caller passes
	// something hostile.
	noisy, err := st.AppendMCPAppAudit(ctx, MCPAppAuditEntry{
		TenantID: &tenant, Server: server, AppID: "mcpapp_0123456789abcdef",
		Action: "resources/read", Result: "refused",
		Detail: "bad\x00\x07" + strings.Repeat("x", MCPAppAuditMaxDetail*2),
	})
	if err != nil {
		t.Fatalf("append noisy: %v", err)
	}
	if len(noisy.Detail) > MCPAppAuditMaxDetail {
		t.Fatalf("detail = %d bytes, over the %d-byte bound", len(noisy.Detail), MCPAppAuditMaxDetail)
	}
	if strings.ContainsAny(noisy.Detail, "\x00\x07") {
		t.Fatalf("detail kept control characters: %q", noisy.Detail)
	}
}

func listAuditByServer(t *testing.T, st *Store, tenant *uuid.UUID, server string, limit int) []MCPAppAuditEntry {
	t.Helper()
	rows, err := st.pool.Query(context.Background(),
		`SELECT audit_id, ts, tenant_id, session_id, server, app_id, action, result, detail
		   FROM mcp_app_audit WHERE server = $1 ORDER BY audit_id`, server)
	if err != nil {
		t.Fatalf("query by server: %v", err)
	}
	defer rows.Close()
	out := make([]MCPAppAuditEntry, 0, 2)
	for rows.Next() {
		var entry MCPAppAuditEntry
		if err := rows.Scan(&entry.AuditID, &entry.TS, &entry.TenantID, &entry.SessionID,
			&entry.Server, &entry.AppID, &entry.Action, &entry.Result, &entry.Detail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, entry)
	}
	return out
}

func listAuditByAction(t *testing.T, st *Store, tenant *uuid.UUID, server, action string, limit int) []MCPAppAuditEntry {
	t.Helper()
	rows, err := st.pool.Query(context.Background(),
		`SELECT audit_id, ts, tenant_id, session_id, server, app_id, action, result, detail
		   FROM mcp_app_audit WHERE server = $1 AND action = $2 ORDER BY audit_id`, server, action)
	if err != nil {
		t.Fatalf("query by action: %v", err)
	}
	defer rows.Close()
	out := make([]MCPAppAuditEntry, 0, 2)
	for rows.Next() {
		var entry MCPAppAuditEntry
		if err := rows.Scan(&entry.AuditID, &entry.TS, &entry.TenantID, &entry.SessionID,
			&entry.Server, &entry.AppID, &entry.Action, &entry.Result, &entry.Detail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, entry)
	}
	return out
}

// TestMCPAppAuditTenantIsolation pins the RLS half: a tenant scope sees its own
// rows only, and cannot insert a row tagged with another tenant.
func TestMCPAppAuditTenantIsolation(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	st := tenancyStore(t)
	if !st.RLSEnabled() {
		t.Skip("RLS is disabled for this store")
	}
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	for _, tenant := range []uuid.UUID{tenantA, tenantB} {
		if err := st.CreateTenant(ctx, tenant, "mcp-audit-"+tenant.String()[:8]); err != nil {
			t.Fatal(err)
		}
	}
	server := "rls-fixture-" + uuid.NewString()
	for _, tenant := range []uuid.UUID{tenantA, tenantB} {
		tenantID := tenant
		if _, err := st.AppendMCPAppAudit(ctx, MCPAppAuditEntry{
			TenantID: &tenantID, Server: server, AppID: "mcpapp_0123456789abcdef",
			Action: "tools/call", Result: "ok", Detail: "tool=x",
		}); err != nil {
			t.Fatalf("append for %s: %v", tenant, err)
		}
	}

	if err := st.TenantScoped(ctx, tenantA, func(tx TenantTx) error {
		var mine, foreign int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM mcp_app_audit WHERE server = $1 AND tenant_id = $2`, server, tenantA).Scan(&mine); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM mcp_app_audit WHERE server = $1 AND tenant_id = $2`, server, tenantB).Scan(&foreign); err != nil {
			return err
		}
		if mine != 1 {
			t.Fatalf("tenant A sees %d of its own rows, want 1", mine)
		}
		if foreign != 0 {
			t.Fatalf("tenant A sees %d tenant B rows", foreign)
		}
		// A predicate-free scan is filtered too.
		var total int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM mcp_app_audit WHERE server = $1`, server).Scan(&total); err != nil {
			return err
		}
		if total != 1 {
			t.Fatalf("predicate-free scan returned %d rows, want 1", total)
		}
		return nil
	}); err != nil {
		t.Fatalf("tenant A scope: %v", err)
	}

	// A write tagged with another tenant is refused by WITH CHECK; the failed
	// statement aborts the transaction, so the scope's commit surfaces the
	// error (the existing RLS write test has the same shape).
	err := st.TenantScoped(ctx, tenantA, func(tx TenantTx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO mcp_app_audit (tenant_id, server, app_id, action, result) VALUES ($1, $2, $3, $4, $5)`,
			tenantB, server, "mcpapp_0123456789abcdef", "tools/call", "ok")
		return err
	})
	if err == nil {
		t.Fatal("a cross-tenant audit insert was accepted")
	}
}
