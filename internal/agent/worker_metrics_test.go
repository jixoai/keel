package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

type recordingRunObserver struct {
	mu           sync.Mutex
	started      int
	attempts     []string
	finished     []string
	finishErrors int
	leaseExpired int
}

func (o *recordingRunObserver) RunStarted() {
	o.mu.Lock()
	o.started++
	o.mu.Unlock()
}

func (o *recordingRunObserver) RunAttempt(outcome string, duration time.Duration) {
	o.mu.Lock()
	o.attempts = append(o.attempts, outcome)
	o.mu.Unlock()
}

func (o *recordingRunObserver) RunFinished(status string) {
	o.mu.Lock()
	o.finished = append(o.finished, status)
	o.mu.Unlock()
}

func (o *recordingRunObserver) FinishFailed() {
	o.mu.Lock()
	o.finishErrors++
	o.mu.Unlock()
}

func (o *recordingRunObserver) LeaseExpired() {
	o.mu.Lock()
	o.leaseExpired++
	o.mu.Unlock()
}

func (o *recordingRunObserver) snapshot() (int, []string, []string, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.started, append([]string(nil), o.attempts...), append([]string(nil), o.finished...), o.finishErrors
}

// newTestWorker builds a worker whose lease verbs go through the kernel and
// whose mirror reads go through the store.
func newTestWorker(k *kernel.Kernel, st *store.Store, runner *Runner, obs *recordingRunObserver) *Worker {
	return &Worker{
		Leases:      k,
		Runs:        st,
		Runner:      runner,
		Owner:       "worker-test",
		Lease:       30 * time.Second,
		MaxAttempts: 3,
		Logger:      discardLog(),
		Metrics:     obs,
	}
}

// claimForWorker claims the session the way Worker.Run does.
func claimForWorker(t *testing.T, k *kernel.Kernel, tenant, session uuid.UUID) kernel.Lease {
	t.Helper()
	lease, err := k.ClaimScope(context.Background(),
		kernel.Scope{TenantID: tenant, SessionID: session}, "worker-test", time.Minute)
	if err != nil {
		t.Fatalf("claim scope: %v", err)
	}
	return lease
}

// TestWorkerRetryThenSuccessCountsOnlyTerminalFinished drives the real worker
// through a failed attempt (row stays pending) and a successful retry. The
// observer must see attempts [pending, done] but only one terminal finished.
func TestWorkerRetryThenSuccessCountsOnlyTerminalFinished(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	ctx := context.Background()
	if _, err := st.AppendEvent(ctx, tenant, session, store.EventUserMessage, map[string]string{"text": "hello"}); err != nil {
		t.Fatal(err)
	}
	k := testKernel(t, st)

	// A kernel over a closed pool lets the first attempt fail at event load
	// without breaking the worker's kernel handle. This is a deterministic
	// stand-in for a transient store failure that leaves the row pending.
	broken := brokenKernel(t)
	runner := NewRunner(broken, broken, FakeStream(), "fake", "fake", "", discardLog())
	obs := &recordingRunObserver{}
	worker := newTestWorker(k, st, runner, obs)
	sem := make(chan struct{}, 1)
	// Drive execute directly: Claim is a global queue that parallel test
	// packages share, and this test only needs the Finish lifecycle.
	runOnce := func() {
		t.Helper()
		lease := claimForWorker(t, k, tenant, session)
		sem <- struct{}{}
		worker.execute(ctx, sem, lease)
	}

	runOnce()
	row, err := st.GetRun(ctx, tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != store.RunPending {
		t.Fatalf("after failed attempt status = %q, want pending", row.Status)
	}

	runner.Committer = k
	runner.Events = k
	runner.Reads = k
	runOnce()
	row, err = st.GetRun(ctx, tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != store.RunDone {
		t.Fatalf("after retry status = %q, want done", row.Status)
	}
	if row.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", row.Attempts)
	}

	started, attempts, finished, finishErrors := obs.snapshot()
	if started != 2 {
		t.Fatalf("started = %d, want 2", started)
	}
	if len(attempts) != 2 || attempts[0] != "pending" || attempts[1] != "done" {
		t.Fatalf("attempt outcomes = %v, want [pending done]", attempts)
	}
	if len(finished) != 1 || finished[0] != "done" {
		t.Fatalf("terminal outcomes = %v, want [done]", finished)
	}
	if finishErrors != 0 {
		t.Fatalf("finish errors = %d", finishErrors)
	}
}

func TestWorkerFinishErrorDoesNotRecordTerminal(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	ctx := context.Background()
	if _, err := st.AppendEvent(ctx, tenant, session, store.EventUserMessage, map[string]string{"text": "hello"}); err != nil {
		t.Fatal(err)
	}
	k := testKernel(t, st)
	lease := claimForWorker(t, k, tenant, session)

	// The runner commits through the healthy kernel, but the worker's lease
	// verbs go to a closed pool: Finish fails with a store error, not a fence
	// rejection, and the attempt is reported as finish_error.
	broken := brokenKernel(t)
	obs := &recordingRunObserver{}
	worker := newTestWorker(broken, st, NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog()), obs)
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	worker.execute(ctx, sem, lease)

	started, attempts, finished, finishErrors := obs.snapshot()
	if started != 1 || len(attempts) != 1 || attempts[0] != "finish_error" {
		t.Fatalf("started=%d attempts=%v, want one finish_error", started, attempts)
	}
	if len(finished) != 0 || finishErrors != 1 {
		t.Fatalf("finished=%v finishErrors=%d, want no terminal", finished, finishErrors)
	}
	row, err := st.GetRun(ctx, tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != store.RunRunning || row.Attempts != 1 {
		t.Fatalf("row mutated by failed finish: %+v", row)
	}
}

func TestWorkerAbandonedDoesNotRecordTerminal(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	ctx := context.Background()
	if _, err := st.AppendEvent(ctx, tenant, session, store.EventUserMessage, map[string]string{"text": "hello"}); err != nil {
		t.Fatal(err)
	}
	k := testKernel(t, st)
	lease := claimForWorker(t, k, tenant, session)

	obs := &recordingRunObserver{}
	worker := newTestWorker(k, st, NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog()), obs)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	worker.execute(canceled, sem, lease)

	started, attempts, finished, finishErrors := obs.snapshot()
	if started != 1 || len(attempts) != 1 || attempts[0] != "abandoned" {
		t.Fatalf("started=%d attempts=%v, want one abandoned", started, attempts)
	}
	if len(finished) != 0 || finishErrors != 0 {
		t.Fatalf("finished=%v finishErrors=%d, want no terminal", finished, finishErrors)
	}
	row, err := st.GetRun(ctx, tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != store.RunPending || row.Attempts != 1 || row.LeaseUntil != nil {
		// B3/N8: an abandoned attempt gives the claim back (status pending,
		// lease cleared) but must not write a terminal status or a failure.
		t.Fatalf("abandoned attempt must release the claim without a terminal status: %+v", row)
	}
}

// TestWorkerStaleLeaseAbandoned is the fencing case at the worker level: the
// session was handed over (new owner, higher epoch) while this worker still
// holds the old lease. Each commit is refused, the finish is refused, and the
// result is reported as abandoned — never as a terminal state.
func TestWorkerStaleLeaseAbandoned(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	ctx := context.Background()
	if _, err := st.AppendEvent(ctx, tenant, session, store.EventUserMessage, map[string]string{"text": "hello"}); err != nil {
		t.Fatal(err)
	}
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	stale := claimForWorker(t, k, tenant, session)
	// The handoff: the claim is given back and taken over by another owner.
	if err := k.Release(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := k.ClaimScope(ctx, scope, "runtime-new", time.Minute); err != nil {
		t.Fatal(err)
	}

	obs := &recordingRunObserver{}
	worker := newTestWorker(k, st, NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog()), obs)
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	worker.execute(ctx, sem, stale)

	started, attempts, finished, finishErrors := obs.snapshot()
	if started != 1 || len(attempts) != 1 || attempts[0] != "abandoned" {
		t.Fatalf("started=%d attempts=%v, want one abandoned", started, attempts)
	}
	if len(finished) != 0 || finishErrors != 0 {
		t.Fatalf("finished=%v finishErrors=%d, want no terminal", finished, finishErrors)
	}
	row, err := st.GetRun(ctx, tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != store.RunRunning || row.Attempts != 2 {
		t.Fatalf("stale attempt mutated row: %+v", row)
	}
	if got := countKind(eventsOf(t, st, tenant, session), store.EventAssistantMessage); got != 0 {
		t.Fatalf("stale writer appended %d assistant messages", got)
	}
}

// scopedLeases restricts the global claim to one session, so the shared test
// database cannot leak other packages' runs into this test. ClaimScope is the
// same claim path (session row lock, epoch bump) the global Claim uses.
type scopedLeases struct {
	kernel.LeaseManager
	scope kernel.Scope
}

func (s scopedLeases) Claim(ctx context.Context, owner string, ttl time.Duration) (kernel.Lease, error) {
	return s.LeaseManager.ClaimScope(ctx, s.scope, owner, ttl)
}

// TestWorkerClaimsThroughKernel drives Worker.Run through the kernel claim
// lifecycle: the run is claimed (epoch >= 1), executed through the kernel, and
// settled with the lease cleared.
func TestWorkerClaimsThroughKernel(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	ctx := context.Background()
	if _, err := st.AppendEvent(ctx, tenant, session, store.EventUserMessage, map[string]string{"text": "hello"}); err != nil {
		t.Fatal(err)
	}
	k := testKernel(t, st)
	worker := NewWorker(scopedLeases{LeaseManager: k, scope: kernel.Scope{TenantID: tenant, SessionID: session}},
		st, NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog()),
		"worker-claim-test", 4, discardLog())
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go worker.Run(wctx)

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if run, err := st.GetRun(ctx, tenant, session); err == nil && run.Status == store.RunDone {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()

	run, err := st.GetRun(ctx, tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunDone {
		t.Fatalf("run status = %q, want done", run.Status)
	}
	if run.Attempts < 1 {
		t.Fatalf("attempts = %d, want >= 1 (claimed through the kernel)", run.Attempts)
	}
	state, err := k.LeaseState(ctx, kernel.Scope{TenantID: tenant, SessionID: session})
	if err != nil {
		t.Fatal(err)
	}
	if state.Live {
		t.Fatalf("lease still live after finish: %+v", state)
	}
	if state.Lease.Token.Epoch < 1 {
		t.Fatalf("epoch = %d, want >= 1", state.Lease.Token.Epoch)
	}
	events := eventsOf(t, st, tenant, session)
	if got := countKind(events, store.EventUserMessage); got != 1 {
		t.Fatalf("user messages = %d, want 1", got)
	}
	if got := countKind(events, store.EventAssistantMessage); got != 1 {
		t.Fatalf("assistant messages = %d, want 1", got)
	}
}

// TestWorkerLeaseExpiredMetric exercises the Reclaimed signal the kernel
// carries: a claim that takes over a running row whose lease expired is counted.
func TestWorkerLeaseExpiredMetric(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	ctx := context.Background()
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	// A crashed owner: the run is running with an already-expired lease. The ttl
	// must be representable as a PostgreSQL interval (sub-millisecond ones are
	// not), so 20ms plus a short wait is the shortest portable expiry.
	expired, err := k.ClaimScope(ctx, scope, "runtime-dead", 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if expired.Reclaimed {
		t.Fatalf("first claim reported Reclaimed")
	}
	time.Sleep(60 * time.Millisecond)
	takeover, err := k.ClaimScope(ctx, scope, "runtime-alive", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !takeover.Reclaimed {
		t.Fatalf("takeover claim did not report Reclaimed: %+v", takeover)
	}
	if takeover.Token.Epoch != expired.Token.Epoch+1 {
		t.Fatalf("epoch = %d, want %d", takeover.Token.Epoch, expired.Token.Epoch+1)
	}
	if _, err := k.Renew(ctx, expired, time.Minute); !errors.Is(err, kernel.ErrStaleEpoch) {
		t.Fatalf("stale renew error = %v, want stale-epoch", err)
	}
}
