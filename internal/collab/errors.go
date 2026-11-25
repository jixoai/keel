package collab

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Sentinel error kinds. Every refusal is one of these wrapped in *Error, so a
// caller can branch with errors.Is and still read the structured detail with
// errors.As. The sentinels are stable contracts; the codes are the wire/log
// vocabulary.
var (
	ErrInvalidMode           = errors.New("collab: invalid mode descriptor")
	ErrDuplicateMode         = errors.New("collab: duplicate mode id")
	ErrUnknownMode           = errors.New("collab: unknown mode id")
	ErrModeDisabled          = errors.New("collab: mode is disabled for new work")
	ErrTenantRequired        = errors.New("collab: tenant is required")
	ErrTenantMismatch        = errors.New("collab: tenant mismatch")
	ErrInvalidRequest        = errors.New("collab: invalid request")
	ErrInvalidCreation       = errors.New("collab: invalid session creation")
	ErrSessionNotFound       = errors.New("collab: session not found")
	ErrSessionExists         = errors.New("collab: session already tracked")
	ErrParentSessionNotFound = errors.New("collab: parent session not found")
	ErrParentSessionInactive = errors.New("collab: parent session is not active")
	ErrWorkItemNotFound      = errors.New("collab: work item not found")
	ErrWorkItemClosed        = errors.New("collab: work item is closed")
	ErrAssignmentNotFound    = errors.New("collab: assignment not found")
	ErrAttentionNotFound     = errors.New("collab: attention not found")
	ErrAttentionClosed       = errors.New("collab: attention is already terminal")
	ErrAttentionExpired      = errors.New("collab: attention deadline passed")
	ErrAttentionConflict     = errors.New("collab: attention was already answered differently")
	ErrPermissionDenied      = errors.New("collab: permission denied")
	ErrLimitExceeded         = errors.New("collab: limit exceeded")
	ErrDepthExceeded         = errors.New("collab: depth limit exceeded")
	ErrWidthExceeded         = errors.New("collab: width limit exceeded")
	ErrConcurrencyExceeded   = errors.New("collab: concurrency limit exceeded")
	ErrBudgetExceeded        = errors.New("collab: budget exceeded")
	ErrAuditUnavailable      = errors.New("collab: audit sink is unavailable")
	ErrCancelFailed          = errors.New("collab: cancel propagation failed")
	ErrAttentionPublish      = errors.New("collab: attention could not be published")
)

// ErrorCode is the stable machine-readable refusal vocabulary returned in
// typed errors and written to audit rows.
type ErrorCode string

const (
	CodeInvalidMode         ErrorCode = "invalid_mode"
	CodeDuplicateMode       ErrorCode = "duplicate_mode"
	CodeUnknownMode         ErrorCode = "unknown_mode"
	CodeModeDisabled        ErrorCode = "mode_disabled"
	CodeTenantRequired      ErrorCode = "tenant_required"
	CodeTenantMismatch      ErrorCode = "tenant_mismatch"
	CodeInvalidRequest      ErrorCode = "invalid_request"
	CodeInvalidCreation     ErrorCode = "invalid_creation"
	CodeSessionNotFound     ErrorCode = "session_not_found"
	CodeSessionExists       ErrorCode = "session_exists"
	CodeParentNotFound      ErrorCode = "parent_session_not_found"
	CodeParentInactive      ErrorCode = "parent_session_inactive"
	CodeWorkItemNotFound    ErrorCode = "work_item_not_found"
	CodeWorkItemClosed      ErrorCode = "work_item_closed"
	CodeAssignmentNotFound  ErrorCode = "assignment_not_found"
	CodeAttentionNotFound   ErrorCode = "attention_not_found"
	CodeAttentionClosed     ErrorCode = "attention_closed"
	CodeAttentionExpired    ErrorCode = "attention_expired"
	CodeAttentionConflict   ErrorCode = "attention_conflict"
	CodePermissionDenied    ErrorCode = "permission_denied"
	CodeLimitExceeded       ErrorCode = "limit_exceeded"
	CodeDepthExceeded       ErrorCode = "depth_exceeded"
	CodeWidthExceeded       ErrorCode = "width_exceeded"
	CodeConcurrencyExceeded ErrorCode = "concurrency_exceeded"
	CodeBudgetExceeded      ErrorCode = "budget_exceeded"
	CodeAuditUnavailable    ErrorCode = "audit_unavailable"
	CodeCancelFailed        ErrorCode = "cancel_failed"
	CodeAttentionPublish    ErrorCode = "attention_publish_failed"
)

// Error is one structured collaboration refusal. Kind is the errors.Is target;
// Code is the stable log/UI vocabulary; the ids identify the refused operation
// without carrying resource contents.
type Error struct {
	Kind         error
	Code         ErrorCode
	Operation    string
	TenantID     uuid.UUID
	ModeID       string
	WorkItemID   uuid.UUID
	AssignmentID uuid.UUID
	SessionID    uuid.UUID
	BotID        string
	Field        string
	Limit        int64
	Actual       int64
	Detail       string
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	base := "collab: error"
	if e.Kind != nil {
		base = e.Kind.Error()
	}
	var b strings.Builder
	b.WriteString(base)
	if e.Operation != "" {
		b.WriteString(" [")
		b.WriteString(e.Operation)
		b.WriteString("]")
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	if e.Field != "" {
		fmt.Fprintf(&b, " (%s: requested %d, limit %d)", e.Field, e.Actual, e.Limit)
	}
	return b.String()
}

// Unwrap exposes the sentinel so errors.Is works across adapter boundaries.
func (e *Error) Unwrap() error { return e.Kind }

// IsCode reports whether err is a collab refusal with the given code.
func IsCode(err error, code ErrorCode) bool {
	var target *Error
	return errors.As(err, &target) && target.Code == code
}

// CodeOf returns the structured code of err ("" when err is not a collab
// refusal).
func CodeOf(err error) ErrorCode {
	var target *Error
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

func newError(kind error, code ErrorCode, operation, detail string) *Error {
	return &Error{Kind: kind, Code: code, Operation: operation, Detail: detail}
}

func invalidMode(detail string) *Error {
	return newError(ErrInvalidMode, CodeInvalidMode, "register_mode", detail)
}
