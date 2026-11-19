package store

// The MCP app context table (P5.5): schema, FORCE RLS, overwrite-only upsert
// and the JSON bounds. A table added to schema.sql but not to rlsTenantTables
// would silently be cross-tenant readable; this test guards that mistake.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestMCPAppContextSchemaAndRLS(t *testing.T) {
	st := tenancyStore(t)
	ctx := context.Background()
	var enabled, forced bool
	if err := st.pool.QueryRow(ctx,
		`SELECT c.relrowsecurity, c.relforcerowsecurity
		   FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = current_schema() AND c.relname = 'mcp_app_contexts'`).
		Scan(&enabled, &forced); err != nil {
		t.Fatalf("mcp_app_contexts catalog row: %v", err)
	}
	if !enabled || !forced {
		t.Fatalf("mcp_app_contexts row security: enabled=%v forced=%v, want both true", enabled, forced)
	}
	var policies int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_policies WHERE schemaname = current_schema()
		    AND tablename = 'mcp_app_contexts' AND policyname = 'tenant_isolation'`).Scan(&policies); err != nil {
		t.Fatalf("mcp_app_contexts policies: %v", err)
	}
	if policies != 1 {
		t.Fatalf("mcp_app_contexts tenant_isolation policies = %d, want 1", policies)
	}
	found := false
	for _, table := range rlsTenantTables {
		if table == "mcp_app_contexts" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("mcp_app_contexts is missing from rlsTenantTables")
	}
}

func TestMCPAppContextUpsertOverwrites(t *testing.T) {
	st := tenancyStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "mcp-context-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	session := uuid.New()
	appID := "mcpapp_0123456789abcdef"
	first := MCPAppContext{
		TenantID: tenant, SessionID: session, AppID: appID,
		Content: json.RawMessage(`[{"type":"text","text":"old view"}]`),
	}
	if err := st.UpsertMCPAppContext(ctx, first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	second := MCPAppContext{
		TenantID: tenant, SessionID: session, AppID: appID,
		Content:    json.RawMessage(`[{"type":"text","text":"new view"}]`),
		Structured: json.RawMessage(`{"rows":3}`),
	}
	if err := st.UpsertMCPAppContext(ctx, second); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	rows, err := st.ListMCPAppContexts(ctx, tenant, session)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want the overwrite to leave one row", len(rows))
	}
	if !strings.Contains(string(rows[0].Content), "new view") {
		t.Fatalf("content = %s, want the latest value", rows[0].Content)
	}
	if rows[0].Structured == nil {
		t.Fatal("structuredContent was not stored")
	}
	// A different app in the same session is a separate row.
	other := MCPAppContext{TenantID: tenant, SessionID: session, AppID: "mcpapp_fedcba9876543210",
		Content: json.RawMessage(`[{"type":"text","text":"other"}]`)}
	if err := st.UpsertMCPAppContext(ctx, other); err != nil {
		t.Fatal(err)
	}
	if rows, _ := st.ListMCPAppContexts(ctx, tenant, session); len(rows) != 2 {
		t.Fatalf("rows after a second app = %d, want 2", len(rows))
	}
}

func TestMCPAppContextBoundsAndValidation(t *testing.T) {
	st := tenancyStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "mcp-context-bounds-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	session := uuid.New()
	base := MCPAppContext{TenantID: tenant, SessionID: session, AppID: "mcpapp_0123456789abcdef",
		Content: json.RawMessage(`{"ok":true}`)}
	if err := st.UpsertMCPAppContext(ctx, MCPAppContext{SessionID: session, AppID: "x", Content: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("a row without a tenant was accepted")
	}
	if err := st.UpsertMCPAppContext(ctx, MCPAppContext{TenantID: tenant, AppID: "x", Content: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("a row without a session was accepted")
	}
	if err := st.UpsertMCPAppContext(ctx, MCPAppContext{TenantID: tenant, SessionID: session, AppID: ""}); err == nil {
		t.Fatal("a row without an app id was accepted")
	}
	invalid := base
	invalid.Content = json.RawMessage(`{not json`)
	if err := st.UpsertMCPAppContext(ctx, invalid); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
	tooBig := base
	tooBig.Content = json.RawMessage(`{"x":"` + strings.Repeat("a", MCPAppContextMaxBytes) + `"}`)
	if err := st.UpsertMCPAppContext(ctx, tooBig); err == nil {
		t.Fatal("an over-bound document was accepted")
	}
	empty := base
	empty.Content = nil
	if err := st.UpsertMCPAppContext(ctx, empty); err == nil {
		t.Fatal("a row with neither document was accepted")
	}
}
