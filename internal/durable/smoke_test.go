package durable

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSmokeIdleInputAnswer(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	faux.setResponses(textMessage("hello"))
	h, root := openChat(t, nil, Options{Provider: faux})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("hi")})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := sub.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != SubmissionDone || rec.AnswerID == 0 {
		t.Fatalf("record = %+v", rec)
	}
	if got := textMessages(root.Context()); len(got) != 2 || got[0] != "user:hi" || got[1] != "assistant:hello" {
		t.Fatalf("context = %v", got)
	}
	if err := h.WaitForIdle(ctx, root); err != nil {
		t.Fatal(err)
	}
}

func TestSmokeToolRound(t *testing.T) {
	ctx := ctxT(t)
	var ran int
	echo := &Tool{
		Name:       "echo",
		Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}, "required": []string{"x"}}),
		Replay:     ReplaySafe,
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			ran++
			return resultOK("echoed"), nil
		},
	}
	faux := &fauxProvider{}
	faux.setResponses(
		calls(callSpec{name: "echo", args: map[string]any{"x": "a"}, id: "c1"}),
		textMessage("done"),
	)
	h, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{echo}})
	sub, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("go")})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := sub.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != SubmissionDone {
		t.Fatalf("record = %+v", rec)
	}
	if ran != 1 {
		t.Fatalf("ran = %d", ran)
	}
	results := toolResults(h, root)
	if len(results) != 1 || results["c1"].IsError || OutputText(results["c1"].Content) != "echoed" {
		t.Fatalf("results = %+v", results)
	}
}
