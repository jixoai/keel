package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/supervisor"
)

// Crash/restart fixture (change keel-harness-v2, W12 G2).
//
// The test binary re-executes itself in two roles:
//
//   - precommit: claims the run, executes one real Runner attempt (which
//     commits the answer and the read watermark) and then sleeps without
//     settling. The parent SIGKILLs it, which is indistinguishable from a
//     crashed process.
//   - worker: runs the real agent.Worker. It waits for the dead lease to expire,
//     reclaims the same run and settles it normally.
//
// The assertions are the recovery contract: one answer (no duplicate final),
// one resume.attempt fact (lease_expired/adopted), one run.finished, contiguous
// seqs and a done run.
func TestRecoveryCrashHelper(t *testing.T) {
	mode := os.Getenv("KEEL_RECOVERY_HELPER")
	if mode == "" {
		t.Skip("crash helper process only")
	}
	dsn := strings.TrimSpace(os.Getenv("KEEL_TEST_DSN"))
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "helper: KEEL_TEST_DSN is required")
		os.Exit(2)
	}
	tenantID, err := uuid.Parse(os.Getenv("KEEL_RECOVERY_TENANT"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: tenant: %v\n", err)
		os.Exit(2)
	}
	sessionID, err := uuid.Parse(os.Getenv("KEEL_RECOVERY_SESSION"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: session: %v\n", err)
		os.Exit(2)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: open store: %v\n", err)
		os.Exit(2)
	}
	defer st.Close()
	pg, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: kernel pg: %v\n", err)
		os.Exit(2)
	}
	kern := kernel.New(pg, kernel.Options{})
	runner := NewRunner(kern, kern, FakeStream(), "fake", "fake", "", discardLog())
	runner.ConfigHash = "sha256:crash-fixture"
	runner.ConfigRevision = 9
	runner.Guardrails = func() supervisor.GuardrailPolicy { return supervisor.GuardrailPolicy{} }
	scope := kernel.Scope{TenantID: tenantID, SessionID: sessionID}

	switch mode {
	case "precommit":
		lease, err := kern.ClaimScope(ctx, scope, "crashing-attempt", 5*time.Second)
		if err != nil {
			fmt.Fprintf(os.Stderr, "helper: claim: %v\n", err)
			os.Exit(2)
		}
		if _, err := runner.RunAttempt(ctx, lease); err != nil {
			fmt.Fprintf(os.Stderr, "helper: attempt: %v\n", err)
			os.Exit(2)
		}
		fmt.Println("PRECOMMITTED")
		os.Stdout.Sync()
		select {} // killed -9 by the parent
	case "worker":
		worker := NewWorker(kern, st, runner, "recovering-worker", 1, discardLog())
		worker.Lease = 400 * time.Millisecond
		worker.Poll = 50 * time.Millisecond
		worker.MaxAttempts = 3
		worker.Committer = kern
		worker.Run(ctx)
	default:
		fmt.Fprintf(os.Stderr, "helper: unknown mode %q\n", mode)
		os.Exit(2)
	}
}

func TestCrashThenRestartResumesWithoutADuplicateFinal(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KEEL_TEST_DSN"))
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	st := testStore(t)
	tenant, session := newSession(t, st)
	ctx := context.Background()
	if _, err := st.AppendEvent(ctx, tenant, session, store.EventUserMessage, map[string]string{"text": "hello after crash"}); err != nil {
		t.Fatal(err)
	}
	helperEnv := append(os.Environ(),
		"KEEL_TEST_DSN="+dsn,
		"KEEL_RECOVERY_TENANT="+tenant.String(),
		"KEEL_RECOVERY_SESSION="+session.String(),
	)
	precommit := exec.Command(os.Args[0], "-test.run=^TestRecoveryCrashHelper$", "-test.v")
	precommit.Env = append(helperEnv, "KEEL_RECOVERY_HELPER=precommit")
	stdout, err := precommit.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	precommit.Stderr = os.Stderr
	if err := precommit.Start(); err != nil {
		t.Fatalf("start precommit helper: %v", err)
	}
	ready := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "PRECOMMITTED") {
				close(ready)
				return
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		_ = precommit.Process.Kill()
		t.Fatal("the precommit helper never committed its answer")
	}
	// SIGKILL is the crash: no defer runs, no finish, no release.
	if err := precommit.Process.Kill(); err != nil {
		t.Fatalf("kill precommit: %v", err)
	}
	_ = precommit.Wait()

	before := eventsOf(t, st, tenant, session)
	if got := countKind(before, store.EventAssistantMessage); got != 1 {
		t.Fatalf("assistant messages before restart = %d, want 1", got)
	}
	if got := countKind(before, store.EventRunFinished); got != 0 {
		t.Fatalf("run.finished before restart = %d, want 0", got)
	}
	run, err := st.GetRun(ctx, tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunRunning {
		t.Fatalf("run status after crash = %q, want running", run.Status)
	}

	worker := exec.Command(os.Args[0], "-test.run=^TestRecoveryCrashHelper$", "-test.v")
	worker.Env = append(helperEnv, "KEEL_RECOVERY_HELPER=worker")
	worker.Stderr = os.Stderr
	if err := worker.Start(); err != nil {
		t.Fatalf("start worker helper: %v", err)
	}
	defer func() {
		_ = worker.Process.Kill()
		_ = worker.Wait()
	}()

	deadline := time.Now().Add(45 * time.Second)
	for {
		run, err = st.GetRun(ctx, tenant, session)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == store.RunDone || run.Status == store.RunFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the restarted worker did not settle the run: %+v", run)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if run.Status != store.RunDone {
		t.Fatalf("run settled as %q, want done (last error %q)", run.Status, run.LastError)
	}

	after := eventsOf(t, st, tenant, session)
	if got := countKind(after, store.EventAssistantMessage); got != 1 {
		t.Fatalf("assistant messages after recovery = %d, want 1 (duplicate final)", got)
	}
	if got := countKind(after, store.EventRunFinished); got != 1 {
		t.Fatalf("run.finished events = %d, want 1", got)
	}
	var recovery []map[string]any
	for _, event := range after {
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
		recovery[0]["configHash"] != "sha256:crash-fixture" || recovery[0]["configRevision"] != float64(9) {
		t.Fatalf("recovery payload = %+v", recovery[0])
	}
	for i, event := range after {
		if event.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d, want %d (gap in the ledger)", i, event.Seq, i+1)
		}
	}
}
