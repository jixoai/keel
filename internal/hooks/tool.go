package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
)

// ToolWrapOptions configures WrapToolWithOptions.
type ToolWrapOptions struct {
	// EmitExecutionEvents controls emission of tool_execution_start,
	// tool_execution_update, and tool_execution_end around the wrapped call.
	// WrapTool enables them. A host whose executor already emits these events
	// (for example agenttool.ExecuteToolCalls with an EmitFunc) should disable
	// them here to avoid duplicates.
	EmitExecutionEvents bool
	// ParentToolCallID is set on emitted events for nested calls.
	ParentToolCallID string
}

// DeniedError reports a tool call blocked by a tool_call hook. Result is the
// denial result the model should receive; Reason is the hook's reason.
type DeniedError struct {
	ToolName string
	Reason   string
	Result   agentcore.AgentToolResult
}

func (e *DeniedError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("tool %q denied by hook", e.ToolName)
	}
	return fmt.Sprintf("tool %q denied by hook: %s", e.ToolName, e.Reason)
}

// IsDenied reports whether err is a hook denial, including wrapped errors.
func IsDenied(err error) bool {
	var denied *DeniedError
	return errors.As(err, &denied)
}

// WrapTool decorates tool with the registry's tool hooks:
//
//  1. tool_execution_start (notification, before gate)
//  2. tool_call (gate/transform; a deny prevents execution)
//  3. tool execution, forwarding partial results as tool_execution_update
//  4. tool_result (transform; partial patches chain)
//  5. tool_execution_end (notification)
//
// The wrapped tool keeps the original name, description, schema, and execution
// mode. A denied call returns the denial result plus a *DeniedError; the
// underlying tool is never invoked.
func WrapTool(registry *Registry, tool agentcore.AgentTool) agentcore.AgentTool {
	return WrapToolWithOptions(registry, tool, ToolWrapOptions{EmitExecutionEvents: true})
}

// WrapToolWithOptions is WrapTool with explicit options.
func WrapToolWithOptions(registry *Registry, tool agentcore.AgentTool, options ToolWrapOptions) agentcore.AgentTool {
	return &wrappedTool{registry: registry, tool: tool, options: options}
}

type wrappedTool struct {
	registry *Registry
	tool     agentcore.AgentTool
	options  ToolWrapOptions
}

func (t *wrappedTool) Name() string        { return t.tool.Name() }
func (t *wrappedTool) Description() string { return t.tool.Description() }
func (t *wrappedTool) Schema() json.RawMessage {
	return t.tool.Schema()
}
func (t *wrappedTool) ExecutionMode() agentcore.ToolExecutionMode {
	return t.tool.ExecutionMode()
}

func (t *wrappedTool) Execute(
	ctx context.Context,
	id string,
	args json.RawMessage,
	onUpdate agentcore.ToolUpdateFunc,
) (agentcore.AgentToolResult, error) {
	name := t.tool.Name()

	if t.options.EmitExecutionEvents {
		t.registry.Emit(ctx, &ToolExecutionStartEvent{
			ToolCallID:       id,
			ToolName:         name,
			Args:             args,
			ParentToolCallID: t.options.ParentToolCallID,
		})
	}

	callEvent := &ToolCallEvent{
		ToolCallID:       id,
		ToolName:         name,
		Input:            args,
		ParentToolCallID: t.options.ParentToolCallID,
	}
	decision := t.registry.EmitToolCall(ctx, callEvent)
	if decision.Action.Normalized() == ActionDeny {
		return t.deny(ctx, id, name, decision)
	}
	args = callEvent.Input

	update := func(partial agentcore.AgentToolResult) {
		if t.options.EmitExecutionEvents {
			t.registry.Emit(ctx, &ToolExecutionUpdateEvent{
				ToolCallID:       id,
				ToolName:         name,
				Args:             args,
				PartialResult:    partial,
				ParentToolCallID: t.options.ParentToolCallID,
			})
		}
		if onUpdate != nil {
			onUpdate(partial)
		}
	}

	started := time.Now()
	result, execErr := t.tool.Execute(ctx, id, args, update)
	elapsed := time.Since(started).Milliseconds()
	isError := execErr != nil

	// tool_result sees a shaped result even when execution failed, so redaction
	// and logging hooks still run.
	if execErr != nil && len(result.Content) == 0 {
		result.Content = agentcore.ContentList{agentcore.NewTextContent(execErr.Error())}
	}
	resultEvent := &ToolResultEvent{
		ToolCallID:       id,
		ToolName:         name,
		ParentToolCallID: t.options.ParentToolCallID,
		Input:            args,
		Content:          result.Content,
		Details:          result.Details,
		DetailsSet:       result.Details != nil,
		IsError:          isError,
		Terminate:        result.Terminate,
	}
	patched, _ := t.registry.EmitToolResult(ctx, resultEvent)
	result.Content = patched.Content
	result.Details = patched.Details
	result.Terminate = patched.Terminate
	if patched.IsError {
		// agentcore.AgentToolResult has no isError channel; the end event below
		// carries it, and the host may promote it at its own tool seam.
		isError = patched.IsError
	}

	if t.options.EmitExecutionEvents {
		t.registry.Emit(ctx, &ToolExecutionEndEvent{
			ToolCallID:       id,
			ToolName:         name,
			Result:           result,
			IsError:          isError,
			DurationMs:       &elapsed,
			ParentToolCallID: t.options.ParentToolCallID,
		})
	}

	return result, execErr
}

func (t *wrappedTool) deny(ctx context.Context, id, name string, decision Decision) (agentcore.AgentToolResult, error) {
	reason := decision.Reason
	if reason == "" {
		reason = fmt.Sprintf("tool %q blocked by hook", name)
	}
	result := agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent(reason)},
	}
	if decision.Result != nil {
		if decision.Result.Content != nil {
			result.Content = *decision.Result.Content
		}
		if decision.Result.DetailsSet {
			result.Details = decision.Result.Details
		}
		if decision.Result.Terminate != nil {
			result.Terminate = decision.Result.Terminate
		}
	}
	if decision.Terminate {
		terminate := true
		result.Terminate = &terminate
	}

	if t.options.EmitExecutionEvents {
		t.registry.Emit(ctx, &ToolExecutionEndEvent{
			ToolCallID:       id,
			ToolName:         name,
			Result:           result,
			IsError:          true,
			DurationMs:       nil,
			ParentToolCallID: t.options.ParentToolCallID,
		})
	}
	return result, &DeniedError{ToolName: name, Reason: reason, Result: result}
}
