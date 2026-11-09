package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/store"
)

// TestResolveFailureOnFirstInputIsRetriedNotStranded is the regression test for
// a silent-loss bug an adversarial review proved: the attempt that fails before
// committing anything (an unrunnable selection, a missing credential) settles
// with a run.finished event, and the successor's legacy "is anything owed?"
// heuristic read that summary as the log tail and concluded the input had been
// answered. The input stayed unanswered while the run said done.
func TestResolveFailureOnFirstInputIsRetriedNotStranded(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "hello", "", map[string]any{
		"selection": map[string]any{"providerId": "p", "modelId": "gone"},
	})
	resolver := &stubResolver{err: errors.New(`modelcfg: unknown model "gone"`)}
	runner := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	runner.Models = resolver

	lease := claimScope(t, k, tenant, session)
	first, err := runner.RunAttempt(ctx, lease)
	if err == nil {
		t.Fatal("the first attempt must fail at resolution")
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease, Cursor: first.Cursor,
		ReadThrough: first.ReadThrough, Err: err, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if pin, err := k.RunPin(ctx, scope); err != nil || pin.Status != kernel.RunStatusPending {
		t.Fatalf("run status after the failed attempt = %v (%v), want pending", pin.Status, err)
	}

	// The successor must reach the resolver again instead of settling done on a
	// log whose tail is another attempt's summary.
	lease2 := claimScope(t, k, tenant, session)
	second, err2 := runner.RunAttempt(ctx, lease2)
	if err2 == nil {
		t.Fatalf("the retry settled without resolving (status %q): the input would be stranded", second.Summary.Status)
	}
	if got := len(resolver.selections()); got != 2 {
		t.Fatalf("resolver calls = %d, want 2 (the input must be retried)", got)
	}
	k.Finish(context.Background(), kernel.FinishRequest{Lease: lease2, Cursor: second.Cursor,
		ReadThrough: second.ReadThrough, Err: err2, MaxAttempts: 3}) //nolint:errcheck // test cleanup

	// No answer was invented anywhere along the way.
	for _, assistant := range assistantEvents(eventsOf(t, st, tenant, session)) {
		t.Fatalf("a failed resolution must not produce an answer: %+v", assistant)
	}
	// The caller-side Finish (not the worker) committed nothing: the log holds the
	// unanswered input and no invented answer.
	if n := len(eventsOf(t, st, tenant, session)); n != 1 {
		t.Fatalf("events = %d, want only the unanswered input", n)
	}
}

// TestCancelAfterCommittedAnswerIsNotAborted pins the ordering rule of the
// cancellation guard: a run-level stop reason the loop sets after the answer was
// committed (the turn cap) must not turn a committed answer into "aborted". The
// guard reads the answer fact, not the run label.
func TestCancelAfterCommittedAnswerIsNotAborted(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "hello", "", nil)
	lease := claimScope(t, k, tenant, session)
	control := &controlStub{}
	fake := FakeStream()
	// The caller stops the attempt only after the answer is committed — the real
	// "stop pressed as the answer lands" race, and the only ordering in which the
	// guard's exception matters.
	var stopOnce sync.Once
	stream := func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		go func() {
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				for _, ev := range eventsOf(t, st, tenant, session) {
					if ev.Kind == store.EventAssistantMessage {
						stopOnce.Do(control.stop)
						return
					}
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
		return fake(ctx, model, llm, cfg)
	}
	runner := NewRunner(k, k, stream, "fake", "fake", "", discardLog())
	runner.MaxTurns = 1
	worker := &Worker{
		Leases: k, Runs: st, Runner: runner, Owner: "cancel-cap-test",
		Lease: time.Minute, MaxAttempts: 3, Logger: discardLog(), Committer: k,
		Control: control,
	}
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	worker.execute(ctx, sem, lease)

	summary := lastRunFinished(t, st, tenant, session)
	if summary["stopReason"] != StopReasonMaxTurns {
		t.Fatalf("stopReason = %v, want the turn cap to label the run", summary["stopReason"])
	}
	if summary["status"] == kernel.RunSummaryCancelled {
		t.Fatalf("a committed answer must not be reported cancelled: %v", summary)
	}
	if texts := assistantTexts(t, eventsOf(t, st, tenant, session)); len(texts) == 0 {
		t.Fatal("the answer should be committed")
	}
}

// TestResolverWithoutTransportIsRefused pins the fail-closed contract of the
// resolver seam: an identity without a transport would run the boot stream while
// the ledger records the resolved model.
func TestResolverWithoutTransportIsRefused(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "hello", "", nil)
	runner := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	runner.Models = &stubResolver{resolved: ActiveModel{ProviderID: "p", Model: "m", APIKey: "k"}}
	lease := claimScope(t, k, tenant, session)
	res, err := runner.RunAttempt(ctx, lease)
	k.Finish(context.Background(), kernel.FinishRequest{Lease: lease, Cursor: res.Cursor,
		ReadThrough: res.ReadThrough, Err: err, MaxAttempts: 3}) //nolint:errcheck // test cleanup
	if err == nil || !strings.Contains(err.Error(), "no transport") {
		t.Fatalf("err = %v, want a refusal naming the missing transport", err)
	}
	if res.Summary.Status != kernel.RunSummaryFailed {
		t.Fatalf("status = %q", res.Summary.Status)
	}
}

// TestModelWithoutToolSupportRefusesTools pins the tool half of the capability
// gate: a model whose configuration denies tool calls must never be handed any.
func TestModelWithoutToolSupportRefusesTools(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "hello", "", nil)
	rec := &recordingStream{}
	runner := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	runner.Models = &stubResolver{resolved: ActiveModel{
		ProviderID: "p", Model: "no-tools", APIKey: "k", Stream: rec.fn(),
		ThinkingLevel: agentcore.ThinkingLow,
		Properties:    &ModelProperties{SupportsToolCall: false},
	}}
	lease := claimScope(t, k, tenant, session)
	res, err := runner.RunAttempt(ctx, lease)
	k.Finish(context.Background(), kernel.FinishRequest{Lease: lease, Cursor: res.Cursor,
		ReadThrough: res.ReadThrough, Err: err, MaxAttempts: 3}) //nolint:errcheck // test cleanup
	if err == nil || !strings.Contains(err.Error(), "no tool-call support") {
		t.Fatalf("err = %v, want the tool gate to refuse", err)
	}
	if models, _ := rec.snapshot(); len(models) != 0 {
		t.Fatalf("the provider must not be called: %v", models)
	}

	// The same model declaring tool support runs normally.
	runner2 := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	rec2 := &recordingStream{}
	runner2.Models = &stubResolver{resolved: ActiveModel{
		ProviderID: "p", Model: "with-tools", APIKey: "k", Stream: rec2.fn(),
		ThinkingLevel: agentcore.ThinkingLow,
		Properties:    &ModelProperties{SupportsToolCall: true},
	}}
	requeue(t, st, tenant, session)
	lease2 := claimScope(t, k, tenant, session)
	res2, err2 := runner2.RunAttempt(ctx, lease2)
	if err2 != nil {
		t.Fatalf("run with tool support: %v", err2)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease2, Cursor: res2.Cursor,
		ReadThrough: res2.ReadThrough, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if models, _ := rec2.snapshot(); len(models) != 1 {
		t.Fatalf("want exactly one provider call, got %v", models)
	}
}

// TestSelectionGuideStopsTheScan pins the ordering rule of the guide downgrade:
// a selection-bearing guide waits for the next attempt, and the scan stops there
// so a later plain guide cannot be injected ahead of it.
func TestSelectionGuideStopsTheScan(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "echo one", "", nil)
	lease := claimScope(t, k, tenant, session)

	fake := FakeStream()
	var (
		once sync.Once
		cerr error
	)
	stream := func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		once.Do(func() {
			for _, input := range []map[string]any{
				{"text": "switch model", "requestId": "r2", "delivery": kernel.DeliveryGuide,
					"selection": map[string]any{"providerId": "p", "modelId": "other"}},
				{"text": "plain follow-up", "requestId": "r3", "delivery": kernel.DeliveryGuide},
			} {
				if _, err := k.Commit(ctx, kernel.CommitRequest{
					Scope: scope, Kind: kernel.KindUserMessage,
					RequestID: input["requestId"].(string), Payload: mustJSON(input),
				}); err != nil {
					cerr = err
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
	if cerr != nil {
		t.Fatalf("commit guides: %v", cerr)
	}
	for _, assistant := range assistantEvents(eventsOf(t, st, tenant, session)) {
		for _, seq := range inputSeqsOf(t, assistant) {
			if seq == 2 || seq == 3 {
				t.Fatalf("assistant inputSeqs consumed a deferred guide (seq %d): %+v", seq, assistant)
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
		t.Fatalf("run status = %s, want pending (both guides await the next attempt)", pin.Status)
	}
}

// lastRunFinished decodes the newest run.finished payload of a session.
func lastRunFinished(t *testing.T, st *store.Store, tenant, session uuid.UUID) map[string]any {
	t.Helper()
	var summary map[string]any
	for _, ev := range eventsOf(t, st, tenant, session) {
		if ev.Kind == store.EventRunFinished {
			if err := json.Unmarshal(ev.Payload, &summary); err != nil {
				t.Fatalf("decode run.finished: %v", err)
			}
		}
	}
	if summary == nil {
		t.Fatal("no run.finished event")
	}
	return summary
}
