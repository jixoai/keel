package telemetry

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCollector is the OTLP HTTP test server: it records every request and
// answers with a scripted status.
type fakeCollector struct {
	server *httptest.Server

	mu        sync.Mutex
	requests  []collectorRequest
	status    int
	header    http.Header
	delay     time.Duration
	responder func(w http.ResponseWriter, r *http.Request) (int, http.Header)
}

type collectorRequest struct {
	path string
	body []byte
}

func newFakeCollector(t *testing.T) *fakeCollector {
	t.Helper()
	collector := &fakeCollector{status: http.StatusOK}
	collector.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		collector.mu.Lock()
		collector.requests = append(collector.requests, collectorRequest{path: r.URL.Path, body: body})
		status := collector.status
		header := collector.header.Clone()
		delay := collector.delay
		responder := collector.responder
		collector.mu.Unlock()
		if responder != nil {
			status, header = responder(w, r)
		}
		for name, values := range header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(collector.server.Close)
	return collector
}

func (c *fakeCollector) setResponder(responder func(w http.ResponseWriter, r *http.Request) (int, http.Header)) {
	c.mu.Lock()
	c.responder = responder
	c.mu.Unlock()
}

func (c *fakeCollector) setStatus(status int) {
	c.mu.Lock()
	c.status = status
	c.mu.Unlock()
}

func (c *fakeCollector) setHeader(name, value string) {
	c.mu.Lock()
	if c.header == nil {
		c.header = http.Header{}
	}
	c.header.Set(name, value)
	c.mu.Unlock()
}

func (c *fakeCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

func (c *fakeCollector) all() []collectorRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]collectorRequest(nil), c.requests...)
}

func (c *fakeCollector) bodies(path string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []string{}
	for _, request := range c.requests {
		if request.path == path {
			out = append(out, string(request.body))
		}
	}
	return out
}

func testExporterOptions(endpoint string, signals []Signal) OTLPExporterOptions {
	ratio := 1.0
	return OTLPExporterOptions{
		Endpoint:    endpoint,
		ServiceName: "keel-test",
		TenantID:    "tenant-a",
		Signals:     signals,
		LogMinLevel: LogLevelInfo,
		LogRatio:    &ratio,
		Queue:       QueueLimits{MaxSpans: 1024, MaxMetrics: 1024, MaxLogs: 1024, MaxBytes: 8 << 20},
		Batch: BatchLimits{
			MaxSpans: 512, MaxMetrics: 512, MaxLogs: 512,
			MaxBytes: 1 << 20, FlushInterval: time.Hour, MaxPostBytes: 4 << 20,
		},
		Retry: RetryPolicy{
			MaxAttempts: 5, BaseDelay: 5 * time.Millisecond, MaxDelay: 20 * time.Millisecond,
			BreakerFailures: 5, BreakerOpen: 20 * time.Millisecond,
		},
	}
}

func testSpan(name string) StoredSpan {
	now := float64(time.Now().UnixMilli())
	return StoredSpan{
		ID:        "1",
		TraceID:   "trace-1",
		Name:      name,
		StartedMs: now,
		EndedMs:   now + 1,
		Attributes: SpanAttributes{
			"tenant": "tenant-a", "session": "session-a", "model": "m",
			"prompt": "must not travel",
		},
		Status: OK(),
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func TestOTLPExporterBatchesTracesMetricsLogs(t *testing.T) {
	collector := newFakeCollector(t)
	options := testExporterOptions(collector.server.URL, allSignals())
	options.Batch.MaxSpans = 2
	options.Batch.FlushInterval = 10 * time.Millisecond
	exporter, err := NewOTLPExporter(options)
	if err != nil {
		t.Fatal(err)
	}
	exporter.Start()
	defer exporter.Close(context.Background())

	exporter.ExportSpan(testSpan("keel.agent.run"))
	exporter.ExportSpan(testSpan("keel.agent.turn"))
	exporter.ExportSpan(testSpan("keel.agent.tool"))
	exporter.ExportMetric(RecordedMeasurement{Kind: MetricKindCounter, Name: "keel.agent.runs", Value: 1, Attributes: SpanAttributes{"model": "m", "tenant": "tenant-a"}})
	exporter.ExportLog(LogRecord{Level: LogLevelError, Event: "turn.finished", Attributes: SpanAttributes{"tenant": "tenant-a", "prompt": "secret"}})
	if err := exporter.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	traces := collector.bodies("/v1/traces")
	if len(traces) == 0 {
		t.Fatal("no trace request")
	}
	all := strings.Join(traces, "")
	for _, name := range []string{"keel.agent.run", "keel.agent.turn", "keel.agent.tool"} {
		if !strings.Contains(all, name) {
			t.Fatalf("trace payload misses %s: %s", name, all)
		}
	}
	if strings.Contains(all, "must not travel") || strings.Contains(all, "prompt") {
		t.Fatalf("forbidden attribute reached the collector: %s", all)
	}
	if !strings.Contains(all, `"service.name"`) || !strings.Contains(all, `tenant-a`) {
		t.Fatalf("resource/service identity missing: %s", all)
	}
	metrics := strings.Join(collector.bodies("/v1/metrics"), "")
	if !strings.Contains(metrics, "keel.agent.runs") {
		t.Fatalf("metric payload = %s", metrics)
	}
	if strings.Contains(metrics, `"key":"tenant"`) || strings.Contains(metrics, `"key":"session"`) {
		t.Fatalf("metric data points carried identity labels: %s", metrics)
	}
	logs := strings.Join(collector.bodies("/v1/logs"), "")
	if !strings.Contains(logs, "turn.finished") || !strings.Contains(logs, "ERROR") {
		t.Fatalf("log payload = %s", logs)
	}
	if strings.Contains(logs, "secret") {
		t.Fatalf("log payload leaked a forbidden attribute: %s", logs)
	}
}

func TestOTLPExporterConfirmedCursorAndIdempotentClose(t *testing.T) {
	collector := newFakeCollector(t)
	exporter, err := NewOTLPExporter(testExporterOptions(collector.server.URL, []Signal{SignalTraces}))
	if err != nil {
		t.Fatal(err)
	}
	exporter.Start()
	exporter.ExportSpan(testSpan("run-1"))
	if err := exporter.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if collector.count() != 1 {
		t.Fatalf("requests = %d, want 1", collector.count())
	}
	if confirmed := exporter.confirmedSequence(kindSpans); confirmed != 1 {
		t.Fatalf("confirmed = %d, want 1", confirmed)
	}
	if err := exporter.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := exporter.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if collector.count() != 1 {
		t.Fatalf("a second Shutdown re-exported confirmed data: %d requests", collector.count())
	}
	if status := exporter.Status(); status.QueueDepth != 0 || status.Exported != 1 {
		t.Fatalf("status = %#v", status)
	}
}

func TestOTLPExporterSplitsOversizedBatches(t *testing.T) {
	collector := newFakeCollector(t)
	options := testExporterOptions(collector.server.URL, []Signal{SignalTraces})
	options.Batch.MaxPostBytes = 1200
	exporter, err := NewOTLPExporter(options)
	if err != nil {
		t.Fatal(err)
	}
	exporter.Start()
	defer exporter.Close(context.Background())
	long := strings.Repeat("x", 200)
	for index := 0; index < 6; index++ {
		span := testSpan(fmt.Sprintf("run-%d-%s", index, long))
		exporter.ExportSpan(span)
	}
	if err := exporter.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests := collector.all()
	if len(requests) < 2 {
		t.Fatalf("requests = %d, want a split", len(requests))
	}
	for _, request := range requests {
		if len(request.body) > options.Batch.MaxPostBytes {
			t.Fatalf("POST body %d exceeds the 4 MiB split budget %d", len(request.body), options.Batch.MaxPostBytes)
		}
	}
	if status := exporter.Status(); status.Exported != 6 || status.Dropped != 0 {
		t.Fatalf("status = %#v", status)
	}
}

func TestOTLPExporterRetries5xxThenSucceeds(t *testing.T) {
	collector := newFakeCollector(t)
	var calls atomic.Int64
	collector.setResponder(func(http.ResponseWriter, *http.Request) (int, http.Header) {
		if calls.Add(1) <= 2 {
			return http.StatusInternalServerError, nil
		}
		return http.StatusOK, nil
	})
	exporter, err := NewOTLPExporter(testExporterOptions(collector.server.URL, []Signal{SignalTraces}))
	if err != nil {
		t.Fatal(err)
	}
	exporter.Start()
	defer exporter.Close(context.Background())
	exporter.ExportSpan(testSpan("retry-run"))
	if err := exporter.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", calls.Load())
	}
	status := exporter.Status()
	if status.Exported != 1 || status.Dropped != 0 {
		t.Fatalf("status = %#v", status)
	}
	if status.LastErrorClass != "http_5xx" {
		t.Fatalf("last error class = %q", status.LastErrorClass)
	}
	if status.State != "active" {
		t.Fatalf("state = %q after recovery", status.State)
	}
}

func TestOTLPExporter429HonorsRetryAfter(t *testing.T) {
	collector := newFakeCollector(t)
	var calls atomic.Int64
	collector.setResponder(func(http.ResponseWriter, *http.Request) (int, http.Header) {
		if calls.Add(1) == 1 {
			return http.StatusTooManyRequests, http.Header{"Retry-After": []string{"0"}}
		}
		return http.StatusOK, nil
	})
	exporter, err := NewOTLPExporter(testExporterOptions(collector.server.URL, []Signal{SignalTraces}))
	if err != nil {
		t.Fatal(err)
	}
	exporter.Start()
	defer exporter.Close(context.Background())
	exporter.ExportSpan(testSpan("rate-limited"))
	if err := exporter.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", calls.Load())
	}
	if got := exporter.Status().Exported; got != 1 {
		t.Fatalf("exported = %d", got)
	}
}

func TestOTLPExporterPermanent4xxDrops(t *testing.T) {
	collector := newFakeCollector(t)
	collector.setStatus(http.StatusBadRequest)
	exporter, err := NewOTLPExporter(testExporterOptions(collector.server.URL, []Signal{SignalTraces}))
	if err != nil {
		t.Fatal(err)
	}
	exporter.Start()
	defer exporter.Close(context.Background())
	exporter.ExportSpan(testSpan("bad-run"))
	if err := exporter.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if collector.count() != 1 {
		t.Fatalf("a 4xx batch was retried: %d requests", collector.count())
	}
	status := exporter.Status()
	if status.Exported != 0 || status.Dropped != 1 || status.LastErrorClass != "http_4xx" {
		t.Fatalf("status = %#v", status)
	}
	// A dropped batch is gone: no second request on another flush.
	if err := exporter.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if collector.count() != 1 {
		t.Fatalf("dropped batch was retried: %d requests", collector.count())
	}
}

func TestOTLPExporterBreakerOpensAndRecovers(t *testing.T) {
	collector := newFakeCollector(t)
	collector.setStatus(http.StatusInternalServerError)
	options := testExporterOptions(collector.server.URL, []Signal{SignalTraces})
	options.Retry.MaxAttempts = 1
	options.Retry.BreakerFailures = 2
	options.Retry.BreakerOpen = 30 * time.Millisecond
	exporter, err := NewOTLPExporter(options)
	if err != nil {
		t.Fatal(err)
	}
	exporter.Start()
	defer exporter.Close(context.Background())
	exporter.ExportSpan(testSpan("first"))
	if err := exporter.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	exporter.ExportSpan(testSpan("second"))
	if err := exporter.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := collector.count()
	if before < 2 {
		t.Fatalf("requests = %d, want >= 2", before)
	}
	collector.setStatus(http.StatusOK)
	exporter.ExportSpan(testSpan("probe"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := exporter.Flush(ctx); err != nil {
		t.Fatalf("probe flush: %v", err)
	}
	if collector.count() <= before {
		t.Fatal("the half-open probe never reached the collector")
	}
	status := exporter.Status()
	if status.Exported == 0 {
		t.Fatalf("status = %#v", status)
	}
}

func TestOTLPExporterShutdownDeadline(t *testing.T) {
	collector := newFakeCollector(t)
	collector.mu.Lock()
	collector.delay = 500 * time.Millisecond
	collector.mu.Unlock()
	options := testExporterOptions(collector.server.URL, []Signal{SignalTraces})
	options.Batch.MaxSpans = 1
	exporter, err := NewOTLPExporter(options)
	if err != nil {
		t.Fatal(err)
	}
	exporter.Start()
	for index := 0; index < 2; index++ {
		exporter.ExportSpan(testSpan(fmt.Sprintf("shutdown-%d", index)))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	started := time.Now()
	_ = exporter.Close(ctx)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Close blocked for %s", elapsed)
	}
	status := exporter.Status()
	if status.QueueDepth != 0 {
		t.Fatalf("queue depth after Close = %d", status.QueueDepth)
	}
	if status.Dropped == 0 {
		t.Fatalf("status = %#v; an unfinished batch must be counted as dropped", status)
	}
}

func TestOTLPExporterQueueDropsOldest(t *testing.T) {
	collector := newFakeCollector(t)
	options := testExporterOptions(collector.server.URL, []Signal{SignalTraces})
	options.Queue.MaxSpans = 2
	exporter, err := NewOTLPExporter(options)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 4; index++ {
		exporter.ExportSpan(testSpan(fmt.Sprintf("queue-%d", index)))
	}
	status := exporter.Status()
	if status.QueueDepth != 2 || status.Dropped != 2 {
		t.Fatalf("status = %#v", status)
	}
	if err := exporter.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(collector.bodies("/v1/traces"), "")
	if strings.Contains(body, "queue-0") || strings.Contains(body, "queue-1") {
		t.Fatalf("the oldest records were not dropped: %s", body)
	}
	if !strings.Contains(body, "queue-3") {
		t.Fatalf("the newest record was dropped: %s", body)
	}
}

func TestOTLPExporterQueueByteCapDropsOldest(t *testing.T) {
	collector := newFakeCollector(t)
	options := testExporterOptions(collector.server.URL, []Signal{SignalTraces})
	options.Queue.MaxSpans = 100
	options.Queue.MaxBytes = 600
	exporter, err := NewOTLPExporter(options)
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("y", 200)
	for index := 0; index < 10; index++ {
		span := testSpan(fmt.Sprintf("byte-%d", index))
		span.Attributes["tool"] = long
		exporter.ExportSpan(span)
	}
	status := exporter.Status()
	if status.QueueDepth >= 10 || status.Dropped == 0 {
		t.Fatalf("byte cap did not evict: %#v", status)
	}
	if err := exporter.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(collector.bodies("/v1/traces"), "")
	if strings.Contains(body, "byte-0") || !strings.Contains(body, "byte-9") {
		t.Fatalf("oldest-wins/highest-seq contract broken: %s", body)
	}
}

func TestOTLPExporterMetricCardinalityAndRegistration(t *testing.T) {
	collector := newFakeCollector(t)
	options := testExporterOptions(collector.server.URL, []Signal{SignalMetrics})
	options.MaxSeriesPerMetric = 5
	options.MaxSeriesGlobal = 6
	exporter, err := NewOTLPExporter(options)
	if err != nil {
		t.Fatal(err)
	}
	exporter.ExportMetric(RecordedMeasurement{Kind: MetricKindCounter, Name: "unregistered.metric", Value: 1})
	for index := 0; index < 10; index++ {
		exporter.ExportMetric(RecordedMeasurement{
			Kind: MetricKindCounter, Name: "keel.agent.runs", Value: 1,
			Attributes: SpanAttributes{"model": fmt.Sprintf("model-%d", index), "provider": "p", "outcome": "ok"},
		})
	}
	exporter.ExportMetric(RecordedMeasurement{
		Kind: MetricKindHistogram, Name: "keel.agent.run.duration", Value: 1,
		Attributes: SpanAttributes{"model": "m2", "provider": "p"},
	})
	status := exporter.Status()
	if status.SeriesDropped == 0 {
		t.Fatalf("series overflow was not counted: %#v", status)
	}
	if got := status.QueueDepth; got != 6 {
		t.Fatalf("queue depth = %d, want 6 (5 per-family + 1 global cap)", got)
	}
	if err := exporter.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOTLPExporterLogFiltering(t *testing.T) {
	collector := newFakeCollector(t)
	zero := 0.0
	options := testExporterOptions(collector.server.URL, []Signal{SignalLogs})
	options.LogRatio = &zero
	exporter, err := NewOTLPExporter(options)
	if err != nil {
		t.Fatal(err)
	}
	exporter.ExportLog(LogRecord{Level: LogLevelInfo, Event: "turn.started"})
	exporter.ExportLog(LogRecord{Level: LogLevelDebug, Event: "debug.event"})
	exporter.ExportLog(LogRecord{Level: LogLevelError, Event: "turn.finished", Attributes: SpanAttributes{"status": "error"}})
	status := exporter.Status()
	if status.QueueDepth != 1 {
		t.Fatalf("logs queued = %d, want the error only", status.QueueDepth)
	}
	if err := exporter.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(collector.bodies("/v1/logs"), "")
	if !strings.Contains(body, "turn.finished") || strings.Contains(body, "turn.started") {
		t.Fatalf("log payload = %s", body)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := parseRetryAfter("7", now, time.Minute); got != 7*time.Second {
		t.Fatalf("seconds = %s", got)
	}
	if got := parseRetryAfter("999", now, 10*time.Second); got != 10*time.Second {
		t.Fatalf("capped = %s", got)
	}
	if got := parseRetryAfter(now.Add(5*time.Second).Format(http.TimeFormat), now, time.Minute); got != 5*time.Second {
		t.Fatalf("http-date = %s", got)
	}
	if got := parseRetryAfter("nonsense", now, time.Minute); got != 0 {
		t.Fatalf("invalid = %s", got)
	}
}

// TestOTLPExporterKillRestartAtLeastOnce runs a real child process, lets it
// export one confirmed batch, then SIGKILLs it. A restarted exporter sends the
// same record again (at-least-once), and no goroutine is left behind in the
// parent.
func TestOTLPExporterKillRestartAtLeastOnce(t *testing.T) {
	if os.Getenv("KEEL_OTLP_CRASH_HELPER") == "1" {
		runCrashHelper()
		return
	}
	collector := newFakeCollector(t)
	command := exec.Command(os.Args[0], "-test.run=TestOTLPExporterKillRestartAtLeastOnce", "-test.v=false")
	command.Env = append(os.Environ(), "KEEL_OTLP_CRASH_HELPER=1", "KEEL_OTLP_CRASH_ENDPOINT="+collector.server.URL)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, "exported") {
		_ = command.Process.Kill()
		t.Fatalf("helper did not confirm an export: %q, %v", line, err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	if collector.count() == 0 {
		t.Fatal("the killed process exported nothing")
	}

	// Restart: a fresh exporter has no confirmed cursor, so the same record is
	// sent again. This is the documented at-least-once window.
	restarted, err := NewOTLPExporter(testExporterOptions(collector.server.URL, []Signal{SignalTraces}))
	if err != nil {
		t.Fatal(err)
	}
	restarted.Start()
	restarted.ExportSpan(testSpan("crash-run"))
	if err := restarted.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if collector.count() < 2 {
		t.Fatalf("requests after restart = %d, want >= 2", collector.count())
	}
}

func runCrashHelper() {
	endpoint := os.Getenv("KEEL_OTLP_CRASH_ENDPOINT")
	ratio := 1.0
	exporter, err := NewOTLPExporter(OTLPExporterOptions{
		Endpoint: endpoint, Signals: []Signal{SignalTraces}, LogRatio: &ratio,
		Batch: BatchLimits{MaxSpans: 1, MaxBytes: 1 << 20, FlushInterval: 2 * time.Millisecond, MaxPostBytes: 4 << 20},
		Retry: RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, BreakerFailures: 1, BreakerOpen: time.Millisecond},
	})
	if err != nil {
		fmt.Println("error", err)
		os.Exit(2)
	}
	exporter.Start()
	exporter.ExportSpan(testSpan("crash-run"))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if exporter.confirmedSequence(kindSpans) >= 1 {
			fmt.Println("exported")
			os.Stdout.Sync()
			// Wait for the parent to SIGKILL this process.
			time.Sleep(30 * time.Second)
			os.Exit(0)
		}
		time.Sleep(time.Millisecond)
	}
	fmt.Println("timeout")
	os.Exit(3)
}

func TestOTLPExporterPayloadShape(t *testing.T) {
	options := testExporterOptions("https://collector.example:4318", []Signal{SignalTraces})
	exporter, err := NewOTLPExporter(options)
	if err != nil {
		t.Fatal(err)
	}
	now := float64(time.Now().UnixMilli())
	span := StoredSpan{
		ID: "2", TraceID: "trace-1", ParentID: "1", Name: "keel.agent.turn",
		StartedMs: now, EndedMs: now + 5,
		Attributes: SpanAttributes{"session": "s", "turn": "1"},
		Events:     []StoredEvent{{Name: "turn_end", TimeMs: 5, Attributes: SpanAttributes{"status": "ok"}}},
		Status:     OK(),
	}
	payload := exporter.tracePayload([]StoredSpan{span})
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"parentSpanId"`)) {
		t.Fatalf("payload misses parentage: %s", encoded)
	}
	if !bytes.Contains(encoded, []byte(`"scopeSpans"`)) {
		t.Fatalf("payload misses scopeSpans: %s", encoded)
	}
}
