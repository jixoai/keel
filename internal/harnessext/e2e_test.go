package harnessext_test

// Extension-surface E2E: a real PostgreSQL store, a real agent worker running
// the deterministic fake provider, the real product services (hooks, decisions,
// triggers, sub-agents, plugins) and the real HarnessAPI over HTTP. The chain
// proven here is exactly the change's X5 acceptance: plugin registration →
// trigger → hook execution → sub-agent return, plus the durable decision answer.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/harnessapi"
	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/telemetry"
	"github.com/jixoai/keel/internal/testkit"
)

func testLogger() *slog.Logger {
	if os.Getenv("KEEL_E2E_LOGS") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// e2eAccess is the minimal permissive Access seam for this fixture. The test
// creates one tenant and its sessions directly, so there is no account or
// relation row to resolve; a request is that tenant's single synthetic actor
// and every candidate is visible. Production uses accesswire.Bridge and the
// same handlers answer 403/404 by relation — this double only proves the
// permission-aware code path is wired, not the policy itself (which the
// identity/edge suites cover against real PG).
type e2eAccess struct{}

func (e2eAccess) Principal(_ context.Context, tenantID uuid.UUID) (access.Principal, error) {
	return access.Principal{
		TenantID: tenantID,
		AccountID: uuid.NewSHA1(uuid.NameSpaceOID,
			[]byte("harnessext-e2e:"+tenantID.String())),
	}, nil
}

func (e2eAccess) Authorize(_ context.Context, _ access.Principal, key string, _ access.Resource) (access.Decision, error) {
	return access.Decision{Allow: true, Reason: access.ReasonAllow, Key: key, Scope: access.ScopeAll}, nil
}

func (e2eAccess) Visible(_ context.Context, _ access.Principal, key, _ string, candidates []string) (access.VisibleResult, error) {
	return access.VisibleResult{
		IDs:      append([]string(nil), candidates...),
		Scope:    access.ScopeAll,
		Decision: access.Decision{Allow: true, Reason: access.ReasonAllow, Key: key, Scope: access.ScopeAll},
	}, nil
}

func (e2eAccess) Effective(context.Context, access.Principal) (access.Grants, error) {
	return access.Grants{}, nil
}

func (e2eAccess) CanGrant(context.Context, access.Principal, []access.Grant) error { return nil }

func (e2eAccess) BotOwnerAccount(context.Context, uuid.UUID, string) (uuid.UUID, bool, error) {
	return uuid.Nil, false, nil
}

func (e2eAccess) AuditGrant(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string, string, string, string, map[string]any) error {
	return nil
}

func (e2eAccess) AuditRelation(context.Context, uuid.UUID, uuid.UUID, string, string, string, string, string, string) error {
	return nil
}

func (e2eAccess) AuditSuperAdmin(context.Context, access.Principal, string, map[string]any) error {
	return nil
}

func (e2eAccess) AuditInvoke(context.Context, uuid.UUID, access.Principal, string, uuid.UUID, string) error {
	return nil
}

func (e2eAccess) AuditBotKey(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string, string, string, string) error {
	return nil
}

func (e2eAccess) Denied(context.Context, access.Principal, access.Decision, access.Resource) {}

// openTestStore creates a dedicated database for one test so parallel packages
// sharing KEEL_TEST_DSN can never claim each other's runs. It skips without a
// DSN, like the rest of the repository's database tests.
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
	name := "keel_agent_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close(ctx)
		t.Fatalf("create test database: %v", err)
	}
	// pgx.ConnConfig.ConnString returns the ORIGINAL connection string, so the
	// database override must be applied to the DSN itself (a mistake here would
	// point the test worker at the shared database).
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

// dsnWithDatabase rewrites a PostgreSQL DSN to target another database.
func dsnWithDatabase(dsn, name string) string {
	if parsed, err := url.Parse(dsn); err == nil && parsed.Scheme != "" {
		parsed.Path = "/" + name
		return parsed.String()
	}
	return dsn + " dbname=" + name
}

// storeTriggerOwner binds the trigger engine's ownership seams to the real
// store, exactly like cmd/keel's triggerBotOwner (the test package cannot
// import package main).
type storeTriggerOwner struct{ store *store.Store }

func (o storeTriggerOwner) ResolveTriggerBot(ctx context.Context, tenantID uuid.UUID, botID string) (harnessext.TriggerBot, error) {
	bot, err := o.store.GetBot(ctx, tenantID, botID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return harnessext.TriggerBot{}, harnessext.ErrTriggerBotMissing
		}
		return harnessext.TriggerBot{}, err
	}
	if bot.Archived {
		return harnessext.TriggerBot{}, harnessext.ErrTriggerBotArchived
	}
	profile, err := o.store.GetProfile(ctx, tenantID, bot.ProfileID)
	if err != nil {
		return harnessext.TriggerBot{}, err
	}
	return harnessext.TriggerBot{TenantID: tenantID, BotID: bot.ID, AccountID: profile.AccountID}, nil
}

func (o storeTriggerOwner) CreateOwnedSession(ctx context.Context, ownership harnessext.TriggerSessionOwnership) error {
	return o.store.CreateOwnedSession(ctx, store.SessionOwnership{
		TenantID: ownership.TenantID, SessionID: ownership.SessionID, BotID: ownership.BotID,
		AccountID: ownership.AccountID, CreatedByAccountID: ownership.CreatedByAccountID,
		TeamBotID: ownership.TeamBotID,
	})
}

// createE2EBot creates the account/profile/bot triple a trigger fires as.
func createE2EBot(t *testing.T, st *store.Store, ctx context.Context, tenantID uuid.UUID, botID string) (store.BotRecord, store.AccountRecord) {
	t.Helper()
	account, err := st.CreateAccount(ctx, store.AccountRecord{
		TenantID: tenantID, ID: uuid.New(),
		Username: botID + "-" + tenantID.String()[:8], DisplayName: botID, Kind: "bot",
	})
	if err != nil {
		t.Fatalf("create bot account: %v", err)
	}
	role, err := st.CreateRole(ctx, store.RoleRecord{TenantID: tenantID, ID: uuid.New(), Name: "Operator"})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	profile, err := st.CreateProfile(ctx, store.ProfileRecord{
		TenantID: tenantID, ID: uuid.New(), AccountID: account.ID, RoleID: role.ID,
	})
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	bot, err := st.CreateBotWithOwner(ctx, store.BotRecord{
		TenantID: tenantID, ID: botID, Name: botID, ProfileID: profile.ID, ModelKind: "text",
		DefaultModel: json.RawMessage(`{"providerId":"fake","modelId":"fake"}`),
		Harness:      json.RawMessage(`{"tools":["read"],"context":{"permissions":["file.read"]}}`),
	}, account.ID, account.ID)
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	return bot, account
}

type storeSessions struct{ *store.Store }

func (s storeSessions) RunState(ctx context.Context, tenantID, sessionID uuid.UUID) (harnessext.RunState, error) {
	run, err := s.Store.GetRun(ctx, tenantID, sessionID)
	if err != nil {
		if err == store.ErrNoRun {
			return harnessext.RunState{}, harnessext.ErrNoRun
		}
		return harnessext.RunState{}, err
	}
	return harnessext.RunState{Status: run.Status, Attempts: run.Attempts, CursorSeq: run.CursorSeq, LastError: run.LastError}, nil
}

func doJSON(t *testing.T, client *http.Client, method, url string, tenant uuid.UUID, body any, out any) int {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", tenant.String())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decode %s %s (%d): %v body=%s", method, url, res.StatusCode, err, raw)
		}
	}
	return res.StatusCode
}

func TestExtensionSurfaceE2E(t *testing.T) {
	st := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := testLogger()

	klog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	kern := kernel.New(klog, kernel.Options{})
	tenantID := uuid.New()
	if err := st.CreateTenant(ctx, tenantID, "harnessext-e2e"); err != nil {
		t.Fatal(err)
	}
	// Every trigger fires as a Bot now: the fixture owns one real Bot so the
	// example trigger can register and its fired session carries ownership.
	e2eBot, e2eAccount := createE2EBot(t, st, ctx, tenantID, "e2e-bot")
	parentID := uuid.New()
	if err := st.CreateOwnedSession(ctx, store.SessionOwnership{
		TenantID: tenantID, SessionID: parentID, BotID: e2eBot.ID,
		AccountID: e2eAccount.ID, CreatedByAccountID: e2eAccount.ID,
	}); err != nil {
		t.Fatal(err)
	}

	// A real worker over the real kernel, with the deterministic fake provider
	// and the product turn-telemetry decorator (the same wiring servicehost
	// performs through ProductOptions.WrapTelemetry). The worker also gets the
	// product run-control seam, so an HTTP cancel stops a live attempt and the
	// attempt summary lands as run.finished.
	hooks := harnessext.NewHookBus(harnessext.HookBusOptions{Logger: log})
	defer hooks.Close()
	memTelemetry := telemetry.NewMemory()
	runner := agent.NewRunner(kern, kern, agent.FakeStream(), "fake", "fake", "", log)
	// This suite drives fake-provider turns through the deterministic echo tool
	// without the identity surface: the allow-all fake is the explicit opt-out
	// the fail-closed production default requires (A4.2b1).
	runner.ToolAccess = agent.AllowAllToolAccess{}
	// Production runners get the tool from the keel.testkit plugin; the test
	// registers it directly because it exercises the extension runtime, not the
	// product tool surface.
	if err := runner.Tools.Register(testkit.EchoTool{}); err != nil {
		t.Fatal(err)
	}
	runner.Telemetry = &agent.RunTelemetry{
		Context: &harnessext.TurnTelemetry{Base: memTelemetry, Hooks: hooks},
		Metrics: memTelemetry,
	}
	sessions := storeSessions{Store: st}
	runControl := harnessext.NewRunControl(sessions, log)
	worker := agent.NewWorker(kern, st, runner, "harnessext-e2e-"+uuid.NewString()[:8], 4, log)
	worker.Poll = 50 * time.Millisecond
	worker.Lease = 10 * time.Second
	worker.Committer = kern
	worker.Control = runControl
	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	go worker.Run(workerCtx)

	decisions := harnessext.NewDecisionBroker(harnessext.DecisionBrokerOptions{
		Store: harnessext.NewTasksDecisionStore(st), Hooks: hooks, Logger: log,
	})
	defer decisions.Close()
	partials := harnessext.NewPartialHub()
	starter := harnessext.NewTurnStarter(sessions, kern, hooks, log)
	subagents := harnessext.NewSubagentRunner(harnessext.SubagentOptions{
		Gate: sessions, Edge: kern, Events: kern, Runs: sessions,
		Control: runControl, Hooks: hooks, Logger: log, Poll: 50 * time.Millisecond,
	})
	triggerOwner := storeTriggerOwner{store: st}
	triggers := harnessext.NewTriggerEngine(harnessext.TriggerEngineOptions{
		Starter: starter, Hooks: hooks, Logger: log,
		Bots: triggerOwner, Ownership: triggerOwner,
	})
	defer triggers.Close()

	// The probe plugin records what it observes; it is the E2E's proof that
	// hooks run, and that their payloads carry tenant/session/parent ids.
	observed := make(chan harnessext.HookPayload, 64)
	probe := &harnessext.Plugin{
		ID: "test.probe",
		Hooks: []harnessext.HookSpec{
			{Event: harnessext.HookTriggerAfter, Handler: func(_ context.Context, payload harnessext.HookPayload) error {
				observed <- payload
				return nil
			}},
			{Event: harnessext.HookDecisionAnswered, Handler: func(_ context.Context, payload harnessext.HookPayload) error {
				observed <- payload
				return nil
			}},
			{Event: harnessext.HookSubagentFinished, Handler: func(_ context.Context, payload harnessext.HookPayload) error {
				observed <- payload
				return nil
			}},
			{Event: harnessext.HookTurnStarted, Handler: func(_ context.Context, payload harnessext.HookPayload) error {
				observed <- payload
				return nil
			}},
			{Event: harnessext.HookTurnFinished, Handler: func(_ context.Context, payload harnessext.HookPayload) error {
				observed <- payload
				return nil
			}},
		},
	}
	example := harnessext.NewExamplePlugin(harnessext.ExamplePluginOptions{
		TenantID: tenantID.String(), BotID: e2eBot.ID,
	})
	plugins := harnessext.NewRuntime(harnessext.RuntimeOptions{
		Plugins:   []*harnessext.Plugin{probe, example},
		Hooks:     hooks,
		Triggers:  triggers,
		Decisions: decisions,
		Subagents: subagents,
		Partials:  partials,
		Logger:    log,
	})
	if err := plugins.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	defer plugins.Close(ctx)

	cancelRun := func(cancelCtx context.Context, tenant, session uuid.UUID) (harnessext.CancelResult, error) {
		owner, exists, err := st.SessionOwner(cancelCtx, tenant, session)
		if err != nil {
			return harnessext.CancelResult{}, err
		}
		if !exists {
			return harnessext.CancelResult{}, harnessapi.ErrSessionNotFound
		}
		if owner == store.StateOwnerExternal {
			return harnessext.CancelResult{}, harnessapi.ErrExternalSession
		}
		return runControl.Cancel(cancelCtx, kernel.Scope{TenantID: tenant, SessionID: session})
	}
	api := harnessapi.New(harnessapi.Config{
		Sessions: harnessapi.NewPGSessionLister(st),
		// The A4.2a handlers authorize list/get sub-agents and sessions through
		// the Access seam; without it those routes fail closed (503). This fixture
		// has no account/relation rows, so the double above makes the tenant's
		// synthetic actor visible to everything it owns.
		Access:    e2eAccess{},
		Cancel:    cancelRun,
		Decisions: decisions,
		Partials:  partials,
		Plugins:   plugins,
		Triggers:  triggers,
		Subagents: subagents,
		Hooks:     hooks,
		RunStatus: func(tenant, session uuid.UUID) (string, bool) {
			return runControl.Status(kernel.Scope{TenantID: tenant, SessionID: session})
		},
		Logger: log,
	})
	server := httptest.NewServer(api)
	defer server.Close()
	client := server.Client()

	// --- 1. plugin assembly is observable ---
	var pluginsBody struct {
		Generation int64 `json:"generation"`
		Plugins    []struct {
			ID       string                   `json:"id"`
			State    string                   `json:"state"`
			Tools    []string                 `json:"tools"`
			Triggers []harnessext.TriggerSpec `json:"triggers"`
		} `json:"plugins"`
		Tools []string `json:"tools"`
	}
	if code := doJSON(t, client, http.MethodGet, server.URL+"/v1/plugins", tenantID, nil, &pluginsBody); code != 200 {
		t.Fatalf("plugins: %d", code)
	}
	if pluginsBody.Generation == 0 {
		t.Fatal("plugin generation is zero")
	}
	states := map[string]string{}
	for _, plugin := range pluginsBody.Plugins {
		states[plugin.ID] = plugin.State
	}
	if states[harnessext.ExamplePluginID] != "active" || states["test.probe"] != "active" {
		t.Fatalf("plugin states = %+v", states)
	}

	// --- 2. trigger → turn → hooooooks ---
	fireKey := "e2e-" + uuid.NewString()[:8]
	var fireBody struct {
		Result harnessext.FireResult `json:"result"`
	}
	if code := doJSON(t, client, http.MethodPost, server.URL+"/v1/triggers/"+harnessext.ExamplePingTriggerID+"/fire", tenantID,
		map[string]any{"idempotencyKey": fireKey}, &fireBody); code != 200 {
		t.Fatalf("fire: %d", code)
	}
	if fireBody.Result.Status != "fired" || fireBody.Result.SessionID == "" {
		t.Fatalf("fire result = %+v", fireBody.Result)
	}
	triggerSession := uuid.MustParse(fireBody.Result.SessionID)

	// Duplicate key: no second turn.
	var duplicateBody struct {
		Result harnessext.FireResult `json:"result"`
	}
	if code := doJSON(t, client, http.MethodPost, server.URL+"/v1/triggers/"+harnessext.ExamplePingTriggerID+"/fire", tenantID,
		map[string]any{"idempotencyKey": fireKey}, &duplicateBody); code != 200 {
		t.Fatalf("duplicate fire: %d", code)
	}
	if !duplicateBody.Result.Deduped || duplicateBody.Result.SessionID != fireBody.Result.SessionID {
		t.Fatalf("duplicate fire = %+v", duplicateBody.Result)
	}

	waitForEvents(t, kern, tenantID, triggerSession, func(events []kernel.Event) bool {
		userMessages, assistant := 0, 0
		for _, event := range events {
			switch event.Kind {
			case kernel.KindUserMessage:
				userMessages++
				if !bytes.Contains(event.Payload, []byte(`"trigger"`)) || !bytes.Contains(event.Payload, []byte(fireKey)) {
					t.Errorf("user.message lacks trigger provenance: %s", event.Payload)
				}
			case kernel.KindAssistantMessage:
				assistant++
			}
		}
		return userMessages == 1 && assistant >= 1
	}, "trigger turn to complete with exactly one user message")

	triggerHook := waitForHook(t, observed, harnessext.HookTriggerAfter)
	if triggerHook.TriggerID != harnessext.ExamplePingTriggerID || triggerHook.Status != "fired" {
		t.Fatalf("trigger.after payload = %+v", triggerHook)
	}
	if triggerHook.TenantID != tenantID.String() || triggerHook.SessionID != triggerSession.String() {
		t.Fatalf("trigger.after ids = %+v", triggerHook)
	}
	// The run telemetry span became the turn lifecycle hooks for the same
	// session (the decorator is what keeps the worker unaware of hooks).
	turnStarted := waitForHook(t, observed, harnessext.HookTurnStarted)
	if turnStarted.SessionID != triggerSession.String() || turnStarted.RunID == "" {
		t.Fatalf("turn.started = %+v", turnStarted)
	}
	turnFinished := waitForHook(t, observed, harnessext.HookTurnFinished)
	if turnFinished.SessionID != turnStarted.SessionID || turnFinished.RunID != turnStarted.RunID || turnFinished.Status != "ok" {
		t.Fatalf("turn.finished = %+v (started %+v)", turnFinished, turnStarted)
	}

	// --- 3. decision: durable, idempotent, hook-visible ---
	var openBody struct {
		Decision harnessext.Decision `json:"decision"`
	}
	if code := doJSON(t, client, http.MethodPost, server.URL+"/v1/sessions/"+triggerSession.String()+"/decisions", tenantID,
		map[string]any{"requestId": "e2e-approval", "prompt": "Ship it?", "options": []map[string]string{{"id": "yes", "label": "Yes"}, {"id": "no", "label": "No"}}}, &openBody); code != 201 {
		t.Fatalf("open decision: %d", code)
	}
	decisionID := openBody.Decision.DecisionID
	answerURL := server.URL + "/v1/sessions/" + triggerSession.String() + "/decisions/" + decisionID
	var answerBody struct {
		Duplicate bool                `json:"duplicate"`
		Status    string              `json:"status"`
		Decision  harnessext.Decision `json:"decision"`
	}
	if code := doJSON(t, client, http.MethodPost, answerURL, tenantID, map[string]string{"optionId": "yes"}, &answerBody); code != 200 || answerBody.Duplicate {
		t.Fatalf("answer: %d %+v", code, answerBody)
	}
	if code := doJSON(t, client, http.MethodPost, answerURL, tenantID, map[string]string{"optionId": "yes"}, &answerBody); code != 200 || !answerBody.Duplicate {
		t.Fatalf("duplicate answer: %d %+v", code, answerBody)
	}
	if code := doJSON(t, client, http.MethodPost, answerURL, tenantID, map[string]string{"optionId": "no"}, nil); code != 409 {
		t.Fatalf("conflicting answer: %d", code)
	}
	decisionHook := waitForHook(t, observed, harnessext.HookDecisionAnswered)
	if decisionHook.DecisionID != decisionID {
		t.Fatalf("decision hook = %+v", decisionHook)
	}
	// Durability: a fresh broker over the same rows answers the same way.
	restarted := harnessext.NewDecisionBroker(harnessext.DecisionBrokerOptions{Store: harnessext.NewTasksDecisionStore(st), Logger: log})
	defer restarted.Close()
	reloaded, duplicate, err := restarted.Answer(ctx, harnessext.AnswerDecisionRequest{
		TenantID: tenantID, SessionID: triggerSession, DecisionID: uuid.MustParse(decisionID), OptionID: "yes",
	})
	if err != nil || !duplicate || reloaded.Status != harnessext.DecisionAnswered {
		t.Fatalf("durable answer after restart: %v duplicate=%v status=%s", err, duplicate, reloaded.Status)
	}
	// The question itself must survive the answer: an update that only wrote the
	// status would pass the check above while losing the prompt.
	if reloaded.RequestID != "e2e-approval" || reloaded.Prompt != "Ship it?" || reloaded.Deadline.IsZero() || len(reloaded.Options) != 2 {
		t.Fatalf("durable decision lost its question: %+v", reloaded)
	}

	// --- 4. sub-agent: independent session + structured result + hooks ---
	var spawnBody struct {
		Subagent harnessext.SubagentRecord `json:"subagent"`
		Result   harnessext.SubagentResult `json:"result"`
	}
	if code := doJSON(t, client, http.MethodPost, server.URL+"/v1/sessions/"+triggerSession.String()+"/subagents", tenantID,
		map[string]any{"task": "echo delegated work", "wait": true, "budget": map[string]any{"timeoutMs": 20000}},
		&spawnBody); code != 200 {
		t.Fatalf("spawn: %d", code)
	}
	if spawnBody.Result.Status != "done" || spawnBody.Result.SessionID == "" {
		t.Fatalf("sub-agent result = %+v", spawnBody.Result)
	}
	if !strings.Contains(spawnBody.Result.Summary, "echo") {
		t.Fatalf("sub-agent summary = %q", spawnBody.Result.Summary)
	}
	if spawnBody.Result.ParentSessionID != triggerSession.String() {
		t.Fatalf("sub-agent parent = %q", spawnBody.Result.ParentSessionID)
	}
	var subagentsBody struct {
		Subagents []harnessext.SubagentRecord `json:"subagents"`
	}
	if code := doJSON(t, client, http.MethodGet, server.URL+"/v1/subagents?parentSessionId="+triggerSession.String(), tenantID, nil, &subagentsBody); code != 200 {
		t.Fatalf("list subagents: %d", code)
	}
	if len(subagentsBody.Subagents) != 1 || subagentsBody.Subagents[0].Status != "done" {
		t.Fatalf("subagents = %+v", subagentsBody.Subagents)
	}
	subagentHook := waitForHook(t, observed, harnessext.HookSubagentFinished)
	if subagentHook.Status != "done" || subagentHook.ParentSessionID != triggerSession.String() {
		t.Fatalf("subagent.finished = %+v", subagentHook)
	}

	// --- 5. session list: both sessions, keyset page, parent link ---
	var sessionsBody struct {
		Sessions []harnessapi.SessionSummary `json:"sessions"`
		HasMore  bool                        `json:"hasMore"`
	}
	if code := doJSON(t, client, http.MethodGet, server.URL+"/v1/sessions?limit=50", tenantID, nil, &sessionsBody); code != 200 {
		t.Fatalf("sessions: %d", code)
	}
	foundTrigger, foundChild := false, false
	for _, summary := range sessionsBody.Sessions {
		switch summary.SessionID {
		case triggerSession.String():
			foundTrigger = true
			if summary.RunStatus != "done" || summary.LastSeq == 0 {
				t.Fatalf("trigger session summary = %+v", summary)
			}
		case spawnBody.Result.SessionID:
			foundChild = true
			if summary.ParentSessionID != triggerSession.String() {
				t.Fatalf("child summary parent = %q", summary.ParentSessionID)
			}
		}
	}
	if !foundTrigger || !foundChild {
		t.Fatalf("sessions page missing rows: %+v", sessionsBody.Sessions)
	}

	// --- 6. cancel: a live attempt is stopped through the run-control seam and
	// the attempt summary records the deliberate stop. The echo tool sleeps so
	// the attempt is reliably in flight. ---
	if err := os.Setenv("KEEL_TOOL_DELAY_MS", "1500"); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv("KEEL_TOOL_DELAY_MS")
	cancelSession := uuid.New()
	if err := st.CreateSession(ctx, tenantID, cancelSession); err != nil {
		t.Fatal(err)
	}
	if _, err := starter.StartTurn(ctx, harnessext.StartTurnRequest{
		TenantID: tenantID, SessionID: cancelSession, Text: "echo slow cancel", RequestID: "cancel-e2e",
	}); err != nil {
		t.Fatal(err)
	}
	waitForRunStatus(t, sessions, tenantID, cancelSession, "running", 10*time.Second)
	var cancelBody map[string]any
	if code := doJSON(t, client, http.MethodPost, server.URL+"/v1/sessions/"+cancelSession.String()+"/cancel", tenantID, nil, &cancelBody); code != 200 {
		t.Fatalf("cancel live session: %d %v", code, cancelBody)
	}
	if cancelBody["status"] != "cancelled" || cancelBody["cancelled"] != true {
		t.Fatalf("cancel live session body = %v", cancelBody)
	}
	waitForEvents(t, kern, tenantID, cancelSession, func(events []kernel.Event) bool {
		for _, event := range events {
			if event.Kind != kernel.KindRunFinished {
				continue
			}
			var summary struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(event.Payload, &summary) == nil && summary.Status == "cancelled" {
				return true
			}
		}
		return false
	}, "run.finished with a cancelled status")
	os.Unsetenv("KEEL_TOOL_DELAY_MS")

	// The session stays usable: the next input starts a fresh attempt.
	if _, err := starter.StartTurn(ctx, harnessext.StartTurnRequest{
		TenantID: tenantID, SessionID: cancelSession, Text: "hello again", RequestID: "after-cancel",
	}); err != nil {
		t.Fatal(err)
	}
	waitForEvents(t, kern, tenantID, cancelSession, func(events []kernel.Event) bool {
		users, assistants := 0, 0
		for _, event := range events {
			switch event.Kind {
			case kernel.KindUserMessage:
				users++
			case kernel.KindAssistantMessage:
				assistants++
			}
		}
		return users >= 2 && assistants >= 1
	}, "the session to keep working after a cancel")

	// A run that already settled is idempotently reported as settled, and an
	// unknown session is 404.
	var settledBody map[string]any
	if code := doJSON(t, client, http.MethodPost, server.URL+"/v1/sessions/"+triggerSession.String()+"/cancel", tenantID, nil, &settledBody); code != 200 {
		t.Fatalf("cancel settled session: %d %v", code, settledBody)
	}
	if settledBody["status"] != "settled" {
		t.Fatalf("cancel settled session body = %v", settledBody)
	}
	if code := doJSON(t, client, http.MethodPost, server.URL+"/v1/sessions/"+uuid.NewString()+"/cancel", tenantID, nil, nil); code != 404 {
		t.Fatalf("cancel unknown session: %d", code)
	}
	if code := doJSON(t, client, http.MethodGet, server.URL+"/v1/sessions", uuid.New(), nil, nil); code != 200 {
		t.Fatalf("foreign tenant list: %d", code)
	}
}

func waitForRunStatus(t *testing.T, reader harnessext.RunStatusReader, tenantID, sessionID uuid.UUID, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := reader.RunState(context.Background(), tenantID, sessionID)
		if err == nil && state.Status == want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	state, err := reader.RunState(context.Background(), tenantID, sessionID)
	t.Fatalf("run status never became %q (last: %+v err=%v)", want, state, err)
}

func waitForEvents(t *testing.T, reader kernel.Reader, tenantID, sessionID uuid.UUID, condition func([]kernel.Event) bool, message string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		events, err := loadAll(reader, tenantID, sessionID)
		if err == nil && condition(events) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	events, _ := loadAll(reader, tenantID, sessionID)
	var kinds []string
	for _, event := range events {
		kinds = append(kinds, string(event.Kind)+":"+string(event.Payload))
	}
	t.Fatalf("timed out: %s (events: %s)", message, strings.Join(kinds, ","))
}

func loadAll(reader kernel.Reader, tenantID, sessionID uuid.UUID) ([]kernel.Event, error) {
	var all []kernel.Event
	var after int64
	for {
		page, err := reader.LoadEvents(context.Background(), kernel.Scope{TenantID: tenantID, SessionID: sessionID}, after, 200)
		if err != nil {
			return all, err
		}
		all = append(all, page...)
		if len(page) < 200 {
			return all, nil
		}
		after = page[len(page)-1].Seq
	}
}

func waitForHook(t *testing.T, observed <-chan harnessext.HookPayload, event harnessext.HookEvent) harnessext.HookPayload {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case payload := <-observed:
			if payload.Event == event {
				return payload
			}
		case <-deadline:
			t.Fatalf("hook %s was not delivered", event)
		}
	}
}

var _ = fmt.Sprintf
