package servicegraph

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1700000000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// versioned returns the descriptor with a bumped implementation version.
func versioned(d ServiceDescriptor, version string) ServiceDescriptor {
	d.Version = version
	return d
}

func TestAcquireRequiresLoadout(t *testing.T) {
	e := newTestEngine()
	if _, err := e.Acquire(); !errors.Is(err, ErrNotAssembled) {
		t.Fatalf("Acquire before Apply: want ErrNotAssembled, got %v", err)
	}
	rec := &recorder{}
	mustApply(t, e, mustLoadout(t, "default", 1, desc("svc", instanceFactory(rec, "svc"), []string{"ctx.svc"}, nil, nil)))
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Acquire(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Acquire after Close: want ErrClosed, got %v", err)
	}
}

// TestRetiringKeepsInstancesForInFlightLease is the core RETIRING scenario:
// swapping the llm provider while a run leases generation 1 must not unload
// the old instances; they drain in reverse order when the lease is released.
func TestRetiringKeepsInstancesForInFlightLease(t *testing.T) {
	rec := &recorder{}
	lo1 := mustLoadout(t, "default", 1,
		desc("llm-a", instanceFactory(rec, "llm-a", "llm-client"), []string{"ctx.llm"}, nil, nil),
		desc("loop", instanceFactory(rec, "loop", "loop-listener"), []string{"ctx.loop"}, []string{"ctx.llm"}, nil),
	)
	lo2 := mustLoadout(t, "default", 2,
		desc("llm-b", instanceFactory(rec, "llm-b", "llm-client"), []string{"ctx.llm"}, nil, nil),
		desc("loop", instanceFactory(rec, "loop", "loop-listener"), []string{"ctx.loop"}, []string{"ctx.llm"}, nil),
	)
	e := newTestEngine()
	mustApply(t, e, lo1)
	oldLoop, ok := e.Lookup("loop")
	if !ok {
		t.Fatal("loop should be live")
	}
	oldLLM, ok := e.Lookup("llm-a")
	if !ok {
		t.Fatal("llm-a should be live")
	}

	lease, err := e.Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lease.Generation() != 1 || lease.Revision() != 1 || lease.LoadoutID() != "default" {
		t.Fatalf("lease header: gen=%d rev=%d id=%s", lease.Generation(), lease.Revision(), lease.LoadoutID())
	}

	// The reassembly must NOT unload the leased instances.
	snap := mustApply(t, e, lo2)
	if rec.contains("stop:loop") || rec.contains("stop:llm-a") {
		t.Fatalf("replaced instances must not unload while leased: %v", rec.list())
	}
	if rec.contains("release:loop/loop-listener") {
		t.Fatalf("leased effects must not be released: %v", rec.list())
	}
	if !snap.Active("loop") || !snap.Active("llm-b") {
		t.Fatalf("new generation should be ACTIVE:\n%s", snap)
	}
	if snap.State("llm-a") != StateRetiring {
		t.Fatalf("llm-a should be RETIRING, got %q", snap.State("llm-a"))
	}
	if len(snap.Retiring) != 2 {
		t.Fatalf("retiring rows: %+v", snap.Retiring)
	}
	for _, row := range snap.Retiring {
		if row.Generation != 1 || row.Pins != 1 {
			t.Fatalf("retiring row: %+v", row)
		}
	}
	if explain := snap.Explain("llm-a"); !strings.Contains(explain, string(StateRetiring)) {
		t.Fatalf("explain should describe the retiring instance:\n%s", explain)
	}

	// The in-flight run keeps resolving its generation through the lease.
	if got, ok := lease.Lookup("loop"); !ok || got != oldLoop {
		t.Fatal("lease must keep resolving the old loop instance")
	}
	if got, ok := lease.Lookup("llm-a"); !ok || got != oldLLM {
		t.Fatal("lease must keep resolving the old llm instance")
	}

	// A new run pins the new generation: RETIRING accepts no new runs.
	newLease, err := e.Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if newLease.Generation() != 2 {
		t.Fatalf("new runs must pin generation 2, got %d", newLease.Generation())
	}
	if _, ok := newLease.Lookup("llm-a"); ok {
		t.Fatal("the new generation must not expose the retired provider")
	}
	if _, ok := newLease.Lookup("llm-b"); !ok {
		t.Fatal("the new generation must expose the new provider")
	}

	// Draining: dependents unload before providers, effects in reverse order.
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	rec.mustBeBefore(t, "release:loop/loop-listener", "stop:loop")
	rec.mustBeBefore(t, "stop:loop", "stop:llm-a")
	if rec.count("stop:llm-a") != 1 || rec.count("stop:loop") != 1 {
		t.Fatalf("old instances must unload exactly once: %v", rec.list())
	}
	snap = e.Snapshot()
	if len(snap.Retiring) != 0 {
		t.Fatalf("retiring should be empty after drain: %+v", snap.Retiring)
	}
	if _, ok := lease.Lookup("loop"); ok {
		t.Fatal("a released lease must not resolve")
	}
	if !snap.Active("llm-b") || !snap.Active("loop") {
		t.Fatalf("the new generation must stay ACTIVE:\n%s", snap)
	}
	drained := false
	for _, d := range snap.Diagnostics {
		if d.Code == DiagDrained {
			drained = true
		}
	}
	if !drained {
		t.Fatalf("drain should be recorded in diagnostics: %+v", snap.Diagnostics)
	}
	if err := newLease.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRetiringDrainsOnlyWhenLastLeaseReleases(t *testing.T) {
	rec := &recorder{}
	lo1 := mustLoadout(t, "default", 1, desc("svc", instanceFactory(rec, "svc"), []string{"ctx.svc"}, nil, nil))
	lo2 := mustLoadout(t, "default", 2, versioned(desc("svc", instanceFactory(rec, "svc"), []string{"ctx.svc"}, nil, nil), "v2"))
	e := newTestEngine()
	mustApply(t, e, lo1)
	l1, err := e.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	l2, err := e.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	snap := mustApply(t, e, lo2)
	if len(snap.Retiring) != 1 || snap.Retiring[0].Pins != 2 {
		t.Fatalf("expected one retiring row with two pins: %+v", snap.Retiring)
	}
	if err := l1.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec.contains("stop:svc") {
		t.Fatalf("one reference remains; must not unload: %v", rec.list())
	}
	if err := l2.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec.count("stop:svc") != 1 {
		t.Fatalf("unload exactly once after the last release: %v", rec.list())
	}
	if len(e.Snapshot().Retiring) != 0 {
		t.Fatal("retiring should be empty")
	}
}

// TestLeaseSurvivesIntermediateGenerations covers an instance that is shared
// across generations and only torn down later: the leases taken earlier must
// keep it alive even though the intermediate generation did not replace it.
func TestLeaseSurvivesIntermediateGenerations(t *testing.T) {
	rec := &recorder{}
	lo1 := mustLoadout(t, "default", 1,
		desc("a", instanceFactory(rec, "a"), []string{"ctx.a"}, nil, nil),
		desc("b", instanceFactory(rec, "b"), []string{"ctx.b"}, []string{"ctx.a"}, nil),
	)
	e := newTestEngine()
	mustApply(t, e, lo1)
	l1, err := e.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	mustApply(t, e, lo1) // generation 2: identical, nothing reloaded
	l2, err := e.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	lo3 := mustLoadout(t, "default", 3,
		versioned(desc("a", instanceFactory(rec, "a"), []string{"ctx.a"}, nil, nil), "v2"),
		desc("b", instanceFactory(rec, "b"), []string{"ctx.b"}, []string{"ctx.a"}, nil),
	)
	snap := mustApply(t, e, lo3)
	if len(snap.Retiring) != 2 {
		t.Fatalf("a and b should be retiring: %+v", snap.Retiring)
	}
	for _, row := range snap.Retiring {
		if row.Pins != 2 {
			t.Fatalf("both leases pin the shared instances: %+v", row)
		}
	}
	if rec.contains("stop:a") || rec.contains("stop:b") {
		t.Fatalf("leased instances must not unload: %v", rec.list())
	}
	if err := l1.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec.contains("stop:a") || rec.contains("stop:b") {
		t.Fatalf("l2 still references the shared instances: %v", rec.list())
	}
	if err := l2.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.mustBeBefore(t, "stop:b", "stop:a")
	if rec.count("stop:a") != 1 || rec.count("stop:b") != 1 {
		t.Fatalf("each old instance unloads exactly once: %v", rec.list())
	}
}

func TestDrainDeadlineForcesUnload(t *testing.T) {
	rec := &recorder{}
	clock := newFakeClock()
	e := NewEngine(
		WithLogger(discardLogger()),
		WithRetireTimeout(30*time.Second),
		WithClock(clock.Now),
	)
	lo1 := mustLoadout(t, "default", 1, desc("svc", instanceFactory(rec, "svc", "listener"), []string{"ctx.svc"}, nil, nil))
	lo2 := mustLoadout(t, "default", 2, versioned(desc("svc", instanceFactory(rec, "svc", "listener"), []string{"ctx.svc"}, nil, nil), "v2"))
	mustApply(t, e, lo1)
	lease, err := e.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	snap := mustApply(t, e, lo2)
	if len(snap.Retiring) != 1 {
		t.Fatalf("retiring rows: %+v", snap.Retiring)
	}
	wantDeadline := clock.Now().Add(30 * time.Second)
	if !snap.Retiring[0].Deadline.Equal(wantDeadline) {
		t.Fatalf("deadline: want %s got %s", wantDeadline, snap.Retiring[0].Deadline)
	}

	clock.Advance(29 * time.Second)
	n, err := e.CollectExpired(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("before the deadline: n=%d err=%v", n, err)
	}
	if rec.contains("stop:svc") {
		t.Fatal("must not unload before the deadline")
	}

	clock.Advance(2 * time.Second)
	n, err = e.CollectExpired(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("after the deadline: n=%d err=%v", n, err)
	}
	if rec.count("stop:svc") != 1 || rec.count("release:svc/listener") != 1 {
		t.Fatalf("force unload should release and stop once: %v", rec.list())
	}
	if !lease.Broken() {
		t.Fatal("the outstanding lease must be invalidated")
	}
	if _, ok := lease.Lookup("svc"); ok {
		t.Fatal("a broken lease must not resolve instances")
	}
	found := false
	for _, d := range e.Snapshot().Diagnostics {
		if d.Code == DiagDrainExpired && strings.Contains(d.Message, "must fail") {
			found = true
		}
	}
	if !found {
		t.Fatalf("drain deadline must be diagnosable: %+v", e.Snapshot().Diagnostics)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("releasing a broken lease: %v", err)
	}
	if rec.count("stop:svc") != 1 {
		t.Fatalf("broken lease release must not double-unload: %v", rec.list())
	}
}

func TestCloseInvalidatesLeases(t *testing.T) {
	rec := &recorder{}
	e := newTestEngine()
	mustApply(t, e, mustLoadout(t, "default", 1, desc("svc", instanceFactory(rec, "svc"), []string{"ctx.svc"}, nil, nil)))
	lease, err := e.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !lease.Broken() {
		t.Fatal("close must invalidate outstanding leases")
	}
	if _, ok := lease.Lookup("svc"); ok {
		t.Fatal("closed engine leases must not resolve")
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("release after close: %v", err)
	}
	if rec.count("stop:svc") != 1 {
		t.Fatalf("stop exactly once: %v", rec.list())
	}
}

// TestConcurrentLeasesAndApply exercises Acquire/Release racing reapplies
// under -race: retiring generations must neither leak nor unload while
// referenced.
func TestConcurrentLeasesAndApply(t *testing.T) {
	rec := &recorder{}
	loA := mustLoadout(t, "default", 1,
		desc("a", instanceFactory(rec, "a"), []string{"ctx.a"}, nil, nil),
		desc("b", instanceFactory(rec, "b"), []string{"ctx.b"}, []string{"ctx.a"}, nil),
	)
	loB := mustLoadout(t, "default", 2,
		versioned(desc("a", instanceFactory(rec, "a"), []string{"ctx.a"}, nil, nil), "v2"),
		desc("b", instanceFactory(rec, "b"), []string{"ctx.b"}, []string{"ctx.a"}, nil),
	)
	e := NewEngine(WithLogger(discardLogger()), WithRetireTimeout(time.Hour))
	ctx := context.Background()
	mustApply(t, e, loA)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 15; j++ {
				lo := loA
				if (i+j)%2 == 1 {
					lo = loB
				}
				if _, err := e.Apply(ctx, lo); err != nil {
					t.Errorf("apply: %v", err)
					return
				}
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				lease, err := e.Acquire()
				if err != nil {
					t.Errorf("acquire: %v", err)
					return
				}
				if lease.Broken() {
					t.Error("an hour-long drain deadline must not break leases")
					return
				}
				if _, ok := lease.Lookup("a"); !ok {
					t.Error("lease must resolve a")
					return
				}
				if err := lease.Release(ctx); err != nil {
					t.Errorf("release: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	mustApply(t, e, loA)
	snap := e.Snapshot()
	if !snap.Active("a") || !snap.Active("b") {
		t.Fatalf("final generation should be ACTIVE:\n%s", snap)
	}
	if len(snap.Retiring) != 0 {
		t.Fatalf("all leases released; nothing may stay retiring: %+v", snap.Retiring)
	}
}
