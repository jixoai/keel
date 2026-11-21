package httpapi

// Real-database tests for the session binding projection and the registry's
// session reference guard (change keel-agent-team-v1, P3). The map-backed tests
// in agents_test.go pin the handler contract; these pin the SQL the contract
// depends on: the projection is written from the committed input (including a
// deduped replay), the newest seq wins under concurrent input, and the delete/
// archive guards read the same rows the store query returns.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/agentteam"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/modelcfg"
	"github.com/jixoai/keel/internal/sessionbinding"
	"github.com/jixoai/keel/internal/store"
)

// newSessionRefServer wires the HTTP surface and the registry onto the real test
// database, with the input gate accepting the shape (existence of agents/teams
// is covered by servicehost tests; these tests are about the projection).
func newSessionRefServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	kernelLog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatalf("apply kernel migration: %v", err)
	}
	kern := kernel.New(kernelLog, kernel.Options{})
	hs := httptest.NewServer(New(Config{
		Store:      st,
		Edge:       kern,
		Events:     kern,
		AgentTeams: st,
		ValidateSelection: func(raw json.RawMessage) error {
			return modelcfg.ValidateSelectionFact(raw)
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))
	t.Cleanup(hs.Close)
	return hs, st
}

func cleanupSessionRefRows(t *testing.T, st *store.Store, tenants ...uuid.UUID) {
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

func newSessionRefTenant(t *testing.T, st *store.Store) uuid.UUID {
	t.Helper()
	tenant := uuid.New()
	if err := st.CreateTenant(context.Background(), tenant, "session-ref-"+tenant.String()[:8]); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	cleanupSessionRefRows(t, st, tenant)
	return tenant
}

func newSessionRefSession(t *testing.T, st *store.Store, tenant uuid.UUID) uuid.UUID {
	t.Helper()
	session := uuid.New()
	if err := st.CreateSession(context.Background(), tenant, session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return session
}

func postInput(t *testing.T, hs *httptest.Server, tenant, session string, body map[string]any) int {
	t.Helper()
	status, err := postInputStatus(hs, tenant, session, body)
	if err != nil {
		t.Fatalf("post input: %v", err)
	}
	return status
}

// postInputStatus is the goroutine-safe form: it never calls testing.T (a Fatal
// from a worker goroutine is illegal), so the concurrency test can fan out.
func postInputStatus(hs *httptest.Server, tenant, session string, body map[string]any) (int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequest(http.MethodPost,
		hs.URL+"/v1/sessions/"+session+"/input", bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	request.Header.Set("X-Tenant-ID", tenant)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode, nil
}

// TestInputProjectsNewestSelection pins the projection write rules through the
// real commit path: agent binding, team-wins, model-only clear, an input without
// a selection leaving the projection alone, and a deduped replay projecting the
// COMMITTED event rather than the request body.
func TestInputProjectsNewestSelection(t *testing.T) {
	hs, st := newSessionRefServer(t)
	tenant := newSessionRefTenant(t, st)
	session := newSessionRefSession(t, st, tenant)
	tenantText, sessionText := tenant.String(), session.String()
	ctx := context.Background()

	step := func(name string, body map[string]any, wantAgent, wantTeam string, wantSeq int64) {
		t.Helper()
		if status := postInput(t, hs, tenantText, sessionText, body); status != http.StatusCreated && status != http.StatusOK {
			t.Fatalf("%s: status %d", name, status)
		}
		binding, found, err := st.GetSessionBinding(ctx, tenant, session)
		if err != nil || !found {
			t.Fatalf("%s: get binding found=%v err=%v", name, found, err)
		}
		if binding.AgentID != wantAgent || binding.TeamID != wantTeam || binding.Seq != wantSeq {
			t.Fatalf("%s: binding = %+v, want agent=%q team=%q seq=%d", name, binding, wantAgent, wantTeam, wantSeq)
		}
	}

	step("agent", map[string]any{"text": "a", "requestId": "ref-1",
		"selection": map[string]any{"agentId": "agent_a"}}, "agent_a", "", 1)
	step("team wins", map[string]any{"text": "b", "requestId": "ref-2",
		"selection": map[string]any{"agentId": "agent_lost", "teamId": "team_t"}}, "", "team_t", 2)
	step("model only clears", map[string]any{"text": "c", "requestId": "ref-3",
		"selection": map[string]any{"providerId": "p", "modelId": "m"}}, "", "", 3)
	// An input without a selection leaves the newest selection-bearing fact
	// authoritative, exactly like the runner's runSelection. (An explicit null is
	// refused by the HTTP gate before the commit, so it never reaches the
	// projection.)
	step("absent leaves", map[string]any{"text": "d", "requestId": "ref-4"}, "", "", 3)

	// A replayed requestId does not write a second event. An identical replay is
	// accepted as deduped; a divergent replay is the kernel's 409 key conflict.
	// Either way the projection must follow the committed payload, never the
	// request body of the replay.
	dedupeBody := func(agentID string) map[string]any {
		return map[string]any{"text": "dedupe", "requestId": "ref-dedupe",
			"selection": map[string]any{"agentId": agentID}}
	}
	if status := postInput(t, hs, tenantText, sessionText, dedupeBody("agent_committed")); status != http.StatusCreated {
		t.Fatalf("dedupe first: status %d", status)
	}
	if status := postInput(t, hs, tenantText, sessionText, dedupeBody("agent_committed")); status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("identical replay: status %d", status)
	}
	if status := postInput(t, hs, tenantText, sessionText, dedupeBody("agent_request_only")); status != http.StatusConflict {
		t.Fatalf("divergent replay: status %d, want 409 key conflict", status)
	}
	binding, _, _ := st.GetSessionBinding(ctx, tenant, session)
	if binding.AgentID != "agent_committed" {
		t.Fatalf("deduped replay projected the request, not the ledger: %+v", binding)
	}
}

// TestDeleteAndArchiveUseRealProjection pins the guard on real rows: active
// binding blocks delete/archive, archiving the session unblocks, a model-only
// input unbinds without archiving, and an archived session with a live run keeps
// blocking until the run settles.
func TestDeleteAndArchiveUseRealProjection(t *testing.T) {
	hs, st := newSessionRefServer(t)
	tenant := newSessionRefTenant(t, st)
	tenantText := tenant.String()
	ctx := context.Background()

	// One captain, one team, one session bound to the team through /input.
	var captain AgentView
	if status := doJSON(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/agents", tenantText,
		map[string]any{"name": "Ref 队长"}, &captain); status != http.StatusCreated {
		t.Fatalf("create agent: status %d", status)
	}
	var team AgentTeamView
	if status := doJSON(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams", tenantText,
		map[string]any{"name": "Ref 队", "captain": agentteam.TeamMember{AgentID: captain.ID}}, &team); status != http.StatusCreated {
		t.Fatalf("create team: status %d", status)
	}
	session := newSessionRefSession(t, st, tenant)
	if status := postInput(t, hs, tenantText, session.String(), map[string]any{
		"text": "bind team", "requestId": "bind-1",
		"selection": map[string]any{"teamId": team.ID},
	}); status != http.StatusCreated {
		t.Fatalf("bind team input: status %d", status)
	}
	// A team selection records the team, not the captain: the agent needs its own
	// session binding to exercise the agent half of the guard.
	sessionAgent := newSessionRefSession(t, st, tenant)
	if status := postInput(t, hs, tenantText, sessionAgent.String(), map[string]any{
		"text": "bind agent", "requestId": "bind-2",
		"selection": map[string]any{"agentId": captain.ID},
	}); status != http.StatusCreated {
		t.Fatalf("bind agent input: status %d", status)
	}

	var refusal sessionRefusal
	for _, request := range []struct {
		name       string
		method     string
		path       string
		wantCode   string
		wantSessID string
	}{
		{"delete team", http.MethodDelete, "/v1/teams/" + team.ID, "team_in_use", session.String()},
		{"archive team", http.MethodPost, "/v1/teams/" + team.ID + "/archive", "team_in_use", session.String()},
		{"delete agent", http.MethodDelete, "/v1/agents/" + captain.ID, "agent_in_use", sessionAgent.String()},
		{"archive agent", http.MethodPost, "/v1/agents/" + captain.ID + "/archive", "agent_in_use", sessionAgent.String()},
	} {
		refusal = sessionRefusal{}
		status := doJSON(t, http.DefaultClient, request.method, hs.URL+request.path, tenantText, nil, &refusal)
		if status != http.StatusConflict || refusal.Code != request.wantCode {
			t.Fatalf("%s: status %d code %q, want 409 %s", request.name, status, refusal.Code, request.wantCode)
		}
		found := false
		for _, reference := range refusal.References {
			if reference["kind"] == "session" && reference["id"] == request.wantSessID {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s references = %+v, want session %s", request.name, refusal.References, request.wantSessID)
		}
	}

	// Archive both sessions: the session guard stops blocking. The agent still
	// has a team seat, which blocks delete but not archive (archive is the
	// documented remedy for a seated agent).
	for _, id := range []uuid.UUID{session, sessionAgent} {
		if found, err := st.ArchiveSession(ctx, tenant, id); err != nil || !found {
			t.Fatalf("archive session %s: found=%v err=%v", id, found, err)
		}
	}
	// The inputs queued runs and this test server has no worker, so the archived
	// sessions still own live (pending) runs and must keep blocking. Settle them:
	// that is the boundary the reference query documents.
	refusal = sessionRefusal{}
	status := doJSON(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/agents/"+captain.ID+"/archive", tenantText, nil, &refusal)
	if status != http.StatusConflict || refusal.Code != "agent_in_use" {
		t.Fatalf("archive agent with archived-live sessions: status %d code %q, want 409", status, refusal.Code)
	}
	if _, err := st.Pool().Exec(ctx,
		`UPDATE runs SET status = 'done' WHERE tenant_id = $1 AND session_id = ANY($2)`,
		tenant, []uuid.UUID{session, sessionAgent}); err != nil {
		t.Fatalf("settle session runs: %v", err)
	}
	var archivedAgent AgentView
	if status := doJSON(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/agents/"+captain.ID+"/archive", tenantText, nil, &archivedAgent); status != http.StatusOK || !archivedAgent.Archived {
		t.Fatalf("archive agent after sessions archived: status %d view %+v", status, archivedAgent)
	}
	refusal = sessionRefusal{}
	status = doJSON(t, http.DefaultClient, http.MethodDelete, hs.URL+"/v1/agents/"+captain.ID, tenantText, nil, &refusal)
	if status != http.StatusConflict || refusal.Code != "agent_in_use" || len(refusal.References) != 1 || refusal.References[0]["id"] != team.ID {
		t.Fatalf("agent delete with a team seat: status %d code %q refs %+v, want team-only 409", status, refusal.Code, refusal.References)
	}
	var archivedTeam AgentTeamView
	if status := doJSON(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/teams/"+team.ID+"/archive", tenantText, nil, &archivedTeam); status != http.StatusOK || !archivedTeam.Archived {
		t.Fatalf("archive team after sessions archived: status %d view %+v", status, archivedTeam)
	}
	if status := doJSON(t, http.DefaultClient, http.MethodDelete, hs.URL+"/v1/teams/"+team.ID, tenantText, nil, nil); status != http.StatusOK {
		t.Fatalf("delete archived team: status %d", status)
	}
	if status := doJSON(t, http.DefaultClient, http.MethodDelete, hs.URL+"/v1/agents/"+captain.ID, tenantText, nil, nil); status != http.StatusOK {
		t.Fatalf("delete archived agent after team removal: status %d", status)
	}

	// A second session demonstrates the unbind path without archiving: a
	// model-only input clears the projection and frees the agent.
	agentOnly := createAgentFixture(t, hs.URL, tenantText, "独立 Agent", nil)
	session2 := newSessionRefSession(t, st, tenant)
	if status := postInput(t, hs, tenantText, session2.String(), map[string]any{
		"text": "bind agent", "requestId": "bind-agent",
		"selection": map[string]any{"agentId": agentOnly.ID},
	}); status != http.StatusCreated {
		t.Fatalf("bind agent input: status %d", status)
	}
	if status := postInput(t, hs, tenantText, session2.String(), map[string]any{
		"text": "unbind", "requestId": "unbind-agent",
		"selection": map[string]any{"providerId": "p", "modelId": "m"},
	}); status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("unbind input: status %d", status)
	}
	if status := doJSON(t, http.DefaultClient, http.MethodDelete, hs.URL+"/v1/agents/"+agentOnly.ID, tenantText, nil, nil); status != http.StatusOK {
		t.Fatalf("delete unbound agent: status %d", status)
	}

	// A third session proves the archived-but-live-run boundary on real SQL.
	var liveAgent AgentView
	if status := doJSON(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/agents", tenantText,
		map[string]any{"name": "Live Agent"}, &liveAgent); status != http.StatusCreated {
		t.Fatalf("create live agent: status %d", status)
	}
	session3 := newSessionRefSession(t, st, tenant)
	if status := postInput(t, hs, tenantText, session3.String(), map[string]any{
		"text": "live", "requestId": "bind-live",
		"selection": map[string]any{"agentId": liveAgent.ID},
	}); status != http.StatusCreated {
		t.Fatalf("bind live input: status %d", status)
	}
	if err := st.EnqueueRun(ctx, tenant, session3); err != nil {
		t.Fatalf("enqueue live run: %v", err)
	}
	if found, err := st.ArchiveSession(ctx, tenant, session3); err != nil || !found {
		t.Fatalf("archive live session: found=%v err=%v", found, err)
	}
	refusal = sessionRefusal{}
	status = doJSON(t, http.DefaultClient, http.MethodDelete, hs.URL+"/v1/agents/"+liveAgent.ID, tenantText, nil, &refusal)
	if status != http.StatusConflict || refusal.Code != "agent_in_use" {
		t.Fatalf("live archived session: status %d code %q, want 409", status, refusal.Code)
	}
	if len(refusal.References) != 1 || refusal.References[0]["archived"] != true ||
		refusal.References[0]["runStatus"] != "pending" {
		t.Fatalf("live archived reference = %+v", refusal.References)
	}
	if _, err := st.Pool().Exec(ctx,
		`UPDATE runs SET status = 'done' WHERE tenant_id = $1 AND session_id = $2`,
		tenant, session3); err != nil {
		t.Fatalf("settle live run: %v", err)
	}
	if status := doJSON(t, http.DefaultClient, http.MethodDelete, hs.URL+"/v1/agents/"+liveAgent.ID, tenantText, nil, nil); status != http.StatusOK {
		t.Fatalf("delete after run settles: status %d", status)
	}
}

// TestConcurrentInputsProjectNewestSeq pins the concurrency rule end to end:
// concurrent /input commits on one session leave the binding equal to the
// newest committed user.message selection, not the last HTTP response to land.
func TestConcurrentInputsProjectNewestSeq(t *testing.T) {
	hs, st := newSessionRefServer(t)
	tenant := newSessionRefTenant(t, st)
	tenantText := tenant.String()
	ctx := context.Background()

	const rounds, writers = 5, 8
	for round := 0; round < rounds; round++ {
		session := newSessionRefSession(t, st, tenant)
		var wg sync.WaitGroup
		statuses := make(chan int, writers)
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				status, err := postInputStatus(hs, tenantText, session.String(), map[string]any{
					"text":      fmt.Sprintf("round %d writer %d", round, i),
					"requestId": fmt.Sprintf("conc-%d-%d", round, i),
					"selection": map[string]any{"agentId": fmt.Sprintf("agent_%d_%d", round, i)},
				})
				if err != nil {
					statuses <- -1
					return
				}
				statuses <- status
			}(i)
		}
		wg.Wait()
		close(statuses)
		for status := range statuses {
			if status != http.StatusCreated && status != http.StatusOK {
				t.Fatalf("round %d: input status %d", round, status)
			}
		}

		events, err := st.LoadEvents(ctx, tenant, session, 0, 1000)
		if err != nil {
			t.Fatalf("load events: %v", err)
		}
		var newestSeq int64
		var newestAgent string
		for _, event := range events {
			if event.Kind != "user.message" {
				continue
			}
			var payload struct {
				Selection json.RawMessage `json:"selection"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("decode user.message: %v", err)
			}
			if !presentRawJSON(payload.Selection) {
				continue
			}
			var selection struct {
				AgentID string `json:"agentId"`
			}
			if err := json.Unmarshal(payload.Selection, &selection); err != nil {
				t.Fatalf("decode selection: %v", err)
			}
			newestSeq, newestAgent = event.Seq, selection.AgentID
		}
		if newestSeq == 0 {
			t.Fatalf("round %d: no selection-bearing user.message", round)
		}
		binding, found, err := st.GetSessionBinding(ctx, tenant, session)
		if err != nil || !found {
			t.Fatalf("round %d: get binding found=%v err=%v", round, found, err)
		}
		if binding.Seq != newestSeq || binding.AgentID != newestAgent {
			t.Fatalf("round %d: binding = %+v, want seq %d agent %q (newest committed input)",
				round, binding, newestSeq, newestAgent)
		}
	}
}

// sessionRefRuns adapts the store's run row to agentext.RunStateReader for the
// spawn integration test.
type sessionRefRuns struct{ *store.Store }

func (r sessionRefRuns) RunState(ctx context.Context, tenantID, sessionID uuid.UUID) (agentext.RunState, error) {
	run, err := r.GetRun(ctx, tenantID, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNoRun) {
			return agentext.RunState{}, agentext.ErrNoRun
		}
		return agentext.RunState{}, err
	}
	return agentext.RunState{Status: run.Status, Attempts: run.Attempts, CursorSeq: run.CursorSeq, LastError: run.LastError}, nil
}

// TestSpawnedSubagentBlocksAgentDeletion pins the spawn path end to end: a child
// created by agentext.TurnStarter projects its selection onto the child session,
// and the HTTP delete guard therefore refuses the Agent with 409 agent_in_use.
// This is the regression the B-axis review found (TurnStarter bypassed the
// projection, so a live child session could not block its Agent).
func TestSpawnedSubagentBlocksAgentDeletion(t *testing.T) {
	hs, st := newSessionRefServer(t)
	tenant := newSessionRefTenant(t, st)
	tenantText := tenant.String()
	parent := newSessionRefSession(t, st, tenant)
	ctx := context.Background()

	var agent AgentView
	if status := doJSON(t, http.DefaultClient, http.MethodPost, hs.URL+"/v1/agents", tenantText,
		map[string]any{"id": "agent_spawn_ref", "name": "Spawn Agent"}, &agent); status != http.StatusCreated {
		t.Fatalf("create agent: status %d", status)
	}

	klog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatalf("apply kernel migration: %v", err)
	}
	kern := kernel.New(klog, kernel.Options{})
	runner := agentext.NewSubagentRunner(agentext.SubagentOptions{
		Gate:   st,
		Edge:   kern,
		Events: kern,
		Runs:   sessionRefRuns{st},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ValidateSelection: func(raw json.RawMessage) error {
			return modelcfg.ValidateSelectionFact(raw)
		},
		Bindings: sessionbinding.New(st),
	})
	record, err := runner.SpawnAsync(ctx, agentext.SpawnRequest{
		TenantID:        tenant,
		ParentSessionID: parent,
		Task:            "child task",
		AgentID:         agent.ID,
	})
	if err != nil {
		t.Fatalf("SpawnAsync: %v", err)
	}
	child, err := uuid.Parse(record.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	binding, found, err := st.GetSessionBinding(ctx, tenant, child)
	if err != nil || !found {
		t.Fatalf("child binding: found=%v err=%v", found, err)
	}
	events, err := st.LoadEvents(ctx, tenant, child, 0, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("child events = %d err=%v, want one user.message", len(events), err)
	}
	if binding.AgentID != agent.ID || binding.Seq != events[0].Seq {
		t.Fatalf("child binding = %+v, want agent %s seq %d", binding, agent.ID, events[0].Seq)
	}

	// The guard must see the spawned child, not only sessions started through
	// POST /input.
	var refusal sessionRefusal
	status := doJSON(t, http.DefaultClient, http.MethodDelete, hs.URL+"/v1/agents/"+agent.ID, tenantText, nil, &refusal)
	if status != http.StatusConflict || refusal.Code != "agent_in_use" {
		t.Fatalf("delete spawned agent: status %d code %q, want 409 agent_in_use", status, refusal.Code)
	}
	foundRef := false
	for _, reference := range refusal.References {
		if reference["kind"] == "session" && reference["id"] == child.String() {
			foundRef = true
		}
	}
	if !foundRef {
		t.Fatalf("references = %+v, want the child session %s", refusal.References, child)
	}
}
