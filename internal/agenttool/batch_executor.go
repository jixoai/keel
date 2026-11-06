// Copied from github.com/smallnest/pigo@891d1f3 (MIT License).
// Upstream: internal/agenttool/batch_executor.go
// Modified for keel: module import paths rewritten; see docs/UPSTREAM.md.

// This file implements batch tool execution (US-005): a batch of tool calls
// from one assistant message is run either sequentially or in parallel, mirroring
// pi's semantics.
//
//   - sequential mode runs each call prepare→execute→finalize in order and stops
//     early if the context is aborted.
//   - parallel mode preserves ordering by index-backfilling results, running the
//     allowed calls in goroutines. (prepare is not separately staged here because
//     executeToolCall keeps prepare+execute together per call; ordering is still
//     guaranteed by writing each result to its source index.)
//
// The whole batch signals termination only when every finalized result has
// terminate=true, matching pi.
package agenttool

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/jixoai/keel/internal/agentcore"
)

// ForceSequential, when true, makes the whole batch run serially regardless of
// per-tool ExecutionMode.
type BatchConfig struct {
	ToolExecutorConfig
	ForceSequential bool
}

// ExecuteToolCalls runs a batch of tool calls belonging to one assistant
// message. It returns the tool-result messages in source order and whether the
// whole batch requests termination (only when every result terminates).
func ExecuteToolCalls(ctx context.Context, cfg BatchConfig, calls []agentcore.AgentToolCall, emit agentcore.EmitFunc) ([]agentcore.ToolResultMessage, bool) {
	if len(calls) == 0 {
		return nil, false
	}

	results := make([]agentcore.ToolResultMessage, len(calls))
	terminates := make([]bool, len(calls))

	if cfg.ForceSequential || batchRequiresSequential(cfg.Registry, calls) {
		for i, call := range calls {
			if ctx.Err() != nil {
				// Abort: fill the remaining calls with aborted error results so
				// every tool call still gets a result message.
				for j := i; j < len(calls); j++ {
					results[j] = errorToolResult(calls[j], "tool call aborted")
					terminates[j] = false
				}
				break
			}
			results[i], terminates[i] = executeToolCall(ctx, cfg.ToolExecutorConfig, call, emit)
		}
	} else {
		// Parallel: preflight every call sequentially in source order (emitting
		// tool_execution_start in that order), then execute the allowed calls
		// concurrently. tool_execution_end is emitted per call in completion
		// order by the executing goroutine (pi's executeToolCallsParallel).
		type preparedExecution struct {
			index int
			call  agentcore.AgentToolCall
			tool  agentcore.AgentTool
			args  json.RawMessage
		}
		executions := make([]preparedExecution, 0, len(calls))
		for i, call := range calls {
			if ctx.Err() != nil {
				for j := i; j < len(calls); j++ {
					results[j] = errorToolResult(calls[j], "tool call aborted")
					terminates[j] = false
				}
				break
			}
			if emit != nil {
				if err := emit(ctx, agentcore.ToolExecutionStartEvent{
					ToolCallID: call.ID, ToolName: call.Name, Args: call.Arguments,
				}); err != nil {
					results[i] = errorToolResult(call, "aborted before execution: "+err.Error())
					continue
				}
			}
			tool, args, prep, isError := prepareToolCall(ctx, cfg.ToolExecutorConfig, call)
			if prep != nil {
				results[i], terminates[i] = finalizeToolCall(
					ctx, cfg.ToolExecutorConfig, call, *prep, isError, nil, emit, false)
				continue
			}
			executions = append(executions, preparedExecution{index: i, call: call, tool: tool, args: args})
		}

		var wg sync.WaitGroup
		for _, ex := range executions {
			wg.Add(1)
			go func(ex preparedExecution) {
				defer wg.Done()
				results[ex.index], terminates[ex.index] = executePreparedToolCall(
					ctx, cfg.ToolExecutorConfig, ex.call, ex.tool, ex.args, emit)
			}(ex)
		}
		wg.Wait()
	}

	// Whole batch terminates only when every result terminates (pi semantics).
	allTerminate := true
	for _, t := range terminates {
		if !t {
			allTerminate = false
			break
		}
	}
	return results, allTerminate
}

// batchRequiresSequential reports whether any tool in the batch declares
// ExecutionMode sequential, which forces the whole batch to run serially.
func batchRequiresSequential(reg *ToolRegistry, calls []agentcore.AgentToolCall) bool {
	for _, call := range calls {
		if tool, ok := reg.Get(call.Name); ok && tool.ExecutionMode() == agentcore.ToolExecutionSequential {
			return true
		}
	}
	return false
}
