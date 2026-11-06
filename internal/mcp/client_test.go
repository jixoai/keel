// Tests ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/test/client.test.ts (MIT License), plus MCP tool-adaptation cases
// from packages/coding-agent/test/mcp-extension.test.ts. Client-side behaviour
// runs over MemoryTransport instead of pi's InMemoryTransport.

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
)

// testServer answers client requests over the server endpoint of a memory pair
// and records every message it sees, standing in for pi's TestServer.
type testServer struct {
	transport *MemoryTransport
	mu        sync.Mutex
	messages  []Message
	handlers  map[string]func(json.RawMessage) (any, *ErrorObject)
	silent    map[string]bool
}

func newTestServer(t *testing.T) (*MemoryTransport, *testServer) {
	t.Helper()
	clientTransport, serverTransport := NewMemoryTransportPair()
	server := &testServer{
		transport: serverTransport,
		handlers:  make(map[string]func(json.RawMessage) (any, *ErrorObject)),
		silent:    make(map[string]bool),
	}
	server.setHandler("initialize", func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{
			"protocolVersion": LatestProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
			"serverInfo":      map[string]any{"name": "test-server", "version": "1.0.0"},
			"instructions":    "Use test tools.",
		}, nil
	})
	if err := serverTransport.Start(context.Background()); err != nil {
		t.Fatalf("start server transport: %v", err)
	}
	go server.loop()

	t.Cleanup(func() { _ = serverTransport.Close() })
	return clientTransport, server
}

func (s *testServer) loop() {
	for {
		message, err := s.transport.Receive(context.Background())
		if err != nil {
			return
		}
		s.mu.Lock()
		s.messages = append(s.messages, message)
		s.mu.Unlock()
		request, ok := message.(*Request)
		if !ok {
			continue
		}
		s.mu.Lock()
		handler := s.handlers[request.Method]
		silent := s.silent[request.Method]
		s.mu.Unlock()
		if silent {
			continue
		}
		if handler == nil {
			s.respond(request.ID, nil, &ErrorObject{Code: CodeMethodNotFound, Message: "Method not found: " + request.Method})
			continue
		}
		result, rpcError := handler(request.Params)
		s.respond(request.ID, result, rpcError)
	}
}

func (s *testServer) respond(id ID, result any, rpcError *ErrorObject) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if rpcError != nil {
		_ = s.transport.Send(ctx, &ErrorResponse{JSONRPC: JSONRPCVersion, ID: id, Error: *rpcError})
		return
	}
	data, err := json.Marshal(result)
	if err != nil {
		_ = s.transport.Send(ctx, &ErrorResponse{JSONRPC: JSONRPCVersion, ID: id, Error: ErrorObject{Code: CodeInternalError, Message: err.Error()}})
		return
	}
	_ = s.transport.Send(ctx, &SuccessResponse{JSONRPC: JSONRPCVersion, ID: id, Result: data})
}

func (s *testServer) setHandler(method string, handler func(json.RawMessage) (any, *ErrorObject)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = handler
}

// neverRespond makes the server drop requests for method, to test timeouts.
func (s *testServer) neverRespond(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.silent[method] = true
}

func (s *testServer) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.messages...)
}

func (s *testServer) requests(method string) []*Request {
	var out []*Request
	for _, message := range s.Messages() {
		if request, ok := message.(*Request); ok && request.Method == method {
			out = append(out, request)
		}
	}
	return out
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return condition()
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func connectTestClient(t *testing.T, clientTransport *MemoryTransport) *Client {
	t.Helper()
	client := NewClient(ClientOptions{Name: "test-client", Version: "2.0.0"})
	if _, err := client.Connect(testContext(t), clientTransport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestClientInitialize(t *testing.T) {
	clientTransport, server := newTestServer(t)
	client := connectTestClient(t, clientTransport)

	if got := client.State(); got != ClientConnected {
		t.Errorf("State() = %q, want %q", got, ClientConnected)
	}
	if got := client.ProtocolVersion(); got != LatestProtocolVersion {
		t.Errorf("ProtocolVersion() = %q, want %q", got, LatestProtocolVersion)
	}
	if got := client.ServerInfo(); got != (Implementation{Name: "test-server", Version: "1.0.0"}) {
		t.Errorf("ServerInfo() = %+v, want test-server 1.0.0", got)
	}
	if capabilities := client.ServerCapabilities(); capabilities.Tools == nil || !capabilities.Tools.ListChanged {
		t.Errorf("ServerCapabilities().Tools = %+v, want listChanged", capabilities.Tools)
	}
	if got := client.Instructions(); got != "Use test tools." {
		t.Errorf("Instructions() = %q, want %q", got, "Use test tools.")
	}

	if !waitFor(t, 2*time.Second, func() bool { return len(server.Messages()) == 2 }) {
		t.Fatalf("server saw %d messages, want initialize + notifications/initialized: %#v", len(server.Messages()), server.Messages())
	}
	messages := server.Messages()
	if len(messages) != 2 {
		t.Fatalf("server saw %d messages, want initialize + notifications/initialized: %#v", len(messages), messages)
	}
	initialize, ok := messages[0].(*Request)
	if !ok || initialize.Method != "initialize" {
		t.Fatalf("first message = %#v, want initialize request", messages[0])
	}
	if initialize.ID != NumberID(1) {
		t.Errorf("initialize id = %s, want 1", initialize.ID)
	}
	var params struct {
		ProtocolVersion string            `json:"protocolVersion"`
		Capabilities    map[string]any    `json:"capabilities"`
		ClientInfo      map[string]string `json:"clientInfo"`
	}
	if err := json.Unmarshal(initialize.Params, &params); err != nil {
		t.Fatalf("decode initialize params: %v", err)
	}
	if params.ProtocolVersion != LatestProtocolVersion {
		t.Errorf("protocolVersion = %q, want %q", params.ProtocolVersion, LatestProtocolVersion)
	}
	if len(params.Capabilities) != 0 {
		t.Errorf("capabilities = %#v, want {}", params.Capabilities)
	}
	if params.ClientInfo["name"] != "test-client" || params.ClientInfo["version"] != "2.0.0" {
		t.Errorf("clientInfo = %#v, want test-client 2.0.0", params.ClientInfo)
	}
	if _, hasTitle := params.ClientInfo["title"]; hasTitle {
		t.Errorf("clientInfo = %#v, want title omitted", params.ClientInfo)
	}
	initialized, ok := messages[1].(*Notification)
	if !ok || initialized.Method != "notifications/initialized" {
		t.Fatalf("second message = %#v, want notifications/initialized", messages[1])
	}
	if initialized.Params != nil {
		t.Errorf("notifications/initialized params = %s, want omitted", initialized.Params)
	}
}

func TestClientListToolsPaginates(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.setHandler("tools/list", func(params json.RawMessage) (any, *ErrorObject) {
		var page struct {
			Cursor string `json:"cursor"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &page); err != nil {
				return nil, &ErrorObject{Code: CodeInvalidParams, Message: err.Error()}
			}
		}
		if page.Cursor == "" {
			return map[string]any{
				"tools": []any{
					map[string]any{"name": "search", "description": "Search", "inputSchema": map[string]any{"type": "object"}},
				},
				"nextCursor": "page-2",
			}, nil
		}
		return map[string]any{
			"tools": []any{
				map[string]any{
					"name":         "read",
					"inputSchema":  map[string]any{"type": "object"},
					"outputSchema": map[string]any{"type": "object"},
					"annotations":  map[string]any{"readOnlyHint": true},
				},
			},
			// Some servers end pagination with an empty cursor.
			"nextCursor": "",
		}, nil
	})
	client := connectTestClient(t, clientTransport)

	tools, err := client.ListTools(testContext(t))
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("ListTools returned %d tools, want 2: %#v", len(tools), tools)
	}
	if tools[0].Name != "search" || tools[0].Description != "Search" {
		t.Errorf("tools[0] = %+v, want search/Search", tools[0])
	}
	if tools[1].Name != "read" {
		t.Errorf("tools[1].Name = %q, want read", tools[1].Name)
	}
	if string(tools[1].OutputSchema) != `{"type":"object"}` {
		t.Errorf("tools[1].OutputSchema = %s, want {\"type\":\"object\"}", tools[1].OutputSchema)
	}
	if !strings.Contains(string(tools[1].Annotations), `"readOnlyHint":true`) {
		t.Errorf("tools[1].Annotations = %s, want readOnlyHint:true", tools[1].Annotations)
	}

	pages := server.requests("tools/list")
	if len(pages) != 2 {
		t.Fatalf("server saw %d tools/list requests, want 2", len(pages))
	}
	if pages[0].Params != nil {
		t.Errorf("first page params = %s, want omitted", pages[0].Params)
	}
	if got := string(pages[1].Params); got != `{"cursor":"page-2"}` {
		t.Errorf("second page params = %s, want {\"cursor\":\"page-2\"}", got)
	}
}

func TestClientCallTool(t *testing.T) {
	clientTransport, server := newTestServer(t)
	var calls []string
	var callsMu sync.Mutex
	server.setHandler("tools/call", func(params json.RawMessage) (any, *ErrorObject) {
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &call); err != nil {
			return nil, &ErrorObject{Code: CodeInvalidParams, Message: err.Error()}
		}
		callsMu.Lock()
		calls = append(calls, call.Name+":"+string(call.Arguments))
		callsMu.Unlock()
		switch call.Name {
		case "count":
			var arguments struct {
				Count int `json:"count"`
			}
			_ = json.Unmarshal(call.Arguments, &arguments)
			return map[string]any{
				"content":           []any{map[string]any{"type": "text", "text": "ok"}},
				"structuredContent": map[string]any{"count": arguments.Count},
			}, nil
		case "fail":
			return nil, &ErrorObject{Code: 1234, Message: "tool failed", Data: json.RawMessage(`{"retryable":false}`)}
		case "is-error":
			return map[string]any{"content": []any{map[string]any{"type": "text", "text": "boom"}}, "isError": true}, nil
		case "structured-only":
			return map[string]any{"structuredContent": map[string]any{"ok": true}}, nil
		case "structured-any":
			// MCP 2026-07-28 allows any JSON value as structured content.
			return map[string]any{"content": []any{}, "structuredContent": []any{
				map[string]any{"id": 1}, map[string]any{"id": 2},
			}}, nil
		case "invalid-content":
			return map[string]any{"content": "not a list"}, nil
		default:
			return map[string]any{"content": []any{map[string]any{"type": "text", "text": "done"}}}, nil
		}
	})
	client := connectTestClient(t, clientTransport)

	result, err := client.CallTool(testContext(t), "count", json.RawMessage(`{"count":3}`))
	if err != nil {
		t.Fatalf("CallTool(count): %v", err)
	}
	if len(result.Content) != 1 || result.Content[0].Type != "text" || result.Content[0].Text != "ok" {
		t.Errorf("content = %#v, want text ok", result.Content)
	}
	if string(result.StructuredContent) != `{"count":3}` {
		t.Errorf("structuredContent = %s, want {\"count\":3}", result.StructuredContent)
	}
	if result.IsError {
		t.Error("IsError = true, want false")
	}

	structuredOnly, err := client.CallTool(testContext(t), "structured-only", nil)
	if err != nil {
		t.Fatalf("CallTool(structured-only): %v", err)
	}
	if structuredOnly.Content == nil || len(structuredOnly.Content) != 0 {
		t.Errorf("content = %#v, want empty list", structuredOnly.Content)
	}
	if string(structuredOnly.StructuredContent) != `{"ok":true}` {
		t.Errorf("structuredContent = %s, want {\"ok\":true}", structuredOnly.StructuredContent)
	}

	structuredAny, err := client.CallTool(testContext(t), "structured-any", nil)
	if err != nil {
		t.Fatalf("CallTool(structured-any): %v", err)
	}
	if string(structuredAny.StructuredContent) != `[{"id":1},{"id":2}]` {
		t.Errorf("structuredContent = %s, want [{\"id\":1},{\"id\":2}]", structuredAny.StructuredContent)
	}

	_, err = client.CallTool(testContext(t), "invalid-content", nil)
	if err == nil || !strings.Contains(err.Error(), "Invalid MCP tools/call result") {
		t.Errorf("CallTool(invalid-content) error = %v, want Invalid MCP tools/call result", err)
	}

	isError, err := client.CallTool(testContext(t), "is-error", nil)
	if err != nil {
		t.Fatalf("CallTool(is-error): %v", err)
	}
	if !isError.IsError {
		t.Error("IsError = false, want true")
	}

	_, err = client.CallTool(testContext(t), "fail", nil)
	var mcpError *McpError
	if !errors.As(err, &mcpError) {
		t.Fatalf("CallTool(fail) error = %v, want *McpError", err)
	}
	if mcpError.Code != 1234 || mcpError.Message != "tool failed" {
		t.Errorf("McpError = %+v, want code 1234 message tool failed", mcpError)
	}
	if string(mcpError.Data) != `{"retryable":false}` {
		t.Errorf("McpError.Data = %s, want {\"retryable\":false}", mcpError.Data)
	}
}

func TestClientRequestTimeout(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.neverRespond("tools/call")
	client := NewClient(ClientOptions{Name: "test-client", Version: "1.0.0", RequestTimeout: 40 * time.Millisecond})
	if _, err := client.Connect(testContext(t), clientTransport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	started := time.Now()
	_, err := client.CallTool(testContext(t), "slow", nil)
	var timeout *TimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("CallTool error = %v, want *TimeoutError", err)
	}
	if timeout.Timeout != 40*time.Millisecond {
		t.Errorf("TimeoutError.Timeout = %v, want 40ms", timeout.Timeout)
	}
	if elapsed := time.Since(started); elapsed < 40*time.Millisecond {
		t.Errorf("CallTool returned after %v, want at least 40ms", elapsed)
	}
}

func TestClientIgnoresUnknownResponses(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.setHandler("tools/call", func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "done"}}}, nil
	})
	client := connectTestClient(t, clientTransport)

	// A response for an id this client never issued must be dropped without
	// failing the connection or any pending call.
	if err := server.transport.Send(testContext(t), &SuccessResponse{
		JSONRPC: JSONRPCVersion,
		ID:      NumberID(999),
		Result:  json.RawMessage(`{"unexpected":true}`),
	}); err != nil {
		t.Fatalf("send unknown response: %v", err)
	}
	if err := server.transport.Send(testContext(t), &ErrorResponse{
		JSONRPC: JSONRPCVersion,
		ID:      StringID("unknown"),
		Error:   ErrorObject{Code: CodeInternalError, Message: "stray"},
	}); err != nil {
		t.Fatalf("send unknown error response: %v", err)
	}

	result, err := client.CallTool(testContext(t), "wait", nil)
	if err != nil {
		t.Fatalf("CallTool after unknown responses: %v", err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "done" {
		t.Errorf("content = %#v, want done", result.Content)
	}
	if got := client.State(); got != ClientConnected {
		t.Errorf("State() = %q, want connected", got)
	}
}

func TestClientAnswersServerRequestWithMethodNotFound(t *testing.T) {
	clientTransport, server := newTestServer(t)
	client := connectTestClient(t, clientTransport)

	if err := server.transport.Send(testContext(t), &Request{
		JSONRPC: JSONRPCVersion,
		ID:      StringID("srv-1"),
		Method:  "roots/list",
	}); err != nil {
		t.Fatalf("send server request: %v", err)
	}

	var response *ErrorResponse
	if !waitFor(t, 2*time.Second, func() bool {
		for _, message := range server.Messages() {
			if candidate, ok := message.(*ErrorResponse); ok && candidate.ID == StringID("srv-1") {
				response = candidate
				return true
			}
		}
		return false
	}) {
		t.Fatal("client did not answer the server request")
	}
	if response.Error.Code != CodeMethodNotFound {
		t.Errorf("error code = %d, want %d", response.Error.Code, CodeMethodNotFound)
	}
	if !strings.Contains(response.Error.Message, "roots/list") {
		t.Errorf("error message = %q, want it to name roots/list", response.Error.Message)
	}
	if got := client.State(); got != ClientConnected {
		t.Errorf("State() = %q, want connected", got)
	}
}

func TestClientCloseRejectsPendingAndIsIdempotent(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.neverRespond("tools/call")
	client := connectTestClient(t, clientTransport)

	done := make(chan error, 1)
	go func() {
		_, err := client.CallTool(testContext(t), "wait", nil)
		done <- err
	}()
	if !waitFor(t, 2*time.Second, func() bool { return len(server.requests("tools/call")) == 1 }) {
		t.Fatal("server did not receive tools/call")
	}

	if err := server.transport.Close(); err != nil {
		t.Fatalf("close server transport: %v", err)
	}
	select {
	case err := <-done:
		var closed *ConnectionClosedError
		if !errors.As(err, &closed) {
			t.Fatalf("pending CallTool error = %v, want *ConnectionClosedError", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending CallTool did not fail when the server dropped")
	}
	if got := client.State(); got != ClientClosed {
		t.Errorf("State() = %q, want closed", got)
	}
	if err := client.Close(); err != nil {
		t.Errorf("second Close() = %v, want nil", err)
	}
}

func TestClientRejectsUnsupportedProtocolVersion(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.setHandler("initialize", func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{
			"protocolVersion": "1999-01-01",
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "ancient-server", "version": "0.1.0"},
		}, nil
	})
	client := NewClient(ClientOptions{Name: "test-client", Version: "1.0.0"})
	_, err := client.Connect(testContext(t), clientTransport)
	if err == nil || !strings.Contains(err.Error(), "unsupported protocol version") {
		t.Fatalf("Connect error = %v, want unsupported protocol version", err)
	}
	if got := client.State(); got != ClientClosed {
		t.Errorf("State() = %q, want closed", got)
	}
}

func TestClientAcceptsOlderProtocolVersion(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.setHandler("initialize", func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "old-server", "version": "0.1.0"},
		}, nil
	})
	client := NewClient(ClientOptions{Name: "test-client", Version: "1.0.0"})
	if _, err := client.Connect(testContext(t), clientTransport); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if got := client.ProtocolVersion(); got != "2024-11-05" {
		t.Errorf("ProtocolVersion() = %q, want 2024-11-05", got)
	}
}

func TestAgentTools(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.setHandler("tools/list", func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{
			"tools": []any{
				map[string]any{
					"name":        "search",
					"description": "  Search docs  ",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}},
				},
				map[string]any{
					"name":        "no-description",
					"inputSchema": map[string]any{},
					"annotations": map[string]any{"title": "Annotated title"},
				},
			},
		}, nil
	})
	server.setHandler("tools/call", func(params json.RawMessage) (any, *ErrorObject) {
		var call struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(params, &call)
		if call.Name == "search" {
			return map[string]any{
				"content":           []any{map[string]any{"type": "text", "text": "found"}},
				"structuredContent": map[string]any{"hits": 1},
			}, nil
		}
		return map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "boom"}},
			"isError": true,
		}, nil
	})
	client := connectTestClient(t, clientTransport)

	tools, err := client.AgentTools("docs")
	if err != nil {
		t.Fatalf("AgentTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("AgentTools returned %d tools, want 2", len(tools))
	}
	if got := tools[0].Name(); got != "mcp__docs__search" {
		t.Errorf("tools[0].Name() = %q, want mcp__docs__search", got)
	}
	if got := tools[0].Description(); got != "Search docs" {
		t.Errorf("tools[0].Description() = %q, want trimmed Search docs", got)
	}
	if got := string(tools[0].Schema()); !strings.Contains(got, `"type":"object"`) || !strings.Contains(got, `"q"`) {
		t.Errorf("tools[0].Schema() = %s, want normalized object schema", got)
	}
	if got := tools[1].Name(); got != "mcp__docs__no_description" {
		t.Errorf("tools[1].Name() = %q, want mcp__docs__no_description", got)
	}
	if got := tools[1].Description(); got != "Annotated title" {
		t.Errorf("tools[1].Description() = %q, want annotations title", got)
	}
	if got := string(tools[1].Schema()); got != `{"properties":{},"type":"object"}` {
		t.Errorf("tools[1].Schema() = %s, want normalized empty object (encoding/json sorts keys)", got)
	}

	result, err := tools[0].Execute(testContext(t), "call-1", json.RawMessage(`{"q":"x"}`), nil)
	if err != nil {
		t.Fatalf("Execute(search): %v", err)
	}
	if text := agentcore.ContentToText(result.Content); text != "found" {
		t.Errorf("Execute(search) content = %q, want found", text)
	}
	details, ok := result.Details.(MCPToolDetails)
	if !ok {
		t.Fatalf("Details = %#v, want MCPToolDetails", result.Details)
	}
	if details.Server != "docs" || details.Tool != "search" || details.IsError {
		t.Errorf("Details = %+v, want docs/search without error", details)
	}
	structured, ok := result.StructuredContent.(CallToolResult)
	if !ok {
		t.Fatalf("StructuredContent = %#v, want CallToolResult", result.StructuredContent)
	}
	if string(structured.StructuredContent) != `{"hits":1}` {
		t.Errorf("StructuredContent.structuredContent = %s, want {\"hits\":1}", structured.StructuredContent)
	}

	// A server-reported isError keeps the model-facing content and is passed
	// through in Details/StructuredContent instead of becoming a Go error.
	errorResult, err := tools[1].Execute(testContext(t), "call-2", nil, nil)
	if err != nil {
		t.Fatalf("Execute(no-description): %v", err)
	}
	if text := agentcore.ContentToText(errorResult.Content); text != "boom" {
		t.Errorf("error content = %q, want boom", text)
	}
	errorDetails := errorResult.Details.(MCPToolDetails)
	if !errorDetails.IsError {
		t.Error("Details.IsError = false, want true")
	}
	errorStructured := errorResult.StructuredContent.(CallToolResult)
	if !errorStructured.IsError {
		t.Error("StructuredContent.IsError = false, want true")
	}
}

func TestAgentToolsLimitsOutput(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.setHandler("tools/list", func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{"tools": []any{
			map[string]any{"name": "big", "inputSchema": map[string]any{"type": "object"}},
		}}, nil
	})
	big := strings.Repeat("x", 30_000)
	server.setHandler("tools/call", func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": big}}}, nil
	})
	client := connectTestClient(t, clientTransport)

	tools, err := client.AgentTools("docs")
	if err != nil {
		t.Fatalf("AgentTools: %v", err)
	}
	result, err := tools[0].Execute(testContext(t), "call-1", nil, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	text := agentcore.ContentToText(result.Content)
	if !strings.Contains(text, "Warning: truncated output") {
		t.Errorf("content = %.200q, want truncation warning", text)
	}
	if len(text) >= len(big) {
		t.Errorf("truncated content length = %d, want less than %d", len(text), len(big))
	}
	if details := result.Details.(MCPToolDetails); details.FullOutputPath != "" {
		t.Errorf("FullOutputPath = %q, want empty (no saver)", details.FullOutputPath)
	}
}

func TestNewClientDefaultTimeout(t *testing.T) {
	if got := NewClient(ClientOptions{}).options.RequestTimeout; got != DefaultRequestTimeout {
		t.Errorf("default RequestTimeout = %v, want %v", got, DefaultRequestTimeout)
	}
}
