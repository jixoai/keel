// Identity invalidation stream (change keel-harness-v2, identity realtime).
//
// The identity pages (accounts, permission groups, Bots, TeamBots and the
// session list's share state) must reflect a tenant-local change without a
// manual refresh. This file is the one seam between the mutating handlers and
// the browser: a small in-process hub publishes *invalidation facts* — never
// rows and never credentials — and GET /v1/identity/events is the SSE tail of
// one tenant's facts.
//
// The event vocabulary is deliberately coarse. A client does not learn what
// changed, only which projection is stale and must be re-read through its
// ordinary, authorized endpoint:
//
//	accounts     account rows (create/disable/enable/password reset)
//	permissions  permission groups, grants, memberships, roles/profiles
//	bots         Bot rows, Harness, archive state and keys
//	teams        TeamBot rows, Mission/Roster and archive state
//	sessions     session visibility/list (shares, archive, bot invocation)
//	plugins      plugin inventory/lifecycle (install, enable, disable, uninstall)
//
// Publishing happens after the business write (and its mandatory audit row)
// succeeded. A refused or failed write publishes nothing, so a 4xx/5xx never
// looks like a state change.
package harnessapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
)

// IdentityEventKind is one invalidation projection.
type IdentityEventKind string

const (
	IdentityEventAccounts    IdentityEventKind = "accounts"
	IdentityEventPermissions IdentityEventKind = "permissions"
	IdentityEventBots        IdentityEventKind = "bots"
	IdentityEventTeams       IdentityEventKind = "teams"
	IdentityEventSessions    IdentityEventKind = "sessions"
	IdentityEventPlugins     IdentityEventKind = "plugins"
)

// validIdentityEventKind keeps a programming mistake out of the wire: the
// vocabulary is closed, so a misspelled publish is dropped rather than taught
// to clients that do not know it.
func validIdentityEventKind(kind IdentityEventKind) bool {
	switch kind {
	case IdentityEventAccounts, IdentityEventPermissions, IdentityEventBots,
		IdentityEventTeams, IdentityEventSessions, IdentityEventPlugins:
		return true
	}
	return false
}

// PublishPluginChange tells the tenant's clients that the plugin inventory
// changed. The manager's change sink calls it after a lifecycle write; the web
// client re-reads GET /v1/plugins (never a row from the event).
func (s *Server) PublishPluginChange(tenantID uuid.UUID) {
	if s == nil || s.identityEvents == nil {
		return
	}
	s.identityEvents.Publish(tenantID, IdentityEventPlugins)
}

// IdentityEvent is one invalidation fact. It carries no ids, names, grants or
// credentials: an authenticated client re-reads whatever its own authority
// allows through the normal routes.
type IdentityEvent struct {
	Kind IdentityEventKind `json:"kind"`
	At   time.Time         `json:"at"`
}

const (
	// identityEventMaxSubscribers bounds the whole deployment. Each SSE
	// connection is one goroutine plus a small buffered channel.
	identityEventMaxSubscribers = 256
	// identityEventMaxPerTenant keeps one tenant's browser fleet from starving
	// another tenant's.
	identityEventMaxPerTenant = 32
	// identityEventSubscriberBuffer bounds a slow client's backlog. When it is
	// full the oldest invalidation is dropped: a missed refetch is recoverable
	// (the next event or the reader's own action re-reads), unbounded memory is
	// not.
	identityEventSubscriberBuffer = 64
	// identityEventHeartbeatDefault keeps the connection through proxies with a
	// 75s idle timeout.
	identityEventHeartbeatDefault = 15 * time.Second
)

// ErrIdentityEventStreamFull reports that the deployment (or the tenant) hit
// its subscriber budget. It is a 503 at the edge, never a silent no-stream.
var ErrIdentityEventStreamFull = errors.New("identity event stream: subscriber limit reached")

// IdentityEvents is the tenant-scoped invalidation hub.
type IdentityEvents struct {
	mu        sync.Mutex
	nextID    uint64
	subs      map[uint64]*IdentitySubscription
	maxSubs   int
	maxTenant int
}

// NewIdentityEvents builds a hub with the deployment defaults.
func NewIdentityEvents() *IdentityEvents {
	return &IdentityEvents{
		subs:      make(map[uint64]*IdentitySubscription),
		maxSubs:   identityEventMaxSubscribers,
		maxTenant: identityEventMaxPerTenant,
	}
}

// Subscribe registers one SSE client of a tenant.
func (h *IdentityEvents) Subscribe(tenantID uuid.UUID) (*IdentitySubscription, error) {
	if h == nil || tenantID == uuid.Nil {
		return nil, errors.New("identity event stream: a tenant is required")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	maxSubs := h.maxSubs
	if maxSubs <= 0 {
		maxSubs = identityEventMaxSubscribers
	}
	maxTenant := h.maxTenant
	if maxTenant <= 0 {
		maxTenant = identityEventMaxPerTenant
	}
	if h.subs == nil {
		h.subs = make(map[uint64]*IdentitySubscription)
	}
	if len(h.subs) >= maxSubs {
		return nil, fmt.Errorf("%w: %d subscribers", ErrIdentityEventStreamFull, len(h.subs))
	}
	tenantCount := 0
	for _, sub := range h.subs {
		if sub.tenantID == tenantID {
			tenantCount++
		}
	}
	if tenantCount >= maxTenant {
		return nil, fmt.Errorf("%w: tenant has %d subscribers", ErrIdentityEventStreamFull, tenantCount)
	}
	h.nextID++
	sub := &IdentitySubscription{
		hub:      h,
		id:       h.nextID,
		tenantID: tenantID,
		events:   make(chan IdentityEvent, identityEventSubscriberBuffer),
		done:     make(chan struct{}),
	}
	h.subs[sub.id] = sub
	return sub, nil
}

// Publish appends one invalidation fact to every subscriber of the tenant.
// The call never blocks on a slow client.
func (h *IdentityEvents) Publish(tenantID uuid.UUID, kind IdentityEventKind) {
	if h == nil || tenantID == uuid.Nil || !validIdentityEventKind(kind) {
		return
	}
	event := IdentityEvent{Kind: kind, At: time.Now().UTC()}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, sub := range h.subs {
		if sub.tenantID != tenantID {
			continue
		}
		select {
		case sub.events <- event:
		default:
			// The client is behind. Drop the oldest fact and enqueue the newest:
			// the backlog stays bounded, and the next event (or the reader's own
			// action) re-reads the projection anyway.
			select {
			case <-sub.events:
			default:
			}
			select {
			case sub.events <- event:
			default:
			}
		}
	}
}

// Subscribers reports the live subscription count (tests and diagnostics).
func (h *IdentityEvents) Subscribers() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// TenantSubscribers reports one tenant's live subscription count.
func (h *IdentityEvents) TenantSubscribers(tenantID uuid.UUID) int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for _, sub := range h.subs {
		if sub.tenantID == tenantID {
			count++
		}
	}
	return count
}

// IdentitySubscription is one live SSE client. Close is idempotent.
type IdentitySubscription struct {
	hub      *IdentityEvents
	id       uint64
	tenantID uuid.UUID
	events   chan IdentityEvent
	done     chan struct{}
	once     sync.Once
}

// Events is the bounded invalidation queue.
func (s *IdentitySubscription) Events() <-chan IdentityEvent { return s.events }

// Done is closed when the subscription is removed.
func (s *IdentitySubscription) Done() <-chan struct{} { return s.done }

// TenantID names the tenant this subscription belongs to.
func (s *IdentitySubscription) TenantID() uuid.UUID { return s.tenantID }

// Close removes the subscription and wakes a reader waiting on Done.
func (s *IdentitySubscription) Close() {
	if s == nil || s.hub == nil {
		return
	}
	s.once.Do(func() {
		s.hub.mu.Lock()
		delete(s.hub.subs, s.id)
		close(s.done)
		s.hub.mu.Unlock()
	})
}

// identityEventStream serves GET /v1/identity/events: one tenant's invalidation
// facts as Server-Sent Events. The route is authenticated and authorized with
// the existing tenant-level app.read key (the same discovery read a client
// already holds); the hub scopes every event to the caller's tenant.
func (s *Server) identityEventStream(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	_, tenantID, ok := s.requireKey(w, r, access.KeyAppRead)
	if !ok {
		return
	}
	s.serveIdentityEvents(w, r, tenantID)
}

func (s *Server) serveIdentityEvents(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) {
	if s.identityEvents == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("the identity event stream is not wired in this deployment"))
		return
	}
	subscription, err := s.identityEvents.Subscribe(tenantID)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	defer subscription.Close()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// Stop an intermediate proxy from buffering the stream into uselessness.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher := http.NewResponseController(w)
	// A reconnect hint the browser's EventSource honours; the facts themselves
	// are not replayable, so a reconnected client simply re-reads on the next
	// fact it receives.
	if _, err := fmt.Fprint(w, "retry: 2000\n\n: connected\n\n"); err != nil {
		return
	}
	if err := flusher.Flush(); err != nil {
		return
	}
	heartbeat := s.cfg.IdentityEventsHeartbeat
	if heartbeat <= 0 {
		heartbeat = identityEventHeartbeatDefault
	}
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-subscription.Done():
			return
		case <-ticker.C:
			if _, err := fmt.Fprintf(w, ": heartbeat %s\n\n", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return
			}
			if err := flusher.Flush(); err != nil {
				return
			}
		case event := <-subscription.Events():
			payload, err := json.Marshal(event)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: identity\ndata: %s\n\n", payload); err != nil {
				return
			}
			if err := flusher.Flush(); err != nil {
				return
			}
		}
	}
}

// identityChanged publishes after a successful identity write (and its audit).
// A handler that returns before this call publishes nothing.
func (s *Server) identityChanged(tenantID uuid.UUID, kinds ...IdentityEventKind) {
	if s == nil || s.identityEvents == nil {
		return
	}
	for _, kind := range kinds {
		s.identityEvents.Publish(tenantID, kind)
	}
}
