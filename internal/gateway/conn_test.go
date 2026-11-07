// Connection tests: the handshake and its rejections, request correlation,
// timeouts, cancellation propagation, the unhealthy policy, audit records and
// the isolation between the two provider surfaces.

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHandshakeRejectsUnsupportedVersion(t *testing.T) {
	transport, provider := startFakeProvider(t, StdioTransportOptions{})
	provider.declare(providerDecl{kind: ProviderTypeToolProvider, version: "v0"})
	conn := NewConn(ConnOptions{RequestTimeout: time.Second})
	t.Cleanup(func() { _ = conn.Close() })

	_, err := conn.Connect(context.Background(), transport)
	var handshake *HandshakeError
	if !errors.As(err, &handshake) || handshake.Code != HandshakeUnsupportedVersion {
		t.Fatalf("Connect error = %v, want unsupported_protocol_version", err)
	}
	if !strings.Contains(handshake.Error(), "v0") {
		t.Errorf("error = %q, want it to name the provider's version", handshake)
	}
	if state := conn.State(); state != StateClosed {
		t.Errorf("state = %q, want closed: a rejected handshake must not activate", state)
	}
	if conn.ProtocolVersion() != "" {
		t.Errorf("protocol version = %q, want empty after a rejected handshake", conn.ProtocolVersion())
	}
}

func TestHandshakeRejectsUnacceptedProviderKind(t *testing.T) {
	transport, provider := startFakeProvider(t, StdioTransportOptions{})
	provider.declare(providerDecl{kind: ProviderTypeAgentBackend})
	conn := NewConn(ConnOptions{
		RequestTimeout:  time.Second,
		AcceptProviders: []ProviderType{ProviderTypeToolProvider},
	})
	t.Cleanup(func() { _ = conn.Close() })

	_, err := conn.Connect(context.Background(), transport)
	var handshake *HandshakeError
	if !errors.As(err, &handshake) || handshake.Code != HandshakeProviderNotAccepted {
		t.Fatalf("Connect error = %v, want provider_type_not_accepted", err)
	}
}

func TestHandshakeRejectsMissingCapability(t *testing.T) {
	transport, provider := startFakeProvider(t, StdioTransportOptions{})
	provider.setHandler(MethodInitialize, func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{}, // agent capability deliberately missing
			"serverInfo":      map[string]any{"name": "bad", "version": "1"},
			"provider":        map[string]any{"type": string(ProviderTypeAgentBackend)},
		}, nil
	})
	conn := NewConn(ConnOptions{RequestTimeout: time.Second})
	t.Cleanup(func() { _ = conn.Close() })

	_, err := conn.Connect(context.Background(), transport)
	var handshake *HandshakeError
	if !errors.As(err, &handshake) || handshake.Code != HandshakeMissingCapability {
		t.Fatalf("Connect error = %v, want missing_capability", err)
	}
}

func TestHandshakeNormalizesExternalSessionMarks(t *testing.T) {
	// No stateOwner, no metering, no usage capability: the safe marks are
	// external + none, never "backend" and never a silent zero.
	group := newAgentFixture(t, providerDecl{recovery: RecoveryBackend})
	provider := group.conn.Provider()
	if provider.StateOwner != StateOwnerExternal {
		t.Errorf("stateOwner = %q, want external (the safe default)", provider.StateOwner)
	}
	if provider.Metering != MeteringNone {
		t.Errorf("metering = %q, want none without a usage capability", provider.Metering)
	}
	if provider.Recovery != RecoveryBackend {
		t.Errorf("recovery = %q, want backend", provider.Recovery)
	}
}

func TestHandshakeMarksMeteringExternalWhenBackendReportsUsage(t *testing.T) {
	group := newAgentFixture(t, providerDecl{usage: true})
	if got := group.conn.Provider().Metering; got != MeteringExternal {
		t.Fatalf("metering = %q, want external when the backend reports usage", got)
	}
}

func TestHandshakeRejectsUnknownStateOwner(t *testing.T) {
	transport, provider := startFakeProvider(t, StdioTransportOptions{})
	provider.declare(providerDecl{kind: ProviderTypeAgentBackend, stateOwner: "middleman"})
	conn := NewConn(ConnOptions{RequestTimeout: time.Second})
	t.Cleanup(func() { _ = conn.Close() })

	_, err := conn.Connect(context.Background(), transport)
	var handshake *HandshakeError
	if !errors.As(err, &handshake) || handshake.Code != HandshakeInvalidProvider {
		t.Fatalf("Connect error = %v, want invalid_provider", err)
	}
}

func TestRequestTimeoutCancelsAndReturnsDeterministicError(t *testing.T) {
	transport, provider := startFakeProvider(t, StdioTransportOptions{})
	provider.declare(providerDecl{kind: ProviderTypeToolProvider})
	provider.silence(MethodToolsCall)
	conn := NewConn(ConnOptions{RequestTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Connect(context.Background(), transport); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	tool := NewToolProvider(conn)

	started := time.Now()
	_, err := tool.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"slow"}`))
	var timeout *TimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("error = %v, want *TimeoutError", err)
	}
	if timeout.Method != MethodToolsCall {
		t.Errorf("timeout method = %q, want tools/call", timeout.Method)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("call took %v, want it bounded by the timeout", elapsed)
	}
	// The cancellation reaches the provider, carrying the request id.
	request := provider.waitForRequest(MethodToolsCall)
	notification := provider.waitForNotification(MethodRequestCancel)
	var params struct {
		RequestID ID     `json:"requestId"`
		Reason    string `json:"reason"`
	}
	decodeParams(t, notification.Params, &params)
	if params.RequestID.Key() != request.ID.Key() {
		t.Errorf("cancel notification requestId = %s, want %s", params.RequestID, request.ID)
	}
	if params.Reason == "" {
		t.Error("cancel notification carried no reason")
	}
}

func TestCallerCancellationPropagatesAndLateResponseIsDropped(t *testing.T) {
	group := newToolFixture(t)
	group.provider.silence(MethodToolsCall)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := group.tool.CallTool(ctx, "echo", json.RawMessage(`{"text":"cancelled"}`))
		done <- err
	}()
	request := group.provider.waitForRequest(MethodToolsCall)
	cancel()

	select {
	case err := <-done:
		var canceled *CanceledError
		if !errors.As(err, &canceled) {
			t.Fatalf("error = %v, want *CanceledError", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("cancelled call did not return")
	}
	group.provider.waitForNotification(MethodRequestCancel)

	// The provider answers the cancelled call anyway: the response must be
	// dropped without disturbing the connection or a later call.
	group.provider.respond(request.ID, map[string]any{"content": []any{}})

	type outcome struct {
		result ToolResult
		err    error
	}
	second := make(chan outcome, 1)
	go func() {
		result, err := group.tool.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"after"}`))
		second <- outcome{result: result, err: err}
	}()
	// Answer the second call: it is a distinct request id.
	deadline := time.Now().Add(testTimeout)
	var secondRequest *Request
	for time.Now().Before(deadline) {
		for _, candidate := range group.provider.requests() {
			if candidate.Method == MethodToolsCall && candidate.ID.Key() != request.ID.Key() {
				secondRequest = candidate
			}
		}
		if secondRequest != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if secondRequest == nil {
		t.Fatal("the second call never reached the provider")
	}
	group.provider.respond(secondRequest.ID, map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "after"}},
	})
	select {
	case out := <-second:
		if out.err != nil {
			t.Fatalf("call after a dropped late response: %v", out.err)
		}
		if len(out.result.Content) != 1 || out.result.Content[0].Text != "after" {
			t.Fatalf("result = %+v, want the fresh response", out.result)
		}
	case <-time.After(testTimeout):
		t.Fatal("the second call did not settle")
	}
}

func TestUnknownResponseIDIsDiagnosed(t *testing.T) {
	group := newToolFixture(t)
	group.provider.respond(NumberID(4242), map[string]any{"nobody": true})
	if !waitFor(t, testTimeout, func() bool { return group.diags.hasKind(DiagUnknownResponse) }) {
		t.Fatalf("diagnostics = %s, want an unknown-response diagnostic", group.diags.all())
	}
}

func TestProviderInitiatedRequestIsAnsweredWithMethodNotFound(t *testing.T) {
	group := newToolFixture(t)
	group.provider.sendRequest(NumberID(77), "session.open", nil)
	if !waitFor(t, testTimeout, func() bool { return group.diags.hasKind(DiagServerRequest) }) {
		t.Fatalf("diagnostics = %s, want a server-request diagnostic", group.diags.all())
	}
	// The client must have written an error response with the same id.
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		stats := group.transport.Stats()
		if stats.Sent >= 2 { // initialize + the error response
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the provider-initiated request was never answered")
}

func TestTooManyBadFramesMarkTheProviderUnhealthy(t *testing.T) {
	transport, provider := startFakeProvider(t, StdioTransportOptions{})
	provider.declare(providerDecl{kind: ProviderTypeToolProvider})
	conn := NewConn(ConnOptions{RequestTimeout: time.Second, MaxBadFrames: 3})
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Connect(context.Background(), transport); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	for index := 0; index < 10; index++ {
		provider.writeRaw([]byte("this is not JSON\n"))
	}
	if !waitFor(t, testTimeout, func() bool { return conn.Health().State == HealthUnhealthy }) {
		t.Fatalf("health = %+v, want unhealthy after more bad frames than the budget", conn.Health())
	}
	select {
	case <-conn.Done():
	case <-time.After(testTimeout):
		t.Fatal("connection did not end after the bad-frame budget was exhausted")
	}
	if _, err := NewToolProvider(conn).ListTools(context.Background()); err == nil {
		t.Fatal("ListTools succeeded on an unhealthy connection")
	}
}

func TestDegradedThenHealthyAgain(t *testing.T) {
	group := newToolFixture(t)
	group.provider.silence(MethodToolsCall)
	failed := make(chan error, 1)
	go func() {
		_, err := group.tool.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"x"}`))
		failed <- err
	}()
	request := group.provider.waitForRequest(MethodToolsCall)
	group.provider.writeRaw([]byte("garbage\n"))
	group.provider.respond(request.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": "x"}}})
	if err := <-failed; err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if state := group.conn.Health().State; state != HealthOK {
		t.Errorf("health = %q, want ok again after a good message", state)
	}
	if group.conn.Health().BadFrames == 0 {
		t.Error("BadFrames counter did not record the skipped frame")
	}
}

func TestConcurrentRequestsOverOneConnection(t *testing.T) {
	group := newToolFixture(t)
	var wait sync.WaitGroup
	errs := make(chan error, 8)
	for index := 0; index < 8; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			result, err := group.tool.CallTool(context.Background(), "echo",
				json.RawMessage(fmt.Sprintf(`{"text":"c%d"}`, index)))
			if err != nil {
				errs <- err
				return
			}
			if want := fmt.Sprintf("c%d", index); len(result.Content) != 1 || result.Content[0].Text != want {
				errs <- fmt.Errorf("call %d = %+v, want %s", index, result.Content, want)
			}
		}(index)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestAuditRecordsCarryNoPayloads is the spec's 日志检查 scenario at the
// metadata layer: a call whose arguments contain a secret produces a record
// that cannot contain it.
func TestAuditRecordsCarryNoPayloads(t *testing.T) {
	const secret = "sk-live-9f8e7d6c5b4a"
	var (
		mu      sync.Mutex
		records []CallRecord
	)
	group := newFixture(t, ConnOptions{
		OnCall: func(record CallRecord) {
			mu.Lock()
			records = append(records, record)
			mu.Unlock()
		},
	}, providerDecl{kind: ProviderTypeToolProvider, name: "audit-provider"})
	group.provider.setHandler(MethodToolsCall, func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{"content": []any{}}, nil
	})
	if _, err := group.tool.CallTool(context.Background(), "secret-tool",
		json.RawMessage(fmt.Sprintf(`{"api_key":%q,"text":"hello"}`, secret))); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(records) == 0 {
		t.Fatal("no audit records emitted")
	}
	for _, record := range records {
		rendered := record.String()
		if strings.Contains(rendered, secret) {
			t.Fatalf("audit record leaked the secret: %s", rendered)
		}
	}
	found := false
	for _, record := range records {
		if record.Method == MethodToolsCall && record.Target == "secret-tool" && record.Status == CallOK {
			found = true
			if record.Provider != "audit-provider" {
				t.Errorf("record provider = %q, want audit-provider", record.Provider)
			}
			if record.Duration <= 0 {
				t.Errorf("record duration = %v, want a positive duration", record.Duration)
			}
		}
	}
	if !found {
		t.Fatalf("no tools/call record with the tool name: %+v", records)
	}
}

// TestProviderSurfacesAreIsolated: the tool surface and the agent method set
// never mix on one connection.
func TestProviderSurfacesAreIsolated(t *testing.T) {
	toolGroup := newToolFixture(t)
	if err := toolGroup.agent.requireKind("Open"); err == nil {
		t.Fatal("agent calls are allowed on a tool_provider connection")
	} else {
		var kindErr *ProviderTypeError
		if !errors.As(err, &kindErr) || kindErr.Want != ProviderTypeAgentBackend {
			t.Fatalf("error = %v, want a *ProviderTypeError about agent_backend", err)
		}
	}

	agentGroup := newAgentFixture(t, providerDecl{})
	if err := agentGroup.tool.requireKind("ListTools"); err == nil {
		t.Fatal("tool calls are allowed on an agent_backend connection")
	} else {
		var kindErr *ProviderTypeError
		if !errors.As(err, &kindErr) || kindErr.Want != ProviderTypeToolProvider {
			t.Fatalf("error = %v, want a *ProviderTypeError about tool_provider", err)
		}
	}
}

func TestCloseFailsPendingCallsAndClosesEvents(t *testing.T) {
	group := newToolFixture(t)
	group.provider.silence(MethodToolsCall)
	failed := make(chan error, 1)
	go func() {
		_, err := group.tool.CallTool(context.Background(), "echo", nil)
		failed <- err
	}()
	group.provider.waitForRequest(MethodToolsCall)

	if err := group.conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-failed:
		var closed *ConnClosedError
		if !errors.As(err, &closed) {
			t.Fatalf("pending call error = %v, want *ConnClosedError", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("pending call did not fail on Close")
	}
	if _, open := <-group.conn.Events(); open {
		t.Error("events channel is still open after Close")
	}
	if state := group.conn.State(); state != StateClosed {
		t.Errorf("state = %q, want closed", state)
	}
}

func TestOnNotificationHandlerReceivesProviderNotifications(t *testing.T) {
	group := newToolFixture(t)
	received := make(chan json.RawMessage, 1)
	group.conn.OnNotification("tools/list_changed", func(params json.RawMessage) {
		received <- params
	})
	group.provider.sendNotification("tools/list_changed", map[string]any{"reason": "test"})
	select {
	case params := <-received:
		if !strings.Contains(string(params), "test") {
			t.Errorf("handler params = %s, want the payload", params)
		}
	case <-time.After(testTimeout):
		t.Fatal("the notification handler was never called")
	}
	// Removing a handler is allowed and leaves the connection healthy.
	group.conn.OnNotification("tools/list_changed", nil)
	group.provider.sendNotification("tools/list_changed", map[string]any{"reason": "ignored"})
	if state := group.conn.Health().State; state != HealthOK {
		t.Errorf("health = %q, want ok", state)
	}
}

func TestOptionsDefaults(t *testing.T) {
	conn := NewConn(ConnOptions{})
	if conn.opts.ClientInfo.Name != ClientName {
		t.Errorf("client name = %q, want %q", conn.opts.ClientInfo.Name, ClientName)
	}
	if conn.opts.RequestTimeout != DefaultRequestTimeout {
		t.Errorf("request timeout = %v, want %v", conn.opts.RequestTimeout, DefaultRequestTimeout)
	}
	if len(conn.opts.AcceptProviders) != 2 {
		t.Errorf("accept providers = %v, want both kinds", conn.opts.AcceptProviders)
	}
	if conn.opts.Capabilities.Tools == nil || conn.opts.Capabilities.Agent == nil {
		t.Errorf("default capabilities = %+v, want tools and agent", conn.opts.Capabilities)
	}
}
