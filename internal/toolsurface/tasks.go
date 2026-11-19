package toolsurface

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/tasklog"
)

// TasksBackend is the task-log backend behind the harness-owned `tasks` tool.
// The plugin (keel.tasks) supplies Invoke, so disabling it yields
// plugin_disabled while the tool name stays on the surface. Attribute is the
// harness's own attribution seam: it resolves the card a tool call belongs to
// (auto-creating the session's initial card when absent), and it keeps working
// when the plugin is disabled because task attribution is a harness contract,
// not a plugin feature.
type TasksBackend interface {
	Invoke(ctx context.Context, action string, args json.RawMessage) (agentcore.AgentToolResult, error)
	Attribute(ctx context.Context, scope agentext.RunScope, taskID string) (string, error)
}

// Typed result codes specific to task attribution. They extend the shared
// vocabulary in surface.go without changing it.
const (
	// CodeTaskRequired: the call cannot be attributed to any card.
	CodeTaskRequired = "task_id_required"
	// CodeTaskNotFound: the call names a card that does not exist in this
	// session/tenant.
	CodeTaskNotFound = "task_not_found"
)

var tasksActions = []string{ActionCreate, ActionUpdate, ActionAttach, ActionDetach, ActionClose, ActionList, ActionShow, ActionNote}

const tasksToolDescription = "The task graph (memory) aggregate tool. A task card is a durable, event-sourced goal; every tool call is attributed to one. " +
	"Actions: create {title, status?, parentTaskId?}; update {taskId, title?, status?, parentTaskId?}; " +
	"attach {taskId, path, hash}; detach {taskId, path, hash?}; close {taskId, reason?}; " +
	"list {sessionId?, parentTaskId?, status?}; show {taskId} (card, events, children, attachments); note {taskId, text}. " +
	"The keel.tasks plugin supplies the backend."

func tasksSchema() json.RawMessage {
	return schema(`, "taskId":{"type":"string"}, "title":{"type":"string"}, "status":{"type":"string"}, ` +
		`"parentTaskId":{"type":"string"}, "note":{"type":"string"}, "text":{"type":"string"}, ` +
		`"path":{"type":"string"}, "hash":{"type":"string"}, "sessionId":{"type":"string"}, "reason":{"type":"string"}`)
}

func newTasksTool(backend TasksBackend) *actionTool {
	return &actionTool{
		name:        Tasks,
		description: tasksToolDescription,
		schema:      tasksSchema(),
		actions:     tasksActions,
		sequential:  true,
		run: func(ctx context.Context, action string, raw json.RawMessage) (agentcore.AgentToolResult, error) {
			if backend == nil {
				return notWired(Tasks, "no task-log backend is wired in this deployment")
			}
			result, err := backend.Invoke(ctx, action, raw)
			if err != nil {
				switch {
				case errors.Is(err, agentext.ErrPluginState), errors.Is(err, agentext.ErrPluginNotFound):
					return ErrorResult(CodePluginDisabled, Tasks,
						"keel.tasks is not enabled; enable the plugin to use task actions", nil)
				case errors.Is(err, tasklog.ErrTaskNotFound):
					return ErrorResult(CodeTaskNotFound, Tasks, err.Error(), map[string]any{"action": action})
				case errors.Is(err, tasklog.ErrInvalidTaskID), errors.Is(err, tasklog.ErrInvalidStatus),
					errors.Is(err, tasklog.ErrInvalidEvent), errors.Is(err, tasklog.ErrTaskCycle):
					return invalidArguments(Tasks, err.Error())
				default:
					return ErrorResult(CodeBackendFailed, Tasks, err.Error(), map[string]any{"action": action})
				}
			}
			return result, nil
		},
	}
}

// gatedTool stamps and validates the task attribution of one builtin tool call.
// It is transparent to the manifest: Name, Description, Schema and execution
// mode are the wrapped tool's, so the task log never changes the tool hash.
type gatedTool struct {
	inner   agentcore.AgentTool
	backend TasksBackend
	// target is true for the tasks tool itself: its `taskId` names the card the
	// action operates on, not the caller's card, so it is not validated as an
	// attribution reference (the plugin validates it). The ambient card is
	// still resolved, so every tasks call is attributable.
	target bool
}

func (g *gatedTool) Name() string            { return g.inner.Name() }
func (g *gatedTool) Description() string     { return g.inner.Description() }
func (g *gatedTool) Schema() json.RawMessage { return g.inner.Schema() }
func (g *gatedTool) ExecutionMode() agentcore.ToolExecutionMode {
	return g.inner.ExecutionMode()
}

// Execute enforces the mandatory taskId contract (spec: 每次工具调用必须带
// taskId):
//
//   - inside a run, the call is resolved to a card of the run's session; a
//     model-supplied taskId must name a card of that session, otherwise the
//     call is refused;
//   - an absent taskId is auto-attributed to the session's initial card, which
//     is created on demand (会话起步即有卡);
//   - when no card can be resolved the call is refused with a typed
//     task_id_required result before the wrapped tool runs, and the refusal is
//     committed as the tool's (error) result, so the event stream records the
//     rejected call;
//   - calls outside any run (the plugin UI bridge executes tools over the same
//     registry) are passed through untouched: there is no run to attribute and
//     inventing a session would be worse than an unstamped call.
func (g *gatedTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	scope, ok := agentext.RunScopeFromContext(ctx)
	if !ok {
		return g.inner.Execute(ctx, id, args, onUpdate)
	}
	backend := g.backend
	if backend == nil {
		return g.inner.Execute(ctx, id, args, onUpdate)
	}
	fields, parseErr := argumentObject(args)
	taskID := ""
	if parseErr == nil {
		taskID = stringField(fields, "taskId")
	}
	if g.target {
		// The tasks tool's own actions address a card; only the ambient card is
		// required so the call itself is attributable.
		taskID = ""
	}
	resolved, err := backend.Attribute(ctx, scope, taskID)
	if err != nil {
		return taskAttributeError(g.Name(), err)
	}
	if !g.target && taskID == "" && parseErr == nil {
		fields["taskId"] = json.RawMessage(fmt.Sprintf("%q", resolved))
		if stamped, marshalErr := json.Marshal(fields); marshalErr == nil {
			args = stamped
		}
	}
	// The tool-call id is the idempotency suffix of any task event this call
	// appends, so a retried call does not duplicate its history.
	ctx = tasklog.WithCallID(ctx, id)
	return g.inner.Execute(ctx, id, args, onUpdate)
}

// wrapWithTaskGate returns tools that enforce the taskId contract. A nil backend
// leaves the surface exactly as it is (tests and deployments without the task
// log).
func wrapWithTaskGate(tools []agentcore.AgentTool, backend TasksBackend) []agentcore.AgentTool {
	if backend == nil {
		return tools
	}
	out := make([]agentcore.AgentTool, 0, len(tools))
	for _, tool := range tools {
		out = append(out, &gatedTool{inner: tool, backend: backend, target: tool.Name() == Tasks})
	}
	return out
}

func taskAttributeError(tool string, err error) (agentcore.AgentToolResult, error) {
	switch {
	case errors.Is(err, tasklog.ErrTaskNotFound):
		return ErrorResult(CodeTaskNotFound, tool,
			"the taskId does not name a card of this session: "+err.Error(), nil)
	case errors.Is(err, tasklog.ErrInvalidTaskID), errors.Is(err, tasklog.ErrTaskRequired):
		return ErrorResult(CodeTaskRequired, tool,
			"this tool call cannot be attributed to a task card: "+err.Error(), nil)
	default:
		return ErrorResult(CodeBackendFailed, tool, err.Error(), nil)
	}
}

// argumentObject decodes the argument object for stamping. A non-object or
// malformed payload is passed through untouched: the wrapped tool owns its own
// invalid-arguments error.
func argumentObject(args json.RawMessage) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		return nil, err
	}
	return fields, nil
}

func stringField(fields map[string]json.RawMessage, name string) string {
	raw, ok := fields[name]
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}
