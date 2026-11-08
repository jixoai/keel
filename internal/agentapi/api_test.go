package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/kernel"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

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
	if tenant != uuid.Nil {
		req.Header.Set("X-Tenant-ID", tenant.String())
	}
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

// fakeLister answers keyset pages over a fixed slice, using the real cursor
// encoding so the server's pagination loop is exercised.
type fakeLister struct {
	sessions []SessionSummary
	tenant   uuid.UUID
	limits   []int
}

func (f *fakeLister) ListSessionSummaries(_ context.Context, tenantID uuid.UUID, limit int, after string) (SessionPage, error) {
	f.tenant = tenantID
	f.limits = append(f.limits, limit)
	start := 0
	if after != "" {
		cursor, err := decodeSessionCursor(after)
		if err != nil {
			return SessionPage{}, err
		}
		for i, summary := range f.sessions {
			if summary.SessionID == cursor.sessionID.String() {
				start = i + 1
				break
			}
		}
	}
	end := start + limit
	if end > len(f.sessions) {
		end = len(f.sessions)
	}
	page := SessionPage{Sessions: append([]SessionSummary(nil), f.sessions[start:end]...)}
	if end < len(f.sessions) {
		page.HasMore = true
		last := f.sessions[end-1]
		id, err := uuid.Parse(last.SessionID)
		if err != nil {
			return SessionPage{}, err
		}
		page.NextCursor = encodeSessionCursor(last.UpdatedAt, id)
	}
	return page, nil
}

func TestCapabilities(t *testing.T) {
	broker := agentext.NewDecisionBroker(agentext.DecisionBrokerOptions{Store: agentext.NewMemoryDecisionStore(), Logger: testLogger()})
	defer broker.Close()
	server := httptest.NewServer(New(Config{
		Sessions: &fakeLister{},
		Cancel: func(context.Context, uuid.UUID, uuid.UUID) (agentext.CancelResult, error) {
			return agentext.CancelResult{}, nil
		},
		Decisions: broker,
		Partials:  agentext.NewPartialHub(),
		Plugins:   agentext.NewRuntime(agentext.RuntimeOptions{Logger: testLogger()}),
		Triggers:  agentext.NewTriggerEngine(agentext.TriggerEngineOptions{Logger: testLogger()}),
		Subagents: nil,
		Logger:    testLogger(),
	}))
	defer server.Close()
	var body Capabilities
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/capabilities", uuid.Nil, nil, &body); code != 200 {
		t.Fatalf("capabilities: %d", code)
	}
	if body.APIVersion != "v1" || body.ServiceVersion == "" {
		t.Fatalf("capabilities = %+v", body)
	}
	for _, feature := range []string{"sessions.list", "sessions.cancel", "decisions", "partials", "plugins", "triggers", "hooks"} {
		if !body.Features[feature] {
			t.Fatalf("feature %s reported false: %+v", feature, body.Features)
		}
	}
	if body.Features["subagents"] {
		t.Fatal("subagents must be false when the runner is not wired")
	}
	if len(body.Hooks) != len(agentext.HookEvents()) {
		t.Fatalf("hooks = %v", body.Hooks)
	}
}

func TestSessionListPaginationAndIsolation(t *testing.T) {
	base := time.Now().UTC().Add(-10 * time.Minute)
	lister := &fakeLister{}
	for i := 0; i < 120; i++ {
		lister.sessions = append(lister.sessions, SessionSummary{
			SessionID: uuid.NewString(),
			CreatedAt: base,
			UpdatedAt: base.Add(time.Duration(120-i) * time.Second),
			LastSeq:   int64(i),
			RunStatus: "done",
		})
	}
	server := httptest.NewServer(New(Config{Sessions: lister, Logger: testLogger()}))
	defer server.Close()
	tenant := uuid.New()
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		url := server.URL + "/v1/sessions?limit=50"
		if cursor != "" {
			url += "&after=" + cursor
		}
		var page struct {
			Sessions   []SessionSummary `json:"sessions"`
			NextCursor string           `json:"nextCursor"`
			HasMore    bool             `json:"hasMore"`
		}
		if code := doJSON(t, server.Client(), http.MethodGet, url, tenant, nil, &page); code != 200 {
			t.Fatalf("page %d: %d", pages, code)
		}
		pages++
		for _, summary := range page.Sessions {
			if seen[summary.SessionID] {
				t.Fatalf("duplicate session %s across pages", summary.SessionID)
			}
			seen[summary.SessionID] = true
			if summary.RunStatus == "" {
				t.Fatalf("runStatus must be filled: %+v", summary)
			}
		}
		if !page.HasMore {
			if page.NextCursor != "" {
				t.Fatalf("last page carries a cursor: %q", page.NextCursor)
			}
			break
		}
		cursor = page.NextCursor
		if cursor == "" {
			t.Fatal("hasMore without a cursor")
		}
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if pages != 3 || len(seen) != 120 {
		t.Fatalf("pages=%d sessions=%d", pages, len(seen))
	}
	if lister.tenant != tenant {
		t.Fatalf("lister tenant = %s", lister.tenant)
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/sessions?after=not-a-cursor", tenant, nil, nil); code != 400 {
		t.Fatalf("bad cursor: %d", code)
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/sessions", uuid.Nil, nil, nil); code != 401 {
		t.Fatalf("missing tenant: %d", code)
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/sessions?limit=0", tenant, nil, nil); code != 400 {
		t.Fatalf("zero limit: %d", code)
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/sessions?limit=1000", tenant, nil, nil); code != 400 {
		t.Fatalf("oversized limit: %d", code)
	}
}

func TestCancelIdempotencyAndErrorMapping(t *testing.T) {
	type step struct {
		result agentext.CancelResult
		err    error
	}
	queue := []step{
		{agentext.CancelResult{Status: "cancelled", RunStatus: "cancelled", Cancelled: true}, nil},
		{agentext.CancelResult{Status: "settled", RunStatus: "done"}, nil},
		{agentext.CancelResult{Status: "idle"}, nil},
		{agentext.CancelResult{Status: "not_wired", RunStatus: "running"}, agentext.ErrCancelUnavailable},
		{agentext.CancelResult{}, ErrSessionNotFound},
		{agentext.CancelResult{Status: "idle", RunStatus: "external"}, ErrExternalSession},
	}
	var mu sync.Mutex
	cancel := func(context.Context, uuid.UUID, uuid.UUID) (agentext.CancelResult, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(queue) == 0 {
			return agentext.CancelResult{Status: "idle"}, nil
		}
		next := queue[0]
		queue = queue[1:]
		return next.result, next.err
	}
	broker := agentext.NewDecisionBroker(agentext.DecisionBrokerOptions{Store: agentext.NewMemoryDecisionStore(), Logger: testLogger()})
	defer broker.Close()
	server := httptest.NewServer(New(Config{Cancel: cancel, Decisions: broker, Logger: testLogger()}))
	defer server.Close()
	tenant, session := uuid.New(), uuid.New()
	cancelURL := server.URL + "/v1/sessions/" + session.String() + "/cancel"

	var first struct {
		Status             string `json:"status"`
		Cancelled          bool   `json:"cancelled"`
		DecisionsCancelled int    `json:"decisionsCancelled"`
	}
	if code := doJSON(t, server.Client(), http.MethodPost, cancelURL, tenant, nil, &first); code != 200 {
		t.Fatalf("cancel: %d", code)
	}
	if first.Status != "cancelled" || !first.Cancelled {
		t.Fatalf("cancel body = %+v", first)
	}
	var second struct {
		Status    string `json:"status"`
		Cancelled bool   `json:"cancelled"`
	}
	if code := doJSON(t, server.Client(), http.MethodPost, cancelURL, tenant, nil, &second); code != 200 {
		t.Fatalf("repeat cancel: %d", code)
	}
	if second.Status != "settled" || second.Cancelled {
		t.Fatalf("repeat cancel body = %+v", second)
	}
	if code := doJSON(t, server.Client(), http.MethodPost, cancelURL, tenant, nil, nil); code != 200 {
		t.Fatalf("idle cancel: %d", code)
	}
	var notWired map[string]any
	if code := doJSON(t, server.Client(), http.MethodPost, cancelURL, tenant, nil, &notWired); code != 501 {
		t.Fatalf("not wired: %d %v", code, notWired)
	}
	if notWired["status"] != "not_wired" {
		t.Fatalf("not wired body = %v", notWired)
	}
	if code := doJSON(t, server.Client(), http.MethodPost, cancelURL, tenant, nil, nil); code != 404 {
		t.Fatalf("unknown session: %d", code)
	}
	if code := doJSON(t, server.Client(), http.MethodPost, cancelURL, tenant, nil, nil); code != 409 {
		t.Fatalf("external session: %d", code)
	}
}

func TestCancelUnwiredAnswers501(t *testing.T) {
	server := httptest.NewServer(New(Config{Logger: testLogger()}))
	defer server.Close()
	var body map[string]any
	if code := doJSON(t, server.Client(), http.MethodPost, server.URL+"/v1/sessions/"+uuid.NewString()+"/cancel", uuid.New(), nil, &body); code != 501 {
		t.Fatalf("unwired cancel: %d %v", code, body)
	}
	if body["status"] != "not_wired" {
		t.Fatalf("body = %v", body)
	}
}

func TestDecisionHTTPIdempotency(t *testing.T) {
	broker := agentext.NewDecisionBroker(agentext.DecisionBrokerOptions{Store: agentext.NewMemoryDecisionStore(), Logger: testLogger()})
	defer broker.Close()
	server := httptest.NewServer(New(Config{Decisions: broker, Logger: testLogger()}))
	defer server.Close()
	tenant, session := uuid.New(), uuid.New()
	openURL := server.URL + "/v1/sessions/" + session.String() + "/decisions"

	var opened struct {
		Decision agentext.Decision `json:"decision"`
		Created  bool              `json:"created"`
	}
	if code := doJSON(t, server.Client(), http.MethodPost, openURL, tenant, map[string]any{
		"requestId": "approval-1",
		"prompt":    "Approve?",
		"options":   []map[string]string{{"id": "yes"}, {"id": "no"}},
	}, &opened); code != 201 || !opened.Created {
		t.Fatalf("open: %d %+v", code, opened)
	}
	decisionID := opened.Decision.DecisionID
	if decisionID == "" || opened.Decision.Status != agentext.DecisionOpen {
		t.Fatalf("decision = %+v", opened.Decision)
	}
	var reopened struct {
		Decision agentext.Decision `json:"decision"`
		Created  bool              `json:"created"`
	}
	if code := doJSON(t, server.Client(), http.MethodPost, openURL, tenant, map[string]any{
		"requestId": "approval-1", "prompt": "Approve?",
	}, &reopened); code != 200 || reopened.Created || reopened.Decision.DecisionID != decisionID {
		t.Fatalf("idempotent open: %d %+v", code, reopened)
	}
	answerURL := openURL + "/" + decisionID
	var answer struct {
		Duplicate bool              `json:"duplicate"`
		Status    string            `json:"status"`
		Decision  agentext.Decision `json:"decision"`
	}
	if code := doJSON(t, server.Client(), http.MethodPost, answerURL, tenant, map[string]string{"optionId": "yes"}, &answer); code != 200 || answer.Duplicate {
		t.Fatalf("answer: %d %+v", code, answer)
	}
	if code := doJSON(t, server.Client(), http.MethodPost, answerURL, tenant, map[string]string{"optionId": "yes"}, &answer); code != 200 || !answer.Duplicate {
		t.Fatalf("repeat answer: %d %+v", code, answer)
	}
	if len(answer.Decision.Events) != 2 {
		t.Fatalf("journal must hold opened+answered once: %+v", answer.Decision.Events)
	}
	if code := doJSON(t, server.Client(), http.MethodPost, answerURL, tenant, map[string]string{"optionId": "no"}, nil); code != 409 {
		t.Fatalf("conflicting answer: %d", code)
	}
	if code := doJSON(t, server.Client(), http.MethodPost, answerURL, uuid.New(), map[string]string{"optionId": "yes"}, nil); code != 404 {
		t.Fatalf("foreign tenant answer: %d", code)
	}
	var strict struct {
		Decision agentext.Decision `json:"decision"`
	}
	if code := doJSON(t, server.Client(), http.MethodPost, openURL, tenant, map[string]any{
		"requestId": "strict-1", "prompt": "pick", "options": []map[string]string{{"id": "yes"}},
	}, &strict); code != 201 {
		t.Fatalf("open option-only decision: %d", code)
	}
	if code := doJSON(t, server.Client(), http.MethodPost, openURL+"/"+strict.Decision.DecisionID, tenant,
		map[string]string{"text": "free text"}, nil); code != 400 {
		t.Fatalf("free text without allowFreeText: %d", code)
	}
	if code := doJSON(t, server.Client(), http.MethodPost, openURL+"/"+strict.Decision.DecisionID, tenant,
		map[string]string{"optionId": "maybe"}, nil); code != 400 {
		t.Fatalf("unknown option: %d", code)
	}
	var list struct {
		Decisions []agentext.Decision `json:"decisions"`
	}
	if code := doJSON(t, server.Client(), http.MethodGet, openURL, tenant, nil, &list); code != 200 || len(list.Decisions) != 2 {
		t.Fatalf("list: %d %+v", code, list)
	}
	if code := doJSON(t, server.Client(), http.MethodGet, openURL+"/"+decisionID, tenant, nil, nil); code != 200 {
		t.Fatalf("get decision: %d", code)
	}
	if code := doJSON(t, server.Client(), http.MethodPost, answerURL, tenant, map[string]string{"optionId": "maybe"}, nil); code != 409 {
		t.Fatalf("answer after terminal: %d", code)
	}
	if code := doJSON(t, server.Client(), http.MethodPost, openURL+"/not-a-uuid", tenant, map[string]string{"optionId": "yes"}, nil); code != 400 {
		t.Fatalf("bad decision id: %d", code)
	}
}

// countingStarter counts trigger starts.
type countingStarter struct {
	mu     sync.Mutex
	counts int
}

func (s *countingStarter) StartTurn(_ context.Context, req agentext.StartTurnRequest) (agentext.StartTurnResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts++
	return agentext.StartTurnResult{TenantID: req.TenantID, SessionID: req.SessionID, Seq: int64(s.counts), RequestID: req.RequestID}, nil
}

func TestExtensionRoutes(t *testing.T) {
	starter := &countingStarter{}
	hooks := agentext.NewHookBus(agentext.HookBusOptions{Logger: testLogger()})
	defer hooks.Close()
	triggers := agentext.NewTriggerEngine(agentext.TriggerEngineOptions{Starter: starter, Hooks: hooks, Logger: testLogger()})
	defer triggers.Close()
	plugin := &agentext.Plugin{
		ID:       "test.plugin",
		Version:  "1.0.0",
		Provides: []string{"cap:test"},
		Tools: []agentext.ToolSpec{{
			Name:   "test_tool",
			Schema: json.RawMessage(`{"type":"object"}`),
		}},
		Hooks:    []agentext.HookSpec{{Event: agentext.HookTurnStarted, Handler: func(context.Context, agentext.HookPayload) error { return nil }}},
		Triggers: []agentext.TriggerSpec{{ID: "test.webhook", Kind: agentext.TriggerWebhook, Prompt: "go", Enabled: true}},
	}
	plugins := agentext.NewRuntime(agentext.RuntimeOptions{
		Plugins: []*agentext.Plugin{plugin}, Hooks: hooks, Triggers: triggers, Logger: testLogger(),
	})
	if err := plugins.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer plugins.Close(context.Background())
	server := httptest.NewServer(New(Config{
		Plugins: plugins, Triggers: triggers, Hooks: hooks, Logger: testLogger(),
	}))
	defer server.Close()
	tenant := uuid.New()

	var pluginsBody struct {
		Generation int64 `json:"generation"`
		Plugins    []struct {
			ID    string   `json:"id"`
			State string   `json:"state"`
			Tools []string `json:"tools"`
		} `json:"plugins"`
		HookStats []agentext.HookStat `json:"hookStats"`
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/plugins", tenant, nil, &pluginsBody); code != 200 {
		t.Fatalf("plugins: %d", code)
	}
	if pluginsBody.Generation != 1 || len(pluginsBody.Plugins) != 1 || pluginsBody.Plugins[0].State != "active" {
		t.Fatalf("plugins body = %+v", pluginsBody)
	}
	if len(pluginsBody.HookStats) != 1 || pluginsBody.HookStats[0].Event != agentext.HookTurnStarted {
		t.Fatalf("hook stats = %+v", pluginsBody.HookStats)
	}
	var triggersBody struct {
		Triggers []agentext.TriggerStatus `json:"triggers"`
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/triggers", tenant, nil, &triggersBody); code != 200 || len(triggersBody.Triggers) != 1 {
		t.Fatalf("triggers: %d %+v", code, triggersBody)
	}
	var fireBody struct {
		Result agentext.FireResult `json:"result"`
	}
	if code := doJSON(t, server.Client(), http.MethodPost, server.URL+"/v1/triggers/test.webhook/fire", tenant,
		map[string]any{"idempotencyKey": "abc"}, &fireBody); code != 200 || fireBody.Result.Status != "fired" {
		t.Fatalf("fire: %d %+v", code, fireBody)
	}
	if code := doJSON(t, server.Client(), http.MethodPost, server.URL+"/v1/triggers/test.webhook/fire", tenant,
		map[string]any{"idempotencyKey": "abc"}, &fireBody); code != 200 || !fireBody.Result.Deduped {
		t.Fatalf("dedupe: %d %+v", code, fireBody)
	}
	if starter.counts != 1 {
		t.Fatalf("starts = %d", starter.counts)
	}
	if code := doJSON(t, server.Client(), http.MethodPost, server.URL+"/v1/triggers/missing/fire", tenant,
		map[string]any{"idempotencyKey": "x"}, nil); code != 404 {
		t.Fatalf("unknown trigger: %d", code)
	}
	if code := doJSON(t, server.Client(), http.MethodPost, server.URL+"/v1/triggers/test.webhook/fire", tenant,
		map[string]any{"reason": "webhook"}, nil); code != 400 {
		t.Fatalf("webhook without key: %d", code)
	}
}

// fakeGate is an in-memory session gate.
type fakeGate struct {
	mu       sync.Mutex
	sessions map[uuid.UUID]bool
}

func newFakeGate(sessions ...uuid.UUID) *fakeGate {
	gate := &fakeGate{sessions: make(map[uuid.UUID]bool)}
	for _, session := range sessions {
		gate.sessions[session] = true
	}
	return gate
}

func (g *fakeGate) CreateSession(_ context.Context, _, sessionID uuid.UUID) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sessions[sessionID] = true
	return nil
}

func (g *fakeGate) SessionOwner(_ context.Context, _, sessionID uuid.UUID) (string, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.sessions[sessionID] {
		return "", false, nil
	}
	return "keel", true, nil
}

// fakeCommitter records commits and answers a receipt.
type fakeCommitter struct {
	mu      sync.Mutex
	commits []kernel.CommitRequest
	err     error
}

func (c *fakeCommitter) Commit(_ context.Context, req kernel.CommitRequest) (kernel.Receipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return kernel.Receipt{}, c.err
	}
	c.commits = append(c.commits, req)
	return kernel.Receipt{Seq: int64(len(c.commits)), Kind: req.Kind}, nil
}

func (c *fakeCommitter) Lookup(context.Context, kernel.Scope, kernel.KeyRef) (kernel.Receipt, bool, error) {
	return kernel.Receipt{}, false, nil
}

// fakeRuns answers a fixed run state.
type fakeRuns struct {
	mu    sync.Mutex
	state agentext.RunState
	err   error
}

func (r *fakeRuns) RunState(context.Context, uuid.UUID, uuid.UUID) (agentext.RunState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state, r.err
}

// fakeEvents answers a fixed log.
type fakeEvents struct{ events []kernel.Event }

func (f *fakeEvents) LoadEvents(context.Context, kernel.Scope, int64, int) ([]kernel.Event, error) {
	return f.events, nil
}

func (f *fakeEvents) UnresolvedToolCalls(context.Context, kernel.Scope) ([]kernel.ToolCallRef, error) {
	return nil, nil
}

func TestSubagentRoutes(t *testing.T) {
	tenant, parent := uuid.New(), uuid.New()
	gate := newFakeGate(parent)
	committer := &fakeCommitter{}
	runs := &fakeRuns{state: agentext.RunState{Status: "done"}}
	assistant, _ := json.Marshal(map[string]any{
		"role":    "assistant",
		"content": []map[string]any{{"type": "text", "text": "child summary"}},
	})
	events := &fakeEvents{events: []kernel.Event{
		{Seq: 1, Kind: kernel.KindUserMessage, Payload: json.RawMessage(`{"text":"delegate"}`)},
		{Seq: 2, Kind: kernel.KindAssistantMessage, Payload: assistant},
	}}
	runner := agentext.NewSubagentRunner(agentext.SubagentOptions{
		Gate: gate, Edge: committer, Events: events, Runs: runs, Logger: testLogger(), Poll: time.Millisecond,
	})
	server := httptest.NewServer(New(Config{Subagents: runner, Logger: testLogger()}))
	defer server.Close()
	spawnURL := server.URL + "/v1/sessions/" + parent.String() + "/subagents"

	var async struct {
		Subagent agentext.SubagentRecord `json:"subagent"`
	}
	if code := doJSON(t, server.Client(), http.MethodPost, spawnURL, tenant, map[string]any{"task": "delegate"}, &async); code != 202 {
		t.Fatalf("async spawn: %d", code)
	}
	if async.Subagent.SessionID == "" || async.Subagent.ParentSessionID != parent.String() {
		t.Fatalf("record = %+v", async.Subagent)
	}
	committer.mu.Lock()
	if len(committer.commits) != 1 || committer.commits[0].Kind != kernel.KindUserMessage {
		t.Fatalf("commits = %+v", committer.commits)
	}
	committer.mu.Unlock()

	var sync struct {
		Subagent agentext.SubagentRecord `json:"subagent"`
		Result   agentext.SubagentResult `json:"result"`
	}
	if code := doJSON(t, server.Client(), http.MethodPost, spawnURL, tenant, map[string]any{"task": "delegate", "wait": true}, &sync); code != 200 {
		t.Fatalf("sync spawn: %d", code)
	}
	if sync.Result.Status != "done" || sync.Result.Summary != "child summary" {
		t.Fatalf("result = %+v", sync.Result)
	}
	var list struct {
		Subagents []agentext.SubagentRecord `json:"subagents"`
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/subagents?parentSessionId="+parent.String(), tenant, nil, &list); code != 200 || len(list.Subagents) != 2 {
		t.Fatalf("list: %d %+v", code, list)
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/subagents/"+sync.Result.SessionID, tenant, nil, nil); code != 200 {
		t.Fatalf("get: %d", code)
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/subagents/"+sync.Result.SessionID, uuid.New(), nil, nil); code != 404 {
		t.Fatalf("foreign tenant get: %d", code)
	}
	if code := doJSON(t, server.Client(), http.MethodPost, server.URL+"/v1/sessions/"+uuid.NewString()+"/subagents", tenant, map[string]any{"task": "x"}, nil); code != 404 {
		t.Fatalf("unknown parent: %d", code)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	id := uuid.New()
	at := time.Now().UTC().Truncate(time.Microsecond)
	cursor := encodeSessionCursor(at, id)
	decoded, err := decodeSessionCursor(cursor)
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.updatedAt.Equal(at) || decoded.sessionID != id {
		t.Fatalf("round trip: %+v", decoded)
	}
	if _, err := decodeSessionCursor("!!!"); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("bad cursor error: %v", err)
	}
}
