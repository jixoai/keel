package placement

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/kernel"
)

// replayCommitter is an idempotent kernel double: a requestId already in the
// map is answered with its receipt (Deduped), exactly as the kernel's dedupe
// branch does. It records calls so the test can assert the replay did not take
// the admission path.
type replayCommitter struct {
	mu       sync.Mutex
	receipts map[string]kernel.Receipt
	lookups  int
	commits  int
}

func (f *replayCommitter) Commit(_ context.Context, req kernel.CommitRequest) (kernel.Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits++
	if req.RequestID != "" {
		if existing, ok := f.receipts[req.RequestID]; ok {
			return existing, nil
		}
		receipt := kernel.Receipt{Seq: int64(f.commits), Kind: req.Kind}
		f.receipts[req.RequestID] = receipt
		return receipt, nil
	}
	return kernel.Receipt{Seq: int64(f.commits), Kind: req.Kind}, nil
}

func (f *replayCommitter) Lookup(_ context.Context, _ kernel.Scope, key kernel.KeyRef) (kernel.Receipt, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	receipt, ok := f.receipts[key.Value]
	return receipt, ok, nil
}

func (f *replayCommitter) counts() (lookups, commits int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups, f.commits
}

// TestGatedCommitterReplaySkipsAFullGroup is the P2 regression: a pure replay
// of an already-committed requestId must return the existing receipt even when
// the group is full, without admitting a run or consuming a slot. Before the
// fix the wrapper admitted first, so the replay answered 429 and (with room)
// left an empty pending run behind.
func TestGatedCommitterReplaySkipsAFullGroup(t *testing.T) {
	ctx := context.Background()
	svc, store := memService(t, Options{})
	tenant := uuid.New()
	group := mustGroup(t, svc, tenant, "replay-full", 1)
	filler, replaying := uuid.New(), uuid.New()
	for _, session := range []uuid.UUID{filler, replaying} {
		mustJoin(t, svc, tenant, session, group.GroupID, "runtime-1", time.Minute)
	}
	gate := memGate(t, svc, "runtime-1", newTestSessions(true).state)
	if _, err := gate.Admit(ctx, tenant, filler); err != nil {
		t.Fatalf("fill the group: %v", err)
	}

	edgeCommitter := &replayCommitter{receipts: map[string]kernel.Receipt{
		"replay-1": {Seq: 41, Kind: kernel.KindUserMessage, Deduped: true},
	}}
	edge, err := NewGatedCommitter(edgeCommitter, gate, nil)
	if err != nil {
		t.Fatal(err)
	}

	receipt, err := edge.Commit(ctx, kernel.CommitRequest{
		Scope:      kernel.Scope{TenantID: tenant, SessionID: replaying},
		Kind:       kernel.KindUserMessage,
		RequestID:  "replay-1",
		NativeEdge: true,
	})
	if err != nil {
		t.Fatalf("replay in a full group = %v, want the stored receipt", err)
	}
	if receipt.Seq != 41 || !receipt.Deduped {
		t.Fatalf("receipt = %+v, want the deduped receipt at seq 41", receipt)
	}

	if _, exists := store.runs[memSessionKey{tenant, replaying}]; exists {
		t.Fatal("a replay reserved a run slot")
	}
	if lookups, commits := edgeCommitter.counts(); lookups != 1 || commits != 1 {
		t.Fatalf("committer lookups/commits = %d/%d, want 1/1 (lookup, then dedupe)", lookups, commits)
	}

	// The slot the filler owns is untouched: a fresh admission for the replayed
	// session still sees the full group instead of inheriting a phantom slot.
	_, err = gate.Admit(ctx, tenant, replaying)
	var capacity *CapacityError
	if !errors.As(err, &capacity) || capacity.Reason != CapacityGroup {
		t.Fatalf("admission after replay = %v, want the group to stay full", err)
	}
	if got := store.runs[memSessionKey{tenant, filler}].status; got != RunStatusPending {
		t.Fatalf("filler run = %q, want pending (the only slot)", got)
	}
}
