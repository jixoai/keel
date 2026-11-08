package agentext

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func waitFor(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", message)
}

func TestHookBusDispatchesAndRecordsFailures(t *testing.T) {
	bus := NewHookBus(HookBusOptions{Logger: testLogger(), Queue: 16, Workers: 1, RetryDelay: time.Millisecond})
	defer bus.Close()

	var delivered atomic.Int64
	var failed atomic.Int64
	if _, err := bus.Subscribe("probe", HookOptions{Event: HookTurnStarted, Handler: func(_ context.Context, payload HookPayload) error {
		if payload.SessionID == "" || payload.TenantID == "" {
			t.Errorf("payload lost its identifiers: %+v", payload)
		}
		delivered.Add(1)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Subscribe("broken", HookOptions{Event: HookTurnStarted, Handler: func(context.Context, HookPayload) error {
		failed.Add(1)
		return errors.New("boom")
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Subscribe("x", HookOptions{Event: HookEvent("nope"), Handler: func(context.Context, HookPayload) error { return nil }}); err == nil {
		t.Fatal("an event outside the closed set must be rejected")
	}
	bus.Emit(HookPayload{Event: HookTurnStarted, TenantID: "t", SessionID: "s"})
	waitFor(t, 2*time.Second, func() bool { return delivered.Load() == 1 && failed.Load() >= 2 }, "hook delivery and retry")
	stats := bus.Stats()
	if len(stats) != 2 {
		t.Fatalf("stats: %+v", stats)
	}
	for _, stat := range stats {
		if stat.Owner == "broken" && (stat.Failed == 0 || stat.LastError == "" || stat.Retries == 0) {
			t.Fatalf("failed hook not visible: %+v", stat)
		}
	}
}

func TestDecisionOpenAnswerIdempotent(t *testing.T) {
	store := NewMemoryDecisionStore()
	broker := NewDecisionBroker(DecisionBrokerOptions{Store: store, Logger: testLogger()})
	defer broker.Close()
	tenant, session := uuid.New(), uuid.New()
	req := OpenDecisionRequest{
		TenantID:  tenant,
		SessionID: session,
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "Deploy to production?",
		Options:   []DecisionOption{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}},
	}
	first, created, err := broker.Open(context.Background(), req)
	if err != nil || !created {
		t.Fatalf("open: %v created=%v", err, created)
	}
	again, created, err := broker.Open(context.Background(), req)
	if err != nil || created || again.DecisionID != first.DecisionID {
		t.Fatalf("idempotent open: err=%v created=%v id=%s", err, created, again.DecisionID)
	}
	decisionID := uuid.MustParse(first.DecisionID)

	answered, duplicate, err := broker.Answer(context.Background(), AnswerDecisionRequest{
		TenantID: tenant, SessionID: session, DecisionID: decisionID, OptionID: "yes",
	})
	if err != nil || duplicate || answered.Status != DecisionAnswered {
		t.Fatalf("answer: %v duplicate=%v status=%s", err, duplicate, answered.Status)
	}
	repeated, duplicate, err := broker.Answer(context.Background(), AnswerDecisionRequest{
		TenantID: tenant, SessionID: session, DecisionID: decisionID, OptionID: "yes",
	})
	if err != nil || !duplicate {
		t.Fatalf("repeat answer must dedupe: err=%v duplicate=%v", err, duplicate)
	}
	if len(repeated.Events) != 2 {
		t.Fatalf("a duplicate answer must not append a journal entry: %+v", repeated.Events)
	}
	if _, _, err := broker.Answer(context.Background(), AnswerDecisionRequest{
		TenantID: tenant, SessionID: session, DecisionID: decisionID, OptionID: "no",
	}); !errors.Is(err, ErrDecisionConflict) {
		t.Fatalf("conflicting answer: %v", err)
	}
	if _, _, err := broker.Answer(context.Background(), AnswerDecisionRequest{
		TenantID: tenant, SessionID: session, DecisionID: uuid.New(), OptionID: "yes",
	}); !errors.Is(err, ErrDecisionNotFound) {
		t.Fatalf("unknown decision: %v", err)
	}
	// A foreign tenant sees nothing.
	if _, found := broker.Get(context.Background(), uuid.New(), session, decisionID); found {
		t.Fatal("decision leaked across tenants")
	}
}

func TestDecisionWaitAndTimeout(t *testing.T) {
	broker := NewDecisionBroker(DecisionBrokerOptions{Store: NewMemoryDecisionStore(), Logger: testLogger()})
	defer broker.Close()
	tenant, session := uuid.New(), uuid.New()
	decision, _, err := broker.Open(context.Background(), OpenDecisionRequest{
		TenantID: tenant, SessionID: session, RequestID: "wait-1",
		Prompt: "Proceed?", AllowFreeText: true, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	decisionID := uuid.MustParse(decision.DecisionID)
	waited := make(chan Decision, 1)
	go func() {
		result, err := broker.Wait(context.Background(), tenant, session, decisionID)
		if err == nil {
			waited <- result
		}
	}()
	time.Sleep(20 * time.Millisecond)
	if _, _, err := broker.Answer(context.Background(), AnswerDecisionRequest{
		TenantID: tenant, SessionID: session, DecisionID: decisionID, Text: "ship it",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-waited:
		if result.Status != DecisionAnswered || result.Answer == nil || result.Answer.Text != "ship it" {
			t.Fatalf("waiter got %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not wake")
	}

	short, _, err := broker.Open(context.Background(), OpenDecisionRequest{
		TenantID: tenant, SessionID: session, RequestID: "timeout-1",
		Prompt: "Quick?", Timeout: 15 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	shortID := uuid.MustParse(short.DecisionID)
	waitFor(t, 2*time.Second, func() bool {
		got, found := broker.Get(context.Background(), tenant, session, shortID)
		return found && got.Status == DecisionTimedOut
	}, "decision timeout")
	if _, _, err := broker.Answer(context.Background(), AnswerDecisionRequest{
		TenantID: tenant, SessionID: session, DecisionID: shortID, Text: "late",
	}); !errors.Is(err, ErrDecisionClosed) {
		t.Fatalf("answer after timeout: %v", err)
	}
}

func TestDecisionCancelSession(t *testing.T) {
	broker := NewDecisionBroker(DecisionBrokerOptions{Store: NewMemoryDecisionStore(), Logger: testLogger()})
	defer broker.Close()
	tenant, session := uuid.New(), uuid.New()
	for i := 0; i < 3; i++ {
		if _, _, err := broker.Open(context.Background(), OpenDecisionRequest{
			TenantID: tenant, SessionID: session, RequestID: "c-" + string(rune('a'+i)), Prompt: "?",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if changed := broker.CancelSession(context.Background(), tenant, session, "session cancelled"); changed != 3 {
		t.Fatalf("cancelled %d decisions", changed)
	}
	decisions, err := broker.List(context.Background(), tenant, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, decision := range decisions {
		if decision.Status != DecisionCancelled {
			t.Fatalf("decision %s is %s", decision.DecisionID, decision.Status)
		}
	}
}

func TestCronSchedule(t *testing.T) {
	schedule, err := parseCron("*/5 9-10 * * 1-5")
	if err != nil {
		t.Fatal(err)
	}
	monday := time.Date(2026, 11, 2, 8, 58, 0, 0, time.UTC) // Monday
	next := schedule.Next(monday)
	if want := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next = %s, want %s", next, want)
	}
	next = schedule.Next(next)
	if want := time.Date(2026, 11, 2, 9, 5, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next = %s, want %s", next, want)
	}
	// Saturday is excluded by 1-5: the next fire is Monday 09:00.
	friday := time.Date(2026, 11, 6, 10, 59, 0, 0, time.UTC)
	next = schedule.Next(friday)
	if want := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("weekend skip: next = %s, want %s", next, want)
	}
	if _, err := parseCron("* * * *"); err == nil {
		t.Fatal("4 fields must be rejected")
	}
	if _, err := parseCron("60 * * * *"); err == nil {
		t.Fatal("out-of-range minute must be rejected")
	}
	sunday, err := parseCron("0 0 * * 7")
	if err != nil {
		t.Fatal(err)
	}
	next = sunday.Next(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC))
	if want := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("7 must mean Sunday: next = %s, want %s", next, want)
	}
}

// countingStarter is a trigger starter that counts calls.
type countingStarter struct {
	mu      sync.Mutex
	starts  []StartTurnRequest
	results map[string]StartTurnResult
	failFor int
	err     error
}

func (s *countingStarter) StartTurn(_ context.Context, req StartTurnRequest) (StartTurnResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil && (s.failFor == 0 || len(s.starts) < s.failFor) {
		s.starts = append(s.starts, req)
		return StartTurnResult{}, s.err
	}
	s.starts = append(s.starts, req)
	sessionID := req.SessionID
	if sessionID == uuid.Nil {
		sessionID = uuid.New()
	}
	return StartTurnResult{TenantID: req.TenantID, SessionID: sessionID, Seq: int64(len(s.starts)), RequestID: req.RequestID}, nil
}

func (s *countingStarter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.starts)
}

func (s *countingStarter) last() StartTurnRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.starts) == 0 {
		return StartTurnRequest{}
	}
	return s.starts[len(s.starts)-1]
}

func TestTriggerManualFireIdempotent(t *testing.T) {
	starter := &countingStarter{}
	hooks := NewHookBus(HookBusOptions{Logger: testLogger()})
	defer hooks.Close()
	engine := NewTriggerEngine(TriggerEngineOptions{Starter: starter, Hooks: hooks, Logger: testLogger()})
	defer engine.Close()
	tenant := uuid.New()
	if _, err := engine.Register(TriggerSpec{
		ID: "test.ping", Kind: TriggerWebhook, Prompt: "ping", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	first, err := engine.Fire(context.Background(), "test.ping", tenant, FireRequest{IdempotencyKey: "k1", Reason: "manual"})
	if err != nil || first.Status != "fired" || first.Deduped {
		t.Fatalf("first fire: %+v err=%v", first, err)
	}
	second, err := engine.Fire(context.Background(), "test.ping", tenant, FireRequest{IdempotencyKey: "k1", Reason: "manual"})
	if err != nil || !second.Deduped || second.SessionID != first.SessionID {
		t.Fatalf("dedupe: %+v err=%v", second, err)
	}
	if starter.count() != 1 {
		t.Fatalf("the same idempotency key started %d turns", starter.count())
	}
	request := starter.last()
	if request.RequestID != "trigger:test.ping:k1" {
		t.Fatalf("request id = %q", request.RequestID)
	}
	// The durable provenance travels inside the committed user.message payload.
	triggerMark, ok := request.Extras["trigger"].(map[string]any)
	if !ok || triggerMark["id"] != "test.ping" || triggerMark["idempotencyKey"] != "k1" {
		t.Fatalf("trigger provenance missing: %#v", request.Extras)
	}
	if _, err := engine.Fire(context.Background(), "test.ping", tenant, FireRequest{Reason: "webhook"}); !errors.Is(err, ErrTriggerKeyRequired) {
		t.Fatalf("webhook without key: %v", err)
	}
	if _, err := engine.Fire(context.Background(), "missing", tenant, FireRequest{IdempotencyKey: "x"}); !errors.Is(err, ErrTriggerNotFound) {
		t.Fatalf("unknown trigger: %v", err)
	}
	if _, err := engine.Fire(context.Background(), "test.ping", uuid.New(), FireRequest{IdempotencyKey: "k1"}); err != nil {
		// The trigger is tenantless, so any tenant can fire it; the session is
		// derived per tenant+key, so this is a different target, not a dedupe.
		t.Fatalf("tenantless trigger fire: %v", err)
	}
	status, ok := engine.Get("test.ping")
	if !ok || status.Fires != 2 || len(status.Audit) == 0 {
		t.Fatalf("audit: %+v", status)
	}
}

func TestTriggerRetriesAndFailure(t *testing.T) {
	starter := &countingStarter{err: kernel.ErrStoreFailure, failFor: 2}
	engine := NewTriggerEngine(TriggerEngineOptions{
		Starter: starter, Logger: testLogger(), DefaultBackoff: time.Millisecond,
	})
	defer engine.Close()
	tenant := uuid.New()
	if _, err := engine.Register(TriggerSpec{ID: "retry", Kind: TriggerWebhook, Prompt: "x", Enabled: true, MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}
	// Two failures then success: the retry budget covers it.
	if _, err := engine.Fire(context.Background(), "retry", tenant, FireRequest{IdempotencyKey: "a"}); err != nil {
		t.Fatalf("retry budget should succeed: %v", err)
	}
	if starter.count() != 3 {
		t.Fatalf("attempts = %d, want 3", starter.count())
	}
	// Now exhaust the budget.
	starter.mu.Lock()
	starter.failFor = 0
	starter.mu.Unlock()
	if _, err := engine.Fire(context.Background(), "retry", tenant, FireRequest{IdempotencyKey: "b"}); !errors.Is(err, ErrTriggerStartFailed) {
		t.Fatalf("expected start failure, got %v", err)
	}
	status, _ := engine.Get("retry")
	if status.Failures != 1 || status.LastStatus != "failed" {
		t.Fatalf("failure not visible: %+v", status)
	}
}

func TestPluginAssemblyUnloadOrderAndPending(t *testing.T) {
	var order []string
	var mu sync.Mutex
	record := func(event string) {
		mu.Lock()
		order = append(order, event)
		mu.Unlock()
	}
	hooks := NewHookBus(HookBusOptions{Logger: testLogger()})
	defer hooks.Close()
	triggers := NewTriggerEngine(TriggerEngineOptions{Starter: &countingStarter{}, Logger: testLogger()})
	defer triggers.Close()
	var toolsMu sync.Mutex
	var published []string

	provider := &Plugin{
		ID:       "provider",
		Provides: []string{"cap:db"},
		Tools: []ToolSpec{{
			Name: "provider_tool", Description: "d", Schema: json.RawMessage(`{"type":"object"}`),
			Execute: func(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
				return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("ok")}}, nil
			},
		}},
		Hooks: []HookSpec{{Event: HookTurnStarted, Handler: func(context.Context, HookPayload) error {
			record("hook")
			return nil
		}}},
		Triggers: []TriggerSpec{{ID: "provider.webhook", Kind: TriggerWebhook, Prompt: "p", Enabled: true}},
		Effects: []Effect{
			{Name: "first", Stop: func(context.Context) error { record("effect:first"); return nil }},
			{Name: "second", Stop: func(context.Context) error { record("effect:second"); return nil }},
		},
		Start: func(context.Context, *PluginEnv) error { record("start:provider"); return nil },
		Stop:  func(context.Context) error { record("stop:provider"); return nil },
	}
	consumer := &Plugin{
		ID:       "consumer",
		Requires: []string{"cap:db"},
		Start:    func(context.Context, *PluginEnv) error { record("start:consumer"); return nil },
	}
	orphan := &Plugin{
		ID:       "orphan",
		Requires: []string{"cap:missing"},
	}
	runtime := NewRuntime(RuntimeOptions{
		Plugins:  []*Plugin{consumer, provider, orphan},
		Hooks:    hooks,
		Triggers: triggers,
		Tools: func(tools []agentcore.AgentTool) error {
			toolsMu.Lock()
			published = published[:0]
			for _, tool := range tools {
				published = append(published, tool.Name())
			}
			toolsMu.Unlock()
			return nil
		},
		Logger: testLogger(),
	})
	if err := runtime.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	joined := strings.Join(order, ",")
	mu.Unlock()
	// Dependency order: the consumer waits for the provider even though it was
	// declared first.
	if joined != "start:provider,start:consumer" {
		t.Fatalf("activation order = %q", joined)
	}
	snapshot := runtime.Snapshot()
	states := map[string]PluginState{}
	for _, plugin := range snapshot.Plugins {
		states[plugin.ID] = plugin.State
	}
	if states["provider"] != PluginActive || states["consumer"] != PluginActive {
		t.Fatalf("states = %+v", states)
	}
	if states["orphan"] != PluginPending {
		t.Fatalf("missing dependency must be PENDING, got %s", states["orphan"])
	}
	if snapshot.Generation != 1 {
		t.Fatalf("generation = %d", snapshot.Generation)
	}
	toolsMu.Lock()
	if len(published) != 1 || published[0] != "provider_tool" {
		t.Fatalf("published tools = %v", published)
	}
	toolsMu.Unlock()

	hooks.Emit(HookPayload{Event: HookTurnStarted, TenantID: "t"})
	waitFor(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(strings.Join(order, ","), "hook")
	}, "plugin hook")

	// Reassembly: unload is the exact reverse (effects LIFO, then Stop), then a
	// fresh activation with generation+1.
	if err := runtime.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	joined = strings.Join(order, ",")
	mu.Unlock()
	want := "start:provider,start:consumer,hook,effect:second,effect:first,stop:provider,start:provider,start:consumer"
	if joined != want {
		t.Fatalf("unload order = %q, want %q", joined, want)
	}
	if runtime.Generation() != 2 {
		t.Fatalf("generation = %d", runtime.Generation())
	}
	runtime.Close(context.Background())
	toolsMu.Lock()
	if len(published) != 0 {
		t.Fatalf("unload must clear published tools: %v", published)
	}
	toolsMu.Unlock()
}

func TestPluginDuplicateToolFailsClosed(t *testing.T) {
	makeTool := func(name string) ToolSpec {
		return ToolSpec{Name: name, Schema: json.RawMessage(`{"type":"object"}`)}
	}
	a := &Plugin{ID: "a", Tools: []ToolSpec{makeTool("same")}}
	b := &Plugin{ID: "b", Tools: []ToolSpec{makeTool("same")}}
	runtime := NewRuntime(RuntimeOptions{
		Plugins: []*Plugin{a, b}, Logger: testLogger(),
		Tools: func([]agentcore.AgentTool) error { return nil },
	})
	if err := runtime.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	states := map[string]PluginState{}
	for _, plugin := range runtime.Snapshot().Plugins {
		states[plugin.ID] = plugin.State
	}
	if states["a"] != PluginActive || states["b"] != PluginFailed {
		t.Fatalf("states = %+v", states)
	}
}

func TestRunControlCancelSeam(t *testing.T) {
	control := NewRunControl(nil, testLogger())
	scope := kernel.Scope{TenantID: uuid.New(), SessionID: uuid.New()}
	ctx, finish := control.BeginRun(context.Background(), scope, "run-1")
	cancelled := make(chan CancelResult, 1)
	go func() {
		result, err := control.Cancel(context.Background(), scope)
		if err != nil {
			t.Errorf("cancel: %v", err)
		}
		cancelled <- result
	}()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("cancel did not cancel the run context")
	}
	time.Sleep(20 * time.Millisecond)
	finish("failed")
	select {
	case result := <-cancelled:
		if result.Status != "cancelled" || !result.Cancelled {
			t.Fatalf("cancel result: %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not observe the settlement")
	}
	result, err := control.Cancel(context.Background(), scope)
	if err != nil || result.Status != "cancelled" || !result.Cancelled {
		t.Fatalf("settled cancel: %+v err=%v", result, err)
	}
	if status, ok := control.Status(scope); !ok || status != "cancelled" {
		t.Fatalf("overlay = %q ok=%v", status, ok)
	}
	// No seam at all: the endpoint must say so instead of faking success.
	if _, err := control.Cancel(context.Background(), kernel.Scope{TenantID: uuid.New(), SessionID: uuid.New()}); !errors.Is(err, ErrCancelUnavailable) {
		t.Fatalf("unwired cancel: %v", err)
	}
}

// toolGate / toolCommitter / fixed events / done runs are the minimal deps of a
// SubagentRunner for the tool test.
type toolGate struct{ known map[uuid.UUID]bool }

func (g toolGate) CreateSession(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (g toolGate) SessionOwner(_ context.Context, _, sessionID uuid.UUID) (string, bool, error) {
	return "keel", g.known[sessionID], nil
}

type toolCommitter struct {
	mu      sync.Mutex
	commits []kernel.CommitRequest
}

func (c *toolCommitter) Commit(_ context.Context, req kernel.CommitRequest) (kernel.Receipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.commits = append(c.commits, req)
	return kernel.Receipt{Seq: int64(len(c.commits)), Kind: req.Kind}, nil
}

func (c *toolCommitter) Lookup(context.Context, kernel.Scope, kernel.KeyRef) (kernel.Receipt, bool, error) {
	return kernel.Receipt{}, false, nil
}

type doneRuns struct{}

func (doneRuns) RunState(context.Context, uuid.UUID, uuid.UUID) (RunState, error) {
	return RunState{Status: "done"}, nil
}

type childLog struct{}

func (childLog) LoadEvents(context.Context, kernel.Scope, int64, int) ([]kernel.Event, error) {
	assistant, _ := json.Marshal(map[string]any{
		"role":    "assistant",
		"content": []map[string]any{{"type": "text", "text": "child summary"}},
	})
	return []kernel.Event{
		{Seq: 1, Kind: kernel.KindUserMessage, Payload: json.RawMessage(`{"text":"do"}`)},
		{Seq: 2, Kind: kernel.KindAssistantMessage, Payload: assistant},
	}, nil
}

func (childLog) UnresolvedToolCalls(context.Context, kernel.Scope) ([]kernel.ToolCallRef, error) {
	return nil, nil
}

func TestSubagentToolRequiresRunScope(t *testing.T) {
	tenant, parent := uuid.New(), uuid.New()
	runner := NewSubagentRunner(SubagentOptions{
		Gate:   toolGate{known: map[uuid.UUID]bool{parent: true}},
		Edge:   &toolCommitter{},
		Events: childLog{},
		Runs:   doneRuns{},
		Logger: testLogger(),
		Poll:   time.Millisecond,
	})
	tool := &SubagentTool{Runner: runner}
	// Without the ambient scope the tool must fail with an actionable message,
	// never invent a parent session.
	result, err := tool.Execute(context.Background(), "call-1", json.RawMessage(`{"task":"do"}`), nil)
	if err != nil || result.IsError == nil || !*result.IsError {
		t.Fatalf("missing scope result = %+v err=%v", result, err)
	}
	result, err = tool.Execute(WithRunScope(context.Background(), RunScope{TenantID: tenant, SessionID: parent}), "call-2",
		json.RawMessage(`{"task":"do"}`), nil)
	if err != nil || (result.IsError != nil && *result.IsError) {
		t.Fatalf("scoped tool result = %+v err=%v", result, err)
	}
	structured, ok := result.StructuredContent.(SubagentResult)
	if !ok || structured.Status != "done" || structured.Summary != "child summary" {
		t.Fatalf("structured result = %#v", result.StructuredContent)
	}
	if structured.ParentSessionID != parent.String() {
		t.Fatalf("parent = %q", structured.ParentSessionID)
	}

	// The builtin plugin publishes the same tool to the model.
	hooks := NewHookBus(HookBusOptions{Logger: testLogger()})
	defer hooks.Close()
	var published []string
	plugins := NewRuntime(RuntimeOptions{
		Plugins: []*Plugin{NewSubagentPlugin(runner)},
		Hooks:   hooks,
		Tools: func(tools []agentcore.AgentTool) error {
			published = published[:0]
			for _, tool := range tools {
				published = append(published, tool.Name())
			}
			return nil
		},
		Logger: testLogger(),
	})
	if err := plugins.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer plugins.Close(context.Background())
	if len(published) != 1 || published[0] != "spawn_subagent" {
		t.Fatalf("published tools = %v", published)
	}
}

func TestRunControlInstallsRunScope(t *testing.T) {
	control := NewRunControl(nil, testLogger())
	scope := kernel.Scope{TenantID: uuid.New(), SessionID: uuid.New()}
	ctx, finish := control.BeginRun(context.Background(), scope, "run-42")
	defer finish("done")
	runScope, ok := RunScopeFromContext(ctx)
	if !ok || runScope.TenantID != scope.TenantID || runScope.SessionID != scope.SessionID || runScope.RunID != "run-42" {
		t.Fatalf("run scope = %+v ok=%v", runScope, ok)
	}
	if _, ok := RunScopeFromContext(context.Background()); ok {
		t.Fatal("a plain context must not carry a run scope")
	}
}

func TestStreamTeePublishesDeltasAndForwardsEvents(t *testing.T) {
	hub := NewPartialHub()
	tenant, session := uuid.New(), uuid.New()
	ch, cancel := hub.Subscribe(tenant, session)
	defer cancel()
	text := func(value string) agentcore.AssistantMessage {
		return agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant,
			Content: agentcore.ContentList{agentcore.NewTextContent(value)}}
	}
	base := func(ctx context.Context, _ string, _ provider.LlmContext, _ provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		stream := provider.NewAssistantMessageEventStream(4)
		go func() {
			defer stream.Close()
			_ = stream.Emit(ctx, provider.StreamStartEvent{Partial: text("")})
			for _, partial := range []string{"Hel", "Hello", "Hello world"} {
				_ = stream.Emit(ctx, provider.StreamTextEvent{Partial: text(partial)})
			}
			_ = stream.Emit(ctx, provider.StreamDoneEvent{Message: text("Hello world")})
		}()
		return stream, nil
	}
	wrapped := StreamTee(base, hub)
	ctx := WithRunScope(context.Background(), RunScope{TenantID: tenant, SessionID: session})
	stream, err := wrapped(ctx, "model", provider.LlmContext{}, provider.StreamConfig{})
	if err != nil {
		t.Fatal(err)
	}
	textEvents := 0
	for event := range stream.Events() {
		if _, ok := event.(provider.StreamTextEvent); ok {
			textEvents++
		}
	}
	if textEvents != 3 {
		t.Fatalf("the tee changed the event stream: %d text events", textEvents)
	}
	result, err := stream.Result(ctx)
	if err != nil || messageText(result) != "Hello world" {
		t.Fatalf("terminal result = %q err=%v", messageText(result), err)
	}
	var deltas []string
	for {
		select {
		case frame := <-ch:
			deltas = append(deltas, frame.Delta)
			continue
		default:
		}
		break
	}
	if strings.Join(deltas, "|") != "Hel|lo| world" {
		t.Fatalf("deltas = %v", deltas)
	}

	// Without an ambient run scope nothing is published (and the stream still
	// works).
	plain, err := wrapped(context.Background(), "model", provider.LlmContext{}, provider.StreamConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for range plain.Events() {
	}
	if len(ch) != 0 {
		t.Fatalf("frames published without a run scope: %d", len(ch))
	}
}

func TestPartialHubDropsSlowSubscribers(t *testing.T) {
	hub := NewPartialHub()
	tenant, session := uuid.New(), uuid.New()
	ch, cancel := hub.Subscribe(tenant, session)
	defer cancel()
	if !hub.Publish(tenant, session, Partial{Type: "assistant.delta", Delta: "a"}) {
		t.Fatal("a live subscriber must receive the frame")
	}
	frame := <-ch
	if frame.Type != "assistant.delta" || frame.Delta != "a" || frame.At.IsZero() {
		t.Fatalf("frame = %+v", frame)
	}
	for i := 0; i < partialBuffer+8; i++ {
		hub.Publish(tenant, session, Partial{Type: "assistant.delta", Delta: "x"})
	}
	published, dropped, subscribers := hub.Stats()
	if published == 0 || dropped == 0 || subscribers != 1 {
		t.Fatalf("stats published=%d dropped=%d subscribers=%d", published, dropped, subscribers)
	}
}
