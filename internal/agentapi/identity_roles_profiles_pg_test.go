package agentapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/store"
)

// TestRolesHTTPSurface pins the role CRUD contract: role.read/manage, bounded
// pagination, tenant scoping (a foreign id is 404) and the 409 reference guard
// (a role still named by a profile cannot be deleted).
func TestRolesHTTPSurface(t *testing.T) {
	env := newIdentityEnv(t)

	// Zero permission: Bob holds no role.read.
	status, body := env.asBobGet("/v1/roles")
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "bob lists roles")
	status, body = env.do(http.MethodPost, "/v1/roles", env.bob, false, map[string]any{"name": "Nope"})
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "bob creates a role")

	// Alice (tenant admin) lists the seeded role.
	status, body = env.asAlice(http.MethodGet, "/v1/roles", nil)
	wantStatus(t, status, body, http.StatusOK, "list roles")
	if !containsRecordID(listField(body, "roles"), "roleId", env.role.ID.String()) {
		t.Fatalf("seeded role missing from %v", body)
	}

	// Create: name is trimmed, instruction is optional.
	status, body = env.asAlice(http.MethodPost, "/v1/roles", map[string]any{
		"name": "  Night Shift  ", "instruction": "  stay calm  ",
	})
	wantStatus(t, status, body, http.StatusCreated, "create role")
	created, _ := body["role"].(map[string]any)
	roleID, _ := uuid.Parse(stringField(created, "roleId"))
	if roleID == uuid.Nil || created["name"] != "Night Shift" || created["instruction"] != "stay calm" {
		t.Fatalf("created role = %v", body)
	}
	status, body = env.asAlice(http.MethodPost, "/v1/roles", map[string]any{"name": "   "})
	wantStatus(t, status, body, http.StatusBadRequest, "create role without a name")

	// Pagination is bounded.
	status, body = env.asAlice(http.MethodGet, "/v1/roles?limit=1", nil)
	wantStatus(t, status, body, http.StatusOK, "list roles limit 1")
	if len(listField(body, "roles")) > 1 {
		t.Fatalf("limit ignored: %v", body)
	}
	status, body = env.asAlice(http.MethodGet, "/v1/roles?limit=0", nil)
	wantStatus(t, status, body, http.StatusBadRequest, "list roles limit 0")
	status, body = env.asAlice(http.MethodGet, "/v1/roles?limit=501", nil)
	wantStatus(t, status, body, http.StatusBadRequest, "list roles limit 501")

	// PATCH preserves the fields it does not name.
	status, body = env.asAlice(http.MethodPatch, "/v1/roles/"+roleID.String(), map[string]any{"name": "Day Shift"})
	wantStatus(t, status, body, http.StatusOK, "patch role")
	patched, _ := body["role"].(map[string]any)
	if patched["name"] != "Day Shift" || patched["instruction"] != "stay calm" {
		t.Fatalf("patched role = %v", body)
	}
	status, body = env.asAlice(http.MethodPatch, "/v1/roles/"+roleID.String(), map[string]any{})
	wantStatus(t, status, body, http.StatusBadRequest, "empty role patch")

	// A role still named by a profile cannot be deleted (FK RESTRICT -> 409).
	status, body = env.asAlice(http.MethodDelete, "/v1/roles/"+env.role.ID.String(), nil)
	wantStatus(t, status, body, http.StatusConflict, "delete referenced role")

	// Cross-tenant ids are 404 for read, patch and delete.
	foreignRole, err := env.st.CreateRole(context.Background(), store.RoleRecord{
		TenantID: env.otherTenant, ID: uuid.New(), Name: "Foreign",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, probe := range []struct {
		method string
		body   any
	}{
		{http.MethodGet, nil},
		{http.MethodPatch, map[string]any{"name": "Stolen"}},
		{http.MethodDelete, nil},
	} {
		status, body = env.asAlice(probe.method, "/v1/roles/"+foreignRole.ID.String(), probe.body)
		wantStatus(t, status, body, http.StatusNotFound, "cross-tenant role "+probe.method)
	}

	// Delete the unreferenced role, then read it back as missing.
	status, body = env.asAlice(http.MethodDelete, "/v1/roles/"+roleID.String(), nil)
	wantStatus(t, status, body, http.StatusOK, "delete role")
	status, body = env.asAlice(http.MethodGet, "/v1/roles/"+roleID.String(), nil)
	wantStatus(t, status, body, http.StatusNotFound, "get deleted role")
}

// TestProfilesHTTPSurface pins profile CRUD, the same-tenant account/role
// requirement, bounded pagination, the 409 Bot-reference guard and the
// ownership move that a profile rebind must perform.
func TestProfilesHTTPSurface(t *testing.T) {
	env := newIdentityEnv(t)
	ctx := context.Background()

	status, body := env.asBobGet("/v1/profiles")
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "bob lists profiles")
	status, body = env.do(http.MethodPost, "/v1/profiles", env.bob, false, map[string]any{
		"accountId": env.bob.ID.String(), "roleId": env.role.ID.String(),
	})
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "bob creates a profile")

	status, body = env.asAlice(http.MethodGet, "/v1/profiles", nil)
	wantStatus(t, status, body, http.StatusOK, "list profiles")
	if !containsRecordID(listField(body, "profiles"), "profileId", env.aliceProfile.ID.String()) {
		t.Fatalf("seeded profile missing from %v", body)
	}
	status, body = env.asAlice(http.MethodGet, "/v1/profiles?limit=0", nil)
	wantStatus(t, status, body, http.StatusBadRequest, "list profiles limit 0")
	status, body = env.asAlice(http.MethodGet, "/v1/profiles?limit=501", nil)
	wantStatus(t, status, body, http.StatusBadRequest, "list profiles limit 501")
	status, body = env.asAlice(http.MethodGet, "/v1/profiles?limit=1", nil)
	wantStatus(t, status, body, http.StatusOK, "list profiles limit 1")
	if len(listField(body, "profiles")) > 1 {
		t.Fatalf("limit ignored: %v", body)
	}

	// A profile may only bind an account and a role of the same tenant.
	for _, probe := range []struct {
		name string
		body map[string]any
		code string
	}{
		{"missing account", map[string]any{"accountId": uuid.NewString(), "roleId": env.role.ID.String()}, "invalid_account_id"},
		{"missing role", map[string]any{"accountId": env.bob.ID.String(), "roleId": uuid.NewString()}, "invalid_role_id"},
		{"foreign account", map[string]any{"accountId": env.otherAcct.ID.String(), "roleId": env.role.ID.String()}, "invalid_account_id"},
	} {
		status, body = env.asAlice(http.MethodPost, "/v1/profiles", probe.body)
		wantStatus(t, status, body, http.StatusBadRequest, probe.name)
		if body["code"] != probe.code {
			t.Fatalf("%s: code = %v, want %q", probe.name, body["code"], probe.code)
		}
	}
	foreignRole, err := env.st.CreateRole(ctx, store.RoleRecord{
		TenantID: env.otherTenant, ID: uuid.New(), Name: "Foreign",
	})
	if err != nil {
		t.Fatal(err)
	}
	status, body = env.asAlice(http.MethodPost, "/v1/profiles", map[string]any{
		"accountId": env.bob.ID.String(), "roleId": foreignRole.ID.String(),
	})
	wantStatus(t, status, body, http.StatusBadRequest, "foreign role")
	if body["code"] != "invalid_role_id" {
		t.Fatalf("foreign role code = %v", body["code"])
	}

	// Create a profile for Bob.
	status, body = env.asAlice(http.MethodPost, "/v1/profiles", map[string]any{
		"accountId": env.bob.ID.String(), "roleId": env.role.ID.String(),
	})
	wantStatus(t, status, body, http.StatusCreated, "create profile")
	created, _ := body["profile"].(map[string]any)
	profileID, _ := uuid.Parse(stringField(created, "profileId"))
	if profileID == uuid.Nil || created["accountId"] != env.bob.ID.String() {
		t.Fatalf("created profile = %v", body)
	}
	status, body = env.asAlice(http.MethodGet, "/v1/profiles/"+profileID.String(), nil)
	wantStatus(t, status, body, http.StatusOK, "get profile")

	// Changing only the role does not move ownership.
	status, body = env.asAlice(http.MethodPost, "/v1/roles", map[string]any{"name": "Second Role"})
	wantStatus(t, status, body, http.StatusCreated, "create second role")
	secondRole, _ := body["role"].(map[string]any)
	secondRoleID := stringField(secondRole, "roleId")
	status, body = env.asAlice(http.MethodPatch, "/v1/profiles/"+profileID.String(), map[string]any{
		"roleId": secondRoleID,
	})
	wantStatus(t, status, body, http.StatusOK, "patch profile role")
	patched, _ := body["profile"].(map[string]any)
	if patched["roleId"] != secondRoleID || patched["accountId"] != env.bob.ID.String() {
		t.Fatalf("patched profile = %v", body)
	}
	status, body = env.asAlice(http.MethodPatch, "/v1/profiles/"+profileID.String(), map[string]any{})
	wantStatus(t, status, body, http.StatusBadRequest, "empty profile patch")

	// Rebinding the Bot's profile to Alice moves the Bot's owner relation in the
	// same transaction. Before the move Alice has bot.read (tenant) but no
	// relation; after it the same request succeeds.
	status, body = env.asAlice(http.MethodGet, "/v1/bots/"+env.supportBot.ID, nil)
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingRelation, "alice reads support-1 before the move")
	status, body = env.asAlice(http.MethodPatch, "/v1/profiles/"+env.botProfile.ID.String(), map[string]any{
		"accountId": env.alice.ID.String(),
	})
	wantStatus(t, status, body, http.StatusOK, "move support profile to alice")
	status, body = env.asAlice(http.MethodGet, "/v1/bots/"+env.supportBot.ID, nil)
	wantStatus(t, status, body, http.StatusOK, "alice reads support-1 after the move")
	oldOwner, err := env.st.HasResourceRelation(ctx, env.tenant, "bot", env.supportBot.ID,
		store.RelationOwner, store.RelationSubjectAccount, env.botAccount.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	newOwner, err := env.st.HasResourceRelation(ctx, env.tenant, "bot", env.supportBot.ID,
		store.RelationOwner, store.RelationSubjectAccount, env.alice.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if oldOwner || !newOwner {
		t.Fatalf("owner relation not moved: old=%v new=%v", oldOwner, newOwner)
	}

	// A Bot-referenced profile cannot be deleted (FK RESTRICT -> 409).
	status, body = env.asAlice(http.MethodDelete, "/v1/profiles/"+env.botProfile.ID.String(), nil)
	wantStatus(t, status, body, http.StatusConflict, "delete referenced profile")

	// Cross-tenant ids are 404 for read, patch and delete.
	foreignProfile, err := env.st.CreateProfile(ctx, store.ProfileRecord{
		TenantID: env.otherTenant, ID: uuid.New(), AccountID: env.otherAcct.ID, RoleID: foreignRole.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, probe := range []struct {
		method string
		body   any
	}{
		{http.MethodGet, nil},
		{http.MethodPatch, map[string]any{"roleId": env.role.ID.String()}},
		{http.MethodDelete, nil},
	} {
		status, body = env.asAlice(probe.method, "/v1/profiles/"+foreignProfile.ID.String(), probe.body)
		wantStatus(t, status, body, http.StatusNotFound, "cross-tenant profile "+probe.method)
	}

	// Delete the unreferenced profile, then read it back as missing.
	status, body = env.asAlice(http.MethodDelete, "/v1/profiles/"+profileID.String(), nil)
	wantStatus(t, status, body, http.StatusOK, "delete profile")
	status, body = env.asAlice(http.MethodGet, "/v1/profiles/"+profileID.String(), nil)
	wantStatus(t, status, body, http.StatusNotFound, "get deleted profile")
}

func containsRecordID(records []any, field, want string) bool {
	for _, raw := range records {
		record, _ := raw.(map[string]any)
		if record[field] == want {
			return true
		}
	}
	return false
}

func stringField(record map[string]any, field string) string {
	value, _ := record[field].(string)
	return value
}
