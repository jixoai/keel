package harnessext

// Real-PostgreSQL coverage for the telemetry plugin lifecycle (task 7.1/8.1):
// enable/disable intent and the telemetry audit projection survive a manager
// restart. Skipped without KEEL_TEST_DSN.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/pluginpkg/builtin"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/telemetry"
)

func TestTelemetryPluginPersistenceWithPostgres(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "telemetry-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM plugin_audit WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM plugins WHERE tenant_id = $1`, tenant)
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM tenants WHERE tenant_id = $1`, tenant)
	})

	collector := newTelemetryCollector(t)
	manifest, ok := builtin.Manifest(TelemetryPluginID)
	if !ok {
		t.Fatal("keel.telemetry manifest is missing")
	}
	newManager := func(hub *telemetry.Hub) *Manager {
		config := telemetry.Config{
			Adapter:  "otlp",
			Endpoint: collector.server.URL,
			Signals:  []string{"traces", "metrics"},
			TenantID: tenant.String(),
		}
		var manager *Manager
		runtime := NewRuntime(RuntimeOptions{
			Telemetry: hub,
			Logger:    testLogger(),
			Audit: func(ctx context.Context, pluginID, version, action, result string, detail map[string]any) {
				if manager == nil {
					return
				}
				tenantID := tenant
				manager.RecordAudit(ctx, AuditContext{TenantID: &tenantID, Actor: "system:test"}, pluginID, version, action, result, detail)
			},
		})
		var err error
		manager, err = NewPluginManager(ManagerOptions{
			TenantID: tenant, Runtime: runtime, Persistence: st,
			Builtins: []Builtin{{
				Manifest:       manifest,
				DefaultEnabled: false,
				Factory: func() *Plugin {
					return NewTelemetryPlugin(TelemetryPluginOptions{TenantID: tenant.String(), Config: config, Logger: testLogger()})
				},
			}},
			KeelVersion: "0.1.0",
			Logger:      testLogger(),
			AuditDetail: func(pluginID string) map[string]any {
				if pluginID != TelemetryPluginID {
					return nil
				}
				status := hub.Status()
				return map[string]any{
					"signals":      telemetrySignalNames(status.Signals),
					"endpointHost": status.EndpointHost,
					"sampleRatio":  status.SampleRatio,
				}
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.Boot(ctx); err != nil {
			t.Fatal(err)
		}
		return manager
	}

	hub := telemetry.NewHub(telemetry.HubOptions{TenantID: tenant.String(), Base: telemetry.NoopContext, Logger: testLogger()})
	manager := newManager(hub)
	if _, err := manager.Enable(ctx, TelemetryPluginID, AuditContext{TenantID: &tenant, Actor: "pg-test"}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := st.GetPlugin(ctx, tenant, TelemetryPluginID); err != nil || !found {
		t.Fatalf("plugin row found=%v err=%v", found, err)
	}
	rows, err := st.ListPluginAudit(ctx, tenant, TelemetryPluginID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("no telemetry audit rows")
	}
	detail := string(rows[len(rows)-1].Detail)
	if !strings.Contains(detail, "endpointHost") || !strings.Contains(detail, "signals") {
		t.Fatalf("audit detail = %s", detail)
	}
	var configureDetail string
	for _, row := range rows {
		if row.Action == store.PluginActionConfigure {
			configureDetail = string(row.Detail)
		}
	}
	if configureDetail == "" || !strings.Contains(configureDetail, "signals") || !strings.Contains(configureDetail, "sampleRatio") {
		t.Fatalf("configure audit row = %q", configureDetail)
	}
	_ = hub.Close(context.Background())

	// A restarted manager reads the persisted enable state and starts the
	// exporter from it.
	restartedHub := telemetry.NewHub(telemetry.HubOptions{TenantID: tenant.String(), Base: telemetry.NoopContext, Logger: testLogger()})
	restarted := newManager(restartedHub)
	if status := restartedHub.Status(); status.State != telemetry.PluginActive {
		t.Fatalf("restarted state = %s", status.State)
	}
	if _, err := restarted.Disable(ctx, TelemetryPluginID, AuditContext{TenantID: &tenant, Actor: "pg-test"}); err != nil {
		t.Fatal(err)
	}
	if got := restartedHub.ExporterCount(); got != 0 {
		t.Fatalf("disable left %d exporters", got)
	}
	_ = restartedHub.Close(context.Background())
}
