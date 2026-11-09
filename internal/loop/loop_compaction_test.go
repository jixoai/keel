package loop

import (
	"context"
	"errors"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/provider"
)

// TestCompactionCarriesTheModelOverlay pins the summary request's provider
// options: the resolved model's overlay (reasoning patch, vendor keys) belongs
// to the model, so the compaction request must carry it exactly as the primary
// turn does. Losing it silently drops options the tenant configured.
func TestCompactionCarriesTheModelOverlay(t *testing.T) {
	var got provider.StreamConfig
	called := false
	stream := func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		called = true
		got = cfg
		return nil, errors.New("stop: the request was recorded")
	}
	cfg := &RunConfig{
		LoopConfig: LoopConfig{
			Model: "m", Provider: "p", APIKey: "k", Stream: stream,
			Extra:         map[string]any{"reasoning_effort": "high", "vendor_flag": true},
			Compaction:    compaction.CompactionSettings{Enabled: true, ReserveTokens: 0, KeepRecentTokens: 1},
			ContextWindow: 1000,
		},
	}
	msgs := agentcore.MessageList{
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("turn one")}},
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("answer one")}},
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("turn two")}},
		agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent("answer two")}},
		agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("turn three")}},
	}
	if _, err := runCompaction(context.Background(), msgs, cfg); err == nil {
		t.Fatal("the stub stream's error must surface")
	}
	if !called {
		t.Fatal("the summary request was never issued (no cut point?)")
	}
	if got.Extra["reasoning_effort"] != "high" || got.Extra["vendor_flag"] != true {
		t.Fatalf("summary StreamConfig.Extra = %v, want the model overlay", got.Extra)
	}
	if got.APIKey != "k" {
		t.Fatalf("summary StreamConfig.APIKey = %q", got.APIKey)
	}
}
