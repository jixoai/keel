// Tests ported from @earendil-works/pi-mcp (55a5c6914570e2ae31ca36297654483bbbf46883),
// packages/mcp/test/streamable-http.test.ts (MIT License). Each subtest keeps
// the title of the pi test it ports.

package mcp

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
	"testing/iotest"
	"time"
)

func TestConsumeSSEStream(t *testing.T) {
	t.Run("parses chunked CRLF events, comments, IDs, and multiline data", func(t *testing.T) {
		input := ": keepalive\r\nid: 7\r\ndata: {\"one\":\r\ndata: 1}\r\n\r\n"
		var events []SSEEvent
		if err := ConsumeSSEStream(iotest.OneByteReader(strings.NewReader(input)), SSEOptions{
			OnEvent: func(event SSEEvent) { events = append(events, event) },
		}); err != nil {
			t.Fatal(err)
		}
		want := []SSEEvent{{ID: "7", Data: "{\"one\":\n1}"}}
		if len(events) != 1 || events[0] != want[0] {
			t.Fatalf("events = %#v, want %#v", events, want)
		}
	})

	t.Run("recognizes CR, LF, and CRLF line endings, including CRLF split across chunks", func(t *testing.T) {
		parse := func(reader io.Reader) []SSEEvent {
			t.Helper()
			var events []SSEEvent
			if err := ConsumeSSEStream(reader, SSEOptions{OnEvent: func(event SSEEvent) { events = append(events, event) }}); err != nil {
				t.Fatal(err)
			}
			return events
		}
		want := []SSEEvent{{ID: "1", Data: "a\nb"}, {Data: "c"}}
		for _, separator := range []string{"\n", "\r\n", "\r"} {
			text := strings.Join([]string{"id: 1", "data: a", "data: b", "", "data: c", "", ""}, separator)
			if got := parse(strings.NewReader(text)); !equalEvents(got, want) {
				t.Errorf("separator %q events = %#v, want %#v", separator, got, want)
			}
			// One character per read splits every CRLF between reads.
			if got := parse(iotest.OneByteReader(strings.NewReader(text))); !equalEvents(got, want) {
				t.Errorf("separator %q one-byte events = %#v, want %#v", separator, got, want)
			}
		}
		text := "id: 1\rdata: a\r\ndata: b\n\r" + "\ndata: c\r\r"
		if got := parse(strings.NewReader(text)); !equalEvents(got, want) {
			t.Errorf("mixed endings events = %#v, want %#v", got, want)
		}
	})

	t.Run("dispatches CR-terminated events while the stream stays open", func(t *testing.T) {
		reader, writer := io.Pipe()
		received := make(chan SSEEvent, 1)
		done := make(chan error, 1)
		go func() {
			done <- ConsumeSSEStream(reader, SSEOptions{OnEvent: func(event SSEEvent) {
				select {
				case received <- event:
				default:
				}
			}})
		}()
		if _, err := writer.Write([]byte("data: first\r\r")); err != nil {
			t.Fatal(err)
		}
		select {
		case event := <-received:
			if event.Data != "first" {
				t.Errorf("event = %#v", event)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("event was not dispatched before the stream closed")
		}
		writer.Close()
		<-done
	})

	t.Run("rejects events whose data lines exceed the limit without a blank line", func(t *testing.T) {
		input := strings.Repeat("data: xxxxxxxxxxxxxxxx\n", 1000)
		err := ConsumeSSEStream(strings.NewReader(input), SSEOptions{MaxEventBytes: 256, OnEvent: func(SSEEvent) {}})
		if err == nil || !strings.Contains(err.Error(), "MCP SSE event exceeds 256 bytes") {
			t.Fatalf("error = %v, want the event size error", err)
		}
	})
}

func equalEvents(got, want []SSEEvent) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

type recordedRequest struct {
	Method  string
	Header  http.Header
	Message map[string]any
}

type requestRecorder struct {
	mu       sync.Mutex
	requests []recordedRequest
}

func (r *requestRecorder) add(request recordedRequest) {
	r.mu.Lock()
	r.requests = append(r.requests, request)
	r.mu.Unlock()
}

func (r *requestRecorder) snapshot() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedRequest(nil), r.requests...)
}

func (r *requestRecorder) count(method string, predicate func(recordedRequest) bool) int {
	count := 0
	for _, request := range r.snapshot() {
		if request.Method == method && (predicate == nil || predicate(request)) {
			count++
		}
	}
	return count
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not met before the timeout")
}

func initializeResponse(id any) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]any{
			"protocolVersion": LatestProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "http-fixture", "version": "1.0.0"},
		},
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("content-type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func readMessage(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		t.Errorf("read body: %v", err)
	}
	var message map[string]any
	if err := json.Unmarshal(data, &message); err != nil {
		t.Errorf("decode message: %v", err)
	}
	return message
}

func newHTTPTransport(t *testing.T, url string, options HTTPTransportOptions) *HTTPTransport {
	t.Helper()
	options.URL = url
	transport := NewHTTPTransport(options)
	if err := transport.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transport.Close() })
	return transport
}

func TestStreamableHTTPTransport(t *testing.T) {
	t.Run("handles JSON and SSE responses with session and protocol headers", func(t *testing.T) {
		recorder := &requestRecorder{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				recorder.add(recordedRequest{Method: "GET", Header: r.Header.Clone()})
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			case http.MethodPost:
				message := readMessage(t, r)
				recorder.add(recordedRequest{Method: "POST", Header: r.Header.Clone(), Message: message})
				id, hasID := message["id"]
				if !hasID {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				switch message["method"] {
				case "initialize":
					w.Header().Set("mcp-session-id", "session-1")
					writeJSON(t, w, initializeResponse(id))
				case "tools/list":
					writeJSON(t, w, map[string]any{
						"jsonrpc": "2.0", "id": id,
						"result": map[string]any{"tools": []any{map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}},
					})
				case "tools/call":
					w.Header().Set("content-type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					fmt.Fprintf(w, "id: tool-result\n")
					payload, _ := json.Marshal(map[string]any{
						"jsonrpc": "2.0", "id": id,
						"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "hello"}}},
					})
					fmt.Fprintf(w, "data: %s\n\n", payload)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}
		}))
		defer server.Close()

		transport := NewHTTPTransport(HTTPTransportOptions{URL: server.URL})
		client := NewClient(ClientOptions{Name: "keel-test", Version: "1.0.0"})
		if _, err := client.Connect(context.Background(), transport); err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		tools, err := client.ListTools(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(tools) != 1 || tools[0].Name != "echo" {
			t.Fatalf("tools = %#v", tools)
		}
		result, err := client.CallTool(context.Background(), "echo", nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Content) != 1 || result.Content[0].Text != "hello" {
			t.Fatalf("call result = %#v", result)
		}
		var listRequest, initialized *recordedRequest
		for index := range recorder.snapshot() {
			request := recorder.snapshot()[index]
			if request.Message != nil && request.Message["method"] == "tools/list" {
				listRequest = &request
			}
			if request.Message != nil && request.Message["method"] == "notifications/initialized" {
				initialized = &request
			}
		}
		if listRequest == nil || initialized == nil {
			t.Fatalf("requests = %#v", recorder.snapshot())
		}
		if got := listRequest.Header.Get("Mcp-Session-Id"); got != "session-1" {
			t.Errorf("tools/list session header = %q", got)
		}
		if got := listRequest.Header.Get("MCP-Protocol-Version"); got != LatestProtocolVersion {
			t.Errorf("tools/list protocol header = %q", got)
		}
		if got := initialized.Header.Get("Mcp-Session-Id"); got != "session-1" {
			t.Errorf("initialized session header = %q", got)
		}
		eventually(t, time.Second, func() bool { return recorder.count("GET", nil) > 0 })
		if got := recorder.count("GET", func(request recordedRequest) bool {
			return request.Header.Get("Last-Event-ID") != ""
		}); got != 0 {
			t.Errorf("the first GET stream sent Last-Event-ID %d times", got)
		}
	})

	t.Run("classifies authentication failures", func(t *testing.T) {
		cases := []struct {
			name      string
			status    int
			challenge string
			wantAuth  bool
		}{
			{"401", http.StatusUnauthorized, `Bearer realm="x"`, true},
			// A 403 only reaches the auth provider as a step-up challenge; it is
			// not an McpAuthRequiredError, like pi's checkResponse.
			{"403 insufficient_scope", http.StatusForbidden, `Bearer error="insufficient_scope"`, false},
			{"403 other", http.StatusForbidden, `Bearer error="invalid_token"`, false},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if test.challenge != "" {
						w.Header().Set("www-authenticate", test.challenge)
					}
					w.WriteHeader(test.status)
					fmt.Fprint(w, "denied")
				}))
				defer server.Close()
				transport := newHTTPTransport(t, server.URL, HTTPTransportOptions{})
				err := transport.Send(context.Background(), &Request{JSONRPC: JSONRPCVersion, ID: NumberID(1), Method: "initialize"})
				var authErr *AuthRequiredError
				if test.wantAuth {
					if !errors.As(err, &authErr) {
						t.Fatalf("error = %v, want *AuthRequiredError", err)
					}
					return
				}
				var httpErr *HTTPError
				if !errors.As(err, &httpErr) || errors.As(err, &authErr) {
					t.Fatalf("error = %v, want a plain *HTTPError", err)
				}
			})
		}
	})

	t.Run("fails only the request whose SSE stream breaks", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			message := readMessage(t, r)
			id, hasID := message["id"]
			if !hasID {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			if message["method"] == "initialize" {
				writeJSON(t, w, initializeResponse(id))
				return
			}
			w.Header().Set("content-type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "id: broken-1\n")
			fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
			// The stream ends before the response arrives, and the GET resume
			// below offers none (405), so only this request fails.
		}))
		defer server.Close()
		transport := NewHTTPTransport(HTTPTransportOptions{
			URL:       server.URL,
			Reconnect: ReconnectOptions{InitialDelay: time.Millisecond, MaxRetries: 1},
		})
		client := NewClient(ClientOptions{Name: "keel-test", Version: "1.0.0"})
		if _, err := client.Connect(context.Background(), transport); err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		_, err := client.ListTools(context.Background())
		var mcpErr *McpError
		if !errors.As(err, &mcpErr) || mcpErr.Code != CodeInternalError || !strings.Contains(mcpErr.Message, "MCP response stream failed") {
			t.Fatalf("error = %v, want an internal MCP error about the response stream", err)
		}
	})

	t.Run("resumes a response stream the server closed before answering", func(t *testing.T) {
		recorder := &requestRecorder{}
		var idMu sync.Mutex
		var listID any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				recorder.add(recordedRequest{Method: "GET", Header: r.Header.Clone()})
				if r.Header.Get("Last-Event-ID") != "event-1" {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				idMu.Lock()
				id := listID
				idMu.Unlock()
				payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"tools": []any{}}})
				w.Header().Set("content-type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, "data: %s\n\n", payload)
				return
			}
			message := readMessage(t, r)
			id, hasID := message["id"]
			if !hasID {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			if message["method"] == "initialize" {
				writeJSON(t, w, initializeResponse(id))
				return
			}
			idMu.Lock()
			listID = id
			idMu.Unlock()
			w.Header().Set("content-type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "id: event-1\n")
			fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
		}))
		defer server.Close()
		transport := NewHTTPTransport(HTTPTransportOptions{
			URL:       server.URL,
			Reconnect: ReconnectOptions{InitialDelay: time.Millisecond},
		})
		client := NewClient(ClientOptions{Name: "keel-test", Version: "1.0.0"})
		if _, err := client.Connect(context.Background(), transport); err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		tools, err := client.ListTools(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(tools) != 0 {
			t.Fatalf("tools = %#v, want the resumed empty list", tools)
		}
		if got := recorder.count("GET", func(request recordedRequest) bool {
			return request.Header.Get("Last-Event-ID") == "event-1"
		}); got == 0 {
			t.Errorf("resume GET never carried Last-Event-ID: %#v", recorder.snapshot())
		}
	})

	t.Run("fails a request whose response stream ends without an answer", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			message := readMessage(t, r)
			id, hasID := message["id"]
			if !hasID {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			if message["method"] == "initialize" {
				writeJSON(t, w, initializeResponse(id))
				return
			}
			w.Header().Set("content-type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// No id field: the stream cannot be resumed.
		}))
		defer server.Close()
		transport := NewHTTPTransport(HTTPTransportOptions{URL: server.URL})
		client := NewClient(ClientOptions{Name: "keel-test", Version: "1.0.0"})
		if _, err := client.Connect(context.Background(), transport); err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		_, err := client.ListTools(context.Background())
		var mcpErr *McpError
		if !errors.As(err, &mcpErr) || !strings.Contains(mcpErr.Message, "stream ended without a response") {
			t.Fatalf("error = %v, want the stream-ended error", err)
		}
	})

	t.Run("rejects a request the server accepts without a response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
		}))
		defer server.Close()
		transport := newHTTPTransport(t, server.URL, HTTPTransportOptions{})
		err := transport.Send(context.Background(), &Request{JSONRPC: JSONRPCVersion, ID: NumberID(1), Method: "initialize"})
		var httpErr *HTTPError
		if !errors.As(err, &httpErr) || !strings.Contains(err.Error(), "accepted request initialize without a response") {
			t.Fatalf("error = %v, want the accepted-without-response error", err)
		}
	})

	t.Run("applies maxMessageBytes to JSON responses", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			message := readMessage(t, r)
			response := initializeResponse(message["id"])
			response["result"].(map[string]any)["serverInfo"] = map[string]any{
				"name": strings.Repeat("x", 256), "version": "1.0.0",
			}
			writeJSON(t, w, response)
		}))
		defer server.Close()
		transport := newHTTPTransport(t, server.URL, HTTPTransportOptions{MaxMessageBytes: 64})
		err := transport.Send(context.Background(), &Request{JSONRPC: JSONRPCVersion, ID: NumberID(1), Method: "initialize"})
		if err == nil || !strings.Contains(err.Error(), "MCP JSON response exceeds 64 bytes") {
			t.Fatalf("error = %v, want the JSON size error", err)
		}
	})

	t.Run("decodes JSON responses split inside a BOM and a multi-byte character", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			message := readMessage(t, r)
			response := initializeResponse(message["id"])
			response["result"].(map[string]any)["serverInfo"] = map[string]any{"name": "héllo", "version": "1.0.0"}
			payload, _ := json.Marshal(response)
			w.Header().Set("content-type", "application/json")
			// A BOM precedes the document; the multi-byte name is inside it.
			_, _ = w.Write(append([]byte{0xEF, 0xBB, 0xBF}, payload...))
		}))
		defer server.Close()
		transport := NewHTTPTransport(HTTPTransportOptions{URL: server.URL})
		client := NewClient(ClientOptions{Name: "keel-test", Version: "1.0.0"})
		result, err := client.Connect(context.Background(), transport)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		if result.ServerInfo.Name != "héllo" {
			t.Fatalf("serverInfo.name = %q", result.ServerInfo.Name)
		}
	})

	t.Run("reads only an excerpt of HTTP error bodies and cancels the rest", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, strings.Repeat("x", 20_000)+"TAIL")
		}))
		defer server.Close()
		transport := newHTTPTransport(t, server.URL, HTTPTransportOptions{})
		err := transport.Send(context.Background(), &Request{JSONRPC: JSONRPCVersion, ID: NumberID(1), Method: "initialize"})
		var httpErr *HTTPError
		if !errors.As(err, &httpErr) {
			t.Fatalf("error = %v, want *HTTPError", err)
		}
		if len(httpErr.Body) > maxHTTPErrorBodyBytes {
			t.Errorf("body excerpt = %d bytes, want <= %d", len(httpErr.Body), maxHTTPErrorBodyBytes)
		}
		if strings.Contains(httpErr.Body, "TAIL") {
			t.Error("the excerpt kept the end of the body instead of its first bytes")
		}
		if len(err.Error()) > 700 {
			t.Errorf("error message = %d chars, want a bounded message", len(err.Error()))
		}
	})

	t.Run("includes the response body in HTTP errors", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "bad request body")
		}))
		defer server.Close()
		transport := newHTTPTransport(t, server.URL, HTTPTransportOptions{})
		err := transport.Send(context.Background(), &Request{JSONRPC: JSONRPCVersion, ID: NumberID(1), Method: "initialize"})
		if err == nil || !strings.Contains(err.Error(), "bad request body") {
			t.Fatalf("error = %v, want the body in the message", err)
		}
	})

	t.Run("hands 401 and insufficient-scope 403 responses to the auth provider with the rejected token", func(t *testing.T) {
		var mu sync.Mutex
		events := []UnauthorizedEvent{}
		provider := &testAuthProvider{
			token: func(context.Context) (string, error) {
				mu.Lock()
				defer mu.Unlock()
				if len(events) == 0 {
					return "old", nil
				}
				return "new", nil
			},
			onUnauthorized: func(ctx context.Context, event UnauthorizedEvent) error {
				mu.Lock()
				events = append(events, event)
				mu.Unlock()
				return nil
			},
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer new" {
				w.Header().Set("www-authenticate", `Bearer error="insufficient_scope"`)
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, "need more scope")
				return
			}
			message := readMessage(t, r)
			id, hasID := message["id"]
			if !hasID {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			writeJSON(t, w, initializeResponse(id))
		}))
		defer server.Close()
		transport := NewHTTPTransport(HTTPTransportOptions{URL: server.URL, AuthProvider: provider})
		client := NewClient(ClientOptions{Name: "keel-test", Version: "1.0.0"})
		if _, err := client.Connect(context.Background(), transport); err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		mu.Lock()
		defer mu.Unlock()
		if len(events) != 1 {
			t.Fatalf("provider saw %d unauthorized events, want 1", len(events))
		}
		if events[0].Status != http.StatusForbidden || events[0].Token != "old" || events[0].WWWAuthenticate == "" {
			t.Errorf("event = %#v", events[0])
		}
	})

	t.Run("closes the session with the last request's token, without asking the auth provider", func(t *testing.T) {
		var mu sync.Mutex
		tokenCalls := 0
		var deleteAuth string
		provider := &testAuthProvider{
			token: func(context.Context) (string, error) {
				mu.Lock()
				tokenCalls++
				mu.Unlock()
				return "session-token", nil
			},
			onUnauthorized: func(context.Context, UnauthorizedEvent) error { return nil },
		}
		recorder := &requestRecorder{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			recorder.add(recordedRequest{Method: r.Method, Header: r.Header.Clone()})
			switch r.Method {
			case http.MethodDelete:
				mu.Lock()
				deleteAuth = r.Header.Get("Authorization")
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
				return
			case http.MethodGet:
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			message := readMessage(t, r)
			if _, hasID := message["id"]; !hasID {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			w.Header().Set("mcp-session-id", "session-1")
			writeJSON(t, w, initializeResponse(message["id"]))
		}))
		defer server.Close()
		transport := NewHTTPTransport(HTTPTransportOptions{URL: server.URL, AuthProvider: provider})
		client := NewClient(ClientOptions{Name: "keel-test", Version: "1.0.0"})
		if _, err := client.Connect(context.Background(), transport); err != nil {
			t.Fatal(err)
		}
		eventually(t, time.Second, func() bool { return recorder.count("GET", nil) > 0 })
		mu.Lock()
		before := tokenCalls
		mu.Unlock()
		if err := transport.Close(); err != nil {
			t.Fatal(err)
		}
		eventually(t, time.Second, func() bool { return recorder.count("DELETE", nil) > 0 })
		mu.Lock()
		after := tokenCalls
		auth := deleteAuth
		mu.Unlock()
		if after != before {
			t.Errorf("Close asked the auth provider for a token (%d -> %d)", before, after)
		}
		if auth != "Bearer session-token" {
			t.Errorf("DELETE Authorization = %q, want the last request's token", auth)
		}
	})

	t.Run("classifies an expired established session", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			case http.MethodDelete:
				w.WriteHeader(http.StatusOK)
				return
			}
			message := readMessage(t, r)
			id, hasID := message["id"]
			if !hasID {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			if message["method"] == "initialize" {
				w.Header().Set("mcp-session-id", "session-1")
				writeJSON(t, w, initializeResponse(id))
				return
			}
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "no such session")
		}))
		defer server.Close()
		transport := NewHTTPTransport(HTTPTransportOptions{URL: server.URL})
		client := NewClient(ClientOptions{Name: "keel-test", Version: "1.0.0"})
		if _, err := client.Connect(context.Background(), transport); err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		_, err := client.ListTools(context.Background())
		var expired *SessionExpiredError
		if !errors.As(err, &expired) {
			t.Fatalf("error = %v, want *SessionExpiredError", err)
		}
	})
}

type testAuthProvider struct {
	token          func(context.Context) (string, error)
	onUnauthorized func(context.Context, UnauthorizedEvent) error
}

func (p *testAuthProvider) Token(ctx context.Context) (string, error) { return p.token(ctx) }

func (p *testAuthProvider) OnUnauthorized(ctx context.Context, event UnauthorizedEvent) error {
	return p.onUnauthorized(ctx, event)
}
