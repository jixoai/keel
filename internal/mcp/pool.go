package mcp

// Connection pool + health supervision (change feat-keel-pi-parity-closure-2).
//
// One MCP server is served by one live connection: the MCP client multiplexes
// concurrent tool calls by JSON-RPC id, so a second transport would add a
// subprocess (stdio) or session (HTTP) without adding throughput. The pool is
// therefore the lifecycle of that slot, not a fan-out:
//
//   - a borrow (CallToolWithOptions / HealthCheck) validates the slot is live;
//   - a failed health probe marks the slot unhealthy and triggers one bounded
//     reconnect, shared by concurrent borrowers;
//   - reconnects back off exponentially per server (1s, 2s, 4s, ..., 30s) so a
//     crashing server cannot be restarted in a tight loop;
//   - a successful reconnect resets the backoff and increments the pool's
//     reconnect counter (visible on /v1/mcp/servers and PoolStatus).
//
// The optional health monitor probes every interval and reconnects failed
// servers, so a crash between tool calls does not wait for the next borrow.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ErrServerUnavailable reports that a server's connection could not be
// restored; ErrReconnectBackoff reports that the next attempt is still in the
// future.
var (
	ErrServerUnavailable = errors.New("mcp: server unavailable")
	ErrReconnectBackoff  = errors.New("mcp: reconnect backoff")
)

// ReconnectBackoffError carries the wait remaining before the next attempt.
type ReconnectBackoffError struct {
	Server     string
	RetryAfter time.Duration
}

func (e *ReconnectBackoffError) Error() string {
	if e == nil {
		return "mcp: reconnect backoff"
	}
	return fmt.Sprintf("mcp: server %q reconnect backoff: retry in %s", e.Server, e.RetryAfter.Round(time.Millisecond))
}

func (e *ReconnectBackoffError) Unwrap() error { return ErrReconnectBackoff }

// Default pool timings. They are deliberately conservative: a healthy server
// pays one tools/list request per interval, and a crashed server is retried
// quickly at first and then at most every 30 seconds.
const (
	defaultHealthInterval = 30 * time.Second
	defaultBackoffInitial = time.Second
	defaultBackoffMax     = 30 * time.Second
)

// PoolStatus is the observable state of one server's connection slot.
type PoolStatus struct {
	Server          string
	Connected       bool
	Healthy         bool
	LastHealthCheck time.Time
	Failures        int
	NextRetry       time.Time
	Reconnects      int
	// CheckError is the last failed health probe (bounded, identifier-level).
	CheckError string
}

// serverSlot is one server's mutable health/reconnect state. It is guarded by
// its own mutex so a slow server never blocks another server's borrow.
type serverSlot struct {
	mu             sync.Mutex
	healthy        bool
	lastCheck      time.Time
	failures       int
	nextAttempt    time.Time
	reconnects     int
	lastCheckErr   string
	disconnected   bool
	attempting     bool
	attemptDone    chan struct{}
	backoffInitial time.Duration
	backoffMax     time.Duration
	healthInterval time.Duration
}

func newServerSlot(healthInterval time.Duration) *serverSlot {
	if healthInterval < 0 {
		healthInterval = 0
	}
	return &serverSlot{
		healthy:        true,
		lastCheck:      time.Now(),
		backoffInitial: defaultBackoffInitial,
		backoffMax:     defaultBackoffMax,
		healthInterval: healthInterval,
	}
}

// due reports whether a health probe is due at now.
func (s *serverSlot) due(now time.Time) bool {
	if s.healthInterval <= 0 {
		return false
	}
	return s.lastCheck.IsZero() || now.Sub(s.lastCheck) >= s.healthInterval
}

// canAttempt reports whether a reconnect may start at now.
func (s *serverSlot) canAttempt(now time.Time) (bool, time.Duration) {
	if s.nextAttempt.IsZero() || !now.Before(s.nextAttempt) {
		return true, 0
	}
	return false, s.nextAttempt.Sub(now)
}

// recordSuccess resets the backoff after a live probe or reconnect.
func (s *serverSlot) recordSuccess(reconnected bool, now time.Time) {
	s.healthy = true
	s.lastCheck = now
	s.failures = 0
	s.nextAttempt = time.Time{}
	s.lastCheckErr = ""
	s.disconnected = false
	if reconnected {
		s.reconnects++
	}
}

// recordFailure marks the slot unhealthy and schedules the next attempt.
func (s *serverSlot) recordFailure(err error, now time.Time) time.Duration {
	s.healthy = false
	s.disconnected = true
	s.failures++
	if err != nil {
		message := err.Error()
		if len(message) > 512 {
			message = message[:512] + "…"
		}
		s.lastCheckErr = message
	}
	delay := s.backoffInitial
	for i := 1; i < s.failures && delay < s.backoffMax; i++ {
		delay *= 2
	}
	if delay > s.backoffMax {
		delay = s.backoffMax
	}
	s.nextAttempt = now.Add(delay)
	return delay
}

// status snapshots the slot.
func (s *serverSlot) status(server string, connected bool) PoolStatus {
	return PoolStatus{
		Server:          server,
		Connected:       connected,
		Healthy:         s.healthy,
		LastHealthCheck: s.lastCheck,
		Failures:        s.failures,
		NextRetry:       s.nextAttempt,
		Reconnects:      s.reconnects,
		CheckError:      s.lastCheckErr,
	}
}

// slot returns the server's slot, creating one on first use (a manually built
// manager in tests still gets a coherent state).
func (m *Manager) slot(server string) *serverSlot {
	m.healthMu.Lock()
	defer m.healthMu.Unlock()
	if m.slots == nil {
		m.slots = make(map[string]*serverSlot)
	}
	slot, ok := m.slots[server]
	if !ok {
		interval := m.options.HealthCheckInterval
		if interval == 0 {
			interval = defaultHealthInterval
		}
		slot = newServerSlot(interval)
		slot.lastCheck = time.Now()
		m.slots[server] = slot
	}
	return slot
}

// HealthCheck probes one connected server with tools/list. It updates the
// pool state and returns the probe error; a failed probe also schedules the
// reconnect backoff.
func (m *Manager) HealthCheck(ctx context.Context, name string) error {
	if m == nil {
		return fmt.Errorf("%w: no manager", ErrServerUnavailable)
	}
	server := m.findServer(name)
	if server == nil {
		return fmt.Errorf("%w: %q", ErrServerNotFound, name)
	}
	slot := m.slot(name)
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if server.Err != nil || server.Client == nil {
		err := server.Err
		if err == nil {
			err = fmt.Errorf("mcp: server %q is not connected", name)
		}
		slot.recordFailure(err, time.Now())
		return fmt.Errorf("%w: %s", ErrServerUnavailable, name)
	}
	if _, err := server.Client.ListTools(ctx); err != nil {
		slot.recordFailure(err, time.Now())
		m.logger.Warn("mcp health check failed", "server", name, "err", err)
		return fmt.Errorf("%w: %s: %v", ErrServerUnavailable, name, err)
	}
	slot.recordSuccess(false, time.Now())
	return nil
}

// EnsureConnected validates a server's slot and, when the probe fails or the
// slot is already marked disconnected, performs at most one reconnect under
// the per-server backoff. Concurrent borrowers share one attempt: the first
// starts it, the rest wait for its outcome and re-check.
func (m *Manager) EnsureConnected(ctx context.Context, name string) error {
	if m == nil {
		return fmt.Errorf("%w: no manager", ErrServerUnavailable)
	}
	server := m.findServer(name)
	if server == nil {
		return fmt.Errorf("%w: %q", ErrServerNotFound, name)
	}
	slot := m.slot(name)

	for {
		slot.mu.Lock()
		// A live, recently checked connection is returned without I/O.
		if server.Err == nil && server.Client != nil && slot.healthy && !slot.due(time.Now()) {
			slot.mu.Unlock()
			return nil
		}
		if slot.attempting {
			done := slot.attemptDone
			slot.mu.Unlock()
			select {
			case <-done:
				// Another borrower finished an attempt; re-evaluate its result.
				server = m.findServer(name)
				if server == nil {
					return fmt.Errorf("%w: %q", ErrServerNotFound, name)
				}
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		// Probe first: a healthy server recovers without tearing the transport.
		if server.Err == nil && server.Client != nil {
			if _, err := server.Client.ListTools(ctx); err == nil {
				slot.recordSuccess(false, time.Now())
				slot.mu.Unlock()
				return nil
			} else {
				slot.recordFailure(err, time.Now())
				m.logger.Warn("mcp health check failed; reconnecting", "server", name, "err", err)
			}
		}
		if ok, wait := slot.canAttempt(time.Now()); !ok {
			slot.mu.Unlock()
			return &ReconnectBackoffError{Server: name, RetryAfter: wait}
		}
		slot.attempting = true
		slot.attemptDone = make(chan struct{})
		done := slot.attemptDone
		slot.mu.Unlock()

		reconnectErr := m.Reconnect(ctx, name)

		slot.mu.Lock()
		if reconnectErr != nil {
			delay := slot.recordFailure(reconnectErr, time.Now())
			m.logger.Warn("mcp reconnect failed", "server", name, "retry_in", delay, "err", reconnectErr)
		} else {
			slot.recordSuccess(true, time.Now())
		}
		slot.attempting = false
		close(done)
		slot.mu.Unlock()
		if reconnectErr != nil {
			return fmt.Errorf("%w: %s: %v", ErrServerUnavailable, name, reconnectErr)
		}
		return nil
	}
}

// StartHealthMonitor probes every connected server on an interval and
// reconnects failures (with backoff). It is idempotent; Close stops it.
func (m *Manager) StartHealthMonitor(ctx context.Context, interval time.Duration) {
	if m == nil {
		return
	}
	if interval <= 0 {
		interval = m.options.HealthCheckInterval
	}
	if interval <= 0 {
		interval = defaultHealthInterval
	}
	m.healthMu.Lock()
	if m.healthCancel != nil {
		m.healthMu.Unlock()
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	m.healthCancel, m.healthDone = cancel, done
	m.healthMu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				for _, server := range m.Servers() {
					name := server.Entry.Name
					probeCtx, probeCancel := context.WithTimeout(runCtx, interval)
					err := m.HealthCheck(probeCtx, name)
					probeCancel()
					if err != nil {
						reconnectCtx, reconnectCancel := context.WithTimeout(runCtx, interval)
						_ = m.EnsureConnected(reconnectCtx, name)
						reconnectCancel()
					}
				}
			}
		}
	}()
}

// stopHealthMonitor stops the background probe and waits for it to exit.
func (m *Manager) stopHealthMonitor() {
	if m == nil {
		return
	}
	m.healthMu.Lock()
	cancel, done := m.healthCancel, m.healthDone
	m.healthCancel, m.healthDone = nil, nil
	m.healthMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// PoolStatus reports every server's connection-slot state, sorted by name.
func (m *Manager) PoolStatus() []PoolStatus {
	if m == nil {
		return nil
	}
	servers := m.Servers()
	out := make([]PoolStatus, 0, len(servers))
	for _, server := range servers {
		slot := m.slot(server.Entry.Name)
		slot.mu.Lock()
		status := slot.status(server.Entry.Name, server.Err == nil && server.Client != nil)
		slot.mu.Unlock()
		out = append(out, status)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Server < out[j].Server })
	return out
}
