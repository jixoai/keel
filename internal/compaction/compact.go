// Copied from github.com/smallnest/pigo@891d1f3 (MIT License).
// Upstream: internal/compaction/compact.go
// Modified for keel: module import paths rewritten; see docs/UPSTREAM.md.

// This file (US-003) ties the compaction pieces together: given a message list
// and settings, it finds the cut point, extracts file operations from the
// summarized range, generates the structured summary, and returns a
// CompactionResult ready to be persisted as a session compaction entry and used
// to rebuild the agent context. Mirrors pi's prepareCompaction + compact.
package compaction

import (
	"context"
	"encoding/json"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/provider"
)

// CompactionDetails records the files touched in the compacted history, stored
// alongside a compaction entry so a later iterative compaction can seed its
// file lists. Mirrors pi's CompactionDetails.
type CompactionDetails struct {
	// ReadFiles are files read but not modified in the compacted range.
	ReadFiles []string `json:"readFiles"`
	// ModifiedFiles are files written or edited in the compacted range.
	ModifiedFiles []string `json:"modifiedFiles"`
}

// CompactionResult is the outcome of a compaction: the summary text (with file
// metadata appended), the watermark where retained history begins, how many
// messages the summary covers, the estimated tokens before compaction, and the
// extracted file details. Mirrors pi's CompactionResult, adapted to keel's flat
// message list (index vs entry id).
type CompactionResult struct {
	// Summary is the structured summary that replaces the compacted history.
	Summary string
	// FirstKeptIndex is the watermark: the index of the first retained message,
	// keel's equivalent of pi's CompactionEntry.head (the first kept entry id).
	FirstKeptIndex int
	// CoveredMessages is the number of messages the summary stands in for: the
	// range [previous checkpoint or 0, FirstKeptIndex). Like pi's
	// `entries: view.entries.slice(0, cut)` (harness/compaction.ts:132-140), it
	// includes an earlier summary marker when one is being folded in.
	CoveredMessages int
	// TokensBefore is the estimated context tokens before compaction.
	TokensBefore int
	// Details holds the file operations extracted from the compacted range.
	Details CompactionDetails
}

// Compact prepares and generates a compaction over msgs. It cuts at
// findCutPoint(msgs, settings.KeepRecentTokens) and summarizes the messages
// before the cut, seeding file ops from a prior compaction's details when
// provided, and appends <read-files>/<modified-files> metadata to the summary
// (the metadata blocks are a keel/pigo extension: pi's CompactionEntry stores
// the bare summary).
//
// prevCompactionIndex is the index of the last already-applied compaction
// checkpoint (-1 when none). Like pi, whose context view starts at its head
// marker (harness/compaction.ts:287), the messages after that checkpoint are
// the only candidates for the new cut, but the checkpoint itself is still part
// of the summarized range so the newer summary folds the older one in
// (harness/compaction.ts:132-140). prevDetails carries that compaction's file
// lists so long-lived reads/edits survive across successive compactions.
//
// previousSummary, when non-empty, additionally embeds that text in
// <previous-summary> tags and switches to keel's update template; pi has no such
// channel and callers that pass "" get pi's exact summarization request.
//
// It returns (nil, nil) when there is nothing to compact (no valid cut point, or
// nothing but the previous checkpoint would be summarized), matching pi's
// "completes without a summary when there is nothing to compact".
func Compact(
	ctx context.Context,
	stream provider.StreamFn,
	model provider.Model,
	msgs []agentcore.Message,
	settings CompactionSettings,
	prevCompactionIndex int,
	prevDetails *CompactionDetails,
	previousSummary string,
	cfg provider.StreamConfig,
) (*CompactionResult, error) {
	first := prevCompactionIndex + 1
	if first < 0 {
		first = 0
	}
	cut := findCutPoint(msgs, settings.KeepRecentTokens, first)
	if cut.FirstKeptIndex <= first {
		// No cut, or the cut would cover only the previous checkpoint.
		return nil, nil
	}

	start := first
	if prevCompactionIndex >= 0 {
		start = prevCompactionIndex
	}
	// pi's summary source is built from the context view's contributions, which
	// drop error/aborted assistant answers before anything is serialized
	// (harness/context.ts:283-289, harness/compaction.ts:328-330).
	toSummarize := make([]agentcore.Message, 0, cut.FirstKeptIndex-start)
	for _, m := range msgs[start:cut.FirstKeptIndex] {
		if excludedFromContext(m) {
			continue
		}
		toSummarize = append(toSummarize, m)
	}

	// Seed file ops from the previous compaction, then fold in this range.
	ops := NewFileOps()
	if prevDetails != nil {
		for _, f := range prevDetails.ReadFiles {
			ops.Read[f] = struct{}{}
		}
		for _, f := range prevDetails.ModifiedFiles {
			ops.Edited[f] = struct{}{}
		}
	}
	for _, m := range toSummarize {
		extractFileOpsFromMessage(m, ops)
	}
	readFiles, modifiedFiles := computeFileLists(ops)

	summary, err := GenerateSummary(ctx, stream, model, toSummarize, settings.ReserveTokens, previousSummary, cfg)
	if err != nil {
		return nil, err
	}
	summary += formatFileOperations(readFiles, modifiedFiles)

	return &CompactionResult{
		Summary:         summary,
		FirstKeptIndex:  cut.FirstKeptIndex,
		CoveredMessages: cut.FirstKeptIndex - start,
		TokensBefore:    EstimateContextTokens(msgs).Tokens,
		Details:         CompactionDetails{ReadFiles: readFiles, ModifiedFiles: modifiedFiles},
	}, nil
}

// Message builds the CompactionMessage to persist for this result: the summary
// text plus the estimated tokens-before and the file details (as raw JSON).
func (r *CompactionResult) Message(now int64) agentcore.CompactionMessage {
	var details json.RawMessage
	if b, err := json.Marshal(r.Details); err == nil {
		details = b
	}
	return agentcore.CompactionMessage{
		RoleField:    agentcore.RoleCompaction,
		Summary:      r.Summary,
		TokensBefore: r.TokensBefore,
		Details:      details,
		Timestamp:    now,
	}
}

// RebuildContext returns the post-compaction message list: the compaction
// checkpoint followed by the retained recent messages (msgs[FirstKeptIndex:]).
// The summarized prefix is dropped and replaced by the single checkpoint,
// keeping context continuous while staying within the window. Mirrors pi's
// post-compact context reconstruction (summary entry + retained tail).
func (r *CompactionResult) RebuildContext(msgs []agentcore.Message, now int64) agentcore.MessageList {
	out := make(agentcore.MessageList, 0, len(msgs)-r.FirstKeptIndex+1)
	out = append(out, r.Message(now))
	out = append(out, msgs[r.FirstKeptIndex:]...)
	return out
}
