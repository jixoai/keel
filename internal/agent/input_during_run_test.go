package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/store"
)

// midRunInput builds a committer wrapper that commits one more client input on
// the FIRST model call: the exact window the "input during the model stream"
// safety point describes. It then behaves like the deterministic fake.
//
// The input is committed through the kernel's edge path (user.message carries
// no fence), and the client's enqueue is part of the commit: the same
// transaction marks the run row pending_input, so a run already executing sees
// it without another writer.
func midRunInput(t *testing.T, k *kernel.Kernel, scope kernel.Scope, requestID, text string) (provider.StreamFn, func() error) {
	t.Helper()
	fake := FakeStream()
	var (
		once sync.Once
		err  error
	)
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		once.Do(func() {
			_, err = k.Commit(ctx, kernel.CommitRequest{
				Scope:     scope,
				Kind:      kernel.KindUserMessage,
				RequestID: requestID,
				Payload:   mustJSON(map[string]any{"text": text, "requestId": requestID}),
			})
		})
		return fake(ctx, model, llm, cfg)
	}, func() error { return err }
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

// TestInputDuringRunIsNotStranded is the regression test for the input-during-
// run defect: a user message committed while the model stream is running was
// read by nobody, and because the run's own assistant commit advanced the
// cursor past it, the finish probe reported "no more input" and the session
// converged to done with the message unanswered forever.
//
// The fix is the read-through watermark: the run reports the highest seq its
// transcript was built from, and input committed after it forces a re-queue.
func TestInputDuringRunIsNotStranded(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	if _, err := k.Commit(ctx, kernel.CommitRequest{Scope: scope, Kind: kernel.KindUserMessage,
		RequestID: "r1", Payload: mustJSON(map[string]any{"text": "one", "requestId": "r1"})}); err != nil {
		t.Fatal(err)
	}

	lease := claimScope(t, k, tenant, session)
	stream, inputErr := midRunInput(t, k, scope, "r2", "two")
	runner := NewRunner(k, k, stream, "fake", "fake", "", discardLog())
	cursor, readThrough, err := runner.RunWithWatermark(ctx, lease)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := inputErr(); err != nil {
		t.Fatalf("mid-run input: %v", err)
	}
	if readThrough != 1 {
		t.Fatalf("read-through watermark = %d, want 1 (the transcript was built from seq 1)", readThrough)
	}
	if cursor != 3 {
		t.Fatalf("cursor = %d, want 3 (the run committed the assistant message after the input)", cursor)
	}

	// Settle exactly the way the worker does, with the watermark.
	if err := k.Finish(ctx, kernel.FinishRequest{
		Lease: lease, Cursor: cursor, ReadThrough: readThrough, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	pin, err := k.RunPin(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Status != kernel.RunStatusPending {
		t.Fatalf("run status = %s, want pending: the input at seq 2 was never read", pin.Status)
	}

	// The re-queued run answers it: exactly two final replies, no duplicates.
	lease2, err := k.ClaimScope(ctx, scope, "runner-test", time.Minute)
	if err != nil {
		t.Fatalf("claim after requeue: %v", err)
	}
	runner2 := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	cursor2, readThrough2, err := runner2.RunWithWatermark(ctx, lease2)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if readThrough2 != 3 {
		t.Fatalf("second watermark = %d, want 3", readThrough2)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{
		Lease: lease2, Cursor: cursor2, ReadThrough: readThrough2, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish second run: %v", err)
	}
	pin, err = k.RunPin(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Status != kernel.RunStatusDone {
		t.Fatalf("run status = %s, want done after the input was answered", pin.Status)
	}

	events := eventsOf(t, st, tenant, session)
	if len(events) != 4 {
		t.Fatalf("log = %d events, want 4 (2 user + 2 assistant)", len(events))
	}
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("log has a hole: seq %d at position %d", ev.Seq, i)
		}
	}
	user, assistant := countKind(events, store.EventUserMessage), countKind(events, store.EventAssistantMessage)
	if user != 2 || assistant != 2 {
		t.Fatalf("log = %d user / %d assistant, want 2/2", user, assistant)
	}
	// The second input has an answer after it: that is the invariant the defect
	// violated. (The exact text is the fake provider's business; the ordering
	// is the log's contract.)
	var last agentcore.AssistantMessage
	if err := json.Unmarshal(events[len(events)-1].Payload, &last); err != nil {
		t.Fatal(err)
	}
	if len(last.Content) == 0 {
		t.Fatalf("final reply has no content: %s", events[len(events)-1].Payload)
	}
	if hasUnansweredInput(t, st, tenant, session) {
		t.Fatal("the second input is still unanswered")
	}
}

// TestFinishUsesWatermarkAndFlag pins the three sources of evidence the finish
// uses to decide "does the next attempt owe a turn", from most to least
// precise: the read-through watermark, the edge-written pending_input flag, and
// the failure case.
func TestFinishUsesWatermarkAndFlag(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	if _, err := k.Commit(ctx, kernel.CommitRequest{Scope: scope, Kind: kernel.KindUserMessage,
		RequestID: "r1", Payload: mustJSON(map[string]any{"text": "one", "requestId": "r1"})}); err != nil {
		t.Fatal(err)
	}
	lease := claimScope(t, k, tenant, session)
	stream, inputErr := midRunInput(t, k, scope, "r2", "two")
	runner := NewRunner(k, k, stream, "fake", "fake", "", discardLog())
	cursor, readThrough, err := runner.RunWithWatermark(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	if err := inputErr(); err != nil {
		t.Fatal(err)
	}

	// A caller that supplies no watermark only has the legacy contract: its
	// cursor is the read boundary, and the kernel takes it at its word. That is
	// why the production worker always passes the watermark.
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease, Cursor: cursor, MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}
	pin, err := k.RunPin(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Status != kernel.RunStatusDone {
		t.Fatalf("cursor-only finish status = %s, want done (the caller's cursor is its read point)", pin.Status)
	}
	if pin.PendingInput {
		t.Fatal("cursor-only finish left the unread-input flag set")
	}

	// The truthful watermark plus a claim that delivers the flag: input the
	// attempt never read re-queues the run and reaches the next attempt.
	if err := st.EnqueueRun(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	lease2, err := k.ClaimScope(ctx, scope, "runner-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{
		Lease: lease2, Cursor: cursor, ReadThrough: readThrough, MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}
	pin, err = k.RunPin(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Status != kernel.RunStatusPending || !pin.PendingInput {
		t.Fatalf("watermark finish = %s (flag %t), want pending with the flag set", pin.Status, pin.PendingInput)
	}

	// The claim delivers the flag, and the attempt that reads the input leaves
	// nothing pending: no spurious extra turn.
	lease3, err := k.ClaimScope(ctx, scope, "runner-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !lease3.PendingInput {
		t.Fatal("the claim did not deliver the unread-input flag")
	}
	runner3 := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	cursor3, watermark3, err := runner3.RunWithWatermark(ctx, lease3)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{
		Lease: lease3, Cursor: cursor3, ReadThrough: watermark3, MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}
	pin, err = k.RunPin(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Status != kernel.RunStatusDone || pin.PendingInput {
		t.Fatalf("finish after reading the input = %s (flag %t), want done clean", pin.Status, pin.PendingInput)
	}
}

// hasUnansweredInput reports whether some committed user message has no
// assistant message after it (the invariant, not just the tail).
func hasUnansweredInput(t *testing.T, st *store.Store, tenant, session uuid.UUID) bool {
	t.Helper()
	pending := false
	for _, ev := range eventsOf(t, st, tenant, session) {
		switch ev.Kind {
		case store.EventUserMessage:
			pending = true
		case store.EventAssistantMessage:
			pending = false
		}
	}
	return pending
}

var _ = errors.Is

// countingStream wraps the deterministic fake and counts model calls.
func countingStream(calls *atomic.Int64) provider.StreamFn {
	fake := FakeStream()
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		calls.Add(1)
		return fake(ctx, model, llm, cfg)
	}
}

// TestAbandonedAttemptStillOwesAModelCall is the B3 regression at the runner
// level: an attempt abandoned before it reads the log (drain, kill, release)
// must not consume the unread-input fact. The next attempt has to issue a model
// call — the exact assertion the adversarial review asked for — and only a
// finish whose watermark covers the input clears it.
func TestAbandonedAttemptStillOwesAModelCall(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	// Input one is answered by attempt 1; input two arrives while it runs.
	if _, err := k.Commit(ctx, kernel.CommitRequest{Scope: scope, Kind: kernel.KindUserMessage,
		RequestID: "r1", Payload: mustJSON(map[string]any{"text": "one", "requestId": "r1"})}); err != nil {
		t.Fatal(err)
	}
	lease1 := claimScope(t, k, tenant, session)
	if !lease1.PendingInput {
		t.Fatal("the first claim did not deliver the queued input")
	}
	stream, inputErr := midRunInput(t, k, scope, "r2", "two")
	runner := NewRunner(k, k, stream, "fake", "fake", "", discardLog())
	cursor, readThrough, err := runner.RunWithWatermark(ctx, lease1)
	if err != nil {
		t.Fatal(err)
	}
	if err := inputErr(); err != nil {
		t.Fatal(err)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{
		Lease: lease1, Cursor: cursor, ReadThrough: readThrough, MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}

	// Attempt 2 claims and is abandoned before reading anything (the drain/kill
	// path: it returns without Finish; the release stands in for the lease
	// hand-back).
	lease2, err := k.ClaimScope(ctx, scope, "runtime-abandoned", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !lease2.PendingInput {
		t.Fatal("the abandoned attempt's claim did not deliver the unread input")
	}
	if err := k.Release(ctx, lease2); err != nil {
		t.Fatal(err)
	}

	// Attempt 3 must call the model: the input two is still unread.
	lease3, err := k.ClaimScope(ctx, scope, "runtime-successor", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !lease3.PendingInput {
		t.Fatal("abandoning an attempt lost the unread-input fact")
	}
	var calls atomic.Int64
	runner3 := NewRunner(k, k, countingStream(&calls), "fake", "fake", "", discardLog())
	cursor3, watermark3, err := runner3.RunWithWatermark(ctx, lease3)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() < 1 {
		t.Fatal("the successor settled without calling the model: the unread input was stranded")
	}
	if err := k.Finish(ctx, kernel.FinishRequest{
		Lease: lease3, Cursor: cursor3, ReadThrough: watermark3, MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}
	pin, err := k.RunPin(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Status != kernel.RunStatusDone || pin.PendingInput {
		t.Fatalf("after the answering attempt: status=%s flag=%t, want done and clear", pin.Status, pin.PendingInput)
	}
	if calls.Load() < 1 {
		t.Fatalf("model calls = %d, want at least 1", calls.Load())
	}
}

// TestInFlightInputIsAnsweredWithItsToolCall is the B5 regression (R1
// differential): input that arrives while an attempt is running must be
// PRESENTED to the successor's model as the last message. In the log it sits
// before that attempt's answer, so a transcript in log order ends with an
// assistant message and any model treats the pending input as already
// answered — no reply, no tool call.
//
// The interleaving is deterministic: the input is committed from inside the
// first model call (the in-flight window), and the assertion is that the
// successor answers it, including the tool call the input asks for.
func TestInFlightInputIsAnsweredWithItsToolCall(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	// Attempt 1 reads "hello" (a plain answer, no tool call).
	if _, err := k.Commit(ctx, kernel.CommitRequest{Scope: scope, Kind: kernel.KindUserMessage,
		RequestID: "r1", Payload: mustJSON(map[string]any{"text": "hello", "requestId": "r1"})}); err != nil {
		t.Fatal(err)
	}
	lease1 := claimScope(t, k, tenant, session)
	// The tool-triggering input arrives while attempt 1 streams.
	stream, inputErr := midRunInput(t, k, scope, "r2", "please echo this")
	runner := NewRunner(k, k, stream, "fake", "fake", "", discardLog())
	cursor1, watermark1, err := runner.RunWithWatermark(ctx, lease1)
	if err != nil {
		t.Fatal(err)
	}
	if err := inputErr(); err != nil {
		t.Fatal(err)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{
		Lease: lease1, Cursor: cursor1, ReadThrough: watermark1, MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}

	// Attempt 2 must answer the in-flight input, not settle on the old answer.
	lease2, err := k.ClaimScope(ctx, scope, "runtime-successor", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !lease2.PendingInput {
		t.Fatal("the successor did not see the in-flight input")
	}
	if lease2.ReadWatermark != 1 {
		t.Fatalf("successor watermark = %d, want 1 (the previous attempt's read point)", lease2.ReadWatermark)
	}
	runner2 := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	cursor2, watermark2, err := runner2.RunWithWatermark(ctx, lease2)
	if err != nil {
		t.Fatalf("successor run: %v", err)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{
		Lease: lease2, Cursor: cursor2, ReadThrough: watermark2, MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}

	// ① The echo tool ran exactly once, and its result was committed once.
	events := eventsOf(t, st, tenant, session)
	if got := countKind(events, store.EventToolResult); got != 1 {
		t.Fatalf("tool results = %d, want exactly 1 (the echo call for the in-flight input)", got)
	}
	toolCalls, results := 0, 0
	for _, ev := range events {
		switch ev.Kind {
		case store.EventAssistantMessage:
			var msg agentcore.AssistantMessage
			if err := json.Unmarshal(ev.Payload, &msg); err != nil {
				t.Fatal(err)
			}
			for _, call := range msg.ToolCalls() {
				toolCalls++
				if call.Name != "echo" {
					t.Fatalf("tool call name = %q, want echo", call.Name)
				}
			}
		case store.EventToolResult:
			var res agentcore.ToolResultMessage
			if err := json.Unmarshal(ev.Payload, &res); err != nil {
				t.Fatal(err)
			}
			if res.ToolName != "echo" {
				t.Fatalf("tool result name = %q, want echo", res.ToolName)
			}
			results++
		}
	}
	if toolCalls != 1 || results != 1 {
		t.Fatalf("echo calls/results = %d/%d, want 1/1", toolCalls, results)
	}

	// ② The final answer is the one produced from the tool result, and it comes
	// after the in-flight input.
	var last agentcore.AssistantMessage
	if err := json.Unmarshal(events[len(events)-1].Payload, &last); err != nil {
		t.Fatal(err)
	}
	text := ContentText(last.Content)
	if !strings.Contains(text, "tool echo returned") {
		t.Fatalf("final answer = %q, want the answer produced from the echo tool result", text)
	}
	var inFlightSeq int64
	for _, ev := range events {
		if ev.Kind != store.EventUserMessage {
			continue
		}
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Text == "please echo this" {
			inFlightSeq = ev.Seq
		}
	}
	if inFlightSeq == 0 || events[len(events)-1].Seq <= inFlightSeq {
		t.Fatalf("the final answer (seq %d) does not follow the in-flight input (seq %d)", events[len(events)-1].Seq, inFlightSeq)
	}

	// ③ The run converged and the log has no gap or duplicate.
	pin, err := k.RunPin(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Status != kernel.RunStatusDone {
		t.Fatalf("run status = %s, want done", pin.Status)
	}
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("log has a gap: seq %d at position %d", ev.Seq, i)
		}
	}
	if got := countKind(events, store.EventUserMessage); got != 2 {
		t.Fatalf("user events = %d, want 2", got)
	}
}
