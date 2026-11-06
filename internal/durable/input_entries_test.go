package durable

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
)

// Ported from packages/durable/test/harness-input-entries.test.ts.

func mention(text string) *InputEntry {
	return &InputEntry{Kind: "chat.mention", Data: raw(map[string]string{"author": "ann", "text": text})}
}

func transcriptOf(h *Harness, conv *Conversation) [][]any {
	var out [][]any
	for _, e := range h.Entries(conv) {
		if e.Kind == "pi.system" {
			continue
		}
		row := []any{e.Kind}
		if len(e.Data) > 0 && e.Kind != "pi.user" {
			row = append(row, json.RawMessage(e.Data))
		}
		if len(e.Model) > 0 {
			if msg, ok := e.Model[0].(agentcore.UserMessage); ok {
				row = append(row, OutputText(msg.Content))
			} else if msg, ok := e.Model[0].(agentcore.AssistantMessage); ok {
				row = append(row, textOfMessage(msg))
			}
		}
		out = append(out, row)
	}
	return out
}

func holdTool(gate *deferred) *Tool {
	return &Tool{
		Name: "hold", Parameters: raw(map[string]any{"type": "object", "properties": map[string]any{}}),
		Execute: func(ctx context.Context, args json.RawMessage, api *ToolAPI) (ToolResult, error) {
			if err := gate.wait(ctx); err != nil {
				return ToolResult{}, err
			}
			return resultOK("held"), nil
		},
	}
}

func TestInputEntries_PlacesAnIdleInputAsItsEntry(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	var requests [][]string
	faux.setResponses(func(_ context.Context, req Request) (agentcore.AssistantMessage, error) {
		requests = append(requests, textMessages(req.Messages))
		return textMessage("hi ann"), nil
	})
	h, root := openChat(t, nil, Options{Provider: faux, Now: func() int64 { return 42 }})
	sub, err := root.Submit(ctx, SubmissionDraft{
		Type: SubmissionInput, Content: raw("ann: hi"), Entry: mention("hi"),
	})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := sub.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	entries := h.Entries(root)
	if len(entries) != 2 || entries[0].Kind != "chat.mention" || entries[1].Kind != "pi.assistant" {
		t.Fatalf("entries = %+v", transcriptOf(h, root))
	}
	user, ok := entries[0].Model[0].(agentcore.UserMessage)
	if !ok || OutputText(user.Content) != "ann: hi" || user.Timestamp != 42 {
		t.Fatalf("placed model = %+v", entries[0].Model)
	}
	var data map[string]string
	_ = json.Unmarshal(entries[0].Data, &data)
	if data["author"] != "ann" || data["text"] != "hi" {
		t.Fatalf("data = %v", data)
	}
	if settled.Status != SubmissionDone || settled.EntryID != entries[0].ID || settled.AnswerID != entries[1].ID {
		t.Fatalf("settled = %+v", settled)
	}
	if len(requests) != 1 || len(requests[0]) != 1 || requests[0][0] != "user:ann: hi" {
		t.Fatalf("requests = %v", requests)
	}
}

func TestInputEntries_RejectsAnInputEntryWhoseKindIsNotANonEmptyStringOrStartsWithPi(t *testing.T) {
	ctx := ctxT(t)
	h, root := openChat(t, nil, Options{Provider: &fauxProvider{}})
	before := len(h.Entries(root))
	for _, kind := range []string{"", "pi.user", "pi.compaction"} {
		_, err := root.Submit(ctx, SubmissionDraft{
			Type: SubmissionInput, Content: raw("x"), Entry: &InputEntry{Kind: kind},
		})
		if err == nil {
			t.Fatalf("kind %q accepted", kind)
		}
	}
	if after := len(h.Entries(root)); after != before {
		t.Fatalf("rejected inputs wrote entries: %d -> %d", before, after)
	}
	if live := h.Live(root); live.Run != nil {
		t.Fatalf("rejected input started a run")
	}
	if err := root.Reset(ctx, ""); err != nil {
		t.Fatal(err)
	}
	entries := h.Entries(root)
	if entries[len(entries)-1].Kind != "pi.reset" {
		t.Fatalf("reset kind = %s", entries[len(entries)-1].Kind)
	}
}

func TestInputEntries_TakesOnlyKindAndDataFromAnUntypedCallersEntry(t *testing.T) {
	// Go's InputEntry has only kind and data, so a forged model/head/edits cannot
	// reach the entry in the first place; the placement still uses only them.
	ctx := ctxT(t)
	faux := &fauxProvider{}
	step, reached, release := gated(textMessage("first"))
	faux.setResponses(textMessage("idle"), step, textMessage("queued"))
	h, root := openChat(t, nil, Options{Provider: faux})
	idle, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("idle"), Entry: mention("idle")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = idle.Wait(ctx)
	_, err = root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("a")})
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	queued, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("later"), Entry: mention("later")})
	if err != nil {
		t.Fatal(err)
	}
	items := h.Inbox(root)
	if len(items) != 1 || items[0].Entry == nil || items[0].Entry.Kind != "chat.mention" || items[0].Entry.Data == nil {
		t.Fatalf("inbox = %+v", items)
	}
	release.resolve()
	if settled, err := queued.Wait(ctx); err != nil || settled.Status != SubmissionDone {
		t.Fatalf("queued = %+v, %v", settled, err)
	}
	var mentions []EntryRecord
	for _, e := range h.Entries(root) {
		if e.Kind == "chat.mention" {
			mentions = append(mentions, e)
		}
	}
	if len(mentions) != 2 {
		t.Fatalf("mentions = %d", len(mentions))
	}
	for i, want := range []string{"idle", "later"} {
		msg := mentions[i].Model[0].(agentcore.UserMessage)
		if OutputText(msg.Content) != want || len(mentions[i].Model) != 1 {
			t.Fatalf("mention %d = %+v", i, mentions[i])
		}
		var data map[string]string
		_ = json.Unmarshal(mentions[i].Data, &data)
		if data["text"] != want {
			t.Fatalf("mention data %d = %v", i, data)
		}
	}
}

func TestInputEntries_QueuesBusyInputsWithTheirEntriesInTheInboxAndPlacesThemAtBoundaries(t *testing.T) {
	ctx := ctxT(t)
	gate := newDeferred()
	faux := &fauxProvider{}
	faux.setResponses(
		calls(callSpec{name: "hold", args: map[string]any{}, id: "c1"}),
		textMessage("after tools"),
		textMessage("follow-up"),
	)
	h, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{holdTool(gate)}})
	input, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("a")})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		live := h.Live(root)
		return len(live.Tools) == 1 && live.Tools[0].Status == "running"
	})
	steer, err := root.Submit(ctx, SubmissionDraft{
		Type: SubmissionInput, Content: raw("s"), Entry: mention("s"), WhenBusy: "steer", RequestID: "r-s",
	})
	if err != nil {
		t.Fatal(err)
	}
	followUp, err := root.Submit(ctx, SubmissionDraft{
		Type: SubmissionInput, Content: raw("f"), Entry: &InputEntry{Kind: "p2.input"}, WhenBusy: "followUp",
	})
	if err != nil {
		t.Fatal(err)
	}
	items := h.Inbox(root)
	if len(items) != 2 || items[0].ID != steer.ID() || items[0].Mode != "steer" || items[1].ID != followUp.ID() || items[1].Mode != "followUp" {
		t.Fatalf("inbox = %+v", items)
	}
	if items[0].Entry == nil || items[0].Entry.Kind != "chat.mention" || items[1].Entry == nil || items[1].Entry.Kind != "p2.input" {
		t.Fatalf("inbox entries = %+v", items)
	}
	again, err := root.Submit(ctx, SubmissionDraft{
		Type: SubmissionInput, Content: raw("other"), Entry: mention("other"), WhenBusy: "steer", RequestID: "r-s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID() != steer.ID() {
		t.Fatalf("steer request id not deduplicated")
	}
	if _, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("x"), Entry: mention("x"), WhenBusy: "reject"}); err == nil {
		t.Fatalf("reject input accepted")
	}
	if len(h.Inbox(root)) != 2 {
		t.Fatalf("inbox changed on rejected input")
	}
	gate.resolve()
	if settled, err := followUp.Wait(ctx); err != nil || settled.Status != SubmissionDone {
		t.Fatalf("follow-up = %+v, %v", settled, err)
	}
	var assistants []EntryRecord
	var mentionEntries []EntryRecord
	var p2 []EntryRecord
	var tools []EntryRecord
	for _, e := range h.Entries(root) {
		switch e.Kind {
		case "pi.assistant":
			assistants = append(assistants, e)
		case "chat.mention":
			mentionEntries = append(mentionEntries, e)
		case "p2.input":
			p2 = append(p2, e)
		case "pi.tool-result":
			tools = append(tools, e)
		}
	}
	if len(assistants) != 3 || len(mentionEntries) != 1 || len(p2) != 1 || len(tools) != 1 {
		t.Fatalf("transcript = %+v", transcriptOf(h, root))
	}
	if rec, _ := input.Status(); rec.AnswerID != assistants[1].ID {
		t.Fatalf("input status = %+v", rec)
	}
	if rec, _ := steer.Status(); rec.Status != SubmissionDone || rec.EntryID != mentionEntries[0].ID || rec.AnswerID != assistants[1].ID {
		t.Fatalf("steer status = %+v", rec)
	}
	if rec, _ := followUp.Status(); rec.Status != SubmissionDone || rec.EntryID != p2[0].ID || rec.AnswerID != assistants[2].ID {
		t.Fatalf("follow-up status = %+v", rec)
	}
}

func TestInputEntries_WithdrawsQueuedInputsWithEntriesOneByOneAndByConversationAbort(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	step, reached, release := gated(textMessage("first"))
	faux.setResponses(step)
	h, root := openChat(t, nil, Options{Provider: faux})
	input, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("a")})
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	withdrawn, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("w"), Entry: mention("w")})
	if err != nil {
		t.Fatal(err)
	}
	aborted, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("x"), Entry: mention("x")})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := withdrawn.Abort(ctx); err != nil || got != AbortAborted {
		t.Fatalf("abort queued = %v, %v", got, err)
	}
	items := h.Inbox(root)
	if len(items) != 1 || items[0].ID != aborted.ID() {
		t.Fatalf("inbox = %+v", items)
	}
	if err := root.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []*Submission{withdrawn, aborted, input} {
		rec, err := sub.Wait(ctx)
		if err != nil || rec.Status != SubmissionUnanswered || rec.Reason != ReasonAborted {
			t.Fatalf("submission = %+v, %v", rec, err)
		}
	}
	if len(h.Inbox(root)) != 0 {
		t.Fatalf("inbox not empty")
	}
	for _, e := range h.Entries(root) {
		if e.Kind == "chat.mention" {
			t.Fatalf("withdrawn mention was placed")
		}
	}
	release.resolve()
}

func TestDataOnlyWrites_AppendsADataOnlyWriteMidRunInTimeOrder(t *testing.T) {
	ctx := ctxT(t)
	gate := newDeferred()
	faux := &fauxProvider{}
	var requests [][]string
	faux.setResponses(
		calls(callSpec{name: "hold", args: map[string]any{}, id: "c1"}),
		func(_ context.Context, req Request) (agentcore.AssistantMessage, error) {
			requests = append(requests, textMessages(req.Messages))
			return textMessage("done"), nil
		},
	)
	h, root := openChat(t, nil, Options{Provider: faux, Tools: []*Tool{holdTool(gate)}})
	input, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("a")})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		live := h.Live(root)
		return len(live.Tools) == 1 && live.Tools[0].Status == "running"
	})
	chat := &EntryDraft{Kind: "chat.message", Data: raw(map[string]string{"author": "bob", "text": "lunch?"})}
	write, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionWrite, Write: chat, RequestID: "bob:1"})
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := write.Status()
	if rec.Status != SubmissionDone || rec.EntryID == 0 {
		t.Fatalf("write = %+v", rec)
	}
	if len(h.Inbox(root)) != 0 {
		t.Fatalf("display-only write queued")
	}
	kinds := []string{}
	for _, e := range h.Entries(root) {
		kinds = append(kinds, e.Kind)
	}
	want := []string{"pi.user", "pi.assistant", "chat.message"}
	if len(kinds) != len(want) {
		t.Fatalf("entries = %v", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("entries = %v", kinds)
		}
	}
	again, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionWrite, Write: chat, RequestID: "bob:1"})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID() != write.ID() {
		t.Fatalf("write request id not deduplicated")
	}
	gate.resolve()
	if settled, err := input.Wait(ctx); err != nil || settled.Status != SubmissionDone {
		t.Fatalf("input = %+v, %v", settled, err)
	}
	kinds = nil
	for _, e := range h.Entries(root) {
		kinds = append(kinds, e.Kind)
	}
	want = []string{"pi.user", "pi.assistant", "chat.message", "pi.tool-result", "pi.assistant"}
	if len(kinds) != len(want) {
		t.Fatalf("entries after = %v", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("entries after = %v", kinds)
		}
	}
	if len(requests) != 1 {
		t.Fatalf("requests = %v", requests)
	}
	wantReq := []string{"user:a", "assistant:calling", "toolResult:held"}
	_ = wantReq
	if got := requests[0]; len(got) != 3 || got[0] != "user:a" || got[2] != "toolResult:held" {
		t.Fatalf("request = %v", got)
	}
}

func TestDataOnlyWrites_QueuesADataOnlyWriteBehindAQueuedReset(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	step, reached, release := gated(textMessage("first"))
	faux.setResponses(step)
	h, root := openChat(t, nil, Options{Provider: faux})
	input, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("a")})
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	if err := root.Reset(ctx, ""); err != nil {
		t.Fatal(err)
	}
	chat, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionWrite, Write: &EntryDraft{
		Kind: "chat.message", Data: raw("hi"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rec, _ := chat.Status(); rec.Status != SubmissionQueued {
		t.Fatalf("chat write = %+v", rec)
	}
	release.resolve()
	_, _ = input.Wait(ctx)
	settled, err := chat.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Status != SubmissionDone {
		t.Fatalf("chat = %+v", settled)
	}
	var reset *EntryRecord
	var message *EntryRecord
	for i := range h.Entries(root) {
		e := h.Entries(root)[i]
		if e.Kind == "pi.reset" {
			reset = &e
		}
		if e.Kind == "chat.message" {
			message = &e
		}
	}
	if reset == nil || message == nil || message.ID <= reset.ID {
		t.Fatalf("entries = %+v", transcriptOf(h, root))
	}
	entries := h.Entries(root)
	if entries[len(entries)-2].Kind != "pi.reset" || entries[len(entries)-1].Kind != "chat.message" {
		t.Fatalf("after reset = %v", transcriptOf(h, root))
	}
	later, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionWrite, Write: &EntryDraft{
		Kind: "chat.message", Data: raw("again"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rec, _ := later.Status(); rec.Status != SubmissionDone {
		t.Fatalf("later = %+v", rec)
	}
}

func TestDataOnlyWrites_AppendsADataOnlyWriteWhileIdleAfterAFailedRunWithoutPlacingTheQueuedItems(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	step, reached, release := gated(errorMessage("invalid request"))
	faux.setResponses(step, textMessage("for f"))
	h, root := openChat(t, nil, Options{Provider: faux})
	input, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("a")})
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	f, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("f"), Entry: mention("f")})
	if err != nil {
		t.Fatal(err)
	}
	release.resolve()
	if settled, err := input.Wait(ctx); err != nil || settled.Reason != ReasonModelError {
		t.Fatalf("input = %+v, %v", settled, err)
	}
	if err := h.WaitForIdle(ctx, root); err != nil {
		t.Fatal(err)
	}
	note, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionWrite, Write: &EntryDraft{Kind: "note", Data: raw("n")}})
	if err != nil {
		t.Fatal(err)
	}
	if rec, _ := note.Status(); rec.Status != SubmissionDone {
		t.Fatalf("note = %+v", rec)
	}
	if err := h.WaitForIdle(ctx, root); err != nil {
		t.Fatal(err)
	}
	if rec, _ := f.Status(); rec.Status != SubmissionQueued {
		t.Fatalf("queued input = %+v", rec)
	}
	if items := h.Inbox(root); len(items) != 1 || items[0].ID != f.ID() {
		t.Fatalf("inbox = %+v", items)
	}
	failure := h.Live(root).RunFailure
	if failure == nil || failure.Reason != ReasonModelError || failure.Detail != "invalid request" {
		t.Fatalf("failure = %+v", failure)
	}
	if faux.callCount() != 1 {
		t.Fatalf("calls = %d", faux.callCount())
	}
	g, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("g")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = g.Wait(ctx)
	var visible [][]any
	for _, e := range h.Entries(root) {
		if e.Kind == "pi.assistant" {
			continue
		}
		row := []any{e.Kind}
		if len(e.Data) > 0 {
			row = append(row, json.RawMessage(e.Data))
		}
		if len(e.Model) > 0 {
			if msg, ok := e.Model[0].(agentcore.UserMessage); ok {
				row = append(row, OutputText(msg.Content))
			}
		}
		visible = append(visible, row)
	}
	if len(visible) < 4 {
		t.Fatalf("transcript = %v", visible)
	}
	if visible[0][0] != "pi.user" || visible[1][0] != "note" || visible[2][0] != "chat.mention" || visible[3][0] != "pi.user" {
		t.Fatalf("transcript = %v", visible)
	}
}

func TestDataOnlyWrites_KeepsAQueuedInputsEntryAcrossReopenAndPlacesItAsThatEntry(t *testing.T) {
	ctx := ctxT(t)
	store := NewMemStore()
	faux1 := &fauxProvider{}
	step, reached, _ := gated(textMessage("first"))
	faux1.setResponses(step)
	h1, root1 := openChat(t, store, Options{Provider: faux1})
	_, err := root1.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("a")})
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	f, err := root1.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("f"), Entry: mention("f")})
	if err != nil {
		t.Fatal(err)
	}
	fid := f.ID()
	h1.Close()

	faux2 := &fauxProvider{}
	faux2.setResponses(textMessage("first again"), textMessage("for f"))
	h2, root2 := openChat(t, store, Options{Provider: faux2})
	h2.Recover()
	items := h2.Inbox(root2)
	found := false
	for _, item := range items {
		if item.ID == fid {
			found = true
			if item.Entry == nil || item.Entry.Kind != "chat.mention" {
				t.Fatalf("item = %+v", item)
			}
		}
	}
	if !found {
		t.Fatalf("queued item missing: %+v", items)
	}
	sub, ok := h2.Submission(fid)
	if !ok {
		t.Fatalf("submission missing")
	}
	settled, err := sub.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Status != SubmissionDone {
		t.Fatalf("settled = %+v", settled)
	}
	var placed *EntryRecord
	for i := range h2.Entries(root2) {
		e := h2.Entries(root2)[i]
		if e.Kind == "chat.mention" {
			placed = &e
		}
	}
	if placed == nil || settled.EntryID != placed.ID {
		t.Fatalf("placed = %+v", placed)
	}
	msg := placed.Model[0].(agentcore.UserMessage)
	if OutputText(msg.Content) != "f" {
		t.Fatalf("placed model = %+v", placed.Model)
	}
	var data map[string]string
	_ = json.Unmarshal(placed.Data, &data)
	if data["author"] != "ann" || data["text"] != "f" {
		t.Fatalf("placed data = %v", data)
	}
}

func TestDataOnlyWrites_AppendsADataOnlyWriteAtOnceBehindANonEmptyInbox(t *testing.T) {
	ctx := ctxT(t)
	faux := &fauxProvider{}
	step, reached, release := gated(textMessage("first"))
	faux.setResponses(step, textMessage("second"))
	h, root := openChat(t, nil, Options{Provider: faux})
	input, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("a")})
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	followUp, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionInput, Content: raw("f")})
	if err != nil {
		t.Fatal(err)
	}
	noteModel, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionWrite, Write: &EntryDraft{
		Kind: "note",
		Model: agentcore.MessageList{agentcore.UserMessage{
			RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("m")}, Timestamp: 0,
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var target EntryID
	for _, e := range h.Entries(root) {
		if e.Kind == "pi.user" {
			target = e.ID
		}
	}
	edit, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionWrite, Write: &EntryDraft{
		Kind: "redact", Edits: []ContextEdit{{Target: target, Action: "omit"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	note, err := root.Submit(ctx, SubmissionDraft{Type: SubmissionWrite, Write: &EntryDraft{Kind: "note", Data: raw("n")}})
	if err != nil {
		t.Fatal(err)
	}
	if rec, _ := note.Status(); rec.Status != SubmissionDone {
		t.Fatalf("data-only note = %+v", rec)
	}
	items := h.Inbox(root)
	if len(items) != 3 {
		t.Fatalf("inbox = %+v", items)
	}
	if items[0].ID != followUp.ID() || items[0].Mode != "followUp" ||
		items[1].ID != noteModel.ID() || items[1].Mode != "write" ||
		items[2].ID != edit.ID() || items[2].Mode != "write" {
		t.Fatalf("inbox = %+v", items)
	}
	release.resolve()
	if settled, err := followUp.Wait(ctx); err != nil || settled.Status != SubmissionDone {
		t.Fatalf("follow-up = %+v, %v", settled, err)
	}
	kinds := []string{}
	for _, e := range h.Entries(root) {
		kinds = append(kinds, e.Kind)
	}
	want := []string{"pi.user", "note", "pi.assistant", "note", "redact", "pi.user", "pi.assistant"}
	if len(kinds) != len(want) {
		t.Fatalf("entries = %v", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("entries = %v", kinds)
		}
	}
	if rec, _ := input.Status(); rec.Status != SubmissionDone {
		t.Fatalf("input = %+v", rec)
	}
}
