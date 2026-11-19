package toolsurface

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/fileplugin"
)

// Typed file-tool failures. not_found / invalid_arguments / plugin_disabled /
// run_scope_missing already exist in the package's error vocabulary; conflict,
// quota_exceeded, io and unavailable complete the layerfs/file-plugin set.
const (
	CodeConflict      = "conflict"
	CodeQuotaExceeded = "quota_exceeded"
	CodeIO            = "io"
	CodeUnavailable   = "unavailable"
)

// FileBackend is the keel.file plugin's action backend. The read/write/edit
// tools are harness-owned and constant; the plugin supplies the implementation,
// so disabling keel.file yields plugin_disabled instead of removing the tools.
type FileBackend interface {
	Read(ctx context.Context, args json.RawMessage) (agentcore.AgentToolResult, error)
	Write(ctx context.Context, args json.RawMessage) (agentcore.AgentToolResult, error)
	Edit(ctx context.Context, args json.RawMessage) (agentcore.AgentToolResult, error)
}

// fileTool is one single-action tool (read/write/edit). It installs the ambient
// session scope and the model tool-call id on the context before dispatching:
// the file plugin needs both (scope for the manifest, call id for fs.write
// idempotency).
type fileTool struct {
	name        string
	description string
	schema      json.RawMessage
	available   bool
	run         func(ctx context.Context, args json.RawMessage) (agentcore.AgentToolResult, error)
}

func (t *fileTool) Name() string            { return t.name }
func (t *fileTool) Description() string     { return t.description }
func (t *fileTool) Schema() json.RawMessage { return t.schema }
func (t *fileTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}

func (t *fileTool) Execute(ctx context.Context, callID string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	if !t.available || t.run == nil {
		return notWired(t.name, "no file backend is wired in this deployment")
	}
	callCtx, err := fileCallContext(ctx, callID)
	if err != nil {
		return mapFileError(t.name, err)
	}
	result, err := t.run(callCtx, args)
	if err != nil {
		return mapFileError(t.name, err)
	}
	return result, nil
}

// fileCallContext carries the run identity and the tool-call id into the plugin.
func fileCallContext(ctx context.Context, callID string) (context.Context, error) {
	scope, ok := agentext.RunScopeFromContext(ctx)
	if !ok {
		return ctx, fileplugin.Errorf(fileplugin.CodeRunScopeMissing,
			"read/write/edit need the ambient run scope; a model tool call inside a run always has it")
	}
	ctx = fileplugin.WithScope(ctx, fileplugin.Scope{TenantID: scope.TenantID, SessionID: scope.SessionID})
	ctx = fileplugin.WithCallID(ctx, callID)
	return ctx, nil
}

func mapFileError(tool string, err error) (agentcore.AgentToolResult, error) {
	if errors.Is(err, fileplugin.ErrPluginDisabled) {
		return ErrorResult(CodePluginDisabled, tool,
			"keel.file is not enabled; enable the plugin to use read/write/edit", nil)
	}
	switch fileplugin.CodeOf(err) {
	case fileplugin.CodeNotFound:
		return notFound(tool, err.Error())
	case fileplugin.CodeConflict:
		return ErrorResult(CodeConflict, tool, err.Error(), nil)
	case fileplugin.CodeInvalid:
		return invalidArguments(tool, err.Error())
	case fileplugin.CodeQuotaExceeded:
		return ErrorResult(CodeQuotaExceeded, tool, err.Error(), nil)
	case fileplugin.CodeUnavailable:
		return ErrorResult(CodeUnavailable, tool, err.Error(), nil)
	case fileplugin.CodeRunScopeMissing:
		return ErrorResult(CodeRunScopeMissing, tool, err.Error(), nil)
	case fileplugin.CodePluginDisabled:
		return ErrorResult(CodePluginDisabled, tool, err.Error(), nil)
	case fileplugin.CodeIO:
		return ErrorResult(CodeIO, tool, err.Error(), nil)
	default:
		return ErrorResult(CodeBackendFailed, tool, err.Error(), nil)
	}
}

func newReadTool(backend FileBackend) agentcore.AgentTool {
	return &fileTool{
		name: Read,
		description: "Read a file from the session's layerfs view. offset/limit are 0-based line numbers; " +
			"the result is bounded (default 200 lines / 256KiB) and reports totalLines, the content hash and " +
			"truncated. Pass the returned hash as expectedHash to write/edit.",
		schema:    json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":0}},"required":["path"],"additionalProperties":true}`),
		available: backend != nil,
		run: func(ctx context.Context, args json.RawMessage) (agentcore.AgentToolResult, error) {
			if backend == nil {
				return notWired(Read, "no file backend is wired in this deployment")
			}
			return backend.Read(ctx, args)
		},
	}
}
