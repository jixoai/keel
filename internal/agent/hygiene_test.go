package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/spill"
)

type prePassSpill struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newPrePassSpill() *prePassSpill { return &prePassSpill{data: map[string][]byte{}} }

func (s *prePassSpill) Spill(_ context.Context, _ spill.Scope, mime string, data []byte) (spill.Ref, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	digest := spill.Digest(data)
	id := "pre-" + digest[:12]
	s.data[id] = append([]byte(nil), data...)
	return spill.Ref{ID: id, MIME: mime, SHA256: digest, Size: int64(len(data))}, nil
}

func (s *prePassSpill) Load(_ context.Context, _ spill.Scope, ref spill.Ref) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.data[ref.ID]...), nil
}

// TestRunnerHygienePrePassOffloadsBeforeImageResolution pins the placement: the
// rebuilt transcript is pruned/offloaded before the runner resolves image
// references into wire bytes, so an old image is never uploaded or inlined on a
// resumed attempt.
func TestRunnerHygienePrePassOffloadsBeforeImageResolution(t *testing.T) {
	store := newPrePassSpill()
	runner := &Runner{
		Logger: discardLog(),
		Compaction: &CompactionOptions{
			Settings: compaction.CompactionSettings{Enabled: true},
			Hygiene: compaction.HygieneSettings{
				Enabled:            true,
				KeepRecentTokens:   20,
				MinToolResultBytes: 128,
				PreviewBytes:       32,
				OffloadImages:      true,
				ImageRecentTokens:  20,
			},
			Spill: store,
		},
	}
	lease := kernel.Lease{Scope: kernel.Scope{TenantID: uuid.New(), SessionID: uuid.New()}}
	large := strings.Repeat("z", 1000)
	msgs := agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{
			agentcore.NewTextContent("look"),
			agentcore.ImageRefContent{Type: agentcore.ContentTypeImageRef, BlobID: "blob-old", MimeType: "image/png", Size: 2048},
		}},
		agentcore.ToolResultMessage{RoleField: agentcore.RoleToolResult, ToolCallID: "c1", ToolName: "bash",
			Content: agentcore.ContentList{agentcore.NewTextContent(large)}},
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("next")}},
	}

	out := runner.applyHygienePrePass(context.Background(), msgs, lease)
	if len(out) != len(msgs) {
		t.Fatalf("message count = %d, want %d", len(out), len(msgs))
	}
	first := agentcore.ContentToText(out[0].(agentcore.UserMessage).Content)
	if !strings.Contains(first, compaction.ImageReferencePrefix) || !strings.Contains(first, "blob-old") {
		t.Fatalf("old image was not offloaded: %q", first)
	}
	if _, ok := out[0].(agentcore.UserMessage).Content[1].(agentcore.ImageRefContent); ok {
		t.Fatal("the old image reference is still resolvable")
	}
	second := agentcore.ContentToText(out[1].(agentcore.ToolResultMessage).Content)
	if !strings.HasPrefix(second, spill.ReferencePrefix) {
		t.Fatalf("old tool result was not pruned: %q", second)
	}
	// The caller's transcript is untouched.
	if got := agentcore.ContentToText(msgs[1].(agentcore.ToolResultMessage).Content); got != large {
		t.Fatal("the pre-pass mutated the caller's transcript")
	}
	if _, ok := msgs[0].(agentcore.UserMessage).Content[1].(agentcore.ImageRefContent); !ok {
		t.Fatal("the pre-pass mutated the caller's image reference")
	}
}

// TestRunnerHygienePrePassDisabledWithoutCompaction keeps the master gates
// honest: no compaction options, no rewrite.
func TestRunnerHygienePrePassDisabledWithoutCompaction(t *testing.T) {
	runner := &Runner{Logger: discardLog()}
	lease := kernel.Lease{Scope: kernel.Scope{TenantID: uuid.New(), SessionID: uuid.New()}}
	large := strings.Repeat("z", 1000)
	msgs := agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("go")}},
		agentcore.ToolResultMessage{RoleField: agentcore.RoleToolResult, ToolCallID: "c1", ToolName: "bash",
			Content: agentcore.ContentList{agentcore.NewTextContent(large)}},
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("next")}},
	}
	out := runner.applyHygienePrePass(context.Background(), msgs, lease)
	if got := agentcore.ContentToText(out[1].(agentcore.ToolResultMessage).Content); got != large {
		t.Fatal("hygiene rewrote a transcript without compaction options")
	}
}
