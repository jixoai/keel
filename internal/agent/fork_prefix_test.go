package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
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
