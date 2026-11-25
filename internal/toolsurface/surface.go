// Package toolsurface defines the harness-owned, constant model tool surface.
//
// The model sees exactly eleven builtin tools (bash, read, write, edit, mcp,
// skills, tasks, agent, approve, notify, creation) plus whatever approved
// plugins declare in provides.tools. Dynamic capabilities enter through an
// aggregate tool's action, never by registering a new tool name: adding an MCP
// server, a skill or a task must not change the tool manifest (and therefore its
// hash). Tool names from the v1 surface (spawn_subagent, creation_*, skill,
// clock/echo, mcp__<server>__<tool>) are deleted, with no aliases or shims.
package toolsurface

import (
	"context"
	"encoding/json"

	"github.com/jixoai/keel/internal/agentcore"
)

// Builtin tool names. This list is the contract: the model-visible builtin set
// is exact, not a minimum.
const (
	Bash     = "bash"
	Read     = "read"
	Write    = "write"
	Edit     = "edit"
	MCP      = "mcp"
	Skills   = "skills"
	Tasks    = "tasks"
	Agent    = "agent"
	Approve  = "approve"
	Notify   = "notify"
	Creation = "creation"
)

// Action names, in the closed sets of the tool-surface spec.
const (
	ActionRun  = "run"
	ActionJobs = "jobs"
	// ActionCheckpoint is the explicit capture trigger of the checkpoint/manual
	// policies: it settles the session's pending workspace changes as one
	// fs.capture, without ending the run or starting a command.
	ActionCheckpoint = "checkpoint"
	ActionList       = "list"
	ActionDesc       = "describe"
	ActionCall       = "call"
	ActionLs         = "ls"
	ActionView       = "view"
	ActionManage     = "manage"
	ActionSources    = "sources"
	ActionCreate     = "create"
	ActionUpdate     = "update"
	ActionAttach     = "attach"
	ActionDetach     = "detach"
	ActionClose      = "close"
	ActionShow       = "show"
	ActionNote       = "note"
	ActionSpawn      = "spawn"
	ActionFork       = "fork"
	ActionResume     = "resume"
	ActionCancel     = "cancel"
	ActionStatus     = "status"
	ActionConfirm    = "confirm"
	ActionPrompt     = "prompt"
	ActionChoice     = "choice"
	ActionPush       = "push"
	ActionAck        = "ack"
	ActionCatalog    = "catalog"
	ActionScaffold   = "scaffold"
	ActionValidate   = "validate"
	ActionInstall    = "install"
	ActionRemove     = "remove"
)

// Typed error codes carried in a failed tool result's structured content.
const (
	CodeUnknownAction    = "unknown_action"
	CodeNotImplemented   = "not_implemented"
	CodeInvalidArguments = "invalid_arguments"
	CodeNotFound         = "not_found"
	CodePluginDisabled   = "plugin_disabled"
	CodeNotWired         = "not_wired"
	CodeRunScopeMissing  = "run_scope_missing"
	CodeBackendFailed    = "backend_error"
	// CodeCapacity is the typed "not now" refusal of a group/tenant slot. It
	// mirrors the HTTP 429 + Retry-After (W2's admission vocabulary); the model
	// can read retryAfterMs and decide whether to wait.
	CodeCapacity = "capacity_exhausted"
	// CodeForkSnapshotMissing reports a fork origin whose file view is not an
	// exact captured snapshot. Forking there would invent a file view.
	CodeForkSnapshotMissing = "fork_snapshot_missing"
	// Sub-agent mode refusals (change keel-collab-subagent). The codes are the
	// mode contract's own vocabulary, returned unchanged by the Host and both
	// entry points.
	CodeSubagentDepthExceeded    = "subagent_depth_exceeded"
	CodeSubagentChildrenExceeded = "subagent_children_exceeded"
	CodeSubagentLinkInvalid      = "subagent_link_invalid"
	CodeSubagentMaxDepthWidened  = "subagent_max_depth_widened"
	CodePermissionDenied         = "permission_denied"
)

// actionInput is the envelope every aggregate tool parses.
type actionInput struct {
	Action string `json:"action"`
}

// actionTool is one aggregate tool with a closed action set. The schema does not
// enumerate action values: an unknown action must reach Execute so the model
// gets a typed unknown_action result with the available list, instead of the
// registry's generic schema error.
type actionTool struct {
	name        string
	description string
	schema      json.RawMessage
	actions     []string
	sequential  bool
	run         func(ctx context.Context, action string, args json.RawMessage) (agentcore.AgentToolResult, error)
}

func (t *actionTool) Name() string            { return t.name }
func (t *actionTool) Description() string     { return t.description }
func (t *actionTool) Schema() json.RawMessage { return t.schema }

func (t *actionTool) ExecutionMode() agentcore.ToolExecutionMode {
	if t.sequential {
		return agentcore.ToolExecutionSequential
	}
	return agentcore.ToolExecutionParallel
}

func (t *actionTool) Execute(ctx context.Context, callID string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	// The call id is the correlation key a progress frame echoes (P4.5): tools
	// that can receive out-of-band frames read it from the context.
	ctx = agentcore.WithToolCallID(ctx, callID)
	var input actionInput
	if err := json.Unmarshal(args, &input); err != nil {
		return ErrorResult(CodeInvalidArguments, t.name, " arguments are not valid JSON: "+err.Error(), nil)
	}
	if input.Action == "" {
		return unknownAction(t.name, input.Action, t.actions)
	}
	for _, action := range t.actions {
		if action == input.Action {
			return t.run(ctx, input.Action, args)
		}
	}
	return unknownAction(t.name, input.Action, t.actions)
}

// schema builds a small top-level schema. Action-specific fields stay
// additionalProperties=true so the action router, not the generic validator,
// owns the closed-set error.
func schema(extra string) json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","description":"The action to run."}` + extra + `},"required":["action"],"additionalProperties":true}`)
}
