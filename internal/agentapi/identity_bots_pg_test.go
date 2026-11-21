package agentapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/store"
)

func TestBotsHTTPSurface(t *testing.T) {
	env := newIdentityEnv(t)

	// Alice is a tenant admin (bot.read at tenant) but only owns alice-1: the
	// relation filter, not the scope, decides the list.
	status, body := env.asAlice(http.MethodGet, "/v1/bots", nil)
	wantStatus(t, status, body, http.StatusOK, "alice lists bots")
	ids := botIDs(body)
	if len(ids) != 1 || ids[0] != env.aliceBot.ID {
		t.Fatalf("alice bots = %v, want [%s]", ids, env.aliceBot.ID)
	}
	// The super admin sees both.
	status, body = env.asSuper(http.MethodGet, "/v1/bots", nil)
	wantStatus(t, status, body, http.StatusOK, "super lists bots")
	if got := botIDs(body); len(got) != 2 {
		t.Fatalf("super bots = %v, want both", got)
	}
	status, body = env.asAlice(http.MethodGet, "/v1/bots/"+env.supportBot.ID, nil)
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingRelation, "alice reads support-1")
	status, body = env.do(http.MethodGet, "/v1/bots", env.bob, false, nil)
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "bob lists bots")

	// Create: Alice may, the limited account may not.
	status, body = env.asAlice(http.MethodPost, "/v1/bots", map[string]any{
		"botId": "alice-2", "name": "Alice Two", "profileId": env.aliceProfile.ID.String(),
		"modelKind":    "text",
		"defaultModel": map[string]any{"providerId": "fake", "modelId": "fake"},
		"harness":      map[string]any{"tools": []string{}, "context": map[string]any{"permissions": []string{"session.read"}}},
	})
	wantStatus(t, status, body, http.StatusCreated, "alice creates a bot")
	created, _ := body["bot"].(map[string]any)
	if created["accountId"] != env.alice.ID.String() {
		t.Fatalf("created bot owner = %v", created)
	}
	if _, err := env.st.HasResourceRelation(context.Background(), env.tenant, "bot", "alice-2",
		store.RelationOwner, store.RelationSubjectAccount, env.alice.ID.String()); err != nil {
		t.Fatal(err)
	}
	status, body = env.do(http.MethodPost, "/v1/bots", env.limited, false, map[string]any{
		"name": "nope", "profileId": env.aliceProfile.ID.String(), "modelKind": "text",
		"defaultModel": map[string]any{"providerId": "fake", "modelId": "fake"},
		"harness":      map[string]any{"tools": []string{}, "context": map[string]any{"permissions": []string{}}},
	})
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "limited creates a bot")
	// Invalid model kind is a field-level 400, not a 403.
	status, body = env.asAlice(http.MethodPost, "/v1/bots", map[string]any{
		"name": "bad", "profileId": env.aliceProfile.ID.String(), "modelKind": "telepathy",
		"defaultModel": map[string]any{"providerId": "fake", "modelId": "fake"},
		"harness":      map[string]any{"tools": []string{}, "context": map[string]any{"permissions": []string{}}},
	})
	wantStatus(t, status, body, http.StatusBadRequest, "invalid model kind")

	// CAS update: wrong expectedArchived after an archive is a 409.
	status, body = env.asAlice(http.MethodPatch, "/v1/bots/"+env.aliceBot.ID, map[string]any{
		"name": "Alice One",
	})
	wantStatus(t, status, body, http.StatusOK, "update bot name")
	status, body = env.asAlice(http.MethodPost, "/v1/bots/"+env.aliceBot.ID+"/archive", nil)
	wantStatus(t, status, body, http.StatusOK, "archive bot")
	status, body = env.asAlice(http.MethodPatch, "/v1/bots/"+env.aliceBot.ID, map[string]any{
		"name": "Resurrected", "expectedArchived": false,
	})
	wantStatus(t, status, body, http.StatusConflict, "stale update after archive")
	status, body = env.asAlice(http.MethodGet, "/v1/bots?includeArchived=1", nil)
	wantStatus(t, status, body, http.StatusOK, "list archived")
	if !containsID(botIDs(body), env.aliceBot.ID) {
		t.Fatalf("archived bot missing from includeArchived list: %v", body)
	}

	// A second bot for the invoke tests (alice-1 is archived now).
	status, body = env.asAlice(http.MethodPost, "/v1/bots", map[string]any{
		"botId": "alice-3", "name": "Alice Three", "profileId": env.aliceProfile.ID.String(),
		"modelKind":    "text",
		"defaultModel": map[string]any{"providerId": "fake", "modelId": "fake"},
		"harness":      map[string]any{"tools": []string{}, "context": map[string]any{"permissions": []string{"session.read"}}},
	})
	wantStatus(t, status, body, http.StatusCreated, "create invoke bot")
	status, body = env.asAlice(http.MethodPost, "/v1/bots/alice-3/invoke", map[string]any{"text": "hello"})
	wantStatus(t, status, body, http.StatusCreated, "invoke bot")
	sessionID, _ := uuid.Parse(body["sessionId"].(string))
	if sessionID == uuid.Nil {
		t.Fatalf("invoke answer = %v", body)
	}
	ownership, err := env.st.GetSessionOwnership(context.Background(), env.tenant, sessionID)
	if err != nil {
		t.Fatalf("session ownership: %v", err)
	}
	if ownership.BotID != "alice-3" || ownership.AccountID != env.alice.ID || ownership.CreatedByAccountID != env.alice.ID {
		t.Fatalf("session ownership = %+v", ownership)
	}
	owner, err := env.st.HasResourceRelation(context.Background(), env.tenant, "session", sessionID.String(),
		store.RelationOwner, store.RelationSubjectAccount, env.alice.ID.String())
	if err != nil || !owner {
		t.Fatalf("session owner relation = %v err=%v", owner, err)
	}
	// Default private: a same-tenant account holding session.read/tenant still has
	// no relation to this session.
	if _, err := env.st.CreatePermissionGrant(context.Background(), store.PermissionGrantRecord{
		TenantID: env.tenant, ID: uuid.New(), SubjectType: store.PermissionSubjectAccount,
		SubjectID: env.bob.ID, PermissionKey: access.KeySessionRead,
		Scope: string(access.ScopeTenant), GrantedBy: env.super.ID,
	}); err != nil {
		t.Fatal(err)
	}
	bobPrincipal, err := env.bridge.Principal(auth.WithPrincipal(context.Background(), auth.Principal{
		Kind: auth.PrincipalHuman, TenantID: env.tenant, AccountID: env.bob.ID,
	}), env.tenant)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := env.bridge.Authorize(context.Background(), bobPrincipal,
		access.KeySessionRead, access.Resource{TenantID: env.tenant, Type: "session", ID: sessionID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Allow {
		t.Fatal("an unrelated account read a private session")
	}
	status, body = env.do(http.MethodPost, "/v1/bots/alice-3/invoke", env.bob, false, map[string]any{"text": "x"})
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "bob invokes alice's bot")
	status, body = env.asAlice(http.MethodPost, "/v1/bots/alice-1/invoke", map[string]any{"text": "x"})
	wantStatus(t, status, body, http.StatusConflict, "invoke archived bot")
}

func botIDs(body map[string]any) []string {
	raw, _ := body["bots"].([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		entry, _ := item.(map[string]any)
		if id, ok := entry["botId"].(string); ok {
			out = append(out, id)
		}
	}
	return out
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func TestTeamBotsHTTPSurface(t *testing.T) {
	env := newIdentityEnv(t)
	roster := []map[string]any{{"botId": env.aliceBot.ID, "role": "captain"}}
	status, body := env.asAlice(http.MethodPost, "/v1/team-bots", map[string]any{
		"teamBotId": "support-team", "name": "Support Team",
		"mission": "resolve every ticket", "roster": roster,
	})
	wantStatus(t, status, body, http.StatusCreated, "create team bot")
	team, _ := body["teamBot"].(map[string]any)
	if team["revision"] != float64(1) || team["hash"] == "" {
		t.Fatalf("sealed team = %v", team)
	}
	owner, err := env.st.HasResourceRelation(context.Background(), env.tenant, "team_bot", "support-team",
		store.RelationOwner, store.RelationSubjectAccount, env.alice.ID.String())
	if err != nil || !owner {
		t.Fatalf("team owner relation = %v err=%v", owner, err)
	}

	// Roster seats require bot.invoke on the member: Alice owns alice-1 but not
	// support-1.
	status, body = env.asAlice(http.MethodPost, "/v1/team-bots", map[string]any{
		"teamBotId": "other-team", "name": "Other", "mission": "m",
		"roster": []map[string]any{{"botId": env.supportBot.ID, "role": "captain"}},
	})
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingRelation, "roster a bot without invoke")
	status, body = env.do(http.MethodPost, "/v1/team-bots", env.bob, false, map[string]any{
		"teamBotId": "bob-team", "name": "Bob", "mission": "m", "roster": roster,
	})
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "bob creates a team bot")

	status, body = env.asAlice(http.MethodGet, "/v1/team-bots", nil)
	wantStatus(t, status, body, http.StatusOK, "alice lists team bots")
	status, body = env.do(http.MethodGet, "/v1/team-bots", env.bob, false, nil)
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "bob lists team bots")
	status, body = env.asAlice(http.MethodGet, "/v1/team-bots/support-team", nil)
	wantStatus(t, status, body, http.StatusOK, "get team bot")

	// CAS roster edit: stale revision is a 409, the fresh one advances.
	status, body = env.asAlice(http.MethodPost, "/v1/team-bots/support-team/roster", map[string]any{
		"roster": roster, "expectedRevision": 99,
	})
	wantStatus(t, status, body, http.StatusConflict, "stale roster edit")
	status, body = env.asAlice(http.MethodPost, "/v1/team-bots/support-team/roster", map[string]any{
		"roster": roster, "expectedRevision": 1,
	})
	wantStatus(t, status, body, http.StatusOK, "roster edit")
	updated, _ := body["teamBot"].(map[string]any)
	if updated["revision"] != float64(2) {
		t.Fatalf("revision = %v", updated["revision"])
	}

	// Start creates a captain-owned session with the mission snapshot.
	status, body = env.asAlice(http.MethodPost, "/v1/team-bots/support-team/start", map[string]any{
		"text": "go", "requestId": "req-1",
	})
	wantStatus(t, status, body, http.StatusCreated, "start team bot")
	sessionID, _ := uuid.Parse(body["sessionId"].(string))
	ownership, err := env.st.GetSessionOwnership(context.Background(), env.tenant, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if ownership.BotID != env.aliceBot.ID || ownership.TeamBotID != "support-team" ||
		ownership.AccountID != env.alice.ID || ownership.CreatedByAccountID != env.alice.ID {
		t.Fatalf("team session ownership = %+v", ownership)
	}
	last := env.starter.calls[len(env.starter.calls)-1]
	if last.TeamBotID != "support-team" || last.Mission != "resolve every ticket" {
		t.Fatalf("starter request = %+v", last)
	}

	status, body = env.asAlice(http.MethodPost, "/v1/team-bots/support-team/archive", nil)
	wantStatus(t, status, body, http.StatusOK, "archive team bot")
	status, body = env.asAlice(http.MethodPost, "/v1/team-bots/support-team/start", map[string]any{"text": "go"})
	wantStatus(t, status, body, http.StatusConflict, "start archived team bot")
}

func TestBotKeysHTTPSurface(t *testing.T) {
	env := newIdentityEnv(t)

	status, body := env.asAlice(http.MethodPost, "/v1/bots/"+env.aliceBot.ID+"/keys", map[string]any{
		"label": "ci",
	})
	wantStatus(t, status, body, http.StatusCreated, "issue bot key")
	plaintext, _ := body["plaintext"].(string)
	if !strings.HasPrefix(plaintext, auth.BotKeyScheme) {
		t.Fatalf("plaintext = %q", plaintext)
	}
	key, _ := body["key"].(map[string]any)
	if key["accountId"] != env.alice.ID.String() || key["botId"] != env.aliceBot.ID {
		t.Fatalf("key = %v", key)
	}
	if key["keyHash"] != nil {
		t.Fatal("issue response exposed a hash")
	}
	service := auth.NewService(env.st, env.tenant)
	if principal, err := service.AuthenticateBotKey(context.Background(), plaintext); err != nil || principal.BotID != env.aliceBot.ID {
		t.Fatalf("authenticate minted key: %+v err=%v", principal, err)
	}

	status, body = env.asAlice(http.MethodGet, "/v1/bots/"+env.aliceBot.ID+"/keys", nil)
	wantStatus(t, status, body, http.StatusOK, "list bot keys")
	if len(listField(body, "keys")) != 1 {
		t.Fatalf("keys = %v", body)
	}

	// A key may not bind to another account.
	status, body = env.asAlice(http.MethodPost, "/v1/bots/"+env.aliceBot.ID+"/keys", map[string]any{
		"accountId": env.bob.ID.String(),
	})
	wantStatus(t, status, body, http.StatusForbidden, "mismatched owner account")
	if body["code"] != "key_account_mismatch" {
		t.Fatalf("code = %v", body)
	}
	status, body = env.do(http.MethodPost, "/v1/bots/"+env.aliceBot.ID+"/keys", env.bob, false, map[string]any{})
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "bob issues a key")

	// A key of another bot answers 404 through this bot's path.
	status, supportBody := env.asSuper(http.MethodPost, "/v1/bots/"+env.supportBot.ID+"/keys", map[string]any{})
	wantStatus(t, status, supportBody, http.StatusCreated, "super issues support key")
	supportKey, _ := supportBody["key"].(map[string]any)
	status, body = env.asAlice(http.MethodDelete, "/v1/bots/"+env.aliceBot.ID+"/keys/"+supportKey["keyId"].(string), nil)
	wantStatus(t, status, body, http.StatusNotFound, "revoke another bot's key")

	// Revocation is immediate at authentication.
	keyID, _ := uuid.Parse(key["keyId"].(string))
	status, body = env.asAlice(http.MethodDelete, "/v1/bots/"+env.aliceBot.ID+"/keys/"+keyID.String(), nil)
	wantStatus(t, status, body, http.StatusOK, "revoke key")
	if _, err := service.AuthenticateBotKey(context.Background(), plaintext); err == nil {
		t.Fatal("revoked key still authenticates")
	}
}

func TestWhoAmIRealBridge(t *testing.T) {
	env := newIdentityEnv(t)

	status, body := env.do(http.MethodGet, "/v1/auth/whoami", env.limited, false, nil)
	wantStatus(t, status, body, http.StatusOK, "limited whoami")
	permissions, _ := body["permissions"].(map[string]any)
	if permissions[access.KeySessionRead] != string(access.ScopeOwn) ||
		permissions[access.KeyPermissionManage] != string(access.ScopeTenant) {
		t.Fatalf("limited permissions = %v", permissions)
	}
	if _, ok := permissions[access.KeyBotRead]; ok {
		t.Fatalf("limited has an unexpected grant: %v", permissions)
	}
	if _, ok := body["role"]; ok {
		t.Fatal("whoami still returns the old tenant role")
	}

	status, body = env.asSuper(http.MethodGet, "/v1/auth/whoami", nil)
	wantStatus(t, status, body, http.StatusOK, "super whoami")
	account, _ := body["account"].(map[string]any)
	if account["isSuperAdmin"] != true {
		t.Fatalf("super account = %v", account)
	}
	superPermissions, _ := body["permissions"].(map[string]any)
	if len(superPermissions) != len(access.Keys()) {
		t.Fatalf("super permissions = %d keys, want %d", len(superPermissions), len(access.Keys()))
	}

	status, body = env.do(http.MethodGet, "/v1/auth/whoami", env.bob, false, nil)
	wantStatus(t, status, body, http.StatusOK, "bob whoami")
	if got := body["permissions"].(map[string]any); len(got) != 0 {
		t.Fatalf("bob permissions = %v, want empty", got)
	}
}

// TestSessionListUsesVisibleResources is the list-endpoint proof for the
// private-by-default session model: a tenant-scoped session.read key does not
// list another account's session, and a caller without the key is refused.
func TestSessionListUsesVisibleResources(t *testing.T) {
	env := newIdentityEnv(t)
	sessionID := uuid.New()
	if _, err := env.starter.start(context.Background(), BotSessionRequest{
		TenantID: env.tenant, SessionID: sessionID, BotID: env.aliceBot.ID,
		AccountID: env.alice.ID, ActorAccountID: env.alice.ID, Text: "hello",
	}); err != nil {
		t.Fatal(err)
	}
	listIDs := func(account store.AccountRecord, super bool) ([]string, int, map[string]any) {
		status, body := env.do(http.MethodGet, "/v1/sessions", account, super, nil)
		raw, _ := body["sessions"].([]any)
		ids := make([]string, 0, len(raw))
		for _, item := range raw {
			entry, _ := item.(map[string]any)
			if id, ok := entry["sessionId"].(string); ok {
				ids = append(ids, id)
			}
		}
		return ids, status, body
	}
	ids, status, body := listIDs(env.alice, false)
	wantStatus(t, status, body, http.StatusOK, "alice lists sessions")
	if !containsID(ids, sessionID.String()) {
		t.Fatalf("alice sessions = %v, want %s", ids, sessionID)
	}
	ids, status, body = listIDs(env.super, true)
	wantStatus(t, status, body, http.StatusOK, "super lists sessions")
	if !containsID(ids, sessionID.String()) {
		t.Fatalf("super sessions = %v", ids)
	}

	// Bob holds session.read at tenant scope but has no relation to the session.
	if _, err := env.st.CreatePermissionGrant(context.Background(), store.PermissionGrantRecord{
		TenantID: env.tenant, ID: uuid.New(), SubjectType: store.PermissionSubjectAccount,
		SubjectID: env.bob.ID, PermissionKey: access.KeySessionRead,
		Scope: string(access.ScopeTenant), GrantedBy: env.super.ID,
	}); err != nil {
		t.Fatal(err)
	}
	ids, status, body = listIDs(env.bob, false)
	wantStatus(t, status, body, http.StatusOK, "bob lists sessions")
	if containsID(ids, sessionID.String()) {
		t.Fatalf("tenant-scoped session.read leaked another account's session: %v", ids)
	}
	// A caller with no session.read at all is refused, not given an empty list.
	ids, status, body = listIDs(env.botAccount, false)
	wantReason(t, status, body, http.StatusForbidden, access.ReasonMissingGrant, "no session.read lists sessions")
	_ = ids
}
