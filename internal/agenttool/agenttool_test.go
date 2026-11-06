// Conformance tests ported from pi's packages/agent/test/agent-loop.test.ts
// describe("runToolCall") (pin 55a5c69, MIT License).

package agenttool

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
)

// echoToolWithOutput mirrors pi's `echo` test tool: it streams one partial
// update and returns structured content.
type echoToolWithOutput struct{}

func (echoToolWithOutput) Name() string        { return "echo" }
func (echoToolWithOutput) Description() string { return "Echo tool" }
func (echoToolWithOutput) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`)
}
func (echoToolWithOutput) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}
func (echoToolWithOutput) Execute(_ context.Context, _ string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	var in struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return agentcore.AgentToolResult{}, err
	}
	onUpdate(agentcore.AgentToolResult{Content: text("partial"), Details: map[string]any{}})
	return agentcore.AgentToolResult{
		Content:           text(in.Value),
		Details:           map[string]any{},
		StructuredContent: map[string]any{"value": in.Value},
	}, nil
}

// failingTool returns an error result without throwing.
type failingTool struct{}

func (failingTool) Name() string            { return "failing" }
func (failingTool) Description() string     { return "Returns an error result" }
func (failingTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (failingTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}
func (failingTool) Execute(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	isError := true
	return agentcore.AgentToolResult{
		Content: text("bad"),
		Details: map[string]any{"partial": true},
		IsError: &isError,
	}, nil
}

func text(s string) agentcore.ContentList {
	return agentcore.ContentList{agentcore.NewTextContent(s)}
}

func toolCall(id, name, args string) agentcore.AgentToolCall {
	return agentcore.AgentToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

func contentText(c agentcore.ContentList) string {
	return agentcore.ContentToText(c)
}

// PI: it("validates, runs the hooks, and reports failures as error outcomes")
func TestRunToolCallValidatesRunsHooksAndReportsFailures(t *testing.T) {
	var hookCalls []string
	var updates []agentcore.AgentToolResult
	blocked := text("nope")
	options := RunToolCallOptions{
		Tools: []agentcore.AgentTool{echoToolWithOutput{}, failingTool{}},
		BeforeToolCall: func(_ context.Context, call agentcore.AgentToolCall) *agentcore.BeforeToolCallDecision {
			hookCalls = append(hookCalls, "before "+call.ID)
			var in struct {
				Value string `json:"value"`
			}
			if err := json.Unmarshal(call.Arguments, &in); err == nil && in.Value == "blocked" {
				return &agentcore.BeforeToolCallDecision{Block: true, Content: &blocked}
			}
			return nil
		},
		AfterToolCall: func(_ context.Context, call agentcore.AgentToolCall, _ agentcore.AgentToolResult, _ bool) *agentcore.AfterToolCallResult {
			hookCalls = append(hookCalls, "after "+call.ID)
			return nil
		},
		OnUpdate: func(partial agentcore.AgentToolResult) {
			updates = append(updates, partial)
		},
	}
	ctx := context.Background()

	a := RunToolCall(ctx, toolCall("a", "echo", `{"value":"a"}`), options)
	if a.IsError {
		t.Fatalf("a isError = true: %v", contentText(a.Result.Content))
	}
	if sc, ok := a.Result.StructuredContent.(map[string]any); !ok || sc["value"] != "a" {
		t.Fatalf("a structured content = %#v", a.Result.StructuredContent)
	}

	b := RunToolCall(ctx, toolCall("b", "echo", `{"value":{"nested":true}}`), options)
	if !b.IsError {
		t.Fatal("b (invalid args) isError = false, want true")
	}

	c := RunToolCall(ctx, toolCall("c", "echo", `{"value":"blocked"}`), options)
	if !c.IsError || contentText(c.Result.Content) != "nope" {
		t.Fatalf("c = isError:%v content:%q", c.IsError, contentText(c.Result.Content))
	}

	d := RunToolCall(ctx, toolCall("d", "missing", `{}`), options)
	if !d.IsError || contentText(d.Result.Content) != "Tool missing not found" {
		t.Fatalf("d = isError:%v content:%q", d.IsError, contentText(d.Result.Content))
	}

	e := RunToolCall(ctx, toolCall("e", "failing", `{}`), options)
	if !e.IsError {
		t.Fatal("e isError = false, want true")
	}
	details, _ := e.Result.Details.(map[string]any)
	if details["partial"] != true {
		t.Fatalf("e details = %#v, want the error result's details", e.Result.Details)
	}

	if len(updates) != 1 || contentText(updates[0].Content) != "partial" {
		t.Fatalf("updates = %#v", updates)
	}
	// Validation failures and unknown tools never reach the hooks; blocked calls
	// skip afterToolCall.
	wantHooks := []string{"before a", "after a", "before c", "before e", "after e"}
	if !reflect.DeepEqual(hookCalls, wantHooks) {
		t.Fatalf("hook calls = %v, want %v", hookCalls, wantHooks)
	}
}

// PI: it("lets afterToolCall replace structured content and drops it when only
// content is replaced")
func TestRunToolCallAfterToolCallStructuredContent(t *testing.T) {
	redacted := text("redacted")
	note := map[string]any{"note": "kept"}
	results := []*agentcore.AfterToolCallResult{
		{Content: &redacted},
		{StructuredContent: anyPtr(map[string]any{"value": "replaced"})},
		{Content: &redacted, StructuredContent: anyPtr(map[string]any{"value": "both"})},
		{Details: anyPtr(note)},
	}
	var seen []any
	for _, after := range results {
		outcome := RunToolCall(context.Background(), toolCall("x", "echo", `{"value":"original"}`), RunToolCallOptions{
			Tools: []agentcore.AgentTool{echoToolWithOutput{}},
			AfterToolCall: func(context.Context, agentcore.AgentToolCall, agentcore.AgentToolResult, bool) *agentcore.AfterToolCallResult {
				return after
			},
		})
		seen = append(seen, outcome.Result.StructuredContent)
	}
	want := []any{nil, map[string]any{"value": "replaced"}, map[string]any{"value": "both"}, map[string]any{"value": "original"}}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("structured content = %#v, want %#v", seen, want)
	}
}

func anyPtr(v any) *any { return &v }
