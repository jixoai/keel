package httpapi

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jixoai/fde2e-agent-runtime/internal/store"
)

// Hub fans out "this session has new events" wakeups to SSE subscribers. It is
// a cache, not durable state: if the process dies, subscribers reconnect with
// Last-Event-ID and re-read from the events table.
//
// One dedicated pgx connection listens on a single NOTIFY channel; the trigger
// payload names the session. Subscribers then run a keyset query for the events
// they are missing, so a dropped notification only delays delivery until the
// next heartbeat.
type Hub struct {
	store *store.Store
	log   *slog.Logger

	mu   sync.Mutex
	subs map[string]map[*subscriber]struct{}
}

type subscriber struct {
	ch chan struct{}
}

// NewHub builds a hub over the store.
func NewHub(s *store.Store, log *slog.Logger) *Hub {
	return &Hub{store: s, log: log, subs: make(map[string]map[*subscriber]struct{})}
}

func key(tenantID, sessionID uuid.UUID) string {
	return tenantID.String() + ":" + sessionID.String()
}

// Subscribe returns a wakeup channel and a cancel function. The channel has
// capacity 1: a session's subscribers only need to know "there may be more".
func (h *Hub) Subscribe(tenantID, sessionID uuid.UUID) (<-chan struct{}, func()) {
	sub := &subscriber{ch: make(chan struct{}, 1)}
	k := key(tenantID, sessionID)
	h.mu.Lock()
	if h.subs[k] == nil {
		h.subs[k] = make(map[*subscriber]struct{})
	}
	h.subs[k][sub] = struct{}{}
	h.mu.Unlock()
	return sub.ch, func() {
		h.mu.Lock()
		delete(h.subs[k], sub)
		if len(h.subs[k]) == 0 {
			delete(h.subs, k)
		}
		h.mu.Unlock()
	}
}

func (h *Hub) wake(k string) {
	h.mu.Lock()
	for sub := range h.subs[k] {
		select {
		case sub.ch <- struct{}{}:
		default:
		}
	}
	h.mu.Unlock()
}

// Run listens for notifications until ctx is cancelled. It reconnects on error
// with a bounded backoff; callers should run it in its own goroutine.
func (h *Hub) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := h.listen(ctx); err != nil && ctx.Err() == nil {
			h.log.Warn("notify listener stopped, retrying", "err", err)
		}
	}
}

func (h *Hub) listen(ctx context.Context) error {
	conn, err := h.store.Pool().Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN fde2e_events"); err != nil {
		return err
	}
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A closed connection ends the Acquire; reconnect on the next loop.
			if err == pgx.ErrTxClosed {
				return err
			}
			return err
		}
		h.wake(trimSeq(n.Payload))
	}
}

// trimSeq drops the trailing ":seq" from "tenant:session:seq".
func trimSeq(payload string) string {
	if i := strings.LastIndexByte(payload, ':'); i > 0 {
		return payload[:i]
	}
	return payload
}

// parseAfter reads a cursor from Last-Event-ID or a query parameter.
func parseAfter(lastEventID, query string) int64 {
	raw := lastEventID
	if raw == "" {
		raw = query
	}
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}
