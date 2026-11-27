package main

// Serve wiring coverage for the telemetry plugin (review P0): the tenant
// telemetry section lives in the stored tenant document, so Boot must load it,
// the approval question must render it, and an enable must actually reach a
// collector.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/loadout"
	"github.com/jixoai/keel/internal/servicehost"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/telemetry"
)

type fakeTenantDocuments struct {
	record store.TenantConfigRecord
	err    error
}

func (f fakeTenantDocuments) GetTenantConfig(context.Context, uuid.UUID) (store.TenantConfigRecord, error) {
	if f.err != nil {
		return store.TenantConfigRecord{}, f.err
	}
	return f.record, nil
}

func TestTenantTelemetryLoaderMergesStoredDocumentOverDefaults(t *testing.T) {
	stored := store.TenantConfigRecord{
		Revision: 7,
		Config: json.RawMessage(`{
			"telemetry": {
				"adapter": "otlp",
				"endpoint": "https://collector.example:4318",
				"headers": {"Authorization": "secret:OTLP_TOKEN"},
				"signals": ["traces", "logs"],
				"sampleRatio": 0.25,
				"logMinLevel": "warn"
			}
		}`),
	}
	loader := newTenantTelemetryLoader(fakeTenantDocuments{record: stored}, uuid.New(), host.TenantConfig{
		Telemetry: host.TelemetryConfig{Adapter: "none", ServiceName: "boot-name"},
	}, nil)
	config, err := loader.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if config.Adapter != "otlp" || config.Endpoint != "https://collector.example:4318" {
		t.Fatalf("config = %#v", config)
	}
	if len(config.Signals) != 2 || config.Signals[0] != "traces" || config.Signals[1] != "logs" {
		t.Fatalf("signals = %v", config.Signals)
	}
	if config.SampleRatio == nil || *config.SampleRatio != 0.25 || config.LogMinLevel != "warn" {
		t.Fatalf("sampling = %#v", config)
	}
	if config.Headers["Authorization"] != "secret:OTLP_TOKEN" {
		t.Fatalf("headers = %#v", config.Headers)
	}
	if config.TenantID == "" {
		t.Fatal("tenant id is missing")
	}
}

func TestTenantTelemetryLoaderSurfacesInvalidStoredDocument(t *testing.T) {
	loader := newTenantTelemetryLoader(fakeTenantDocuments{record: store.TenantConfigRecord{
		Config: json.RawMessage(`{"telemetry":{"adapter":"otlp","endpoint":"http://public.example:4318"}}`),
	}}, uuid.New(), host.TenantConfig{}, nil)
	config, err := loader.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Validation belongs to the plugin Start: the loader only resolves the
	// document; an invalid endpoint leaves the plugin FAILED before any request.
	if _, err := telemetry.ParseConfig(config); err == nil {
		t.Fatal("a plaintext public endpoint in the stored document was accepted")
	}
}

func TestTelemetryApprovalShowsUnavailableOnLoaderError(t *testing.T) {
	lines := telemetryApprovalLinesWithLoader(telemetry.Config{
		Adapter:  "otlp",
		Endpoint: "https://boot.example:4318",
	}, func(context.Context) (telemetry.Config, error) {
		return telemetry.Config{}, errors.New("store unavailable")
	})
	joined := strings.Join(lines, ";")
	if !strings.Contains(joined, "configUnavailable") {
		t.Fatalf("approval lines = %s", joined)
	}
	if strings.Contains(joined, "boot.example") {
		t.Fatalf("approval rendered a stale boot snapshot: %s", joined)
	}
}

type wiringCollector struct {
	server  *httptest.Server
	mu      sync.Mutex
	paths   []string
	headers []http.Header
	bodies  []string
}

func newWiringCollector(t *testing.T) *wiringCollector {
	t.Helper()
	collector := &wiringCollector{}
	collector.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		collector.mu.Lock()
		collector.paths = append(collector.paths, r.URL.Path)
		collector.headers = append(collector.headers, r.Header.Clone())
		collector.bodies = append(collector.bodies, string(body))
		collector.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.server.Close)
	return collector
}

func (c *wiringCollector) count(path string) int {
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

func (c *wiringCollector) lastHeader(name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.headers) == 0 {
		return ""
	}
	return c.headers[len(c.headers)-1].Get(name)
}

func (c *wiringCollector) allBodies() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.bodies, "\n")
}

func telemetryServeRuntime(t *testing.T, tenant uuid.UUID, hub *telemetry.Hub) *servicehost.Runtime {
	t.Helper()
	mem := kernel.NewMemStore()
	k := kernel.New(mem, kernel.Options{})
	rt, err := servicehost.Boot(context.Background(), servicehost.Config{
		Seams: servicehost.Seams{
			Committer: k, Reader: k, Leases: k, External: k,
			Runs: promptMirrorForTelemetry{}, Logger: discardLogger(),
		},
		Tenant:    host.TenantConfig{Agent: host.AgentConfig{SystemPrompt: "telemetry serve wiring test"}},
		TenantID:  tenant,
		Telemetry: servicehost.TelemetryOptions{Hub: hub},
		Options: loadout.Options{
			Revision: 1,
			Worker:   true,
			// keel serve sets this whenever it has a process hub; the tenant
			// document here deliberately has no telemetry section.
			Telemetry: true,
		},
		Product: servicehost.ProductOptions{
			WrapTelemetry: func(base telemetry.Context) telemetry.Context {
				return &harnessext.TurnTelemetry{Base: base}
			},
		},
	})
	if err != nil {
		t.Fatalf("boot runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	return rt
}

// promptMirrorForTelemetry satisfies agent.RunMirror for the boot-only runtime.
type promptMirrorForTelemetry struct{}

func (promptMirrorForTelemetry) GetRun(context.Context, uuid.UUID, uuid.UUID) (store.Run, error) {
	return store.Run{}, store.ErrNoRun
}

func TestServeTelemetryConfigReachesApprovalAndCollector(t *testing.T) {
	collector := newWiringCollector(t)
	tenant := uuid.New()
	stored := store.TenantConfigRecord{
		Revision: 3,
		Config: json.RawMessage(`{
			"telemetry": {
				"adapter": "otlp",
				"endpoint": "` + collector.server.URL + `",
				"headers": {"Authorization": "secret:OTLP_TOKEN"},
				"signals": ["traces", "metrics"]
			}
		}`),
	}
	loader := newTenantTelemetryLoader(fakeTenantDocuments{record: stored}, tenant, host.TenantConfig{}, nil)
	config, err := loader.LoadBounded(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	hub := telemetry.NewHub(telemetry.HubOptions{TenantID: tenant.String(), Base: telemetry.NoopContext, Logger: discardLogger()})
	hub.Configure(telemetry.SummarizeConfig(config))
	rt := telemetryServeRuntime(t, tenant, hub)
	hooks := harnessext.NewHookBus(harnessext.HookBusOptions{Logger: discardLogger()})
	decisions := harnessext.NewDecisionBroker(harnessext.DecisionBrokerOptions{Logger: discardLogger()})
	triggers := harnessext.NewTriggerEngine(harnessext.TriggerEngineOptions{Hooks: hooks, Logger: discardLogger()})
	partials := harnessext.NewPartialHub()
	subagents := harnessext.NewSubagentRunner(harnessext.SubagentOptions{Hooks: hooks, Logger: discardLogger()})
	t.Cleanup(func() {
		triggers.Close()
		decisions.Close()
		hooks.Close()
	})
	surface, err := newPluginSurface(context.Background(), rt, nil, serveOptions{
		TenantID:              tenant,
		PluginsRoot:           t.TempDir(),
		TelemetryConfig:       config,
		TelemetryConfigLoader: loader.LoadBounded,
	}, hooks, triggers, decisions, nil, nil, subagents, partials, nil,
		func(name string) (string, bool) {
			if name == "OTLP_TOKEN" {
				return "Bearer wiring-token", true
			}
			return "", false
		}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { surface.runtime.Close(context.Background()) })

	// The run path, not a hand-made hub span: the telemetry service must be
	// active even though the tenant document has no telemetry section, and the
	// worker's runner must carry a non-nil telemetry context.
	if !rt.Snapshot().Active(loadout.ServiceTelemetry) {
		t.Fatalf("telemetry service state = %s", rt.Snapshot().State(loadout.ServiceTelemetry))
	}
	bindCtx, bindCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer bindCancel()
	bound, err := rt.Binding().Acquire(bindCtx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bound.Release(context.Background()) })
	runner := bound.Runner()
	if runner.Telemetry == nil || runner.Telemetry.Context == nil || runner.Telemetry.Metrics == nil {
		t.Fatalf("runner telemetry is not bound: %#v", runner.Telemetry)
	}

	// The approval question is the stored endpoint, never its secret name/value.
	lines := strings.Join(telemetryApprovalLines(config), ";")
	if !strings.Contains(lines, "endpointHost=") || !strings.Contains(lines, "headers=Authorization") {
		t.Fatalf("approval lines = %s", lines)
	}
	for _, forbidden := range []string{"OTLP_TOKEN", "wiring-token"} {
		if strings.Contains(lines, forbidden) {
			t.Fatalf("approval line leaked %q: %s", forbidden, lines)
		}
	}

	// Default disabled: no request.
	if got := surface.manager.Snapshot().Plugins; len(got) == 0 {
		t.Fatal("no plugin inventory")
	}
	if _, err := surface.manager.Enable(context.Background(), harnessext.TelemetryPluginID, harnessext.AuditContext{TenantID: &tenant, Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if status := hub.Status(); status.State != telemetry.PluginActive {
		t.Fatalf("hub state = %s", status.State)
	}
	if err := runner.Telemetry.Context.StartSpan(telemetry.SpanOptions{Name: "keel.agent.run", Attributes: telemetry.SpanAttributes{
		"tenant": tenant.String(), "session": "session-a",
	}}, func(telemetry.Span) error { return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := hub.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if collector.count("/v1/traces") == 0 {
		t.Fatal("the collector received no trace after enable")
	}
	if !strings.Contains(collector.allBodies(), "keel.agent.run") {
		t.Fatalf("body = %s", collector.allBodies())
	}
	if got := collector.lastHeader("Authorization"); got != "Bearer wiring-token" {
		t.Fatalf("collector Authorization = %q", got)
	}

	// Disable leaves zero residue.
	if _, err := surface.manager.Disable(context.Background(), harnessext.TelemetryPluginID, harnessext.AuditContext{TenantID: &tenant, Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	if hub.ExporterCount() != 0 || hub.Status().State != telemetry.PluginDisabled {
		t.Fatalf("disable residue: %#v", hub.Status())
	}
}
