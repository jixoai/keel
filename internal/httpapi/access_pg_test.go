package httpapi_test

// Real-PostgreSQL negative controls for the A4.2a route permission gate. The
// fixture is one tenant with an owner account (A), a zero-grant account (B), a
// third account in another tenant, and a Bot-owned session. Requests run
// through the real httpapi edge with the product AgentAPI nested, so a denial
// proves the route matrix + engine, not a handler unit.
//
// The suite is skipped without KEEL_TEST_DSN. The test database is expected to
// be dedicated (the runner creates and drops it); tenants are fresh uuids, so
// packages sharing one database do not collide.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/agentapi"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/httpapi"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/modelcfg"
	"github.com/jixoai/keel/internal/store"
)

type accessPGEnv struct {
	t           *testing.T
	st          *store.Store
	bridge      *accesswire.Bridge
	tenant      uuid.UUID
	other       uuid.UUID
	owner       store.AccountRecord
	zero        store.AccountRecord
	super       store.AccountRecord
	otherAcct   store.AccountRecord
	ownerBot    store.BotRecord
	session     uuid.UUID
	triggers    *agentext.TriggerEngine
	server      *httptest.Server
	ownerRole   store.RoleRecord
	ownerProf   store.ProfileRecord
	memberBot   store.BotRecord
	ownerGrants []store.PermissionGrantRecord
}

type fakeModelSettingsStore struct{ revision int64 }

func (f *fakeModelSettingsStore) GetModelSettings(context.Context, uuid.UUID) (json.RawMessage, int64, time.Time, bool, error) {
	return json.RawMessage(`{}`), f.revision, time.Time{}, false, nil
}

func (f *fakeModelSettingsStore) SetModelSettings(_ context.Context, _ uuid.UUID, _ int64, _ json.RawMessage) (int64, error) {
	f.revision++
	return f.revision, nil
}

func (f *fakeModelSettingsStore) DeleteModelSettings(_ context.Context, _ uuid.UUID, _ int64) (int64, error) {
	f.revision++
	return f.revision, nil
}

type fakeModelRuntime struct{}

func (fakeModelRuntime) DeploymentModels() json.RawMessage { return nil }

func (fakeModelRuntime) ValidateModelsDocument(json.RawMessage) (agentapi.ModelCandidate, []modelcfg.Issue, error) {
	return agentapi.ModelCandidate{}, nil, nil
}

func (fakeModelRuntime) ApplyModels(context.Context, json.RawMessage) error { return nil }

func (fakeModelRuntime) RejectedModelsOverride() (int64, string, bool) { return 0, "", false }

type fakeSecretStore struct{ values map[string][]byte }

func (f *fakeSecretStore) List(context.Context, uuid.UUID) ([]agentapi.SecretMeta, error) {
	out := make([]agentapi.SecretMeta, 0, len(f.values))
	for name := range f.values {
		out = append(out, agentapi.SecretMeta{Name: name, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
	}
	return out, nil
}

func (f *fakeSecretStore) Put(_ context.Context, _ uuid.UUID, name string, value []byte) (time.Time, error) {
	if f.values == nil {
		f.values = map[string][]byte{}
	}
	f.values[name] = append([]byte(nil), value...)
	return time.Now().UTC(), nil
}

func (f *fakeSecretStore) Delete(_ context.Context, _ uuid.UUID, name string) (bool, error) {
	if f.values == nil {
		return false, nil
	}
	_, ok := f.values[name]
	delete(f.values, name)
	return ok, nil
}

type emptyEvents struct{}

func (emptyEvents) LoadEvents(context.Context, kernel.Scope, int64, int) ([]kernel.Event, error) {
	return []kernel.Event{}, nil
}

func (emptyEvents) UnresolvedToolCalls(context.Context, kernel.Scope) ([]kernel.ToolCallRef, error) {
	return nil, nil
}

func newAccessPGEnv(t *testing.T) *accessPGEnv {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	st, err := store.Open(context.Background(), store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	ctx := context.Background()
	env := &accessPGEnv{t: t, st: st, tenant: uuid.New(), other: uuid.New()}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(),
			`UPDATE accounts SET is_super_admin = false WHERE tenant_id = ANY($1)`,
			[]uuid.UUID{env.tenant, env.other})
	})
	for _, tenant := range []uuid.UUID{env.tenant, env.other} {
		if err := st.CreateTenant(ctx, tenant, "a42a-"+tenant.String()[:8]); err != nil {
			t.Fatal(err)
		}
	}
	if err := accesswire.EnsureBuiltinGroups(ctx, st, env.tenant); err != nil {
		t.Fatal(err)
	}
	makeAccount := func(tenant uuid.UUID, name string) store.AccountRecord {
		t.Helper()
		account, err := st.CreateAccount(ctx, store.AccountRecord{
			TenantID: tenant, ID: uuid.New(), Username: name + "-" + tenant.String()[:8],
			DisplayName: name, Kind: "human",
		})
		if err != nil {
			t.Fatal(err)
		}
		return account
	}
	env.owner = makeAccount(env.tenant, "owner")
	env.zero = makeAccount(env.tenant, "zero")
	env.otherAcct = makeAccount(env.other, "outsider")
	super, err := st.CreateAccount(ctx, store.AccountRecord{
		TenantID: env.tenant, ID: uuid.New(), Username: "super-" + env.tenant.String()[:8],
		DisplayName: "super", Kind: "human", IsSuperAdmin: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	env.super = super

	role, err := st.CreateRole(ctx, store.RoleRecord{TenantID: env.tenant, ID: uuid.New(), Name: "Access"})
	if err != nil {
		t.Fatal(err)
	}
	env.ownerRole = role
	profile, err := st.CreateProfile(ctx, store.ProfileRecord{
		TenantID: env.tenant, ID: uuid.New(), AccountID: env.owner.ID, RoleID: role.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	env.ownerProf = profile
	memberProfile, err := st.CreateProfile(ctx, store.ProfileRecord{
		TenantID: env.tenant, ID: uuid.New(), AccountID: env.owner.ID, RoleID: role.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	createBot := func(id string, profileID uuid.UUID, accountID uuid.UUID) store.BotRecord {
		t.Helper()
		bot, err := st.CreateBotWithOwner(ctx, store.BotRecord{
			TenantID: env.tenant, ID: id, Name: id, ProfileID: profileID, ModelKind: "text",
			DefaultModel: json.RawMessage(`{"providerId":"fake","modelId":"fake"}`),
			Harness:      json.RawMessage(`{"tools":[],"context":{"permissions":["session.read","session.write"]}}`),
		}, accountID, env.owner.ID)
		if err != nil {
			t.Fatal(err)
		}
		return bot
	}
	env.ownerBot = createBot("owner-bot", profile.ID, env.owner.ID)
	env.memberBot = createBot("member-bot", memberProfile.ID, env.owner.ID)

	grant := func(account store.AccountRecord, key string, scope access.Scope) {
		t.Helper()
		record, err := st.CreatePermissionGrant(ctx, store.PermissionGrantRecord{
			TenantID: env.tenant, ID: uuid.New(), SubjectType: store.PermissionSubjectAccount,
			SubjectID: account.ID, PermissionKey: key, Scope: string(scope), GrantedBy: env.owner.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		env.ownerGrants = append(env.ownerGrants, record)
	}
	for _, key := range []string{
		access.KeySessionRead, access.KeySessionWrite, access.KeySessionDelete,
		access.KeyBotRead, access.KeyBotManage, access.KeyBotBind, access.KeyBotInvoke,
		access.KeyTeamBotRead, access.KeyTeamBotManage,
	} {
		grant(env.owner, key, access.ScopeOwn)
	}
	for _, key := range []string{
		access.KeyModelRead, access.KeyModelConfigure, access.KeySecretWrite,
		access.KeySecretReadMetadata, access.KeyAppRead, access.KeyBotCreate,
		access.KeyAccountRead, access.KeyPermissionRead, access.KeyTriggerManage,
	} {
		grant(env.owner, key, access.ScopeTenant)
	}
	grant(env.owner, access.KeyTriggerRead, access.ScopeTenant)

	env.session = uuid.New()
	if err := st.CreateOwnedSession(ctx, store.SessionOwnership{
		TenantID: env.tenant, SessionID: env.session, BotID: env.ownerBot.ID,
		AccountID: env.owner.ID, CreatedByAccountID: env.owner.ID,
	}); err != nil {
		t.Fatal(err)
	}

	bridge, err := accesswire.NewBridge(st, accessTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	env.bridge = bridge
	triggers := agentext.NewTriggerEngine(agentext.TriggerEngineOptions{Logger: accessTestLogger()})
	t.Cleanup(triggers.Close)
	env.triggers = triggers
	for _, spec := range []agentext.TriggerSpec{
		{ID: "owner-trigger", Kind: agentext.TriggerWebhook, TenantID: env.tenant.String(), Prompt: "owner", Enabled: true},
		{ID: "other-trigger", Kind: agentext.TriggerWebhook, TenantID: env.other.String(), Prompt: "other", Enabled: true},
	} {
		if _, err := triggers.Register(spec); err != nil {
			t.Fatal(err)
		}
	}

	starter := func(context.Context, agentapi.BotSessionRequest) (agentapi.BotSessionResult, error) {
		return agentapi.BotSessionResult{SessionID: uuid.New(), Seq: 1, RequestID: uuid.NewString()}, nil
	}
	product := agentapi.New(agentapi.Config{
		Sessions: agentapi.NewPGSessionLister(st),
		Identity: st, Access: bridge, StartBotSession: starter,
		Triggers:             triggers,
		ModelSettingsStore:   &fakeModelSettingsStore{},
		ModelSettingsRuntime: fakeModelRuntime{},
		SecretStore:          &fakeSecretStore{},
		Logger:               accessTestLogger(),
	})
	api := httpapi.New(httpapi.Config{
		Store: st, Events: emptyEvents{}, Product: product, Access: bridge,
		AgentTeams: st, Logger: accessTestLogger(),
	})
	env.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawAccount := r.Header.Get("X-Test-Account")
		accountID, err := uuid.Parse(rawAccount)
		if err != nil {
			t.Fatalf("X-Test-Account must be a uuid: %q", rawAccount)
		}
		requestTenant := env.tenant
		if raw := r.Header.Get("X-Test-Tenant"); raw != "" {
			requestTenant, err = uuid.Parse(raw)
			if err != nil {
				t.Fatalf("X-Test-Tenant must be a uuid: %q", raw)
			}
		}
		r.Header.Del("X-Test-Account")
		r.Header.Del("X-Test-Tenant")
		r.Header.Set("X-Tenant-ID", requestTenant.String())
		principal := auth.Principal{
			Kind: auth.PrincipalHuman, TenantID: requestTenant, AccountID: accountID,
			IsSuperAdmin: r.Header.Get("X-Test-Super") == "1",
		}
		r.Header.Del("X-Test-Super")
		api.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	}))
	t.Cleanup(env.server.Close)
	return env
}

func (e *accessPGEnv) do(method, path string, account store.AccountRecord, body any) (int, map[string]any) {
	e.t.Helper()
	return e.doAs(method, path, account, uuid.Nil, body)
}

func (e *accessPGEnv) doAs(method, path string, account store.AccountRecord, tenant uuid.UUID, body any) (int, map[string]any) {
	e.t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, e.server.URL+path, bytes.NewReader(raw))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("X-Test-Account", account.ID.String())
	if account.IsSuperAdmin {
		req.Header.Set("X-Test-Super", "1")
	}
	if tenant != uuid.Nil {
		req.Header.Set("X-Test-Tenant", tenant.String())
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := e.server.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	decoded := map[string]any{}
	_ = json.NewDecoder(res.Body).Decode(&decoded)
	return res.StatusCode, decoded
}

func wantStatus(t *testing.T, status int, body map[string]any, want int, what string) {
	t.Helper()
	if status != want {
		t.Fatalf("%s: status %d, want %d; body=%v", what, status, want, body)
	}
}

// TestSessionSingleResourceNegativeControls is the original A4.1 gap: a
// zero-grant account could read/write another account's Session and drive its
// Bot through /input. Every single-resource route must now deny.
func TestSessionSingleResourceNegativeControls(t *testing.T) {
	env := newAccessPGEnv(t)
	id := env.session.String()
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/v1/sessions/" + id + "/events"},
		{http.MethodGet, "/v1/sessions/" + id + "/events/stream"},
		{http.MethodPost, "/v1/sessions/" + id + "/input"},
		{http.MethodPost, "/v1/sessions/" + id + "/retry"},
		{http.MethodPost, "/v1/sessions/" + id + "/run"},
		{http.MethodGet, "/v1/sessions/" + id + "/run"},
		{http.MethodGet, "/v1/sessions/" + id + "/run/loadout"},
		{http.MethodPost, "/v1/sessions/" + id + "/cancel"},
		{http.MethodDelete, "/v1/sessions/" + id},
		{http.MethodGet, "/v1/sessions/" + id + "/decisions"},
		{http.MethodPost, "/v1/sessions/" + id + "/decisions"},
		{http.MethodGet, "/v1/sessions/" + id + "/approvals"},
		{http.MethodGet, "/v1/sessions/" + id + "/team-tasks"},
		{http.MethodGet, "/v1/sessions/" + id + "/files"},
		{http.MethodGet, "/v1/sessions/" + id + "/tasks"},
		{http.MethodPost, "/v1/sessions/" + id + "/subagents"},
		{http.MethodPost, "/v1/sessions/" + id + "/fork"},
		{http.MethodPost, "/v1/sessions/" + id + "/resume"},
	} {
		status, body := env.do(probe.method, probe.path, env.zero, nil)
		wantStatus(t, status, body, http.StatusForbidden, probe.method+" "+probe.path)
		if body["requiredKey"] == nil || body["reason"] == nil {
			t.Fatalf("%s: denial body lacks key/reason: %v", probe.path, body)
		}
	}
	// Cross-tenant caller: a principal from another tenant cannot reach the
	// session either (the resource read is tenant-scoped).
	status, body := env.doAs(http.MethodGet, "/v1/sessions/"+id+"/events", env.otherAcct, env.other, nil)
	wantStatus(t, status, body, http.StatusForbidden, "cross-tenant events")
}

// TestSessionSingleResourceAllowsOwner proves the denial above is not "deny
// everything": the owning account reads 200 and a missing run is an honest 404.
func TestSessionSingleResourceAllowsOwner(t *testing.T) {
	env := newAccessPGEnv(t)
	status, body := env.do(http.MethodGet, "/v1/sessions/"+env.session.String()+"/events", env.owner, nil)
	wantStatus(t, status, body, http.StatusOK, "owner events")
	if _, ok := body["events"]; !ok {
		t.Fatalf("events body = %v", body)
	}
	status, body = env.do(http.MethodGet, "/v1/sessions/"+env.session.String()+"/run", env.owner, nil)
	wantStatus(t, status, body, http.StatusNotFound, "owner run status without a run")
}

// TestTriggersAreTenantFiltered covers the pre-existing cross-tenant read: the
// engine is process-wide, the list must be the caller's tenant slice.
func TestTriggersAreTenantFiltered(t *testing.T) {
	env := newAccessPGEnv(t)
	status, body := env.do(http.MethodGet, "/v1/triggers", env.owner, nil)
	wantStatus(t, status, body, http.StatusOK, "owner trigger list")
	triggers, _ := body["triggers"].([]any)
	if len(triggers) != 1 {
		t.Fatalf("owner sees %d triggers, want 1: %v", len(triggers), body)
	}
	first, _ := triggers[0].(map[string]any)
	if first["id"] != "owner-trigger" {
		t.Fatalf("owner sees %v", first["id"])
	}
	status, body = env.do(http.MethodGet, "/v1/triggers", env.zero, nil)
	wantStatus(t, status, body, http.StatusForbidden, "zero-grant trigger list")
	// Firing another tenant's trigger is a 404 from the engine, never a fire: the
	// owner holds trigger.manage, so the denial is the engine's tenant boundary,
	// not the key gate.
	status, body = env.do(http.MethodPost, "/v1/triggers/other-trigger/fire", env.owner,
		map[string]any{"idempotencyKey": "x"})
	wantStatus(t, status, body, http.StatusNotFound, "fire another tenant's trigger")
}

// TestSettingsZeroGrantCannotWriteSecretsOrModels is the settings-surface
// negative control: a zero-grant account is refused before the handler (so it
// cannot probe whether a store is configured).
func TestSettingsZeroGrantCannotWriteSecretsOrModels(t *testing.T) {
	env := newAccessPGEnv(t)
	for _, probe := range []struct{ method, path string }{
		{http.MethodPut, "/v1/settings/secrets/OPENAI_KEY"},
		{http.MethodDelete, "/v1/settings/secrets/OPENAI_KEY"},
		{http.MethodGet, "/v1/settings/secrets"},
		{http.MethodPut, "/v1/settings/models"},
		{http.MethodGet, "/v1/settings/models"},
		{http.MethodGet, "/v1/providers"},
		{http.MethodGet, "/v1/models"},
	} {
		status, body := env.do(probe.method, probe.path, env.zero, nil)
		wantStatus(t, status, body, http.StatusForbidden, probe.method+" "+probe.path)
	}
	// The owner is allowed past the gate and the (fake) store serves the write.
	status, body := env.do(http.MethodPut, "/v1/settings/secrets/OPENAI_KEY", env.owner,
		map[string]any{"value": "sk-not-a-real-key"})
	wantStatus(t, status, body, http.StatusOK, "owner secret write")
	status, body = env.do(http.MethodPut, "/v1/settings/models", env.owner, map[string]any{
		"providers": []map[string]any{},
	})
	wantStatus(t, status, body, http.StatusOK, "owner model settings write")
}

// TestBotProfileRebindRollsBackOnStaleCAS proves the ownership move is part of
// the write, not a second best-effort update: a stale CAS leaves the old owner
// relation intact and never creates the new owner.
func TestBotProfileRebindRollsBackOnStaleCAS(t *testing.T) {
	env := newAccessPGEnv(t)
	// A profile that would become the new owner.
	newProfile, err := env.st.CreateProfile(context.Background(), store.ProfileRecord{
		TenantID: env.tenant, ID: uuid.New(), AccountID: env.zero.ID, RoleID: env.ownerRole.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Archive after the client read expectedArchived=false: the PATCH below is
	// stale and must fail the CAS.
	if err := env.st.ArchiveBot(context.Background(), env.tenant, env.ownerBot.ID); err != nil {
		t.Fatal(err)
	}
	status, body := env.do(http.MethodPatch, "/v1/bots/"+env.ownerBot.ID, env.owner, map[string]any{
		"profileId": newProfile.ID.String(), "expectedArchived": false,
	})
	wantStatus(t, status, body, http.StatusConflict, "stale rebind")
	relations, err := env.st.ListResourceRelations(context.Background(), env.tenant, "bot", env.ownerBot.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var owner, wrong uuid.UUID
	for _, relation := range relations {
		if relation.Relation != store.RelationOwner {
			continue
		}
		id, _ := uuid.Parse(relation.SubjectID)
		switch id {
		case env.owner.ID:
			owner = id
		case env.zero.ID:
			wrong = id
		}
	}
	if owner != env.owner.ID || wrong != uuid.Nil {
		t.Fatalf("owner relation after failed rebind = %v (old=%s new=%s)", relations, env.owner.ID, env.zero.ID)
	}
}

// TestBotCreateWritesOwnerRelation atomically pins the create contract: the
// relation exists as soon as the create answers 201.
func TestBotCreateWritesOwnerRelation(t *testing.T) {
	env := newAccessPGEnv(t)
	status, body := env.do(http.MethodPost, "/v1/bots", env.owner, map[string]any{
		"botId": "atomic-bot", "name": "Atomic", "profileId": env.ownerProf.ID.String(),
		"modelKind":    "text",
		"defaultModel": map[string]any{"providerId": "fake", "modelId": "fake"},
		"harness":      map[string]any{"tools": []string{}, "context": map[string]any{"permissions": []string{"session.read"}}},
	})
	wantStatus(t, status, body, http.StatusCreated, "create bot")
	relations, err := env.st.ListResourceRelations(context.Background(), env.tenant, "bot", "atomic-bot", 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, relation := range relations {
		if relation.Relation == store.RelationOwner && relation.SubjectID == env.owner.ID.String() {
			found = true
		}
	}
	if !found {
		t.Fatalf("owner relation missing after create: %v", relations)
	}
}

// TestRosterArchivedBotFailsNextStart pins the roster lifecycle: a member
// archived after the roster was written makes the next start a 409, not a run.
func TestRosterArchivedBotFailsNextStart(t *testing.T) {
	env := newAccessPGEnv(t)
	status, body := env.do(http.MethodPost, "/v1/team-bots", env.owner, map[string]any{
		"teamBotId": "archived-roster", "name": "Archived roster", "mission": "test",
		"roster": []map[string]any{{"botId": env.memberBot.ID, "role": "captain"}},
	})
	wantStatus(t, status, body, http.StatusCreated, "create team bot")
	if err := env.st.ArchiveBot(context.Background(), env.tenant, env.memberBot.ID); err != nil {
		t.Fatal(err)
	}
	status, body = env.do(http.MethodPost, "/v1/team-bots/archived-roster/start", env.owner,
		map[string]any{"text": "start"})
	wantStatus(t, status, body, http.StatusConflict, "start with archived member")
}

// TestDirectGrantsOverBoundFailClosed is the >500 direct-grant rule: authority
// is never truncated, so the authorization read fails and the route answers
// 500 instead of silently dropping grants.
func TestDirectGrantsOverBoundFailClosed(t *testing.T) {
	env := newAccessPGEnv(t)
	account, err := env.st.CreateAccount(context.Background(), store.AccountRecord{
		TenantID: env.tenant, ID: uuid.New(), Username: "flood-" + env.tenant.String()[:8],
		DisplayName: "flood", Kind: "human",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.st.Pool().Exec(context.Background(),
		`INSERT INTO permission_grants
		    (tenant_id, grant_id, subject_type, subject_id, permission_key, scope, granted_by)
		 SELECT $1, gen_random_uuid(), 'account', $2,
		        'flood.' || series::text, 'own', $3
		   FROM generate_series(1, 501) AS series`,
		env.tenant, account.ID, env.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	status, body := env.do(http.MethodGet, "/v1/capabilities", account, nil)
	wantStatus(t, status, body, http.StatusInternalServerError, "over-bound direct grants")
}

func accessTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestEndpointFamilyStatuses covers the per-family 200/403/404 contract through
// the real gate: an authorized owner reads, a zero-grant account is refused, and
// a super admin gets an honest 404 for a missing resource (the only principal
// allowed past the relation check without a relation).
func TestEndpointFamilyStatuses(t *testing.T) {
	env := newAccessPGEnv(t)
	// A TeamBot for the team family's 200 and 404 probes.
	status, body := env.do(http.MethodPost, "/v1/team-bots", env.owner, map[string]any{
		"teamBotId": "family-team", "name": "Family", "mission": "test",
		"roster": []map[string]any{{"botId": env.memberBot.ID, "role": "captain"}},
	})
	wantStatus(t, status, body, http.StatusCreated, "create family team bot")

	allowed := []struct{ method, path string }{
		{http.MethodGet, "/v1/accounts"},
		{http.MethodGet, "/v1/permission-groups"},
		{http.MethodGet, "/v1/bots"},
		{http.MethodGet, "/v1/bots/" + env.ownerBot.ID},
		{http.MethodGet, "/v1/team-bots"},
		{http.MethodGet, "/v1/team-bots/family-team"},
		{http.MethodGet, "/v1/triggers"},
		{http.MethodGet, "/v1/agents"},
		{http.MethodGet, "/v1/teams"},
		{http.MethodGet, "/v1/capabilities"},
	}
	for _, probe := range allowed {
		status, body := env.do(probe.method, probe.path, env.owner, nil)
		wantStatus(t, status, body, http.StatusOK, "owner "+probe.method+" "+probe.path)
	}
	for _, probe := range allowed {
		status, body := env.do(probe.method, probe.path, env.zero, nil)
		wantStatus(t, status, body, http.StatusForbidden, "zero "+probe.method+" "+probe.path)
	}
	// Super admin: the relation check short-circuits, so a missing resource is an
	// honest 404 rather than a denial.
	status, body = env.do(http.MethodGet, "/v1/bots/missing-bot", env.super, nil)
	wantStatus(t, status, body, http.StatusNotFound, "super missing bot")
	status, body = env.do(http.MethodPost, "/v1/triggers/missing/fire", env.owner,
		map[string]any{"idempotencyKey": "missing"})
	wantStatus(t, status, body, http.StatusNotFound, "owner missing trigger")
}

// TestLegacyAgentTeamRoutesAreNeverZeroPermission pins the A4.2b-deferred
// surface: the old routes stay mounted but are gated (bot/team_bot held keys),
// so removing them later is not a security prerequisite.
func TestLegacyAgentTeamRoutesAreNeverZeroPermission(t *testing.T) {
	env := newAccessPGEnv(t)
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/v1/agents"},
		{http.MethodPost, "/v1/agents"},
		{http.MethodGet, "/v1/teams"},
		{http.MethodPost, "/v1/teams"},
	} {
		status, body := env.do(probe.method, probe.path, env.zero, nil)
		wantStatus(t, status, body, http.StatusForbidden, "zero "+probe.method+" "+probe.path)
	}
}

// TestBotProfileRebindMovesOwner is the success half of the rebind contract:
// one transaction updates the Bot and replaces the owner relation, never
// leaving two owners.
func TestBotProfileRebindMovesOwner(t *testing.T) {
	env := newAccessPGEnv(t)
	newProfile, err := env.st.CreateProfile(context.Background(), store.ProfileRecord{
		TenantID: env.tenant, ID: uuid.New(), AccountID: env.zero.ID, RoleID: env.ownerRole.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	status, body := env.do(http.MethodPatch, "/v1/bots/"+env.ownerBot.ID, env.owner, map[string]any{
		"profileId": newProfile.ID.String(), "expectedArchived": false,
	})
	wantStatus(t, status, body, http.StatusOK, "rebind bot")
	relations, err := env.st.ListResourceRelations(context.Background(), env.tenant, "bot", env.ownerBot.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var owners []string
	for _, relation := range relations {
		if relation.Relation == store.RelationOwner {
			owners = append(owners, relation.SubjectID)
		}
	}
	if len(owners) != 1 || owners[0] != env.zero.ID.String() {
		t.Fatalf("owner relations after rebind = %v, want only %s", owners, env.zero.ID)
	}
}

// TestRosterRevokedInvokeFailsNextStart is the runtime half of bot.invoke:
// revoking the grant after the roster was written fails the next start, not
// just the next roster edit.
func TestRosterRevokedInvokeFailsNextStart(t *testing.T) {
	env := newAccessPGEnv(t)
	status, body := env.do(http.MethodPost, "/v1/team-bots", env.owner, map[string]any{
		"teamBotId": "revoked-roster", "name": "Revoked roster", "mission": "test",
		"roster": []map[string]any{{"botId": env.memberBot.ID, "role": "captain"}},
	})
	wantStatus(t, status, body, http.StatusCreated, "create team bot")
	grants, err := env.st.ListPermissionGrants(context.Background(), env.tenant,
		store.PermissionSubjectAccount, env.owner.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	removed := 0
	for _, grant := range grants {
		if grant.PermissionKey != access.KeyBotInvoke {
			continue
		}
		if err := env.st.DeletePermissionGrant(context.Background(), env.tenant, grant.ID); err != nil {
			t.Fatal(err)
		}
		removed++
	}
	if removed == 0 {
		t.Fatal("fixture has no bot.invoke grant to revoke")
	}
	status, body = env.do(http.MethodPost, "/v1/team-bots/revoked-roster/start", env.owner,
		map[string]any{"text": "start"})
	wantStatus(t, status, body, http.StatusForbidden, "start after invoke revocation")
}

// TestGrantAuditFailureReturnsError proves a grant write is never reported as
// success when its audit row could not be written. The auth_events trigger is
// installed only for this test and dropped immediately; the isolated test
// database is the only place this schema mutation exists.
func TestGrantAuditFailureReturnsError(t *testing.T) {
	env := newAccessPGEnv(t)
	group, err := env.st.CreatePermissionGroup(context.Background(), store.PermissionGroupRecord{
		TenantID: env.tenant, ID: uuid.New(), Name: "audit-failure",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := env.st.Pool().Exec(ctx, `
		CREATE OR REPLACE FUNCTION a42a_fail_audit() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'audit store is down'; END;
		$$ LANGUAGE plpgsql`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.st.Pool().Exec(ctx, `
		CREATE TRIGGER a42a_fail_audit BEFORE INSERT ON auth_events
		FOR EACH ROW EXECUTE FUNCTION a42a_fail_audit()`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = env.st.Pool().Exec(context.Background(), `DROP TRIGGER IF EXISTS a42a_fail_audit ON auth_events`)
		_, _ = env.st.Pool().Exec(context.Background(), `DROP FUNCTION IF EXISTS a42a_fail_audit()`)
	}()
	status, body := env.do(http.MethodPost, "/v1/permission-groups/"+group.ID.String()+"/grants", env.super,
		map[string]any{"key": access.KeySessionRead, "scope": string(access.ScopeTenant)})
	wantStatus(t, status, body, http.StatusInternalServerError, "grant with failing audit")
	// The business write committed; the caller was told the audit failed (and can
	// retry the audit through a second grant attempt), rather than being told the
	// grant never happened.
	grants, err := env.st.ListPermissionGroupGrants(ctx, env.tenant, group.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 {
		t.Fatalf("group grants after failed audit = %v", grants)
	}
}
