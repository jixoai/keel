package toolsurface

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentext"
)

// AgentBackend is the internal sub-agent registry the aggregate tool drives.
// *agentext.SubagentRunner implements it.
type AgentBackend interface {
	Spawn(ctx context.Context, scope agentext.RunScope, args json.RawMessage) (agentext.SubagentResult, error)
	List(tenantID uuid.UUID, parent *uuid.UUID) []agentext.SubagentRecord
	Get(sessionID uuid.UUID) (agentext.SubagentRecord, bool)
}

var agentActions = []string{ActionSpawn, ActionFork, ActionResume, ActionCancel, ActionList, ActionStatus}

const agentToolDescription = "Internal sub-agents (independent session + task card). Actions: spawn {task, agentId?, agent?, " +
	"idempotencyKey?, maxTurns?, maxTokens?, timeoutMs?} (returns the child's structured result); list (this session's " +
	"children); status {sessionId}. fork, resume and cancel depend on task-log and are reserved for a later wave. " +
	"External runtimes (Codex/Claude/DSH) are tools, not internal agents."

func agentSchema() json.RawMessage {
	return schema(`, "task":{"type":"string"}, "agent":{"type":"string"}, "agentId":{"type":"string"}, ` +
		`"idempotencyKey":{"type":"string"}, "sessionId":{"type":"string"}, "maxTurns":{"type":"integer","minimum":0}, ` +
		`"maxTokens":{"type":"integer","minimum":0}, "timeoutMs":{"type":"integer","minimum":0}`)
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
			case ActionList:
				return agentList(ctx, backend)
			case ActionStatus:
				return agentStatus(ctx, backend, raw)
			case ActionFork, ActionResume, ActionCancel:
				return NotImplemented(Agent, action)
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

func agentSpawn(ctx context.Context, backend AgentBackend, raw json.RawMessage) (agentcore.AgentToolResult, error) {
	scope, ok := runScope(ctx)
	if !ok {
		return agentScopeMissing()
	}
	if backend == nil {
		return notWired(Agent, "no sub-agent runner is wired in this deployment")
	}
	result, err := backend.Spawn(ctx, scope, raw)
	if err != nil {
		switch {
		case errors.Is(err, agentext.ErrRunScopeMissing):
			return agentScopeMissing()
		case errors.Is(err, agentext.ErrInvalidSpawn):
			return invalidArguments(Agent, err.Error())
		default:
			return ErrorResult(CodeBackendFailed, Agent, err.Error(), map[string]any{"action": ActionSpawn})
		}
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
	if backend == nil {
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
	if backend == nil {
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
