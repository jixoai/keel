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

// TestGroupSlotsRefuseWithRetryAfter pins the 429 shape: a full group refuses
// with a typed capacity error and a non-zero Retry-After; releasing a slot
// admits the next run.
func TestGroupSlotsRefuseWithRetryAfter(t *testing.T) {
	ctx := context.Background()
	svc, store := memService(t, Options{})
	tenant := uuid.New()
	group := mustGroup(t, svc, tenant, "slots", 1)
	s1, s2 := uuid.New(), uuid.New()
	for _, session := range []uuid.UUID{s1, s2} {
		mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	}
	_, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: uuid.New()})
	if conflict := asConflict(t, err); conflict.Reason != ReasonPlacementNotFound {
		t.Fatalf("admit for an unplaced session = %+v, want placement_not_found", conflict)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: s1}); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	if err := store.SetRunStatus(tenant, s1, RunStatusRunning, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	_, err = svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: s2})
	var capacity *CapacityError
	if !errors.As(err, &capacity) {
		t.Fatalf("full group err = %v, want *CapacityError", err)
	}
	if capacity.Reason != CapacityGroup || capacity.Limit != 1 || capacity.Active != 1 {
		t.Fatalf("capacity = %+v, want group 1/1", capacity)
	}
	if capacity.RetryAfter < DefaultRetryAfter || capacity.RetryAfter > 3*time.Second {
		t.Fatalf("retry_after = %s, want [1s, 3s]", capacity.RetryAfter)
	}
	if !IsCapacity(err) {
		t.Fatal("IsCapacity did not recognise the refusal")
	}

	if err := svc.ReleaseRun(ctx, tenant, s1); err != nil {
		t.Fatal(err)
	}
	// ReleaseRun only frees an unstarted run; a running run is finished by the
	// kernel. Simulate that finish, then the next admission fits.
	if err := store.SetRunStatus(tenant, s1, RunStatusDone, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: s2}); err != nil {
		t.Fatalf("admit after release: %v", err)
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
// group limit and one cross-group tenant limit. Exactly the limit is admitted
// and every other request gets a typed capacity error.
func TestConcurrentAdmissionsDoNotOversell(t *testing.T) {
	ctx := context.Background()
	const attempts = 40
	tenant := uuid.New()
	svc, _ := memService(t, Options{
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

	var admitted, refused, untyped atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, attempts)
	for _, session := range sessions {
		session := session
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: session})
			switch {
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
	if admitted.Load() != 5 || refused.Load() != attempts-5 || untyped.Load() != 0 {
		t.Fatalf("admissions admitted=%d refused=%d untyped=%d, want 5/%d/0",
			admitted.Load(), refused.Load(), untyped.Load(), attempts-5)
	}

	// Nothing beyond the limits is active: each admitted session reports the
	// same tenant count, so a fresh admissive probe on an already counted
	// session sees exactly 5.
	var report Admission
	for _, session := range sessions {
		report, _ = svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: session})
		if report.AlreadyActive {
			break
		}
	}
	if !report.AlreadyActive || report.ActiveTenant != 5 {
		t.Fatalf("active tenant after race = %+v, want 5", report)
	}
}
