package agent

// Manual checkpoint compaction (change keel-checkpoint): the explicit
// POST /v1/sessions/{id}/compact path. It shares the automatic path's cut
// safety and failure semantics: a summary failure or cancellation leaves the
// context (the event log) unchanged and writes nothing; success appends one
// compaction event and saves one checkpoint projection keyed by that event's
// seq and the caller's requestId.
//
// The caller must hold the session's run lease. Manual compaction runs at a
// turn boundary, never concurrently with an attempt: the lease is the same
// mutual exclusion the automatic path relies on, and the compaction event is an
// owner-scoped fact.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
)

// ErrManualCompactDisabled reports that the effective checkpoint policy does
// not allow manual compaction (or checkpoints at all).
var ErrManualCompactDisabled = errors.New("agent: manual compaction is disabled by the checkpoint policy")

// ErrManualCompactNothing reports that the transcript has no safe cut to
// summarize; the context is unchanged.
var ErrManualCompactNothing = errors.New("agent: nothing to compact at a safe cut point")

// ManualCompactResult is the outcome of one manual compaction (or of the
// idempotent replay of an earlier one).
type ManualCompactResult struct {
	// CheckpointID is the compaction event's seq: the checkpoint projection's
	// identity. Zero when nothing was performed.
	CheckpointID int64
	// Performed is true when this call produced a new checkpoint.
	Performed bool
	// Replayed is true when the requestId already produced a checkpoint and the
	// same result is being returned without a second LLM call.
	Replayed bool
	// TokensBefore and TokensAfter are the context-token estimates around the
	// compaction (equal when nothing was performed).
	TokensBefore int
	TokensAfter  int
	// KeptFromSeq is the seq of the first retained event (0 when unanchored).
	KeptFromSeq int64
	// PolicyVersion is the effective policy identity recorded on the checkpoint.
	PolicyVersion string
	// Reason is an identifier-only explanation ("compacted", "nothing-to-compact",
	// "replayed").
	Reason string
}

// CompactNow performs one manual checkpoint compaction. requestID is required
// and idempotent per session: a repeat returns the earlier checkpoint without
// calling the model again. The original context is preserved on every failure;
// only a successful summary is committed.
func (r *Runner) CompactNow(ctx context.Context, lease kernel.Lease, requestID string) (ManualCompactResult, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return ManualCompactResult{}, errors.New("agent: manual compaction needs a requestId")
	}
	if r.Committer == nil || r.Events == nil {
		return ManualCompactResult{}, errors.New("agent: manual compaction needs the kernel seams (Committer, Reader)")
	}
	if r.Checkpoints == nil {
		return ManualCompactResult{}, errors.New("agent: manual compaction needs the checkpoint seam")
	}
	policy := r.checkpointPolicy()
	if !policy.Enabled || !policy.ManualCompact {
		return ManualCompactResult{}, ErrManualCompactDisabled
	}

	// Idempotent replay: the requestId is the manual surface's key. An earlier
	// successful call is returned as-is; a failed/cancelled call left no row and
	// may be retried.
	if lookup, ok := r.Checkpoints.(kernel.CheckpointRequestLookup); ok {
		cp, found, err := lookup.CheckpointByRequest(ctx, lease.Scope, requestID)
		if err != nil {
			return ManualCompactResult{}, err
		}
		if found {
			return ManualCompactResult{
				CheckpointID:  cp.UptoSeq,
				Replayed:      true,
				TokensBefore:  cp.TokensBefore,
				TokensAfter:   cp.TokensAfter,
				KeptFromSeq:   cp.KeptFromSeq,
				PolicyVersion: cp.PolicyVersion,
				Reason:        "replayed",
			}, nil
		}
	}

	events, _, err := r.loadEvents(ctx, lease.Scope)
	if err != nil {
		return ManualCompactResult{}, err
	}
	view, checkpoint, checkpointSeq := r.modelView(ctx, lease.Scope, events)
	msgs, seqs := DecodeMessages(view, nil)
	prevIndex := -1
	if checkpoint != nil {
		msgs = append(agentcore.MessageList{*checkpoint}, msgs...)
		seqs = append([]int64{checkpointSeq}, seqs...)
		prevIndex = 0
	}
	if len(msgs) == 0 {
		return ManualCompactResult{PolicyVersion: policy.Version(), Reason: "nothing-to-compact"}, ErrManualCompactNothing
	}
	before := compaction.EstimateContextTokensConservative(msgs)

	settings := compaction.DefaultCompactionSettings
	if r.Compaction != nil {
		settings = r.Compaction.Settings
	}
	if r.Compaction != nil && r.Compaction.Policy != nil {
		// An explicit policy is authoritative, including a zero retained budget.
		settings.KeepRecentTokens = policy.RetainTokens
	}
	// The explicit call bypasses the threshold: the user asked now. Safety rules
	// (pending user, tool boundary) still apply inside Compact.
	model := provider.Model{
		Provider:      r.ProviderName,
		ID:            r.Model,
		ContextWindow: r.compactionContextWindow(),
	}
	streamConfig := provider.StreamConfig{APIKey: r.APIKey, ThinkingLevel: r.ThinkingLevel}
	res, err := compaction.Compact(ctx, r.Stream, model, msgs, settings, prevIndex, nil, "", streamConfig)
	if err != nil {
		// The summary request failed or was cancelled: the original context (the
		// event log) is untouched and nothing is written.
		r.Logger.Warn("manual compaction failed; context unchanged",
			"tenant_id", lease.Scope.TenantID, "session_id", lease.Scope.SessionID,
			"tokens_before", before, "err", err)
		return ManualCompactResult{TokensBefore: before, TokensAfter: before, PolicyVersion: policy.Version(), Reason: "summary-failed"}, err
	}
	if res == nil {
		return ManualCompactResult{TokensBefore: before, TokensAfter: before, PolicyVersion: policy.Version(), Reason: "nothing-to-compact"}, ErrManualCompactNothing
	}

	now := time.Now().UnixMilli()
	rebuilt := res.RebuildContext(msgs, now)
	tokensAfter := compaction.EstimateContextTokensConservative(rebuilt)
	if res.FirstKeptIndex <= 0 || res.FirstKeptIndex >= len(seqs) {
		return ManualCompactResult{TokensBefore: before, TokensAfter: before, PolicyVersion: policy.Version(), Reason: "unanchored-cut"}, errors.New("agent: manual compaction cut cannot be anchored to committed events")
	}
	anchor := seqs[res.FirstKeptIndex]
	if anchor <= 0 {
		return ManualCompactResult{TokensBefore: before, TokensAfter: before, PolicyVersion: policy.Version(), Reason: "unanchored-cut"}, errors.New("agent: manual compaction cut cannot be anchored to committed events")
	}
	summarized := len(msgs) - (len(rebuilt) - 1)
	raw, err := marshalPayload(map[string]any{
		"summary":             res.Summary,
		"tokensBefore":        res.TokensBefore,
		"tokensAfter":         tokensAfter,
		"firstKept":           res.FirstKeptIndex,
		"firstKeptTranscript": res.FirstKeptIndex,
		"summarized":          summarized,
		"kept":                len(rebuilt) - 1,
		"timestamp":           now,
		"reason":              "manual",
		"requestId":           requestID,
	})
	if err != nil {
		return ManualCompactResult{}, err
	}
	seq, err := r.commitEvent(ctx, lease, kernel.CommitRequest{
		Kind:    kernel.KindCompaction,
		Payload: raw,
	})
	if err != nil {
		// Nothing durable changed; the caller can retry.
		return ManualCompactResult{}, err
	}
	record := kernel.CheckpointRecord{
		UptoSeq:       seq,
		KeptFromSeq:   anchor,
		Summary:       res.Summary,
		TokensBefore:  res.TokensBefore,
		TokensAfter:   tokensAfter,
		PolicyVersion: policy.Version(),
		RequestID:     requestID,
	}
	if err := r.Checkpoints.SaveCheckpoint(ctx, lease.Scope, record); err != nil {
		// The compaction event is the truth and stays; the projection is missing,
		// so the call reports failure rather than a checkpoint id it cannot
		// promise. A retry would compact again (the event log is not shortened).
		r.Logger.Warn("manual compaction checkpoint save failed",
			"tenant_id", lease.Scope.TenantID, "session_id", lease.Scope.SessionID,
			"seq", seq, "err", err)
		return ManualCompactResult{}, err
	}
	r.pruneCheckpoints(ctx, lease.Scope)
	r.Logger.Info("manual checkpoint created",
		"tenant_id", lease.Scope.TenantID, "session_id", lease.Scope.SessionID,
		"cut_seq", seq, "kept_from_seq", anchor,
		"tokens_before", res.TokensBefore, "tokens_after", tokensAfter,
		"policy_version", policy.Version(), "request_id", requestID)
	return ManualCompactResult{
		CheckpointID:  seq,
		Performed:     true,
		TokensBefore:  res.TokensBefore,
		TokensAfter:   tokensAfter,
		KeptFromSeq:   anchor,
		PolicyVersion: policy.Version(),
		Reason:        "compacted",
	}, nil
}

// compactionContextWindow is the model window used while summarizing. The loop's
// configured window wins; the provider model description is the fallback.
func (r *Runner) compactionContextWindow() int {
	if r.Compaction != nil && r.Compaction.ContextWindow > 0 {
		return r.Compaction.ContextWindow
	}
	return 0
}
