package store

// PostgreSQL tests for the agent and agent-team rows (change
// keel-agent-team-v1, P2/P3). They need KEEL_TEST_DSN and skip otherwise, like
// the rest of the store suite; the HTTP layer's contract is covered without a
// database in internal/httpapi.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func testAgentRecord(tenantID uuid.UUID, id, name string) AgentRecord {
	document, _ := json.Marshal(map[string]any{"id": id, "name": name})
	return AgentRecord{TenantID: tenantID, ID: id, Document: document}
}

func testTeamRecord(t *testing.T, tenantID uuid.UUID, id, name, captain string, revision int64, extra map[string]any) TeamRecord {
	t.Helper()
	document := map[string]any{
		"id":      id,
		"name":    name,
		"captain": map[string]any{"agentId": captain},
	}
	for key, value := range extra {
		document[key] = value
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return TeamRecord{TenantID: tenantID, ID: id, Revision: revision, ContentHash: "hash-" + id, Document: raw}
}

// TestAgentTeamSchemaContract pins the migration/query agreement whose absence
// produced a real defect: a dev database already held an `agents` table with an
// earlier shape, so CREATE TABLE IF NOT EXISTS skipped creation and every query
// failed at request time with `column "document" does not exist` (SQLSTATE
// 42703). The test reads the columns the SQL actually names and then runs the
// list/insert path against the real table, so a migration that stops producing
// these columns fails here instead of in a running deployment.
//
// It runs through store.Open, i.e. the same schema.sql migration keel serve
// applies at boot; there is no test-only DDL anywhere.
func TestAgentTeamSchemaContract(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	required := map[string][]string{
		"agents":      {"tenant_id", "agent_id", "document", "archived", "created_at", "updated_at"},
		"agent_teams": {"tenant_id", "team_id", "revision", "content_hash", "document", "archived", "created_at", "updated_at"},
	}
	for table, columns := range required {
		for _, column := range columns {
			var exists bool
			if err := st.Pool().QueryRow(ctx,
				`SELECT EXISTS (
				   SELECT 1 FROM information_schema.columns
				    WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2)`,
				table, column).Scan(&exists); err != nil {
				t.Fatalf("read %s.%s: %v", table, column, err)
			}
			if !exists {
				t.Fatalf("%s.%s is missing after migration: the store queries name a column the schema does not create", table, column)
			}
		}
	}

	// The round trip the HTTP surface performs, against the migrated table.
	tenant := uuid.New()
	t.Cleanup(func() {
		_, _ = st.DeleteAgent(ctx, tenant, "agent_contract")
		_, _ = st.DeleteTeam(ctx, tenant, "team_contract")
	})
	if _, err := st.CreateAgent(ctx, testAgentRecord(tenant, "agent_contract", "contract")); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	if agents, err := st.ListAgents(ctx, tenant); err != nil || len(agents) != 1 {
		t.Fatalf("list agents = %d err=%v", len(agents), err)
	}
	team := testTeamRecord(t, tenant, "team_contract", "contract", "agent_contract", 1, nil)
	if _, err := st.CreateTeam(ctx, team); err != nil {
		t.Fatalf("insert team: %v", err)
	}
	if teams, err := st.ListTeams(ctx, tenant); err != nil || len(teams) != 1 {
		t.Fatalf("list teams = %d err=%v", len(teams), err)
	}
	if references, err := st.TeamsReferencingAgent(ctx, tenant, "agent_contract"); err != nil || len(references) != 1 {
		t.Fatalf("teams referencing agent = %+v err=%v", references, err)
	}
}

func TestAgentRecordsCRUDAndTenantIsolation(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	owner, other := uuid.New(), uuid.New()
	t.Cleanup(func() {
		_, _ = st.DeleteAgent(ctx, owner, "agent_a")
		_, _ = st.DeleteAgent(ctx, owner, "agent_b")
		_, _ = st.DeleteAgent(ctx, other, "agent_a")
	})

	created, err := st.CreateAgent(ctx, testAgentRecord(owner, "agent_a", "A"))
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	// A create never overwrites: the second insert reports created=false and the
	// stored document is untouched.
	if again, err := st.CreateAgent(ctx, testAgentRecord(owner, "agent_a", "A2")); err != nil || again {
		t.Fatalf("duplicate create: created=%v err=%v, want false", again, err)
	}
	record, found, err := st.GetAgent(ctx, owner, "agent_a")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if !json.Valid(record.Document) || record.CreatedAt.IsZero() {
		t.Fatalf("stored record = %+v", record)
	}

	updated := testAgentRecord(owner, "agent_a", "A updated")
	updated.Archived = true
	if found, err := st.UpdateAgent(ctx, updated); err != nil || !found {
		t.Fatalf("update: found=%v err=%v", found, err)
	}
	record, _, err = st.GetAgent(ctx, owner, "agent_a")
	if err != nil {
		t.Fatal(err)
	}
	if !record.Archived {
		t.Fatal("archived column was not updated")
	}

	if _, err := st.CreateAgent(ctx, testAgentRecord(owner, "agent_b", "B")); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListAgents(ctx, owner)
	if err != nil || len(list) != 2 || list[0].ID != "agent_a" || list[1].ID != "agent_b" {
		t.Fatalf("list = %+v err=%v, want the two agents ordered by id", list, err)
	}

	// Every query is tenant-scoped: another tenant sees nothing and cannot
	// update or delete the owner's row.
	if _, found, err := st.GetAgent(ctx, other, "agent_a"); err != nil || found {
		t.Fatalf("foreign get: found=%v err=%v", found, err)
	}
	if list, err := st.ListAgents(ctx, other); err != nil || len(list) != 0 {
		t.Fatalf("foreign list = %+v err=%v", list, err)
	}
	if found, err := st.UpdateAgent(ctx, testAgentRecord(other, "agent_a", "hijack")); err != nil || found {
		t.Fatalf("foreign update: found=%v err=%v", found, err)
	}
	if deleted, err := st.DeleteAgent(ctx, other, "agent_a"); err != nil || deleted {
		t.Fatalf("foreign delete: deleted=%v err=%v", deleted, err)
	}

	if deleted, err := st.DeleteAgent(ctx, owner, "agent_a"); err != nil || !deleted {
		t.Fatalf("delete: deleted=%v err=%v", deleted, err)
	}
	if _, found, err := st.GetAgent(ctx, owner, "agent_a"); err != nil || found {
		t.Fatalf("get after delete: found=%v err=%v", found, err)
	}
	if deleted, err := st.DeleteAgent(ctx, owner, "agent_a"); err != nil || deleted {
		t.Fatalf("second delete: deleted=%v err=%v, want false", deleted, err)
	}
}

func TestAgentRecordsRejectBadInput(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if _, err := st.CreateAgent(ctx, AgentRecord{TenantID: tenant, ID: "agent_x", Document: json.RawMessage(`{`)}); err == nil {
		t.Fatal("invalid JSON document was accepted")
	}
	if _, err := st.CreateAgent(ctx, AgentRecord{TenantID: tenant, ID: "", Document: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("empty id was accepted")
	}
	if _, err := st.CreateAgent(ctx, AgentRecord{TenantID: uuid.Nil, ID: "agent_x", Document: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("nil tenant was accepted")
	}
}

func TestTeamRecordsCRUDRevisionGuardAndReferences(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	ids := []string{"agent_captain", "agent_member", "agent_inline", "agent_unused"}
	t.Cleanup(func() {
		for _, id := range []string{"team_alpha", "team_beta"} {
			_, _ = st.DeleteTeam(ctx, tenant, id)
		}
		for _, id := range ids {
			_, _ = st.DeleteAgent(ctx, tenant, id)
		}
	})
	for _, id := range ids {
		if _, err := st.CreateAgent(ctx, testAgentRecord(tenant, id, id)); err != nil {
			t.Fatal(err)
		}
	}

	// team_alpha seats agent_member as a member; team_beta puts agent_inline in
	// an inline definition, which is not a reference to the library entry.
	alpha := testTeamRecord(t, tenant, "team_alpha", "Alpha", "agent_captain", 1, map[string]any{
		"members": []map[string]any{{"agentId": "agent_member"}},
	})
	if created, err := st.CreateTeam(ctx, alpha); err != nil || !created {
		t.Fatalf("create team: created=%v err=%v", created, err)
	}
	beta := testTeamRecord(t, tenant, "team_beta", "Beta", "agent_captain", 1, map[string]any{
		"members": []map[string]any{{
			"agentId": "agent_inline",
			"inline":  map[string]any{"id": "agent_inline", "name": "inline"},
		}},
	})
	if created, err := st.CreateTeam(ctx, beta); err != nil || !created {
		t.Fatalf("create beta: created=%v err=%v", created, err)
	}
	if created, err := st.CreateTeam(ctx, alpha); err != nil || created {
		t.Fatalf("duplicate team create: created=%v err=%v, want false", created, err)
	}

	record, found, err := st.GetTeam(ctx, tenant, "team_alpha")
	if err != nil || !found {
		t.Fatalf("get team: found=%v err=%v", found, err)
	}
	if record.Revision != 1 || record.ContentHash != "hash-team_alpha" || !json.Valid(record.Document) {
		t.Fatalf("stored team = %+v", record)
	}

	// The revision guard is optimistic: a stale expectation is refused, a fresh
	// one is applied. The update carries the same member seat, so the reference
	// lookup below still sees it.
	stale := testTeamRecord(t, tenant, "team_alpha", "Alpha v2", "agent_captain", 2, map[string]any{
		"members": []map[string]any{{"agentId": "agent_member"}},
	})
	if updated, err := st.UpdateTeam(ctx, stale, 0); err != nil || updated {
		t.Fatalf("stale update: updated=%v err=%v, want false", updated, err)
	}
	if updated, err := st.UpdateTeam(ctx, stale, 1); err != nil || !updated {
		t.Fatalf("fresh update: updated=%v err=%v", updated, err)
	}
	record, _, err = st.GetTeam(ctx, tenant, "team_alpha")
	if err != nil {
		t.Fatal(err)
	}
	if record.Revision != 2 || record.ContentHash != "hash-team_alpha" {
		t.Fatalf("after update: revision %d hash %s", record.Revision, record.ContentHash)
	}
	// Replaying the same expectation finds revision 2, not 1.
	if updated, err := st.UpdateTeam(ctx, stale, 1); err != nil || updated {
		t.Fatalf("replayed update: updated=%v err=%v, want false", updated, err)
	}

	references, err := st.TeamsReferencingAgent(ctx, tenant, "agent_member")
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 1 || references[0].ID != "team_alpha" || references[0].Name != "Alpha v2" {
		t.Fatalf("member references = %+v", references)
	}
	references, err = st.TeamsReferencingAgent(ctx, tenant, "agent_captain")
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 2 {
		t.Fatalf("captain references = %+v, want both teams", references)
	}
	// An inline definition is self-contained: it is not a library reference.
	references, err = st.TeamsReferencingAgent(ctx, tenant, "agent_inline")
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 0 {
		t.Fatalf("inline references = %+v, want none", references)
	}
	references, err = st.TeamsReferencingAgent(ctx, tenant, "agent_unused")
	if err != nil || len(references) != 0 {
		t.Fatalf("unused references = %+v err=%v", references, err)
	}

	if list, err := st.ListTeams(ctx, tenant); err != nil || len(list) != 2 || list[0].ID != "team_alpha" {
		t.Fatalf("list teams = %+v err=%v", list, err)
	}

	// Another tenant sees and mutates nothing.
	other := uuid.New()
	if _, found, err := st.GetTeam(ctx, other, "team_alpha"); err != nil || found {
		t.Fatalf("foreign get: found=%v err=%v", found, err)
	}
	if deleted, err := st.DeleteTeam(ctx, other, "team_alpha"); err != nil || deleted {
		t.Fatalf("foreign delete: deleted=%v err=%v", deleted, err)
	}

	if deleted, err := st.DeleteTeam(ctx, tenant, "team_alpha"); err != nil || !deleted {
		t.Fatalf("delete: deleted=%v err=%v", deleted, err)
	}
	if _, found, err := st.GetTeam(ctx, tenant, "team_alpha"); err != nil || found {
		t.Fatalf("get after delete: found=%v err=%v", found, err)
	}
}

// TestProvidersReferencingProvider pins the query behind the provider
// registry's deletion protection: an agent's defaultModel, a team's
// defaults.modelOverride and an inline member agent's defaultModel are the
// three bindings that name a provider; a team whose members reference agents
// from the library, or one naming a different provider, does not count.
func TestProvidersReferencingProvider(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	t.Cleanup(func() {
		_, _ = st.DeleteAgent(ctx, tenant, "agent_ref_provider")
		_, _ = st.DeleteTeam(ctx, tenant, "team_ref_provider")
		_, _ = st.DeleteTeam(ctx, tenant, "team_inline_provider")
	})

	agentDocument, _ := json.Marshal(map[string]any{
		"id": "agent_ref_provider", "name": "provider referrer",
		"defaultModel": map[string]any{"providerId": "prov_ref", "modelId": "m1"},
	})
	if _, err := st.CreateAgent(ctx, AgentRecord{TenantID: tenant, ID: "agent_ref_provider", Document: agentDocument}); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	teamDocument, _ := json.Marshal(map[string]any{
		"id": "team_ref_provider", "name": "team referrer",
		"captain": map[string]any{"agentId": "agent_ref_provider"},
		"defaults": map[string]any{
			"modelOverride": map[string]any{"providerId": "prov_ref", "modelId": "m2"},
		},
	})
	if _, err := st.CreateTeam(ctx, TeamRecord{
		TenantID: tenant, ID: "team_ref_provider", Revision: 1, ContentHash: "hash-team_ref_provider", Document: teamDocument,
	}); err != nil {
		t.Fatalf("insert team: %v", err)
	}
	inlineDocument, _ := json.Marshal(map[string]any{
		"id": "team_inline_provider", "name": "inline referrer",
		"captain": map[string]any{"agentId": "agent_inline"},
		"members": []any{map[string]any{
			"agentId": "agent_inline",
			"inline": map[string]any{
				"id": "agent_inline", "name": "inline agent",
				"defaultModel": map[string]any{"providerId": "prov_ref", "modelId": "m3"},
			},
		}},
	})
	if _, err := st.CreateTeam(ctx, TeamRecord{
		TenantID: tenant, ID: "team_inline_provider", Revision: 1, ContentHash: "hash-team_inline_provider", Document: inlineDocument,
	}); err != nil {
		t.Fatalf("insert inline team: %v", err)
	}

	references, err := st.ProvidersReferencingProvider(ctx, tenant, "prov_ref")
	if err != nil {
		t.Fatalf("references: %v", err)
	}
	if len(references) != 3 {
		t.Fatalf("references = %+v, want 3", references)
	}
	byKey := map[string]ProviderReference{}
	for _, reference := range references {
		byKey[reference.Kind+"/"+reference.ID] = reference
	}
	if got := byKey["agent/agent_ref_provider"]; got.Field != "defaultModel" || got.Name != "provider referrer" {
		t.Fatalf("agent reference = %+v", got)
	}
	if got := byKey["team/team_ref_provider"]; got.Field != "defaults.modelOverride" || got.Name != "team referrer" {
		t.Fatalf("team reference = %+v", got)
	}
	if got := byKey["team/team_inline_provider"]; got.Field != "members[].inline.defaultModel" {
		t.Fatalf("inline reference = %+v", got)
	}

	// Another provider id (and a tenant without a row) answers nothing.
	if other, err := st.ProvidersReferencingProvider(ctx, tenant, "prov_other"); err != nil || len(other) != 0 {
		t.Fatalf("references for an unrelated provider = %+v (%v)", other, err)
	}
	if none, err := st.ProvidersReferencingProvider(ctx, uuid.New(), "prov_ref"); err != nil || len(none) != 0 {
		t.Fatalf("a foreign tenant must see nothing: %+v (%v)", none, err)
	}
}
