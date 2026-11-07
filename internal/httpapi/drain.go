// Graceful drain: SIGTERM stops admitting new work, tells every open SSE stream
// to reconnect (with a retry hint), and closes connections at a bounded rate so
// a release never looks like a network partition.

package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Drainer is the drain contract shared by the HTTP middleware and SSE handlers.
type Drainer interface {
	// Draining reports whether the process stopped admitting new requests.
	Draining() bool
	// Done is closed when draining starts.
	Done() <-chan struct{}
	// RetryAfterMS is the reconnect delay advertised to SSE clients.
	RetryAfterMS() int
	// WaitClose blocks until this connection's rate-limited close slot.
	WaitClose(ctx context.Context) error
}

// GracefulDrainer is the process-wide drain signal. Begin is idempotent.
type GracefulDrainer struct {
	draining     atomic.Bool
	done         chan struct{}
	once         sync.Once
	retryAfterMS int
	closeEvery   time.Duration

	mu        sync.Mutex
	nextClose time.Time
}

// NewDrainer builds a drainer. retryAfterMS is advertised to SSE clients;
// closePerSecond bounds how fast open streams are closed (1000 streams at 200/s
// finish in five seconds).
func NewDrainer(retryAfterMS, closePerSecond int) *GracefulDrainer {
	if retryAfterMS <= 0 {
		retryAfterMS = 3000
	}
	if closePerSecond <= 0 {
		closePerSecond = 200
	}
	return &GracefulDrainer{
		done:         make(chan struct{}),
		retryAfterMS: retryAfterMS,
		closeEvery:   time.Second / time.Duration(closePerSecond),
	}
}

// Begin marks the process as draining and wakes every SSE handler.
func (d *GracefulDrainer) Begin() {
	d.once.Do(func() {
		d.draining.Store(true)
		close(d.done)
	})
}

// Draining reports the current state.
func (d *GracefulDrainer) Draining() bool { return d.draining.Load() }

// Done is closed when draining starts.
func (d *GracefulDrainer) Done() <-chan struct{} { return d.done }

// RetryAfterMS is the reconnect delay advertised to SSE clients.
func (d *GracefulDrainer) RetryAfterMS() int { return d.retryAfterMS }

// WaitClose spreads connection closes over time. Each caller reserves the next
// slot; the mutex is held only for the reservation, never while waiting. The
// first close waits one full interval too, so a single stream is not cut before
// the orchestrator has had a chance to observe the draining state.
func (d *GracefulDrainer) WaitClose(ctx context.Context) error {
	d.mu.Lock()
	now := time.Now()
	if d.nextClose.Before(now.Add(d.closeEvery)) {
		d.nextClose = now.Add(d.closeEvery)
	}
	slot := d.nextClose
	d.nextClose = slot.Add(d.closeEvery)
	d.mu.Unlock()

	wait := time.Until(slot)
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// drainExempt reports paths that must stay reachable while draining: probes and
// metrics are how the orchestrator observes the drain itself.
func drainExempt(path string) bool {
	switch path {
	case "/v1/health", "/v1/ready", "/metrics":
		return true
	}
	return false
}

// rejectDraining writes the 503 every new API request receives during drain.
func (s *Server) rejectDraining(w http.ResponseWriter) {
	retry := s.cfg.Drain.RetryAfterMS() / 1000
	if retry < 1 {
		retry = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("server is draining"))
}

// drainStream flushes committed events the client has not seen yet, emits the
// drain frame, and waits for this connection's rate-limited close slot. The
// cursor is advanced only after a successful write, so a client that reconnects
// with Last-Event-ID resumes exactly after the last delivered event.
func (s *Server) drainStream(ctx context.Context, w http.ResponseWriter, flusher http.Flusher,
	rc *http.ResponseController, tenantID, sessionID uuid.UUID, after *int64) error {
	for {
		events, err := s.cfg.Store.LoadEvents(ctx, tenantID, sessionID, *after, 200)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.cfg.Logger.Warn("sse drain flush failed", "err", err)
			break
		}
		for _, ev := range events {
			if err := setWriteDeadline(rc, 10*time.Second); err != nil {
				return err
			}
			frame := fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Kind, ev.Payload)
			if _, err := w.Write([]byte(frame)); err != nil {
				return err
			}
			*after = ev.Seq
		}
		flusher.Flush()
		if len(events) < 200 {
			break
		}
	}

	if err := setWriteDeadline(rc, 10*time.Second); err != nil {
		return err
	}
	frame := fmt.Sprintf("event: drain\ndata: {\"retry_after_ms\":%d}\n\n", s.cfg.Drain.RetryAfterMS())
	if _, err := w.Write([]byte(frame)); err != nil {
		return err
	}
	flusher.Flush()
	return s.cfg.Drain.WaitClose(ctx)
}
