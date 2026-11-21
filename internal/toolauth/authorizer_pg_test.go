package toolauth_test

// Real-PostgreSQL negative and positive controls for the runtime tool surface
// (A4.2b1). Every test creates an isolated database and drops it with FORCE;
// the suite skips without KEEL_TEST_DSN, like the rest of the repository's
// database tests. Only KEEL_TEST_DSN is read: no project .env is sourced.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/testkit"
	"github.com/jixoai/keel/internal/toolauth"
	"github.com/jixoai/keel/internal/toolsurface"
)

func testLogger() *slog.Logger {
	if os.Getenv("KEEL_E2E_LOGS") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// openTestStore creates one isolated database per test.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse test dsn: %v", err)
	}
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	name := "keel_toolauth_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close(ctx)
		t.Fatalf("create test database: %v", err)
	}
	st, err := store.Open(ctx, store.Config{DSN: dsnWithDatabase(dsn, name)})
	if err != nil {
		admin.Close(ctx)
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() {
		st.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer dropCancel()
		_, _ = admin.Exec(dropCtx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		admin.Close(dropCtx)
	})
	return st
}

func dsnWithDatabase(dsn, name string) string {
	if parsed, err := url.Parse(dsn); err == nil && parsed.Scheme != "" {
		parsed.Path = "/" + name
		return parsed.String()
	}
	return dsn + " dbname=" + name
}

// env is the A4.2b1 fixture: one tenant, a captain Bot and a member Bot with
// their own accounts/profiles/harnesses, and one owned session per Bot.
type env struct {
	t   *testing.T
	st  *store.Store
	ctx context.Context

	tenant uuid.UUID
	owner  store.AccountRecord
	member store.AccountRecord
	human  store.AccountRecord
	super  store.AccountRecord

	captain   store.BotRecord
	memberBot store.BotRecord

	captainSession uuid.UUID
	memberSession  uuid.UUID

	bridge *accesswire.Bridge
	auth   *toolauth.Authorizer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st := openTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "toolauth-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, st: st, ctx: ctx, tenant: tenant}
	makeAccount := func(label, kind string, super bool) store.AccountRecord {
		account, err := st.CreateAccount(ctx, store.AccountRecord{
			TenantID: tenant, ID: uuid.New(),
			Username: label + "-" + tenant.String()[:8], DisplayName: label,
			Kind: kind, IsSuperAdmin: super,
		})
		if err != nil {
			t.Fatal(err)
		}
		return account
	}
	e.owner = makeAccount("captain-owner", "bot", false)
	e.member = makeAccount("member-owner", "bot", false)
	e.human = makeAccount("human", "human", false)
	e.super = makeAccount("super", "human", true)

	role, err := st.CreateRole(ctx, store.RoleRecord{TenantID: tenant, ID: uuid.New(), Name: "Operator"})
	if err != nil {
		t.Fatal(err)
	}
	makeProfile := func(account store.AccountRecord) store.ProfileRecord {
		profile, err := st.CreateProfile(ctx, store.ProfileRecord{
			TenantID: tenant, ID: uuid.New(), AccountID: account.ID, RoleID: role.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		return profile
	}
	ownerProfile := makeProfile(e.owner)
	memberProfile := makeProfile(e.member)

	permissions := allToolKeys()
	e.captain = createBot(t, st, tenant, "captain-1", ownerProfile.ID, e.owner.ID, permissions)
	e.memberBot = createBot(t, st, tenant, "member-1", memberProfile.ID, e.member.ID, permissions)

	e.captainSession = e.newOwnedSession(e.captain.ID, e.owner.ID, e.owner.ID)
	e.memberSession = e.newOwnedSession(e.memberBot.ID, e.member.ID, e.member.ID)

	bridge, err := accesswire.NewBridge(st, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	e.bridge = bridge
	auth, err := toolauth.New(bridge, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	e.auth = auth
	return e
}

func createBot(t *testing.T, st *store.Store, tenant uuid.UUID, botID string, profileID, ownerID uuid.UUID, permissions []string) store.BotRecord {
	t.Helper()
	harness, err := json.Marshal(map[string]any{
		"tools": []string{},
		"context": map[string]any{
			"permissions": permissions,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	bot, err := st.CreateBotWithOwner(context.Background(), store.BotRecord{
		TenantID: tenant, ID: botID, Name: botID, ProfileID: profileID, ModelKind: "text",
		DefaultModel: json.RawMessage(`{"providerId":"fake","modelId":"fake"}`),
		Harness:      harness,
	}, ownerID, ownerID)
	if err != nil {
		t.Fatalf("create bot %s: %v", botID, err)
	}
	return bot
}

func (e *env) newOwnedSession(botID string, accountID, createdBy uuid.UUID) uuid.UUID {
	e.t.Helper()
	session := uuid.New()
	if err := e.st.CreateOwnedSession(e.ctx, store.SessionOwnership{
		TenantID: e.tenant, SessionID: session, BotID: botID,
		AccountID: accountID, CreatedByAccountID: createdBy,
	}); err != nil {
		e.t.Fatalf("create owned session: %v", err)
	}
	return session
}

func (e *env) scope(session uuid.UUID) kernel.Scope {
	return kernel.Scope{TenantID: e.tenant, SessionID: session}
}

func allToolKeys() []string {
	set := make(map[string]bool)
	for _, entry := range toolsurface.ToolPermissionManifest() {
		set[entry.Key] = true
	}
	// The notify cross-session guard and general session reads/writes.
	set[access.KeySessionRead] = true
	set[access.KeySessionWrite] = true
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func toolScope(key string) access.Scope {
	spec, ok := access.Lookup(key)
	if !ok {
		panic("unknown key " + key)
	}
	if spec.AllowsScope(access.ScopeOwn) {
		return access.ScopeOwn
	}
	return access.ScopeTenant
}

func (e *env) grant(accountID uuid.UUID, key string, scope access.Scope) store.PermissionGrantRecord {
	e.t.Helper()
	record, err := e.st.CreatePermissionGrant(e.ctx, store.PermissionGrantRecord{
		TenantID: e.tenant, ID: uuid.New(), SubjectType: store.PermissionSubjectAccount,
		SubjectID: accountID, PermissionKey: key, Scope: string(scope), GrantedBy: e.owner.ID,
	})
	if err != nil {
		e.t.Fatalf("grant %s to %s: %v", key, accountID, err)
	}
	return record
}

func (e *env) grantAll(accountID uuid.UUID) {
	for _, key := range allToolKeys() {
		e.grant(accountID, key, toolScope(key))
	}
}

func (e *env) addViewer(accountID uuid.UUID, botID string) {
	e.t.Helper()
	if _, err := e.st.PutResourceRelation(e.ctx, store.ResourceRelation{
		TenantID: e.tenant, ResourceType: "bot", ResourceID: botID,
		Relation: store.RelationViewer, SubjectType: store.RelationSubjectAccount,
		SubjectID: accountID.String(), CreatedBy: e.owner.ID,
	}); err != nil {
		e.t.Fatalf("add viewer: %v", err)
	}
}

func (e *env) removeViewer(accountID uuid.UUID, botID string) {
	e.t.Helper()
	if err := e.st.DeleteResourceRelation(e.ctx, e.tenant, "bot", botID,
		store.RelationViewer, store.RelationSubjectAccount, accountID.String()); err != nil {
		e.t.Fatalf("remove viewer: %v", err)
	}
}

func (e *env) setHarness(botID string, permissions []string) {
	e.t.Helper()
	bot, err := e.st.GetBot(e.ctx, e.tenant, botID)
	if err != nil {
		e.t.Fatal(err)
	}
	harness, err := json.Marshal(map[string]any{
		"tools": []string{},
		"context": map[string]any{
			"permissions": permissions,
		},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	bot.Harness = harness
	if _, err := e.st.UpdateBotWithOwner(e.ctx, bot, false, uuid.Nil, uuid.Nil, e.owner.ID); err != nil {
		e.t.Fatalf("update harness: %v", err)
	}
}

func callFor(entry toolsurface.ToolPermission) agentcore.AgentToolCall {
	args := map[string]any{}
	if entry.Action != "" {
		args["action"] = entry.Action
	}
	switch entry.Tool {
	case toolsurface.Agent:
		if entry.Action == toolsurface.ActionSpawn || entry.Action == toolsurface.ActionFork {
			args["task"] = "delegate"
		}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return agentcore.AgentToolCall{ID: "call-1", Name: entry.Tool, Arguments: raw}
}

func rawCall(name, args string) agentcore.AgentToolCall {
	return agentcore.AgentToolCall{ID: "call-1", Name: name, Arguments: json.RawMessage(args)}
}

func wantRefusal(t *testing.T, err error) *agent.ToolAccessError {
	t.Helper()
	if err == nil {
		t.Fatal("authorization allowed, want a refusal")
	}
	var refusal *agent.ToolAccessError
	if !errors.As(err, &refusal) {
		t.Fatalf("error = %T %v, want *agent.ToolAccessError", err, err)
	}
	if refusal.Code != agent.ToolAccessPermissionDenied {
		t.Fatalf("code = %q, want %q (%v)", refusal.Code, agent.ToolAccessPermissionDenied, err)
	}
	return refusal
}

func TestEveryToolWithoutGrantIsDenied(t *testing.T) {
	e := newEnv(t)
	for _, entry := range toolsurface.ToolPermissionManifest() {
		t.Run(entry.Tool+"."+entry.Action, func(t *testing.T) {
			refusal := wantRefusal(t, e.auth.AuthorizeToolCall(e.ctx, e.scope(e.captainSession), callFor(entry)))
			if refusal.Key != entry.Key {
				t.Fatalf("key = %q, want %q", refusal.Key, entry.Key)
			}
			if refusal.Reason != string(access.ReasonMissingGrant) {
				t.Fatalf("reason = %q, want missing_grant", refusal.Reason)
			}
		})
	}
}

func TestEveryToolWithGrantsIsAuthorized(t *testing.T) {
	e := newEnv(t)
	e.grantAll(e.owner.ID)
	// The agent tool may target the member Bot; a viewer relation is what the
	// parent account needs for that target.
	e.addViewer(e.owner.ID, e.memberBot.ID)
	for _, entry := range toolsurface.ToolPermissionManifest() {
		t.Run(entry.Tool+"."+entry.Action, func(t *testing.T) {
			if err := e.auth.AuthorizeToolCall(e.ctx, e.scope(e.captainSession), callFor(entry)); err != nil {
				t.Fatalf("authorize %s.%s: %v", entry.Tool, entry.Action, err)
			}
		})
	}
}

func TestHarnessNarrowingDeniesTheNextCall(t *testing.T) {
	e := newEnv(t)
	e.grantAll(e.owner.ID)
	if err := e.auth.AuthorizeToolCall(e.ctx, e.scope(e.captainSession), rawCall("bash", `{"action":"run","command":"pwd"}`)); err != nil {
		t.Fatalf("bash before narrowing: %v", err)
	}
	withoutShell := make([]string, 0, len(allToolKeys()))
	for _, key := range allToolKeys() {
		if key != access.KeyShellExec {
			withoutShell = append(withoutShell, key)
		}
	}
	e.setHarness(e.captain.ID, withoutShell)

	refusal := wantRefusal(t, e.auth.AuthorizeToolCall(e.ctx, e.scope(e.captainSession), rawCall("bash", `{"action":"run","command":"pwd"}`)))
	if refusal.Key != access.KeyShellExec || refusal.Reason != string(access.ReasonMissingKey) {
		t.Fatalf("refusal = %+v, want shell.exec/missing_key", refusal)
	}
	// The rest of the surface is untouched.
	if err := e.auth.AuthorizeToolCall(e.ctx, e.scope(e.captainSession), rawCall("read", `{"path":"a.txt"}`)); err != nil {
		t.Fatalf("read after narrowing: %v", err)
	}
}

func TestGrantRevocationDeniesTheNextCall(t *testing.T) {
	e := newEnv(t)
	grant := e.grant(e.owner.ID, access.KeyShellExec, access.ScopeOwn)
	if err := e.auth.AuthorizeToolCall(e.ctx, e.scope(e.captainSession), rawCall("bash", `{"action":"run"}`)); err != nil {
		t.Fatalf("bash with grant: %v", err)
	}
	if err := e.st.DeletePermissionGrant(e.ctx, e.tenant, grant.ID); err != nil {
		t.Fatal(err)
	}
	refusal := wantRefusal(t, e.auth.AuthorizeToolCall(e.ctx, e.scope(e.captainSession), rawCall("bash", `{"action":"run"}`)))
	if refusal.Key != access.KeyShellExec || refusal.Reason != string(access.ReasonMissingGrant) {
		t.Fatalf("refusal = %+v", refusal)
	}
}

func TestBotInvokeChecksTheTargetRelationAndAudits(t *testing.T) {
	e := newEnv(t)
	e.grant(e.owner.ID, access.KeyBotInvoke, access.ScopeOwn)

	spawnMember := rawCall("agent", `{"action":"spawn","task":"delegate","agentId":"member-1"}`)
	refusal := wantRefusal(t, e.auth.AuthorizeToolCall(e.ctx, e.scope(e.captainSession), spawnMember))
	if refusal.Reason != string(access.ReasonMissingRelation) {
		t.Fatalf("denied spawn reason = %q, want missing_relation", refusal.Reason)
	}

	e.addViewer(e.owner.ID, e.memberBot.ID)
	if err := e.auth.AuthorizeToolCall(e.ctx, e.scope(e.captainSession), spawnMember); err != nil {
		t.Fatalf("spawn with viewer relation: %v", err)
	}
	events, err := e.st.ListAuthEvents(e.ctx, e.tenant, store.AuthEventFilter{Kind: store.AuthEventBotInvoked}, 20)
	if err != nil {
		t.Fatal(err)
	}
	foundAccepted := false
	for _, event := range events {
		if event.Detail["bot_id"] == "member-1" && event.Detail["result"] == "authorized" {
			foundAccepted = true
		}
	}
	if !foundAccepted {
		t.Fatalf("accepted spawn left no bot.invoked audit: %+v", events)
	}

	e.removeViewer(e.owner.ID, e.memberBot.ID)
	refusalMember := wantRefusal(t, e.auth.AuthorizeToolCall(e.ctx, e.scope(e.captainSession), spawnMember))
	refusalGhost := wantRefusal(t, e.auth.AuthorizeToolCall(e.ctx, e.scope(e.captainSession),
		rawCall("agent", `{"action":"spawn","task":"delegate","agentId":"ghost-1"}`)))
	if refusalMember.Reason != refusalGhost.Reason || refusalMember.Detail != refusalGhost.Detail {
		t.Fatalf("existence leak: member=(%q,%q) ghost=(%q,%q)",
			refusalMember.Reason, refusalMember.Detail, refusalGhost.Reason, refusalGhost.Detail)
	}

	events, err = e.st.ListAuthEvents(e.ctx, e.tenant, store.AuthEventFilter{Kind: store.AuthEventPermissionDenied}, 50)
	if err != nil {
		t.Fatal(err)
	}
	deniedTargets := make(map[string]bool)
	for _, event := range events {
		if event.Detail["key"] != access.KeyBotInvoke || event.Detail["reason"] != string(access.ReasonMissingRelation) {
			continue
		}
		if id, _ := event.Detail["resource_id"].(string); id != "" {
			deniedTargets[id] = true
		}
	}
	if !deniedTargets["member-1"] || !deniedTargets["ghost-1"] {
		t.Fatalf("denials were not audited with their reasons: %+v", deniedTargets)
	}
}

func TestSessionPrincipalComesFromOwnership(t *testing.T) {
	e := newEnv(t)
	principal, err := e.bridge.SessionPrincipal(e.ctx, e.tenant, e.captainSession)
	if err != nil {
		t.Fatal(err)
	}
	if principal.BotID != e.captain.ID || principal.AccountID != e.owner.ID || principal.TenantID != e.tenant {
		t.Fatalf("principal = %+v", principal)
	}
	if !contains(principal.HarnessPermissions, access.KeyShellExec) {
		t.Fatalf("harness permissions = %v, want shell.exec", principal.HarnessPermissions)
	}
	if principal.IsSuperAdmin {
		t.Fatal("a Bot principal is a super admin")
	}

	// A legacy account-owned session resolves to the account, with the
	// account's super-admin flag (never the Bot flag).
	humanSession := uuid.New()
	if err := e.st.CreateAccountOwnedSession(e.ctx, e.tenant, humanSession, e.human.ID); err != nil {
		t.Fatal(err)
	}
	human, err := e.bridge.SessionPrincipal(e.ctx, e.tenant, humanSession)
	if err != nil {
		t.Fatal(err)
	}
	if human.BotID != "" || human.IsSuperAdmin {
		t.Fatalf("human principal = %+v", human)
	}
	superSession := uuid.New()
	if err := e.st.CreateAccountOwnedSession(e.ctx, e.tenant, superSession, e.super.ID); err != nil {
		t.Fatal(err)
	}
	super, err := e.bridge.SessionPrincipal(e.ctx, e.tenant, superSession)
	if err != nil {
		t.Fatal(err)
	}
	if !super.IsSuperAdmin || super.BotID != "" {
		t.Fatalf("super principal = %+v", super)
	}

	// Disabled accounts and unowned sessions are not principals.
	if err := e.st.SetAccountDisabled(e.ctx, e.tenant, e.human.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.bridge.SessionPrincipal(e.ctx, e.tenant, humanSession); !errors.Is(err, accesswire.ErrNoPrincipal) {
		t.Fatalf("disabled account principal error = %v, want ErrNoPrincipal", err)
	}
	unowned := uuid.New()
	if _, err := e.st.Pool().Exec(e.ctx,
		`INSERT INTO sessions (tenant_id, session_id) VALUES ($1, $2)`, e.tenant, unowned); err != nil {
		t.Fatal(err)
	}
	if _, err := e.bridge.SessionPrincipal(e.ctx, e.tenant, unowned); !errors.Is(err, accesswire.ErrNoPrincipal) {
		t.Fatalf("unowned session principal error = %v, want ErrNoPrincipal", err)
	}
	var refusal *agent.ToolAccessError
	err = e.auth.AuthorizeToolCall(e.ctx, e.scope(unowned), rawCall("read", `{"path":"a"}`))
	if !errors.As(err, &refusal) || refusal.Code != agent.ToolAccessNoPrincipal {
		t.Fatalf("unowned session tool call = %v, want no_principal", err)
	}

	// An archived Bot refuses its sessions' principal.
	if _, err := e.st.Pool().Exec(e.ctx,
		`UPDATE bots SET archived = true WHERE tenant_id = $1 AND bot_id = $2`, e.tenant, e.captain.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.bridge.SessionPrincipal(e.ctx, e.tenant, e.captainSession); !errors.Is(err, accesswire.ErrNoPrincipal) {
		t.Fatalf("archived bot principal error = %v, want ErrNoPrincipal", err)
	}
}

func TestEachSessionUsesItsOwnPrincipal(t *testing.T) {
	e := newEnv(t)
	e.grantAll(e.owner.ID)

	// The captain session may run the shell.
	if err := e.auth.AuthorizeToolCall(e.ctx, e.scope(e.captainSession), rawCall("bash", `{"action":"run"}`)); err != nil {
		t.Fatalf("captain bash: %v", err)
	}
	// The member session has its own account and no grants: authority is not
	// inherited from the session that spawned it.
	refusal := wantRefusal(t, e.auth.AuthorizeToolCall(e.ctx, e.scope(e.memberSession), rawCall("bash", `{"action":"run"}`)))
	if refusal.Key != access.KeyShellExec || refusal.Reason != string(access.ReasonMissingGrant) {
		t.Fatalf("member refusal = %+v", refusal)
	}
	// Granting the member's own account is what enables it, with the member
	// session's owner relation.
	e.grant(e.member.ID, access.KeyShellExec, access.ScopeOwn)
	if err := e.auth.AuthorizeToolCall(e.ctx, e.scope(e.memberSession), rawCall("bash", `{"action":"run"}`)); err != nil {
		t.Fatalf("member bash after its own grant: %v", err)
	}
}

// TestRunnerEnforcesToolAccessWithRealPG is the runner/tool integration: the
// real kernel + runner execute a fake-provider echo call through the access
// authorizer. Without plugin.invoke the tool never runs and the model receives
// the structured refusal; with the grant the tool executes.
func TestRunnerEnforcesToolAccessWithRealPG(t *testing.T) {
	e := newEnv(t)
	kpg, err := kernel.NewPG(e.ctx, e.st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	kern := kernel.New(kpg, kernel.Options{})
	runner := agent.NewRunner(kern, kern, agent.FakeStream(), "fake", "fake", "", testLogger())
	runner.ToolAccess = e.auth
	if err := runner.Tools.Register(testkit.EchoTool{}); err != nil {
		t.Fatal(err)
	}

	runEcho := func() store.Event {
		t.Helper()
		session := e.newOwnedSession(e.captain.ID, e.owner.ID, e.owner.ID)
		if err := e.st.EnqueueRun(e.ctx, e.tenant, session); err != nil {
			t.Fatal(err)
		}
		scope := e.scope(session)
		if _, err := kern.Commit(e.ctx, kernel.CommitRequest{
			Scope: scope, Kind: kernel.KindUserMessage, RequestID: "r1",
			Payload: json.RawMessage(`{"text":"please echo this","requestId":"r1"}`),
		}); err != nil {
			t.Fatal(err)
		}
		lease, err := kern.ClaimScope(e.ctx, scope, "toolauth-test", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		cursor, runErr := runner.Run(e.ctx, lease)
		if err := kern.Finish(e.ctx, kernel.FinishRequest{Lease: lease, Cursor: cursor, Err: runErr, MaxAttempts: 3}); err != nil {
			t.Fatal(err)
		}
		events, err := e.st.LoadEvents(e.ctx, e.tenant, session, 0, 500)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Kind == store.EventToolResult {
				return event
			}
		}
		t.Fatal("the run produced no tool result event")
		return store.Event{}
	}

	blocked := runEcho()
	var denied struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(blocked.Payload, &denied); err != nil {
		t.Fatal(err)
	}
	if !denied.IsError || !contentContains(denied.Content, access.KeyPluginInvoke) {
		t.Fatalf("unprivileged tool result = %s", blocked.Payload)
	}

	e.grant(e.owner.ID, access.KeyPluginInvoke, access.ScopeTenant)
	allowed := runEcho()
	var ran struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(allowed.Payload, &ran); err != nil {
		t.Fatal(err)
	}
	if ran.IsError || !contentContains(ran.Content, "please echo this") {
		t.Fatalf("privileged tool result = %s", allowed.Payload)
	}
}

func contentContains(blocks []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}, want string) bool {
	for _, block := range blocks {
		if strings.Contains(block.Text, want) {
			return true
		}
	}
	return false
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestToolPermissionCatalogKeysResolve(t *testing.T) {
	// This is a cheap non-PG guard in the PG file's package: the keys the
	// runtime uses are all registry keys, so a test setup can grant them.
	for _, entry := range toolsurface.ToolPermissionManifest() {
		if err := access.ValidateKey(entry.Key); err != nil {
			t.Fatalf("tool %s.%s: %v", entry.Tool, entry.Action, err)
		}
	}
}
