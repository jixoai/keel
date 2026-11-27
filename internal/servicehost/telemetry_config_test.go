package servicehost

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/servicegraph"
	"github.com/jixoai/keel/internal/telemetry"
)

// The telemetry factory must never create an exporter: OTLP export belongs to
// the optional keel.telemetry plugin, so a configured-but-disabled plugin sends
// nothing. These cases are the configuration matrix of task 1.3.
func TestTelemetryFactoryContexts(t *testing.T) {
	cases := []struct {
		name        string
		config      host.TelemetryConfig
		wantState   telemetry.PluginState
		wantValid   bool
		wantExports int
	}{
		{
			name:        "adapter otlp without endpoint stays inert",
			config:      host.TelemetryConfig{Adapter: "otlp"},
			wantState:   telemetry.PluginDisabled,
			wantValid:   false,
			wantExports: 0,
		},
		{
			name:        "adapter otlp with endpoint still creates no exporter",
			config:      host.TelemetryConfig{Adapter: "otlp", Endpoint: "https://collector.example:4318"},
			wantState:   telemetry.PluginDisabled,
			wantValid:   true,
			wantExports: 0,
		},
		{
			name:        "adapter none",
			config:      host.TelemetryConfig{Adapter: "none"},
			wantState:   telemetry.PluginDisabled,
			wantValid:   true,
			wantExports: 0,
		},
		{
			name:        "unknown adapter does not fail boot",
			config:      host.TelemetryConfig{Adapter: "prometheus"},
			wantState:   telemetry.PluginDisabled,
			wantValid:   false,
			wantExports: 0,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			factory := newTelemetryFactory(host.TenantConfig{Telemetry: test.config}, Config{})
			instance, err := factory(context.Background(), servicegraph.Resolved{})
			if err != nil {
				t.Fatalf("factory error = %v", err)
			}
			service, ok := instance.(*telemetryService)
			if !ok || service.run == nil || service.run.Context == nil {
				t.Fatalf("unexpected telemetry instance: %#v", instance)
			}
			if service.hub == nil {
				t.Fatal("telemetry service has no hub")
			}
			if service.hub.ExporterCount() != test.wantExports {
				t.Fatalf("exporters = %d, want %d", service.hub.ExporterCount(), test.wantExports)
			}
			status := service.hub.Status()
			if status.State != test.wantState {
				t.Fatalf("state = %s, want %s", status.State, test.wantState)
			}
			if status.Config.Valid != test.wantValid {
				t.Fatalf("config valid = %v, want %v (%s)", status.Config.Valid, test.wantValid, status.Config.Reason)
			}
		})
	}
}

func TestTelemetryFactoryUsesTheProcessHub(t *testing.T) {
	hub := telemetry.NewHub(telemetry.HubOptions{TenantID: "tenant-a"})
	factory := newTelemetryFactory(host.TenantConfig{Telemetry: host.TelemetryConfig{
		Adapter:  "otlp",
		Endpoint: "https://collector.example:4318",
	}}, Config{TenantID: uuid.New(), Telemetry: TelemetryOptions{Hub: hub}})
	instance, err := factory(context.Background(), servicegraph.Resolved{})
	if err != nil {
		t.Fatal(err)
	}
	service := instance.(*telemetryService)
	if service.hub != hub {
		t.Fatalf("service hub = %p, want the process hub %p", service.hub, hub)
	}
	if status := hub.Status(); status.State != telemetry.PluginDisabled {
		t.Fatalf("hub state = %s", status.State)
	}
}

func TestTelemetryNoneUsesNoopBase(t *testing.T) {
	factory := newTelemetryFactory(host.TenantConfig{Telemetry: host.TelemetryConfig{
		Adapter: "none",
	}}, Config{})
	instance, err := factory(context.Background(), servicegraph.Resolved{})
	if err != nil {
		t.Fatal(err)
	}
	service := instance.(*telemetryService)
	if service.run.Context != telemetry.Context(service.hub) {
		t.Fatalf("adapter=none context = %T", service.run.Context)
	}
}

func TestTelemetryMemoryUsesMemoryBase(t *testing.T) {
	factory := newTelemetryFactory(host.TenantConfig{Telemetry: host.TelemetryConfig{
		Adapter: "memory",
	}}, Config{})
	instance, err := factory(context.Background(), servicegraph.Resolved{})
	if err != nil {
		t.Fatal(err)
	}
	service := instance.(*telemetryService)
	memory, ok := service.hub.Base().(*telemetry.Memory)
	if !ok {
		t.Fatalf("memory base = %T", service.hub.Base())
	}
	if len(memory.Spans()) != 0 {
		t.Fatalf("unexpected spans: %d", len(memory.Spans()))
	}
}
