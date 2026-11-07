// One external call leaves one audit record.
//
// The external-gateway spec requires every call to record provider, tool or
// agent, duration and outcome, and forbids recording parameters or message
// bodies. That is enforced by construction here: CallRecord has no field that
// can hold a payload, so no caller can accidentally log one. Secrets that do
// appear in logs elsewhere go through redact.go.

package gateway

import (
	"fmt"
	"time"
)

// CallRecord is the metadata of one external call. Fields are identifiers,
// statuses and numbers only.
type CallRecord struct {
	// Provider is the provider's advertised name from initialize.
	Provider string
	// Method is the JSON-RPC method (tools/call, agent/send, ...).
	Method string
	// Target is the tool name or session id the method acted on, if any.
	Target string
	// Status is one of the Call* constants.
	Status string
	// ErrorCode is the JSON-RPC code of an error response, or 0.
	ErrorCode int
	// Duration is the wall-clock time the call took.
	Duration time.Duration
}

// Call statuses.
const (
	CallOK        = "ok"
	CallError     = "error"
	CallTimeout   = "timeout"
	CallCanceled  = "canceled"
	CallUnhealthy = "unhealthy"
)

// String renders the record as one log line. It contains no payload data.
func (r CallRecord) String() string {
	return fmt.Sprintf("gateway call provider=%s method=%s target=%s status=%s error_code=%d duration=%s",
		r.Provider, r.Method, r.Target, r.Status, r.ErrorCode, r.Duration)
}

// callStatus maps a request outcome to a CallRecord status and JSON-RPC code.
func callStatus(err error) (string, int) {
	switch typed := err.(type) {
	case nil:
		return CallOK, 0
	case *RPCError:
		return CallError, typed.Code
	case *TimeoutError:
		return CallTimeout, 0
	case *CanceledError:
		return CallCanceled, 0
	default:
		return CallUnhealthy, 0
	}
}
