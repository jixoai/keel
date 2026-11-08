package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/store"
)

// The crash matrix of the input-consumption fact (kernel-loop K2.3). A "kill"
// is modelled as an attempt that ends without a settle: the runner returns, the
// worker releases the claim instead of finishing it, and the next claim is the
// successor. The log — not the process — is the only state that survives.

// TestCrashBeforeAssistantCommitReanswers pins "assistant 未提交 → 必重答": the
// attempt dies while the model stream is in flight, nothing is committed, and
// the successor answers the input exactly once.
func TestCrashBeforeAssistantCommitReanswers(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "hello", "", nil)
	lease := claimScope(t, k, tenant, session)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	entered := make(chan struct{})
	blocking := func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	runner := NewRunner(k, k, blocking, "fake", "fake", "", discardLog())
	go func() {
		<-entered
		cancel()
	}()
	if _, err := runner.RunAttempt(runCtx, lease); err == nil {
		t.Fatal("the cancelled attempt returned no error")
	}
	// The process died: hand the claim back without settling.
	if err := k.Release(ctx, lease); err != nil {
		t.Fatalf("release: %v", err)
	}

	// The successor sees the input as still owed and answers it.
	lease2 := claimScope(t, k, tenant, session)
	runner2 := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	res2, err := runner2.RunAttempt(ctx, lease2)
	if err != nil {
		t.Fatalf("successor: %v", err)
	}
	if res2.Summary.Status != kernel.RunSummaryDone {
		t.Fatalf("successor status = %q", res2.Summary.Status)
	}
	evs := eventsOf(t, st, tenant, session)
	if got := countKind(evs, store.EventAssistantMessage); got != 1 {
		t.Fatalf("assistant messages = %d, want exactly the successor's one", got)
	}
	texts := assistantTexts(t, evs)
	if !strings.Contains(texts[0], "hello") {
		t.Fatalf("answer = %q, want the input answered", texts[0])
	}
}

// TestCrashAfterAssistantCommitDoesNotReanswer pins "assistant 已提交 → 不重答":
// the answer landed but the attempt died before Finish; the successor owes
// nothing and must not produce a second answer.
func TestCrashAfterAssistantCommitDoesNotReanswer(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "hello", "", nil)
	lease := claimScope(t, k, tenant, session)
	runner := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	res, err := runner.RunAttempt(ctx, lease)
	if err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	if res.Summary.Status != kernel.RunSummaryDone {
		t.Fatalf("first attempt status = %q", res.Summary.Status)
	}
	before := eventsOf(t, st, tenant, session)
	if got := countKind(before, store.EventAssistantMessage); got != 1 {
		t.Fatalf("assistant messages = %d, want 1", got)
	}
	// Killed between the answer and the settle.
	if err := k.Release(ctx, lease); err != nil {
		t.Fatalf("release: %v", err)
	}

	lease2 := claimScope(t, k, tenant, session)
	runner2 := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	res2, err := runner2.RunAttempt(ctx, lease2)
	if err != nil {
		t.Fatalf("successor: %v", err)
	}
	after := eventsOf(t, st, tenant, session)
	if got := countKind(after, store.EventAssistantMessage); got != 1 {
		t.Fatalf("assistant messages after the successor = %d, want 1 (no duplicate answer)", got)
	}
	if res2.Cursor != res2.ReadThrough {
		t.Fatalf("successor cursor = %d readThrough = %d, want nothing new", res2.Cursor, res2.ReadThrough)
	}
	if res2.Summary.Turns != 0 {
		t.Fatalf("successor turns = %d, want 0", res2.Summary.Turns)
	}
}

// TestLegacyPrefixIsNotReansweredAfterUpgrade pins the migration rule: a log
// that straddles the inputSeqs upgrade (an older answer without the fact, newer
// answers with it) must not re-answer the pre-upgrade input. The watermark
// decided it once; inputSeqs only governs inputs from the upgrade boundary on.
func TestLegacyPrefixIsNotReansweredAfterUpgrade(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "one", "", nil)
	// The pre-upgrade answer: no inputSeqs (written by the old runtime).
	lease := claimScope(t, k, tenant, session)
	legacy := mustJSON(map[string]any{
		"role":       "assistant",
		"content":    []map[string]any{{"type": "text", "text": "legacy answer one"}},
		"stopReason": "end_turn",
	})
	if _, err := k.Commit(ctx, kernel.CommitRequest{Scope: scope, Kind: kernel.KindAssistantMessage,
		Token: lease.Token, GenerationID: "u1-a0", Payload: legacy}); err != nil {
		t.Fatalf("commit legacy answer: %v", err)
	}
	if err := k.Release(ctx, lease); err != nil {
		t.Fatalf("release: %v", err)
	}

	// Post-upgrade input: the answer carries the consumption fact.
	commitInput(t, k, scope, "r2", "two", "", nil)
	lease2 := claimScope(t, k, tenant, session)
	runner := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	res, err := runner.RunAttempt(ctx, lease2)
	if err != nil {
		t.Fatalf("post-upgrade run: %v", err)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease2, Cursor: res.Cursor,
		ReadThrough: res.ReadThrough, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	// A later run must not re-answer "one": its pre-upgrade answer stands.
	commitInput(t, k, scope, "r3", "three", "", nil)
	lease3 := claimScope(t, k, tenant, session)
	runner2 := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	res3, err := runner2.RunAttempt(ctx, lease3)
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease3, Cursor: res3.Cursor,
		ReadThrough: res3.ReadThrough, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish third run: %v", err)
	}
	texts := assistantTexts(t, eventsOf(t, st, tenant, session))
	var ones, twos, threes int
	for _, text := range texts {
		if strings.Contains(text, "one") {
			ones++
		}
		if strings.Contains(text, "two") {
			twos++
		}
		if strings.Contains(text, "three") {
			threes++
		}
	}
	if ones != 1 {
		t.Fatalf("answers to the pre-upgrade input = %d, want exactly the legacy one: %q", ones, texts)
	}
	if twos != 1 || threes != 1 {
		t.Fatalf("answers two=%d three=%d, want one each: %q", twos, threes, texts)
	}
}

// TestCrashDuringToolTurnFillsResults pins the tool-turn half: the assistant
// tool call is committed, the result is not, and the successor fills exactly
// one result before continuing. A guide input that arrived in the crashed
// window is part of the successor's transcript (queue semantics) and is
// answered by it.
func TestCrashDuringToolTurnFillsResults(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "echo hi", "", nil)
	lease := claimScope(t, k, tenant, session)
	// The crashed attempt committed its tool call (with the input it consumed)
	// and died before the result. The guide input landed in the same window.
	toolCall := mustJSON(map[string]any{
		"role": "assistant",
		"content": []map[string]any{{
			"type": "toolCall", "id": "call-crash", "name": "echo",
			"arguments": map[string]string{"x": "hi"},
		}},
		"stopReason": "tool_use",
		"inputSeqs":  []int64{1},
	})
	if _, err := k.Commit(ctx, kernel.CommitRequest{Scope: scope, Kind: kernel.KindAssistantMessage,
		Token: lease.Token, GenerationID: "u1-a0", Payload: toolCall}); err != nil {
		t.Fatalf("commit tool call: %v", err)
	}
	commitInput(t, k, scope, "r2", "two", kernel.DeliveryGuide, nil)
	if err := k.Release(ctx, lease); err != nil {
		t.Fatalf("release: %v", err)
	}

	lease2 := claimScope(t, k, tenant, session)
	runner := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	res, err := runner.RunAttempt(ctx, lease2)
	if err != nil {
		t.Fatalf("successor: %v", err)
	}
	evs := eventsOf(t, st, tenant, session)
	if got := countKind(evs, store.EventToolResult); got != 1 {
		t.Fatalf("tool results = %d, want exactly 1 (the call is not replayed)", got)
	}
	texts := assistantTexts(t, evs)
	if last := texts[len(texts)-1]; !strings.Contains(last, "two") {
		t.Fatalf("final answer = %q, want the guide input answered", last)
	}
	if res.Summary.Status != kernel.RunSummaryDone {
		t.Fatalf("successor status = %q", res.Summary.Status)
	}
}
