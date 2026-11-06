// Package stream implements the network-facing backpressure primitives:
// bandwidth shaping, slow-client protection, size-class admission, bounded
// buffers, and the small metric surface that keeps the 2GB/1000-stream budget
// observable.
//
// The design is recorded in docs/BACKPRESSURE.md. The invariants that matter:
//   - the network is the only explicit upper bound;
//   - a slow client must not pin a goroutine or a PG connection;
//   - no unbounded queue, no sync.Pool under memory pressure.
package stream

import (
	"context"
	"sync"

	"golang.org/x/time/rate"
)

// MaxChunkWire is the base64 size of one 64KiB chunk: 4*ceil(65536/3) = 87384.
const MaxChunkWire = 87384

// Limiter is a global or per-tenant byte-per-second budget. WaitN is called with
// the on-the-wire (post-base64) size so the accounting matches the NIC.
type Limiter struct {
	l *rate.Limiter
}

// NewLimiter builds a budget of bytesPerSec with a burst of at least two chunks,
// because rate.WaitN rejects any single call larger than the burst.
func NewLimiter(bytesPerSec int64) *Limiter {
	if bytesPerSec <= 0 {
		return nil
	}
	burst := int(2 * MaxChunkWire)
	if burst < 4<<10 {
		burst = 4 << 10
	}
	return &Limiter{l: rate.NewLimiter(rate.Limit(bytesPerSec), burst)}
}

// WaitN blocks until n bytes may be sent.
func (l *Limiter) WaitN(ctx context.Context, n int) error {
	if l == nil {
		return nil
	}
	if n <= 0 {
		return nil
	}
	// A single write larger than the burst would be rejected forever; split it.
	for n > 0 {
		step := n
		if step > l.l.Burst() {
			step = l.l.Burst()
		}
		if err := l.l.WaitN(ctx, step); err != nil {
			return err
		}
		n -= step
	}
	return nil
}

// BytesPerSec reports the configured rate (0 = unlimited).
func (l *Limiter) BytesPerSec() int64 {
	if l == nil {
		return 0
	}
	return int64(l.l.Limit())
}

// BufferPool is a bounded freelist. Unlike sync.Pool it has a hard ceiling, so
// memory pressure cannot be made worse by a cache that empties exactly when GC
// runs, and a returned buffer can never grow the heap without bound.
type BufferPool struct {
	ch chan []byte
	sz int
}

// NewBufferPool allows at most capacity buffers of size bytes.
func NewBufferPool(size, capacity int) *BufferPool {
	if size <= 0 {
		size = MaxChunkWire
	}
	if capacity <= 0 {
		capacity = 256
	}
	return &BufferPool{ch: make(chan []byte, capacity), sz: size}
}

// Get returns a zero-length slice with the pool's capacity.
func (p *BufferPool) Get() []byte {
	select {
	case b := <-p.ch:
		return b[:0]
	default:
		return make([]byte, 0, p.sz)
	}
}

// Put returns a buffer if it has the expected capacity; otherwise it is dropped
// to the GC.
func (p *BufferPool) Put(b []byte) {
	if cap(b) != p.sz {
		return
	}
	select {
	case p.ch <- b[:cap(b)]:
	default:
	}
}

// InUse reports how many buffers are currently cached (for metrics).
func (p *BufferPool) InUse() int { return len(p.ch) }

// Class is a request size class.
type Class uint8

const (
	Small  Class = iota // <256KiB
	Medium              // <2MiB
	Large               // >=2MiB
)

func (c Class) String() string {
	switch c {
	case Small:
		return "small"
	case Medium:
		return "medium"
	case Large:
		return "large"
	}
	return "unknown"
}

// ClassOf maps a byte size to a class.
func ClassOf(size int64) Class {
	switch {
	case size < 256<<10:
		return Small
	case size < 2<<20:
		return Medium
	default:
		return Large
	}
}

// Scheduler is a work-conserving three-class admission controller with one
// protected floor. Bulk classes may borrow all free capacity, but they can never
// consume the headroom reserved for Large transfers, so a burst of small streams
// cannot starve a 10MB image. (A plain SJF queue starves; soft reservations for
// every idle class would instead block borrowing entirely.)
type Scheduler struct {
	mu         sync.Mutex
	cap        int
	cls        [3]int
	control    int
	inflight   int
	hard       [3]int
	largeFloor int
	rejected   int64
}

// NewScheduler builds a scheduler for total concurrent streams.
func NewScheduler(total int) *Scheduler {
	if total <= 0 {
		total = 1000
	}
	hard := [3]int{total * 90 / 100, total * 40 / 100, total * 10 / 100}
	for i := range hard {
		if hard[i] < 1 {
			hard[i] = 1
		}
	}
	floor := total * 5 / 100
	if floor < 1 {
		floor = 1
	}
	return &Scheduler{cap: total, hard: hard, largeFloor: floor}
}

// AcquireControl admits a long-lived control stream (SSE). It is bounded only by
// the total cap: SSE is the baseline load, not a size class competing with bulk
// transfers, and 1000 sessions must all be able to hold their stream.
func (s *Scheduler) AcquireControl() (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight >= s.cap {
		s.rejected++
		return nil, false
	}
	s.control++
	s.inflight++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.control--
			s.inflight--
			s.mu.Unlock()
		})
	}, true
}

// Acquire admits one bulk stream of class c or reports false (caller returns 429).
func (s *Scheduler) Acquire(c Class) (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c >= 3 {
		c = Small
	}
	if s.cls[c] >= s.hard[c] || s.inflight >= s.cap {
		s.rejected++
		return nil, false
	}
	if c != Large {
		reserved := 0
		if s.cls[Large] < s.largeFloor {
			reserved = s.largeFloor - s.cls[Large]
		}
		if s.inflight+1+reserved > s.cap {
			s.rejected++
			return nil, false
		}
	}
	s.cls[c]++
	s.inflight++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.cls[c]--
			s.inflight--
			s.mu.Unlock()
		})
	}, true
}

// Snapshot reports current occupancy.
func (s *Scheduler) Snapshot() (inflight, control int, classes [3]int, rejected int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight, s.control, s.cls, s.rejected
}

// Admitter is a token bucket for new connections, used to absorb reconnect
// storms. A client that is denied must receive 429/503 + Retry-After.
type Admitter struct {
	l *rate.Limiter
}

// NewAdmitter allows perSecond new streams with the given burst.
func NewAdmitter(perSecond int, burst int) *Admitter {
	if perSecond <= 0 {
		return nil
	}
	if burst < perSecond {
		burst = perSecond
	}
	return &Admitter{l: rate.NewLimiter(rate.Limit(perSecond), burst)}
}

// Allow reports whether a new connection may start now.
func (a *Admitter) Allow() bool {
	if a == nil {
		return true
	}
	return a.l.Allow()
}

// Delay reports how long a denied client should wait (used for Retry-After).
func (a *Admitter) Delay() int64 {
	if a == nil {
		return 0
	}
	r := a.l.Reserve()
	if !r.OK() {
		return 1000
	}
	d := r.Delay()
	r.Cancel()
	return d.Milliseconds()
}
