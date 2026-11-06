package durable

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
)

// Ported from packages/durable/test/harness-nested-tools.test.ts.

func echoToolDef(state *struct{ runs int }) *Tool {
	return &Tool{
		Name: "echo",
		Parameters: raw(map[string]any{"type": "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			state.runs++
			var in struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(args, &in)
			return resultOK("echo " + in.Text), nil
		},
	}
}

func toolTasksOf(t *testing.T, h *Harness, conv *Conversation) []TaskRecord {
	t.Helper()
	var out []TaskRecord
	for _, task := range h.Tasks(conv) {
		if task.Kind == ToolTaskKind {
			out = append(out, task)
		}
	}
	return out
}

func nestedStructured(r NestedResult) any {
	if r.StructuredOutput == nil {
		return nil
	}
	var out any
	_ = json.Unmarshal(r.StructuredOutput, &out)
	return out
}

func TestNestedToolCalls_RunsANestedCallAsItsOwnToolTaskAndReturnsItsResultToTheCaller(t *testing.T) {
	ctx := ctxT(t)
	state := &struct{ runs int }{}
	echo := echoToolDef(state)
	var nested *NestedResult
	batch := &Tool{
		Name: "batch", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			result, err := api.ExecuteTool(ctx, "echo", raw(map[string]string{"text": "a"}))
			if err != nil {
				return ToolResult{}, err
			}
			nested = &result
			return resultOK("got " + OutputText(resultStructuredContent(result))), nil
		},
	}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "batch", args: map[string]any{}, id: "c1"}), textMessage("done"))
	h, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{echo, batch}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := sub.Wait(ctx)
	if err != nil || settled.Status != SubmissionDone {
		t.Fatalf("settled = %+v, %v", settled, err)
	}
	tasks := toolTasksOf(t, h, root)
	if len(tasks) != 2 {
		t.Fatalf("tasks = %d", len(tasks))
	}
	parent, child := tasks[0], tasks[1]
	if nested == nil || nested.TaskID != child.ID || nested.IsError {
		t.Fatalf("nested = %+v", nested)
	}
	if got := nestedStructured(*nested); got != "echo a" {
		t.Fatalf("structured = %#v", got)
	}
	if len(nested.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %+v", nested.Diagnostics)
	}
	messages := toolResults(h, root)
	if len(messages) != 1 || !strings.HasPrefix(OutputText(messages["c1"].Content), "got echo a") {
		t.Fatalf("results = %+v", messages)
	}
	var parentInput, childInput ToolTaskInput
	_ = json.Unmarshal(parent.Input, &parentInput)
	_ = json.Unmarshal(child.Input, &childInput)
	if parentInput.Kind != "model" || parentInput.CallID != "c1" {
		t.Fatalf("parent input = %+v", parentInput)
	}
	if child.Owner != parent.ID {
		t.Fatalf("child owner = %s", child.Owner)
	}
	if childInput.Kind != "nested" || childInput.Parent != parent.ID || childInput.ParentCallID != "c1" ||
		childInput.Key != "1" || childInput.Call == nil || childInput.Call.ID != "c1/1" || childInput.Call.Name != "echo" {
		t.Fatalf("child input = %+v", childInput)
	}
	if _, ok := h.Store().GetDoc(root.ID(), docNested, string(parent.ID)+"/1"); ok {
		t.Fatalf("nested document not retired")
	}
	if live := h.Live(root); live.Run != nil || live.NestedTools != nil {
		t.Fatalf("live = %+v", live)
	}
}

func resultStructuredContent(r NestedResult) agentcore.ContentList {
	if len(r.StructuredOutput) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(r.StructuredOutput, &s); err == nil {
		return agentcore.ContentList{agentcore.NewTextContent(s)}
	}
	return nil
}

func TestNestedToolCalls_GivesEachNestedCallItsOwnTaskAndCallID(t *testing.T) {
	ctx := ctxT(t)
	type seen struct {
		taskID TaskID
		callID string
	}
	var got []seen
	var gotMu sync.Mutex
	who := &Tool{
		Name: "who", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			gotMu.Lock()
			got = append(got, seen{taskID: api.TaskID, callID: api.CallID})
			gotMu.Unlock()
			return ToolResult{}, nil
		},
	}
	batch := &Tool{
		Name: "batch", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			var wg = make(chan error, 2)
			go func() {
				_, err := api.ExecuteTool(ctx, "who", raw(map[string]any{}), WithKey("a"))
				wg <- err
			}()
			go func() {
				_, err := api.ExecuteTool(ctx, "who", raw(map[string]any{}), WithKey("b"))
				wg <- err
			}()
			<-wg
			<-wg
			return ToolResult{}, nil
		},
	}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "batch", args: map[string]any{}, id: "c1"}), textMessage("done"))
	h, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{who, batch}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sub.Wait(ctx)
	if len(got) != 2 {
		t.Fatalf("seen = %+v", got)
	}
	ids := map[string]bool{got[0].callID: true, got[1].callID: true}
	if !ids["c1/a"] || !ids["c1/b"] {
		t.Fatalf("call ids = %+v", got)
	}
	if got[0].taskID == got[1].taskID {
		t.Fatalf("same task id")
	}
	_ = h
}

func TestNestedToolCalls_RunsTheToolHooksOnNestedCallsWhichSeeTheirParent(t *testing.T) {
	ctx := ctxT(t)
	state := &struct{ runs int }{}
	echo := echoToolDef(state)
	var parents []*ParentCall
	var nested []NestedResult
	hook := &Hook{
		Task: ToolTaskKind,
		BeforeTool: func(ctx context.Context, call ToolHookCall, api *ToolAPI) (*BeforeToolDecision, error) {
			parents = append(parents, call.Parent)
			if call.Parent != nil && string(call.Arguments) != "" {
				var in struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(call.Arguments, &in)
				if in.Text == "secret" {
					return &BeforeToolDecision{Block: "no secrets"}, nil
				}
			}
			return nil, nil
		},
		AfterTool: func(ctx context.Context, call ToolHookCall, result ToolResult, api *ToolAPI) (*ToolResult, error) {
			if call.Parent == nil {
				return nil, nil
			}
			out := result
			out.HasOutput = true
			out.Output = agentcore.ContentList{agentcore.NewTextContent("rewritten")}
			out.Details = map[string]string{"hooked": call.ID}
			out.Diagnostics = append(append([]Diagnostic(nil), out.Diagnostics...),
				Diagnostic{Severity: "info", Code: "hooked", Message: "seen"})
			return &out, nil
		},
	}
	batch := &Tool{
		Name: "batch", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			r1, err := api.ExecuteTool(ctx, "echo", raw(map[string]string{"text": "secret"}))
			if err != nil {
				return ToolResult{}, err
			}
			nested = append(nested, r1)
			r2, err := api.ExecuteTool(ctx, "echo", raw(map[string]string{"text": "public"}))
			if err != nil {
				return ToolResult{}, err
			}
			nested = append(nested, r2)
			return ToolResult{}, nil
		},
	}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "batch", args: map[string]any{}, id: "c1"}), textMessage("done"))
	h, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{echo, batch}, Hooks: []*Hook{hook}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sub.Wait(ctx)
	tasks := toolTasksOf(t, h, root)
	parent := tasks[0]
	if len(parents) != 3 {
		t.Fatalf("parents = %+v", parents)
	}
	if parents[0] != nil || parents[1] == nil || parents[1].TaskID != parent.ID || parents[1].CallID != "c1" {
		t.Fatalf("parents = %+v", parents)
	}
	if len(nested) != 2 || !nested[0].IsError || len(nested[0].Diagnostics) < 2 ||
		nested[0].Diagnostics[0].Code != "blocked" ||
		nested[0].Diagnostics[0].Message != "Tool call blocked: no secrets" {
		t.Fatalf("nested[0] = %+v", nested[0])
	}
	if string(nested[0].Details) != `{"hooked":"c1/1"}` || nested[0].StructuredOutput != nil {
		t.Fatalf("nested[0] details/structured = %+v", nested[0])
	}
	if got := nestedStructured(nested[1]); got != "rewritten" {
		t.Fatalf("nested[1] = %+v", nested[1])
	}
	if state.runs != 1 {
		t.Fatalf("echo runs = %d", state.runs)
	}
}

func TestNestedToolCalls_ReturnsAnErrorResultForAnUnavailableToolAndInvalidArguments(t *testing.T) {
	ctx := ctxT(t)
	echo := echoToolDef(&struct{ runs int }{})
	var nested []NestedResult
	batch := &Tool{
		Name: "batch", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			r1, _ := api.ExecuteTool(ctx, "missing", raw(map[string]any{}))
			nested = append(nested, r1)
			r2, _ := api.ExecuteTool(ctx, "echo", raw(map[string]any{"text": map[string]any{"not": "a string"}}))
			nested = append(nested, r2)
			return ToolResult{}, nil
		},
	}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "batch", args: map[string]any{}, id: "c1"}), textMessage("done"))
	_, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{echo, batch}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sub.Wait(ctx)
	if len(nested) != 2 || !nested[0].IsError || nested[0].Diagnostics[0].Code != "tool_unavailable" ||
		!nested[1].IsError || nested[1].Diagnostics[0].Code != "invalid_arguments" {
		t.Fatalf("nested = %+v", nested)
	}
}

func TestNestedToolCalls_ReturnsTheCallAKeyAlreadyNamesAndRejectsReusingTheKeyForAnotherCall(t *testing.T) {
	ctx := ctxT(t)
	state := &struct{ runs int }{}
	echo := echoToolDef(state)
	var reused *NestedResult
	var reuseErr error
	batch := &Tool{
		Name: "batch", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			if _, err := api.ExecuteTool(ctx, "echo", raw(map[string]string{"text": "a"}), WithKey("k")); err != nil {
				return ToolResult{}, err
			}
			r, err := api.ExecuteTool(ctx, "echo", raw(map[string]string{"text": "a"}), WithKey("k"))
			if err != nil {
				return ToolResult{}, err
			}
			reused = &r
			_, reuseErr = api.ExecuteTool(ctx, "echo", raw(map[string]string{"text": "b"}), WithKey("k"))
			return ToolResult{}, nil
		},
	}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "batch", args: map[string]any{}, id: "c1"}), textMessage("done"))
	h, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{echo, batch}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sub.Wait(ctx)
	if state.runs != 1 {
		t.Fatalf("runs = %d", state.runs)
	}
	if got := nestedStructured(*reused); got != "echo a" {
		t.Fatalf("reused = %+v", reused)
	}
	if reuseErr == nil || !strings.Contains(reuseErr.Error(), "Nested call c1/k was already made with another tool or other arguments") {
		t.Fatalf("reuse error = %v", reuseErr)
	}
	_ = h
}

func TestNestedToolCalls_MatchesAReusedKeysArgumentsRegardlessOfKeyOrder(t *testing.T) {
	ctx := ctxT(t)
	var runs int
	pair := &Tool{
		Name: "pair",
		Parameters: raw(map[string]any{"type": "object",
			"properties": map[string]any{"a": map[string]any{"type": "string"}, "b": map[string]any{"type": "string"}},
			"required":   []string{"a", "b"}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			runs++
			var in struct {
				A string `json:"a"`
				B string `json:"b"`
			}
			_ = json.Unmarshal(args, &in)
			return resultOK(in.A + in.B), nil
		},
	}
	var nested []NestedResult
	batch := &Tool{
		Name: "batch", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			r1, _ := api.ExecuteTool(ctx, "pair", raw(map[string]any{"a": "x", "b": "y"}), WithKey("k"))
			nested = append(nested, r1)
			r2, _ := api.ExecuteTool(ctx, "pair", raw(map[string]any{"b": "y", "a": "x"}), WithKey("k"))
			nested = append(nested, r2)
			return ToolResult{}, nil
		},
	}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "batch", args: map[string]any{}, id: "c1"}), textMessage("done"))
	_, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{pair, batch}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sub.Wait(ctx)
	if runs != 1 {
		t.Fatalf("runs = %d", runs)
	}
	if nestedStructured(nested[0]) != "xy" || nestedStructured(nested[1]) != "xy" {
		t.Fatalf("nested = %+v", nested)
	}
}

func TestNestedToolCalls_AbortsTheNestedCallsACallerLeftRunningWhenItReturns(t *testing.T) {
	ctx := ctxT(t)
	running := newDeferred()
	hang := &Tool{
		Name: "hang", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			api.Output("partial\n")
			api.Details(map[string]int{"step": 1})
			running.resolve()
			<-ctx.Done()
			return ToolResult{}, ctx.Err()
		},
	}
	batch := &Tool{
		Name: "batch", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			go func() {
				_, _ = api.ExecuteTool(ctx, "hang", raw(map[string]any{}))
			}()
			<-running.ch
			return resultOK("returned early"), nil
		},
	}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "batch", args: map[string]any{}, id: "c1"}), textMessage("done"))
	h, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{hang, batch}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := sub.Wait(ctx)
	if err != nil || settled.Status != SubmissionDone {
		t.Fatalf("settled = %+v, %v", settled, err)
	}
	messages := toolResults(h, root)
	if !strings.HasPrefix(OutputText(messages["c1"].Content), "returned early") {
		t.Fatalf("result = %q", OutputText(messages["c1"].Content))
	}
	tasks := toolTasksOf(t, h, root)
	if len(tasks) != 2 || tasks[1].State.Outcome == nil || tasks[1].State.Outcome.Status != OutcomeAborted {
		t.Fatalf("child = %+v", tasks[1].State)
	}
}

func TestNestedToolCalls_AbortsARunningNestedCallWhenItsCallerThrows(t *testing.T) {
	ctx := ctxT(t)
	running := newDeferred()
	hang := &Tool{
		Name: "hang", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			running.resolve()
			<-ctx.Done()
			return ToolResult{}, ctx.Err()
		},
	}
	batch := &Tool{
		Name: "batch", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			go func() {
				_, _ = api.ExecuteTool(ctx, "hang", raw(map[string]any{}))
			}()
			<-running.ch
			return ToolResult{}, errors.New("caller failed")
		},
	}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "batch", args: map[string]any{}, id: "c1"}), textMessage("done"))
	h, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{hang, batch}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := sub.Wait(ctx); err != nil || settled.Status != SubmissionDone {
		t.Fatalf("settled = %+v, %v", settled, err)
	}
	messages := toolResults(h, root)
	if !strings.Contains(OutputText(messages["c1"].Content), "caller failed") {
		t.Fatalf("result = %q", OutputText(messages["c1"].Content))
	}
	tasks := toolTasksOf(t, h, root)
	if len(tasks) != 2 || tasks[1].State.Outcome == nil || tasks[1].State.Outcome.Status != OutcomeAborted {
		t.Fatalf("child = %+v", tasks[1].State)
	}
}

func TestNestedToolCalls_StopsOnlyTheWaitWhenACallerCancelsIt(t *testing.T) {
	ctx := ctxT(t)
	cancel := make(chan struct{})
	release := newDeferred()
	var attempts []string
	var retry *NestedResult
	slow := &Tool{
		Name: "slow", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			close(cancel)
			if err := release.wait(ctx); err != nil {
				return ToolResult{}, err
			}
			return resultOK("slow done"), nil
		},
	}
	batch := &Tool{
		Name: "batch", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			waitCtx, cancelFn := context.WithCancel(ctx)
			go func() {
				<-cancel
				cancelFn()
			}()
			if _, err := api.ExecuteTool(waitCtx, "slow", raw(map[string]any{}), WithKey("same")); err != nil {
				attempts = append(attempts, "first cancelled")
			} else {
				attempts = append(attempts, "first done")
			}
			release.resolve()
			r, err := api.ExecuteTool(ctx, "slow", raw(map[string]any{}), WithKey("same"))
			if err != nil {
				return ToolResult{}, err
			}
			retry = &r
			return ToolResult{}, nil
		},
	}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "batch", args: map[string]any{}, id: "c1"}), textMessage("done"))
	h, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{slow, batch}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sub.Wait(ctx)
	if len(attempts) != 1 || attempts[0] != "first cancelled" {
		t.Fatalf("attempts = %v", attempts)
	}
	if retry == nil || retry.IsError || nestedStructured(*retry) != "slow done" {
		t.Fatalf("retry = %+v", retry)
	}
	if tasks := toolTasksOf(t, h, root); len(tasks) != 2 {
		t.Fatalf("tasks = %d", len(tasks))
	}
}

func TestNestedToolCalls_AdmitsOneNestedCallForAKeyUsedByConcurrentCalls(t *testing.T) {
	ctx := ctxT(t)
	state := &struct{ runs int }{}
	echo := echoToolDef(state)
	var both []NestedResult
	batch := &Tool{
		Name: "batch", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			ch := make(chan NestedResult, 2)
			errs := make(chan error, 2)
			for i := 0; i < 2; i++ {
				go func() {
					r, err := api.ExecuteTool(ctx, "echo", raw(map[string]string{"text": "a"}), WithKey("k"))
					if err != nil {
						errs <- err
						return
					}
					ch <- r
				}()
			}
			for i := 0; i < 2; i++ {
				select {
				case r := <-ch:
					both = append(both, r)
				case err := <-errs:
					return ToolResult{}, err
				}
			}
			return ToolResult{}, nil
		},
	}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "batch", args: map[string]any{}, id: "c1"}), textMessage("done"))
	h, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{echo, batch}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sub.Wait(ctx)
	if state.runs != 1 {
		t.Fatalf("runs = %d", state.runs)
	}
	if len(both) != 2 || nestedStructured(both[0]) != "echo a" || nestedStructured(both[1]) != "echo a" {
		t.Fatalf("results = %+v", both)
	}
	if tasks := toolTasksOf(t, h, root); len(tasks) != 2 {
		t.Fatalf("tasks = %d", len(tasks))
	}
}

func TestNestedToolCalls_RejectsExplicitKeysThatAreEmptyHaveASlashAreProtoOrPositiveIntegers(t *testing.T) {
	ctx := ctxT(t)
	echo := echoToolDef(&struct{ runs int }{})
	var errs []string
	batch := &Tool{
		Name: "batch", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			for _, key := range []string{"", "a/b", "__proto__", "7"} {
				_, err := api.ExecuteTool(ctx, "echo", raw(map[string]any{}), WithKey(key))
				if err != nil {
					errs = append(errs, err.Error())
				}
			}
			return ToolResult{}, nil
		},
	}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "batch", args: map[string]any{}, id: "c1"}), textMessage("done"))
	_, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{echo, batch}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sub.Wait(ctx)
	rule := `must be non-empty, without "/", not "__proto__", and not a positive integer`
	want := []string{
		`Nested call key "" ` + rule,
		`Nested call key "a/b" ` + rule,
		`Nested call key "__proto__" ` + rule,
		`Nested call key "7" ` + rule,
	}
	if len(errs) != len(want) {
		t.Fatalf("errs = %v", errs)
	}
	for i := range want {
		if errs[i] != want[i] {
			t.Fatalf("errs = %v", errs)
		}
	}
}

func TestNestedToolCalls_RunsNestedCallsOfNestedCallsAndEndsThemChildrenFirst(t *testing.T) {
	ctx := ctxT(t)
	state := &struct{ runs int }{}
	echo := echoToolDef(state)
	inner := &Tool{
		Name: "inner", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			r, err := api.ExecuteTool(ctx, "echo", raw(map[string]string{"text": "deep"}), WithKey("x"))
			if err != nil {
				return ToolResult{}, err
			}
			return resultOK(OutputText(resultStructuredContent(r))), nil
		},
	}
	outer := &Tool{
		Name: "outer", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			r, err := api.ExecuteTool(ctx, "inner", raw(map[string]any{}))
			if err != nil {
				return ToolResult{}, err
			}
			return resultOK(OutputText(resultStructuredContent(r))), nil
		},
	}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "outer", args: map[string]any{}, id: "c1"}), textMessage("done"))
	h, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{echo, inner, outer}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sub.Wait(ctx)
	messages := toolResults(h, root)
	if !strings.HasPrefix(OutputText(messages["c1"].Content), "echo deep") {
		t.Fatalf("result = %q", OutputText(messages["c1"].Content))
	}
	tasks := toolTasksOf(t, h, root)
	if len(tasks) != 3 {
		t.Fatalf("tasks = %d", len(tasks))
	}
	if tasks[1].Owner != tasks[0].ID || tasks[2].Owner != tasks[1].ID {
		t.Fatalf("owners = %s, %s, %s", tasks[0].ID, tasks[1].Owner, tasks[2].Owner)
	}
}

func TestNestedToolCalls_GivesCallersWhatASchemaLessToolStreamedBeforeItThrew(t *testing.T) {
	ctx := ctxT(t)
	fails := &Tool{
		Name: "fails", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			api.Output("half way\n")
			return ToolResult{}, errors.New("broke")
		},
	}
	var result *NestedResult
	batch := &Tool{
		Name: "batch", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			r, _ := api.ExecuteTool(ctx, "fails", raw(map[string]any{}))
			result = &r
			return ToolResult{}, nil
		},
	}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "batch", args: map[string]any{}, id: "c1"}), textMessage("done"))
	_, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{fails, batch}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sub.Wait(ctx)
	if result == nil || !result.IsError {
		t.Fatalf("result = %+v", result)
	}
	if got := nestedStructured(*result); got != "half way\n" {
		t.Fatalf("structured = %#v", got)
	}
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "tool_error" || result.Diagnostics[0].Message != "broke" {
		t.Fatalf("diagnostics = %+v", result.Diagnostics)
	}
}

func TestNestedToolCalls_ShowsAFinishedNestedCallAsADoneSlotWithItsSummaryUntilItsCallerSettles(t *testing.T) {
	ctx := ctxT(t)
	checked := newDeferred()
	release := newDeferred()
	echo := echoToolDef(&struct{ runs int }{})
	batch := &Tool{
		Name: "batch", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			if _, err := api.ExecuteTool(ctx, "echo", raw(map[string]string{"text": "a"})); err != nil {
				return ToolResult{}, err
			}
			if _, err := api.ExecuteTool(ctx, "missing", raw(map[string]any{})); err != nil {
				return ToolResult{}, err
			}
			checked.resolve()
			if err := release.wait(ctx); err != nil {
				return ToolResult{}, err
			}
			return ToolResult{}, nil
		},
	}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "batch", args: map[string]any{}, id: "c1"}), textMessage("done"))
	h, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{echo, batch}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	<-checked.ch
	slots := h.Live(root).NestedTools
	if len(slots) != 2 {
		t.Fatalf("slots = %+v", slots)
	}
	if slots[0].Status != "done" || slots[0].Summary == nil || slots[0].Summary.IsError {
		t.Fatalf("slot 1 = %+v", slots[0])
	}
	if slots[1].Status != "done" || slots[1].Summary == nil || !slots[1].Summary.IsError ||
		slots[1].Summary.Error != "Tool missing is not available" {
		t.Fatalf("slot 2 = %+v", slots[1])
	}
	release.resolve()
	_, _ = sub.Wait(ctx)
	if live := h.Live(root); live.NestedTools != nil {
		t.Fatalf("nested slots not retired: %+v", live.NestedTools)
	}
}

func TestNestedToolCalls_ReattachesAReplaySafeCallerToTheNestedCallsItMadeBeforeARestart(t *testing.T) {
	ctx := ctxT(t)
	store := NewMemStore()
	state := &struct{ runs int }{}
	echo := echoToolDef(state)
	blocked := newDeferred()
	runs := 0
	batch := &Tool{
		Name: "batch", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Replay: ReplaySafe,
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			runs++
			first, err := api.ExecuteTool(ctx, "echo", raw(map[string]string{"text": "a"}))
			if err != nil {
				return ToolResult{}, err
			}
			if runs == 1 {
				blocked.resolve()
				<-ctx.Done()
				return ToolResult{}, ctx.Err()
			}
			second, err := api.ExecuteTool(ctx, "echo", raw(map[string]string{"text": "b"}))
			if err != nil {
				return ToolResult{}, err
			}
			return resultOK("run " + itoa(runs) + ": " + OutputText(resultStructuredContent(first)) + ", " +
				OutputText(resultStructuredContent(second))), nil
		},
	}
	faux1 := &fauxProvider{}
	faux1.setResponses(calls(callSpec{name: "batch", args: map[string]any{}, id: "c1"}), textMessage("done"))
	h1, root1 := openChat(t, store, Options{Provider: faux1, Tools: []*Tool{echo, batch}})
	sub, err := root1.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	<-blocked.ch
	h1.Kill()

	faux2 := &fauxProvider{}
	faux2.setResponses(textMessage("done"))
	h2, root2 := openChat(t, store, Options{Provider: faux2, Tools: []*Tool{echo, batch}})
	h2.Recover()
	reopened, ok := h2.Submission(sub.ID())
	if !ok {
		t.Fatalf("submission missing")
	}
	if settled, err := reopened.Wait(ctx); err != nil || settled.Status != SubmissionDone {
		t.Fatalf("settled = %+v, %v", settled, err)
	}
	if runs != 2 || state.runs != 2 {
		t.Fatalf("runs = %d, echo = %d", runs, state.runs)
	}
	messages := toolResults(h2, root2)
	if !strings.HasPrefix(OutputText(messages["c1"].Content), "run 2: echo a, echo b") {
		t.Fatalf("result = %q", OutputText(messages["c1"].Content))
	}
	nestedTasks := 0
	for _, task := range toolTasksOf(t, h2, root2) {
		var input ToolTaskInput
		_ = json.Unmarshal(task.Input, &input)
		if input.Kind == "nested" {
			nestedTasks++
		}
	}
	if nestedTasks != 2 {
		t.Fatalf("nested tasks = %d", nestedTasks)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestNestedToolCalls_RunsOnAnAbandonedNestedCallsResult(t *testing.T) {
	ctx := ctxT(t)
	store := NewMemStore()
	gated := newDeferred()
	var seen []seenHook
	hook := &Hook{
		Task: ToolTaskKind,
		BeforeTool: func(hookCtx context.Context, call ToolHookCall, api *ToolAPI) (*BeforeToolDecision, error) {
			if call.Name != "gated" || call.Parent == nil {
				return nil, nil
			}
			gated.resolve()
			<-hookCtx.Done()
			return nil, hookCtx.Err()
		},
		AfterTool: func(hookCtx context.Context, call ToolHookCall, result ToolResult, api *ToolAPI) (*ToolResult, error) {
			if call.Parent == nil {
				return nil, nil
			}
			seen = append(seen, seenHook{Call: call, Result: result})
			return nil, nil
		},
	}
	gate := &Tool{
		Name: "gated", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			return ToolResult{}, nil
		},
	}
	batch := &Tool{
		Name: "batch", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			if _, err := api.ExecuteTool(ctx, "gated", raw(map[string]any{})); err != nil {
				return ToolResult{}, err
			}
			return ToolResult{}, nil
		},
	}
	faux1 := &fauxProvider{}
	faux1.setResponses(calls(callSpec{name: "batch", args: map[string]any{}, id: "c1"}), textMessage("done"))
	h1, root1 := openChat(t, store, Options{Provider: faux1, Tools: []*Tool{gate, batch}, Hooks: []*Hook{hook}})
	sub, err := root1.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	<-gated.ch
	h1.Close()

	faux2 := &fauxProvider{}
	faux2.setResponses(textMessage("done"))
	h2, _ := openChat(t, store, Options{Provider: faux2, Tools: []*Tool{gate, batch}, Hooks: []*Hook{hook}})
	h2.Recover()
	reopened, _ := h2.Submission(sub.ID())
	if settled, err := reopened.Wait(ctx); err != nil || settled.Status != SubmissionDone {
		t.Fatalf("settled = %+v, %v", settled, err)
	}
	if len(seen) != 1 || seen[0].Call.ID != "c1/1" {
		t.Fatalf("seen = %+v", seen)
	}
	if got := codesOf(seen[0].Result); len(got) != 1 || got[0] != "abandoned" {
		t.Fatalf("codes = %v", got)
	}
}

func TestNestedToolCalls_CommitsOnlyTheStatusOfANestedCallMadeWithProgressFalse(t *testing.T) {
	ctx := ctxT(t)
	running := newDeferred()
	release := newDeferred()
	chatty := &Tool{
		Name: "chatty", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			api.Output("line\n")
			api.Diagnostic(Diagnostic{Severity: "info", Message: "note"})
			api.Details(map[string]int{"step": 1})
			running.resolve()
			if err := release.wait(ctx); err != nil {
				return ToolResult{}, err
			}
			return ToolResult{}, nil
		},
	}
	var result *NestedResult
	batch := &Tool{
		Name: "batch", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			r, err := api.ExecuteTool(ctx, "chatty", raw(map[string]any{}), WithoutProgress())
			if err != nil {
				return ToolResult{}, err
			}
			result = &r
			return ToolResult{}, nil
		},
	}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "batch", args: map[string]any{}, id: "c1"}), textMessage("done"))
	h, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{chatty, batch}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	<-running.ch
	slots := h.Live(root).NestedTools
	if len(slots) != 1 || slots[0].Status != "running" || slots[0].Output != "" ||
		slots[0].Details != nil || slots[0].Diagnostics != nil {
		t.Fatalf("slot = %+v", slots)
	}
	release.resolve()
	_, _ = sub.Wait(ctx)
	if result == nil || nestedStructured(*result) != "line\n" || len(result.Diagnostics) != 1 ||
		result.Diagnostics[0].Message != "note" || string(result.Details) != `{"step":1}` {
		t.Fatalf("result = %+v", result)
	}
}

func TestNestedToolCalls_LosesTheRunningOutputOfANestedCallMadeWithProgressFalseWhenItIsAborted(t *testing.T) {
	ctx := ctxT(t)
	running := newDeferred()
	chatty := &Tool{
		Name: "chatty", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			api.Output("line\n")
			api.Diagnostic(Diagnostic{Severity: "info", Message: "note"})
			api.Details(map[string]int{"step": 1})
			running.resolve()
			<-ctx.Done()
			return ToolResult{}, ctx.Err()
		},
	}
	results := make(chan NestedResult, 1)
	batch := &Tool{
		Name: "batch", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			go func() {
				r, _ := api.ExecuteTool(ctx, "chatty", raw(map[string]any{}), WithoutProgress())
				results <- r
			}()
			<-running.ch
			return ToolResult{}, nil
		},
	}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "batch", args: map[string]any{}, id: "c1"}), textMessage("done"))
	_, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{chatty, batch}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sub.Wait(ctx)
	result := <-results
	if !result.IsError || result.StructuredOutput != nil || result.Details != nil {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "aborted" {
		t.Fatalf("diagnostics = %+v", result.Diagnostics)
	}
}

func TestNestedToolCalls_ShowsANestedCallsArgumentsInItsSlotAsMadeThenAsItRuns(t *testing.T) {
	ctx := ctxT(t)
	hookEntered := newDeferred()
	hookRelease := newDeferred()
	running := newDeferred()
	release := newDeferred()
	sawBefore := make(chan string, 1)
	large := strings.Repeat("x", 64*1024)
	slow := &Tool{
		Name: "slow", Parameters: raw(map[string]any{"type": "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			running.resolve()
			if err := release.wait(ctx); err != nil {
				return ToolResult{}, err
			}
			return ToolResult{}, nil
		},
	}
	var nestedInput ToolTaskInput
	var argsSeen []string
	batch := &Tool{
		Name: "batch", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			_, _ = api.ExecuteTool(ctx, "slow", raw(map[string]string{"text": "asked"}))
			return ToolResult{}, nil
		},
	}
	hook := &Hook{Task: ToolTaskKind, BeforeTool: func(ctx context.Context, call ToolHookCall, api *ToolAPI) (*BeforeToolDecision, error) {
		if call.Parent == nil {
			return nil, nil
		}
		sawBefore <- string(call.Arguments)
		hookEntered.resolve()
		if err := hookRelease.wait(ctx); err != nil {
			return nil, err
		}
		return &BeforeToolDecision{Arguments: raw(map[string]string{"text": large})}, nil
	}}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "batch", args: map[string]any{}, id: "c1"}), textMessage("done"))
	h, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{slow, batch}, Hooks: []*Hook{hook}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	<-hookEntered.ch
	argsSeen = append(argsSeen, <-sawBefore)
	slot := func() NestedToolSlot {
		slots := h.Live(root).NestedTools
		if len(slots) != 1 {
			t.Fatalf("slots = %+v", slots)
		}
		return slots[0]
	}
	before := slot()
	if before.Status != "pending" || string(before.Arguments) != `{"text":"asked"}` {
		t.Fatalf("before = %+v", before)
	}
	hookRelease.resolve()
	<-running.ch
	after := slot()
	if after.Status != "running" || string(after.Arguments) != string(raw(map[string]string{"text": large})) {
		t.Fatalf("after = %+v", after)
	}
	release.resolve()
	_, _ = sub.Wait(ctx)
	for _, task := range toolTasksOf(t, h, root) {
		var input ToolTaskInput
		_ = json.Unmarshal(task.Input, &input)
		if input.Kind == "nested" {
			nestedInput = input
		}
	}
	if string(nestedInput.Call.Arguments) != `{"text":"asked"}` {
		t.Fatalf("nested input = %+v", nestedInput)
	}
	_ = argsSeen
}

func TestNestedToolCalls_RunsANestedCallWhoseRepairedArgumentsSetAnOptionalPropertyToUndefined(t *testing.T) {
	ctx := ctxT(t)
	var seen []string
	repaired := &Tool{
		Name: "repaired",
		Parameters: raw(map[string]any{"type": "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}}}),
		PrepareArguments: func(args json.RawMessage) (json.RawMessage, error) {
			return raw(map[string]any{}), nil
		},
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			seen = append(seen, string(args))
			return ToolResult{}, nil
		},
	}
	var result *NestedResult
	batch := &Tool{
		Name: "batch", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			r, _ := api.ExecuteTool(ctx, "repaired", raw(map[string]string{"text": "x"}))
			result = &r
			return ToolResult{}, nil
		},
	}
	faux := &fauxProvider{}
	faux.setResponses(calls(callSpec{name: "batch", args: map[string]any{}, id: "c1"}), textMessage("done"))
	_, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{repaired, batch}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sub.Wait(ctx)
	if result == nil || result.IsError {
		t.Fatalf("result = %+v", result)
	}
	if len(seen) != 1 || seen[0] != `{}` {
		t.Fatalf("args seen = %v", seen)
	}
}
