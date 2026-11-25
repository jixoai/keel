package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Plugin inventory and lifecycle audit rows (changes keel-plugin-core and
// keel-creation). Skipped when KEEL_TEST_DSN is unset, like the other store
// integration tests. Every row is tenant-scoped; the negative controls name the
// other tenant explicitly.

func pluginTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	st, err := Open(context.Background(), Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func TestPluginInventoryRoundTrip(t *testing.T) {
	st := pluginTestStore(t)
	ctx := context.Background()
	id := "vendor.roundtrip-" + uuid.NewString()[:8]
	tenant, other := uuid.New(), uuid.New()
	installed := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
	record := PluginRecord{
		TenantID:    tenant,
		ID:          id,
		Version:     "1.2.3",
		Kind:        "webview",
		State:       PluginStateInstalled,
		Bundled:     false,
		Source:      json.RawMessage(`{"type":"local","path":"/tmp/pkg"}`),
		Manifest:    json.RawMessage(`{"id":"` + id + `","version":"1.2.3","kind":"webview"}`),
		ContentHash: "abc123",
		Path:        "/var/keel/plugins/" + id,
		Diagnostics: []string{"first reason", "second reason"},
		InstalledAt: installed,
		UpdatedAt:   installed,
	}
	if err := st.UpsertPlugin(ctx, record); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.DeletePlugin(ctx, tenant, id)
		_, _ = st.Pool().Exec(ctx, `DELETE FROM plugin_audit WHERE plugin_id = $1`, id)
	})

	row, found, err := st.GetPlugin(ctx, tenant, id)
	if err != nil || !found {
		t.Fatalf("get plugin: %v found=%v", err, found)
	}
	if row.Version != "1.2.3" || row.Kind != "webview" || row.State != PluginStateInstalled || row.ContentHash != "abc123" {
		t.Fatalf("row = %+v", row)
	}
	if len(row.Diagnostics) != 2 || row.Diagnostics[1] != "second reason" {
		t.Fatalf("diagnostics = %v", row.Diagnostics)
	}
	var source map[string]any
	if err := json.Unmarshal(row.Source, &source); err != nil || source["type"] != "local" {
		t.Fatalf("source = %s (%v)", row.Source, err)
	}
	if !row.InstalledAt.Equal(installed) {
		t.Fatalf("installed_at = %s, want %s", row.InstalledAt, installed)
	}

	// A state change must not reset the installation age.
	enabled := time.Now().UTC().Truncate(time.Millisecond)
	record.State = PluginStateEnabled
	record.EnabledAt = &enabled
	record.Diagnostics = nil
	record.UpdatedAt = enabled
	if err := st.UpsertPlugin(ctx, record); err != nil {
		t.Fatal(err)
	}
	row, found, err = st.GetPlugin(ctx, tenant, id)
	if err != nil || !found {
		t.Fatalf("get plugin: %v found=%v", err, found)
	}
	if row.State != PluginStateEnabled || row.EnabledAt == nil || !row.EnabledAt.Equal(enabled) {
		t.Fatalf("row after enable = %+v", row)
	}
	if !row.InstalledAt.Equal(installed) {
		t.Fatalf("installed_at was reset: %s", row.InstalledAt)
	}
	if len(row.Diagnostics) != 0 {
		t.Fatalf("diagnostics should be cleared: %v", row.Diagnostics)
	}

	rows, err := st.ListPlugins(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, listed := range rows {
		if listed.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("list did not include %s", id)
	}

	// Cross-tenant negative controls: another tenant cannot read or delete the
	// row, and its list never contains it.
	if _, found, err := st.GetPlugin(ctx, other, id); err != nil || found {
		t.Fatalf("cross-tenant get = found %v err %v", found, err)
	}
	if listed, err := st.ListPlugins(ctx, other); err != nil {
		t.Fatal(err)
	} else {
		for _, row := range listed {
			if row.ID == id {
				t.Fatalf("cross-tenant list leaked %s", id)
			}
		}
	}
	if removed, err := st.DeletePlugin(ctx, other, id); err != nil || removed {
		t.Fatalf("cross-tenant delete = %v %v", removed, err)
	}
	if _, found, err := st.GetPlugin(ctx, tenant, id); err != nil || !found {
		t.Fatalf("cross-tenant delete changed the owner's row: found=%v err=%v", found, err)
	}

	// A missing id is (found=false, nil), never an error.
	if _, found, err := st.GetPlugin(ctx, tenant, "vendor.nonexistent-"+uuid.NewString()[:8]); err != nil || found {
		t.Fatalf("missing plugin = found %v err %v", found, err)
	}

	removed, err := st.DeletePlugin(ctx, tenant, id)
	if err != nil || !removed {
		t.Fatalf("delete = %v %v", removed, err)
	}
	if _, found, err := st.GetPlugin(ctx, tenant, id); err != nil || found {
		t.Fatalf("deleted plugin still readable: found=%v err=%v", found, err)
	}
	if removed, err := st.DeletePlugin(ctx, tenant, id); err != nil || removed {
		t.Fatalf("second delete = %v %v", removed, err)
	}
}

func TestPluginAuditTrail(t *testing.T) {
	st := pluginTestStore(t)
	ctx := context.Background()
	id := "vendor.audit-" + uuid.NewString()[:8]
	tenant := uuid.New()
	other := uuid.New()
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(ctx, `DELETE FROM plugin_audit WHERE plugin_id = $1`, id)
	})

	results := []string{PluginResultOK, PluginResultFailed, PluginResultRefused, PluginResultOK}
	for i, action := range []string{PluginActionInstall, PluginActionEnable, PluginActionEnable, PluginActionDisable} {
		entry, err := st.AppendPluginAudit(ctx, PluginAuditEntry{
			TS:       time.Now().UTC().Add(time.Duration(i) * time.Millisecond),
			TenantID: tenant,
			Actor:    "tester",
			PluginID: id,
			Version:  "1.0.0",
			Action:   action,
			Result:   results[i],
			Detail:   json.RawMessage(`{"reason":"test"}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		if entry.AuditID == 0 {
			t.Fatal("audit id was not assigned")
		}
	}
	// Another tenant's trail must not leak into this one's page.
	if _, err := st.AppendPluginAudit(ctx, PluginAuditEntry{
		TenantID: other, Actor: "someone", PluginID: id, Action: PluginActionUninstall, Result: PluginResultOK,
	}); err != nil {
		t.Fatal(err)
	}

	entries, err := st.ListPluginAudit(ctx, tenant, id, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("entries = %+v", entries)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].AuditID <= entries[i-1].AuditID {
			t.Fatalf("audit is not chronological: %+v", entries)
		}
	}
	if entries[0].Action != PluginActionInstall || entries[0].Actor != "tester" || entries[0].Version != "1.0.0" {
		t.Fatalf("first entry = %+v", entries[0])
	}
	if entries[0].TenantID != tenant {
		t.Fatalf("audit row lost its tenant: %+v", entries[0])
	}
	var detail map[string]any
	if err := json.Unmarshal(entries[0].Detail, &detail); err != nil || detail["reason"] != "test" {
		t.Fatalf("detail = %s (%v)", entries[0].Detail, err)
	}

	// Cursor pagination: after the second entry, two more rows are visible.
	page, err := st.ListPluginAudit(ctx, tenant, id, entries[1].AuditID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].AuditID != entries[2].AuditID {
		t.Fatalf("page = %+v", page)
	}
	// limit is bounded, and an unset limiter falls back to the default page.
	if page, err := st.ListPluginAudit(ctx, tenant, id, 0, 0); err != nil || len(page) != 4 {
		t.Fatalf("default limit page = %+v (%v)", page, err)
	}
	// The other tenant sees only its own row.
	otherEntries, err := st.ListPluginAudit(ctx, other, id, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(otherEntries) != 1 || otherEntries[0].TenantID != other {
		t.Fatalf("other tenant entries = %+v", otherEntries)
	}
	// Unknown plugins answer an empty page, not an error.
	if page, err := st.ListPluginAudit(ctx, tenant, "vendor.unknown-"+uuid.NewString()[:8], 0, 10); err != nil || len(page) != 0 {
		t.Fatalf("unknown plugin page = %+v (%v)", page, err)
	}
}

func TestPluginAuditRequiresIdentity(t *testing.T) {
	st := pluginTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if _, err := st.AppendPluginAudit(ctx, PluginAuditEntry{Action: PluginActionInstall, Result: PluginResultOK}); err == nil {
		t.Fatal("an audit row without a plugin id must be refused")
	}
	if _, err := st.AppendPluginAudit(ctx, PluginAuditEntry{TenantID: tenant, PluginID: "vendor.x"}); err == nil {
		t.Fatal("an audit row without an action/result must be refused")
	}
	if _, err := st.AppendPluginAudit(ctx, PluginAuditEntry{PluginID: "vendor.x", Action: PluginActionInstall, Result: PluginResultOK}); err == nil {
		t.Fatal("an audit row without a tenant must be refused")
	}
	if _, err := st.ListPluginAudit(ctx, uuid.Nil, "", 0, 10); err == nil {
		t.Fatal("a tenant-less audit read must be refused")
	}
	if err := st.UpsertPlugin(ctx, PluginRecord{}); err == nil {
		t.Fatal("a plugin row without an id must be refused")
	}
	if err := st.UpsertPlugin(ctx, PluginRecord{ID: "vendor.x"}); err == nil {
		t.Fatal("a plugin row without a tenant must be refused")
	}
	if _, _, err := st.GetPlugin(ctx, uuid.Nil, "vendor.x"); err == nil {
		t.Fatal("a tenant-less plugin read must be refused")
	}
	if !errors.Is(ErrNoSession, ErrNoSession) {
		t.Fatal("unreachable")
	}
}
