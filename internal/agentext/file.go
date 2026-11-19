package agentext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"strings"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/fileplugin"
)

// The file plugin (keel.file) owns the file manifest and the single write path.
// The model-facing read/write/edit tools are harness-owned builtins; this
// plugin supplies their backend through internal tool handlers, exactly like
// keel.creation supplies the `creation` aggregate tool. Disabling the plugin
// therefore yields plugin_disabled instead of removing a tool name.
const (
	FilePluginID      = "keel.file"
	FilePluginVersion = "1.0.0"

	FileToolRead    = "keel.file.read"
	FileToolWrite   = "keel.file.write"
	FileToolEdit    = "keel.file.edit"
	FileToolCapture = "keel.file.capture"
)

// FilePluginOptions wires the file service. A nil service still registers the
// plugin (the manifest is builtin); its handlers answer unavailable, so a
// deployment without the layerfs wiring reports that instead of disappearing.
type FilePluginOptions struct {
	Service *fileplugin.Service
}

// NewFilePlugin builds the keel.file plugin. All handlers are Internal: they
// never enter the model-facing tool set, which is the constant eleven-tool
// surface.
func NewFilePlugin(opts FilePluginOptions) *Plugin {
	service := opts.Service
	read := func(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		if service == nil {
			return agentcore.AgentToolResult{}, fileplugin.Errorf(fileplugin.CodeUnavailable, "the file service is not wired")
		}
		result, err := service.Read(ctx, args)
		if err != nil {
			return agentcore.AgentToolResult{}, err
		}
		return fileReadResult(result), nil
	}
	write := func(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		if service == nil {
			return agentcore.AgentToolResult{}, fileplugin.Errorf(fileplugin.CodeUnavailable, "the file service is not wired")
		}
		result, err := service.Write(ctx, args)
		if err != nil {
			return agentcore.AgentToolResult{}, err
		}
		return fileJSONResult(result), nil
	}
	edit := func(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		if service == nil {
			return agentcore.AgentToolResult{}, fileplugin.Errorf(fileplugin.CodeUnavailable, "the file service is not wired")
		}
		result, err := service.Edit(ctx, args)
		if err != nil {
			return agentcore.AgentToolResult{}, err
		}
		return fileJSONResult(result), nil
	}
	capture := func(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		if service == nil {
			return agentcore.AgentToolResult{}, fileplugin.Errorf(fileplugin.CodeUnavailable, "the file service is not wired")
		}
		result, err := service.Capture(ctx, args)
		if err != nil {
			return agentcore.AgentToolResult{}, err
		}
		return fileJSONResult(result), nil
	}
	return &Plugin{
		ID:       FilePluginID,
		Version:  FilePluginVersion,
		Provides: []string{"cap:file"},
		Tools: []ToolSpec{
			{Name: FileToolRead, Description: "Read a session file page.", Schema: fileToolSchema(), Internal: true, Execute: read},
			{Name: FileToolWrite, Description: "Write a session file.", Schema: fileToolSchema(), Internal: true, Execute: write},
			{Name: FileToolEdit, Description: "Edit a session file.", Schema: fileToolSchema(), Internal: true, Execute: edit},
			{Name: FileToolCapture, Description: "Capture the execution space.", Schema: fileToolSchema(), Internal: true, Execute: capture},
		},
	}
}

// FileBackend adapts the keel.file plugin's internal handlers to the
// harness-owned read/write/edit tools. The Manager getter is lazy because the
// builtin factories are registered before the manager exists.
type FileBackend struct {
	Manager func() *Manager
}

// Read dispatches to keel.file.read.
func (b FileBackend) Read(ctx context.Context, args json.RawMessage) (agentcore.AgentToolResult, error) {
	return b.invoke(ctx, FileToolRead, args)
}

// Write dispatches to keel.file.write.
func (b FileBackend) Write(ctx context.Context, args json.RawMessage) (agentcore.AgentToolResult, error) {
	return b.invoke(ctx, FileToolWrite, args)
}

// Edit dispatches to keel.file.edit.
func (b FileBackend) Edit(ctx context.Context, args json.RawMessage) (agentcore.AgentToolResult, error) {
	return b.invoke(ctx, FileToolEdit, args)
}

func (b FileBackend) invoke(ctx context.Context, tool string, args json.RawMessage) (agentcore.AgentToolResult, error) {
	manager := (*Manager)(nil)
	if b.Manager != nil {
		manager = b.Manager()
	}
	if manager == nil {
		return agentcore.AgentToolResult{}, fileplugin.Errorf(fileplugin.CodeUnavailable, "the plugin manager is not wired")
	}
	result, err := manager.InvokeToolResult(ctx, FilePluginID, tool, args)
	if err != nil {
		if errors.Is(err, ErrPluginState) || errors.Is(err, ErrPluginNotFound) {
			return agentcore.AgentToolResult{}, fmt.Errorf("%w: %v", fileplugin.ErrPluginDisabled, err)
		}
		return agentcore.AgentToolResult{}, err
	}
	return result, nil
}

func fileToolSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":true}`)
}

func fileReadResult(result fileplugin.ReadResult) agentcore.AgentToolResult {
	header := fmt.Sprintf(`<file path="%s" hash="%s" size="%d" totalLines="%d" offset="%d" limit="%d" returnedLines="%d" nextOffset="%d" truncated="%t">`,
		html.EscapeString(result.Path), html.EscapeString(result.Hash), result.Size, result.TotalLines,
		result.Offset, result.Limit, result.ReturnedLines, result.NextOffset, result.Truncated)
	body := result.Content
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	text := header + "\n" + body + "</file>"
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(text)},
		StructuredContent: result,
	}
}

func fileJSONResult(value any) agentcore.AgentToolResult {
	raw, err := json.Marshal(value)
	if err != nil {
		return errorToolResult("encode file tool result: " + err.Error())
	}
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(string(raw))},
		StructuredContent: value,
	}
}
