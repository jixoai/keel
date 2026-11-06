// Copied from github.com/smallnest/pigo@891d1f3 (MIT License).
// Upstream: internal/agenttool/tool_executor.go
// Modified for keel: module import paths rewritten; see docs/UPSTREAM.md.

// This file implements the three-phase tool execution (US-004): prepare →
// execute → finalize, with the beforeToolCall / afterToolCall hooks. It mirrors
// pi's agent-loop tool handling: a tool call is looked up in the registry, its
// arguments are (optionally) prepared and schema-validated, the beforeToolCall
// hook may block it, the tool runs (streaming partial updates), and the
// afterToolCall hook may override the result field-by-field (no deep merge).
//
// Every failure mode (unknown tool, validation failure, block, abort, tool
// error/panic) is turned into an error tool result rather than a Go error, so
// the loop always has a ToolResultMessage to feed back to the model.
package agenttool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
)

// toolResultMaxBytes is the executor-layer budget for a single tool result's
// combined text, applied uniformly to EVERY tool right before its result enters
// the AgentToolResult / message list. Individual tools also impose their own,
// stricter inner caps (read: readToolMaxLines, search: searchMaxResults,
// webfetch: webFetchMaxBytes, bash: bashMaxOutputBytes); those still run first
// and clip a tool below this outer budget. This budget is the last line of
// defense so a tool with no (or a looser) inner cap cannot blow the model's
// context. Override per-executor via ToolExecutorConfig.MaxResultBytes.
const toolResultMaxBytes = 100_000

// ToolExecutorConfig holds the registry and the optional per-phase hooks. Every
// hook is optional (nil = default behavior).
type ToolExecutorConfig struct {
	Registry         *ToolRegistry
	PrepareArguments agentcore.PrepareArgumentsFunc
	BeforeToolCall   agentcore.BeforeToolCallFunc
	AfterToolCall    agentcore.AfterToolCallFunc
	// MaxResultBytes overrides the executor-layer per-result text budget. Zero
	// (the default) uses toolResultMaxBytes; a negative value disables the
	// budget entirely.
	MaxResultBytes int
	// MaxToolRetries overrides the number of RETRIES for a transient tool error
	// (see isRetryableToolError). Zero (the default) uses maxToolRetries; a
	// negative value disables retrying (a single attempt). Mirrors the
	// MaxResultBytes sentinel convention.
	MaxToolRetries int
}

// executeToolCall runs one tool call through prepare → execute → finalize and
// returns the resulting ToolResultMessage plus whether the batch should
// terminate. emit may be nil (no events). It never returns a Go error: every
// failure is encoded into the returned message with IsError=true.
func executeToolCall(ctx context.Context, cfg ToolExecutorConfig, call agentcore.AgentToolCall, emit agentcore.EmitFunc) (agentcore.ToolResultMessage, bool) {
	// tool_execution_start is emitted before prepare, with the raw arguments,
	// matching pi's executeToolCallsSequential.
	if emit != nil {
		if err := emit(ctx, agentcore.ToolExecutionStartEvent{ToolCallID: call.ID, ToolName: call.Name, Args: call.Arguments}); err != nil {
			return errorToolResult(call, "aborted before execution: "+err.Error()), false
		}
	}

	// 1. prepare: lookup, prepareArguments, validate, beforeToolCall.
	tool, args, prep, isError := prepareToolCall(ctx, cfg, call)
	if prep != nil {
		// Prepare short-circuited (unknown tool / prepare error / validation /
		// block / abort): finalize the error result without executing and without
		// the afterToolCall hook (pi: the hook runs only for executed calls).
		return finalizeToolCall(ctx, cfg, call, *prep, isError, nil, emit, false)
	}

	// 2. execute.
	return executePreparedToolCall(ctx, cfg, call, tool, args, emit)
}

// executePreparedToolCall runs an already-prepared call (execute + finalize).
// It is shared by the sequential path and by the parallel path, whose preflight
// phase resolves every call before any execution starts.
func executePreparedToolCall(ctx context.Context, cfg ToolExecutorConfig, call agentcore.AgentToolCall, tool agentcore.AgentTool, args json.RawMessage, emit agentcore.EmitFunc) (agentcore.ToolResultMessage, bool) {
	result, isError, durationMs := runToolWithRetry(ctx, cfg, tool, call, args, emit, func(partial agentcore.AgentToolResult) {
		if emit != nil {
			_ = emit(ctx, agentcore.ToolExecutionUpdateEvent{ToolCallID: call.ID, ToolName: call.Name, PartialResult: partial})
		}
	})

	// 3. finalize: afterToolCall overrides.
	return finalizeToolCall(ctx, cfg, call, result, isError, durationMs, emit, true)
}

// prepareToolCall performs the prepare phase. On success it returns the tool and
// the (possibly rewritten) arguments with a nil result. On any short-circuit it
// returns a non-nil *AgentToolResult and the isError flag.
func prepareToolCall(ctx context.Context, cfg ToolExecutorConfig, call agentcore.AgentToolCall) (agentcore.AgentTool, json.RawMessage, *agentcore.AgentToolResult, bool) {
	if ctx.Err() != nil {
		r := errorResult("Operation aborted")
		return nil, nil, &r, true
	}

	// Registry lookup.
	tool, ok := cfg.Registry.Get(call.Name)
	if !ok {
		r := errorResult(fmt.Sprintf("Tool %s not found", call.Name))
		return nil, nil, &r, true
	}

	// prepareArguments (optional).
	args := call.Arguments
	if cfg.PrepareArguments != nil {
		prepared, err := cfg.PrepareArguments(ctx, call.Name, args)
		if err != nil {
			r := errorResult(fmt.Sprintf("prepareArguments for %q failed: %v", call.Name, err))
			return nil, nil, &r, true
		}
		args = prepared
	}

	// JSON Schema validation.
	if errs := cfg.Registry.Validate(call.Name, args); len(errs) > 0 {
		r := ValidationErrorResult(call.Name, errs)
		return nil, nil, &r, true
	}

	// beforeToolCall hook (may block or rewrite arguments).
	if cfg.BeforeToolCall != nil {
		if dec := cfg.BeforeToolCall(ctx, agentcore.AgentToolCall{ID: call.ID, Name: call.Name, Arguments: args}); dec != nil {
			if dec.Block {
				r := agentcore.AgentToolResult{}
				if dec.Content != nil {
					r.Content = *dec.Content
				} else {
					r.Content = agentcore.ContentList{agentcore.NewTextContent("Tool execution was blocked")}
				}
				if dec.Details != nil {
					r.Details = *dec.Details
				}
				// A blocked result may participate in the batch early-termination
				// rule (pi: BeforeToolCallResult.terminate).
				if dec.Terminate {
					terminate := true
					r.Terminate = &terminate
				}
				return nil, nil, &r, true
			}
			// Argument rewrite (PreToolUse updatedInput): the hook runs after
			// schema validation, so the rewritten input is used as-is and NOT
			// re-validated, matching pi's beforeToolCall contract. The hook is the
			// trust boundary; a host that wants validation enforces it in the hook.
			if len(dec.UpdatedInput) > 0 {
				args = dec.UpdatedInput
			}
		}
	}

	return tool, args, nil, false
}

// runToolWithRetry wraps runTool with the classified, bounded retry policy at
// the single tool-execution seam so EVERY tool gets uniform resilience. It only
// retries when runTool surfaces a non-nil Go error (transport/agent-error path)
// AND isRetryableToolError says that error is transient; a (result, nil) is
// done regardless of the result's IsError flag (a tool's own terminal result is
// never retried). Retries are capped by toolRetryCap and separated by a small
// backoff. Context cancellation short-circuits immediately: a cancelled/expired
// outer ctx is never retried.
func runToolWithRetry(ctx context.Context, cfg ToolExecutorConfig, tool agentcore.AgentTool, call agentcore.AgentToolCall, args json.RawMessage, emit agentcore.EmitFunc, onUpdate func(agentcore.AgentToolResult)) (agentcore.AgentToolResult, bool, *int64) {
	retryCap := toolRetryCap(cfg.MaxToolRetries)

	var lastResult agentcore.AgentToolResult
	var lastIsError bool
	var lastDurationMs *int64
	for attempt := 0; attempt <= retryCap; attempt++ {
		result, err, isError, durationMs := runTool(ctx, tool, call, args, onUpdate)
		lastResult, lastIsError, lastDurationMs = result, isError, durationMs
		if err == nil {
			// Execute returned (result, nil): terminal success regardless of
			// the result's own IsError flag. Done, no retry.
			return result, isError, durationMs
		}

		// Do not retry if the outer context is done (Canceled or its deadline
		// has passed) — a dead context means stop.
		if ctx.Err() != nil {
			break
		}
		// Only transient errors are retried, and only if we have budget left.
		if attempt >= retryCap || !isRetryableToolError(err) {
			break
		}
		// Small backoff; abort the wait early if ctx dies mid-sleep.
		if !waitToolRetryBackoff(ctx, attempt) {
			break
		}
	}
	return lastResult, lastIsError, lastDurationMs
}

// runTool executes the tool, recovering a panic into an error. It returns the
// shaped error result, the raw error (nil on success), the isError flag, and how
// long execute() itself took (monotonic clock, excluding retry backoff and the
// before/after hooks). The raw error is surfaced so the caller can classify it
// for retry; on success err is nil even if the result itself carries IsError
// semantics. durationMs is always non-nil: execute() was called.
func runTool(ctx context.Context, tool agentcore.AgentTool, call agentcore.AgentToolCall, args json.RawMessage, onUpdate func(agentcore.AgentToolResult)) (result agentcore.AgentToolResult, rawErr error, isError bool, durationMs *int64) {
	defer func() {
		if r := recover(); r != nil {
			result = errorResult(fmt.Sprintf("tool %q panicked: %v", call.Name, r))
			rawErr = toolPanic{value: r}
			isError = true
		}
	}()

	// Updates are scoped to the execute() invocation: calls made after the tool
	// settles (or after the update sink is released) are ignored, matching pi's
	// "calls after the tool promise settles are ignored" contract.
	var updateMu sync.Mutex
	acceptingUpdates := true
	sink := func(partial agentcore.AgentToolResult) {
		updateMu.Lock()
		ok := acceptingUpdates
		updateMu.Unlock()
		if ok && onUpdate != nil {
			onUpdate(partial)
		}
	}
	stopUpdates := func() {
		updateMu.Lock()
		acceptingUpdates = false
		updateMu.Unlock()
	}
	defer stopUpdates()

	startedAt := time.Now()
	res, err := tool.Execute(ctx, call.ID, args, sink)
	elapsed := time.Since(startedAt).Milliseconds()
	stopUpdates()
	durationMs = &elapsed
	if err != nil {
		return errorResult(fmt.Sprintf("tool %q failed: %v", call.Name, err)), err, true, durationMs
	}
	// A tool may report a failure without throwing (pi's
	// AgentToolResult.isError); details/structured content are preserved.
	isError = res.IsError != nil && *res.IsError
	return res, nil, isError, durationMs
}

// finalizeToolCall applies the afterToolCall hook (field-level override, no deep
// merge), emits the tool_execution_end event, and builds the ToolResultMessage.
// durationMs is nil when execute() never ran. applyHooks is false for calls that
// short-circuited before execution (pi only runs afterToolCall on executed
// calls). It returns the message and whether this result requests termination.
func finalizeToolCall(ctx context.Context, cfg ToolExecutorConfig, call agentcore.AgentToolCall, result agentcore.AgentToolResult, isError bool, durationMs *int64, emit agentcore.EmitFunc, applyHooks bool) (agentcore.ToolResultMessage, bool) {
	if applyHooks && cfg.AfterToolCall != nil {
		result, isError = applyAfterToolCall(ctx, cfg.AfterToolCall, call, result, isError)
	}

	// Result-shaping seam: every tool's output funnels through here before it
	// becomes a ToolResultMessage, so this is the single point where the
	// executor-layer byte budget is enforced uniformly for ALL tools.
	result.Content = clipToolResultContent(result.Content, cfg.MaxResultBytes)

	if emit != nil {
		_ = emit(ctx, agentcore.ToolExecutionEndEvent{ToolCallID: call.ID, ToolName: call.Name, Result: result, IsError: isError, DurationMs: durationMs})
	}

	terminate := result.Terminate != nil && *result.Terminate
	return agentcore.ToolResultMessage{
		RoleField:  agentcore.RoleToolResult,
		ToolCallID: call.ID,
		ToolName:   call.Name,
		Content:    result.Content,
		Details:    result.Details,
		IsError:    isError,
		DurationMs: durationMs,
	}, terminate
}

// applyAfterToolCall runs the afterToolCall hook and applies its field-level
// overrides. Structured content is dropped when Content is replaced without a
// replacement StructuredContent, because it may no longer match the content
// (pi's AfterToolCallResult contract).
func applyAfterToolCall(ctx context.Context, hook agentcore.AfterToolCallFunc, call agentcore.AgentToolCall, result agentcore.AgentToolResult, isError bool) (agentcore.AgentToolResult, bool) {
	ov := hook(ctx, call, result, isError)
	if ov == nil {
		return result, isError
	}
	if ov.Content != nil {
		result.Content = *ov.Content
		if ov.StructuredContent == nil {
			result.StructuredContent = nil
		}
	}
	if ov.Details != nil {
		result.Details = *ov.Details
	}
	if ov.StructuredContent != nil {
		result.StructuredContent = *ov.StructuredContent
	}
	if ov.Terminate != nil {
		result.Terminate = ov.Terminate
	}
	if ov.IsError != nil {
		isError = *ov.IsError
	}
	return result, isError
}

// errorResult builds an error AgentToolResult carrying a single text block.
func errorResult(msg string) agentcore.AgentToolResult {
	return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent(msg)}}
}

// clipToolResultContent enforces the executor-layer byte budget on a tool
// result's text, uniformly for every tool. budget<=0 with the sentinel meaning:
// 0 => toolResultMaxBytes default, <0 => disabled. Non-text blocks (e.g. images)
// pass through untouched and keep their order; the combined text of all text
// blocks is measured against the budget and, when over, collapsed into a single
// truncated text block via truncateToBudget (head + "[truncated N bytes]" +
// tail, matching the bash idiom). Per-tool inner caps have already run, so this
// only bites when a tool's own cap is looser or absent.
func clipToolResultContent(content agentcore.ContentList, cfgMax int) agentcore.ContentList {
	budget := cfgMax
	if budget == 0 {
		budget = toolResultMaxBytes
	}
	if budget < 0 {
		return content
	}

	total := 0
	textBlocks := 0
	for _, c := range content {
		if t, ok := c.(agentcore.TextContent); ok {
			total += len(t.Text)
			textBlocks++
		}
	}
	if textBlocks == 0 || total <= budget {
		return content
	}

	// Over budget: gather all text (in order) and non-text blocks separately,
	// then emit the non-text blocks followed by one truncated text block.
	var sb strings.Builder
	out := make(agentcore.ContentList, 0, len(content))
	for _, c := range content {
		if t, ok := c.(agentcore.TextContent); ok {
			sb.WriteString(t.Text)
			continue
		}
		out = append(out, c)
	}
	out = append(out, agentcore.NewTextContent(truncateToBudget(sb.String(), budget)))
	return out
}

// truncateToBudget caps s at budget bytes. When s is longer it keeps a head and
// a tail preview (split evenly) joined by a "[truncated N bytes]" marker, so
// both the start and the end of the text survive. Cut points are pulled back to
// UTF-8 rune boundaries so no partial rune is emitted; N counts the raw bytes
// dropped from the middle. This is the single shared truncation idiom reused by
// both the bash tool's inner cap and the executor-layer budget.
func truncateToBudget(s string, budget int) string {
	if budget <= 0 || len(s) <= budget {
		return s
	}
	half := budget / 2
	head := trimUTF8Prefix(s[:half])
	tail := trimUTF8Suffix(s[len(s)-half:])
	removed := len(s) - len(head) - len(tail)
	return head + fmt.Sprintf("\n[truncated %d bytes]\n", removed) + tail
}

// decodeArgs unmarshals a tool's JSON arguments into T. On failure it returns an
// error result already shaped as "<tool>: invalid arguments: ...", so a tool's
// Execute can decode and bail in one line:
//
//	a, bad := decodeArgs[readToolArgs](args, "read")
//	if bad != nil {
//		return *bad, nil
//	}
//
// The ok flag distinguishes the failure case without comparing the zero value.
func decodeArgs[T any](args json.RawMessage, tool string) (T, *agentcore.AgentToolResult) {
	var a T
	if err := json.Unmarshal(args, &a); err != nil {
		res := errorResult(fmt.Sprintf("%s: invalid arguments: %v", tool, err))
		return a, &res
	}
	return a, nil
}

// errorToolResult builds an error ToolResultMessage directly (used when a call
// is aborted outside the normal finalize path).
func errorToolResult(call agentcore.AgentToolCall, msg string) agentcore.ToolResultMessage {
	return agentcore.ToolResultMessage{
		RoleField:  agentcore.RoleToolResult,
		ToolCallID: call.ID,
		ToolName:   call.Name,
		Content:    agentcore.ContentList{agentcore.NewTextContent(msg)},
		IsError:    true,
	}
}
