package placement

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/kernel"
	keelstore "github.com/jixoai/keel/internal/store"
)

// pgPlacementStore opens the integration database. Every test uses fresh
// tenant/group/session ids, so tests do not clean up and cannot collide.
//
// The kernel migration runs too: production boots kernel.NewPG before the
// placement gate, and the runs table's pending_input/read_watermark columns
// (kernel 0004) are part of the accounting ReleaseRun reads.
func pgPlacementStore(t *testing.T) *keelstore.Store {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := keelstore.Open(ctx, keelstore.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := kernel.NewPG(ctx, st.Pool()); err != nil {
		st.Close()
		t.Fatalf("kernel migrate: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func pgTenant(t *testing.T, st *keelstore.Store) uuid.UUID {
	t.Helper()
	tenant := uuid.New()
	if err := st.CreateTenant(context.Background(), tenant, "placement-pg-"+tenant.String()[:8]); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	return tenant
}

func pgService(t *testing.T, st *keelstore.Store, tenantLimit int) *Service {
	t.Helper()
	svc, err := New(Options{
		Store: NewPGStore(st.Pool()),
		TenantRunLimit: func(context.Context, uuid.UUID) (int, error) {
			return tenantLimit, nil
		},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

func pgCount(t *testing.T, st *keelstore.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	return n
}

// TestPGClaimEpochAndLiveLease pins the database write: epoch CAS, holder-only
// renewal of a live lease, and takeover after expiry.
func TestPGClaimEpochAndLiveLease(t *testing.T) {
	st := pgPlacementStore(t)
	svc := pgService(t, st, 0)
	ctx := context.Background()
	tenant := pgTenant(t, st)
	group := mustGroup(t, svc, tenant, "pg-cas", 0)
	session := uuid.New()

	first := mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	if first.Epoch != 1 {
		t.Fatalf("first epoch = %d, want 1", first.Epoch)
	}
	_, err := svc.Claim(ctx, ClaimRequest{TenantID: tenant, SessionID: session,
		GroupID: &group.GroupID, WorkerID: "w2", ExpectedEpoch: 0})
	if conflict := asConflict(t, err); conflict.Reason != ReasonEpochMismatch || conflict.CurrentEpoch != 1 {
		t.Fatalf("claim expected 0 = %+v, want epoch_mismatch current 1", conflict)
	}
	_, err = svc.Claim(ctx, ClaimRequest{TenantID: tenant, SessionID: session,
		GroupID: &group.GroupID, WorkerID: "w2", ExpectedEpoch: 1})
	if conflict := asConflict(t, err); conflict.Reason != ReasonLeaseHeld || conflict.Holder != "w1" {
		t.Fatalf("live lease takeover = %+v, want lease_held by w1", conflict)
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

	// Lease expiry is server time: a short TTL then a short sleep, then a new
	// worker takes over with the expected epoch.
	expiring := uuid.New()
	mustJoin(t, svc, tenant, expiring, group.GroupID, "w1", 120*time.Millisecond)
	time.Sleep(250 * time.Millisecond)
	taken, err := svc.Claim(ctx, ClaimRequest{TenantID: tenant, SessionID: expiring,
		GroupID: &group.GroupID, WorkerID: "w2", ExpectedEpoch: 1})
	if err != nil {
		t.Fatalf("expired takeover: %v", err)
	}
	if taken.WorkerID != "w2" || taken.Epoch != 2 {
		t.Fatalf("expired takeover = %+v, want w2 epoch 2", taken)
	}

	leaving := uuid.New()
	left := mustJoin(t, svc, tenant, leaving, group.GroupID, "w1", time.Minute)
	if _, err := svc.Leave(ctx, LeaveRequest{TenantID: tenant, SessionID: leaving,
		WorkerID: "w2", ExpectedEpoch: left.Epoch}); !IsConflict(err) {
		t.Fatalf("leave by non-holder = %v, want conflict", err)
	}
	if _, err := svc.Leave(ctx, LeaveRequest{TenantID: tenant, SessionID: leaving,
		WorkerID: "w1", ExpectedEpoch: left.Epoch}); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if _, err := svc.Store().GetPlacement(ctx, tenant, leaving); !errors.Is(err, ErrPlacementNotFound) {
		t.Fatalf("placement still present after leave: %v", err)
	}
}

// TestPGConcurrentClaimsSingleWinner is the two-group race against PostgreSQL:
// exactly one placement row and one active run, every loser typed as a
// conflict, so no second concurrent run exists.
func TestPGConcurrentClaimsSingleWinner(t *testing.T) {
	st := pgPlacementStore(t)
	svc := pgService(t, st, 0)
	ctx := context.Background()
	tenant := pgTenant(t, st)
	groupA := mustGroup(t, svc, tenant, "pg-race-a", 0)
	groupB := mustGroup(t, svc, tenant, "pg-race-b", 0)
	session := uuid.New()

	const claims = 24
	var wins, conflicts atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, claims)
	for i := 0; i < claims; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			group := groupA.GroupID
			if i%2 == 1 {
				group = groupB.GroupID
			}
			_, err := svc.Claim(ctx, ClaimRequest{TenantID: tenant, SessionID: session,
				GroupID: &group, WorkerID: "w-" + uuid.NewString()})
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
	if wins.Load() != 1 || conflicts.Load() != claims-1 {
		t.Fatalf("claim race wins=%d conflicts=%d, want 1/%d", wins.Load(), conflicts.Load(), claims-1)
	}
	if n := pgCount(t, st, `SELECT count(*) FROM session_placement WHERE tenant_id = $1 AND session_id = $2`,
		tenant, session); n != 1 {
		t.Fatalf("placement rows = %d, want 1", n)
	}

	admission, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: session})
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if admission.AlreadyActive || admission.ActiveTenant != 1 {
		t.Fatalf("first admission = %+v", admission)
	}
	again, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: session})
	if err != nil {
		t.Fatalf("second admit: %v", err)
	}
	if !again.AlreadyActive || again.ActiveTenant != 1 {
		t.Fatalf("second admission = %+v, want already-active", again)
	}
	if n := pgCount(t, st, `SELECT count(*) FROM runs WHERE tenant_id = $1 AND session_id = $2`,
		tenant, session); n != 1 {
		t.Fatalf("run rows = %d, want 1 (a second concurrent run must not exist)", n)
	}
}

// TestPGRunSlotGroupLimit pins the group slot refusal and its Retry-After. The
// slot is freed by finishing the run in the runs table (the kernel's path).
// TestPGRunSlotGroupLimit pins the database queue: a full group stores the run
// as a queued FIFO row (no slot, no claimable status), and promotion after the
// holder settles turns exactly the head into a pending run.
func TestPGRunSlotGroupLimit(t *testing.T) {
	st := pgPlacementStore(t)
	svc := pgService(t, st, 0)
	ctx := context.Background()
	tenant := pgTenant(t, st)
	group := mustGroup(t, svc, tenant, "pg-slots", 1)
	s1, s2 := uuid.New(), uuid.New()
	for _, session := range []uuid.UUID{s1, s2} {
		mustJoin(t, svc, tenant, session, group.GroupID, "w1", time.Minute)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: s1}); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	if _, err := st.Pool().Exec(ctx,
		`UPDATE runs SET status = 'running', lease_until = now() + interval '5 seconds'
		  WHERE tenant_id = $1 AND session_id = $2`, tenant, s1); err != nil {
		t.Fatal(err)
	}
	queued, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: s2})
	if err != nil {
		t.Fatalf("full group admit: %v", err)
	}
	if !queued.Queued || queued.QueuePosition != 1 || queued.ActiveGroup != 1 {
		t.Fatalf("full group admission = %+v, want queued at position 1", queued)
	}
	if queued.ETA.Known {
		t.Fatalf("ETA = %+v, want unknown with no observed durations", queued.ETA)
	}
	if status := pgRunStatus(t, st, tenant, s2); status != "queued" {
		t.Fatalf("queued run status = %q, want queued", status)
	}
	// The queued row holds no slot and is not claimable by the kernel: the
	// claim predicate matches pending/running only.
	if n := pgCount(t, st, `SELECT count(*) FROM runs
	                        WHERE tenant_id = $1 AND status IN ('pending','running')`, tenant); n != 1 {
		t.Fatalf("active runs = %d, want 1 (the queued row holds no slot)", n)
	}

	if _, err := st.Pool().Exec(ctx,
		`UPDATE runs SET status = 'done', lease_until = NULL
		  WHERE tenant_id = $1 AND session_id = $2`, tenant, s1); err != nil {
		t.Fatal(err)
	}
	promoted, err := svc.PromoteQueued(ctx, tenant)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if len(promoted) != 1 || promoted[0].SessionID != s2 {
		t.Fatalf("promoted = %+v, want the FIFO head %s", promoted, s2)
	}
	if status := pgRunStatus(t, st, tenant, s2); status != "pending" {
		t.Fatalf("promoted run status = %q, want pending", status)
	}
}

// TestPGTenantQuotaAcrossGroupsNoOversell is the cross-group quota aggregate:
// four sessions in two groups fill a limit of four, and the fifth is refused.
func TestPGTenantQuotaAcrossGroupsNoOversell(t *testing.T) {
	st := pgPlacementStore(t)
	const limit = 4
	svc := pgService(t, st, limit)
	ctx := context.Background()
	tenant := pgTenant(t, st)
	groupA := mustGroup(t, svc, tenant, "pg-quota-a", 0)
	groupB := mustGroup(t, svc, tenant, "pg-quota-b", 0)
	sessions := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	for i, session := range sessions {
		group := groupA.GroupID
		if i%2 == 1 {
			group = groupB.GroupID
		}
		mustJoin(t, svc, tenant, session, group, "w1", time.Minute)
	}
	for i := 0; i < limit; i++ {
		if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: sessions[i]}); err != nil {
			t.Fatalf("admit %d: %v", i, err)
		}
	}
	_, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: sessions[4]})
	var capacity *CapacityError
	if !errors.As(err, &capacity) || capacity.Reason != CapacityTenant {
		t.Fatalf("fifth admit = %v, want tenant quota refusal", err)
	}
	if capacity.Limit != limit || capacity.Active != limit {
		t.Fatalf("capacity = %+v, want %d/%d", capacity, limit, limit)
	}
	if n := pgCount(t, st, `SELECT count(*) FROM runs
	                        WHERE tenant_id = $1 AND status IN ('pending','running')`, tenant); n != limit {
		t.Fatalf("active runs = %d, want %d", n, limit)
	}
}

// TestPGConcurrentAdmissionsNoOversell runs the two races against one database:
// a cross-group tenant limit of five out of thirty attempts, and a single
// group's limit of three out of twenty. The winners are exactly the limits,
// every loser is typed, and the runs table agrees.
func TestPGConcurrentAdmissionsNoOversell(t *testing.T) {
	st := pgPlacementStore(t)
	ctx := context.Background()
	tenant := pgTenant(t, st)
	svc := pgService(t, st, 5)
	groupA := mustGroup(t, svc, tenant, "pg-race-slot-a", 3)
	groupB := mustGroup(t, svc, tenant, "pg-race-slot-b", 0)
	sessions := make([]uuid.UUID, 30)
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
	errs := make(chan error, len(sessions))
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
		t.Fatalf("tenant race admitted=%d queued=%d refused=%d untyped=%d, want 5 admitted",
			admitted.Load(), queued.Load(), refused.Load(), untyped.Load())
	}
	if got := admitted.Load() + queued.Load() + refused.Load(); got != int32(len(sessions)) {
		t.Fatalf("accounted tenant race admissions = %d, want %d", got, len(sessions))
	}
	if n := pgCount(t, st, `SELECT count(*) FROM runs
	                        WHERE tenant_id = $1 AND status IN ('pending','running')`, tenant); n != 5 {
		t.Fatalf("active runs after tenant race = %d, want 5", n)
	}

	// Group race: a fresh tenant, no tenant limit, one group with three slots.
	groupTenant := pgTenant(t, st)
	groupSvc := pgService(t, st, 0)
	limited := mustGroup(t, groupSvc, groupTenant, "pg-race-g", 3)
	groupSessions := make([]uuid.UUID, 20)
	for i := range groupSessions {
		groupSessions[i] = uuid.New()
		mustJoin(t, groupSvc, groupTenant, groupSessions[i], limited.GroupID, "w1", time.Minute)
	}
	admitted.Store(0)
	queued.Store(0)
	refused.Store(0)
	wg = sync.WaitGroup{}
	for _, session := range groupSessions {
		session := session
		wg.Add(1)
		go func() {
			defer wg.Done()
			admission, err := groupSvc.AdmitRun(ctx, AdmitRequest{TenantID: groupTenant, SessionID: session})
			switch {
			case err == nil && admission.Queued:
				queued.Add(1)
			case err == nil:
				admitted.Add(1)
			case IsCapacity(err):
				refused.Add(1)
			default:
				t.Errorf("untyped group admission error: %v", err)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 3 || queued.Load() != 17 || refused.Load() != 0 {
		t.Fatalf("group race admitted=%d queued=%d refused=%d, want 3/17/0",
			admitted.Load(), queued.Load(), refused.Load())
	}
	if n := pgCount(t, st, `SELECT count(*) FROM runs
	                        WHERE tenant_id = $1 AND group_id = $2
	                          AND status IN ('pending','running')`, groupTenant, limited.GroupID); n != 3 {
		t.Fatalf("active group runs = %d, want 3", n)
	}
}

// TestPGDrainingGroup pins drain semantics: an existing placement may renew,
// but no new session is placed into a draining group and no run is admitted.
func TestPGDrainingGroup(t *testing.T) {
	st := pgPlacementStore(t)
	svc := pgService(t, st, 0)
	ctx := context.Background()
	tenant := pgTenant(t, st)
	group := mustGroup(t, svc, tenant, "pg-drain", 0)
	existing := uuid.New()
	placed := mustJoin(t, svc, tenant, existing, group.GroupID, "w1", time.Minute)

	if _, err := svc.SetGroupStatus(ctx, tenant, group.GroupID, StatusDraining); err != nil {
		t.Fatalf("drain: %v", err)
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
	if _, err := svc.SetGroupStatus(ctx, tenant, group.GroupID, StatusRetired); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if _, err := svc.SetGroupStatus(ctx, tenant, group.GroupID, StatusActive); err == nil {
		t.Fatal("retired group was reactivated")
	}
}
