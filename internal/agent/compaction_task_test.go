package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/store"
)

// toggleCheckpoints makes the projection save fail on demand while Load
// delegates to the real kernel. It models the crash window this feature
// closes: the compaction event committed, its projection did not.
type toggleCheckpoints struct {
	inner kernel.CheckpointStore
	mu    sync.Mutex
	fail  bool
	// requestPrunes counts the independent request-ledger prunes.
	requestPrunes int
}

func (t *toggleCheckpoints) SaveCheckpoint(ctx context.Context, scope kernel.Scope, rec kernel.CheckpointRecord) error {
	t.mu.Lock()
	fail := t.fail
	t.mu.Unlock()
	if fail {
		return errors.New("checkpoint store unavailable")
	}
	return t.inner.SaveCheckpoint(ctx, scope, rec)
}

func (t *toggleCheckpoints) LoadCheckpoint(ctx context.Context, scope kernel.Scope) (kernel.Checkpoint, bool, error) {
	return t.inner.LoadCheckpoint(ctx, scope)
}

func (t *toggleCheckpoints) setFail(fail bool) {
	t.mu.Lock()
	t.fail = fail
	t.mu.Unlock()
}

// PruneCheckpointRequests lets the runner's maintenance call reach the real
// store and records that the manual path invoked it.
func (t *toggleCheckpoints) PruneCheckpointRequests(ctx context.Context, scope kernel.Scope, req kernel.CheckpointRequestPruneRequest) (int64, error) {
	t.mu.Lock()
	t.requestPrunes++
	t.mu.Unlock()
	if pruner, ok := t.inner.(kernel.CheckpointRequestPruner); ok {
		return pruner.PruneCheckpointRequests(ctx, scope, req)
	}
	return 0, nil
}

func (t *toggleCheckpoints) requestPruneCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.requestPrunes
}

// countedSummaryStream fails the first N summary calls (the deterministic
// stand-in for a process killed mid-summary) and succeeds afterwards.
type countedSummaryStream struct {
	mu        sync.Mutex
	calls     int
	failFirst int
}

func (s *countedSummaryStream) callsMade() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *countedSummaryStream) stream(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if call <= s.failFirst {
		return failingSummaryStream()(ctx, model, llm, cfg)
	}
	return shortSummaryStream()(ctx, model, llm, cfg)
}

// noManualCompactRetryDelay removes the retry wait for tests that resume
// immediately after an injected summary failure.
func noManualCompactRetryDelay(t *testing.T) {
	t.Helper()
	previous := manualCompactRetryDelay
	manualCompactRetryDelay = -time.Second
	t.Cleanup(func() { manualCompactRetryDelay = previous })
}

// TestManualCompactRecoversCommittedEntry pins the entry-checkpoint recovery
// rule: when the compaction event is committed but its projection is lost (the
// process died between Commit and SaveCheckpoint), a retry with the same
// requestId restores the projection from the event and does NOT call the model
// again or append a second compaction.
func TestManualCompactRecoversCommittedEntry(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session, k, scope := seedManualHistory(t, st)
	runner := manualRunner(t, st)
	summary := &countedSummaryStream{}
	runner.Stream = summary.stream
	checkpoints := &toggleCheckpoints{inner: k, fail: true}
	runner.Checkpoints = checkpoints

	before := countKind(eventsOf(t, st, tenant, session), store.EventCompaction)
	lease := claimScope(t, k, tenant, session)
	if _, err := runner.CompactNow(ctx, lease, "manual-recover"); err == nil {
		t.Fatal("CompactNow succeeded despite the lost projection")
	}
	if got := countKind(eventsOf(t, st, tenant, session), store.EventCompaction); got != before+1 {
		t.Fatalf("compaction events = %d, want %d (the event survived)", got, before+1)
	}
	if _, found, err := k.CheckpointByRequest(ctx, scope, "manual-recover"); err != nil || found {
		t.Fatalf("lost projection = (found=%v, err=%v), want absent", found, err)
	}
	task, found, err := k.LoadCompactionTask(ctx, scope, "manual-recover")
	if err != nil || !found {
		t.Fatalf("in-flight task = (found=%v, err=%v), want present", found, err)
	}
	if task.Phase != kernel.CompactionPhaseSummarize || task.FirstKeptSeq <= 0 {
		t.Fatalf("task = %+v, want a pinned summarize checkpoint", task)
	}

	// The crashed projection is recoverable: no second summary request.
	checkpoints.setFail(false)
	res, err := runner.CompactNow(ctx, lease, "manual-recover")
	if err != nil {
		t.Fatalf("recovery CompactNow: %v", err)
	}
	if !res.Recovered || res.Performed || res.Replayed {
		t.Fatalf("result = %+v, want recovered (not performed, not replayed)", res)
	}
	if got := summary.callsMade(); got != 1 {
		t.Fatalf("summary calls = %d, want 1 (recovery must not call the model)", got)
	}
	if got := countKind(eventsOf(t, st, tenant, session), store.EventCompaction); got != before+1 {
		t.Fatalf("compaction events after recovery = %d, want %d (no duplicate)", got, before+1)
	}
	cp, found, err := k.CheckpointByRequest(ctx, scope, "manual-recover")
	if err != nil || !found {
		t.Fatalf("recovered projection = (found=%v, err=%v)", found, err)
	}
	if cp.UptoSeq != res.CheckpointID || cp.KeptFromSeq != res.KeptFromSeq ||
		cp.RequestID != "manual-recover" || cp.Summary == "" {
		t.Fatalf("projection = %+v, want the recovered event facts", cp)
	}
	if _, found, err := k.LoadCompactionTask(ctx, scope, "manual-recover"); err != nil || found {
		t.Fatalf("task after recovery = (found=%v, err=%v), want deleted", found, err)
	}
}

// TestManualCompactResumesPinnedSummarizePhase pins the other half: a summary
// call that fails leaves a pinned summarize checkpoint, and the retry resumes
// that exact retained boundary instead of selecting a new cut after new input
// arrived.
func TestManualCompactResumesPinnedSummarizePhase(t *testing.T) {
	noManualCompactRetryDelay(t)
	st := testStore(t)
	ctx := context.Background()
	tenant, session, k, scope := seedManualHistory(t, st)
	runner := manualRunner(t, st)
	summary := &countedSummaryStream{failFirst: 1}
	runner.Stream = summary.stream
	lease := claimScope(t, k, tenant, session)

	if _, err := runner.CompactNow(ctx, lease, "manual-resume"); err == nil {
		t.Fatal("first CompactNow succeeded, want the injected summary failure")
	}
	task, found, err := k.LoadCompactionTask(ctx, scope, "manual-resume")
	if err != nil || !found {
		t.Fatalf("task after failure = (found=%v, err=%v), want the pinned checkpoint", found, err)
	}
	if task.Phase != kernel.CompactionPhaseRetry || task.FirstKeptSeq <= 0 || task.TailSeq < task.FirstKeptSeq {
		t.Fatalf("pinned task = %+v, want a retry-phase pin", task)
	}
	if task.RetryUntil.IsZero() {
		t.Fatalf("task retry_until is zero, want a durable retry deadline")
	}
	if task.Attempt != 1 {
		t.Fatalf("task attempt = %d, want 1 after the first summary failure", task.Attempt)
	}

	// New input arrives while the task is interrupted. A fresh selection could
	// choose a different boundary; the retry must keep the pinned one.
	commitInput(t, k, scope, "r-late", strings.Repeat("later filler that must not move the cut. ", 200), "", nil)

	res, err := runner.CompactNow(ctx, lease, "manual-resume")
	if err != nil {
		t.Fatalf("resumed CompactNow: %v", err)
	}
	if !res.Performed || res.KeptFromSeq != task.FirstKeptSeq {
		t.Fatalf("resumed result = %+v, want kept_from_seq %d from the pinned task", res, task.FirstKeptSeq)
	}
	if got := summary.callsMade(); got != 2 {
		t.Fatalf("summary calls = %d, want 2 (one failure + one resumed summary)", got)
	}
	if _, found, err := k.LoadCompactionTask(ctx, scope, "manual-resume"); err != nil || found {
		t.Fatalf("task after success = (found=%v, err=%v), want deleted", found, err)
	}
}

// TestRecoverAutomaticCompactionProjection pins the automatic half: an entry
// checkpoint whose committed event exists is projected on the next attempt, and
// one whose event never committed is discarded without touching the log.
func TestRecoverAutomaticCompactionProjection(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}
	runner, _ := newTestRunner(t, st)

	// Committed automatic event with a lost projection.
	commitInput(t, k, scope, "r1", "summarize me", "", nil)
	events := eventsOf(t, st, tenant, session)
	lastSeq := events[len(events)-1].Seq
	requestID := autoCompactionRequestID(lastSeq)
	raw := mustJSON(map[string]any{
		"summary": "automatic summary", "reason": "threshold",
		"tokensBefore": 900, "tokensAfter": 90,
		"firstKept": 1, "firstKeptTranscript": 1,
		"keptFromSeq": lastSeq, "requestId": requestID,
	})
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, tenant, session, store.EventCompaction, payload); err != nil {
		t.Fatalf("commit compaction event: %v", err)
	}
	if err := k.SaveCompactionTask(ctx, scope, kernel.CompactionTaskCheckpoint{
		RequestID: requestID, Phase: kernel.CompactionPhaseSelect, TokensBefore: 900,
	}); err != nil {
		t.Fatalf("save entry task: %v", err)
	}
	// A stale marker with no committed event must disappear as well.
	staleID := autoCompactionRequestID(lastSeq + 100)
	if err := k.SaveCompactionTask(ctx, scope, kernel.CompactionTaskCheckpoint{
		RequestID: staleID, Phase: kernel.CompactionPhaseSelect, TokensBefore: 10,
	}); err != nil {
		t.Fatalf("save stale task: %v", err)
	}

	runner.recoverAutomaticCompactions(ctx, scope, eventsOf(t, st, tenant, session))

	cp, found, err := k.LoadCheckpoint(ctx, scope)
	if err != nil || !found {
		t.Fatalf("recovered projection = (found=%v, err=%v)", found, err)
	}
	if cp.Summary != "automatic summary" || cp.KeptFromSeq != lastSeq || cp.RequestID != "" {
		t.Fatalf("projection = %+v, want the automatic event facts", cp)
	}
	tasks, err := k.ListCompactionTasks(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("tasks = %+v, want both markers consumed", tasks)
	}
}

// TestCompactionResumeKeepsLogContiguousAndSingleAnswer is the resume half of
// the crash contract: a compaction interrupted mid-summary leaves no event, the
// next normal attempt still answers each pending input exactly once, and the
// retried compaction adds exactly one event. The committed log is checked for
// contiguous seqs (no lost events) and one terminal reply per user input (no
// duplicate final answers).
func TestCompactionResumeKeepsLogContiguousAndSingleAnswer(t *testing.T) {
	noManualCompactRetryDelay(t)
	st := testStore(t)
	ctx := context.Background()
	tenant, session, k, scope := seedManualHistory(t, st)
	runner := manualRunner(t, st)
	runner.Stream = (&countedSummaryStream{failFirst: 1}).stream
	lease := claimScope(t, k, tenant, session)

	beforeCompactions := countKind(eventsOf(t, st, tenant, session), store.EventCompaction)
	if _, err := runner.CompactNow(ctx, lease, "manual-crash-resume"); err == nil {
		t.Fatal("CompactNow succeeded, want the injected summary failure")
	}
	if _, found, err := k.LoadCompactionTask(ctx, scope, "manual-crash-resume"); err != nil || !found {
		t.Fatalf("interrupted task = (found=%v, err=%v), want the pinned checkpoint", found, err)
	}
	// The manual call held the session lease; release it before the successor
	// run claims the same session.
	if err := k.Release(ctx, lease); err != nil {
		t.Fatalf("release lease after interrupted compaction: %v", err)
	}

	// The interrupted compaction committed nothing: a normal attempt must answer
	// the pending input, not treat the compaction as a lost turn.
	runner.Stream = FakeStream()
	requeue(t, st, tenant, session)
	if _, err := execRun(t, k, runner, tenant, session); err != nil {
		t.Fatalf("run after interrupted compaction: %v", err)
	}
	events := eventsOf(t, st, tenant, session)
	assertContiguousLog(t, events)
	if got := countKind(events, store.EventCompaction); got != beforeCompactions {
		t.Fatalf("compaction events after the normal attempt = %d, want %d", got, beforeCompactions)
	}
	// Every user input has exactly one terminal assistant answer.
	users, finals := 0, 0
	for _, event := range events {
		switch event.Kind {
		case store.EventUserMessage:
			users++
		case store.EventAssistantMessage:
			var payload struct {
				StopReason string `json:"stopReason"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("decode assistant message: %v", err)
			}
			if payload.StopReason == "end_turn" {
				finals++
			}
		}
	}
	if users == 0 || finals != users {
		t.Fatalf("users=%d terminal answers=%d, want one final answer per input", users, finals)
	}

	// The retry resumes the pinned range and adds exactly one compaction event.
	runner.Stream = shortSummaryStream()
	requeue(t, st, tenant, session)
	lease = claimScope(t, k, tenant, session)
	res, err := runner.CompactNow(ctx, lease, "manual-crash-resume")
	if err != nil || !res.Performed {
		t.Fatalf("resumed CompactNow = (%+v, %v), want a performed compaction", res, err)
	}
	events = eventsOf(t, st, tenant, session)
	assertContiguousLog(t, events)
	if got := countKind(events, store.EventCompaction); got != beforeCompactions+1 {
		t.Fatalf("compaction events after resume = %d, want %d (exactly one)", got, beforeCompactions+1)
	}
}

// TestManualCompactRetryBackoffAndAttempts pins the retry half of the task
// state machine: a failed summary records attempt=1, phase=retry and a durable
// retry_until; an immediate retry is refused without a second model call; after
// the deadline the same requestId resumes and spends exactly one more attempt.
func TestManualCompactRetryBackoffAndAttempts(t *testing.T) {
	previous := manualCompactRetryDelay
	manualCompactRetryDelay = time.Hour
	t.Cleanup(func() { manualCompactRetryDelay = previous })

	st := testStore(t)
	ctx := context.Background()
	tenant, session, k, scope := seedManualHistory(t, st)
	runner := manualRunner(t, st)
	summary := &countedSummaryStream{failFirst: 1}
	runner.Stream = summary.stream
	lease := claimScope(t, k, tenant, session)

	if _, err := runner.CompactNow(ctx, lease, "manual-retry"); err == nil {
		t.Fatal("first CompactNow succeeded, want the injected summary failure")
	}
	task, found, err := k.LoadCompactionTask(ctx, scope, "manual-retry")
	if err != nil || !found {
		t.Fatalf("task after failure = (found=%v, err=%v), want present", found, err)
	}
	if task.Phase != kernel.CompactionPhaseRetry || task.Attempt != 1 || task.RetryUntil.IsZero() {
		t.Fatalf("task after failure = %+v, want retry/attempt 1/deadline", task)
	}
	if !task.RetryUntil.After(time.Now()) {
		t.Fatalf("retry_until = %s, want a future deadline", task.RetryUntil)
	}

	// The deadline has not elapsed: no model call, typed refusal, same state.
	if _, err := runner.CompactNow(ctx, lease, "manual-retry"); !errors.Is(err, ErrManualCompactRetry) {
		t.Fatalf("immediate retry = %v, want ErrManualCompactRetry", err)
	}
	if got := summary.callsMade(); got != 1 {
		t.Fatalf("summary calls after backoff refusal = %d, want 1", got)
	}
	still, found, err := k.LoadCompactionTask(ctx, scope, "manual-retry")
	if err != nil || !found || still.Attempt != 1 || still.Phase != kernel.CompactionPhaseRetry {
		t.Fatalf("task after backoff refusal = (%+v, found=%v, err=%v)", still, found, err)
	}

	// Simulate the deadline elapsing, then resume: one more model call and a
	// performed compaction; the task is consumed.
	past := time.Now().Add(-time.Second)
	if err := k.SaveCompactionTask(ctx, scope, kernel.CompactionTaskCheckpoint{
		RequestID:     task.RequestID,
		Phase:         kernel.CompactionPhaseRetry,
		FirstKeptSeq:  task.FirstKeptSeq,
		TailSeq:       task.TailSeq,
		TokensBefore:  task.TokensBefore,
		PolicyVersion: task.PolicyVersion,
		Attempt:       task.Attempt,
		RetryUntil:    past,
	}); err != nil {
		t.Fatalf("clear retry deadline: %v", err)
	}
	res, err := runner.CompactNow(ctx, lease, "manual-retry")
	if err != nil || !res.Performed {
		t.Fatalf("resumed retry = (%+v, %v), want performed", res, err)
	}
	if got := summary.callsMade(); got != 2 {
		t.Fatalf("summary calls after resume = %d, want 2 (one failure + one retry)", got)
	}
	if _, found, err := k.LoadCompactionTask(ctx, scope, "manual-retry"); err != nil || found {
		t.Fatalf("task after success = (found=%v, err=%v), want deleted", found, err)
	}
}

// TestManualCompactPrunesRequestLedgerOnManualPaths pins P2-1: a manual-only
// session (no worker/run read path) still bounds the request ledger on both the
// failure and success exits.
func TestManualCompactPrunesRequestLedgerOnManualPaths(t *testing.T) {
	noManualCompactRetryDelay(t)
	st := testStore(t)
	ctx := context.Background()
	tenant, session, k, _ := seedManualHistory(t, st)
	runner := manualRunner(t, st)
	summary := &countedSummaryStream{failFirst: 1}
	runner.Stream = summary.stream
	checkpoints := &toggleCheckpoints{inner: k}
	runner.Checkpoints = checkpoints
	lease := claimScope(t, k, tenant, session)

	if _, err := runner.CompactNow(ctx, lease, "manual-prune"); err == nil {
		t.Fatal("first CompactNow succeeded, want the injected summary failure")
	}
	if got := checkpoints.requestPruneCount(); got != 1 {
		t.Fatalf("request prunes after failure = %d, want 1", got)
	}
	if _, err := runner.CompactNow(ctx, lease, "manual-prune"); err != nil {
		t.Fatalf("resumed CompactNow: %v", err)
	}
	if got := checkpoints.requestPruneCount(); got != 2 {
		t.Fatalf("request prunes after success = %d, want 2", got)
	}
}

// TestManualCompactCancellationPersistsRetryTask pins P2-2: the summary call
// cancels the request context, but the retry row is written on a detached
// bounded context, so the attempt/retry fact is not lost.
func TestManualCompactCancellationPersistsRetryTask(t *testing.T) {
	st := testStore(t)
	tenant, session, k, scope := seedManualHistory(t, st)
	runner := manualRunner(t, st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner.Stream = func(streamCtx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		cancel()
		return failingSummaryStream()(streamCtx, model, llm, cfg)
	}
	lease := claimScope(t, k, tenant, session)
	if _, err := runner.CompactNow(ctx, lease, "manual-cancel-retry"); err == nil {
		t.Fatal("cancelled CompactNow succeeded")
	}
	task, found, err := k.LoadCompactionTask(context.Background(), scope, "manual-cancel-retry")
	if err != nil || !found {
		t.Fatalf("retry task after cancellation = (found=%v, err=%v), want persisted", found, err)
	}
	if task.Phase != kernel.CompactionPhaseRetry || task.Attempt != 1 || task.RetryUntil.IsZero() {
		t.Fatalf("retry task = %+v, want retry/attempt 1/deadline", task)
	}
}

// assertContiguousLog fails when the committed seqs are not 1..N.
func assertContiguousLog(t *testing.T, events []store.Event) {
	t.Helper()
	for index, event := range events {
		if want := int64(index + 1); event.Seq != want {
			t.Fatalf("event %d has seq %d, want %d (lost or duplicated event)", index, event.Seq, want)
		}
	}
}
