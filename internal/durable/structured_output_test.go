package durable

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
)

// Ported from packages/durable/test/harness-structured-output.test.ts.

var countSchema = raw(map[string]any{
	"type":       "object",
	"properties": map[string]any{"count": map[string]any{"type": "number"}},
	"required":   []string{"count"},
})

func returning(name string, result ToolResult, schema json.RawMessage) *Tool {
	tool := &Tool{
		Name: name, Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			return result, nil
		},
	}
	tool.StructuredOutputSchema = schema
	return tool
}

func runNested(t *testing.T, tools []*Tool, name string, hooks []*Hook) NestedResult {
	t.Helper()
	ctx := ctxT(t)
	var received *NestedResult
	probe := &Tool{
		Name: "probe", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			r, err := api.ExecuteTool(ctx, name, raw(map[string]any{}))
			if err != nil {
				return ToolResult{}, err
			}
			received = &r
			return ToolResult{}, nil
		},
	}
	all := append(append([]*Tool{}, tools...), probe)
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "probe", args: map[string]any{}, id: "c1"}), textMessage("done"))
	_, root := openChat(t, nil, Options{Provider: faux, Tools: all, Hooks: hooks})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sub.Wait(ctx)
	if received == nil {
		t.Fatalf("probe received no result")
	}
	return *received
}

func runDirect(t *testing.T, tools []*Tool, name string) agentcore.ToolResultMessage {
	t.Helper()
	ctx := ctxT(t)
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: name, args: map[string]any{}, id: "c1"}), textMessage("done"))
	h, root := openChat(t, nil, Options{Provider: faux, Tools: tools})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sub.Wait(ctx)
	messages := toolResults(h, root)
	return messages["c1"]
}

func TestStructuredOutput_GivesCallersASchemaLessToolsOutput(t *testing.T) {
	text := agentcore.NewTextContent("one")
	image := agentcore.NewImageContent("AAAA", "image/png")
	cases := []struct {
		name   string
		output agentcore.ContentList
		value  any
	}{
		{"one text", agentcore.ContentList{text}, "one"},
		{"one image", agentcore.ContentList{image}, map[string]any{"type": "image", "data": "AAAA", "mimeType": "image/png"}},
		{"none", agentcore.ContentList{}, ""},
		{"text and image", agentcore.ContentList{text, image}, []any{
			map[string]any{"type": "text", "text": "one"},
			map[string]any{"type": "image", "data": "AAAA", "mimeType": "image/png"},
		}},
		{"two texts", agentcore.ContentList{text, agentcore.NewTextContent("two")}, []any{
			map[string]any{"type": "text", "text": "one"},
			map[string]any{"type": "text", "text": "two"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := returning("plain", ToolResult{HasOutput: true, Output: tc.output}, nil)
			received := runNested(t, []*Tool{tool}, "plain", nil)
			if received.IsError || len(received.Diagnostics) != 0 {
				t.Fatalf("received = %+v", received)
			}
			got := nestedStructured(received)
			a, _ := json.Marshal(got)
			b, _ := json.Marshal(tc.value)
			if string(a) != string(b) {
				t.Fatalf("structured = %s, want %s", a, b)
			}
		})
	}
}

func TestStructuredOutput_GivesCallersTheBoundedOutputAsTheModelSeesItWithoutTheRenderedDiagnostics(t *testing.T) {
	tool := returning("long",
		ToolResult{HasOutput: true, Output: agentcore.ContentList{agentcore.NewTextContent("a\nb\nc")},
			Diagnostics: []Diagnostic{{Severity: "info", Message: "note"}}},
		nil)
	tool.OutputLimits = OutputLimits{MaxLines: 1}
	received := runNested(t, []*Tool{tool}, "long", nil)
	if got := nestedStructured(received); got != "a\n" {
		t.Fatalf("structured = %#v", got)
	}
	codes := codesOf(ToolResult{Diagnostics: received.Diagnostics})
	if len(codes) != 2 || codes[0] != "" || codes[1] != "truncated" {
		t.Fatalf("codes = %v", codes)
	}
}

func TestStructuredOutput_GivesCallersASchemaToolsValidatedStructuredOutputWhichTheTranscriptDoesNotStore(t *testing.T) {
	result := ToolResult{HasOutput: true, Output: agentcore.ContentList{agentcore.NewTextContent("3 things")},
		StructuredOutput: raw(map[string]any{"count": 3})}
	tool := returning("counter", result, countSchema)
	received := runNested(t, []*Tool{tool}, "counter", nil)
	if got := nestedStructured(received); got == nil || received.IsError {
		t.Fatalf("received = %+v", received)
	}
	var value map[string]any
	_ = json.Unmarshal(received.StructuredOutput, &value)
	if value["count"] != float64(3) {
		t.Fatalf("structured = %v", value)
	}
	direct := runDirect(t, []*Tool{tool}, "counter")
	if direct.IsError || OutputText(direct.Content) != "3 things" {
		t.Fatalf("direct = %+v", direct)
	}
}

func TestStructuredOutput_TurnsStructuredOutputThatBreaksTheSchemaIntoAnErrorResultForCallers(t *testing.T) {
	cases := []struct {
		name    string
		result  ToolResult
		schema  json.RawMessage
		message string
	}{
		{"wrong", ToolResult{StructuredOutput: raw(map[string]any{"count": "three"})}, countSchema,
			"does not match its schema: count:"},
		{"missing", ToolResult{HasOutput: true}, countSchema, "returned no structuredOutput"},
		{"undeclared", ToolResult{StructuredOutput: raw(map[string]any{"count": 3})}, nil,
			"declares no structuredOutputSchema"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := returning(tc.name, tc.result, tc.schema)
			received := runNested(t, []*Tool{tool}, tc.name, nil)
			if !received.IsError {
				t.Fatalf("received = %+v", received)
			}
			if tc.schema != nil {
				if received.StructuredOutput != nil {
					t.Fatalf("broken value kept: %#v", received.StructuredOutput)
				}
			} else if got := nestedStructured(received); got != "" {
				t.Fatalf("schema-less error value = %#v", got)
			}
			if got := codesOf(ToolResult{Diagnostics: received.Diagnostics}); len(got) != 1 || got[0] != "invalid_structured_output" {
				t.Fatalf("codes = %v", got)
			}
			if !strings.Contains(received.Diagnostics[0].Message, tc.message) {
				t.Fatalf("message = %q", received.Diagnostics[0].Message)
			}
			var reports []error
			ctx := ctxT(t)
			faux := &fauxProvider{}
			faux.setResponses(calls(callSpec{name: tc.name, args: map[string]any{}, id: "c1"}), textMessage("done"))
			_, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{tool},
				OnReport: func(err error) { reports = append(reports, err) }})
			sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
			if err != nil {
				t.Fatal(err)
			}
			_, _ = sub.Wait(ctx)
			direct := toolResults(h2or(root), root)["c1"]
			if direct.IsError {
				t.Fatalf("model call became an error")
			}
			if strings.Contains(OutputText(direct.Content), tc.message) {
				t.Fatalf("model saw the break: %q", OutputText(direct.Content))
			}
			joined := ""
			for _, r := range reports {
				joined += r.Error() + "\n"
			}
			if !strings.Contains(joined, tc.message) {
				t.Fatalf("reports = %q", joined)
			}
		})
	}
}

// h2or returns the harness behind a conversation handle.
func h2or(c *Conversation) *Harness { return c.h }

func TestStructuredOutput_GivesCallersTheOutputOfAnErrorResultOfAToolWithoutASchema(t *testing.T) {
	tool := returning("bad", ToolResult{HasOutput: true,
		Output: agentcore.ContentList{agentcore.NewTextContent("bad")}, IsError: true}, nil)
	received := runNested(t, []*Tool{tool}, "bad", nil)
	if !received.IsError || nestedStructured(received) != "bad" {
		t.Fatalf("received = %+v", received)
	}
}

func TestStructuredOutput_GivesCallersNoStructuredOutputForAnErrorResultTheHarnessWrote(t *testing.T) {
	received := runNested(t, nil, "missing", nil)
	if !received.IsError || received.StructuredOutput != nil {
		t.Fatalf("received = %+v", received)
	}
	if got := codesOf(ToolResult{Diagnostics: received.Diagnostics}); len(got) != 1 || got[0] != "tool_unavailable" {
		t.Fatalf("codes = %v", got)
	}
}

func TestStructuredOutput_GivesCallersASchemaToolsErrorTextAsADiagnosticNeverAsStructuredOutput(t *testing.T) {
	fails := returning("fails", ToolResult{HasOutput: true,
		Output: agentcore.ContentList{agentcore.NewTextContent("nope")}, IsError: true}, countSchema)
	received := runNested(t, []*Tool{fails}, "fails", nil)
	if !received.IsError || received.StructuredOutput != nil {
		t.Fatalf("received = %+v", received)
	}
	if len(received.Diagnostics) != 1 || received.Diagnostics[0].Code != "tool_error" || received.Diagnostics[0].Message != "nope" {
		t.Fatalf("diagnostics = %+v", received.Diagnostics)
	}
	direct := runDirect(t, []*Tool{fails}, "fails")
	if OutputText(direct.Content) != "nope" {
		t.Fatalf("direct = %q", OutputText(direct.Content))
	}

	throws := &Tool{
		Name: "throws", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		StructuredOutputSchema: countSchema,
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			return ToolResult{}, errPermissionDenied
		},
	}
	thrown := runNested(t, []*Tool{throws}, "throws", nil)
	if !thrown.IsError || thrown.StructuredOutput != nil {
		t.Fatalf("thrown = %+v", thrown)
	}
	if len(thrown.Diagnostics) != 1 || thrown.Diagnostics[0].Code != "tool_error" ||
		thrown.Diagnostics[0].Message != "permission denied" {
		t.Fatalf("thrown diagnostics = %+v", thrown.Diagnostics)
	}

	explained := returning("fails", ToolResult{HasOutput: true,
		Output: agentcore.ContentList{agentcore.NewTextContent("partial log")}, IsError: true,
		Diagnostics: []Diagnostic{{Severity: "error", Code: "quota", Message: "quota exceeded"}}}, countSchema)
	explainedResult := runNested(t, []*Tool{explained}, "fails", nil)
	if len(explainedResult.Diagnostics) != 1 || explainedResult.Diagnostics[0].Code != "quota" {
		t.Fatalf("explained = %+v", explainedResult.Diagnostics)
	}

	kept := returning("fails", ToolResult{HasOutput: true,
		Output:           agentcore.ContentList{agentcore.NewTextContent("nope")},
		StructuredOutput: raw(map[string]any{"count": 0}),
		IsError:          true}, countSchema)
	keptResult := runNested(t, []*Tool{kept}, "fails", nil)
	var keptValue map[string]any
	_ = json.Unmarshal(keptResult.StructuredOutput, &keptValue)
	if !keptResult.IsError || keptValue["count"] != float64(0) {
		t.Fatalf("kept = %+v", keptResult)
	}
}

var errPermissionDenied = permissionDenied{}

type permissionDenied struct{}

func (permissionDenied) Error() string { return "permission denied" }

func TestStructuredOutput_KeepsStructuredOutputWhenAfterToolReplacesTheOutputAndValidatesOneAfterToolReplaces(t *testing.T) {
	plain := returning("plain", ToolResult{HasOutput: true,
		Output: agentcore.ContentList{agentcore.NewTextContent("secret")}}, nil)
	counter := returning("counter", ToolResult{StructuredOutput: raw(map[string]any{"count": 1})}, countSchema)
	other := returning("other", ToolResult{StructuredOutput: raw(map[string]any{"count": 1})}, countSchema)
	hook := &Hook{Task: ToolTaskKind, AfterTool: func(ctx context.Context, call ToolHookCall, result ToolResult, api *ToolAPI) (*ToolResult, error) {
		out := result
		switch call.Name {
		case "probe":
			return nil, nil
		case "other":
			out.StructuredOutput = raw(map[string]any{"count": "broken"})
		default:
			out.HasOutput = true
			out.Output = agentcore.ContentList{agentcore.NewTextContent("rewritten")}
		}
		return &out, nil
	}}
	var plainResult, counterResult, otherResult *NestedResult
	probe := &Tool{
		Name: "probe", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			r1, _ := api.ExecuteTool(ctx, "plain", raw(map[string]any{}))
			plainResult = &r1
			r2, _ := api.ExecuteTool(ctx, "counter", raw(map[string]any{}))
			counterResult = &r2
			r3, _ := api.ExecuteTool(ctx, "other", raw(map[string]any{}))
			otherResult = &r3
			return ToolResult{}, nil
		},
	}
	ctx := ctxT(t)
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "probe", args: map[string]any{}, id: "c1"}), textMessage("done"))
	_, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{plain, counter, other, probe}, Hooks: []*Hook{hook}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sub.Wait(ctx)
	if got := nestedStructured(*plainResult); got != "rewritten" {
		t.Fatalf("plain = %#v", got)
	}
	var counterValue map[string]any
	_ = json.Unmarshal(counterResult.StructuredOutput, &counterValue)
	if counterValue["count"] != float64(1) || counterResult.IsError {
		t.Fatalf("counter = %+v", counterResult)
	}
	if !otherResult.IsError || otherResult.StructuredOutput != nil {
		t.Fatalf("other = %+v", otherResult)
	}
	if got := codesOf(ToolResult{Diagnostics: otherResult.Diagnostics}); len(got) != 1 || got[0] != "invalid_structured_output" {
		t.Fatalf("other codes = %v", got)
	}
}

func TestStructuredOutput_GivesANestedCallAHookTurnsIntoASuccessItsOutputAsValueUnlessTheToolHasASchema(t *testing.T) {
	ctx := ctxT(t)
	plain := returning("plain", ToolResult{HasOutput: true}, nil)
	typed := returning("typed", ToolResult{HasOutput: true, StructuredOutput: raw(map[string]any{"ok": true})},
		raw(map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}}))
	hook := &Hook{Task: ToolTaskKind,
		BeforeTool: func(ctx context.Context, call ToolHookCall, api *ToolAPI) (*BeforeToolDecision, error) {
			if call.Parent == nil {
				return nil, nil
			}
			return &BeforeToolDecision{Block: "no"}, nil
		},
		AfterTool: func(ctx context.Context, call ToolHookCall, result ToolResult, api *ToolAPI) (*ToolResult, error) {
			if call.Parent == nil {
				return nil, nil
			}
			return &ToolResult{HasOutput: true, Output: agentcore.ContentList{agentcore.NewTextContent("fine " + call.Name)}}, nil
		},
	}
	var nested []NestedResult
	batch := &Tool{
		Name: "batch", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			r1, _ := api.ExecuteTool(ctx, "plain", raw(map[string]any{}))
			nested = append(nested, r1)
			r2, _ := api.ExecuteTool(ctx, "typed", raw(map[string]any{}))
			nested = append(nested, r2)
			return ToolResult{}, nil
		},
	}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "batch", args: map[string]any{}, id: "c1"}), textMessage("done"))
	_, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{plain, typed, batch}, Hooks: []*Hook{hook}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sub.Wait(ctx)
	if len(nested) != 2 {
		t.Fatalf("nested = %+v", nested)
	}
	if nested[0].IsError || nestedStructured(nested[0]) != "fine plain" {
		t.Fatalf("plain = %+v", nested[0])
	}
	if nested[1].IsError || nested[1].StructuredOutput != nil {
		t.Fatalf("typed = %+v", nested[1])
	}
}

func TestStructuredOutput_StoresExactlyWhatCallersAndEventsGetAndKeepsAStoredNull(t *testing.T) {
	ctx := ctxT(t)
	plain := returning("plain", ToolResult{HasOutput: true,
		Output: agentcore.ContentList{agentcore.NewTextContent("a")}}, nil)
	nothing := returning("nothing", ToolResult{StructuredOutput: json.RawMessage(`null`)},
		json.RawMessage(`{"type":"null"}`))
	var received []NestedResult
	probe := &Tool{
		Name: "probe", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			r1, _ := api.ExecuteTool(ctx, "plain", raw(map[string]any{}))
			received = append(received, r1)
			r2, _ := api.ExecuteTool(ctx, "nothing", raw(map[string]any{}))
			received = append(received, r2)
			return ToolResult{}, nil
		},
	}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "probe", args: map[string]any{}, id: "c1"}), textMessage("done"))
	_, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{plain, nothing, probe}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sub.Wait(ctx)
	if len(received) != 2 {
		t.Fatalf("received = %+v", received)
	}
	if received[0].IsError || nestedStructured(received[0]) != "a" || len(received[0].StructuredOutput) == 0 {
		t.Fatalf("plain = %+v", received[0])
	}
	if received[1].IsError || received[1].StructuredOutput == nil ||
		string(received[1].StructuredOutput) != "null" {
		t.Fatalf("nothing = %+v", received[1])
	}
}
