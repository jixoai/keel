package telemetry

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// captureExporter is the in-memory test sink. It records every signal it is
// offered; the tests assert hub fan-out, sampling and redaction through it.
type captureExporter struct {
	name    string
	signals []Signal

	mu      sync.Mutex
	spans   []StoredSpan
	metrics []RecordedMeasurement
	logs    []LogRecord
	flushes int
	closes  int
	panicOn bool
}

func (e *captureExporter) Name() string {
	if e.name == "" {
		return "capture"
	}
	return e.name
}

func (e *captureExporter) Signals() []Signal { return e.signals }

func (e *captureExporter) ExportSpan(span StoredSpan) {
	if e.panicOn {
		panic("exporter failure must be contained")
	}
	e.mu.Lock()
	e.spans = append(e.spans, span)
	e.mu.Unlock()
}

func (e *captureExporter) ExportMetric(measurement RecordedMeasurement) {
	e.mu.Lock()
	e.metrics = append(e.metrics, measurement)
	e.mu.Unlock()
}

func (e *captureExporter) ExportLog(record LogRecord) {
	e.mu.Lock()
	e.logs = append(e.logs, record)
	e.mu.Unlock()
}

func (e *captureExporter) Status() ExporterStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	return ExporterStatus{Name: e.Name(), State: "active", Signals: e.signals, Exported: uint64(len(e.spans) + len(e.metrics) + len(e.logs))}
}

func (e *captureExporter) Flush(context.Context) error {
	e.mu.Lock()
	e.flushes++
	e.mu.Unlock()
	return nil
}

func (e *captureExporter) Close(context.Context) error {
	e.mu.Lock()
	e.closes++
	e.mu.Unlock()
	return nil
}

func (e *captureExporter) snapshot() ([]StoredSpan, []RecordedMeasurement, []LogRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]StoredSpan(nil), e.spans...), append([]RecordedMeasurement(nil), e.metrics...), append([]LogRecord(nil), e.logs...)
}

func allSignals() []Signal { return Signals() }

func TestHubFansOutSpansMetricsAndLogs(t *testing.T) {
	hub := NewHub(HubOptions{Base: NewMemory(), TenantID: "tenant-a"})
	exporter := &captureExporter{signals: allSignals()}
	if err := hub.RegisterExporter(exporter); err != nil {
		t.Fatal(err)
	}

	rootAttrs := SpanAttributes{
		"tenant": "tenant-a", "session": "session-a", "model": "m", "provider": "p",
		"prompt": "do not export me", "message": "nor me", "tool.args": `{"secret":"x"}`,
	}
	var childID string
	err := hub.StartSpan(SpanOptions{Name: "keel.agent.run", Attributes: rootAttrs}, func(span Span) error {
		span.AddEvent("message_start", SpanAttributes{"role": "assistant", "message": "secret"})
		return span.StartSpan(SpanOptions{Name: "keel.agent.turn", Attributes: SpanAttributes{"turn": "1"}}, func(child Span) error {
			child.AddEvent("tool_end", SpanAttributes{"tool": "search", "path": "/etc/passwd"})
			child.SetStatus(SpanStatus{Status: SpanStatusError, Error: &SpanError{Name: "error", Message: "secret prompt"}})
			childID = "used"
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if childID == "" {
		t.Fatal("child callback did not run")
	}
	hub.Metrics().Counter("keel.agent.runs", nil).Add(1, SpanAttributes{"model": "m", "provider": "p", "outcome": "ok", "tenant": "tenant-a"})
	hub.Metrics().Histogram("keel.agent.run.duration", nil).Record(12, SpanAttributes{"model": "m"}, nil)
	hub.Log(LogRecord{Level: LogLevelError, Event: "turn.finished", Attributes: SpanAttributes{
		"tenant": "tenant-a", "session": "session-a", "status": "error", "prompt": "secret",
	}})

	spans, metrics, logs := exporter.snapshot()
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want 2: %#v", len(spans), spans)
	}
	root := capturedSpan(t, spans, "keel.agent.run")
	child := capturedSpan(t, spans, "keel.agent.turn")
	if root.ParentID != "" {
		t.Fatalf("root = %#v", root)
	}
	if child.ParentID != root.ID {
		t.Fatalf("child parent = %q, root id = %q", child.ParentID, root.ID)
	}
	if _, exists := root.Attributes["prompt"]; exists {
		t.Fatal("prompt attribute reached the exporter")
	}
	if root.Attributes["tenant"] != "tenant-a" || root.Attributes["session"] != "session-a" {
		t.Fatalf("identity attributes = %#v", root.Attributes)
	}
	if len(root.Events) != 1 || root.Events[0].Name != "message_start" {
		t.Fatalf("root events = %#v", root.Events)
	}
	if _, exists := root.Events[0].Attributes["message"]; exists {
		t.Fatal("event message attribute reached the exporter")
	}
	if child.Status.Status != SpanStatusError || child.Status.Error != nil {
		t.Fatalf("child status = %#v; raw error text must not travel", child.Status)
	}
	if len(metrics) != 2 {
		t.Fatalf("metrics = %d, want 2", len(metrics))
	}
	if metrics[0].TimeMs == 0 {
		t.Fatal("metric time is zero")
	}
	if _, exists := metrics[0].Attributes["tenant"]; exists {
		t.Fatalf("metric labels carried tenant: %#v", metrics[0].Attributes)
	}
	if len(logs) != 1 || logs[0].Event != "turn.finished" || logs[0].Level != LogLevelError {
		t.Fatalf("logs = %#v", logs)
	}
	if _, exists := logs[0].Attributes["prompt"]; exists {
		t.Fatal("log prompt attribute reached the exporter")
	}
}

func TestHubLateRegistrationAndUnregister(t *testing.T) {
	hub := NewHub(HubOptions{Base: NewMemory()})
	if err := hub.StartSpan(SpanOptions{Name: "before"}, func(Span) error { return nil }); err != nil {
		t.Fatal(err)
	}
	exporter := &captureExporter{signals: allSignals()}
	if err := hub.RegisterExporter(exporter); err != nil {
		t.Fatal(err)
	}
	if err := hub.StartSpan(SpanOptions{Name: "after"}, func(Span) error { return nil }); err != nil {
		t.Fatal(err)
	}
	spans, _, _ := exporter.snapshot()
	if len(spans) != 1 || spans[0].Name != "after" {
		t.Fatalf("late registration spans = %#v", spans)
	}
	if !hub.UnregisterExporter(exporter.Name()) {
		t.Fatal("unregister reported false")
	}
	if hub.UnregisterExporter(exporter.Name()) {
		t.Fatal("second unregister reported true")
	}
	if err := hub.StartSpan(SpanOptions{Name: "disabled"}, func(Span) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if spans, _, _ := exporter.snapshot(); len(spans) != 1 {
		t.Fatalf("unregistered exporter received %d spans", len(spans))
	}
}

func TestHubContainsExporterPanic(t *testing.T) {
	hub := NewHub(HubOptions{Base: NewMemory()})
	exporter := &captureExporter{signals: allSignals(), panicOn: true}
	if err := hub.RegisterExporter(exporter); err != nil {
		t.Fatal(err)
	}
	var callbackErr error
	err := hub.StartSpan(SpanOptions{Name: "run"}, func(Span) error {
		callbackErr = nil
		return nil
	})
	if err != nil {
		t.Fatalf("span returned %v", err)
	}
	if callbackErr != nil {
		t.Fatalf("callback changed: %v", callbackErr)
	}
}

func TestHubSamplingZeroStopsTracesOnly(t *testing.T) {
	ratio := 0.0
	hub := NewHub(HubOptions{Base: NewMemory(), SampleRatio: &ratio})
	exporter := &captureExporter{signals: allSignals()}
	if err := hub.RegisterExporter(exporter); err != nil {
		t.Fatal(err)
	}
	used := false
	err := hub.StartSpan(SpanOptions{Name: "run"}, func(span Span) error {
		used = true
		span.AddEvent("event", nil)
		return span.StartSpan(SpanOptions{Name: "child"}, func(Span) error { return nil })
	})
	if err != nil {
		t.Fatal(err)
	}
	if !used {
		t.Fatal("callback did not run for an unsampled span")
	}
	hub.Metrics().Counter("keel.agent.runs", nil).Add(1, nil)
	hub.Log(LogRecord{Level: LogLevelError, Event: "turn.finished"})
	spans, metrics, logs := exporter.snapshot()
	if len(spans) != 0 {
		t.Fatalf("sample_ratio=0 exported %d spans", len(spans))
	}
	if len(metrics) != 1 || len(logs) != 1 {
		t.Fatalf("sample_ratio=0 metrics/logs = %d/%d, want 1/1", len(metrics), len(logs))
	}
}

func TestHubSamplingIsDeterministicAndInherited(t *testing.T) {
	ratio := 0.1
	hub := NewHub(HubOptions{SampleRatio: &ratio})
	second := NewHub(HubOptions{SampleRatio: &ratio})
	sampled := 0
	const traces = 2000
	for index := 0; index < traces; index++ {
		traceID := "trace-" + strconv.Itoa(index)
		first := hub.shouldSample(traceID)
		if second.shouldSample(traceID) != first {
			t.Fatalf("sampling for %q is not deterministic", traceID)
		}
		if first {
			sampled++
		}
	}
	if sampled == 0 || sampled > traces/2 {
		t.Fatalf("sampled %d/%d traces at ratio 0.1", sampled, traces)
	}

	all := 1.0
	always := NewHub(HubOptions{SampleRatio: &all})
	exporter := &captureExporter{signals: allSignals()}
	if err := always.RegisterExporter(exporter); err != nil {
		t.Fatal(err)
	}
	if err := always.StartSpan(SpanOptions{Name: "run"}, func(span Span) error {
		return span.StartSpan(SpanOptions{Name: "child"}, func(Span) error { return nil })
	}); err != nil {
		t.Fatal(err)
	}
	spans, _, _ := exporter.snapshot()
	if len(spans) != 2 {
		t.Fatalf("inherited sample exported %d spans, want 2", len(spans))
	}
	parent := capturedSpan(t, spans, "run")
	child := capturedSpan(t, spans, "child")
	if child.ParentID != parent.ID {
		t.Fatalf("child parent = %q, root = %q", child.ParentID, parent.ID)
	}
}

func TestHubManualSpan(t *testing.T) {
	hub := NewHub(HubOptions{Base: NewMemory()})
	exporter := &captureExporter{signals: allSignals()}
	if err := hub.RegisterExporter(exporter); err != nil {
		t.Fatal(err)
	}
	root, endRoot := hub.StartManualSpan(SpanOptions{Name: "keel.agent.run"})
	child, endChild := root.(ManualStarter).StartManualSpan(SpanOptions{Name: "keel.agent.turn"})
	child.AddEvent("turn_end", SpanAttributes{"status": "ok"})
	endChild()
	endChild()
	endRoot()
	endRoot()
	spans, _, _ := exporter.snapshot()
	if len(spans) != 2 {
		t.Fatalf("manual spans = %d, want 2 (end must be idempotent)", len(spans))
	}
	parent := capturedSpan(t, spans, "keel.agent.run")
	childSpan := capturedSpan(t, spans, "keel.agent.turn")
	if childSpan.ParentID != parent.ID {
		t.Fatalf("manual child parent = %q, root = %q", childSpan.ParentID, parent.ID)
	}
}

func capturedSpan(t *testing.T, spans []StoredSpan, name string) StoredSpan {
	t.Helper()
	for _, span := range spans {
		if span.Name == name {
			return span
		}
	}
	t.Fatalf("span %q not found in the exporter", name)
	return StoredSpan{}
}

func TestHubStatus(t *testing.T) {
	hub := NewHub(HubOptions{TenantID: "tenant-a"})
	hub.Configure(SummarizeConfig(Config{
		Adapter:     "otlp",
		Endpoint:    "https://collector.example:4318",
		Headers:     map[string]string{"Authorization": "secret:OTLP_TOKEN"},
		SampleRatio: floatPointer(0.2),
		Signals:     []string{"traces", "metrics"},
	}))
	hub.SetPluginState(PluginInactive, "adapter is none")
	status := hub.Status()
	if status.State != PluginInactive || !status.Enabled {
		t.Fatalf("status = %#v", status)
	}
	if status.TenantID != "tenant-a" || status.EndpointHost != "collector.example:4318" {
		t.Fatalf("status identity = %#v", status)
	}
	if status.HeaderCount != 1 || len(status.HeaderNames) != 1 {
		t.Fatalf("header projection = %#v", status)
	}
	if status.SampleRatio != 0.2 {
		t.Fatalf("sample ratio = %v", status.SampleRatio)
	}
	if strings.Contains(strings.Join(status.HeaderNames, ","), "OTLP_TOKEN") {
		t.Fatal("status leaked a secret reference")
	}
}

func TestHubTenantIsolation(t *testing.T) {
	hubA := NewHub(HubOptions{TenantID: "tenant-a"})
	hubB := NewHub(HubOptions{TenantID: "tenant-b"})
	exporterA := &captureExporter{name: "a", signals: allSignals()}
	exporterB := &captureExporter{name: "b", signals: allSignals()}
	if err := hubA.RegisterExporter(exporterA); err != nil {
		t.Fatal(err)
	}
	if err := hubB.RegisterExporter(exporterB); err != nil {
		t.Fatal(err)
	}
	if err := hubA.StartSpan(SpanOptions{Name: "a-run", Attributes: SpanAttributes{"tenant": "tenant-a"}}, func(Span) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := hubB.StartSpan(SpanOptions{Name: "b-run", Attributes: SpanAttributes{"tenant": "tenant-b"}}, func(Span) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if spans, _, _ := exporterA.snapshot(); len(spans) != 1 || spans[0].Attributes["tenant"] != "tenant-a" {
		t.Fatalf("tenant A exporter = %#v", spans)
	}
	if spans, _, _ := exporterB.snapshot(); len(spans) != 1 || spans[0].Attributes["tenant"] != "tenant-b" {
		t.Fatalf("tenant B exporter = %#v", spans)
	}
}
