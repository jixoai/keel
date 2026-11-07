package servicegraph

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestConcurrentApplyAndSnapshot exercises serialized reassembly racing with
// readers under -race: 4 appliers alternate two loadouts while 4 readers take
// snapshots and explain pending chains.
func TestConcurrentApplyAndSnapshot(t *testing.T) {
	rec := &recorder{}
	loA := mustLoadout(t, "default", 1,
		desc("a", instanceFactory(rec, "a", "a-effect"), []string{"ctx.a"}, nil, nil),
		desc("b", binderFactory(rec, "b", []string{"ctx.a"}), []string{"ctx.b"}, []string{"ctx.a"}, nil),
	)
	loB := mustLoadout(t, "default", 2,
		desc("a", instanceFactory(rec, "a", "a-effect"), []string{"ctx.a"}, nil, nil),
		desc("b", binderFactory(rec, "b", []string{"ctx.a"}), []string{"ctx.b"}, []string{"ctx.a"}, nil),
		desc("c", instanceFactory(rec, "c"), []string{"ctx.c"}, []string{"ctx.b"}, nil),
	)

	e := newTestEngine()
	mustApply(t, e, loA)

	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				lo := loA
				if (i+j)%2 == 1 {
					lo = loB
				}
				if _, err := e.Apply(ctx, lo); err != nil {
					t.Errorf("concurrent apply: %v", err)
					return
				}
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				snap := e.Snapshot()
				if snap.Generation == 0 {
					t.Error("snapshot with zero generation")
					return
				}
				_ = snap.Explain("b")
				_, _ = e.Lookup("a")
			}
		}()
	}
	wg.Wait()

	// A final deterministic apply proves the engine is still consistent.
	snap := mustApply(t, e, loB)
	for _, name := range []string{"a", "b", "c"} {
		if !snap.Active(name) {
			t.Fatalf("%s should be ACTIVE after the storm:\n%s", name, snap)
		}
	}
}

// TestConcurrentApplyWithFailures races failing and succeeding generations.
func TestConcurrentApplyWithFailures(t *testing.T) {
	rec := &recorder{}
	flaky := func(version string, fail bool) *Loadout {
		factory := instanceFactory(rec, "svc")
		if fail {
			factory = failingFactory(rec, "svc", errors.New("flaky"))
		}
		d := desc("svc", factory, []string{"ctx.svc"}, nil, nil)
		d.Version = version
		return mustLoadout(t, "default", 1, d)
	}
	loOK := flaky("v1", false)
	loBad := flaky("v2", true)

	e := newTestEngine()
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 15; j++ {
				lo := loOK
				if (i+j)%2 == 1 {
					lo = loBad
				}
				if _, err := e.Apply(ctx, lo); err != nil {
					var actErr *ActivationError
					if !errors.As(err, &actErr) {
						t.Errorf("unexpected error kind: %v", err)
						return
					}
				}
			}
		}(i)
	}
	wg.Wait()
	mustApply(t, e, loOK)
	if !e.Snapshot().Active("svc") {
		t.Fatalf("final state should be ACTIVE:\n%s", e.Snapshot())
	}
}

// TestConcurrentScopeRegisterAndRelease races effect registration against the
// release triggered by unload.
func TestConcurrentScopeRegisterAndRelease(t *testing.T) {
	scope := newEffectScope("svc", 1)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if err := scope.Register("concurrent", func() error { return nil }); err != nil {
					if errors.Is(err, ErrScopeClosed) {
						return
					}
					t.Errorf("register: %v", err)
					return
				}
			}
		}()
	}
	// Let the goroutines get going, then close the scope underneath them.
	for i := 0; i < 100; i++ {
		_ = scope.Effects()
	}
	_ = scope.closeAndRelease()
	wg.Wait()
	if err := scope.Register("after", func() error { return nil }); !errors.Is(err, ErrScopeClosed) {
		t.Fatalf("closed scope must refuse registration: %v", err)
	}
}

// TestConcurrentUnloadWhileServiceStarts races effect registration (from a
// goroutine the service keeps running) against the release triggered by
// unload, proving no effect survives its generation and no data race exists.
func TestConcurrentUnloadWhileServiceStarts(t *testing.T) {
	rec := &recorder{}
	inst := &spinningInstance{rec: rec, done: make(chan struct{})}
	lo := mustLoadout(t, "default", 1, desc("svc", fixedFactory(inst), []string{"ctx.svc"}, nil, nil))
	e := newTestEngine()
	mustApply(t, e, lo)

	if err := e.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-inst.done:
	case <-time.After(5 * time.Second):
		t.Fatal("effect registration goroutine did not stop after unload")
	}
	if !rec.contains("stopped-registering") {
		t.Fatalf("goroutine should have been refused by the closed scope: %v", rec.list())
	}
}

type spinningInstance struct {
	rec  *recorder
	done chan struct{}
}

// Start spawns a goroutine that registers effects until the scope closes;
// every registration races the unload path.
func (s *spinningInstance) Start(_ context.Context, effects *EffectScope) error {
	s.rec.add("start:spinning")
	go func() {
		defer close(s.done)
		for {
			if err := effects.Register("timer", func() error { return nil }); err != nil {
				s.rec.add("stopped-registering:%v", err)
				return
			}
		}
	}()
	return nil
}

func (s *spinningInstance) Stop(context.Context) error {
	s.rec.add("stop:spinning")
	return nil
}
