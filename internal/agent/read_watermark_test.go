package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/store"
)

// midRunInputThenFailAssistant wraps the kernel and fails the SECOND assistant
// commit while the first model call commits one more client input (the
// in-flight window). The attempt therefore commits an assistant tool call and
// its result, then dies with a store error on its final answer — the "committed
// facts, then Err Finish" window the adversarial review (N1) describes, made
// deterministic without racing the worker.
func midRunInputThenFailAssistant(t *testing.T, k *kernel.Kernel, scope kernel.Scope, requestID, text string) (*Runner, func() error) {
	t.Helper()
	var (
		inputOnce sync.Once
		inputErr  error
		assistant atomic.Int64
	)
	stream := func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		inputOnce.Do(func() {
			_, inputErr = k.Commit(ctx, kernel.CommitRequest{
				Scope:     scope,
				Kind:      kernel.KindUserMessage,
				RequestID: requestID,
				Payload:   mustJSON(map[string]any{"text": text, "requestId": requestID}),
			})
		})
		return FakeStream()(ctx, model, llm, cfg)
	}
	failing := &failingAssistantCommitter{Committer: k, failAt: 2, count: &assistant}
	runner := NewRunner(failing, k, stream, "fake", "fake", "", discardLog())
	runner.Reads = k // the wrapper only intercepts commits
	return runner, func() error { return inputErr }
}

// failingAssistantCommitter fails the Nth assistant.message commit with a store
// error (the deterministic "commit failed after earlier facts landed" window).
type failingAssistantCommitter struct {
	kernel.Committer
	failAt int
	count  *atomic.Int64
}

func (f *failingAssistantCommitter) Commit(ctx context.Context, req kernel.CommitRequest) (kernel.Receipt, error) {
	if req.Kind == kernel.KindAssistantMessage && f.count.Add(1) == int64(f.failAt) {
		return kernel.Receipt{}, errors.New("injected commit failure")
	}
	return f.Committer.Commit(ctx, req)
}

// isFinalAssistant reports whether the event is an assistant message with no
// tool call (a user-visible final answer).
func isFinalAssistant(t *testing.T, ev store.Event) bool {
	t.Helper()
	var msg agentcore.AssistantMessage
	if err := json.Unmarshal(ev.Payload, &msg); err != nil {
		t.Fatalf("assistant payload: %v", err)
	}
	return len(msg.ToolCalls()) == 0
}

// assistantTexts lists the text of every assistant event, in order.
func assistantTexts(t *testing.T, events []store.Event) []string {
	t.Helper()
	var out []string
	for _, ev := range events {
		if ev.Kind != store.EventAssistantMessage {
			continue
		}
		var msg agentcore.AssistantMessage
		if err := json.Unmarshal(ev.Payload, &msg); err != nil {
			t.Fatalf("assistant payload: %v", err)
		}
		out = append(out, ContentText(msg.Content))
	}
	return out
}

// finalAssistantCount counts user-visible final answers.
func finalAssistantCount(t *testing.T, events []store.Event) int {
	t.Helper()
	n := 0
	for _, ev := range events {
		if ev.Kind == store.EventAssistantMessage && isFinalAssistant(t, ev) {
			n++
		}
	}
	return n
}

// TestFailedAttemptKeepsWatermarkAndInputIsAnswered is the N1 regression: an
// attempt that committed its answer and then FAILED must still hand its read
// watermark to the successor. The unread input it never saw sits above that
// watermark, so the successor answers it — before the fix the failed attempt
// wrote no watermark, the successor fell back to the log tail (an assistant
// message) and settled "nothing to answer" while the input stayed unanswered.
func TestFailedAttemptKeepsWatermarkAndInputIsAnswered(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	// Input one asks for a tool, so the loop needs a second model call — the one
	// that fails after the tool call and its result were committed.
	if _, err := k.Commit(ctx, kernel.CommitRequest{Scope: scope, Kind: kernel.KindUserMessage,
		RequestID: "r1", Payload: mustJSON(map[string]any{"text": "please echo this", "requestId": "r1"})}); err != nil {
		t.Fatal(err)
	}
	lease1 := claimScope(t, k, tenant, session)
	// The second input arrives mid-run; the attempt commits the tool call and
	// its result, then its final answer fails to commit.
	runner, inputErr := midRunInputThenFailAssistant(t, k, scope, "r2", "please answer me")
	cursor, watermark, runErr := runner.RunWithWatermark(ctx, lease1)
	if runErr == nil {
		t.Fatal("attempt 1 did not fail; the test needs the failure window")
	}
	if err := inputErr(); err != nil {
		t.Fatal(err)
	}
	if watermark != 1 {
		t.Fatalf("attempt 1 watermark = %d, want 1", watermark)
	}
	// The worker settles the failed attempt with its read-through watermark.
	if err := k.Finish(ctx, kernel.FinishRequest{
		Lease: lease1, Cursor: cursor, ReadThrough: watermark, Err: runErr, MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}
	pin, err := k.RunPin(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if pin.ReadWatermark != 1 {
		t.Fatalf("a failed attempt lost its read watermark: %d, want 1", pin.ReadWatermark)
	}
	if !pin.PendingInput {
		t.Fatal("a failed attempt did not leave the unread input pending")
	}

	// The successor must answer the in-flight input.
	lease2, err := k.ClaimScope(ctx, scope, "runtime-successor", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if lease2.ReadWatermark != 1 {
		t.Fatalf("successor watermark = %d, want 1", lease2.ReadWatermark)
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

	events := eventsOf(t, st, tenant, session)
	if got := countKind(events, store.EventUserMessage); got != 2 {
		t.Fatalf("user events = %d, want 2", got)
	}
	texts := assistantTexts(t, events)
	last := texts[len(texts)-1]
	if want := "fake provider received: please answer me"; last != want {
		t.Fatalf("final answer = %q, want %q (the unread input's text)", last, want)
	}
	pin, err = k.RunPin(ctx, scope)
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
}

// TestCrashBeforeFinishDoesNotDuplicateTheFinalAnswer is the N2 regression: the
// attempt committed its final answer but was killed before Finish (so the row
// still says pending_input). The successor must not produce a second final
// answer: its read watermark proves the answer already covers every input.
func TestCrashBeforeFinishDoesNotDuplicateTheFinalAnswer(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	if _, err := k.Commit(ctx, kernel.CommitRequest{Scope: scope, Kind: kernel.KindUserMessage,
		RequestID: "r1", Payload: mustJSON(map[string]any{"text": "hello", "requestId": "r1"})}); err != nil {
		t.Fatal(err)
	}
	lease1, err := k.ClaimScope(ctx, scope, "runtime-killed", 1500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	if _, _, err := runner.RunWithWatermark(ctx, lease1); err != nil {
		t.Fatalf("attempt 1: %v", err)
	}
	// The process dies HERE: the answer is committed, Finish never runs.
	events := eventsOf(t, st, tenant, session)
	if got := finalAssistantCount(t, events); got != 1 {
		t.Fatalf("attempt 1 finals = %d, want 1", got)
	}

	// The lease expires and a successor claims the session.
	waitForLeaseLapse(t, k, scope)
	lease2, err := k.ClaimScope(ctx, scope, "runtime-successor", time.Minute)
	if err != nil {
		t.Fatalf("successor claim: %v", err)
	}
	var calls atomic.Int64
	runner2 := NewRunner(k, k, countingStream(&calls), "fake", "fake", "", discardLog())
	cursor2, watermark2, err := runner2.RunWithWatermark(ctx, lease2)
	if err != nil {
		t.Fatalf("successor run: %v", err)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{
		Lease: lease2, Cursor: cursor2, ReadThrough: watermark2, MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("the successor called the model %d times; the answer was already committed", calls.Load())
	}
	events = eventsOf(t, st, tenant, session)
	if got := countKind(events, store.EventUserMessage); got != 1 {
		t.Fatalf("user events = %d, want 1", got)
	}
	if got := countKind(events, store.EventAssistantMessage); got != 1 {
		t.Fatalf("assistant events = %d, want exactly 1 final answer (no duplicate)", got)
	}
	if got := finalAssistantCount(t, events); got != 1 {
		t.Fatalf("final answers = %d, want 1", got)
	}
	pin, err := k.RunPin(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Status != kernel.RunStatusDone {
		t.Fatalf("run status = %s, want done", pin.Status)
	}
}

// TestKillBetweenLoadAndFinishNeitherDuplicatesNorMisses: attempt 1 loads the
// log, an input arrives while it runs, its answer is committed, and the process
// is killed before Finish. The successor must answer the in-flight input
// exactly once and must not re-answer the input attempt 1 already answered.
func TestKillBetweenLoadAndFinishNeitherDuplicatesNorMisses(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	if _, err := k.Commit(ctx, kernel.CommitRequest{Scope: scope, Kind: kernel.KindUserMessage,
		RequestID: "r1", Payload: mustJSON(map[string]any{"text": "hello", "requestId": "r1"})}); err != nil {
		t.Fatal(err)
	}
	lease1, err := k.ClaimScope(ctx, scope, "runtime-killed", 1500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	stream, inputErr := midRunInput(t, k, scope, "r2", "second input")
	runner := NewRunner(k, k, stream, "fake", "fake", "", discardLog())
	if _, _, err := runner.RunWithWatermark(ctx, lease1); err != nil {
		t.Fatalf("attempt 1: %v", err)
	}
	if err := inputErr(); err != nil {
		t.Fatal(err)
	}
	// Killed before Finish: the durable read point (recorded after the load) is
	// the only fact about what attempt 1 read.
	waitForLeaseLapse(t, k, scope)
	lease2, err := k.ClaimScope(ctx, scope, "runtime-successor", time.Minute)
	if err != nil {
		t.Fatalf("successor claim: %v", err)
	}
	if lease2.ReadWatermark != 1 {
		t.Fatalf("successor watermark = %d, want 1 (attempt 1's load point)", lease2.ReadWatermark)
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

	events := eventsOf(t, st, tenant, session)
	var users, finals int
	var lastFinal string
	for _, ev := range events {
		switch ev.Kind {
		case store.EventUserMessage:
			users++
		case store.EventAssistantMessage:
			if isFinalAssistant(t, ev) {
				finals++
				var msg agentcore.AssistantMessage
				if err := json.Unmarshal(ev.Payload, &msg); err != nil {
					t.Fatal(err)
				}
				lastFinal = ContentText(msg.Content)
			}
		}
	}
	if users != 2 {
		t.Fatalf("user events = %d, want 2", users)
	}
	if finals != 2 {
		t.Fatalf("final answers = %d, want 2 (one per input, no duplicate)", finals)
	}
	if want := "fake provider received: second input"; lastFinal != want {
		t.Fatalf("last final = %q, want %q", lastFinal, want)
	}
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
}

// waitForLeaseLapse waits until the session's lease is no longer live.
func waitForLeaseLapse(t *testing.T, k *kernel.Kernel, scope kernel.Scope) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		state, err := k.LeaseState(ctx, scope)
		if err != nil {
			t.Fatalf("lease state: %v", err)
		}
		if !state.Live {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("lease never lapsed")
}
