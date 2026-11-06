// Behaviour ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/src/protocol/jsonrpc.ts and protocol/types.ts (MIT License);
// implemented independently in Go. Contract kept identical; see docs/UPSTREAM.md.
//
// This file is the wire layer the client and its transports share: JSON-RPC
// 2.0 message shapes, identifiers, error codes, the MCP types that appear in
// method results, and the protocol versions the client accepts.

package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// JSONRPCVersion is the only protocol version JSON-RPC 2.0 defines.
const JSONRPCVersion = "2.0"

// LatestProtocolVersion is the MCP revision the client requests unless
// configured otherwise. Mirrors LATEST_PROTOCOL_VERSION (types.ts:4).
const LatestProtocolVersion = "2025-11-25"

// SupportedProtocolVersions are the revisions a server may select. Servers that
// do not know the requested one answer with their own latest, so older versions
// stay accepted for servers built on older SDKs (types.ts:5-9).
var SupportedProtocolVersions = []string{
	LatestProtocolVersion,
	"2025-06-18",
	"2025-03-26",
	"2024-11-05",
}

// JSON-RPC error codes (jsonrpc.ts:37-43).
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// ID is a JSON-RPC request identifier: a string or a number (jsonrpc.ts:1).
// The zero value is not a valid identifier.
type ID struct {
	isString bool
	// value is the number literal for numbers and the unescaped string for
	// strings.
	value string
}

// NumberID returns a numeric identifier, as the client uses for its requests.
func NumberID(n int64) ID { return ID{value: strconv.FormatInt(n, 10)} }

// StringID returns a string identifier, as servers commonly use.
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
// request (JSON numbers 1 and 1.0 collide, like JS Map keys).
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
		return nil, fmt.Errorf("mcp: empty JSON-RPC id")
	}
	return []byte(id.value), nil
}

// UnmarshalJSON reads a JSON string or finite number (jsonrpc.ts:89-91).
func (id *ID) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return fmt.Errorf("mcp: invalid JSON-RPC id")
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
		return fmt.Errorf("mcp: invalid JSON-RPC id: %w", err)
	}
	if _, err := strconv.ParseFloat(number.String(), 64); err != nil {
		return fmt.Errorf("mcp: invalid JSON-RPC id: %w", err)
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
// order pi classifies them: response, then request, then notification
// (jsonrpc.ts:93-113). An invalid message is an *McpError with code
// CodeInvalidRequest.
func ParseMessage(data []byte) (Message, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("mcp: parse JSON-RPC message: %w", err)
	}
	if fields == nil {
		return nil, invalidMessage()
	}
	if !jsonStringEquals(fields["jsonrpc"], JSONRPCVersion) {
		return nil, invalidMessage()
	}
	idRaw, hasID := fields["id"]
	methodRaw, hasMethod := fields["method"]
	methodIsString := hasMethod && jsonStringEquals(methodRaw, "") == false
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
			return &SuccessResponse{JSONRPC: JSONRPCVersion, ID: id, Result: append(json.RawMessage(nil), resultRaw...)}, nil
		}
		if hasError {
			var object ErrorObject
			if err := json.Unmarshal(errorRaw, &object); err != nil {
				return nil, invalidMessage()
			}
			// The object decode above validates the shape loosely; require the
			// numeric code and string message that pi checks (jsonrpc.ts:106-107).
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
	return &McpError{Code: CodeInvalidRequest, Message: "Invalid JSON-RPC message"}
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

// McpError is an error a server reported in a JSON-RPC error response, or a
// client-side protocol violation (jsonrpc.ts:45-55).
type McpError struct {
	Code    int
	Message string
	Data    json.RawMessage
}

func (e *McpError) Error() string { return e.Message }

// ConnectionClosedError reports use of a client or transport that closed
// (jsonrpc.ts:57-62).
type ConnectionClosedError struct {
	Message string
}

func (e *ConnectionClosedError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "MCP connection closed"
}

// TimeoutError reports a request that did not settle within its timeout
// (jsonrpc.ts:64-72).
type TimeoutError struct {
	Timeout time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("MCP request timed out after %dms", e.Timeout.Milliseconds())
}

// AbortError reports a request the caller cancelled (jsonrpc.ts:74-79).
type AbortError struct {
	Reason string
}

func (e *AbortError) Error() string {
	if e.Reason != "" {
		return "MCP request aborted: " + e.Reason
	}
	return "MCP request aborted"
}

// Implementation identifies a client or server (types.ts:12-16).
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Title   string `json:"title,omitempty"`
}

// RootsCapability declares support for roots/list.
type RootsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ClientCapabilities is the capabilities object the client sends in initialize
// (types.ts:23-28). Nil maps and pointers are omitted.
type ClientCapabilities struct {
	Experimental map[string]json.RawMessage `json:"experimental,omitempty"`
	Roots        *RootsCapability           `json:"roots,omitempty"`
	Sampling     map[string]json.RawMessage `json:"sampling,omitempty"`
	Elicitation  map[string]json.RawMessage `json:"elicitation,omitempty"`
}

// ListChangedCapability is the shape of a capability with only listChanged.
type ListChangedCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ResourcesCapability is the server's resources capability.
type ResourcesCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

// ServerCapabilities is the capabilities object of an initialize result
// (types.ts:30-37). Unknown fields are ignored.
type ServerCapabilities struct {
	Experimental map[string]json.RawMessage `json:"experimental,omitempty"`
	Logging      map[string]json.RawMessage `json:"logging,omitempty"`
	Prompts      *ListChangedCapability     `json:"prompts,omitempty"`
	Resources    *ResourcesCapability       `json:"resources,omitempty"`
	Tools        *ListChangedCapability     `json:"tools,omitempty"`
	Completions  map[string]json.RawMessage `json:"completions,omitempty"`
}

// Root is an entry of a roots/list result.
type Root struct {
	URI  string `json:"uri"`
	Name string `json:"name,omitempty"`
}

// InitializeResult is a validated initialize result (types.ts:45-50).
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation     `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
}

// ProgressNotification is a notifications/progress payload (types.ts:52-57).
// ProgressToken keeps its wire representation (string or number).
type ProgressNotification struct {
	ProgressToken json.RawMessage `json:"progressToken"`
	Progress      float64         `json:"progress"`
	Total         *float64        `json:"total,omitempty"`
	Message       string          `json:"message,omitempty"`
}

// CancelledNotification is a notifications/cancelled payload (types.ts:59-62).
type CancelledNotification struct {
	RequestID json.RawMessage `json:"requestId"`
	Reason    string          `json:"reason,omitempty"`
}

// Tool is a tool definition from tools/list (types.ts:76-85).
type Tool struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Annotations  json.RawMessage `json:"annotations,omitempty"`
	Execution    json.RawMessage `json:"execution,omitempty"`
	Meta         json.RawMessage `json:"_meta,omitempty"`
}

// Resource is a resource entry from resources/list (types.ts:94-103). Name is
// always present after conversion: servers that omit it get their URI.
type Resource struct {
	URI         string          `json:"uri"`
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	MimeType    string          `json:"mimeType,omitempty"`
	Size        *int64          `json:"size,omitempty"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
	Meta        json.RawMessage `json:"_meta,omitempty"`
}

// ResourceTemplate is an entry of resources/templates/list (types.ts:106-114).
type ResourceTemplate struct {
	URITemplate string          `json:"uriTemplate"`
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	MimeType    string          `json:"mimeType,omitempty"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
	Meta        json.RawMessage `json:"_meta,omitempty"`
}

// ListToolsResult is a page of tools/list.
type ListToolsResult struct {
	Tools      []Tool          `json:"tools"`
	NextCursor string          `json:"nextCursor,omitempty"`
	Meta       json.RawMessage `json:"_meta,omitempty"`
}

// ListResourcesResult is a page of resources/list.
type ListResourcesResult struct {
	Resources  []Resource      `json:"resources"`
	NextCursor string          `json:"nextCursor,omitempty"`
	Meta       json.RawMessage `json:"_meta,omitempty"`
}

// ListResourceTemplatesResult is a page of resources/templates/list.
type ListResourceTemplatesResult struct {
	ResourceTemplates []ResourceTemplate `json:"resourceTemplates"`
	NextCursor        string             `json:"nextCursor,omitempty"`
	Meta              json.RawMessage    `json:"_meta,omitempty"`
}

// ResourceContents is one item of a resources/read result: text or a base64
// blob (types.ts:128-131).
type ResourceContents struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// ReadResourceResult is a validated resources/read result.
type ReadResourceResult struct {
	Contents []ResourceContents `json:"contents"`
	Meta     json.RawMessage    `json:"_meta,omitempty"`
}

// isSupportedProtocolVersion reports whether a server may select version.
func isSupportedProtocolVersion(version string) bool {
	for _, supported := range SupportedProtocolVersions {
		if version == supported {
			return true
		}
	}
	return false
}
