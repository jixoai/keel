package agentloop_test

import (
	"reflect"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentloop"
	"github.com/jixoai/keel/internal/agentloop/testloop"
)

// toolTurnPrefix is the deterministic event prefix of the tool scenario up to
// and including the second turn_start: agent_start, the tool-loadout
// declaration and the prompt, the first turn (assistant stream, tool
// execution, tool result), turn_end and the second turn_start. Everything
// after it happens in parallel with a (possibly blocked) provider call.
var toolTurnPrefix = []string{
	agentcore.EventAgentStart,
	agentcore.EventMessageStart, agentcore.EventMessageEnd,
	agentcore.EventMessageStart, agentcore.EventMessageEnd,
	agentcore.EventTurnStart,
	agentcore.EventMessageStart, agentcore.EventMessageUpdate, agentcore.EventMessageEnd,
	agentcore.EventToolExecutionStart, agentcore.EventToolExecutionEnd,
	agentcore.EventMessageStart, agentcore.EventMessageEnd,
	agentcore.EventTurnEnd,
	agentcore.EventTurnStart,
}

// toolTurnFirstTurn is the produced-message prefix after the first turn of the
// tool scenario.
var toolTurnFirstTurn = []msgShape{
	{Role: agentcore.RoleSystem, DeclaredTools: []string{"echo"}},
	{Role: agentcore.RoleUser, Text: "hello"},
	{Role: agentcore.RoleAssistant, StopReason: agentcore.StopReasonToolUse, Calls: []string{`call-1|echo|{"value":"hello"}`}},
	{Role: agentcore.RoleToolResult, Text: "echoed: hello", ToolCallID: "call-1", ToolName: "echo"},
}

// checkLoopContract asserts the invariants every agentloop.Loop implementation
// must honor, independent of which implementation it is:
//
//   - the stream starts with agent_start and ends with exactly one agent_end;
//   - the terminal event carries the run's produced messages, and the stream
//     result is the same list;
//   - the result never repeats the pre-existing transcript;
//   - no tool result dangles (every result matches an earlier call);
//   - every started tool execution ends.
func checkLoopContract(t *testing.T, name string, l agentloop.Loop, in agentloop.Input) ([]agentcore.AgentEvent, []agentcore.AgentMessage) {
	t.Helper()
	events, result := runLoopOnce(t, l, in)

	if len(events) == 0 {
		t.Fatalf("%s: no events emitted", name)
	}
	if _, ok := events[0].(agentcore.AgentStartEvent); !ok {
		t.Fatalf("%s: first event = %T, want AgentStartEvent", name, events[0])
	}
	endIndex := -1
	for i, ev := range events {
		end, ok := ev.(agentcore.AgentEndEvent)
		if !ok {
			continue
		}
		if endIndex >= 0 {
			t.Fatalf("%s: multiple agent_end events", name)
		}
		endIndex = i
		assertShapesEqual(t, name+": agent_end messages vs stream result", shapesOf(end.Messages), shapesOf(result))
	}
	if endIndex < 0 {
		t.Fatalf("%s: no agent_end; events = %v", name, eventTypes(events))
	}
	if endIndex != len(events)-1 {
		t.Fatalf("%s: events after agent_end: %v", name, eventTypes(events[endIndex+1:]))
	}
	for _, sh := range shapesOf(result) {
		if sh.Text == "old history" {
			t.Fatalf("%s: the result repeats the pre-existing transcript: %+v", name, sh)
		}
	}
	assertToolResultsPaired(t, name, result)

	open := map[string]bool{}
	for _, ev := range events {
		switch e := ev.(type) {
		case agentcore.ToolExecutionStartEvent:
			open[e.ToolCallID] = true
		case agentcore.ToolExecutionEndEvent:
			delete(open, e.ToolCallID)
		}
	}
	if len(open) != 0 {
		t.Fatalf("%s: tool executions never ended: %v", name, open)
	}
	return events, result
}

// TestLoopContractDefaultLoop runs the shared invariants against pi-loop (text
// and tool scenarios) through the service interface.
func TestLoopContractDefaultLoop(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		prov := newFakeProvider(assistantText("Hi there!", agentcore.StopReasonEndTurn))
		checkLoopContract(t, "pi-loop text", agentloop.NewPiLoop(prov.stream), baseInput())
	})
	t.Run("tools", func(t *testing.T) {
		prov := newFakeProvider(
			assistantToolUse(toolCall("call-1", "echo", `{"value":"hello"}`)),
			assistantText("done", agentcore.StopReasonEndTurn),
		)
		in := baseInput()
		in.Context.Tools = []agentcore.AgentTool{&echoTool{}}
		events, result := checkLoopContract(t, "pi-loop tools", agentloop.NewPiLoop(prov.stream), in)
		if len(events) < len(toolTurnPrefix) || len(result) < len(toolTurnFirstTurn) {
			t.Fatalf("tool scenario too short: events = %v, produced = %v", eventTypes(events), shapesOf(result))
		}
		if got := piEventTypes(events[:len(toolTurnPrefix)]); !reflect.DeepEqual(got, toolTurnPrefix) {
			t.Fatalf("tool turn prefix = %v, want %v", got, toolTurnPrefix)
		}
		assertShapesEqual(t, "tool turn produced prefix", shapesOf(result[:len(toolTurnFirstTurn)]), toolTurnFirstTurn)
	})
}

// TestLoopContractAlternateLoop runs the same invariants against a different
// implementation: the interface is not shaped around internal/loop.
func TestLoopContractAlternateLoop(t *testing.T) {
	events, result := checkLoopContract(t, "testloop", testloop.New("canned: "), baseInput())
	want := []string{
		agentcore.EventAgentStart,
		agentcore.EventTurnStart,
		agentcore.EventMessageStart, agentcore.EventMessageUpdate, agentcore.EventMessageEnd,
		agentcore.EventTurnEnd,
		agentcore.EventAgentEnd,
	}
	if got := piEventTypes(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("testloop event sequence = %v, want %v", got, want)
	}
	assertShapesEqual(t, "testloop produced messages", shapesOf(result), []msgShape{
		{Role: agentcore.RoleAssistant, Text: "canned: hello", StopReason: agentcore.StopReasonEndTurn},
	})
}

// TestLoopFuncAdaptsAFunction: the interface accepts a plain function, the
// shape most fakes and wiring code have.
func TestLoopFuncAdaptsAFunction(t *testing.T) {
	canned := testloop.New("via func: ")
	in := baseInput()
	_, result := checkLoopContract(t, "LoopFunc", agentloop.LoopFunc(canned.Run), in)
	assertShapesEqual(t, "LoopFunc produced messages", shapesOf(result), []msgShape{
		{Role: agentcore.RoleAssistant, Text: "via func: hello", StopReason: agentcore.StopReasonEndTurn},
	})
}

// TestProducedMessagesAreNotTheTranscript: the run result carries only what
// this run produced, never the history it answered from (the coordinator
// already has the committed transcript).
func TestProducedMessagesAreNotTheTranscript(t *testing.T) {
	prov := newFakeProvider(assistantText("answer", agentcore.StopReasonEndTurn))
	in := baseInput()
	in.Context.Messages = agentcore.MessageList{
		userMsg("old history"),
		userMsg("more old history"),
		userMsg("hello"),
	}
	in.Context.PromptCount = 1

	_, result := runLoopOnce(t, agentloop.NewPiLoop(prov.stream), in)

	assertShapesEqual(t, "produced messages", shapesOf(result), []msgShape{
		{Role: agentcore.RoleUser, Text: "hello"},
		{Role: agentcore.RoleAssistant, Text: "answer", StopReason: agentcore.StopReasonEndTurn},
	})
}
