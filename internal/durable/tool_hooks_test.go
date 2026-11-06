package durable

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
)

// Ported from packages/durable/test/harness-tool-hooks.test.ts.

type seenHook struct {
	Call   ToolHookCall
	Result ToolResult
}

func codesOf(result ToolResult) []string {
	var out []string
	for _, d := range result.Diagnostics {
		out = append(out, d.Code)
	}
	return out
}

func TestAfterToolOnResultsTheCallPhaseDecides_SeesAndReplacesUnavailableInvalidAndBlockedResults(t *testing.T) {
	ctx := ctxT(t)
	var ran int
	echo := &Tool{
		Name: "echo",
		Parameters: raw(map[string]any{"type": "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			ran++
			return resultOK("ran"), nil
		},
	}
	gone := &Tool{
		Name: "gone", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			return ToolResult{}, nil
		},
	}
	var seen []seenHook
	var seenMu sync.Mutex
	hook := &Hook{
		Task: ToolTaskKind,
		BeforeTool: func(ctx context.Context, call ToolHookCall, api *ToolAPI) (*BeforeToolDecision, error) {
			switch call.ID {
			case "block":
				return &BeforeToolDecision{Block: "not today"}, nil
			case "rewrite":
				return &BeforeToolDecision{Arguments: raw(map[string]any{"text": map[string]any{"not": "a string"}})}, nil
			}
			return nil, nil
		},
		AfterTool: func(ctx context.Context, call ToolHookCall, result ToolResult, api *ToolAPI) (*ToolResult, error) {
			seenMu.Lock()
			seen = append(seen, seenHook{Call: call, Result: result})
			seenMu.Unlock()
			out := result
			out.HasOutput = true
			out.Output = agentcore.ContentList{agentcore.NewTextContent("hooked " + call.ID)}
			out.Details = map[string]string{"by": "hook"}
			return &out, nil
		},
	}
	faux := &fauxProvider{}
	var root *Conversation
	faux.setResponses(
		func(_ context.Context, req Request) (agentcore.AssistantMessage, error) {
			_ = root.Configure(ctx, AgentState{Tools: &[]string{"echo"}})
			return calls(
				callSpec{name: "gone", args: map[string]any{}, id: "unavailable"},
				callSpec{name: "echo", args: map[string]any{"text": map[string]any{"not": "a string"}}, id: "invalid"},
				callSpec{name: "echo", args: map[string]any{"text": "x"}, id: "rewrite"},
				callSpec{name: "echo", args: map[string]any{}, id: "block"},
			), nil
		},
		textMessage("done"),
	)
	h, r := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{echo, gone}, Hooks: []*Hook{hook}})
	root = r
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := sub.Wait(ctx); err != nil || settled.Status != SubmissionDone {
		t.Fatalf("settled = %+v, %v", settled, err)
	}
	if ran != 0 {
		t.Fatalf("ran = %d", ran)
	}
	byID := map[string]seenHook{}
	var ids []string
	for _, s := range seen {
		byID[s.Call.ID] = s
		ids = append(ids, s.Call.ID)
	}
	if len(ids) != 4 {
		t.Fatalf("seen = %v", ids)
	}
	if got := codesOf(byID["unavailable"].Result); len(got) == 0 || got[0] != "tool_unavailable" {
		t.Fatalf("unavailable codes = %v", got)
	}
	if got := codesOf(byID["invalid"].Result); len(got) == 0 || got[0] != "invalid_arguments" {
		t.Fatalf("invalid codes = %v", got)
	}
	if got := codesOf(byID["rewrite"].Result); len(got) == 0 || got[0] != "invalid_arguments" {
		t.Fatalf("rewrite codes = %v", got)
	}
	if got := codesOf(byID["block"].Result); len(got) == 0 || got[0] != "blocked" {
		t.Fatalf("block codes = %v", got)
	}
	if string(byID["invalid"].Call.Arguments) != `{"text":{"not":"a string"}}` {
		t.Fatalf("invalid seen arguments = %s", byID["invalid"].Call.Arguments)
	}
	if string(byID["rewrite"].Call.Arguments) != `{"text":"x"}` {
		t.Fatalf("rewrite seen arguments = %s", byID["rewrite"].Call.Arguments)
	}
	messages := toolResults(h, root)
	for _, id := range []string{"unavailable", "invalid", "rewrite", "block"} {
		msg, ok := messages[id]
		if !ok {
			t.Fatalf("missing result %s", id)
		}
		if !msg.IsError {
			t.Fatalf("%s not an error", id)
		}
		if !strings.Contains(fmt.Sprint(msg.Details), "by:") {
			t.Fatalf("%s details = %#v", id, msg.Details)
		}
		if !strings.HasPrefix(OutputText(msg.Content), "hooked "+id) ||
			!strings.Contains(OutputText(msg.Content), "<harness>") {
			t.Fatalf("%s text = %q", id, OutputText(msg.Content))
		}
	}
}

func TestAfterToolOnResultsTheCallPhaseDecides_BoundsAReplacementOutputWithTheToolsLimitsOrTheDefaults(t *testing.T) {
	ctx := ctxT(t)
	small := &Tool{
		Name: "small", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		OutputLimits: OutputLimits{MaxBytes: 10},
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			return ToolResult{}, nil
		},
	}
	hook := &Hook{
		Task: ToolTaskKind,
		BeforeTool: func(ctx context.Context, call ToolHookCall, api *ToolAPI) (*BeforeToolDecision, error) {
			return &BeforeToolDecision{Block: "no"}, nil
		},
		AfterTool: func(ctx context.Context, call ToolHookCall, result ToolResult, api *ToolAPI) (*ToolResult, error) {
			text := strings.Repeat("x", 100)
			if call.ID == "c1" {
				text = strings.Repeat("y", 60*1024)
			}
			out := result
			out.HasOutput = true
			out.Output = agentcore.ContentList{agentcore.NewTextContent(text)}
			return &out, nil
		},
	}
	faux := &fauxProvider{}
	var root *Conversation
	faux.setResponses(
		func(_ context.Context, req Request) (agentcore.AssistantMessage, error) {
			_ = root.Configure(ctx, AgentState{Tools: &[]string{}})
			return calls(callSpec{name: "small", args: map[string]any{}, id: "c1"}), nil
		},
		textMessage("done"),
		calls(callSpec{name: "small", args: map[string]any{}, id: "c2"}),
		textMessage("done"),
	)
	h, r := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{small}, Hooks: []*Hook{hook}})
	root = r
	first, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = first.Wait(ctx)
	_ = root.Configure(ctx, AgentState{Tools: nil})
	second, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("again")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = second.Wait(ctx)
	messages := toolResults(h, root)
	unresolved := messages["c1"]
	if len(unresolved.Content) == 0 || OutputText(agentcore.ContentList{unresolved.Content[0]}) != strings.Repeat("y", 50*1024) {
		t.Fatalf("unresolved first block = %d bytes", len(OutputText(agentcore.ContentList{unresolved.Content[0]})))
	}
	if codes := toolCodes(h, root, "c1"); len(codes) != 2 || codes[0] != "tool_unavailable" || codes[1] != "truncated" {
		t.Fatalf("c1 codes = %v", codes)
	}
	bounded := messages["c2"]
	if len(bounded.Content) == 0 || OutputText(agentcore.ContentList{bounded.Content[0]}) != strings.Repeat("x", 10) {
		t.Fatalf("bounded = %q", OutputText(bounded.Content))
	}
	if codes := toolCodes(h, root, "c2"); len(codes) != 2 || codes[0] != "blocked" || codes[1] != "truncated" {
		t.Fatalf("c2 codes = %v", codes)
	}
}

func TestAfterToolOnResultsTheCallPhaseDecides_LetsAHooksTerminateOnABlockedCallCountTowardTheRound(t *testing.T) {
	ctx := ctxT(t)
	echo := &Tool{
		Name: "echo", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			return ToolResult{Control: &ToolControl{Terminate: true}}, nil
		},
	}
	hook := &Hook{
		Task: ToolTaskKind,
		BeforeTool: func(ctx context.Context, call ToolHookCall, api *ToolAPI) (*BeforeToolDecision, error) {
			if call.ID == "c2" {
				return &BeforeToolDecision{Block: "no"}, nil
			}
			return nil, nil
		},
		AfterTool: func(ctx context.Context, call ToolHookCall, result ToolResult, api *ToolAPI) (*ToolResult, error) {
			out := result
			out.Control = &ToolControl{Terminate: true}
			return &out, nil
		},
	}
	faux := &fauxProvider{}
	faux.setResponses(
		calls(callSpec{name: "echo", args: map[string]any{}, id: "c1"}, callSpec{name: "echo", args: map[string]any{}, id: "c2"}),
		textMessage("done"),
	)
	_, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{echo}, Hooks: []*Hook{hook}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := sub.Wait(ctx); err != nil || settled.Status != SubmissionDone {
		t.Fatalf("settled = %+v, %v", settled, err)
	}
	if faux.callCount() != 1 {
		t.Fatalf("calls = %d", faux.callCount())
	}
}

func TestAfterToolOnResultsTheCallPhaseDecides_RunsOnAnAbortResultButNotOnCallsThatNeverStarted(t *testing.T) {
	ctx := ctxT(t)
	started := newDeferred()
	first := &Tool{
		Name: "first", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			started.resolve()
			<-ctx.Done()
			return ToolResult{}, ctx.Err()
		},
	}
	second := &Tool{
		Name: "second", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			return ToolResult{}, nil
		},
	}
	var seen []seenHook
	hook := &Hook{Task: ToolTaskKind, AfterTool: func(ctx context.Context, call ToolHookCall, result ToolResult, api *ToolAPI) (*ToolResult, error) {
		seen = append(seen, seenHook{Call: call, Result: result})
		out := result
		out.Details = map[string]bool{"hooked": true}
		return &out, nil
	}}
	faux := &fauxProvider{}
	faux.setResponses(
		calls(callSpec{name: "first", args: map[string]any{}, id: "c1"}, callSpec{name: "second", args: map[string]any{}, id: "c2"}),
		textMessage("done"),
	)
	h, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{first, second}, Hooks: []*Hook{hook},
		Settings: Settings{ToolExecution: ToolExecutionSequential}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	<-started.ch
	if err := root.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	if settled, err := sub.Wait(ctx); err != nil || settled.Status != SubmissionUnanswered {
		t.Fatalf("submission = %+v, %v", settled, err)
	}
	if len(seen) != 1 || seen[0].Call.ID != "c1" || len(codesOf(seen[0].Result)) == 0 || codesOf(seen[0].Result)[0] != "aborted" {
		t.Fatalf("seen = %+v", seen)
	}
	messages := toolResults(h, root)
	if !strings.Contains(fmt.Sprint(messages["c1"].Details), "hooked:true") {
		t.Fatalf("c1 details = %#v", messages["c1"].Details)
	}
	if messages["c2"].Details != nil {
		t.Fatalf("c2 details = %#v", messages["c2"].Details)
	}
}

func TestAfterToolOnResultsTheCallPhaseDecides_ReportsAThrowingHookOnAnAbortResultAndSettlesTheResultAsBuilt(t *testing.T) {
	ctx := ctxT(t)
	started := newDeferred()
	work := &Tool{
		Name: "work", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			api.Output("work " + strings.Repeat("o", 40) + "\n")
			api.Details(map[string]int{"run": 1})
			started.resolve()
			<-ctx.Done()
			return ToolResult{}, ctx.Err()
		},
	}
	var reports []error
	hook := &Hook{Task: ToolTaskKind, AfterTool: func(ctx context.Context, call ToolHookCall, result ToolResult, api *ToolAPI) (*ToolResult, error) {
		return nil, context.DeadlineExceeded
	}}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "work", args: map[string]any{}, id: "c1"}), textMessage("done"))
	h, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{work}, Hooks: []*Hook{hook},
		OnReport: func(err error) { reports = append(reports, err) }})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	<-started.ch
	if err := root.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	_, _ = sub.Wait(ctx)
	messages := toolResults(h, root)
	if !strings.HasPrefix(OutputText(messages["c1"].Content), "work "+strings.Repeat("o", 40)) {
		t.Fatalf("c1 text = %q", OutputText(messages["c1"].Content))
	}
	if codes := toolCodes(h, root, "c1"); len(codes) != 1 || codes[0] != "aborted" {
		t.Fatalf("codes = %v", codes)
	}
	if len(reports) != 1 {
		t.Fatalf("reports = %v", reports)
	}
}

func TestCallsToToolsTheRequestDidNotOffer_RunAfterToolInsideTheirOwnToolTask(t *testing.T) {
	ctx := ctxT(t)
	var seen []struct {
		id     string
		taskID TaskID
	}
	hook := &Hook{Task: ToolTaskKind, AfterTool: func(ctx context.Context, call ToolHookCall, result ToolResult, api *ToolAPI) (*ToolResult, error) {
		seen = append(seen, struct {
			id     string
			taskID TaskID
		}{call.ID, api.TaskID})
		out := result
		out.HasOutput = true
		out.Output = agentcore.ContentList{agentcore.NewTextContent("try another tool")}
		return &out, nil
	}}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "ghost", args: map[string]any{}, id: "c1"}), textMessage("done"))
	h, root := openChat(t, nil, Options{Provider: faux, Hooks: []*Hook{hook}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sub.Wait(ctx)
	if len(seen) != 1 || seen[0].id != "c1" {
		t.Fatalf("seen = %+v", seen)
	}
	task, ok := h.Task(seen[0].taskID)
	if !ok || task.Kind != ToolTaskKind {
		t.Fatalf("task = %+v", task)
	}
	var input ToolTaskInput
	_ = json.Unmarshal(task.Input, &input)
	if input.Kind != "model" || input.CallID != "c1" || !input.Unoffered {
		t.Fatalf("input = %+v", input)
	}
	messages := toolResults(h, root)
	if !strings.HasPrefix(OutputText(messages["c1"].Content), "try another tool") {
		t.Fatalf("text = %q", OutputText(messages["c1"].Content))
	}
	if codes := toolCodes(h, root, "c1"); len(codes) != 1 || codes[0] != "tool_unavailable" {
		t.Fatalf("codes = %v", codes)
	}
}

func TestCallsToToolsTheRequestDidNotOffer_CountTowardARoundsTerminate(t *testing.T) {
	ctx := ctxT(t)
	echo := &Tool{Name: "echo", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			return ToolResult{}, nil
		}}
	hook := &Hook{Task: ToolTaskKind, AfterTool: func(ctx context.Context, call ToolHookCall, result ToolResult, api *ToolAPI) (*ToolResult, error) {
		out := result
		out.Control = &ToolControl{Terminate: true}
		return &out, nil
	}}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "echo", args: map[string]any{}, id: "c1"}, callSpec{name: "ghost", args: map[string]any{}, id: "c2"}), textMessage("done"))
	_, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{echo}, Hooks: []*Hook{hook}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := sub.Wait(ctx); err != nil || settled.Status != SubmissionDone {
		t.Fatalf("settled = %+v, %v", settled, err)
	}
	if faux.callCount() != 1 {
		t.Fatalf("calls = %d", faux.callCount())
	}
}

func TestCallsToToolsTheRequestDidNotOffer_TakeTheirTurnInASequentialRoundAndTheLastHandoffInCallOrderWins(t *testing.T) {
	ctx := ctxT(t)
	var order []string
	mk := func(name string) *Tool {
		return &Tool{Name: name, Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
			Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
				order = append(order, name)
				return ToolResult{}, nil
			}}
	}
	hook := &Hook{Task: ToolTaskKind, AfterTool: func(ctx context.Context, call ToolHookCall, result ToolResult, api *ToolAPI) (*ToolResult, error) {
		order = append(order, "hook "+call.ID)
		if call.ID == "c1" {
			return nil, nil
		}
		out := result
		out.Control = &ToolControl{Handoff: "from " + call.ID}
		return &out, nil
	}}
	faux := &fauxProvider{}
	faux.setResponses(
		calls(callSpec{name: "a", args: map[string]any{}, id: "c1"}, callSpec{name: "ghost", args: map[string]any{}, id: "c2"}, callSpec{name: "b", args: map[string]any{}, id: "c3"}),
		textMessage("done"),
	)
	h, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{mk("a"), mk("b")}, Hooks: []*Hook{hook},
		Settings: Settings{ToolExecution: ToolExecutionSequential}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sub.Wait(ctx)
	want := []string{"a", "hook c1", "hook c2", "b", "hook c3"}
	if len(order) != len(want) {
		t.Fatalf("order = %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v", order)
		}
	}
	var resets []EntryRecord
	for _, e := range h.Entries(root) {
		if e.Kind == "pi.reset" {
			resets = append(resets, e)
		}
	}
	if len(resets) != 1 {
		t.Fatalf("resets = %d", len(resets))
	}
	msg := resets[0].Model[0].(agentcore.UserMessage)
	if OutputText(msg.Content) != "from c3" {
		t.Fatalf("handoff = %q", OutputText(msg.Content))
	}
}

func TestAfterToolOnResultsRebuiltFromTheSlot_RunsOnAnInterruptedCallAReplacedOutputLosesTheSlotsTruncationDiagnostic(t *testing.T) {
	ctx := ctxT(t)
	store := NewMemStore()
	started := map[string]*deferred{"work": newDeferred(), "keep": newDeferred()}
	mk := func(name string) *Tool {
		return &Tool{
			Name: name, Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
			OutputLimits: OutputLimits{MaxBytes: 10, Retain: "tail"},
			Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
				api.Output(name + " " + strings.Repeat("o", 40) + "\n")
				api.Details(map[string]int{"run": 1})
				started[name].resolve()
				<-ctx.Done()
				return ToolResult{}, ctx.Err()
			},
		}
	}
	var seen []seenHook
	hook := &Hook{Task: ToolTaskKind, AfterTool: func(ctx context.Context, call ToolHookCall, result ToolResult, api *ToolAPI) (*ToolResult, error) {
		seen = append(seen, seenHook{Call: call, Result: result})
		if call.Name != "work" {
			return nil, nil
		}
		out := result
		out.HasOutput = true
		out.Output = agentcore.ContentList{agentcore.NewTextContent("redacted")}
		return &out, nil
	}}
	faux1 := &fauxProvider{}
	faux1.setResponses(calls(
		callSpec{name: "work", args: map[string]any{}, id: "c1"},
		callSpec{name: "keep", args: map[string]any{}, id: "c2"},
	), textMessage("done"))
	h1, root1 := openChat(t, store, Options{Provider: faux1, Tools: []*Tool{mk("work"), mk("keep")}, Hooks: []*Hook{hook}})
	sub, err := root1.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	<-started["work"].ch
	<-started["keep"].ch
	h1.Kill()

	faux2 := &fauxProvider{}
	faux2.setResponses(textMessage("done"))
	h2, root2 := openChat(t, store, Options{Provider: faux2, Tools: []*Tool{mk("work"), mk("keep")}, Hooks: []*Hook{hook}})
	h2.Recover()
	reopened, _ := h2.Submission(sub.ID())
	if settled, err := reopened.Wait(ctx); err != nil || settled.Status != SubmissionDone {
		t.Fatalf("settled = %+v, %v", settled, err)
	}
	if len(seen) != 2 {
		t.Fatalf("seen = %+v", seen)
	}
	byID := map[string]seenHook{}
	for _, s := range seen {
		byID[s.Call.ID] = s
	}
	if got := codesOf(byID["c1"].Result); len(got) != 1 || got[0] != "interrupted" {
		t.Fatalf("c1 codes = %v", got)
	}
	if got := codesOf(byID["c2"].Result); len(got) != 1 || got[0] != "interrupted" {
		t.Fatalf("c2 hook codes = %v", got)
	}
	messages := toolResults(h2, root2)
	if !strings.HasPrefix(OutputText(messages["c1"].Content), "redacted") {
		t.Fatalf("c1 text = %q", OutputText(messages["c1"].Content))
	}
	if got := toolCodes(h2, root2, "c1"); len(got) != 1 || got[0] != "interrupted" {
		t.Fatalf("c1 stored codes = %v", got)
	}
	if got := toolCodes(h2, root2, "c2"); len(got) != 2 || got[0] != "interrupted" || got[1] != "truncated" {
		t.Fatalf("c2 stored codes = %v", got)
	}
}

func TestCallsToToolsTheRequestDidNotOffer_SettleUnavailableNotAbortedWhenTheirSequentialRoundIsAbortedBeforeTheyStart(t *testing.T) {
	ctx := ctxT(t)
	started := newDeferred()
	wait := &Tool{
		Name: "wait", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			started.resolve()
			<-ctx.Done()
			return ToolResult{}, ctx.Err()
		},
	}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "wait", args: map[string]any{}, id: "c1"}, callSpec{name: "ghost", args: map[string]any{}, id: "c2"}), textMessage("done"))
	h, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{wait},
		Settings: Settings{ToolExecution: ToolExecutionSequential}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	<-started.ch
	if err := root.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	if settled, err := sub.Wait(ctx); err != nil || settled.Status != SubmissionUnanswered {
		t.Fatalf("submission = %+v, %v", settled, err)
	}
	if got := toolCodes(h, root, "c1"); len(got) != 1 || got[0] != "aborted" {
		t.Fatalf("c1 codes = %v", got)
	}
	if got := toolCodes(h, root, "c2"); len(got) != 1 || got[0] != "tool_unavailable" {
		t.Fatalf("c2 codes = %v", got)
	}
}

func TestCallsToToolsTheRequestDidNotOffer_EachGetAToolTaskInAParallelRoundOfOnlySuchCalls(t *testing.T) {
	ctx := ctxT(t)
	var seen []string
	var seenMu sync.Mutex
	hook := &Hook{Task: ToolTaskKind, AfterTool: func(ctx context.Context, call ToolHookCall, result ToolResult, api *ToolAPI) (*ToolResult, error) {
		seenMu.Lock()
		seen = append(seen, call.ID)
		seenMu.Unlock()
		return nil, nil
	}}
	faux := &fauxProvider{}
	faux.setResponses(
		calls(
			callSpec{name: "ghost", args: map[string]any{}, id: "c1"},
			callSpec{name: "spirit", args: map[string]any{}, id: "c2"},
			callSpec{name: "ghost", args: map[string]any{}, id: "c3"},
		),
		textMessage("done"),
	)
	h, root := openChat(t, nil, Options{Provider: faux, Hooks: []*Hook{hook}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := sub.Wait(ctx); err != nil || settled.Status != SubmissionDone {
		t.Fatalf("settled = %+v, %v", settled, err)
	}
	if len(seen) != 3 {
		t.Fatalf("seen = %v", seen)
	}
	for _, id := range []string{"c1", "c2", "c3"} {
		if got := toolCodes(h, root, id); len(got) != 1 || got[0] != "tool_unavailable" {
			t.Fatalf("%s codes = %v", id, got)
		}
	}
	if faux.callCount() != 2 {
		t.Fatalf("calls = %d", faux.callCount())
	}
}
