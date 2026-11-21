package toolsurface

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/placement"
)

// AgentBackend is the internal sub-agent registry the aggregate tool drives.
// *agentext.AgentBackend implements it.
type AgentBackend interface {
	Spawn(ctx context.Context, scope agentext.RunScope, args json.RawMessage) (agentext.SubagentResult, error)
	Fork(ctx context.Context, scope agentext.RunScope, args json.RawMessage) (agentext.SubagentResult, error)
	Resume(ctx context.Context, scope agentext.RunScope, args json.RawMessage) (agentext.ResumeResult, error)
	Cancel(ctx context.Context, scope agentext.RunScope, args json.RawMessage) (agentext.CancelResult, error)
	List(tenantID uuid.UUID, parent *uuid.UUID) []agentext.SubagentRecord
	Get(sessionID uuid.UUID) (agentext.SubagentRecord, bool)
}

var agentActions = []string{ActionSpawn, ActionFork, ActionResume, ActionCancel, ActionList, ActionStatus}

const agentToolDescription = "Internal sub-agents (independent session + task card). Actions: " +
	"spawn {task, botId?, agent?, idempotencyKey?, maxTurns?, maxTokens?, timeoutMs?} (returns the child's structured result); " +
	"fork {eventSeq, task?, title?, taskId?, botId?, idempotencyKey?, budget fields} (a new session inherits the parent ledger " +
	"and file view at eventSeq; the new card's parentTaskId points at the origin card); " +
	"resume {sessionId, requestId?, reason?, selection?, botId?} (continue the same child after an interruption; " +
	"an optional selection changes its binding for the resumed attempt; never reopens a completed task); " +
	"cancel {sessionId, reason?}; list (this session's children); status {sessionId}. " +
	"External runtimes (Codex/Claude/DSH) are tools, not internal agents."

func agentSchema() json.RawMessage {
	return schema(`, "task":{"type":"string"}, "agent":{"type":"string"}, "botId":{"type":"string"}, ` +
		`"idempotencyKey":{"type":"string"}, "sessionId":{"type":"string"}, "requestId":{"type":"string"}, ` +
		`"reason":{"type":"string"}, "title":{"type":"string"}, "taskId":{"type":"string"}, ` +
		`"eventSeq":{"type":"integer","minimum":0}, "selection":{"type":"object"}, ` +
		`"maxTurns":{"type":"integer","minimum":0}, "maxTokens":{"type":"integer","minimum":0}, ` +
		`"timeoutMs":{"type":"integer","minimum":0}`)
}

func newAgentTool(backend AgentBackend) *actionTool {
	return &actionTool{
		name:        Agent,
		description: agentToolDescription,
		schema:      agentSchema(),
		actions:     agentActions,
		run: func(ctx context.Context, action string, raw json.RawMessage) (agentcore.AgentToolResult, error) {
			switch action {
			case ActionSpawn:
				return agentSpawn(ctx, backend, raw)
			case ActionFork:
				return agentFork(ctx, backend, raw)
			case ActionResume:
				return agentResume(ctx, backend, raw)
			case ActionCancel:
				return agentCancel(ctx, backend, raw)
			case ActionList:
				return agentList(ctx, backend)
			case ActionStatus:
				return agentStatus(ctx, backend, raw)
			default:
				return unknownAction(Agent, action, agentActions)
			}
		},
	}
}

func runScope(ctx context.Context) (agentext.RunScope, bool) {
	return agentext.RunScopeFromContext(ctx)
}

func agentScopeMissing() (agentcore.AgentToolResult, error) {
	return ErrorResult(CodeRunScopeMissing, Agent,
		"agent actions need the ambient run scope (WithRunScope); a model tool call inside a run always has it", nil)
}

// wiredBackend is the optional probe a backend uses to report that its lazy
// service getter has nothing to resolve. A backend that does not implement it
// is assumed wired (a nil backend is handled separately).
type wiredBackend interface {
	Wired() bool
}

// agentBackendWired reports whether the backend can actually serve actions.
// The distinction matters: an injected but unresolved backend is a deployment
// wiring gap (not_wired), not a backend failure (backend_error) and not an
// empty result set.
func agentBackendWired(backend AgentBackend) bool {
	if backend == nil {
		return false
	}
	if probe, ok := backend.(wiredBackend); ok {
		return probe.Wired()
	}
	return true
}

// agentFailure maps the runner's typed errors onto the aggregate tool's error
// vocabulary. Capacity is deliberately its own code: the model can read
// retryAfterMs and wait, exactly like the HTTP edge's 429 + Retry-After.
func agentFailure(action string, err error) (agentcore.AgentToolResult, error) {
	var capacity *placement.CapacityError
	switch {
	case errors.Is(err, agentext.ErrRunScopeMissing):
		return agentScopeMissing()
	case errors.Is(err, agentext.ErrInvalidSpawn), errors.Is(err, agentext.ErrForkOriginInvalid):
		return invalidArguments(Agent, err.Error())
	case errors.Is(err, agentext.ErrForkSnapshotMissing):
		return ErrorResult(CodeForkSnapshotMissing, Agent, err.Error(), map[string]any{"action": action})
	case errors.Is(err, agentext.ErrSubagentNotFound):
		return notFound(Agent, err.Error())
	case errors.Is(err, agentext.ErrForkUnavailable),
		errors.Is(err, agentext.ErrResumeUnavailable),
		errors.Is(err, agentext.ErrNotWired):
		return notWired(Agent, err.Error())
	case errors.As(err, &capacity):
		return ErrorResult(CodeCapacity, Agent, capacity.Error(), map[string]any{
			"action":       action,
			"retryAfterMs": capacity.RetryAfter.Milliseconds(),
			"resource":     capacity.Resource,
			"limit":        capacity.Limit,
			"active":       capacity.Active,
		})
	case errors.Is(err, kernel.ErrQuota):
		return ErrorResult(CodeCapacity, Agent, err.Error(), map[string]any{"action": action})
	default:
		return ErrorResult(CodeBackendFailed, Agent, err.Error(), map[string]any{"action": action})
	}
}

func agentSpawn(ctx context.Context, backend AgentBackend, raw json.RawMessage) (agentcore.AgentToolResult, error) {
	scope, ok := runScope(ctx)
	if !ok {
		return agentScopeMissing()
	}
	if !agentBackendWired(backend) {
		return notWired(Agent, "no sub-agent runner is wired in this deployment")
	}
	result, err := backend.Spawn(ctx, scope, raw)
	if err != nil {
		return agentFailure(ActionSpawn, err)
	}
	encoded, _ := json.Marshal(result)
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(string(encoded))},
		StructuredContent: result,
	}, nil
}

func agentFork(ctx context.Context, backend AgentBackend, raw json.RawMessage) (agentcore.AgentToolResult, error) {
	scope, ok := runScope(ctx)
	if !ok {
		return agentScopeMissing()
	}
	if !agentBackendWired(backend) {
		return notWired(Agent, "no sub-agent runner is wired in this deployment")
	}
	result, err := backend.Fork(ctx, scope, raw)
	if err != nil {
		return agentFailure(ActionFork, err)
	}
	encoded, _ := json.Marshal(result)
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(string(encoded))},
		StructuredContent: result,
	}, nil
}

func agentResume(ctx context.Context, backend AgentBackend, raw json.RawMessage) (agentcore.AgentToolResult, error) {
	scope, ok := runScope(ctx)
	if !ok {
		return agentScopeMissing()
	}
	if !agentBackendWired(backend) {
		return notWired(Agent, "no sub-agent runner is wired in this deployment")
	}
	result, err := backend.Resume(ctx, scope, raw)
	if err != nil {
		return agentFailure(ActionResume, err)
	}
	encoded, _ := json.Marshal(result)
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(string(encoded))},
		StructuredContent: result,
	}, nil
}

func agentCancel(ctx context.Context, backend AgentBackend, raw json.RawMessage) (agentcore.AgentToolResult, error) {
	scope, ok := runScope(ctx)
	if !ok {
		return agentScopeMissing()
	}
	if !agentBackendWired(backend) {
		return notWired(Agent, "no sub-agent runner is wired in this deployment")
	}
	result, err := backend.Cancel(ctx, scope, raw)
	if err != nil {
		return agentFailure(ActionCancel, err)
	}
	encoded, _ := json.Marshal(result)
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(string(encoded))},
		StructuredContent: result,
	}, nil
}

func agentList(ctx context.Context, backend AgentBackend) (agentcore.AgentToolResult, error) {
	scope, ok := runScope(ctx)
	if !ok {
		return agentScopeMissing()
	}
	if !agentBackendWired(backend) {
		return notWired(Agent, "no sub-agent runner is wired in this deployment")
	}
	parent := scope.SessionID
	records := backend.List(scope.TenantID, &parent)
	document := map[string]any{"count": len(records), "agents": records}
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(marshalDocument(document))},
		StructuredContent: document,
	}, nil
}

func agentStatus(ctx context.Context, backend AgentBackend, raw json.RawMessage) (agentcore.AgentToolResult, error) {
	scope, ok := runScope(ctx)
	if !ok {
		return agentScopeMissing()
	}
	if !agentBackendWired(backend) {
		return notWired(Agent, "no sub-agent runner is wired in this deployment")
	}
	var input struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return invalidArguments(Agent, err.Error())
	}
	sessionID, err := uuid.Parse(input.SessionID)
	if err != nil {
		return invalidArguments(Agent, "status requires a child sessionId")
	}
	record, ok := backend.Get(sessionID)
	if !ok || record.TenantID != scope.TenantID.String() || record.ParentSessionID != scope.SessionID.String() {
		return notFound(Agent, fmt.Sprintf("no child session %s under this run", input.SessionID))
	}
	document := map[string]any{"agent": record}
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(marshalDocument(document))},
		StructuredContent: document,
	}, nil
}
