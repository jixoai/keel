package compaction

import (
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
)

// TestPendingUserIndex pins the "never summarize the last user message" rule:
// the newest user message is protected until a visible assistant answer follows
// it. An error/aborted answer does not count (the turn produced nothing), while
// an assistant tool call does (the model has seen the prompt).
func TestPendingUserIndex(t *testing.T) {
	user := piUser("prompt")
	answer := piAssistant("answered")
	toolCall := piAssistant("working", "c1")
	result := piToolResult("c1", "done")
	failure := piAssistantWith("boom", agentcore.StopReasonError)

	cases := []struct {
		name string
		msgs []agentcore.Message
		want int
	}{
		{"trailing user", []agentcore.Message{user}, 0},
		{"user after an answered turn", []agentcore.Message{piUser("old"), answer, user}, 2},
		{"user with an error answer", []agentcore.Message{user, failure}, 0},
		{"user with a tool loop", []agentcore.Message{user, toolCall, result}, -1},
		{"answered turn", []agentcore.Message{user, answer}, -1},
		{"no user", []agentcore.Message{answer}, -1},
	}
	for _, tc := range cases {
		if got := pendingUserIndex(tc.msgs, 0); got != tc.want {
			t.Fatalf("%s: pendingUserIndex = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestFindCutPointNeverSummarizesPendingUser proves the cut backs off to the
// pending user message (or refuses a cut) instead of folding it into a summary.
func TestFindCutPointNeverSummarizesPendingUser(t *testing.T) {
	t.Run("backs off to the pending user when the budget would cut past it", func(t *testing.T) {
		msgs := []agentcore.Message{
			piUser(piText("u1", 100)),
			piAssistant(piText("a1", 100)),
			piUser(piText("pending", 100)),
		}
		if cut := FindCutPoint(msgs, 50).FirstKeptIndex; cut != 2 {
			t.Fatalf("FirstKeptIndex = %d, want 2 (the pending user heads the tail)", cut)
		}
	})

	t.Run("refuses a cut when the pending user is the first allowed message", func(t *testing.T) {
		msgs := []agentcore.Message{piUser(piText("only", 100))}
		if cut := FindCutPoint(msgs, 0).FirstKeptIndex; cut != 0 {
			t.Fatalf("FirstKeptIndex = %d, want 0 (no safe cut)", cut)
		}
	})

	t.Run("refuses a cut when a tool result trails the pending user", func(t *testing.T) {
		// A user with no assistant answer but a stray result: the result is not a
		// valid cut point and the user must not be summarized, so this pass has no
		// safe cut.
		msgs := []agentcore.Message{
			piUser(piText("u1", 100)),
			piToolResult("c1", piText("r1", 100)),
		}
		if cut := FindCutPoint(msgs, 10).FirstKeptIndex; cut != 0 {
			t.Fatalf("FirstKeptIndex = %d, want 0 (no safe cut)", cut)
		}
	})

	t.Run("allows a cut inside a long tool loop after the model saw the prompt", func(t *testing.T) {
		// The newest user was answered with a tool call, so it may be summarized;
		// the cut itself still never lands on a tool result.
		msgs := []agentcore.Message{piUser(piText("go", 100))}
		for i := 0; i < 4; i++ {
			id := "c" + string(rune('0'+i))
			msgs = append(msgs, piAssistant(piText("step", 100), id), piToolResult(id, piText("r", 100)))
		}
		cut := FindCutPoint(msgs, 150).FirstKeptIndex
		if cut == 0 {
			t.Fatal("a long tool loop after a seen prompt should still compact")
		}
		if got := msgs[cut].Role(); got == agentcore.RoleToolResult {
			t.Fatalf("cut role = %q, want never a tool result", got)
		}
	})
}
