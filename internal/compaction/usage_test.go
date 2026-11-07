package compaction

import (
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
)

// Provider decoders hand compaction mutually exclusive input-side counters, so
// the context estimate is a plain sum. A cached OpenAI response must not be
// counted twice (prompt total + cache read).
func TestCalculateContextTokensSumsExclusiveCounters(t *testing.T) {
	usage := agentcore.Usage{
		InputTokens:      50,
		OutputTokens:     40,
		CacheReadTokens:  30,
		CacheWriteTokens: 20,
		ReasoningTokens:  10,
	}
	if got := calculateContextTokens(usage); got != 140 {
		t.Fatalf("context tokens = %d, want 140", got)
	}
}

func TestCalculateContextTokensWithoutCacheIsInputPlusOutput(t *testing.T) {
	usage := agentcore.Usage{InputTokens: 7, OutputTokens: 3}
	if got := calculateContextTokens(usage); got != 10 {
		t.Fatalf("context tokens = %d, want 10", got)
	}
}
