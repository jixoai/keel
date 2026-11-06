package telemetry

import (
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"
)

// testClock replaces Memory.now so span times and metric intervals are
// deterministic, mirroring PI's vi.spyOn(Date/performance) in the store tests.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock(milliseconds float64) *testClock {
	return &testClock{now: time.Unix(0, 0).Add(time.Duration(milliseconds * float64(time.Millisecond)))}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Set(milliseconds float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = time.Unix(0, 0).Add(time.Duration(milliseconds * float64(time.Millisecond)))
}

func newTestMemory(clock *testClock) *Memory {
	memory := NewMemory()
	memory.now = clock.Now
	return memory
}

func spanByName(t *testing.T, spans []RecordedSpan, name string) RecordedSpan {
	t.Helper()
	for _, span := range spans {
		if span.Name == name {
			return span
		}
	}
	t.Fatalf("span %q not found in %v", name, spanNames(spans))
	return RecordedSpan{}
}

func spanNames(spans []RecordedSpan) []string {
	names := make([]string, len(spans))
	for index, span := range spans {
		names[index] = span.Name
	}
	return names
}

func mustSpan(t *testing.T, memory *Memory, name string) RecordedSpan {
	t.Helper()
	return spanByName(t, memory.Spans(), name)
}

func requireAttributes(t *testing.T, got, want SpanAttributes) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attributes = %#v, want %#v", got, want)
	}
}

func jsonRoundTrip(t *testing.T, carrier Carrier) Carrier {
	t.Helper()
	encoded, err := json.Marshal(carrier)
	if err != nil {
		t.Fatalf("marshal carrier: %v", err)
	}
	var decoded Carrier
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal carrier: %v", err)
	}
	return decoded
}

func findTraces(t *testing.T, memory *Memory, query TraceQuery) TracePage {
	t.Helper()
	page, err := memory.FindTraces(query)
	if err != nil {
		t.Fatalf("find traces: %v", err)
	}
	return page
}

func findMetrics(t *testing.T, memory *Memory, query MetricQuery) []StoredMetric {
	t.Helper()
	metrics, err := memory.FindMetrics(query)
	if err != nil {
		t.Fatalf("find metrics: %v", err)
	}
	return metrics
}

func traceFirstNames(page TracePage) []string {
	names := make([]string, 0, len(page.Traces))
	for _, trace := range page.Traces {
		if len(trace.Spans) > 0 {
			names = append(names, trace.Spans[0].Name)
		}
	}
	return names
}

func requireSpanStatus(t *testing.T, span RecordedSpan, kind SpanStatusKind) {
	t.Helper()
	if span.Status.Status != kind {
		t.Fatalf("%s status = %q, want %q (%+v)", span.Name, span.Status.Status, kind, span.Status)
	}
}

func requireParentID(t *testing.T, span RecordedSpan, want int64) {
	t.Helper()
	if span.ParentID == nil {
		t.Fatalf("%s parent ID = nil, want %d", span.Name, want)
	}
	if *span.ParentID != want {
		t.Fatalf("%s parent ID = %d, want %d", span.Name, *span.ParentID, want)
	}
}

func requireRoot(t *testing.T, span RecordedSpan) {
	t.Helper()
	if span.ParentID != nil {
		t.Fatalf("%s parent ID = %d, want nil", span.Name, *span.ParentID)
	}
	if span.ParentContext != nil {
		t.Fatalf("%s parent context = %v, want nil", span.Name, span.ParentContext)
	}
}

func requireEqual[T comparable](t *testing.T, label string, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

func requireFloat(t *testing.T, label string, got, want float64) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

func requireError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
