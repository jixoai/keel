package agent

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/provider"
)

func fakeUserMsg(text string) agentcore.UserMessage {
	return agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewTextContent(text)},
	}
}

func fakeToolResultMsg(text string) agentcore.ToolResultMessage {
	return agentcore.ToolResultMessage{
		RoleField:  agentcore.RoleToolResult,
		ToolCallID: "call-1",
		ToolName:   "echo",
		Content:    agentcore.ContentList{agentcore.NewTextContent(text)},
	}
}

// toolCallIDFrom extracts the single tool-call id of a fake reply.
func toolCallIDFrom(t *testing.T, msg agentcore.AssistantMessage) string {
	t.Helper()
	calls := msg.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("fake reply tool calls = %d, want 1", len(calls))
	}
	return calls[0].ID
}

// TestFakeToolCallIDIsDeterministic: the id is a pure function of the committed
// input, not the clock. Two identical inputs produce the same id (replay is
// idempotent); the format is stable and clock-free.
func TestFakeToolCallIDIsDeterministic(t *testing.T) {
	msgs := agentcore.MessageList{fakeUserMsg("please echo this")}
	first := toolCallIDFrom(t, fakeReply("fake", msgs))
	second := toolCallIDFrom(t, fakeReply("fake", msgs))
	if first != second {
		t.Fatalf("same committed input produced different toolCallIds: %q vs %q", first, second)
	}
	if !regexp.MustCompile(`^fake-[0-9a-f]{16}$`).MatchString(first) {
		t.Fatalf("toolCallId = %q, want fake-<16 hex chars>", first)
	}
	if want := fakeToolCallID("fake", msgs, 0); first != want {
		t.Fatalf("fakeReply id = %q, want fakeToolCallID = %q", first, want)
	}
}

// TestFakeToolCallIDVariesWithCommittedInput: different transcripts (prompt,
// sequence position, call index, model) produce different ids, so independent
// calls cannot collide.
func TestFakeToolCallIDVariesWithCommittedInput(t *testing.T) {
	baseMsgs := agentcore.MessageList{fakeUserMsg("echo one")}
	base := fakeToolCallID("fake", baseMsgs, 0)

	cases := map[string]string{
		"different prompt":     fakeToolCallID("fake", agentcore.MessageList{fakeUserMsg("echo two")}, 0),
		"same tail, later seq": fakeToolCallID("fake", agentcore.MessageList{fakeUserMsg("earlier"), fakeUserMsg("echo one")}, 0),
		"second call in turn":  fakeToolCallID("fake", baseMsgs, 1),
		"different model":      fakeToolCallID("other-model", baseMsgs, 0),
		"tool result tail":     fakeToolCallID("fake", agentcore.MessageList{fakeToolResultMsg("echoed")}, 0),
	}
	for name, got := range cases {
		if got == base {
			t.Fatalf("%s: toolCallId %q collides with the base input", name, got)
		}
	}
}

// TestFakeToolCallIDDoubleClickDoesNotCollide: two clicks on the same prompt
// append two user messages, so the transcript length differs and the ids differ.
// A replay of the *same* committed transcript still yields the same id — that is
// the kernel's idempotency case, not a collision.
func TestFakeToolCallIDDoubleClickDoesNotCollide(t *testing.T) {
	singleClick := agentcore.MessageList{fakeUserMsg("echo hi")}
	doubleClick := agentcore.MessageList{
		fakeUserMsg("echo hi"),
		assistantTextForFake(t),
		fakeToolResultMsg("tool echo returned: echo hi"),
		fakeText("tool echo returned: echo hi"),
		fakeUserMsg("echo hi"),
	}
	first := fakeToolCallID("fake", singleClick, 0)
	second := fakeToolCallID("fake", doubleClick, 0)
	if first == second {
		t.Fatalf("the repeated prompt collided: %q", first)
	}
	if replay := fakeToolCallID("fake", doubleClick, 0); replay != second {
		t.Fatalf("replaying the same committed transcript changed the id: %q vs %q", replay, second)
	}
}

func assistantTextForFake(t *testing.T) agentcore.AssistantMessage {
	t.Helper()
	return agentcore.AssistantMessage{
		RoleField: agentcore.RoleAssistant,
		Content: agentcore.ContentList{
			agentcore.NewToolCallContent("call-1", "echo", []byte(`{"x":"echo hi"}`)),
		},
		StopReason: agentcore.StopReasonToolUse,
	}
}

// TestFakeStreamUsesCommittedInputForToolCallID: the provider path (FakeStream)
// emits the derived id, not a clock-derived one. Two runs with the same
// committed transcript emit the same call id.
func TestFakeStreamUsesCommittedInputForToolCallID(t *testing.T) {
	msgs := agentcore.MessageList{fakeUserMsg("echo hello")}
	first := drainFakeToolCallID(t, msgs)
	second := drainFakeToolCallID(t, msgs)
	if first != second {
		t.Fatalf("FakeStream emitted different toolCallIds for the same input: %q vs %q", first, second)
	}
	if want := fakeToolCallID("fake", msgs, 0); first != want {
		t.Fatalf("FakeStream toolCallId = %q, want the committed-input derivation %q", first, want)
	}
}

func drainFakeToolCallID(t *testing.T, msgs agentcore.MessageList) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := FakeStream()(ctx, "fake", provider.LlmContext{Messages: msgs}, provider.StreamConfig{})
	if err != nil {
		t.Fatalf("FakeStream: %v", err)
	}
	var done agentcore.AssistantMessage
	sawDone := false
	for ev := range stream.Events() {
		if d, ok := ev.(provider.StreamDoneEvent); ok {
			done = d.Message
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatal("FakeStream emitted no done event")
	}
	return toolCallIDFrom(t, done)
}
