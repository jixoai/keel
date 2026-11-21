package agentext_test

// Real-PostgreSQL + HTTP proof of trigger Bot ownership (change
// keel-bot-identity-access, A4.2b3a follow-up). Every trigger must fire as one
// Bot: the fired session carries bot/account/created_by and the owner relation,
// the committed input names the same bot, and the runtime tool surface then
// authorizes as that Bot's Account. The suite is skipped without KEEL_TEST_DSN
// and creates/drops its own database.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/agentapi"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/httpapi"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/servicehost"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/toolauth"
)

type triggerOwnershipEnv struct {
	t        *testing.T
	st       *store.Store
	kern     *kernel.Kernel
	bridge   *accesswire.Bridge
	tenant   uuid.UUID
	caller   store.AccountRecord
	bot      store.BotRecord
	botAcct  store.AccountRecord
	triggers *agentext.TriggerEngine
	server   *httptest.Server
}

func newTriggerOwnershipEnv(t *testing.T) *triggerOwnershipEnv {
	t.Helper()
	st := openTestStore(t)
	ctx := context.Background()
	klog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	kern := kernel.New(klog, kernel.Options{})
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "trigger-owner"); err != nil {
		t.Fatal(err)
	}
	caller, err := st.CreateAccount(ctx, store.AccountRecord{
		TenantID: tenant, ID: uuid.New(), Username: "operator-" + tenant.String()[:8],
		DisplayName: "operator", Kind: "human",
	})
	if err != nil {
		t.Fatal(err)
	}
	bot, botAcct := createE2EBot(t, st, ctx, tenant, "trigger-bot")
	bridge, err := accesswire.NewBridge(st, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	grant := func(accountID uuid.UUID, key string, scope access.Scope) {
		t.Helper()
		if _, err := st.CreatePermissionGrant(ctx, store.PermissionGrantRecord{
			TenantID: tenant, ID: uuid.New(), SubjectType: store.PermissionSubjectAccount,
			SubjectID: accountID, PermissionKey: key, Scope: string(scope), GrantedBy: caller.ID,
		}); err != nil {
			t.Fatalf("grant %s: %v", key, err)
		}
	}
	grant(caller.ID, access.KeyTriggerManage, access.ScopeTenant)
	grant(caller.ID, access.KeyTriggerRead, access.ScopeTenant)

	hooks := agentext.NewHookBus(agentext.HookBusOptions{Logger: testLogger()})
	starter := agentext.NewTurnStarter(storeSessions{Store: st}, kern, hooks, testLogger())
	owner := storeTriggerOwner{store: st}
	triggers := agentext.NewTriggerEngine(agentext.TriggerEngineOptions{
		Starter: starter, Hooks: hooks, Logger: testLogger(),
		Bots: owner, Ownership: owner,
	})
	product := agentapi.New(agentapi.Config{
		Sessions: agentapi.NewPGSessionLister(st),
		Access:   bridge,
		Triggers: triggers,
		Logger:   testLogger(),
	})
	api := httpapi.New(httpapi.Config{
		Store: st, Edge: kern, Events: kern, Product: product, Access: bridge, Logger: testLogger(),
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Tenant-ID", tenant.String())
		principal := auth.Principal{
			Kind: auth.PrincipalHuman, TenantID: tenant, AccountID: caller.ID,
		}
		api.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
	}))
	env := &triggerOwnershipEnv{
		t: t, st: st, kern: kern, bridge: bridge, tenant: tenant, caller: caller,
		bot: bot, botAcct: botAcct, triggers: triggers, server: server,
	}
	t.Cleanup(func() {
		server.Close()
		triggers.Close()
		hooks.Close()
	})
	return env
}

func (e *triggerOwnershipEnv) fire(body map[string]any) (int, map[string]any) {
	e.t.Helper()
	status, out := e.do(http.MethodPost, "/v1/triggers/"+triggerID+"/fire", body)
	return status, out
}

const triggerID = "owner-trigger"

func (e *triggerOwnershipEnv) do(method, path string, body any) (int, map[string]any) {
	e.t.Helper()
	var out map[string]any
	code := doJSON(e.t, e.server.Client(), method, e.server.URL+path, e.tenant, body, &out)
	return code, out
}

func (e *triggerOwnershipEnv) register(t *testing.T, spec agentext.TriggerSpec) {
	t.Helper()
	if _, err := e.triggers.Register(spec); err != nil {
		t.Fatalf("register trigger: %v", err)
	}
}

func (e *triggerOwnershipEnv) ownership(t *testing.T, sessionID uuid.UUID) store.SessionOwnership {
	t.Helper()
	ownership, err := e.st.GetSessionOwnership(context.Background(), e.tenant, sessionID)
	if err != nil {
		t.Fatalf("get session ownership: %v", err)
	}
	return ownership
}

func deterministicTriggerSession(tenant uuid.UUID, triggerID, key string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("keel.trigger.v1|"+
		tenant.String()+"|"+triggerID+"|"+key))
}

// TestTriggerRegistrationRefusesUnusableBot proves registration is the first
// fail-closed gate: no bot id, an archived bot and a foreign/unknown handle are
// all refused before the trigger can be observed or fired.
func TestTriggerRegistrationRefusesUnusableBot(t *testing.T) {
	env := newTriggerOwnershipEnv(t)
	ctx := context.Background()

	if _, err := env.triggers.Register(agentext.TriggerSpec{
		ID: "no-bot", Kind: agentext.TriggerWebhook, TenantID: env.tenant.String(), Prompt: "p", Enabled: true,
	}); !errors.Is(err, agentext.ErrInvalidTrigger) {
		t.Fatalf("register without botId = %v, want ErrInvalidTrigger", err)
	}

	archived, _ := createE2EBot(t, env.st, ctx, env.tenant, "archived-bot")
	if err := env.st.ArchiveBot(ctx, env.tenant, archived.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.triggers.Register(agentext.TriggerSpec{
		ID: "archived-bot", Kind: agentext.TriggerWebhook, TenantID: env.tenant.String(),
		BotID: archived.ID, Prompt: "p", Enabled: true,
	}); !errors.Is(err, agentext.ErrInvalidTrigger) || !errors.Is(err, agentext.ErrTriggerBotArchived) {
		t.Fatalf("register archived bot = %v, want ErrInvalidTrigger wrapping ErrTriggerBotArchived", err)
	}

	if _, err := env.triggers.Register(agentext.TriggerSpec{
		ID: "missing-bot", Kind: agentext.TriggerWebhook, TenantID: env.tenant.String(),
		BotID: "does-not-exist", Prompt: "p", Enabled: true,
	}); !errors.Is(err, agentext.ErrInvalidTrigger) || !errors.Is(err, agentext.ErrTriggerBotMissing) {
		t.Fatalf("register missing bot = %v, want ErrInvalidTrigger wrapping ErrTriggerBotMissing", err)
	}
}

// TestTriggerFireCreatesBotOwnedSession is the functional core: HTTP fire →
// Bot-owned session + owner relation + selection fact → tool authorization as
// the Bot's Account. It also pins manual vs webhook created_by, dedupe,
// restart, and the archived-Bot refusal (no session, no turn).
func TestTriggerFireCreatesBotOwnedSession(t *testing.T) {
	env := newTriggerOwnershipEnv(t)
	env.register(t, agentext.TriggerSpec{
		ID: triggerID, Kind: agentext.TriggerWebhook, TenantID: env.tenant.String(),
		BotID: env.bot.ID, Prompt: "trigger work", Enabled: true,
	})

	// --- manual fire: created_by is the caller ---
	status, body := env.fire(map[string]any{"idempotencyKey": "manual-1", "reason": "manual"})
	if status != http.StatusOK {
		t.Fatalf("manual fire = %d %v", status, body)
	}
	var first struct {
		Result agentext.FireResult `json:"result"`
	}
	raw, _ := json.Marshal(body)
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatal(err)
	}
	if first.Result.Status != "fired" || first.Result.BotID != env.bot.ID {
		t.Fatalf("manual fire result = %+v", first.Result)
	}
	if first.Result.CreatedBy != env.caller.ID.String() {
		t.Fatalf("manual created_by = %q, want caller %s", first.Result.CreatedBy, env.caller.ID)
	}
	sessionID := uuid.MustParse(first.Result.SessionID)
	ownership := env.ownership(t, sessionID)
	if ownership.BotID != env.bot.ID || ownership.AccountID != env.botAcct.ID {
		t.Fatalf("session ownership = %+v", ownership)
	}
	if ownership.CreatedByAccountID != env.caller.ID {
		t.Fatalf("session created_by = %s, want caller %s", ownership.CreatedByAccountID, env.caller.ID)
	}
	if ownership.TeamBotID != "" {
		t.Fatalf("bot trigger wrote a team snapshot: %q", ownership.TeamBotID)
	}
	if owner, found, err := env.st.SessionOwnerAccount(context.Background(), env.tenant, sessionID); err != nil || !found || owner != env.botAcct.ID {
		t.Fatalf("owner relation = %s found=%v err=%v", owner, found, err)
	}
	env.assertSelection(t, sessionID)

	// --- tool surface: the Bot's Account is the principal ---
	toolAuth, err := toolauth.New(env.bridge, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	scope := kernel.Scope{TenantID: env.tenant, SessionID: sessionID}
	readCall := agentcore.AgentToolCall{ID: "call-1", Name: "read", Arguments: json.RawMessage(`{"path":"a.txt"}`)}
	if err := toolAuth.AuthorizeToolCall(context.Background(), scope, readCall); err == nil {
		t.Fatal("tool call was allowed before the Bot Account held file.read")
	}
	if _, err := env.st.CreatePermissionGrant(context.Background(), store.PermissionGrantRecord{
		TenantID: env.tenant, ID: uuid.New(), SubjectType: store.PermissionSubjectAccount,
		SubjectID: env.botAcct.ID, PermissionKey: access.KeyFileRead,
		Scope: string(access.ScopeTenant), GrantedBy: env.caller.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := toolAuth.AuthorizeToolCall(context.Background(), scope, readCall); err != nil {
		t.Fatalf("tool call after the Bot Account grant = %v", err)
	}
	principal, err := env.bridge.SessionPrincipal(context.Background(), env.tenant, sessionID)
	if err != nil || principal.BotID != env.bot.ID || principal.AccountID != env.botAcct.ID {
		t.Fatalf("session principal = %+v err=%v", principal, err)
	}

	// --- same idempotency key: one session, one turn, one ownership write ---
	status, body = env.fire(map[string]any{"idempotencyKey": "manual-1", "reason": "manual"})
	if status != http.StatusOK {
		t.Fatalf("duplicate fire = %d %v", status, body)
	}
	var duplicate struct {
		Result agentext.FireResult `json:"result"`
	}
	raw, _ = json.Marshal(body)
	_ = json.Unmarshal(raw, &duplicate)
	if !duplicate.Result.Deduped || duplicate.Result.SessionID != first.Result.SessionID {
		t.Fatalf("duplicate fire = %+v", duplicate.Result)
	}
	env.assertOneUserMessage(t, sessionID)

	// --- webhook fire: created_by is the Bot's service account ---
	status, body = env.fire(map[string]any{"idempotencyKey": "webhook-1", "reason": "webhook"})
	if status != http.StatusOK {
		t.Fatalf("webhook fire = %d %v", status, body)
	}
	var webhook struct {
		Result agentext.FireResult `json:"result"`
	}
	raw, _ = json.Marshal(body)
	_ = json.Unmarshal(raw, &webhook)
	if webhook.Result.CreatedBy != env.botAcct.ID.String() {
		t.Fatalf("webhook created_by = %q, want bot account %s", webhook.Result.CreatedBy, env.botAcct.ID)
	}
	webhookOwnership := env.ownership(t, uuid.MustParse(webhook.Result.SessionID))
	if webhookOwnership.CreatedByAccountID != env.botAcct.ID {
		t.Fatalf("webhook session created_by = %s, want bot account %s",
			webhookOwnership.CreatedByAccountID, env.botAcct.ID)
	}

	// --- restart: a fresh engine re-fires the same key; ownership is unchanged
	// and the kernel dedupes the committed message ---
	restarted := agentext.NewTriggerEngine(agentext.TriggerEngineOptions{
		Starter: agentext.NewTurnStarter(storeSessions{Store: env.st}, env.kern,
			agentext.NewHookBus(agentext.HookBusOptions{Logger: testLogger()}), testLogger()),
		Logger: testLogger(), Bots: storeTriggerOwner{store: env.st},
		Ownership: storeTriggerOwner{store: env.st},
	})
	defer restarted.Close()
	if _, err := restarted.Register(agentext.TriggerSpec{
		ID: triggerID, Kind: agentext.TriggerWebhook, TenantID: env.tenant.String(),
		BotID: env.bot.ID, Prompt: "trigger work", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	replayed, err := restarted.Fire(context.Background(), triggerID, env.tenant, agentext.FireRequest{
		IdempotencyKey: "manual-1", Reason: "manual", CreatedByAccountID: env.caller.ID,
	})
	if err != nil || !replayed.Deduped || replayed.SessionID != first.Result.SessionID {
		t.Fatalf("restart replay = %+v err=%v", replayed, err)
	}
	if got := env.ownership(t, sessionID); got.BotID != env.bot.ID || got.AccountID != env.botAcct.ID ||
		got.CreatedByAccountID != env.caller.ID {
		t.Fatalf("restart changed ownership: %+v", got)
	}
	env.assertOneUserMessage(t, sessionID)

	// --- missing Bot on a tenantless trigger: 403 at fire, no session row ---
	if _, err := env.triggers.Register(agentext.TriggerSpec{
		ID: "missing-bot-trigger", Kind: agentext.TriggerWebhook,
		BotID: "ghost-bot", Prompt: "p", Enabled: true,
	}); err != nil {
		t.Fatalf("register tenantless trigger: %v", err)
	}
	missingSession := deterministicTriggerSession(env.tenant, "missing-bot-trigger", "ghost-1")
	status, body = env.do(http.MethodPost, "/v1/triggers/missing-bot-trigger/fire",
		map[string]any{"idempotencyKey": "ghost-1"})
	if status != http.StatusForbidden {
		t.Fatalf("fire missing bot = %d %v, want 403", status, body)
	}
	if _, err := env.st.GetSessionOwnership(context.Background(), env.tenant, missingSession); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing Bot wrote a session row: %v", err)
	}

	// --- archived Bot: 409, no session row, no turn ---
	if err := env.st.ArchiveBot(context.Background(), env.tenant, env.bot.ID); err != nil {
		t.Fatal(err)
	}
	refusedID := deterministicTriggerSession(env.tenant, triggerID, "after-archive")
	status, body = env.fire(map[string]any{"idempotencyKey": "after-archive", "reason": "manual"})
	if status != http.StatusConflict {
		t.Fatalf("fire archived bot = %d %v, want 409", status, body)
	}
	if _, err := env.st.GetSessionOwnership(context.Background(), env.tenant, refusedID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("archived Bot wrote a session row: %v", err)
	}
	statusTrigger, _ := env.triggers.Get(triggerID)
	if statusTrigger.LastStatus != "refused" || statusTrigger.Failures == 0 {
		t.Fatalf("archived refusal audit = %+v", statusTrigger)
	}
}

// TestGenericSessionWithoutBotStillRefusesToRun confirms the A4.2b3a fail
// closed rule has no bypass: a session created without botId is account-owned,
// and the runtime binding resolver refuses it before any attempt.
func TestGenericSessionWithoutBotStillRefusesToRun(t *testing.T) {
	env := newTriggerOwnershipEnv(t)
	sessionID := uuid.New()
	if err := env.st.CreateAccountOwnedSession(context.Background(), env.tenant, sessionID, env.caller.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := servicehost.LoadSessionBinding(context.Background(), env.st, env.tenant, sessionID, nil); !errors.Is(err, servicehost.ErrBindingMissing) {
		t.Fatalf("unowned session binding = %v, want ErrBindingMissing", err)
	}
}

func (e *triggerOwnershipEnv) assertSelection(t *testing.T, sessionID uuid.UUID) {
	t.Helper()
	events, err := e.kern.LoadEvents(context.Background(), kernel.Scope{TenantID: e.tenant, SessionID: sessionID}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind != kernel.KindUserMessage {
			continue
		}
		var payload struct {
			Selection struct {
				BotID string `json:"botId"`
			} `json:"selection"`
			Trigger struct {
				ID string `json:"id"`
			} `json:"trigger"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Selection.BotID != e.bot.ID || payload.Trigger.ID != triggerID {
			t.Fatalf("user.message payload = %s", event.Payload)
		}
		return
	}
	t.Fatal("fired session has no user.message")
}

func (e *triggerOwnershipEnv) assertOneUserMessage(t *testing.T, sessionID uuid.UUID) {
	t.Helper()
	events, err := e.kern.LoadEvents(context.Background(), kernel.Scope{TenantID: e.tenant, SessionID: sessionID}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	users := 0
	for _, event := range events {
		if event.Kind == kernel.KindUserMessage {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("session %s has %d user.message events, want 1", sessionID, users)
	}
}
