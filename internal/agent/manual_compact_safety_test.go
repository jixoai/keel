package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/store"
)

// seedTwoUnreadInputs builds the review's P0 shape: run 1 is already in flight
// with u2 when u3 and u4 arrive. Its answer consumes u2 only, so u3/u4 stay
// unread and sit before that answer in log order.
func seedTwoUnreadInputs(t *testing.T, st *store.Store, u2Text string) (uuid.UUID, uuid.UUID, *kernel.Kernel, kernel.Scope, int64, int64) {
	t.Helper()
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "remember the word ALPHA", "", nil)
	runner, _ := newTestRunner(t, st)
	if _, err := execRun(t, k, runner, tenant, session); err != nil {
		t.Fatalf("run 0: %v", err)
	}
	commitInput(t, k, scope, "r2", u2Text, "queue", nil)
	started := make(chan struct{})
	release := make(chan struct{})
	blocking := func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		close(started)
		<-release
		return FakeStream()(ctx, model, llm, cfg)
	}
	run1, _ := newTestRunner(t, st)
	run1.Stream = blocking
	lease := claimScope(t, k, tenant, session)
	type result struct {
		cursor int64
		err    error
	}
	done := make(chan result, 1)
	go func() {
		cursor, err := run1.Run(ctx, lease)
		done <- result{cursor: cursor, err: err}
	}()
	<-started
	u3 := strings.Repeat("unread BLUE. ", 20)
	u4 := strings.Repeat("unread GREEN. ", 20)
	u3Seq := commitInput(t, k, scope, "r3", u3, "queue", nil)
	u4Seq := commitInput(t, k, scope, "r4", u4, "queue", nil)
	close(release)
	res := <-done
	if res.err != nil {
		t.Fatalf("run 1: %v", res.err)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease, Cursor: res.cursor, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish run 1: %v", err)
	}
	// A manual compaction claims the same run lease; queue the run the way the
	// product adapter does when no attempt is claimable.
	requeue(t, st, tenant, session)
	unread, known := unreadInputs(eventsOf(t, st, tenant, session), lease.ReadWatermark)
	if !known || !unread[u3Seq] || !unread[u4Seq] {
		t.Fatalf("fixture unread = %v (known=%v), want u3=%d and u4=%d", unread, known, u3Seq, u4Seq)
	}
	return tenant, session, k, scope, u3Seq, u4Seq
}

func manualSafetyRunner(t *testing.T, st *store.Store, policy compaction.CheckpointPolicy) *Runner {
	t.Helper()
	runner, _ := newTestRunner(t, st)
	runner.Stream = shortSummaryStream()
	runner.Compaction = &CompactionOptions{
		ContextWindow: 100000,
		Settings:      compaction.CompactionSettings{Enabled: true, KeepRecentTokens: policy.RetainTokens},
		Policy:        &policy,
	}
	return runner
}

func compactionEventCount(t *testing.T, st *store.Store, tenant, session uuid.UUID) int {
	t.Helper()
	return countKind(eventsOf(t, st, tenant, session), store.EventCompaction)
}

// TestManualCompactRefusesTwoUnreadInputsWithZeroRetain pins the P0: with a zero
// retained budget the cut would land inside the moved unread block; the call is
// refused before any summary/event/checkpoint, so u3/u4 cannot be lost.
func TestManualCompactRefusesTwoUnreadInputsWithZeroRetain(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session, k, _, _, _ := seedTwoUnreadInputs(t, st, "second input")
	runner := manualSafetyRunner(t, st, compaction.CheckpointPolicy{Enabled: true, RetainTokens: 0, ManualCompact: true})
	lease := claimScope(t, k, tenant, session)
	before := compactionEventCount(t, st, tenant, session)

	_, err := runner.CompactNow(ctx, lease, "manual-unread-0")
	if !errors.Is(err, ErrManualCompactUnsafe) {
		t.Fatalf("CompactNow err = %v, want ErrManualCompactUnsafe", err)
	}
	if got := compactionEventCount(t, st, tenant, session); got != before {
		t.Fatalf("compaction events = %d, want unchanged %d", got, before)
	}
	if _, found, err := k.CheckpointByRequest(ctx, kernel.Scope{TenantID: tenant, SessionID: session}, "manual-unread-0"); err != nil || found {
		t.Fatalf("refused compaction left a checkpoint (found=%v, err=%v)", found, err)
	}
}

// TestManualCompactKeepsTwoUnreadInputsWithDefaultRetain pins the safe half of
// P0: a cut before the moved unread block anchors to log order (never past u3),
// and the following attempt can still answer both unread inputs.
func TestManualCompactKeepsTwoUnreadInputsWithDefaultRetain(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	u2 := strings.Repeat("second input filler. ", 4000)
	tenant, session, k, scope, u3Seq, u4Seq := seedTwoUnreadInputs(t, st, u2)
	runner := manualSafetyRunner(t, st, compaction.CheckpointPolicy{Enabled: true, RetainTokens: 20000, ManualCompact: true})
	lease := claimScope(t, k, tenant, session)

	result, err := runner.CompactNow(ctx, lease, "manual-unread-default")
	if err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	cp, found, err := k.LoadCheckpoint(ctx, scope)
	if err != nil || !found {
		t.Fatalf("checkpoint = (found=%v, err=%v), want found", found, err)
	}
	if cp.KeptFromSeq > u3Seq || cp.KeptFromSeq > u4Seq {
		t.Fatalf("checkpoint kept_from_seq = %d, must not start after unread u3=%d/u4=%d", cp.KeptFromSeq, u3Seq, u4Seq)
	}
	if result.KeptFromSeq != cp.KeptFromSeq {
		t.Fatalf("result kept_from = %d, stored = %d", result.KeptFromSeq, cp.KeptFromSeq)
	}
	if err := k.Release(ctx, lease); err != nil {
		t.Fatalf("release manual claim: %v", err)
	}

	// The next attempt answers the unread inputs; they were never summarized.
	before := countKind(eventsOf(t, st, tenant, session), store.EventAssistantMessage)
	requeue(t, st, tenant, session)
	next, _ := newTestRunner(t, st)
	if _, err := execRun(t, k, next, tenant, session); err != nil {
		t.Fatalf("successor run: %v", err)
	}
	events := eventsOf(t, st, tenant, session)
	assistants := assistantEvents(events)
	if len(assistants) <= before {
		t.Fatalf("successor did not answer the unread inputs (assistants %d -> %d)", before, len(assistants))
	}
	consumed := map[int64]bool{}
	for _, seq := range inputSeqsOf(t, assistants[len(assistants)-1]) {
		consumed[seq] = true
	}
	if !consumed[u3Seq] || !consumed[u4Seq] {
		t.Fatalf("successor consumed %v, want unread u3=%d and u4=%d", consumed, u3Seq, u4Seq)
	}
}

// TestCheckpointRecoveryRejectsUnreadBelowTail pins the recovery half of P0: a
// projection whose kept_from starts after an unread input is ignored and the
// full log is replayed.
func TestCheckpointRecoveryRejectsUnreadBelowTail(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	u2 := strings.Repeat("second input filler. ", 4000)
	tenant, session, k, scope, u3Seq, _ := seedTwoUnreadInputs(t, st, u2)
	runner := manualSafetyRunner(t, st, compaction.CheckpointPolicy{Enabled: true, RetainTokens: 20000, ManualCompact: true})
	lease := claimScope(t, k, tenant, session)
	if _, err := runner.CompactNow(ctx, lease, "manual-guard"); err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	if err := k.Release(ctx, lease); err != nil {
		t.Fatalf("release manual claim: %v", err)
	}

	// Corrupt the projection to start after u3: it would hide the unread input.
	if _, err := st.Pool().Exec(ctx,
		`UPDATE checkpoints SET kept_from_seq = $3 WHERE tenant_id = $1 AND session_id = $2`,
		tenant, session, u3Seq+1); err != nil {
		t.Fatalf("corrupt checkpoint: %v", err)
	}
	cp, found, err := k.LoadCheckpoint(ctx, scope)
	if err != nil || !found || cp.KeptFromSeq != u3Seq+1 {
		t.Fatalf("corrupted checkpoint = (%+v, found=%v, err=%v)", cp, found, err)
	}

	commitInput(t, k, scope, "r5", "after corruption", "", nil)
	requeue(t, st, tenant, session)
	recorder := &checkpointRecordingStream{}
	next, _ := newTestRunner(t, st)
	next.Stream = recorder.stream
	if _, err := execRun(t, k, next, tenant, session); err != nil {
		t.Fatalf("recovery run: %v", err)
	}
	msgs := recorder.last(t)
	if _, ok := firstUserMessageWith(msgs, "unread BLUE"); !ok {
		t.Fatal("recovery used a checkpoint that hides an unread input")
	}
}

// TestManualCompactUsesResolvedModel pins the P0 model-resolution fix: the
// summary goes to the resolved provider stream/key/model/thinking/window/patch,
// never the static/fake runner fields; a resolver failure writes nothing.
func TestManualCompactUsesResolvedModel(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session, k, _ := seedManualHistory(t, st)
	rec := &recordingStream{}
	resolver := &stubResolver{resolved: ActiveModel{
		ProviderID:    "resolved-provider",
		Model:         "resolved-model",
		APIKey:        "resolved-key",
		Stream:        rec.fn(),
		ThinkingLevel: agentcore.ThinkingHigh,
		ContextWindow: 64000,
		RequestPatch:  map[string]any{"max_tokens": 4096},
	}}
	runner := NewRunner(k, k, FakeStream(), "static-provider", "static-model", "static-key", discardLog())
	runner.Models = resolver
	policy := compaction.CheckpointPolicy{Enabled: true, RetainTokens: 10, ManualCompact: true}
	runner.Compaction = &CompactionOptions{
		ContextWindow: 100000,
		Settings:      compaction.CompactionSettings{Enabled: true, KeepRecentTokens: 10},
		Policy:        &policy,
	}
	lease := claimScope(t, k, tenant, session)
	if _, err := runner.CompactNow(ctx, lease, "manual-model"); err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	models, calls := rec.snapshot()
	if len(models) != 1 || models[0] != "resolved-model" {
		t.Fatalf("wire models = %v, want the resolved model", models)
	}
	if calls[0].APIKey != "resolved-key" || calls[0].ThinkingLevel != agentcore.ThinkingHigh {
		t.Fatalf("wire auth/thinking = %q/%q", calls[0].APIKey, calls[0].ThinkingLevel)
	}
	if calls[0].Extra["max_tokens"] != 4096 {
		t.Fatalf("wire request patch = %v", calls[0].Extra)
	}

	// A resolver failure is fail closed: no compaction event, no checkpoint.
	failing := &stubResolver{err: errors.New("model unavailable")}
	runner.Models = failing
	before := compactionEventCount(t, st, tenant, session)
	if _, err := runner.CompactNow(ctx, lease, "manual-model-fail"); err == nil {
		t.Fatal("CompactNow with a failing resolver = nil error")
	}
	if got := compactionEventCount(t, st, tenant, session); got != before {
		t.Fatalf("resolver failure committed %d compaction event(s)", got-before)
	}
}

// TestManualCompactRefusesDanglingToolCall pins P1: an assistant tool call with
// no result must be settled by a normal attempt, not summarized into a done run.
func TestManualCompactRefusesDanglingToolCall(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}
	commitInput(t, k, scope, "r1", "call a tool", "queue", nil)
	lease := claimScope(t, k, tenant, session)
	assistant := agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList{agentcore.NewToolCallContent("call-open", "echo", json.RawMessage(`{"x":"y"}`))},
		StopReason: agentcore.StopReasonToolUse,
	}
	raw, err := json.Marshal(assistant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Commit(ctx, kernel.CommitRequest{
		Scope: scope, Kind: kernel.KindAssistantMessage, Token: lease.Token,
		GenerationID: "gen-open", Payload: raw,
	}); err != nil {
		t.Fatalf("commit dangling assistant: %v", err)
	}
	commitInput(t, k, scope, "r2", "next user", "queue", nil)
	if err := k.Release(ctx, lease); err != nil {
		t.Fatalf("release: %v", err)
	}
	if calls, err := k.UnresolvedToolCalls(ctx, scope); err != nil || len(calls) != 1 {
		t.Fatalf("unresolved tool calls = (%v, %v), want one", calls, err)
	}

	runner := manualSafetyRunner(t, st, compaction.CheckpointPolicy{Enabled: true, RetainTokens: 10, ManualCompact: true})
	manualLease := claimScope(t, k, tenant, session)
	before := compactionEventCount(t, st, tenant, session)
	if _, err := runner.CompactNow(ctx, manualLease, "manual-tool"); !errors.Is(err, ErrManualCompactToolCallOpen) {
		t.Fatalf("CompactNow err = %v, want ErrManualCompactToolCallOpen", err)
	}
	if got := compactionEventCount(t, st, tenant, session); got != before {
		t.Fatalf("dangling-tool compaction committed %d event(s)", got-before)
	}
}
