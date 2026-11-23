package httpapi

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jixoai/keel/internal/store"
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

// Notify listener backoff bounds. A database that is down must not turn the
// reconnect loop into a busy loop: each failed attempt logs once and waits
// (exponentially, with a cap) instead of hammering connect+log thousands of
// times per second.
const (
	notifyInitialBackoff = time.Second
	notifyMaxBackoff     = 30 * time.Second
)

// nextNotifyBackoff doubles a failed reconnect delay up to the cap.
func nextNotifyBackoff(current time.Duration) time.Duration {
	if current <= 0 {
		current = notifyInitialBackoff
	}
	next := current * 2
	if next > notifyMaxBackoff {
		return notifyMaxBackoff
	}
	return next
}

// Run listens for notifications until ctx is cancelled. It reconnects on error
// with a bounded, exponential backoff; callers should run it in its own
// goroutine. A listener that stayed up for a while starts over at the initial
// delay, so one early blip does not slow every later reconnect.
func (h *Hub) Run(ctx context.Context) {
	backoff := notifyInitialBackoff
	for ctx.Err() == nil {
		started := time.Now()
		err := h.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			continue
		}
		if time.Since(started) >= notifyMaxBackoff {
			backoff = notifyInitialBackoff
		}
		h.log.Warn("notify listener stopped, retrying", "err", err, "retryIn", backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = nextNotifyBackoff(backoff)
	}
}

func (h *Hub) listen(ctx context.Context) error {
	conn, err := h.store.Pool().Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN keel_events"); err != nil {
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
