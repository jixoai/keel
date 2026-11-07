// Worker abandonment: every path that gives up an attempt must return the
// claim (and the tenant's admission slot), not just drop the goroutine.

package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
)

// releaseRecorder records Release calls; the embedded nil LeaseManager panics
// on any other verb, which keeps the test focused on the abandon path.
type releaseRecorder struct {
	kernel.LeaseManager
	mu       sync.Mutex
	releases []kernel.Lease
}

func (r *releaseRecorder) Release(_ context.Context, lease kernel.Lease) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.releases = append(r.releases, lease)
	return nil
}

func (r *releaseRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.releases)
}

type cancelBinding struct{}

func (cancelBinding) Acquire(context.Context) (BoundRun, error) {
	return nil, context.Canceled
}

// TestWorkerAbandonReleasesClaimOnShutdown: a shutdown while binding services
// abandons the attempt; the lease must be released so its admission slot is
// returned and the next runtime can claim the run.
func TestWorkerAbandonReleasesClaimOnShutdown(t *testing.T) {
	recorder := &releaseRecorder{}
	worker := &Worker{
		Leases:      recorder,
		Binding:     cancelBinding{},
		Logger:      discardLog(),
		Lease:       time.Minute,
		MaxAttempts: 1,
	}
	lease := kernel.Lease{
		Scope: kernel.Scope{TenantID: uuid.New(), SessionID: uuid.New()},
		Token: kernel.OwnerToken{Owner: "runtime-a", Epoch: 1},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	worker.execute(ctx, sem, lease)
	if got := recorder.count(); got != 1 {
		t.Fatalf("release calls on shutdown abandon = %d, want 1", got)
	}
	if recorder.releases[0].Token != lease.Token {
		t.Fatalf("released token = %v, want %v", recorder.releases[0].Token, lease.Token)
	}
}

// fenceLeases loses the claim while pinning the loadout (a newer owner took
// over); the worker must release even though the token is stale.
type fenceLeases struct {
	*releaseRecorder
}

func (f *fenceLeases) PinRunLoadout(context.Context, kernel.Lease, string, int64) error {
	return kernel.ErrStaleEpoch
}

type abandonBound struct {
	released atomic.Int32
}

func (b *abandonBound) Runner() *Runner     { return nil }
func (b *abandonBound) Loadout() LoadoutRef { return LoadoutRef{ID: "default", Revision: 1} }
func (b *abandonBound) Release(context.Context) error {
	b.released.Add(1)
	return nil
}

type abandonBinding struct{ bound *abandonBound }

func (b abandonBinding) Acquire(context.Context) (BoundRun, error) { return b.bound, nil }

// TestWorkerAbandonReleasesClaimOnFenceLoss: the pin fence fires (the claim is
// gone). The kernel's Release is called (it returns the admission slot even
// when the fenced store write fails) and the loadout pin is released.
func TestWorkerAbandonReleasesClaimOnFenceLoss(t *testing.T) {
	recorder := &releaseRecorder{}
	leases := &fenceLeases{releaseRecorder: recorder}
	bound := &abandonBound{}
	worker := &Worker{
		Leases:      leases,
		Binding:     abandonBinding{bound: bound},
		Logger:      discardLog(),
		Lease:       time.Minute,
		MaxAttempts: 1,
	}
	lease := kernel.Lease{
		Scope: kernel.Scope{TenantID: uuid.New(), SessionID: uuid.New()},
		Token: kernel.OwnerToken{Owner: "runtime-a", Epoch: 1},
	}
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	worker.execute(context.Background(), sem, lease)
	if got := recorder.count(); got != 1 {
		t.Fatalf("release calls after pin fence loss = %d, want 1", got)
	}
	if got := bound.released.Load(); got != 1 {
		t.Fatalf("loadout pin releases = %d, want 1", got)
	}
}
