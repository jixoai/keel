package agent

import (
	"context"
	"encoding/json"
	"reflect"
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

// TestFakeMCPDirectives pins the deterministic MCP app harness surface: the
// fake provider is the only caller of the aggregate `mcp` tool in cmd/e2e, and
// a directive must never leak into an ordinary reply or the echo path.
func TestFakeMCPDirectives(t *testing.T) {
	tests := []struct {
		text string
		args map[string]any
	}{
		{
			text: "please mcp-list now",
			args: map[string]any{"action": "list"},
		},
		{
			text: "run mcp-call:fixture/show_dashboard",
			args: map[string]any{
				"action": "call", "server": "fixture", "tool": "show_dashboard",
				"arguments": map[string]any{},
			},
		},
		{
			text: "mcp-call:fixture/refresh.",
			args: map[string]any{
				"action": "call", "server": "fixture", "tool": "refresh",
				"arguments": map[string]any{},
			},
		},
	}
	for _, test := range tests {
		reply := fakeReply("fake", agentcore.MessageList{fakeUserMsg(test.text)})
		calls := reply.ToolCalls()
		if len(calls) != 1 {
			t.Fatalf("%q: tool calls = %d, want 1", test.text, len(calls))
		}
		if calls[0].Name != "mcp" {
			t.Fatalf("%q: tool = %q, want mcp", test.text, calls[0].Name)
		}
		var args map[string]any
		if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
			t.Fatalf("%q: arguments are not JSON: %v", test.text, err)
		}
		if !reflect.DeepEqual(args, test.args) {
			t.Fatalf("%q: arguments = %v, want %v", test.text, args, test.args)
		}
	}

	// A malformed directive is not an MCP call: it stays an ordinary reply.
	for _, text := range []string{"mcp-call:no-slash", "mcp-call:/tool", "mcp-call:server/"} {
		reply := fakeReply("fake", agentcore.MessageList{fakeUserMsg(text)})
		if calls := reply.ToolCalls(); len(calls) != 0 {
			t.Fatalf("%q: produced a tool call %+v", text, calls)
		}
	}
	// The pre-existing echo behaviour is unchanged.
	reply := fakeReply("fake", agentcore.MessageList{fakeUserMsg("please echo this")})
	if calls := reply.ToolCalls(); len(calls) != 1 || calls[0].Name != "echo" {
		t.Fatalf("echo directive = %+v", calls)
	}
}

// TestFakeScriptedDirectives pins the vertical-E2E scripting surface: a user
// turn may name an ordered list of tool calls ("tasks-create:" then "bash:"),
// the fake provider emits exactly one per round, and the script advances only
// when the previous call's result is in the transcript. A directive is
// line-anchored, so the same words inside prose never call a tool.
func TestFakeScriptedDirectives(t *testing.T) {
	text := "prepare the workspace\n" +
		"tasks-create: write a report\n" +
		"bash:printf 'hello-from-bash\\n' > out.txt"
	msgs := agentcore.MessageList{fakeUserMsg(text)}

	first := fakeReply("fake", msgs)
	calls := first.ToolCalls()
	if len(calls) != 1 || calls[0].Name != "tasks" {
		t.Fatalf("first round = %+v, want the tasks call", calls)
	}
	var args map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if args["action"] != "create" || args["title"] != "write a report" {
		t.Fatalf("tasks arguments = %v", args)
	}
	// The call is part of the transcript now; the next round must be the bash
	// directive, with a different deterministic id.
	msgs = append(msgs, first, fakeToolResultMsg("card created"))
	second := fakeReply("fake", msgs)
	calls = second.ToolCalls()
	if len(calls) != 1 || calls[0].Name != "bash" {
		t.Fatalf("second round = %+v, want the bash call", calls)
	}
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if args["action"] != "run" || args["command"] != "printf 'hello-from-bash\\n' > out.txt" {
		t.Fatalf("bash arguments = %v", args)
	}
	if calls[0].ID == first.ToolCalls()[0].ID {
		t.Fatalf("the two calls share id %q", calls[0].ID)
	}
	// The script is exhausted: the turn ends with text, not a third call.
	msgs = append(msgs, second, fakeToolResultMsg("exited 0"))
	if calls := fakeReply("fake", msgs).ToolCalls(); len(calls) != 0 {
		t.Fatalf("exhausted script produced %+v", calls)
	}
	// Prose that merely mentions the words is not a directive.
	prose := fakeReply("fake", agentcore.MessageList{fakeUserMsg("I will run bash: later and tasks-create: someday")})
	if calls := prose.ToolCalls(); len(calls) != 0 {
		t.Fatalf("prose produced %+v", calls)
	}
}
