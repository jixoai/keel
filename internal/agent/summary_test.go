package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/store"
)

// usageStream answers every request with one final message carrying provider
// usage, so the attempt summary's token accounting is observable.
func usageStream() provider.StreamFn {
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		msg := agentcore.AssistantMessage{
			RoleField:  agentcore.RoleAssistant,
			Content:    agentcore.ContentList{agentcore.NewTextContent("ok")},
			StopReason: agentcore.StopReasonEndTurn,
			Usage:      &agentcore.Usage{InputTokens: 11, OutputTokens: 4},
		}
		s := provider.NewAssistantMessageEventStream(4)
		go func() {
			defer s.Close()
			_ = s.Emit(ctx, provider.StreamStartEvent{Partial: msg})
			_ = s.Emit(ctx, provider.StreamDoneEvent{Message: msg})
		}()
		return s, nil
	}
}

// TestRunAttemptSummary pins the attempt-level accounting: turns, provider
// tokens, the final context size and the stop reason.
func TestRunAttemptSummary(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "hello", "", nil)
	lease := claimScope(t, k, tenant, session)
	runner := NewRunner(k, k, usageStream(), "fake", "fake", "", discardLog())
	res, err := runner.RunAttempt(ctx, lease)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Summary.Status != kernel.RunSummaryDone {
		t.Fatalf("status = %q, want done", res.Summary.Status)
	}
	if res.Summary.Turns != 1 {
		t.Fatalf("turns = %d, want 1", res.Summary.Turns)
	}
	if res.Summary.Tokens.Input != 11 || res.Summary.Tokens.Output != 4 || res.Summary.Tokens.Total != 15 {
		t.Fatalf("tokens = %+v, want 11/4/15", res.Summary.Tokens)
	}
	// The loop declares the tool loadout as a system message before the prompt,
	// so the final context is at least the user message plus the answer.
	if res.Summary.Context.Messages < 2 || res.Summary.Context.Tokens != 11 {
		t.Fatalf("context = %+v, want >=2 messages / 11 tokens", res.Summary.Context)
	}
	if res.Summary.StopReason != agentcore.StopReasonEndTurn {
		t.Fatalf("stopReason = %q, want end_turn", res.Summary.StopReason)
	}
	if res.Summary.DurationMs < 0 {
		t.Fatalf("durationMs = %d", res.Summary.DurationMs)
	}
}

// failingStream is a provider that cannot answer (the 401 path).
func failingStream() provider.StreamFn {
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		return nil, errors.New("provider 401")
	}
}

// TestRunAttemptSummaryOnFailure pins the failure side: pi delivers a provider
// failure as a terminal assistant message (stopReason=error), not as a Go
// error, so the attempt reports it in the summary rather than as an
// infrastructure error.
func TestRunAttemptSummaryOnFailure(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "hello", "", nil)
	lease := claimScope(t, k, tenant, session)
	runner := NewRunner(k, k, failingStream(), "fake", "fake", "", discardLog())
	res, err := runner.RunAttempt(ctx, lease)
	if err != nil {
		t.Fatalf("RunAttempt = %v; a provider failure is a terminal message, not an infrastructure error", err)
	}
	if res.Summary.Status != kernel.RunSummaryFailed {
		t.Fatalf("status = %q, want failed", res.Summary.Status)
	}
	if res.Summary.StopReason != agentcore.StopReasonError {
		t.Fatalf("stopReason = %q, want error", res.Summary.StopReason)
	}
	if !strings.Contains(res.Summary.ErrorMessage, "provider 401") {
		t.Fatalf("error = %q, want the provider failure", res.Summary.ErrorMessage)
	}
}

// TestWorkerSettlesModelFailure pins the settle side of a provider failure: the
// run row carries the error, the attempt is retried (the failed turn consumed
// nothing), and the retry actually re-asks the model instead of concluding
// "nothing owed".
func TestWorkerSettlesModelFailure(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "hello", "", nil)
	lease := claimScope(t, k, tenant, session)
	worker := &Worker{
		Leases: k, Runs: st, Runner: NewRunner(k, k, failingStream(), "fake", "fake", "", discardLog()),
		Owner: "failure-test", Lease: time.Minute, MaxAttempts: 3, Logger: discardLog(), Committer: k,
	}
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	worker.execute(ctx, sem, lease)

	var summary map[string]any
	for _, ev := range eventsOf(t, st, tenant, session) {
		if ev.Kind == store.EventRunFinished {
			if err := json.Unmarshal(ev.Payload, &summary); err != nil {
				t.Fatalf("decode run.finished: %v", err)
			}
		}
	}
	if summary == nil {
		t.Fatal("no run.finished event for the failed attempt")
	}
	if summary["status"] != kernel.RunSummaryFailed || summary["stopReason"] != agentcore.StopReasonError {
		t.Fatalf("summary = %v, want failed/error", summary)
	}
	pin, err := k.RunPin(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Status != kernel.RunStatusPending {
		t.Fatalf("run status = %s, want pending (retryable attempt)", pin.Status)
	}
	row, err := st.GetRun(ctx, tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(row.LastError, "provider 401") {
		t.Fatalf("last error = %q, want the provider failure", row.LastError)
	}

	// The failed turn consumed nothing: the retry asks the model again.
	lease2 := claimScope(t, k, tenant, session)
	runner2 := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	res2, err := runner2.RunAttempt(ctx, lease2)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res2.Summary.Status != kernel.RunSummaryDone {
		t.Fatalf("retry status = %q, want done", res2.Summary.Status)
	}
	texts := assistantTexts(t, eventsOf(t, st, tenant, session))
	if last := texts[len(texts)-1]; !strings.Contains(last, "hello") {
		t.Fatalf("retry answer = %q, want the input answered", last)
	}
}

// TestWorkerCommitsRunFinishedSummary pins the durable side: the worker commits
// run.finished under the run's lease before the settle, and the payload is the
// attempt summary the runner produced.
func TestWorkerCommitsRunFinishedSummary(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "hello", "", nil)
	lease := claimScope(t, k, tenant, session)
	runner := NewRunner(k, k, usageStream(), "fake", "fake", "", discardLog())
	worker := &Worker{
		Leases: k, Runs: st, Runner: runner, Owner: "summary-test",
		Lease: time.Minute, MaxAttempts: 3, Logger: discardLog(), Committer: k,
	}
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	worker.execute(ctx, sem, lease)

	evs := eventsOf(t, st, tenant, session)
	var summary map[string]any
	for _, ev := range evs {
		if ev.Kind != store.EventRunFinished {
			continue
		}
		if err := json.Unmarshal(ev.Payload, &summary); err != nil {
			t.Fatalf("decode run.finished: %v", err)
		}
	}
	if summary == nil {
		t.Fatalf("no run.finished event in %d events", len(evs))
	}
	if summary["status"] != kernel.RunSummaryDone {
		t.Fatalf("summary status = %v, want done", summary["status"])
	}
	if summary["stopReason"] != agentcore.StopReasonEndTurn {
		t.Fatalf("summary stopReason = %v", summary["stopReason"])
	}
	if summary["turns"].(float64) != 1 {
		t.Fatalf("summary turns = %v, want 1", summary["turns"])
	}
	// The settle happened after the summary: the run row is terminal.
	pin, err := k.RunPin(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Status != kernel.RunStatusDone {
		t.Fatalf("run status = %s, want done", pin.Status)
	}
}

// controlStub is the product cancel seam: it cancels the attempt context on
// demand and records the outcome it was told.
type controlStub struct {
	mu       sync.Mutex
	cancel   context.CancelFunc
	outcomes []string
}

func (c *controlStub) BeginRun(parent context.Context, scope kernel.Scope, runID string) (context.Context, func(outcome string)) {
	ctx, cancel := context.WithCancel(parent)
	c.mu.Lock()
	c.cancel = cancel
	c.mu.Unlock()
	return ctx, func(outcome string) {
		c.mu.Lock()
		c.outcomes = append(c.outcomes, outcome)
		c.mu.Unlock()
	}
}

func (c *controlStub) stop() {
	c.mu.Lock()
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *controlStub) reported() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.outcomes...)
}

// TestWorkerCancelledAttemptIsTerminal pins the cancel semantics: the attempt
// stops, the summary says cancelled, the run settles terminally (no automatic
// re-run of the input it did not answer), and the cancel seam learns the
// outcome.
func TestWorkerCancelledAttemptIsTerminal(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "hello", "", nil)
	lease := claimScope(t, k, tenant, session)
	control := &controlStub{}
	fake := FakeStream()
	var once sync.Once
	stream := func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		once.Do(control.stop)
		return fake(ctx, model, llm, cfg)
	}
	runner := NewRunner(k, k, stream, "fake", "fake", "", discardLog())
	worker := &Worker{
		Leases: k, Runs: st, Runner: runner, Owner: "cancel-test",
		Lease: time.Minute, MaxAttempts: 3, Logger: discardLog(), Committer: k,
		Control: control,
	}
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	worker.execute(ctx, sem, lease)

	var summary map[string]any
	for _, ev := range eventsOf(t, st, tenant, session) {
		if ev.Kind == store.EventRunFinished {
			if err := json.Unmarshal(ev.Payload, &summary); err != nil {
				t.Fatalf("decode run.finished: %v", err)
			}
		}
	}
	if summary == nil {
		t.Fatal("no run.finished event for the cancelled attempt")
	}
	if summary["status"] != kernel.RunSummaryCancelled {
		t.Fatalf("summary status = %v, want cancelled", summary["status"])
	}
	if summary["stopReason"] != agentcore.StopReasonAborted {
		t.Fatalf("summary stopReason = %v, want aborted", summary["stopReason"])
	}
	if outcomes := control.reported(); len(outcomes) != 1 || outcomes[0] != kernel.RunSummaryCancelled {
		t.Fatalf("cancel seam outcomes = %v, want [cancelled]", outcomes)
	}
	// Terminal: the cancelled attempt is not retried on its own.
	if _, err := k.ClaimScope(ctx, scope, "successor", time.Minute); !errors.Is(err, kernel.ErrNoRunnable) {
		t.Fatalf("claim after cancel = %v, want no-runnable (terminal settle)", err)
	}
}

// TestWorkerSummaryFailureDoesNotChangeTheRun pins the isolation rule: a broken
// summary commit (here: the committer is missing) never changes the run's
// terminal state.
func TestWorkerSummaryFailureDoesNotChangeTheRun(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "hello", "", nil)
	lease := claimScope(t, k, tenant, session)
	runner := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	worker := &Worker{
		Leases: k, Runs: st, Runner: runner, Owner: "no-summary",
		Lease: time.Minute, MaxAttempts: 3, Logger: discardLog(),
	}
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	worker.execute(ctx, sem, lease)

	for _, ev := range eventsOf(t, st, tenant, session) {
		if ev.Kind == store.EventRunFinished {
			t.Fatal("worker without a committer wrote a summary")
		}
	}
	pin, err := k.RunPin(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Status != kernel.RunStatusDone {
		t.Fatalf("run status = %s, want done", pin.Status)
	}
	if !strings.Contains(pin.Status, "done") {
		t.Fatalf("status = %q", pin.Status)
	}
}
