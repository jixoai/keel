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
