// Ported from pi/packages/telemetry/test/store.test.ts:1-128 and
// test/store-conformance.ts:19-225 @ 55a5c6914570e2ae31ca36297654483bbbf46883.

package telemetry

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestStoreReturnsOnlySettledDetachedTraces(t *testing.T) {
	clock := newTestClock(20_000)
	memory := newTestMemory(clock)
	started := make(chan struct{})
	gate := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- memory.StartSpan(SpanOptions{
			Name:       "root",
			Attributes: SpanAttributes{"tags": []string{"original"}},
		}, func(root Span) error {
			root.AddEvent("event", SpanAttributes{"tags": []string{"original"}})
			close(started)
			<-gate
			return root.StartSpan(SpanOptions{Name: "child"}, func(Span) error { return nil })
		})
	}()
	<-started

	if traces := findTraces(t, memory, TraceQuery{}).Traces; len(traces) != 0 {
		t.Fatalf("open trace leaked into store: %v", traces)
	}
	trace, err := memory.GetTrace("missing")
	requireError(t, err)
	if trace != nil {
		t.Fatalf("missing trace = %v, want nil", trace)
	}

	close(gate)
	requireError(t, <-done)

	page := findTraces(t, memory, TraceQuery{})
	if len(page.Traces) != 1 {
		t.Fatalf("traces = %d, want 1", len(page.Traces))
	}
	trace = &page.Traces[0]
	root := storedSpanByName(t, trace.Spans, "root")
	child := storedSpanByName(t, trace.Spans, "child")

	idPattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}:[1-9][0-9]*$`)
	if !idPattern.MatchString(root.ID) {
		t.Fatalf("root id = %q, want scope:local", root.ID)
	}
	requireEqual(t, "root trace ID", root.TraceID, root.ID)
	requireEqual(t, "root parent ID", root.ParentID, "")
	requireEqual(t, "child parent ID", child.ParentID, root.ID)
	requireEqual(t, "child trace ID", child.TraceID, root.TraceID)
	requireFloat(t, "root started", root.StartedMs, 20_000)
	if root.EndedMs < root.StartedMs {
		t.Fatalf("root ended %v before start %v", root.EndedMs, root.StartedMs)
	}
	if len(root.Events) != 1 || root.Events[0].TimeMs < 0 {
		t.Fatalf("root events = %v", root.Events)
	}

	root.Attributes["tags"] = []string{"changed"}
	root.Events[0].Attributes["tags"] = []string{"changed"}
	fresh, err := memory.GetTrace(trace.TraceID)
	requireError(t, err)
	if fresh == nil {
		t.Fatal("expected fresh trace")
	}
	freshRoot := storedSpanByName(t, fresh.Spans, "root")
	requireAttributes(t, freshRoot.Attributes, SpanAttributes{"tags": []string{"original"}})
	requireAttributes(t, freshRoot.Events[0].Attributes, SpanAttributes{"tags": []string{"original"}})
}

func TestStoreFiltersByTraceShape(t *testing.T) {
	clock := newTestClock(10_000)
	memory := newTestMemory(clock)

	requireError(t, memory.StartSpan(SpanOptions{Name: "old"}, func(root Span) error {
		clock.Set(30_000)
		if err := root.StartSpan(SpanOptions{
			Name:       "recent-child",
			Attributes: SpanAttributes{"left": 1},
		}, func(Span) error { return nil }); err != nil {
			return err
		}
		return root.StartSpan(SpanOptions{
			Name:       "split",
			Attributes: SpanAttributes{"right": 2},
		}, func(Span) error { return nil })
	}))

	clock.Set(20_000)
	requireError(t, memory.StartSpan(SpanOptions{Name: "wanted"}, func(root Span) error {
		clock.Set(20_001)
		return root.StartSpan(SpanOptions{
			Name: "failure",
			Attributes: SpanAttributes{
				"left":     1,
				"right":    2,
				"valid":    true,
				`odd."key`: "value",
			},
		}, func(child Span) error {
			child.SetStatus(ErrorStatus(nil))
			return nil
		})
	}))

	clock.Set(40_000)
	requireError(t, memory.StartSpan(SpanOptions{
		Name:       "new",
		Attributes: SpanAttributes{"valid": 1},
	}, func(Span) error { return nil }))

	names := func(query TraceQuery) []string {
		t.Helper()
		return traceFirstNames(findTraces(t, memory, query))
	}
	startMin := 20_000.0
	startMax := 20_000.0
	if got := names(TraceQuery{
		Name:       "wanted",
		Attributes: SpanAttributes{"left": 1, "right": 2},
		Errors:     true,
		StartMin:   &startMin,
		StartMax:   &startMax,
	}); !reflect.DeepEqual(got, []string{"wanted"}) {
		t.Fatalf("combined filter = %v, want [wanted]", got)
	}
	if got := names(TraceQuery{Name: "failure"}); len(got) != 0 {
		t.Fatalf("child name filter = %v, want none", got)
	}
	if got := names(TraceQuery{Attributes: SpanAttributes{"left": 1, "right": 2}}); !reflect.DeepEqual(got, []string{"wanted"}) {
		t.Fatalf("split attributes = %v, want [wanted]", got)
	}
	if got := names(TraceQuery{Attributes: SpanAttributes{"valid": true, `odd."key`: "value"}}); !reflect.DeepEqual(got, []string{"wanted"}) {
		t.Fatalf("exact attributes = %v, want [wanted]", got)
	}
	if got := names(TraceQuery{Attributes: SpanAttributes{"valid": 1}}); !reflect.DeepEqual(got, []string{"new"}) {
		t.Fatalf("typed attributes = %v, want [new]", got)
	}
	if got := names(TraceQuery{Errors: true}); !reflect.DeepEqual(got, []string{"wanted"}) {
		t.Fatalf("errors = %v, want [wanted]", got)
	}
	startFloor := 20_000.0
	if got := names(TraceQuery{StartMin: &startFloor}); !reflect.DeepEqual(got, []string{"new", "wanted"}) {
		t.Fatalf("start min = %v, want [new wanted]", got)
	}
	startCeiling := 10_000.0
	if got := names(TraceQuery{StartMax: &startCeiling}); !reflect.DeepEqual(got, []string{"old"}) {
		t.Fatalf("start max = %v, want [old]", got)
	}
}

func TestStorePagesTracesWithTiedStarts(t *testing.T) {
	clock := newTestClock(20_000)
	memory := newTestMemory(clock)
	for index := 0; index < 25; index++ {
		clock.Set(20_000)
		requireError(t, memory.StartSpan(SpanOptions{
			Name:       "root",
			Attributes: SpanAttributes{"index": index},
		}, func(root Span) error {
			clock.Set(20_001)
			return root.StartSpan(SpanOptions{Name: "child"}, func(Span) error { return nil })
		}))
	}

	first := findTraces(t, memory, TraceQuery{})
	requireEqual(t, "first page length", len(first.Traces), 20)
	if first.Cursor == "" {
		t.Fatal("expected first page cursor")
	}
	rest := findTraces(t, memory, TraceQuery{Cursor: first.Cursor})
	requireEqual(t, "second page length", len(rest.Traces), 5)
	if rest.Cursor != "" {
		t.Fatalf("second page cursor = %q, want empty", rest.Cursor)
	}
	all := findTraces(t, memory, TraceQuery{Limit: 100}).Traces
	combined := append(append([]StoredTrace{}, first.Traces...), rest.Traces...)
	if !reflect.DeepEqual(combined, all) {
		t.Fatalf("pages differ from full result")
	}
	for _, trace := range all {
		if len(trace.Spans) != 2 {
			t.Fatalf("trace %s has %d spans, want 2", trace.TraceID, len(trace.Spans))
		}
	}

	var ids []string
	cursor := ""
	for {
		page := findTraces(t, memory, TraceQuery{Name: "root", Limit: 3, Cursor: cursor})
		for _, trace := range page.Traces {
			ids = append(ids, trace.TraceID)
		}
		cursor = page.Cursor
		if cursor == "" {
			break
		}
	}
	allIDs := make([]string, 0, len(all))
	for _, trace := range all {
		allIDs = append(allIDs, trace.TraceID)
	}
	if !reflect.DeepEqual(ids, allIDs) {
		t.Fatalf("paged ids = %v, want %v", ids, allIDs)
	}
}

func TestStoreContinuesRemoteTracesAndLinks(t *testing.T) {
	clock := newTestClock(20_000)
	sender := newTestMemory(clock)
	receiver := newTestMemory(clock)

	var carrier Carrier
	requireError(t, sender.StartSpan(SpanOptions{Name: "sender"}, func(span Span) error {
		injected, ok := sender.Inject(span)
		if !ok {
			t.Fatal("expected sender carrier")
		}
		carrier = injected
		return nil
	}))
	sentPage := findTraces(t, sender, TraceQuery{})
	if len(sentPage.Traces) != 1 || len(sentPage.Traces[0].Spans) != 1 {
		t.Fatalf("sender traces = %v", sentPage.Traces)
	}
	sent := sentPage.Traces[0].Spans[0]

	remote := receiver.Extract(jsonRoundTrip(t, carrier))
	requireError(t, remote.StartSpan(SpanOptions{Name: "receiver"}, func(Span) error { return nil }))
	requireError(t, receiver.StartSpan(SpanOptions{
		Name:  "linked",
		Links: []SpanLink{{Context: remote, Attributes: SpanAttributes{"reason": "background"}}},
	}, func(Span) error { return nil }))

	continued, err := receiver.GetTrace(sent.TraceID)
	requireError(t, err)
	if continued == nil || len(continued.Spans) != 1 {
		t.Fatalf("continued trace = %v", continued)
	}
	resumed := continued.Spans[0]
	requireEqual(t, "continued trace ID", resumed.TraceID, sent.TraceID)
	requireEqual(t, "continued parent ID", resumed.ParentID, sent.ID)
	requireEqual(t, "continued name", resumed.Name, "receiver")
	if resumed.ID == sent.ID {
		t.Fatal("continued span reused the remote span ID")
	}

	linkedPage := findTraces(t, receiver, TraceQuery{Name: "linked"})
	if len(linkedPage.Traces) != 1 {
		t.Fatalf("linked traces = %v", linkedPage.Traces)
	}
	linkedTrace := linkedPage.Traces[0]
	if linkedTrace.TraceID == sent.TraceID {
		t.Fatal("linked root inherited the remote trace ID")
	}
	expectedLinks := []StoredLink{{
		TraceID:    sent.TraceID,
		SpanID:     sent.ID,
		Attributes: SpanAttributes{"reason": "background"},
	}}
	if !reflect.DeepEqual(linkedTrace.Spans[0].Links, expectedLinks) {
		t.Fatalf("linked links = %v, want %v", linkedTrace.Spans[0].Links, expectedLinks)
	}

	for key := range carrier {
		incomplete := copyCarrier(carrier)
		delete(incomplete, key)
		requireError(t, receiver.Extract(incomplete).StartSpan(
			SpanOptions{Name: "fallback"},
			func(Span) error { return nil },
		))
	}
	fallbacks := findTraces(t, receiver, TraceQuery{Name: "fallback"}).Traces
	requireEqual(t, "fallback trace count", len(fallbacks), len(carrier))
	for _, trace := range fallbacks {
		span := trace.Spans[0]
		requireEqual(t, "fallback parent ID", span.ParentID, "")
		requireEqual(t, "fallback root trace ID", trace.TraceID, span.ID)
	}
}

func TestStoreAcceptsExternalStringIdentities(t *testing.T) {
	memory := NewMemory()
	var carrier Carrier
	requireError(t, memory.StartSpan(SpanOptions{Name: "source"}, func(span Span) error {
		injected, ok := memory.Inject(span)
		if !ok {
			t.Fatal("expected carrier")
		}
		carrier = injected
		return nil
	}))

	external := make(Carrier, len(carrier))
	for key := range carrier {
		external[key] = "external-" + key
	}
	parent := memory.Extract(external)
	injected, ok := memory.Inject(parent)
	if !ok || !reflect.DeepEqual(injected, external) {
		t.Fatalf("reinjected parent = %v (ok=%v), want %v", injected, ok, external)
	}
	requireError(t, parent.StartSpan(SpanOptions{Name: "continued"}, func(Span) error { return nil }))

	page := findTraces(t, memory, TraceQuery{Name: "continued"})
	if len(page.Traces) != 1 {
		t.Fatalf("traces = %v", page.Traces)
	}
	trace := page.Traces[0]
	if !strings.HasPrefix(trace.TraceID, "external-") {
		t.Fatalf("trace ID = %q, want external prefix", trace.TraceID)
	}
	if !strings.HasPrefix(trace.Spans[0].ParentID, "external-") {
		t.Fatalf("parent ID = %q, want external prefix", trace.Spans[0].ParentID)
	}
}

func TestStoreQueriesMetricIntervals(t *testing.T) {
	clock := newTestClock(20_001)
	memory := newTestMemory(clock)
	counter := memory.Counter("hits", &InstrumentOptions{Unit: "By"})
	counter.Add(2, SpanAttributes{"method": "read", "valid": true})
	counter.Add(3, SpanAttributes{"valid": true, "method": "read"})
	counter.Add(11, SpanAttributes{"method": "write", "valid": 1})
	memory.Counter("other", nil).Add(100, SpanAttributes{"method": "read", "valid": true})
	clock.Set(30_001)
	counter.Add(7, SpanAttributes{"method": "read", "valid": true})
	clock.Set(40_001)

	startMin := 20_000.0
	startMax := 20_000.0
	query := MetricQuery{
		Name:       "hits",
		StartMin:   &startMin,
		StartMax:   &startMax,
		Attributes: SpanAttributes{"method": "read", "valid": true},
	}
	rows := findMetrics(t, memory, query)
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want one aggregated interval", rows)
	}
	row := rows[0]
	requireEqual(t, "count", row.Count, int64(2))
	requireFloat(t, "sum", row.Sum, 5)
	requireFloat(t, "min", row.Min, 2)
	requireFloat(t, "max", row.Max, 3)
	requireEqual(t, "kind", row.Kind, MetricKindCounter)
	requireEqual(t, "unit", row.Unit, "By")
	requireEqual(t, "interval", row.IntervalMs, int64(20_000))

	row.Attributes["method"] = "changed"
	if again := findMetrics(t, memory, query)[0]; again.Attributes["method"] != "read" {
		t.Fatalf("metric attributes not detached: %v", again.Attributes)
	}
	if missing := findMetrics(t, memory, MetricQuery{Name: "missing"}); len(missing) != 0 {
		t.Fatalf("missing metrics = %v, want none", missing)
	}
	typed := findMetrics(t, memory, MetricQuery{Name: "hits", Attributes: SpanAttributes{"valid": 1}})
	var typedSum float64
	for _, row := range typed {
		typedSum += row.Sum
	}
	requireFloat(t, "typed attribute sum", typedSum, 11)

	lateStart := 30_000.0
	late := findMetrics(t, memory, MetricQuery{Name: "hits", StartMin: &lateStart})
	var lateSum float64
	for _, row := range late {
		lateSum += row.Sum
	}
	requireFloat(t, "late sum", lateSum, 7)
}

func TestStoreRecordsWallClockAndRelativeEventTimes(t *testing.T) {
	clock := newTestClock(1_000)
	memory := newTestMemory(clock)
	var id string
	requireError(t, memory.StartSpan(SpanOptions{Name: "timed"}, func(span Span) error {
		carrier, ok := memory.Inject(span)
		if !ok {
			t.Fatal("expected carrier")
		}
		id = carrierID(carrier)
		clock.Set(1_010)
		span.AddEvent("event", SpanAttributes{"count": 1})
		clock.Set(1_025)
		return nil
	}))

	trace, err := memory.GetTrace(id)
	requireError(t, err)
	expected := StoredTrace{
		TraceID: id,
		Spans: []StoredSpan{{
			ID:         id,
			TraceID:    id,
			Name:       "timed",
			StartedMs:  1_000,
			EndedMs:    1_025,
			Attributes: SpanAttributes{},
			Events: []StoredEvent{{
				Name:       "event",
				TimeMs:     10,
				Attributes: SpanAttributes{"count": float64(1)},
			}},
			Links:  []StoredLink{},
			Status: OK(),
		}},
	}
	if !reflect.DeepEqual(trace, &expected) {
		t.Fatalf("trace = %#v, want %#v", trace, &expected)
	}
	requireEqual(t, "recorded event name", memory.Spans()[0].Events[0].Name, "event")
	requireAttributes(t, memory.Spans()[0].Events[0].Attributes, SpanAttributes{"count": float64(1)})
}

func TestStoreStampsSubMillisecondStartsInOrder(t *testing.T) {
	clock := newTestClock(1_000)
	memory := newTestMemory(clock)
	requireError(t, memory.StartSpan(SpanOptions{Name: "first"}, func(Span) error {
		clock.Set(1_000.25)
		return nil
	}))
	requireError(t, memory.StartSpan(SpanOptions{Name: "second"}, func(Span) error {
		clock.Set(1_000.5)
		return nil
	}))

	var spans []StoredSpan
	for _, trace := range findTraces(t, memory, TraceQuery{}).Traces {
		spans = append(spans, trace.Spans...)
	}
	got := make([][3]any, 0, len(spans))
	for _, span := range spans {
		got = append(got, [3]any{span.Name, span.StartedMs, span.EndedMs})
	}
	want := [][3]any{
		{"second", 1_000.25, 1_000.5},
		{"first", 1_000.0, 1_000.25},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("spans = %v, want %v", got, want)
	}
}

func TestStoreDetachesLinksAndErrorDetails(t *testing.T) {
	memory := NewMemory()
	var target Span
	requireError(t, memory.StartSpan(SpanOptions{Name: "target"}, func(span Span) error {
		target = span
		return nil
	}))
	requireError(t, memory.StartSpan(SpanOptions{
		Name:  "source",
		Links: []SpanLink{{Context: target, Attributes: SpanAttributes{"tags": []string{"original"}}}},
	}, func(span Span) error {
		span.SetStatus(ErrorStatus(&SpanError{Name: "Expected", Message: "original"}))
		return nil
	}))

	page := findTraces(t, memory, TraceQuery{Name: "source"})
	if len(page.Traces) != 1 {
		t.Fatalf("traces = %v", page.Traces)
	}
	trace := page.Traces[0]
	span := trace.Spans[0]
	span.Links[0].Attributes["tags"] = []string{"changed"}
	if span.Status.Error != nil {
		span.Status.Error.Message = "changed"
	}

	fresh, err := memory.GetTrace(trace.TraceID)
	requireError(t, err)
	freshSpan := fresh.Spans[0]
	requireAttributes(t, freshSpan.Links[0].Attributes, SpanAttributes{"tags": []string{"original"}})
	expected := ErrorStatus(&SpanError{Name: "Expected", Message: "original"})
	if !reflect.DeepEqual(freshSpan.Status, expected) {
		t.Fatalf("status = %+v, want %+v", freshSpan.Status, expected)
	}
}

func storedSpanByName(t *testing.T, spans []StoredSpan, name string) StoredSpan {
	t.Helper()
	for _, span := range spans {
		if span.Name == name {
			return span
		}
	}
	t.Fatalf("stored span %q not found", name)
	return StoredSpan{}
}
