package toolauth

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/toolsurface"
)

type authorizeCall struct {
	Principal access.Principal
	Key       string
	Resource  access.Resource
}

type invokeAudit struct {
	BotID    string
	Session  uuid.UUID
	Result   string
	Caller   uuid.UUID
	TenantID uuid.UUID
}

type deniedCall struct {
	Principal access.Principal
	Decision  access.Decision
	Resource  access.Resource
}

// fakeAccess is the policy test double: it answers a scripted principal and
// decision set while recording every decision the authorizer asks for.
type fakeAccess struct {
	principal    access.Principal
	principalErr error
	botIDs       map[uuid.UUID]string
	botErr       error
	decisions    map[string]access.Decision
	auditErr     error

	authorized []authorizeCall
	audits     []invokeAudit
	denied     []deniedCall
}

func (f *fakeAccess) SessionPrincipal(context.Context, uuid.UUID, uuid.UUID) (access.Principal, error) {
	if f.principalErr != nil {
		return access.Principal{}, f.principalErr
	}
	return f.principal, nil
}

func (f *fakeAccess) SessionBotID(_ context.Context, _ uuid.UUID, sessionID uuid.UUID) (string, error) {
	if f.botErr != nil {
		return "", f.botErr
	}
	if bot, ok := f.botIDs[sessionID]; ok {
		return bot, nil
	}
	return "", accesswire.ErrNoPrincipal
}

func (f *fakeAccess) Authorize(_ context.Context, principal access.Principal, key string, resource access.Resource) (access.Decision, error) {
	f.authorized = append(f.authorized, authorizeCall{Principal: principal, Key: key, Resource: resource})
	if decision, ok := f.decisions[key+"/"+resource.ID]; ok {
		return decision, nil
	}
	return access.Decision{Allow: true, Reason: access.ReasonAllow, Key: key, Scope: access.ScopeOwn}, nil
}

func (f *fakeAccess) AuditInvoke(_ context.Context, tenantID uuid.UUID, caller access.Principal, botID string, sessionID uuid.UUID, result string) error {
	if f.auditErr != nil {
		return f.auditErr
	}
	f.audits = append(f.audits, invokeAudit{TenantID: tenantID, Caller: caller.AccountID, BotID: botID, Session: sessionID, Result: result})
	return nil
}

func (f *fakeAccess) Denied(_ context.Context, principal access.Principal, decision access.Decision, resource access.Resource) {
	f.denied = append(f.denied, deniedCall{Principal: principal, Decision: decision, Resource: resource})
}

var (
	testTenant  = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	testSession = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	testAccount = uuid.MustParse("33333333-3333-3333-3333-333333333333")
	testScope   = kernel.Scope{TenantID: testTenant, SessionID: testSession}
)

func newTestAuthorizer(t *testing.T, fake *fakeAccess) *Authorizer {
	t.Helper()
	auth, err := New(fake, nil)
	if err != nil {
		t.Fatal(err)
	}
	return auth
}

func call(name, args string) agentcore.AgentToolCall {
	return agentcore.AgentToolCall{ID: "call-1", Name: name, Arguments: json.RawMessage(args)}
}

func wantAccessError(t *testing.T, err error, code string) *agent.ToolAccessError {
	t.Helper()
	if err == nil {
		t.Fatal("authorization allowed, want a refusal")
	}
	var refusal *agent.ToolAccessError
	if !errors.As(err, &refusal) {
		t.Fatalf("error = %T %v, want *agent.ToolAccessError", err, err)
	}
	if refusal.Code != code {
		t.Fatalf("code = %q, want %q (%v)", refusal.Code, code, err)
	}
	return refusal
}

func TestAuthorizeRequiresAPrincipal(t *testing.T) {
	fake := &fakeAccess{principalErr: accesswire.ErrNoPrincipal}
	auth := newTestAuthorizer(t, fake)
	refusal := wantAccessError(t, auth.AuthorizeToolCall(context.Background(), testScope, call("read", `{"path":"a"}`)), agent.ToolAccessNoPrincipal)
	if !strings.Contains(refusal.Reason, "principal") {
		t.Fatalf("reason = %q", refusal.Reason)
	}
}

func TestAuthorizeKeysUseThePermissionRegistry(t *testing.T) {
	cases := []struct {
		name     string
		call     agentcore.AgentToolCall
		wantKey  string
		wantType string
		wantID   string
	}{
		{"bash", call("bash", `{"action":"run","command":"pwd"}`), access.KeyShellExec, "session", testSession.String()},
		{"read", call("read", `{"path":"a.txt"}`), access.KeyFileRead, "session", testSession.String()},
		{"edit", call("edit", `{"path":"a.txt"}`), access.KeyFileWrite, "session", testSession.String()},
		{"mcp", call("mcp", `{"action":"call","server":"s","tool":"t"}`), access.KeyMCPUse, "session", testSession.String()},
		{"skills-read", call("skills", `{"action":"ls"}`), access.KeySkillRead, "session", testSession.String()},
		{"skills-manage", call("skills", `{"action":"manage","op":"delete"}`), access.KeySkillManage, "session", testSession.String()},
		{"tasks-write", call("tasks", `{"action":"create","title":"x"}`), access.KeyTaskWrite, "session", testSession.String()},
		{"tasks-read", call("tasks", `{"action":"show","taskId":"t"}`), access.KeyTaskRead, "session", testSession.String()},
		{"approve", call("approve", `{"action":"confirm","prompt":"ok?"}`), access.KeyApprovalWrite, "session", testSession.String()},
		{"notify", call("notify", `{"action":"push","text":"x"}`), access.KeyNotificationWrite, "session", testSession.String()},
		{"creation", call("creation", `{"action":"catalog"}`), access.KeyPluginInvoke, "session", testSession.String()},
		{"media", call("image_generate", `{"prompt":"x"}`), access.KeyMediaInvoke, "session", testSession.String()},
		{"decision", call("decision_ask", `{}`), access.KeyDecisionAsk, "session", testSession.String()},
		{"plugin-tool", call("vendor_tool", `{}`), access.KeyPluginInvoke, "session", testSession.String()},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeAccess{principal: access.Principal{TenantID: testTenant, AccountID: testAccount}}
			auth := newTestAuthorizer(t, fake)
			if err := auth.AuthorizeToolCall(context.Background(), testScope, test.call); err != nil {
				t.Fatalf("authorize: %v", err)
			}
			if len(fake.authorized) != 1 {
				t.Fatalf("authorize calls = %d, want 1", len(fake.authorized))
			}
			got := fake.authorized[0]
			if got.Key != test.wantKey {
				t.Fatalf("key = %q, want %q", got.Key, test.wantKey)
			}
			if got.Resource.Type != test.wantType || got.Resource.ID != test.wantID {
				t.Fatalf("resource = %+v, want %s/%s", got.Resource, test.wantType, test.wantID)
			}
			if got.Principal.AccountID != testAccount || got.Principal.TenantID != testTenant {
				t.Fatalf("principal = %+v", got.Principal)
			}
		})
	}
}

func TestNamedSessionIsTheResource(t *testing.T) {
	other := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	fake := &fakeAccess{principal: access.Principal{TenantID: testTenant, AccountID: testAccount}}
	auth := newTestAuthorizer(t, fake)
	if err := auth.AuthorizeToolCall(context.Background(), testScope,
		call("tasks", `{"action":"list","sessionId":"`+other.String()+`"}`)); err != nil {
		t.Fatal(err)
	}
	if len(fake.authorized) != 1 || fake.authorized[0].Resource.ID != other.String() || fake.authorized[0].Key != access.KeyTaskRead {
		t.Fatalf("authorize = %+v", fake.authorized)
	}
}

func TestNotifyListCrossSessionAlsoNeedsSessionRead(t *testing.T) {
	other := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	fake := &fakeAccess{principal: access.Principal{TenantID: testTenant, AccountID: testAccount}}
	auth := newTestAuthorizer(t, fake)
	if err := auth.AuthorizeToolCall(context.Background(), testScope,
		call("notify", `{"action":"list","sessionId":"`+other.String()+`"}`)); err != nil {
		t.Fatal(err)
	}
	if len(fake.authorized) != 2 {
		t.Fatalf("authorize calls = %d, want 2: %+v", len(fake.authorized), fake.authorized)
	}
	if fake.authorized[0].Key != access.KeyNotificationRead || fake.authorized[0].Resource.ID != testSession.String() {
		t.Fatalf("first decision = %+v", fake.authorized[0])
	}
	if fake.authorized[1].Key != access.KeySessionRead || fake.authorized[1].Resource.ID != other.String() {
		t.Fatalf("second decision = %+v", fake.authorized[1])
	}
}

func TestEngineDenialKeepsItsReason(t *testing.T) {
	fake := &fakeAccess{
		principal: access.Principal{TenantID: testTenant, AccountID: testAccount},
		decisions: map[string]access.Decision{
			access.KeyShellExec + "/" + testSession.String(): {
				Allow: false, Key: access.KeyShellExec, Reason: access.ReasonMissingKey, Detail: "harness does not include shell.exec",
			},
		},
	}
	auth := newTestAuthorizer(t, fake)
	refusal := wantAccessError(t, auth.AuthorizeToolCall(context.Background(), testScope, call("bash", `{"action":"run"}`)), agent.ToolAccessPermissionDenied)
	if refusal.Key != access.KeyShellExec || refusal.Reason != "missing_key" {
		t.Fatalf("refusal = %+v", refusal)
	}
	if len(fake.audits) != 0 {
		t.Fatalf("a denied call was audited as accepted: %+v", fake.audits)
	}
}

func TestAgentSpawnTargetsTheNamedOrOwnBot(t *testing.T) {
	t.Run("explicit target", func(t *testing.T) {
		fake := &fakeAccess{principal: access.Principal{TenantID: testTenant, AccountID: testAccount, BotID: "captain-1"}}
		auth := newTestAuthorizer(t, fake)
		if err := auth.AuthorizeToolCall(context.Background(), testScope,
			call("agent", `{"action":"spawn","task":"x","agentId":"member-1"}`)); err != nil {
			t.Fatal(err)
		}
		if len(fake.authorized) != 1 || fake.authorized[0].Key != access.KeyBotInvoke ||
			fake.authorized[0].Resource.Type != "bot" || fake.authorized[0].Resource.ID != "member-1" {
			t.Fatalf("authorize = %+v", fake.authorized)
		}
		if len(fake.audits) != 1 || fake.audits[0].BotID != "member-1" || fake.audits[0].Result != "authorized" {
			t.Fatalf("audits = %+v", fake.audits)
		}
	})
	t.Run("own bot", func(t *testing.T) {
		fake := &fakeAccess{principal: access.Principal{TenantID: testTenant, AccountID: testAccount, BotID: "captain-1"}}
		auth := newTestAuthorizer(t, fake)
		if err := auth.AuthorizeToolCall(context.Background(), testScope, call("agent", `{"action":"spawn","task":"x"}`)); err != nil {
			t.Fatal(err)
		}
		if len(fake.authorized) != 1 || fake.authorized[0].Resource.ID != "captain-1" {
			t.Fatalf("authorize = %+v", fake.authorized)
		}
		if len(fake.audits) != 1 || fake.audits[0].BotID != "captain-1" {
			t.Fatalf("audits = %+v", fake.audits)
		}
	})
	t.Run("no bot identity", func(t *testing.T) {
		fake := &fakeAccess{principal: access.Principal{TenantID: testTenant, AccountID: testAccount}}
		auth := newTestAuthorizer(t, fake)
		refusal := wantAccessError(t, auth.AuthorizeToolCall(context.Background(), testScope,
			call("agent", `{"action":"spawn","task":"x"}`)), agent.ToolAccessPermissionDenied)
		if refusal.Key != access.KeyBotInvoke || refusal.Reason != string(access.ReasonMissingRelation) {
			t.Fatalf("refusal = %+v", refusal)
		}
		if len(fake.authorized) != 0 {
			t.Fatalf("an unresolved target reached the engine: %+v", fake.authorized)
		}
		if len(fake.denied) != 1 || fake.denied[0].Decision.Reason != access.ReasonMissingRelation {
			t.Fatalf("audit = %+v", fake.denied)
		}
	})
}

func TestAgentChildSessionResolvesItsOwner(t *testing.T) {
	child := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	fake := &fakeAccess{
		principal: access.Principal{TenantID: testTenant, AccountID: testAccount, BotID: "captain-1"},
		botIDs:    map[uuid.UUID]string{child: "member-1"},
	}
	auth := newTestAuthorizer(t, fake)
	if err := auth.AuthorizeToolCall(context.Background(), testScope,
		call("agent", `{"action":"resume","sessionId":"`+child.String()+`"}`)); err != nil {
		t.Fatal(err)
	}
	if len(fake.authorized) != 1 || fake.authorized[0].Resource.ID != "member-1" || fake.authorized[0].Key != access.KeyBotInvoke {
		t.Fatalf("authorize = %+v", fake.authorized)
	}
}

func TestAgentUnknownChildDeniesWithoutExistenceLeak(t *testing.T) {
	child := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	fake := &fakeAccess{
		principal: access.Principal{TenantID: testTenant, AccountID: testAccount, BotID: "captain-1"},
		botIDs:    map[uuid.UUID]string{},
	}
	auth := newTestAuthorizer(t, fake)
	refusal := wantAccessError(t, auth.AuthorizeToolCall(context.Background(), testScope,
		call("agent", `{"action":"status","sessionId":"`+child.String()+`"}`)), agent.ToolAccessPermissionDenied)
	if refusal.Reason != string(access.ReasonMissingRelation) {
		t.Fatalf("reason = %q, want missing_relation", refusal.Reason)
	}
	if strings.Contains(strings.ToLower(refusal.Error()), "not found") {
		t.Fatalf("the refusal leaks existence: %v", refusal)
	}
	if len(fake.authorized) != 0 {
		t.Fatalf("an unknown child reached the engine: %+v", fake.authorized)
	}
	if len(fake.denied) != 1 {
		t.Fatalf("policy denial was not audited: %+v", fake.denied)
	}
}

func TestAgentSpawnEngineDenialStopsBeforeAudit(t *testing.T) {
	fake := &fakeAccess{
		principal: access.Principal{TenantID: testTenant, AccountID: testAccount, BotID: "captain-1"},
		decisions: map[string]access.Decision{
			access.KeyBotInvoke + "/member-1": {
				Allow: false, Key: access.KeyBotInvoke, Reason: access.ReasonMissingRelation,
			},
		},
	}
	auth := newTestAuthorizer(t, fake)
	refusal := wantAccessError(t, auth.AuthorizeToolCall(context.Background(), testScope,
		call("agent", `{"action":"spawn","task":"x","agentId":"member-1"}`)), agent.ToolAccessPermissionDenied)
	if refusal.Key != access.KeyBotInvoke || refusal.Reason != string(access.ReasonMissingRelation) {
		t.Fatalf("refusal = %+v", refusal)
	}
	if len(fake.audits) != 0 {
		t.Fatalf("denied invocation was audited as accepted: %+v", fake.audits)
	}
}

func TestInvokeAuditFailureFailsClosed(t *testing.T) {
	fake := &fakeAccess{
		principal: access.Principal{TenantID: testTenant, AccountID: testAccount, BotID: "captain-1"},
		auditErr:  errors.New("audit stream down"),
	}
	auth := newTestAuthorizer(t, fake)
	refusal := wantAccessError(t, auth.AuthorizeToolCall(context.Background(), testScope,
		call("agent", `{"action":"spawn","task":"x","agentId":"member-1"}`)), agent.ToolAccessUnavailable)
	if refusal.Reason != "invoke_audit_failed" {
		t.Fatalf("reason = %q", refusal.Reason)
	}
}

func TestUnknownAgentActionStillChecksItsTarget(t *testing.T) {
	fake := &fakeAccess{principal: access.Principal{TenantID: testTenant, AccountID: testAccount, BotID: "captain-1"}}
	auth := newTestAuthorizer(t, fake)
	if err := auth.AuthorizeToolCall(context.Background(), testScope,
		call("agent", `{"action":"bogus","agentId":"member-1"}`)); err != nil {
		t.Fatal(err)
	}
	if len(fake.authorized) != 1 || fake.authorized[0].Key != access.KeyBotInvoke || fake.authorized[0].Resource.ID != "member-1" {
		t.Fatalf("authorize = %+v", fake.authorized)
	}
}

// TestEveryDeclaredToolAuthorizes is the surface-level completeness check: a
// call for every declared tool/action reaches the engine with a key from the
// access registry. It catches a tool that was added to the surface but left
// outside the runtime authorization path.
func TestEveryDeclaredToolAuthorizes(t *testing.T) {
	for _, entry := range toolsurface.ToolPermissionManifest() {
		t.Run(entry.Tool+"."+entry.Action, func(t *testing.T) {
			fake := &fakeAccess{principal: access.Principal{TenantID: testTenant, AccountID: testAccount, BotID: "captain-1"}}
			auth := newTestAuthorizer(t, fake)
			args := map[string]any{}
			if entry.Action != "" {
				args["action"] = entry.Action
			}
			// The agent tool resolves a target; give its spawn/fork actions the
			// run's own Bot so the test exercises the target path too.
			if entry.Tool == toolsurface.Agent && (entry.Action == toolsurface.ActionSpawn || entry.Action == toolsurface.ActionFork) {
				args["task"] = "x"
			}
			raw, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			if err := auth.AuthorizeToolCall(context.Background(), testScope,
				agentcore.AgentToolCall{ID: "c", Name: entry.Tool, Arguments: raw}); err != nil {
				t.Fatalf("authorize %s.%s: %v", entry.Tool, entry.Action, err)
			}
			if len(fake.authorized) == 0 {
				t.Fatalf("%s.%s reached no authorization decision", entry.Tool, entry.Action)
			}
			if fake.authorized[0].Key != entry.Key {
				t.Fatalf("%s.%s key = %q, want %q", entry.Tool, entry.Action, fake.authorized[0].Key, entry.Key)
			}
		})
	}
}
