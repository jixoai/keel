package servicehost

import (
	"context"
	"testing"

	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/servicegraph"
	"github.com/jixoai/keel/internal/telemetry"
)

func TestTelemetryOTLPWithoutEndpointStaysInProcess(t *testing.T) {
	factory := newTelemetryFactory(host.TenantConfig{Telemetry: host.TelemetryConfig{
		Adapter: "otlp",
	}}, Config{})
	instance, err := factory(context.Background(), servicegraph.Resolved{})
	if err != nil {
		t.Fatal(err)
	}
	service, ok := instance.(*telemetryService)
	if !ok || service.run == nil || service.run.Context == nil {
		t.Fatalf("unexpected telemetry instance: %#v", instance)
	}
	if service.shutdown != nil {
		t.Fatal("OTLP without endpoint created an exporter shutdown")
	}
}

func TestTelemetryRejectsUnknownAdapter(t *testing.T) {
	factory := newTelemetryFactory(host.TenantConfig{Telemetry: host.TelemetryConfig{
		Adapter: "nope",
	}}, Config{})
	if _, err := factory(context.Background(), servicegraph.Resolved{}); err == nil {
		t.Fatal("unknown telemetry adapter was accepted")
	}
}

func TestTelemetryNoneUsesNoop(t *testing.T) {
	factory := newTelemetryFactory(host.TenantConfig{Telemetry: host.TelemetryConfig{
		Adapter: "none",
	}}, Config{})
	instance, err := factory(context.Background(), servicegraph.Resolved{})
	if err != nil {
		t.Fatal(err)
	}
	service := instance.(*telemetryService)
	if service.run.Context != telemetry.NoopContext {
		t.Fatalf("adapter=none context = %T", service.run.Context)
	}
}
