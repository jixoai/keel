package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

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
	if task.Phase != kernel.CompactionPhaseSummarize || task.FirstKeptSeq <= 0 || task.TailSeq < task.FirstKeptSeq {
		t.Fatalf("pinned task = %+v", task)
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
