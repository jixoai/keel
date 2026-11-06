package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/provider"
)

// FakeStream is a deterministic provider used when no model credentials are
// configured. Its reply is a pure function of the committed messages, so a
// restart produces the same behaviour and E2E does not need a network.
//
// Contract:
//   - last message is a tool result → a final text answer quoting the result
//   - last user text contains "echo" → a call to the echo tool
//   - otherwise → a short acknowledgement
func FakeStream() provider.StreamFn {
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		msg := fakeReply(llm.Messages)
		if d := fakeDelay(); d > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(d):
			}
		}
		s := provider.NewAssistantMessageEventStream(4)
		go func() {
			defer s.Close()
			partial := agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}
			_ = s.Emit(ctx, provider.StreamStartEvent{Partial: partial})
			_ = s.Emit(ctx, provider.StreamTextEvent{Partial: msg})
			_ = s.Emit(ctx, provider.StreamDoneEvent{Message: msg})
		}()
		return s, nil
	}
}

func fakeReply(msgs agentcore.MessageList) agentcore.AssistantMessage {
	if len(msgs) == 0 {
		return fakeText("fake provider: no input")
	}
	last := msgs[len(msgs)-1]
	switch m := last.(type) {
	case agentcore.ToolResultMessage:
		return fakeText(fmt.Sprintf("tool %s returned: %s", m.ToolName, ContentText(m.Content)))
	case agentcore.UserMessage:
		text := ContentText(m.Content)
		if strings.Contains(text, "echo") {
			args, _ := json.Marshal(map[string]string{"x": text})
			return agentcore.AssistantMessage{
				RoleField: agentcore.RoleAssistant,
				Content: agentcore.ContentList{
					agentcore.NewToolCallContent(fmt.Sprintf("fake-%d", time.Now().UnixNano()), "echo", args),
				},
				StopReason: agentcore.StopReasonToolUse,
			}
		}
		return fakeText("fake provider received: " + text)
	default:
		return fakeText("fake provider: nothing to answer")
	}
}

// fakeDelay lets E2E simulate a slow model call; production never sets it.
func fakeDelay() time.Duration {
	ms, _ := strconv.Atoi(os.Getenv("KEEL_FAKE_DELAY_MS"))
	return time.Duration(ms) * time.Millisecond
}

func fakeText(text string) agentcore.AssistantMessage {
	return agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList{agentcore.NewTextContent(text)},
		StopReason: agentcore.StopReasonEndTurn,
	}
}
