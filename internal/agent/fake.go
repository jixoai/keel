package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
		msg := fakeReply(model, llm.Messages)
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

func fakeReply(model string, msgs agentcore.MessageList) agentcore.AssistantMessage {
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
					agentcore.NewToolCallContent(fakeToolCallID(model, msgs, 0), "echo", args),
				},
				StopReason: agentcore.StopReasonToolUse,
			}
		}
		return fakeText("fake provider received: " + text)
	default:
		return fakeText("fake provider: nothing to answer")
	}
}

// fakeToolCallID derives a deterministic tool-call id from the committed input
// the fake provider is answering. The derivation rule is:
//
//	sha256("model=<model>\0seq=<len(messages)>\0index=<call index>\0"
//	       "role=<last message role>\0text=<last message text>")[:8]  (hex)
//
// where seq is the committed transcript length — the provider-visible proxy for
// the session sequence number — and index is the call's position inside the
// response (the fake emits one call, so it is 0; the parameter exists so a
// multi-call script cannot produce duplicate ids).
//
// Consequences, which the tests pin:
//   - the same committed input sequence produces the same id (a crash/restart
//     retry or an E2E replay is idempotent rather than a new call);
//   - a repeated prompt gets a different id because the transcript grew by the
//     first exchange (double-click cannot collide);
//   - a different prompt or a different transcript position changes the id.
//
// No clock, no randomness: the id must never be derived from UnixNano (the W1
// test-infrastructure rule) or a replay would mint different ledger keys.
func fakeToolCallID(model string, msgs agentcore.MessageList, index int) string {
	h := sha256.New()
	fmt.Fprintf(h, "model=%s\x00seq=%d\x00index=%d\x00", model, len(msgs), index)
	if len(msgs) > 0 {
		last := msgs[len(msgs)-1]
		fmt.Fprintf(h, "role=%s\x00text=%s", last.Role(), messageText(last))
	}
	return "fake-" + hex.EncodeToString(h.Sum(nil)[:8])
}

// messageText returns the deterministic semantic text of a message. Timestamps
// and provider metadata are deliberately excluded: they are not part of the id
// contract.
func messageText(m agentcore.AgentMessage) string {
	switch msg := m.(type) {
	case agentcore.UserMessage:
		return ContentText(msg.Content)
	case agentcore.ToolResultMessage:
		return ContentText(msg.Content)
	case agentcore.AssistantMessage:
		return ContentText(msg.Content)
	case agentcore.CompactionMessage:
		return msg.Summary
	case agentcore.SystemMessage:
		return msg.Content
	default:
		return ""
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
