package stream

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSchedulerReservesCapacityForLarge(t *testing.T) {
	s := NewScheduler(100)
	// The large floor (5%) is protected: small can fill its 90% hard limit but
	// can never borrow the last five slots.
	var releases []func()
	for i := 0; i < 90; i++ {
		rel, ok := s.Acquire(Small)
		if !ok {
			t.Fatalf("small admission failed at %d", i)
		}
		releases = append(releases, rel)
	}
	if _, ok := s.Acquire(Small); ok {
		t.Fatal("small class exceeded its hard limit")
	}
	rel, ok := s.Acquire(Large)
	if !ok {
		t.Fatal("large starved while small was saturated")
	}
	releases = append(releases, rel)
	for _, rel := range releases {
		rel()
	}
	inflight, _, _, _ := s.Snapshot()
	if inflight != 0 {
		t.Fatalf("release leaked: inflight=%d", inflight)
	}
}

func TestSchedulerRejectsWhenTotalExhausted(t *testing.T) {
	s := NewScheduler(10)
	var releases []func()
	// Small can take 9 (leaving the 1-slot large floor), then large may use it.
	for i := 0; i < 9; i++ {
		rel, ok := s.Acquire(Small)
		if !ok {
			t.Fatalf("small admission failed at %d", i)
		}
		releases = append(releases, rel)
	}
	if _, ok := s.Acquire(Small); ok {
		t.Fatal("small borrowed the large floor")
	}
	rel, ok := s.Acquire(Large)
	if !ok {
		t.Fatal("large admission failed")
	}
	releases = append(releases, rel)
	if _, ok := s.Acquire(Medium); ok {
		t.Fatal("admitted beyond total capacity")
	}
	for _, rel := range releases {
		rel()
	}
}

func TestGuardKillsSlowClient(t *testing.T) {
	var mu sync.Mutex
	var wrote int
	w := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		wrote += len(p)
		mu.Unlock()
		time.Sleep(12 * time.Millisecond) // ~1.6KB/s
		return len(p), nil
	})
	g := NewGuardWriter(w, func(time.Time) error { return nil }, GuardConfig{
		Window:       10 * time.Millisecond,
		MinRate:      64 << 10,
		StallWindows: 2,
	})
	var err error
	for i := 0; i < 10; i++ {
		if _, err = g.Write(make([]byte, 20)); err != nil {
			break
		}
	}
	if !errors.Is(err, ErrSlowClient) {
		t.Fatalf("want ErrSlowClient, got %v", err)
	}
}

func TestGuardKeepsFastClient(t *testing.T) {
	w := writerFunc(func(p []byte) (int, error) { return len(p), nil })
	g := NewGuardWriter(w, func(time.Time) error { return nil }, GuardConfig{
		Window:       10 * time.Millisecond,
		MinRate:      1 << 20,
		// A generous stall count keeps this test about "fast writes are not
		// killed", not about scheduler pauses on a shared CI box.
		StallWindows: 1000,
	})
	for i := 0; i < 100; i++ {
		if _, err := g.Write(make([]byte, 4096)); err != nil {
			t.Fatalf("fast client killed at %d: %v", i, err)
		}
		time.Sleep(100 * time.Microsecond)
	}
}

func TestLimiterSplitsOversizedWrite(t *testing.T) {
	l := NewLimiter(10 << 20) // 10MB/s
	start := time.Now()
	if err := l.WaitN(t.Context(), 3*MaxChunkWire); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("limiter took too long: %v", elapsed)
	}
}

func TestBufferPoolBounded(t *testing.T) {
	p := NewBufferPool(1024, 2)
	a, b, c := p.Get(), p.Get(), p.Get()
	if cap(a) != 1024 || cap(b) != 1024 || cap(c) != 1024 {
		t.Fatalf("bad caps %d %d %d", cap(a), cap(b), cap(c))
	}
	p.Put(a)
	p.Put(b)
	p.Put(c) // dropped (channel full)
	if got := p.InUse(); got != 2 {
		t.Fatalf("pool exceeded its ceiling: %d", got)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
