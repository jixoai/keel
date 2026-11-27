package telemetry

// OTLP/HTTP JSON exporter pipeline (change keel-plugin-telemetry §5).
//
// This replaces the old "hold everything in Memory and POST it all on Shutdown"
// adapter with a bounded, incremental pipeline:
//
//	record -> allowlist/redaction -> bounded queue -> batch -> POST /v1/{traces,metrics,logs}
//	                                                     2xx: advance the confirmed cursor
//	                                                     429: Retry-After (capped)
//	                                                     5xx/network: exponential backoff + breaker
//	                                                     other 4xx: drop the batch and count
//
// No OpenTelemetry SDK is involved: the wire encoding below is the plain JSON
// shape, and the binary-size gate (scripts/binary-size.sh) is the reason.
// Delivery semantics: there is no persistent queue. A crash loses unflushed
// batches and a restart does not replay acked ones (at-most-once across a
// restart). Within one live exporter, a batch whose response was lost after the
// collector processed it may be retried, so a collector should deduplicate by
// trace/span id.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxWarnKeys bounds the once-only warning memo.
const MaxWarnKeys = 256

// Queue limits (defaults from design §5).
type QueueLimits struct {
	MaxSpans   int
	MaxMetrics int
	MaxLogs    int
	MaxBytes   int
}

// DefaultQueueLimits returns the design defaults.
func DefaultQueueLimits() QueueLimits {
	return QueueLimits{MaxSpans: 8192, MaxMetrics: 4096, MaxLogs: 2048, MaxBytes: 8 << 20}
}

// Batch limits (defaults from design §5).
type BatchLimits struct {
	MaxSpans      int
	MaxMetrics    int
	MaxLogs       int
	MaxBytes      int
	FlushInterval time.Duration
	MaxPostBytes  int
}

// DefaultBatchLimits returns the design defaults.
func DefaultBatchLimits() BatchLimits {
	return BatchLimits{
		MaxSpans:      512,
		MaxMetrics:    4096,
		MaxLogs:       2048,
		MaxBytes:      1 << 20,
		FlushInterval: 5 * time.Second,
		MaxPostBytes:  4 << 20,
	}
}

// RetryPolicy is the retry/breaker policy (defaults from design §5).
type RetryPolicy struct {
	MaxAttempts     int
	BaseDelay       time.Duration
	MaxDelay        time.Duration
	BreakerFailures int
	BreakerOpen     time.Duration
}

// DefaultRetryPolicy returns the design defaults.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:     5,
		BaseDelay:       time.Second,
		MaxDelay:        60 * time.Second,
		BreakerFailures: 5,
		BreakerOpen:     30 * time.Second,
	}
}

// DefaultMetricNames is the closed set of telemetry metric names registered at
// plugin start. A measurement with another name is dropped and counted.
func DefaultMetricNames() map[string]MetricKind {
	return map[string]MetricKind{
		"keel.agent.runs":            MetricKindCounter,
		"keel.agent.run.duration":    MetricKindHistogram,
		"pi.ai.request.duration":     MetricKindHistogram,
		"pi.ai.first_chunk.duration": MetricKindHistogram,
		"pi.ai.tokens":               MetricKindCounter,
	}
}

// OTLPExporterOptions configures the pipeline.
type OTLPExporterOptions struct {
	Endpoint    string
	ServiceName string
	TenantID    string
	Headers     map[string]string
	Signals     []Signal
	// MetricNames is the registered metric vocabulary. Nil uses
	// DefaultMetricNames.
	MetricNames map[string]MetricKind
	LogMinLevel LogLevel
	// LogRatio defaults to 1.0; a pointer distinguishes unset from explicit 0.
	LogRatio *float64
	// MaxSeriesPerMetric and MaxSeriesGlobal bound metric cardinality.
	MaxSeriesPerMetric int
	MaxSeriesGlobal    int
	Queue              QueueLimits
	Batch              BatchLimits
	Retry              RetryPolicy
	HTTPClient         *http.Client
	Logger             *slog.Logger
	Now                func() time.Time
}

// OTLPExporter is the zero-dependency OTLP/HTTP JSON exporter.
type OTLPExporter struct {
	endpoint string
	service  string
	tenant   string
	headers  map[string]string
	client   *http.Client
	ownHTTP  bool
	logger   *slog.Logger
	now      func() time.Time

	signals     []Signal
	metricNames map[string]MetricKind
	logMin      LogLevel
	logRatio    float64
	queueLimits QueueLimits
	batchLimits BatchLimits
	retry       RetryPolicy
	maxPerName  int
	maxGlobal   int

	mu          sync.Mutex
	spans       *signalQueue
	metrics     *signalQueue
	logs        *signalQueue
	totalBytes  int
	dropped     uint64
	exported    uint64
	seriesDrop  uint64
	lastError   string
	degraded    bool
	breaker     breakerState
	series      map[string]map[string]bool
	globalCount int
	warned      map[string]bool
	nextSeq     [3]int64
	confirmed   [3]int64
	closed      bool
	started     bool
	workerCtx   context.Context
	workerStop  context.CancelFunc
	doneCh      chan struct{}
	wakeCh      chan struct{}
	flushCh     chan flushRequest
}

type flushRequest struct {
	ctx  context.Context
	done chan error
}

type breakerState struct {
	failures  int
	openUntil time.Time
	probing   bool
}

type signalKind int

const (
	kindSpans signalKind = iota
	kindMetrics
	kindLogs
)

func (k signalKind) signal() Signal {
	switch k {
	case kindMetrics:
		return SignalMetrics
	case kindLogs:
		return SignalLogs
	default:
		return SignalTraces
	}
}

func (k signalKind) path() string {
	switch k {
	case kindMetrics:
		return "/v1/metrics"
	case kindLogs:
		return "/v1/logs"
	default:
		return "/v1/traces"
	}
}

type queuedRecord struct {
	seq    int64
	size   int
	span   StoredSpan
	metric RecordedMeasurement
	log    LogRecord
}

type signalQueue struct {
	items []queuedRecord
	head  int
	bytes int
}

func (q *signalQueue) count() int { return len(q.items) - q.head }

func (q *signalQueue) oldest() (queuedRecord, bool) {
	if q.count() == 0 {
		return queuedRecord{}, false
	}
	return q.items[q.head], true
}

func (q *signalQueue) evictOldest() (queuedRecord, bool) {
	if q.count() == 0 {
		return queuedRecord{}, false
	}
	record := q.items[q.head]
	q.head++
	q.bytes -= record.size
	q.compact()
	return record, true
}

func (q *signalQueue) take(maxRecords, maxBytes int) []queuedRecord {
	if q.count() == 0 || maxRecords <= 0 {
		return nil
	}
	count := 0
	size := 0
	for q.head+count < len(q.items) && count < maxRecords {
		next := q.items[q.head+count].size
		if count > 0 && size+next > maxBytes {
			break
		}
		size += next
		count++
	}
	if count == 0 {
		return nil
	}
	out := append([]queuedRecord(nil), q.items[q.head:q.head+count]...)
	q.head += count
	q.bytes -= size
	q.compact()
	return out
}

func (q *signalQueue) compact() {
	if q.head == 0 {
		return
	}
	if q.head >= len(q.items) {
		q.items = nil
		q.head = 0
		return
	}
	if q.head > 1024 && q.head*2 > len(q.items) {
		q.items = append([]queuedRecord(nil), q.items[q.head:]...)
		q.head = 0
	}
}

// NewOTLPExporter validates the endpoint and builds a stopped exporter.
func NewOTLPExporter(options OTLPExporterOptions) (*OTLPExporter, error) {
	endpoint := strings.TrimSpace(options.Endpoint)
	if endpoint == "" {
		return nil, &ConfigErrors{Errors: []ConfigError{{Field: "endpoint", Message: "OTLP endpoint is empty"}}}
	}
	if len(endpoint) > MaxEndpointBytes {
		return nil, &ConfigErrors{Errors: []ConfigError{{Field: "endpoint", Message: fmt.Sprintf("must be at most %d bytes", MaxEndpointBytes)}}}
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, &ConfigErrors{Errors: []ConfigError{{Field: "endpoint", Message: "must be an absolute http(s) URL"}}}
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, &ConfigErrors{Errors: []ConfigError{{Field: "endpoint", Message: "scheme must be http or https"}}}
	}
	if parsed.User != nil {
		return nil, &ConfigErrors{Errors: []ConfigError{{Field: "endpoint", Message: "must not carry userinfo"}}}
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, &ConfigErrors{Errors: []ConfigError{{Field: "endpoint", Message: "must not carry a query or fragment"}}}
	}
	if err := endpointPolicy(parsed); err != nil {
		return nil, &ConfigErrors{Errors: []ConfigError{{Field: "endpoint", Message: err.Error()}}}
	}
	service := strings.TrimSpace(options.ServiceName)
	if service == "" {
		service = "keel"
	}
	if len(service) > MaxServiceNameBytes {
		return nil, &ConfigErrors{Errors: []ConfigError{{Field: "serviceName", Message: fmt.Sprintf("must be at most %d bytes", MaxServiceNameBytes)}}}
	}
	if len(options.Headers) > MaxConfigHeaders {
		return nil, &ConfigErrors{Errors: []ConfigError{{Field: "headers", Message: fmt.Sprintf("at most %d headers are allowed", MaxConfigHeaders)}}}
	}
	headers := make(map[string]string, len(options.Headers))
	for name, value := range options.Headers {
		if name == "" || len(name) > MaxHeaderNameBytes || strings.ContainsAny(name, "\r\n") || strings.ContainsAny(value, "\r\n") {
			return nil, &ConfigErrors{Errors: []ConfigError{{Field: "headers", Message: "header name/value must be non-empty, bounded and newline-free"}}}
		}
		headers[name] = value
	}
	queue := options.Queue
	defaults := DefaultQueueLimits()
	if queue.MaxSpans <= 0 {
		queue.MaxSpans = defaults.MaxSpans
	}
	if queue.MaxMetrics <= 0 {
		queue.MaxMetrics = defaults.MaxMetrics
	}
	if queue.MaxLogs <= 0 {
		queue.MaxLogs = defaults.MaxLogs
	}
	if queue.MaxBytes <= 0 {
		queue.MaxBytes = defaults.MaxBytes
	}
	batch := options.Batch
	batchDefaults := DefaultBatchLimits()
	if batch.MaxSpans <= 0 {
		batch.MaxSpans = batchDefaults.MaxSpans
	}
	if batch.MaxMetrics <= 0 {
		batch.MaxMetrics = batchDefaults.MaxMetrics
	}
	if batch.MaxLogs <= 0 {
		batch.MaxLogs = batchDefaults.MaxLogs
	}
	if batch.MaxBytes <= 0 {
		batch.MaxBytes = batchDefaults.MaxBytes
	}
	if batch.FlushInterval <= 0 {
		batch.FlushInterval = batchDefaults.FlushInterval
	}
	if batch.MaxPostBytes <= 0 {
		batch.MaxPostBytes = batchDefaults.MaxPostBytes
	}
	retry := options.Retry
	retryDefaults := DefaultRetryPolicy()
	if retry.MaxAttempts <= 0 {
		retry.MaxAttempts = retryDefaults.MaxAttempts
	}
	if retry.BaseDelay <= 0 {
		retry.BaseDelay = retryDefaults.BaseDelay
	}
	if retry.MaxDelay <= 0 {
		retry.MaxDelay = retryDefaults.MaxDelay
	}
	if retry.BreakerFailures <= 0 {
		retry.BreakerFailures = retryDefaults.BreakerFailures
	}
	if retry.BreakerOpen <= 0 {
		retry.BreakerOpen = retryDefaults.BreakerOpen
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	metricNames := options.MetricNames
	if metricNames == nil {
		metricNames = DefaultMetricNames()
	} else {
		copied := make(map[string]MetricKind, len(metricNames))
		for name, kind := range metricNames {
			copied[name] = kind
		}
		metricNames = copied
	}
	minLevel := options.LogMinLevel
	if _, ok := ParseLogLevel(string(minLevel)); !ok {
		minLevel = LogLevelInfo
	}
	logRatio := 1.0
	if options.LogRatio != nil {
		logRatio = *options.LogRatio
	}
	if logRatio < 0 {
		logRatio = 0
	}
	if logRatio > 1 {
		logRatio = 1
	}
	client := options.HTTPClient
	ownHTTP := false
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
		ownHTTP = true
	} else {
		// Do not mutate the caller's client; redirects are refused below.
		copied := *client
		client = &copied
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	perName := options.MaxSeriesPerMetric
	if perName <= 0 {
		perName = 1000
	}
	global := options.MaxSeriesGlobal
	if global <= 0 {
		global = 5000
	}
	return &OTLPExporter{
		endpoint:    strings.TrimRight(endpoint, "/"),
		service:     service,
		tenant:      strings.TrimSpace(options.TenantID),
		headers:     headers,
		client:      client,
		ownHTTP:     ownHTTP,
		logger:      logger,
		now:         now,
		signals:     SignalList(options.Signals),
		metricNames: metricNames,
		logMin:      minLevel,
		logRatio:    logRatio,
		queueLimits: queue,
		batchLimits: batch,
		retry:       retry,
		maxPerName:  perName,
		maxGlobal:   global,
		spans:       &signalQueue{},
		metrics:     &signalQueue{},
		logs:        &signalQueue{},
		series:      map[string]map[string]bool{},
		warned:      map[string]bool{},
		doneCh:      make(chan struct{}),
		wakeCh:      make(chan struct{}, 1),
		flushCh:     make(chan flushRequest),
	}, nil
}

// Name is the exporter identity shown in status.
func (e *OTLPExporter) Name() string { return "otlp-http" }

// Signals returns the enabled signal set.
func (e *OTLPExporter) Signals() []Signal { return append([]Signal(nil), e.signals...) }

// Start launches the batch worker (idempotent).
func (e *OTLPExporter) Start() {
	e.mu.Lock()
	if e.started || e.closed {
		e.mu.Unlock()
		return
	}
	e.started = true
	ctx, cancel := context.WithCancel(context.Background())
	e.workerCtx, e.workerStop = ctx, cancel
	e.mu.Unlock()
	go e.run(ctx)
}

func (e *OTLPExporter) enabled(kind signalKind) bool {
	return hasSignal(e.signals, kind.signal())
}

// ExportSpan queues one settled span. It never blocks on the network.
func (e *OTLPExporter) ExportSpan(span StoredSpan) {
	if !e.enabled(kindSpans) {
		return
	}
	span = e.prepareSpan(span)
	if span.Name == "" {
		return
	}
	record := queuedRecord{span: span, size: estimateSpanBytes(span)}
	e.enqueue(kindSpans, record)
}

// ExportMetric queues one measurement, enforcing the registered-name and
// cardinality policy.
func (e *OTLPExporter) ExportMetric(measurement RecordedMeasurement) {
	if !e.enabled(kindMetrics) {
		return
	}
	kind, registered := e.metricNames[measurement.Name]
	if !registered {
		e.recordSeriesDrop(measurement.Name, "unregistered")
		return
	}
	if kind != measurement.Kind {
		// A name has exactly one instrument kind; a mismatch is a caller bug and
		// is dropped instead of corrupting the emitted metric.
		e.recordSeriesDrop(measurement.Name, "kind")
		return
	}
	if measurement.TimeMs == 0 {
		measurement.TimeMs = e.now().UnixMilli()
	}
	measurement.Attributes, _ = SanitizeMetricLabels(measurement.Attributes)
	if !e.admitSeries(measurement.Name, attributesKey(measurement.Attributes)) {
		return
	}
	record := queuedRecord{metric: measurement, size: estimateMetricBytes(measurement)}
	e.enqueue(kindMetrics, record)
}

// ExportLog queues one structured log record after level/ratio filtering.
func (e *OTLPExporter) ExportLog(record LogRecord) {
	if !e.enabled(kindLogs) {
		return
	}
	level, ok := ParseLogLevel(string(record.Level))
	if !ok {
		level = LogLevelInfo
	}
	if LogLevelRank(level) < LogLevelRank(e.logMin) {
		return
	}
	// Errors are never dropped by the ratio: a failure must stay visible.
	if level != LogLevelError && !sampleRatio(record, e.logRatio) {
		return
	}
	record.Level = level
	record.Event = SanitizeEventName(record.Event)
	if record.Event == "" {
		return
	}
	record.Attributes, _ = SanitizeSpanAttributes(record.Attributes)
	if record.TimeMs == 0 {
		record.TimeMs = milliseconds(e.now())
	}
	if record.Logger != "" {
		if name := SanitizeEventName(record.Logger); name != "" {
			record.Logger = name
		}
	}
	queued := queuedRecord{log: record, size: estimateLogBytes(record)}
	e.enqueue(kindLogs, queued)
}

func (e *OTLPExporter) prepareSpan(span StoredSpan) StoredSpan {
	span.Name = SanitizeEventName(span.Name)
	if span.Name == "" {
		return span
	}
	span.Attributes, _ = SanitizeSpanAttributes(span.Attributes)
	span.Status = exportableStatus(span.Status)
	events := make([]StoredEvent, 0, len(span.Events))
	for _, event := range span.Events {
		name := SanitizeEventName(event.Name)
		if name == "" {
			continue
		}
		attributes, _ := SanitizeSpanAttributes(event.Attributes)
		events = append(events, StoredEvent{Name: name, TimeMs: event.TimeMs, Attributes: attributes})
	}
	span.Events = events
	links := make([]StoredLink, 0, len(span.Links))
	for _, link := range span.Links {
		attributes, _ := SanitizeSpanAttributes(link.Attributes)
		links = append(links, StoredLink{TraceID: link.TraceID, SpanID: link.SpanID, Attributes: attributes})
	}
	span.Links = links
	return span
}

func (e *OTLPExporter) admitSeries(name, key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	family := e.series[name]
	if family == nil {
		family = map[string]bool{}
		e.series[name] = family
	}
	if family[key] {
		return true
	}
	if len(family) >= e.maxPerName || e.globalCount >= e.maxGlobal {
		e.seriesDrop++
		e.warnOnceLocked("series:"+name, "telemetry metric series budget reached",
			"metric", name, "perMetric", e.maxPerName, "global", e.maxGlobal)
		return false
	}
	family[key] = true
	e.globalCount++
	return true
}

func (e *OTLPExporter) recordSeriesDrop(name, reason string) {
	e.mu.Lock()
	e.seriesDrop++
	e.warnOnceLocked("metric:"+name+":"+reason, "telemetry metric dropped",
		"metric", name, "reason", reason)
	e.mu.Unlock()
}

func (e *OTLPExporter) warnOnceLocked(key, message string, args ...any) {
	if e.warned[key] {
		return
	}
	// A hostile/buggy metric name space must not grow the warn map without
	// bound; the counters stay accurate past this cap.
	if len(e.warned) >= MaxWarnKeys {
		return
	}
	e.warned[key] = true
	e.logger.Warn(message, args...)
}

func (e *OTLPExporter) enqueue(kind signalKind, record queuedRecord) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	queue := e.queue(kind)
	record.seq = e.nextSeq[kind] + 1
	e.nextSeq[kind] = record.seq
	queue.items = append(queue.items, record)
	queue.bytes += record.size
	e.totalBytes += record.size
	limit := e.queueLimit(kind)
	for {
		if queue.count() <= limit && e.totalBytes <= e.queueLimits.MaxBytes {
			break
		}
		victim := e.oldestQueueLocked()
		if victim == nil {
			break
		}
		removed, ok := victim.evictOldest()
		if !ok {
			break
		}
		e.totalBytes -= removed.size
		e.dropped++
		e.warnOnceLocked("queue-full", "telemetry queue is full; dropping the oldest records")
	}
	wake := queue.count() >= e.batchLimit(kind) || e.totalBytes >= e.batchLimits.MaxBytes
	e.mu.Unlock()
	if wake {
		e.wake()
	}
}

func (e *OTLPExporter) queue(kind signalKind) *signalQueue {
	switch kind {
	case kindMetrics:
		return e.metrics
	case kindLogs:
		return e.logs
	default:
		return e.spans
	}
}

func (e *OTLPExporter) queueLimit(kind signalKind) int {
	switch kind {
	case kindMetrics:
		return e.queueLimits.MaxMetrics
	case kindLogs:
		return e.queueLimits.MaxLogs
	default:
		return e.queueLimits.MaxSpans
	}
}

func (e *OTLPExporter) batchLimit(kind signalKind) int {
	switch kind {
	case kindMetrics:
		return e.batchLimits.MaxMetrics
	case kindLogs:
		return e.batchLimits.MaxLogs
	default:
		return e.batchLimits.MaxSpans
	}
}

func (e *OTLPExporter) oldestQueueLocked() *signalQueue {
	var chosen *signalQueue
	var chosenSeq int64
	for _, queue := range []*signalQueue{e.spans, e.metrics, e.logs} {
		record, ok := queue.oldest()
		if !ok {
			continue
		}
		if chosen == nil || record.seq < chosenSeq {
			chosen, chosenSeq = queue, record.seq
		}
	}
	return chosen
}

func (e *OTLPExporter) wake() {
	select {
	case e.wakeCh <- struct{}{}:
	default:
	}
}

func (e *OTLPExporter) run(ctx context.Context) {
	defer close(e.doneCh)
	ticker := time.NewTicker(e.batchLimits.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = e.drain(ctx)
		case <-e.wakeCh:
			_ = e.drain(ctx)
		case request := <-e.flushCh:
			request.done <- e.drain(request.ctx)
		}
	}
}

// Flush drains the queues within ctx. A record budget miss is counted as a
// drop by Close; Flush itself reports the context error.
func (e *OTLPExporter) Flush(ctx context.Context) error {
	e.mu.Lock()
	started := e.started
	closed := e.closed
	e.mu.Unlock()
	if closed {
		return nil
	}
	if !started {
		return e.drain(ctx)
	}
	request := flushRequest{ctx: ctx, done: make(chan error, 1)}
	select {
	case e.flushCh <- request:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.doneCh:
		return e.drain(ctx)
	}
	select {
	case err := <-request.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the worker, flushes within ctx and releases every queue. It is
// idempotent; a second call is a no-op.
func (e *OTLPExporter) Close(ctx context.Context) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	started := e.started
	stop := e.workerStop
	e.mu.Unlock()
	if started && stop != nil {
		stop()
		select {
		case <-e.doneCh:
		case <-ctx.Done():
		}
	}
	err := e.drain(ctx)
	remaining := e.discardQueues()
	if remaining > 0 {
		e.mu.Lock()
		e.dropped += uint64(remaining)
		e.mu.Unlock()
	}
	if e.ownHTTP {
		e.client.CloseIdleConnections()
	}
	return err
}

func (e *OTLPExporter) discardQueues() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	remaining := e.spans.count() + e.metrics.count() + e.logs.count()
	e.spans = &signalQueue{}
	e.metrics = &signalQueue{}
	e.logs = &signalQueue{}
	e.totalBytes = 0
	return remaining
}

// drain sends every queued batch until the queues are empty or ctx ends.
func (e *OTLPExporter) drain(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		kind, batch := e.takeBatch()
		if len(batch) == 0 {
			return nil
		}
		if err := e.postBatch(ctx, kind, batch); err != nil {
			return err
		}
	}
}

func (e *OTLPExporter) takeBatch() (signalKind, []queuedRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Rotate by oldest sequence so a busy signal cannot starve another.
	victim := e.oldestQueueLocked()
	if victim == nil {
		return kindSpans, nil
	}
	var kind signalKind
	switch victim {
	case e.metrics:
		kind = kindMetrics
	case e.logs:
		kind = kindLogs
	default:
		kind = kindSpans
	}
	records := victim.take(e.batchLimit(kind), e.batchLimits.MaxBytes)
	for _, record := range records {
		e.totalBytes -= record.size
	}
	return kind, records
}

func (e *OTLPExporter) postBatch(ctx context.Context, kind signalKind, records []queuedRecord) error {
	if len(records) == 0 {
		return nil
	}
	payload := e.payload(kind, records)
	body, err := json.Marshal(payload)
	if err != nil {
		e.dropRecords(len(records), "encode")
		return nil
	}
	if len(body) <= e.batchLimits.MaxPostBytes {
		return e.postWithRetry(ctx, kind, records, body)
	}
	if len(records) == 1 {
		// A single record that alone exceeds 4 MiB is dropped rather than posted
		// partially.
		e.dropRecords(1, "oversize")
		return nil
	}
	middle := len(records) / 2
	if err := e.postBatch(ctx, kind, records[:middle]); err != nil {
		// The second half was never attempted: count it as dropped so the
		// report accounts for every record.
		e.dropRecords(len(records)-middle, "shutdown")
		return err
	}
	return e.postBatch(ctx, kind, records[middle:])
}

func (e *OTLPExporter) payload(kind signalKind, records []queuedRecord) any {
	switch kind {
	case kindMetrics:
		measurements := make([]RecordedMeasurement, 0, len(records))
		for _, record := range records {
			measurements = append(measurements, record.metric)
		}
		return e.metricPayload(measurements)
	case kindLogs:
		logs := make([]LogRecord, 0, len(records))
		for _, record := range records {
			logs = append(logs, record.log)
		}
		return e.logPayload(logs)
	default:
		spans := make([]StoredSpan, 0, len(records))
		for _, record := range records {
			spans = append(spans, record.span)
		}
		return e.tracePayload(spans)
	}
}

func (e *OTLPExporter) postWithRetry(ctx context.Context, kind signalKind, records []queuedRecord, body []byte) error {
	endSeq := records[len(records)-1].seq
	path := kind.path()
	maxAttempts := e.retry.MaxAttempts
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := e.waitBreaker(ctx); err != nil {
			e.dropRecords(len(records), "breaker_open")
			return err
		}
		status, retryAfter, class, err := e.sendOnce(ctx, path, body)
		if err == nil && status >= 200 && status < 300 {
			e.recordSuccess(kind, endSeq, len(records))
			return nil
		}
		if err == nil && status >= 300 && status < 500 && status != http.StatusTooManyRequests {
			// Other 3xx/4xx: the collector refused or redirected this batch;
			// retrying cannot fix it, so drop and count instead of looping. The
			// collector is reachable, so the breaker closes even though the
			// batch is lost. Redirects are never followed: an OTLP collector
			// must be the configured origin, or headers could be forwarded
			// elsewhere.
			e.recordReachable()
			e.dropRecords(len(records), class)
			return nil
		}
		e.recordFailure(class)
		if attempt >= maxAttempts {
			e.dropRecords(len(records), class)
			return nil
		}
		delay := e.backoff(attempt)
		if status == http.StatusTooManyRequests && retryAfter > 0 {
			delay = retryAfter
		}
		if waitErr := sleepContext(ctx, delay); waitErr != nil {
			e.dropRecords(len(records), class)
			return waitErr
		}
	}
	return nil
}

func (e *OTLPExporter) sendOnce(ctx context.Context, path string, body []byte) (int, time.Duration, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return 0, 0, "request", err
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range e.headers {
		request.Header.Set(name, value)
	}
	response, err := e.client.Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) || timeoutError(err) {
			return 0, 0, "timeout", err
		}
		return 0, 0, "network", err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		return response.StatusCode, 0, "", nil
	case response.StatusCode == http.StatusTooManyRequests:
		return response.StatusCode, parseRetryAfter(response.Header.Get("Retry-After"), e.now(), e.retry.MaxDelay), "http_429", nil
	case response.StatusCode >= 500:
		return response.StatusCode, 0, "http_5xx", nil
	case response.StatusCode >= 400:
		return response.StatusCode, 0, "http_4xx", nil
	case response.StatusCode >= 300:
		return response.StatusCode, 0, "http_3xx", nil
	default:
		return response.StatusCode, 0, "http_4xx", nil
	}
}

func (e *OTLPExporter) waitBreaker(ctx context.Context) error {
	for {
		e.mu.Lock()
		openUntil := e.breaker.openUntil
		e.mu.Unlock()
		if openUntil.IsZero() {
			return nil
		}
		now := e.now()
		if openUntil.After(now) {
			if err := sleepContext(ctx, openUntil.Sub(now)); err != nil {
				return err
			}
			continue
		}
		// Half-open: let exactly one probe through.
		e.mu.Lock()
		if e.breaker.openUntil.Equal(openUntil) {
			e.breaker.openUntil = time.Time{}
			e.breaker.probing = true
		}
		e.mu.Unlock()
		return nil
	}
}

func (e *OTLPExporter) recordSuccess(kind signalKind, endSeq int64, records int) {
	e.mu.Lock()
	e.exported += uint64(records)
	e.confirmed[kind] = endSeq
	e.degraded = false
	e.breaker = breakerState{}
	e.mu.Unlock()
}

// recordReachable closes the breaker after a response proves the collector is
// reachable (a permanent 4xx is a rejected batch, not an outage).
func (e *OTLPExporter) recordReachable() {
	e.mu.Lock()
	e.breaker = breakerState{}
	e.mu.Unlock()
}

func (e *OTLPExporter) recordFailure(class string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.degraded = true
	e.lastError = class
	breaker := &e.breaker
	if breaker.probing {
		breaker.probing = false
		breaker.openUntil = e.now().Add(e.retry.BreakerOpen)
		return
	}
	breaker.failures++
	if breaker.failures >= e.retry.BreakerFailures {
		breaker.failures = 0
		breaker.openUntil = e.now().Add(e.retry.BreakerOpen)
	}
}

func (e *OTLPExporter) dropRecords(count int, class string) {
	e.mu.Lock()
	e.dropped += uint64(count)
	if class != "" {
		e.lastError = class
		e.degraded = true
	}
	e.mu.Unlock()
}

func (e *OTLPExporter) backoff(attempt int) time.Duration {
	delay := e.retry.BaseDelay
	for index := 1; index < attempt; index++ {
		delay *= 2
		if delay >= e.retry.MaxDelay {
			return e.retry.MaxDelay
		}
	}
	if delay > e.retry.MaxDelay {
		return e.retry.MaxDelay
	}
	return delay
}

// Status renders one exporter's bounded status.
func (e *OTLPExporter) Status() ExporterStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	state := "active"
	if e.degraded {
		state = "degraded"
	}
	if e.closed {
		state = "idle"
	}
	return ExporterStatus{
		Name:           e.Name(),
		State:          state,
		Signals:        append([]Signal(nil), e.signals...),
		EndpointHost:   endpointHost(e.endpoint),
		QueueDepth:     e.spans.count() + e.metrics.count() + e.logs.count(),
		Dropped:        e.dropped,
		Exported:       e.exported,
		SeriesDropped:  e.seriesDrop,
		LastErrorClass: e.lastError,
	}
}

// confirmedSequence returns the last acknowledged record sequence for a signal
// (tests and in-process diagnostics). It is not persisted across restarts.
func (e *OTLPExporter) confirmedSequence(kind signalKind) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.confirmed[kind]
}

func endpointHost(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return parsed.Host
}

func parseRetryAfter(raw string, now time.Time, max time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds < 0 {
			return 0
		}
		delay := time.Duration(seconds) * time.Second
		if delay > max {
			return max
		}
		return delay
	}
	if when, err := http.ParseTime(raw); err == nil {
		delay := when.Sub(now)
		if delay < 0 {
			return 0
		}
		if delay > max {
			return max
		}
		return delay
	}
	return 0
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func timeoutError(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

func sampleRatio(record LogRecord, ratio float64) bool {
	if ratio >= 1 {
		return true
	}
	if ratio <= 0 {
		return false
	}
	key := record.Event + "|" + record.Logger + "|" + strconv.FormatFloat(record.TimeMs, 'f', -1, 64)
	sum := sha256.Sum256([]byte(key))
	value := binaryBigEndian(sum[:8])
	return float64(value)/float64(math.MaxUint64) < ratio
}

func binaryBigEndian(bytes []byte) uint64 {
	var value uint64
	for _, b := range bytes {
		value = value<<8 | uint64(b)
	}
	return value
}

// --- wire encoding ---------------------------------------------------------

func (e *OTLPExporter) resource() map[string]any {
	attributes := SpanAttributes{"service.name": e.service}
	if e.tenant != "" {
		attributes["tenant.id"] = e.tenant
	}
	return map[string]any{"attributes": otlpAttrs(attributes)}
}

func (e *OTLPExporter) tracePayload(spans []StoredSpan) map[string]any {
	out := make([]any, 0, len(spans))
	for _, span := range spans {
		item := map[string]any{
			"traceId":           otlpTraceID(span.TraceID),
			"spanId":            otlpSpanID(span.ID),
			"name":              span.Name,
			"kind":              1,
			"startTimeUnixNano": fmt.Sprintf("%d", int64(span.StartedMs*1e6)),
			"endTimeUnixNano":   fmt.Sprintf("%d", int64(span.EndedMs*1e6)),
		}
		if span.ParentID != "" {
			item["parentSpanId"] = otlpSpanID(span.ParentID)
		}
		if attrs := otlpAttrs(span.Attributes); len(attrs) > 0 {
			item["attributes"] = attrs
		}
		if len(span.Events) > 0 {
			events := make([]any, 0, len(span.Events))
			for _, event := range span.Events {
				events = append(events, map[string]any{
					"name":         event.Name,
					"timeUnixNano": fmt.Sprintf("%d", int64((span.StartedMs+event.TimeMs)*1e6)),
					"attributes":   otlpAttrs(event.Attributes),
				})
			}
			item["events"] = events
		}
		if len(span.Links) > 0 {
			links := make([]any, 0, len(span.Links))
			for _, link := range span.Links {
				encoded := map[string]any{
					"traceId": otlpTraceID(link.TraceID),
					"spanId":  otlpSpanID(link.SpanID),
				}
				if attrs := otlpAttrs(link.Attributes); len(attrs) > 0 {
					encoded["attributes"] = attrs
				}
				links = append(links, encoded)
			}
			item["links"] = links
		}
		status := map[string]any{"code": 1}
		if span.Status.Status == SpanStatusError {
			status["code"] = 2
			// No status.message: the raw error text is not a telemetry fact.
		}
		item["status"] = status
		out = append(out, item)
	}
	return map[string]any{"resourceSpans": []any{map[string]any{
		"resource": e.resource(),
		"scopeSpans": []any{map[string]any{
			"scope": map[string]any{"name": "keel"},
			"spans": out,
		}},
	}}}
}

func (e *OTLPExporter) metricPayload(measurements []RecordedMeasurement) map[string]any {
	type metricAgg struct {
		kind   MetricKind
		unit   string
		points []any
	}
	order := make([]string, 0)
	byName := map[string]*metricAgg{}
	for _, measurement := range measurements {
		key := string(measurement.Kind) + "\x00" + measurement.Name
		agg, ok := byName[key]
		if !ok {
			agg = &metricAgg{kind: measurement.Kind, unit: measurement.Unit}
			byName[key] = agg
			order = append(order, key)
		}
		timestamp := measurement.TimeMs
		if timestamp == 0 {
			timestamp = e.now().UnixMilli()
		}
		timeUnixNano := fmt.Sprintf("%d", timestamp*1e6)
		if measurement.Kind == MetricKindHistogram {
			point := map[string]any{
				// A DELTA point still carries the interval start; this pipeline
				// exports instant measurements, so start == end.
				"startTimeUnixNano": timeUnixNano,
				"timeUnixNano":      timeUnixNano,
				"count":             "1",
				"sum":               measurement.Value,
			}
			if len(measurement.Boundaries) > 0 {
				point["bucketCounts"] = make([]string, len(measurement.Boundaries)+1)
				counts := point["bucketCounts"].([]string)
				for index := range counts {
					counts[index] = "0"
				}
				bucket := len(measurement.Boundaries) // +Inf unless a bound is larger
				for index, bound := range measurement.Boundaries {
					if measurement.Value < bound {
						bucket = index
						break
					}
				}
				counts[bucket] = "1"
				point["explicitBounds"] = measurement.Boundaries
			} else {
				point["bucketCounts"] = []string{"1"}
				point["explicitBounds"] = []float64{}
			}
			if attrs := otlpAttrs(measurement.Attributes); len(attrs) > 0 {
				point["attributes"] = attrs
			}
			agg.points = append(agg.points, point)
			continue
		}
		point := map[string]any{
			"startTimeUnixNano": timeUnixNano,
			"timeUnixNano":      timeUnixNano,
			"asDouble":          measurement.Value,
		}
		if attrs := otlpAttrs(measurement.Attributes); len(attrs) > 0 {
			point["attributes"] = attrs
		}
		agg.points = append(agg.points, point)
	}
	metrics := make([]any, 0, len(order))
	for _, key := range order {
		agg := byName[key]
		name := strings.SplitN(key, "\x00", 2)[1]
		metric := map[string]any{"name": name}
		if agg.unit != "" {
			metric["unit"] = agg.unit
		}
		if agg.kind == MetricKindHistogram {
			metric["histogram"] = map[string]any{
				// Each data point is one measurement delta, not a cumulative sum:
				// declaring CUMULATIVE would make consumers under-count.
				"aggregationTemporality": 1,
				"dataPoints":             agg.points,
			}
		} else {
			metric["sum"] = map[string]any{
				"aggregationTemporality": 1,
				"isMonotonic":            agg.kind == MetricKindCounter,
				"dataPoints":             agg.points,
			}
		}
		metrics = append(metrics, metric)
	}
	return map[string]any{"resourceMetrics": []any{map[string]any{
		"resource": e.resource(),
		"scopeMetrics": []any{map[string]any{
			"scope":   map[string]any{"name": "keel"},
			"metrics": metrics,
		}},
	}}}
}

func (e *OTLPExporter) logPayload(records []LogRecord) map[string]any {
	out := make([]any, 0, len(records))
	for _, record := range records {
		severity, text := severityNumber(record.Level)
		timestamp := record.TimeMs
		if timestamp == 0 {
			timestamp = milliseconds(e.now())
		}
		item := map[string]any{
			"timeUnixNano":   fmt.Sprintf("%d", int64(timestamp*1e6)),
			"severityNumber": severity,
			"severityText":   text,
			"body":           map[string]any{"stringValue": record.Event},
		}
		attributes := SpanAttributes{}
		for name, value := range record.Attributes {
			attributes[name] = value
		}
		if record.Event != "" {
			if _, exists := attributes["event"]; !exists {
				attributes["event"] = record.Event
			}
		}
		if record.Logger != "" {
			if _, exists := attributes["logger"]; !exists {
				attributes["logger"] = record.Logger
			}
		}
		if _, exists := attributes["level"]; !exists {
			attributes["level"] = string(record.Level)
		}
		clean, _ := SanitizeSpanAttributes(attributes)
		if attrs := otlpAttrs(clean); len(attrs) > 0 {
			item["attributes"] = attrs
		}
		out = append(out, item)
	}
	return map[string]any{"resourceLogs": []any{map[string]any{
		"resource": e.resource(),
		"scopeLogs": []any{map[string]any{
			"scope":      map[string]any{"name": "keel"},
			"logRecords": out,
		}},
	}}}
}

func severityNumber(level LogLevel) (int, string) {
	switch level {
	case LogLevelDebug:
		return 5, "DEBUG"
	case LogLevelWarn:
		return 13, "WARN"
	case LogLevelError:
		return 17, "ERROR"
	default:
		return 9, "INFO"
	}
}

// --- byte estimates --------------------------------------------------------

func estimateSpanBytes(span StoredSpan) int {
	size := 64 + len(span.Name) + len(span.ID) + len(span.TraceID) + len(span.ParentID)
	size += estimateAttributesBytes(span.Attributes)
	for _, event := range span.Events {
		size += 24 + len(event.Name) + estimateAttributesBytes(event.Attributes)
	}
	for _, link := range span.Links {
		size += 32 + estimateAttributesBytes(link.Attributes)
	}
	return size
}

func estimateMetricBytes(measurement RecordedMeasurement) int {
	size := 64 + len(measurement.Name) + len(measurement.Unit)
	size += estimateAttributesBytes(measurement.Attributes)
	return size
}

func estimateLogBytes(record LogRecord) int {
	return 48 + len(record.Event) + len(record.Logger) + estimateAttributesBytes(record.Attributes)
}

func estimateAttributesBytes(attributes SpanAttributes) int {
	size := 0
	for name, value := range attributes {
		size += len(name) + 8
		switch typed := value.(type) {
		case string:
			size += len(typed)
		case []string:
			for _, item := range typed {
				size += len(item)
			}
		case []float64:
			size += 8 * len(typed)
		case []bool:
			size += len(typed)
		default:
			size += 8
		}
	}
	return size
}

// --- OTLP JSON primitives --------------------------------------------------

func otlpTraceID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:16])
}

func otlpSpanID(id string) string {
	if index := strings.LastIndexByte(id, ':'); index >= 0 {
		id = id[index+1:]
	}
	if value, err := strconv.ParseUint(id, 10, 64); err == nil {
		return fmt.Sprintf("%016x", value)
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:8])
}

func otlpAttrs(attrs SpanAttributes) []any {
	if len(attrs) == 0 {
		return nil
	}
	names := sortedKeys(attrs)
	out := make([]any, 0, len(names))
	for _, key := range names {
		encoded := otlpValue(attrs[key])
		if encoded == nil {
			continue
		}
		out = append(out, map[string]any{"key": key, "value": encoded})
	}
	return out
}

func otlpValue(value AttributeValue) any {
	switch typed := value.(type) {
	case string:
		return map[string]any{"stringValue": typed}
	case bool:
		return map[string]any{"boolValue": typed}
	case float64:
		return map[string]any{"doubleValue": typed}
	case int:
		return map[string]any{"intValue": fmt.Sprintf("%d", typed)}
	case int64:
		return map[string]any{"intValue": fmt.Sprintf("%d", typed)}
	case []string:
		values := make([]any, 0, len(typed))
		for _, item := range typed {
			values = append(values, map[string]any{"stringValue": item})
		}
		return map[string]any{"arrayValue": map[string]any{"values": values}}
	case []float64:
		values := make([]any, 0, len(typed))
		for _, item := range typed {
			values = append(values, map[string]any{"doubleValue": item})
		}
		return map[string]any{"arrayValue": map[string]any{"values": values}}
	case []bool:
		values := make([]any, 0, len(typed))
		for _, item := range typed {
			values = append(values, map[string]any{"boolValue": item})
		}
		return map[string]any{"arrayValue": map[string]any{"values": values}}
	default:
		return nil
	}
}
