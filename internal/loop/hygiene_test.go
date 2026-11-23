package loop

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/spill"
)

// loopSpill is the in-memory durable store for the loop integration test.
type loopSpill struct {
	mu   sync.Mutex
	data map[string][]byte
	refs map[string]spill.Ref
}

func newLoopSpill() *loopSpill {
	return &loopSpill{data: map[string][]byte{}, refs: map[string]spill.Ref{}}
}

func (s *loopSpill) Spill(_ context.Context, _ spill.Scope, mime string, data []byte) (spill.Ref, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	digest := spill.Digest(data)
	id := "loop-" + digest[:12]
	if ref, ok := s.refs[id]; ok {
		return ref, nil
	}
	ref := spill.Ref{ID: id, MIME: mime, SHA256: digest, Size: int64(len(data))}
	s.refs[id] = ref
	s.data[id] = append([]byte(nil), data...)
	return ref, nil
}

func (s *loopSpill) Load(_ context.Context, _ spill.Scope, ref spill.Ref) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.data[ref.ID]...), nil
}

// TestLoopAppliesMemoryHygieneAtTurnBoundary drives the real loop for two
// turns: the second provider request must carry the bounded spill reference
// instead of the old large tool result, while the run's input (the ledger view)
// keeps the original message.
func TestLoopAppliesMemoryHygieneAtTurnBoundary(t *testing.T) {
	store := newLoopSpill()
	large := strings.Repeat("tool output line\n", 300) // 5400 bytes
	original := agentcore.ToolResultMessage{
		RoleField:  agentcore.RoleToolResult,
		ToolCallID: "call-1",
		ToolName:   "bash",
		Content:    agentcore.ContentList{agentcore.NewTextContent(large)},
	}
	input := agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("run it")}},
		original,
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("what happened?")}},
	}

	var requests []provider.LlmContext
	responses := []agentcore.AssistantMessage{
		assistantText("first answer", agentcore.StopReasonEndTurn),
		assistantText("second answer", agentcore.StopReasonEndTurn),
	}
	next := 0
	stream := func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		requests = append(requests, llm)
		msg := responses[next]
		next++
		s := provider.NewAssistantMessageEventStream(4)
		go func() {
			defer s.Close()
			_ = s.Emit(ctx, provider.StreamStartEvent{Partial: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}})
			_ = s.Emit(ctx, provider.StreamTextEvent{Partial: msg})
			_ = s.Emit(ctx, provider.StreamDoneEvent{Message: msg})
		}()
		return s, nil
	}

	followUps := 0
	var reports []compaction.HygieneReport
	cfg := RunConfig{
		LoopConfig: LoopConfig{
			Model: "fake", Provider: "fake", Stream: stream,
			Compaction: compaction.CompactionSettings{Enabled: true, KeepRecentTokens: 20},
			Hygiene: compaction.HygieneSettings{
				Enabled:            true,
				KeepRecentTokens:   20,
				MinToolResultBytes: 256,
				PreviewBytes:       48,
				OffloadImages:      false,
			},
			Spill:      store,
			SpillScope: spill.Scope{TenantID: "tenant", SessionID: "session"},
			OnHygiene: func(_ context.Context, report compaction.HygieneReport) {
				reports = append(reports, report)
			},
		},
		EventBuffer: 8,
		GetFollowUpMessages: func(context.Context, *agentcore.AgentContext) []agentcore.AgentMessage {
			followUps++
			if followUps > 1 {
				return nil
			}
			return []agentcore.AgentMessage{
				agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("continue")}},
			}
		},
	}

	events, result := runLoopOnce(t, cfg, input...)
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(requests))
	}
	// First request: the large result is still present (nothing is old yet).
	firstText := providerMessagesText(requests[0].Messages)
	if !strings.Contains(firstText, large) {
		t.Fatal("the first request unexpectedly lost the tool result")
	}
	// Second request: the old result is a bounded reference.
	secondText := providerMessagesText(requests[1].Messages)
	if strings.Contains(secondText, large) {
		t.Fatal("the second request still carries the full tool result")
	}
	if !strings.Contains(secondText, spill.ReferencePrefix) || !strings.Contains(secondText, "recoverable by ref") {
		t.Fatalf("second request has no bounded reference:\n%s", secondText)
	}
	if len(reports) != 1 || len(reports[0].Pruned) != 1 {
		t.Fatalf("hygiene reports = %+v, want one pruned result", reports)
	}
	pruned := reports[0].Pruned[0]
	if reports[0].TokensAfter >= reports[0].TokensBefore {
		t.Fatalf("token estimate did not drop: %d → %d", reports[0].TokensBefore, reports[0].TokensAfter)
	}
	// The exact payload is recoverable from the durable store.
	payload, err := store.Load(context.Background(), spill.Scope{}, pruned.Ref)
	if err != nil {
		t.Fatalf("load spilled payload: %v", err)
	}
	var recovered agentcore.ToolResultMessage
	if err := json.Unmarshal(payload, &recovered); err != nil {
		t.Fatalf("decode spilled tool result: %v", err)
	}
	if got := agentcore.ContentToText(recovered.Content); got != large {
		t.Fatalf("recovered %d bytes, want %d", len(got), len(large))
	}
	// The input list the caller handed to the loop is the ledger view; it must
	// keep the original message. (The loop rewrites a copy.)
	kept := input[1].(agentcore.ToolResultMessage)
	if got := agentcore.ContentToText(kept.Content); got != large {
		t.Fatal("the loop mutated the caller's transcript instead of a copy")
	}
	// The run produced the two assistant answers (the follow-up prompt may also
	// appear in the result: it is an input this run appended).
	answers := 0
	for _, msg := range result {
		if _, ok := msg.(agentcore.AssistantMessage); ok {
			answers++
		}
	}
	if answers != 2 {
		t.Fatalf("run result = %d assistant answers, want 2 (%d messages)", answers, len(result))
	}
	if _, ok := events[len(events)-1].(agentcore.AgentEndEvent); !ok {
		t.Fatalf("last event = %T, want agent_end", events[len(events)-1])
	}
}

// TestLoopHygieneRequiresCompaction: hygiene is a compaction extension, so it
// does not run with compaction disabled.
func TestLoopHygieneRequiresCompaction(t *testing.T) {
	store := newLoopSpill()
	large := strings.Repeat("x", 2000)
	var requested string
	stream := func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		requested = providerMessagesText(llm.Messages)
		s := provider.NewAssistantMessageEventStream(2)
		go func() {
			defer s.Close()
			msg := assistantText("ok", agentcore.StopReasonEndTurn)
			_ = s.Emit(ctx, provider.StreamStartEvent{Partial: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}})
			_ = s.Emit(ctx, provider.StreamDoneEvent{Message: msg})
		}()
		return s, nil
	}
	reported := false
	cfg := RunConfig{
		LoopConfig: LoopConfig{
			Model: "fake", Provider: "fake", Stream: stream,
			Compaction: compaction.CompactionSettings{Enabled: false},
			Hygiene: compaction.HygieneSettings{
				Enabled: true, KeepRecentTokens: 10, MinToolResultBytes: 100,
			},
			Spill: store,
			OnHygiene: func(context.Context, compaction.HygieneReport) {
				reported = true
			},
		},
		EventBuffer: 4,
	}
	runLoopOnce(t, cfg,
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("go")}},
		agentcore.ToolResultMessage{RoleField: agentcore.RoleToolResult, ToolCallID: "c", ToolName: "bash", Content: agentcore.ContentList{agentcore.NewTextContent(large)}},
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("next")}},
	)
	if reported {
		t.Fatal("hygiene ran with compaction disabled")
	}
	if !strings.Contains(requested, large) {
		t.Fatal("content was pruned with compaction disabled")
	}
}

// providerMessagesText flattens every text block in a provider request.
func providerMessagesText(msgs agentcore.MessageList) string {
	var b strings.Builder
	for _, msg := range msgs {
		switch m := msg.(type) {
		case agentcore.UserMessage:
			b.WriteString(agentcore.ContentToText(m.Content))
		case agentcore.AssistantMessage:
			b.WriteString(agentcore.ContentToText(m.Content))
		case agentcore.ToolResultMessage:
			b.WriteString(agentcore.ContentToText(m.Content))
		case agentcore.CompactionMessage:
			b.WriteString(m.Summary)
		}
		b.WriteString("\n")
	}
	return b.String()
}
