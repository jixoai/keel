// Ported from pi/packages/telemetry/test/metrics.test.ts:48-165 and the
// metrics group of src/testing/conformance.ts:75-126
// @ 55a5c6914570e2ae31ca36297654483bbbf46883.
// One deliberate difference: FindMetrics aggregates intervals (count/sum/min/
// max) while Measurements() keeps PI's ordered raw rows.

package telemetry

import (
	"reflect"
	"testing"
)

func TestMetricsRecordOrderedMeasurementsAndExemplars(t *testing.T) {
	memory := NewMemory()
	memory.Counter("bytes", &InstrumentOptions{Description: "Encoded bytes", Unit: "By"}).
		Add(12, SpanAttributes{"direction": "out"})

	active := memory.UpDownCounter("active", nil)
	active.Add(1, nil)
	active.Add(-1, nil)

	histogram := memory.Histogram("duration", &HistogramOptions{
		InstrumentOptions: InstrumentOptions{Unit: "ms"},
		Boundaries:        []float64{1, 10},
	})

	var spanCarrier Carrier
	requireError(t, memory.StartSpan(SpanOptions{Name: "measured"}, func(span Span) error {
		histogram.Record(5, SpanAttributes{"method": "read"}, span)
		carrier, ok := memory.Inject(span)
		if !ok {
			t.Fatal("expected span carrier")
		}
		spanCarrier = carrier
		return nil
	}))

	measurements := memory.Measurements()
	if len(measurements) != 4 {
		t.Fatalf("measurements = %d, want 4", len(measurements))
	}
	got := make([][3]any, 0, len(measurements))
	for _, measurement := range measurements {
		got = append(got, [3]any{measurement.Kind, measurement.Name, measurement.Value})
	}
	want := [][3]any{
		{MetricKindCounter, "bytes", float64(12)},
		{MetricKindUpDownCounter, "active", float64(1)},
		{MetricKindUpDownCounter, "active", float64(-1)},
		{MetricKindHistogram, "duration", float64(5)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("measurements = %v, want %v", got, want)
	}
	requireAttributes(t, measurements[0].Attributes, SpanAttributes{"direction": "out"})
	requireEqual(t, "counter description", measurements[0].Description, "Encoded bytes")
	requireEqual(t, "counter unit", measurements[0].Unit, "By")
	if !reflect.DeepEqual(measurements[3].Boundaries, []float64{1, 10}) {
		t.Fatalf("histogram boundaries = %v, want [1 10]", measurements[3].Boundaries)
	}
	if !reflect.DeepEqual(measurements[3].Context, spanCarrier) {
		t.Fatalf("histogram exemplar = %v, want %v", measurements[3].Context, spanCarrier)
	}
}

func TestMetricsAggregateCountSumMinMax(t *testing.T) {
	memory := NewMemory()
	counter := memory.Counter("bytes", &InstrumentOptions{Description: "Encoded bytes", Unit: "By"})
	counter.Add(12, SpanAttributes{"direction": "out"})
	active := memory.UpDownCounter("active", nil)
	active.Add(1, nil)
	active.Add(-1, nil)
	histogram := memory.Histogram("duration", &HistogramOptions{Boundaries: []float64{1, 10}})
	requireError(t, memory.StartSpan(SpanOptions{Name: "measured"}, func(span Span) error {
		histogram.Record(5, SpanAttributes{"method": "read"}, span)
		return nil
	}))

	bytes := findMetrics(t, memory, MetricQuery{Name: "bytes"})
	if len(bytes) != 1 {
		t.Fatalf("bytes rows = %v, want 1", bytes)
	}
	requireEqual(t, "bytes kind", bytes[0].Kind, MetricKindCounter)
	requireEqual(t, "bytes count", bytes[0].Count, int64(1))
	requireFloat(t, "bytes sum", bytes[0].Sum, 12)
	requireFloat(t, "bytes min", bytes[0].Min, 12)
	requireFloat(t, "bytes max", bytes[0].Max, 12)
	requireEqual(t, "bytes unit", bytes[0].Unit, "By")
	requireAttributes(t, bytes[0].Attributes, SpanAttributes{"direction": "out"})

	activeRows := findMetrics(t, memory, MetricQuery{Name: "active"})
	if len(activeRows) != 1 {
		t.Fatalf("active rows = %v, want 1", activeRows)
	}
	requireEqual(t, "active kind", activeRows[0].Kind, MetricKindUpDownCounter)
	requireEqual(t, "active count", activeRows[0].Count, int64(2))
	requireFloat(t, "active sum", activeRows[0].Sum, 0)
	requireFloat(t, "active min", activeRows[0].Min, -1)
	requireFloat(t, "active max", activeRows[0].Max, 1)

	duration := findMetrics(t, memory, MetricQuery{Name: "duration"})
	if len(duration) != 1 {
		t.Fatalf("duration rows = %v, want 1", duration)
	}
	requireEqual(t, "duration kind", duration[0].Kind, MetricKindHistogram)
	requireEqual(t, "duration count", duration[0].Count, int64(1))
	requireFloat(t, "duration sum", duration[0].Sum, 5)
	requireFloat(t, "duration min", duration[0].Min, 5)
	requireFloat(t, "duration max", duration[0].Max, 5)
}

func TestMetricsCopyInstrumentMetadataAndMeasurementAttributes(t *testing.T) {
	memory := NewMemory()
	boundaries := []float64{1, 10}
	options := &HistogramOptions{
		InstrumentOptions: InstrumentOptions{Description: "Original"},
		Boundaries:        boundaries,
	}
	histogram := memory.Histogram("snapshot", options)
	boundaries[0] = 100
	options.Description = "Changed"

	attributes := SpanAttributes{"tags": []string{"original"}}
	histogram.Record(-1, attributes, nil)
	attributes["tags"].([]string)[0] = "changed"

	measurements := memory.Measurements()
	if len(measurements) != 1 {
		t.Fatalf("measurements = %d, want 1", len(measurements))
	}
	requireAttributes(t, measurements[0].Attributes, SpanAttributes{"tags": []string{"original"}})
	if !reflect.DeepEqual(measurements[0].Boundaries, []float64{1, 10}) {
		t.Fatalf("boundaries = %v, want [1 10]", measurements[0].Boundaries)
	}
	requireEqual(t, "description", measurements[0].Description, "Original")
	if measurements[0].Context != nil {
		t.Fatalf("context = %v, want nil", measurements[0].Context)
	}
}

func TestMetricsMeasurementsAreDetached(t *testing.T) {
	memory := NewMemory()
	histogram := memory.Histogram("duration", &HistogramOptions{Boundaries: []float64{1, 2}})
	requireError(t, memory.StartSpan(SpanOptions{Name: "request"}, func(span Span) error {
		histogram.Record(1, SpanAttributes{"tags": []string{"original"}}, span)
		return nil
	}))

	measurement := memory.Measurements()[0]
	if measurement.Context == nil {
		t.Fatal("expected exemplar context")
	}
	measurement.Boundaries[0] = 3
	measurement.Attributes["tags"].([]string)[0] = "mutated"
	measurement.Context[carrierSpanKey] = "999"

	fresh := memory.Measurements()[0]
	if !reflect.DeepEqual(fresh.Boundaries, []float64{1, 2}) {
		t.Fatalf("boundaries = %v, want [1 2]", fresh.Boundaries)
	}
	requireAttributes(t, fresh.Attributes, SpanAttributes{"tags": []string{"original"}})
	if !reflect.DeepEqual(fresh.Context, memory.Spans()[0].Context) {
		t.Fatalf("exemplar = %v, want %v", fresh.Context, memory.Spans()[0].Context)
	}
}

func TestNoopMetricsShareInstruments(t *testing.T) {
	counter := NoopMetrics.Counter("bytes", &InstrumentOptions{Unit: "By"})
	active := NoopMetrics.UpDownCounter("active", &InstrumentOptions{Unit: "By"})
	histogram := NoopMetrics.Histogram("duration", &HistogramOptions{
		InstrumentOptions: InstrumentOptions{Unit: "By"},
		Boundaries:        []float64{1},
	})
	if counter != NoopMetrics.Counter("other", nil) {
		t.Fatal("noop counters are not shared")
	}
	if active != NoopMetrics.UpDownCounter("other", nil) {
		t.Fatal("noop up/down counters are not shared")
	}
	if histogram != NoopMetrics.Histogram("other", nil) {
		t.Fatal("noop histograms are not shared")
	}
	counter.Add(1, SpanAttributes{"direction": "out"})
	active.Add(-1, SpanAttributes{"direction": "out"})
	histogram.Record(1, SpanAttributes{"direction": "out"}, NoopContext)
}
