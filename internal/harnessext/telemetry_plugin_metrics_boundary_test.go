package harnessext

// Boundary gate (task 6.1): telemetry must not become a second Prometheus
// surface. This is a source-level assertion because the registry is a process
// singleton the plugin never receives: importing internal/metrics in the
// telemetry path would be the first step towards double-writing a family.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jixoai/keel/internal/metrics"
	"github.com/jixoai/keel/internal/telemetry"
)

func TestTelemetryPathDoesNotImportMetricsRegistry(t *testing.T) {
	files := []string{
		"telemetry_plugin.go",
		"../telemetry/hub.go",
		"../telemetry/otlp.go",
		"../telemetry/config.go",
		"../telemetry/signals.go",
		"../telemetry/metrics.go",
	}
	for _, file := range files {
		content, err := os.ReadFile(filepath.Clean(file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, forbidden := range []string{
			"github.com/jixoai/keel/internal/metrics",
			"prometheus/client_golang",
			"remote_write",
			"promhttp",
		} {
			if bytes.Contains(content, []byte(forbidden)) {
				t.Fatalf("%s references %q: telemetry is not a second metrics registry", file, forbidden)
			}
		}
	}
}

func TestTelemetryExportLeavesMetricsRegistryEmpty(t *testing.T) {
	registry := metrics.NewRegistry()
	before := registry.Snapshot(context.Background())

	collector := newTelemetryCollector(t)
	harness := newTelemetryHarness(t, telemetryFixture{config: telemetry.Config{Adapter: "otlp", Endpoint: collector.server.URL}})
	if _, err := harness.manager.Enable(context.Background(), TelemetryPluginID, AuditContext{TenantID: &harness.tenant, Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := harness.hub.StartSpan(telemetry.SpanOptions{Name: "keel.agent.run"}, func(telemetry.Span) error { return nil }); err != nil {
		t.Fatal(err)
	}
	harness.hub.Metrics().Counter("keel.agent.runs", nil).Add(1, nil)
	_ = harness.hub.Flush(context.Background())

	after := registry.Snapshot(context.Background())
	if len(after) != len(before) {
		t.Fatalf("telemetry changed the Prometheus registry: before %d families, after %d", len(before), len(after))
	}
}
