package harnessapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/store"
)

func TestAccountsHTTPSurface(t *testing.T) {
	env := newIdentityEnv(t)

	status, body := env.asAlice(http.MethodGet, "/v1/accounts", nil)
	wantStatus(t, status, body, http.StatusOK, "list accounts")
	accounts := listField(body, "accounts")
	if len(accounts) < 4 {
		t.Fatalf("accounts = %v", body)
	}
	for _, raw := range accounts {
		entry, _ := raw.(map[string]any)
		if entry["accountId"] == env.otherAcct.ID.String() {
			t.Fatal("cross-tenant account leaked into the list")
		}
	}
	status, body = env.asBobGet("/v1/accounts")
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "bob lists accounts")

	// Create a human account. The password is hashed, never echoed.
	status, body = env.asAlice(http.MethodPost, "/v1/accounts", map[string]any{
		"username": "carol", "displayName": "Carol", "kind": "human", "password": "initial-password-123",
	})
	wantStatus(t, status, body, http.StatusCreated, "create account")
	created, _ := body["account"].(map[string]any)
	carolID, _ := uuid.Parse(created["accountId"].(string))
	if carolID == uuid.Nil {
		t.Fatalf("created account = %v", body)
	}
	if _, ok := created["passwordHash"]; ok {
		t.Fatal("create account echoed a password hash")
	}

	// The super-admin flag is reserved: an explicit request is refused and no
	// account is written.
	before := len(listField(mustListAccounts(t, env), "accounts"))
	status, body = env.asAlice(http.MethodPost, "/v1/accounts", map[string]any{
		"username": "would-be-super", "displayName": "Nope", "kind": "human",
		"password": "initial-password-123", "isSuperAdmin": true,
	})
	wantStatus(t, status, body, http.StatusForbidden, "create super admin")
	if body["code"] != "super_admin_reserved" {
		t.Fatalf("code = %v", body)
	}
	after := len(listField(mustListAccounts(t, env), "accounts"))
	if after != before {
		t.Fatalf("refused super-admin create wrote an account (%d -> %d)", before, after)
	}
	status, body = env.do(http.MethodPost, "/v1/accounts", env.bob, false, map[string]any{
		"username": "bob-created", "displayName": "Bob", "kind": "human", "password": "initial-password-123",
	})
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "bob creates an account")

	// Human without a password and bot with a password are both 400.
	status, body = env.asAlice(http.MethodPost, "/v1/accounts", map[string]any{
		"username": "no-password", "displayName": "No", "kind": "human",
	})
	wantStatus(t, status, body, http.StatusBadRequest, "human without password")
	status, body = env.asAlice(http.MethodPost, "/v1/accounts", map[string]any{
		"username": "bot-with-password", "displayName": "Bot", "kind": "bot", "password": "x",
	})
	wantStatus(t, status, body, http.StatusBadRequest, "bot with password")

	// Detail: own tenant 200, another tenant's id 404.
	status, body = env.asAlice(http.MethodGet, "/v1/accounts/"+carolID.String(), nil)
	wantStatus(t, status, body, http.StatusOK, "get account")
	status, body = env.asAlice(http.MethodGet, "/v1/accounts/"+env.otherAcct.ID.String(), nil)
	wantStatus(t, status, body, http.StatusNotFound, "get cross-tenant account")

	// Disable and re-enable.
	status, body = env.asAlice(http.MethodPost, "/v1/accounts/"+carolID.String()+"/disable", map[string]any{
		"disabled": true, "reason": "offboarded",
	})
	wantStatus(t, status, body, http.StatusOK, "disable account")
	disabledAccount, _ := body["account"].(map[string]any)
	if disabledAccount["disabledAt"] == nil {
		t.Fatalf("disabledAt = %v", disabledAccount)
	}
	status, body = env.asAlice(http.MethodPost, "/v1/accounts/"+carolID.String()+"/disable", map[string]any{
		"disabled": false,
	})
	wantStatus(t, status, body, http.StatusOK, "enable account")

	// The last super admin cannot be disabled.
	status, body = env.asSuper(http.MethodPost, "/v1/accounts/"+env.super.ID.String()+"/disable", map[string]any{
		"disabled": true,
	})
	wantStatus(t, status, body, http.StatusConflict, "disable last super admin")
	if body["code"] != "super_admin_protected" {
		t.Fatalf("code = %v", body)
	}

	// Password reset revokes the old credential.
	status, body = env.asAlice(http.MethodPost, "/v1/accounts/"+carolID.String()+"/password-reset", map[string]any{
		"password": "reset-password-456",
	})
	wantStatus(t, status, body, http.StatusOK, "reset password")
	if body["mustChangePassword"] != true {
		t.Fatalf("reset did not require a password change: %v", body)
	}
	stored, err := env.st.GetAccount(context.Background(), env.tenant, carolID)
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.VerifyPassword(stored.PasswordHash, "reset-password-456"); err != nil {
		t.Fatalf("new password does not verify: %v", err)
	}
	if err := auth.VerifyPassword(stored.PasswordHash, "initial-password-123"); err == nil {
		t.Fatal("old password still verifies after reset")
	}
	// The reset records password.reset (not password.changed) and leaves the
	// account in must-change state until a real password change follows.
	must, err := env.st.AccountMustChangePassword(context.Background(), env.tenant, carolID)
	if err != nil || !must {
		t.Fatalf("must change after reset = %v, err=%v", must, err)
	}
	events, err := env.st.ListAuthEvents(context.Background(), env.tenant, store.AuthEventFilter{
		AccountID: carolID, Kind: store.AuthEventPasswordReset,
	}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != store.AuthEventPasswordReset || events[0].Result != store.AuthResultOK {
		t.Fatalf("reset audit rows = %+v", events)
	}
	if events[0].ActorAccountID == nil || *events[0].ActorAccountID != env.alice.ID {
		t.Fatalf("reset audit actor = %v, want alice", events[0].ActorAccountID)
	}

	// A tenant admin (not a deployment super admin) must not reset the super
	// admin credential: that would be an account takeover.
	status, body = env.asAlice(http.MethodPost, "/v1/accounts/"+env.super.ID.String()+"/password-reset", map[string]any{
		"password": "takeover-password-123",
	})
	wantStatus(t, status, body, http.StatusForbidden, "tenant admin resets super admin")
	if body["code"] != "super_admin_protected" {
		t.Fatalf("code = %v", body)
	}

	// Group assignment: tenant-admin grantable by Alice, super-admin is not.
	tenantAdmin := access.BuiltinGroupID(env.tenant, "tenant-admin")
	status, body = env.asAlice(http.MethodPost, "/v1/accounts/"+env.bob.ID.String()+"/groups", map[string]any{
		"groupId": tenantAdmin.String(),
	})
	wantStatus(t, status, body, http.StatusOK, "assign tenant-admin")
	status, body = env.do(http.MethodGet, "/v1/accounts", env.bob, false, nil)
	wantStatus(t, status, body, http.StatusOK, "bob after tenant-admin")
	status, body = env.asAlice(http.MethodPost, "/v1/accounts/"+env.bob.ID.String()+"/groups", map[string]any{
		"groupId": access.BuiltinGroupID(env.tenant, "super-admin").String(),
	})
	wantStatus(t, status, body, http.StatusForbidden, "assign super-admin group")
	if body["code"] != "grant_exceeds_own" {
		t.Fatalf("code = %v", body)
	}
	// Built-in presets are usable but read-only: membership removed, definition untouched.
	status, body = env.asAlice(http.MethodDelete, "/v1/accounts/"+env.bob.ID.String()+"/groups/"+tenantAdmin.String(), nil)
	wantStatus(t, status, body, http.StatusOK, "remove tenant-admin")
	status, body = env.do(http.MethodGet, "/v1/accounts", env.bob, false, nil)
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "bob after removal")

	// A missing group is an honest 404.
	status, body = env.asAlice(http.MethodPost, "/v1/accounts/"+env.bob.ID.String()+"/groups", map[string]any{
		"groupId": uuid.NewString(),
	})
	wantStatus(t, status, body, http.StatusNotFound, "assign missing group")
}

func mustListAccounts(t *testing.T, env *identityEnv) map[string]any {
	t.Helper()
	status, body := env.asAlice(http.MethodGet, "/v1/accounts", nil)
	wantStatus(t, status, body, http.StatusOK, "list accounts")
	return body
}

// asBobGet runs a GET as Bob and returns status/body; it exists because the
// wantReason helper reads both.
func (e *identityEnv) asBobGet(path string) (int, map[string]any) {
	return e.do(http.MethodGet, path, e.bob, false, nil)
}

func TestPermissionGroupsHTTPSurface(t *testing.T) {
	env := newIdentityEnv(t)

	status, body := env.asAlice(http.MethodGet, "/v1/permission-groups", nil)
	wantStatus(t, status, body, http.StatusOK, "list groups")
	groups := listField(body, "groups")
	if len(groups) < len(access.BuiltinGroups()) {
		t.Fatalf("groups = %v, want the seeded presets", body)
	}
	status, body = env.asBobGet("/v1/permission-groups")
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "bob lists groups")

	// The limited account may create a group but may not grant beyond its own
	// (session.read/own).
	status, body = env.do(http.MethodPost, "/v1/permission-groups", env.limited, false, map[string]any{
		"name": "narrow",
	})
	wantStatus(t, status, body, http.StatusCreated, "limited creates a group")
	created, _ := body["group"].(map[string]any)
	groupID := created["groupId"].(string)

	status, body = env.do(http.MethodPost, "/v1/permission-groups/"+groupID+"/grants", env.limited, false, map[string]any{
		"key": access.KeySessionRead, "scope": string(access.ScopeTenant),
	})
	wantStatus(t, status, body, http.StatusForbidden, "grant wider scope than own")
	if body["code"] != "grant_exceeds_own" {
		t.Fatalf("code = %v", body)
	}
	status, body = env.do(http.MethodPost, "/v1/permission-groups/"+groupID+"/grants", env.limited, false, map[string]any{
		"key": access.KeySecretReadValue, "scope": string(access.ScopeTenant),
	})
	wantStatus(t, status, body, http.StatusForbidden, "grant a key not held")
	if body["code"] != "grant_exceeds_own" {
		t.Fatalf("code = %v", body)
	}
	// The subset tuple is accepted.
	status, body = env.do(http.MethodPost, "/v1/permission-groups/"+groupID+"/grants", env.limited, false, map[string]any{
		"key": access.KeySessionRead, "scope": string(access.ScopeOwn),
	})
	wantStatus(t, status, body, http.StatusOK, "grant a subset tuple")

	status, body = env.do(http.MethodGet, "/v1/permission-groups/"+groupID, env.limited, false, nil)
	wantStatus(t, status, body, http.StatusOK, "get group")
	grants := listField(body, "grants")
	if len(grants) != 1 {
		t.Fatalf("grants = %v", body["grants"])
	}

	// Built-in groups are read-only.
	builtinID := access.BuiltinGroupID(env.tenant, "viewer")
	status, body = env.asAlice(http.MethodPatch, "/v1/permission-groups/"+builtinID.String(), map[string]any{
		"name": "renamed",
	})
	wantStatus(t, status, body, http.StatusConflict, "rename builtin")
	if body["code"] != "builtin_group_read_only" {
		t.Fatalf("code = %v", body)
	}
	status, body = env.asAlice(http.MethodDelete, "/v1/permission-groups/"+builtinID.String(), nil)
	wantStatus(t, status, body, http.StatusConflict, "delete builtin")
	status, body = env.asAlice(http.MethodPost, "/v1/permission-groups/"+builtinID.String()+"/grants", map[string]any{
		"key": access.KeySessionRead, "scope": string(access.ScopeOwn),
	})
	wantStatus(t, status, body, http.StatusConflict, "mutate builtin grants")

	// Membership: Alice (account.permission.grant) can add/remove Bob; the
	// limited account cannot (it does not hold that key).
	status, body = env.asAlice(http.MethodPost, "/v1/permission-groups/"+groupID+"/members", map[string]any{
		"accountId": env.bob.ID.String(),
	})
	wantStatus(t, status, body, http.StatusOK, "add member")
	status, body = env.do(http.MethodPost, "/v1/permission-groups/"+groupID+"/members", env.limited, false, map[string]any{
		"accountId": env.bob.ID.String(),
	})
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "limited adds a member")
	status, body = env.asAlice(http.MethodDelete, "/v1/permission-groups/"+groupID+"/members/"+env.bob.ID.String(), nil)
	wantStatus(t, status, body, http.StatusOK, "remove member")

	// Rename and delete the custom group (limited holds permission.manage).
	status, body = env.do(http.MethodPatch, "/v1/permission-groups/"+groupID, env.limited, false, map[string]any{
		"name": "renamed-narrow",
	})
	wantStatus(t, status, body, http.StatusOK, "rename custom group")
	status, body = env.do(http.MethodDelete, "/v1/permission-groups/"+groupID, env.limited, false, nil)
	wantStatus(t, status, body, http.StatusOK, "delete custom group")
	status, body = env.do(http.MethodGet, "/v1/permission-groups/"+groupID, env.limited, false, nil)
	wantStatus(t, status, body, http.StatusNotFound, "get deleted group")
}

func TestIdentityEndpointsFailClosedWithoutAccess(t *testing.T) {
	api := New(Config{Logger: testLogger()})
	server := httptest.NewServer(api)
	defer server.Close()
	for _, path := range []string{"/v1/bots", "/v1/team-bots", "/v1/accounts", "/v1/permission-groups"} {
		req, _ := http.NewRequest(http.MethodGet, server.URL+path, nil)
		req.Header.Set("X-Tenant-ID", uuid.NewString())
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("unwired %s = %d, want 503", path, res.StatusCode)
		}
	}
}

func TestIdentityEndpointCoverageGate(t *testing.T) {
	report := access.CheckCoverageFloor(IdentityEndpoints(), access.MinHTTPEndpoints)
	if !report.OK() {
		t.Fatalf("coverage failed: %v", report.Err())
	}
	if report.Checked < access.MinHTTPEndpoints {
		t.Fatalf("checked %d endpoints, want at least %d", report.Checked, access.MinHTTPEndpoints)
	}
	// The catalog must exercise every high-risk identity key.
	used := map[string]bool{}
	for _, endpoint := range IdentityEndpoints() {
		used[endpoint.RequiredKey] = true
	}
	for _, key := range []string{
		access.KeyAccountRead, access.KeyAccountManage, access.KeyAccountCredentialReset,
		access.KeyAccountPermissionGrant, access.KeyPermissionRead, access.KeyPermissionManage,
		access.KeyRoleRead, access.KeyRoleManage, access.KeyProfileRead, access.KeyProfileManage,
		access.KeyBotRead, access.KeyBotCreate, access.KeyBotManage, access.KeyBotInvoke,
		access.KeyTeamBotRead, access.KeyTeamBotManage,
	} {
		if !used[key] {
			t.Fatalf("endpoint catalog does not declare %q", key)
		}
	}
}

// TestIdentityWritesAreAudited proves the A4.1 audit surface: grant/revoke,
// relation change and super-admin action each land in auth_events with
// non-sensitive detail.
func TestIdentityWritesAreAudited(t *testing.T) {
	env := newIdentityEnv(t)
	ctx := context.Background()

	status, body := env.do(http.MethodPost, "/v1/permission-groups", env.limited, false, map[string]any{
		"name": "audited",
	})
	wantStatus(t, status, body, http.StatusCreated, "create audited group")
	group, _ := body["group"].(map[string]any)
	groupID := group["groupId"].(string)
	status, body = env.do(http.MethodPost, "/v1/permission-groups/"+groupID+"/grants", env.limited, false, map[string]any{
		"key": access.KeySessionRead, "scope": string(access.ScopeOwn),
	})
	wantStatus(t, status, body, http.StatusOK, "add audited grant")
	grantEvents, err := env.st.ListAuthEvents(ctx, env.tenant, store.AuthEventFilter{
		Kind: store.AuthEventPermissionGrant,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range grantEvents {
		if event.Detail["via"] == "group_grant" && event.Detail["key"] == access.KeySessionRead &&
			event.Detail["group_id"] == groupID && event.Detail["scope"] == string(access.ScopeOwn) {
			found = true
		}
	}
	if !found {
		t.Fatalf("grant audit rows = %+v, want the group grant", grantEvents)
	}
	status, body = env.do(http.MethodDelete,
		"/v1/permission-groups/"+groupID+"/grants/"+access.KeySessionRead+"?scope="+string(access.ScopeOwn),
		env.limited, false, nil)
	wantStatus(t, status, body, http.StatusOK, "remove audited grant")
	revokeEvents, err := env.st.ListAuthEvents(ctx, env.tenant, store.AuthEventFilter{
		Kind: store.AuthEventPermissionRevoke,
	}, 0)
	if err != nil || len(revokeEvents) == 0 {
		t.Fatalf("revoke audit rows = %d err=%v", len(revokeEvents), err)
	}

	// Creating a bot writes the owner relation and audits the tuple.
	status, body = env.asAlice(http.MethodPost, "/v1/bots", map[string]any{
		"botId": "audited-bot", "name": "Audited", "profileId": env.aliceProfile.ID.String(),
		"modelKind":    "text",
		"defaultModel": map[string]any{"providerId": "fake", "modelId": "fake"},
		"harness":      map[string]any{"tools": []string{}, "context": map[string]any{"permissions": []string{}}},
	})
	wantStatus(t, status, body, http.StatusCreated, "create audited bot")
	relationEvents, err := env.st.ListAuthEvents(ctx, env.tenant, store.AuthEventFilter{
		Kind: store.AuthEventRelationChanged,
	}, 0)
	if err != nil || len(relationEvents) == 0 {
		t.Fatalf("relation audit rows = %d err=%v", len(relationEvents), err)
	}
	if relationEvents[0].Detail["resource_type"] != "bot" || relationEvents[0].Detail["relation"] != "owner" {
		t.Fatalf("relation audit detail = %+v", relationEvents[0].Detail)
	}

	// A super admin write is marked as such.
	status, body = env.asSuper(http.MethodPost, "/v1/accounts", map[string]any{
		"username": "audited-account", "displayName": "Audited", "kind": "human",
		"password": "audited-password-123",
	})
	wantStatus(t, status, body, http.StatusCreated, "super creates account")
	superEvents, err := env.st.ListAuthEvents(ctx, env.tenant, store.AuthEventFilter{
		Kind: store.AuthEventSuperAdmin,
	}, 0)
	if err != nil || len(superEvents) == 0 {
		t.Fatalf("superadmin audit rows = %d err=%v", len(superEvents), err)
	}
	if superEvents[0].Detail["action"] != "account.create" || superEvents[0].Detail["super_admin"] != true {
		t.Fatalf("superadmin audit detail = %+v", superEvents[0].Detail)
	}
	// No audit detail carries a credential-shaped value.
	for _, event := range append(append(grantEvents, revokeEvents...), superEvents...) {
		raw, _ := json.Marshal(event.Detail)
		if strings.Contains(string(raw), "password") || strings.Contains(string(raw), "keel_bot_") {
			t.Fatalf("audit detail leaked credential material: %s", raw)
		}
	}
}
