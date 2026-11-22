package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/supervisor"
)

func TestGuardrailMemoryPressureIsAuditedInTheLedger(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	ctx := context.Background()
	runner, kern := newTestRunner(t, st)
	runner.Guardrails = func() supervisor.GuardrailPolicy {
		return supervisor.GuardrailPolicy{MaxMemoryMB: 1}
	}
	if _, err := st.AppendEvent(ctx, tenant, session, store.EventUserMessage, map[string]string{"text": "hello"}); err != nil {
		t.Fatal(err)
	}
	_, runErr := execRun(t, kern, runner, tenant, session)
	if !errors.Is(runErr, ErrGuardrailAborted) {
		t.Fatalf("run error = %v, want ErrGuardrailAborted", runErr)
	}
	var tripped []map[string]any
	for _, event := range eventsOf(t, st, tenant, session) {
		if event.Kind != store.EventGuardrailTripped {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		tripped = append(tripped, payload)
	}
	if len(tripped) != 1 {
		t.Fatalf("guardrail facts = %d, want 1", len(tripped))
	}
	if tripped[0]["trigger"] != GuardrailMemoryPressure || tripped[0]["outcome"] != "run_aborted" ||
		tripped[0]["actor"] != "supervisor" {
		t.Fatalf("payload = %+v", tripped[0])
	}
	run, err := st.GetRun(ctx, tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	// The trip is deterministic: the attempt failed with the guardrail error.
	// Whether the run row retries (pending) or is exhausted (failed) is the
	// worker's attempt policy, not the guardrail's decision.
	if run.Status == store.RunDone {
		t.Fatalf("guardrail trip settled the run as done: %+v", run)
	}
	if !strings.Contains(run.LastError, "guardrail") {
		t.Fatalf("run last error = %q, want the guardrail error", run.LastError)
	}
}

func TestReclaimedLeaseRecordsTheRecoveryFact(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	ctx := context.Background()
	runner, kern := newTestRunner(t, st)
	runner.ConfigHash = "sha256:recovery"
	runner.ConfigRevision = 4
	if _, err := st.AppendEvent(ctx, tenant, session, store.EventUserMessage, map[string]string{"text": "hello"}); err != nil {
		t.Fatal(err)
	}
	scope := kernel.Scope{TenantID: tenant, SessionID: session}
	first, err := kern.ClaimScope(ctx, scope, "crashed-runtime", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if first.Reclaimed {
		t.Fatal("first claim reported a reclaim")
	}
	time.Sleep(80 * time.Millisecond)
	second, err := kern.ClaimScope(ctx, scope, "successor-runtime", time.Minute)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if !second.Reclaimed {
		t.Fatal("the successor claim did not report the expired lease")
	}
	result, runErr := runner.RunAttempt(ctx, second)
	if runErr != nil {
		t.Fatalf("RunAttempt: %v", runErr)
	}
	if err := kern.Finish(ctx, kernel.FinishRequest{
		Lease: second, Cursor: result.Cursor, ReadThrough: result.ReadThrough, MaxAttempts: 3,
	}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	var recovery []map[string]any
	for _, event := range eventsOf(t, st, tenant, session) {
		if event.Kind != store.EventResumeAttempt {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		recovery = append(recovery, payload)
	}
	if len(recovery) != 1 {
		t.Fatalf("recovery facts = %d, want 1", len(recovery))
	}
	if recovery[0]["reason"] != "lease_expired" || recovery[0]["result"] != "adopted" ||
		recovery[0]["counted"] != true || recovery[0]["owner"] != "successor-runtime" ||
		recovery[0]["configHash"] != "sha256:recovery" || recovery[0]["configRevision"] != float64(4) {
		t.Fatalf("recovery payload = %+v", recovery[0])
	}
	// The attempt itself still produced exactly one answer.
	if got := countKind(eventsOf(t, st, tenant, session), store.EventAssistantMessage); got != 1 {
		t.Fatalf("assistant messages = %d, want 1", got)
	}
}
