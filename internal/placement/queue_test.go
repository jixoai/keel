package placement

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/kernel"
)

// estimator returns a per-session memory estimate from a map; an absent entry
// means "unknown" (non-positive), exactly what a real estimator signals when it
// has no observation.
func estimator(estimates map[uuid.UUID]int64) MemoryEstimator {
	return func(_ context.Context, _, sessionID uuid.UUID) (int64, error) {
		return estimates[sessionID], nil
	}
}

func memoryGroup(t *testing.T, svc *Service, tenant uuid.UUID, name string, maxRuns int, memoryLimit int64) Group {
	t.Helper()
	group, err := svc.CreateGroup(context.Background(), GroupSpec{
		TenantID: tenant, Name: name, MaxConcurrentRuns: maxRuns, MemoryLimitBytes: memoryLimit,
	})
	if err != nil {
		t.Fatalf("create group %s: %v", name, err)
	}
	return group
}

func claimInto(t *testing.T, svc *Service, tenant, session uuid.UUID) Placement {
	t.Helper()
	placement, err := svc.Claim(context.Background(), ClaimRequest{
		TenantID: tenant, SessionID: session, WorkerID: "w1", Strategy: Pack,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return placement
}

// TestPackMemoryBudgetChoosesHeadroom pins the pack strategy's memory packing:
// a session goes to the group with the smallest leftover that still fits
// (best fit), not to the first group and not round-robin.
func TestPackMemoryBudgetChoosesHeadroom(t *testing.T) {
	ctx := context.Background()
	estimates := map[uuid.UUID]int64{}
	svc, _ := memService(t, Options{Strategy: Pack, MemoryEstimator: estimator(estimates)})
	tenant := uuid.New()
	groupA := memoryGroup(t, svc, tenant, "pack-a", 10, 100)
	groupB := memoryGroup(t, svc, tenant, "pack-b", 10, 100)

	a, b, c, d, e := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for session, estimate := range map[uuid.UUID]int64{a: 60, b: 50, c: 30, d: 30, e: 30} {
		estimates[session] = estimate
	}
	cases := []struct {
		session uuid.UUID
		want    uuid.UUID
	}{
		{a, groupA.GroupID}, // empty tie -> name order
		{b, groupB.GroupID}, // A: 60+50 > 100
		{c, groupA.GroupID}, // A leftover 10, B leftover 20 -> best fit A
		{d, groupB.GroupID}, // A: 120, B: 80 -> B
	}
	for _, tc := range cases {
		placement := claimInto(t, svc, tenant, tc.session)
		if placement.GroupID != tc.want {
			t.Fatalf("session %s packed into %s, want %s", tc.session, placement.GroupID, tc.want)
		}
		if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: tc.session}); err != nil {
			t.Fatalf("admit %s: %v", tc.session, err)
		}
	}

	// No group has headroom for another 30 bytes: the pack strategy refuses at
	// claim time with the memory reason instead of inventing a place, and writes
	// no placement.
	_, err := svc.Claim(ctx, ClaimRequest{TenantID: tenant, SessionID: e, WorkerID: "w1", Strategy: Pack})
	var capacity *CapacityError
	if !errors.As(err, &capacity) || capacity.Reason != CapacityGroupMemory {
		t.Fatalf("claim with no headroom = %v, want %s", err, CapacityGroupMemory)
	}
	// The diagnostics describe the refused pack pool: 4 active runs (not two
	// groups), 170/200 bytes of estimates across the two 100-byte groups.
	if capacity.Active != 4 || capacity.UsedBytes != 170 || capacity.LimitBytes != 200 {
		t.Fatalf("capacity = %+v, want 4 active runs, 170/200 used bytes", capacity)
	}
	if _, err := svc.Store().GetPlacement(ctx, tenant, e); !errors.Is(err, ErrPlacementNotFound) {
		t.Fatalf("refused claim wrote a placement: %v", err)
	}
}

// TestPackChoosesGroupByHeadroomAtClaim is the placement half of the packing
// test: when no group fits the estimate, Claim fails with the same typed memory
// capacity error and creates no placement.
func TestPackChoosesGroupByHeadroomAtClaim(t *testing.T) {
	estimates := map[uuid.UUID]int64{}
	svc, store := memService(t, Options{Strategy: Pack, MemoryEstimator: estimator(estimates)})
	tenant := uuid.New()
	memoryGroup(t, svc, tenant, "pack-a", 10, 100)
	holder, newcomer := uuid.New(), uuid.New()
	estimates[holder] = 90
	estimates[newcomer] = 30
	claimInto(t, svc, tenant, holder)
	if _, err := svc.AdmitRun(context.Background(), AdmitRequest{TenantID: tenant, SessionID: holder}); err != nil {
		t.Fatalf("admit holder: %v", err)
	}
	_, err := svc.Claim(context.Background(), ClaimRequest{
		TenantID: tenant, SessionID: newcomer, WorkerID: "w1", Strategy: Pack,
	})
	var capacity *CapacityError
	if !errors.As(err, &capacity) || capacity.Reason != CapacityGroupMemory ||
		capacity.LimitBytes != 100 || capacity.UsedBytes != 90 || capacity.Active != 1 {
		t.Fatalf("claim = %v, want %s with 90/100 used bytes and one active run", err, CapacityGroupMemory)
	}
	if _, err := store.GetPlacement(context.Background(), tenant, newcomer); !errors.Is(err, ErrPlacementNotFound) {
		t.Fatalf("refused claim wrote a placement: %v", err)
	}
}

// TestPackWithoutEstimateIsUnsupported is the honesty guard: a memory-bounded
// pack group with no estimate fails typed (HTTP 501), never with a made-up
// number.
func TestPackWithoutEstimateIsUnsupported(t *testing.T) {
	svc, store := memService(t, Options{Strategy: Pack})
	tenant := uuid.New()
	placement, err := svc.Claim(context.Background(), ClaimRequest{
		TenantID: tenant, SessionID: uuid.New(), WorkerID: "w1", Strategy: Pack,
	})
	if err != nil || placement.GroupID == uuid.Nil {
		t.Fatalf("unbounded default pack claim = %+v err=%v, want a placement", placement, err)
	}

	boundedSvc, boundedStore := memService(t, Options{Strategy: Pack})
	boundedTenant := uuid.New()
	memoryGroup(t, boundedSvc, boundedTenant, "pack-bounded", 10, 100)
	boundedSession := uuid.New()
	_, err = boundedSvc.Claim(context.Background(), ClaimRequest{
		TenantID: boundedTenant, SessionID: boundedSession, WorkerID: "w1", Strategy: Pack,
	})
	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) || unsupported.Reason != ReasonMemoryEstimateUnavailable {
		t.Fatalf("bounded pack claim without an estimate = %v, want the typed unsupported failure", err)
	}
	if !IsUnsupported(err) {
		t.Fatal("IsUnsupported did not recognise the failure")
	}
	if _, err := boundedStore.GetPlacement(context.Background(), boundedTenant, boundedSession); !errors.Is(err, ErrPlacementNotFound) {
		t.Fatalf("unsupported claim wrote a placement: %v", err)
	}
	_ = store
}

// TestDefaultMemoryGroupWithoutEstimateCreatesNothing: the default pack group's
// memory limit is checked before the group is created, so an unsupported first
// claim does not leave an unusable group behind.
func TestDefaultMemoryGroupWithoutEstimateCreatesNothing(t *testing.T) {
	svc, store := memService(t, Options{Strategy: Pack, DefaultMemoryLimitBytes: 1 << 20})
	tenant := uuid.New()
	_, err := svc.Claim(context.Background(), ClaimRequest{
		TenantID: tenant, SessionID: uuid.New(), WorkerID: "w1", Strategy: Pack,
	})
	if !IsUnsupported(err) {
		t.Fatalf("default memory claim = %v, want unsupported", err)
	}
	groups, err := svc.ListGroups(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 0 || len(store.groups) != 0 {
		t.Fatalf("unsupported claim created groups: %+v", groups)
	}
}

// TestAdmissionMemoryCeilingIsHardRefusal: a group's memory budget is enforced
// at admission, with the used/limit bytes in the typed error.
func TestAdmissionMemoryCeilingIsHardRefusal(t *testing.T) {
	ctx := context.Background()
	estimates := map[uuid.UUID]int64{}
	svc, _ := memService(t, Options{MemoryEstimator: estimator(estimates)})
	tenant := uuid.New()
	group := memoryGroup(t, svc, tenant, "memory", 10, 100)
	first, second := uuid.New(), uuid.New()
	estimates[first], estimates[second] = 60, 60
	for _, session := range []uuid.UUID{first, second} {
		mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: first}); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	_, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: second})
	var capacity *CapacityError
	if !errors.As(err, &capacity) || capacity.Reason != CapacityGroupMemory {
		t.Fatalf("over-budget admit = %v, want %s", err, CapacityGroupMemory)
	}
	if capacity.LimitBytes != 100 || capacity.UsedBytes != 60 || capacity.Active != 1 {
		t.Fatalf("memory capacity = %+v, want 1 active run and 60/100 bytes", capacity)
	}
	// A smaller estimate fits.
	estimates[second] = 40
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: second}); err != nil {
		t.Fatalf("admit that fits: %v", err)
	}
}

// TestQueueMemoryBlocksPromotionWithoutJumpingFIFO: when a group's memory
// budget is lowered under a queue, the head waits (head-of-line) even though a
// later, smaller entry would fit.
func TestQueueMemoryBlocksPromotionWithoutJumpingFIFO(t *testing.T) {
	ctx := context.Background()
	estimates := map[uuid.UUID]int64{}
	svc, store := memService(t, Options{MemoryEstimator: estimator(estimates)})
	tenant := uuid.New()
	group := memoryGroup(t, svc, tenant, "memory-queue", 2, 100)
	holderA, holderB := uuid.New(), uuid.New()
	queuedC, queuedE, queuedD := uuid.New(), uuid.New(), uuid.New()
	estimates[holderA], estimates[holderB] = 30, 30
	estimates[queuedC], estimates[queuedE], estimates[queuedD] = 30, 30, 5
	for _, session := range []uuid.UUID{holderA, holderB, queuedC, queuedE, queuedD} {
		mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	}
	for _, session := range []uuid.UUID{holderA, holderB} {
		if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: session}); err != nil {
			t.Fatalf("admit %s: %v", session, err)
		}
	}
	for i, session := range []uuid.UUID{queuedC, queuedE, queuedD} {
		admission, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: session})
		if err != nil || !admission.Queued || admission.QueuePosition != i+1 {
			t.Fatalf("queue %s = %+v err=%v, want position %d", session, admission, err, i+1)
		}
	}

	// An administrator lowers the memory budget under the queue, then both
	// slots settle: C (30) fits 35, E (30) does not, and D (5) must not jump.
	store.mu.Lock()
	shrunk := store.groups[memGroupKey{tenant, group.GroupID}]
	shrunk.MemoryLimitBytes = 35
	store.groups[memGroupKey{tenant, group.GroupID}] = shrunk
	store.mu.Unlock()
	for _, session := range []uuid.UUID{holderA, holderB} {
		if err := store.SetRunStatus(tenant, session, RunStatusDone, 0); err != nil {
			t.Fatal(err)
		}
	}
	promoted, err := svc.PromoteQueued(ctx, tenant)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if len(promoted) != 1 || promoted[0].SessionID != queuedC {
		t.Fatalf("promoted = %+v, want only the head C", promoted)
	}
	stateE, err := svc.QueueState(ctx, tenant, queuedE)
	if err != nil || !stateE.Queued || stateE.Position != 1 {
		t.Fatalf("E state = %+v err=%v, want still queued at position 1", stateE, err)
	}
	stateD, err := svc.QueueState(ctx, tenant, queuedD)
	if err != nil || !stateD.Queued || stateD.Position != 2 {
		t.Fatalf("D state = %+v err=%v, want still queued at position 2 (no queue jumping)", stateD, err)
	}
}

// TestQueuedReAdmissionCannotJumpTheQueue is the FIFO guard for repeated input:
// a later queued run whose input arrives again must not be promoted ahead of an
// earlier queue head, even when a slot is free.
func TestQueuedReAdmissionCannotJumpTheQueue(t *testing.T) {
	ctx := context.Background()
	svc, store := memService(t, Options{})
	tenant := uuid.New()
	group := mustGroup(t, svc, tenant, "no-jump", 1)
	holder, first, second := uuid.New(), uuid.New(), uuid.New()
	for _, session := range []uuid.UUID{holder, first, second} {
		mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: holder}); err != nil {
		t.Fatal(err)
	}
	if admission, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: first}); err != nil || !admission.Queued {
		t.Fatalf("queue first = %+v err=%v", admission, err)
	}
	if admission, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: second}); err != nil || !admission.Queued {
		t.Fatalf("queue second = %+v err=%v", admission, err)
	}

	// The holder settles without promotion running (a crash window, then a new
	// input for the *second* queued session arrives).
	if err := store.SetRunStatus(tenant, holder, RunStatusDone, 0); err != nil {
		t.Fatal(err)
	}
	admission, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: second})
	if err != nil {
		t.Fatalf("re-admit second: %v", err)
	}
	if !admission.Queued || admission.QueuePosition != 1 {
		t.Fatalf("re-admitted second = %+v, want still queued at position 1", admission)
	}
	// The freed slot went to the earlier head, not to the re-admitted run.
	state, err := svc.QueueState(ctx, tenant, first)
	if err != nil || state.Queued {
		t.Fatalf("first after re-admission = %+v err=%v, want promoted", state, err)
	}
}

// TestPackUnboundedFallback pins the pack preference order: a bounded group
// that fits is denser than an unbounded one, so it wins; the unbounded group is
// the fallback when no bounded group has headroom.
func TestPackUnboundedFallback(t *testing.T) {
	ctx := context.Background()
	estimates := map[uuid.UUID]int64{}
	svc, _ := memService(t, Options{Strategy: Pack, MemoryEstimator: estimator(estimates)})
	tenant := uuid.New()
	bounded := memoryGroup(t, svc, tenant, "pack-a", 10, 100)
	open := memoryGroup(t, svc, tenant, "pack-open", 10, 0)
	first, second := uuid.New(), uuid.New()
	estimates[first], estimates[second] = 60, 50
	claimInto(t, svc, tenant, first)
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: first}); err != nil {
		t.Fatalf("admit first: %v", err)
	}
	claimInto(t, svc, tenant, second)
	// Re-derive the chosen groups from the placements.
	firstPlacement, err := svc.Store().GetPlacement(ctx, tenant, first)
	if err != nil {
		t.Fatal(err)
	}
	if firstPlacement.GroupID != bounded.GroupID {
		t.Fatalf("first pack group = %s, want the bounded group %s", firstPlacement.GroupID, bounded.GroupID)
	}
	secondPlacement, err := svc.Store().GetPlacement(ctx, tenant, second)
	if err != nil {
		t.Fatal(err)
	}
	if secondPlacement.GroupID != open.GroupID {
		t.Fatalf("second pack group = %s, want the unbounded fallback %s", secondPlacement.GroupID, open.GroupID)
	}
}

// TestQueuedInputSurvivesALoweredQuota covers the promise that a queued input
// is durable: once a run holds a queue place, new input for it is accepted even
// when the tenant quota or the group memory budget was lowered under it. Only a
// *new* reservation is refused by those ceilings.
func TestQueuedInputSurvivesALoweredQuota(t *testing.T) {
	ctx := context.Background()
	limit := 2
	estimates := map[uuid.UUID]int64{}
	svc, store := memService(t, Options{
		MemoryEstimator: estimator(estimates),
		TenantRunLimit:  func(context.Context, uuid.UUID) (int, error) { return limit, nil },
	})
	tenant := uuid.New()
	group := memoryGroup(t, svc, tenant, "lowered-queued", 1, 100)
	holder, waiting := uuid.New(), uuid.New()
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

	// Lower both ceilings under the queued run.
	limit = 1
	store.mu.Lock()
	shrunk := store.groups[memGroupKey{tenant, group.GroupID}]
	shrunk.MemoryLimitBytes = 60
	store.groups[memGroupKey{tenant, group.GroupID}] = shrunk
	store.mu.Unlock()

	again, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: waiting})
	if err != nil {
		t.Fatalf("re-admit queued under lowered ceilings: %v", err)
	}
	if !again.Queued || again.QueuePosition != 1 {
		t.Fatalf("re-admission = %+v, want the existing queue place", again)
	}
	// A new reservation is still refused by the hard ceilings.
	newcomer := uuid.New()
	estimates[newcomer] = 1
	mustJoin(t, svc, tenant, newcomer, group.GroupID, "w1", time.Minute)
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: newcomer}); !IsCapacity(err) {
		t.Fatalf("new admission under the lowered quota = %v, want a capacity refusal", err)
	}
}

// TestQueuedRunFollowsAPlacementMove: a queued row belongs to the group its
// placement names. If the placement moves while the run waits, the next
// admission re-homes the queued row to the new group's queue (positions only
// mean something inside one group) and promotes it there if capacity allows.
func TestQueuedRunFollowsAPlacementMove(t *testing.T) {
	ctx := context.Background()
	svc, _ := memService(t, Options{})
	tenant := uuid.New()
	groupA := mustGroup(t, svc, tenant, "move-a", 1)
	groupB := mustGroup(t, svc, tenant, "move-b", 1)
	holder, moving := uuid.New(), uuid.New()
	mustJoin(t, svc, tenant, holder, groupA.GroupID, "w1", time.Minute)
	mustJoin(t, svc, tenant, moving, groupA.GroupID, "w1", time.Minute)
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: holder}); err != nil {
		t.Fatal(err)
	}
	if admission, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: moving}); err != nil || !admission.Queued {
		t.Fatalf("queue moving = %+v err=%v", admission, err)
	}

	// The placement moves to an empty group (drain/expire + re-claim).
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
	state, err := svc.QueueState(ctx, tenant, moving)
	if err != nil || state.Queued {
		t.Fatalf("queue state after the move = %+v err=%v, want admitted", state, err)
	}
}

func TestQueuedRunMoveToMemoryBoundedGroupRefreshesEstimate(t *testing.T) {
	ctx := context.Background()
	estimates := map[uuid.UUID]int64{}
	svc, _ := memService(t, Options{MemoryEstimator: estimator(estimates)})
	tenant := uuid.New()
	groupA := mustGroup(t, svc, tenant, "move-unbounded", 1)
	groupB := memoryGroup(t, svc, tenant, "move-bounded", 1, 100)
	holder, moving := uuid.New(), uuid.New()
	estimates[holder], estimates[moving] = 10, 20
	mustJoin(t, svc, tenant, holder, groupA.GroupID, "w1", time.Minute)
	mustJoin(t, svc, tenant, moving, groupA.GroupID, "w1", time.Minute)
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: holder}); err != nil {
		t.Fatal(err)
	}
	queued, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: moving})
	if err != nil || !queued.Queued {
		t.Fatalf("queue moving = %+v err=%v", queued, err)
	}
	if queued.MemoryBytes != 0 {
		t.Fatalf("unbounded source memory = %d, want the zero estimate", queued.MemoryBytes)
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
		t.Fatalf("admission after the move = %+v, want it admitted in %s", admission, groupB.GroupID)
	}
	if admission.MemoryBytes != estimates[moving] {
		t.Fatalf("re-homed memory = %d, want the target group's estimate %d",
			admission.MemoryBytes, estimates[moving])
	}
}

func TestQueuedRunMoveToBoundedGroupWithoutEstimatorIsTyped(t *testing.T) {
	ctx := context.Background()
	svc, _ := memService(t, Options{}) // deliberately no estimator
	tenant := uuid.New()
	groupA := mustGroup(t, svc, tenant, "move-no-estimator-a", 1)
	groupB := memoryGroup(t, svc, tenant, "move-no-estimator-b", 1, 100)
	holder, moving := uuid.New(), uuid.New()
	mustJoin(t, svc, tenant, holder, groupA.GroupID, "w1", time.Minute)
	mustJoin(t, svc, tenant, moving, groupA.GroupID, "w1", time.Minute)
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: holder}); err != nil {
		t.Fatal(err)
	}
	if queued, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: moving}); err != nil || !queued.Queued {
		t.Fatalf("queue moving = %+v err=%v", queued, err)
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
	var unsupported *UnsupportedError
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: moving}); !errors.As(err, &unsupported) {
		t.Fatalf("re-admit without an estimator = %v, want a typed unsupported refusal", err)
	}
	if unsupported.Reason != ReasonMemoryEstimateUnavailable {
		t.Fatalf("unsupported reason = %q, want %q", unsupported.Reason, ReasonMemoryEstimateUnavailable)
	}
}

// TestRunSettledPromotesAndFeedsETA pins the settlement hook: a settled attempt
// promotes the FIFO head and its observed duration becomes the next ETA.
func TestRunSettledPromotesAndFeedsETA(t *testing.T) {
	ctx := context.Background()
	svc, store := memService(t, Options{})
	tenant := uuid.New()
	group := mustGroup(t, svc, tenant, "settle", 1)
	holder, waiting, next := uuid.New(), uuid.New(), uuid.New()
	for _, session := range []uuid.UUID{holder, waiting, next} {
		mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	}
	gate := memGate(t, svc, "runtime-1", newTestSessions(true).state)
	if _, err := gate.Admit(ctx, tenant, holder); err != nil {
		t.Fatal(err)
	}
	if admission, err := gate.Admit(ctx, tenant, waiting); err != nil || !admission.Queued {
		t.Fatalf("queue waiting = %+v err=%v", admission, err)
	}

	// The kernel settles the attempt; the worker's settlement hook runs.
	if err := store.SetRunStatus(tenant, holder, RunStatusDone, 0); err != nil {
		t.Fatal(err)
	}
	gate.RunSettled(ctx, kernel.Scope{TenantID: tenant, SessionID: holder}, "done", 7*time.Second)

	state, err := svc.QueueState(ctx, tenant, waiting)
	if err != nil || state.Queued {
		t.Fatalf("waiting after settlement = %+v err=%v, want promoted", state, err)
	}
	// The freed slot is now held by the promoted run; the next input queues and
	// its ETA comes from the one observed 7s attempt.
	queued, err := gate.Admit(ctx, tenant, next)
	if err != nil || !queued.Queued {
		t.Fatalf("queue next = %+v err=%v", queued, err)
	}
	if !queued.ETA.Known || queued.ETA.Wait != 7*time.Second || queued.ETA.Samples != 1 || queued.ETA.Basis != "group" {
		t.Fatalf("ETA = %+v, want known 7s from the observed attempt", queued.ETA)
	}
}

// TestSweepQueuesConvergesACrashLeftQueue: a settlement whose hook never ran
// (process crash) still converges on the next recovery sweep.
func TestSweepQueuesConvergesACrashLeftQueue(t *testing.T) {
	ctx := context.Background()
	svc, store := memService(t, Options{})
	tenant := uuid.New()
	group := mustGroup(t, svc, tenant, "crash", 1)
	holder, waiting := uuid.New(), uuid.New()
	for _, session := range []uuid.UUID{holder, waiting} {
		mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	}
	gate := memGate(t, svc, "runtime-1", newTestSessions(true).state)
	if _, err := gate.Admit(ctx, tenant, holder); err != nil {
		t.Fatal(err)
	}
	if admission, err := gate.Admit(ctx, tenant, waiting); err != nil || !admission.Queued {
		t.Fatalf("queue waiting = %+v err=%v", admission, err)
	}
	// The holder's row goes terminal without the settlement hook (crash window).
	if err := store.SetRunStatus(tenant, holder, RunStatusDone, 0); err != nil {
		t.Fatal(err)
	}
	promoted, err := gate.SweepQueues(ctx)
	if err != nil || len(promoted) != 1 || promoted[0].SessionID != waiting {
		t.Fatalf("sweep = %+v err=%v, want the waiting run promoted", promoted, err)
	}
	// A second sweep is a no-op (nothing is promoted twice).
	if promoted, err := gate.SweepQueues(ctx); err != nil || len(promoted) != 0 {
		t.Fatalf("second sweep = %+v err=%v, want none", promoted, err)
	}
}

// TestQueuedHeadGetsAFreeSlotBeforeANewAdmission is the crash/reconfigure
// fairness guard: a queued head must be promoted into the free slot before a
// new admission can take it. The holder's row is terminal without the
// settlement hook (the crash window), so the slot is free while the queue is
// not empty; admitting the newcomer first would silently jump the queue.
func TestQueuedHeadGetsAFreeSlotBeforeANewAdmission(t *testing.T) {
	ctx := context.Background()
	svc, store := memService(t, Options{})
	tenant := uuid.New()
	group := mustGroup(t, svc, tenant, "fifo-window", 1)
	holder, head, newcomer := uuid.New(), uuid.New(), uuid.New()
	for _, session := range []uuid.UUID{holder, head, newcomer} {
		mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: holder}); err != nil {
		t.Fatalf("admit holder: %v", err)
	}
	if admission, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: head}); err != nil || !admission.Queued {
		t.Fatalf("queue head = %+v err=%v", admission, err)
	}
	// The crash/reconfigure window: the holder is terminal, but nobody promoted
	// the queue yet, so a slot is free while `head` still waits.
	if err := store.SetRunStatus(tenant, holder, RunStatusDone, 0); err != nil {
		t.Fatal(err)
	}
	admission, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: newcomer})
	if err != nil {
		t.Fatalf("admit newcomer: %v", err)
	}
	if !admission.Queued || admission.QueuePosition != 1 {
		t.Fatalf("newcomer = %+v, want queued behind the head", admission)
	}
	headRun := store.runs[memSessionKey{tenant, head}]
	if headRun.status != RunStatusPending {
		t.Fatalf("queued head = %q, want promoted to pending before the newcomer", headRun.status)
	}
	newcomerRun := store.runs[memSessionKey{tenant, newcomer}]
	if newcomerRun.status != RunStatusQueued {
		t.Fatalf("newcomer = %q, want queued (it must not take the head's slot)", newcomerRun.status)
	}
}

// TestRunSettledOnlySamplesCompletedAttempts defines the ETA sample rule: only
// a completed ("done") attempt feeds the average. Abandoned, failed,
// cancelled, finish_error and superseded attempts free the slot (promotion
// still runs) but do not distort the advertised wait.
func TestRunSettledOnlySamplesCompletedAttempts(t *testing.T) {
	ctx := context.Background()
	svc, store := memService(t, Options{})
	tenant := uuid.New()
	group := mustGroup(t, svc, tenant, "eta-rule", 1)
	session := uuid.New()
	mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: session}); err != nil {
		t.Fatal(err)
	}
	gate := memGate(t, svc, "runtime-1", newTestSessions(true).state)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}
	key := memGroupKey{tenant, group.GroupID}

	for _, status := range []string{"abandoned", "finish_error", "failed", "cancelled", "superseded"} {
		gate.RunSettled(ctx, scope, status, 3*time.Second)
		if samples := store.groupSamples[key]; len(samples) != 0 {
			t.Fatalf("%s attempt added an ETA sample: %v", status, samples)
		}
	}
	gate.RunSettled(ctx, scope, kernel.RunSummaryDone, 9*time.Second)
	samples := store.groupSamples[key]
	if len(samples) != 1 || samples[0] != 9*time.Second {
		t.Fatalf("completed attempt samples = %v, want exactly the 9s observation", samples)
	}
}
