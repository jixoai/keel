package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/store"
)

// failingSummaryStream makes every summarization request fail with a provider
// error, the deterministic stand-in for an overloaded upstream.
func failingSummaryStream() provider.StreamFn {
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		s := provider.NewAssistantMessageEventStream(2)
		go func() {
			defer s.Close()
			_ = s.Emit(ctx, provider.StreamDoneEvent{Message: agentcore.AssistantMessage{
				RoleField:    agentcore.RoleAssistant,
				StopReason:   agentcore.StopReasonError,
				ErrorMessage: "upstream overloaded",
			}})
		}()
		return s, nil
	}
}

// seedManualHistory builds a transcript with one long summarized candidate and a
// trailing user message, leaving the run claimable for a manual compaction.
func seedManualHistory(t *testing.T, st *store.Store) (uuid.UUID, uuid.UUID, *kernel.Kernel, kernel.Scope) {
	t.Helper()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", strings.Repeat("context filler ALPHA. ", 300), "", nil)
	runner, _ := newTestRunner(t, st)
	if _, err := execRun(t, k, runner, tenant, session); err != nil {
		t.Fatalf("first exchange: %v", err)
	}
	commitInput(t, k, scope, "r2", "second question", "", nil)
	if _, err := execRun(t, k, runner, tenant, session); err != nil {
		t.Fatalf("second exchange: %v", err)
	}
	// A trailing input wakes the run and stays unanswered: manual compaction must
	// retain it while summarizing the earlier turns.
	commitInput(t, k, scope, "r3", "please compact now", "", nil)
	return tenant, session, k, scope
}

// shortSummaryStream returns a compact summary for every request. FakeStream
// echoes the prompt (which is the whole conversation), so a manual-compaction
// test that asserts a token reduction must not use it for the summary call.
func shortSummaryStream() provider.StreamFn {
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		msg := agentcore.AssistantMessage{
			RoleField:  agentcore.RoleAssistant,
			Content:    agentcore.ContentList{agentcore.NewTextContent("## Goal\ncontinue the work")},
			StopReason: agentcore.StopReasonEndTurn,
		}
		s := provider.NewAssistantMessageEventStream(2)
		go func() {
			defer s.Close()
			_ = s.Emit(ctx, provider.StreamStartEvent{Partial: agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant}})
			_ = s.Emit(ctx, provider.StreamTextEvent{Partial: msg})
			_ = s.Emit(ctx, provider.StreamDoneEvent{Message: msg})
		}()
		return s, nil
	}
}

func manualRunner(t *testing.T, st *store.Store) *Runner {
	t.Helper()
	runner, _ := newTestRunner(t, st)
	runner.Stream = shortSummaryStream()
	policy := compaction.CheckpointPolicy{
		Enabled:        true,
		RetainTokens:   10,
		ManualCompact:  true,
		MaxCheckpoints: 0,
	}
	runner.Compaction = &CompactionOptions{
		ContextWindow: 100000,
		Settings:      compaction.CompactionSettings{Enabled: true, KeepRecentTokens: 10},
		Policy:        &policy,
	}
	return runner
}

// TestManualCompactCreatesIdempotentCheckpoint pins the manual surface's core
// contract: one compaction event plus one projection, the pending user retained,
// identifier-only result fields, and a repeated requestId replaying the same
// checkpoint without a second LLM call or a second event.
func TestManualCompactCreatesIdempotentCheckpoint(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session, k, scope := seedManualHistory(t, st)
	runner := manualRunner(t, st)
	lease := claimScope(t, k, tenant, session)

	beforeEvents := countKind(eventsOf(t, st, tenant, session), store.EventCompaction)
	res, err := runner.CompactNow(ctx, lease, "manual-1")
	if err != nil {
		t.Fatalf("CompactNow: %v", err)
	}
	if !res.Performed || res.Replayed || res.CheckpointID <= 0 {
		t.Fatalf("result = %+v, want a performed checkpoint", res)
	}
	if res.TokensBefore <= res.TokensAfter {
		t.Fatalf("tokens before/after = %d/%d, want a reduction", res.TokensBefore, res.TokensAfter)
	}
	if res.KeptFromSeq <= 0 || res.KeptFromSeq >= res.CheckpointID {
		t.Fatalf("kept_from_seq = %d, checkpoint = %d", res.KeptFromSeq, res.CheckpointID)
	}
	if res.PolicyVersion == "" {
		t.Fatal("policy version is empty")
	}
	cp, found, err := k.CheckpointByRequest(ctx, scope, "manual-1")
	if err != nil || !found {
		t.Fatalf("checkpoint by request = (found=%v, err=%v)", found, err)
	}
	if cp.UptoSeq != res.CheckpointID || cp.KeptFromSeq != res.KeptFromSeq ||
		cp.TokensBefore != res.TokensBefore || cp.TokensAfter != res.TokensAfter ||
		cp.PolicyVersion != res.PolicyVersion || cp.RequestID != "manual-1" {
		t.Fatalf("stored checkpoint = %+v, want the reported result", cp)
	}
	if got := countKind(eventsOf(t, st, tenant, session), store.EventCompaction); got != beforeEvents+1 {
		t.Fatalf("compaction events = %d, want %d", got, beforeEvents+1)
	}

	// The pending user survives in the replay view (summary + retained tail).
	events := eventsOf(t, st, tenant, session)
	view, checkpoint, _ := runner.modelView(ctx, scope, events)
	if checkpoint == nil {
		t.Fatal("model view lost the checkpoint")
	}
	msgs, _ := DecodeMessages(view, nil)
	if _, ok := firstUserMessageWith(msgs, "please compact now"); !ok {
		t.Fatal("manual compaction summarized the pending user message")
	}

	// A repeat returns the same checkpoint and writes nothing.
	again, err := runner.CompactNow(ctx, lease, "manual-1")
	if err != nil {
		t.Fatalf("replayed CompactNow: %v", err)
	}
	if !again.Replayed || again.Performed || again.CheckpointID != res.CheckpointID {
		t.Fatalf("replay = %+v, want the same checkpoint replayed", again)
	}
	if got := countKind(eventsOf(t, st, tenant, session), store.EventCompaction); got != beforeEvents+1 {
		t.Fatalf("compaction events after replay = %d, want %d", got, beforeEvents+1)
	}
	if n := checkpointRows(t, st, tenant, session); n != 1 {
		t.Fatalf("checkpoint rows = %d, want 1", n)
	}
}

// TestManualCompactFailureKeepsContext pins the failure semantics: a provider
// error or cancellation leaves the event log and checkpoint projections exactly
// as they were.
func TestManualCompactFailureKeepsContext(t *testing.T) {
	st := testStore(t)
	tenant, session, k, scope := seedManualHistory(t, st)
	runner := manualRunner(t, st)
	runner.Stream = failingSummaryStream()
	lease := claimScope(t, k, tenant, session)

	beforeEvents := countKind(eventsOf(t, st, tenant, session), store.EventCompaction)
	beforeRows := checkpointRows(t, st, tenant, session)
	if _, err := runner.CompactNow(context.Background(), lease, "manual-failed"); err == nil {
		t.Fatal("CompactNow = nil error, want the summary failure")
	}
	if got := countKind(eventsOf(t, st, tenant, session), store.EventCompaction); got != beforeEvents {
		t.Fatalf("compaction events = %d, want unchanged %d", got, beforeEvents)
	}
	if got := checkpointRows(t, st, tenant, session); got != beforeRows {
		t.Fatalf("checkpoint rows = %d, want unchanged %d", got, beforeRows)
	}
	if _, found, err := k.CheckpointByRequest(context.Background(), scope, "manual-failed"); err != nil || found {
		t.Fatalf("failed request left a checkpoint (found=%v, err=%v)", found, err)
	}

	// Cancellation is the same: no event, no projection.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runner.CompactNow(ctx, lease, "manual-cancelled"); err == nil {
		t.Fatal("cancelled CompactNow = nil error")
	}
	if got := countKind(eventsOf(t, st, tenant, session), store.EventCompaction); got != beforeEvents {
		t.Fatalf("compaction events after cancel = %d, want %d", got, beforeEvents)
	}
	if n := checkpointRows(t, st, tenant, session); n != beforeRows {
		t.Fatalf("checkpoint rows after cancel = %d, want %d", n, beforeRows)
	}
}

// TestManualCompactPolicyGate pins the policy switch: a tenant/session policy
// that disables manual compaction refuses the call without reading or writing
// the log.
func TestManualCompactPolicyGate(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session, k, _ := seedManualHistory(t, st)
	runner := manualRunner(t, st)
	disabled := *runner.Compaction.Policy
	disabled.ManualCompact = false
	runner.Compaction.Policy = &disabled
	lease := claimScope(t, k, tenant, session)

	if _, err := runner.CompactNow(ctx, lease, "manual-disabled"); !errors.Is(err, ErrManualCompactDisabled) {
		t.Fatalf("CompactNow err = %v, want ErrManualCompactDisabled", err)
	}
	disabled.Enabled = true
	disabled.ManualCompact = true
	// Sanity: the same history is compactable once manual is allowed.
	if _, err := runner.CompactNow(ctx, lease, "manual-allowed"); err != nil {
		t.Fatalf("CompactNow with manual enabled: %v", err)
	}
}

// TestCheckpointCorruptionFallsBackToFullLog pins fail-closed recovery: a
// projection that does not resolve against committed events is ignored and the
// model sees the full event log (the summarized input is present again).
func TestCheckpointCorruptionFallsBackToFullLog(t *testing.T) {
	cases := []struct {
		name    string
		corrupt func(t *testing.T, st *store.Store, tenant, session uuid.UUID, cp kernel.Checkpoint, events []store.Event)
	}{
		{
			name: "empty summary",
			corrupt: func(t *testing.T, st *store.Store, tenant, session uuid.UUID, cp kernel.Checkpoint, _ []store.Event) {
				t.Helper()
				if _, err := st.Pool().Exec(context.Background(),
					`UPDATE checkpoints SET summary = '' WHERE tenant_id = $1 AND session_id = $2`,
					tenant, session); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "missing tail anchor",
			corrupt: func(t *testing.T, st *store.Store, tenant, session uuid.UUID, cp kernel.Checkpoint, _ []store.Event) {
				t.Helper()
				if _, err := st.Pool().Exec(context.Background(),
					`UPDATE checkpoints SET kept_from_seq = 999999 WHERE tenant_id = $1 AND session_id = $2`,
					tenant, session); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "upto seq is not a compaction event",
			corrupt: func(t *testing.T, st *store.Store, tenant, session uuid.UUID, cp kernel.Checkpoint, events []store.Event) {
				t.Helper()
				var assistantSeq int64
				for _, ev := range events {
					if ev.Kind == store.EventAssistantMessage && ev.Seq < cp.UptoSeq {
						assistantSeq = ev.Seq
						break
					}
				}
				if assistantSeq == 0 {
					t.Fatal("fixture has no assistant event")
				}
				if _, err := st.Pool().Exec(context.Background(),
					`UPDATE checkpoints SET upto_seq = $3 WHERE tenant_id = $1 AND session_id = $2`,
					tenant, session, assistantSeq); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			tenant, session, k, cp, _ := seedCompaction(t, st)
			scope := kernel.Scope{TenantID: tenant, SessionID: session}
			tc.corrupt(t, st, tenant, session, cp, eventsOf(t, st, tenant, session))

			commitInput(t, k, scope, "r3", "continue", "", nil)
			requeue(t, st, tenant, session)
			recorder := &checkpointRecordingStream{}
			runner, _ := newTestRunner(t, st)
			runner.Stream = recorder.stream
			if _, err := execRun(t, k, runner, tenant, session); err != nil {
				t.Fatalf("fallback run: %v", err)
			}
			msgs := recorder.last(t)
			if _, ok := firstUserMessageWith(msgs, "ALPHA"); !ok {
				t.Fatal("corrupt checkpoint truncated the summarized input instead of falling back")
			}
			// The fallback rebuilds from the committed event log, so a compaction
			// marker may still appear (the original event is truth); the proof is
			// that the summarized prefix is visible again, not truncated.
		})
	}
}

// TestCheckpointRetentionPrunesOnlyProjections pins MaxCheckpoints through the
// runner's own policy hook: the newest checkpoint is kept, older projections are
// deleted, and the event log is untouched.
func TestCheckpointRetentionPrunesOnlyProjections(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session, k, first, _ := seedCompaction(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}
	if n := checkpointRows(t, st, tenant, session); n != 1 {
		t.Fatalf("seed checkpoint rows = %d, want 1", n)
	}

	long := strings.Repeat("another context filler about GREEN. ", 400)
	commitInput(t, k, scope, "r3", long, "", nil)
	requeue(t, st, tenant, session)
	runner := newCompactionRunner(t, st)
	runner.Compaction.Policy = &compaction.CheckpointPolicy{
		Enabled:        true,
		RetainTokens:   1,
		MaxCheckpoints: 1,
		ManualCompact:  true,
	}
	if _, err := execRun(t, k, runner, tenant, session); err != nil {
		t.Fatalf("second compaction: %v", err)
	}
	second, found, err := k.LoadCheckpoint(ctx, scope)
	if err != nil || !found {
		t.Fatalf("checkpoint after policy run = (found=%v, err=%v)", found, err)
	}
	if second.UptoSeq <= first.UptoSeq {
		t.Fatalf("newest checkpoint = %d, want > %d", second.UptoSeq, first.UptoSeq)
	}
	if n := checkpointRows(t, st, tenant, session); n != 1 {
		t.Fatalf("checkpoint rows = %d, want 1 (MaxCheckpoints)", n)
	}
	// Days after pruning, the summarized events are still readable.
	if evs := eventsOf(t, st, tenant, session); countKind(evs, store.EventCompaction) != 2 {
		t.Fatalf("compaction events = %d, want 2: pruning must never delete events", countKind(evs, store.EventCompaction))
	}
}

// TestCheckpointPolicyVersionPersisted pins that a checkpoint records the
// effective policy identity it was produced under.
func TestCheckpointPolicyVersionPersisted(t *testing.T) {
	st := testStore(t)
	tenant, session, _, cp, _ := seedCompaction(t, st)
	_ = tenant
	_ = session
	if cp.PolicyVersion == "" {
		t.Fatal("checkpoint has no policy version")
	}
	if cp.TokensBefore <= 0 || cp.TokensAfter <= 0 {
		t.Fatalf("checkpoint tokens = %d/%d, want the compaction accounting", cp.TokensBefore, cp.TokensAfter)
	}
}

// TestCompactionSettingsPolicyThreshold pins the automatic trigger mapping: an
// absolute policy threshold becomes the loop's reserve, and the loop's existing
// ShouldCompact test then fires exactly above that threshold.
func TestCompactionSettingsPolicyThreshold(t *testing.T) {
	policy := compaction.CheckpointPolicy{Enabled: true, ThresholdTokens: 1000}
	runner := &Runner{Compaction: &CompactionOptions{
		ContextWindow: 5000,
		Settings:      compaction.CompactionSettings{Enabled: true, ReserveTokens: 1, KeepRecentTokens: 100},
		Policy:        &policy,
	}}
	settings := runner.compactionSettings(context.Background(), kernel.Scope{}, 5000)
	if settings.ReserveTokens != 4000 {
		t.Fatalf("reserve = %d, want 4000 (window 5000 - threshold 1000)", settings.ReserveTokens)
	}
	if !compaction.ShouldCompact(1001, 5000, settings) {
		t.Fatal("1001 tokens should trigger with an absolute threshold of 1000")
	}
	if compaction.ShouldCompact(1000, 5000, settings) {
		t.Fatal("the threshold itself is not above the threshold")
	}
	// Without a policy threshold the configured reserve is untouched.
	plain := &Runner{Compaction: &CompactionOptions{
		Settings: compaction.CompactionSettings{Enabled: true, ReserveTokens: 777},
	}}
	if got := plain.compactionSettings(context.Background(), kernel.Scope{}, 5000); got.ReserveTokens != 777 {
		t.Fatalf("reserve without policy = %d, want 777", got.ReserveTokens)
	}

	// The effective policy also gates the automatic loop and its retained
	// budget: a session disabling compaction must stop it, not just skip the
	// checkpoint write.
	disabled := compaction.CheckpointPolicy{Enabled: false, RetainTokens: 50, ManualCompact: true}
	resolver := &fakePolicyResolver{policy: disabled}
	gated := &Runner{
		Compaction:         &CompactionOptions{Settings: compaction.CompactionSettings{Enabled: true, KeepRecentTokens: 100}},
		CheckpointPolicies: resolver,
	}
	settings = gated.compactionSettings(context.Background(), kernel.Scope{}, 5000)
	if settings.Enabled || settings.KeepRecentTokens != 50 {
		t.Fatalf("gated settings = %+v, want disabled and retain 50", settings)
	}
}

// TestHandoffRestoresFromCheckpoint pins the handoff path: the successor owner
// (a different runtime id and a higher fence epoch) rebuilds the model context
// from the checkpoint + tail, exactly like a restart. No special-case recovery
// code exists for handoff; it is the same Runner.modelView path.
func TestHandoffRestoresFromCheckpoint(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session, k, cp, _ := seedCompaction(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}
	requeue(t, st, tenant, session)

	first, err := k.ClaimScope(ctx, scope, "runtime-a", time.Minute)
	if err != nil {
		t.Fatalf("claim runtime-a: %v", err)
	}
	if err := k.Release(ctx, first); err != nil {
		t.Fatalf("release runtime-a: %v", err)
	}
	second, err := k.ClaimScope(ctx, scope, "runtime-b", time.Minute)
	if err != nil {
		t.Fatalf("claim runtime-b: %v", err)
	}
	if second.Token.Epoch <= first.Token.Epoch {
		t.Fatalf("handoff epoch = %d, want > %d", second.Token.Epoch, first.Token.Epoch)
	}

	commitInput(t, k, scope, "r4", "what color did I mention?", "", nil)
	recorder := &checkpointRecordingStream{}
	successor, _ := newTestRunner(t, st)
	successor.Stream = recorder.stream
	cursor, err := successor.Run(ctx, second)
	if err != nil {
		t.Fatalf("successor run: %v", err)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: second, Cursor: cursor, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish successor: %v", err)
	}

	msgs := recorder.last(t)
	checkpoint, ok := msgs[0].(agentcore.CompactionMessage)
	if !ok {
		t.Fatalf("handoff transcript starts with %T, want the checkpoint summary", msgs[0])
	}
	if checkpoint.Summary != cp.Summary {
		t.Fatal("handoff used a checkpoint other than the newest")
	}
	if _, ok := firstUserMessageWith(msgs, "ALPHA"); ok {
		t.Fatal("handoff replayed the summarized prefix instead of the checkpoint")
	}
	if _, ok := firstUserMessageWith(msgs, "what color"); !ok {
		t.Fatal("handoff lost the input that arrived after the checkpoint")
	}
}

// fakePolicyResolver is the session policy seam double.
type fakePolicyResolver struct {
	policy compaction.CheckpointPolicy
	err    error
	calls  int
}

func (f *fakePolicyResolver) CheckpointPolicy(context.Context, uuid.UUID, uuid.UUID) (compaction.CheckpointPolicy, error) {
	f.calls++
	return f.policy, f.err
}

// TestRunnerUsesSessionCheckpointPolicy pins the per-attempt resolver: a
// session policy overrides the generation policy, and a resolver failure falls
// back to the generation policy instead of stopping the run.
func TestRunnerUsesSessionCheckpointPolicy(t *testing.T) {
	ctx := context.Background()
	base := compaction.CheckpointPolicy{Enabled: true, RetainTokens: 20000, MaxCheckpoints: 3, ManualCompact: true}
	resolver := &fakePolicyResolver{policy: compaction.CheckpointPolicy{
		Enabled: true, RetainTokens: 100, MaxCheckpoints: 1, TTL: time.Hour, ManualCompact: false,
	}}
	runner := &Runner{Compaction: &CompactionOptions{Policy: &base}, CheckpointPolicies: resolver, Logger: discardLog()}
	got := runner.checkpointPolicy(ctx, kernel.Scope{TenantID: uuid.New(), SessionID: uuid.New()})
	if got.MaxCheckpoints != 1 || got.RetainTokens != 100 || got.TTL != time.Hour || got.ManualCompact {
		t.Fatalf("resolved policy = %+v, want the session override", got)
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolver.calls)
	}

	// A failing resolver keeps the generation policy (a policy read never stops
	// a run).
	resolver = &fakePolicyResolver{err: errors.New("policy store down")}
	runner.CheckpointPolicies = resolver
	if got := runner.checkpointPolicy(ctx, kernel.Scope{}); got.MaxCheckpoints != 3 || got.RetainTokens != 20000 {
		t.Fatalf("fallback policy = %+v, want the generation policy", got)
	}
	// An invalid policy from the resolver also falls back.
	resolver = &fakePolicyResolver{policy: compaction.CheckpointPolicy{RetainTokens: -1}}
	runner.CheckpointPolicies = resolver
	if got := runner.checkpointPolicy(ctx, kernel.Scope{}); got.RetainTokens != 20000 {
		t.Fatalf("invalid policy fallback = %+v", got)
	}
}
