package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/store"
)

// commitInput commits one client input through the edge seam (the way the HTTP
// shell does) and returns its seq.
func commitInput(t *testing.T, k *kernel.Kernel, scope kernel.Scope, requestID, text, delivery string, extra map[string]any) int64 {
	t.Helper()
	payload := map[string]any{"text": text, "requestId": requestID}
	if delivery != "" {
		payload["delivery"] = delivery
	}
	for key, value := range extra {
		payload[key] = value
	}
	receipt, err := k.Commit(context.Background(), kernel.CommitRequest{
		Scope: scope, Kind: kernel.KindUserMessage, RequestID: requestID, Payload: mustJSON(payload),
	})
	if err != nil {
		t.Fatalf("commit input %s: %v", requestID, err)
	}
	return receipt.Seq
}

// inputSeqsOf decodes the consumption fact of an assistant message.
func inputSeqsOf(t *testing.T, ev store.Event) []int64 {
	t.Helper()
	var payload struct {
		InputSeqs []int64 `json:"inputSeqs"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("decode assistant payload: %v", err)
	}
	return payload.InputSeqs
}

func assistantEvents(evs []store.Event) []store.Event {
	var out []store.Event
	for _, ev := range evs {
		if ev.Kind == store.EventAssistantMessage {
			out = append(out, ev)
		}
	}
	return out
}

// TestGuideInputIsInjectedAfterToolResults is the kernel-loop steering
// contract: a guide input committed while the model stream is running is
// injected inside the same turn, after every sibling tool result of that turn
// was committed, and the assistant message records it in inputSeqs.
func TestGuideInputIsInjectedAfterToolResults(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "echo one", "", nil)
	lease := claimScope(t, k, tenant, session)

	fake := FakeStream()
	var (
		once     sync.Once
		guideErr error
	)
	stream := func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		once.Do(func() {
			_, guideErr = k.Commit(ctx, kernel.CommitRequest{
				Scope: scope, Kind: kernel.KindUserMessage, RequestID: "r2",
				Payload: mustJSON(map[string]any{"text": "two", "requestId": "r2", "delivery": kernel.DeliveryGuide}),
			})
		})
		return fake(ctx, model, llm, cfg)
	}
	runner := NewRunner(k, k, stream, "fake", "fake", "", discardLog())
	res, err := runner.RunAttempt(ctx, lease)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if guideErr != nil {
		t.Fatalf("guide input: %v", guideErr)
	}

	evs := eventsOf(t, st, tenant, session)
	assistants := assistantEvents(evs)
	if len(assistants) != 2 {
		t.Fatalf("assistant messages = %d, want 2 (tool call + answer)", len(assistants))
	}
	if got := inputSeqsOf(t, assistants[0]); len(got) != 1 || got[0] != 1 {
		t.Fatalf("first turn inputSeqs = %v, want [1]: the guide did not exist yet", got)
	}
	texts := assistantTexts(t, evs)
	if !strings.Contains(texts[1], "two") {
		t.Fatalf("final answer = %q, want the guide input answered inside the run", texts[1])
	}
	if got := inputSeqsOf(t, assistants[1]); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("second turn inputSeqs = %v, want [1 2]", got)
	}
	if res.Summary.Turns != 2 {
		t.Fatalf("turns = %d, want 2", res.Summary.Turns)
	}
	// The guide was consumed: the session owes nothing, so the settle is done
	// rather than a re-queue.
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease, Cursor: res.Cursor,
		ReadThrough: res.ReadThrough, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	pin, err := k.RunPin(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Status != kernel.RunStatusDone {
		t.Fatalf("run status = %s, want done (the guide was answered)", pin.Status)
	}
}

// TestGuideInputWithAttachmentsWaitsForTheNextRun pins the downgrade rule: an
// input that cannot be injected mid-turn (it carries attachments) stays in the
// log unanswered, the settle re-queues the session, and the next run presents
// it as a normal prompt.
func TestGuideInputWithAttachmentsWaitsForTheNextRun(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "echo one", "", nil)
	lease := claimScope(t, k, tenant, session)

	fake := FakeStream()
	var (
		once     sync.Once
		guideErr error
	)
	stream := func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		once.Do(func() {
			_, guideErr = k.Commit(ctx, kernel.CommitRequest{
				Scope: scope, Kind: kernel.KindUserMessage, RequestID: "r2",
				Payload: mustJSON(map[string]any{
					"text": "two", "requestId": "r2", "delivery": kernel.DeliveryGuide,
					"attachments": []map[string]any{{"type": "image", "url": "blob:1"}},
				}),
			})
		})
		return fake(ctx, model, llm, cfg)
	}
	runner := NewRunner(k, k, stream, "fake", "fake", "", discardLog())
	res, err := runner.RunAttempt(ctx, lease)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if guideErr != nil {
		t.Fatalf("guide input: %v", guideErr)
	}
	for _, assistant := range assistantEvents(eventsOf(t, st, tenant, session)) {
		for _, seq := range inputSeqsOf(t, assistant) {
			if seq == 2 {
				t.Fatalf("assistant inputSeqs consumed the attachment guide (seq 2): %+v", assistant)
			}
		}
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease, Cursor: res.Cursor,
		ReadThrough: res.ReadThrough, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	pin, err := k.RunPin(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Status != kernel.RunStatusPending {
		t.Fatalf("run status = %s, want pending (the guide input was not consumed)", pin.Status)
	}

	// The successor presents it as a prompt and answers it.
	lease2 := claimScope(t, k, tenant, session)
	runner2 := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	res2, err := runner2.RunAttempt(ctx, lease2)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	texts := assistantTexts(t, eventsOf(t, st, tenant, session))
	if last := texts[len(texts)-1]; !strings.Contains(last, "two") {
		t.Fatalf("successor answer = %q, want the queued guide input answered", last)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease2, Cursor: res2.Cursor,
		ReadThrough: res2.ReadThrough, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish second run: %v", err)
	}
	pin, err = k.RunPin(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Status != kernel.RunStatusDone {
		t.Fatalf("run status = %s, want done", pin.Status)
	}
}

// TestSelectionGuideInputWaitsForTheNextRun pins the second downgrade: an
// attempt's execution identity is fixed at load, so a guide input that names a
// model of its own must not be injected into the running turn (it would be
// answered by the running model while the log says it asked for another one).
// It waits for the next attempt, which resolves the selection it carries.
func TestSelectionGuideInputWaitsForTheNextRun(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "echo one", "", nil)
	lease := claimScope(t, k, tenant, session)

	fake := FakeStream()
	var (
		once     sync.Once
		guideErr error
	)
	stream := func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		once.Do(func() {
			_, guideErr = k.Commit(ctx, kernel.CommitRequest{
				Scope: scope, Kind: kernel.KindUserMessage, RequestID: "r2",
				Payload: mustJSON(map[string]any{
					"text": "two", "requestId": "r2", "delivery": kernel.DeliveryGuide,
					"selection": map[string]any{"providerId": "s2api", "modelId": "other-model"},
				}),
			})
		})
		return fake(ctx, model, llm, cfg)
	}
	runner := NewRunner(k, k, stream, "fake", "fake", "", discardLog())
	res, err := runner.RunAttempt(ctx, lease)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if guideErr != nil {
		t.Fatalf("guide input: %v", guideErr)
	}
	for _, assistant := range assistantEvents(eventsOf(t, st, tenant, session)) {
		for _, seq := range inputSeqsOf(t, assistant) {
			if seq == 2 {
				t.Fatalf("assistant inputSeqs consumed the selection guide (seq 2): %+v", assistant)
			}
		}
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease, Cursor: res.Cursor,
		ReadThrough: res.ReadThrough, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	pin, err := k.RunPin(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Status != kernel.RunStatusPending {
		t.Fatalf("run status = %s, want pending (the selection guide was not consumed)", pin.Status)
	}

	// The successor presents it and resolves the model the input asked for.
	lease2 := claimScope(t, k, tenant, session)
	resolver := &stubResolver{resolved: ActiveModel{
		ProviderID: "s2api", Model: "other-model", APIKey: "k",
		Stream: FakeStream(), ThinkingLevel: agentcore.ThinkingLow,
	}}
	runner2 := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	runner2.Models = resolver
	res2, err := runner2.RunAttempt(ctx, lease2)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if selections := resolver.selections(); len(selections) != 1 || !strings.Contains(selections[0], "other-model") {
		t.Fatalf("the successor resolved %v, want the queued selection", selections)
	}
	texts := assistantTexts(t, eventsOf(t, st, tenant, session))
	if last := texts[len(texts)-1]; !strings.Contains(last, "two") {
		t.Fatalf("successor answer = %q, want the queued guide input answered", last)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease2, Cursor: res2.Cursor,
		ReadThrough: res2.ReadThrough, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish second run: %v", err)
	}
}

// TestQueueAndGuideMixAreEachAnsweredOnce is the interleaving case: a queue
// input and a guide input committed in the same window. The guide is consumed
// inside the running turn, the queue input waits for the next run, and neither
// is answered twice.
func TestQueueAndGuideMixAreEachAnsweredOnce(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "echo one", "", nil)
	lease := claimScope(t, k, tenant, session)

	fake := FakeStream()
	var (
		once     sync.Once
		inputErr error
	)
	stream := func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		once.Do(func() {
			_, inputErr = k.Commit(ctx, kernel.CommitRequest{
				Scope: scope, Kind: kernel.KindUserMessage, RequestID: "r-queue",
				Payload: mustJSON(map[string]any{"text": "three", "requestId": "r-queue"}),
			})
			if inputErr != nil {
				return
			}
			_, inputErr = k.Commit(ctx, kernel.CommitRequest{
				Scope: scope, Kind: kernel.KindUserMessage, RequestID: "r-guide",
				Payload: mustJSON(map[string]any{"text": "two", "requestId": "r-guide", "delivery": kernel.DeliveryGuide}),
			})
		})
		return fake(ctx, model, llm, cfg)
	}
	runner := NewRunner(k, k, stream, "fake", "fake", "", discardLog())
	res, err := runner.RunAttempt(ctx, lease)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if inputErr != nil {
		t.Fatalf("mid-run input: %v", inputErr)
	}
	evs := eventsOf(t, st, tenant, session)
	assistants := assistantEvents(evs)
	if len(assistants) != 2 {
		t.Fatalf("assistant messages = %d, want 2", len(assistants))
	}
	if got := inputSeqsOf(t, assistants[1]); len(got) != 2 || got[1] != 3 {
		t.Fatalf("guide turn inputSeqs = %v, want the guide seq 3 consumed", got)
	}
	texts := assistantTexts(t, evs)
	if !strings.Contains(texts[1], "two") {
		t.Fatalf("guide answer = %q", texts[1])
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease, Cursor: res.Cursor,
		ReadThrough: res.ReadThrough, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	pin, err := k.RunPin(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Status != kernel.RunStatusPending {
		t.Fatalf("run status = %s, want pending (the queue input at seq 2 is unanswered)", pin.Status)
	}

	// The next run answers the queue input — and only it: the guide is already
	// consumed, so presenting it as a new prompt would answer it twice.
	lease2 := claimScope(t, k, tenant, session)
	runner2 := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	res2, err := runner2.RunAttempt(ctx, lease2)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	evs = eventsOf(t, st, tenant, session)
	texts = assistantTexts(t, evs)
	if len(texts) != 3 {
		t.Fatalf("assistant messages = %d, want 3", len(texts))
	}
	if !strings.Contains(texts[2], "three") {
		t.Fatalf("queue answer = %q, want the queue input", texts[2])
	}
	var twos, threes int
	for _, text := range texts {
		if strings.Contains(text, "two") {
			twos++
		}
		if strings.Contains(text, "three") {
			threes++
		}
	}
	if twos != 1 || threes != 1 {
		t.Fatalf("answers two=%d three=%d, want exactly one each", twos, threes)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease2, Cursor: res2.Cursor,
		ReadThrough: res2.ReadThrough, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish second run: %v", err)
	}
	pin, err = k.RunPin(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Status != kernel.RunStatusDone {
		t.Fatalf("run status = %s, want done", pin.Status)
	}
}

// TestTwoGuidesAreInjectedOneAtATime pins the one-at-a-time rule: two guide
// inputs committed in the same window are injected at successive drain points,
// each answered exactly once and in order.
func TestTwoGuidesAreInjectedOneAtATime(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "echo one", "", nil)
	lease := claimScope(t, k, tenant, session)

	fake := FakeStream()
	var (
		once     sync.Once
		guideErr error
	)
	stream := func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		once.Do(func() {
			for i, text := range []string{"two", "three"} {
				requestID := "g" + string(rune('1'+i))
				if _, err := k.Commit(ctx, kernel.CommitRequest{
					Scope: scope, Kind: kernel.KindUserMessage, RequestID: requestID,
					Payload: mustJSON(map[string]any{"text": text, "requestId": requestID, "delivery": kernel.DeliveryGuide}),
				}); err != nil {
					guideErr = err
					return
				}
			}
		})
		return fake(ctx, model, llm, cfg)
	}
	runner := NewRunner(k, k, stream, "fake", "fake", "", discardLog())
	res, err := runner.RunAttempt(ctx, lease)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if guideErr != nil {
		t.Fatalf("guide inputs: %v", guideErr)
	}
	texts := assistantTexts(t, eventsOf(t, st, tenant, session))
	if len(texts) != 3 {
		t.Fatalf("assistant messages = %d, want 3 (tool turn + two guide turns): %q", len(texts), texts)
	}
	if !strings.Contains(texts[1], "two") || !strings.Contains(texts[2], "three") {
		t.Fatalf("guide answers = %q / %q, want two then three", texts[1], texts[2])
	}
	var twos, threes int
	for _, text := range texts {
		if strings.Contains(text, "two") {
			twos++
		}
		if strings.Contains(text, "three") {
			threes++
		}
	}
	if twos != 1 || threes != 1 {
		t.Fatalf("answers two=%d three=%d, want exactly one each", twos, threes)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease, Cursor: res.Cursor,
		ReadThrough: res.ReadThrough, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	pin, err := k.RunPin(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Status != kernel.RunStatusDone {
		t.Fatalf("run status = %s, want done (both guides consumed)", pin.Status)
	}
}

// TestGuideCommittedBeforeTheRunIsQueueInput pins the "no live attempt" half of
// the downgrade: an input marked guide while nothing is running is simply part
// of the next run's transcript (queue semantics), consumed exactly once.
func TestGuideCommittedBeforeTheRunIsQueueInput(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "one", "", nil)
	commitInput(t, k, scope, "r2", "two", kernel.DeliveryGuide, nil)

	lease := claimScope(t, k, tenant, session)
	runner := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	res, err := runner.RunAttempt(ctx, lease)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	evs := eventsOf(t, st, tenant, session)
	assistants := assistantEvents(evs)
	if len(assistants) != 1 {
		t.Fatalf("assistant messages = %d, want 1", len(assistants))
	}
	texts := assistantTexts(t, evs)
	if !strings.Contains(texts[0], "two") {
		t.Fatalf("answer = %q, want the newest input answered", texts[0])
	}
	if got := inputSeqsOf(t, assistants[0]); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("inputSeqs = %v, want [1 2]", got)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease, Cursor: res.Cursor,
		ReadThrough: res.ReadThrough, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	pin, err := k.RunPin(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Status != kernel.RunStatusDone {
		t.Fatalf("run status = %s, want done (both inputs were presented)", pin.Status)
	}
}

// TestRunFinishedIsBookkeepingForTheTranscript pins that the attempt summary
// never becomes a model message: the transcript decoder skips it.
func TestRunFinishedIsBookkeepingForTheTranscript(t *testing.T) {
	evs := []store.Event{
		{Seq: 1, Kind: store.EventUserMessage, Payload: mustJSON(map[string]any{"text": "hi"})},
		{Seq: 2, Kind: store.EventAssistantMessage, Payload: mustJSON(agentcore.AssistantMessage{
			RoleField: agentcore.RoleAssistant,
			Content:   agentcore.ContentList{agentcore.NewTextContent("hello")},
		})},
		{Seq: 3, Kind: store.EventRunFinished, Payload: mustJSON(map[string]any{"status": "done", "turns": 1})},
	}
	msgs, seqs := DecodeMessages(evs, nil)
	if len(msgs) != 2 || len(seqs) != 2 {
		t.Fatalf("decoded %d messages / %d seqs, want 2/2", len(msgs), len(seqs))
	}
	for _, m := range msgs {
		if m.Role() == "" || m.Role() == "run.finished" {
			t.Fatalf("summary leaked into the transcript: %+v", m)
		}
	}
}

// TestFeedSnapshotsAlignWithRequests pins the fix for the guide consumption
// race: the inputSeqs of an assistant come from the snapshot taken at ITS
// request boundary, not from the feed's live state at commit time (the loop can
// run ahead through the event buffer, and a hand-out after the request must not
// be attributed to the answer that request produced).
func TestFeedSnapshotsAlignWithRequests(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	first := commitInput(t, k, scope, "g1", "guide one", kernel.DeliveryGuide, nil)
	second := commitInput(t, k, scope, "g2", "guide two", kernel.DeliveryGuide, nil)
	lease := claimScope(t, k, tenant, session)
	runner := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	feed := newGuideFeed(runner, lease, 0)

	// Request 1: the loop polls once, then the request boundary is taken.
	if got := feed.poll(ctx); len(got) != 1 {
		t.Fatalf("first poll = %d messages, want 1", len(got))
	}
	feed.beginRequest()
	// Request 2: the loop polls again (one at a time) and prepares.
	if got := feed.poll(ctx); len(got) != 1 {
		t.Fatalf("second poll = %d messages, want 1", len(got))
	}
	feed.beginRequest()

	// The assistant of request 1 must record only what request 1 presented.
	firstSnapshot := feed.takeRequestSeqs()
	if len(firstSnapshot) != 1 || firstSnapshot[0] != first {
		t.Fatalf("request 1 snapshot = %v, want [%d]", firstSnapshot, first)
	}
	secondSnapshot := feed.takeRequestSeqs()
	if len(secondSnapshot) != 2 || secondSnapshot[0] != first || secondSnapshot[1] != second {
		t.Fatalf("request 2 snapshot = %v, want [%d %d]", secondSnapshot, first, second)
	}
	// A late hand-out is never attributed backwards.
	if got := feed.poll(ctx); len(got) != 0 {
		t.Fatalf("third poll = %d messages, want 0 (both handed out)", len(got))
	}
	if got := feed.takeRequestSeqs(); len(got) != 0 {
		t.Fatalf("snapshot after the two requests = %v, want none", got)
	}
}
