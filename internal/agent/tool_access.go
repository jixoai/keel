// Runtime tool-surface authorization (change keel-bot-identity-access,
// A4.2b1).
//
// The runner authorizes EVERY model tool call before execution through this
// seam. The product layer implements it over the session's ownership (Bot
// Account ∩ Harness) and the access engine; the kernel only knows the shape.
//
// Nil is fail-closed by construction: a Runner without a ToolAccess seam
// refuses every tool call with a structured error. Tests and internal runtimes
// that drive tools without the identity surface must inject AllowAllToolAccess
// explicitly, so "forgot to wire it" can never silently open the surface in a
// deployment.
package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
)

// ToolAccess authorizes one tool call for the session that is running it.
// scope is the attempt's tenant/session; call is the validated model tool call.
// A nil return allows execution; any error blocks the call and is returned to
// the model as a structured tool error.
type ToolAccess interface {
	AuthorizeToolCall(ctx context.Context, scope kernel.Scope, call agentcore.AgentToolCall) error
}

// Structured tool-access refusal codes. They are the details.code the model
// and the UI see; they never carry a resource id, so a refusal cannot answer
// "does this resource exist?".
const (
	// ToolAccessPermissionDenied means the session's principal does not satisfy
	// the tool's required key/relation.
	ToolAccessPermissionDenied = "permission_denied"
	// ToolAccessNoPrincipal means the session has no resolvable account/Bot
	// identity (an unowned session, a disabled account, an archived Bot).
	ToolAccessNoPrincipal = "no_principal"
	// ToolAccessUnavailable means the authorization seam failed (not wired, a
	// grant/relation read failed, or the invocation audit could not be written).
	ToolAccessUnavailable = "authorization_unavailable"
)

// ToolAccessError is the stable refusal a ToolAccess implementation returns.
// Key and Reason describe the missing authority; Detail is for logs/audit and
// is deliberately never rendered into the model-visible message (it may name
// internal ids).
type ToolAccessError struct {
	Code   string
	Key    string
	Reason string
	Detail string
}

// Error renders the refusal for logs. The model-visible text is built by the
// runner (see toolAccessDecision) and is intentionally narrower.
func (e *ToolAccessError) Error() string {
	if e == nil {
		return "tool authorization denied"
	}
	message := e.Code
	if message == "" {
		message = ToolAccessPermissionDenied
	}
	if e.Key != "" {
		message += ": requires " + e.Key
	}
	if e.Reason != "" {
		message += " (" + e.Reason + ")"
	}
	if e.Detail != "" {
		message += ": " + e.Detail
	}
	return message
}

// AllowAllToolAccess is the explicit test/internal-runtime fake. Production
// MUST wire the access-backed implementation; a deployment that uses this value
// deliberately turns the tool surface into an unguarded surface.
type AllowAllToolAccess struct{}

// AuthorizeToolCall allows every call.
func (AllowAllToolAccess) AuthorizeToolCall(context.Context, kernel.Scope, agentcore.AgentToolCall) error {
	return nil
}

var _ ToolAccess = AllowAllToolAccess{}

// toolAccessGuard returns the beforeToolCall hook for one attempt. The hook is
// the executor's documented permission seam: it runs after schema validation
// and before execution, for model-issued calls and crash-recovery replays
// alike.
func (r *Runner) toolAccessGuard(scope kernel.Scope) agentcore.BeforeToolCallFunc {
	return func(ctx context.Context, call agentcore.AgentToolCall) *agentcore.BeforeToolCallDecision {
		var err error
		if r == nil || r.ToolAccess == nil {
			err = &ToolAccessError{
				Code:   ToolAccessUnavailable,
				Reason: "tool_access_seam_not_wired",
				Detail: "the runner has no ToolAccess seam",
			}
		} else {
			err = r.ToolAccess.AuthorizeToolCall(ctx, scope, call)
		}
		if err == nil {
			return nil
		}
		return toolAccessDecision(call, err)
	}
}

// toolAccessDecision converts a refusal into the blocked-call decision the
// executor finalizes as an error tool result. The message names what authority
// is missing and nothing about the resource, so the model can ask for a grant
// without learning whether a guessed resource exists.
func toolAccessDecision(call agentcore.AgentToolCall, err error) *agentcore.BeforeToolCallDecision {
	refusal := &ToolAccessError{Code: ToolAccessUnavailable, Reason: "authorization_failed"}
	var typed *ToolAccessError
	if errors.As(err, &typed) && typed != nil {
		refusal = typed
	}
	code := refusal.Code
	if code == "" {
		code = ToolAccessPermissionDenied
	}

	var message string
	switch code {
	case ToolAccessPermissionDenied:
		if refusal.Key == "" {
			message = fmt.Sprintf("Tool %q is not authorized: a required permission is missing.", call.Name)
		} else {
			message = fmt.Sprintf("Tool %q is not authorized: it requires the %q permission.", call.Name, refusal.Key)
		}
		if refusal.Reason != "" {
			message += " (reason: " + refusal.Reason + ")"
		}
		message += " Ask the account owner to grant it."
	case ToolAccessNoPrincipal:
		message = fmt.Sprintf("Tool %q is not authorized: this session has no resolvable account or bot identity.", call.Name)
	default:
		message = fmt.Sprintf("Tool %q is not authorized: the tool authorization service is unavailable.", call.Name)
	}

	details := map[string]any{"code": code, "tool": call.Name}
	if refusal.Key != "" {
		details["requiredKey"] = refusal.Key
	}
	if refusal.Reason != "" {
		details["reason"] = refusal.Reason
	}
	var payload any = details
	content := agentcore.ContentList{agentcore.NewTextContent(message)}
	return &agentcore.BeforeToolCallDecision{Block: true, Content: &content, Details: &payload}
}
