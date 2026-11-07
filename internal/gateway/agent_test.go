// Agent-surface tests: the versioned private method set (P0 §6), the external
// session data model (P0 §4) and the usage metering boundary (P0 §3).

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// serveAgentDefaults serves the whole agent method set with sane answers.
func serveAgentDefaults(p *fakeProvider) {
	p.setHandler(MethodSessionOpen, func(params json.RawMessage) (any, *ErrorObject) {
		var request SessionOpenRequest
		_ = json.Unmarshal(params, &request)
		return map[string]any{"sessionId": "ext-1", "reference": request.Reference, "lastSeq": 0}, nil
	})
	p.setHandler(MethodSessionResume, func(params json.RawMessage) (any, *ErrorObject) {
		var request SessionResumeRequest
		_ = json.Unmarshal(params, &request)
		return map[string]any{"sessionId": request.SessionID, "resumed": true, "lastSeq": 41}, nil
	})
	p.setHandler(MethodMessageSend, func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{"turnId": "turn-1"}, nil
	})
	p.setHandler(MethodEventStream, func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{"streamId": "stream-1"}, nil
	})
	p.setHandler(MethodCancel, func(json.RawMessage) (any, *ErrorObject) { return map[string]any{}, nil })
	p.setHandler(MethodHealth, func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{"status": BackendStatusOK, "version": "9.9.9"}, nil
	})
	p.setHandler(MethodClose, func(json.RawMessage) (any, *ErrorObject) { return map[string]any{}, nil })
}

// TestAgentMethodSetIsTheVersionedPrivateSet pins the P0 §6 method set: these
// seven literal names (plus initialize) are the protocol's own, and none of
// them is an MCP method name.
func TestAgentMethodSetIsTheVersionedPrivateSet(t *testing.T) {
	agentMethods := map[string]string{
		"session.open":   MethodSessionOpen,
		"session.resume": MethodSessionResume,
		"message.send":   MethodMessageSend,
		"event.stream":   MethodEventStream,
		"cancel":         MethodCancel,
		"health":         MethodHealth,
		"close":          MethodClose,
	}
	for want, got := range agentMethods {
		if got != want {
			t.Errorf("agent method constant = %q, want %q", got, want)
		}
	}
	if MethodInitialize != "initialize" {
		t.Errorf("MethodInitialize = %q, want initialize", MethodInitialize)
	}
	mcpNames := map[string]bool{
		"tools/list":                true,
		"tools/call":                true,
		"notifications/initialized": true,
		"notifications/cancelled":   true,
		"resources/list":            true,
		"prompts/list":              true,
	}
	for _, method := range agentMethods {
		if mcpNames[method] {
			t.Errorf("agent method %q is an MCP method name; the agent surface must not dress itself in MCP semantics", method)
		}
	}
	if MethodToolsList != "tools/list" || MethodToolsCall != "tools/call" {
		t.Errorf("tool surface = %q/%q, want the MCP names tools/list and tools/call", MethodToolsList, MethodToolsCall)
	}
}

func TestExternalSessionCarriesRecordableMarks(t *testing.T) {
	group := newAgentFixture(t, providerDecl{recovery: RecoveryBackend})
	serveAgentDefaults(group.provider)

	session, err := group.agent.Open(context.Background(), SessionOpenRequest{Reference: "ref-9"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if session.SessionID != "ext-1" {
		t.Errorf("session id = %q, want ext-1", session.SessionID)
	}
	if session.StateOwner != StateOwnerExternal {
		t.Errorf("stateOwner = %q, want external", session.StateOwner)
	}
	if session.Status != ExternalStatusActive {
		t.Errorf("status = %q, want active", session.Status)
	}
	if session.Backend != "fake-agent" {
		t.Errorf("backend = %q, want fake-agent", session.Backend)
	}
	if session.Metering != MeteringNone {
		t.Errorf("metering = %q, want none without a usage capability", session.Metering)
	}
	if session.Recovery != RecoveryBackend {
		t.Errorf("recovery = %q, want backend", session.Recovery)
	}
	if !session.CompatMode() {
		t.Error("an external session must report compatibility mode")
	}
	markers := session.Markers()
	want := map[string]string{
		"state_owner":      "external",
		"external_backend": "fake-agent",
		"external_ref":     "ext-1",
		"external_status":  "active",
		"metering":         "none",
	}
	for key, value := range want {
		if markers[key] != value {
			t.Errorf("markers[%q] = %q, want %q", key, markers[key], value)
		}
	}
	if got := group.agent.Status("ext-1"); got != ExternalStatusActive {
		t.Errorf("Status = %q, want active", got)
	}
	if got := group.agent.Status("never-seen"); got != "" {
		t.Errorf("Status of an unknown session = %q, want empty", got)
	}
	sessions := group.agent.Sessions()
	if tracked, ok := sessions["ext-1"]; !ok || tracked.Backend != "fake-agent" {
		t.Errorf("Sessions() = %+v, want the opened session tracked", sessions)
	}
}

func TestExternalSessionStatusIsQueryableWhenBackendDies(t *testing.T) {
	group := newAgentFixture(t, providerDecl{})
	serveAgentDefaults(group.provider)
	session, err := group.agent.Open(context.Background(), SessionOpenRequest{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// The backend dies.
	group.provider.close()
	select {
	case <-group.conn.Done():
	case <-time.After(testTimeout):
		t.Fatal("connection did not end after the backend died")
	}
	if got := group.agent.Status(session.SessionID); got != ExternalStatusUnavailable {
		t.Errorf("Status = %q, want backend-unavailable", got)
	}
	if _, err := group.agent.Health(context.Background()); err == nil {
		t.Error("Health succeeded although the backend is gone")
	} else {
		var unhealthy *UnhealthyError
		if !errors.As(err, &unhealthy) {
			t.Errorf("Health error = %v, want *UnhealthyError", err)
		}
	}
	if state := group.conn.Health().State; state != HealthUnhealthy {
		t.Errorf("connection health = %q, want unhealthy", state)
	}
}

func TestResumeReportsUnavailableWhenBackendDidNotResume(t *testing.T) {
	group := newAgentFixture(t, providerDecl{})
	serveAgentDefaults(group.provider)
	group.provider.setHandler(MethodSessionResume, func(params json.RawMessage) (any, *ErrorObject) {
		var request SessionResumeRequest
		_ = json.Unmarshal(params, &request)
		return map[string]any{"sessionId": request.SessionID, "resumed": false}, nil
	})

	session, err := group.agent.Resume(context.Background(), SessionResumeRequest{SessionID: "ext-9", FromSeq: 10})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if session.Resumed {
		t.Error("Resumed = true although the backend said false")
	}
	if session.Status != ExternalStatusUnavailable {
		t.Errorf("status = %q, want backend-unavailable", session.Status)
	}
	if got := group.agent.Status("ext-9"); got != ExternalStatusUnavailable {
		t.Errorf("Status = %q, want the terminal backend-unavailable", got)
	}
}

func TestSessionLifecycleSendsCancelsAndCloses(t *testing.T) {
	group := newAgentFixture(t, providerDecl{usage: true})
	serveAgentDefaults(group.provider)
	ctx := context.Background()

	session, err := group.agent.Open(ctx, SessionOpenRequest{Reference: "ref-1"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if session.Metering != MeteringExternal {
		t.Errorf("metering = %q, want external when the backend reports usage", session.Metering)
	}

	send, err := group.agent.Send(ctx, MessageSendRequest{
		SessionID: session.SessionID,
		Message:   json.RawMessage(`{"text":"hello"}`),
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if send.TurnID != "turn-1" {
		t.Errorf("turn id = %q, want turn-1", send.TurnID)
	}

	ack, err := group.agent.EventStream(ctx, EventStreamRequest{SessionID: session.SessionID})
	if err != nil {
		t.Fatalf("EventStream: %v", err)
	}
	if ack.StreamID != "stream-1" {
		t.Errorf("stream id = %q, want stream-1", ack.StreamID)
	}

	if err := group.agent.Cancel(ctx, CancelRequest{SessionID: session.SessionID, TurnID: "turn-1", Reason: "user stop"}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// The cancel request carried the turn (the protocol's own cancel, not an
	// MCP notification).
	cancelRequest := group.provider.waitForRequest(MethodCancel)
	var cancelParams CancelRequest
	decodeParams(t, cancelRequest.Params, &cancelParams)
	if cancelParams.SessionID != "ext-1" || cancelParams.TurnID != "turn-1" {
		t.Errorf("cancel params = %+v, want the session and turn", cancelParams)
	}

	health, err := group.agent.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.Status != BackendStatusOK || health.Version != "9.9.9" {
		t.Errorf("health = %+v, want ok/9.9.9", health)
	}

	if err := group.agent.Close(ctx, CloseRequest{SessionID: session.SessionID, Reason: "done"}); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := group.agent.Status(session.SessionID); got != ExternalStatusClosed {
		t.Errorf("Status = %q, want closed", got)
	}
}

func TestAgentCallValidation(t *testing.T) {
	group := newAgentFixture(t, providerDecl{})
	serveAgentDefaults(group.provider)
	ctx := context.Background()
	if _, err := group.agent.Send(ctx, MessageSendRequest{}); err == nil {
		t.Error("Send without a session id was accepted")
	}
	if _, err := group.agent.EventStream(ctx, EventStreamRequest{}); err == nil {
		t.Error("EventStream without a session id was accepted")
	}
	if err := group.agent.Cancel(ctx, CancelRequest{}); err == nil {
		t.Error("Cancel without a session id was accepted")
	}
	if _, err := group.agent.Resume(ctx, SessionResumeRequest{}); err == nil {
		t.Error("Resume without a session id was accepted")
	}
}

func TestHealthResultIsValidated(t *testing.T) {
	group := newAgentFixture(t, providerDecl{})
	group.provider.setHandler(MethodHealth, func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{"status": "splendid"}, nil
	})
	_, err := group.agent.Health(context.Background())
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) {
		t.Fatalf("error = %v, want *ProtocolError for an unknown health status", err)
	}
}

// TestEventStreamDeliversEventsAndUsage covers the event path plus the usage
// passthrough: a usage object that the backend reported is forwarded unchanged.
func TestEventStreamDeliversEventsAndUsage(t *testing.T) {
	group := newAgentFixture(t, providerDecl{usage: true})
	serveAgentDefaults(group.provider)
	session, err := group.agent.Open(context.Background(), SessionOpenRequest{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	group.provider.sendNotification(MethodEventStream, map[string]any{
		"sessionId": session.SessionID,
		"turnId":    "turn-1",
		"seq":       7,
		"type":      "message",
		"data":      map[string]any{"text": "hello"},
		"usage": map[string]any{
			"metering":     string(MeteringExternal),
			"model":        "test-model",
			"inputTokens":  12,
			"outputTokens": 3,
		},
	})

	select {
	case event := <-group.agent.Events():
		if event.SessionID != "ext-1" || event.TurnID != "turn-1" || event.Sequence != 7 {
			t.Errorf("event = %+v, want the backend's identity fields", event)
		}
		if event.Type != "message" {
			t.Errorf("event type = %q, want message", event.Type)
		}
		if event.Usage == nil {
			t.Fatal("usage was dropped")
		}
		if event.Usage.Metering != MeteringExternal || event.Usage.InputTokens != 12 || event.Usage.Model != "test-model" {
			t.Errorf("usage = %+v, want the backend's numbers verbatim", event.Usage)
		}
	case <-time.After(testTimeout):
		t.Fatal("no event delivered")
	}
}

// TestEventWithoutUsageIsNilNotZero: absence of usage is never recorded as a
// zero usage object.
func TestEventWithoutUsageIsNilNotZero(t *testing.T) {
	group := newAgentFixture(t, providerDecl{})
	serveAgentDefaults(group.provider)
	if _, err := group.agent.Open(context.Background(), SessionOpenRequest{}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	group.provider.sendNotification(MethodEventStream, map[string]any{
		"sessionId": "ext-1",
		"type":      "state",
	})
	select {
	case event := <-group.agent.Events():
		if event.Usage != nil {
			t.Fatalf("usage = %+v, want nil when the backend reported none", event.Usage)
		}
	case <-time.After(testTimeout):
		t.Fatal("no event delivered")
	}
}

// TestEventBackpressureBlocksTheReceiveLoop: with strict backpressure
// (EventsBuffer negative selects an unbuffered channel) an unconsumed event
// stops responses from being delivered, which is what pushes back through the
// transport to the backend.
func TestEventBackpressureBlocksTheReceiveLoop(t *testing.T) {
	transport, provider := startFakeProvider(t, StdioTransportOptions{})
	t.Cleanup(provider.close)
	provider.declare(providerDecl{kind: ProviderTypeAgentBackend, name: "fake-agent"})
	serveAgentDefaults(provider)
	conn := NewConn(ConnOptions{RequestTimeout: 2 * time.Second, EventsBuffer: -1})
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Connect(context.Background(), transport); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	agent := NewAgentBackend(conn)
	provider.sendNotification(MethodEventStream, map[string]any{"sessionId": "ext-1", "type": "tick"})

	healthDone := make(chan error, 1)
	go func() {
		_, err := agent.Health(context.Background())
		healthDone <- err
	}()
	select {
	case err := <-healthDone:
		t.Fatalf("Health settled (%v) while the event consumer was blocked: backpressure is broken", err)
	case <-time.After(100 * time.Millisecond):
	}

	select {
	case event := <-agent.Events():
		if event.Type != "tick" {
			t.Fatalf("event = %+v, want the tick", event)
		}
	case <-time.After(testTimeout):
		t.Fatal("no event delivered")
	}
	select {
	case err := <-healthDone:
		if err != nil {
			t.Fatalf("Health after consuming the event: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Health did not settle after the event was consumed")
	}
}

func TestAgentEventsChannelClosesWithTheConnection(t *testing.T) {
	group := newAgentFixture(t, providerDecl{})
	serveAgentDefaults(group.provider)
	group.provider.close()
	select {
	case <-group.conn.Done():
	case <-time.After(testTimeout):
		t.Fatal("connection did not end")
	}
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if _, open := <-group.agent.Events(); !open {
			return
		}
	}
	t.Fatal("events channel did not close with the connection")
}
