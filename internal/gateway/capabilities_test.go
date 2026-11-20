package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// agentInitializeResult builds a provider's initialize result with an explicit
// agent capability object.
func agentInitializeResult(agent map[string]any, provider map[string]any) map[string]any {
	if provider == nil {
		provider = map[string]any{"type": string(ProviderTypeAgentBackend)}
	}
	return map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{"agent": agent},
		"serverInfo":      map[string]any{"name": "cap-provider", "version": "1"},
		"provider":        provider,
	}
}

// TestAgentCapabilityMatrixIsNegotiated pins the full external-kernel
// capability matrix: every bit a provider declares is visible on the handshake
// and through SupportsAgentCapability/Matrix, including the fork/approval/
// compaction bits the agent-loop-contract requires.
func TestAgentCapabilityMatrixIsNegotiated(t *testing.T) {
	transport, provider := startFakeProvider(t, StdioTransportOptions{})
	provider.setHandler(MethodInitialize, func(json.RawMessage) (any, *ErrorObject) {
		return agentInitializeResult(map[string]any{
			"events":     true,
			"cancel":     true,
			"usage":      true,
			"streaming":  true,
			"tools":      true,
			"approval":   true,
			"fork":       true,
			"resume":     true,
			"compaction": true,
		}, nil), nil
	})
	conn := NewConn(ConnOptions{RequestTimeout: time.Second})
	t.Cleanup(func() { _ = conn.Close() })

	handshake, err := conn.Connect(context.Background(), transport)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if handshake.Capabilities.Agent == nil {
		t.Fatal("agent capability is nil")
	}
	for _, name := range agentCapabilityNames {
		if !handshake.SupportsAgentCapability(name) {
			t.Errorf("capability %q not declared", name)
		}
	}
	matrix := handshake.Capabilities.Agent.Matrix()
	if len(matrix) != len(agentCapabilityNames) {
		t.Fatalf("matrix size = %d, want %d", len(matrix), len(agentCapabilityNames))
	}
	if !matrix[AgentCapabilityFork] || !matrix[AgentCapabilityApproval] || !matrix[AgentCapabilityCompaction] {
		t.Fatalf("matrix = %v, want fork/approval/compaction true", matrix)
	}
	// A legacy bit is still understood, not dropped by the new vocabulary.
	if !matrix[AgentCapabilityEvents] || !matrix[AgentCapabilityUsage] {
		t.Fatalf("matrix = %v, want the legacy events/usage bits", matrix)
	}
}

// TestLegacyAgentCapabilitiesStillActivate is the no-regression control: a
// provider that predates the matrix declares only events/cancel and keeps
// activating when the caller requires nothing.
func TestLegacyAgentCapabilitiesStillActivate(t *testing.T) {
	transport, provider := startFakeProvider(t, StdioTransportOptions{})
	provider.setHandler(MethodInitialize, func(json.RawMessage) (any, *ErrorObject) {
		return agentInitializeResult(map[string]any{"events": true, "cancel": true}, nil), nil
	})
	conn := NewConn(ConnOptions{RequestTimeout: time.Second})
	t.Cleanup(func() { _ = conn.Close() })

	handshake, err := conn.Connect(context.Background(), transport)
	if err != nil {
		t.Fatalf("a legacy provider must still activate: %v", err)
	}
	if handshake.SupportsAgentCapability(AgentCapabilityFork) {
		t.Fatal("legacy provider claims fork")
	}
}

// TestUnsupportedRequiredCapabilityIsTyped rejects a provider that activated
// but lacks a capability the caller requires. The rejection is typed and
// happens before any session is opened.
func TestUnsupportedRequiredCapabilityIsTyped(t *testing.T) {
	transport, provider := startFakeProvider(t, StdioTransportOptions{})
	provider.setHandler(MethodInitialize, func(json.RawMessage) (any, *ErrorObject) {
		return agentInitializeResult(map[string]any{"events": true, "cancel": true}, nil), nil
	})
	conn := NewConn(ConnOptions{
		RequestTimeout:       time.Second,
		RequiredCapabilities: []AgentCapabilityName{AgentCapabilityFork},
	})
	t.Cleanup(func() { _ = conn.Close() })

	_, err := conn.Connect(context.Background(), transport)
	var handshake *HandshakeError
	if !errors.As(err, &handshake) || handshake.Code != HandshakeUnsupportedCapability {
		t.Fatalf("Connect error = %v, want unsupported_capability", err)
	}
	if conn.State() != StateClosed {
		t.Fatalf("state = %q, want closed", conn.State())
	}
	for _, request := range provider.requests() {
		if request.Method == MethodSessionOpen {
			t.Fatalf("a rejected backend received %s; no session may be opened", request.Method)
		}
	}
}

// TestUnknownRequiredCapabilityIsTyped rejects a capability name outside the
// closed vocabulary before the transport starts; it is a caller bug, not a
// provider limitation.
func TestUnknownRequiredCapabilityIsTyped(t *testing.T) {
	transport, provider := startFakeProvider(t, StdioTransportOptions{})
	conn := NewConn(ConnOptions{
		RequestTimeout:       time.Second,
		RequiredCapabilities: []AgentCapabilityName{"teleport"},
	})
	t.Cleanup(func() { _ = conn.Close() })

	_, err := conn.Connect(context.Background(), transport)
	var handshake *HandshakeError
	if !errors.As(err, &handshake) || handshake.Code != HandshakeInvalidCapability {
		t.Fatalf("Connect error = %v, want invalid_capability", err)
	}
	if requests := provider.requests(); len(requests) != 0 {
		t.Fatalf("the provider saw %d requests; an invalid requirement must not start the transport", len(requests))
	}
}

// TestMalformedCapabilityBitIsInvalidResult pins the type check: a capability
// bit that is not a boolean refuses the handshake instead of being coerced.
func TestMalformedCapabilityBitIsInvalidResult(t *testing.T) {
	transport, provider := startFakeProvider(t, StdioTransportOptions{})
	provider.setHandler(MethodInitialize, func(json.RawMessage) (any, *ErrorObject) {
		return agentInitializeResult(map[string]any{"events": true, "fork": "yes"}, nil), nil
	})
	conn := NewConn(ConnOptions{RequestTimeout: time.Second})
	t.Cleanup(func() { _ = conn.Close() })

	_, err := conn.Connect(context.Background(), transport)
	var handshake *HandshakeError
	if !errors.As(err, &handshake) || handshake.Code != HandshakeInvalidResult {
		t.Fatalf("Connect error = %v, want invalid_result", err)
	}
}
