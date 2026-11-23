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
//   - a user line "<tool-directive>" (see fakeTurnDirectives: "bash:<command>",
//     "tasks-create:<title>") → the next directive of that turn, one tool call
//     per round, so an E2E can script "create a card, then run a command"
//   - last user text contains "echo" → a call to a tool named "echo" (the
//     caller supplies it; tests use keel.testkit's EchoTool)
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
	// Explicit scripted directives win over the keyword shortcuts and keep
	// advancing across tool results: a vertical E2E needs a real card before a
	// real command, and each round emits exactly the next directive of the
	// current user turn.
	if text, ok := fakeLastUserText(msgs); ok {
		if call, ok := fakeNextDirective(model, msgs, text); ok {
			return agentcore.AssistantMessage{
				RoleField:  agentcore.RoleAssistant,
				Content:    agentcore.ContentList{call},
				StopReason: agentcore.StopReasonToolUse,
			}
		}
	}
	last := msgs[len(msgs)-1]
	switch m := last.(type) {
	case agentcore.ToolResultMessage:
		return fakeText(fmt.Sprintf("tool %s returned: %s", m.ToolName, ContentText(m.Content)))
	case agentcore.UserMessage:
		text := ContentText(m.Content)
		// The MCP app crash/e2e harness drives the aggregate `mcp` tool through
		// the deterministic provider: "mcp-list" asks for the catalogue and
		// "mcp-call:<server>/<tool>" calls one tool. This is test-harness surface
		// only (FakeStream is used only when no credentials are configured), and
		// it does not change the production provider path.
		if server, tool, ok := fakeMCPCallDirective(text); ok {
			args, _ := json.Marshal(map[string]any{
				"action":    "call",
				"server":    server,
				"tool":      tool,
				"arguments": map[string]any{},
			})
			return agentcore.AssistantMessage{
				RoleField: agentcore.RoleAssistant,
				Content: agentcore.ContentList{
					agentcore.NewToolCallContent(fakeToolCallID(model, msgs, 0), "mcp", args),
				},
				StopReason: agentcore.StopReasonToolUse,
			}
		}
		if strings.Contains(text, "mcp-list") {
			args, _ := json.Marshal(map[string]any{"action": "list"})
			return agentcore.AssistantMessage{
				RoleField: agentcore.RoleAssistant,
				Content: agentcore.ContentList{
					agentcore.NewToolCallContent(fakeToolCallID(model, msgs, 0), "mcp", args),
				},
				StopReason: agentcore.StopReasonToolUse,
			}
		}
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

// fakeCommandDirectivePrefix and fakeTaskDirectivePrefix are the scripted
// directives of the vertical E2E harness. They are line-anchored so ordinary
// prose (including the word "bash") cannot trigger them by accident.
const (
	fakeBashDirectivePrefix = "bash:"
	fakeTaskDirectivePrefix = "tasks-create:"
)

// fakeDirective is one scripted tool call of a user turn.
type fakeDirective struct {
	tool string
	args json.RawMessage
}

// fakeTurnDirectives parses the directive lines of one user message. Unknown or
// empty directives are ignored: a typo degrades to the ordinary reply, never to
// a half-built tool call.
func fakeTurnDirectives(text string) []fakeDirective {
	var out []fakeDirective
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, fakeTaskDirectivePrefix):
			title := strings.TrimSpace(strings.TrimPrefix(line, fakeTaskDirectivePrefix))
			if title == "" {
				continue
			}
			args, _ := json.Marshal(map[string]any{"action": "create", "title": title})
			out = append(out, fakeDirective{tool: "tasks", args: args})
		case strings.HasPrefix(line, fakeBashDirectivePrefix):
			command := strings.TrimSpace(strings.TrimPrefix(line, fakeBashDirectivePrefix))
			if command == "" {
				continue
			}
			args, _ := json.Marshal(map[string]any{"action": "run", "command": command})
			out = append(out, fakeDirective{tool: "bash", args: args})
		}
	}
	return out
}

// fakeLastUserText returns the newest user message's text. The scripted
// directives live there, so a tool result round can still find them.
func fakeLastUserText(msgs agentcore.MessageList) (string, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if user, ok := msgs[i].(agentcore.UserMessage); ok {
			return ContentText(user.Content), true
		}
	}
	return "", false
}

// fakeNextDirective returns the next scripted call of the current user turn.
// Progress is read from the transcript: the number of assistant messages with
// tool calls after the newest user message is the number of directives already
// executed. A tool result therefore advances the script by exactly one step,
// and a retried attempt (same transcript) replays the same step.
func fakeNextDirective(model string, msgs agentcore.MessageList, text string) (agentcore.ToolCallContent, bool) {
	directives := fakeTurnDirectives(text)
	if len(directives) == 0 {
		return agentcore.ToolCallContent{}, false
	}
	start := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role() == agentcore.RoleUser {
			start = i + 1
			break
		}
	}
	done := 0
	for _, msg := range msgs[start:] {
		if assistant, ok := msg.(agentcore.AssistantMessage); ok && assistantHasToolCall(assistant) {
			done++
		}
	}
	if done >= len(directives) {
		return agentcore.ToolCallContent{}, false
	}
	next := directives[done]
	return agentcore.NewToolCallContent(fakeToolCallID(model, msgs, done), next.tool, next.args), true
}

// assistantHasToolCall reports whether the assistant message carries one.
func assistantHasToolCall(msg agentcore.AssistantMessage) bool {
	for _, block := range msg.Content {
		if _, ok := block.(agentcore.ToolCallContent); ok {
			return true
		}
	}
	return false
}

// fakeToolCallID derives a deterministic tool-call id from the committed input// the fake provider is answering. The derivation rule is:
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

// fakeMCPCallDirective parses "mcp-call:<server>/<tool>" out of the last user
// text. The token ends at whitespace, newline or punctuation; an empty server or
// tool is not a directive (the text falls through to the ordinary reply).
func fakeMCPCallDirective(text string) (server, tool string, ok bool) {
	const marker = "mcp-call:"
	index := strings.Index(text, marker)
	if index < 0 {
		return "", "", false
	}
	rest := text[index+len(marker):]
	if end := strings.IndexAny(rest, " \t\r\n,.;!?。，；！？"); end >= 0 {
		rest = rest[:end]
	}
	server, tool, found := strings.Cut(rest, "/")
	if !found || server == "" || tool == "" {
		return "", "", false
	}
	if strings.ContainsAny(server+tool, "/:") {
		return "", "", false
	}
	return server, tool, true
}
