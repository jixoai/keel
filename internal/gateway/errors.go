// Error types of the gateway. Every failure a caller can observe is a typed
// error: which failures are retryable, which are the caller's fault and which
// mean the provider is unhealthy must be distinguishable without string
// matching (external-gateway spec: error mapping, deterministic timeouts).

package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrTransportClosed is the sentinel wrapped by a *TransportError caused by
// Close.
var ErrTransportClosed = errors.New("gateway: transport closed")

// ProtocolError reports a message that is not a valid JSON-RPC 2.0 frame
// (parse failures and envelope violations).
type ProtocolError struct {
	Code    int
	Message string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("gateway: invalid JSON-RPC message: %s", e.Message)
}

// RPCError is an error a provider reported in a JSON-RPC error response. The
// gateway's own failure modes never use it; they have their own types below.
type RPCError struct {
	Code    int
	Message string
	Data    json.RawMessage
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("gateway: provider error %d: %s", e.Code, e.Message)
}

// IsCode reports whether the provider used the given JSON-RPC error code.
func (e *RPCError) IsCode(code int) bool { return e.Code == code }

// TimeoutError reports a request that did not settle within its deadline. It
// is always raised locally: the provider may answer later and that answer is
// dropped.
type TimeoutError struct {
	Method    string
	RequestID string
	Timeout   time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("gateway: %s (%s) timed out after %s", e.Method, e.RequestID, e.Timeout)
}

// CanceledError reports a request the caller cancelled, or one the provider
// answered with CodeCanceled.
type CanceledError struct {
	Method    string
	RequestID string
	Reason    string
}

func (e *CanceledError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("gateway: %s (%s) cancelled: %s", e.Method, e.RequestID, e.Reason)
	}
	return fmt.Sprintf("gateway: %s (%s) cancelled", e.Method, e.RequestID)
}

// UnhealthyError reports use of a provider the gateway has marked unhealthy,
// or a request failed by the connection dying (crash, framing desync, too many
// bad frames). Callers degrade this provider; other providers are unaffected
// (external-gateway spec: failure isolation).
type UnhealthyError struct {
	Reason string
	Err    error
}

func (e *UnhealthyError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("gateway: provider unhealthy: %s: %v", e.Reason, e.Err)
	}
	return "gateway: provider unhealthy: " + e.Reason
}

func (e *UnhealthyError) Unwrap() error { return e.Err }

// ConnClosedError reports use of a connection the caller closed.
type ConnClosedError struct {
	Reason string
}

func (e *ConnClosedError) Error() string {
	if e.Reason != "" {
		return "gateway: connection closed: " + e.Reason
	}
	return "gateway: connection closed"
}

// TransportError reports a transport-level failure: a read or write error, or
// a stream that ended because the transport was closed.
type TransportError struct {
	Reason string
	Err    error
}

func (e *TransportError) Error() string {
	if e.Err != nil && e.Reason != "" {
		return fmt.Sprintf("gateway: %s: %v", e.Reason, e.Err)
	}
	if e.Err != nil {
		return "gateway: " + e.Err.Error()
	}
	return "gateway: " + e.Reason
}

func (e *TransportError) Unwrap() error { return e.Err }

// HandshakeError reports a connection that must not activate: an unsupported
// protocol revision, a provider kind the client does not accept, a missing
// capability, or a malformed initialize result.
type HandshakeError struct {
	Code   string
	Detail string
}

func (e *HandshakeError) Error() string {
	return fmt.Sprintf("gateway: handshake failed (%s): %s", e.Code, e.Detail)
}

// Handshake error codes.
const (
	// HandshakeInvalidResult is a malformed or incomplete initialize result.
	HandshakeInvalidResult = "invalid_result"
	// HandshakeUnsupportedVersion is a protocol revision this client cannot speak.
	HandshakeUnsupportedVersion = "unsupported_protocol_version"
	// HandshakeProviderNotAccepted is a provider kind the client did not offer.
	HandshakeProviderNotAccepted = "provider_type_not_accepted"
	// HandshakeMissingCapability is a provider whose kind's mandatory capability
	// object is absent.
	HandshakeMissingCapability = "missing_capability"
	// HandshakeInvalidProvider is an unknown provider kind or state owner.
	HandshakeInvalidProvider = "invalid_provider"
	// HandshakeTransportStart is a transport that failed to start.
	HandshakeTransportStart = "transport_start"
	// HandshakeInitialize is an initialize request that failed or timed out.
	HandshakeInitialize = "initialize"
)

// ProviderTypeError reports a provider call made against the wrong kind of
// connection (for example CallTool on an AgentBackend connection).
type ProviderTypeError struct {
	Call string
	Want ProviderType
	Got  ProviderType
}

func (e *ProviderTypeError) Error() string {
	return fmt.Sprintf("gateway: %s requires a %s connection, this one is %s", e.Call, e.Want, e.Got)
}

// HTTPStatusError is a non-2xx response from a Streamable HTTP provider. Body
// is a bounded excerpt, never the whole body.
type HTTPStatusError struct {
	Status int
	Body   string
}

func (e *HTTPStatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("gateway: HTTP request failed with status %d", e.Status)
	}
	return fmt.Sprintf("gateway: HTTP request failed with status %d: %s", e.Status, e.Body)
}

// FrameErrorKind classifies framing failures.
type FrameErrorKind string

const (
	// FrameMalformed is bytes that are not valid JSON, or a frame cut off by EOF.
	FrameMalformed FrameErrorKind = "malformed"
	// FrameTooLarge is a frame over the configured limit.
	FrameTooLarge FrameErrorKind = "too_large"
	// FrameDesync is lost framing: the bad frame's boundary could not be found,
	// so no further frame can be trusted. It is fatal for the connection.
	FrameDesync FrameErrorKind = "desync"
)

// FrameError reports one bad frame.
//
// A non-fatal inbound FrameError means the frame was skipped and the reader
// resynchronized; the connection is usable and the diagnostic explains it. A
// fatal one (FrameDesync, or any FrameError with Fatal set) means framing is
// lost. An outbound FrameError (Direction "outbound") means the message was
// never written because it exceeds the limit.
//
// Detail never contains raw frame bytes (external-gateway spec: secrets must
// not leak into logs); Size is the number of bytes involved.
type FrameError struct {
	Kind      FrameErrorKind
	Direction string
	Size      int
	Limit     int
	Fatal     bool
	Detail    string
}

func (e *FrameError) Error() string {
	switch e.Kind {
	case FrameTooLarge:
		if e.Direction == "outbound" {
			return fmt.Sprintf("gateway: outbound frame of %d bytes exceeds the %d byte limit", e.Size, e.Limit)
		}
		return fmt.Sprintf("gateway: inbound frame exceeded the %d byte limit and was skipped", e.Limit)
	case FrameDesync:
		return "gateway: framing lost after an oversized frame; the stream cannot be resynchronized"
	default:
		return "gateway: malformed frame: " + e.Detail
	}
}
