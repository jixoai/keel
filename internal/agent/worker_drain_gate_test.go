package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/kernel"
)

// countingLeases counts Claim calls; the underlying kernel has no runnable
// sessions, so every claim resolves ErrNoRunnable.
type countingLeases struct {
	kernel.LeaseManager
	claims atomic.Int32
}

func (c *countingLeases) Claim(ctx context.Context, owner string, ttl time.Duration) (kernel.Lease, error) {
	c.claims.Add(1)
	return kernel.Lease{}, kernel.ErrNoRunnable
}

type gateStub struct {
	claiming atomic.Bool
	started  atomic.Int32
	finished atomic.Int32
}

func (g *gateStub) Claiming() bool { return g.claiming.Load() }
func (g *gateStub) RunStarted()    { g.started.Add(1) }
func (g *gateStub) RunFinished()   { g.finished.Add(1) }

// TestWorkerStopsClaimingWhenGateClosed is the B1 drain gate: while the gate
// reports "not claiming", the worker must not call Claim once; when it opens,
// claiming resumes.
func TestWorkerStopsClaimingWhenGateClosed(t *testing.T) {
	k := kernel.New(kernel.NewMemStore(), kernel.Options{})
	leases := &countingLeases{LeaseManager: k}
	gate := &gateStub{}
	worker := &Worker{
		Leases:      leases,
		Runs:        mirrorStub{},
		Owner:       "gate-test",
		Concurrency: 2,
		Lease:       time.Minute,
		Poll:        5 * time.Millisecond,
		MaxAttempts: 1,
		Logger:      discardLog(),
		DrainGate:   gate,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Run(ctx)
	}()

	// Closed gate: several poll periods pass without a single claim.
	time.Sleep(60 * time.Millisecond)
	if got := leases.claims.Load(); got != 0 {
		cancel()
		<-done
		t.Fatalf("worker claimed %d runs while the drain gate was closed", got)
	}

	gate.claiming.Store(true)
	deadline := time.Now().Add(2 * time.Second)
	for leases.claims.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if leases.claims.Load() == 0 {
		cancel()
		<-done
		t.Fatal("worker did not resume claiming after the gate opened")
	}
	// Closing the gate again stops it within a few polls.
	before := leases.claims.Load()
	gate.claiming.Store(false)
	time.Sleep(40 * time.Millisecond)
	if got := leases.claims.Load(); got != before {
		cancel()
		<-done
		t.Fatalf("worker kept claiming with a closed gate: %d -> %d", before, got)
	}
	cancel()
	<-done
}
