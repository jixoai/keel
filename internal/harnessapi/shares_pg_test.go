package harnessapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/store"
)

// shareGrant gives one account a direct (key, scope) grant. The fixture's
// bob/limited accounts start with none; the tests grant exactly the capability
// under test.
func shareGrant(t *testing.T, env *identityEnv, accountID uuid.UUID, key string, scope access.Scope) {
	t.Helper()
	if _, err := env.st.CreatePermissionGrant(context.Background(), store.PermissionGrantRecord{
		TenantID: env.tenant, ID: uuid.New(), SubjectType: store.PermissionSubjectAccount,
		SubjectID: accountID, PermissionKey: key, Scope: string(scope), GrantedBy: env.super.ID,
	}); err != nil {
		t.Fatal(err)
	}
}

// sharePrincipal resolves an account through the real access bridge (so Bot
// harness narrowing is exercised too).
func sharePrincipal(t *testing.T, env *identityEnv, account store.AccountRecord, botID string) access.Principal {
	t.Helper()
	kind := auth.PrincipalHuman
	if botID != "" {
		kind = auth.PrincipalBot
	}
	principal, err := env.bridge.Principal(auth.WithPrincipal(context.Background(), auth.Principal{
		Kind: kind, TenantID: env.tenant, AccountID: account.ID, BotID: botID,
	}), env.tenant)
	if err != nil {
		t.Fatal(err)
	}
	return principal
}

// shareAllowed runs one engine decision. It is the "next request" the product
// promise talks about: the engine reads relations live, so no cache is involved.
func shareAllowed(t *testing.T, env *identityEnv, principal access.Principal, key, resourceType, resourceID string) bool {
	t.Helper()
	decision, err := env.bridge.Authorize(context.Background(), principal, key,
		access.Resource{TenantID: env.tenant, Type: resourceType, ID: resourceID})
	if err != nil {
		t.Fatal(err)
	}
	return decision.Allow
}

func shareIDs(body map[string]any) []map[string]any {
	raw, _ := body["shares"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		entry, _ := item.(map[string]any)
		if entry != nil {
			out = append(out, entry)
		}
	}
	return out
}

func shareRelationRows(t *testing.T, env *identityEnv, action, resourceID string) int {
	t.Helper()
	events, err := env.st.ListAuthEvents(context.Background(), env.tenant,
		store.AuthEventFilter{Kind: store.AuthEventRelationChanged}, 200)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Detail["action"] == action && event.Detail["resource_id"] == resourceID {
			count++
		}
	}
	return count
}

// TestSessionShareSurface is the A6 session journey: owner shares viewer, the
// viewer reads but cannot write, an editor writes but cannot re-share, revoke
// takes effect on the next decision, and the list projections carry ownership.
func TestSessionShareSurface(t *testing.T) {
	env := newIdentityEnv(t)
	ctx := context.Background()
	sessionID := uuid.New()
	if err := env.st.CreateOwnedSession(ctx, store.SessionOwnership{
		TenantID: env.tenant, SessionID: sessionID, BotID: env.aliceBot.ID,
		AccountID: env.alice.ID, CreatedByAccountID: env.alice.ID,
	}); err != nil {
		t.Fatal(err)
	}
	sessionPath := "/v1/sessions/" + sessionID.String() + "/shares"
	shareGrant(t, env, env.bob.ID, access.KeySessionRead, access.ScopeOwn)
	shareGrant(t, env, env.bob.ID, access.KeySessionWrite, access.ScopeOwn)

	// An unauthorized caller gets the same 403 for an existing and a random
	// session: the relation check ran before any row lookup.
	statusExisting, bodyExisting := env.do(http.MethodGet, sessionPath, env.bob, false, nil)
	statusMissing, bodyMissing := env.do(http.MethodGet,
		"/v1/sessions/"+uuid.NewString()+"/shares", env.bob, false, nil)
	wantReason(t, statusExisting, bodyExisting, http.StatusForbidden, access.ReasonMissingGrant, "bob lists existing shares")
	wantReason(t, statusMissing, bodyMissing, http.StatusForbidden, access.ReasonMissingGrant, "bob lists missing shares")
	if bodyExisting["requiredKey"] != bodyMissing["requiredKey"] {
		t.Fatalf("existence leak: requiredKey %v vs %v", bodyExisting["requiredKey"], bodyMissing["requiredKey"])
	}

	// Self-share is a 400 before any write.
	status, body := env.asAlice(http.MethodPost, sessionPath, map[string]any{
		"subjectType": "account", "subjectId": env.alice.ID.String(), "relation": "viewer",
	})
	wantStatus(t, status, body, http.StatusBadRequest, "self share")

	// Owner grants viewer: 201 created, display name resolved, audit row written.
	status, body = env.asAlice(http.MethodPost, sessionPath, map[string]any{
		"subjectType": "account", "subjectId": env.bob.ID.String(), "relation": "viewer",
	})
	wantStatus(t, status, body, http.StatusCreated, "share viewer")
	if body["created"] != true {
		t.Fatalf("share viewer created = %v", body["created"])
	}
	share, _ := body["share"].(map[string]any)
	if share["displayName"] != "bob" || share["relation"] != "viewer" {
		t.Fatalf("share view = %v", share)
	}
	if rows := shareRelationRows(t, env, "share", sessionID.String()); rows != 1 {
		t.Fatalf("audit rows after share = %d, want 1", rows)
	}

	// Duplicate is idempotent.
	status, body = env.asAlice(http.MethodPost, sessionPath, map[string]any{
		"subjectType": "account", "subjectId": env.bob.ID.String(), "relation": "viewer",
	})
	wantStatus(t, status, body, http.StatusOK, "duplicate share")
	if body["created"] != false {
		t.Fatalf("duplicate created = %v", body["created"])
	}

	// List carries the tuple, bounded and ordered.
	status, body = env.asAlice(http.MethodGet, sessionPath, nil)
	wantStatus(t, status, body, http.StatusOK, "list shares")
	if shares := shareIDs(body); len(shares) != 1 || shares[0]["subjectId"] != env.bob.ID.String() {
		t.Fatalf("shares = %v", body["shares"])
	}

	// Viewer can read, cannot write.
	bobPrincipal := sharePrincipal(t, env, env.bob, "")
	if !shareAllowed(t, env, bobPrincipal, access.KeySessionRead, "session", sessionID.String()) {
		t.Fatal("viewer cannot read the session they were shared")
	}
	if shareAllowed(t, env, bobPrincipal, access.KeySessionWrite, "session", sessionID.String()) {
		t.Fatal("viewer can write the session")
	}

	// A session.write-only caller cannot share.
	status, body = env.do(http.MethodPost, sessionPath, env.bob, false, map[string]any{
		"subjectType": "account", "subjectId": env.limited.ID.String(), "relation": "viewer",
	})
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "write-only caller shares")

	// Editor can write and still cannot re-share (share requires owner).
	status, body = env.asAlice(http.MethodPost, sessionPath, map[string]any{
		"subjectType": "account", "subjectId": env.bob.ID.String(), "relation": "editor",
	})
	wantStatus(t, status, body, http.StatusCreated, "share editor")
	if !shareAllowed(t, env, bobPrincipal, access.KeySessionWrite, "session", sessionID.String()) {
		t.Fatal("editor cannot write the session")
	}
	shareGrant(t, env, env.bob.ID, access.KeySessionShare, access.ScopeOwn)
	status, body = env.do(http.MethodPost, sessionPath, env.bob, false, map[string]any{
		"subjectType": "account", "subjectId": env.limited.ID.String(), "relation": "viewer",
	})
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingRelation, "editor re-shares")

	// Revocation is live: the next decision and the next list both see it.
	// Both tuples have to go (the editor grant did not replace the viewer one:
	// relations are additive, like grants).
	status, body = env.asAlice(http.MethodPost, sessionPath+"/revoke", map[string]any{
		"subjectType": "account", "subjectId": env.bob.ID.String(), "relation": "editor",
	})
	wantStatus(t, status, body, http.StatusOK, "revoke editor")
	status, body = env.asAlice(http.MethodPost, sessionPath+"/revoke", map[string]any{
		"subjectType": "account", "subjectId": env.bob.ID.String(), "relation": "viewer",
	})
	wantStatus(t, status, body, http.StatusOK, "revoke viewer")
	if shareAllowed(t, env, bobPrincipal, access.KeySessionRead, "session", sessionID.String()) {
		t.Fatal("revoked viewer still reads the session")
	}
	status, body = env.asAlice(http.MethodGet, "/v1/sessions", nil)
	wantStatus(t, status, body, http.StatusOK, "owner lists sessions")
	sessions, _ := body["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("owner sessions = %v", body["sessions"])
	}
	summary, _ := sessions[0].(map[string]any)
	if summary["ownerAccountId"] != env.alice.ID.String() || summary["createdByAccountId"] != env.alice.ID.String() {
		t.Fatalf("session ownership fields = %v", summary)
	}
	if summary["shared"] != false {
		t.Fatalf("revoked session still marks shared: %v", summary)
	}

	// Revoke absent tuple is 404.
	status, body = env.asAlice(http.MethodPost, sessionPath+"/revoke", map[string]any{
		"subjectType": "account", "subjectId": env.bob.ID.String(), "relation": "viewer",
	})
	wantStatus(t, status, body, http.StatusNotFound, "revoke absent tuple")

	// Owner/team_member can never be granted through sharing.
	status, body = env.asAlice(http.MethodPost, sessionPath, map[string]any{
		"subjectType": "account", "subjectId": env.bob.ID.String(), "relation": "owner",
	})
	wantStatus(t, status, body, http.StatusBadRequest, "grant owner")

	// Shared list + sharedWithMe are computed against the caller.
	status, body = env.asAlice(http.MethodPost, sessionPath, map[string]any{
		"subjectType": "account", "subjectId": env.bob.ID.String(), "relation": "viewer",
	})
	wantStatus(t, status, body, http.StatusCreated, "re-share viewer")
	status, body = env.do(http.MethodGet, "/v1/sessions", env.bob, false, nil)
	wantStatus(t, status, body, http.StatusOK, "viewer lists sessions")
	rows, _ := body["sessions"].([]any)
	if len(rows) != 1 {
		t.Fatalf("viewer sessions = %v", body["sessions"])
	}
	shared, _ := rows[0].(map[string]any)
	if shared["sharedWithMe"] != true || shared["ownerDisplayName"] != "alice" {
		t.Fatalf("shared-with-me projection = %v", shared)
	}
	status, body = env.asAlice(http.MethodGet, "/v1/sessions", nil)
	wantStatus(t, status, body, http.StatusOK, "owner lists shared sessions")
	rows, _ = body["sessions"].([]any)
	ownerRow, _ := rows[0].(map[string]any)
	if ownerRow["shared"] != true || ownerRow["sharedWithMe"] != false {
		t.Fatalf("owner shared projection = %v", ownerRow)
	}
}

// TestBotShareSurface is the A6 bot journey: a shared viewer can read and be
// rostered/invoked by another account, but can never manage or re-share.
func TestBotShareSurface(t *testing.T) {
	env := newIdentityEnv(t)
	sharePath := "/v1/bots/" + env.aliceBot.ID + "/shares"
	shareGrant(t, env, env.bob.ID, access.KeyBotRead, access.ScopeOwn)
	shareGrant(t, env, env.bob.ID, access.KeyBotInvoke, access.ScopeOwn)
	shareGrant(t, env, env.bob.ID, access.KeyTeamBotManage, access.ScopeOwn)

	// Bob holds no bot.manage: one 403 whether or not the bot exists.
	status, body := env.do(http.MethodGet, sharePath, env.bob, false, nil)
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "bob lists bot shares")
	statusMissing, bodyMissing := env.do(http.MethodGet, "/v1/bots/absent-1/shares", env.bob, false, nil)
	wantReason(t, statusMissing, bodyMissing, http.StatusForbidden, access.ReasonMissingGrant, "bob lists absent bot shares")

	status, body = env.asAlice(http.MethodPost, sharePath, map[string]any{
		"subjectType": "account", "subjectId": env.bob.ID.String(), "relation": "viewer",
	})
	wantStatus(t, status, body, http.StatusCreated, "share bot viewer")
	status, body = env.asAlice(http.MethodGet, sharePath, nil)
	wantStatus(t, status, body, http.StatusOK, "list bot shares")
	if shares := shareIDs(body); len(shares) != 1 || shares[0]["displayName"] != "bob" {
		t.Fatalf("bot shares = %v", body["shares"])
	}

	// Viewer gets read + invoke through the relation lattice.
	status, body = env.do(http.MethodGet, "/v1/bots/"+env.aliceBot.ID, env.bob, false, nil)
	wantStatus(t, status, body, http.StatusOK, "viewer reads bot")
	status, body = env.do(http.MethodPost, "/v1/bots/"+env.aliceBot.ID+"/invoke", env.bob, false, map[string]any{"text": "hi"})
	wantStatus(t, status, body, http.StatusCreated, "viewer invokes bot")

	// Cross-account roster: bob can seat and start a bot shared with him.
	status, body = env.do(http.MethodPost, "/v1/team-bots", env.bob, false, map[string]any{
		"teamBotId": "bob-shared-team", "name": "Bob Shared", "mission": "use alice's bot",
		"roster": []map[string]any{{"botId": env.aliceBot.ID, "role": "captain"}},
	})
	wantStatus(t, status, body, http.StatusCreated, "bob rosters shared bot")

	// Viewer cannot manage or re-share: the key is held, but manage requires the
	// owner relation (viewer never implies owner).
	shareGrant(t, env, env.bob.ID, access.KeyBotManage, access.ScopeOwn)
	status, body = env.do(http.MethodPatch, "/v1/bots/"+env.aliceBot.ID, env.bob, false, map[string]any{"name": "stolen"})
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingRelation, "viewer manages bot")
	status, body = env.do(http.MethodPost, sharePath, env.bob, false, map[string]any{
		"subjectType": "account", "subjectId": env.limited.ID.String(), "relation": "viewer",
	})
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingRelation, "viewer re-shares bot")

	// Bot list badge sees the share.
	status, body = env.asAlice(http.MethodGet, "/v1/bots", nil)
	wantStatus(t, status, body, http.StatusOK, "owner lists bots")
	found := false
	for _, item := range listField(body, "bots") {
		entry, _ := item.(map[string]any)
		if entry["botId"] == env.aliceBot.ID {
			found = entry["sharedCount"] == float64(1)
		}
	}
	if !found {
		t.Fatalf("bot sharedCount missing: %v", body["bots"])
	}

	// Revoke: the next read and invoke are denied.
	status, body = env.asAlice(http.MethodPost, sharePath+"/revoke", map[string]any{
		"subjectType": "account", "subjectId": env.bob.ID.String(), "relation": "viewer",
	})
	wantStatus(t, status, body, http.StatusOK, "revoke bot viewer")
	status, body = env.do(http.MethodGet, "/v1/bots/"+env.aliceBot.ID, env.bob, false, nil)
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingRelation, "revoked viewer reads bot")
	status, body = env.do(http.MethodPost, "/v1/bots/"+env.aliceBot.ID+"/invoke", env.bob, false, map[string]any{"text": "hi"})
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingRelation, "revoked viewer invokes bot")
}

// TestTeamBotShareSurface pins the visibility-only semantics: viewer/editor
// can read the team, but starting and sharing still require the owner relation.
func TestTeamBotShareSurface(t *testing.T) {
	env := newIdentityEnv(t)
	roster := []map[string]any{{"botId": env.aliceBot.ID, "role": "captain"}}
	status, body := env.asAlice(http.MethodPost, "/v1/team-bots", map[string]any{
		"teamBotId": "shared-team", "name": "Shared Team", "mission": "visibility only", "roster": roster,
	})
	wantStatus(t, status, body, http.StatusCreated, "create team bot")
	created, _ := body["teamBot"].(map[string]any)
	if created["ownerAccountId"] != env.alice.ID.String() {
		t.Fatalf("team owner in create view = %v", created)
	}
	shareGrant(t, env, env.bob.ID, access.KeyTeamBotRead, access.ScopeOwn)
	shareGrant(t, env, env.bob.ID, access.KeyTeamBotManage, access.ScopeOwn)
	sharePath := "/v1/team-bots/shared-team/shares"

	// Bob holds team_bot.manage but is not the owner: sharing is refused by the
	// relation, not by the key.
	status, body = env.do(http.MethodPost, sharePath, env.bob, false, map[string]any{
		"subjectType": "account", "subjectId": env.limited.ID.String(), "relation": "viewer",
	})
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingRelation, "non-owner shares team bot")

	status, body = env.asAlice(http.MethodPost, sharePath, map[string]any{
		"subjectType": "account", "subjectId": env.bob.ID.String(), "relation": "viewer",
	})
	wantStatus(t, status, body, http.StatusCreated, "share team viewer")

	// Viewer reads but cannot start (start is a manage action).
	status, body = env.do(http.MethodGet, "/v1/team-bots/shared-team", env.bob, false, nil)
	wantStatus(t, status, body, http.StatusOK, "viewer reads team bot")
	status, body = env.do(http.MethodPost, "/v1/team-bots/shared-team/start", env.bob, false, map[string]any{"text": "go"})
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingRelation, "viewer starts team bot")

	// Revoke: the next read is denied.
	status, body = env.asAlice(http.MethodPost, sharePath+"/revoke", map[string]any{
		"subjectType": "account", "subjectId": env.bob.ID.String(), "relation": "viewer",
	})
	wantStatus(t, status, body, http.StatusOK, "revoke team viewer")
	status, body = env.do(http.MethodGet, "/v1/team-bots/shared-team", env.bob, false, nil)
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingRelation, "revoked viewer reads team bot")
}

// TestShareSubjectValidation covers group/team_bot subjects and the closed
// failure set: malformed 400, missing/foreign 404, owner/team_member 400.
func TestShareSubjectValidation(t *testing.T) {
	env := newIdentityEnv(t)
	botPath := "/v1/bots/" + env.aliceBot.ID + "/shares"

	// A group subject resolves and reads back with its display name.
	status, body := env.asAlice(http.MethodPost, botPath, map[string]any{
		"subjectType": "group", "subjectId": env.opsGroup.ID.String(), "relation": "viewer",
	})
	wantStatus(t, status, body, http.StatusCreated, "share to group")
	share, _ := body["share"].(map[string]any)
	if share["displayName"] != "ops" {
		t.Fatalf("group share view = %v", share)
	}
	// A member of that group reaches the bot at tenant scope (group relations
	// are not "direct", so own scope correctly does not reach them).
	if _, err := env.st.AddPermissionGroupMember(context.Background(), env.tenant, env.opsGroup.ID, env.bob.ID, env.alice.ID); err != nil {
		t.Fatal(err)
	}
	shareGrant(t, env, env.bob.ID, access.KeyBotRead, access.ScopeTenant)
	status, body = env.do(http.MethodGet, "/v1/bots/"+env.aliceBot.ID, env.bob, false, nil)
	wantStatus(t, status, body, http.StatusOK, "group member reads shared bot")

	// A team_bot subject resolves; the principal must be a Bot in its roster.
	status, body = env.asAlice(http.MethodPost, "/v1/team-bots", map[string]any{
		"teamBotId": "subject-team", "name": "Subject Team", "mission": "m",
		"roster": []map[string]any{{"botId": env.aliceBot.ID, "role": "captain"}},
	})
	wantStatus(t, status, body, http.StatusCreated, "create subject team")
	status, body = env.asAlice(http.MethodPost, botPath, map[string]any{
		"subjectType": "team_bot", "subjectId": "subject-team", "relation": "viewer",
	})
	wantStatus(t, status, body, http.StatusCreated, "share to team bot subject")
	teamBotShare, _ := body["share"].(map[string]any)
	if teamBotShare["displayName"] != "Subject Team" {
		t.Fatalf("team_bot share view = %v", teamBotShare)
	}

	// Malformed subjects are 400; missing and foreign subjects are the same 404.
	status, body = env.asAlice(http.MethodPost, botPath, map[string]any{
		"subjectType": "account", "subjectId": "not-a-uuid", "relation": "viewer",
	})
	wantStatus(t, status, body, http.StatusBadRequest, "malformed account subject")
	status, body = env.asAlice(http.MethodPost, botPath, map[string]any{
		"subjectType": "account", "subjectId": uuid.NewString(), "relation": "viewer",
	})
	wantStatus(t, status, body, http.StatusNotFound, "missing account subject")
	status, body = env.asAlice(http.MethodPost, botPath, map[string]any{
		"subjectType": "account", "subjectId": env.otherAcct.ID.String(), "relation": "viewer",
	})
	wantStatus(t, status, body, http.StatusNotFound, "cross-tenant account subject")
	status, body = env.asAlice(http.MethodPost, botPath, map[string]any{
		"subjectType": "team_bot", "subjectId": "absent-team", "relation": "viewer",
	})
	wantStatus(t, status, body, http.StatusNotFound, "missing team_bot subject")
	status, body = env.asAlice(http.MethodPost, botPath, map[string]any{
		"subjectType": "account", "subjectId": env.bob.ID.String(), "relation": "team_member",
	})
	wantStatus(t, status, body, http.StatusBadRequest, "grant team_member")
	status, body = env.asAlice(http.MethodPost, botPath, map[string]any{
		"subjectType": "tenant", "subjectId": env.bob.ID.String(), "relation": "viewer",
	})
	wantStatus(t, status, body, http.StatusBadRequest, "unknown subject type")
}

// TestShareTenantBoundary proves a group/team_bot subject from another tenant
// never resolves (the store is tenant-scoped and the handler fails closed).
func TestShareTenantBoundary(t *testing.T) {
	env := newIdentityEnv(t)
	foreignGroup, err := env.st.CreatePermissionGroup(context.Background(), store.PermissionGroupRecord{
		TenantID: env.otherTenant, ID: uuid.New(), Name: "foreign",
	})
	if err != nil {
		t.Fatal(err)
	}
	status, body := env.asAlice(http.MethodPost, "/v1/sessions/"+uuid.NewString()+"/shares", map[string]any{
		"subjectType": "group", "subjectId": foreignGroup.ID.String(), "relation": "viewer",
	})
	// The random session is 403 for the owner check first: alice is tenant admin
	// but has no relation to a session that does not exist.
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingRelation, "missing session")

	sessionID := uuid.New()
	if err := env.st.CreateOwnedSession(context.Background(), store.SessionOwnership{
		TenantID: env.tenant, SessionID: sessionID, BotID: env.aliceBot.ID,
		AccountID: env.alice.ID, CreatedByAccountID: env.alice.ID,
	}); err != nil {
		t.Fatal(err)
	}
	status, body = env.asAlice(http.MethodPost, "/v1/sessions/"+sessionID.String()+"/shares", map[string]any{
		"subjectType": "group", "subjectId": foreignGroup.ID.String(), "relation": "viewer",
	})
	wantStatus(t, status, body, http.StatusNotFound, "cross-tenant group subject")

	// A super admin passes the owner check (key + all scope) but still gets an
	// honest 404 for an absent resource: authority never invents rows.
	status, body = env.asSuper(http.MethodGet, "/v1/sessions/"+uuid.NewString()+"/shares", nil)
	wantStatus(t, status, body, http.StatusNotFound, "super admin on absent session")
}
