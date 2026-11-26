package agent

// presentUnreadInputLast is the ordering seam between the event log and the
// model request. A queued unread input is usually already last; only a genuine
// reorder may be reported as a moved block, because the checkpoint anchor
// treats that block as unmapable against log-order seqs.

import (
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
)

func unreadMessage(text string) agentcore.UserMessage {
	return agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewTextContent(text)},
	}
}

func TestPresentUnreadInputLastAlreadyAtTailDoesNotMove(t *testing.T) {
	msgs := agentcore.MessageList{
		unreadMessage("first"),
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("answer")}},
		unreadMessage("queued"),
	}
	seqs := []int64{1, 2, 3}

	out, outSeqs, movedStart := presentUnreadInputLast(msgs, seqs, func(seq int64) bool {
		return seq == 3
	})
	if movedStart != len(msgs) {
		t.Fatalf("movedStart = %d, want %d (already at the tail is not a reorder)", movedStart, len(msgs))
	}
	if len(out) != len(msgs) || len(outSeqs) != len(seqs) {
		t.Fatalf("length changed: %d/%d -> %d/%d", len(msgs), len(seqs), len(out), len(outSeqs))
	}
	for i := range outSeqs {
		if outSeqs[i] != seqs[i] {
			t.Fatalf("seqs[%d] = %d, want %d", i, outSeqs[i], seqs[i])
		}
	}
}

func TestPresentUnreadInputLastRealReorderIsReported(t *testing.T) {
	// log order: user1, user2 (unread), assistant1 — the old run's answer sits
	// after the input that arrived while it ran.
	msgs := agentcore.MessageList{
		unreadMessage("first"),
		unreadMessage("arrived while running"),
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("answer to first")}},
	}
	seqs := []int64{1, 2, 3}

	out, outSeqs, movedStart := presentUnreadInputLast(msgs, seqs, func(seq int64) bool {
		return seq == 2
	})
	if movedStart != 2 {
		t.Fatalf("movedStart = %d, want 2 (the moved block starts after the kept prefix)", movedStart)
	}
	if len(out) != 3 || len(outSeqs) != 3 {
		t.Fatalf("length changed: %d/%d", len(out), len(outSeqs))
	}
	if outSeqs[0] != 1 || outSeqs[1] != 3 || outSeqs[2] != 2 {
		t.Fatalf("order = %v, want [1 3 2]", outSeqs)
	}
	if out[2].Role() != agentcore.RoleUser {
		t.Fatalf("tail role = %s, want user", out[2].Role())
	}
}
