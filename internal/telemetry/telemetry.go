// Package telemetry implements PI's vendor-neutral telemetry contract in Go.
//
// Contract source: pi/packages/telemetry/src/{index.ts,metrics.ts,memory.ts,
// noop.ts} @ 55a5c6914570e2ae31ca36297654483bbbf46883. The callback-scoped span
// model, instrument shape, opaque propagation carrier, and settled trace/metric
// snapshots are kept; storage, clock, and concurrency are Go-native.
//
// Times are wall-clock milliseconds since the Unix epoch, recorded at
// microsecond resolution (PI index.ts:32-35). Event times are relative to their
// span start. All recorded snapshots are detached copies: mutating a value
// returned by this package never changes recorder state.
package telemetry

// AttributeValue is one attribute value. PI allows a string, number, boolean,
// or a homogeneous array of those (index.ts:3). Keel normalizes every Go
// numeric kind to float64 and copies arrays on write. Unsupported values are
// dropped, matching PI's unspecified malformed-input behavior (index.ts:101).
type AttributeValue interface{}

// SpanAttributes maps attribute names to values. A nil value is ignored on
// write (PI treats undefined the same way; index.ts:6, memory.ts:141-147).
type SpanAttributes map[string]AttributeValue

// Carrier is an opaque, JSON-serializable propagation payload (index.ts:9-10).
type Carrier map[string]string

// Propagation is the optional transport capability of an adapter (index.ts:12-17).
// Inject reports false when the context has no adapter-owned identity. Extract
// treats a nil or unrecognized carrier as the adapter root.
type Propagation interface {
	Inject(context Context) (Carrier, bool)
	Extract(carrier Carrier) Context
}

// SpanLink links a span to another context, optionally with attributes
// (index.ts:19-22).
type SpanLink struct {
	Context    Context
	Attributes SpanAttributes
}

// SpanOptions configures a new span (index.ts:24-28).
type SpanOptions struct {
	Name       string
	Attributes SpanAttributes
	Links      []SpanLink
}

// SpanStatusKind is the status discriminant (index.ts:30).
type SpanStatusKind string

const (
	// SpanStatusOK is a successful span.
	SpanStatusOK SpanStatusKind = "ok"
	// SpanStatusError marks a failed span.
	SpanStatusError SpanStatusKind = "error"
)

// SpanError is the optional error detail of an error status (index.ts:30).
type SpanError struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

// SpanStatus is OK or error with optional detail (index.ts:30).
type SpanStatus struct {
	Status SpanStatusKind `json:"status"`
	Error  *SpanError     `json:"error,omitempty"`
}

// OK returns a successful status.
func OK() SpanStatus {
	return SpanStatus{Status: SpanStatusOK}
}

// ErrorStatus returns an error status with a detached copy of err.
func ErrorStatus(err *SpanError) SpanStatus {
	status := SpanStatus{Status: SpanStatusError}
	if err != nil {
		detail := *err
		status.Error = &detail
	}
	return status
}

// ErrorStatusFromError maps a Go error to an error status. PI uses the JS
// error's name and message; Go's closest stable pair is "error" and Error().
func ErrorStatusFromError(err error) SpanStatus {
	status := SpanStatus{Status: SpanStatusError}
	if err != nil {
		status.Error = &SpanError{Name: "error", Message: err.Error()}
	}
	return status
}

// StoredEvent is a settled span event (index.ts:45).
type StoredEvent struct {
	Name       string         `json:"name"`
	TimeMs     float64        `json:"timeMs"`
	Attributes SpanAttributes `json:"attributes"`
}

// StoredLink is a settled span link (index.ts:46-50).
type StoredLink struct {
	TraceID    string         `json:"traceId"`
	SpanID     string         `json:"spanId"`
	Attributes SpanAttributes `json:"attributes,omitempty"`
}

// StoredSpan is a settled span snapshot independent of the recording adapter
// (index.ts:36-52). ParentID is empty for roots and for parents owned by
// another recorder (PI omits the field; index.ts:39).
type StoredSpan struct {
	ID         string         `json:"id"`
	TraceID    string         `json:"traceId"`
	ParentID   string         `json:"parentId,omitempty"`
	Name       string         `json:"name"`
	StartedMs  float64        `json:"startedMs"`
	EndedMs    float64        `json:"endedMs"`
	Attributes SpanAttributes `json:"attributes"`
	Events     []StoredEvent  `json:"events"`
	Links      []StoredLink   `json:"links"`
	Status     SpanStatus     `json:"status"`
}

// StoredTrace is all settled spans of one trace (index.ts:54-57).
type StoredTrace struct {
	TraceID string       `json:"traceId"`
	Spans   []StoredSpan `json:"spans"`
}

// TraceQuery filters stored traces (index.ts:59-72).
type TraceQuery struct {
	// Name matches the name of the earliest stored span (span ID breaks ties).
	Name string
	// Attributes must all match one span; that span may be anywhere in the trace.
	Attributes SpanAttributes
	// Errors restricts results to traces with at least one error span.
	Errors bool
	// StartMin and StartMax are inclusive bounds on the earliest span start.
	StartMin *float64
	StartMax *float64
	// Limit is the page size; values <= 0 use the default of 20.
	Limit int
	// Cursor is an opaque continuation from the previous page. Keep the same
	// filters when following it.
	Cursor string
}

// MetricQuery filters stored metrics (index.ts:74-80).
type MetricQuery struct {
	Name string
	// StartMin and StartMax are inclusive bounds on the interval start.
	StartMin   *float64
	StartMax   *float64
	Attributes SpanAttributes
}

// MetricKind is the instrument discriminant (index.ts:85).
type MetricKind string

const (
	MetricKindCounter       MetricKind = "counter"
	MetricKindUpDownCounter MetricKind = "upDownCounter"
	MetricKindHistogram     MetricKind = "histogram"
)

// StoredMetric is one aggregated measurement interval (index.ts:82-92).
type StoredMetric struct {
	IntervalMs int64          `json:"intervalMs"`
	Name       string         `json:"name"`
	Kind       MetricKind     `json:"kind"`
	Unit       string         `json:"unit,omitempty"`
	Attributes SpanAttributes `json:"attributes"`
	Count      int64          `json:"count"`
	Sum        float64        `json:"sum"`
	Min        float64        `json:"min"`
	Max        float64        `json:"max"`
}

// TracePage is one page of stored traces (index.ts:96).
type TracePage struct {
	Traces []StoredTrace `json:"traces"`
	// Cursor is empty when there are no more results.
	Cursor string `json:"cursor,omitempty"`
}

// Store reads settled traces without depending on a storage backend
// (index.ts:94-99). Memory is the in-process reference implementation;
// PostgreSQL and other backends implement the same interface.
type Store interface {
	FindTraces(query TraceQuery) (TracePage, error)
	GetTrace(traceID string) (*StoredTrace, error)
	FindMetrics(query MetricQuery) ([]StoredMetric, error)
}

// Context is the adapter contract for trusted typed inputs (index.ts:101-107).
// Metrics or Propagation may be nil when an adapter does not implement them.
type Context interface {
	Metrics() Metrics
	Propagation() Propagation
	// StartSpan admits the callback once, synchronously, and settles the span
	// when the callback returns or panics.
	StartSpan(options SpanOptions, callback func(Span) error) error
}

// Span is a live recording scope that is also a Context for child spans
// (index.ts:109-114). Mutating calls after settlement are inert.
type Span interface {
	Context
	AddLink(link SpanLink)
	AddEvent(name string, attributes SpanAttributes)
	SetAttributes(attributes SpanAttributes)
	SetStatus(status SpanStatus)
}

// StartSpan is the generic convenience wrapper around Context.StartSpan. It
// returns the callback result and error, and re-panics after settling if the
// callback panics.
func StartSpan[T any](ctx Context, options SpanOptions, callback func(Span) (T, error)) (T, error) {
	var value T
	err := ctx.StartSpan(options, func(span Span) error {
		var callbackErr error
		value, callbackErr = callback(span)
		return callbackErr
	})
	return value, err
}

func copyStatus(status SpanStatus) SpanStatus {
	copied := SpanStatus{Status: status.Status}
	if status.Error != nil {
		detail := *status.Error
		copied.Error = &detail
	}
	return copied
}
