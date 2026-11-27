package boardintegration

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// sseHandler adds the two stream properties keel-board's Phase 0 server does
// not implement: a keepalive comment and a bounded connection lifetime that
// makes the client reconnect with Last-Event-ID. Both are transport concerns,
// not board semantics, so they stay in the keel adapter rather than in the
// board module's API.
type sseHandler struct {
	next      http.Handler
	heartbeat time.Duration
	lifetime  time.Duration
	logger    *slog.Logger
}

func (s *sseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || !isBoardEventsPath(r.URL.Path) {
		s.next.ServeHTTP(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.next.ServeHTTP(w, r)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	writer := newHeartbeatWriter(w, flusher)
	done := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		writer.pulse(ctx, s.heartbeat, done)
	}()
	var lifetime *time.Timer
	if s.lifetime > 0 {
		lifetime = time.AfterFunc(s.lifetime, cancel)
	}
	defer func() {
		if lifetime != nil {
			lifetime.Stop()
		}
		cancel()
		close(done)
		writer.close()
		workers.Wait()
	}()
	s.next.ServeHTTP(writer, r.WithContext(ctx))
}

func isBoardEventsPath(path string) bool {
	if len(path) > 1 && path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}
	return path == boardserverEventsPath
}

const boardserverEventsPath = "/v1/board/events"

// heartbeatWriter serializes the board handler's event writes and the
// keepalive goroutine on one ResponseWriter. net/http does not permit
// concurrent writes; a mutex is what makes the extra source safe.
type heartbeatWriter struct {
	http.ResponseWriter
	flusher http.Flusher
	started chan struct{}
	once    sync.Once

	mu        sync.Mutex
	closed    bool
	lastWrite time.Time
}

func newHeartbeatWriter(w http.ResponseWriter, flusher http.Flusher) *heartbeatWriter {
	return &heartbeatWriter{ResponseWriter: w, flusher: flusher, started: make(chan struct{})}
}

func (w *heartbeatWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, http.ErrHandlerTimeout
	}
	w.lastWrite = time.Now()
	return w.ResponseWriter.Write(p)
}

// Flush keeps the board handler's explicit flushing (it flushes each event).
func (w *heartbeatWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		w.flusher.Flush()
	}
}

// Unwrap lets http.NewResponseController reach the original writer.
func (w *heartbeatWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *heartbeatWriter) close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
}

// pulse writes a comment frame when the stream has been idle for one interval.
// It never starts before the board has written its own opening frame, so a
// client cannot observe a ping before the stream's content type is set.
func (w *heartbeatWriter) pulse(ctx context.Context, interval time.Duration, done <-chan struct{}) {
	if interval <= 0 {
		return
	}
	select {
	case <-w.started:
	case <-ctx.Done():
		return
	case <-done:
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			w.mu.Lock()
			if w.closed {
				w.mu.Unlock()
				return
			}
			if time.Since(w.lastWrite) < interval {
				w.mu.Unlock()
				continue
			}
			_, err := fmt.Fprint(w.ResponseWriter, ": ping\n\n")
			if err == nil {
				w.flusher.Flush()
				w.lastWrite = time.Now()
			}
			w.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}
