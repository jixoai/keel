// Package toolauth is the runtime tool surface's authorization seam
// (change keel-bot-identity-access, A4.2b1).
//
// It answers one question for every model tool call: may THIS session's
// principal run THIS tool action on THIS resource? The answer is:
//
//   - the principal comes from the session's ownership (Bot Account + Bot
//     Harness narrowing), loaded per call, never from the model or a header;
//   - the action key comes from the toolsurface permission registry;
//   - the resource comes from the call's own arguments (agent target, named
//     session, else the ambient session);
//   - the access engine decides key + relation live, so a revoked grant or a
//     narrowed Harness applies to the very next call.
//
// The package is product-layer code: it imports the access engine, the
// accesswire bridge and the tool-surface registry. The kernel runner only sees
// the agent.ToolAccess interface, so the dependency direction stays
// cmd/keel → toolauth → accesswire/access.
package toolauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/toolsurface"
)

// Access is the subset of *accesswire.Bridge this package needs. Keeping it an
// interface lets the policy be unit-tested without PostgreSQL; the bridge is
// the only production implementation.
type Access interface {
	// SessionPrincipal resolves the Bot/account a session runs as.
	SessionPrincipal(ctx context.Context, tenantID, sessionID uuid.UUID) (access.Principal, error)
	// SessionBotID resolves the Bot a child session is owned by.
	SessionBotID(ctx context.Context, tenantID, sessionID uuid.UUID) (string, error)
	// Authorize decides one key + resource and audits a denial.
	Authorize(ctx context.Context, principal access.Principal, key string, resource access.Resource) (access.Decision, error)
	// AuditInvoke records one accepted bot invocation.
	AuditInvoke(ctx context.Context, tenantID uuid.UUID, caller access.Principal, botID string, sessionID uuid.UUID, result string) error
	// Denied records a denial the policy decided on itself (an unresolvable
	// target, a missing target identity).
	Denied(ctx context.Context, principal access.Principal, decision access.Decision, resource access.Resource)
}

// Authorizer implements agent.ToolAccess over the access bridge.
type Authorizer struct {
	Access Access
	Logger *slog.Logger
}

// New builds the authorizer. A nil access seam is refused at construction: the
// deployment must wire it, and a half-armed authorizer must not look usable.
func New(access Access, log *slog.Logger) (*Authorizer, error) {
	if access == nil {
		return nil, errors.New("toolauth: access seam is required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Authorizer{Access: access, Logger: log}, nil
}

var _ agent.ToolAccess = (*Authorizer)(nil)

// requirement is one decision the call needs: a key against one resource.
type requirement struct {
	Key      string
	Resource access.Resource
}

// AuthorizeToolCall authorizes one model tool call. It fails closed on every
// uncertainty: no session scope, no principal, an unreadable grant/relation
// source, or an invocation whose audit row cannot be written all deny the
// call.
func (a *Authorizer) AuthorizeToolCall(ctx context.Context, scope kernel.Scope, call agentcore.AgentToolCall) error {
	if a == nil || a.Access == nil {
		return &agent.ToolAccessError{
			Code:   agent.ToolAccessUnavailable,
			Reason: "tool_access_not_wired",
			Detail: "the tool authorization seam is not configured",
		}
	}
	if scope.TenantID == uuid.Nil || scope.SessionID == uuid.Nil {
		return &agent.ToolAccessError{
			Code:   agent.ToolAccessNoPrincipal,
			Reason: "session_scope_missing",
			Detail: "the tool call has no tenant/session scope",
		}
	}

	principal, err := a.Access.SessionPrincipal(ctx, scope.TenantID, scope.SessionID)
	if err != nil {
		if errors.Is(err, accesswire.ErrNoPrincipal) {
			return &agent.ToolAccessError{
				Code:   agent.ToolAccessNoPrincipal,
				Reason: "no_session_principal",
				Detail: "the session has no usable Bot/account identity",
			}
		}
		return &agent.ToolAccessError{
			Code:   agent.ToolAccessUnavailable,
			Reason: "principal_lookup_failed",
			Detail: err.Error(),
		}
	}

	tool := strings.TrimSpace(call.Name)
	action := toolsurface.ActionOf(tool, call.Arguments)
	key, declared := toolsurface.RequiredKey(tool, action)
	if !declared {
		// A tool not in the builtin/media/decision map is published by an
		// approved plugin. plugin.invoke is the approval boundary that installed
		// it, so an unknown name can never fall through ungated.
		key = access.KeyPluginInvoke
	}

	requirements, invocation, refusal := a.requirements(ctx, scope, principal, tool, action, key, call.Arguments)
	if refusal != nil {
		return refusal
	}
	if len(requirements) == 0 {
		requirements = []requirement{{
			Key:      key,
			Resource: access.Resource{TenantID: scope.TenantID, Type: "session", ID: scope.SessionID.String()},
		}}
	}

	for _, need := range requirements {
		decision, err := a.Access.Authorize(ctx, principal, need.Key, need.Resource)
		if err != nil {
			return &agent.ToolAccessError{
				Code:   agent.ToolAccessUnavailable,
				Key:    need.Key,
				Reason: "authorization_failed",
				Detail: err.Error(),
			}
		}
		if decision.Denied() {
			return &agent.ToolAccessError{
				Code:   agent.ToolAccessPermissionDenied,
				Key:    need.Key,
				Reason: decision.Reason.String(),
				Detail: decision.Detail,
			}
		}
	}

	// An accepted bot invocation is audited with the caller and the callee
	// before the child exists. A failed audit row denies the call: the spec
	// treats bot.invoke as high-risk and every invocation as traceable.
	if invocation != "" {
		if err := a.Access.AuditInvoke(ctx, scope.TenantID, principal, invocation, scope.SessionID, "authorized"); err != nil {
			return &agent.ToolAccessError{
				Code:   agent.ToolAccessUnavailable,
				Key:    key,
				Reason: "invoke_audit_failed",
				Detail: err.Error(),
			}
		}
	}
	return nil
}

// requirements resolves the resource(s) a call is decided against.
//
// The ambient session is the default resource for every session-scoped tool.
// The agent tool is different: its resource is the TARGET Bot, resolved from
// agentId/sessionId (or the run's own Bot when the call delegates to itself).
// An unresolvable target denies with the same missing_relation shape as a real
// unauthorized target, so the model cannot use the error to probe existence.
func (a *Authorizer) requirements(ctx context.Context, scope kernel.Scope, principal access.Principal, tool, action, key string, args json.RawMessage) ([]requirement, string, *agent.ToolAccessError) {
	ambient := access.Resource{TenantID: scope.TenantID, Type: "session", ID: scope.SessionID.String()}

	if tool == toolsurface.Agent {
		return a.agentRequirements(ctx, scope, principal, action, key, args)
	}

	// A named session argument is a resource id. tasks.list addresses a session
	// directly, so that session is the decision resource (task.read is
	// session-scoped, so the engine checks the relation). notify.list is
	// tenant-level keyed; a named other session additionally needs session.read,
	// otherwise the key alone could list another account's notifications.
	resource := ambient
	named, hasNamed := sessionArgument(args, "sessionId")
	if hasNamed && named != scope.SessionID {
		switch {
		case tool == toolsurface.Tasks && action == toolsurface.ActionList:
			resource = access.Resource{TenantID: scope.TenantID, Type: "session", ID: named.String()}
		case tool == toolsurface.Notify && action == toolsurface.ActionList:
			return []requirement{
				{Key: key, Resource: ambient},
				{Key: access.KeySessionRead, Resource: access.Resource{TenantID: scope.TenantID, Type: "session", ID: named.String()}},
			}, "", nil
		}
	}
	return []requirement{{Key: key, Resource: resource}}, "", nil
}

// agentRequirements resolves the bot.invoke target(s) of the `agent` tool.
func (a *Authorizer) agentRequirements(ctx context.Context, scope kernel.Scope, principal access.Principal, action, key string, args json.RawMessage) ([]requirement, string, *agent.ToolAccessError) {
	explicit, hasExplicit := stringArgument(args, "agentId")

	switch action {
	case toolsurface.ActionSpawn, toolsurface.ActionFork:
		target := explicit
		if !hasExplicit || target == "" {
			target = principal.BotID
		}
		if target == "" {
			return nil, "", a.deny(ctx, principal, access.Resource{TenantID: scope.TenantID, Type: "bot"}, key,
				access.ReasonMissingRelation,
				"the session has no bot identity to delegate from and the call named no target bot")
		}
		return []requirement{{Key: key, Resource: botResource(scope.TenantID, target)}}, target, nil

	case toolsurface.ActionResume:
		// Resume may change the child's binding and continues an existing child
		// session. Both the child's owning Bot and an explicitly named target
		// are bot.invoke targets; the call needs authority on each.
		targets := make([]string, 0, 2)
		if explicit != "" {
			targets = append(targets, explicit)
		}
		if named, ok := sessionArgument(args, "sessionId"); ok {
			childBot, err := a.Access.SessionBotID(ctx, scope.TenantID, named)
			if err != nil {
				// Missing, foreign and unowned children deliberately produce the
				// same refusal; the tool backend's own not_found cannot be reached
				// without this authority.
				return nil, "", a.deny(ctx, principal, access.Resource{TenantID: scope.TenantID, Type: "bot"}, key,
					access.ReasonMissingRelation,
					"the resumed child session has no resolvable owning bot")
			}
			targets = append(targets, childBot)
		}
		if len(targets) == 0 {
			targets = append(targets, principal.BotID)
		}
		if targets[0] == "" {
			return nil, "", a.deny(ctx, principal, access.Resource{TenantID: scope.TenantID, Type: "bot"}, key,
				access.ReasonMissingRelation,
				"the resumed call named no target bot and the session has no bot identity")
		}
		return botRequirements(scope.TenantID, key, targets), targets[0], nil

	case toolsurface.ActionCancel, toolsurface.ActionStatus:
		target := ""
		if named, ok := sessionArgument(args, "sessionId"); ok {
			childBot, err := a.Access.SessionBotID(ctx, scope.TenantID, named)
			if err != nil {
				return nil, "", a.deny(ctx, principal, access.Resource{TenantID: scope.TenantID, Type: "bot"}, key,
					access.ReasonMissingRelation,
					"the child session has no resolvable owning bot")
			}
			target = childBot
		}
		if target == "" {
			target = principal.BotID
		}
		if target == "" {
			return nil, "", a.deny(ctx, principal, access.Resource{TenantID: scope.TenantID, Type: "bot"}, key,
				access.ReasonMissingRelation,
				"the call named no target bot and the session has no bot identity")
		}
		return []requirement{{Key: key, Resource: botResource(scope.TenantID, target)}}, "", nil

	case toolsurface.ActionList:
		if principal.BotID == "" {
			return nil, "", a.deny(ctx, principal, access.Resource{TenantID: scope.TenantID, Type: "bot"}, key,
				access.ReasonMissingRelation,
				"the session has no bot identity to list children for")
		}
		return []requirement{{Key: key, Resource: botResource(scope.TenantID, principal.BotID)}}, "", nil

	default:
		// An unknown action reaches the tool's own unknown_action result; the
		// call still needs authority on its target (or its own Bot) so the
		// unknown action cannot be used as an unauthenticated probe.
		target := explicit
		if target == "" {
			target = principal.BotID
		}
		if target == "" {
			return nil, "", a.deny(ctx, principal, access.Resource{TenantID: scope.TenantID, Type: "bot"}, key,
				access.ReasonMissingRelation,
				"the session has no bot identity and the call named no target bot")
		}
		return []requirement{{Key: key, Resource: botResource(scope.TenantID, target)}}, target, nil
	}
}

// botRequirements maps target bot handles to decisions, dropping duplicates.
func botRequirements(tenantID uuid.UUID, key string, targets []string) []requirement {
	seen := make(map[string]bool, len(targets))
	out := make([]requirement, 0, len(targets))
	for _, target := range targets {
		if target == "" || seen[target] {
			continue
		}
		seen[target] = true
		out = append(out, requirement{Key: key, Resource: botResource(tenantID, target)})
	}
	return out
}

func botResource(tenantID uuid.UUID, botID string) access.Resource {
	return access.Resource{TenantID: tenantID, Type: "bot", ID: botID}
}

// deny records the policy-decided denial (the engine never saw it) and returns
// the structured refusal. The resource may be incomplete; the audit row still
// names its type, and the model-visible error never includes it.
func (a *Authorizer) deny(ctx context.Context, principal access.Principal, resource access.Resource, key string, reason access.Reason, detail string) *agent.ToolAccessError {
	decision := access.Decision{Allow: false, Key: key, Reason: reason, Detail: detail}
	if a.Access != nil {
		a.Access.Denied(ctx, principal, decision, resource)
	}
	return &agent.ToolAccessError{
		Code:   agent.ToolAccessPermissionDenied,
		Key:    key,
		Reason: reason.String(),
		Detail: detail,
	}
}

// sessionArgument parses a uuid-valued argument. Absent, empty and malformed
// values are "not named"; the tool's own argument validation owns the shape
// error.
func sessionArgument(args json.RawMessage, name string) (uuid.UUID, bool) {
	raw, ok := stringArgument(args, name)
	if !ok {
		return uuid.Nil, false
	}
	parsed, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == uuid.Nil {
		return uuid.Nil, false
	}
	return parsed, true
}

// stringArgument reads one string field from a JSON object.
func stringArgument(args json.RawMessage, name string) (string, bool) {
	if len(strings.TrimSpace(string(args))) == 0 {
		return "", false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		return "", false
	}
	raw, ok := fields[name]
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return strings.TrimSpace(value), true
}

// String renders the authorizer for logs without exposing configuration.
func (a *Authorizer) String() string {
	if a == nil || a.Access == nil {
		return "toolauth.Authorizer(unconfigured)"
	}
	return fmt.Sprintf("toolauth.Authorizer(%T)", a.Access)
}
