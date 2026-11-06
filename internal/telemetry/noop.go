// Ported from pi/packages/telemetry/src/noop.ts @ 55a5c6914570e2ae31ca36297654483bbbf46883.
// Go has no Object.freeze; the no-op values are stateless and never retain
// inputs, so sharing them is safe.

package telemetry

type noopSpan struct{}

// NoopContext is the shared inert span used when an application does not
// provide telemetry (noop.ts:25-37). StartSpan admits the callback directly
// and never records or allocates span state.
var NoopContext Span = noopSpan{}

func (noopSpan) Metrics() Metrics         { return NoopMetrics }
func (noopSpan) Propagation() Propagation { return NoopPropagation }
func (noopSpan) StartSpan(_ SpanOptions, callback func(Span) error) error {
	return callback(NoopContext)
}
func (noopSpan) AddLink(SpanLink)                {}
func (noopSpan) AddEvent(string, SpanAttributes) {}
func (noopSpan) SetAttributes(SpanAttributes)    {}
func (noopSpan) SetStatus(SpanStatus)            {}

type noopCounter struct{}

func (noopCounter) Add(float64, SpanAttributes) {}

type noopUpDownCounter struct{}

func (noopUpDownCounter) Add(float64, SpanAttributes) {}

type noopHistogram struct{}

func (noopHistogram) Record(float64, SpanAttributes, Context) {}

type noopMetrics struct{}

// NoopMetrics returns shared instruments that ignore all inputs (noop.ts:8-17).
var NoopMetrics Metrics = noopMetrics{}

func (noopMetrics) Counter(string, *InstrumentOptions) Counter { return noopCounter{} }
func (noopMetrics) UpDownCounter(string, *InstrumentOptions) UpDownCounter {
	return noopUpDownCounter{}
}
func (noopMetrics) Histogram(string, *HistogramOptions) Histogram { return noopHistogram{} }

type noopPropagation struct{}

// NoopPropagation neither inspects nor retains carriers (noop.ts:19-23).
var NoopPropagation Propagation = noopPropagation{}

func (noopPropagation) Inject(Context) (Carrier, bool) { return nil, false }
func (noopPropagation) Extract(Carrier) Context        { return NoopContext }
