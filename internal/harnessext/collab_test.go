package harnessext

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/kernel"
)

func TestCollabSubagentLauncherLaunchAndCancel(t *testing.T) {
	ctx := context.Background()
	tenant, parent := uuid.New(), uuid.New()
	control := NewRunControl(nil, testLogger())
	runner := NewSubagentRunner(SubagentOptions{
		Gate:    toolGate{known: map[uuid.UUID]bool{parent: true}},
		Edge:    &toolCommitter{},
		Runs:    doneRuns{},
		Control: control,
		Logger:  testLogger(),
		// The Host already resolved the target Bot; the deployment's input edge
		// validator is still the seam the runner expects.
		ValidateSelection: func(json.RawMessage) error { return nil },
	})
	launcher := CollabSubagentLauncher{Runner: func() *SubagentRunner { return runner }}

	ref, err := launcher.Launch(ctx, collab.LaunchRequest{
		TenantID:        tenant,
		ParentSessionID: parent,
		BotID:           "worker",
		Task:            "do the work",
		IdempotencyKey:  "adapter-1",
		Budget:          collab.Budget{MaxTurns: 2, MaxTokens: 100, TimeoutMs: 500},
		Depth:           1,
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if ref.TenantID != tenant || ref.SessionID == uuid.Nil || ref.ParentSessionID != parent {
		t.Fatalf("session ref = %+v", ref)
	}
	if ref.ModeID != collab.ModeSubagent || ref.Depth != 1 || ref.State != collab.SessionActive {
		t.Fatalf("session ref identity = %+v", ref)
	}
	record, ok := runner.Get(ref.SessionID)
	if !ok || record.Task != "do the work" || record.ParentSessionID != parent.String() {
		t.Fatalf("runner record = %+v, ok=%v", record, ok)
	}

	// The same idempotency key targets the same child session.
	retry, err := launcher.Launch(ctx, collab.LaunchRequest{
		TenantID:        tenant,
		ParentSessionID: parent,
		BotID:           "worker",
		Task:            "do the work",
		IdempotencyKey:  "adapter-1",
		Depth:           1,
	})
	if err != nil {
		t.Fatalf("Launch retry: %v", err)
	}
	if retry.SessionID != ref.SessionID {
		t.Fatalf("idempotent retry session = %s, want %s", retry.SessionID, ref.SessionID)
	}

	// A session the runner never created has no parent link to cancel.
	if err := launcher.CancelSession(ctx, tenant, uuid.New(), "stop"); !errors.Is(err, collab.ErrSessionNotFound) {
		t.Fatalf("CancelSession(unknown) = %v, want ErrSessionNotFound", err)
	}

	runCtx, finish := control.BeginRun(ctx, kernel.Scope{TenantID: tenant, SessionID: ref.SessionID}, "run-1")
	cancelled := make(chan error, 1)
	go func() { cancelled <- launcher.CancelSession(ctx, tenant, ref.SessionID, "operator") }()
	select {
	case <-runCtx.Done():
	case <-ctx.Done():
		t.Fatal("cancel did not cancel the local run")
	}
	finish("cancelled")
	if err := <-cancelled; err != nil {
		t.Fatalf("CancelSession: %v", err)
	}
	record, ok = runner.Get(ref.SessionID)
	if !ok || record.Status != "cancelled" {
		t.Fatalf("record after cancel = %+v, ok=%v", record, ok)
	}
}

func TestCollabSubagentLauncherRequiresRunner(t *testing.T) {
	launcher := CollabSubagentLauncher{}
	if _, err := launcher.Launch(context.Background(), collab.LaunchRequest{}); !errors.Is(err, ErrNotWired) {
		t.Fatalf("Launch without runner = %v, want ErrNotWired", err)
	}
	if err := launcher.CancelSession(context.Background(), uuid.New(), uuid.New(), ""); !errors.Is(err, ErrNotWired) {
		t.Fatalf("CancelSession without runner = %v, want ErrNotWired", err)
	}
}
