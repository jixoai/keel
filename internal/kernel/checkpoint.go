package kernel

import (
	"context"
	"fmt"
)

// SaveCheckpoint stores the compaction projection named by the compaction
// event's sequence. It is not an append: the event log is already committed and
// remains the only truth. The tenant boundary is enforced like every other read
// and write; a foreign or missing session answers ErrNotFound.
func (k *Kernel) SaveCheckpoint(ctx context.Context, scope Scope, uptoSeq, keptFromSeq int64, summary string) error {
	if scope.IsZero() {
		return k.reject(ReasonInvalidRequest, OpCommit, scope, KindCompaction, OwnerToken{}, "tenant and session are required", ErrInvalidRequest)
	}
	if uptoSeq <= 0 {
		return k.reject(ReasonInvalidRequest, OpCommit, scope, KindCompaction, OwnerToken{}, "checkpoint needs the compaction event's seq", ErrInvalidRequest)
	}
	if keptFromSeq < 0 || (keptFromSeq > 0 && keptFromSeq > uptoSeq) {
		return k.reject(ReasonInvalidRequest, OpCommit, scope, KindCompaction, OwnerToken{},
			fmt.Sprintf("checkpoint kept_from_seq %d is outside (0, upto_seq=%d]", keptFromSeq, uptoSeq), ErrInvalidRequest)
	}
	if err := k.log.SaveCheckpoint(ctx, scope.TenantID, scope.SessionID, uptoSeq, keptFromSeq, summary); err != nil {
		return k.classifyStoreError(OpCommit, CommitRequest{Scope: scope, Kind: KindCompaction}, err)
	}
	return nil
}

// LoadCheckpoint returns the session's newest checkpoint projection. It never
// falls back to an older row or to the event log: found=false means the caller
// must rebuild from events.
func (k *Kernel) LoadCheckpoint(ctx context.Context, scope Scope) (Checkpoint, bool, error) {
	if scope.IsZero() {
		return Checkpoint{}, false, k.reject(ReasonInvalidRequest, OpRead, scope, KindCompaction, OwnerToken{}, "tenant and session are required", ErrInvalidRequest)
	}
	cp, found, err := k.log.LoadCheckpoint(ctx, scope.TenantID, scope.SessionID)
	if err != nil {
		return Checkpoint{}, false, k.classifyStoreError(OpRead, CommitRequest{Scope: scope, Kind: KindCompaction}, err)
	}
	return cp, found, nil
}
