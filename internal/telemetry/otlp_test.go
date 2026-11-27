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
	path    string
	body    []byte
	headers http.Header
}

func newFakeCollector(t *testing.T) *fakeCollector {
	t.Helper()
	collector := &fakeCollector{status: http.StatusOK}
	collector.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		collector.mu.Lock()
		collector.requests = append(collector.requests, collectorRequest{path: r.URL.Path, body: body, headers: r.Header.Clone()})
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

func (c *fakeCollector) lastHeaders() http.Header {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) == 0 {
		return nil
	}
	return c.requests[len(c.requests)-1].headers.Clone()
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

func TestDefaultMetricNamesCoverTheInstrumentVocabulary(t *testing.T) {
	names := DefaultMetricNames()
	for name, kind := range map[string]MetricKind{
		"keel.agent.runs":            MetricKindCounter,
		"keel.agent.run.duration":    MetricKindHistogram,
		"pi.ai.request.duration":     MetricKindHistogram,
		"pi.ai.first_chunk.duration": MetricKindHistogram,
		"pi.ai.tokens":               MetricKindCounter,
	} {
		if names[name] != kind {
			t.Fatalf("metric %s kind = %s, want %s", name, names[name], kind)
		}
	}
}

func TestOTLPExporterIdentityGrowthDoesNotGrowSeries(t *testing.T) {
	collector := newFakeCollector(t)
	options := testExporterOptions(collector.server.URL, []Signal{SignalMetrics})
	options.Queue.MaxMetrics = 20000
	options.Queue.MaxBytes = 64 << 20
	exporter, err := NewOTLPExporter(options)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 10000; index++ {
		exporter.ExportMetric(RecordedMeasurement{
			Kind: MetricKindCounter, Name: "keel.agent.runs", Value: 1,
			Attributes: SpanAttributes{
				"model": "m", "provider": "p", "outcome": "ok",
				"tenant": fmt.Sprintf("tenant-%d", index), "session": fmt.Sprintf("session-%d", index),
			},
		})
	}
	status := exporter.Status()
	if status.QueueDepth != 10000 || status.SeriesDropped != 0 {
		t.Fatalf("status = %#v", status)
	}
	exporter.mu.Lock()
	families := len(exporter.series["keel.agent.runs"])
	exporter.mu.Unlock()
	if families != 1 {
		t.Fatalf("10k sessions produced %d metric series; identity must not become a label", families)
	}
	_ = exporter.Close(context.Background())
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

func TestOTLPExporterMetricShapeIsDeltaWithCorrectBuckets(t *testing.T) {
	options := testExporterOptions("https://collector.example:4318", []Signal{SignalMetrics})
	exporter, err := NewOTLPExporter(options)
	if err != nil {
		t.Fatal(err)
	}
	payload := exporter.metricPayload([]RecordedMeasurement{
		{Kind: MetricKindCounter, Name: "keel.agent.runs", Value: 2, Attributes: SpanAttributes{"model": "m"}},
		{Kind: MetricKindHistogram, Name: "keel.agent.run.duration", Value: 3, Boundaries: []float64{1, 5}, Attributes: SpanAttributes{"model": "m"}},
	})
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		ResourceMetrics []struct {
			ScopeMetrics []struct {
				Metrics []struct {
					Name string `json:"name"`
					Sum  *struct {
						Temporality int `json:"aggregationTemporality"`
					} `json:"sum"`
					Histogram *struct {
						Temporality int `json:"aggregationTemporality"`
						DataPoints  []struct {
							BucketCounts   []string  `json:"bucketCounts"`
							ExplicitBounds []float64 `json:"explicitBounds"`
						} `json:"dataPoints"`
					} `json:"histogram"`
				} `json:"metrics"`
			} `json:"scopeMetrics"`
		} `json:"resourceMetrics"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	metrics := decoded.ResourceMetrics[0].ScopeMetrics[0].Metrics
	if len(metrics) != 2 {
		t.Fatalf("metrics = %d: %s", len(metrics), encoded)
	}
	for _, metric := range metrics {
		if metric.Sum != nil && metric.Sum.Temporality != 1 {
			t.Fatalf("%s sum temporality = %d, want DELTA(1)", metric.Name, metric.Sum.Temporality)
		}
		if metric.Histogram != nil {
			if metric.Histogram.Temporality != 1 {
				t.Fatalf("%s histogram temporality = %d, want DELTA(1)", metric.Name, metric.Histogram.Temporality)
			}
			point := metric.Histogram.DataPoints[0]
			if len(point.BucketCounts) != 3 || point.BucketCounts[0] != "0" || point.BucketCounts[1] != "1" || point.BucketCounts[2] != "0" {
				t.Fatalf("%s bucketCounts = %v (value 3 belongs in [1,5))", metric.Name, point.BucketCounts)
			}
			if len(point.ExplicitBounds) != 2 || point.ExplicitBounds[0] != 1 || point.ExplicitBounds[1] != 5 {
				t.Fatalf("%s explicitBounds = %v", metric.Name, point.ExplicitBounds)
			}
		}
	}
}

func TestOTLPExporterRefusesRedirects(t *testing.T) {
	collector := newFakeCollector(t)
	collector.setStatus(http.StatusFound)
	exporter, err := NewOTLPExporter(testExporterOptions(collector.server.URL, []Signal{SignalTraces}))
	if err != nil {
		t.Fatal(err)
	}
	exporter.Start()
	defer exporter.Close(context.Background())
	exporter.ExportSpan(testSpan("redirected"))
	if err := exporter.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := exporter.Status()
	if collector.count() != 1 {
		t.Fatalf("a redirect was followed: %d requests", collector.count())
	}
	if status.Dropped != 1 || status.LastErrorClass != "http_3xx" {
		t.Fatalf("status = %#v", status)
	}
}

func TestEndpointPolicyAndBounds(t *testing.T) {
	private := []string{"http://127.0.0.1:4318", "http://localhost:4318", "http://10.1.2.3:4318", "https://collector.example:4318"}
	for _, endpoint := range private {
		if _, err := ParseConfig(Config{Adapter: "otlp", Endpoint: endpoint}); err != nil {
			t.Fatalf("%s rejected: %v", endpoint, err)
		}
		if _, err := NewOTLPExporter(OTLPExporterOptions{Endpoint: endpoint}); err != nil {
			t.Fatalf("%s rejected by exporter: %v", endpoint, err)
		}
	}
	if _, err := ParseConfig(Config{Adapter: "otlp", Endpoint: "http://collector.example:4318"}); err == nil {
		t.Fatal("public plaintext endpoint was accepted")
	}
	if _, err := NewOTLPExporter(OTLPExporterOptions{Endpoint: "http://collector.example:4318"}); err == nil {
		t.Fatal("public plaintext endpoint was accepted by the exporter")
	}
	longService := strings.Repeat("s", MaxServiceNameBytes+1)
	if _, err := ParseConfig(Config{Adapter: "otlp", Endpoint: "https://c.example", ServiceName: longService}); err == nil {
		t.Fatal("oversized service name was accepted")
	}
	headers := map[string]string{}
	for index := 0; index < MaxConfigHeaders+1; index++ {
		headers[fmt.Sprintf("X-%d", index)] = "secret:S"
	}
	if _, err := ParseConfig(Config{Adapter: "otlp", Endpoint: "https://c.example", Headers: headers}); err == nil {
		t.Fatal("too many headers were accepted")
	}
	if _, err := ParseConfig(Config{Adapter: "otlp", Endpoint: "https://c.example", Headers: map[string]string{
		"Authorization": "secret:" + strings.Repeat("A", MaxHeaderReferenceBytes),
	}}); err == nil {
		t.Fatal("oversized header reference was accepted")
	}
}

func TestResolveHeadersDoesNotExposeSecretName(t *testing.T) {
	effective, err := ParseConfig(Config{
		Adapter: "otlp", Endpoint: "https://c.example",
		Headers: map[string]string{"Authorization": "secret:VERY_SECRET_NAME"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = effective.ResolveHeaders(func(string) (string, bool) { return "", false })
	if err == nil {
		t.Fatal("missing secret was accepted")
	}
	if strings.Contains(err.Error(), "VERY_SECRET_NAME") {
		t.Fatalf("error exposes the secret name: %v", err)
	}
	if !strings.Contains(err.Error(), "headers.Authorization") {
		t.Fatalf("error does not name the header: %v", err)
	}
}

func TestOTLPExporterSendsConfiguredHeaders(t *testing.T) {
	collector := newFakeCollector(t)
	options := testExporterOptions(collector.server.URL, []Signal{SignalTraces})
	options.Headers = map[string]string{"Authorization": "Bearer test-token", "X-Scope": "tenant"}
	exporter, err := NewOTLPExporter(options)
	if err != nil {
		t.Fatal(err)
	}
	exporter.Start()
	defer exporter.Close(context.Background())
	exporter.ExportSpan(testSpan("with-headers"))
	if err := exporter.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	headers := collector.lastHeaders()
	if headers.Get("Authorization") != "Bearer test-token" || headers.Get("X-Scope") != "tenant" {
		t.Fatalf("collector headers = %v", headers)
	}
}

func TestOTLPExporterPerFamilyAndGlobalSeriesCaps(t *testing.T) {
	collector := newFakeCollector(t)
	perFamily, err := NewOTLPExporter(OTLPExporterOptions{
		Endpoint: collector.server.URL, Signals: []Signal{SignalMetrics},
		MaxSeriesPerMetric: 2, MaxSeriesGlobal: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 5; index++ {
		perFamily.ExportMetric(RecordedMeasurement{Kind: MetricKindCounter, Name: "keel.agent.runs", Value: 1,
			Attributes: SpanAttributes{"model": fmt.Sprintf("m-%d", index)}})
	}
	status := perFamily.Status()
	if status.QueueDepth != 2 || status.SeriesDropped != 3 {
		t.Fatalf("per-family status = %#v", status)
	}

	global, err := NewOTLPExporter(OTLPExporterOptions{
		Endpoint: collector.server.URL, Signals: []Signal{SignalMetrics},
		MaxSeriesPerMetric: 100, MaxSeriesGlobal: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	global.ExportMetric(RecordedMeasurement{Kind: MetricKindCounter, Name: "keel.agent.runs", Value: 1, Attributes: SpanAttributes{"model": "a"}})
	global.ExportMetric(RecordedMeasurement{Kind: MetricKindCounter, Name: "keel.agent.runs", Value: 1, Attributes: SpanAttributes{"model": "b"}})
	global.ExportMetric(RecordedMeasurement{Kind: MetricKindHistogram, Name: "keel.agent.run.duration", Value: 1, Attributes: SpanAttributes{"model": "c"}})
	global.ExportMetric(RecordedMeasurement{Kind: MetricKindHistogram, Name: "keel.agent.run.duration", Value: 1, Attributes: SpanAttributes{"model": "d"}})
	status = global.Status()
	if status.QueueDepth != 3 || status.SeriesDropped != 1 {
		t.Fatalf("global status = %#v", status)
	}
}

func TestOTLPExporterLiveRetryMayDuplicate(t *testing.T) {
	collector := newFakeCollector(t)
	collector.setResponder(func(http.ResponseWriter, *http.Request) (int, http.Header) {
		collector.mu.Lock()
		count := len(collector.requests)
		collector.mu.Unlock()
		if count == 1 {
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
	exporter.ExportSpan(testSpan("ambiguous"))
	if err := exporter.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	bodies := collector.bodies("/v1/traces")
	if len(bodies) != 2 || !strings.Contains(bodies[0], "ambiguous") || !strings.Contains(bodies[1], "ambiguous") {
		t.Fatalf("ambiguous retry bodies = %d; a live retry may duplicate a batch", len(bodies))
	}
	if got := exporter.Status().Exported; got != 1 {
		t.Fatalf("exported = %d", got)
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

// TestOTLPExporterKillRestartDoesNotReplay runs a real child process, lets it
// export one confirmed batch, then SIGKILLs it. There is no durable queue: a
// restarted exporter starts with an empty queue and does NOT replay the killed
// process's confirmed batch. Unflushed records are lost (at-most-once across a
// crash); a live exporter's ambiguous retry is the only duplicate window.
func TestOTLPExporterKillRestartDoesNotReplay(t *testing.T) {
	if os.Getenv("KEEL_OTLP_CRASH_HELPER") == "1" {
		runCrashHelper()
		return
	}
	collector := newFakeCollector(t)
	command := exec.Command(os.Args[0], "-test.run=TestOTLPExporterKillRestartDoesNotReplay", "-test.v=false")
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

	// Restart with a new record. The confirmed batch from the killed process is
	// not replayed: a crash loses unflushed data and does not duplicate acked
	// data (no persistent queue).
	restarted, err := NewOTLPExporter(testExporterOptions(collector.server.URL, []Signal{SignalTraces}))
	if err != nil {
		t.Fatal(err)
	}
	restarted.Start()
	restarted.ExportSpan(testSpan("after-restart"))
	if err := restarted.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(collector.bodies("/v1/traces"), "\n")
	if strings.Count(all, "crash-run") != 1 {
		t.Fatalf("the confirmed crash batch was replayed: %s", all)
	}
	if !strings.Contains(all, "after-restart") {
		t.Fatalf("the restarted record was not exported: %s", all)
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
