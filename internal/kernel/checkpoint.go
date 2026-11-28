package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// CheckpointSummaryHash returns the stable identity of a checkpoint summary.
// The projection stores it so a later read can prove the summary it serves is
// the one that was recorded, and a compaction event can carry the same fact.
func CheckpointSummaryHash(summary string) string {
	sum := sha256.Sum256([]byte(summary))
	return hex.EncodeToString(sum[:])
}

// SaveCheckpoint stores the compaction projection described by rec. It is not
// an append: the event log is already committed and remains the only truth. The
// tenant boundary is enforced like every other read and write; a foreign or
// missing session answers ErrNotFound.
//
// The compaction event's sequence is the automatic idempotency boundary (a
// replay updates the row). A non-empty RequestID is the manual surface's
// idempotency boundary: the store refuses to create a second row for it.
func (k *Kernel) SaveCheckpoint(ctx context.Context, scope Scope, rec CheckpointRecord) error {
	if scope.IsZero() {
		return k.reject(ReasonInvalidRequest, OpCommit, scope, KindCompaction, OwnerToken{}, "tenant and session are required", ErrInvalidRequest)
	}
	if rec.UptoSeq <= 0 {
		return k.reject(ReasonInvalidRequest, OpCommit, scope, KindCompaction, OwnerToken{}, "checkpoint needs the compaction event's seq", ErrInvalidRequest)
	}
	if rec.KeptFromSeq < 0 || (rec.KeptFromSeq > 0 && rec.KeptFromSeq >= rec.UptoSeq) {
		return k.reject(ReasonInvalidRequest, OpCommit, scope, KindCompaction, OwnerToken{},
			fmt.Sprintf("checkpoint kept_from_seq %d must be inside (0, upto_seq=%d)", rec.KeptFromSeq, rec.UptoSeq), ErrInvalidRequest)
	}
	if strings.TrimSpace(rec.Summary) == "" {
		return k.reject(ReasonInvalidRequest, OpCommit, scope, KindCompaction, OwnerToken{}, "checkpoint summary is empty", ErrInvalidRequest)
	}
	if rec.TokensBefore < 0 || rec.TokensAfter < 0 {
		return k.reject(ReasonInvalidRequest, OpCommit, scope, KindCompaction, OwnerToken{}, "checkpoint tokens must be >= 0", ErrInvalidRequest)
	}
	summaryHash := CheckpointSummaryHash(rec.Summary)
	if rec.SummaryHash == "" {
		rec.SummaryHash = summaryHash
	} else if rec.SummaryHash != summaryHash {
		return k.reject(ReasonInvalidRequest, OpCommit, scope, KindCompaction, OwnerToken{},
			"checkpoint summary hash does not match the summary", ErrInvalidRequest)
	}
	if err := k.log.SaveCheckpoint(ctx, scope.TenantID, scope.SessionID, rec); err != nil {
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

// CheckpointByRequest resolves a manual-compaction requestId to the checkpoint
// its earlier call produced. found=false means the request never completed (or
// was pruned) and a fresh compaction may run.
func (k *Kernel) CheckpointByRequest(ctx context.Context, scope Scope, requestID string) (Checkpoint, bool, error) {
	if scope.IsZero() || strings.TrimSpace(requestID) == "" {
		return Checkpoint{}, false, k.reject(ReasonInvalidRequest, OpRead, scope, KindCompaction, OwnerToken{}, "tenant, session and requestId are required", ErrInvalidRequest)
	}
	cp, found, err := k.log.CheckpointByRequest(ctx, scope.TenantID, scope.SessionID, requestID)
	if err != nil {
		return Checkpoint{}, false, k.classifyStoreError(OpRead, CommitRequest{Scope: scope, Kind: KindCompaction}, err)
	}
	return cp, found, nil
}

// PruneCheckpoints deletes old checkpoint projections of one session. It only
// removes projections; the event log is never shortened, and the newest valid
// checkpoint is always kept so the session keeps a recovery anchor.
func (k *Kernel) PruneCheckpoints(ctx context.Context, scope Scope, req PruneRequest) (int64, error) {
	if scope.IsZero() {
		return 0, k.reject(ReasonInvalidRequest, OpCommit, scope, KindCompaction, OwnerToken{}, "tenant and session are required", ErrInvalidRequest)
	}
	if req.MaxCheckpoints < 0 || req.TTL < 0 {
		return 0, k.reject(ReasonInvalidRequest, OpCommit, scope, KindCompaction, OwnerToken{}, "prune bounds must be >= 0", ErrInvalidRequest)
	}
	if req.MaxCheckpoints == 0 && req.TTL == 0 {
		return 0, nil
	}
	pruned, err := k.log.PruneCheckpoints(ctx, scope.TenantID, scope.SessionID, req)
	if err != nil {
		return 0, k.classifyStoreError(OpCommit, CommitRequest{Scope: scope, Kind: KindCompaction}, err)
	}
	return pruned, nil
}
