package agentapi

// P5.5: POST /v1/mcp/apps/{appId}/context — the ui/update-model-context
// persistence route. An unwired deployment answers -32601 and reports the
// capability false; a wired one upserts and audits identifiers only.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/store"
)

type fakeContextStore struct {
	mu      sync.Mutex
	entries []store.MCPAppContext
	err     error
	// started/block let a test hold writes in flight and exercise the shared
	// bridge concurrency gate.
	started chan struct{}
	block   chan struct{}
}

func (f *fakeContextStore) UpsertMCPAppContext(_ context.Context, entry store.MCPAppContext) error {
	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.entries = append(f.entries, entry)
	return nil
}

func (f *fakeContextStore) snapshot() []store.MCPAppContext {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.MCPAppContext(nil), f.entries...)
}

func TestMCPAppContextRouteUnwiredAnswersMethodNotFound(t *testing.T) {
	tenant := uuid.New()
	server := testAgentAPI(t, Config{
		MCPApps: &fakeMCPApps{tenant: tenant, published: true, surface: testSurface(t)},
	})
	status, body := postBridge(t, server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/context", tenant, map[string]any{
		"sessionId": uuid.NewString(),
		"content":   []any{map[string]any{"type": "text", "text": "viewing"}},
	})
	if status != http.StatusNotImplemented {
		t.Fatalf("status = %d body=%s, want 501", status, body)
	}
	var rpc struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil || rpc.Error.Code != -32601 {
		t.Fatalf("body = %s, want a -32601 error", body)
	}
	// The capability must be false, or the View would send a method this
	// deployment refuses.
	capabilities := rawGet(t, server, "/v1/capabilities", tenant)
	defer capabilities.Body.Close()
	var document struct {
		Features map[string]bool `json:"features"`
	}
	if err := json.NewDecoder(capabilities.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	if document.Features["mcp.apps.context"] {
		t.Fatal("mcp.apps.context is true without a context store")
	}
}

func TestMCPAppContextRouteUsesTheBridgeGate(t *testing.T) {
	tenant := uuid.New()
	block := make(chan struct{})
	started := make(chan struct{}, mcpAppConcurrency)
	contexts := &fakeContextStore{started: started, block: block}
	audit := &fakeAppAudit{}
	api := New(Config{
		MCPApps:        &fakeMCPApps{tenant: tenant, published: true, surface: testSurface(t)},
		MCPAppContexts: contexts,
		MCPAppAudit:    audit,
	})
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	url := server.URL + "/v1/mcp/apps/" + testAppID + "/context"
	body := map[string]any{
		"sessionId": uuid.NewString(),
		"content":   []any{map[string]any{"type": "text", "text": "viewing"}},
	}

	// Fill every in-flight slot with a write the store holds open.
	results := make([]<-chan bridgeResult, 0, mcpAppConcurrency)
	for index := 0; index < mcpAppConcurrency; index++ {
		results = append(results, postBridgeAsync(server.Client(), url, tenant, body))
	}
	for index := 0; index < mcpAppConcurrency; index++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d context writes started", index)
		}
	}

	// The context route is a bridge action: the fifth request must be refused
	// by the shared gate instead of reaching the store.
	status, response := postBridge(t, server.Client(), url, tenant, body)
	if status != http.StatusTooManyRequests {
		t.Fatalf("fifth context request = %d, want 429: %s", status, response)
	}
	if entries := contexts.snapshot(); len(entries) != 0 {
		t.Fatalf("a gated request reached the store: %d rows", len(entries))
	}

	close(block)
	for _, done := range results {
		if result := <-done; result.status != http.StatusOK {
			t.Fatalf("in-flight context request = %d: %s", result.status, result.body)
		}
	}
	api.flushMCPAppAuditSamples()
	refused := false
	for _, entry := range audit.snapshot() {
		if entry.Action == "bridge.concurrency" && entry.Result == "refused" {
			refused = true
		}
	}
	if !refused {
		t.Fatal("the context gate refusal was not audited")
	}
}

func TestMCPAppContextRoutePersistsOverwrite(t *testing.T) {
	tenant := uuid.New()
	contexts := &fakeContextStore{}
	server := testAgentAPI(t, Config{
		MCPApps:        &fakeMCPApps{tenant: tenant, published: true, surface: testSurface(t)},
		MCPAppContexts: contexts,
	})
	sessionID := uuid.New()
	status, body := postBridge(t, server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/context", tenant, map[string]any{
		"sessionId":         sessionID.String(),
		"content":           []any{map[string]any{"type": "text", "text": "viewing invoice 42"}},
		"structuredContent": map[string]any{"invoiceId": 42},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%s", status, body)
	}
	if !strings.Contains(string(body), `"result"`) {
		t.Fatalf("body = %s, want a JSON-RPC result", body)
	}
	entries := contexts.snapshot()
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	entry := entries[0]
	if entry.TenantID != tenant || entry.SessionID != sessionID || entry.AppID != testAppID {
		t.Fatalf("entry identity = %+v", entry)
	}
	if !strings.Contains(string(entry.Content), "viewing invoice 42") {
		t.Fatalf("content = %s", entry.Content)
	}
	if !strings.Contains(string(entry.Structured), `"invoiceId":42`) {
		t.Fatalf("structured = %s", entry.Structured)
	}

	// capability true once the store is wired.
	capabilities := rawGet(t, server, "/v1/capabilities", tenant)
	defer capabilities.Body.Close()
	var document struct {
		Features map[string]bool `json:"features"`
	}
	if err := json.NewDecoder(capabilities.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	if !document.Features["mcp.apps.context"] {
		t.Fatal("mcp.apps.context is false with a wired context store")
	}
}

func TestMCPAppContextRouteValidatesInput(t *testing.T) {
	tenant := uuid.New()
	contexts := &fakeContextStore{}
	server := testAgentAPI(t, Config{
		MCPApps:        &fakeMCPApps{tenant: tenant, published: true, surface: testSurface(t)},
		MCPAppContexts: contexts,
	})
	cases := []struct {
		name   string
		body   map[string]any
		status int
	}{
		{"missing session", map[string]any{"content": []any{map[string]any{"type": "text", "text": "x"}}}, http.StatusBadRequest},
		{"missing content", map[string]any{"sessionId": uuid.NewString()}, http.StatusBadRequest},
		{"unknown app", map[string]any{"sessionId": uuid.NewString()}, http.StatusNotFound},
	}
	for _, test := range cases {
		url := server.URL + "/v1/mcp/apps/" + testAppID + "/context"
		if test.name == "unknown app" {
			url = server.URL + "/v1/mcp/apps/mcpapp_ffffffffffffffff/context"
		}
		status, body := postBridge(t, server.Client(), url, tenant, test.body)
		if status != test.status {
			t.Fatalf("%s: status = %d body=%s, want %d", test.name, status, body, test.status)
		}
	}
	if entries := contexts.snapshot(); len(entries) != 0 {
		t.Fatalf("invalid requests wrote %d rows", len(entries))
	}
}

func TestMCPAppContextRouteForeignTenantIsNotFound(t *testing.T) {
	owner := uuid.New()
	contexts := &fakeContextStore{}
	server := testAgentAPI(t, Config{
		MCPApps:        &fakeMCPApps{tenant: owner, published: true, surface: testSurface(t)},
		MCPAppContexts: contexts,
	})
	status, _ := postBridge(t, server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/context", uuid.New(), map[string]any{
		"sessionId": uuid.NewString(),
		"content":   []any{map[string]any{"type": "text", "text": "x"}},
	})
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}

func TestMCPAppContextRouteStoreFailureIsNotSuccess(t *testing.T) {
	tenant := uuid.New()
	contexts := &fakeContextStore{err: context.DeadlineExceeded}
	server := testAgentAPI(t, Config{
		MCPApps:        &fakeMCPApps{tenant: tenant, published: true, surface: testSurface(t)},
		MCPAppContexts: contexts,
	})
	status, body := postBridge(t, server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/context", tenant, map[string]any{
		"sessionId": uuid.NewString(),
		"content":   []any{map[string]any{"type": "text", "text": "x"}},
	})
	if status == http.StatusOK {
		t.Fatalf("a failed store write answered 200: %s", body)
	}
}

func TestMCPAppCallContextStampsSessionScope(t *testing.T) {
	tenant, session := uuid.New(), uuid.New()
	ctx := mcpAppCallContext(context.Background(), tenant, session, "call-7")
	scope, ok := agentcore.ToolRunScopeFromContext(ctx)
	if !ok || scope.TenantID != tenant.String() || scope.SessionID != session.String() {
		t.Fatalf("scope = %+v ok=%v", scope, ok)
	}
	if got := agentcore.ToolCallIDFromContext(ctx); got != "call-7" {
		t.Fatalf("tool call id = %q", got)
	}
	if untouched := mcpAppCallContext(context.Background(), tenant, uuid.Nil, "call-8"); untouched != context.Background() {
		if _, ok := agentcore.ToolRunScopeFromContext(untouched); ok {
			t.Fatal("a session-less call was stamped with a scope")
		}
	}
}

var _ MCPAppContextStore = (*fakeContextStore)(nil)
