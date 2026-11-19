package agentext

// keel.tasks: the plugin behind the harness-owned `tasks` aggregate tool.
//
// The plugin boundary rule (plugin-architecture / plugin-boundaries) is the
// reason this exists at all: the model-facing name `tasks` belongs to the
// harness and is always registered, while the implementation is a normal
// builtin plugin an operator can disable. A disabled plugin makes the aggregate
// tool answer plugin_disabled; the task *attribution* gate stays in the harness
// because "every tool call belongs to a card" is a harness contract, not a
// plugin feature.
//
// The plugin also owns the sub-agent link: when a sub-agent starts, the parent
// session and the child session are joined by parentTaskId so the task tree is
// cross-session (spec: 任务树是投影，跨会话可拼接).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/tasklog"
)

const (
	// TasksPluginID is the builtin plugin id (manifest:
	// internal/pluginpkg/builtin/keel.tasks.yaml).
	TasksPluginID = "keel.tasks"
	// TasksPluginVersion is the bundled version.
	TasksPluginVersion = "1.0.0"
)

// Internal tool names. They are namespaced and never published to the model:
// the harness-owned `tasks` tool owns the model-facing name and dispatches here
// through Manager.InvokeToolResult.
const (
	TasksToolCreate = "keel.tasks.create"
	TasksToolUpdate = "keel.tasks.update"
	TasksToolAttach = "keel.tasks.attach"
	TasksToolDetach = "keel.tasks.detach"
	TasksToolClose  = "keel.tasks.close"
	TasksToolList   = "keel.tasks.list"
	TasksToolShow   = "keel.tasks.show"
	TasksToolNote   = "keel.tasks.note"
)

// tasksBackendActions maps the aggregate tool's closed action set onto the
// plugin's internal handlers.
var tasksBackendActions = map[string]string{
	"create": TasksToolCreate,
	"update": TasksToolUpdate,
	"attach": TasksToolAttach,
	"detach": TasksToolDetach,
	"close":  TasksToolClose,
	"list":   TasksToolList,
	"show":   TasksToolShow,
	"note":   TasksToolNote,
}

// TasksOptions wires the keel.tasks plugin. Service is the task log the plugin
// drives; nil leaves every action answering a typed backend error (useful in
// tests that only inspect the declared surface).
type TasksOptions struct {
	Service tasklog.API
}

type tasksPlugin struct {
	service tasklog.API
}

// NewTasksPlugin builds the keel.tasks plugin.
func NewTasksPlugin(opts TasksOptions) *Plugin {
	plugin := &tasksPlugin{service: opts.Service}
	return &Plugin{
		ID:      TasksPluginID,
		Version: TasksPluginVersion,
		Tools:   plugin.tools(),
		Hooks:   plugin.hooks(),
	}
}

// TasksBackend adapts keel.tasks to the harness-owned `tasks` tool. Invoke goes
// through the plugin manager so a disabled plugin answers plugin_disabled;
// Attribute calls the task log directly because attribution must survive a
// disabled plugin.
type TasksBackend struct {
	Manager func() *Manager
	Service tasklog.API
}

// Invoke runs one tasks action through the plugin manager.
func (b TasksBackend) Invoke(ctx context.Context, action string, args json.RawMessage) (agentcore.AgentToolResult, error) {
	tool, ok := tasksBackendActions[action]
	if !ok {
		return agentcore.AgentToolResult{}, fmt.Errorf("agentext: unknown tasks action %q", action)
	}
	var manager *Manager
	if b.Manager != nil {
		manager = b.Manager()
	}
	if manager == nil {
		return agentcore.AgentToolResult{}, fmt.Errorf("agentext: %w: the plugin manager is not wired", ErrPluginState)
	}
	return manager.InvokeToolResult(ctx, TasksPluginID, tool, args)
}

// Attribute resolves the card one tool call belongs to. An empty taskID means
// "the session's active card": the task log creates the initial card on demand,
// which is how a session's first tool call is already attributable.
func (b TasksBackend) Attribute(ctx context.Context, scope RunScope, taskID string) (string, error) {
	if b.Service == nil {
		return "", fmt.Errorf("%w: the task log is not wired", tasklog.ErrNoBackend)
	}
	if scope.TenantID == uuid.Nil || scope.SessionID == uuid.Nil {
		return "", fmt.Errorf("%w: the run scope carries no session", tasklog.ErrTaskRequired)
	}
	card, err := b.Service.Resolve(ctx, kernel.Scope{TenantID: scope.TenantID, SessionID: scope.SessionID}, taskID)
	if err != nil {
		return "", err
	}
	return card.TaskID.String(), nil
}

func (p *tasksPlugin) tools() []ToolSpec {
	return []ToolSpec{
		{
			Name:        TasksToolCreate,
			Description: "Create a task card in this session, optionally as a child of another card.",
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "title": {"type": "string", "description": "what this task is for"},
    "status": {"type": "string", "enum": ["open", "in_progress", "blocked", "done"]},
    "parentTaskId": {"type": "string", "description": "parent card id (cross-session allowed)"}
  },
  "required": ["title"],
  "additionalProperties": true
}`),
			Sequential: true,
			Internal:   true,
			Execute:    p.create,
		},
		{
			Name:        TasksToolUpdate,
			Description: "Append a change to a card (title/status/parentTaskId). History is preserved.",
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "taskId": {"type": "string"},
    "title": {"type": "string"},
    "status": {"type": "string", "enum": ["open", "in_progress", "blocked", "done", "closed"]},
    "parentTaskId": {"type": "string"}
  },
  "required": ["taskId"],
  "additionalProperties": true
}`),
			Sequential: true,
			Internal:   true,
			Execute:    p.update,
		},
		{
			Name:        TasksToolAttach,
			Description: "Attach a (path, hash) reference to a card. Bytes stay in the file layer; only the reference is recorded.",
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "taskId": {"type": "string"},
    "path": {"type": "string"},
    "hash": {"type": "string", "description": "content hash, e.g. sha256:..."}
  },
  "required": ["taskId", "path", "hash"],
  "additionalProperties": true
}`),
			Sequential: true,
			Internal:   true,
			Execute:    p.attach,
		},
		{
			Name:        TasksToolDetach,
			Description: "Unbind a path (or one hash of it) from a card with a task.detached event.",
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "taskId": {"type": "string"},
    "path": {"type": "string"},
    "hash": {"type": "string"}
  },
  "required": ["taskId", "path"],
  "additionalProperties": true
}`),
			Sequential: true,
			Internal:   true,
			Execute:    p.detach,
		},
		{
			Name:        TasksToolClose,
			Description: "Close a card. The card, its history and its resources remain readable.",
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "taskId": {"type": "string"},
    "reason": {"type": "string"}
  },
  "required": ["taskId"],
  "additionalProperties": true
}`),
			Sequential: true,
			Internal:   true,
			Execute:    p.close,
		},
		{
			Name:        TasksToolList,
			Description: "List task cards of this session (or another session of the same tenant).",
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "sessionId": {"type": "string"},
    "parentTaskId": {"type": "string"},
    "status": {"type": "string", "enum": ["open", "in_progress", "blocked", "done", "closed"]}
  },
  "additionalProperties": true
}`),
			Sequential: true,
			Internal:   true,
			Execute:    p.list,
		},
		{
			Name:        TasksToolShow,
			Description: "Show one card with its events, children and live attachment references.",
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "taskId": {"type": "string"}
  },
  "required": ["taskId"],
  "additionalProperties": true
}`),
			Sequential: true,
			Internal:   true,
			Execute:    p.show,
		},
		{
			Name:        TasksToolNote,
			Description: "Append a free-form note to a card's history.",
			Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "taskId": {"type": "string"},
    "text": {"type": "string"}
  },
  "required": ["taskId", "text"],
  "additionalProperties": true
}`),
			Sequential: true,
			Internal:   true,
			Execute:    p.note,
		},
	}
}

func (p *tasksPlugin) hooks() []HookSpec {
	return []HookSpec{{
		// The child card exists before the child's first run reads its ledger:
		// the started hook fires right after the child input is committed. Task
		// events are bookkeeping on the transcript path (the run's owed-turn
		// heuristic skips them), so the card never hides the input.
		Event: HookSubagentStarted,
		Handler: func(ctx context.Context, payload HookPayload) error {
			tenantID, err := uuid.Parse(payload.TenantID)
			if err != nil {
				return nil
			}
			parentSession, err := uuid.Parse(payload.ParentSessionID)
			if err != nil {
				return nil
			}
			childSession, err := uuid.Parse(payload.SubagentSessionID)
			if err != nil {
				return nil
			}
			if p.service == nil {
				return nil
			}
			_, linkErr := p.service.LinkSubagent(ctx, kernel.Scope{TenantID: tenantID, SessionID: parentSession}, childSession)
			return linkErr
		},
	}}
}

// runScope converts the ambient run identity into the kernel's scope.
func runScopeForTasks(ctx context.Context) (kernel.Scope, bool) {
	scope, ok := RunScopeFromContext(ctx)
	if !ok || scope.TenantID == uuid.Nil || scope.SessionID == uuid.Nil {
		return kernel.Scope{}, false
	}
	return kernel.Scope{TenantID: scope.TenantID, SessionID: scope.SessionID}, true
}

func (p *tasksPlugin) create(ctx context.Context, _ string, raw json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	scope, ok := runScopeForTasks(ctx)
	if !ok {
		return taskError("run_scope_missing", "tasks actions need the ambient run scope")
	}
	var input struct {
		Title        string `json:"title"`
		Status       string `json:"status"`
		ParentTaskID string `json:"parentTaskId"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return taskError("invalid_arguments", err.Error())
	}
	var parent *uuid.UUID
	if strings.TrimSpace(input.ParentTaskID) != "" {
		parsed, err := uuid.Parse(input.ParentTaskID)
		if err != nil {
			return taskError("invalid_arguments", "parentTaskId is not a UUID")
		}
		parent = &parsed
	}
	if p.service == nil {
		return taskError("backend_error", "the task log is not wired")
	}
	card, err := p.service.Create(ctx, scope, tasklog.CreateInput{Title: input.Title, Status: input.Status, ParentTaskID: parent},
		tasklog.CallIDFromContext(ctx))
	if err != nil {
		return taskFailure(err)
	}
	return taskDocument(cardDocument(card))
}

func (p *tasksPlugin) update(ctx context.Context, _ string, raw json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	scope, ok := runScopeForTasks(ctx)
	if !ok {
		return taskError("run_scope_missing", "tasks actions need the ambient run scope")
	}
	var input struct {
		TaskID       string  `json:"taskId"`
		Title        *string `json:"title"`
		Status       *string `json:"status"`
		ParentTaskID *string `json:"parentTaskId"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return taskError("invalid_arguments", err.Error())
	}
	taskID, err := requiredTaskID(input.TaskID)
	if err != nil {
		return taskError("invalid_arguments", err.Error())
	}
	update := tasklog.UpdateInput{Title: input.Title, Status: input.Status}
	if input.ParentTaskID != nil {
		parent, err := uuid.Parse(*input.ParentTaskID)
		if err != nil {
			return taskError("invalid_arguments", "parentTaskId is not a UUID")
		}
		update.ParentTaskID = &parent
	}
	if p.service == nil {
		return taskError("backend_error", "the task log is not wired")
	}
	card, err := p.service.Update(ctx, scope, taskID, update, tasklog.CallIDFromContext(ctx))
	if err != nil {
		return taskFailure(err)
	}
	return taskDocument(cardDocument(card))
}

func (p *tasksPlugin) attach(ctx context.Context, _ string, raw json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	scope, ok := runScopeForTasks(ctx)
	if !ok {
		return taskError("run_scope_missing", "tasks actions need the ambient run scope")
	}
	var input struct {
		TaskID string `json:"taskId"`
		Path   string `json:"path"`
		Hash   string `json:"hash"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return taskError("invalid_arguments", err.Error())
	}
	taskID, err := requiredTaskID(input.TaskID)
	if err != nil {
		return taskError("invalid_arguments", err.Error())
	}
	if p.service == nil {
		return taskError("backend_error", "the task log is not wired")
	}
	event, err := p.service.Attach(ctx, scope, taskID, input.Path, input.Hash, tasklog.CallIDFromContext(ctx))
	if err != nil {
		return taskFailure(err)
	}
	return taskDocument(map[string]any{"taskId": taskID.String(), "event": eventDocument(event)})
}

func (p *tasksPlugin) detach(ctx context.Context, _ string, raw json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	scope, ok := runScopeForTasks(ctx)
	if !ok {
		return taskError("run_scope_missing", "tasks actions need the ambient run scope")
	}
	var input struct {
		TaskID string `json:"taskId"`
		Path   string `json:"path"`
		Hash   string `json:"hash"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return taskError("invalid_arguments", err.Error())
	}
	taskID, err := requiredTaskID(input.TaskID)
	if err != nil {
		return taskError("invalid_arguments", err.Error())
	}
	if p.service == nil {
		return taskError("backend_error", "the task log is not wired")
	}
	event, err := p.service.Detach(ctx, scope, taskID, input.Path, input.Hash, tasklog.CallIDFromContext(ctx))
	if err != nil {
		return taskFailure(err)
	}
	return taskDocument(map[string]any{"taskId": taskID.String(), "event": eventDocument(event)})
}

func (p *tasksPlugin) close(ctx context.Context, _ string, raw json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	scope, ok := runScopeForTasks(ctx)
	if !ok {
		return taskError("run_scope_missing", "tasks actions need the ambient run scope")
	}
	var input struct {
		TaskID string `json:"taskId"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return taskError("invalid_arguments", err.Error())
	}
	taskID, err := requiredTaskID(input.TaskID)
	if err != nil {
		return taskError("invalid_arguments", err.Error())
	}
	if p.service == nil {
		return taskError("backend_error", "the task log is not wired")
	}
	card, err := p.service.Close(ctx, scope, taskID, input.Reason, tasklog.CallIDFromContext(ctx))
	if err != nil {
		return taskFailure(err)
	}
	return taskDocument(cardDocument(card))
}

func (p *tasksPlugin) list(ctx context.Context, _ string, raw json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	scope, ok := runScopeForTasks(ctx)
	if !ok {
		return taskError("run_scope_missing", "tasks actions need the ambient run scope")
	}
	var input struct {
		SessionID    string `json:"sessionId"`
		ParentTaskID string `json:"parentTaskId"`
		Status       string `json:"status"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return taskError("invalid_arguments", err.Error())
	}
	filter := tasklog.ListFilter{Status: input.Status}
	if strings.TrimSpace(input.SessionID) != "" {
		sessionID, err := uuid.Parse(input.SessionID)
		if err != nil {
			return taskError("invalid_arguments", "sessionId is not a UUID")
		}
		filter.SessionID = &sessionID
	}
	if strings.TrimSpace(input.ParentTaskID) != "" {
		parent, err := uuid.Parse(input.ParentTaskID)
		if err != nil {
			return taskError("invalid_arguments", "parentTaskId is not a UUID")
		}
		filter.ParentTaskID = &parent
	}
	if p.service == nil {
		return taskError("backend_error", "the task log is not wired")
	}
	cards, err := p.service.List(ctx, scope, filter)
	if err != nil {
		return taskFailure(err)
	}
	documents := make([]map[string]any, 0, len(cards))
	for _, card := range cards {
		documents = append(documents, cardDocument(card))
	}
	return taskDocument(map[string]any{"count": len(documents), "tasks": documents})
}

func (p *tasksPlugin) show(ctx context.Context, _ string, raw json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	scope, ok := runScopeForTasks(ctx)
	if !ok {
		return taskError("run_scope_missing", "tasks actions need the ambient run scope")
	}
	var input struct {
		TaskID string `json:"taskId"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return taskError("invalid_arguments", err.Error())
	}
	taskID, err := requiredTaskID(input.TaskID)
	if err != nil {
		return taskError("invalid_arguments", err.Error())
	}
	if p.service == nil {
		return taskError("backend_error", "the task log is not wired")
	}
	view, err := p.service.Show(ctx, scope, taskID)
	if err != nil {
		return taskFailure(err)
	}
	documents := make([]map[string]any, 0, len(view.Children))
	for _, child := range view.Children {
		documents = append(documents, cardDocument(child))
	}
	events := make([]map[string]any, 0, len(view.Events))
	for _, event := range view.Events {
		events = append(events, eventDocument(event))
	}
	return taskDocument(map[string]any{
		"card":        cardDocument(view.Card),
		"events":      events,
		"children":    documents,
		"attachments": view.Attachments,
	})
}

func (p *tasksPlugin) note(ctx context.Context, _ string, raw json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	scope, ok := runScopeForTasks(ctx)
	if !ok {
		return taskError("run_scope_missing", "tasks actions need the ambient run scope")
	}
	var input struct {
		TaskID string `json:"taskId"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return taskError("invalid_arguments", err.Error())
	}
	taskID, err := requiredTaskID(input.TaskID)
	if err != nil {
		return taskError("invalid_arguments", err.Error())
	}
	if p.service == nil {
		return taskError("backend_error", "the task log is not wired")
	}
	event, err := p.service.Note(ctx, scope, taskID, input.Text, tasklog.CallIDFromContext(ctx))
	if err != nil {
		return taskFailure(err)
	}
	return taskDocument(map[string]any{"taskId": taskID.String(), "event": eventDocument(event)})
}

func requiredTaskID(raw string) (uuid.UUID, error) {
	if strings.TrimSpace(raw) == "" {
		return uuid.Nil, fmt.Errorf("taskId is required")
	}
	return uuid.Parse(raw)
}

func cardDocument(card tasklog.Card) map[string]any {
	document := map[string]any{
		"taskId":     card.TaskID.String(),
		"sessionId":  card.SessionID.String(),
		"title":      card.Title,
		"status":     card.Status,
		"origin":     card.Origin,
		"createdSeq": card.CreatedSeq,
		"updatedSeq": card.UpdatedSeq,
		"createdAt":  card.CreatedAt,
		"updatedAt":  card.UpdatedAt,
	}
	if card.ParentTaskID != nil {
		document["parentTaskId"] = card.ParentTaskID.String()
	}
	if card.OriginSeq != 0 {
		document["originSeq"] = card.OriginSeq
	}
	if card.ClosedSeq != 0 {
		document["closedSeq"] = card.ClosedSeq
	}
	return document
}

func eventDocument(event tasklog.Event) map[string]any {
	document := map[string]any{
		"taskId": event.TaskID.String(),
		"kind":   event.Kind,
		"seq":    event.Seq,
	}
	if len(event.Payload) > 0 {
		document["payload"] = json.RawMessage(event.Payload)
	}
	if !event.CreatedAt.IsZero() {
		document["createdAt"] = event.CreatedAt
	}
	return document
}

// taskFailure maps a task-log error onto the typed tool-result code the model
// reads. The codes are the plugin-side vocabulary of toolsurface's shared set.
func taskFailure(err error) (agentcore.AgentToolResult, error) {
	switch {
	case errors.Is(err, tasklog.ErrTaskNotFound):
		return taskError("not_found", err.Error())
	case errors.Is(err, tasklog.ErrTaskRequired), errors.Is(err, tasklog.ErrInvalidTaskID):
		return taskError("task_id_required", err.Error())
	case errors.Is(err, tasklog.ErrInvalidEvent), errors.Is(err, tasklog.ErrInvalidStatus),
		errors.Is(err, tasklog.ErrTaskCycle):
		return taskError("invalid_arguments", err.Error())
	default:
		return taskError("backend_error", err.Error())
	}
}

func taskError(code, message string) (agentcore.AgentToolResult, error) {
	document := map[string]any{"error": map[string]any{
		"code":    code,
		"tool":    TasksPluginID,
		"message": strings.TrimSpace(message),
	}}
	encoded, _ := json.Marshal(document)
	isError := true
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(string(encoded))},
		StructuredContent: document,
		IsError:           &isError,
	}, nil
}

func taskDocument(document map[string]any) (agentcore.AgentToolResult, error) {
	encoded, err := json.Marshal(document)
	if err != nil {
		return taskError("backend_error", err.Error())
	}
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(string(encoded))},
		StructuredContent: document,
	}, nil
}
