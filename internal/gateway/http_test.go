// Streamable HTTP transport tests, on an httptest server that speaks the same
// JSON-RPC shapes: JSON responses, SSE responses, the server-to-client GET
// stream, session headers and error mapping.

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeHTTPServer struct {
	t      *testing.T
	server *httptest.Server

	mu            sync.Mutex
	handlers      map[string]func(json.RawMessage) (any, *ErrorObject)
	silent        map[string]bool
	sseMethods    map[string]bool
	requests      []*Request
	notifications []*Notification
	deletes       int
	postHeaders   []http.Header
	streamHeaders []http.Header
	streams       []chan string
	sessionID     string
	oversizeBody  bool
	failStatus    int
}

func newFakeHTTPServer(t *testing.T) *fakeHTTPServer {
	t.Helper()
	server := &fakeHTTPServer{
		t:          t,
		handlers:   make(map[string]func(json.RawMessage) (any, *ErrorObject)),
		silent:     make(map[string]bool),
		sseMethods: make(map[string]bool),
	}
	server.server = httptest.NewServer(http.HandlerFunc(server.handle))
	t.Cleanup(server.server.Close)
	return server
}

func (s *fakeHTTPServer) url() string { return s.server.URL }

func (s *fakeHTTPServer) client() *http.Client { return s.server.Client() }

func (s *fakeHTTPServer) setHandler(method string, handler func(json.RawMessage) (any, *ErrorObject)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = handler
}

func (s *fakeHTTPServer) silence(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.silent[method] = true
}

func (s *fakeHTTPServer) respondAsSSE(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sseMethods[method] = true
}

func (s *fakeHTTPServer) setSessionID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionID = id
}

func (s *fakeHTTPServer) declare(decl providerDecl) {
	if decl.name == "" {
		decl.name = "fake-http-provider"
	}
	s.setHandler(MethodInitialize, func(json.RawMessage) (any, *ErrorObject) {
		capabilities := map[string]any{}
		provider := map[string]any{"type": string(decl.kind)}
		switch decl.kind {
		case ProviderTypeToolProvider:
			capabilities["tools"] = map[string]any{"listChanged": false}
		case ProviderTypeAgentBackend:
			agent := map[string]any{"events": true, "cancel": true}
			if decl.usage {
				agent["usage"] = true
			}
			capabilities["agent"] = agent
		}
		version := decl.version
		if version == "" {
			version = ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    capabilities,
			"serverInfo":      map[string]any{"name": decl.name, "version": "1.0.0"},
			"provider":        provider,
		}, nil
	})
}

func (s *fakeHTTPServer) handle(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleStream(w, r)
	case http.MethodDelete:
		s.mu.Lock()
		s.deletes++
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPost:
		s.handlePost(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *fakeHTTPServer) handlePost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.postHeaders = append(s.postHeaders, r.Header.Clone())
	sessionID := s.sessionID
	failStatus := s.failStatus
	oversize := s.oversizeBody
	s.mu.Unlock()
	if sessionID != "" {
		w.Header().Set(headerSessionID, sessionID)
	}
	if failStatus != 0 {
		http.Error(w, "server unhappy", failStatus)
		return
	}
	message, err := ParseMessage(body)
	if err != nil {
		http.Error(w, "bad message", http.StatusBadRequest)
		return
	}
	switch typed := message.(type) {
	case *Request:
		s.mu.Lock()
		s.requests = append(s.requests, typed)
		handler := s.handlers[typed.Method]
		silent := s.silent[typed.Method]
		useSSE := s.sseMethods[typed.Method]
		s.mu.Unlock()
		if silent {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if oversize {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusOK)
			blob := strings.Repeat("x", 2048)
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"blob":%q}}`, typed.ID, blob)
			return
		}
		if useSSE {
			s.writeSSEResponse(w, typed, handler)
			return
		}
		if handler == nil {
			s.writeErrorResponse(w, typed.ID, CodeMethodNotFound, "no handler: "+typed.Method)
			return
		}
		result, errorObject := handler(typed.Params)
		if errorObject != nil {
			s.writeErrorResponse(w, typed.ID, errorObject.Code, errorObject.Message)
			return
		}
		s.writeResultResponse(w, typed.ID, result)
	case *Notification:
		s.mu.Lock()
		s.notifications = append(s.notifications, typed)
		s.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

func (s *fakeHTTPServer) writeResultResponse(w http.ResponseWriter, id ID, result any) {
	data, err := json.Marshal(result)
	if err != nil {
		http.Error(w, "encode", http.StatusInternalServerError)
		return
	}
	payload, err := json.Marshal(&SuccessResponse{JSONRPC: JSONRPCVersion, ID: id, Result: data})
	if err != nil {
		http.Error(w, "encode", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(payload, '\n'))
}

func (s *fakeHTTPServer) writeErrorResponse(w http.ResponseWriter, id ID, code int, message string) {
	payload, _ := json.Marshal(&ErrorResponse{JSONRPC: JSONRPCVersion, ID: id, Error: ErrorObject{Code: code, Message: message}})
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(payload, '\n'))
}

// writeSSEResponse answers a request on an SSE body: a progress notification
// first, then the response.
func (s *fakeHTTPServer) writeSSEResponse(w http.ResponseWriter, request *Request, handler func(json.RawMessage) (any, *ErrorObject)) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	progress, _ := json.Marshal(&Notification{
		JSONRPC: JSONRPCVersion,
		Method:  MethodEventStream,
		Params:  json.RawMessage(`{"sessionId":"ext-1","type":"progress"}`),
	})
	_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", progress)
	flusher.Flush()

	result := any(map[string]any{})
	if handler != nil {
		value, errorObject := handler(request.Params)
		if errorObject != nil {
			payload, _ := json.Marshal(&ErrorResponse{JSONRPC: JSONRPCVersion, ID: request.ID, Error: *errorObject})
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
			flusher.Flush()
			return
		}
		result = value
	}
	data, _ := json.Marshal(result)
	payload, _ := json.Marshal(&SuccessResponse{JSONRPC: JSONRPCVersion, ID: request.ID, Result: data})
	_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
	flusher.Flush()
}

// handleStream is the server-to-client GET stream.
func (s *fakeHTTPServer) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.streamHeaders = append(s.streamHeaders, r.Header.Clone())
	s.mu.Unlock()
	w.Header().Set("content-type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	events := make(chan string, 32)
	s.mu.Lock()
	s.streams = append(s.streams, events)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		for index, candidate := range s.streams {
			if candidate == events {
				s.streams = append(s.streams[:index], s.streams[index+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}()
	// An id-only line (resumable cursor) before any event.
	_, _ = fmt.Fprint(w, "id: 1\n\n")
	flusher.Flush()
	for {
		select {
		case data := <-events:
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *fakeHTTPServer) pushMessage(message Message) {
	data, err := json.Marshal(message)
	if err != nil {
		s.t.Logf("encode pushed message: %v", err)
		return
	}
	s.mu.Lock()
	streams := append([]chan string(nil), s.streams...)
	s.mu.Unlock()
	for _, stream := range streams {
		select {
		case stream <- string(data):
		default:
		}
	}
}

func (s *fakeHTTPServer) waitForStream(t *testing.T) bool {
	t.Helper()
	return waitFor(t, testTimeout, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.streams) > 0
	})
}

func (s *fakeHTTPServer) requestsSnapshot() []*Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Request(nil), s.requests...)
}

func (s *fakeHTTPServer) deletesCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deletes
}

type httpFixture struct {
	conn      *Conn
	tool      *ToolClient
	agent     *AgentClient
	transport *HTTPTransport
	server    *fakeHTTPServer
	diags     *diagnosticLog
}

func newHTTPFixture(t *testing.T, decl providerDecl, prepare ...func(*fakeHTTPServer)) *httpFixture {
	t.Helper()
	server := newFakeHTTPServer(t)
	server.declare(decl)
	for _, step := range prepare {
		step(server)
	}
	transport := NewHTTPTransport(HTTPTransportOptions{
		URL:    server.url(),
		Client: server.client(),
	})
	return buildHTTPFixture(t, server, transport)
}

// newHTTPFixtureWithCap is the same fixture with a custom transport (used for
// the bounded-body tests).
func newHTTPFixtureWithCap(t *testing.T, decl providerDecl, maxFrameBytes int) *httpFixture {
	t.Helper()
	server := newFakeHTTPServer(t)
	server.declare(decl)
	transport := NewHTTPTransport(HTTPTransportOptions{
		URL:           server.url(),
		Client:        server.client(),
		MaxFrameBytes: maxFrameBytes,
	})
	return buildHTTPFixture(t, server, transport)
}

func buildHTTPFixture(t *testing.T, server *fakeHTTPServer, transport *HTTPTransport) *httpFixture {
	t.Helper()
	diags := &diagnosticLog{}
	conn := NewConn(ConnOptions{
		RequestTimeout: 5 * time.Second,
		OnDiagnostic:   diags.add,
	})
	if _, err := conn.Connect(context.Background(), transport); err != nil {
		t.Fatalf("connect over HTTP: %v", err)
	}
	group := &httpFixture{
		conn:      conn,
		tool:      NewToolProvider(conn),
		agent:     NewAgentBackend(conn),
		transport: transport,
		server:    server,
		diags:     diags,
	}
	t.Cleanup(func() { _ = conn.Close() })
	return group
}

func TestHTTPTransportJSONRoundTrip(t *testing.T) {
	group := newHTTPFixture(t, providerDecl{kind: ProviderTypeToolProvider, name: "http-tools"},
		func(server *fakeHTTPServer) { server.setSessionID("session-42") })
	group.server.setHandler(MethodToolsList, func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{
			"tools": []any{map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}}},
		}, nil
	})
	group.server.setHandler(MethodToolsCall, func(params json.RawMessage) (any, *ErrorObject) {
		var call callToolParams
		_ = json.Unmarshal(params, &call)
		return map[string]any{
			"content": []any{map[string]any{"type": "text", "text": call.Name}},
		}, nil
	})

	tools, err := group.tool.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v, want echo", tools)
	}
	result, err := group.tool.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "echo" {
		t.Fatalf("result = %+v", result)
	}

	// The session id from initialize is echoed on later requests, and the
	// negotiated revision travels in the MCP-Protocol-Version header.
	if got := group.transport.SessionID(); got != "session-42" {
		t.Errorf("session id = %q, want session-42", got)
	}
	requests := group.server.requestsSnapshot()
	if len(requests) < 3 {
		t.Fatalf("server saw %d requests, want initialize + list + call", len(requests))
	}
	group.server.mu.Lock()
	headers := append([]http.Header(nil), group.server.postHeaders...)
	group.server.mu.Unlock()
	if len(headers) < 3 {
		t.Fatalf("recorded %d request headers", len(headers))
	}
	if got := headers[1].Get(headerSessionID); got != "session-42" {
		t.Errorf("tools/list session header = %q, want session-42", got)
	}
	if got := headers[1].Get(headerProtocolVersion); got != ProtocolVersion {
		t.Errorf("protocol version header = %q, want %q", got, ProtocolVersion)
	}
	if !strings.Contains(headers[0].Get("accept"), "text/event-stream") {
		t.Errorf("accept header = %q, want it to offer SSE", headers[0].Get("accept"))
	}
}

func TestHTTPTransportSSEResponseCarriesNotifications(t *testing.T) {
	group := newHTTPFixture(t, providerDecl{kind: ProviderTypeToolProvider})
	group.server.respondAsSSE(MethodToolsCall)
	group.server.setHandler(MethodToolsCall, func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "streamed"}}}, nil
	})

	result, err := group.tool.CallTool(context.Background(), "echo", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "streamed" {
		t.Fatalf("result = %+v, want the streamed response", result)
	}
	// The notification that preceded the response was consumed too.
	if !waitFor(t, testTimeout, func() bool { return group.transport.Stats().Received >= 2 }) {
		t.Fatalf("received = %d messages, want the notification and the response", group.transport.Stats().Received)
	}
}

func TestHTTPTransportAgentEventStream(t *testing.T) {
	group := newHTTPFixture(t, providerDecl{kind: ProviderTypeAgentBackend, name: "http-agent"})
	group.server.setHandler(MethodSessionOpen, func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{"sessionId": "ext-7"}, nil
	})

	session, err := group.agent.Open(context.Background(), SessionOpenRequest{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if session.SessionID != "ext-7" {
		t.Fatalf("session = %+v, want ext-7", session)
	}
	// The handshake opened the server-to-client stream (the AgentBackend kind
	// is what triggers it).
	if !group.server.waitForStream(t) {
		t.Fatal("the server-to-client event stream was never opened")
	}

	event := &Notification{
		JSONRPC: JSONRPCVersion,
		Method:  MethodEventStream,
		Params:  json.RawMessage(`{"sessionId":"ext-7","type":"message","seq":3,"data":{"text":"hi"}}`),
	}
	group.server.pushMessage(event)
	select {
	case received := <-group.agent.Events():
		if received.SessionID != "ext-7" || received.Type != "message" || received.Sequence != 3 {
			t.Fatalf("event = %+v, want the pushed one", received)
		}
	case <-time.After(testTimeout):
		t.Fatal("no event arrived over the HTTP event stream")
	}
}

func TestHTTPTransportMapsErrors(t *testing.T) {
	group := newHTTPFixture(t, providerDecl{kind: ProviderTypeToolProvider})
	group.server.mu.Lock()
	group.server.failStatus = http.StatusInternalServerError
	group.server.mu.Unlock()
	_, err := group.tool.ListTools(context.Background())
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error = %v, want *HTTPStatusError", err)
	}
	if statusErr.Status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", statusErr.Status)
	}

	// A response body over the transport's frame cap is refused, not buffered.
	bounded := newHTTPFixtureWithCap(t, providerDecl{kind: ProviderTypeToolProvider}, 512)
	bounded.server.mu.Lock()
	bounded.server.oversizeBody = true
	bounded.server.mu.Unlock()
	_, err = bounded.tool.ListTools(context.Background())
	var frameErr *FrameError
	if !errors.As(err, &frameErr) || frameErr.Kind != FrameTooLarge {
		t.Fatalf("error = %v, want *FrameError too_large", err)
	}
}

func TestHTTPTransportClosesSessionOnClose(t *testing.T) {
	group := newHTTPFixture(t, providerDecl{kind: ProviderTypeToolProvider},
		func(server *fakeHTTPServer) { server.setSessionID("session-9") })
	group.server.setHandler(MethodToolsList, func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{"tools": []any{}}, nil
	})
	if _, err := group.tool.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if err := group.conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !waitFor(t, testTimeout, func() bool { return group.server.deletesCount() == 1 }) {
		t.Fatalf("delete requests = %d, want 1", group.server.deletesCount())
	}
}
