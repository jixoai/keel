package placement

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/kernel"
	keelstore "github.com/jixoai/keel/internal/store"
)

func pgMemoryService(t *testing.T, st *keelstore.Store, estimator MemoryEstimator) *Service {
	t.Helper()
	svc, err := New(Options{Store: NewPGStore(st.Pool()), MemoryEstimator: estimator})
	if err != nil {
		t.Fatalf("new memory service: %v", err)
	}
	return svc
}

// TestPGQueueFIFOAndPromotionOnRelease pins the durable FIFO order and the
// release path on the real database: the head becomes pending, the next one
// moves to position 1, and the promoted row is the one the kernel may claim.
func TestPGQueueFIFOAndPromotionOnRelease(t *testing.T) {
	st := pgPlacementStore(t)
	svc := pgService(t, st, 0)
	ctx := context.Background()
	tenant := pgTenant(t, st)
	group := mustGroup(t, svc, tenant, "pg-queue-fifo", 1)
	holder, second, third := pgSession(t, st, tenant), pgSession(t, st, tenant), pgSession(t, st, tenant)
	for _, session := range []uuid.UUID{holder, second, third} {
		mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: holder}); err != nil {
		t.Fatalf("admit holder: %v", err)
	}
	q2, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: second})
	if err != nil || !q2.Queued || q2.QueuePosition != 1 {
		t.Fatalf("queue second = %+v err=%v, want position 1", q2, err)
	}
	q3, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: third})
	if err != nil || !q3.Queued || q3.QueuePosition != 2 {
		t.Fatalf("queue third = %+v err=%v, want position 2", q3, err)
	}

	// Release frees the unstarted holder and promotes the head in one
	// transaction.
	if err := svc.ReleaseRun(ctx, tenant, holder); err != nil {
		t.Fatalf("release holder: %v", err)
	}
	if status := pgRunStatus(t, st, tenant, holder); status != RunStatusDone {
		t.Fatalf("holder after release = %q, want done", status)
	}
	if status := pgRunStatus(t, st, tenant, second); status != RunStatusPending {
		t.Fatalf("second after release = %q, want pending (promoted)", status)
	}
	state, err := svc.QueueState(ctx, tenant, third)
	if err != nil || !state.Queued || state.Position != 1 {
		t.Fatalf("third queue state = %+v err=%v, want queued at position 1", state, err)
	}
	// Promotion happens once: the second row is no longer queued, so a repeated
	// sweep has nothing to promote.
	promoted, err := svc.PromoteQueued(ctx, tenant)
	if err != nil || len(promoted) != 0 {
		t.Fatalf("second promotion = %+v err=%v, want none", promoted, err)
	}
}

// TestPGQueuedRunIsNeverClaimable is the execution guard: the kernel's claim
// predicate must not match a queued row, and promotion is what makes it
// claimable.
func TestPGQueuedRunIsNeverClaimable(t *testing.T) {
	st := pgPlacementStore(t)
	svc := pgService(t, st, 0)
	ctx := context.Background()
	tenant := pgTenant(t, st)
	group := mustGroup(t, svc, tenant, "pg-queue-claim", 1)
	holder, waiting := pgSession(t, st, tenant), pgSession(t, st, tenant)
	for _, session := range []uuid.UUID{holder, waiting} {
		mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	}
	kernLog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatalf("kernel migrate: %v", err)
	}
	kern := kernel.New(kernLog, kernel.Options{})
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: holder}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: waiting}); err != nil {
		t.Fatal(err)
	}

	scope := kernel.Scope{TenantID: tenant, SessionID: waiting}
	if _, err := kern.ClaimScope(ctx, scope, "w-claim", 30*time.Second); !errors.Is(err, kernel.ErrNoRunnable) {
		t.Fatalf("claim of a queued run = %v, want ErrNoRunnable", err)
	}
	// The holder's attempt settles; promotion makes the queued row claimable.
	if err := svc.ReleaseRun(ctx, tenant, holder); err != nil {
		t.Fatalf("release holder: %v", err)
	}
	lease, err := kern.ClaimScope(ctx, scope, "w-claim", 30*time.Second)
	if err != nil {
		t.Fatalf("claim after promotion: %v", err)
	}
	if lease.Scope != scope {
		t.Fatalf("lease scope = %v, want %v", lease.Scope, scope)
	}
}

// TestPGQueuedReAdmissionCannotJumpTheQueue is the FIFO guard on the database:
// a repeated input for a later queued run promotes the earlier head instead.
func TestPGQueuedReAdmissionCannotJumpTheQueue(t *testing.T) {
	st := pgPlacementStore(t)
	svc := pgService(t, st, 0)
	ctx := context.Background()
	tenant := pgTenant(t, st)
	group := mustGroup(t, svc, tenant, "pg-no-jump", 1)
	holder, first, second := pgSession(t, st, tenant), pgSession(t, st, tenant), pgSession(t, st, tenant)
	for _, session := range []uuid.UUID{holder, first, second} {
		mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: holder}); err != nil {
		t.Fatal(err)
	}
	for _, session := range []uuid.UUID{first, second} {
		if admission, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: session}); err != nil || !admission.Queued {
			t.Fatalf("queue %s = %+v err=%v", session, admission, err)
		}
	}
	// The holder settles without a promotion pass; then the later queued run
	// receives new input while a slot is free.
	if _, err := st.Pool().Exec(ctx,
		`UPDATE runs SET status = 'done', lease_until = NULL WHERE tenant_id = $1 AND session_id = $2`,
		tenant, holder); err != nil {
		t.Fatal(err)
	}
	admission, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: second})
	if err != nil {
		t.Fatalf("re-admit second: %v", err)
	}
	if !admission.Queued || admission.QueuePosition != 1 {
		t.Fatalf("re-admitted second = %+v, want still queued at position 1", admission)
	}
	if status := pgRunStatus(t, st, tenant, first); status != RunStatusPending {
		t.Fatalf("first after re-admission = %q, want pending (the head keeps FIFO)", status)
	}
}

// TestPGUserMessageCommitKeepsAQueuedRunQueued is the integration guard for the
// two writers that force a run to pending: a user.message commit (and the
// store's repair enqueue) must leave a queued row queued, because promotion, not
// the wake, is what makes it runnable.
func TestPGUserMessageCommitKeepsAQueuedRunQueued(t *testing.T) {
	st := pgPlacementStore(t)
	svc := pgService(t, st, 0)
	ctx := context.Background()
	tenant := pgTenant(t, st)
	group := mustGroup(t, svc, tenant, "pg-commit-queue", 1)
	holder, waiting := pgSession(t, st, tenant), pgSession(t, st, tenant)
	for _, session := range []uuid.UUID{holder, waiting} {
		mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: holder}); err != nil {
		t.Fatal(err)
	}
	if admission, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: waiting}); err != nil || !admission.Queued {
		t.Fatalf("queue waiting = %+v err=%v", admission, err)
	}
	kernLog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatalf("kernel: %v", err)
	}
	kern := kernel.New(kernLog, kernel.Options{})
	if _, err := kern.Commit(ctx, kernel.CommitRequest{
		Scope:      kernel.Scope{TenantID: tenant, SessionID: waiting},
		Kind:       kernel.KindUserMessage,
		RequestID:  "queued-input-1",
		Payload:    []byte(`{"text":"queued input"}`),
		NativeEdge: true,
	}); err != nil {
		t.Fatalf("commit user.message: %v", err)
	}
	if status := pgRunStatus(t, st, tenant, waiting); status != RunStatusQueued {
		t.Fatalf("run after user.message = %q, want queued (the commit must not bypass promotion)", status)
	}
	var pendingInput bool
	if err := st.Pool().QueryRow(ctx,
		`SELECT pending_input FROM runs WHERE tenant_id = $1 AND session_id = $2`,
		tenant, waiting).Scan(&pendingInput); err != nil {
		t.Fatal(err)
	}
	if !pendingInput {
		t.Fatal("the committed input is not marked pending_input")
	}
	// The HTTP commit path's repair enqueue runs after the commit; it must not
	// turn the queued row into a claimable pending row either.
	if err := st.EnqueueRun(ctx, tenant, waiting); err != nil {
		t.Fatalf("repair enqueue: %v", err)
	}
	if status := pgRunStatus(t, st, tenant, waiting); status != RunStatusQueued {
		t.Fatalf("run after the repair enqueue = %q, want queued", status)
	}
	// The input is durable and the run is promoted when the slot frees.
	if err := svc.ReleaseRun(ctx, tenant, holder); err != nil {
		t.Fatal(err)
	}
	if status := pgRunStatus(t, st, tenant, waiting); status != RunStatusPending {
		t.Fatalf("run after promotion = %q, want pending", status)
	}
}

// TestPGQueuedInputSurvivesALoweredQuota covers the durable-input promise on
// the database: a queued run's new input is accepted even after the tenant
// quota and the group memory budget are lowered under it.
func TestPGQueuedInputSurvivesALoweredQuota(t *testing.T) {
	st := pgPlacementStore(t)
	ctx := context.Background()
	limit := 2
	estimates := map[uuid.UUID]int64{}
	svc, err := New(Options{
		Store:           NewPGStore(st.Pool()),
		MemoryEstimator: estimator(estimates),
		TenantRunLimit:  func(context.Context, uuid.UUID) (int, error) { return limit, nil },
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	tenant := pgTenant(t, st)
	group := memoryGroup(t, svc, tenant, "pg-lowered-queued", 1, 100)
	holder, waiting := pgSession(t, st, tenant), pgSession(t, st, tenant)
	estimates[holder], estimates[waiting] = 60, 30
	for _, session := range []uuid.UUID{holder, waiting} {
		mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: holder}); err != nil {
		t.Fatal(err)
	}
	queued, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: waiting})
	if err != nil || !queued.Queued {
		t.Fatalf("queue waiting = %+v err=%v", queued, err)
	}

	limit = 1
	if _, err := st.Pool().Exec(ctx,
		`UPDATE groups SET memory_limit_bytes = 60 WHERE tenant_id = $1 AND group_id = $2`,
		tenant, group.GroupID); err != nil {
		t.Fatal(err)
	}
	again, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: waiting})
	if err != nil {
		t.Fatalf("re-admit queued under lowered ceilings: %v", err)
	}
	if !again.Queued || again.QueuePosition != 1 {
		t.Fatalf("re-admission = %+v, want the existing queue place", again)
	}
	// A new reservation is still refused by the hard ceilings (the tenant quota
	// is checked first and is spent).
	newcomer := pgSession(t, st, tenant)
	estimates[newcomer] = 1
	mustJoin(t, svc, tenant, newcomer, group.GroupID, "w1", time.Minute)
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: newcomer}); !IsCapacity(err) {
		t.Fatalf("new admission under the lowered quota = %v, want a capacity refusal", err)
	}
}

// TestPGQueuedRunFollowsAPlacementMove: a queued row is re-homed to the group
// its placement now names, and promoted there when capacity allows.
func TestPGQueuedRunFollowsAPlacementMove(t *testing.T) {
	st := pgPlacementStore(t)
	svc := pgService(t, st, 0)
	ctx := context.Background()
	tenant := pgTenant(t, st)
	groupA := mustGroup(t, svc, tenant, "pg-move-a", 1)
	groupB := mustGroup(t, svc, tenant, "pg-move-b", 1)
	holder, moving := pgSession(t, st, tenant), pgSession(t, st, tenant)
	mustJoin(t, svc, tenant, holder, groupA.GroupID, "w1", time.Minute)
	mustJoin(t, svc, tenant, moving, groupA.GroupID, "w1", time.Minute)
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: holder}); err != nil {
		t.Fatal(err)
	}
	if admission, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: moving}); err != nil || !admission.Queued {
		t.Fatalf("queue moving = %+v err=%v", admission, err)
	}
	current, err := svc.Store().GetPlacement(ctx, tenant, moving)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Leave(ctx, LeaveRequest{
		TenantID: tenant, SessionID: moving, WorkerID: current.WorkerID, ExpectedEpoch: current.Epoch,
	}); err != nil {
		t.Fatalf("leave: %v", err)
	}
	mustJoin(t, svc, tenant, moving, groupB.GroupID, "w2", time.Minute)
	admission, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: moving})
	if err != nil {
		t.Fatalf("re-admit after the move: %v", err)
	}
	if admission.GroupID != groupB.GroupID || admission.Queued {
		t.Fatalf("admission after the move = %+v, want a run admitted in %s", admission, groupB.GroupID)
	}
	if status := pgRunStatus(t, st, tenant, moving); status != RunStatusPending {
		t.Fatalf("moved run status = %q, want pending", status)
	}
	var groupText string
	if err := st.Pool().QueryRow(ctx,
		`SELECT group_id::text FROM runs WHERE tenant_id = $1 AND session_id = $2`,
		tenant, moving).Scan(&groupText); err != nil {
		t.Fatal(err)
	}
	if groupText != groupB.GroupID.String() {
		t.Fatalf("moved run group = %s, want %s", groupText, groupB.GroupID)
	}
}

// TestPGQueueETAKnownFromObservedDurations pins the ETA on the database: the
// group window comes from recorded attempts, and no history stays unknown.
func TestPGQueueETAKnownFromObservedDurations(t *testing.T) {
	st := pgPlacementStore(t)
	svc := pgService(t, st, 0)
	ctx := context.Background()
	tenant := pgTenant(t, st)
	group := mustGroup(t, svc, tenant, "pg-eta", 1)
	holder, waiting := pgSession(t, st, tenant), pgSession(t, st, tenant)
	for _, session := range []uuid.UUID{holder, waiting} {
		mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: holder}); err != nil {
		t.Fatal(err)
	}
	before, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: waiting})
	if err != nil || before.ETA.Known {
		t.Fatalf("ETA before observation = %+v err=%v, want unknown", before.ETA, err)
	}
	for i := 0; i < 3; i++ {
		if err := svc.ObserveRunDuration(ctx, tenant, holder, 4*time.Second); err != nil {
			t.Fatalf("observe duration: %v", err)
		}
	}
	state, err := svc.QueueState(ctx, tenant, waiting)
	if err != nil {
		t.Fatal(err)
	}
	if !state.ETA.Known || state.ETA.Wait != 4*time.Second || state.ETA.Samples != 3 || state.ETA.Basis != "group" {
		t.Fatalf("ETA = %+v, want known 4s from 3 group samples", state.ETA)
	}
	// Samples are pruned to the retention bound; the ETA still reads a window.
	for i := 0; i < ETASampleRetention+10; i++ {
		if err := svc.ObserveRunDuration(ctx, tenant, holder, time.Duration(i+1)*time.Second); err != nil {
			t.Fatalf("observe duration: %v", err)
		}
	}
	if n := pgCount(t, st, `SELECT count(*) FROM run_duration_samples
	                        WHERE tenant_id = $1 AND group_id = $2`, tenant, group.GroupID); n > ETASampleRetention {
		t.Fatalf("retained samples = %d, want <= %d", n, ETASampleRetention)
	}
}

// TestPGQueueNeverBypassesTheTenantQuota: with the tenant quota spent, the
// second run is a hard 429 and leaves no queued row behind.
func TestPGQueueNeverBypassesTheTenantQuota(t *testing.T) {
	st := pgPlacementStore(t)
	svc := pgService(t, st, 1)
	ctx := context.Background()
	tenant := pgTenant(t, st)
	group := mustGroup(t, svc, tenant, "pg-queue-quota", 1)
	first, second := pgSession(t, st, tenant), pgSession(t, st, tenant)
	for _, session := range []uuid.UUID{first, second} {
		mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: first}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: second})
	var capacity *CapacityError
	if !errors.As(err, &capacity) || capacity.Reason != CapacityTenant {
		t.Fatalf("spent-quota admission = %v, want the tenant refusal", err)
	}
	if n := pgCount(t, st, `SELECT count(*) FROM runs
	                        WHERE tenant_id = $1 AND session_id = $2 AND status = 'queued'`,
		tenant, second); n != 0 {
		t.Fatalf("tenant quota refusal wrote %d queued rows, want 0", n)
	}
}

// TestPGPackMemoryBudgetAcrossGroups pins the pack strategy on real group rows:
// a session goes to the group with the smallest leftover, and the estimate is
// recorded on the run row so the next packing decision sees it.
func TestPGPackMemoryBudgetAcrossGroups(t *testing.T) {
	st := pgPlacementStore(t)
	estimates := map[uuid.UUID]int64{}
	svc := pgMemoryService(t, st, estimator(estimates))
	ctx := context.Background()
	tenant := pgTenant(t, st)
	memoryGroup(t, svc, tenant, "pack-a", 10, 100)
	memoryGroup(t, svc, tenant, "pack-b", 10, 100)
	a, b, c := pgSession(t, st, tenant), pgSession(t, st, tenant), pgSession(t, st, tenant)
	estimates[a], estimates[b], estimates[c] = 60, 50, 30
	for _, session := range []uuid.UUID{a, b, c} {
		claimInto(t, svc, tenant, session)
		if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: session}); err != nil {
			t.Fatalf("admit %s: %v", session, err)
		}
	}
	var groupA, groupB uuid.UUID
	for _, group := range mustListGroups(t, svc, tenant) {
		switch group.Name {
		case "pack-a":
			groupA = group.GroupID
		case "pack-b":
			groupB = group.GroupID
		}
	}
	if groupA == uuid.Nil || groupB == uuid.Nil {
		t.Fatalf("pack groups missing: %s/%s", groupA, groupB)
	}
	// a (60) lands in pack-a; b (50) cannot fit pack-a and lands in pack-b; c (30)
	// leaves pack-a with 10 bytes and pack-b with 20, so best fit picks pack-a.
	if used := pgMemoryUsed(t, st, tenant, groupA); used != 90 {
		t.Fatalf("pack-a memory = %d, want 90 (60 + 30)", used)
	}
	if used := pgMemoryUsed(t, st, tenant, groupB); used != 50 {
		t.Fatalf("pack-b memory = %d, want 50", used)
	}
}

func pgMemoryUsed(t *testing.T, st *keelstore.Store, tenant, group uuid.UUID) int64 {
	t.Helper()
	var used int64
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT COALESCE(sum(memory_bytes), 0) FROM runs
		  WHERE tenant_id = $1 AND group_id = $2 AND status IN ('pending', 'running')`,
		tenant, group).Scan(&used); err != nil {
		t.Fatalf("sum group memory: %v", err)
	}
	return used
}

// TestPGMemoryEstimateUnavailableOnAdmission is the typed failure on the real
// database: a memory-bounded group without an estimator refuses, writes nothing,
// and never invents a budget.
func TestPGMemoryEstimateUnavailableOnAdmission(t *testing.T) {
	st := pgPlacementStore(t)
	svc := pgService(t, st, 0)
	ctx := context.Background()
	tenant := pgTenant(t, st)
	group := memoryGroup(t, svc, tenant, "pg-memory-unsupported", 2, 64<<20)
	session := pgSession(t, st, tenant)
	mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	_, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: session})
	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) || unsupported.Reason != ReasonMemoryEstimateUnavailable {
		t.Fatalf("memory admission without an estimate = %v, want the typed unsupported failure", err)
	}
	if n := pgCount(t, st, `SELECT count(*) FROM runs WHERE tenant_id = $1 AND session_id = $2`,
		tenant, session); n != 0 {
		t.Fatalf("unsupported admission wrote %d run rows, want 0", n)
	}
}

func mustListGroups(t *testing.T, svc *Service, tenant uuid.UUID) []Group {
	t.Helper()
	groups, err := svc.ListGroups(context.Background(), tenant)
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	return groups
}
