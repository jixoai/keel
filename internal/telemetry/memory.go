// Ported from pi/packages/telemetry/src/memory.ts @ 55a5c6914570e2ae31ca36297654483bbbf46883.
// The in-process recorder keeps PI's span tree, event order, link/status
// detachment, callback lifecycle, and inert-after-settlement behavior. Go adds
// a mutex so concurrent child spans are safe.

package telemetry

import (
	"crypto/rand"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// RecordedEvent is a memory-adapter snapshot event. PI's getSpans() omits the
// relative time here (memory.ts:27-30, 485-488); StoredEvent carries it.
type RecordedEvent struct {
	Name       string
	Attributes SpanAttributes
}

// RecordedLink is a memory-adapter link snapshot (memory.ts:32-35).
type RecordedLink struct {
	Context    Carrier
	Attributes SpanAttributes
}

// RecordedSpan is a memory-adapter span snapshot. ParentID is nil for roots
// and for parents owned by another recorder (memory.ts:37-50).
type RecordedSpan struct {
	ID            int64
	ParentID      *int64
	Context       Carrier
	ParentContext Carrier
	Links         []RecordedLink
	Name          string
	Attributes    SpanAttributes
	Events        []RecordedEvent
	Status        SpanStatus
	Settled       bool
	EndSequence   *int64
}

type spanEventRecord struct {
	name       string
	timeMs     float64
	attributes SpanAttributes
}

type spanRecord struct {
	id             int64
	parentID       *int64
	context        Carrier
	parentContext  Carrier
	links          []RecordedLink
	name           string
	attributes     SpanAttributes
	events         []spanEventRecord
	status         SpanStatus
	explicitStatus bool
	settled        bool
	startedMs      float64
	startTime      time.Time
	endedMs        float64
	endSequence    *int64
}

// spanParent describes either a live local span or an extracted remote parent.
type spanParent struct {
	carrier Carrier
	localID *int64
	record  *spanRecord
}

// carrierProvider is the private bridge from a Context to its opaque carrier.
// PI keeps the same association in a WeakMap (memory.ts:128-129); noop and
// foreign contexts do not implement it, so their links are ignored.
type carrierProvider interface {
	telemetryCarrier() Carrier
}

// Memory is the backend-neutral reference adapter. It implements Context,
// Metrics, Propagation, and Store. Create one per test or recording scope.
type Memory struct {
	scope string
	now   func() time.Time

	mu              sync.Mutex
	spans           []*spanRecord
	nextSpanID      int64
	nextEndSequence int64
	measurements    []measurementRecord
}

var (
	_ Context     = (*Memory)(nil)
	_ Metrics     = (*Memory)(nil)
	_ Propagation = (*Memory)(nil)
	_ Store       = (*Memory)(nil)
)

// NewMemory creates an isolated in-process recorder.
func NewMemory() *Memory {
	return &Memory{
		scope:           newScopeID(),
		now:             time.Now,
		nextSpanID:      1,
		nextEndSequence: 1,
	}
}

// Metrics returns the adapter's metrics capability.
func (m *Memory) Metrics() Metrics { return m }

// Propagation returns the adapter's propagation capability.
func (m *Memory) Propagation() Propagation { return m }

// StartSpan starts a root span. The callback is admitted exactly once,
// synchronously; the span settles when the callback returns or panics.
func (m *Memory) StartSpan(options SpanOptions, callback func(Span) error) error {
	return m.startSpan(nil, options, callback)
}

func (m *Memory) startSpan(parent *spanParent, options SpanOptions, callback func(Span) error) (err error) {
	started := m.now()
	record, ok := m.createSpan(parent, options, started)
	if !ok {
		// A child of a settled span is admitted but not recorded (memory.ts:219).
		return callback(&inertSpan{memory: m})
	}
	span := &memorySpan{memory: m, record: record}
	defer func() {
		if recovered := recover(); recovered != nil {
			m.settle(record, nil, recovered)
			panic(recovered)
		}
		m.settle(record, err, nil)
	}()
	err = callback(span)
	return err
}

func (m *Memory) createSpan(parent *spanParent, options SpanOptions, started time.Time) (*spanRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if parent != nil && parent.record != nil && parent.record.settled {
		return nil, false
	}

	id := m.nextSpanID
	m.nextSpanID++

	context := Carrier{
		carrierScopeKey: m.scope,
		carrierSpanKey:  strconv.FormatInt(id, 10),
	}
	if parent != nil {
		context[carrierTraceKey] = parent.carrier[carrierTraceKey]
	} else {
		context[carrierTraceKey] = m.scope + ":" + strconv.FormatInt(id, 10)
	}

	var parentID *int64
	if parent != nil && parent.localID != nil {
		value := *parent.localID
		parentID = &value
	}
	var parentContext Carrier
	if parent != nil {
		parentContext = copyCarrier(parent.carrier)
	}

	links := make([]RecordedLink, 0, len(options.Links))
	for _, link := range options.Links {
		if recorded, ok := resolveLink(link); ok {
			links = append(links, recorded)
		}
	}

	record := &spanRecord{
		id:            id,
		parentID:      parentID,
		context:       context,
		parentContext: parentContext,
		links:         links,
		name:          options.Name,
		attributes:    copyAttributes(options.Attributes),
		events:        make([]spanEventRecord, 0),
		status:        OK(),
		startedMs:     milliseconds(started),
		startTime:     started,
	}
	m.spans = append(m.spans, record)
	return record, true
}

func (m *Memory) settle(record *spanRecord, callbackErr error, panicValue any) {
	ended := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if record.settled {
		return
	}
	record.settled = true
	record.endedMs = roundMicroseconds(record.startedMs + durationMilliseconds(ended.Sub(record.startTime)))
	sequence := m.nextEndSequence
	m.nextEndSequence++
	record.endSequence = &sequence
	if record.explicitStatus {
		return
	}
	if panicValue != nil {
		record.status = statusFromPanic(panicValue)
		return
	}
	if callbackErr != nil {
		record.status = ErrorStatusFromError(callbackErr)
	}
}

func statusFromPanic(value any) SpanStatus {
	if err, ok := value.(error); ok {
		return ErrorStatusFromError(err)
	}
	return SpanStatus{Status: SpanStatusError}
}

func resolveLink(link SpanLink) (RecordedLink, bool) {
	if link.Context == nil {
		return RecordedLink{}, false
	}
	provider, ok := link.Context.(carrierProvider)
	if !ok {
		return RecordedLink{}, false
	}
	carrier := provider.telemetryCarrier()
	if carrier == nil {
		return RecordedLink{}, false
	}
	return RecordedLink{
		Context:    copyCarrier(carrier),
		Attributes: copyAttributes(link.Attributes),
	}, true
}

type memorySpan struct {
	memory *Memory
	record *spanRecord
}

func (s *memorySpan) Metrics() Metrics         { return s.memory }
func (s *memorySpan) Propagation() Propagation { return s.memory }

func (s *memorySpan) StartSpan(options SpanOptions, callback func(Span) error) error {
	id := s.record.id
	return s.memory.startSpan(&spanParent{
		carrier: s.record.context,
		localID: &id,
		record:  s.record,
	}, options, callback)
}

func (s *memorySpan) AddLink(link SpanLink) {
	s.memory.mu.Lock()
	defer s.memory.mu.Unlock()
	if s.record.settled {
		return
	}
	if recorded, ok := resolveLink(link); ok {
		s.record.links = append(s.record.links, recorded)
	}
}

func (s *memorySpan) AddEvent(name string, attributes SpanAttributes) {
	s.memory.mu.Lock()
	defer s.memory.mu.Unlock()
	if s.record.settled {
		return
	}
	now := s.memory.now()
	s.record.events = append(s.record.events, spanEventRecord{
		name:       name,
		timeMs:     durationMilliseconds(now.Sub(s.record.startTime)),
		attributes: copyAttributes(attributes),
	})
}

func (s *memorySpan) SetAttributes(attributes SpanAttributes) {
	s.memory.mu.Lock()
	defer s.memory.mu.Unlock()
	if s.record.settled {
		return
	}
	mergeAttributes(s.record.attributes, attributes)
}

func (s *memorySpan) SetStatus(status SpanStatus) {
	s.memory.mu.Lock()
	defer s.memory.mu.Unlock()
	if s.record.settled {
		return
	}
	s.record.status = copyStatus(status)
	s.record.explicitStatus = true
}

func (s *memorySpan) telemetryCarrier() Carrier {
	return copyCarrier(s.record.context)
}

// inertSpan carries adapter capabilities for callbacks admitted after the
// parent settled, without recording anything (memory.ts:195-211).
type inertSpan struct {
	memory *Memory
}

func (s *inertSpan) Metrics() Metrics         { return s.memory }
func (s *inertSpan) Propagation() Propagation { return s.memory }
func (s *inertSpan) StartSpan(_ SpanOptions, callback func(Span) error) error {
	return callback(s)
}
func (s *inertSpan) AddLink(SpanLink)                {}
func (s *inertSpan) AddEvent(string, SpanAttributes) {}
func (s *inertSpan) SetAttributes(SpanAttributes)    {}
func (s *inertSpan) SetStatus(SpanStatus)            {}

// Spans returns detached span snapshots in span-start order (memory.ts:472-493).
func (m *Memory) Spans() []RecordedSpan {
	m.mu.Lock()
	defer m.mu.Unlock()
	spans := make([]RecordedSpan, 0, len(m.spans))
	for _, record := range m.spans {
		spans = append(spans, snapshotSpan(record))
	}
	return spans
}

func snapshotSpan(record *spanRecord) RecordedSpan {
	links := make([]RecordedLink, 0, len(record.links))
	for _, link := range record.links {
		links = append(links, RecordedLink{
			Context:    copyCarrier(link.Context),
			Attributes: copyAttributes(link.Attributes),
		})
	}
	events := make([]RecordedEvent, 0, len(record.events))
	for _, event := range record.events {
		events = append(events, RecordedEvent{
			Name:       event.name,
			Attributes: copyAttributes(event.attributes),
		})
	}
	var parentID *int64
	if record.parentID != nil {
		value := *record.parentID
		parentID = &value
	}
	var endSequence *int64
	if record.endSequence != nil {
		value := *record.endSequence
		endSequence = &value
	}
	return RecordedSpan{
		ID:            record.id,
		ParentID:      parentID,
		Context:       copyCarrier(record.context),
		ParentContext: copyCarrier(record.parentContext),
		Links:         links,
		Name:          record.name,
		Attributes:    copyAttributes(record.attributes),
		Events:        events,
		Status:        copyStatus(record.status),
		Settled:       record.settled,
		EndSequence:   endSequence,
	}
}

func newScopeID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(fmt.Sprintf("telemetry: crypto/rand failed: %v", err))
	}
	bytes[6] = bytes[6]&0x0f | 0x40
	bytes[8] = bytes[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}
