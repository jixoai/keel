// Copied from github.com/smallnest/pigo@891d1f3 (MIT License).
// Upstream: internal/compaction/cutpoint.go
// Modified for keel: module import paths rewritten; see docs/UPSTREAM.md.

package compaction

import "github.com/jixoai/keel/internal/agentcore"

// CutPointResult describes the cut selected for compaction, mirroring pi's
// CutPointResult (adapted to keel's flat message list).
type CutPointResult struct {
	// FirstKeptIndex is the index of the first message retained after
	// compaction; everything before it is summarized. Zero means there is no
	// cut: pi's selectCut returns undefined when the retained budget never fills
	// or when only a previous summary marker would be summarized
	// (harness/compaction.ts:285-303), and a cut at index zero has nothing to
	// summarize either.
	FirstKeptIndex int
	// TurnStartIndex is the index of the user message that starts the turn the
	// cut falls inside, or -1 when the cut lands on a clean turn boundary.
	TurnStartIndex int
	// IsSplitTurn reports whether the cut splits an in-progress assistant turn.
	IsSplitTurn bool
}

// isValidCutPoint reports whether the message at idx may head the kept range,
// mirroring pi's isCandidate (harness/compaction.ts:305-325). A cut may land on
// a user or assistant message but never on a toolResult (it must stay attached
// to the toolCall that produced it); nor on an error/aborted assistant answer,
// which pi drops from the context view before selecting a cut.
func isValidCutPoint(msgs []agentcore.Message, idx int) bool {
	switch msgs[idx].Role() {
	case agentcore.RoleUser:
		// pi refuses a user entry that a result of the preceding assistant's
		// calls still follows (harness/compaction.ts:309-316, 317-324).
		return !hasDanglingToolResult(msgs, idx)
	case agentcore.RoleAssistant:
		return !excludedFromContext(msgs[idx])
	default: // toolResult, compaction checkpoints, and custom kinds are not cuttable.
		return false
	}
}

// hasDanglingToolResult reports whether a toolResult answering the closest
// preceding assistant's calls still follows idx before the next assistant
// message. Cutting there would strand the result away from its call, so pi
// moves the cut to the next assistant (harness/compaction.ts:317-324).
func hasDanglingToolResult(msgs []agentcore.Message, idx int) bool {
	calls := map[string]struct{}{}
	for before := idx - 1; before >= 0; before-- {
		a, ok := msgs[before].(agentcore.AssistantMessage)
		// pi's contributions drop error/aborted answers, so the nearest visible
		// assistant is what its backward scan finds.
		if !ok || excludedFromContext(a) {
			continue
		}
		for _, call := range a.ToolCalls() {
			calls[call.ID] = struct{}{}
		}
		break
	}
	if len(calls) == 0 {
		return false
	}
	for after := idx; after < len(msgs); after++ {
		switch m := msgs[after].(type) {
		case agentcore.AssistantMessage:
			// A later visible assistant ends the turn this user entry steered:
			// the result cannot belong to the preceding call any more.
			if after > idx && !excludedFromContext(m) {
				return false
			}
		case agentcore.ToolResultMessage:
			if _, ok := calls[m.ToolCallID]; ok {
				return true
			}
		}
	}
	return false
}

// findTurnStartIndex scans backwards from idx to find the user message that
// starts the turn containing idx, returning -1 if none is found.
func findTurnStartIndex(msgs []agentcore.Message, idx int) int {
	for i := idx; i >= 0; i-- {
		if msgs[i].Role() == agentcore.RoleUser {
			return i
		}
	}
	return -1
}

// FindCutPoint finds the compaction cut that keeps approximately
// keepRecentTokens worth of the most recent messages. It is the flat-list form
// of pi's selectCut (harness/compaction.ts:285-303): it accumulates token
// estimates from the newest message backwards, and once the retained budget is
// reached snaps to the first valid cut point at or after that message (or the
// last valid cut point when the budget is reached inside the final tool
// result), never splitting a toolCall from its toolResult.
//
// It returns a zero FirstKeptIndex when no cut exists: the budget is never
// reached (nothing is old enough to compact) or the only messages before the
// cut would be empty/excluded ones.
func FindCutPoint(msgs []agentcore.Message, keepRecentTokens int) CutPointResult {
	return findCutPoint(msgs, keepRecentTokens, 0)
}

// findCutPoint is FindCutPoint with a lower bound: only messages at or after
// from may be candidates or count toward the retained budget. Compact passes
// the index just past the previous checkpoint, because pi's view starts at its
// head marker (harness/compaction.ts:287).
func findCutPoint(msgs []agentcore.Message, keepRecentTokens, from int) CutPointResult {
	noCut := CutPointResult{FirstKeptIndex: 0, TurnStartIndex: -1, IsSplitTurn: false}
	if from < 0 {
		from = 0
	}

	var candidates []int
	for i := from; i < len(msgs); i++ {
		if isValidCutPoint(msgs, i) {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return noCut
	}

	cut := -1
	kept := 0
	for i := len(msgs) - 1; i >= from; i-- {
		// pi's view drops error/aborted answers before summing contributions
		// (harness/compaction.ts:305), so they add no tokens.
		if !excludedFromContext(msgs[i]) {
			kept += EstimateTokens(msgs[i])
		}
		if kept < keepRecentTokens {
			continue
		}
		// Snap to the first candidate at or after this message; when the budget
		// is reached inside trailing tool results, keep them with their call by
		// cutting at the last candidate (pi: candidates.at(-1)).
		for _, c := range candidates {
			if c >= i {
				cut = c
				break
			}
		}
		if cut < 0 {
			cut = candidates[len(candidates)-1]
		}
		break
	}
	if cut < 0 {
		// The budget never filled: everything is recent enough to keep.
		return noCut
	}

	// pi requires a real message before the cut; a range holding only an
	// earlier summary marker (or nothing) is not worth compacting
	// (harness/compaction.ts:301-302).
	hasContent := false
	for i := from; i < cut; i++ {
		if !excludedFromContext(msgs[i]) {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return noCut
	}

	cutOnUser := msgs[cut].Role() == agentcore.RoleUser
	turnStart := -1
	if !cutOnUser {
		turnStart = findTurnStartIndex(msgs, cut)
	}

	return CutPointResult{
		FirstKeptIndex: cut,
		TurnStartIndex: turnStart,
		IsSplitTurn:    !cutOnUser && turnStart != -1,
	}
}
