// runToolCall ported from pi's agent-loop.ts (pin 55a5c69, MIT License).
//
// It runs one tool call through the same steps as a model-issued call:
// argument preparation, schema validation, beforeToolCall, execution, and
// afterToolCall. It emits no events and adds no messages. Tools that call other
// tools use it so the hooks (for example permission checks) apply to those
// calls too.

package agenttool

import (
	"context"

	"github.com/jixoai/keel/internal/agentcore"
)

// RunToolCallOptions configures RunToolCall.
type RunToolCallOptions struct {
	// Tools the call resolves against.
	Tools []agentcore.AgentTool
	// AssistantMessage and Context are accepted for hook parity with pi's
	// RunToolCallOptions. Keel's hook signatures carry only the call, so they
	// are not forwarded; they stay for call-site clarity.
	AssistantMessage agentcore.AssistantMessage
	Context          *agentcore.AgentContext

	PrepareArguments agentcore.PrepareArgumentsFunc
	BeforeToolCall   agentcore.BeforeToolCallFunc
	AfterToolCall    agentcore.AfterToolCallFunc
	// OnUpdate receives partial results streamed by the tool while it runs.
	OnUpdate func(agentcore.AgentToolResult)
}

// RunToolCall runs one tool call. It never returns an error for tool failures:
// unknown tools, validation errors, blocked calls, and thrown errors come back
// as IsError=true outcomes (pi's runToolCall contract).
func RunToolCall(ctx context.Context, call agentcore.AgentToolCall, options RunToolCallOptions) agentcore.AgentToolCallOutcome {
	reg := NewToolRegistry()
	for _, tool := range options.Tools {
		if err := reg.Register(tool); err != nil {
			return agentcore.AgentToolCallOutcome{
				ToolCall: call,
				Result:   errorResult(err.Error()),
				IsError:  true,
			}
		}
	}
	cfg := ToolExecutorConfig{
		Registry:         reg,
		PrepareArguments: options.PrepareArguments,
		BeforeToolCall:   options.BeforeToolCall,
		AfterToolCall:    options.AfterToolCall,
	}

	tool, args, prep, isError := prepareToolCall(ctx, cfg, call)
	if prep != nil {
		return agentcore.AgentToolCallOutcome{ToolCall: call, Result: *prep, IsError: isError}
	}

	result, isError, durationMs := runToolWithRetry(ctx, cfg, tool, call, args, nil, options.OnUpdate)
	if cfg.AfterToolCall != nil {
		result, isError = applyAfterToolCall(ctx, cfg.AfterToolCall, call, result, isError)
	}
	return agentcore.AgentToolCallOutcome{
		ToolCall:   call,
		Result:     result,
		IsError:    isError,
		DurationMs: durationMs,
	}
}
