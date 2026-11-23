package placement

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func memService(t *testing.T, opts Options) (*Service, *MemStore) {
	t.Helper()
	store := NewMemStore(nil)
	opts.Store = store
	svc, err := New(opts)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc, store
}

func mustGroup(t *testing.T, svc *Service, tenant uuid.UUID, name string, maxRuns int) Group {
	t.Helper()
	group, err := svc.CreateGroup(context.Background(), GroupSpec{
		TenantID: tenant, Name: name, MaxConcurrentRuns: maxRuns,
	})
	if err != nil {
		t.Fatalf("create group %s: %v", name, err)
	}
	return group
}

func mustJoin(t *testing.T, svc *Service, tenant, session, group uuid.UUID, worker string, ttl time.Duration) Placement {
	t.Helper()
	placement, err := svc.Join(context.Background(), JoinRequest{
		TenantID: tenant, SessionID: session, GroupID: &group, WorkerID: worker, TTL: ttl,
	})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	return placement
}

// TestStrategyResolution pins pack/user/pin: pack and user create their named
// group on demand, pin refuses to guess a group.
func TestStrategyResolution(t *testing.T) {
	ctx := context.Background()
	svc, _ := memService(t, Options{Strategy: Pack})

	packTenant := uuid.New()
	packSession := uuid.New()
	placement, err := svc.Claim(ctx, ClaimRequest{
		TenantID: packTenant, SessionID: packSession, WorkerID: "w1",
	})
	if err != nil {
		t.Fatalf("pack claim: %v", err)
	}
	group, err := svc.GetGroup(ctx, packTenant, placement.GroupID)
	if err != nil {
		t.Fatalf("get pack group: %v", err)
	}
	if group.Name != PackDefaultGroupName {
		t.Fatalf("pack group name = %q, want %q", group.Name, PackDefaultGroupName)
	}

	userTenant := uuid.New()
	placement, err = svc.Claim(ctx, ClaimRequest{
		TenantID: userTenant, SessionID: uuid.New(), Strategy: User, WorkerID: "w1",
	})
	if err != nil {
		t.Fatalf("user claim: %v", err)
	}
	group, err = svc.GetGroup(ctx, userTenant, placement.GroupID)
	if err != nil {
		t.Fatalf("get user group: %v", err)
	}
	if group.Name != UserGroupName {
		t.Fatalf("user group name = %q, want %q", group.Name, UserGroupName)
	}

	_, err = svc.Claim(ctx, ClaimRequest{
		TenantID: uuid.New(), SessionID: uuid.New(), Strategy: Pin, WorkerID: "w1",
	})
	var conflict *Conflict
	if !errors.As(err, &conflict) || conflict.Reason != ReasonPinRequiresGroup {
		t.Fatalf("pin without group = %v, want %s", err, ReasonPinRequiresGroup)
	}

	pinTenant := uuid.New()
	pinGroup := mustGroup(t, svc, pinTenant, "pinned", 0)
	placement, err = svc.Claim(ctx, ClaimRequest{
		TenantID: pinTenant, SessionID: uuid.New(), Strategy: Pin,
		GroupID: &pinGroup.GroupID, WorkerID: "w1",
	})
	if err != nil {
		t.Fatalf("pin claim: %v", err)
	}
	if placement.GroupID != pinGroup.GroupID {
		t.Fatalf("pin placement group = %s, want %s", placement.GroupID, pinGroup.GroupID)
	}

	// The pin target may also come from configuration (Options.PinGroupID).
	configuredID := uuid.New()
	cfgSvc, _ := memService(t, Options{Strategy: Pin, PinGroupID: &configuredID})
	cfgTenant := uuid.New()
	if _, err := cfgSvc.CreateGroup(ctx, GroupSpec{
		TenantID: cfgTenant, GroupID: configuredID, Name: "configured-pin",
	}); err != nil {
		t.Fatalf("create configured pin group: %v", err)
	}
	configured, err := cfgSvc.Claim(ctx, ClaimRequest{
		TenantID: cfgTenant, SessionID: uuid.New(), WorkerID: "w1",
	})
	if err != nil {
		t.Fatalf("configured pin claim: %v", err)
	}
	if configured.GroupID != configuredID {
		t.Fatalf("configured pin group = %s, want %s", configured.GroupID, configuredID)
	}
}

// TestClaimEpochCASAndLiveLease pins the lease write: the epoch is a
// compare-and-set, a live lease belongs to its holder, and an expired lease is
// adoptable.
func TestClaimEpochCASAndLiveLease(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	store := NewMemStore(func() time.Time { return now })
	svc, err := New(Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	group := mustGroup(t, svc, tenant, "cas", 0)
	session := uuid.New()

	first := mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	if first.Epoch != 1 {
		t.Fatalf("first epoch = %d, want 1", first.Epoch)
	}

	_, err = svc.Claim(ctx, ClaimRequest{TenantID: tenant, SessionID: session,
		GroupID: &group.GroupID, WorkerID: "w2", ExpectedEpoch: 0})
	if conflict := asConflict(t, err); conflict.Reason != ReasonEpochMismatch || conflict.CurrentEpoch != 1 {
		t.Fatalf("claim expected 0 = %+v, want epoch_mismatch current 1", conflict)
	}

	_, err = svc.Claim(ctx, ClaimRequest{TenantID: tenant, SessionID: session,
		GroupID: &group.GroupID, WorkerID: "w2", ExpectedEpoch: 1})
	if conflict := asConflict(t, err); conflict.Reason != ReasonLeaseHeld || conflict.Holder != "w1" {
		t.Fatalf("live lease takeover = %+v, want lease_held by w1", conflict)
	}
	// No group in the request means "keep the current group"; the same live
	// lease still refuses the other worker (the strategy is not consulted).
	_, err = svc.Claim(ctx, ClaimRequest{TenantID: tenant, SessionID: session,
		WorkerID: "w2", ExpectedEpoch: 1})
	if conflict := asConflict(t, err); conflict.Reason != ReasonLeaseHeld {
		t.Fatalf("no-group claim against live lease = %+v, want lease_held", conflict)
	}

	renewed, err := svc.Claim(ctx, ClaimRequest{TenantID: tenant, SessionID: session,
		GroupID: &group.GroupID, WorkerID: "w1", ExpectedEpoch: 1, TTL: time.Minute})
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if renewed.Epoch != 2 {
		t.Fatalf("renewed epoch = %d, want 2", renewed.Epoch)
	}

	_, err = svc.Claim(ctx, ClaimRequest{TenantID: tenant, SessionID: session,
		GroupID: &group.GroupID, WorkerID: "w1", ExpectedEpoch: 1})
	if conflict := asConflict(t, err); conflict.Reason != ReasonEpochMismatch || conflict.CurrentEpoch != 2 {
		t.Fatalf("stale claim = %+v, want epoch_mismatch current 2", conflict)
	}

	now = now.Add(2 * time.Minute)
	taken, err := svc.Claim(ctx, ClaimRequest{TenantID: tenant, SessionID: session,
		GroupID: &group.GroupID, WorkerID: "w2", ExpectedEpoch: 2})
	if err != nil {
		t.Fatalf("expired lease takeover: %v", err)
	}
	if taken.WorkerID != "w2" || taken.Epoch != 3 {
		t.Fatalf("takeover = %+v, want w2 epoch 3", taken)
	}
}

func asConflict(t *testing.T, err error) *Conflict {
	t.Helper()
	var conflict *Conflict
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want *Conflict", err)
	}
	return conflict
}

// TestConcurrentClaimsHaveOneWinner is the two-group race: thirty-two claims
// for the same unplaced session (half naming group A, half group B) must
// produce exactly one placement. The losers get a typed conflict, and a
// second run cannot be admitted for the session.
func TestConcurrentClaimsHaveOneWinner(t *testing.T) {
	ctx := context.Background()
	svc, _ := memService(t, Options{})
	tenant := uuid.New()
	groupA := mustGroup(t, svc, tenant, "race-a", 0)
	groupB := mustGroup(t, svc, tenant, "race-b", 0)
	session := uuid.New()

	var wins, conflicts atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			group := groupA.GroupID
			if i%2 == 1 {
				group = groupB.GroupID
			}
			_, err := svc.Claim(ctx, ClaimRequest{
				TenantID: tenant, SessionID: session, GroupID: &group,
				WorkerID: "worker-" + uuid.NewString(),
			})
			if err == nil {
				wins.Add(1)
				return
			}
			var conflict *Conflict
			if errors.As(err, &conflict) {
				conflicts.Add(1)
				return
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("untyped claim error: %v", err)
	}
	if wins.Load() != 1 || conflicts.Load() != 31 {
		t.Fatalf("claim race wins=%d conflicts=%d, want 1/31", wins.Load(), conflicts.Load())
	}
	placement, err := svc.Store().GetPlacement(ctx, tenant, session)
	if err != nil {
		t.Fatalf("read winner: %v", err)
	}
	if placement.GroupID != groupA.GroupID && placement.GroupID != groupB.GroupID {
		t.Fatalf("winner group %s is neither candidate", placement.GroupID)
	}

	// The session has one placement, and admits exactly one active run.
	admission, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: session})
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if admission.ActiveTenant != 1 || admission.AlreadyActive {
		t.Fatalf("first admission = %+v, want one new active run", admission)
	}
	again, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: session})
	if err != nil {
		t.Fatalf("second admit: %v", err)
	}
	if !again.AlreadyActive || again.ActiveTenant != 1 {
		t.Fatalf("second admission = %+v, want already-active with one run", again)
	}
}

// TestJoinIdempotentAndMoveNeedsExpiry pins join semantics: the same worker
// re-joining is a renewal, a live lease in another group is AlreadyPlaced, and
// an expired lease allows the move.
func TestJoinIdempotentAndMoveNeedsExpiry(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	store := NewMemStore(func() time.Time { return now })
	svc, err := New(Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	groupA := mustGroup(t, svc, tenant, "join-a", 0)
	groupB := mustGroup(t, svc, tenant, "join-b", 0)
	session := uuid.New()

	first := mustJoin(t, svc, tenant, session, groupA.GroupID, "w1", time.Minute)
	rejoin := mustJoin(t, svc, tenant, session, groupA.GroupID, "w1", time.Minute)
	if rejoin.GroupID != groupA.GroupID || rejoin.Epoch != first.Epoch+1 {
		t.Fatalf("rejoin = %+v, want same group epoch %d", rejoin, first.Epoch+1)
	}

	_, err = svc.Join(ctx, JoinRequest{TenantID: tenant, SessionID: session,
		GroupID: &groupB.GroupID, WorkerID: "w1", TTL: time.Minute})
	if conflict := asConflict(t, err); conflict.Reason != ReasonAlreadyPlaced {
		t.Fatalf("live move = %+v, want already_placed", conflict)
	}

	now = now.Add(2 * time.Minute)
	moved := mustJoin(t, svc, tenant, session, groupB.GroupID, "w2", time.Minute)
	if moved.GroupID != groupB.GroupID || moved.WorkerID != "w2" || moved.Epoch != rejoin.Epoch+1 {
		t.Fatalf("expired move = %+v, want group B w2 epoch %d", moved, rejoin.Epoch+1)
	}
}

// TestLeaveEpochGuard pins leave: the epoch and holder are compare-and-set,
// and a successful leave removes the placement fact entirely.
func TestLeaveEpochGuard(t *testing.T) {
	ctx := context.Background()
	svc, _ := memService(t, Options{})
	tenant := uuid.New()
	group := mustGroup(t, svc, tenant, "leave", 0)
	session := uuid.New()
	placed := mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)

	_, err := svc.Leave(ctx, LeaveRequest{TenantID: tenant, SessionID: session,
		WorkerID: "w2", ExpectedEpoch: placed.Epoch})
	if conflict := asConflict(t, err); conflict.Reason != ReasonEpochMismatch {
		t.Fatalf("leave by non-holder = %+v, want epoch_mismatch", conflict)
	}
	_, err = svc.Leave(ctx, LeaveRequest{TenantID: tenant, SessionID: session,
		WorkerID: "w1", ExpectedEpoch: placed.Epoch + 5})
	if conflict := asConflict(t, err); conflict.Reason != ReasonEpochMismatch {
		t.Fatalf("leave with stale epoch = %+v, want epoch_mismatch", conflict)
	}
	left, err := svc.Leave(ctx, LeaveRequest{TenantID: tenant, SessionID: session,
		WorkerID: "w1", ExpectedEpoch: placed.Epoch})
	if err != nil {
		t.Fatalf("leave: %v", err)
	}
	if left.GroupID != group.GroupID || left.Epoch != placed.Epoch {
		t.Fatalf("leave returned %+v, want the removed fact", left)
	}
	if _, err := svc.Store().GetPlacement(ctx, tenant, session); !errors.Is(err, ErrPlacementNotFound) {
		t.Fatalf("placement still present after leave: %v", err)
	}
	if _, err := svc.Leave(ctx, LeaveRequest{TenantID: tenant, SessionID: session,
		WorkerID: "w1", ExpectedEpoch: placed.Epoch}); !errors.Is(err, ErrPlacementNotFound) {
		t.Fatalf("second leave = %v, want ErrPlacementNotFound", err)
	}
}

// TestAdoptFollowsKernelEpoch pins adopt: lower epochs are stale, equal epochs
// renew, and a group change is only allowed once the old lease expired.
func TestAdoptFollowsKernelEpoch(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	store := NewMemStore(func() time.Time { return now })
	svc, err := New(Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	groupA := mustGroup(t, svc, tenant, "adopt-a", 0)
	groupB := mustGroup(t, svc, tenant, "adopt-b", 0)
	session := uuid.New()

	first, err := svc.Adopt(ctx, AdoptRequest{TenantID: tenant, SessionID: session,
		GroupID: &groupA.GroupID, WorkerID: "w1", Epoch: 5, TTL: time.Minute})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if first.Epoch != 5 {
		t.Fatalf("adopted epoch = %d, want 5", first.Epoch)
	}

	_, err = svc.Adopt(ctx, AdoptRequest{TenantID: tenant, SessionID: session,
		GroupID: &groupA.GroupID, WorkerID: "w1", Epoch: 4, TTL: time.Minute})
	if conflict := asConflict(t, err); conflict.Reason != ReasonEpochMismatch {
		t.Fatalf("lower epoch = %+v, want epoch_mismatch", conflict)
	}
	_, err = svc.Adopt(ctx, AdoptRequest{TenantID: tenant, SessionID: session,
		GroupID: &groupA.GroupID, WorkerID: "w2", Epoch: 5, TTL: time.Minute})
	if conflict := asConflict(t, err); conflict.Reason != ReasonLeaseHeld {
		t.Fatalf("live adopt by other = %+v, want lease_held", conflict)
	}

	now = now.Add(2 * time.Minute)
	renewed, err := svc.Adopt(ctx, AdoptRequest{TenantID: tenant, SessionID: session,
		GroupID: &groupA.GroupID, WorkerID: "w2", Epoch: 5, TTL: time.Minute})
	if err != nil {
		t.Fatalf("expired adopt: %v", err)
	}
	if renewed.Epoch != 5 || renewed.WorkerID != "w2" {
		t.Fatalf("expired adopt = %+v, want epoch 5 by w2", renewed)
	}
	now = now.Add(2 * time.Minute)
	moved, err := svc.Adopt(ctx, AdoptRequest{TenantID: tenant, SessionID: session,
		GroupID: &groupB.GroupID, WorkerID: "w2", Epoch: 7, TTL: time.Minute})
	if err != nil {
		t.Fatalf("adopt move: %v", err)
	}
	if moved.GroupID != groupB.GroupID || moved.Epoch != 7 {
		t.Fatalf("adopt move = %+v, want group B epoch 7", moved)
	}
}

// TestSeekPlan pins the migration plan: no move, drain flag from the live
// lease, next epoch, and a full-ledger rebuild.
func TestSeekPlan(t *testing.T) {
	ctx := context.Background()
	svc, _ := memService(t, Options{})
	tenant := uuid.New()
	groupA := mustGroup(t, svc, tenant, "seek-a", 0)
	groupB := mustGroup(t, svc, tenant, "seek-b", 0)
	session := uuid.New()
	placed := mustJoin(t, svc, tenant, session, groupA.GroupID, "w1", time.Minute)

	plan, err := svc.Seek(ctx, SeekRequest{TenantID: tenant, SessionID: session,
		ToGroupID: groupB.GroupID, ExpectedEpoch: placed.Epoch})
	if err != nil {
		t.Fatalf("seek: %v", err)
	}
	if !plan.DrainRequired || plan.CopyProcessState || plan.NextEpoch != placed.Epoch+1 ||
		plan.From.GroupID != groupA.GroupID || plan.To.GroupID != groupB.GroupID {
		t.Fatalf("plan = %+v", plan)
	}
	current, err := svc.Store().GetPlacement(ctx, tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	if current.GroupID != groupA.GroupID || current.Epoch != placed.Epoch {
		t.Fatalf("seek mutated the placement: %+v", current)
	}
	_, err = svc.Seek(ctx, SeekRequest{TenantID: tenant, SessionID: session,
		ToGroupID: groupB.GroupID, ExpectedEpoch: placed.Epoch + 9})
	if conflict := asConflict(t, err); conflict.Reason != ReasonEpochMismatch {
		t.Fatalf("stale seek = %+v, want epoch_mismatch", conflict)
	}
	if _, err := svc.SetGroupStatus(ctx, tenant, groupB.GroupID, StatusDraining); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Seek(ctx, SeekRequest{TenantID: tenant, SessionID: session,
		ToGroupID: groupB.GroupID, ExpectedEpoch: placed.Epoch})
	if conflict := asConflict(t, err); conflict.Reason != ReasonGroupDraining {
		t.Fatalf("seek to draining = %+v, want group_draining", conflict)
	}
}

// TestDrainingGroupRenewsButTakesNoNewWork pins drain semantics in the memory
// store: an existing placement may renew, a new placement or run is refused.
func TestDrainingGroupRenewsButTakesNoNewWork(t *testing.T) {
	ctx := context.Background()
	svc, _ := memService(t, Options{})
	tenant := uuid.New()
	group := mustGroup(t, svc, tenant, "drain-mem", 0)
	existing := uuid.New()
	placed := mustJoin(t, svc, tenant, existing, group.GroupID, "w1", time.Minute)
	if _, err := svc.SetGroupStatus(ctx, tenant, group.GroupID, StatusDraining); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Claim(ctx, ClaimRequest{TenantID: tenant, SessionID: existing,
		GroupID: &group.GroupID, WorkerID: "w1", ExpectedEpoch: placed.Epoch}); err != nil {
		t.Fatalf("renewal in draining group: %v", err)
	}
	_, err := svc.Join(ctx, JoinRequest{TenantID: tenant, SessionID: uuid.New(),
		GroupID: &group.GroupID, WorkerID: "w2"})
	if conflict := asConflict(t, err); conflict.Reason != ReasonGroupDraining {
		t.Fatalf("new placement in draining group = %+v, want group_draining", conflict)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: existing}); err == nil {
		t.Fatal("run admitted into a draining group")
	}
}

// TestFullGroupQueuesFIFOWithPosition pins the queue contract: a full group
// admits the run into a durable FIFO entry (no slot) with a 1-based position
// and an unknown ETA until durations are observed; releasing the holder
// promotes the head, in arrival order, exactly once.
func TestFullGroupQueuesFIFOWithPosition(t *testing.T) {
	ctx := context.Background()
	svc, store := memService(t, Options{})
	tenant := uuid.New()
	group := mustGroup(t, svc, tenant, "slots", 1)
	holder, second, third := uuid.New(), uuid.New(), uuid.New()
	for _, session := range []uuid.UUID{holder, second, third} {
		mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	}
	_, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: uuid.New()})
	if conflict := asConflict(t, err); conflict.Reason != ReasonPlacementNotFound {
		t.Fatalf("admit for an unplaced session = %+v, want placement_not_found", conflict)
	}
	first, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: holder})
	if err != nil {
		t.Fatalf("first admit: %v", err)
	}
	if first.Queued {
		t.Fatalf("first admission = %+v, want a slot", first)
	}
	if err := store.SetRunStatus(tenant, holder, RunStatusRunning, 2*time.Second); err != nil {
		t.Fatal(err)
	}

	queuedSecond, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: second})
	if err != nil {
		t.Fatalf("queue second: %v", err)
	}
	if !queuedSecond.Queued || queuedSecond.QueuePosition != 1 || queuedSecond.ActiveGroup != 1 {
		t.Fatalf("second admission = %+v, want queued at position 1 with one active", queuedSecond)
	}
	if queuedSecond.ETA.Known || queuedSecond.ETA.Wait != 0 {
		t.Fatalf("ETA = %+v, want unknown with no observed durations (never a zero estimate)", queuedSecond.ETA)
	}
	queuedThird, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: third})
	if err != nil {
		t.Fatalf("queue third: %v", err)
	}
	if !queuedThird.Queued || queuedThird.QueuePosition != 2 {
		t.Fatalf("third admission = %+v, want queued at position 2", queuedThird)
	}

	// A repeated input on a queued session keeps its place (idempotent queueing).
	again, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: second})
	if err != nil {
		t.Fatalf("re-admit queued: %v", err)
	}
	if !again.Queued || again.QueuePosition != 1 || again.Created {
		t.Fatalf("re-admission = %+v, want queued at position 1 and not created", again)
	}

	// The kernel settles the holder (a terminal settlement, observed through the
	// promotion path): the head is promoted, in FIFO order, and counts again.
	if err := store.SetRunStatus(tenant, holder, RunStatusDone, 0); err != nil {
		t.Fatal(err)
	}
	promoted, err := svc.PromoteQueued(ctx, tenant)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if len(promoted) != 1 || promoted[0].SessionID != second {
		t.Fatalf("promoted = %+v, want exactly the FIFO head (%s)", promoted, second)
	}
	state, err := svc.QueueState(ctx, tenant, second)
	if err != nil || state.Queued {
		t.Fatalf("second queue state = %+v err=%v, want promoted", state, err)
	}
	state, err = svc.QueueState(ctx, tenant, third)
	if err != nil || !state.Queued || state.Position != 1 {
		t.Fatalf("third queue state = %+v err=%v, want queued at position 1", state, err)
	}
	// Promotion is idempotent: a second sweep has nothing to promote.
	promoted, err = svc.PromoteQueued(ctx, tenant)
	if err != nil || len(promoted) != 0 {
		t.Fatalf("second promotion = %+v err=%v, want none (the slot is taken)", promoted, err)
	}
}

// TestQueueETAKnownFromObservedDurations pins the ETA source: the wait comes
// only from observed attempt durations (group window, then tenant fallback) and
// stays unknown with no history.
func TestQueueETAKnownFromObservedDurations(t *testing.T) {
	ctx := context.Background()
	svc, _ := memService(t, Options{})
	tenant := uuid.New()
	group := mustGroup(t, svc, tenant, "eta", 1)
	holder, waiting := uuid.New(), uuid.New()
	for _, session := range []uuid.UUID{holder, waiting} {
		mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: holder}); err != nil {
		t.Fatalf("admit holder: %v", err)
	}
	before, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: waiting})
	if err != nil {
		t.Fatalf("queue waiting: %v", err)
	}
	if before.ETA.Known {
		t.Fatalf("ETA before any observation = %+v, want unknown", before.ETA)
	}

	// Four observed 10s attempts give the group a 10s average; position 1 in a
	// one-slot group is one wave, so the ETA is 10s.
	for i := 0; i < 4; i++ {
		if err := svc.ObserveRunDuration(ctx, tenant, holder, 10*time.Second); err != nil {
			t.Fatalf("observe duration: %v", err)
		}
	}
	state, err := svc.QueueState(ctx, tenant, waiting)
	if err != nil {
		t.Fatal(err)
	}
	if !state.ETA.Known || state.ETA.Wait != 10*time.Second || state.ETA.Basis != "group" || state.ETA.Samples != 4 {
		t.Fatalf("ETA = %+v, want known 10s from 4 group samples", state.ETA)
	}

	// A second group with no samples falls back to the tenant window.
	other := mustGroup(t, svc, tenant, "eta-other", 1)
	otherHolder, otherWaiting := uuid.New(), uuid.New()
	for _, session := range []uuid.UUID{otherHolder, otherWaiting} {
		mustJoin(t, svc, tenant, session, other.GroupID, "w1", time.Minute)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: otherHolder}); err != nil {
		t.Fatalf("admit other holder: %v", err)
	}
	fallback, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: otherWaiting})
	if err != nil {
		t.Fatalf("queue other waiting: %v", err)
	}
	if !fallback.ETA.Known || fallback.ETA.Basis != "tenant" || fallback.ETA.Wait != 10*time.Second {
		t.Fatalf("tenant fallback ETA = %+v, want known 10s from tenant samples", fallback.ETA)
	}
}

// TestQueuePromotionIsExactlyOnce races many promotion passes over one queue:
// only the free slots are filled, each row once, in FIFO order.
func TestQueuePromotionIsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	svc, _ := memService(t, Options{})
	tenant := uuid.New()
	group := mustGroup(t, svc, tenant, "promote-race", 2)
	holderA, holderB := uuid.New(), uuid.New()
	for _, session := range []uuid.UUID{holderA, holderB} {
		mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	}
	waiting := make([]uuid.UUID, 5)
	for i := range waiting {
		waiting[i] = uuid.New()
		mustJoin(t, svc, tenant, waiting[i], group.GroupID, "w1", time.Minute)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: holderA}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: holderB}); err != nil {
		t.Fatal(err)
	}
	for _, session := range waiting {
		admission, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: session})
		if err != nil || !admission.Queued {
			t.Fatalf("queue %s = %+v err=%v", session, admission, err)
		}
	}

	// Both holders settle; exactly two promotion passes may win two rows.
	store := svc.Store().(*MemStore)
	for _, session := range []uuid.UUID{holderA, holderB} {
		if err := store.SetRunStatus(tenant, session, RunStatusDone, 0); err != nil {
			t.Fatal(err)
		}
	}
	var promoted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := svc.PromoteQueued(ctx, tenant)
			if err != nil {
				t.Errorf("promote: %v", err)
				return
			}
			promoted.Add(int32(len(got)))
		}()
	}
	wg.Wait()
	if promoted.Load() != 2 {
		t.Fatalf("promoted = %d, want exactly 2 (one per freed slot)", promoted.Load())
	}
	// FIFO: the two oldest waiting sessions are the ones that run.
	for i, session := range waiting {
		state, err := svc.QueueState(ctx, tenant, session)
		if err != nil {
			t.Fatal(err)
		}
		wantQueued := i >= 2
		if state.Queued != wantQueued {
			t.Fatalf("waiting[%d] queued=%v, want %v", i, state.Queued, wantQueued)
		}
		if wantQueued && state.Position != i-1 {
			t.Fatalf("waiting[%d] position = %d, want %d", i, state.Position, i-1)
		}
	}
}

// TestQueueTenantQuotaIsHardRefusal keeps the tenant quota out of the queue: a
// spent quota is a 429 even when the group also has room, and a queued run
// blocked at promotion by a lowered quota waits instead of overselling it.
func TestQueueTenantQuotaIsHardRefusal(t *testing.T) {
	ctx := context.Background()
	limit := 1
	svc, _ := memService(t, Options{
		TenantRunLimit: func(context.Context, uuid.UUID) (int, error) { return limit, nil },
	})
	tenant := uuid.New()
	group := mustGroup(t, svc, tenant, "quota-queue", 1)
	first, second := uuid.New(), uuid.New()
	for _, session := range []uuid.UUID{first, second} {
		mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: first}); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	_, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: second})
	var capacity *CapacityError
	if !errors.As(err, &capacity) || capacity.Reason != CapacityTenant {
		t.Fatalf("admit under a spent quota = %v, want the tenant refusal (the queue is not a bypass)", err)
	}
	if !IsCapacity(err) {
		t.Fatal("IsCapacity did not recognise the refusal")
	}
	if capacity.RetryAfter < DefaultRetryAfter {
		t.Fatalf("retry_after = %s, want >= %s", capacity.RetryAfter, DefaultRetryAfter)
	}
}

// TestQueuePromotionStopsAtALoweredTenantQuota covers the promotion side: a
// queued run admitted while the quota had room waits when an administrator
// lowers the quota under it, and is promoted once room returns.
func TestQueuePromotionStopsAtALoweredTenantQuota(t *testing.T) {
	ctx := context.Background()
	limit := 2
	svc, _ := memService(t, Options{
		TenantRunLimit: func(context.Context, uuid.UUID) (int, error) { return limit, nil },
	})
	tenant := uuid.New()
	groupA := mustGroup(t, svc, tenant, "lowered-a", 1)
	groupB := mustGroup(t, svc, tenant, "lowered-b", 0)
	holder, waiting, other := uuid.New(), uuid.New(), uuid.New()
	mustJoin(t, svc, tenant, holder, groupA.GroupID, "w1", time.Minute)
	mustJoin(t, svc, tenant, waiting, groupA.GroupID, "w1", time.Minute)
	mustJoin(t, svc, tenant, other, groupB.GroupID, "w1", time.Minute)
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: holder}); err != nil {
		t.Fatal(err)
	}
	queued, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: waiting})
	if err != nil || !queued.Queued {
		t.Fatalf("queue waiting = %+v err=%v", queued, err)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: other}); err != nil {
		t.Fatalf("admit other: %v", err)
	}

	// The quota is lowered under the queue, then the holder frees its slot.
	limit = 1
	store := svc.Store().(*MemStore)
	store.SetRunStatus(tenant, holder, RunStatusDone, 0)
	promoted, err := svc.PromoteQueued(ctx, tenant)
	if err != nil {
		t.Fatalf("promote under a lowered quota: %v", err)
	}
	if len(promoted) != 0 {
		t.Fatalf("promoted = %+v, want none (the tenant quota has no room)", promoted)
	}
	state, err := svc.QueueState(ctx, tenant, waiting)
	if err != nil || !state.Queued || state.Position != 1 {
		t.Fatalf("waiting state = %+v err=%v, want still queued", state, err)
	}

	// The other group's run settles: room returns and the head is promoted.
	store.SetRunStatus(tenant, other, RunStatusDone, 0)
	promoted, err = svc.PromoteQueued(ctx, tenant)
	if err != nil || len(promoted) != 1 || promoted[0].SessionID != waiting {
		t.Fatalf("promotion after room returned = %+v err=%v", promoted, err)
	}
}

// TestTenantQuotaAggregatesAcrossGroups pins the no-oversell rule: the tenant
// limit is a cross-group sum, so a second group cannot sell a slot the first
// one already used.
func TestTenantQuotaAggregatesAcrossGroups(t *testing.T) {
	ctx := context.Background()
	limit := 3
	svc, _ := memService(t, Options{
		TenantRunLimit: func(context.Context, uuid.UUID) (int, error) { return limit, nil },
	})
	tenant := uuid.New()
	groupA := mustGroup(t, svc, tenant, "quota-a", 0)
	groupB := mustGroup(t, svc, tenant, "quota-b", 0)
	sessions := []struct {
		id    uuid.UUID
		group uuid.UUID
	}{
		{uuid.New(), groupA.GroupID},
		{uuid.New(), groupB.GroupID},
		{uuid.New(), groupA.GroupID},
		{uuid.New(), groupB.GroupID},
	}
	for _, s := range sessions {
		mustJoin(t, svc, tenant, s.id, s.group, "w1", time.Minute)
	}
	for i := 0; i < 3; i++ {
		if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: sessions[i].id}); err != nil {
			t.Fatalf("admit %d: %v", i, err)
		}
	}
	_, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: sessions[3].id})
	var capacity *CapacityError
	if !errors.As(err, &capacity) || capacity.Reason != CapacityTenant {
		t.Fatalf("fourth admit = %v, want tenant quota refusal", err)
	}
	if capacity.Limit != limit || capacity.Active != limit {
		t.Fatalf("capacity = %+v, want %d/%d", capacity, limit, limit)
	}
	if err := svc.ReleaseRun(ctx, tenant, sessions[0].id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: sessions[3].id}); err != nil {
		t.Fatalf("admit after release: %v", err)
	}
}

// TestConcurrentAdmissionsDoNotOversell runs the two races concurrently: one
// group limit and one cross-group tenant limit. Exactly the tenant limit is
// admitted (the group limit only queues), and every refusal is a typed
// capacity error, never a silent oversell.
func TestConcurrentAdmissionsDoNotOversell(t *testing.T) {
	ctx := context.Background()
	const attempts = 40
	tenant := uuid.New()
	svc, store := memService(t, Options{
		TenantRunLimit: func(context.Context, uuid.UUID) (int, error) { return 5, nil },
	})
	groupA := mustGroup(t, svc, tenant, "race-slot-a", 3)
	groupB := mustGroup(t, svc, tenant, "race-slot-b", 0)
	sessions := make([]uuid.UUID, attempts)
	for i := range sessions {
		sessions[i] = uuid.New()
		group := groupA.GroupID
		if i%2 == 1 {
			group = groupB.GroupID
		}
		mustJoin(t, svc, tenant, sessions[i], group, "w1", time.Minute)
	}

	var admitted, queued, refused, untyped atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, attempts)
	for _, session := range sessions {
		session := session
		wg.Add(1)
		go func() {
			defer wg.Done()
			admission, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: session})
			switch {
			case err == nil && admission.Queued:
				queued.Add(1)
			case err == nil:
				admitted.Add(1)
			case IsCapacity(err):
				refused.Add(1)
			default:
				untyped.Add(1)
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("untyped admission error: %v", err)
	}
	if admitted.Load() != 5 || untyped.Load() != 0 {
		t.Fatalf("admissions admitted=%d queued=%d refused=%d untyped=%d, want 5 admitted",
			admitted.Load(), queued.Load(), refused.Load(), untyped.Load())
	}
	if got := admitted.Load() + queued.Load() + refused.Load(); got != attempts {
		t.Fatalf("accounted admissions = %d, want %d", got, attempts)
	}
	// Nothing beyond the limits is active: queued rows hold no slot.
	if active := store.activeTenantLocked(tenant); active != 5 {
		t.Fatalf("active tenant rows = %d, want exactly the quota 5", active)
	}
	if active := store.activeGroupLocked(tenant, groupA.GroupID); active > 3 {
		t.Fatalf("active group A rows = %d, want <= 3", active)
	}
}
