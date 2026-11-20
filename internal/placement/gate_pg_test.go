package placement

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	keelstore "github.com/jixoai/keel/internal/store"
)

// pgGate wires a gate over the real database with the production session check
// (the session must exist and must not be external).
func pgGate(t *testing.T, st *keelstore.Store, svc *Service, worker string) *Gate {
	t.Helper()
	gate, err := NewGate(GateOptions{
		Service:  svc,
		WorkerID: worker,
		SessionState: func(ctx context.Context, tenantID, sessionID uuid.UUID) (bool, bool, error) {
			owner, exists, err := st.SessionOwner(ctx, tenantID, sessionID)
			if err != nil {
				return false, false, err
			}
			return exists, exists && owner == keelstore.StateOwnerExternal, nil
		},
	})
	if err != nil {
		t.Fatalf("new gate: %v", err)
	}
	return gate
}

func pgSession(t *testing.T, st *keelstore.Store, tenant uuid.UUID) uuid.UUID {
	t.Helper()
	session := uuid.New()
	if err := st.CreateSession(context.Background(), tenant, session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return session
}

func pgRunStatus(t *testing.T, st *keelstore.Store, tenant, session uuid.UUID) string {
	t.Helper()
	var status string
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT status FROM runs WHERE tenant_id = $1 AND session_id = $2`,
		tenant, session).Scan(&status); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	return status
}

// TestPGGateAutoPlacesAndAdmits is the production gate path against PostgreSQL:
// an unplaced session lands in the strategy group and its first run is stamped
// with that group.
func TestPGGateAutoPlacesAndAdmits(t *testing.T) {
	st := pgPlacementStore(t)
	svc := pgService(t, st, 0)
	ctx := context.Background()
	tenant := pgTenant(t, st)
	session := pgSession(t, st, tenant)
	gate := pgGate(t, st, svc, "runtime-gate")

	admission, err := gate.Admit(ctx, tenant, session)
	if err != nil {
		t.Fatalf("gate admit: %v", err)
	}
	if admission.AlreadyActive || admission.GroupID == uuid.Nil {
		t.Fatalf("admission = %+v, want a new run in a group", admission)
	}
	group, err := svc.GetGroup(ctx, tenant, admission.GroupID)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if group.Name != PackDefaultGroupName {
		t.Fatalf("group = %q, want %q", group.Name, PackDefaultGroupName)
	}
	placement, err := svc.Store().GetPlacement(ctx, tenant, session)
	if err != nil {
		t.Fatalf("get placement: %v", err)
	}
	if placement.WorkerID != "runtime-gate" {
		t.Fatalf("placement worker = %q, want runtime-gate", placement.WorkerID)
	}
	if n := pgCount(t, st, `SELECT count(*) FROM runs
	                        WHERE tenant_id = $1 AND session_id = $2 AND group_id = $3
	                          AND status = 'pending'`, tenant, session, admission.GroupID); n != 1 {
		t.Fatalf("stamped pending runs = %d, want 1", n)
	}

	// A second input for the same session keeps one slot.
	again, err := gate.Admit(ctx, tenant, session)
	if err != nil {
		t.Fatalf("second gate admit: %v", err)
	}
	if !again.AlreadyActive || again.ActiveTenant != 1 {
		t.Fatalf("second admission = %+v, want already-active with one run", again)
	}
}

// TestPGGateConcurrentAdmissionsNoOversell includes the ensure-placement step in
// the race: every session is placed already, and exactly the group limit wins.
func TestPGGateConcurrentAdmissionsNoOversell(t *testing.T) {
	st := pgPlacementStore(t)
	svc := pgService(t, st, 0)
	ctx := context.Background()
	tenant := pgTenant(t, st)
	group := mustGroup(t, svc, tenant, "pg-gate-race", 3)
	gate := pgGate(t, st, svc, "runtime-gate")
	const attempts = 16
	sessions := make([]uuid.UUID, attempts)
	for i := range sessions {
		sessions[i] = pgSession(t, st, tenant)
		mustJoin(t, svc, tenant, sessions[i], group.GroupID, "runtime-gate", time.Minute)
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
		t.Fatalf("untyped gate admission error: %v", err)
	}
	if admitted.Load() != 3 || refused.Load() != attempts-3 || untyped.Load() != 0 {
		t.Fatalf("admitted=%d refused=%d untyped=%d, want 3/%d/0",
			admitted.Load(), refused.Load(), untyped.Load(), attempts-3)
	}
	if n := pgCount(t, st, `SELECT count(*) FROM runs
	                        WHERE tenant_id = $1 AND group_id = $2
	                          AND status IN ('pending','running')`, tenant, group.GroupID); n != 3 {
		t.Fatalf("active group runs = %d, want 3", n)
	}
}

// TestPGReleaseRunSparesACommittedInput pins the release guard: only a pending
// run with no unread input is dropped.
func TestPGReleaseRunSparesACommittedInput(t *testing.T) {
	st := pgPlacementStore(t)
	svc := pgService(t, st, 0)
	ctx := context.Background()
	tenant := pgTenant(t, st)
	session := pgSession(t, st, tenant)
	gate := pgGate(t, st, svc, "runtime-gate")
	if _, err := gate.Admit(ctx, tenant, session); err != nil {
		t.Fatalf("admit: %v", err)
	}

	// A successful input commit sets pending_input in the same transaction that
	// appends the message. A later release must not drop that queued run.
	if _, err := st.Pool().Exec(ctx,
		`UPDATE runs SET pending_input = true WHERE tenant_id = $1 AND session_id = $2`,
		tenant, session); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReleaseRun(ctx, tenant, session); err != nil {
		t.Fatalf("release committed: %v", err)
	}
	if got := pgRunStatus(t, st, tenant, session); got != RunStatusPending {
		t.Fatalf("committed run after release = %q, want pending", got)
	}

	// An abandoned reservation has no unread input and is dropped, freeing the
	// slot (it is not claimable anymore).
	if _, err := st.Pool().Exec(ctx,
		`UPDATE runs SET pending_input = false WHERE tenant_id = $1 AND session_id = $2`,
		tenant, session); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReleaseRun(ctx, tenant, session); err != nil {
		t.Fatalf("release abandoned: %v", err)
	}
	if got := pgRunStatus(t, st, tenant, session); got != RunStatusDone {
		t.Fatalf("abandoned run after release = %q, want done", got)
	}
}

// TestPGAdmitAdoptsAnUnstampedPendingRow is the regression guard for the old
// selfActive shortcut, on the real runs table: the kernel's edge wake creates a
// pending row with a NULL group, and admission must still charge the group.
func TestPGAdmitAdoptsAnUnstampedPendingRow(t *testing.T) {
	st := pgPlacementStore(t)
	svc := pgService(t, st, 0)
	ctx := context.Background()
	tenant := pgTenant(t, st)
	group := mustGroup(t, svc, tenant, "pg-unstamped", 1)
	filler, legacy := pgSession(t, st, tenant), pgSession(t, st, tenant)
	for _, session := range []uuid.UUID{filler, legacy} {
		mustJoin(t, svc, tenant, session, group.GroupID, "runtime-gate", time.Minute)
	}
	if _, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: filler}); err != nil {
		t.Fatalf("fill group: %v", err)
	}
	// The kernel wake inserts the run row before placement admits it: pending,
	// group_id NULL.
	if _, err := st.Pool().Exec(ctx,
		`INSERT INTO runs (tenant_id, session_id, status) VALUES ($1, $2, 'pending')
		 ON CONFLICT (tenant_id, session_id) DO UPDATE SET status = 'pending'`,
		tenant, legacy); err != nil {
		t.Fatal(err)
	}

	_, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: legacy})
	var capacity *CapacityError
	if !errors.As(err, &capacity) || capacity.Reason != CapacityGroup {
		t.Fatalf("unstamped pending admission = %v, want a full-group refusal", err)
	}

	// Free the group: the pending run is adopted (already active, tenant slot
	// unchanged) and stamped with the group.
	if _, err := st.Pool().Exec(ctx,
		`UPDATE runs SET status = 'done' WHERE tenant_id = $1 AND session_id = $2`,
		tenant, filler); err != nil {
		t.Fatal(err)
	}
	admission, err := svc.AdmitRun(ctx, AdmitRequest{TenantID: tenant, SessionID: legacy})
	if err != nil {
		t.Fatalf("adopt unstamped pending run: %v", err)
	}
	if !admission.AlreadyActive || admission.GroupID != group.GroupID {
		t.Fatalf("adoption = %+v, want already active in %s", admission, group.GroupID)
	}
	if n := pgCount(t, st, `SELECT count(*) FROM runs
	                        WHERE tenant_id = $1 AND session_id = $2 AND group_id = $3`,
		tenant, legacy, group.GroupID); n != 1 {
		t.Fatalf("adopted run rows with group = %d, want 1", n)
	}
}
