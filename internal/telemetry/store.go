// Ported from pi/packages/telemetry/src/memory.ts @ 55a5c6914570e2ae31ca36297654483bbbf46883
// (storedSpans/findTraces/getTrace/findMetrics).
//
// Deliberate difference: PI's memory adapter returns count-one metric rows
// (memory.ts:440-459). The Store contract's StoredMetric shape carries
// count/sum/min/max, so Keel aggregates measurements by interval, instrument,
// unit, and attribute set before returning them.

package telemetry

import (
	"encoding/json"
	"fmt"
	"sort"
)

const defaultTraceLimit = 20

type traceCursor struct {
	StartedMs float64 `json:"startedMs"`
	TraceID   string  `json:"traceId"`
}

// FindTraces returns settled traces, newest first. Filters have PI's semantics:
// name and start bounds apply to the earliest span; errors and attributes may
// match any span in the trace (memory.ts:399-437).
func (m *Memory) FindTraces(query TraceQuery) (TracePage, error) {
	var after *traceCursor
	if query.Cursor != "" {
		var cursor traceCursor
		if err := json.Unmarshal([]byte(query.Cursor), &cursor); err != nil {
			return TracePage{}, fmt.Errorf("telemetry: invalid trace cursor: %w", err)
		}
		after = &cursor
	}

	limit := query.Limit
	if limit <= 0 {
		limit = defaultTraceLimit
	}

	m.mu.Lock()
	spans := m.storedSpansLocked()
	m.mu.Unlock()

	grouped := make(map[string]*StoredTrace)
	for _, span := range spans {
		trace := grouped[span.TraceID]
		if trace == nil {
			trace = &StoredTrace{TraceID: span.TraceID, Spans: make([]StoredSpan, 0)}
			grouped[span.TraceID] = trace
		}
		trace.Spans = append(trace.Spans, span)
	}

	matches := make([]StoredTrace, 0, len(grouped))
	for _, trace := range grouped {
		if traceMatches(*trace, query, after) {
			matches = append(matches, *trace)
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		left, right := matches[i].Spans[0], matches[j].Spans[0]
		if left.StartedMs != right.StartedMs {
			return left.StartedMs > right.StartedMs
		}
		return left.TraceID < right.TraceID
	})

	if len(matches) <= limit {
		return TracePage{Traces: matches}, nil
	}
	last := matches[limit-1].Spans[0]
	cursor, err := json.Marshal(traceCursor{StartedMs: last.StartedMs, TraceID: last.TraceID})
	if err != nil {
		return TracePage{}, fmt.Errorf("telemetry: encode trace cursor: %w", err)
	}
	return TracePage{Traces: matches[:limit], Cursor: string(cursor)}, nil
}

func traceMatches(trace StoredTrace, query TraceQuery, after *traceCursor) bool {
	if len(trace.Spans) == 0 {
		return false
	}
	first := trace.Spans[0]
	if query.Name != "" && first.Name != query.Name {
		return false
	}
	if query.StartMin != nil && first.StartedMs < *query.StartMin {
		return false
	}
	if query.StartMax != nil && first.StartedMs > *query.StartMax {
		return false
	}
	if after != nil {
		continues := first.StartedMs < after.StartedMs ||
			(first.StartedMs == after.StartedMs && trace.TraceID > after.TraceID)
		if !continues {
			return false
		}
	}
	if query.Errors && !traceHasError(trace.Spans) {
		return false
	}
	if len(query.Attributes) > 0 && !traceHasAttributes(trace.Spans, query.Attributes) {
		return false
	}
	return true
}

func traceHasError(spans []StoredSpan) bool {
	for _, span := range spans {
		if span.Status.Status == SpanStatusError {
			return true
		}
	}
	return false
}

func traceHasAttributes(spans []StoredSpan, query SpanAttributes) bool {
	for _, span := range spans {
		if attributesMatch(span.Attributes, query) {
			return true
		}
	}
	return false
}

// GetTrace returns one settled trace, or nil when it has no settled spans.
func (m *Memory) GetTrace(traceID string) (*StoredTrace, error) {
	m.mu.Lock()
	spans := m.storedSpansLocked()
	m.mu.Unlock()

	trace := StoredTrace{TraceID: traceID, Spans: make([]StoredSpan, 0)}
	for _, span := range spans {
		if span.TraceID == traceID {
			trace.Spans = append(trace.Spans, span)
		}
	}
	if len(trace.Spans) == 0 {
		return nil, nil
	}
	return &trace, nil
}

type metricGroupKey struct {
	intervalMs int64
	name       string
	kind       MetricKind
	unit       string
	attributes string
}

type metricGroup struct {
	key    metricGroupKey
	metric StoredMetric
}

// FindMetrics returns aggregated measurement intervals matching the query
// (index.ts:74-80). Start bounds are inclusive on the interval start.
func (m *Memory) FindMetrics(query MetricQuery) ([]StoredMetric, error) {
	m.mu.Lock()
	measurements := make([]measurementRecord, len(m.measurements))
	copy(measurements, m.measurements)
	m.mu.Unlock()

	groups := make(map[metricGroupKey]*metricGroup)
	for _, record := range measurements {
		if record.name != query.Name {
			continue
		}
		if query.StartMin != nil && float64(record.intervalMs) < *query.StartMin {
			continue
		}
		if query.StartMax != nil && float64(record.intervalMs) > *query.StartMax {
			continue
		}
		if !attributesMatch(record.attributes, query.Attributes) {
			continue
		}
		key := metricGroupKey{
			intervalMs: record.intervalMs,
			name:       record.name,
			kind:       record.kind,
			unit:       record.unit,
			attributes: attributesKey(record.attributes),
		}
		group := groups[key]
		if group == nil {
			group = &metricGroup{
				key: key,
				metric: StoredMetric{
					IntervalMs: record.intervalMs,
					Name:       record.name,
					Kind:       record.kind,
					Unit:       record.unit,
					Attributes: copyAttributes(record.attributes),
					Min:        record.value,
					Max:        record.value,
				},
			}
			groups[key] = group
		}
		group.metric.Count++
		group.metric.Sum += record.value
		if record.value < group.metric.Min {
			group.metric.Min = record.value
		}
		if record.value > group.metric.Max {
			group.metric.Max = record.value
		}
	}

	ordered := make([]*metricGroup, 0, len(groups))
	for _, group := range groups {
		ordered = append(ordered, group)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return metricGroupKeyLess(ordered[i].key, ordered[j].key)
	})

	metrics := make([]StoredMetric, 0, len(ordered))
	for _, group := range ordered {
		metrics = append(metrics, group.metric)
	}
	return metrics, nil
}

func metricGroupKeyLess(left, right metricGroupKey) bool {
	if left.intervalMs != right.intervalMs {
		return left.intervalMs < right.intervalMs
	}
	if left.name != right.name {
		return left.name < right.name
	}
	if left.kind != right.kind {
		return left.kind < right.kind
	}
	if left.unit != right.unit {
		return left.unit < right.unit
	}
	return left.attributes < right.attributes
}

func (m *Memory) storedSpansLocked() []StoredSpan {
	spans := make([]StoredSpan, 0, len(m.spans))
	for _, record := range m.spans {
		if !record.settled {
			continue
		}
		events := make([]StoredEvent, 0, len(record.events))
		for _, event := range record.events {
			events = append(events, StoredEvent{
				Name:       event.name,
				TimeMs:     event.timeMs,
				Attributes: copyAttributes(event.attributes),
			})
		}
		links := make([]StoredLink, 0, len(record.links))
		for _, link := range record.links {
			links = append(links, StoredLink{
				TraceID:    link.Context[carrierTraceKey],
				SpanID:     carrierID(link.Context),
				Attributes: copyAttributes(link.Attributes),
			})
		}
		parentID := ""
		if record.parentContext != nil {
			parentID = carrierID(record.parentContext)
		}
		spans = append(spans, StoredSpan{
			ID:         carrierID(record.context),
			TraceID:    record.context[carrierTraceKey],
			ParentID:   parentID,
			Name:       record.name,
			StartedMs:  record.startedMs,
			EndedMs:    record.endedMs,
			Attributes: copyAttributes(record.attributes),
			Events:     events,
			Links:      links,
			Status:     copyStatus(record.status),
		})
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].StartedMs != spans[j].StartedMs {
			return spans[i].StartedMs < spans[j].StartedMs
		}
		return spans[i].ID < spans[j].ID
	})
	return spans
}
