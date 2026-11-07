package dshref

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/gateway"
)

// servePipe starts the reference sidecar on one end of an in-process pipe and
// returns the gateway client connected to the other end.
func servePipe(t *testing.T, cfg Config) (*gateway.Conn, *gateway.AgentClient, func()) {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() {
		err := Serve(server, server, cfg)
		_ = server.Close()
		done <- err
	}()
	conn := gateway.NewConn(gateway.ConnOptions{
		AcceptProviders: []gateway.ProviderType{gateway.ProviderTypeAgentBackend},
		RequestTimeout:  5 * time.Second,
		EventsBuffer:    64,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := conn.Connect(ctx, gateway.NewStdioTransport(gateway.StdioTransportOptions{Stream: client})); err != nil {
		t.Fatalf("connect: %v", err)
	}
	cleanup := func() {
		_ = conn.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("reference sidecar did not exit")
		}
	}
	return conn, gateway.NewAgentBackend(conn), cleanup
}

func TestAgentBackendRoundTrip(t *testing.T) {
	conn, agent, cleanup := servePipe(t, Config{Usage: true, Prompt: "ref:"})
	defer cleanup()
	if conn.Provider().Type != gateway.ProviderTypeAgentBackend {
		t.Fatalf("provider type = %q", conn.Provider().Type)
	}
	if conn.Provider().Metering != gateway.MeteringExternal {
		t.Fatalf("metering = %q, want external", conn.Provider().Metering)
	}
	ctx := context.Background()
	session, err := agent.Open(ctx, gateway.SessionOpenRequest{Reference: "ref-1"})
	if err != nil {
		t.Fatalf("session.open: %v", err)
	}
	if session.StateOwner != gateway.StateOwnerExternal || session.Status != gateway.ExternalStatusActive ||
		session.Metering != gateway.MeteringExternal || !session.CompatMode() {
		t.Fatalf("session marks = %+v", session)
	}
	if _, err := agent.EventStream(ctx, gateway.EventStreamRequest{SessionID: session.SessionID}); err != nil {
		t.Fatalf("event.stream: %v", err)
	}
	if _, err := agent.Send(ctx, gateway.MessageSendRequest{
		SessionID: session.SessionID, Message: json.RawMessage(`{"text":"ping"}`),
	}); err != nil {
		t.Fatalf("message.send: %v", err)
	}
	var events []gateway.AgentEvent
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-agent.Events():
			events = append(events, event)
			if event.Type == "turn.completed" {
				goto done
			}
		case <-deadline:
			t.Fatalf("timed out; events = %+v", events)
		}
	}
done:
	if len(events) < 3 {
		t.Fatalf("events = %d, want a streamed turn", len(events))
	}
	final := events[len(events)-2]
	if final.Type != "assistant.message" || final.Usage == nil || final.Usage.Metering != gateway.MeteringExternal {
		t.Fatalf("final event = %+v", final)
	}
	var data struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(final.Data, &data); err != nil || data.Text != "ref:ping" {
		t.Fatalf("final text = %q, %v", data.Text, err)
	}
}

func TestNoUsageMeansNoUsageRecord(t *testing.T) {
	_, agent, cleanup := servePipe(t, Config{})
	defer cleanup()
	ctx := context.Background()
	session, err := agent.Open(ctx, gateway.SessionOpenRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if session.Metering != gateway.MeteringNone {
		t.Fatalf("metering = %q, want none", session.Metering)
	}
	if _, err := agent.EventStream(ctx, gateway.EventStreamRequest{SessionID: session.SessionID}); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Send(ctx, gateway.MessageSendRequest{SessionID: session.SessionID, Message: json.RawMessage(`{"text":"x"}`)}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-agent.Events():
			if event.Usage != nil {
				t.Fatalf("event %s carries usage on a metering=none backend: %+v", event.Type, event.Usage)
			}
			if event.Type == "turn.completed" {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for turn.completed")
		}
	}
}

func TestCancelStopsTheTurn(t *testing.T) {
	_, agent, cleanup := servePipe(t, Config{Slow: 200 * time.Millisecond})
	defer cleanup()
	ctx := context.Background()
	session, err := agent.Open(ctx, gateway.SessionOpenRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.EventStream(ctx, gateway.EventStreamRequest{SessionID: session.SessionID}); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Send(ctx, gateway.MessageSendRequest{SessionID: session.SessionID, Message: json.RawMessage(`{"text":"slow answer"}`)}); err != nil {
		t.Fatal(err)
	}
	// Wait for the first event, then cancel before the first chunk completes.
	select {
	case <-agent.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("no initial event")
	}
	if err := agent.Cancel(ctx, gateway.CancelRequest{SessionID: session.SessionID, TurnID: "ignored", Reason: "test"}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-agent.Events():
			if event.Type == "turn.completed" {
				t.Fatal("turn completed after cancel")
			}
			if event.Type == "turn.canceled" {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for turn.canceled")
		}
	}
}

func TestVersionMismatchDoesNotActivate(t *testing.T) {
	client, server := net.Pipe()
	go func() {
		_ = Serve(server, server, Config{ProtocolVersion: "v0"})
		_ = server.Close()
	}()
	defer client.Close()
	conn := gateway.NewConn(gateway.ConnOptions{
		AcceptProviders: []gateway.ProviderType{gateway.ProviderTypeAgentBackend},
		RequestTimeout:  2 * time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := conn.Connect(ctx, gateway.NewStdioTransport(gateway.StdioTransportOptions{Stream: client}))
	var handshake *gateway.HandshakeError
	if !errors.As(err, &handshake) {
		t.Fatalf("connect error = %v, want a handshake rejection", err)
	}
	if conn.Health().State == gateway.HealthOK {
		t.Fatalf("connection is healthy after a rejected handshake: %+v", conn.Health())
	}
}

func TestCrashOnSendKillsTheConnection(t *testing.T) {
	conn, agent, cleanup := servePipe(t, Config{CrashOnSend: true})
	defer cleanup()
	ctx := context.Background()
	session, err := agent.Open(ctx, gateway.SessionOpenRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Send(ctx, gateway.MessageSendRequest{SessionID: session.SessionID, Message: json.RawMessage(`{"text":"die"}`)}); err == nil {
		t.Fatal("message.send succeeded on a crash-on-send backend")
	}
	select {
	case <-conn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("connection stayed open after the sidecar crashed")
	}
	if got := agent.Status(session.SessionID); got != gateway.ExternalStatusUnavailable {
		t.Fatalf("status = %q, want backend-unavailable", got)
	}
}

func TestForbiddenEnvAbortsStartup(t *testing.T) {
	t.Setenv("DSH_REF_FORBIDDEN", "secret")
	err := Serve(&emptyReader{}, &emptyWriter{}, Config{ForbiddenEnv: []string{"DSH_REF_FORBIDDEN"}})
	if !errors.Is(err, ErrForbiddenEnv) {
		t.Fatalf("Serve = %v, want ErrForbiddenEnv", err)
	}
}

func TestToolProviderRoundTrip(t *testing.T) {
	client, server := net.Pipe()
	go func() {
		_ = Serve(server, server, Config{Kind: "tool"})
		_ = server.Close()
	}()
	defer client.Close()
	conn := gateway.NewConn(gateway.ConnOptions{
		AcceptProviders: []gateway.ProviderType{gateway.ProviderTypeToolProvider},
		RequestTimeout:  5 * time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := conn.Connect(ctx, gateway.NewStdioTransport(gateway.StdioTransportOptions{Stream: client})); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()
	tools := gateway.NewToolProvider(conn)
	list, err := tools.ListTools(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("tools/list = %+v, %v", list, err)
	}
	result, err := tools.CallTool(ctx, "echo", json.RawMessage(`{"text":"hi"}`))
	if err != nil || len(result.Content) != 1 || result.Content[0].Text != `echo: {"text":"hi"}` {
		t.Fatalf("tools/call echo = %+v, %v", result, err)
	}
	failed, err := tools.CallTool(ctx, "fail", nil)
	if err != nil || !failed.IsError {
		t.Fatalf("tools/call fail = %+v, %v", failed, err)
	}
	if _, err := tools.CallTool(ctx, "missing", nil); err == nil {
		t.Fatal("tools/call missing succeeded")
	}
}

// The two small helpers below let TestForbiddenEnvAbortsStartup fail before it
// ever reads: the environment check runs first.
type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, os.ErrClosed }

type emptyWriter struct{}

func (emptyWriter) Write(p []byte) (int, error) { return len(p), nil }
