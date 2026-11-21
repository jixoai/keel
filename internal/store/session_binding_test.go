package store

// Session binding projection (change keel-agent-team-v1, P3 session reference
// protection). These tests run against the real test database (KEEL_TEST_DSN):
// the schema migration, the seq-guarded write, the reference queries' archived/
// live-run boundary, and tenant isolation are all SQL behaviour.

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func cleanupSessionBindingTenant(t *testing.T, st *Store, tenants ...uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, tenant := range tenants {
			_, _ = st.Pool().Exec(ctx, `DELETE FROM runs WHERE tenant_id = $1`, tenant)
			_, _ = st.Pool().Exec(ctx, `DELETE FROM events WHERE tenant_id = $1`, tenant)
			_, _ = st.Pool().Exec(ctx, `DELETE FROM sessions WHERE tenant_id = $1`, tenant)
			_, _ = st.Pool().Exec(ctx, `DELETE FROM agents WHERE tenant_id = $1`, tenant)
			_, _ = st.Pool().Exec(ctx, `DELETE FROM agent_teams WHERE tenant_id = $1`, tenant)
			_, _ = st.Pool().Exec(ctx, `DELETE FROM tenants WHERE tenant_id = $1`, tenant)
		}
	})
}

// TestSessionBindingSchemaMigrates pins the additive DDL: the projection columns
// exist with the types the store scans, and the two reference indexes exist so
// the guard is not a table scan.
func TestSessionBindingSchemaMigrates(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()

	wantColumns := map[string]string{
		"bound_agent_id": "text",
		"bound_team_id":  "text",
		"bound_seq":      "bigint",
		"bound_at":       "timestamp with time zone",
	}
	for column, wantType := range wantColumns {
		var dataType string
		if err := st.Pool().QueryRow(ctx,
			`SELECT data_type FROM information_schema.columns
			  WHERE table_schema = current_schema() AND table_name = 'sessions' AND column_name = $1`,
			column).Scan(&dataType); err != nil {
			t.Fatalf("sessions.%s missing: %v", column, err)
		}
		if dataType != wantType {
			t.Fatalf("sessions.%s type = %q, want %q", column, dataType, wantType)
		}
	}
	for _, index := range []string{"sessions_bound_agent", "sessions_bound_team"} {
		var exists bool
		if err := st.Pool().QueryRow(ctx,
			`SELECT to_regclass($1) IS NOT NULL`, index).Scan(&exists); err != nil {
			t.Fatalf("probe index %s: %v", index, err)
		}
		if !exists {
			t.Fatalf("index %s missing after migrate", index)
		}
	}
}

// TestSessionBindingSeqGuard pins the newest-input-wins rule: an older seq can
// never replace a newer binding, a model-only selection clears both ids, and the
// tenant boundary makes another tenant's session invisible.
func TestSessionBindingSeqGuard(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant, foreign := uuid.New(), uuid.New()
	session := uuid.New()
	cleanupSessionBindingTenant(t, st, tenant, foreign)
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	applied, err := st.SetSessionBinding(ctx, tenant, session, "agent_a", "", 1)
	if err != nil || !applied {
		t.Fatalf("set agent binding: applied=%v err=%v", applied, err)
	}
	binding, found, err := st.GetSessionBinding(ctx, tenant, session)
	if err != nil || !found {
		t.Fatalf("get binding: found=%v err=%v", found, err)
	}
	if binding.AgentID != "agent_a" || binding.TeamID != "" || binding.Seq != 1 {
		t.Fatalf("binding = %+v, want agent_a seq 1", binding)
	}

	// Team wins a fact that names both, and the projection follows the team.
	applied, err = st.SetSessionBinding(ctx, tenant, session, "agent_b", "team_t", 2)
	if err != nil || !applied {
		t.Fatalf("set team binding: applied=%v err=%v", applied, err)
	}
	binding, _, _ = st.GetSessionBinding(ctx, tenant, session)
	if binding.AgentID != "" || binding.TeamID != "team_t" || binding.Seq != 2 {
		t.Fatalf("binding = %+v, want team_t seq 2", binding)
	}

	// A delayed write carrying an older seq is a no-op (applied=false, no error).
	applied, err = st.SetSessionBinding(ctx, tenant, session, "agent_stale", "", 1)
	if err != nil || applied {
		t.Fatalf("stale write: applied=%v err=%v, want false/nil", applied, err)
	}
	binding, _, _ = st.GetSessionBinding(ctx, tenant, session)
	if binding.TeamID != "team_t" || binding.Seq != 2 {
		t.Fatalf("stale write replaced the binding: %+v", binding)
	}

	// A model-only selection (both ids empty) clears the projection.
	applied, err = st.SetSessionBinding(ctx, tenant, session, "", "", 3)
	if err != nil || !applied {
		t.Fatalf("clear binding: applied=%v err=%v", applied, err)
	}
	binding, _, _ = st.GetSessionBinding(ctx, tenant, session)
	if binding.AgentID != "" || binding.TeamID != "" || binding.Seq != 3 {
		t.Fatalf("binding after clear = %+v, want empty seq 3", binding)
	}

	// Cross-tenant: the session exists, but not for this tenant.
	if _, err := st.SetSessionBinding(ctx, foreign, session, "agent_x", "", 4); err != ErrNoSession {
		t.Fatalf("foreign set error = %v, want ErrNoSession", err)
	}
	if _, found, err := st.GetSessionBinding(ctx, foreign, session); err != nil || found {
		t.Fatalf("foreign get: found=%v err=%v, want false/nil", found, err)
	}
	// A missing session is the same typed refusal.
	if _, err := st.SetSessionBinding(ctx, tenant, uuid.New(), "agent_x", "", 5); err != ErrNoSession {
		t.Fatalf("missing set error = %v, want ErrNoSession", err)
	}
	// A non-positive seq is a programming error, never a silent write.
	if _, err := st.SetSessionBinding(ctx, tenant, session, "agent_x", "", 0); err == nil {
		t.Fatal("seq 0 was accepted")
	}
}

// TestSessionReferencesBoundary pins the reference lifecycle:
//   - an unarchived session blocks regardless of run state;
//   - an archived session does not block;
//   - an archived session with a live (pending/running) run still blocks, because
//     archiving does not cancel the run;
//   - the two id columns are separate (agent vs team), and other tenants never
//     appear.
func TestSessionReferencesBoundary(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant, foreign := uuid.New(), uuid.New()
	cleanupSessionBindingTenant(t, st, tenant, foreign)
	for _, id := range []uuid.UUID{tenant, foreign} {
		if err := st.CreateTenant(ctx, id, "session-ref-"+id.String()[:8]); err != nil {
			t.Fatalf("create tenant: %v", err)
		}
	}
	active := uuid.New()
	archived := uuid.New()
	archivedLive := uuid.New()
	agentBound := uuid.New()
	foreignBound := uuid.New()
	for _, session := range []uuid.UUID{active, archived, archivedLive, agentBound} {
		if err := st.CreateSession(ctx, tenant, session); err != nil {
			t.Fatalf("create session %s: %v", session, err)
		}
	}
	if err := st.CreateSession(ctx, foreign, foreignBound); err != nil {
		t.Fatalf("create foreign session: %v", err)
	}
	for _, session := range []uuid.UUID{active, archived, archivedLive} {
		if _, err := st.SetSessionBinding(ctx, tenant, session, "", "team_busy", 1); err != nil {
			t.Fatalf("bind team %s: %v", session, err)
		}
	}
	if _, err := st.SetSessionBinding(ctx, tenant, agentBound, "agent_busy", "", 1); err != nil {
		t.Fatalf("bind agent: %v", err)
	}
	if _, err := st.SetSessionBinding(ctx, foreign, foreignBound, "", "team_busy", 1); err != nil {
		t.Fatalf("bind foreign: %v", err)
	}

	// archivedLive owns a pending run; archiving the session does not cancel it.
	if err := st.EnqueueRun(ctx, tenant, archivedLive); err != nil {
		t.Fatalf("enqueue run: %v", err)
	}
	for _, session := range []uuid.UUID{archived, archivedLive} {
		found, err := st.ArchiveSession(ctx, tenant, session)
		if err != nil || !found {
			t.Fatalf("archive %s: found=%v err=%v", session, found, err)
		}
	}

	byID := func(refs []SessionReference) map[string]SessionReference {
		out := make(map[string]SessionReference, len(refs))
		for _, ref := range refs {
			out[ref.ID] = ref
		}
		return out
	}
	refs, err := st.SessionsReferencingTeam(ctx, tenant, "team_busy")
	if err != nil {
		t.Fatalf("team references: %v", err)
	}
	got := byID(refs)
	if len(got) != 2 {
		t.Fatalf("team references = %+v, want active + archived-live", refs)
	}
	if _, ok := got[active.String()]; !ok {
		t.Fatalf("active session missing from %+v", refs)
	}
	live, ok := got[archivedLive.String()]
	if !ok {
		t.Fatalf("archived session with a live run missing from %+v", refs)
	}
	if !live.Archived || live.RunStatus != "pending" {
		t.Fatalf("archived live reference = %+v, want archived pending", live)
	}
	if _, ok := got[archived.String()]; ok {
		t.Fatalf("archived session without a live run still blocks: %+v", refs)
	}
	if _, ok := got[foreignBound.String()]; ok {
		t.Fatalf("foreign tenant session leaked into references: %+v", refs)
	}

	agentRefs, err := st.SessionsReferencingAgent(ctx, tenant, "agent_busy")
	if err != nil {
		t.Fatalf("agent references: %v", err)
	}
	if len(agentRefs) != 1 || agentRefs[0].ID != agentBound.String() {
		t.Fatalf("agent references = %+v, want the agent-bound session", agentRefs)
	}
	// The team-bound sessions are not agent references.
	if refs, err := st.SessionsReferencingAgent(ctx, tenant, "team_busy"); err != nil || len(refs) != 0 {
		t.Fatalf("team id leaked into agent references: %+v err=%v", refs, err)
	}

	// A settled run stops blocking an archived session.
	if _, err := st.Pool().Exec(ctx,
		`UPDATE runs SET status = 'done' WHERE tenant_id = $1 AND session_id = $2`,
		tenant, archivedLive); err != nil {
		t.Fatalf("settle run: %v", err)
	}
	refs, err = st.SessionsReferencingTeam(ctx, tenant, "team_busy")
	if err != nil || len(refs) != 1 || refs[0].ID != active.String() {
		t.Fatalf("after settle refs = %+v err=%v, want only the active session", refs, err)
	}

	// Unbind paths: archive the last active session, then a model-only input
	// clears the agent binding. Both leave the projection row readable.
	if found, err := st.ArchiveSession(ctx, tenant, active); err != nil || !found {
		t.Fatalf("archive active: found=%v err=%v", found, err)
	}
	if refs, err := st.SessionsReferencingTeam(ctx, tenant, "team_busy"); err != nil || len(refs) != 0 {
		t.Fatalf("archived sessions still block team: %+v err=%v", refs, err)
	}
	if _, err := st.SetSessionBinding(ctx, tenant, agentBound, "", "", 2); err != nil {
		t.Fatalf("model-only clear: %v", err)
	}
	if refs, err := st.SessionsReferencingAgent(ctx, tenant, "agent_busy"); err != nil || len(refs) != 0 {
		t.Fatalf("cleared agent binding still blocks: %+v err=%v", refs, err)
	}
}

// TestSessionBindingConcurrentNewestWins pins the concurrency rule: N writers
// racing on one session leave the highest seq as the binding, never whichever
// goroutine happened to write last.
func TestSessionBindingConcurrentNewestWins(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant, session := uuid.New(), uuid.New()
	cleanupSessionBindingTenant(t, st, tenant)
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	const writers = 32
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			agentID := fmt.Sprintf("agent_%02d", i)
			if _, err := st.SetSessionBinding(ctx, tenant, session, agentID, "", int64(i+1)); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent binding write: %v", err)
	}
	binding, found, err := st.GetSessionBinding(ctx, tenant, session)
	if err != nil || !found {
		t.Fatalf("get binding: found=%v err=%v", found, err)
	}
	if binding.Seq != writers || binding.AgentID != fmt.Sprintf("agent_%02d", writers-1) {
		t.Fatalf("binding = %+v, want seq %d from agent_%02d", binding, writers, writers-1)
	}
}
