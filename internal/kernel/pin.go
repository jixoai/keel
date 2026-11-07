// Loadout pinning: every claim records the loadout revision its runtime is
// running under, so a handoff audit can answer "which run completed on which
// loadout revision and which owner epoch" (runtime-handoff spec: loadout 版本固定).
//
// The pin is written by the owner that holds the claim and is fenced like any
// other run-scoped write: a drained runtime cannot re-pin a run the new owner
// has already claimed.
package kernel

import (
	"context"
	"errors"
	"fmt"
)

// RunPin is the claim pin of one run row.
type RunPin struct {
	Scope    Scope
	Status   string
	Owner    string
	Epoch    int64
	Attempts int
	Cursor   int64
	// LoadoutID and LoadoutRevision are the loadout the owning runtime was
	// started with; zero values mean the pin was never written.
	LoadoutID       string
	LoadoutRevision int64
	// LoadoutGeneration and LoadoutOwnerEpoch are the settle metadata: the
	// in-process service generation and the fencing epoch of the attempt that
	// last finished the run (zero before any settle).
	LoadoutGeneration int64
	LoadoutOwnerEpoch int64
	// PendingInput reports that the row carries unread edge input: a claim
	// delivers it, a finish recomputes it.
	PendingInput bool
	// ReadWatermark is the last proven read point (input with seq > it is
	// unread).
	ReadWatermark int64
}

// RunPinStore is the persistence seam of the loadout pin. Both stores
// implement it; Kernel exposes the fenced operations.
type RunPinStore interface {
	// PinRunLoadout writes the pin under the session lock, only when the
	// presented token is the current claim.
	PinRunLoadout(ctx context.Context, lease Lease, loadoutID string, revision int64) error
	// ReadRunPin answers the run row's pin (zero-value pin, false when the
	// session has no run row).
	ReadRunPin(ctx context.Context, scope Scope) (RunPin, bool, error)
	// RecordRead writes the attempt's load high-water mark, fenced by the
	// claim. It is monotonic: a later (lower) record never moves the watermark
	// backwards.
	RecordRead(ctx context.Context, lease Lease, watermark int64) error
}

// pinStore returns the seam, or an error when the store has none.
func (k *Kernel) pinStore() (RunPinStore, error) {
	store, ok := k.log.(RunPinStore)
	if !ok {
		return nil, &Error{Reason: ReasonInvalidRequest, Op: OpPin, Detail: "store does not support run pins", Err: ErrInvalidRequest}
	}
	return store, nil
}

// PinRunLoadout records the loadout id/revision on the claimed run. It is the
// first thing a runtime does after Claim, and it is fenced: a stale token is
// rejected with the same reasons as a stale commit.
func (k *Kernel) PinRunLoadout(ctx context.Context, lease Lease, loadoutID string, revision int64) error {
	if lease.Token.IsZero() {
		return k.reject(ReasonMissingToken, OpPin, lease.Scope, "", lease.Token, "a lease token is required", ErrMissingToken)
	}
	if lease.Scope.IsZero() {
		return k.reject(ReasonInvalidRequest, OpPin, lease.Scope, "", lease.Token, "tenant and session are required", ErrInvalidRequest)
	}
	store, err := k.pinStore()
	if err != nil {
		return err
	}
	if err := store.PinRunLoadout(ctx, lease, loadoutID, revision); err != nil {
		return k.classifyStoreError(OpPin, CommitRequest{Scope: lease.Scope, Token: lease.Token}, err)
	}
	return nil
}

// RecordRead persists the attempt's load high-water mark. The runner calls it
// immediately after loading the log and before the first model call, so the
// fact survives a kill that never reaches Finish: the successor then knows
// exactly which input this attempt did not read, instead of falling back to a
// tail heuristic that an answer committed by this attempt defeats.
func (k *Kernel) RecordRead(ctx context.Context, lease Lease, watermark int64) error {
	if lease.Token.IsZero() {
		return k.reject(ReasonMissingToken, OpPin, lease.Scope, "", lease.Token, "a lease token is required", ErrMissingToken)
	}
	if lease.Scope.IsZero() {
		return k.reject(ReasonInvalidRequest, OpPin, lease.Scope, "", lease.Token, "tenant and session are required", ErrInvalidRequest)
	}
	if watermark < 0 {
		return k.reject(ReasonInvalidRequest, OpPin, lease.Scope, "", lease.Token, "watermark must not be negative", ErrInvalidRequest)
	}
	store, err := k.pinStore()
	if err != nil {
		return err
	}
	if err := store.RecordRead(ctx, lease, watermark); err != nil {
		return k.classifyStoreError(OpPin, CommitRequest{Scope: lease.Scope, Token: lease.Token}, err)
	}
	return nil
}

// RunPin reads the run's claim pin (owner, epoch, loadout, status). ErrNotFound
// for a foreign or missing session, ErrNoRunnable when the session has no run
// row (an external session, for example).
func (k *Kernel) RunPin(ctx context.Context, scope Scope) (RunPin, error) {
	if scope.IsZero() {
		return RunPin{}, k.reject(ReasonInvalidRequest, OpPin, scope, "", OwnerToken{}, "tenant and session are required", ErrInvalidRequest)
	}
	store, err := k.pinStore()
	if err != nil {
		return RunPin{}, err
	}
	pin, found, err := store.ReadRunPin(ctx, scope)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return RunPin{}, k.reject(ReasonNotFound, OpPin, scope, "", OwnerToken{}, "", ErrNotFound)
		}
		return RunPin{}, k.classifyStoreError(OpPin, CommitRequest{Scope: scope}, err)
	}
	if !found {
		return RunPin{}, k.reject(ReasonNoRunnable, OpPin, scope, "", OwnerToken{}, "the session has no run row", ErrNoRunnable)
	}
	return pin, nil
}

// ensure the compile-time contract for the concrete stores.
var (
	_ RunPinStore   = (*PGStore)(nil)
	_ ExternalStore = (*PGStore)(nil)
	_ ExternalStore = (*MemStore)(nil)
	_ RunPinStore   = (*MemStore)(nil)
)

// pinSummary renders a pin for diagnostics.
func (p RunPin) String() string {
	return fmt.Sprintf("%s/%s owner=%s epoch=%d status=%s loadout=%s@%d",
		p.Scope.TenantID, p.Scope.SessionID, p.Owner, p.Epoch, p.Status, p.LoadoutID, p.LoadoutRevision)
}
