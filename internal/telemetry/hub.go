package telemetry

// TelemetryHub is the process-level seam between the PI telemetry contract and
// optional exporters (change keel-plugin-telemetry §3).
//
// The runner/provider code keeps talking to telemetry.Context; a plugin
// registers an Exporter at Start and unregisters it at Stop. The hub captures
// the exporter set at each span/metric/log creation, so a disable never pushes
// an in-flight record into a new exporter, and registration after the service
// graph exists is normal (Context() is resolved per record, not at boot).
//
// Fan-out never blocks and never returns an error to the caller: an exporter
// owns its bounded queue and drops (with a counter) when full. A panicking
// exporter is contained here too — telemetry must not be able to fail a run.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// PluginState is the telemetry plugin's observable state.
type PluginState string

const (
	// PluginDisabled is the default: the plugin is not enabled. Configured
	// telemetry values are lazy and produce no requests.
	PluginDisabled PluginState = "plugin_disabled"
	// PluginInactive means the plugin is enabled but the adapter is none/memory:
	// a legal state with no exporter.
	PluginInactive PluginState = "inactive"
	// PluginActive means an exporter is registered and running.
	PluginActive PluginState = "active"
	// PluginDegraded means the exporter is running but the collector is failing
	// (breaker/backoff); runs are unaffected.
	PluginDegraded PluginState = "degraded"
	// PluginFailed means configuration/initialization failed; no exporter exists.
	PluginFailed PluginState = "failed"
)

// PluginStates returns the closed set.
func PluginStates() []PluginState {
	return []PluginState{PluginDisabled, PluginInactive, PluginActive, PluginDegraded, PluginFailed}
}

// ExporterStatus is one exporter's bounded status. It never carries a header
// value or a record payload.
type ExporterStatus struct {
	Name           string   `json:"name"`
	State          string   `json:"state"`
	Signals        []Signal `json:"signals"`
	EndpointHost   string   `json:"endpointHost,omitempty"`
	QueueDepth     int      `json:"queueDepth"`
	Dropped        uint64   `json:"dropped"`
	Exported       uint64   `json:"exported"`
	SeriesDropped  uint64   `json:"seriesDropped"`
	LastErrorClass string   `json:"lastErrorClass,omitempty"`
}

// Exporter is the plugin-facing sink. Every method MUST be non-blocking and
// MUST tolerate being called after Close (a record captured before a disable
// may settle afterwards).
type Exporter interface {
	Name() string
	Signals() []Signal
	ExportSpan(StoredSpan)
	ExportMetric(RecordedMeasurement)
	ExportLog(LogRecord)
	Status() ExporterStatus
	Flush(ctx context.Context) error
	Close(ctx context.Context) error
}

// LogSink is the optional logging capability of a telemetry context. The PI
// Context interface stays unchanged; a host decorator type-asserts this to emit
// bounded structured logs without teaching every adapter about logging.
type LogSink interface {
	Log(LogRecord)
}

var _ LogSink = (*Hub)(nil)

// LogRecord is a structured, bounded log signal. There is deliberately no free
// text message field: only a bounded event code, a level and allowlisted
// attributes can travel.
type LogRecord struct {
	TimeMs     float64        `json:"timeMs"`
	Level      LogLevel       `json:"level"`
	Event      string         `json:"event"`
	Logger     string         `json:"logger,omitempty"`
	Attributes SpanAttributes `json:"attributes,omitempty"`
}

// HubOptions configures one tenant's hub.
type HubOptions struct {
	// TenantID is the tenant this hub serves. One process with several tenants
	// MUST create one hub per tenant; a plugin that cannot prove the binding
	// fails closed.
	TenantID string
	Base     Context
	Logger   *slog.Logger
	// SampleRatio defaults to 1.0. A pointer distinguishes "unset" from an
	// explicit 0 (traces off).
	SampleRatio *float64
	LogRatio    *float64
	Now         func() time.Time
}

// HubStatus is the GET /v1/telemetry/status projection.
type HubStatus struct {
	TenantID            string           `json:"tenantId,omitempty"`
	PluginID            string           `json:"pluginId"`
	State               PluginState      `json:"state"`
	Reason              string           `json:"reason,omitempty"`
	Enabled             bool             `json:"enabled"`
	Config              ConfigSummary    `json:"config"`
	Signals             []Signal         `json:"signals"`
	EndpointHost        string           `json:"endpointHost,omitempty"`
	HeaderNames         []string         `json:"headerNames,omitempty"`
	HeaderCount         int              `json:"headerCount"`
	SampleRatio         float64          `json:"sampleRatio"`
	LogRatio            float64          `json:"logRatio"`
	QueueDepth          int              `json:"queueDepth"`
	Dropped             uint64           `json:"dropped"`
	Exported            uint64           `json:"exported"`
	MetricSeriesDropped uint64           `json:"metricSeriesDropped"`
	AttributesDropped   uint64           `json:"attributesDropped"`
	EventsDropped       uint64           `json:"eventsDropped"`
	LinksDropped        uint64           `json:"linksDropped"`
	LastErrorClass      string           `json:"lastErrorClass,omitempty"`
	Exporters           []ExporterStatus `json:"exporters,omitempty"`
}

// Hub is the per-tenant telemetry seam.
type Hub struct {
	tenantID string
	logger   *slog.Logger
	now      func() time.Time
	scope    string

	mu          sync.RWMutex
	base        Context
	exporters   []Exporter
	sampleRatio float64
	logRatio    float64
	state       PluginState
	reason      string
	config      ConfigSummary
	closed      bool

	attributesDropped uint64
	eventsDropped     uint64
	linksDropped      uint64

	nextSpanID atomic.Int64
}

// NewHub builds a hub over the base context (Memory, Noop, ...). A nil base is
// NoopContext.
func NewHub(options HubOptions) *Hub {
	base := options.Base
	if base == nil {
		base = NoopContext
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	sampleRatio := 1.0
	if options.SampleRatio != nil {
		sampleRatio = *options.SampleRatio
	}
	if sampleRatio < 0 {
		sampleRatio = 0
	}
	if sampleRatio > 1 {
		sampleRatio = 1
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
	return &Hub{
		tenantID:    strings.TrimSpace(options.TenantID),
		logger:      logger,
		now:         now,
		scope:       newScopeID(),
		base:        base,
		sampleRatio: sampleRatio,
		logRatio:    logRatio,
		state:       PluginDisabled,
		config:      ConfigSummary{Signals: []Signal{}},
	}
}

// TenantID is the tenant this hub serves (empty in a unit-test hub).
func (h *Hub) TenantID() string { return h.tenantID }

// SetBase swaps the underlying adapter. The runner reads the base per call, so a
// tenant reload that changes adapter granularity applies without a restart.
func (h *Hub) SetBase(base Context) {
	if base == nil {
		base = NoopContext
	}
	h.mu.Lock()
	h.base = base
	h.mu.Unlock()
}

func (h *Hub) baseContext() Context {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.base
}

// Base returns the underlying adapter (diagnostics/tests). It is a snapshot:
// callers must not treat it as a stable identity across a reload.
func (h *Hub) Base() Context { return h.baseContext() }

// Configure stores the sanitized configuration projection used by status and
// approval. It never alters the active exporter set.
func (h *Hub) Configure(summary ConfigSummary) {
	h.mu.Lock()
	h.config = summary
	if summary.SampleRatio >= 0 && summary.SampleRatio <= 1 {
		h.sampleRatio = summary.SampleRatio
	}
	if summary.LogRatio >= 0 && summary.LogRatio <= 1 {
		h.logRatio = summary.LogRatio
	}
	h.mu.Unlock()
}

// SetPluginState records the plugin lifecycle state for status.
func (h *Hub) SetPluginState(state PluginState, reason string) {
	if state == "" {
		state = PluginDisabled
	}
	h.mu.Lock()
	h.state = state
	h.reason = reason
	h.mu.Unlock()
}

// SetSampleRatio updates head sampling (0..1).
func (h *Hub) SetSampleRatio(ratio float64) {
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	h.mu.Lock()
	h.sampleRatio = ratio
	h.mu.Unlock()
}

// SampleRatio reads the current head-sampling ratio.
func (h *Hub) SampleRatio() float64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.sampleRatio
}

// SetLogRatio records the configured log sampling ratio (the exporter applies
// it; the hub keeps it for status).
func (h *Hub) SetLogRatio(ratio float64) {
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	h.mu.Lock()
	h.logRatio = ratio
	h.mu.Unlock()
}

// RegisterExporter adds an exporter. The same name may not be registered twice:
// two pipelines under one name would make status ambiguous and double-export.
func (h *Hub) RegisterExporter(exporter Exporter) error {
	if exporter == nil {
		return fmt.Errorf("telemetry: exporter is nil")
	}
	name := strings.TrimSpace(exporter.Name())
	if name == "" {
		return fmt.Errorf("telemetry: exporter name is empty")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return fmt.Errorf("telemetry: hub is closed")
	}
	for _, existing := range h.exporters {
		if existing.Name() == name {
			return fmt.Errorf("telemetry: exporter %q is already registered", name)
		}
	}
	h.exporters = append(h.exporters, exporter)
	return nil
}

// UnregisterExporter removes an exporter (idempotent).
func (h *Hub) UnregisterExporter(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for index, exporter := range h.exporters {
		if exporter.Name() != name {
			continue
		}
		h.exporters = append(h.exporters[:index], h.exporters[index+1:]...)
		return true
	}
	return false
}

// ExporterCount reports the registered exporter count (tests/diagnostics).
func (h *Hub) ExporterCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.exporters)
}

func (h *Hub) exporterSnapshot() []Exporter {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.exporters) == 0 {
		return nil
	}
	out := make([]Exporter, len(h.exporters))
	copy(out, h.exporters)
	return out
}

// Flush flushes every registered exporter within ctx.
func (h *Hub) Flush(ctx context.Context) error {
	exporters := h.exporterSnapshot()
	var firstErr error
	for _, exporter := range exporters {
		if err := exporter.Flush(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Close flushes and closes every exporter. It is the host's shutdown path; the
// plugin's own Stop normally closes its exporter before this.
func (h *Hub) Close(ctx context.Context) error {
	exporters := h.exporterSnapshot()
	h.mu.Lock()
	h.closed = true
	h.exporters = nil
	h.mu.Unlock()
	var firstErr error
	for _, exporter := range exporters {
		if err := exporter.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Status renders the bounded status document.
func (h *Hub) Status() HubStatus {
	h.mu.RLock()
	state := h.state
	reason := h.reason
	config := h.config
	sampleRatio := h.sampleRatio
	logRatio := h.logRatio
	attributesDropped := h.attributesDropped
	eventsDropped := h.eventsDropped
	linksDropped := h.linksDropped
	exporters := append([]Exporter(nil), h.exporters...)
	h.mu.RUnlock()

	status := HubStatus{
		TenantID:          h.tenantID,
		PluginID:          "keel.telemetry",
		State:             state,
		Reason:            reason,
		Config:            config,
		Signals:           []Signal{},
		SampleRatio:       sampleRatio,
		LogRatio:          logRatio,
		HeaderNames:       append([]string(nil), config.HeaderNames...),
		HeaderCount:       len(config.HeaderNames),
		AttributesDropped: attributesDropped,
		EventsDropped:     eventsDropped,
		LinksDropped:      linksDropped,
	}
	if config.EndpointHost != "" {
		status.EndpointHost = config.EndpointHost
	}
	if state == "" {
		if len(exporters) > 0 {
			status.State = PluginActive
		} else {
			status.State = PluginDisabled
		}
	}
	seen := map[Signal]bool{}
	for _, exporter := range exporters {
		entry := exporter.Status()
		status.Exporters = append(status.Exporters, entry)
		for _, signal := range entry.Signals {
			if !seen[signal] {
				seen[signal] = true
				status.Signals = append(status.Signals, signal)
			}
		}
		status.QueueDepth += entry.QueueDepth
		status.Dropped += entry.Dropped
		status.Exported += entry.Exported
		status.MetricSeriesDropped += entry.SeriesDropped
		if entry.LastErrorClass != "" {
			status.LastErrorClass = entry.LastErrorClass
		}
		if entry.EndpointHost != "" && status.EndpointHost == "" {
			status.EndpointHost = entry.EndpointHost
		}
	}
	status.Signals = SignalList(status.Signals)
	if len(status.Signals) == 0 && len(config.Signals) > 0 {
		// A disabled/failed plugin still reports what it WOULD export; the
		// exporter list is empty and enabled=false, so the distinction stays
		// visible.
		status.Signals = SignalList(config.Signals)
	}
	status.Enabled = status.State == PluginActive || status.State == PluginInactive || status.State == PluginDegraded
	// An active plugin whose exporter is backing off/degraded is DEGRADED, not
	// silently active: the operator must be able to see collector trouble.
	if status.State == PluginActive {
		for _, exporter := range status.Exporters {
			if exporter.State == "degraded" {
				status.State = PluginDegraded
				break
			}
		}
	}
	return status
}

// Metrics returns the fan-out instrument factory. It is stable across exporter
// registration: the runner resolves instruments once and late registration still
// receives later measurements.
func (h *Hub) Metrics() Metrics { return hubMetrics{hub: h} }

// Propagation delegates to the base adapter (the hub owns no wire format).
func (h *Hub) Propagation() Propagation {
	base := h.baseContext()
	if base == nil {
		return NoopPropagation
	}
	return base.Propagation()
}

// Log emits one structured log record to every registered logs exporter. The
// caller must already have bounded the record; the exporter re-applies the
// allowlist.
func (h *Hub) Log(record LogRecord) {
	exporters := h.exporterSnapshot()
	if len(exporters) == 0 {
		return
	}
	if record.TimeMs == 0 {
		record.TimeMs = milliseconds(h.now())
	}
	if record.Level == "" {
		record.Level = LogLevelInfo
	}
	if name := SanitizeEventName(record.Event); name == "" {
		return
	} else {
		record.Event = name
	}
	if _, ok := ParseLogLevel(string(record.Level)); !ok {
		record.Level = LogLevelInfo
	}
	var dropped int
	record.Attributes, dropped = SanitizeSpanAttributes(record.Attributes)
	h.noteDrops(dropped, 0, 0)
	for _, exporter := range exporters {
		if !hasSignal(exporter.Signals(), SignalLogs) {
			continue
		}
		h.safeExport(func() { exporter.ExportLog(record) })
	}
}

// StartSpan starts a root span.
func (h *Hub) StartSpan(options SpanOptions, callback func(Span) error) error {
	return h.startSpan(nil, options, callback)
}

// StartManualSpan opens a root manual span (the run span tracker's shape).
func (h *Hub) StartManualSpan(options SpanOptions) (Span, func()) {
	return h.startManualSpan(nil, options)
}

const hubSpanName = "keel.telemetry.hub"

func (h *Hub) startSpan(parent *hubSpan, options SpanOptions, callback func(Span) error) (err error) {
	exporters := h.exporterSnapshot()
	if parent == nil && len(exporters) == 0 {
		return h.baseContext().StartSpan(options, callback)
	}
	started := h.now()
	run := func(baseSpan Span) error {
		span := newHubSpan(h, parent, baseSpan, exporters, options, started)
		defer func() {
			if recovered := recover(); recovered != nil {
				span.finish(err, recovered)
				panic(recovered)
			}
			span.finish(err, nil)
		}()
		err = callback(span)
		return err
	}
	if parent != nil && parent.base != nil {
		return parent.base.StartSpan(options, run)
	}
	return h.baseContext().StartSpan(options, run)
}

func (h *Hub) startManualSpan(parent *hubSpan, options SpanOptions) (Span, func()) {
	exporters := h.exporterSnapshot()
	if parent == nil && len(exporters) == 0 {
		if starter, ok := h.baseContext().(ManualStarter); ok {
			return starter.StartManualSpan(options)
		}
	}
	started := h.now()
	var baseSpan Span
	endBase := func() {}
	switch {
	case parent != nil && parent.base != nil:
		if starter, ok := parent.base.(ManualStarter); ok {
			baseSpan, endBase = starter.StartManualSpan(options)
		}
	case parent == nil:
		if starter, ok := h.baseContext().(ManualStarter); ok {
			baseSpan, endBase = starter.StartManualSpan(options)
		}
	}
	span := newHubSpan(h, parent, baseSpan, exporters, options, started)
	var once sync.Once
	return span, func() {
		once.Do(func() {
			span.finish(nil, nil)
			endBase()
		})
	}
}

func (h *Hub) shouldSample(traceID string) bool {
	ratio := h.SampleRatio()
	if ratio >= 1 {
		return true
	}
	if ratio <= 0 {
		return false
	}
	sum := sha256.Sum256([]byte(traceID))
	value := binary.BigEndian.Uint64(sum[:8])
	return float64(value)/float64(math.MaxUint64) < ratio
}

func (h *Hub) nextFallbackSpanID() string {
	return strconv.FormatInt(h.nextSpanID.Add(1), 10)
}

func (h *Hub) safeExport(export func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			h.logger.Error("telemetry exporter panicked", "err", recovered)
		}
	}()
	export()
}

// spanIdentityProvider is implemented by adapters that own stable identities
// (the Memory adapter). A hub span uses them verbatim so OTLP parentage matches
// the in-process tree; other adapters fall back to hub-owned identities.
type spanIdentityProvider interface {
	telemetrySpanIdentity() (spanID, traceID, parentID string)
}

const (
	// MaxSpanEvents bounds one span's exported event list.
	MaxSpanEvents = 128
	// MaxSpanLinks bounds one span's exported link list.
	MaxSpanLinks = 32
)

type hubSpan struct {
	hub       *Hub
	base      Span
	exporters []Exporter

	sampled bool

	mu             sync.Mutex
	spanID         string
	traceID        string
	parentID       string
	name           string
	startedMs      float64
	startedAt      time.Time
	endedMs        float64
	attributes     SpanAttributes
	events         []StoredEvent
	links          []StoredLink
	status         SpanStatus
	explicitStatus bool
	errorKind      string
	settled        bool
}

func newHubSpan(hub *Hub, parent *hubSpan, base Span, exporters []Exporter, options SpanOptions, started time.Time) *hubSpan {
	spanID, traceID, parentID := "", "", ""
	if provider, ok := base.(spanIdentityProvider); ok && base != nil {
		spanID, traceID, parentID = provider.telemetrySpanIdentity()
	}
	if spanID == "" {
		spanID = hub.nextFallbackSpanID()
	}
	if traceID == "" {
		if parent != nil {
			traceID = parent.traceID
		} else {
			traceID = hub.scope + ":" + spanID
		}
	}
	if parentID == "" && parent != nil {
		parentID = parent.spanID
	}
	sampled := true
	if parent != nil {
		sampled = parent.sampled
	} else {
		sampled = hub.shouldSample(traceID)
	}
	span := &hubSpan{
		hub:        hub,
		base:       base,
		exporters:  exporters,
		sampled:    sampled,
		spanID:     spanID,
		traceID:    traceID,
		parentID:   parentID,
		name:       options.Name,
		startedMs:  milliseconds(started),
		startedAt:  started,
		status:     OK(),
		attributes: SpanAttributes{},
	}
	if sampled {
		attributes, dropped := SanitizeSpanAttributes(options.Attributes)
		if len(attributes) > 0 {
			span.attributes = attributes
		}
		hub.noteDrops(dropped, 0, 0)
		for _, link := range options.Links {
			if len(span.links) >= MaxSpanLinks {
				hub.noteDrops(0, 0, 1)
				continue
			}
			recorded, ok, linkDropped := hubLink(link)
			hub.noteDrops(linkDropped, 0, 0)
			if ok {
				span.links = append(span.links, recorded)
			}
		}
	}
	return span
}

func (h *Hub) noteDrops(attributes, events, links int) {
	if attributes == 0 && events == 0 && links == 0 {
		return
	}
	h.mu.Lock()
	h.attributesDropped += uint64(attributes)
	h.eventsDropped += uint64(events)
	h.linksDropped += uint64(links)
	h.mu.Unlock()
}

func hubLink(link SpanLink) (StoredLink, bool, int) {
	if link.Context == nil {
		return StoredLink{}, false, 0
	}
	spanID, traceID := "", ""
	switch typed := link.Context.(type) {
	case *hubSpan:
		spanID, traceID = typed.spanID, typed.traceID
	case spanIdentityProvider:
		spanID, traceID, _ = typed.telemetrySpanIdentity()
	case carrierProvider:
		carrier := typed.telemetryCarrier()
		spanID = carrier[carrierSpanKey]
		traceID = carrier[carrierTraceKey]
	}
	if spanID == "" && traceID == "" {
		return StoredLink{}, false, 0
	}
	attributes, dropped := SanitizeSpanAttributes(link.Attributes)
	return StoredLink{TraceID: traceID, SpanID: spanID, Attributes: attributes}, true, dropped
}

func (s *hubSpan) Metrics() Metrics { return s.hub.Metrics() }

func (s *hubSpan) Propagation() Propagation {
	if s.base != nil {
		return s.base.Propagation()
	}
	return s.hub.Propagation()
}

func (s *hubSpan) StartSpan(options SpanOptions, callback func(Span) error) error {
	return s.hub.startSpan(s, options, callback)
}

func (s *hubSpan) StartManualSpan(options SpanOptions) (Span, func()) {
	return s.hub.startManualSpan(s, options)
}

func (s *hubSpan) AddLink(link SpanLink) {
	if s.base != nil {
		s.base.AddLink(link)
	}
	if !s.sampled {
		return
	}
	recorded, ok, dropped := hubLink(link)
	s.hub.noteDrops(dropped, 0, 0)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settled {
		return
	}
	if len(s.links) >= MaxSpanLinks {
		s.hub.noteDrops(0, 0, 1)
		return
	}
	s.links = append(s.links, recorded)
}

func (s *hubSpan) AddEvent(name string, attributes SpanAttributes) {
	if s.base != nil {
		s.base.AddEvent(name, attributes)
	}
	if !s.sampled {
		return
	}
	cleanName := SanitizeEventName(name)
	if cleanName == "" {
		return
	}
	s.mu.Lock()
	if s.settled {
		s.mu.Unlock()
		return
	}
	if len(s.events) >= MaxSpanEvents {
		s.mu.Unlock()
		s.hub.noteDrops(0, 1, 0)
		return
	}
	s.mu.Unlock()
	cleanAttributes, dropped := SanitizeSpanAttributes(attributes)
	s.hub.noteDrops(dropped, 0, 0)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settled {
		return
	}
	if len(s.events) >= MaxSpanEvents {
		s.hub.noteDrops(0, 1, 0)
		return
	}
	s.events = append(s.events, StoredEvent{
		Name:       cleanName,
		TimeMs:     durationMilliseconds(s.hub.now().Sub(s.startedAt)),
		Attributes: cleanAttributes,
	})
}

func (s *hubSpan) SetAttributes(attributes SpanAttributes) {
	if s.base != nil {
		s.base.SetAttributes(attributes)
	}
	if !s.sampled {
		return
	}
	clean, dropped := SanitizeSpanAttributes(attributes)
	s.hub.noteDrops(dropped, 0, 0)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settled {
		return
	}
	for name, value := range clean {
		if _, exists := s.attributes[name]; !exists && len(s.attributes) >= MaxAttributesPerRecord {
			s.hub.noteDrops(1, 0, 0)
			continue
		}
		s.attributes[name] = value
	}
}

func (s *hubSpan) SetStatus(status SpanStatus) {
	if s.base != nil {
		s.base.SetStatus(status)
	}
	if !s.sampled {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settled {
		return
	}
	s.status = exportableStatus(status)
	if status.Status == SpanStatusError {
		s.errorKind = "error"
		if status.Error != nil && strings.TrimSpace(status.Error.Name) != "" {
			s.errorKind = SanitizeEventName(status.Error.Name)
			if s.errorKind == "" {
				s.errorKind = "error"
			}
		}
	}
	s.explicitStatus = true
}

func (s *hubSpan) telemetryCarrier() Carrier {
	if provider, ok := s.base.(carrierProvider); ok {
		if carrier := provider.telemetryCarrier(); carrier != nil {
			return carrier
		}
	}
	return Carrier{
		carrierScopeKey: s.hub.scope,
		carrierSpanKey:  s.spanID,
		carrierTraceKey: s.traceID,
	}
}

func (s *hubSpan) finish(callbackErr error, panicValue any) {
	s.mu.Lock()
	if s.settled {
		s.mu.Unlock()
		return
	}
	s.settled = true
	s.endedMs = roundMicroseconds(s.startedMs + durationMilliseconds(s.hub.now().Sub(s.startedAt)))
	if !s.explicitStatus {
		switch {
		case panicValue != nil, callbackErr != nil:
			s.status = SpanStatus{Status: SpanStatusError}
			s.errorKind = "error"
		default:
			s.status = OK()
		}
	}
	if s.status.Status == SpanStatusError {
		if _, exists := s.attributes["error.kind"]; !exists {
			kind := s.errorKind
			if kind == "" {
				kind = "error"
			}
			s.attributes["error.kind"] = kind
		}
	}
	status := copyStatus(s.status)
	if !s.sampled || len(s.exporters) == 0 {
		s.mu.Unlock()
		return
	}
	stored := StoredSpan{
		ID:         s.spanID,
		TraceID:    s.traceID,
		ParentID:   s.parentID,
		Name:       s.name,
		StartedMs:  s.startedMs,
		EndedMs:    s.endedMs,
		Attributes: copyAttributes(s.attributes),
		Events:     append([]StoredEvent(nil), s.events...),
		Links:      append([]StoredLink(nil), s.links...),
		Status:     status,
	}
	exporters := s.exporters
	s.mu.Unlock()
	for _, exporter := range exporters {
		if !hasSignal(exporter.Signals(), SignalTraces) {
			continue
		}
		span := stored
		s.hub.safeExport(func() { exporter.ExportSpan(span) })
	}
}

// exportableStatus drops the raw error text: only the error class travels.
func exportableStatus(status SpanStatus) SpanStatus {
	if status.Status == SpanStatusError {
		return SpanStatus{Status: SpanStatusError}
	}
	return OK()
}

// --- fan-out metrics -------------------------------------------------------

type hubMetrics struct {
	hub *Hub
}

func (m hubMetrics) Counter(name string, options *InstrumentOptions) Counter {
	instrument := &hubCounter{hub: m.hub, name: name, kind: MetricKindCounter}
	if base := m.hub.baseContext().Metrics(); base != nil {
		instrument.base = base.Counter(name, options)
	}
	if options != nil {
		instrument.unit = options.Unit
	}
	return instrument
}

func (m hubMetrics) UpDownCounter(name string, options *InstrumentOptions) UpDownCounter {
	instrument := &hubCounter{hub: m.hub, name: name, kind: MetricKindUpDownCounter}
	if base := m.hub.baseContext().Metrics(); base != nil {
		instrument.base = base.UpDownCounter(name, options)
	}
	if options != nil {
		instrument.unit = options.Unit
	}
	return instrument
}

func (m hubMetrics) Histogram(name string, options *HistogramOptions) Histogram {
	instrument := &hubHistogram{hub: m.hub, name: name}
	if base := m.hub.baseContext().Metrics(); base != nil {
		instrument.base = base.Histogram(name, options)
	}
	if options != nil {
		instrument.unit = options.Unit
		instrument.boundaries = append([]float64(nil), options.Boundaries...)
	}
	return instrument
}

type hubCounter struct {
	hub  *Hub
	base Counter
	name string
	kind MetricKind
	unit string
}

func (c *hubCounter) Add(value float64, attributes SpanAttributes) {
	if c.base != nil {
		c.base.Add(value, attributes)
	}
	c.hub.recordMeasurement(RecordedMeasurement{
		Kind:       c.kind,
		Name:       c.name,
		Unit:       c.unit,
		Value:      value,
		Attributes: attributes,
	})
}

type hubHistogram struct {
	hub        *Hub
	base       Histogram
	name       string
	unit       string
	boundaries []float64
}

func (h *hubHistogram) Record(value float64, attributes SpanAttributes, context Context) {
	if h.base != nil {
		h.base.Record(value, attributes, context)
	}
	h.hub.recordMeasurement(RecordedMeasurement{
		Kind:       MetricKindHistogram,
		Name:       h.name,
		Unit:       h.unit,
		Boundaries: append([]float64(nil), h.boundaries...),
		Value:      value,
		Attributes: attributes,
	})
}

func (h *Hub) recordMeasurement(measurement RecordedMeasurement) {
	exporters := h.exporterSnapshot()
	if len(exporters) == 0 {
		return
	}
	measurement.TimeMs = h.now().UnixMilli()
	var dropped int
	measurement.Attributes, dropped = SanitizeMetricLabels(measurement.Attributes)
	h.noteDrops(dropped, 0, 0)
	for _, exporter := range exporters {
		if !hasSignal(exporter.Signals(), SignalMetrics) {
			continue
		}
		current := measurement
		h.safeExport(func() { exporter.ExportMetric(current) })
	}
}
