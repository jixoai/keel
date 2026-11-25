package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/sessionfork"
)

type fakeForkPrefix struct {
	tenant  uuid.UUID
	child   uuid.UUID
	fact    sessionfork.Fact
	present bool
}

func (f fakeForkPrefix) SessionFork(_ context.Context, tenantID, sessionID uuid.UUID) (sessionfork.Fact, bool, error) {
	if tenantID != f.tenant || sessionID != f.child || !f.present {
		return sessionfork.Fact{}, false, nil
	}
	return f.fact, true, nil
}

// TestForkPrefixIsAReadTimeUnion pins the ledger half of fork: the child's model
// transcript starts with the parent's committed messages up to the origin, the
// parent log is not copied into the child, and inputSeqs records only the
// child's own input (never the negative synthetic prefix seqs).
func TestForkPrefixIsAReadTimeUnion(t *testing.T) {
	mem := kernel.NewMemStore()
	kern := kernel.New(mem, kernel.Options{})
	ctx := context.Background()
	tenant, parent, child := uuid.New(), uuid.New(), uuid.New()
	for _, session := range []uuid.UUID{parent, child} {
		mem.CreateSession(tenant, session)
		mem.EnqueueRun(tenant, session)
	}
	parentScope := kernel.Scope{TenantID: tenant, SessionID: parent}
	childScope := kernel.Scope{TenantID: tenant, SessionID: child}

	parentInputSeq := commitInput(t, kern, parentScope, "parent-r1", "parent question", "", nil)
	parentLease := claimScope(t, kern, tenant, parent)
	parentRunner := NewRunner(kern, kern, FakeStream(), "fake", "fake", "", discardLog())
	if _, err := parentRunner.RunAttempt(ctx, parentLease); err != nil {
		t.Fatalf("parent attempt: %v", err)
	}
	parentEvents, err := kern.LoadEvents(ctx, parentScope, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(parentEvents) < 2 {
		t.Fatalf("parent events = %d, want input + answer", len(parentEvents))
	}
	childInputSeq := commitInput(t, kern, childScope, "child-r1", "child task", "", nil)
	childLease := claimScope(t, kern, tenant, child)

	var seen []agentcore.MessageList
	recording := func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		seen = append(seen, append(agentcore.MessageList(nil), llm.Messages...))
		return FakeStream()(ctx, model, llm, cfg)
	}
	runner := NewRunner(kern, kern, recording, "fake", "fake", "", discardLog())
	runner.ForkPrefixes = fakeForkPrefix{tenant: tenant, child: child, present: true, fact: sessionfork.Fact{
		ParentSessionID: parent, OriginSeq: parentInputSeq + 1,
	}}
	if _, err := runner.RunAttempt(ctx, childLease); err != nil {
		t.Fatalf("child attempt: %v", err)
	}
	if len(seen) == 0 {
		t.Fatal("the child never called the model")
	}
	first := seen[0]
	if len(first) != 3 {
		t.Fatalf("first request has %d messages, want parent user + parent answer + child input: %+v", len(first), first)
	}
	if got := agentcore.ContentToText(first[0].(agentcore.UserMessage).Content); got != "parent question" {
		t.Fatalf("prefix user = %q", got)
	}
	if first[1].Role() != agentcore.RoleAssistant {
		t.Fatalf("prefix second message role = %q", first[1].Role())
	}
	if got := agentcore.ContentToText(first[2].(agentcore.UserMessage).Content); got != "child task" {
		t.Fatalf("child input = %q", got)
	}

	// The child ledger holds its own input and answer only: the prefix is a
	// reference, not a copy.
	childEvents, err := kern.LoadEvents(ctx, childScope, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var assistant *kernel.Event
	for i := range childEvents {
		if childEvents[i].Kind == kernel.KindAssistantMessage {
			assistant = &childEvents[i]
		}
	}
	if assistant == nil {
		t.Fatal("the child produced no assistant message")
	}
	var payload struct {
		InputSeqs []int64 `json:"inputSeqs"`
	}
	if err := json.Unmarshal(assistant.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.InputSeqs) != 1 || payload.InputSeqs[0] != childInputSeq {
		t.Fatalf("child inputSeqs = %v, want only the child input %d", payload.InputSeqs, childInputSeq)
	}
	if got := len(childEvents); got != 2 {
		t.Fatalf("child ledger has %d events, want input + answer (no copied prefix)", got)
	}
	// And the parent log did not gain the child's input.
	after, err := kern.LoadEvents(ctx, parentScope, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(parentEvents) {
		t.Fatalf("parent log grew from %d to %d events", len(parentEvents), len(after))
	}
}

// TestCheckpointAnchorAccountsForForkPrefix pins P1: firstKeptIndex includes the
// inherited prefix but the committed child seqs do not. The anchor must subtract
// the prefix length (or refuse when the cut is inside it), never index child
// seqs with the prefix-inflated index.
func TestCheckpointAnchorAccountsForForkPrefix(t *testing.T) {
	ctx := context.Background()
	mem := kernel.NewMemStore()
	kern := kernel.New(mem, kernel.Options{})
	tenant, parent, child := uuid.New(), uuid.New(), uuid.New()
	for _, session := range []uuid.UUID{parent, child} {
		mem.CreateSession(tenant, session)
		mem.EnqueueRun(tenant, session)
	}
	parentScope := kernel.Scope{TenantID: tenant, SessionID: parent}
	childScope := kernel.Scope{TenantID: tenant, SessionID: child}
	parentSeq := commitInput(t, kern, parentScope, "parent-r1", "parent question", "", nil)
	for i, req := range []string{"child-r1", "child-r2", "child-r3"} {
		commitInput(t, kern, childScope, req, "child input "+string(rune('1'+i)), "", nil)
	}
	runner := NewRunner(kern, kern, FakeStream(), "fake", "fake", "", discardLog())
	runner.ForkPrefixes = fakeForkPrefix{tenant: tenant, child: child, present: true, fact: sessionfork.Fact{
		ParentSessionID: parent, OriginSeq: parentSeq,
	}}

	// firstKeptIndex 2 = prefix (1 message) + child message index 1 → seq 2.
	if got := runner.checkpointAnchor(ctx, childScope, 2, 4); got != 2 {
		t.Fatalf("anchor with prefix = %d, want child seq 2", got)
	}
	// firstKeptIndex 3 = child index 2 → seq 3.
	if got := runner.checkpointAnchor(ctx, childScope, 3, 4); got != 3 {
		t.Fatalf("anchor with prefix = %d, want child seq 3", got)
	}
	// A cut inside the prefix has no child anchor.
	if got := runner.checkpointAnchor(ctx, childScope, 1, 4); got != 0 {
		t.Fatalf("cut inside prefix anchor = %d, want 0", got)
	}
}

// TestForkCompactionAnchorsAndRestarts pins the full P1 path: a forked child
// compacts with the parent prefix in its context, the checkpoint anchors to a
// child event (not a prefix-inflated index), and a restart replays prefix +
// checkpoint + retained child tail.
func TestForkCompactionAnchorsAndRestarts(t *testing.T) {
	ctx := context.Background()
	mem := kernel.NewMemStore()
	kern := kernel.New(mem, kernel.Options{})
	tenant, parent, child := uuid.New(), uuid.New(), uuid.New()
	for _, session := range []uuid.UUID{parent, child} {
		mem.CreateSession(tenant, session)
		mem.EnqueueRun(tenant, session)
	}
	parentScope := kernel.Scope{TenantID: tenant, SessionID: parent}
	childScope := kernel.Scope{TenantID: tenant, SessionID: child}

	parentSeq := commitInput(t, kern, parentScope, "parent-r1", "parent question", "", nil)
	parentLease := claimScope(t, kern, tenant, parent)
	parentRunner := NewRunner(kern, kern, FakeStream(), "fake", "fake", "", discardLog())
	if _, err := parentRunner.RunAttempt(ctx, parentLease); err != nil {
		t.Fatalf("parent attempt: %v", err)
	}
	if err := kern.Finish(ctx, kernel.FinishRequest{Lease: parentLease, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish parent: %v", err)
	}

	prefix := fakeForkPrefix{tenant: tenant, child: child, present: true, fact: sessionfork.Fact{
		ParentSessionID: parent, OriginSeq: parentSeq + 1,
	}}
	commitInput(t, kern, childScope, "child-r1", strings.Repeat("child history BLUE. ", 300), "", nil)
	childLease := claimScope(t, kern, tenant, child)
	compactor := NewRunner(kern, kern, FakeStream(), "fake", "fake", "", discardLog())
	compactor.ForkPrefixes = prefix
	compactor.Compaction = &CompactionOptions{
		ContextWindow: 1000,
		Settings:      compaction.CompactionSettings{Enabled: true, ReserveTokens: 0, KeepRecentTokens: 1},
	}
	if _, err := compactor.RunAttempt(ctx, childLease); err != nil {
		t.Fatalf("child compaction attempt: %v", err)
	}
	if err := kern.Finish(ctx, kernel.FinishRequest{Lease: childLease, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish child: %v", err)
	}
	checkpoint, found, err := kern.LoadCheckpoint(ctx, childScope)
	if err != nil || !found {
		t.Fatalf("child checkpoint = (found=%v, err=%v), want found", found, err)
	}
	if checkpoint.KeptFromSeq <= 0 {
		t.Fatal("child checkpoint is unanchored")
	}
	if checkpoint.KeptFromSeq > checkpoint.UptoSeq {
		t.Fatalf("kept_from_seq = %d, upto_seq = %d", checkpoint.KeptFromSeq, checkpoint.UptoSeq)
	}

	commitInput(t, kern, childScope, "child-r2", "after restart", "", nil)
	requeueMem(t, mem, tenant, child)
	recorder := &checkpointRecordingStream{}
	restarted := NewRunner(kern, kern, recorder.stream, "fake", "fake", "", discardLog())
	restarted.ForkPrefixes = prefix
	restartLease := claimScope(t, kern, tenant, child)
	if _, err := restarted.RunAttempt(ctx, restartLease); err != nil {
		t.Fatalf("restart attempt: %v", err)
	}
	if err := kern.Finish(ctx, kernel.FinishRequest{Lease: restartLease, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish restart: %v", err)
	}
	msgs := recorder.last(t)
	if len(msgs) < 4 {
		t.Fatalf("restart transcript has %d messages: %+v", len(msgs), msgs)
	}
	if got := agentcore.ContentToText(msgs[0].(agentcore.UserMessage).Content); got != "parent question" {
		t.Fatalf("restart transcript does not start with the parent prefix: %q", got)
	}
	if msgs[1].Role() != agentcore.RoleAssistant {
		t.Fatalf("prefix second message role = %q", msgs[1].Role())
	}
	if _, ok := msgs[2].(agentcore.CompactionMessage); !ok {
		t.Fatalf("restart transcript at index 2 is %T, want the checkpoint summary", msgs[2])
	}
	if _, ok := firstUserMessageWith(msgs, "after restart"); !ok {
		t.Fatal("restart transcript lost the new input")
	}
	if _, ok := firstUserMessageWith(msgs, "child history BLUE"); ok {
		t.Fatal("restart replayed the summarized child input instead of the checkpoint")
	}
}

// requeueMem models new work on the in-memory queue.
func requeueMem(t *testing.T, mem *kernel.MemStore, tenant, session uuid.UUID) {
	t.Helper()
	mem.EnqueueRun(tenant, session)
}
