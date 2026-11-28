package kernel

import (
	"context"
	"fmt"
	"strings"
)

// Compaction task recovery checkpoints (feature: PI CompactionTask parity).
//
// This file is deliberately separate from checkpoint.go: a checkpoint is the
// replayable projection of a FINISHED compaction, while a compaction task
// checkpoint is the in-flight recovery state of an unfinished one. The task
// row never contains a summary and is deleted as soon as the compaction event
// (and its projection) is durable.

// SaveCompactionTask upserts one task checkpoint. The tenant boundary is
// enforced like every other write: a foreign or missing session answers
// ErrNotFound.
func (k *Kernel) SaveCompactionTask(ctx context.Context, scope Scope, checkpoint CompactionTaskCheckpoint) error {
	if err := validateCompactionTask(scope, checkpoint); err != nil {
		return k.reject(ReasonInvalidRequest, OpCommit, scope, KindCompaction, OwnerToken{}, err.Error(), ErrInvalidRequest)
	}
	if err := k.log.SaveCompactionTask(ctx, scope.TenantID, scope.SessionID, checkpoint); err != nil {
		return k.classifyStoreError(OpCommit, CommitRequest{Scope: scope, Kind: KindCompaction}, err)
	}
	return nil
}

// LoadCompactionTask returns one in-flight task checkpoint.
func (k *Kernel) LoadCompactionTask(ctx context.Context, scope Scope, requestID string) (CompactionTaskCheckpoint, bool, error) {
	if scope.IsZero() || strings.TrimSpace(requestID) == "" {
		return CompactionTaskCheckpoint{}, false, k.reject(ReasonInvalidRequest, OpRead, scope, KindCompaction, OwnerToken{}, "tenant, session and requestId are required", ErrInvalidRequest)
	}
	checkpoint, found, err := k.log.LoadCompactionTask(ctx, scope.TenantID, scope.SessionID, requestID)
	if err != nil {
		return CompactionTaskCheckpoint{}, false, k.classifyStoreError(OpRead, CommitRequest{Scope: scope, Kind: KindCompaction}, err)
	}
	return checkpoint, found, nil
}

// ListCompactionTasks returns every in-flight task checkpoint of the session.
func (k *Kernel) ListCompactionTasks(ctx context.Context, scope Scope) ([]CompactionTaskCheckpoint, error) {
	if scope.IsZero() {
		return nil, k.reject(ReasonInvalidRequest, OpRead, scope, KindCompaction, OwnerToken{}, "tenant and session are required", ErrInvalidRequest)
	}
	checkpoints, err := k.log.ListCompactionTasks(ctx, scope.TenantID, scope.SessionID)
	if err != nil {
		return nil, k.classifyStoreError(OpRead, CommitRequest{Scope: scope, Kind: KindCompaction}, err)
	}
	return checkpoints, nil
}

// DeleteCompactionTask removes a finished or abandoned task checkpoint. A
// missing row is a no-op: completion is idempotent.
func (k *Kernel) DeleteCompactionTask(ctx context.Context, scope Scope, requestID string) error {
	if scope.IsZero() || strings.TrimSpace(requestID) == "" {
		return k.reject(ReasonInvalidRequest, OpCommit, scope, KindCompaction, OwnerToken{}, "tenant, session and requestId are required", ErrInvalidRequest)
	}
	if err := k.log.DeleteCompactionTask(ctx, scope.TenantID, scope.SessionID, requestID); err != nil {
		return k.classifyStoreError(OpCommit, CommitRequest{Scope: scope, Kind: KindCompaction}, err)
	}
	return nil
}

// PruneCompactionTasks bounds the in-flight/abandoned task rows of one session.
// It is independent of checkpoints and events.
func (k *Kernel) PruneCompactionTasks(ctx context.Context, scope Scope, req CompactionTaskPruneRequest) (int64, error) {
	if scope.IsZero() {
		return 0, k.reject(ReasonInvalidRequest, OpCommit, scope, KindCompaction, OwnerToken{}, "tenant and session are required", ErrInvalidRequest)
	}
	if req.MaxTasks < 0 {
		return 0, k.reject(ReasonInvalidRequest, OpCommit, scope, KindCompaction, OwnerToken{}, "prune bounds must be >= 0", ErrInvalidRequest)
	}
	if req.MaxTasks == 0 {
		return 0, nil
	}
	pruned, err := k.log.PruneCompactionTasks(ctx, scope.TenantID, scope.SessionID, req)
	if err != nil {
		return 0, k.classifyStoreError(OpCommit, CommitRequest{Scope: scope, Kind: KindCompaction}, err)
	}
	return pruned, nil
}

// validateCompactionTask is the shared structural validation (PG and memory
// both enforce it through the kernel). Field errors are stable enough to log;
// they never contain message bodies.
func validateCompactionTask(scope Scope, checkpoint CompactionTaskCheckpoint) error {
	if scope.IsZero() {
		return fmt.Errorf("tenant and session are required")
	}
	if strings.TrimSpace(checkpoint.RequestID) == "" {
		return fmt.Errorf("compaction task needs a requestId")
	}
	if checkpoint.TokensBefore < 0 {
		return fmt.Errorf("compaction task tokens must be >= 0")
	}
	if checkpoint.Attempt < 0 {
		return fmt.Errorf("compaction task attempt must be >= 0")
	}
	switch checkpoint.Phase {
	case CompactionPhaseSelect:
		if checkpoint.FirstKeptSeq != 0 || checkpoint.TailSeq != 0 {
			return fmt.Errorf("select phase must not pin a range")
		}
	case CompactionPhaseSummarize, CompactionPhaseRetry:
		if checkpoint.FirstKeptSeq <= 0 {
			return fmt.Errorf("%s phase needs a positive first kept seq", checkpoint.Phase)
		}
		if checkpoint.TailSeq < checkpoint.FirstKeptSeq {
			return fmt.Errorf("%s phase tail seq %d must be >= first kept seq %d",
				checkpoint.Phase, checkpoint.TailSeq, checkpoint.FirstKeptSeq)
		}
		if checkpoint.Phase == CompactionPhaseRetry && checkpoint.RetryUntil.IsZero() {
			return fmt.Errorf("retry phase needs a retry deadline")
		}
	default:
		return fmt.Errorf("unknown compaction phase %q", checkpoint.Phase)
	}
	return nil
}
