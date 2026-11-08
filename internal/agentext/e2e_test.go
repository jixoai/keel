package agentext_test

// Extension-surface E2E: a real PostgreSQL store, a real agent worker running
// the deterministic fake provider, the real product services (hooks, decisions,
// triggers, sub-agents, plugins) and the real AgentAPI over HTTP. The chain
// proven here is exactly the change's X5 acceptance: plugin registration →
// trigger → hook execution → sub-agent return, plus the durable decision answer.

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/agentapi"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/telemetry"
)

func testLogger() *slog.Logger {
	if os.Getenv("KEEL_E2E_LOGS") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

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

type storeSessions struct{ *store.Store }

func (s storeSessions) RunState(ctx context.Context, tenantID, sessionID uuid.UUID) (agentext.RunState, error) {
	run, err := s.Store.GetRun(ctx, tenantID, sessionID)
	if err != nil {
		if err == store.ErrNoRun {
			return agentext.RunState{}, agentext.ErrNoRun
		}
		return agentext.RunState{}, err
	}
	return agentext.RunState{Status: run.Status, Attempts: run.Attempts, CursorSeq: run.CursorSeq, LastError: run.LastError}, nil
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
	if err := st.CreateTenant(ctx, tenantID, "agentext-e2e"); err != nil {
		t.Fatal(err)
	}
	parentID := uuid.New()
	if err := st.CreateSession(ctx, tenantID, parentID); err != nil {
		t.Fatal(err)
	}

	// A real worker over the real kernel, with the deterministic fake provider
	// and the product turn-telemetry decorator (the same wiring servicehost
	// performs through ProductOptions.WrapTelemetry). The worker also gets the
	// product run-control seam, so an HTTP cancel stops a live attempt and the
	// attempt summary lands as run.finished.
	hooks := agentext.NewHookBus(agentext.HookBusOptions{Logger: log})
	defer hooks.Close()
	memTelemetry := telemetry.NewMemory()
	runner := agent.NewRunner(kern, kern, agent.FakeStream(), "fake", "fake", "", log)
	runner.Telemetry = &agent.RunTelemetry{
		Context: &agentext.TurnTelemetry{Base: memTelemetry, Hooks: hooks},
		Metrics: memTelemetry,
	}
	sessions := storeSessions{Store: st}
	runControl := agentext.NewRunControl(sessions, log)
	worker := agent.NewWorker(kern, st, runner, "agentext-e2e-"+uuid.NewString()[:8], 4, log)
	worker.Poll = 50 * time.Millisecond
	worker.Lease = 10 * time.Second
	worker.Committer = kern
	worker.Control = runControl
	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	go worker.Run(workerCtx)

	decisions := agentext.NewDecisionBroker(agentext.DecisionBrokerOptions{
		Store: agentext.NewTasksDecisionStore(st), Hooks: hooks, Logger: log,
	})
	defer decisions.Close()
	partials := agentext.NewPartialHub()
	starter := agentext.NewTurnStarter(sessions, kern, hooks, log)
	subagents := agentext.NewSubagentRunner(agentext.SubagentOptions{
		Gate: sessions, Edge: kern, Events: kern, Runs: sessions,
		Control: runControl, Hooks: hooks, Logger: log, Poll: 50 * time.Millisecond,
	})
	triggers := agentext.NewTriggerEngine(agentext.TriggerEngineOptions{
		Starter: starter, Hooks: hooks, Logger: log,
	})
	defer triggers.Close()

	// The probe plugin records what it observes; it is the E2E's proof that
	// hooks run, and that their payloads carry tenant/session/parent ids.
	observed := make(chan agentext.HookPayload, 64)
	probe := &agentext.Plugin{
		ID: "test.probe",
		Hooks: []agentext.HookSpec{
			{Event: agentext.HookTriggerAfter, Handler: func(_ context.Context, payload agentext.HookPayload) error {
				observed <- payload
				return nil
			}},
			{Event: agentext.HookDecisionAnswered, Handler: func(_ context.Context, payload agentext.HookPayload) error {
				observed <- payload
				return nil
			}},
			{Event: agentext.HookSubagentFinished, Handler: func(_ context.Context, payload agentext.HookPayload) error {
				observed <- payload
				return nil
			}},
			{Event: agentext.HookTurnStarted, Handler: func(_ context.Context, payload agentext.HookPayload) error {
				observed <- payload
				return nil
			}},
			{Event: agentext.HookTurnFinished, Handler: func(_ context.Context, payload agentext.HookPayload) error {
				observed <- payload
				return nil
			}},
		},
	}
	example := agentext.NewExamplePlugin(agentext.ExamplePluginOptions{TenantID: tenantID.String()})
	plugins := agentext.NewRuntime(agentext.RuntimeOptions{
		Plugins:   []*agentext.Plugin{probe, example, agentext.NewSubagentPlugin(subagents)},
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

	cancelRun := func(cancelCtx context.Context, tenant, session uuid.UUID) (agentext.CancelResult, error) {
		owner, exists, err := st.SessionOwner(cancelCtx, tenant, session)
		if err != nil {
			return agentext.CancelResult{}, err
		}
		if !exists {
			return agentext.CancelResult{}, agentapi.ErrSessionNotFound
		}
		if owner == store.StateOwnerExternal {
			return agentext.CancelResult{}, agentapi.ErrExternalSession
		}
		return runControl.Cancel(cancelCtx, kernel.Scope{TenantID: tenant, SessionID: session})
	}
	api := agentapi.New(agentapi.Config{
		Sessions:  agentapi.NewPGSessionLister(st),
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
			ID       string                 `json:"id"`
			State    string                 `json:"state"`
			Tools    []string               `json:"tools"`
			Triggers []agentext.TriggerSpec `json:"triggers"`
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
	if states[agentext.ExamplePluginID] != "active" || states["test.probe"] != "active" || states["keel.subagents"] != "active" {
		t.Fatalf("plugin states = %+v", states)
	}
	hasSubagentTool := false
	for _, name := range pluginsBody.Tools {
		if name == "spawn_subagent" {
			hasSubagentTool = true
		}
	}
	if !hasSubagentTool {
		t.Fatalf("spawn_subagent is not published: %v", pluginsBody.Tools)
	}

	// --- 2. trigger → turn → hooooooks ---
	fireKey := "e2e-" + uuid.NewString()[:8]
	var fireBody struct {
		Result agentext.FireResult `json:"result"`
	}
	if code := doJSON(t, client, http.MethodPost, server.URL+"/v1/triggers/"+agentext.ExamplePingTriggerID+"/fire", tenantID,
		map[string]any{"idempotencyKey": fireKey}, &fireBody); code != 200 {
		t.Fatalf("fire: %d", code)
	}
	if fireBody.Result.Status != "fired" || fireBody.Result.SessionID == "" {
		t.Fatalf("fire result = %+v", fireBody.Result)
	}
	triggerSession := uuid.MustParse(fireBody.Result.SessionID)

	// Duplicate key: no second turn.
	var duplicateBody struct {
		Result agentext.FireResult `json:"result"`
	}
	if code := doJSON(t, client, http.MethodPost, server.URL+"/v1/triggers/"+agentext.ExamplePingTriggerID+"/fire", tenantID,
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

	triggerHook := waitForHook(t, observed, agentext.HookTriggerAfter)
	if triggerHook.TriggerID != agentext.ExamplePingTriggerID || triggerHook.Status != "fired" {
		t.Fatalf("trigger.after payload = %+v", triggerHook)
	}
	if triggerHook.TenantID != tenantID.String() || triggerHook.SessionID != triggerSession.String() {
		t.Fatalf("trigger.after ids = %+v", triggerHook)
	}
	// The run telemetry span became the turn lifecycle hooks for the same
	// session (the decorator is what keeps the worker unaware of hooks).
	turnStarted := waitForHook(t, observed, agentext.HookTurnStarted)
	if turnStarted.SessionID != triggerSession.String() || turnStarted.RunID == "" {
		t.Fatalf("turn.started = %+v", turnStarted)
	}
	turnFinished := waitForHook(t, observed, agentext.HookTurnFinished)
	if turnFinished.SessionID != turnStarted.SessionID || turnFinished.RunID != turnStarted.RunID || turnFinished.Status != "ok" {
		t.Fatalf("turn.finished = %+v (started %+v)", turnFinished, turnStarted)
	}

	// --- 3. decision: durable, idempotent, hook-visible ---
	var openBody struct {
		Decision agentext.Decision `json:"decision"`
	}
	if code := doJSON(t, client, http.MethodPost, server.URL+"/v1/sessions/"+triggerSession.String()+"/decisions", tenantID,
		map[string]any{"requestId": "e2e-approval", "prompt": "Ship it?", "options": []map[string]string{{"id": "yes", "label": "Yes"}, {"id": "no", "label": "No"}}}, &openBody); code != 201 {
		t.Fatalf("open decision: %d", code)
	}
	decisionID := openBody.Decision.DecisionID
	answerURL := server.URL + "/v1/sessions/" + triggerSession.String() + "/decisions/" + decisionID
	var answerBody struct {
		Duplicate bool              `json:"duplicate"`
		Status    string            `json:"status"`
		Decision  agentext.Decision `json:"decision"`
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
	decisionHook := waitForHook(t, observed, agentext.HookDecisionAnswered)
	if decisionHook.DecisionID != decisionID {
		t.Fatalf("decision hook = %+v", decisionHook)
	}
	// Durability: a fresh broker over the same rows answers the same way.
	restarted := agentext.NewDecisionBroker(agentext.DecisionBrokerOptions{Store: agentext.NewTasksDecisionStore(st), Logger: log})
	defer restarted.Close()
	reloaded, duplicate, err := restarted.Answer(ctx, agentext.AnswerDecisionRequest{
		TenantID: tenantID, SessionID: triggerSession, DecisionID: uuid.MustParse(decisionID), OptionID: "yes",
	})
	if err != nil || !duplicate || reloaded.Status != agentext.DecisionAnswered {
		t.Fatalf("durable answer after restart: %v duplicate=%v status=%s", err, duplicate, reloaded.Status)
	}
	// The question itself must survive the answer: an update that only wrote the
	// status would pass the check above while losing the prompt.
	if reloaded.RequestID != "e2e-approval" || reloaded.Prompt != "Ship it?" || reloaded.Deadline.IsZero() || len(reloaded.Options) != 2 {
		t.Fatalf("durable decision lost its question: %+v", reloaded)
	}

	// --- 4. sub-agent: independent session + structured result + hooks ---
	var spawnBody struct {
		Subagent agentext.SubagentRecord `json:"subagent"`
		Result   agentext.SubagentResult `json:"result"`
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
		Subagents []agentext.SubagentRecord `json:"subagents"`
	}
	if code := doJSON(t, client, http.MethodGet, server.URL+"/v1/subagents?parentSessionId="+triggerSession.String(), tenantID, nil, &subagentsBody); code != 200 {
		t.Fatalf("list subagents: %d", code)
	}
	if len(subagentsBody.Subagents) != 1 || subagentsBody.Subagents[0].Status != "done" {
		t.Fatalf("subagents = %+v", subagentsBody.Subagents)
	}
	subagentHook := waitForHook(t, observed, agentext.HookSubagentFinished)
	if subagentHook.Status != "done" || subagentHook.ParentSessionID != triggerSession.String() {
		t.Fatalf("subagent.finished = %+v", subagentHook)
	}

	// --- 5. session list: both sessions, keyset page, parent link ---
	var sessionsBody struct {
		Sessions []agentapi.SessionSummary `json:"sessions"`
		HasMore  bool                      `json:"hasMore"`
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
	if _, err := starter.StartTurn(ctx, agentext.StartTurnRequest{
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
	if _, err := starter.StartTurn(ctx, agentext.StartTurnRequest{
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

func waitForRunStatus(t *testing.T, reader agentext.RunStatusReader, tenantID, sessionID uuid.UUID, want string, timeout time.Duration) {
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

func waitForHook(t *testing.T, observed <-chan agentext.HookPayload, event agentext.HookEvent) agentext.HookPayload {
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
