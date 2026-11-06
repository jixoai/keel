package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// ErrSlowClient is returned when a client fails to drain at the minimum rate
// for long enough that continuing would only burn a goroutine and bandwidth.
var ErrSlowClient = errors.New("stream: slow client")

// GuardConfig tunes slow-client detection. The defaults encode the design in
// docs/BACKPRESSURE.md §3: a single write may block 4s, or longer when the write
// itself is larger than MinRate × 4s (so one 87KB chunk gets ~7s at the 12KB/s
// floor), and two consecutive 15s windows below MinRate condemn the stream.
type GuardConfig struct {
	Limiter      *Limiter      // nil = unlimited (SSE text must stay unlimited)
	MaxBlock     time.Duration // default 4s
	Window       time.Duration // default 15s
	MinRate      int64         // bytes/sec, default 12KB/s
	StallWindows int           // default 2
}

func (c *GuardConfig) applyDefaults() {
	if c.MaxBlock == 0 {
		c.MaxBlock = 4 * time.Second
	}
	if c.Window == 0 {
		c.Window = 15 * time.Second
	}
	if c.MinRate == 0 {
		c.MinRate = 12 << 10
	}
	if c.StallWindows == 0 {
		c.StallWindows = 2
	}
}

// Guard is an io.Writer for one HTTP response body. It shapes bandwidth through
// the limiter, bounds each write with a deadline, and detects a client that is
// receiving too slowly to be worth serving.
//
// Order matters: limiter → deadline → Write. Putting the deadline first would
// count queueing time as a stall and kill healthy streams.
type Guard struct {
	w        io.Writer
	deadline func(time.Time) error
	cfg      GuardConfig

	sent       int64 // total on-the-wire bytes for this stream
	windowSent int64
	windowAt   time.Time
	stalls     int
	lastErr    error
}

// NewGuard builds a guard around an HTTP response. The caller must have already
// written the response headers.
func NewGuard(w http.ResponseWriter, cfg GuardConfig) *Guard {
	rc := http.NewResponseController(w)
	return newGuard(w, rc.SetWriteDeadline, cfg)
}

// NewGuardWriter builds a guard around any writer with a deadline (tests and
// non-HTTP transports). A nil deadline disables the per-write bound.
func NewGuardWriter(w io.Writer, deadline func(time.Time) error, cfg GuardConfig) *Guard {
	return newGuard(w, deadline, cfg)
}

func newGuard(w io.Writer, deadline func(time.Time) error, cfg GuardConfig) *Guard {
	cfg.applyDefaults()
	return &Guard{w: w, deadline: deadline, cfg: cfg, windowAt: time.Now()}
}

// Write implements io.Writer.
func (g *Guard) Write(p []byte) (int, error) {
	if g.lastErr != nil {
		return 0, g.lastErr
	}
	if err := g.cfg.Limiter.WaitN(context.Background(), len(p)); err != nil {
		g.lastErr = err
		return 0, err
	}
	if g.deadline != nil {
		block := g.cfg.MaxBlock
		// A write may carry a whole chunk: allow it the time that chunk needs at
		// the minimum rate, or a 50KB/s client would be killed for the kernel
		// draining one 87KB chunk slower than 4s.
		if bySize := time.Duration(int64(len(p)) * int64(time.Second) / g.cfg.MinRate); bySize > block {
			block = bySize
		}
		if err := g.deadline(time.Now().Add(block)); err != nil {
			// A ResponseWriter that cannot set a deadline is a programming error
			// in the middleware chain (a wrapper without Unwrap); fail loudly.
			if errors.Is(err, http.ErrNotSupported) {
				g.lastErr = fmt.Errorf("%w: response writer does not support deadlines", err)
				return 0, g.lastErr
			}
			g.lastErr = err
			return 0, err
		}
	}
	n, err := g.w.Write(p)
	g.sent += int64(n)
	g.windowSent += int64(n)
	if err != nil {
		g.lastErr = err
		return n, err
	}
	if g.newWindow() {
		if g.windowSent < g.cfg.MinRate*int64(g.cfg.Window)/int64(time.Second) {
			g.stalls++
			if g.stalls >= g.cfg.StallWindows {
				g.lastErr = ErrSlowClient
				return n, ErrSlowClient
			}
		} else {
			g.stalls = 0
		}
	}
	return n, nil
}

// newWindow returns true when a measurement window just closed.
func (g *Guard) newWindow() bool {
	now := time.Now()
	if now.Sub(g.windowAt) < g.cfg.Window {
		return false
	}
	g.windowAt = now
	g.windowSent = 0
	return true
}

// Sent reports the number of bytes written to the transport so far. This is not
// proof of delivery: for HTTP the client reports its own resume offset
// (docs/BACKPRESSURE.md §3).
func (g *Guard) Sent() int64 { return atomic.LoadInt64(&g.sent) }

// Close clears the deadline so a pooled connection is not poisoned.
func (g *Guard) Close() {
	if g.deadline != nil {
		_ = g.deadline(time.Time{})
	}
}

// WriteCounter counts bytes for metrics without shaping (SSE).
type WriteCounter struct {
	W    io.Writer
	Sent atomic.Int64
}

func (c *WriteCounter) Write(p []byte) (int, error) {
	n, err := c.W.Write(p)
	c.Sent.Add(int64(n))
	return n, err
}

// Metrics is the always-on, fixed-cardinality metric surface. No per-stream
// labels: 1000 streams × N metrics would explode any scraper.
type Metrics struct {
	StreamsActive   atomic.Int64
	StreamsTotal    atomic.Int64
	SlowKills       atomic.Int64
	Resumes         atomic.Int64
	AdmissionReject atomic.Int64
	BytesSent       atomic.Int64
	ChunksFetched   atomic.Int64

	writeBlockNanos atomic.Int64
	writeSamples    atomic.Int64
}

// ObserveWrite records one write duration (sampled by the caller).
func (m *Metrics) ObserveWrite(d time.Duration) {
	m.writeBlockNanos.Add(int64(d))
	m.writeSamples.Add(1)
}

// Snapshot is a point-in-time metric read.
type Snapshot struct {
	StreamsActive   int64   `json:"streamsActive"`
	StreamsTotal    int64   `json:"streamsTotal"`
	SlowKills       int64   `json:"slowKills"`
	Resumes         int64   `json:"resumes"`
	AdmissionReject int64   `json:"admissionReject"`
	BytesSent       int64   `json:"bytesSent"`
	ChunksFetched   int64   `json:"chunksFetched"`
	AvgWriteBlockMS float64 `json:"avgWriteBlockMs"`
}

// Snapshot reads the counters.
func (m *Metrics) Snapshot() Snapshot {
	s := Snapshot{
		StreamsActive:   m.StreamsActive.Load(),
		StreamsTotal:    m.StreamsTotal.Load(),
		SlowKills:       m.SlowKills.Load(),
		Resumes:         m.Resumes.Load(),
		AdmissionReject: m.AdmissionReject.Load(),
		BytesSent:       m.BytesSent.Load(),
		ChunksFetched:   m.ChunksFetched.Load(),
	}
	if n := m.writeSamples.Load(); n > 0 {
		s.AvgWriteBlockMS = float64(m.writeBlockNanos.Load()) / float64(n) / 1e6
	}
	return s
}
