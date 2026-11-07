// This file is the wire layer shared by the transports, the connection and the
// provider clients: JSON-RPC 2.0 message shapes, identifiers, error codes and
// the protocol constants.
//
// Shapes and classification rules deliberately match internal/mcp (JSON-RPC
// 2.0, and the MCP wire order: response, then request, then notification), so
// the gateway and the MCP client classify the same bytes the same way. The
// gateway keeps its own types only because its lifecycle differs from the MCP
// client's: provider-kind negotiation, cancellation notifications, health and
// bounded backpressure. protocol_test.go cross-checks the two parsers.

package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// JSONRPCVersion is the only protocol version JSON-RPC 2.0 defines.
const JSONRPCVersion = "2.0"

// ProtocolVersion is the revision of the Keel external-provider protocol: its
// method set, its lifecycle and its negotiation rules. Revisions are
// date-based, like MCP's. The method semantics belong to this protocol, not to
// MCP: only the transport, the framing and the request lifecycle are reused
// from MCP (see README.md). A provider that does not know the requested
// revision answers with its own latest, and the client refuses to activate a
// connection whose revision it cannot speak (external-gateway spec: initialize
// negotiates the version, an incompatible version must not activate).
const ProtocolVersion = "2026-11-07"

// SupportedProtocolVersions lists the revisions a connection may settle on.
// Ordered newest first, like internal/mcp's list.
var SupportedProtocolVersions = []string{
	ProtocolVersion,
}

// ClientName and ClientVersion identify this gateway in initialize. They are
// Keel's own names on the wire; an external provider has no business knowing
// which product embeds the gateway.
const (
	ClientName    = "keel-gateway"
	ClientVersion = "0.1.0"
)

// JSON-RPC error codes. The first five are the standard ones
// (jsonrpc.org/specification); the -32001..-32003 range is reserved by this
// protocol for the conditions the gateway itself reports, so a provider can
// say "I timed out" / "I cancelled this" / "I am unavailable" without
// inventing private codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603

	// CodeTimeout reports that the provider's own request deadline expired.
	CodeTimeout = -32001
	// CodeCanceled reports that the provider aborted a request.
	CodeCanceled = -32002
	// CodeUnavailable reports that the provider cannot serve the request now.
	CodeUnavailable = -32003
)

// Method names.
//
// Two surfaces live in one protocol and are mutually exclusive per connection:
// the connections's provider kind decides which one is legal.
//
// The tool surface is MCP's, verbatim (tools/list, tools/call), because a
// stateless JSON tool provider is exactly what MCP already defines and this
// project reuses MCP semantics for tools instead of inventing a third tool
// protocol (proposal: 不另造第三套工具协议). It is only valid on tool_provider
// connections.
//
// The agent surface is this protocol's own versioned, private method set. It
// deliberately does NOT dress itself in MCP method names: an external agent
// runtime is not an MCP server, and pretending otherwise would promise MCP
// guarantees (resources, prompts, sampling) this protocol does not make.
const (
	// MethodInitialize is the first request of every connection: it negotiates
	// the protocol revision, the provider kind and the capability sets.
	MethodInitialize = "initialize"

	// MethodSessionOpen opens a session on an AgentBackend.
	MethodSessionOpen = "session.open"
	// MethodSessionResume resumes (or reports) a session on an AgentBackend.
	MethodSessionResume = "session.resume"
	// MethodMessageSend hands one input message to a backend session.
	MethodMessageSend = "message.send"
	// MethodEventStream is both the subscription request (with an id) and the
	// event notification (without one): see README.md.
	MethodEventStream = "event.stream"
	// MethodCancel cancels a backend session's current turn.
	MethodCancel = "cancel"
	// MethodHealth probes a backend and reports its availability.
	MethodHealth = "health"
	// MethodClose closes a session, or asks the backend to shut down when it
	// carries no session id.
	MethodClose = "close"

	// MethodToolsList returns a tool provider's catalogue (MCP, tool surface).
	MethodToolsList = "tools/list"
	// MethodToolsCall runs one stateless tool (MCP, tool surface).
	MethodToolsCall = "tools/call"

	// MethodRequestCancel is the MCP request-lifecycle cancellation
	// notification. It is part of the transport lifecycle this protocol reuses
	// from MCP, not part of either method surface: it aborts one in-flight
	// JSON-RPC request. Turn-level cancellation is the agent surface's own
	// "cancel" method.
	MethodRequestCancel = "notifications/cancelled"
)

// ID is a JSON-RPC request identifier: a string or a number. The zero value is
// not a valid identifier.
type ID struct {
	isString bool
	// value is the number literal for numbers and the unescaped string for
	// strings.
	value string
}

// NumberID returns a numeric identifier, as this gateway uses for its requests.
func NumberID(n int64) ID { return ID{value: strconv.FormatInt(n, 10)} }

// StringID returns a string identifier, as providers commonly use.
func StringID(s string) ID { return ID{isString: true, value: s} }

// IsZero reports whether id is unset.
func (id ID) IsZero() bool { return id.value == "" && !id.isString }

// String renders the identifier for logs and error messages.
func (id ID) String() string {
	if id.isString {
		return strconv.Quote(id.value)
	}
	return id.value
}

// Key is a map key that treats numerically equal identifiers as the same
// request: JSON numbers 1 and 1.0 collide, like JS Map keys. internal/mcp uses
// the same normalization, so ids correlate identically on both layers.
func (id ID) Key() string {
	if id.isString {
		return "s:" + id.value
	}
	if number, err := strconv.ParseFloat(id.value, 64); err == nil {
		return "n:" + strconv.FormatFloat(number, 'g', -1, 64)
	}
	return "n:" + id.value
}

// MarshalJSON writes the identifier as a JSON string or number.
func (id ID) MarshalJSON() ([]byte, error) {
	if id.isString {
		return json.Marshal(id.value)
	}
	if id.value == "" {
		return nil, fmt.Errorf("gateway: empty JSON-RPC id")
	}
	return []byte(id.value), nil
}

// UnmarshalJSON reads a JSON string or finite number. A null, an object or an
// array is not an identifier.
func (id *ID) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return fmt.Errorf("gateway: invalid JSON-RPC id")
	}
	if trimmed[0] == '"' {
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return err
		}
		*id = StringID(value)
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(trimmed, &number); err != nil {
		return fmt.Errorf("gateway: invalid JSON-RPC id: %w", err)
	}
	if _, err := strconv.ParseFloat(number.String(), 64); err != nil {
		return fmt.Errorf("gateway: invalid JSON-RPC id: %w", err)
	}
	id.isString = false
	id.value = number.String()
	return nil
}

// Request is a JSON-RPC call expecting a response.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      ID              `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (*Request) isMessage() {}

// Notification is a JSON-RPC message without an identifier.
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (*Notification) isMessage() {}

// ErrorObject is the error of a JSON-RPC error response.
type ErrorObject struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// SuccessResponse is a JSON-RPC response with a result. A nil Result marshals
// as JSON null.
type SuccessResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      ID              `json:"id"`
	Result  json.RawMessage `json:"result"`
}

func (*SuccessResponse) isMessage() {}

// ErrorResponse is a JSON-RPC response carrying an error.
type ErrorResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      ID          `json:"id"`
	Error   ErrorObject `json:"error"`
}

func (*ErrorResponse) isMessage() {}

// Message is one JSON-RPC message: *Request, *Notification, *SuccessResponse,
// or *ErrorResponse.
type Message interface {
	isMessage()
}

// ParseMessage validates a decoded JSON value as a JSON-RPC message, in the
// order the wire allows: response, then request, then notification. An invalid
// message is a *ProtocolError with code CodeInvalidRequest.
func ParseMessage(data []byte) (Message, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("gateway: parse JSON-RPC message: %w", err)
	}
	if fields == nil {
		return nil, invalidMessage()
	}
	if !jsonStringEquals(fields["jsonrpc"], JSONRPCVersion) {
		return nil, invalidMessage()
	}
	idRaw, hasID := fields["id"]
	methodRaw, hasMethod := fields["method"]
	methodIsString := hasMethod && !jsonStringEquals(methodRaw, "")
	var method string
	if methodIsString {
		_ = json.Unmarshal(methodRaw, &method)
	}
	if hasID {
		var id ID
		if err := json.Unmarshal(idRaw, &id); err != nil {
			return nil, invalidMessage()
		}
		resultRaw, hasResult := fields["result"]
		errorRaw, hasError := fields["error"]
		if hasResult && !hasError {
			return &SuccessResponse{
				JSONRPC: JSONRPCVersion,
				ID:      id,
				Result:  copyRaw(resultRaw),
			}, nil
		}
		if hasError {
			var object ErrorObject
			if err := json.Unmarshal(errorRaw, &object); err != nil {
				return nil, invalidMessage()
			}
			// The object decode above validates the shape loosely; require the
			// numeric code and string message JSON-RPC mandates.
			var errorFields map[string]json.RawMessage
			if err := json.Unmarshal(errorRaw, &errorFields); err != nil || errorFields == nil {
				return nil, invalidMessage()
			}
			if _, ok := errorFields["code"]; !ok || !jsonNumberValid(errorFields["code"]) {
				return nil, invalidMessage()
			}
			if !jsonStringPresent(errorFields["message"]) {
				return nil, invalidMessage()
			}
			return &ErrorResponse{JSONRPC: JSONRPCVersion, ID: id, Error: object}, nil
		}
		if methodIsString {
			return &Request{
				JSONRPC: JSONRPCVersion,
				ID:      id,
				Method:  method,
				Params:  copyOptionalRaw(fields["params"]),
			}, nil
		}
		return nil, invalidMessage()
	}
	if methodIsString {
		return &Notification{
			JSONRPC: JSONRPCVersion,
			Method:  method,
			Params:  copyOptionalRaw(fields["params"]),
		}, nil
	}
	return nil, invalidMessage()
}

func invalidMessage() error {
	return &ProtocolError{Code: CodeInvalidRequest, Message: "Invalid JSON-RPC message"}
}

func copyRaw(raw json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}

func copyOptionalRaw(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func jsonStringEquals(raw json.RawMessage, want string) bool {
	if raw == nil {
		return false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	return value == want
}

// jsonStringPresent reports whether raw is a JSON string of any value.
func jsonStringPresent(raw json.RawMessage) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil
}

// jsonNumberValid reports whether raw is a JSON number.
func jsonNumberValid(raw json.RawMessage) bool {
	var number json.Number
	return json.Unmarshal(raw, &number) == nil
}
