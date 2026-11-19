package agentapi

// The MCP Apps bridge contract (P3.1–P3.4/P3.7/P3.8): ownership, destructive
// confirmation, the three ui/message branches, the link gate, the resource
// scheme whitelist, the audit trail (including the no-body assertion) and the
// per-app budget.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/mcp"
	"github.com/jixoai/keel/internal/mcphost"
	"github.com/jixoai/keel/internal/store"
)

// fakeAppRuntime is the outbound execution seam.
type fakeAppRuntime struct {
	mu     sync.Mutex
	tools  map[string]MCPAppTool
	calls  []string
	reads  []string
	callOK mcp.CallToolResult
	readOK mcp.ReadResourceResult
	// block, when non-nil, holds CallTool until it is closed (concurrency test).
	block   chan struct{}
	started chan struct{}
}

func (f *fakeAppRuntime) AppTool(server, tool string) (MCPAppTool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.tools[server+"/"+tool]
	return entry, ok
}

func (f *fakeAppRuntime) CallTool(_ context.Context, server, tool string, _ json.RawMessage) (mcp.CallToolResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, server+"/"+tool)
	block, started := f.block, f.started
	f.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
	return f.callOK, nil
}

func (f *fakeAppRuntime) ReadResource(_ context.Context, server, uri string) (mcp.ReadResourceResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, server+" "+uri)
	return f.readOK, nil
}

func (f *fakeAppRuntime) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeAppRuntime) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reads)
}

// fakeAppAudit is the durable audit seam.
type fakeAppAudit struct {
	mu      sync.Mutex
	entries []store.MCPAppAuditEntry
}

func (f *fakeAppAudit) AppendMCPAppAudit(_ context.Context, entry store.MCPAppAuditEntry) (store.MCPAppAuditEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry.AuditID = int64(len(f.entries) + 1)
	f.entries = append(f.entries, entry)
	return entry, nil
}

func (f *fakeAppAudit) ListMCPAppAudit(_ context.Context, tenantID *uuid.UUID, appID, action string, after int64, limit int) ([]store.MCPAppAuditEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.MCPAppAuditEntry, 0, len(f.entries))
	for _, entry := range f.entries {
		if entry.AuditID <= after {
			continue
		}
		if tenantID != nil && entry.TenantID != nil && *entry.TenantID != *tenantID {
			continue
		}
		if appID != "" && entry.AppID != appID {
			continue
		}
		if action != "" && entry.Action != action {
			continue
		}
		out = append(out, entry)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeAppAudit) snapshot() []store.MCPAppAuditEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.MCPAppAuditEntry(nil), f.entries...)
}

// fakeAppCommit records confirmed ui/message sends.
type fakeAppCommit struct {
	mu     sync.Mutex
	inputs []fakeAppInput
	err    error
}

type fakeAppInput struct {
	TenantID    uuid.UUID
	SessionID   uuid.UUID
	RequestID   string
	Text        string
	Attachments json.RawMessage
}

func (f *fakeAppCommit) commit(_ context.Context, tenantID, sessionID uuid.UUID, requestID, text string, attachments json.RawMessage) (MCPAppInput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return MCPAppInput{}, f.err
	}
	f.inputs = append(f.inputs, fakeAppInput{tenantID, sessionID, requestID, text, attachments})
	return MCPAppInput{SubmissionID: uuid.NewString(), RequestID: requestID, Status: "committed", Seq: 1, Created: true}, nil
}

func (f *fakeAppCommit) snapshot() []fakeAppInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeAppInput(nil), f.inputs...)
}

// newBridgeAPI builds an AgentAPI over the standard fixture surface.
func newBridgeAPI(t *testing.T, tenant uuid.UUID, runtime MCPAppRuntime, audit MCPAppAuditStore, commit func(context.Context, uuid.UUID, uuid.UUID, string, string, json.RawMessage) (MCPAppInput, error), decisions *agentext.DecisionBroker, now func() time.Time) *httptest.Server {
	t.Helper()
	apps := &fakeMCPApps{tenant: tenant, published: true, surface: testSurface(t)}
	server := httptest.NewServer(New(Config{
		MCPApps:              apps,
		MCPAppRuntime:        runtime,
		MCPAppAudit:          audit,
		MCPAppInputCommit:    commit,
		Decisions:            decisions,
		MCPAppConfirmTimeout: 2 * time.Second,
		MCPAppNow:            now,
		Logger:               testLogger(),
	}))
	t.Cleanup(server.Close)
	return server
}

// postBridge issues a bridge request and returns status + raw body.
func postBridge(t *testing.T, client *http.Client, url string, tenant uuid.UUID, body any) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tenant != uuid.Nil {
		req.Header.Set("X-Tenant-ID", tenant.String())
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

type bridgeResult struct {
	status int
	body   []byte
}

func postBridgeAsync(client *http.Client, url string, tenant uuid.UUID, body any) <-chan bridgeResult {
	done := make(chan bridgeResult, 1)
	go func() {
		raw, _ := json.Marshal(body)
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
		if err != nil {
			done <- bridgeResult{status: -1}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Tenant-ID", tenant.String())
		res, err := client.Do(req)
		if err != nil {
			done <- bridgeResult{status: -1}
			return
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(res.Body)
		done <- bridgeResult{status: res.StatusCode, body: out}
	}()
	return done
}

// answerOpenDecision polls the session's open decision and answers it.
func answerOpenDecision(t *testing.T, broker *agentext.DecisionBroker, tenant, session uuid.UUID, option string) agentext.Decision {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		decisions, err := broker.List(context.Background(), tenant, session)
		if err != nil {
			t.Fatal(err)
		}
		for _, decision := range decisions {
			if decision.Status != agentext.DecisionOpen {
				continue
			}
			answered, _, err := broker.Answer(context.Background(), agentext.AnswerDecisionRequest{
				TenantID:   tenant,
				SessionID:  session,
				DecisionID: mustDecisionID(t, decision.DecisionID),
				OptionID:   option,
			})
			if err == nil {
				return answered
			}
			if errors.Is(err, agentext.ErrDecisionConflict) {
				continue
			}
			t.Fatalf("answer decision: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no open decision appeared")
	return agentext.Decision{}
}

func mustDecisionID(t *testing.T, raw string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(raw)
	if err != nil {
		t.Fatalf("decision id %q: %v", raw, err)
	}
	return id
}

func auditActions(entries []store.MCPAppAuditEntry) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Action+"="+entry.Result)
	}
	return out
}

// TestMCPAppToolCallOwnership pins the proxy gate: only app-visible tools of
// the catalogue's server may run, a refusal writes no upstream request, and a
// foreign tenant cannot reach the app at all.
func TestMCPAppToolCallOwnership(t *testing.T) {
	tenant := uuid.New()
	runtime := &fakeAppRuntime{
		tools:  map[string]MCPAppTool{"weather/dashboard": {Name: "dashboard"}},
		callOK: mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "ok"}}},
	}
	audit := &fakeAppAudit{}
	server := newBridgeAPI(t, tenant, runtime, audit, nil, nil, nil)

	status, body := postBridge(t, server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/tools/call", tenant, map[string]any{
		"name": "dashboard", "arguments": map[string]any{"x": 1},
	})
	if status != http.StatusOK {
		t.Fatalf("app-visible call = %d: %s", status, body)
	}
	if !strings.Contains(string(body), `"content"`) {
		t.Fatalf("result is not a CallToolResult: %s", body)
	}
	if runtime.callCount() != 1 {
		t.Fatalf("calls = %d, want 1", runtime.callCount())
	}

	// A model-only tool is not app-visible: 403, no upstream call, audited.
	status, _ = postBridge(t, server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/tools/call", tenant, map[string]any{
		"name": "model_only",
	})
	if status != http.StatusForbidden {
		t.Fatalf("model-only call = %d, want 403", status)
	}
	if runtime.callCount() != 1 {
		t.Fatalf("a refused call reached the server: %d calls", runtime.callCount())
	}

	// Another tenant sees the same 404 as an unknown id.
	if status, _ := postBridge(t, server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/tools/call", uuid.New(), map[string]any{"name": "dashboard"}); status != http.StatusNotFound {
		t.Fatalf("foreign tenant = %d, want 404", status)
	}
	if status, _ := postBridge(t, server.Client(), server.URL+"/v1/mcp/apps/mcpapp_0000000000000000/tools/call", tenant, map[string]any{"name": "dashboard"}); status != http.StatusNotFound {
		t.Fatalf("unknown app = %d, want 404", status)
	}
	if runtime.callCount() != 1 {
		t.Fatalf("refusals reached the server: %d calls", runtime.callCount())
	}

	actions := auditActions(audit.snapshot())
	foundRefused := false
	for _, action := range actions {
		if action == "tools/call=refused" {
			foundRefused = true
		}
	}
	if !foundRefused {
		t.Fatalf("audit = %v, want a tools/call=refused row", actions)
	}
}

// TestMCPAppDestructiveConfirmation pins the per-call confirmation gate: allow
// runs the tool, deny never reaches it, and the name-family fallback opens the
// same decision.
func TestMCPAppDestructiveConfirmation(t *testing.T) {
	tenant := uuid.New()
	session := uuid.New()
	broker := agentext.NewDecisionBroker(agentext.DecisionBrokerOptions{Store: agentext.NewMemoryDecisionStore(), Logger: testLogger()})
	runtime := &fakeAppRuntime{tools: map[string]MCPAppTool{
		"weather/destroy":      {Name: "destroy", Destructive: true},
		"weather/delete_thing": {Name: "delete_thing"},
	}, callOK: mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "done"}}}}
	audit := &fakeAppAudit{}
	server := newBridgeAPI(t, tenant, runtime, audit, nil, broker, nil)

	// Allow.
	done := postBridgeAsync(server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/tools/call", tenant, map[string]any{
		"sessionId": session.String(), "name": "destroy", "requestId": "frame-1",
	})
	answerOpenDecision(t, broker, tenant, session, "allow")
	result := <-done
	if result.status != http.StatusOK {
		t.Fatalf("allow = %d: %s", result.status, result.body)
	}
	if runtime.callCount() != 1 {
		t.Fatalf("calls = %d, want 1", runtime.callCount())
	}

	// Deny: no upstream call.
	sessionDeny := uuid.New()
	done = postBridgeAsync(server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/tools/call", tenant, map[string]any{
		"sessionId": sessionDeny.String(), "name": "destroy",
	})
	answerOpenDecision(t, broker, tenant, sessionDeny, "deny")
	result = <-done
	if result.status != http.StatusConflict {
		t.Fatalf("deny = %d, want 409: %s", result.status, result.body)
	}
	if runtime.callCount() != 1 {
		t.Fatalf("a denied destructive call reached the server: %d calls", runtime.callCount())
	}

	// Name-family fallback (no annotation).
	sessionName := uuid.New()
	done = postBridgeAsync(server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/tools/call", tenant, map[string]any{
		"sessionId": sessionName.String(), "name": "delete_thing",
	})
	answerOpenDecision(t, broker, tenant, sessionName, "allow")
	result = <-done
	if result.status != http.StatusOK {
		t.Fatalf("name-family allow = %d: %s", result.status, result.body)
	}

	// Each call opens its own decision: the same frame id cannot reuse an
	// earlier answer.
	sessionRepeat := uuid.New()
	for index := 0; index < 2; index++ {
		done = postBridgeAsync(server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/tools/call", tenant, map[string]any{
			"sessionId": sessionRepeat.String(), "name": "destroy", "requestId": "same-frame",
		})
		answerOpenDecision(t, broker, tenant, sessionRepeat, "allow")
		result = <-done
		if result.status != http.StatusOK {
			t.Fatalf("repeat %d = %d: %s", index, result.status, result.body)
		}
	}

	refused := false
	for _, entry := range audit.snapshot() {
		if entry.Action == "tools/call" && entry.Result == "refused" {
			refused = true
			if strings.Contains(entry.Detail, "secret-message") {
				t.Fatalf("audit detail carried message content: %q", entry.Detail)
			}
		}
	}
	if !refused {
		t.Fatal("the denied call was not audited as refused")
	}
}

// TestMCPAppMessageBranches pins ui/message: role gate, confirmation, the send
// branch writing through the shared input path, edit/discard writing nothing,
// and the queue-only delivery.
func TestMCPAppMessageBranches(t *testing.T) {
	tenant := uuid.New()
	broker := agentext.NewDecisionBroker(agentext.DecisionBrokerOptions{Store: agentext.NewMemoryDecisionStore(), Logger: testLogger()})
	commit := &fakeAppCommit{}
	audit := &fakeAppAudit{}
	server := newBridgeAPI(t, tenant, &fakeAppRuntime{}, audit, commit.commit, broker, nil)

	const bodyText = "hello from the app"
	branch := func(option string) (bridgeResult, fakeAppInput) {
		session := uuid.New()
		done := postBridgeAsync(server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/ui/message", tenant, map[string]any{
			"sessionId": session.String(),
			"requestId": "frame-7",
			"role":      "user",
			"content":   []map[string]any{{"type": "text", "text": bodyText}},
		})
		answerOpenDecision(t, broker, tenant, session, option)
		result := <-done
		inputs := commit.snapshot()
		if len(inputs) == 0 {
			return result, fakeAppInput{}
		}
		return result, inputs[len(inputs)-1]
	}

	// Send.
	result, input := branch("send")
	if result.status != http.StatusOK || !strings.Contains(string(result.body), `"result":"send"`) {
		t.Fatalf("send = %d: %s", result.status, result.body)
	}
	if input.Text != bodyText {
		t.Fatalf("committed text = %q, want %q", input.Text, bodyText)
	}
	if !strings.HasPrefix(input.RequestID, "mcp-app:"+testAppID+":") {
		t.Fatalf("requestId = %q, want the app-attributed prefix", input.RequestID)
	}

	// Edit: prefill only, no ledger write.
	before := len(commit.snapshot())
	result, _ = branch("edit")
	if result.status != http.StatusOK || !strings.Contains(string(result.body), `"result":"edit"`) ||
		!strings.Contains(string(result.body), bodyText) {
		t.Fatalf("edit = %d: %s", result.status, result.body)
	}
	if len(commit.snapshot()) != before {
		t.Fatal("the edit branch wrote a user.message")
	}

	// Discard: no ledger write.
	result, _ = branch("discard")
	if result.status != http.StatusOK || !strings.Contains(string(result.body), `"result":"discard"`) {
		t.Fatalf("discard = %d: %s", result.status, result.body)
	}
	if len(commit.snapshot()) != before {
		t.Fatal("the discard branch wrote a user.message")
	}

	// Role gate: zero ledger, JSON-RPC -32602, no decision even opened.
	session := uuid.New()
	status, body := postBridge(t, server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/ui/message", tenant, map[string]any{
		"sessionId": session.String(), "role": "assistant",
		"content": []map[string]any{{"type": "text", "text": bodyText}},
	})
	if status != http.StatusBadRequest || !strings.Contains(string(body), `"code":-32602`) {
		t.Fatalf("assistant role = %d: %s", status, body)
	}
	if decisions, _ := broker.List(context.Background(), tenant, session); len(decisions) != 0 {
		t.Fatalf("a bad role opened %d decisions", len(decisions))
	}

	// Delivery: a View cannot choose guide.
	status, body = postBridge(t, server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/ui/message", tenant, map[string]any{
		"sessionId": uuid.New().String(), "role": "user", "delivery": "guide",
		"content": []map[string]any{{"type": "text", "text": bodyText}},
	})
	if status != http.StatusBadRequest || !strings.Contains(string(body), `"code":-32602`) {
		t.Fatalf("guide delivery = %d: %s", status, body)
	}

	// Content gate: only text blocks.
	status, body = postBridge(t, server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/ui/message", tenant, map[string]any{
		"sessionId": uuid.New().String(), "role": "user",
		"content": []map[string]any{{"type": "image", "text": "x"}},
	})
	if status != http.StatusBadRequest || !strings.Contains(string(body), `"code":-32602`) {
		t.Fatalf("image content = %d: %s", status, body)
	}

	// The audit carries the branch but never the message body.
	sent := 0
	for _, entry := range audit.snapshot() {
		if entry.Action != "ui/message" {
			continue
		}
		sent++
		if strings.Contains(entry.Detail, bodyText) {
			t.Fatalf("audit detail carried the message body: %q", entry.Detail)
		}
	}
	if sent < 3 {
		t.Fatalf("ui/message audit rows = %d, want send/edit/discard", sent)
	}
}

// TestMCPAppOpenLink pins the link gate: scheme refusal with no decision, and
// the confirmed answer returning the URL for the host to open.
func TestMCPAppOpenLink(t *testing.T) {
	tenant := uuid.New()
	broker := agentext.NewDecisionBroker(agentext.DecisionBrokerOptions{Store: agentext.NewMemoryDecisionStore(), Logger: testLogger()})
	audit := &fakeAppAudit{}
	commit := &fakeAppCommit{}
	server := newBridgeAPI(t, tenant, &fakeAppRuntime{}, audit, commit.commit, broker, nil)

	status, body := postBridge(t, server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/ui/open-link", tenant, map[string]any{
		"sessionId": uuid.New().String(), "url": "javascript:alert(1)",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("javascript: = %d: %s", status, body)
	}
	refused := false
	for _, entry := range audit.snapshot() {
		if entry.Action == "ui/open-link" && entry.Result == "refused" {
			refused = true
		}
	}
	if !refused {
		t.Fatal("a refused scheme was not audited")
	}

	// Confirmed open: the answer carries the exact URL; nothing is written.
	session := uuid.New()
	done := postBridgeAsync(server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/ui/open-link", tenant, map[string]any{
		"sessionId": session.String(), "url": "https://example.com/docs?token=secret-token",
	})
	answerOpenDecision(t, broker, tenant, session, "open")
	result := <-done
	if result.status != http.StatusOK || !strings.Contains(string(result.body), `"opened":true`) {
		t.Fatalf("open = %d: %s", result.status, result.body)
	}
	for _, entry := range audit.snapshot() {
		if strings.Contains(entry.Detail, "secret-token") {
			t.Fatalf("audit detail carried a URL query string: %q", entry.Detail)
		}
	}
	if len(commit.snapshot()) != 0 {
		t.Fatal("open-link wrote a user.message")
	}

	// Denied open is a normal answer, not a navigation.
	sessionDeny := uuid.New()
	done = postBridgeAsync(server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/ui/open-link", tenant, map[string]any{
		"sessionId": sessionDeny.String(), "url": "https://example.com/",
	})
	answerOpenDecision(t, broker, tenant, sessionDeny, "cancel")
	result = <-done
	if result.status != http.StatusOK || !strings.Contains(string(result.body), `"opened":false`) {
		t.Fatalf("deny = %d: %s", result.status, result.body)
	}
}

// TestMCPAppResourceSchemeWhitelist pins the resources/read gate and the size
// validation of the proxy.
func TestMCPAppResourceSchemeWhitelist(t *testing.T) {
	tenant := uuid.New()
	runtime := &fakeAppRuntime{readOK: mcp.ReadResourceResult{Contents: []mcp.ResourceContents{{
		URI: "ui://weather/data", MimeType: "application/json", Text: `{"ok":true}`,
	}}}}
	audit := &fakeAppAudit{}
	server := newBridgeAPI(t, tenant, runtime, audit, nil, nil, nil)

	for _, uri := range []string{"file:///etc/passwd", "data:text/html,<b>x</b>", "", "https:///missing-host", "ftp://example.com/x"} {
		status, body := postBridge(t, server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/resources/read", tenant, map[string]any{"uri": uri})
		if status != http.StatusBadRequest {
			t.Errorf("uri %q = %d: %s", uri, status, body)
		}
	}
	if runtime.readCount() != 0 {
		t.Fatalf("a refused scheme reached the server: %d reads", runtime.readCount())
	}

	for _, uri := range []string{"ui://weather/data", "https://example.com/data", "http://example.com/data"} {
		status, body := postBridge(t, server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/resources/read", tenant, map[string]any{"uri": uri})
		if status != http.StatusOK {
			t.Fatalf("uri %q = %d: %s", uri, status, body)
		}
	}

	// An oversized result is refused at the proxy boundary.
	runtime.readOK = mcp.ReadResourceResult{Contents: []mcp.ResourceContents{{
		URI: "ui://weather/big", MimeType: "text/plain", Text: strings.Repeat("x", mcp.AppResourceMaxBytes+1),
	}}}
	status, _ := postBridge(t, server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/resources/read", tenant, map[string]any{"uri": "ui://weather/big"})
	if status != http.StatusBadGateway {
		t.Fatalf("oversized result = %d, want 502", status)
	}

	refused := 0
	for _, entry := range audit.snapshot() {
		if entry.Action == "resources/read" && entry.Result == "refused" {
			refused++
		}
	}
	if refused == 0 {
		t.Fatal("no resources/read refusal was audited")
	}
}

// TestMCPAppRateLimit pins the per-app request budget with a fake clock.
func TestMCPAppRateLimit(t *testing.T) {
	tenant := uuid.New()
	clock := time.Unix(1_700_000_000, 0)
	var clockMu sync.Mutex
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}
	runtime := &fakeAppRuntime{tools: map[string]MCPAppTool{"weather/dashboard": {Name: "dashboard"}}}
	audit := &fakeAppAudit{}
	server := newBridgeAPI(t, tenant, runtime, audit, nil, nil, now)
	url := server.URL + "/v1/mcp/apps/" + testAppID + "/tools/call"

	for index := 0; index < mcpAppRatePerSecond; index++ {
		if status, body := postBridge(t, server.Client(), url, tenant, map[string]any{"name": "dashboard"}); status != http.StatusOK {
			t.Fatalf("request %d = %d: %s", index, status, body)
		}
	}
	status, body := postBridge(t, server.Client(), url, tenant, map[string]any{"name": "dashboard"})
	if status != http.StatusTooManyRequests {
		t.Fatalf("over-budget request = %d, want 429: %s", status, body)
	}
	if runtime.callCount() != mcpAppRatePerSecond {
		t.Fatalf("upstream calls = %d, want %d", runtime.callCount(), mcpAppRatePerSecond)
	}
	rateAudited := false
	for _, entry := range audit.snapshot() {
		if entry.Action == "bridge.rate" && entry.Result == "refused" {
			rateAudited = true
		}
	}
	if !rateAudited {
		t.Fatal("a rate refusal was not audited")
	}

	clockMu.Lock()
	clock = clock.Add(time.Second)
	clockMu.Unlock()
	if status, body := postBridge(t, server.Client(), url, tenant, map[string]any{"name": "dashboard"}); status != http.StatusOK {
		t.Fatalf("request after refill = %d: %s", status, body)
	}
}

// TestMCPAppConcurrencyLimit pins the four-in-flight bound.
func TestMCPAppConcurrencyLimit(t *testing.T) {
	tenant := uuid.New()
	runtime := &fakeAppRuntime{
		tools:   map[string]MCPAppTool{"weather/dashboard": {Name: "dashboard"}},
		block:   make(chan struct{}),
		started: make(chan struct{}, mcpAppConcurrency),
	}
	audit := &fakeAppAudit{}
	server := newBridgeAPI(t, tenant, runtime, audit, nil, nil, nil)
	url := server.URL + "/v1/mcp/apps/" + testAppID + "/tools/call"

	results := make([]<-chan bridgeResult, 0, mcpAppConcurrency)
	for index := 0; index < mcpAppConcurrency; index++ {
		results = append(results, postBridgeAsync(server.Client(), url, tenant, map[string]any{"name": "dashboard"}))
	}
	for index := 0; index < mcpAppConcurrency; index++ {
		select {
		case <-runtime.started:
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d requests started", index)
		}
	}
	status, body := postBridge(t, server.Client(), url, tenant, map[string]any{"name": "dashboard"})
	if status != http.StatusTooManyRequests {
		t.Fatalf("fifth concurrent request = %d, want 429: %s", status, body)
	}
	close(runtime.block)
	for _, done := range results {
		if result := <-done; result.status != http.StatusOK {
			t.Fatalf("in-flight request = %d: %s", result.status, result.body)
		}
	}
	concurrencyAudited := false
	for _, entry := range audit.snapshot() {
		if entry.Action == "bridge.concurrency" && entry.Result == "refused" {
			concurrencyAudited = true
		}
	}
	if !concurrencyAudited {
		t.Fatal("a concurrency refusal was not audited")
	}
}

// TestMCPAppBodyLimit pins the 64 KiB request bound.
func TestMCPAppBodyLimit(t *testing.T) {
	tenant := uuid.New()
	runtime := &fakeAppRuntime{tools: map[string]MCPAppTool{"weather/dashboard": {Name: "dashboard"}}}
	audit := &fakeAppAudit{}
	server := newBridgeAPI(t, tenant, runtime, audit, nil, nil, nil)

	// The JSON body is padded past the bound with a string argument.
	padding := strings.Repeat("x", mcpAppRequestMaxBytes)
	raw, err := json.Marshal(map[string]any{"name": "dashboard", "arguments": map[string]any{"pad": padding}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/mcp/apps/"+testAppID+"/tools/call", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", tenant.String())
	req.Header.Set("Content-Type", "application/json")
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d, want 413", res.StatusCode)
	}
	if runtime.callCount() != 0 {
		t.Fatal("an oversized body reached the server")
	}
	sizeAudited := false
	for _, entry := range audit.snapshot() {
		if entry.Action == "bridge.size" && entry.Result == "refused" {
			sizeAudited = true
		}
	}
	if !sizeAudited {
		t.Fatal("an oversized body was not audited")
	}
}

// TestMCPAppAuditRoutes pins the tenant-scoped read and the internal host-action
// record entry.
func TestMCPAppAuditRoutes(t *testing.T) {
	tenant := uuid.New()
	audit := &fakeAppAudit{}
	server := newBridgeAPI(t, tenant, &fakeAppRuntime{}, audit, nil, nil, nil)

	// No tenant: 401, no rows read.
	res := rawGet(t, server, "/v1/mcp/apps/audit", uuid.Nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("audit without tenant = %d", res.StatusCode)
	}

	// Seed one bridge row and one host row.
	postBridge(t, server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/tools/call", tenant, map[string]any{"name": "model_only"})
	status, body := postBridge(t, server.Client(), server.URL+"/v1/mcp/apps/audit", tenant, map[string]any{
		"appId": testAppID, "action": "permission.denied", "detail": "permission=microphone",
	})
	if status != http.StatusCreated {
		t.Fatalf("host audit record = %d: %s", status, body)
	}

	var list struct {
		Entries []store.MCPAppAuditEntry `json:"entries"`
		Limit   int                      `json:"limit"`
		Next    int64                    `json:"next"`
	}
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/mcp/apps/audit?limit=10", tenant, nil, &list); code != 200 {
		t.Fatalf("audit list = %d", code)
	}
	if len(list.Entries) < 2 {
		t.Fatalf("audit entries = %d, want the bridge and host rows", len(list.Entries))
	}
	if list.Entries[len(list.Entries)-1].Action != "permission.denied" {
		t.Fatalf("last entry = %+v", list.Entries[len(list.Entries)-1])
	}

	// Only the closed host-action set is accepted.
	status, _ = postBridge(t, server.Client(), server.URL+"/v1/mcp/apps/audit", tenant, map[string]any{
		"appId": testAppID, "action": "tools/call",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("forged bridge action = %d, want 400", status)
	}
	status, _ = postBridge(t, server.Client(), server.URL+"/v1/mcp/apps/audit", tenant, map[string]any{
		"appId": "mcpapp_0000000000000000", "action": "protocol.violation",
	})
	if status != http.StatusNotFound {
		t.Fatalf("unknown app audit = %d, want 404", status)
	}

	// A foreign tenant sees no rows.
	foreign := rawGet(t, server, "/v1/mcp/apps/audit", uuid.New())
	var foreignList struct {
		Entries []store.MCPAppAuditEntry `json:"entries"`
	}
	if err := json.Unmarshal([]byte(readBody(t, foreign)), &foreignList); err != nil {
		t.Fatal(err)
	}
	if len(foreignList.Entries) != 0 {
		t.Fatalf("a foreign tenant saw %d audit rows", len(foreignList.Entries))
	}
}

// TestMCPAppAuditNoBodyAgainstStore is the durable no-body assertion (P3.7):
// a message body and a URL query string used through the bridge must not appear
// in any column of any mcp_app_audit row. It runs against the real schema.
func TestMCPAppAuditNoBodyAgainstStore(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn, MaxConns: 4, MinConns: 1})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "mcp-audit-nobody-"+tenant.String()[:8]); err != nil {
		t.Fatal(err)
	}

	broker := agentext.NewDecisionBroker(agentext.DecisionBrokerOptions{Store: agentext.NewMemoryDecisionStore(), Logger: testLogger()})
	commit := &fakeAppCommit{}
	server := newBridgeAPI(t, tenant, &fakeAppRuntime{}, st, commit.commit, broker, nil)

	marker := "SECRET-BODY-MARKER-" + uuid.NewString()
	for _, option := range []string{"send", "edit", "discard"} {
		session := uuid.New()
		done := postBridgeAsync(server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/ui/message", tenant, map[string]any{
			"sessionId": session.String(),
			"role":      "user",
			"content":   []map[string]any{{"type": "text", "text": marker}},
		})
		answerOpenDecision(t, broker, tenant, session, option)
		if result := <-done; result.status != http.StatusOK {
			t.Fatalf("%s branch = %d: %s", option, result.status, result.body)
		}
	}

	// A link with the same marker in its query string: only the host is audited.
	session := uuid.New()
	done := postBridgeAsync(server.Client(), server.URL+"/v1/mcp/apps/"+testAppID+"/ui/open-link", tenant, map[string]any{
		"sessionId": session.String(),
		"url":       "https://example.com/page?token=" + marker,
	})
	answerOpenDecision(t, broker, tenant, session, "open")
	if result := <-done; result.status != http.StatusOK {
		t.Fatalf("open-link = %d: %s", result.status, result.body)
	}

	// Scan every row and every potentially free-text column.
	var leaked int
	if err := st.Pool().QueryRow(ctx,
		`SELECT count(*) FROM mcp_app_audit
		  WHERE detail LIKE '%' || $1 || '%'
		     OR server LIKE '%' || $1 || '%'
		     OR app_id LIKE '%' || $1 || '%'
		     OR action LIKE '%' || $1 || '%'
		     OR result LIKE '%' || $1 || '%'`, marker).Scan(&leaked); err != nil {
		t.Fatalf("scan audit rows: %v", err)
	}
	if leaked != 0 {
		t.Fatalf("%d audit rows carried the message body or URL query", leaked)
	}
	entries, err := st.ListMCPAppAudit(ctx, &tenant, testAppID, "", 0, 500)
	if err != nil {
		t.Fatalf("list audit rows: %v", err)
	}
	if len(entries) < 4 {
		t.Fatalf("durable audit rows = %d, want the send/edit/discard/open rows", len(entries))
	}
	for _, entry := range entries {
		if strings.Contains(entry.Detail, marker) {
			t.Fatalf("audit detail carried the marker: %q", entry.Detail)
		}
	}
}

// TestMCPAppNameDestructive pins the change-family fallback vocabulary.
func TestMCPAppNameDestructive(t *testing.T) {
	for name, want := range map[string]bool{
		"delete_file":           true,
		"git.commit":            true,
		"apply_patch":           true,
		"publish_release":       true,
		"refresh":               false,
		"show_dashboard":        false,
		"get_weather":           false,
		"":                      false,
		"DELETE_EVERYTHING_NOW": true,
	} {
		if got := mcpAppNameDestructive(name); got != want {
			t.Errorf("mcpAppNameDestructive(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestMCPAppRateLimiterBucket pins the token-bucket arithmetic directly.
func TestMCPAppRateLimiterBucket(t *testing.T) {
	clock := time.Unix(0, 0)
	limiter := newMCPAppLimiter(func() time.Time { return clock })
	for index := 0; index < mcpAppRateBurst; index++ {
		if !limiter.allow("app") {
			t.Fatalf("request %d was refused inside the burst", index)
		}
	}
	if limiter.allow("app") {
		t.Fatal("a request over the burst was allowed")
	}
	clock = clock.Add(500 * time.Millisecond)
	for index := 0; index < mcpAppRatePerSecond/2; index++ {
		if !limiter.allow("app") {
			t.Fatalf("request %d after a half-second refill was refused", index)
		}
	}
	if limiter.allow("app") {
		t.Fatal("the refill over-credits")
	}
}

// Compile-time assertion that the fake implements the seams.
var (
	_ MCPAppRuntime    = (*fakeAppRuntime)(nil)
	_ MCPAppAuditStore = (*fakeAppAudit)(nil)
)

var _ = mcphost.AppSpec{}
