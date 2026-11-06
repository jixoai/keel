// Ported from pi/packages/telemetry/src/metrics.ts @ 55a5c6914570e2ae31ca36297654483bbbf46883.
// Synchronous instrument factories and recording methods are kept as-is;
// metadata and measurement attributes are copied at the boundaries.

package telemetry

// InstrumentOptions carries optional instrument metadata (metrics.ts:10-13).
type InstrumentOptions struct {
	Description string
	Unit        string
}

// HistogramOptions adds bucket hints (metrics.ts:15-18).
type HistogramOptions struct {
	InstrumentOptions
	// Boundaries are caller-supplied finite, strictly increasing bucket hints.
	Boundaries []float64
}

// Counter records nonnegative increments (metrics.ts:20-22).
type Counter interface {
	Add(value float64, attributes SpanAttributes)
}

// UpDownCounter records increments and decrements (metrics.ts:24-26).
type UpDownCounter interface {
	Add(value float64, attributes SpanAttributes)
}

// Histogram records distributions. The context is an optional exemplar
// (metrics.ts:28-30); pass nil when unmatched.
type Histogram interface {
	Record(value float64, attributes SpanAttributes, context Context)
}

// Metrics is the adapter capability with synchronous factories (metrics.ts:32-41).
type Metrics interface {
	Counter(name string, options *InstrumentOptions) Counter
	UpDownCounter(name string, options *InstrumentOptions) UpDownCounter
	Histogram(name string, options *HistogramOptions) Histogram
}

// RecordedMeasurement is one raw measurement in recording order, before
// interval aggregation (memory.ts:52-61). It is the memory adapter's
// equivalent of PI getMetrics().
type RecordedMeasurement struct {
	Kind        MetricKind
	Name        string
	Description string
	Unit        string
	Boundaries  []float64
	Value       float64
	Attributes  SpanAttributes
	// Context is the exemplar span carrier, or nil.
	Context Carrier
}

const metricIntervalMs int64 = 10_000

type memoryCounter struct {
	memory      *Memory
	name        string
	description string
	unit        string
}

func (c *memoryCounter) Add(value float64, attributes SpanAttributes) {
	c.memory.recordMeasurement(measurementRecord{
		kind:        MetricKindCounter,
		name:        c.name,
		description: c.description,
		unit:        c.unit,
		value:       value,
		attributes:  copyAttributes(attributes),
	})
}

type memoryUpDownCounter struct {
	memory      *Memory
	name        string
	description string
	unit        string
}

func (c *memoryUpDownCounter) Add(value float64, attributes SpanAttributes) {
	c.memory.recordMeasurement(measurementRecord{
		kind:        MetricKindUpDownCounter,
		name:        c.name,
		description: c.description,
		unit:        c.unit,
		value:       value,
		attributes:  copyAttributes(attributes),
	})
}

type memoryHistogram struct {
	memory      *Memory
	name        string
	description string
	unit        string
	boundaries  []float64
}

func (h *memoryHistogram) Record(value float64, attributes SpanAttributes, context Context) {
	var carrier Carrier
	if provider, ok := context.(carrierProvider); ok {
		carrier = copyCarrier(provider.telemetryCarrier())
	}
	h.memory.recordMeasurement(measurementRecord{
		kind:        MetricKindHistogram,
		name:        h.name,
		description: h.description,
		unit:        h.unit,
		boundaries:  h.boundaries,
		value:       value,
		attributes:  copyAttributes(attributes),
		context:     carrier,
	})
}

// Counter creates a counter instrument. Metadata is captured at creation time
// (memory.ts:287), so later mutation of options has no effect.
func (m *Memory) Counter(name string, options *InstrumentOptions) Counter {
	instrument := &memoryCounter{memory: m, name: name}
	if options != nil {
		instrument.description = options.Description
		instrument.unit = options.Unit
	}
	return instrument
}

// UpDownCounter creates an up/down counter instrument.
func (m *Memory) UpDownCounter(name string, options *InstrumentOptions) UpDownCounter {
	instrument := &memoryUpDownCounter{memory: m, name: name}
	if options != nil {
		instrument.description = options.Description
		instrument.unit = options.Unit
	}
	return instrument
}

// Histogram creates a histogram instrument with copied bucket hints.
func (m *Memory) Histogram(name string, options *HistogramOptions) Histogram {
	instrument := &memoryHistogram{memory: m, name: name}
	if options != nil {
		instrument.description = options.Description
		instrument.unit = options.Unit
		instrument.boundaries = append([]float64(nil), options.Boundaries...)
	}
	return instrument
}

type measurementRecord struct {
	intervalMs  int64
	kind        MetricKind
	name        string
	description string
	unit        string
	boundaries  []float64
	value       float64
	attributes  SpanAttributes
	context     Carrier
}

func (m *Memory) recordMeasurement(record measurementRecord) {
	interval := m.now().UnixMilli()
	record.intervalMs = interval / metricIntervalMs * metricIntervalMs
	m.mu.Lock()
	m.measurements = append(m.measurements, record)
	m.mu.Unlock()
}

// Measurements returns detached raw measurements in recording order.
func (m *Memory) Measurements() []RecordedMeasurement {
	m.mu.Lock()
	defer m.mu.Unlock()
	measurements := make([]RecordedMeasurement, 0, len(m.measurements))
	for _, record := range m.measurements {
		measurements = append(measurements, RecordedMeasurement{
			Kind:        record.kind,
			Name:        record.name,
			Description: record.description,
			Unit:        record.unit,
			Boundaries:  append([]float64(nil), record.boundaries...),
			Value:       record.value,
			Attributes:  copyAttributes(record.attributes),
			Context:     copyCarrier(record.context),
		})
	}
	return measurements
}
