// Initialize: the first exchange of every connection. It negotiates the
// protocol revision, the provider kind and the capability sets, and it is the
// one place where a provider's place in the Keel service graph is decided
// (external-gateway spec: initialization and capability negotiation; an
// incompatible version must not activate).

package gateway

import (
	"encoding/json"
	"fmt"
)

// ProviderType is the kind of external provider behind a connection.
type ProviderType string

const (
	// ProviderTypeToolProvider is a stateless tool provider: JSON in, JSON out.
	ProviderTypeToolProvider ProviderType = "tool_provider"
	// ProviderTypeAgentBackend is a whole external agent runtime that owns its
	// own session state (a DSH sidecar, for example).
	ProviderTypeAgentBackend ProviderType = "agent_backend"
)

// Valid reports whether p is a provider kind this protocol defines.
func (p ProviderType) Valid() bool {
	return p == ProviderTypeToolProvider || p == ProviderTypeAgentBackend
}

// StateOwner names who owns the state of a session. It is the value that ends
// up in `sessions.state_owner` (P0 data model: keel | external).
type StateOwner string

const (
	// StateOwnerKeel means the session lives in Keel's event log and can be
	// rebuilt from it.
	StateOwnerKeel StateOwner = "keel"
	// StateOwnerExternal means the external backend owns the session state.
	// The session must be recorded as an external session and must not enter
	// Keel's native run/lease flow, and Keel-native recovery must not be
	// claimed for it.
	StateOwnerExternal StateOwner = "external"
)

// Valid reports whether o is a state owner this protocol defines.
func (o StateOwner) Valid() bool { return o == StateOwnerKeel || o == StateOwnerExternal }

// Recovery modes a backend may declare.
const (
	// RecoveryNone means a crashed session is lost; the caller degrades.
	RecoveryNone = "none"
	// RecoveryBackend means the backend can resume a session from its own log.
	RecoveryBackend = "backend"
)

// Metering marks how a backend session's model usage is accounted. The marker
// is mandatory for external sessions: usage MUST NOT be silently recorded as
// zero just because Keel cannot see it (external-gateway spec: 外部用量计量边界).
type Metering string

const (
	// MeteringExternal means usage happens in the external backend and is
	// accounted there; Keel only records the marker and whatever counters the
	// backend reports, never as native provider usage.
	MeteringExternal Metering = "external"
	// MeteringNone means the session's model usage cannot be metered at all.
	// Billing and reports must show "not measurable", not "0".
	MeteringNone Metering = "none"
)

// Valid reports whether m is a metering mark this protocol defines.
func (m Metering) Valid() bool { return m == MeteringExternal || m == MeteringNone }

// Implementation identifies a client or provider.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Title   string `json:"title,omitempty"`
}

// ToolsCapability is the client side of the tools capability.
type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// AgentCapability is the client side of the agent-backend capability.
type AgentCapability struct {
	Events bool `json:"events,omitempty"`
}

// ClientCapabilities is what the gateway offers a provider.
type ClientCapabilities struct {
	Tools        *ToolsCapability           `json:"tools,omitempty"`
	Agent        *AgentCapability           `json:"agent,omitempty"`
	Experimental map[string]json.RawMessage `json:"experimental,omitempty"`
}

// ServerToolsCapability is a provider's tools capability.
type ServerToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ServerAgentCapability is a provider's agent capability: whether it pushes
// events, supports cancellation, and reports token usage.
type ServerAgentCapability struct {
	Events bool `json:"events,omitempty"`
	Cancel bool `json:"cancel,omitempty"`
	// Usage reports whether the backend emits token usage on its events. When
	// it is absent the sessions default to metering=none.
	Usage bool `json:"usage,omitempty"`
}

// ServerCapabilities is what a provider declares it supports.
type ServerCapabilities struct {
	Tools        *ServerToolsCapability     `json:"tools,omitempty"`
	Agent        *ServerAgentCapability     `json:"agent,omitempty"`
	Experimental map[string]json.RawMessage `json:"experimental,omitempty"`
}

// ProviderInfo is a provider's declaration about itself. Version and detailed
// capabilities live in the surrounding initialize result.
type ProviderInfo struct {
	Type ProviderType `json:"type"`
	// StateOwner is meaningful for agent_backend: keel or external. Absent
	// means external, because that is the safe assumption.
	StateOwner StateOwner `json:"stateOwner,omitempty"`
	// Recovery is the declared recovery mode of the backend.
	Recovery string `json:"recovery,omitempty"`
	// Metering is the backend's default usage mark for its sessions. Absent
	// means none (never "zero usage").
	Metering Metering `json:"metering,omitempty"`
}

// InitializeParams is the params of an initialize request.
type InitializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	ClientInfo      Implementation     `json:"clientInfo"`
	Capabilities    ClientCapabilities `json:"capabilities"`
	// AcceptProviders lists the provider kinds this client is willing to
	// activate. A provider whose kind is not listed is rejected.
	AcceptProviders []ProviderType `json:"acceptProviders,omitempty"`
}

// InitializeResult is a provider's answer to initialize.
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation     `json:"serverInfo"`
	Provider        ProviderInfo       `json:"provider"`
	Instructions    string             `json:"instructions,omitempty"`
}

// Handshake is a successfully negotiated connection: what the caller may rely
// on for the lifetime of the connection.
type Handshake struct {
	ProtocolVersion string
	Provider        ProviderInfo
	ServerInfo      Implementation
	Capabilities    ServerCapabilities
	Instructions    string
}

// validateInitializeResult checks the required fields, the protocol revision,
// the provider kind, the advertised capabilities and the state/metering marks,
// then returns the negotiated handshake. Every rejection is a *HandshakeError,
// so a connection either activates with a known contract or does not activate
// at all.
func validateInitializeResult(raw json.RawMessage, accept []ProviderType) (Handshake, error) {
	invalid := func(detail string) error {
		return &HandshakeError{Code: HandshakeInvalidResult, Detail: detail}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return Handshake{}, invalid("result is not a JSON object")
	}
	var version string
	if err := json.Unmarshal(fields["protocolVersion"], &version); err != nil {
		return Handshake{}, invalid("protocolVersion is missing or not a string")
	}
	if !isSupportedProtocolVersion(version) {
		return Handshake{}, &HandshakeError{
			Code:   HandshakeUnsupportedVersion,
			Detail: fmt.Sprintf("provider selected %q, supported: %v", version, SupportedProtocolVersions),
		}
	}
	var capabilities map[string]json.RawMessage
	if rawCaps, ok := fields["capabilities"]; !ok || isJSONNull(rawCaps) ||
		json.Unmarshal(rawCaps, &capabilities) != nil || capabilities == nil {
		return Handshake{}, invalid("capabilities is missing or not an object")
	}
	var serverInfo map[string]json.RawMessage
	if rawInfo, ok := fields["serverInfo"]; !ok || isJSONNull(rawInfo) ||
		json.Unmarshal(rawInfo, &serverInfo) != nil || serverInfo == nil {
		return Handshake{}, invalid("serverInfo is missing or not an object")
	}
	var name, serverVersion string
	if err := json.Unmarshal(serverInfo["name"], &name); err != nil {
		return Handshake{}, invalid("serverInfo.name is missing or not a string")
	}
	if err := json.Unmarshal(serverInfo["version"], &serverVersion); err != nil {
		return Handshake{}, invalid("serverInfo.version is missing or not a string")
	}
	var providerFields map[string]json.RawMessage
	if rawProvider, ok := fields["provider"]; !ok || isJSONNull(rawProvider) ||
		json.Unmarshal(rawProvider, &providerFields) != nil || providerFields == nil {
		return Handshake{}, invalid("provider is missing or not an object")
	}
	var provider ProviderInfo
	if err := json.Unmarshal(fields["provider"], &provider); err != nil {
		return Handshake{}, invalid("provider is malformed")
	}
	if !provider.Type.Valid() {
		return Handshake{}, &HandshakeError{
			Code:   HandshakeInvalidProvider,
			Detail: fmt.Sprintf("unknown provider type %q", provider.Type),
		}
	}
	if len(accept) > 0 && !acceptsProvider(accept, provider.Type) {
		return Handshake{}, &HandshakeError{
			Code:   HandshakeProviderNotAccepted,
			Detail: fmt.Sprintf("provider type %q was not accepted by this client", provider.Type),
		}
	}
	switch provider.Type {
	case ProviderTypeAgentBackend:
		switch provider.StateOwner {
		case "":
			// Unspecified means the safe assumption: the backend owns its state.
			provider.StateOwner = StateOwnerExternal
		case StateOwnerKeel, StateOwnerExternal:
		default:
			return Handshake{}, &HandshakeError{
				Code:   HandshakeInvalidProvider,
				Detail: fmt.Sprintf("unknown stateOwner %q", provider.StateOwner),
			}
		}
		if provider.Recovery == "" {
			provider.Recovery = RecoveryNone
		}
		switch provider.Metering {
		case "":
			// Usage is only marked external when the backend says it reports it;
			// otherwise the honest mark is "none", never a silent zero.
			if capabilities["agent"] != nil {
				var agent ServerAgentCapability
				if err := json.Unmarshal(capabilities["agent"], &agent); err == nil && agent.Usage {
					provider.Metering = MeteringExternal
				} else {
					provider.Metering = MeteringNone
				}
			} else {
				provider.Metering = MeteringNone
			}
		case MeteringExternal, MeteringNone:
		default:
			return Handshake{}, &HandshakeError{
				Code:   HandshakeInvalidProvider,
				Detail: fmt.Sprintf("unknown metering mark %q", provider.Metering),
			}
		}
	case ProviderTypeToolProvider:
		if provider.StateOwner != "" || provider.Metering != "" || provider.Recovery != "" {
			// Tool providers are stateless: session marks on them are a
			// declaration mistake, and accepting it would hide a wiring bug.
			return Handshake{}, &HandshakeError{
				Code:   HandshakeInvalidProvider,
				Detail: "tool_provider must not declare session state ownership, recovery or metering",
			}
		}
	}
	if provider.Type == ProviderTypeToolProvider {
		if capabilities["tools"] == nil || isJSONNull(capabilities["tools"]) {
			return Handshake{}, &HandshakeError{
				Code:   HandshakeMissingCapability,
				Detail: "tool_provider must declare the tools capability",
			}
		}
	} else {
		if capabilities["agent"] == nil || isJSONNull(capabilities["agent"]) {
			return Handshake{}, &HandshakeError{
				Code:   HandshakeMissingCapability,
				Detail: "agent_backend must declare the agent capability",
			}
		}
	}
	var result InitializeResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return Handshake{}, invalid("result is malformed")
	}
	result.Provider = provider
	result.ProtocolVersion = version
	result.ServerInfo = Implementation{Name: name, Version: serverVersion, Title: result.ServerInfo.Title}
	return Handshake{
		ProtocolVersion: result.ProtocolVersion,
		Provider:        result.Provider,
		ServerInfo:      result.ServerInfo,
		Capabilities:    result.Capabilities,
		Instructions:    result.Instructions,
	}, nil
}

func acceptsProvider(accept []ProviderType, got ProviderType) bool {
	for _, candidate := range accept {
		if candidate == got {
			return true
		}
	}
	return false
}

// isSupportedProtocolVersion reports whether the client may activate a
// connection that settles on version.
func isSupportedProtocolVersion(version string) bool {
	for _, supported := range SupportedProtocolVersions {
		if version == supported {
			return true
		}
	}
	return false
}

func isJSONNull(raw json.RawMessage) bool {
	return string(raw) == "null"
}
