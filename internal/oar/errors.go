package oar

import (
	"errors"
	"fmt"
	"strings"
)

// ErrorCode is one stable failure code of the OAR contract. The values are
// part of the frozen contract (docs/OAR.md §7): a host or a test can branch on
// them without matching error strings.
type ErrorCode string

const (
	// CodeDisabled reports that OAR is not configured. It is a normal
	// deployment state, not a failure: callers must not start any process.
	CodeDisabled ErrorCode = "oar_disabled"
	// CodeRuntimeUnavailable reports that the configured external runtime
	// binary (codex) is not installed or cannot be executed.
	CodeRuntimeUnavailable ErrorCode = "runtime_unavailable"
	// CodeRuntimeUnsupported reports that the runtime is installed but lacks a
	// feature the contract requires (for example `codex exec --json`).
	CodeRuntimeUnsupported ErrorCode = "runtime_unsupported"
	// CodeInvalidCapability reports a capability name outside the closed
	// vocabulary, or a declaration that cannot be true (for example a simulated
	// runtime claiming live_model).
	CodeInvalidCapability ErrorCode = "invalid_capability"
	// CodeUnsupportedCapability reports a runtime that activated but did not
	// declare a capability the caller requires. The rejection happens before any
	// session or model call.
	CodeUnsupportedCapability ErrorCode = "unsupported_capability"
	// CodeSimulatedRuntime reports that a caller required a live model but the
	// runtime declared itself simulated. A fake is never silently used as a live
	// model.
	CodeSimulatedRuntime ErrorCode = "simulated_runtime"
	// CodeInvalidRecord reports a record that violates the schema (bad seq,
	// agentPath, kind or required field).
	CodeInvalidRecord ErrorCode = "invalid_record"
	// CodeRecordOrder reports a record whose seq regressed, or reused a seq with
	// different content. Duplicate delivery of the same record is idempotent and
	// is not an error.
	CodeRecordOrder ErrorCode = "record_order"
	// CodeRecordTooLarge reports a record over the journal/bridge size budget.
	CodeRecordTooLarge ErrorCode = "record_too_large"
	// CodeUnknownSession reports a record or call for a session the host did not
	// open (or already closed).
	CodeUnknownSession ErrorCode = "unknown_session"
	// CodeProtocol reports a wire-level failure that is not a gateway typed
	// error (a missing OAR declaration, a malformed result).
	CodeProtocol ErrorCode = "protocol"
)

// Error is the typed error of the OAR contract.
type Error struct {
	Code     ErrorCode
	Runtime  string
	Detail   string
	Required []Capability
	Declared []Capability
	Err      error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("oar: ")
	b.WriteString(string(e.Code))
	if e.Runtime != "" {
		b.WriteString(" (runtime ")
		b.WriteString(e.Runtime)
		b.WriteString(")")
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	if len(e.Required) > 0 {
		fmt.Fprintf(&b, "; required: %s", joinCapabilities(e.Required))
	}
	if len(e.Declared) > 0 {
		fmt.Fprintf(&b, "; declared: %s", joinCapabilities(e.Declared))
	}
	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// Is lets errors.Is(err, &Error{Code: ...}) match by code.
func (e *Error) Is(target error) bool {
	var other *Error
	if !errors.As(target, &other) {
		return false
	}
	return other.Code == "" || other.Code == e.Code
}

// newError is the internal constructor that keeps call sites short.
func newError(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// ErrDisabled is the sentinel errors.Is target for an unconfigured OAR
// service. The returned error is always *Error{Code: CodeDisabled} wrapping it.
var ErrDisabled = errors.New("oar: disabled")

// IsDisabled reports whether err means OAR is not configured.
func IsDisabled(err error) bool {
	return errors.Is(err, ErrDisabled) || hasCode(err, CodeDisabled)
}

// IsCode reports whether err is an OAR typed error with code.
func IsCode(err error, code ErrorCode) bool { return hasCode(err, code) }

func hasCode(err error, code ErrorCode) bool {
	var typed *Error
	return errors.As(err, &typed) && typed.Code == code
}

// CodeOf returns the OAR error code of err ("" when err is not an OAR error or
// is nil). It is the one conversion the HTTP edge and the CLI share.
func CodeOf(err error) ErrorCode {
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return ""
}
