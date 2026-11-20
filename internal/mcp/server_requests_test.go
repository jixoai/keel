// Legacy server-request concurrency tests (P5.2 review fix): answering a
// server-initiated request runs off the receive loop, so a handler that waits
// on a user decision cannot freeze notifications or responses. The handler
// count is bounded and an overflow is answered, never dropped.

package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLegacyServerRequestDoesNotBlockTheReceiveLoop(t *testing.T) {
	clientTransport, server := newTestServer(t)
	block := make(chan struct{})
	var release sync.Once
	releaseBlock := func() { release.Do(func() { close(block) }) }
	defer releaseBlock()

	started := make(chan struct{}, maxConcurrentServerRequests)
	notifications := make(chan string, 4)
	client := NewClient(ClientOptions{
		Name: "test-client", Version: "2.0.0",
		Notifications: func(_ context.Context, method string, _ json.RawMessage) {
			notifications <- method
		},
		ServerRequests: func(ctx context.Context, request *Request) (Message, error) {
			started <- struct{}{}
			select {
			case <-block:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &SuccessResponse{
				JSONRPC: JSONRPCVersion,
				ID:      request.ID,
				Result:  json.RawMessage(`{"ok":true}`),
			}, nil
		},
	})
	if _, err := client.Connect(testContext(t), clientTransport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	server.setHandler("tools/list", func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{"tools": []any{}}, nil
	})

	// Fill every handler slot with a request whose handler is blocked.
	for index := 0; index < maxConcurrentServerRequests; index++ {
		sendServerRequest(t, server.transport, NumberID(int64(100+index)), "sampling/createMessage", map[string]any{})
	}
	for index := 0; index < maxConcurrentServerRequests; index++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d server requests started", index)
		}
	}

	// 1. A notification still reaches the dispatcher.
	sendNotification(t, server.transport, "notifications/message", map[string]any{"level": "info", "data": "hi"})
	select {
	case method := <-notifications:
		if method != "notifications/message" {
			t.Fatalf("notification = %q", method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the receive loop stalled while a server request handler was blocked")
	}

	// 2. A response the client is waiting for is still delivered.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := client.ListTools(ctx); err != nil {
		t.Fatalf("tools/list was not answered while a server request handler was blocked: %v", err)
	}

	// 3. A request past the bound is refused explicitly instead of queued.
	sendServerRequest(t, server.transport, NumberID(200), "sampling/createMessage", map[string]any{})
	var refusal *ErrorResponse
	if !waitFor(t, 2*time.Second, func() bool {
		for _, message := range server.Messages() {
			if response, ok := message.(*ErrorResponse); ok && response.ID == NumberID(200) {
				refusal = response
				return true
			}
		}
		return false
	}) {
		t.Fatalf("the overflow request was never answered; messages=%#v", server.Messages())
	}
	if refusal.Error.Code != CodeInternalError || !strings.Contains(refusal.Error.Message, "too many concurrent server requests") {
		t.Fatalf("overflow answer = %+v, want -32603 too many concurrent server requests", refusal.Error)
	}

	// 4. Releasing the handlers answers the in-flight requests normally.
	releaseBlock()
	if !waitFor(t, 3*time.Second, func() bool {
		answered := 0
		for _, message := range server.Messages() {
			if response, ok := message.(*SuccessResponse); ok {
				if id := response.ID; id == NumberID(100) || id == NumberID(101) || id == NumberID(102) || id == NumberID(103) {
					answered++
				}
			}
		}
		return answered == maxConcurrentServerRequests
	}) {
		t.Fatalf("in-flight server requests were not answered; messages=%#v", server.Messages())
	}
}

// blockingOverflowTransport makes the overflow refusal's Send block, modelling
// a transport that is slow to drain. Every other send is forwarded untouched.
type blockingOverflowTransport struct {
	Transport
	block   chan struct{}
	started chan struct{}
}

func (t *blockingOverflowTransport) Send(ctx context.Context, message Message) error {
	if response, ok := message.(*ErrorResponse); ok &&
		response.Error.Code == CodeInternalError &&
		strings.Contains(response.Error.Message, "too many concurrent server requests") {
		select {
		case t.started <- struct{}{}:
		default:
		}
		select {
		case <-t.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return t.Transport.Send(ctx, message)
}

// TestServerRequestOverflowSendDoesNotBlockTheReceiveLoop pins the bounded
// reject path: with all four handlers occupied, the 5th/6th requests are
// answered by a background worker whose Send is blocked, while notifications
// and responses on the connection keep flowing. Releasing the transport then
// proves both overflow requests were answered (never silently dropped).
func TestServerRequestOverflowSendDoesNotBlockTheReceiveLoop(t *testing.T) {
	clientTransport, server := newTestServer(t)
	overflowBlock := make(chan struct{})
	var releaseOverflow sync.Once
	releaseOverflowSend := func() { releaseOverflow.Do(func() { close(overflowBlock) }) }
	defer releaseOverflowSend()
	overflowStarted := make(chan struct{}, 2)
	transport := &blockingOverflowTransport{
		Transport: clientTransport,
		block:     overflowBlock,
		started:   overflowStarted,
	}

	handlerBlock := make(chan struct{})
	var releaseHandlers sync.Once
	releaseHandler := func() { releaseHandlers.Do(func() { close(handlerBlock) }) }
	defer releaseHandler()
	started := make(chan struct{}, maxConcurrentServerRequests)
	notifications := make(chan string, 4)
	client := NewClient(ClientOptions{
		Name: "test-client", Version: "2.0.0",
		Notifications: func(_ context.Context, method string, _ json.RawMessage) {
			notifications <- method
		},
		ServerRequests: func(ctx context.Context, request *Request) (Message, error) {
			started <- struct{}{}
			select {
			case <-handlerBlock:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &SuccessResponse{
				JSONRPC: JSONRPCVersion,
				ID:      request.ID,
				Result:  json.RawMessage(`{"ok":true}`),
			}, nil
		},
	})
	if _, err := client.Connect(testContext(t), transport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	server.setHandler("tools/list", func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{"tools": []any{}}, nil
	})

	for index := 0; index < maxConcurrentServerRequests; index++ {
		sendServerRequest(t, server.transport, NumberID(int64(100+index)), "sampling/createMessage", map[string]any{})
	}
	for index := 0; index < maxConcurrentServerRequests; index++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d server requests started", index)
		}
	}

	// Two overflows: the first occupies the background sender (Send blocks),
	// the second waits in the bounded queue.
	sendServerRequest(t, server.transport, NumberID(200), "sampling/createMessage", map[string]any{})
	sendServerRequest(t, server.transport, NumberID(201), "sampling/createMessage", map[string]any{})
	select {
	case <-overflowStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("the overflow refusal never reached the transport")
	}

	// 1. A notification is still dispatched.
	sendNotification(t, server.transport, "notifications/message", map[string]any{"level": "info", "data": "hi"})
	select {
	case method := <-notifications:
		if method != "notifications/message" {
			t.Fatalf("notification = %q", method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the receive loop stalled behind the overflow Send")
	}

	// 2. A response is still delivered, and quickly: the threshold is far below
	// the 5s serverRequestTimeout the old inline Send would have imposed.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	begin := time.Now()
	if _, err := client.ListTools(ctx); err != nil {
		t.Fatalf("tools/list was not answered while the overflow Send was blocked: %v", err)
	}
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("tools/list took %s, want under 1s (the receive loop waited on the overflow Send)", elapsed)
	}

	// 3. Releasing the transport answers both overflow requests: the queue does
	// not silently drop the second one.
	releaseOverflowSend()
	if !waitFor(t, 3*time.Second, func() bool {
		seen := map[ID]bool{}
		for _, message := range server.Messages() {
			if response, ok := message.(*ErrorResponse); ok &&
				response.Error.Code == CodeInternalError &&
				strings.Contains(response.Error.Message, "too many concurrent server requests") {
				seen[response.ID] = true
			}
		}
		return seen[NumberID(200)] && seen[NumberID(201)]
	}) {
		t.Fatalf("an overflow request was dropped; messages=%#v", server.Messages())
	}

	// 4. The normal four-slot semantics are unchanged: releasing the handlers
	// answers the four in-flight requests.
	releaseHandler()
	if !waitFor(t, 3*time.Second, func() bool {
		answered := 0
		for _, message := range server.Messages() {
			if response, ok := message.(*SuccessResponse); ok {
				switch response.ID {
				case NumberID(100), NumberID(101), NumberID(102), NumberID(103):
					answered++
				}
			}
		}
		return answered == maxConcurrentServerRequests
	}) {
		t.Fatalf("in-flight server requests were not answered; messages=%#v", server.Messages())
	}
}
