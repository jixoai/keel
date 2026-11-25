// MCP events tests (change feat-mcp-apps-events, P4). Fake servers run over the
// in-memory transport; one HTTP fake server exercises the Streamable HTTP
// response-stream form of subscriptions/listen. No test sleeps for
// synchronization: every wait is a channel or a bounded condition poll.

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
	"time"
)

// sendNotification writes one server notification to the client.
func sendNotification(t *testing.T, transport *MemoryTransport, method string, params any) {
	t.Helper()
	sendNotificationFrom(t, transport, "", method, params)
}

// sendNotificationFrom writes one notification with transport source metadata
// (empty = connection stream, responseStreamSource(id) = request response
// stream). The in-memory transport preserves Source out of band.
func sendNotificationFrom(t *testing.T, transport *MemoryTransport, source, method string, params any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var raw json.RawMessage
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("encode notification params: %v", err)
		}
		raw = encoded
	}
	if err := transport.Send(ctx, &Notification{JSONRPC: JSONRPCVersion, Method: method, Params: raw, Source: source}); err != nil {
		t.Fatalf("send notification %s: %v", method, err)
	}
}

// sendServerRequest writes one server-initiated request to the client.
func sendServerRequest(t *testing.T, transport *MemoryTransport, id ID, method string, params any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var raw json.RawMessage
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("encode request params: %v", err)
		}
		raw = encoded
	}
	if err := transport.Send(ctx, &Request{JSONRPC: JSONRPCVersion, ID: id, Method: method, Params: raw}); err != nil {
		t.Fatalf("send server request %s: %v", method, err)
	}
}

// subscriptionIDParams builds a notification params object tagged with the
// listen request id (the wire contract of the 2026-07-28 subscriptions
// pattern).
func subscriptionIDParams(id ID, body string) json.RawMessage {
	if body == "" || body == "{}" {
		return json.RawMessage(fmt.Sprintf(`{"_meta":{"%s":%s}}`, SubscriptionIDMetaKey, id))
	}
	inner := strings.TrimSpace(body)
	if strings.HasPrefix(inner, "{") {
		inner = strings.TrimPrefix(inner, "{")
		if inner == "" {
			return json.RawMessage(fmt.Sprintf(`{"_meta":{"%s":%s}}`, SubscriptionIDMetaKey, id))
		}
		return json.RawMessage(fmt.Sprintf(`{%s,"_meta":{"%s":%s}}`, inner, SubscriptionIDMetaKey, id))
	}
	return json.RawMessage(body)
}

// ackParams builds the acknowledged frame: the accepted subset under
// `notifications` plus the subscription tag.
func ackParams(id ID, accepted string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"notifications":%s,"_meta":{"%s":%s}}`, accepted, SubscriptionIDMetaKey, id))
}

// waitNotification returns the next server-to-client message of a method.
func waitNotification(t *testing.T, server *testServer, method string) *Notification {
	t.Helper()
	var found *Notification
	ok := waitFor(t, 2*time.Second, func() bool {
		for _, message := range server.Messages() {
			if notification, ok := message.(*Notification); ok && notification.Method == method {
				found = notification
				return true
			}
		}
		return false
	})
	if !ok {
		t.Fatalf("server never saw notification %s; messages=%#v", method, server.Messages())
	}
	return found
}

func TestClientWithoutCallbacksKeepsPreP4Behaviour(t *testing.T) {
	clientTransport, server := newTestServer(t)
	client := connectTestClient(t, clientTransport)
	server.neverRespond("sampling/createMessage")

	// A notification must be dropped and a server request answered -32601: the
	// default is fail closed and byte-identical to the pre-P4 client.
	sendNotification(t, server.transport, "notifications/progress", map[string]any{
		"progressToken": "t", "progress": 1,
	})
	sendServerRequest(t, server.transport, NumberID(77), "sampling/createMessage", map[string]any{})

	var response *ErrorResponse
	ok := waitFor(t, 2*time.Second, func() bool {
		for _, message := range server.Messages() {
			if candidate, ok := message.(*ErrorResponse); ok && candidate.ID == NumberID(77) {
				response = candidate
				return true
			}
		}
		return false
	})
	if !ok {
		t.Fatalf("client never answered the server request; messages=%#v", server.Messages())
	}
	if response.Error.Code != CodeMethodNotFound {
		t.Fatalf("error code = %d, want %d", response.Error.Code, CodeMethodNotFound)
	}
	// The connection is still healthy and the progress frame changed nothing.
	if client.State() != ClientConnected {
		t.Fatalf("state = %q, want connected", client.State())
	}
}

func TestClientCallbacksReceiveNotificationsAndAnswerRequests(t *testing.T) {
	clientTransport, server := newTestServer(t)
	notifications := make(chan Notification, 4)
	client := NewClient(ClientOptions{
		Name: "test-client", Version: "2.0.0",
		Notifications: func(_ context.Context, method string, params json.RawMessage) {
			notifications <- Notification{Method: method, Params: params}
		},
		ServerRequests: func(_ context.Context, request *Request) (Message, error) {
			return &SuccessResponse{JSONRPC: JSONRPCVersion, ID: request.ID, Result: json.RawMessage(`{"ok":true}`)}, nil
		},
	})
	if _, err := client.Connect(testContext(t), clientTransport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	sendNotification(t, server.transport, "notifications/message", map[string]any{"level": "info", "data": "hello"})
	select {
	case notification := <-notifications:
		if notification.Method != "notifications/message" {
			t.Fatalf("method = %q", notification.Method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notification callback was not invoked")
	}

	sendServerRequest(t, server.transport, NumberID(88), "ping", nil)
	ok := waitFor(t, 2*time.Second, func() bool {
		for _, message := range server.Messages() {
			if response, ok := message.(*SuccessResponse); ok && response.ID == NumberID(88) {
				return true
			}
		}
		return false
	})
	if !ok {
		t.Fatalf("server never received the handler's response; messages=%#v", server.Messages())
	}
}

func TestMemoryTransportPreservesNotificationSource(t *testing.T) {
	clientTransport, serverTransport := NewMemoryTransportPair()
	if err := serverTransport.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := clientTransport.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = clientTransport.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := serverTransport.Send(ctx, &Notification{
		JSONRPC: JSONRPCVersion, Method: "notifications/progress", Source: "response:n:7",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	message, err := clientTransport.Receive(ctx)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	notification, ok := message.(*Notification)
	if !ok || notification.Source != "response:n:7" {
		t.Fatalf("message = %#v, want source response:n:7", message)
	}
}

func TestClientListenAckFilterAndTagging(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.neverRespond("subscriptions/listen")
	client := connectTestClient(t, clientTransport)
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	client.now = clock.Now
	var diagnostics []string
	var diagnosticsMu sync.Mutex
	handleCh := make(chan *ListenHandle, 1)
	errCh := make(chan error, 1)
	go func() {
		handle, err := client.Listen(testContext(t), ListenParams{Notifications: ListenFilter{
			ToolsListChanged: true, PromptsListChanged: true, ResourcesListChanged: true,
		}}, ListenOptions{OnDiagnostic: func(message string) {
			diagnosticsMu.Lock()
			diagnostics = append(diagnostics, message)
			diagnosticsMu.Unlock()
		}})
		if err != nil {
			errCh <- err
			return
		}
		handleCh <- handle
	}()

	requests := waitForRequests(t, server, "subscriptions/listen")
	var request *Request
	for _, candidate := range requests {
		request = candidate
	}
	// The first frame must be the ack; this one accepts only toolsListChanged.
	sendNotification(t, server.transport, "notifications/subscriptions/acknowledged",
		ackParams(request.ID, `{"toolsListChanged":true}`))

	var handle *ListenHandle
	select {
	case handle = <-handleCh:
	case err := <-errCh:
		t.Fatalf("listen: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("listen never acknowledged")
	}
	if !handle.Ack.Accepted.ToolsListChanged {
		t.Fatalf("ack = %+v, want toolsListChanged", handle.Ack)
	}
	if len(handle.Ack.Missing) != 2 || handle.Ack.Missing[0] != "promptsListChanged" {
		t.Fatalf("missing = %v, want [promptsListChanged resourcesListChanged]", handle.Ack.Missing)
	}
	if handle.Ack.SubscriptionID == "" {
		t.Fatal("ack carried no subscription id")
	}

	// Opted-in category is delivered.
	sendNotification(t, server.transport, "notifications/tools/list_changed",
		subscriptionIDParams(request.ID, ""))
	select {
	case event := <-handle.Events:
		if event.Method != "notifications/tools/list_changed" {
			t.Fatalf("event = %q", event.Method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("opted-in notification was not delivered")
	}

	// Not opted in: diagnosed, never delivered.
	sendNotification(t, server.transport, "notifications/resources/list_changed",
		subscriptionIDParams(request.ID, ""))
	awaitDiagnostic(t, func() string {
		diagnosticsMu.Lock()
		defer diagnosticsMu.Unlock()
		return strings.Join(diagnostics, "\n")
	}, "outside the accepted subscription")
	select {
	case event := <-handle.Events:
		t.Fatalf("unexpected notification delivered: %q", event.Method)
	default:
	}

	// An untagged notification on the connection stream is not a listen change;
	// the listener must not see it (the global dispatcher may). A source-less
	// transport cannot tell it from a listen frame, so a directory change is
	// diagnosed. Advance the sampling clock between warnings so each is emitted.
	clock.Advance(1100 * time.Millisecond)
	sendNotification(t, server.transport, "notifications/tools/list_changed", nil)
	awaitDiagnostic(t, func() string {
		diagnosticsMu.Lock()
		defer diagnosticsMu.Unlock()
		return strings.Join(diagnostics, "\n")
	}, "untagged directory notification")
	select {
	case event := <-handle.Events:
		t.Fatalf("untagged connection-stream notification delivered to the listener: %q", event.Method)
	default:
	}
	// An untagged notification on the listen stream itself is a protocol
	// violation: diagnosed, never delivered. Advance the sampling clock after the
	// previous warning was emitted so this one cannot fold into it.
	clock.Advance(1100 * time.Millisecond)
	sendNotificationFrom(t, server.transport, responseStreamSource(request.ID),
		"notifications/tools/list_changed", nil)
	awaitDiagnostic(t, func() string {
		diagnosticsMu.Lock()
		defer diagnosticsMu.Unlock()
		return strings.Join(diagnostics, "\n")
	}, "untagged notification on a listen stream")
	select {
	case event := <-handle.Events:
		t.Fatalf("untagged listen-stream notification delivered: %q", event.Method)
	default:
	}
	handle.Close()
}

func TestClientListenIgnoresFramesBeforeAck(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.neverRespond("subscriptions/listen")
	client := connectTestClient(t, clientTransport)

	diagnostics := make(chan string, 4)
	handleCh := make(chan *ListenHandle, 1)
	go func() {
		handle, err := client.Listen(testContext(t), ListenParams{Notifications: ListenFilter{ToolsListChanged: true}},
			ListenOptions{OnDiagnostic: func(message string) { diagnostics <- message }})
		if err == nil {
			handleCh <- handle
		}
	}()
	requests := waitForRequests(t, server, "subscriptions/listen")
	request := requests[len(requests)-1]
	// A change frame before the ack is a protocol violation, not a change.
	sendNotification(t, server.transport, "notifications/tools/list_changed", subscriptionIDParams(request.ID, ""))
	sendNotification(t, server.transport, "notifications/subscriptions/acknowledged",
		ackParams(request.ID, `{"toolsListChanged":true}`))
	var handle *ListenHandle
	select {
	case handle = <-handleCh:
	case <-time.After(2 * time.Second):
		t.Fatal("listen never acknowledged")
	}
	defer handle.Close()
	awaitDiagnostic(t, func() string {
		select {
		case message := <-diagnostics:
			return message
		default:
			return ""
		}
	}, "before acknowledgement")
	select {
	case event := <-handle.Events:
		t.Fatalf("pre-ack frame delivered: %q", event.Method)
	default:
	}
}

func TestClientListenRoutesMultipleSubscriptionsByID(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.neverRespond("subscriptions/listen")
	client := connectTestClient(t, clientTransport)

	opened := make(chan *ListenHandle, 2)
	failed := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			handle, err := client.Listen(testContext(t), ListenParams{Notifications: ListenFilter{ToolsListChanged: true}}, ListenOptions{})
			if err != nil {
				failed <- err
				return
			}
			opened <- handle
		}()
	}
	requests := waitForRequests(t, server, "subscriptions/listen")
	if !waitFor(t, 2*time.Second, func() bool { return len(server.requests("subscriptions/listen")) == 2 }) {
		t.Fatalf("listen requests = %d, want 2", len(server.requests("subscriptions/listen")))
	}
	requests = server.requests("subscriptions/listen")
	for _, request := range requests {
		sendNotification(t, server.transport, "notifications/subscriptions/acknowledged",
			ackParams(request.ID, `{"toolsListChanged":true}`))
	}
	handles := make([]*ListenHandle, 0, 2)
	for i := 0; i < 2; i++ {
		select {
		case handle := <-opened:
			handles = append(handles, handle)
		case err := <-failed:
			t.Fatalf("listen: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("listen never acknowledged")
		}
	}
	// The wire requests are ordered; match each handle to its request by the
	// subscription id the server echoed in the ack.
	bySubscription := make(map[string]*ListenHandle, 2)
	for _, handle := range handles {
		bySubscription[handle.Ack.SubscriptionID] = handle
	}
	first := bySubscription[requests[0].ID.Key()]
	second := bySubscription[requests[1].ID.Key()]
	if first == nil || second == nil {
		t.Fatalf("ack ids did not match the listen requests: %+v vs %s/%s",
			handles, requests[0].ID, requests[1].ID)
	}
	// A change tagged with the first subscription reaches exactly that stream.
	sendNotification(t, server.transport, "notifications/tools/list_changed", subscriptionIDParams(requests[0].ID, ""))
	select {
	case event := <-first.Events:
		if event.Method != "notifications/tools/list_changed" {
			t.Fatalf("event = %q", event.Method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tagged notification did not reach its stream")
	}
	select {
	case event := <-second.Events:
		t.Fatalf("notification leaked to the other subscription: %q", event.Method)
	default:
	}
	for _, handle := range handles {
		handle.Close()
	}
}

func TestClientListenCloseCancelsSubscription(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.neverRespond("subscriptions/listen")
	client := connectTestClient(t, clientTransport)

	done := make(chan *ListenHandle, 1)
	go func() {
		handle, err := client.Listen(testContext(t), ListenParams{Notifications: ListenFilter{ToolsListChanged: true}}, ListenOptions{})
		if err == nil {
			done <- handle
		}
	}()
	requests := waitForRequests(t, server, "subscriptions/listen")
	sendNotification(t, server.transport, "notifications/subscriptions/acknowledged",
		ackParams(requests[0].ID, `{"toolsListChanged":true}`))
	var handle *ListenHandle
	select {
	case handle = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("listen never acknowledged")
	}
	handle.Close()
	cancelled := waitNotification(t, server, "notifications/cancelled")
	var payload struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if err := json.Unmarshal(cancelled.Params, &payload); err != nil {
		t.Fatalf("decode cancellation: %v", err)
	}
	var id ID
	if err := json.Unmarshal(payload.RequestID, &id); err != nil || id.Key() != requests[0].ID.Key() {
		t.Fatalf("cancelled requestId = %s, want %s", payload.RequestID, requests[0].ID)
	}
}

// TestClientListenOverHTTP exercises the modern Streamable HTTP form: the
// listen POST is answered with an SSE stream whose first frame is the ack, and
// the request carries no Last-Event-ID (the modern era has no resumption).
func TestClientListenOverHTTP(t *testing.T) {
	streamHold := make(chan struct{})
	listenHeaders := make(chan http.Header, 1)
	var listenBody struct {
		Method string `json:"method"`
		ID     ID     `json:"id"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// No legacy GET stream in this fixture.
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		case http.MethodDelete:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var message struct {
			Method string          `json:"method"`
			ID     ID              `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(body, &message); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch message.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":%q,"capabilities":{"tools":{"listChanged":true}},"serverInfo":{"name":"http-fixture","version":"1"}}}`,
				message.ID, LatestProtocolVersion)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "subscriptions/listen":
			listenHeaders <- r.Header.Clone()
			listenBody.Method = message.Method
			listenBody.ID = message.ID
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			ack := fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":%s}`,
				ackParams(message.ID, `{"toolsListChanged":true}`))
			_, _ = fmt.Fprintf(w, "data: %s\n\n", ack)
			if flusher != nil {
				flusher.Flush()
			}
			change := fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":%s}`,
				subscriptionIDParams(message.ID, ""))
			_, _ = fmt.Fprintf(w, "data: %s\n\n", change)
			if flusher != nil {
				flusher.Flush()
			}
			// Keep the stream open until the test releases it; the server must
			// not resend or end eagerly.
			select {
			case <-streamHold:
			case <-r.Context().Done():
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}`, message.ID)
		}
	}))
	defer server.Close()
	defer close(streamHold)

	transport := NewHTTPTransport(HTTPTransportOptions{URL: server.URL, DisableGetStream: true})
	// The fixture's notifications/initialized POST answers 202, not a reply.
	client := NewClient(ClientOptions{Name: "http-client", Version: "1", RequestTimeout: 2 * time.Second})
	if _, err := client.Connect(context.Background(), transport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	handle, err := client.Listen(testContext(t), ListenParams{Notifications: ListenFilter{ToolsListChanged: true}}, ListenOptions{})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if !handle.Ack.Accepted.ToolsListChanged {
		t.Fatalf("ack = %+v", handle.Ack)
	}
	select {
	case event := <-handle.Events:
		if event.Method != "notifications/tools/list_changed" {
			t.Fatalf("event = %q", event.Method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SSE notification was not delivered")
	}
	select {
	case headers := <-listenHeaders:
		if headers.Get("Last-Event-ID") != "" {
			t.Fatalf("listen request carried Last-Event-ID=%q", headers.Get("Last-Event-ID"))
		}
	case <-time.After(time.Second):
		t.Fatal("listen request headers were not captured")
	}
	if listenBody.Method != "subscriptions/listen" || listenBody.ID.IsZero() {
		t.Fatalf("listen body = %+v", listenBody)
	}
	handle.Close()
}

func TestClientProgressCorrelatesPerCall(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.neverRespond("tools/call")
	client := NewClient(ClientOptions{
		Name: "test-client", Version: "2.0.0",
		// A dispatcher is wired: that is what enables progress classification.
		Notifications: func(context.Context, string, json.RawMessage) {},
	})
	if _, err := client.Connect(testContext(t), clientTransport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	type callOutcome struct {
		result CallToolResult
		err    error
	}
	outcomes := make(chan callOutcome, 2)
	var progressMu sync.Mutex
	progress := map[string][]ProgressNotification{}
	start := func(token, label string) {
		go func() {
			result, err := client.CallToolWithOptions(testContext(t), "slow", nil, CallOptions{
				ProgressToken: token,
				OnProgress: func(notification ProgressNotification) {
					progressMu.Lock()
					progress[label] = append(progress[label], notification)
					progressMu.Unlock()
				},
			})
			outcomes <- callOutcome{result: result, err: err}
		}()
	}
	// Both callers ask for the same token: the client must disambiguate, so a
	// frame cannot reach the other call.
	start("same-token", "a")
	start("same-token", "b")

	var requests []*Request
	if !waitFor(t, 2*time.Second, func() bool {
		requests = server.requests("tools/call")
		return len(requests) >= 2
	}) {
		t.Fatalf("tools/call requests = %d, want 2", len(requests))
	}
	tokens := make([]string, 0, 2)
	for _, request := range requests {
		var params struct {
			Meta struct {
				ProgressToken string `json:"progressToken"`
			} `json:"_meta"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			t.Fatalf("decode tools/call: %v", err)
		}
		if params.Meta.ProgressToken == "" {
			t.Fatal("tools/call carried no progress token")
		}
		tokens = append(tokens, params.Meta.ProgressToken)
	}
	if tokens[0] == tokens[1] {
		t.Fatalf("the same wire token was used for two calls: %q", tokens[0])
	}
	for index, token := range tokens {
		sendNotification(t, server.transport, "notifications/progress", map[string]any{
			"progressToken": token, "progress": float64(index + 1), "message": fmt.Sprintf("step-%d", index),
		})
	}
	// Each sink only sees its own frame (the correlation is by wire token).
	if !waitFor(t, 2*time.Second, func() bool {
		progressMu.Lock()
		defer progressMu.Unlock()
		return len(progress["a"]) == 1 && len(progress["b"]) == 1
	}) {
		progressMu.Lock()
		got := fmt.Sprintf("a=%d b=%d", len(progress["a"]), len(progress["b"]))
		progressMu.Unlock()
		t.Fatalf("progress was not correlated: %s", got)
	}
	progressMu.Lock()
	// One frame each, and they are different frames: a crossed routing would put
	// both frames in one sink (or both in each sink).
	if len(progress["a"]) != 1 || len(progress["b"]) != 1 ||
		progress["a"][0].Message == progress["b"][0].Message {
		t.Fatalf("progress crossed calls: a=%+v b=%+v", progress["a"], progress["b"])
	}
	progressMu.Unlock()
	// Answer both calls so the goroutines finish.
	for _, request := range requests {
		server.respond(request.ID, map[string]any{"content": []any{}}, nil)
	}
	for i := 0; i < 2; i++ {
		select {
		case outcome := <-outcomes:
			if outcome.err != nil {
				t.Fatalf("call failed: %v", outcome.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("call did not settle")
		}
	}
}

// TestClientRequestDomainNotificationsNotSwallowedByListen is the P1
// regression: progress/cancelled/message are request/connection-domain frames
// and are untagged by contract. An open listen stream must not swallow them.
func TestClientRequestDomainNotificationsNotSwallowedByListen(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.neverRespond("subscriptions/listen")
	server.neverRespond("tools/call")
	notifications := make(chan Notification, 4)
	client := NewClient(ClientOptions{
		Name: "test-client", Version: "2.0.0",
		Notifications: func(_ context.Context, method string, params json.RawMessage) {
			notifications <- Notification{Method: method, Params: params}
		},
	})
	if _, err := client.Connect(testContext(t), clientTransport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// An established listen stream is active for the whole test.
	listenDone := make(chan *ListenHandle, 1)
	go func() {
		handle, err := client.Listen(testContext(t), ListenParams{Notifications: ListenFilter{ToolsListChanged: true}}, ListenOptions{})
		if err == nil {
			listenDone <- handle
		}
	}()
	listenRequests := waitForRequests(t, server, "subscriptions/listen")
	sendNotification(t, server.transport, "notifications/subscriptions/acknowledged",
		ackParams(listenRequests[0].ID, `{"toolsListChanged":true}`))
	var handle *ListenHandle
	select {
	case handle = <-listenDone:
	case <-time.After(2 * time.Second):
		t.Fatal("listen never acknowledged")
	}
	defer handle.Close()

	// One in-flight tool call with a progress token.
	var progressMu sync.Mutex
	var progress []ProgressNotification
	callErr := make(chan error, 1)
	go func() {
		_, err := client.CallToolWithOptions(testContext(t), "slow", nil, CallOptions{
			ProgressToken: "call-progress",
			OnProgress: func(notification ProgressNotification) {
				progressMu.Lock()
				progress = append(progress, notification)
				progressMu.Unlock()
			},
		})
		callErr <- err
	}()
	callRequests := waitForRequests(t, server, "tools/call")
	callRequest := callRequests[0]
	var callParams struct {
		Meta struct {
			ProgressToken string `json:"progressToken"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(callRequest.Params, &callParams); err != nil {
		t.Fatalf("decode tools/call: %v", err)
	}

	// Untagged progress on the call's own response stream reaches the sink.
	sendNotificationFrom(t, server.transport, responseStreamSource(callRequest.ID),
		"notifications/progress", map[string]any{
			"progressToken": callParams.Meta.ProgressToken, "progress": 1, "message": "step",
		})
	if !waitFor(t, 2*time.Second, func() bool {
		progressMu.Lock()
		defer progressMu.Unlock()
		return len(progress) == 1
	}) {
		t.Fatal("untagged progress did not reach its sink while a listen stream was open")
	}

	// Untagged server log on the connection stream reaches the dispatcher.
	sendNotification(t, server.transport, "notifications/message", map[string]any{"level": "info"})
	deadline := time.After(2 * time.Second)
	for {
		select {
		case notification := <-notifications:
			if notification.Method == "notifications/message" {
				deadline = nil
				break
			}
			// The progress frame also reaches the global dispatcher (metrics/log
			// classification); keep reading until the log frame arrives.
		case <-deadline:
			t.Fatal("untagged message did not reach the dispatcher")
		}
		if deadline == nil {
			break
		}
	}

	// Untagged cancellation still aborts the in-flight call.
	sendNotification(t, server.transport, "notifications/cancelled", map[string]any{
		"requestId": mustMarshalID(callRequest.ID), "reason": "stop",
	})
	select {
	case err := <-callErr:
		var abort *AbortError
		if !errors.As(err, &abort) {
			t.Fatalf("call error = %v, want AbortError", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("untagged cancellation did not abort the call")
	}
}

// fakeClock is a manually advanced clock for the diagnostic sampler.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// TestClientDiagnosticSampling: repeated protocol warnings are folded to one
// per second with a suppressed count (P2-f).
func TestClientDiagnosticSampling(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.neverRespond("subscriptions/listen")
	client := connectTestClient(t, clientTransport)
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	client.now = clock.Now

	diagnostics := make(chan string, 16)
	handleCh := make(chan *ListenHandle, 1)
	go func() {
		handle, err := client.Listen(testContext(t), ListenParams{Notifications: ListenFilter{ToolsListChanged: true}},
			ListenOptions{Buffer: 1, OnDiagnostic: func(message string) { diagnostics <- message }})
		if err == nil {
			handleCh <- handle
		}
	}()
	requests := waitForRequests(t, server, "subscriptions/listen")
	request := requests[0]
	sendNotification(t, server.transport, "notifications/subscriptions/acknowledged",
		ackParams(request.ID, `{"toolsListChanged":true}`))
	var handle *ListenHandle
	select {
	case handle = <-handleCh:
	case <-time.After(2 * time.Second):
		t.Fatal("listen never acknowledged")
	}
	defer handle.Close()

	// The first frame fills the buffer; the next three drops are one sampled
	// warning with suppressed=0, then two folded.
	for i := 0; i < 4; i++ {
		sendNotification(t, server.transport, "notifications/tools/list_changed", subscriptionIDParams(request.ID, ""))
	}
	first := awaitMessage(t, diagnostics)
	if !strings.Contains(first, "queue full") || !strings.Contains(first, "(suppressed=0)") {
		t.Fatalf("first diagnostic = %q", first)
	}
	// The same second: no further warning.
	select {
	case message := <-diagnostics:
		t.Fatalf("a second warning was emitted within the window: %q", message)
	case <-time.After(50 * time.Millisecond):
	}
	clock.Advance(1100 * time.Millisecond)
	sendNotification(t, server.transport, "notifications/tools/list_changed", subscriptionIDParams(request.ID, ""))
	second := awaitMessage(t, diagnostics)
	if !strings.Contains(second, "(suppressed=2)") {
		t.Fatalf("second diagnostic = %q, want suppressed=2", second)
	}
}

func awaitMessage(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case message := <-ch:
		return message
	case <-time.After(2 * time.Second):
		t.Fatal("no diagnostic was emitted")
		return ""
	}
}

func TestClientCancellationAbortsPendingCall(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.neverRespond("tools/call")
	client := NewClient(ClientOptions{
		Name: "test-client", Version: "2.0.0",
		Notifications: func(context.Context, string, json.RawMessage) {},
	})
	if _, err := client.Connect(testContext(t), clientTransport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	errCh := make(chan error, 1)
	go func() {
		_, err := client.CallTool(testContext(t), "slow", nil)
		errCh <- err
	}()
	requests := waitForRequests(t, server, "tools/call")
	sendNotification(t, server.transport, "notifications/cancelled", map[string]any{
		"requestId": mustMarshalID(requests[0].ID), "reason": "user aborted",
	})
	select {
	case err := <-errCh:
		var abort *AbortError
		if !errors.As(err, &abort) {
			t.Fatalf("error = %v, want AbortError", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled call was not aborted")
	}
}

func TestClientCallWithoutTokenKeepsWireShape(t *testing.T) {
	clientTransport, server := newTestServer(t)
	var params json.RawMessage
	server.setHandler("tools/call", func(raw json.RawMessage) (any, *ErrorObject) {
		params = append(json.RawMessage(nil), raw...)
		return map[string]any{"content": []any{}}, nil
	})
	client := connectTestClient(t, clientTransport)
	if _, err := client.CallTool(testContext(t), "echo", nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(params, &fields); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if _, hasMeta := fields["_meta"]; hasMeta {
		t.Fatalf("tokenless call carried _meta: %s", params)
	}
}

// TestHTTPTransportSignalsLegacyStreamReconnect pins the legacy half of the
// reconnect contract: a GET stream that drops and is re-established reports
// OnReconnect exactly once (the host then recomputes its catalogue).
func TestHTTPTransportSignalsLegacyStreamReconnect(t *testing.T) {
	var gets int
	var mu sync.Mutex
	hold := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			mu.Lock()
			gets++
			attempt := gets
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if attempt == 1 {
				// The first stream carries one event id and ends immediately.
				_, _ = fmt.Fprint(w, "id: 1\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{\"level\":\"info\"}}\n\n")
				return
			}
			// The second stream stays open until the test ends.
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-hold
			return
		}
		body, _ := io.ReadAll(r.Body)
		var message struct {
			Method string `json:"method"`
			ID     ID     `json:"id"`
		}
		_ = json.Unmarshal(body, &message)
		switch message.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":%q,"capabilities":{},"serverInfo":{"name":"reconnect-fixture","version":"1"}}}`,
				message.ID, LatestProtocolVersion)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer server.Close()
	// Registered after server.Close, so it runs first and releases the handler.
	defer close(hold)

	reconnected := make(chan struct{}, 1)
	transport := NewHTTPTransport(HTTPTransportOptions{
		URL:       server.URL,
		Reconnect: ReconnectOptions{InitialDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond},
		OnReconnect: func() {
			select {
			case reconnected <- struct{}{}:
			default:
			}
		},
	})
	client := NewClient(ClientOptions{Name: "reconnect-client", Version: "1"})
	if _, err := client.Connect(context.Background(), transport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	select {
	case <-reconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("OnReconnect was not called after the GET stream was re-established")
	}
	select {
	case <-reconnected:
		t.Fatal("OnReconnect fired more than once for a single reconnect")
	case <-time.After(50 * time.Millisecond):
	}
}

// awaitDiagnostic waits until snapshot() contains want.
func awaitDiagnostic(t *testing.T, snapshot func() string, want string) {
	t.Helper()
	if !waitFor(t, 2*time.Second, func() bool { return strings.Contains(snapshot(), want) }) {
		t.Fatalf("diagnostic %q missing; got %q", want, snapshot())
	}
}

// TestHTTPListenCloseCancelsServerStream pins the server-visible half of the
// HTTP cancellation form: closing the listen handle closes the response stream,
// which the server observes as a cancelled request context. No cancelled
// notification is needed (that is the stdio form).
func TestHTTPListenCloseCancelsServerStream(t *testing.T) {
	canceled := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var message struct {
			Method string `json:"method"`
			ID     ID     `json:"id"`
		}
		_ = json.Unmarshal(body, &message)
		switch message.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":%q,"capabilities":{},"serverInfo":{"name":"close-fixture","version":"1"}}}`,
				message.ID, LatestProtocolVersion)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "subscriptions/listen":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `data: {"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":%s}`+"\n\n",
				ackParams(message.ID, `{"toolsListChanged":true}`))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-r.Context().Done()
			select {
			case canceled <- struct{}{}:
			default:
			}
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer server.Close()

	transport := NewHTTPTransport(HTTPTransportOptions{URL: server.URL, DisableGetStream: true})
	client := NewClient(ClientOptions{Name: "close-client", Version: "1", RequestTimeout: 2 * time.Second})
	if _, err := client.Connect(context.Background(), transport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	handle, err := client.Listen(testContext(t), ListenParams{Notifications: ListenFilter{ToolsListChanged: true}}, ListenOptions{})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if !handle.Ack.Accepted.ToolsListChanged {
		t.Fatalf("ack = %+v", handle.Ack)
	}
	handle.Close()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("the server never observed the listen stream being closed")
	}
}

// waitForRequests waits until the server saw at least one request of method.
func waitForRequests(t *testing.T, server *testServer, method string) []*Request {
	t.Helper()
	var requests []*Request
	if !waitFor(t, 2*time.Second, func() bool {
		requests = server.requests(method)
		return len(requests) > 0
	}) {
		t.Fatalf("server never saw %s; messages=%#v", method, server.Messages())
	}
	return requests
}
