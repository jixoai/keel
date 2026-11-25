package superagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
)

// FixtureSource is an in-memory Source + Channel used by tests and by a
// development deployment without GitHub credentials. It implements the same
// collab.Source/collab.Channel ports as the GitHub adapter, so the orchestrator
// path is identical.
type FixtureSource struct {
	mu       sync.Mutex
	now      func() time.Time
	events   []collab.SourceEvent
	tenants  []uuid.UUID
	messages []FixtureMessage
	// failSends makes the next n Send calls fail (retry tests).
	failSends int
	// failPolls makes the next n Poll calls fail (housekeeping tests).
	failPolls int
}

// FixtureMessage is one observed outbound message.
type FixtureMessage struct {
	TenantID  uuid.UUID
	ChannelID string
	Message   collab.ChannelMessage
	At        time.Time
}

// NewFixtureSource builds an empty source. A nil clock uses time.Now (UTC).
func NewFixtureSource(now func() time.Time) *FixtureSource {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &FixtureSource{now: now}
}

// Push appends one event for the tenant. The event's ReceivedAt defaults to the
// source clock and its payload is encoded from the canonical EventPayload when
// the caller supplies an Event.
func (f *FixtureSource) Push(tenantID uuid.UUID, event collab.SourceEvent) {
	if event.ReceivedAt.IsZero() {
		event.ReceivedAt = f.now()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
	f.tenants = append(f.tenants, tenantID)
}

// PushEvent is the typed convenience: it encodes the canonical payload and
// appends the event.
func (f *FixtureSource) PushEvent(tenantID uuid.UUID, event Event) error {
	if err := event.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(EventPayload{
		EventID: event.EventID,
		Kind:    event.Kind,
		Actor:   event.Actor,
		Thread:  event.Thread,
		Title:   event.Title,
		Body:    event.Body,
		URL:     event.URL,
	})
	if err != nil {
		return err
	}
	f.Push(tenantID, collab.SourceEvent{
		SourceID:   event.SourceID,
		Kind:       string(event.Kind),
		ExternalID: event.EventID,
		Payload:    payload,
		ReceivedAt: event.ReceivedAt,
	})
	return nil
}

// Poll returns the events after the opaque cursor (a decimal index), bounded by
// limit. A cursor from another source/tenant is treated as stale and starts at
// zero; the orchestrator's seen-event log makes the replay harmless.
func (f *FixtureSource) Poll(_ context.Context, tenantID uuid.UUID, cursor string, limit int) (collab.SourcePage, error) {
	f.mu.Lock()
	if f.failPolls > 0 {
		f.failPolls--
		f.mu.Unlock()
		return collab.SourcePage{}, fmt.Errorf("fixture: source is unavailable")
	}
	f.mu.Unlock()
	if limit <= 0 {
		limit = 50
	}
	start := 0
	if cursor != "" {
		parsed, err := strconv.Atoi(cursor)
		if err != nil || parsed < 0 {
			start = 0
		} else {
			start = parsed
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if start > len(f.events) {
		start = len(f.events)
	}
	end := start + limit
	if end > len(f.events) {
		end = len(f.events)
	}
	page := collab.SourcePage{NextCursor: strconv.Itoa(end)}
	for i := start; i < end; i++ {
		if f.tenants[i] != tenantID {
			continue
		}
		page.Events = append(page.Events, f.events[i])
	}
	return page, nil
}

// Send records one outbound message. The next failSends calls return an error
// without recording the message, which is the retry test's failure injection.
func (f *FixtureSource) Send(_ context.Context, tenantID uuid.UUID, channelID string, message collab.ChannelMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSends > 0 {
		f.failSends--
		return fmt.Errorf("fixture: channel is unavailable")
	}
	if tenantID == uuid.Nil {
		return fmt.Errorf("fixture: tenant is required")
	}
	if channelID == "" {
		return fmt.Errorf("fixture: channel id is required")
	}
	f.messages = append(f.messages, FixtureMessage{TenantID: tenantID, ChannelID: channelID, Message: message, At: f.now()})
	return nil
}

// FailPolls makes the next n Poll calls fail.
func (f *FixtureSource) FailPolls(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failPolls = n
}

// FailSends makes the next n Send calls fail.
func (f *FixtureSource) FailSends(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failSends = n
}

// Messages returns a copy of the observed messages.
func (f *FixtureSource) Messages() []FixtureMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FixtureMessage(nil), f.messages...)
}

// Events returns a copy of the queued source events (tests).
func (f *FixtureSource) Events() []collab.SourceEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]collab.SourceEvent(nil), f.events...)
}
