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
	"fmt"
	"strings"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
)

// ErrManualCompactDisabled reports that the effective checkpoint policy does
// not allow manual compaction.
var ErrManualCompactDisabled = errors.New("agent: manual compaction is disabled by the checkpoint policy")

// ErrManualCompactNothing reports that the transcript has no safe cut to
// summarize; the context is unchanged.
var ErrManualCompactNothing = errors.New("agent: nothing to compact at a safe cut point")

// ErrManualCompactToolCallOpen reports a dangling assistant tool call: the turn
// is incomplete and must be settled by a normal attempt, not summarized.
var ErrManualCompactToolCallOpen = errors.New("agent: manual compaction refused: unresolved tool call")

// ErrManualCompactUnsafe reports a cut that cannot be anchored without hiding an
// unread input, the inherited fork prefix, or an uncommitted boundary.
var ErrManualCompactUnsafe = errors.New("agent: manual compaction has no safe cut")

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
	// Recovered is true when the compaction event was committed by an earlier
	// call that died before saving its checkpoint, and this call restored the
	// projection from that event. No summary request was made.
	Recovered bool
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
	started := time.Now()
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
	policy := r.checkpointPolicy(ctx, lease.Scope)
	// Enabled gates automatic compaction only; the manual switch is independent.
	if !policy.ManualCompact {
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
	// Crash window: the summary was committed but the process died before the
	// checkpoint projection was saved. The committed event is the truth, so
	// restore the projection and return it instead of summarizing again.
	if recovered, found, err := r.recoverCommittedCompaction(ctx, lease.Scope, requestID, events); err != nil {
		return ManualCompactResult{}, err
	} else if found {
		return recovered, nil
	}
	task, hasTask, err := r.loadCompactionTask(ctx, lease.Scope, requestID)
	if err != nil {
		return ManualCompactResult{}, err
	}
	// A dangling tool call is an incomplete turn. Summarizing it would hide the
	// call from the next model request and let the run settle as done; refuse and
	// let a normal attempt fill (or fail) the result instead.
	unresolved, err := r.Events.UnresolvedToolCalls(ctx, lease.Scope)
	if err != nil {
		return ManualCompactResult{}, err
	}
	if len(unresolved) > 0 {
		return ManualCompactResult{}, ErrManualCompactToolCallOpen
	}

	// The summary call uses the same execution identity as a normal attempt:
	// the run's selection/binding, the resolved provider stream, dynamic key,
	// thinking level, context window and request overlay. A resolver failure is
	// fail closed: never call the static/fake stream behind the resolver's back.
	selection := runSelection(events)
	surface, err := r.resolveSurface(ctx, lease, selection)
	if err != nil {
		return ManualCompactResult{}, fmt.Errorf("agent: resolve agent binding: %w", err)
	}
	active, err := r.activeModelFor(ctx, lease, events, MergeSelectionDefaultModel(selection, surface.DefaultModel))
	if err != nil {
		return ManualCompactResult{}, fmt.Errorf("agent: resolve model: %w", err)
	}
	if active.Stream == nil {
		return ManualCompactResult{}, fmt.Errorf("agent: manual compaction has no provider stream for %s/%s", active.ProviderID, active.Model)
	}
	// The summary request is a billable provider call: account it in the same
	// ledger as a normal turn. The request identity includes the caller's
	// requestId, so two different manual compactions at the same generation do
	// not share a ledger row; the same requestId retried after a crash reuses
	// its row instead of double-billing.
	summaryStream := active.Stream
	if r.UsageRecorder != nil {
		summaryStream = NewUsageStream(summaryStream, r.UsageRecorder, lease.Scope,
			attemptUsageBaseID(generationBase(events))+"-compact-"+requestID, lastEventSeq(events),
			active.ProviderID, active.Model)
	}

	// The projection is read with the attempt's unread-input guard: a checkpoint
	// whose retained tail starts after an unread input is corrupt and falls back
	// to the full log.
	view, checkpoint, checkpointSeq := r.loadModelView(ctx, lease.Scope, events, lease.ReadWatermark, true, false)
	msgs, seqs := DecodeMessages(view, nil)
	if checkpoint != nil {
		msgs = append(agentcore.MessageList{*checkpoint}, msgs...)
		seqs = append([]int64{checkpointSeq}, seqs...)
	}
	if len(msgs) == 0 {
		return ManualCompactResult{PolicyVersion: policy.Version(), Reason: "nothing-to-compact"}, ErrManualCompactNothing
	}

	// Forked sessions inherit the parent prefix as a read-time union. The prefix
	// carries negative synthetic seqs, so it can be summarized but never anchors
	// a checkpoint to the child's log.
	prefixMsgs, prefixSeqs, err := r.forkPrefixMessages(ctx, lease.Scope)
	if err != nil {
		return ManualCompactResult{}, err
	}
	prefixLen := len(prefixMsgs)
	if prefixLen > 0 {
		msgs = append(prefixMsgs, msgs...)
		seqs = append(prefixSeqs, seqs...)
	}

	// Unread inputs are presented last, exactly like a normal attempt. A cut that
	// would summarize one of them is refused before the summary request, so a
	// manual compaction can never swallow an input the model still owes an answer.
	unread, consumptionKnown := unreadInputs(events, lease.ReadWatermark)
	isUnread := func(seq int64) bool {
		if seq <= 0 {
			return false
		}
		if consumptionKnown {
			return unread[seq]
		}
		if lease.ReadWatermark <= 0 {
			// No attempt has proven a read point and the log carries no
			// consumption fact: every committed input is treated as unread.
			return true
		}
		return seq > lease.ReadWatermark
	}
	msgs, seqs, movedStart := presentUnreadInputLast(msgs, seqs, isUnread)
	initialLen := len(msgs)
	before := compaction.EstimateContextTokensConservative(msgs)

	settings := compaction.DefaultCompactionSettings
	if r.Compaction != nil {
		settings = r.Compaction.Settings
	}
	// The effective policy is authoritative for retention, including a zero
	// retained budget.
	settings.KeepRecentTokens = policy.RetainTokens
	window := active.ContextWindow
	if window <= 0 && r.Compaction != nil {
		window = r.Compaction.ContextWindow
	}

	// Preflight the cut before spending a summary request: the compaction is only
	// safe when it starts at a committed child event after every unread input and
	// outside the inherited prefix.
	//
	// A task checkpoint in the summarize phase carries the range the interrupted
	// call already selected. Resume that pin (PI CompactionCheckpoint phase
	// "summarize") instead of selecting a new cut: new input arriving after the
	// crash must not move what this request promised to summarize. A pin that no
	// longer names a committed child event falls back to a fresh selection.
	cutIndex := 0
	resumed := false
	if hasTask && task.Phase == kernel.CompactionPhaseSummarize {
		if pinned := indexOfPositiveSeq(seqs, task.FirstKeptSeq); pinned > 0 {
			if problem := manualCutUnsafe(seqs, pinned, prefixLen, movedStart, initialLen, isUnread); problem == "" {
				cutIndex = pinned
				resumed = true
			} else {
				r.Logger.Warn("compaction task pin is no longer safe; re-selecting",
					"session_id", lease.Scope.SessionID, "request_id", requestID,
					"pinned_seq", task.FirstKeptSeq, "problem", problem)
			}
		} else {
			r.Logger.Warn("compaction task pin is no longer in the transcript; re-selecting",
				"session_id", lease.Scope.SessionID, "request_id", requestID,
				"pinned_seq", task.FirstKeptSeq)
		}
	}
	if !resumed {
		cut := compaction.FindCutPoint(msgs, settings.KeepRecentTokens)
		cutIndex = cut.FirstKeptIndex
	}
	if cutIndex <= 0 {
		return ManualCompactResult{TokensBefore: before, TokensAfter: before, PolicyVersion: policy.Version(), Reason: "nothing-to-compact"}, ErrManualCompactNothing
	}
	if problem := manualCutUnsafe(seqs, cutIndex, prefixLen, movedStart, initialLen, isUnread); problem != "" {
		if r.CheckpointMetrics != nil {
			r.CheckpointMetrics.CheckpointFailed("conflict")
		}
		return ManualCompactResult{TokensBefore: before, TokensAfter: before, PolicyVersion: policy.Version(), Reason: "unsafe-cut"}, fmt.Errorf("%w: %s", ErrManualCompactUnsafe, problem)
	}
	// Resolve the anchor against log order, not the in-memory (prefix/reordered)
	// transcript: a cut before a moved unread input maps to an earlier-or-equal
	// committed seq, never to a later one that would hide the input. uptoSeq is
	// the next sequence; the pre-commit mapping is identical to the post-commit
	// one because the compaction event itself is excluded.
	lastSeq := events[len(events)-1].Seq
	anchor := r.checkpointAnchor(ctx, lease.Scope, cutIndex, lastSeq+1)
	if anchor <= 0 {
		if r.CheckpointMetrics != nil {
			r.CheckpointMetrics.CheckpointFailed("conflict")
		}
		return ManualCompactResult{TokensBefore: before, TokensAfter: before, PolicyVersion: policy.Version(), Reason: "unanchored-cut"}, fmt.Errorf("%w: cut is not anchored to a committed child event", ErrManualCompactUnsafe)
	}
	// Pin the selected range durably BEFORE the summary request. A process killed
	// during the model call then resumes this exact range; the row carries no
	// summary and is deleted once the event/projection are durable.
	if err := r.saveCompactionTask(ctx, lease.Scope, kernel.CompactionTaskCheckpoint{
		RequestID:     requestID,
		Phase:         kernel.CompactionPhaseSummarize,
		FirstKeptSeq:  anchor,
		TailSeq:       lastSeq,
		TokensBefore:  before,
		PolicyVersion: policy.Version(),
		Attempt:       task.Attempt,
	}); err != nil {
		return ManualCompactResult{}, fmt.Errorf("agent: save compaction task: %w", err)
	}

	providerModel := provider.Model{Provider: active.ProviderID, ID: active.Model, ContextWindow: window}
	streamConfig := provider.StreamConfig{
		APIKey:        active.APIKey,
		ThinkingLevel: active.ThinkingLevel,
		Extra:         active.RequestPatch,
	}
	var res *compaction.CompactionResult
	if resumed {
		res, err = compaction.CompactAt(ctx, summaryStream, providerModel, msgs, settings, cutIndex, -1, nil, "", streamConfig)
	} else {
		res, err = compaction.Compact(ctx, summaryStream, providerModel, msgs, settings, -1, nil, "", streamConfig)
	}
	if err != nil {
		// The summary request failed or was cancelled: the original context (the
		// event log) is untouched and nothing is written.
		r.Logger.Warn("manual compaction failed; context unchanged",
			"tenant_id", lease.Scope.TenantID, "session_id", lease.Scope.SessionID,
			"tokens_before", before, "err", err)
		if r.CheckpointMetrics != nil {
			r.CheckpointMetrics.CheckpointFailed("summary")
		}
		return ManualCompactResult{TokensBefore: before, TokensAfter: before, PolicyVersion: policy.Version(), Reason: "summary-failed"}, err
	}
	if res == nil {
		return ManualCompactResult{TokensBefore: before, TokensAfter: before, PolicyVersion: policy.Version(), Reason: "nothing-to-compact"}, ErrManualCompactNothing
	}
	// The second cut is computed from the same messages/settings; if it differs
	// or the index cannot be anchored, refuse instead of guessing.
	if problem := manualCutUnsafe(seqs, res.FirstKeptIndex, prefixLen, movedStart, initialLen, isUnread); problem != "" {
		if r.CheckpointMetrics != nil {
			r.CheckpointMetrics.CheckpointFailed("conflict")
		}
		return ManualCompactResult{TokensBefore: before, TokensAfter: before, PolicyVersion: policy.Version(), Reason: "unsafe-cut"}, fmt.Errorf("%w: %s", ErrManualCompactUnsafe, problem)
	}

	now := time.Now().UnixMilli()
	rebuilt := res.RebuildContext(msgs, now)
	tokensAfter := compaction.EstimateContextTokensConservative(rebuilt)
	summarized := len(msgs) - (len(rebuilt) - 1)
	raw, err := marshalPayload(map[string]any{
		"summary":             res.Summary,
		"summaryHash":         kernel.CheckpointSummaryHash(res.Summary),
		"tokensBefore":        res.TokensBefore,
		"tokensAfter":         tokensAfter,
		"firstKept":           res.FirstKeptIndex,
		"firstKeptTranscript": res.FirstKeptIndex,
		"keptFromSeq":         anchor,
		"summarized":          summarized,
		"kept":                len(rebuilt) - 1,
		"timestamp":           now,
		"reason":              "manual",
		"requestId":           requestID,
		"policyVersion":       policy.Version(),
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
		if r.CheckpointMetrics != nil {
			r.CheckpointMetrics.CheckpointFailed("save")
		}
		return ManualCompactResult{}, err
	}
	// Recompute against the committed sequence. The mapping is the same as the
	// preflight; a surprise answers an unanchored projection (replay falls back
	// to the event log) instead of guessing a boundary.
	committedAnchor := r.checkpointAnchor(ctx, lease.Scope, res.FirstKeptIndex, seq)
	reason := "compacted"
	if committedAnchor <= 0 {
		committedAnchor = 0
		reason = "compacted-unanchored"
	}
	anchor = committedAnchor
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
		// promise. The task row is deliberately kept: the next retry recovers the
		// projection from the committed event instead of compacting again.
		r.Logger.Warn("manual compaction checkpoint save failed",
			"tenant_id", lease.Scope.TenantID, "session_id", lease.Scope.SessionID,
			"seq", seq, "err", err)
		if r.CheckpointMetrics != nil {
			r.CheckpointMetrics.CheckpointFailed("save")
		}
		return ManualCompactResult{}, err
	}
	// The event and its projection are both durable: the in-flight task is done.
	r.deleteCompactionTask(ctx, lease.Scope, requestID)
	if r.CheckpointMetrics != nil {
		r.CheckpointMetrics.CheckpointCreated(res.TokensBefore, tokensAfter, time.Since(started))
	}
	r.pruneCheckpointsFor(ctx, lease.Scope, policy)
	r.Logger.Info("manual checkpoint created",
		"tenant_id", lease.Scope.TenantID, "session_id", lease.Scope.SessionID,
		"owner", lease.Token.Owner, "epoch", lease.Token.Epoch,
		"cut_seq", seq, "kept_from_seq", anchor,
		"tokens_before", res.TokensBefore, "tokens_after", tokensAfter,
		"policy_version", policy.Version(), "request_id", requestID,
		"duration_ms", time.Since(started).Milliseconds())
	return ManualCompactResult{
		CheckpointID:  seq,
		Performed:     true,
		TokensBefore:  res.TokensBefore,
		TokensAfter:   tokensAfter,
		KeptFromSeq:   anchor,
		PolicyVersion: policy.Version(),
		Reason:        reason,
	}, nil
}

// manualCutUnsafe rejects a cut that would summarize an unread input, start
// inside the inherited fork prefix, enter the moved-unread tail, or fail to name
// a committed child event.
func manualCutUnsafe(seqs []int64, cut, prefixLen, movedStart, initialLen int, isUnread func(int64) bool) string {
	if cut <= 0 || cut >= len(seqs) {
		return "cut outside the retained transcript"
	}
	if cut <= prefixLen {
		return "cut falls inside the inherited fork prefix"
	}
	if movedStart < initialLen && cut >= movedStart {
		return "cut enters the moved unread inputs"
	}
	for i := 0; i < cut; i++ {
		if seq := seqs[i]; seq > 0 && isUnread(seq) {
			return "cut would summarize an unread input"
		}
	}
	if seqs[cut] <= 0 {
		return "cut is not anchored to a child event"
	}
	return ""
}

// compactionContextWindow is the model window used while summarizing. The loop's
// configured window wins; the provider model description is the fallback.
func (r *Runner) compactionContextWindow() int {
	if r.Compaction != nil && r.Compaction.ContextWindow > 0 {
		return r.Compaction.ContextWindow
	}
	return 0
}
