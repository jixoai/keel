package handoff

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/kernel"
)

// failingRegistry wraps a Registry and fails the configured calls: the
// deterministic stand-in for an unavailable persistence layer.
type failingRegistry struct {
	Registry
	mu sync.Mutex
	// failSaveHandoff / failFinishHandoff / failSaveRuntime count how many
	// upcoming calls fail (0 = none).
	failSaveHandoff   int
	failFinishHandoff int
	failSaveRuntime   int

	saveHandoffCalls int
	finishCalls      int
}

func (f *failingRegistry) SaveHandoff(ctx context.Context, rec HandoffRecord) error {
	f.mu.Lock()
	f.saveHandoffCalls++
	fail := f.failSaveHandoff > 0
	if fail {
		f.failSaveHandoff--
	}
	f.mu.Unlock()
	if fail {
		return errors.New("injected save-handoff failure")
	}
	return f.Registry.SaveHandoff(ctx, rec)
}

func (f *failingRegistry) FinishHandoff(ctx context.Context, id, status string, inFlight int, detail string, at time.Time) error {
	f.mu.Lock()
	f.finishCalls++
	fail := f.failFinishHandoff > 0
	if fail {
		f.failFinishHandoff--
	}
	f.mu.Unlock()
	if fail {
		return errors.New("injected finish-handoff failure")
	}
	return f.Registry.FinishHandoff(ctx, id, status, inFlight, detail, at)
}

func (f *failingRegistry) SaveRuntime(ctx context.Context, rec RuntimeRecord) error {
	f.mu.Lock()
	fail := f.failSaveRuntime > 0
	if fail {
		f.failSaveRuntime--
	}
	f.mu.Unlock()
	if fail {
		return errors.New("injected save-runtime failure")
	}
	return f.Registry.SaveRuntime(ctx, rec)
}

func (f *failingRegistry) counts() (saves, finishes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.saveHandoffCalls, f.finishCalls
}

// newCoordinator builds a coordinator over an explicit registry and records
// how often the drain hook fired.
func newCoordinator(t *testing.T, registry Registry, runtimeID string, drains *int32) *Coordinator {
	t.Helper()
	var mu sync.Mutex
	coord, err := New(Config{
		Identity: Identity{ID: runtimeID, LoadoutID: "keel-default", Revision: 1},
		Registry: registry,
		LeaseTTL: time.Minute,
		Logger:   discardLogger(),
		OnDrain: func(mode Mode, deadline time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			if drains != nil {
				*drains++
			}
		},
	})
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	return coord
}

// TestConcurrentBeginDrainSingleWinner is the M1 regression: two triggers
// (admin POST × SIGTERM) racing on BeginDrain must produce exactly one drain —
// one durable row, one runtime flip, one OnDrain wake. The check-and-set is
// atomic, so the loser always gets ErrAlreadyDraining.
func TestConcurrentBeginDrainSingleWinner(t *testing.T) {
	for i := 0; i < 300; i++ {
		registry := NewMemRegistry()
		var drains int32
		coord := newCoordinator(t, registry, "runtime-a", &drains)
		ctx := context.Background()
		if err := coord.Register(ctx); err != nil {
			t.Fatal(err)
		}
		if err := coord.Ready(ctx); err != nil {
			t.Fatal(err)
		}

		type outcome struct {
			status Status
			err    error
		}
		results := make(chan outcome, 2)
		var start sync.WaitGroup
		start.Add(1)
		for g := 0; g < 2; g++ {
			go func(mode Mode) {
				start.Wait()
				status, err := coord.BeginDrain(ctx, mode, time.Second)
				results <- outcome{status, err}
			}(ModeHandoff)
		}
		start.Done()
		first, second := <-results, <-results

		winners, losers := 0, 0
		for _, res := range []outcome{first, second} {
			switch {
			case res.err == nil:
				winners++
				if !res.status.Draining || res.status.Claiming {
					t.Fatalf("iteration %d: winner status = %+v", i, res.status)
				}
			case errors.Is(res.err, ErrAlreadyDraining):
				losers++
			default:
				t.Fatalf("iteration %d: unexpected error: %v", i, res.err)
			}
		}
		if winners != 1 || losers != 1 {
			t.Fatalf("iteration %d: winners=%d losers=%d, want 1/1", i, winners, losers)
		}
		if drains != 1 {
			t.Fatalf("iteration %d: OnDrain fired %d times, want exactly 1", i, drains)
		}
		rows, err := registry.ListHandoffs(ctx, HandoffQuery{FromRuntime: "runtime-a"})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("iteration %d: %d drain rows, want exactly 1", i, len(rows))
		}
	}
}

// TestBeginDrainSurvivesCancelledCaller is the M2 regression for the HTTP
// trigger: the client hanging up must not abort the durable part of the drain
// (the admin route passes the request context).
func TestBeginDrainSurvivesCancelledCaller(t *testing.T) {
	registry := NewMemRegistry()
	coord := newCoordinator(t, registry, "runtime-a", nil)
	base := context.Background()
	if err := coord.Register(base); err != nil {
		t.Fatal(err)
	}
	if err := coord.Ready(base); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(base)
	cancel()
	if _, err := coord.BeginDrain(ctx, ModeHandoff, time.Second); err != nil {
		t.Fatalf("drain with a cancelled caller context: %v", err)
	}
	if !coord.Draining() || coord.Claiming() {
		t.Fatalf("drain did not take effect: %+v", coord.Status())
	}
	rows, err := registry.ListHandoffs(base, HandoffQuery{FromRuntime: "runtime-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != HandoffStarted {
		t.Fatalf("drain audit rows = %+v, want one started row", rows)
	}
}

// TestBeginDrainPersistFailureRollsBack is the M2 regression for a failed
// durable write: the runtime must not end up half-drained (claiming stopped and
// draining flipped) with no audit row and no retry possible.
func TestBeginDrainPersistFailureRollsBack(t *testing.T) {
	registry := &failingRegistry{Registry: NewMemRegistry()}
	coord := newCoordinator(t, registry, "runtime-a", nil)
	ctx := context.Background()
	if err := coord.Register(ctx); err != nil {
		t.Fatal(err)
	}
	if err := coord.Ready(ctx); err != nil {
		t.Fatal(err)
	}

	registry.failSaveHandoff = 1
	if _, err := coord.BeginDrain(ctx, ModeHandoff, time.Second); err == nil {
		t.Fatal("BeginDrain succeeded although its audit row could not be written")
	}
	if coord.Draining() {
		t.Fatal("a failed drain left the runtime draining (half-drained zombie)")
	}
	if !coord.Claiming() {
		t.Fatal("a failed drain left the claim gate closed")
	}
	// The retry must be possible (not ErrAlreadyDraining).
	if _, err := coord.BeginDrain(ctx, ModeHandoff, time.Second); err != nil {
		t.Fatalf("retry after a failed drain: %v", err)
	}
	if !coord.Draining() || coord.Claiming() {
		t.Fatalf("retry did not take effect: %+v", coord.Status())
	}

	// A failure on the second write (the handoff row exists, the runtime row
	// does not) must close the written row instead of leaving it "started".
	registry2 := &failingRegistry{Registry: NewMemRegistry()}
	coord2 := newCoordinator(t, registry2, "runtime-b", nil)
	if err := coord2.Register(ctx); err != nil {
		t.Fatal(err)
	}
	if err := coord2.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	registry2.failSaveRuntime = 1 // the drain's runtime row write
	if _, err := coord2.BeginDrain(ctx, ModeHandoff, time.Second); err == nil {
		t.Fatal("BeginDrain succeeded although the runtime row could not be written")
	}
	rows, err := registry2.ListHandoffs(ctx, HandoffQuery{FromRuntime: "runtime-b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != HandoffTimedOut || rows[0].FinishedAt.IsZero() {
		t.Fatalf("failed drain left a dangling row: %+v", rows)
	}
}

// TestShutdownDrainAuditReachesCompleted is the M3 regression: the SIGTERM
// path exits through Stop without WaitDrain, and the audit row must still be a
// finished drain, not a row stuck at "started" forever.
func TestShutdownDrainAuditReachesCompleted(t *testing.T) {
	registry := NewMemRegistry()
	coord := newCoordinator(t, registry, "runtime-a", nil)
	ctx := context.Background()
	if err := coord.Register(ctx); err != nil {
		t.Fatal(err)
	}
	if err := coord.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.BeginDrain(ctx, ModeShutdown, time.Second); err != nil {
		t.Fatal(err)
	}
	// No WaitDrain: the shutdown path runs its own stream window and exits.
	if err := coord.Stop(ctx, "process exit"); err != nil {
		t.Fatal(err)
	}
	rows, err := registry.ListHandoffs(ctx, HandoffQuery{FromRuntime: "runtime-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	if rows[0].Status != HandoffCompleted || rows[0].FinishedAt.IsZero() {
		t.Fatalf("shutdown drain row = %+v, want completed with finished_at", rows[0])
	}
	// A second Stop is a no-op on the row.
	if err := coord.Stop(ctx, "again"); err != nil {
		t.Fatal(err)
	}
	rows, err = registry.ListHandoffs(ctx, HandoffQuery{FromRuntime: "runtime-a"})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Status != HandoffCompleted || rows[0].Detail == "again" {
		t.Fatalf("second Stop rewrote the drain row: %+v", rows[0])
	}
}

// TestWaitDrainThenStopDoesNotDoubleFinish: the handoff path finishes its row in
// WaitDrain; the process-exit Stop afterwards must not rewrite it.
func TestWaitDrainThenStopDoesNotDoubleFinish(t *testing.T) {
	registry := NewMemRegistry()
	coord := newCoordinator(t, registry, "runtime-a", nil)
	ctx := context.Background()
	if err := coord.Register(ctx); err != nil {
		t.Fatal(err)
	}
	if err := coord.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.BeginDrain(ctx, ModeHandoff, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := coord.WaitDrain(ctx); err != nil {
		t.Fatalf("wait drain: %v", err)
	}
	if err := coord.Stop(ctx, "process exit"); err != nil {
		t.Fatal(err)
	}
	rows, err := registry.ListHandoffs(ctx, HandoffQuery{FromRuntime: "runtime-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != HandoffCompleted {
		t.Fatalf("drain rows = %+v", rows)
	}
	if rows[0].Detail != "in-flight runs drained" {
		t.Fatalf("Stop rewrote the finish detail: %q", rows[0].Detail)
	}
}

// TestAdoptReleasesClaimOnFailure is the m4 regression: a successor that
// claimed and pinned but then failed to record its adoption must give the
// claim back, or the session stays owned by a runtime that never adopted it
// until the lease expires.
func TestAdoptReleasesClaimOnFailure(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			ctx := context.Background()
			a := h.readyRuntime(t, "runtime-a", 1)
			leaseA := h.claim(a)
			h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindUserMessage,
				RequestID: "r1", Payload: userPayload("hello")})
			if err := a.Release(ctx, leaseA); err != nil {
				t.Fatal(err)
			}

			registry := &failingRegistry{Registry: h.reg, failSaveHandoff: 1}
			b := newCoordinator(t, registry, "runtime-b", nil)
			if err := b.Register(ctx); err != nil {
				t.Fatal(err)
			}
			if err := b.Ready(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Adopt(ctx, h.scope, 2*time.Second); err == nil {
				t.Fatal("Adopt succeeded although its audit row could not be written")
			}

			// The claim must be back in the pool: no live lease, and another
			// owner can take the session over.
			state, err := h.k.LeaseState(ctx, h.scope)
			if err != nil {
				t.Fatal(err)
			}
			if state.Found && state.Live {
				t.Fatalf("failed Adopt leaked its claim: %+v", state)
			}
			c := h.readyRuntime(t, "runtime-c", 1)
			if _, err := c.Adopt(ctx, h.scope, 2*time.Second); err != nil {
				t.Fatalf("another owner could not adopt after the failed attempt: %v", err)
			}
		})
	}
}

// TestAbandonedSuccessorKeepsUnreadInput is the B3 regression at the handoff
// level: a successor that claimed and then died before reading must not have
// consumed the unread-input fact; the next owner still owes a turn (and the
// run converges after it answers).
func TestAbandonedSuccessorKeepsUnreadInput(t *testing.T) {
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			h := im.build(t)
			ctx := context.Background()
			a := h.readyRuntime(t, "runtime-a", 1)
			leaseA := h.claim(a)
			first := h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindUserMessage,
				RequestID: "r1", Payload: userPayload("one")})
			// Input two arrives while attempt A runs; A answers only one.
			h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindUserMessage,
				RequestID: "r2", Payload: userPayload("two")})
			answer := h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindAssistantMessage,
				Token: leaseA.Token, GenerationID: "g1", Payload: assistantText("one?")})
			h.finishWith(leaseA, answer.Seq, first.Seq, nil)
			if got := h.runStatus(); got != kernel.RunStatusPending {
				t.Fatalf("run status = %s, want pending", got)
			}

			// The successor claims and is abandoned before it reads.
			b := h.readyRuntime(t, "runtime-b", 2)
			adoptionB := h.adopt(b, 2*time.Second)
			if !adoptionB.Lease.PendingInput {
				t.Fatal("the successor did not see the unread input")
			}
			// The rebuild must report the debt from the run row, not from the
			// log tail (which ends with the previous attempt's answer).
			if !adoptionB.Rebuild.PendingUserInput {
				t.Fatalf("rebuild %s reports no pending input although the row says otherwise", adoptionB.Rebuild)
			}
			if adoptionB.Rebuild.ReadWatermark != first.Seq {
				t.Fatalf("rebuild watermark = %d, want %d", adoptionB.Rebuild.ReadWatermark, first.Seq)
			}
			if err := b.Release(ctx, adoptionB.Lease); err != nil {
				t.Fatal(err)
			}

			// The next owner must still owe a turn for it.
			c := h.readyRuntime(t, "runtime-c", 3)
			adoptionC := h.adopt(c, 2*time.Second)
			if !adoptionC.Lease.PendingInput {
				t.Fatal("abandoning the successor lost the unread input")
			}
			h.commit(kernel.CommitRequest{Scope: h.scope, Kind: kernel.KindAssistantMessage,
				Token: adoptionC.Lease.Token, GenerationID: "g2", Payload: assistantText("two")})
			h.finishWith(adoptionC.Lease, h.lastSeq(), adoptionC.Rebuild.Cursor, nil)
			if got := h.runStatus(); got != kernel.RunStatusDone {
				t.Fatalf("run status = %s, want done", got)
			}
			if h.unread() {
				t.Fatal("the unread-input flag survived the answering finish")
			}
			if n := h.countKind(kernel.KindAssistantMessage); n != 2 {
				t.Fatalf("assistant events = %d, want 2 (one per input)", n)
			}
			h.assertContiguous()
		})
	}
}
