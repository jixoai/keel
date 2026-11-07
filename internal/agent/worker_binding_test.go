package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

var errNoBinding = errors.New("no service generation available")

// mirrorStub is the run-row mirror seam for a memory-only worker test.
type mirrorStub struct{}

func (mirrorStub) GetRun(context.Context, uuid.UUID, uuid.UUID) (store.Run, error) {
	return store.Run{}, store.ErrNoRun
}

type bindingStub struct {
	runner   *Runner
	ref      LoadoutRef
	acquires atomic.Int32
	releases atomic.Int32
}

func (b *bindingStub) Acquire(context.Context) (BoundRun, error) {
	b.acquires.Add(1)
	return &boundStub{binding: b}, nil
}

type boundStub struct{ binding *bindingStub }

func (b *boundStub) Runner() *Runner     { return b.binding.runner }
func (b *boundStub) Loadout() LoadoutRef { return b.binding.ref }
func (b *boundStub) Release(context.Context) error {
	b.binding.releases.Add(1)
	return nil
}

// TestWorkerPinsLoadoutPerRun: the worker resolves the loop instance from the
// per-run binding (Runner itself is nil), releases the pin when the attempt
// settles, and records the pinned revision/generation with the run.
func TestWorkerPinsLoadoutPerRun(t *testing.T) {
	ctx := context.Background()
	mem := kernel.NewMemStore()
	k := kernel.New(mem, kernel.Options{})
	tenant, session := uuid.New(), uuid.New()
	mem.CreateSession(tenant, session)
	mem.EnqueueRun(tenant, session)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	runner := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	binding := &bindingStub{
		runner: runner,
		ref:    LoadoutRef{ID: "default", Revision: 7, Generation: 3},
	}
	worker := &Worker{
		Leases:      k,
		Runs:        mirrorStub{},
		Binding:     binding,
		Owner:       "worker-test",
		Lease:       30 * time.Second,
		MaxAttempts: 3,
		Logger:      discardLog(),
		// Runner is deliberately nil: the binding must provide it.
	}
	lease, err := k.ClaimScope(ctx, scope, "worker-test", time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	worker.execute(ctx, sem, lease)

	if binding.acquires.Load() != 1 || binding.releases.Load() != 1 {
		t.Fatalf("binding acquire/release: %d/%d", binding.acquires.Load(), binding.releases.Load())
	}
	pin, err := k.RunPin(ctx, scope)
	if err != nil {
		t.Fatalf("read run pin: %v", err)
	}
	if pin.LoadoutID != "default" || pin.LoadoutRevision != 7 || pin.LoadoutGeneration != 3 {
		t.Fatalf("recorded loadout: %+v", pin)
	}
	if pin.LoadoutOwnerEpoch != lease.Token.Epoch {
		t.Fatalf("recorded epoch %d, want %d", pin.LoadoutOwnerEpoch, lease.Token.Epoch)
	}
}

// TestWorkerBindingFailureSettlesAttempt: when no generation can be pinned the
// attempt is settled as failed instead of running with a half-bound worker, and
// the pin is not left dangling.
func TestWorkerBindingFailureSettlesAttempt(t *testing.T) {
	ctx := context.Background()
	mem := kernel.NewMemStore()
	k := kernel.New(mem, kernel.Options{})
	tenant, session := uuid.New(), uuid.New()
	mem.CreateSession(tenant, session)
	mem.EnqueueRun(tenant, session)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	worker := &Worker{
		Leases:      k,
		Runs:        mirrorStub{},
		Binding:     failingBinding{},
		Owner:       "worker-test",
		Lease:       30 * time.Second,
		MaxAttempts: 1,
		Logger:      discardLog(),
	}
	lease, err := k.ClaimScope(ctx, scope, "worker-test", time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	worker.execute(ctx, sem, lease)

	run, err := k.LeaseState(ctx, scope)
	if err != nil {
		t.Fatalf("lease state: %v", err)
	}
	if run.Live {
		t.Fatal("the attempt must be settled (lease cleared)")
	}
}

type failingBinding struct{}

func (failingBinding) Acquire(context.Context) (BoundRun, error) {
	return nil, errNoBinding
}
