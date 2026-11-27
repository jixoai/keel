package harnessapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/telemetry"
)

type statusExporter struct {
	status telemetry.ExporterStatus
}

func (s statusExporter) Name() string                               { return s.status.Name }
func (s statusExporter) Signals() []telemetry.Signal                { return s.status.Signals }
func (s statusExporter) ExportSpan(telemetry.StoredSpan)            {}
func (s statusExporter) ExportMetric(telemetry.RecordedMeasurement) {}
func (s statusExporter) ExportLog(telemetry.LogRecord)              {}
func (s statusExporter) Status() telemetry.ExporterStatus           { return s.status }
func (s statusExporter) Flush(context.Context) error                { return nil }
func (s statusExporter) Close(context.Context) error                { return nil }

func TestTelemetryStatusRoute(t *testing.T) {
	hub := telemetry.NewHub(telemetry.HubOptions{TenantID: pluginTestTenant.String(), Logger: testLogger()})
	ratio := 0.25
	hub.Configure(telemetry.SummarizeConfig(telemetry.Config{
		Adapter:     "otlp",
		Endpoint:    "https://collector.example:4318",
		Headers:     map[string]string{"Authorization": "secret:OTLP_TOKEN"},
		SampleRatio: &ratio,
		Signals:     []string{"traces", "metrics"},
	}))
	hub.SetPluginState(telemetry.PluginActive, "")
	if err := hub.RegisterExporter(statusExporter{status: telemetry.ExporterStatus{
		Name: "otlp-http", State: "active", Signals: []telemetry.Signal{telemetry.SignalTraces, telemetry.SignalMetrics},
		EndpointHost: "collector.example:4318", QueueDepth: 3, Dropped: 2, Exported: 9, LastErrorClass: "http_5xx",
	}}); err != nil {
		t.Fatal(err)
	}
	harness := newPluginAPITest(t, &fakePluginManager{}, func(cfg *Config) { cfg.Telemetry = hub })

	var body telemetryStatusResponse
	code := harness.write(t, http.MethodGet, "/v1/telemetry/status", pluginTestTenant, nil, &body)
	if code != http.StatusOK {
		t.Fatalf("status code = %d", code)
	}
	if body.State != telemetry.PluginActive || !body.Enabled {
		t.Fatalf("body = %#v", body)
	}
	if body.EndpointHost != "collector.example:4318" || body.SampleRatio != 0.25 {
		t.Fatalf("body = %#v", body)
	}
	if body.QueueDepth != 3 || body.Dropped != 2 || body.Exported != 9 || body.LastErrorClass != "http_5xx" {
		t.Fatalf("counters = %#v", body)
	}
	if len(body.HeaderNames) != 1 || body.HeaderNames[0] != "Authorization" || body.HeaderCount != 1 {
		t.Fatalf("headers = %#v", body)
	}
	if strings.Contains(body.HeaderNames[0], "OTLP_TOKEN") {
		t.Fatal("status leaked a secret reference")
	}
	// Cross-tenant read answers 404 and never renders the endpoint.
	other := uuid.New()
	var errBody map[string]any
	if code := harness.write(t, http.MethodGet, "/v1/telemetry/status", other, nil, &errBody); code != http.StatusNotFound {
		t.Fatalf("cross-tenant status = %d, want 404", code)
	}
	if !strings.Contains(strings.ToLower(strings.Join(mapValues(errBody), " ")), "telemetry") {
		t.Fatalf("error body = %#v", errBody)
	}
}

func TestTelemetryStatusUnboundHubIsUnavailable(t *testing.T) {
	hub := telemetry.NewHub(telemetry.HubOptions{Logger: testLogger()})
	harness := newPluginAPITest(t, &fakePluginManager{}, func(cfg *Config) { cfg.Telemetry = hub })
	var body map[string]any
	if code := harness.write(t, http.MethodGet, "/v1/telemetry/status", pluginTestTenant, nil, &body); code != http.StatusServiceUnavailable {
		t.Fatalf("unbound hub status = %d, want 503", code)
	}
}

func TestTelemetryStatusNotWired(t *testing.T) {
	harness := newPluginAPITest(t, &fakePluginManager{})
	var body map[string]any
	if code := harness.write(t, http.MethodGet, "/v1/telemetry/status", pluginTestTenant, nil, &body); code != http.StatusServiceUnavailable {
		t.Fatalf("status code = %d, want 503", code)
	}
}

func TestTelemetryApprovalPromptShowsSanitizedConfig(t *testing.T) {
	approvalLines := []string{
		"exporter=otlp",
		"signals=traces,metrics",
		"sampleRatio=0.25",
		"endpointHost=collector.example:4318",
		"headers=Authorization",
	}
	harness := newPluginAPITest(t, &fakePluginManager{}, func(cfg *Config) {
		cfg.TelemetryApproval = func() []string { return approvalLines }
	})
	var asked struct {
		Decision struct {
			Prompt string `json:"prompt"`
		} `json:"decision"`
	}
	if code := harness.write(t, http.MethodPost, "/v1/plugins/keel.telemetry/enable", pluginTestTenant, nil, &asked); code != http.StatusAccepted {
		t.Fatalf("enable without a decision = %d", code)
	}
	for _, want := range []string{"exporter=otlp", "endpointHost=collector.example:4318", "headers=Authorization"} {
		if !strings.Contains(asked.Decision.Prompt, want) {
			t.Fatalf("prompt misses %q: %s", want, asked.Decision.Prompt)
		}
	}
	if strings.Contains(asked.Decision.Prompt, "secret:") {
		t.Fatalf("prompt leaked a secret reference: %s", asked.Decision.Prompt)
	}
}

func mapValues(values map[string]any) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			out = append(out, text)
		}
	}
	return out
}
