// Listen supervisor tests (P4.3): one stream per server, resubscribe after the
// server ends it, and a full catalogue recompute after every (re)subscription.
// The fake server completes each listen stream immediately, so the supervisor
// exercises the reconnect path without any wall-clock backoff (Sleep is
// injected).

package mcphost

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/mcp"
	"github.com/jixoai/keel/internal/metrics"
)

// listenFixture is a real mcp.Client over the in-memory transport whose server
// acknowledges and completes every subscriptions/listen.
type listenFixture struct {
	server  *mcp.MemoryTransport
	mu      sync.Mutex
	listens int
}

func (f *listenFixture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listens
}

func (f *listenFixture) run(t *testing.T) {
	t.Helper()
	for {
		message, err := f.server.Receive(context.Background())
		if err != nil {
			return
		}
		request, ok := message.(*mcp.Request)
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		switch request.Method {
		case "initialize":
			body := `{"protocolVersion":"2025-11-25","capabilities":{"tools":{"listChanged":true}},"serverInfo":{"name":"listen-fixture","version":"1"}}`
			_ = f.server.Send(ctx, &mcp.SuccessResponse{JSONRPC: mcp.JSONRPCVersion, ID: request.ID, Result: json.RawMessage(body)})
		case "tools/list":
			body := `{"tools":[{"name":"fixture_tool","inputSchema":{"type":"object"}}]}`
			_ = f.server.Send(ctx, &mcp.SuccessResponse{JSONRPC: mcp.JSONRPCVersion, ID: request.ID, Result: json.RawMessage(body)})
		case "subscriptions/listen":
			f.mu.Lock()
			f.listens++
			f.mu.Unlock()
			ack := fmt.Sprintf(`{"notifications":{"toolsListChanged":true},"_meta":{"%s":%s}}`,
				mcp.SubscriptionIDMetaKey, request.ID)
			_ = f.server.Send(ctx, &mcp.Notification{
				JSONRPC: mcp.JSONRPCVersion,
				Method:  "notifications/subscriptions/acknowledged",
				Params:  json.RawMessage(ack),
			})
			// The server declares the stream complete right away; the client must
			// resubscribe (no replay, no Last-Event-ID) and recompute.
			_ = f.server.Send(ctx, &mcp.SuccessResponse{
				JSONRPC: mcp.JSONRPCVersion, ID: request.ID, Result: json.RawMessage(`{"resultType":"complete"}`),
			})
		default:
			_ = f.server.Send(ctx, &mcp.ErrorResponse{
				JSONRPC: mcp.JSONRPCVersion, ID: request.ID,
				Error: mcp.ErrorObject{Code: mcp.CodeMethodNotFound, Message: "method not found"},
			})
		}
		cancel()
	}
}

func TestEventsListenSupervisorResubscribesAndRecomputes(t *testing.T) {
	clientTransport, serverTransport := mcp.NewMemoryTransportPair()
	if err := serverTransport.Start(context.Background()); err != nil {
		t.Fatalf("start server transport: %v", err)
	}
	fixture := &listenFixture{server: serverTransport}
	go fixture.run(t)
	t.Cleanup(func() { _ = clientTransport.Close() })

	metricsReg := metrics.NewRegistry()
	mcpMetrics := NewMetrics(metricsReg)
	fleet := &eventFleet{
		refreshErr:  map[string]error{},
		replacement: map[string][]mcp.Tool{},
	}
	events := NewEvents(EventsOptions{
		Fleet:     fleet,
		Logger:    slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelError})),
		Metrics:   mcpMetrics,
		ListenFor: func(*mcp.Server) bool { return true },
		Sleep:     func(context.Context, time.Duration) bool { return true },
	})
	client := mcp.NewClient(mcp.ClientOptions{
		Name: "listen-client", Version: "1",
		Notifications: func(ctx context.Context, method string, params json.RawMessage) {
			events.Notification(ctx, "fixture", method, params)
		},
		ServerRequests: func(ctx context.Context, request *mcp.Request) (mcp.Message, error) {
			return events.ServerRequest(ctx, "fixture", request)
		},
	})
	if _, err := client.Connect(context.Background(), clientTransport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	fleet.mu.Lock()
	fleet.servers = []*mcp.Server{{
		Entry:  mcp.ServerEntry{Name: "fixture", Config: map[string]any{"url": "https://mcp.example/mcp"}},
		Client: client,
		Tools:  []mcp.Tool{{Name: "old"}},
	}}
	fleet.mu.Unlock()

	events.Start(context.Background())
	defer events.Stop()

	if !waitForCondition(t, 2*time.Second, func() bool { return fixture.count() >= 3 }) {
		t.Fatalf("listen requests = %d, want at least 3", fixture.count())
	}
	// Every subscription establishment recomputes the catalogue once.
	if fleet.calls() < 3 {
		t.Fatalf("catalogue recomputes = %d, want at least 3", fleet.calls())
	}
	// Reconnects are counted (the first subscription is not a reconnect).
	if got := counterValue(t, metricsReg, "mcp_listen_reconnects_total"); got < 2 {
		t.Fatalf("mcp_listen_reconnects_total = %v, want at least 2", got)
	}
}

// TestEventsListenRejectedIsAuditedAndRetried: a server that refuses
// subscriptions/listen leaves the tools usable, audits the error and backs off
// instead of spinning (Sleep is injected and stops after two attempts).
func TestEventsListenRejectedIsAuditedAndRetried(t *testing.T) {
	clientTransport, serverTransport := mcp.NewMemoryTransportPair()
	if err := serverTransport.Start(context.Background()); err != nil {
		t.Fatalf("start server transport: %v", err)
	}
	t.Cleanup(func() { _ = clientTransport.Close() })
	go func() {
		for {
			message, err := serverTransport.Receive(context.Background())
			if err != nil {
				return
			}
			request, ok := message.(*mcp.Request)
			if !ok {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			switch request.Method {
			case "initialize":
				body := `{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"refuser","version":"1"}}`
				_ = serverTransport.Send(ctx, &mcp.SuccessResponse{JSONRPC: mcp.JSONRPCVersion, ID: request.ID, Result: json.RawMessage(body)})
			case "tools/list":
				body := `{"tools":[{"name":"ok","inputSchema":{"type":"object"}}]}`
				_ = serverTransport.Send(ctx, &mcp.SuccessResponse{JSONRPC: mcp.JSONRPCVersion, ID: request.ID, Result: json.RawMessage(body)})
			default:
				_ = serverTransport.Send(ctx, &mcp.ErrorResponse{
					JSONRPC: mcp.JSONRPCVersion, ID: request.ID,
					Error: mcp.ErrorObject{Code: mcp.CodeInvalidRequest, Message: "subscriptions are not supported"},
				})
			}
			cancel()
		}
	}()

	auditor := &fakeAuditor{}
	attempts := make(chan struct{}, 8)
	var attemptsMu sync.Mutex
	count := 0
	events := NewEvents(EventsOptions{
		Logger:    slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelError})),
		Auditor:   auditor,
		ListenFor: func(*mcp.Server) bool { return true },
		Sleep: func(context.Context, time.Duration) bool {
			attemptsMu.Lock()
			count++
			stop := count >= 2
			attemptsMu.Unlock()
			attempts <- struct{}{}
			return !stop
		},
	})
	client := mcp.NewClient(mcp.ClientOptions{Name: "listen-client", Version: "1",
		Notifications: func(ctx context.Context, method string, params json.RawMessage) {
			events.Notification(ctx, "fixture", method, params)
		},
		ServerRequests: func(ctx context.Context, request *mcp.Request) (mcp.Message, error) {
			return events.ServerRequest(ctx, "fixture", request)
		}})
	if _, err := client.Connect(context.Background(), clientTransport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	fleet := &eventFleet{
		servers: []*mcp.Server{{
			Entry:  mcp.ServerEntry{Name: "fixture", Config: map[string]any{"url": "https://mcp.example/mcp"}},
			Client: client,
			Tools:  []mcp.Tool{{Name: "ok"}},
		}},
		refreshErr: map[string]error{}, replacement: map[string][]mcp.Tool{},
	}
	events.SetFleet(fleet)
	events.Start(context.Background())
	defer events.Stop()

	for i := 0; i < 2; i++ {
		select {
		case <-attempts:
		case <-time.After(2 * time.Second):
			t.Fatal("the supervisor did not back off after a refused subscription")
		}
	}
	results := auditResults(auditor, "subscription.listen")
	if len(results) == 0 {
		t.Fatal("a refused subscription was not audited")
	}
	allErrors := true
	for _, result := range results {
		if result != "error" {
			allErrors = false
		}
	}
	if !allErrors {
		t.Fatalf("subscription audit results = %v, want all error", results)
	}
	// Tools remain usable: the dispatcher never touched the catalogue.
	if fleet.calls() != 0 {
		t.Fatalf("a refused subscription refreshed the catalogue %d times", fleet.calls())
	}
}

// testWriter sends slog output to the test log only on failure.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return condition()
}

func counterValue(t *testing.T, reg *metrics.Registry, name string) float64 {
	t.Helper()
	total := 0.0
	for _, family := range reg.Snapshot(context.Background()) {
		if family.Name != name {
			continue
		}
		for _, sample := range family.Samples {
			total += sample.Value
		}
	}
	return total
}
