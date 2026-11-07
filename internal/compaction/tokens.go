// Copied from github.com/smallnest/pigo@891d1f3 (MIT License).
// Upstream: internal/compaction/tokens.go
// Modified for keel: module import paths rewritten; see docs/UPSTREAM.md.

// Package compaction implements context-window token accounting and the
// decision of when a long session must be compacted, mirroring pi's
// harness/compaction/compaction.ts and the estimation helpers it imports from
// @earendil-works/pi-ai/utils/estimate.
//
// This file covers the token side: estimating a message's token footprint from
// the character heuristic, deriving the current context-token usage (preferring
// provider-reported Usage over estimation), and the compaction thresholds.
// Cut-point finding and summarization live in sibling files.
package compaction

import (
	"encoding/json"

	"github.com/jixoai/keel/internal/agentcore"
)

// CompactionSettings holds the thresholds and retention knobs for compaction,
// mirroring pi's CompactionPolicy (packages/durable/src/harness/types.ts) and
// its default DEFAULT_COMPACTION_POLICY (harness/agent.ts:28-33).
type CompactionSettings struct {
	// Enabled gates automatic compaction decisions.
	Enabled bool
	// ReserveTokens is reserved for the summary prompt and its output; the
	// effective usable window is contextWindow - ReserveTokens.
	ReserveTokens int
	// KeepRecentTokens is the approximate recent-context token budget to retain
	// after compaction (consumed by FindCutPoint).
	KeepRecentTokens int
	// BackgroundTokens is the distance below the blocking threshold at which a
	// background (non-blocking) compaction may start, matching pi's
	// backgroundTokens. Zero disables background compaction; the keel loop
	// currently only performs blocking compactions.
	BackgroundTokens int
}

// DefaultCompactionSettings matches pi's DEFAULT_COMPACTION_POLICY:
// {enabled: true, reserveTokens: 16384, keepRecentTokens: 20000,
// backgroundTokens: 32768} (harness/agent.ts:28-33).
var DefaultCompactionSettings = CompactionSettings{
	Enabled:          true,
	ReserveTokens:    16384,
	KeepRecentTokens: 20000,
	BackgroundTokens: 32768,
}

// Compaction trigger kinds, matching pi's thresholdCompaction result
// (harness/generation.ts:455-468).
const (
	// TriggerBlocking means the context crossed contextWindow - reserveTokens;
	// pi compaction waits for the summary before sending the request.
	TriggerBlocking = "blocking"
	// TriggerBackground means the context crossed the background threshold but
	// not the blocking one; pi starts the compaction without blocking the run.
	TriggerBackground = "background"
)

// estimatedImageChars is the fixed character budget attributed to an image
// block, matching pi's ESTIMATED_IMAGE_CHARS (ai/src/utils/estimate.ts:16).
const estimatedImageChars = 4800

// charsPerToken is pi's CHARS_PER_TOKEN (ai/src/utils/estimate.ts:15): 3.5
// characters per token. pi divides by 3.5 and rounds up; 3.5 = charsPerTokenNum
// / charsPerTokenDen, so ceil(chars/3.5) is computed exactly in integers as
// ceil(chars * charsPerTokenDen / charsPerTokenNum).
const (
	charsPerTokenNum = 7
	charsPerTokenDen = 2
)

// estimateCharsTokens converts a character footprint to tokens the way pi's
// estimateTokens does: ceil(chars / 3.5).
func estimateCharsTokens(chars int) int {
	if chars <= 0 {
		return 0
	}
	return (chars*charsPerTokenDen + charsPerTokenNum - 1) / charsPerTokenNum
}

// contentListChars sums the character footprint of a content list, counting
// text/thinking/toolCall blocks by their text length and each image block as a
// fixed estimatedImageChars, mirroring pi's estimateTextAndImageContentChars
// plus its assistant-block handling (ai/src/utils/estimate.ts:30-68).
func contentListChars(content agentcore.ContentList) int {
	chars := 0
	for _, block := range content {
		switch c := block.(type) {
		case agentcore.TextContent:
			chars += len(c.Text)
		case agentcore.ThinkingContent:
			chars += len(c.Thinking)
		case agentcore.ToolCallContent:
			// name + serialized arguments, matching pi's toolCall accounting.
			chars += len(c.Name)
			if len(c.Arguments) > 0 {
				chars += len(c.Arguments)
			} else {
				// nil/empty RawMessage serializes to "null" downstream.
				b, _ := json.Marshal(json.RawMessage(c.Arguments))
				chars += len(b)
			}
		case agentcore.ImageContent:
			chars += estimatedImageChars
		}
	}
	return chars
}

// EstimateTokens returns a conservative token estimate for one message using
// pi's character heuristic (ceil(chars / 3.5), ai/src/utils/estimate.ts:46-69).
func EstimateTokens(msg agentcore.Message) int {
	switch m := msg.(type) {
	case agentcore.UserMessage:
		return estimateCharsTokens(contentListChars(m.Content))
	case agentcore.AssistantMessage:
		return estimateCharsTokens(contentListChars(m.Content))
	case agentcore.ToolResultMessage:
		return estimateCharsTokens(contentListChars(m.Content))
	case agentcore.CompactionMessage:
		// pi stores a checkpoint as a user message carrying the wrapped summary
		// (harness/compaction.ts:450-455), so estimate that rendering rather
		// than the bare summary text.
		return estimateCharsTokens(len(agentcore.ContentToText(m.AsUserMessage().Content)))
	default:
		return 0
	}
}

// excludedFromContext reports whether pi drops a message from the model context
// view it estimates and selects cuts over: an assistant answer that ended in
// error or was aborted contributes nothing and is never a cut candidate
// (harness/compaction.ts:305-325 filters those contributions; the same filter
// feeds estimateContext).
//
// pi also filters a "deferred" stop reason; keel has no deferred responses yet,
// so there is nothing to mirror for it.
func excludedFromContext(msg agentcore.Message) bool {
	a, ok := msg.(agentcore.AssistantMessage)
	if !ok {
		return false
	}
	return a.StopReason == agentcore.StopReasonError || a.StopReason == agentcore.StopReasonAborted
}

// messageTimestamp reads the timestamp every message kind carries.
func messageTimestamp(msg agentcore.Message) int64 {
	switch m := msg.(type) {
	case agentcore.UserMessage:
		return m.Timestamp
	case agentcore.AssistantMessage:
		return m.Timestamp
	case agentcore.ToolResultMessage:
		return m.Timestamp
	case agentcore.CompactionMessage:
		return m.Timestamp
	default:
		return 0
	}
}

// calculateContextTokens derives total context tokens from a provider usage
// block, matching pi's calculateContextTokens (ai/src/utils/estimate.ts:18-20):
// input + output + cacheRead + cacheWrite. The four input-side counters are
// mutually exclusive: provider decoders subtract cache subsets from raw
// prompt/input totals (OpenAI), while Anthropic reports them separately.
// Reasoning tokens are not added because providers report them inside output.
func calculateContextTokens(u agentcore.Usage) int {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// usageApplies reports whether an assistant message's usage may describe the
// current request prefix, mirroring pi's getAssistantUsage
// (ai/src/utils/estimate.ts:71-95): an aborted/error answer and a zero-token
// block are unusable.
func usageApplies(a agentcore.AssistantMessage) bool {
	if a.Usage == nil {
		return false
	}
	if a.StopReason == agentcore.StopReasonAborted || a.StopReason == agentcore.StopReasonError {
		return false
	}
	return calculateContextTokens(*a.Usage) > 0
}

// minTimestamp is pi's Number.NEGATIVE_INFINITY for the applicable-prefix test.
const minTimestamp int64 = -1 << 63

// lastAssistantUsage returns the usage block of the newest assistant message
// that may describe the current prefix, plus its index. It follows pi's
// getLastAssistantUsageInfo (ai/src/utils/estimate.ts:71-95): a newer message
// inserted after a response (a compaction summary, for instance) invalidates
// that response's usage, because the usage was measured over a different
// prefix.
func lastAssistantUsage(msgs []agentcore.Message) (agentcore.Usage, int, bool) {
	latestPrefixTimestamp := minTimestamp
	idx := -1
	var usage agentcore.Usage
	for i, msg := range msgs {
		if a, ok := msg.(agentcore.AssistantMessage); ok {
			if a.Timestamp >= latestPrefixTimestamp && usageApplies(a) {
				usage, idx = *a.Usage, i
			}
		}
		if ts := messageTimestamp(msg); ts > latestPrefixTimestamp {
			latestPrefixTimestamp = ts
		}
	}
	return usage, idx, idx >= 0
}

// lastCompactionIndex returns the index of the newest compaction checkpoint in
// msgs, or -1 when the list has none.
func lastCompactionIndex(msgs []agentcore.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if _, ok := msgs[i].(agentcore.CompactionMessage); ok {
			return i
		}
	}
	return -1
}

// ContextUsageEstimate reports the derived context-token usage for a message
// list, mirroring pi's ContextUsageEstimate (ai/src/utils/estimate.ts:4-13).
type ContextUsageEstimate struct {
	// Tokens is the estimated total context tokens.
	Tokens int
	// UsageTokens is the tokens reported by the most recent applicable assistant
	// usage block.
	UsageTokens int
	// TrailingTokens is the estimated tokens after that usage block.
	TrailingTokens int
	// LastUsageIndex is the index of the message that provided usage, or -1 when
	// none exists.
	LastUsageIndex int
}

// EstimateContextTokens computes context-token usage for messages, preferring
// the most recent valid assistant Usage block and estimating only the messages
// that follow it. When no usage is available it estimates every message. This
// mirrors pi's estimateContextTokens (ai/src/utils/estimate.ts:97-112) together
// with estimateContext's head rule (harness/compaction.ts:337-352): usage
// measured at or before the newest summary marker cannot describe the compacted
// prefix, so it is ignored.
func EstimateContextTokens(msgs []agentcore.Message) ContextUsageEstimate {
	usage, lastIdx, ok := lastAssistantUsage(msgs)
	if ok {
		if head := lastCompactionIndex(msgs); lastIdx <= head {
			ok = false
		}
	}

	if !ok {
		estimated := 0
		for _, m := range msgs {
			if excludedFromContext(m) {
				continue
			}
			estimated += EstimateTokens(m)
		}
		return ContextUsageEstimate{
			Tokens:         estimated,
			UsageTokens:    0,
			TrailingTokens: estimated,
			LastUsageIndex: -1,
		}
	}

	usageTokens := calculateContextTokens(usage)
	trailing := 0
	for i := lastIdx + 1; i < len(msgs); i++ {
		if excludedFromContext(msgs[i]) {
			continue
		}
		trailing += EstimateTokens(msgs[i])
	}
	return ContextUsageEstimate{
		Tokens:         usageTokens + trailing,
		UsageTokens:    usageTokens,
		TrailingTokens: trailing,
		LastUsageIndex: lastIdx,
	}
}

// CompactionTrigger classifies where contextTokens sits relative to the
// compaction thresholds, mirroring pi's thresholdCompaction
// (harness/generation.ts:455-468): blocking above `contextWindow -
// reserveTokens`, background above `blocking - backgroundTokens` when
// background compaction is enabled, and nothing below both. The comparison is
// strict, exactly as in pi. Disabled settings or an unknown (non-positive)
// context window never trigger.
func CompactionTrigger(contextTokens, contextWindow int, settings CompactionSettings) (kind string, ok bool) {
	if !settings.Enabled || contextWindow <= 0 {
		return "", false
	}
	blocking := contextWindow - settings.ReserveTokens
	if contextTokens > blocking {
		return TriggerBlocking, true
	}
	if settings.BackgroundTokens > 0 && contextTokens > blocking-settings.BackgroundTokens {
		return TriggerBackground, true
	}
	return "", false
}

// ShouldCompact reports whether context usage has crossed the blocking
// threshold that makes the run wait for a compaction, matching pi:
// contextTokens > contextWindow - reserveTokens. Disabled settings or a
// non-positive contextWindow (unknown) never trigger compaction.
func ShouldCompact(contextTokens, contextWindow int, settings CompactionSettings) bool {
	kind, ok := CompactionTrigger(contextTokens, contextWindow, settings)
	return ok && kind == TriggerBlocking
}
