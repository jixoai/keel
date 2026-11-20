package placement

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/kernel"
)

// testSessions is the gate's session-state seam for the in-memory tests: a set
// of native sessions plus optional external ones.
type testSessions struct {
	mu       sync.Mutex
	known    map[uuid.UUID]bool
	external map[uuid.UUID]bool
	native   bool
}

func newTestSessions(native bool) *testSessions {
	return &testSessions{known: map[uuid.UUID]bool{}, external: map[uuid.UUID]bool{}, native: native}
}

func (s *testSessions) state(_ context.Context, _ uuid.UUID, sessionID uuid.UUID) (bool, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.native || s.known[sessionID], s.external[sessionID], nil
}

func memGate(t *testing.T, svc *Service, worker string, sessions SessionStateFunc) *Gate {
	t.Helper()
	gate, err := NewGate(GateOptions{Service: svc, WorkerID: worker, SessionState: sessions})
	if err != nil {
		t.Fatalf("new gate: %v", err)
	}
	return gate
}

// TestGateAutoPlacesAndAdmits pins the runtime path: an unplaced session
// resolves the default strategy, gets a placement, and its first run is
// admitted exactly once.
func TestGateAutoPlacesAndAdmits(t *testing.T) {
	ctx := context.Background()
	svc, _ := memService(t, Options{Strategy: Pack})
	gate := memGate(t, svc, "runtime-1", newTestSessions(true).state)
	tenant, session := uuid.New(), uuid.New()

	first, err := gate.Admit(ctx, tenant, session)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if first.AlreadyActive || first.ActiveTenant != 1 || first.GroupID == uuid.Nil {
		t.Fatalf("first admission = %+v, want one new admitted run in a group", first)
	}
	group, err := svc.GetGroup(ctx, tenant, first.GroupID)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if group.Name != PackDefaultGroupName {
		t.Fatalf("group = %q, want the pack default", group.Name)
	}
	placed, err := svc.Store().GetPlacement(ctx, tenant, session)
	if err != nil {
		t.Fatalf("read placement: %v", err)
	}
	if placed.GroupID != first.GroupID || placed.WorkerID != "runtime-1" {
		t.Fatalf("placement = %+v, want the gate's group/worker", placed)
	}

	second, err := gate.Admit(ctx, tenant, session)
	if err != nil {
		t.Fatalf("second admit: %v", err)
	}
	if !second.AlreadyActive || second.ActiveTenant != 1 {
		t.Fatalf("second admission = %+v, want an already-active session", second)
	}
}

// TestGateRefusesMissingAndExternalSessions is the fail-closed check: no
// placement/run row may be written for a session the runtime cannot own.
func TestGateRefusesMissingAndExternalSessions(t *testing.T) {
	ctx := context.Background()
	svc, store := memService(t, Options{})
	missing := newTestSessions(false)
	gate := memGate(t, svc, "runtime-1", missing.state)
	tenant := uuid.New()

	if _, err := gate.Admit(ctx, tenant, uuid.New()); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("missing session admission = %v, want ErrSessionNotFound", err)
	}
	if len(store.placements) != 0 || len(store.runs) != 0 {
		t.Fatalf("missing session wrote placement/runs: %d/%d", len(store.placements), len(store.runs))
	}

	externalSession := uuid.New()
	external := newTestSessions(false)
	external.known[externalSession] = true
	external.external[externalSession] = true
	gate = memGate(t, svc, "runtime-1", external.state)
	if _, err := gate.Admit(ctx, tenant, externalSession); !errors.Is(err, ErrExternalSession) {
		t.Fatalf("external session admission = %v, want ErrExternalSession", err)
	}
	if len(store.placements) != 0 || len(store.runs) != 0 {
		t.Fatalf("external session wrote placement/runs: %d/%d", len(store.placements), len(store.runs))
	}
}

// TestGateDoesNotStealALivePlacementLease is the multi-runtime input check: the
// HTTP edge may run in a process that does not own the session's placement
// lease. Admission reads the stored group and must not bump the epoch or change
// the holder.
func TestGateDoesNotStealALivePlacementLease(t *testing.T) {
	ctx := context.Background()
	svc, _ := memService(t, Options{})
	tenant, session := uuid.New(), uuid.New()
	group := mustGroup(t, svc, tenant, "owned-group", 0)
	held := mustJoin(t, svc, tenant, session, group.GroupID, "worker-owner", time.Minute)

	gate := memGate(t, svc, "worker-edge", newTestSessions(true).state)
	admission, err := gate.Admit(ctx, tenant, session)
	if err != nil {
		t.Fatalf("admit from another runtime: %v", err)
	}
	if admission.GroupID != group.GroupID {
		t.Fatalf("admission group = %s, want %s", admission.GroupID, group.GroupID)
	}
	current, err := svc.Store().GetPlacement(ctx, tenant, session)
	if err != nil {
		t.Fatalf("read placement: %v", err)
	}
	if current.WorkerID != "worker-owner" || current.Epoch != held.Epoch {
		t.Fatalf("placement = %+v, want owner/epoch untouched (%+v)", current, held)
	}
}

// TestGateParentAndChildShareGroupSlots: a parent and its sub-agent child are
// two sessions; both runs consume the same group's slots.
func TestGateParentAndChildShareGroupSlots(t *testing.T) {
	ctx := context.Background()
	svc, _ := memService(t, Options{})
	tenant := uuid.New()
	group := mustGroup(t, svc, tenant, "parent-child", 1)
	parent, child := uuid.New(), uuid.New()
	for _, session := range []uuid.UUID{parent, child} {
		mustJoin(t, svc, tenant, session, group.GroupID, "runtime-1", time.Minute)
	}
	gate := memGate(t, svc, "runtime-1", newTestSessions(true).state)

	if _, err := gate.Admit(ctx, tenant, parent); err != nil {
		t.Fatalf("parent admit: %v", err)
	}
	_, err := gate.Admit(ctx, tenant, child)
	var capacity *CapacityError
	if !errors.As(err, &capacity) || capacity.Reason != CapacityGroup {
		t.Fatalf("child admit = %v, want a group slot refusal", err)
	}
}

// TestGateReleaseFreesOnlyAnUncommittedReservation pins the release guard: an
// admitted run whose input never committed is dropped, while a queued run whose
// input already landed must keep its slot.
func TestGateReleaseFreesOnlyAnUncommittedReservation(t *testing.T) {
	ctx := context.Background()
	svc, store := memService(t, Options{})
	tenant := uuid.New()
	group := mustGroup(t, svc, tenant, "release", 1)
	abandoned, queued, next := uuid.New(), uuid.New(), uuid.New()
	for _, session := range []uuid.UUID{abandoned, queued, next} {
		mustJoin(t, svc, tenant, session, group.GroupID, "runtime-1", time.Minute)
	}
	gate := memGate(t, svc, "runtime-1", newTestSessions(true).state)

	if _, err := gate.Admit(ctx, tenant, abandoned); err != nil {
		t.Fatalf("admit abandoned: %v", err)
	}
	if err := gate.Release(ctx, tenant, abandoned); err != nil {
		t.Fatalf("release abandoned: %v", err)
	}
	if _, err := gate.Admit(ctx, tenant, queued); err != nil {
		t.Fatalf("admit after release: %v", err)
	}

	// The queued run's input commits: the run row now carries the unread-input
	// fact, so a later release must not drop it.
	store.mu.Lock()
	run := store.runs[memSessionKey{tenant, queued}]
	run.pendingInput = true
	store.runs[memSessionKey{tenant, queued}] = run
	store.mu.Unlock()
	if err := gate.Release(ctx, tenant, queued); err != nil {
		t.Fatalf("release committed: %v", err)
	}
	_, err := gate.Admit(ctx, tenant, next)
	var capacity *CapacityError
	if !errors.As(err, &capacity) || capacity.Reason != CapacityGroup {
		t.Fatalf("admit after committed release = %v, want the slot still held", err)
	}
}

// TestAdmitDoesNotTreatAnUnstampedPendingRowAsAdmitted is the regression guard
// for the old selfActive shortcut: a pending row written before admission (the
// kernel's edge wake) has no group yet and must not bypass the group limit.
func TestAdmitDoesNotTreatAnUnstampedPendingRowAsAdmitted(t *testing.T) {
	ctx := context.Background()
	svc, store := memService(t, Options{})
	tenant := uuid.New()
	group := mustGroup(t, svc, tenant, "legacy-pending", 1)
	filler, legacy := uuid.New(), uuid.New()
	for _, session := range []uuid.UUID{filler, legacy} {
		mustJoin(t, svc, tenant, session, group.GroupID, "runtime-1", time.Minute)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: filler}); err != nil {
		t.Fatalf("fill the group: %v", err)
	}
	// Simulate the kernel wake inserting the run row before placement admission:
	// pending, no group stamp.
	store.mu.Lock()
	store.runs[memSessionKey{tenant, legacy}] = memRun{status: RunStatusPending}
	store.mu.Unlock()

	_, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: legacy})
	var capacity *CapacityError
	if !errors.As(err, &capacity) || capacity.Reason != CapacityGroup {
		t.Fatalf("unstamped pending admission = %v, want the full group refusal", err)
	}

	// With room, the same row is adopted into the group instead of being
	// ignored (the run is already active; it needs an accountable slot).
	if err := svc.ReleaseRun(ctx, tenant, filler); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.runs[memSessionKey{tenant, filler}] = memRun{status: RunStatusDone}
	store.mu.Unlock()
	admission, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: legacy})
	if err != nil {
		t.Fatalf("adopt unstamped pending run: %v", err)
	}
	if !admission.AlreadyActive || admission.GroupID != group.GroupID {
		t.Fatalf("adoption = %+v, want active in %s", admission, group.GroupID)
	}
}

// TestGateConcurrentAdmissionsDoNotOversell runs the admission path (placement
// ensure included) concurrently against one group: exactly the limit wins.
func TestGateConcurrentAdmissionsDoNotOversell(t *testing.T) {
	ctx := context.Background()
	svc, _ := memService(t, Options{})
	tenant := uuid.New()
	group := mustGroup(t, svc, tenant, "gate-race", 3)
	gate := memGate(t, svc, "runtime-1", newTestSessions(true).state)
	const attempts = 24
	sessions := make([]uuid.UUID, attempts)
	for i := range sessions {
		sessions[i] = uuid.New()
		mustJoin(t, svc, tenant, sessions[i], group.GroupID, "runtime-1", time.Minute)
	}
	var admitted, refused, untyped atomic.Int32
	errs := make(chan error, attempts)
	var wg sync.WaitGroup
	for _, session := range sessions {
		session := session
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := gate.Admit(ctx, tenant, session)
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
	if admitted.Load() != 3 || refused.Load() != attempts-3 || untyped.Load() != 0 {
		t.Fatalf("admitted=%d refused=%d untyped=%d, want 3/%d/0",
			admitted.Load(), refused.Load(), untyped.Load(), attempts-3)
	}
}

// fakeCommitter records calls and can fail, so the committer tests can assert
// the admission/release behavior without a database.
type fakeCommitter struct {
	mu    sync.Mutex
	calls []kernel.CommitRequest
	err   error
}

func (f *fakeCommitter) Commit(_ context.Context, req kernel.CommitRequest) (kernel.Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if f.err != nil {
		return kernel.Receipt{}, f.err
	}
	return kernel.Receipt{Seq: int64(len(f.calls))}, nil
}

func (f *fakeCommitter) Lookup(context.Context, kernel.Scope, kernel.KeyRef) (kernel.Receipt, bool, error) {
	return kernel.Receipt{}, false, nil
}

func (f *fakeCommitter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// TestGatedCommitterGatesOnlyNativeUserMessages: the wrapper must not add a
// placement write to internal event appends.
func TestGatedCommitterGatesOnlyNativeUserMessages(t *testing.T) {
	ctx := context.Background()
	svc, store := memService(t, Options{})
	gate := memGate(t, svc, "runtime-1", newTestSessions(true).state)
	fake := &fakeCommitter{}
	edge, err := NewGatedCommitter(fake, gate, nil)
	if err != nil {
		t.Fatal(err)
	}
	tenant, session := uuid.New(), uuid.New()
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	if _, err := edge.Commit(ctx, kernel.CommitRequest{Scope: scope, Kind: kernel.KindAssistantMessage}); err != nil {
		t.Fatalf("assistant commit: %v", err)
	}
	if len(store.placements) != 0 || len(store.runs) != 0 {
		t.Fatalf("non-user commit wrote placement/runs: %d/%d", len(store.placements), len(store.runs))
	}
	if _, err := edge.Commit(ctx, kernel.CommitRequest{Scope: scope, Kind: kernel.KindUserMessage}); err != nil {
		t.Fatalf("system user commit: %v", err)
	}
	if len(store.placements) != 0 || len(store.runs) != 0 {
		t.Fatalf("non-native user commit wrote placement/runs: %d/%d", len(store.placements), len(store.runs))
	}

	if _, err := edge.Commit(ctx, kernel.CommitRequest{Scope: scope, Kind: kernel.KindUserMessage, NativeEdge: true}); err != nil {
		t.Fatalf("native user commit: %v", err)
	}
	if len(store.placements) != 1 || len(store.runs) != 1 {
		t.Fatalf("native user commit placement/runs = %d/%d, want 1/1", len(store.placements), len(store.runs))
	}
	if fake.count() != 3 {
		t.Fatalf("kernel commits = %d, want 3", fake.count())
	}
}

// TestGatedCommitterReleasesAFailedNativeCommit: the admission is a slot
// reservation for input that never landed, so it is released; a commit failure
// on an already-active run keeps that run's slot.
func TestGatedCommitterReleasesAFailedNativeCommit(t *testing.T) {
	ctx := context.Background()
	svc, store := memService(t, Options{})
	tenant := uuid.New()
	group := mustGroup(t, svc, tenant, "commit-failure", 1)
	gate := memGate(t, svc, "runtime-1", newTestSessions(true).state)
	fake := &fakeCommitter{err: errors.New("kernel unavailable")}
	edge, err := NewGatedCommitter(fake, gate, nil)
	if err != nil {
		t.Fatal(err)
	}

	failed := uuid.New()
	mustJoin(t, svc, tenant, failed, group.GroupID, "runtime-1", time.Minute)
	if _, err := edge.Commit(ctx, kernel.CommitRequest{
		Scope: kernel.Scope{TenantID: tenant, SessionID: failed}, Kind: kernel.KindUserMessage, NativeEdge: true,
	}); err == nil {
		t.Fatal("failed kernel commit reported success")
	}
	run := store.runs[memSessionKey{tenant, failed}]
	if run.status != RunStatusDone {
		t.Fatalf("run after failed commit = %q, want the reservation released (done)", run.status)
	}

	// An already-active run must not be released by a later failed commit.
	active := uuid.New()
	mustJoin(t, svc, tenant, active, group.GroupID, "runtime-1", time.Minute)
	if _, err := gate.Admit(ctx, tenant, active); err != nil {
		t.Fatalf("admit active: %v", err)
	}
	if _, err := edge.Commit(ctx, kernel.CommitRequest{
		Scope: kernel.Scope{TenantID: tenant, SessionID: active}, Kind: kernel.KindUserMessage, NativeEdge: true,
	}); err == nil {
		t.Fatal("failed kernel commit reported success")
	}
	run = store.runs[memSessionKey{tenant, active}]
	if run.status != RunStatusPending {
		t.Fatalf("already-active run after failed commit = %q, want pending", run.status)
	}
}

// TestGatedCommitterMapsPlacementSentinels: the kernel seam keeps its error
// vocabulary, so callers that branch on ErrNotFound/ErrExternalSession keep
// working.
func TestGatedCommitterMapsPlacementSentinels(t *testing.T) {
	ctx := context.Background()
	svc, _ := memService(t, Options{})
	sessions := newTestSessions(false)
	gate := memGate(t, svc, "runtime-1", sessions.state)
	edge, err := NewGatedCommitter(&fakeCommitter{}, gate, nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := kernel.Scope{TenantID: uuid.New(), SessionID: uuid.New()}
	if _, err := edge.Commit(ctx, kernel.CommitRequest{Scope: scope, Kind: kernel.KindUserMessage, NativeEdge: true}); !errors.Is(err, kernel.ErrNotFound) {
		t.Fatalf("missing session = %v, want kernel.ErrNotFound", err)
	}
	sessions.external[scope.SessionID] = true
	sessions.known[scope.SessionID] = true
	if _, err := edge.Commit(ctx, kernel.CommitRequest{Scope: scope, Kind: kernel.KindUserMessage, NativeEdge: true}); !errors.Is(err, kernel.ErrExternalSession) {
		t.Fatalf("external session = %v, want kernel.ErrExternalSession", err)
	}
}
