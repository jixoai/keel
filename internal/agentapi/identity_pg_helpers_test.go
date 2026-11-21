package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/store"
)

// identityEnv is the real-PG fixture for the A4.1 HTTP surface: one tenant, the
// seeded built-in groups, four accounts with different authority, two bots and
// a test server that injects the edge-resolved principal per request.
type identityEnv struct {
	t       *testing.T
	st      *store.Store
	tenant  uuid.UUID
	super   store.AccountRecord
	alice   store.AccountRecord
	bob     store.AccountRecord
	limited store.AccountRecord
	// botAccount owns support-1; alice owns alice-1.
	botAccount   store.AccountRecord
	aliceBot     store.BotRecord
	supportBot   store.BotRecord
	aliceProfile store.ProfileRecord
	botProfile   store.ProfileRecord
	opsGroup     store.PermissionGroupRecord
	starter      *fakeBotSessionStarter
	bridge       *accesswire.Bridge
	server       *httptest.Server
	otherTenant  uuid.UUID
	otherAcct    store.AccountRecord
}

// fakeBotSessionStarter mirrors the production seam's ownership write so the
// tests can assert the session row and owner relation without a kernel.
type fakeBotSessionStarter struct {
	t     *testing.T
	st    *store.Store
	calls []BotSessionRequest
	err   error
}

func (f *fakeBotSessionStarter) start(ctx context.Context, request BotSessionRequest) (BotSessionResult, error) {
	if f.err != nil {
		return BotSessionResult{}, f.err
	}
	sessionID := request.SessionID
	if sessionID == uuid.Nil {
		sessionID = uuid.New()
	}
	if err := f.st.CreateOwnedSession(ctx, store.SessionOwnership{
		TenantID: request.TenantID, SessionID: sessionID, BotID: request.BotID,
		AccountID: request.AccountID, CreatedByAccountID: request.ActorAccountID,
		TeamBotID: request.TeamBotID,
	}); err != nil {
		return BotSessionResult{}, err
	}
	f.calls = append(f.calls, request)
	requestID := request.RequestID
	if requestID == "" {
		requestID = uuid.NewString()
	}
	return BotSessionResult{SessionID: sessionID, Seq: 1, RequestID: requestID}, nil
}

func newIdentityEnv(t *testing.T) *identityEnv {
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
	tenant := uuid.New()
	// HasSuperAdmin is deployment-global: clear the flag at teardown so another
	// package's bootstrap test does not inherit this fixture's super admin.
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(),
			`UPDATE accounts SET is_super_admin = false WHERE tenant_id = $1`, tenant)
	})
	if err := st.CreateTenant(ctx, tenant, "a41-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	if err := accesswire.EnsureBuiltinGroups(ctx, st, tenant); err != nil {
		t.Fatal(err)
	}
	makeAccount := func(username, kind string, super bool) store.AccountRecord {
		t.Helper()
		account, err := st.CreateAccount(ctx, store.AccountRecord{
			TenantID: tenant, ID: uuid.New(), Username: username + "-" + tenant.String()[:8],
			DisplayName: username, Kind: kind, IsSuperAdmin: super,
		})
		if err != nil {
			t.Fatal(err)
		}
		return account
	}
	env := &identityEnv{
		t: t, st: st, tenant: tenant,
		super:      makeAccount("super", "human", true),
		alice:      makeAccount("alice", "human", false),
		bob:        makeAccount("bob", "human", false),
		limited:    makeAccount("limited", "human", false),
		botAccount: makeAccount("bot-owner", "bot", false),
	}
	role, err := st.CreateRole(ctx, store.RoleRecord{TenantID: tenant, ID: uuid.New(), Name: "Support"})
	if err != nil {
		t.Fatal(err)
	}
	aliceProfile, err := st.CreateProfile(ctx, store.ProfileRecord{
		TenantID: tenant, ID: uuid.New(), AccountID: env.alice.ID, RoleID: role.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	botProfile, err := st.CreateProfile(ctx, store.ProfileRecord{
		TenantID: tenant, ID: uuid.New(), AccountID: env.botAccount.ID, RoleID: role.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	createBot := func(id string, profileID uuid.UUID) store.BotRecord {
		t.Helper()
		bot, err := st.CreateBot(ctx, store.BotRecord{
			TenantID: tenant, ID: id, Name: id, ProfileID: profileID, ModelKind: "text",
			DefaultModel: json.RawMessage(`{"providerId":"fake","modelId":"fake"}`),
			Harness:      json.RawMessage(`{"tools":[],"context":{"permissions":["session.read","bot.invoke"]}}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.PutResourceRelation(ctx, store.ResourceRelation{
			TenantID: tenant, ResourceType: "bot", ResourceID: id,
			Relation: store.RelationOwner, SubjectType: store.RelationSubjectAccount,
			SubjectID: profileIDToAccount(t, st, tenant, profileID).String(), CreatedBy: env.super.ID,
		}); err != nil {
			t.Fatal(err)
		}
		return bot
	}
	env.aliceProfile = aliceProfile
	env.botProfile = botProfile
	env.aliceBot = createBot("alice-1", aliceProfile.ID)
	env.supportBot = createBot("support-1", botProfile.ID)

	// Alice is a tenant administrator through the built-in preset.
	if _, err := st.AddPermissionGroupMember(ctx, tenant,
		access.BuiltinGroupID(tenant, "tenant-admin"), env.alice.ID, env.super.ID); err != nil {
		t.Fatal(err)
	}
	// A non-builtin preset Alice can hand out (its grants are a subset of hers).
	env.opsGroup, err = st.CreatePermissionGroup(ctx, store.PermissionGroupRecord{
		TenantID: tenant, ID: uuid.New(), Name: "ops",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, grant := range []struct{ key, scope string }{
		{access.KeyBotRead, string(access.ScopeTenant)},
		{access.KeySessionRead, string(access.ScopeOwn)},
	} {
		if _, err := st.AddPermissionGroupGrant(ctx, tenant, env.opsGroup.ID, grant.key, grant.scope); err != nil {
			t.Fatal(err)
		}
	}
	// The limited account may manage permission groups but holds only a narrow
	// session grant: grant ⊆ own must reject a wider tuple.
	for _, grant := range []struct{ key, scope string }{
		{access.KeyPermissionRead, string(access.ScopeTenant)},
		{access.KeyPermissionManage, string(access.ScopeTenant)},
		{access.KeySessionRead, string(access.ScopeOwn)},
	} {
		if _, err := st.CreatePermissionGrant(ctx, store.PermissionGrantRecord{
			TenantID: tenant, ID: uuid.New(), SubjectType: store.PermissionSubjectAccount,
			SubjectID: env.limited.ID, PermissionKey: grant.key, Scope: grant.scope,
			GrantedBy: env.super.ID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A second tenant for cross-tenant negative controls.
	env.otherTenant = uuid.New()
	if err := st.CreateTenant(ctx, env.otherTenant, "a41-other-"+env.otherTenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	env.otherAcct, err = st.CreateAccount(ctx, store.AccountRecord{
		TenantID: env.otherTenant, ID: uuid.New(), Username: "outsider-" + env.otherTenant.String()[:8],
		DisplayName: "Outsider", Kind: "human",
	})
	if err != nil {
		t.Fatal(err)
	}

	bridge, err := accesswire.NewBridge(st, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	env.bridge = bridge
	env.starter = &fakeBotSessionStarter{t: t, st: st}
	api := New(Config{
		Identity: st, Access: bridge, StartBotSession: env.starter.start,
		Logger: testLogger(),
	})
	env.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.Header.Get("X-Test-Account")
		accountID, err := uuid.Parse(raw)
		if err != nil {
			t.Fatalf("X-Test-Account must be a uuid: %q", raw)
		}
		principal := auth.Principal{
			Kind: auth.PrincipalHuman, TenantID: env.tenant, AccountID: accountID,
			IsSuperAdmin: r.Header.Get("X-Test-Super") == "1",
		}
		r.Header.Del("X-Test-Account")
		r.Header.Del("X-Test-Super")
		r.Header.Set("X-Tenant-ID", env.tenant.String())
		api.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	}))
	t.Cleanup(env.server.Close)
	return env
}

// profileIDToAccount resolves the account a profile names (the fixture's owner
// relation helper needs it before it can build the tuple).
func profileIDToAccount(t *testing.T, st *store.Store, tenant, profileID uuid.UUID) uuid.UUID {
	t.Helper()
	profile, err := st.GetProfile(context.Background(), tenant, profileID)
	if err != nil {
		t.Fatal(err)
	}
	return profile.AccountID
}

// do issues one request as account, returning the status and decoded body.
func (e *identityEnv) do(method, path string, account store.AccountRecord, super bool, body any) (int, map[string]any) {
	e.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, e.server.URL+path, reader)
	if err != nil {
		e.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Test-Account", account.ID.String())
	if super {
		req.Header.Set("X-Test-Super", "1")
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

func (e *identityEnv) asAlice(method, path string, body any) (int, map[string]any) {
	return e.do(method, path, e.alice, false, body)
}

func (e *identityEnv) asSuper(method, path string, body any) (int, map[string]any) {
	return e.do(method, path, e.super, true, body)
}

// wantStatus fails with the response body, the most useful failure shape.
func wantStatus(t *testing.T, status int, body map[string]any, want int, what string) {
	t.Helper()
	if status != want {
		t.Fatalf("%s: status %d, want %d; body=%v", what, status, want, body)
	}
}

// wantReason asserts the structured 403 reason without pinning the prose.
func wantReason(t *testing.T, status int, body map[string]any, want int, reason access.Reason, what string) {
	t.Helper()
	wantStatus(t, status, body, want, what)
	if want != http.StatusForbidden {
		return
	}
	if body["reason"] != string(reason) {
		t.Fatalf("%s: reason %v, want %q; body=%v", what, body["reason"], reason, body)
	}
	if body["requiredKey"] == nil || body["requiredKey"] == "" {
		t.Fatalf("%s: 403 body has no requiredKey: %v", what, body)
	}
}

// listIDs extracts a nested id list from a response.
func listField(body map[string]any, name string) []any {
	value, _ := body[name].([]any)
	return value
}

var errFakeStart = errors.New("fake start failure")
