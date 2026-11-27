package harnessext

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/pluginpkg/builtin"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/telemetry"
)

// telemetryCollector is the fake OTLP endpoint for plugin lifecycle tests.
type telemetryCollector struct {
	server  *httptest.Server
	mu      sync.Mutex
	traces  []string
	paths   []string
	headers []http.Header
	status  int
}

func newTelemetryCollector(t *testing.T) *telemetryCollector {
	t.Helper()
	collector := &telemetryCollector{status: http.StatusOK}
	collector.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		collector.mu.Lock()
		collector.paths = append(collector.paths, r.URL.Path)
		collector.headers = append(collector.headers, r.Header.Clone())
		if r.URL.Path == "/v1/traces" {
			collector.traces = append(collector.traces, string(body))
		}
		status := collector.status
		collector.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(collector.server.Close)
	return collector
}

func (c *telemetryCollector) setStatus(status int) {
	c.mu.Lock()
	c.status = status
	c.mu.Unlock()
}

func (c *telemetryCollector) count(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, candidate := range c.paths {
		if candidate == path {
			total++
		}
	}
	return total
}

func (c *telemetryCollector) traceBodies() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.traces, "\n")
}

func (c *telemetryCollector) lastHeaders() http.Header {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.headers) == 0 {
		return nil
	}
	return c.headers[len(c.headers)-1].Clone()
}

type telemetryHarness struct {
	tenant    uuid.UUID
	hub       *telemetry.Hub
	manager   *Manager
	runtime   *Runtime
	store     *fakePluginStore
	collector *telemetryCollector
}

type telemetryFixture struct {
	config  telemetry.Config
	lookup  func(string) (string, bool)
	enabled bool
}

func newTelemetryHarness(t *testing.T, fixture telemetryFixture) *telemetryHarness {
	t.Helper()
	tenant := uuid.New()
	hub := telemetry.NewHub(telemetry.HubOptions{TenantID: tenant.String(), Base: telemetry.NoopContext, Logger: testLogger()})
	persistence := newFakePluginStore()
	var manager *Manager
	auditSink := func(ctx context.Context, pluginID, version, action, result string, detail map[string]any) {
		if manager != nil {
			tenantID := tenant
			manager.RecordAudit(ctx, AuditContext{TenantID: &tenantID, Actor: "system:plugin-start"}, pluginID, version, action, result, detail)
		}
	}
	runtime := NewRuntime(RuntimeOptions{
		Telemetry: hub,
		Audit:     auditSink,
		Logger:    testLogger(),
	})
	manifest, ok := builtin.Manifest(TelemetryPluginID)
	if !ok {
		t.Fatal("keel.telemetry manifest is missing")
	}
	config := fixture.config
	config.TenantID = tenant.String()
	manager, err := NewPluginManager(ManagerOptions{
		TenantID:    tenant,
		Runtime:     runtime,
		Persistence: persistence,
		Builtins: []Builtin{{
			Manifest:       manifest,
			DefaultEnabled: fixture.enabled,
			Factory: func() *Plugin {
				return NewTelemetryPlugin(TelemetryPluginOptions{
					TenantID:     tenant.String(),
					Config:       config,
					LookupSecret: fixture.lookup,
					Logger:       testLogger(),
				})
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
				"adapter":      status.Config.Adapter,
				"signals":      telemetrySignalNames(status.Signals),
				"endpointHost": status.EndpointHost,
				"sampleRatio":  status.SampleRatio,
				"headerNames":  status.HeaderNames,
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = manager.Disable(context.Background(), TelemetryPluginID, AuditContext{TenantID: &tenant, Actor: "cleanup"})
		runtime.Close(context.Background())
		hub.Close(context.Background())
	})
	return &telemetryHarness{tenant: tenant, hub: hub, manager: manager, runtime: runtime, store: persistence}
}

func telemetrySignalNames(signals []telemetry.Signal) []string {
	out := make([]string, 0, len(signals))
	for _, signal := range signals {
		out = append(out, string(signal))
	}
	return out
}

func TestTelemetryPluginDefaultDisabledAndLazy(t *testing.T) {
	collector := newTelemetryCollector(t)
	harness := newTelemetryHarness(t, telemetryFixture{config: telemetry.Config{
		Adapter:  "otlp",
		Endpoint: collector.server.URL,
	}})
	if harness.hub.ExporterCount() != 0 {
		t.Fatalf("a disabled plugin registered %d exporters", harness.hub.ExporterCount())
	}
	status := harness.hub.Status()
	if status.State != telemetry.PluginDisabled || status.Enabled {
		t.Fatalf("status = %#v", status)
	}
	// A run still records through the hub, but no exporter exists and no request
	// is made. The callback executes exactly once.
	used := false
	if err := harness.hub.StartSpan(telemetry.SpanOptions{Name: "keel.agent.run"}, func(telemetry.Span) error {
		used = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !used {
		t.Fatal("callback did not run")
	}
	if err := harness.hub.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if collector.count("/v1/traces") != 0 {
		t.Fatal("a disabled plugin sent a collector request")
	}
}

func TestTelemetryPluginEnableExportsThenDisableStops(t *testing.T) {
	collector := newTelemetryCollector(t)
	harness := newTelemetryHarness(t, telemetryFixture{config: telemetry.Config{
		Adapter:  "otlp",
		Endpoint: collector.server.URL,
	}})
	if _, err := harness.manager.Enable(context.Background(), TelemetryPluginID, AuditContext{TenantID: &harness.tenant, Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	status := harness.hub.Status()
	if status.State != telemetry.PluginActive || !status.Enabled {
		t.Fatalf("enabled status = %#v", status)
	}
	if harness.hub.ExporterCount() != 1 {
		t.Fatalf("exporters = %d", harness.hub.ExporterCount())
	}
	runErr := harness.hub.StartSpan(telemetry.SpanOptions{Name: "keel.agent.run", Attributes: telemetry.SpanAttributes{
		"tenant": harness.tenant.String(), "session": "session-a", "model": "m", "provider": "p",
		"prompt": "must not travel",
	}}, func(span telemetry.Span) error {
		return span.StartSpan(telemetry.SpanOptions{Name: "keel.agent.tool", Attributes: telemetry.SpanAttributes{"tool": "search"}}, func(telemetry.Span) error {
			return nil
		})
	})
	if runErr != nil {
		t.Fatal(runErr)
	}
	harness.hub.Metrics().Counter("keel.agent.runs", nil).Add(1, telemetry.SpanAttributes{"model": "m", "provider": "p", "outcome": "ok"})
	if err := harness.hub.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if collector.count("/v1/traces") == 0 || collector.count("/v1/metrics") == 0 {
		t.Fatalf("requests = traces %d metrics %d", collector.count("/v1/traces"), collector.count("/v1/metrics"))
	}
	body := collector.traceBodies()
	for _, want := range []string{"keel.agent.run", "keel.agent.tool", harness.tenant.String()} {
		if !strings.Contains(body, want) {
			t.Fatalf("trace payload misses %q: %s", want, body)
		}
	}
	if strings.Contains(body, "must not travel") || strings.Contains(body, `"key":"prompt"`) {
		t.Fatalf("forbidden payload reached the collector: %s", body)
	}

	if _, err := harness.manager.Disable(context.Background(), TelemetryPluginID, AuditContext{TenantID: &harness.tenant, Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if harness.hub.ExporterCount() != 0 {
		t.Fatalf("disabled plugin left %d exporters", harness.hub.ExporterCount())
	}
	if status := harness.hub.Status(); status.State != telemetry.PluginDisabled {
		t.Fatalf("state = %s", status.State)
	}
	before := collector.count("/v1/traces")
	if err := harness.hub.StartSpan(telemetry.SpanOptions{Name: "after-disable"}, func(telemetry.Span) error { return nil }); err != nil {
		t.Fatal(err)
	}
	_ = harness.hub.Flush(context.Background())
	if got := collector.count("/v1/traces"); got != before {
		t.Fatalf("disabled plugin exported %d additional traces", got-before)
	}
}

func TestTelemetryPluginConfigFailuresMarkFailed(t *testing.T) {
	cases := []struct {
		name    string
		config  telemetry.Config
		lookup  func(string) (string, bool)
		wantErr bool
	}{
		{name: "otlp without endpoint", config: telemetry.Config{Adapter: "otlp"}, wantErr: true},
		{name: "plaintext header", config: telemetry.Config{Adapter: "otlp", Endpoint: "https://collector.example", Headers: map[string]string{"Authorization": "Bearer secret"}}, wantErr: true},
		{name: "missing secret", config: telemetry.Config{Adapter: "otlp", Endpoint: "https://collector.example", Headers: map[string]string{"Authorization": "secret:OTLP"}}, wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			harness := newTelemetryHarness(t, telemetryFixture{config: test.config, lookup: test.lookup})
			_, err := harness.manager.Enable(context.Background(), TelemetryPluginID, AuditContext{TenantID: &harness.tenant, Actor: "test"})
			if test.wantErr && err == nil {
				t.Fatal("enable succeeded with an invalid configuration")
			}
			if harness.hub.ExporterCount() != 0 {
				t.Fatalf("failed plugin registered %d exporters", harness.hub.ExporterCount())
			}
			if status := harness.hub.Status(); status.State != telemetry.PluginFailed {
				t.Fatalf("state = %s (%s)", status.State, status.Reason)
			}
			// The failure is a plugin diagnostic, not a harness failure: a normal
			// span still runs.
			if err := harness.hub.StartSpan(telemetry.SpanOptions{Name: "harness-still-works"}, func(telemetry.Span) error { return nil }); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTelemetryPluginInactiveAdapter(t *testing.T) {
	harness := newTelemetryHarness(t, telemetryFixture{config: telemetry.Config{Adapter: "none"}})
	if _, err := harness.manager.Enable(context.Background(), TelemetryPluginID, AuditContext{TenantID: &harness.tenant, Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	status := harness.hub.Status()
	if status.State != telemetry.PluginInactive || !status.Enabled {
		t.Fatalf("status = %#v", status)
	}
	if harness.hub.ExporterCount() != 0 || len(status.Signals) != 0 {
		t.Fatalf("inactive plugin has an exporter/signals: %#v", status)
	}
	if _, err := harness.manager.Disable(context.Background(), TelemetryPluginID, AuditContext{TenantID: &harness.tenant, Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if status := harness.hub.Status(); status.State != telemetry.PluginDisabled || status.Enabled {
		t.Fatalf("disabled inactive plugin status = %#v", status)
	}
}

func TestTelemetryPluginTenantMismatchFailsClosed(t *testing.T) {
	hub := telemetry.NewHub(telemetry.HubOptions{TenantID: "tenant-a", Logger: testLogger()})
	plugin := NewTelemetryPlugin(TelemetryPluginOptions{
		TenantID: "tenant-b",
		Config:   telemetry.Config{Adapter: "otlp", Endpoint: "https://collector.example"},
		Logger:   testLogger(),
	})
	env := &PluginEnv{Telemetry: hub}
	if err := plugin.Start(context.Background(), env); err == nil {
		t.Fatal("a tenant mismatch was accepted")
	}
	if hub.ExporterCount() != 0 {
		t.Fatalf("exporter registered across tenants: %d", hub.ExporterCount())
	}
	if status := hub.Status(); status.State != telemetry.PluginFailed {
		t.Fatalf("state = %s", status.State)
	}
}

func TestTelemetryPluginBundledCannotBeUninstalled(t *testing.T) {
	harness := newTelemetryHarness(t, telemetryFixture{config: telemetry.Config{Adapter: "none"}})
	if _, err := harness.manager.Uninstall(context.Background(), TelemetryPluginID, AuditContext{TenantID: &harness.tenant, Actor: "test"}); err == nil {
		t.Fatal("a bundled plugin was uninstalled")
	} else if !strings.Contains(err.Error(), "bundled") {
		t.Fatalf("uninstall error = %v", err)
	}
	// Disable is the supported path and leaves zero residue.
	if _, err := harness.manager.Enable(context.Background(), TelemetryPluginID, AuditContext{TenantID: &harness.tenant, Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.manager.Disable(context.Background(), TelemetryPluginID, AuditContext{TenantID: &harness.tenant, Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if harness.hub.ExporterCount() != 0 {
		t.Fatalf("disable left %d exporters", harness.hub.ExporterCount())
	}
}

func TestTelemetryPluginAudit(t *testing.T) {
	collector := newTelemetryCollector(t)
	harness := newTelemetryHarness(t, telemetryFixture{config: telemetry.Config{
		Adapter:  "otlp",
		Endpoint: collector.server.URL,
		Headers:  map[string]string{"Authorization": "secret:OTLP"},
	}, lookup: func(name string) (string, bool) {
		return "super-secret-value", name == "OTLP"
	}})
	if _, err := harness.manager.Enable(context.Background(), TelemetryPluginID, AuditContext{TenantID: &harness.tenant, Actor: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.manager.Disable(context.Background(), TelemetryPluginID, AuditContext{TenantID: &harness.tenant, Actor: "alice"}); err != nil {
		t.Fatal(err)
	}
	entries, err := harness.manager.Audit(context.Background(), &harness.tenant, TelemetryPluginID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no telemetry audit rows")
	}
	actions := map[string]bool{}
	for _, entry := range entries {
		actions[entry.Action] = true
		detail := string(entry.Detail)
		for _, forbidden := range []string{"super-secret-value", "secret:OTLP"} {
			if strings.Contains(detail, forbidden) {
				t.Fatalf("audit row leaked %q: %s", forbidden, detail)
			}
		}
	}
	for _, want := range []string{store.PluginActionEnable, store.PluginActionDisable, store.PluginActionConfigure} {
		if !actions[want] {
			t.Fatalf("audit actions = %v, want %s", actions, want)
		}
	}
	var enableDetail map[string]any
	for _, entry := range entries {
		if entry.Action == store.PluginActionEnable {
			_ = json.Unmarshal(entry.Detail, &enableDetail)
		}
	}
	if enableDetail["endpointHost"] == nil || enableDetail["signals"] == nil || enableDetail["sampleRatio"] == nil {
		t.Fatalf("enable audit detail is missing telemetry facts: %#v", enableDetail)
	}
}

func TestTelemetryPluginSamplingZeroKeepsMetrics(t *testing.T) {
	collector := newTelemetryCollector(t)
	zero := 0.0
	harness := newTelemetryHarness(t, telemetryFixture{config: telemetry.Config{
		Adapter:     "otlp",
		Endpoint:    collector.server.URL,
		Signals:     []string{"traces", "metrics"},
		SampleRatio: &zero,
	}})
	if _, err := harness.manager.Enable(context.Background(), TelemetryPluginID, AuditContext{TenantID: &harness.tenant, Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := harness.hub.StartSpan(telemetry.SpanOptions{Name: "keel.agent.run"}, func(telemetry.Span) error { return nil }); err != nil {
		t.Fatal(err)
	}
	harness.hub.Metrics().Counter("keel.agent.runs", nil).Add(1, telemetry.SpanAttributes{"model": "m", "provider": "p", "outcome": "ok"})
	if err := harness.hub.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if collector.count("/v1/traces") != 0 {
		t.Fatalf("sample_ratio=0 exported %d trace requests", collector.count("/v1/traces"))
	}
	if collector.count("/v1/metrics") == 0 {
		t.Fatal("sample_ratio=0 also dropped metrics")
	}
}

func TestTelemetryPluginFailureDoesNotFailRuns(t *testing.T) {
	collector := newTelemetryCollector(t)
	collector.setStatus(http.StatusInternalServerError)
	harness := newTelemetryHarness(t, telemetryFixture{config: telemetry.Config{
		Adapter:  "otlp",
		Endpoint: collector.server.URL,
	}})
	if _, err := harness.manager.Enable(context.Background(), TelemetryPluginID, AuditContext{TenantID: &harness.tenant, Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	result := 0
	err := harness.hub.StartSpan(telemetry.SpanOptions{Name: "keel.agent.run"}, func(span telemetry.Span) error {
		result = 42
		return span.StartSpan(telemetry.SpanOptions{Name: "keel.agent.tool"}, func(telemetry.Span) error { return nil })
	})
	if err != nil || result != 42 {
		t.Fatalf("run = %d, %v; exporter failure leaked into the run", result, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = harness.hub.Flush(ctx)
	// The flush deadline can win the race against the worker recording its drop;
	// wait for the bounded counter to settle instead of reading once.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && harness.hub.Status().Dropped == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	status := harness.hub.Status()
	if status.Dropped == 0 {
		t.Fatalf("collector failures were not counted: %#v", status)
	}
	if status.State != telemetry.PluginDegraded {
		t.Fatalf("state = %s, want degraded (%#v)", status.State, status)
	}
}

func TestTelemetryPluginStartStopCyclesReleaseResources(t *testing.T) {
	collector := newTelemetryCollector(t)
	harness := newTelemetryHarness(t, telemetryFixture{config: telemetry.Config{
		Adapter:  "otlp",
		Endpoint: collector.server.URL,
	}})
	baseline := runtimeGoroutines()
	for round := 0; round < 3; round++ {
		if _, err := harness.manager.Enable(context.Background(), TelemetryPluginID, AuditContext{TenantID: &harness.tenant, Actor: "test"}); err != nil {
			t.Fatalf("round %d enable: %v", round, err)
		}
		if harness.hub.ExporterCount() != 1 {
			t.Fatalf("round %d exporters = %d", round, harness.hub.ExporterCount())
		}
		if _, err := harness.manager.Disable(context.Background(), TelemetryPluginID, AuditContext{TenantID: &harness.tenant, Actor: "test"}); err != nil {
			t.Fatalf("round %d disable: %v", round, err)
		}
		if harness.hub.ExporterCount() != 0 {
			t.Fatalf("round %d left %d exporters", round, harness.hub.ExporterCount())
		}
	}
	// The exporter worker is the only goroutine a start creates; after three
	// disable cycles the count must be back at (or very near) the baseline.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && runtimeGoroutines() > baseline+2 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtimeGoroutines(); got > baseline+2 {
		t.Fatalf("goroutines = %d, baseline = %d", got, baseline)
	}
	before := collector.count("/v1/traces")
	_ = harness.hub.StartSpan(telemetry.SpanOptions{Name: "after-cycles"}, func(telemetry.Span) error { return nil })
	_ = harness.hub.Flush(context.Background())
	if got := collector.count("/v1/traces"); got != before {
		t.Fatalf("a stopped exporter still wrote: %d", got-before)
	}
}

func TestTelemetryPluginEmptyTenantFailsClosed(t *testing.T) {
	cases := []struct {
		name         string
		hubTenant    string
		pluginTenant string
	}{
		{"hub unbound", "", "tenant-a"},
		{"plugin unbound", "tenant-a", ""},
		{"both unbound", "", ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			hub := telemetry.NewHub(telemetry.HubOptions{TenantID: test.hubTenant, Logger: testLogger()})
			plugin := NewTelemetryPlugin(TelemetryPluginOptions{
				TenantID: test.pluginTenant,
				Config:   telemetry.Config{Adapter: "otlp", Endpoint: "https://collector.example"},
				Logger:   testLogger(),
			})
			if err := plugin.Start(context.Background(), &PluginEnv{Telemetry: hub}); err == nil {
				t.Fatal("an unbound tenant was accepted")
			}
			if hub.ExporterCount() != 0 {
				t.Fatalf("exporter registered: %d", hub.ExporterCount())
			}
			if status := hub.Status(); status.State != telemetry.PluginFailed {
				t.Fatalf("state = %s", status.State)
			}
		})
	}
}

func TestTelemetryPluginDisableFlushesQueuedBatch(t *testing.T) {
	collector := newTelemetryCollector(t)
	harness := newTelemetryHarness(t, telemetryFixture{config: telemetry.Config{
		Adapter:  "otlp",
		Endpoint: collector.server.URL,
	}})
	if _, err := harness.manager.Enable(context.Background(), TelemetryPluginID, AuditContext{TenantID: &harness.tenant, Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := harness.hub.StartSpan(telemetry.SpanOptions{Name: "queued-before-disable"}, func(telemetry.Span) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// No explicit Flush: Disable's Stop must drain the queue within its bound.
	if _, err := harness.manager.Disable(context.Background(), TelemetryPluginID, AuditContext{TenantID: &harness.tenant, Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if collector.count("/v1/traces") == 0 {
		t.Fatal("disable did not flush the queued batch")
	}
	if !strings.Contains(collector.traceBodies(), "queued-before-disable") {
		t.Fatalf("flushed body = %s", collector.traceBodies())
	}
}

func TestTelemetryPluginSendsResolvedHeader(t *testing.T) {
	collector := newTelemetryCollector(t)
	harness := newTelemetryHarness(t, telemetryFixture{
		config: telemetry.Config{
			Adapter:  "otlp",
			Endpoint: collector.server.URL,
			Headers:  map[string]string{"Authorization": "secret:OTLP"},
		},
		lookup: func(name string) (string, bool) { return "Bearer plugin-token", name == "OTLP" },
	})
	if _, err := harness.manager.Enable(context.Background(), TelemetryPluginID, AuditContext{TenantID: &harness.tenant, Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := harness.hub.StartSpan(telemetry.SpanOptions{Name: "header-run"}, func(telemetry.Span) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := harness.hub.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := collector.lastHeaders().Get("Authorization"); got != "Bearer plugin-token" {
		t.Fatalf("collector Authorization = %q", got)
	}
}

func TestTelemetryStopContextCapsLongerCallerDeadline(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx, stop := telemetryStopContext(parent, 50*time.Millisecond)
	defer stop()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("stop context has no deadline")
	}
	if remaining := time.Until(deadline); remaining > 100*time.Millisecond {
		t.Fatalf("stop deadline = %s, want <= 50ms", remaining)
	}
	short, cancelShort := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelShort()
	ctxShort, stopShort := telemetryStopContext(short, 5*time.Second)
	defer stopShort()
	if remaining := time.Until(mustDeadline(t, ctxShort)); remaining > 100*time.Millisecond {
		t.Fatalf("short parent deadline = %s, want the caller's bound", remaining)
	}
}

func mustDeadline(t *testing.T, ctx context.Context) time.Time {
	t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("context has no deadline")
	}
	return deadline
}

func TestTelemetryPluginTenantIsolation(t *testing.T) {
	collectorA := newTelemetryCollector(t)
	collectorB := newTelemetryCollector(t)
	harnessA := newTelemetryHarness(t, telemetryFixture{config: telemetry.Config{Adapter: "otlp", Endpoint: collectorA.server.URL}})
	harnessB := newTelemetryHarness(t, telemetryFixture{config: telemetry.Config{Adapter: "otlp", Endpoint: collectorB.server.URL}})
	for _, harness := range []*telemetryHarness{harnessA, harnessB} {
		if _, err := harness.manager.Enable(context.Background(), TelemetryPluginID, AuditContext{TenantID: &harness.tenant, Actor: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := harnessA.hub.StartSpan(telemetry.SpanOptions{Name: "a-run", Attributes: telemetry.SpanAttributes{"tenant": harnessA.tenant.String()}}, func(telemetry.Span) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := harnessB.hub.StartSpan(telemetry.SpanOptions{Name: "b-run", Attributes: telemetry.SpanAttributes{"tenant": harnessB.tenant.String()}}, func(telemetry.Span) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := harnessA.hub.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := harnessB.hub.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	bodyA := collectorA.traceBodies()
	bodyB := collectorB.traceBodies()
	if !strings.Contains(bodyA, harnessA.tenant.String()) || strings.Contains(bodyA, harnessB.tenant.String()) {
		t.Fatalf("tenant A collector = %s", bodyA)
	}
	if !strings.Contains(bodyB, harnessB.tenant.String()) || strings.Contains(bodyB, harnessA.tenant.String()) {
		t.Fatalf("tenant B collector = %s", bodyB)
	}
	// Disabling B must not affect A.
	if _, err := harnessB.manager.Disable(context.Background(), TelemetryPluginID, AuditContext{TenantID: &harnessB.tenant, Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if harnessA.hub.ExporterCount() != 1 {
		t.Fatal("tenant B disable affected tenant A")
	}
	if err := harnessA.hub.StartSpan(telemetry.SpanOptions{Name: "a-after-b-disable"}, func(telemetry.Span) error { return nil }); err != nil {
		t.Fatal(err)
	}
	_ = harnessA.hub.Flush(context.Background())
	if collectorA.count("/v1/traces") < 2 {
		t.Fatal("tenant A stopped exporting after tenant B was disabled")
	}
}

func runtimeGoroutines() int { return runtime.NumGoroutine() }

type logCaptureExporter struct {
	mu   sync.Mutex
	logs []telemetry.LogRecord
}

func (e *logCaptureExporter) Name() string { return "log-capture" }
func (e *logCaptureExporter) Signals() []telemetry.Signal {
	return []telemetry.Signal{telemetry.SignalLogs}
}
func (e *logCaptureExporter) ExportSpan(telemetry.StoredSpan)            {}
func (e *logCaptureExporter) ExportMetric(telemetry.RecordedMeasurement) {}
func (e *logCaptureExporter) ExportLog(record telemetry.LogRecord) {
	e.mu.Lock()
	e.logs = append(e.logs, record)
	e.mu.Unlock()
}
func (e *logCaptureExporter) Status() telemetry.ExporterStatus {
	return telemetry.ExporterStatus{Name: e.Name(), State: "active", Signals: e.Signals()}
}
func (e *logCaptureExporter) Flush(context.Context) error { return nil }
func (e *logCaptureExporter) Close(context.Context) error { return nil }

func TestTurnTelemetryEmitsBoundedLog(t *testing.T) {
	hub := telemetry.NewHub(telemetry.HubOptions{TenantID: "tenant-a", Logger: testLogger()})
	exporter := &logCaptureExporter{}
	if err := hub.RegisterExporter(exporter); err != nil {
		t.Fatal(err)
	}
	telemetryContext := &TurnTelemetry{Base: hub}
	err := telemetryContext.StartSpan(telemetry.SpanOptions{
		Name: RunSpanName,
		Attributes: telemetry.SpanAttributes{
			"tenant": "tenant-a", "session": "session-a", "model": "m", "provider": "p",
		},
	}, func(telemetry.Span) error {
		return context.DeadlineExceeded
	})
	if err == nil {
		t.Fatal("callback error was swallowed")
	}
	exporter.mu.Lock()
	logs := append([]telemetry.LogRecord(nil), exporter.logs...)
	exporter.mu.Unlock()
	if len(logs) != 1 {
		t.Fatalf("logs = %#v", logs)
	}
	if logs[0].Event != "turn.finished" || logs[0].Level != telemetry.LogLevelError {
		t.Fatalf("log = %#v", logs[0])
	}
	if logs[0].Attributes["status"] != "error" || logs[0].Attributes["tenant"] != "tenant-a" {
		t.Fatalf("log attributes = %#v", logs[0].Attributes)
	}
	for _, key := range []string{"message", "prompt", "error"} {
		if _, exists := logs[0].Attributes[key]; exists {
			t.Fatalf("log carried forbidden key %q: %#v", key, logs[0].Attributes)
		}
	}
}
