// Copied from github.com/smallnest/pigo@891d1f3 (MIT License).
// Upstream: internal/agentcore/tool.go
// Modified for keel: module import paths rewritten; see docs/UPSTREAM.md.

package agentcore

import (
	"context"
	"encoding/json"
)

// AgentContext is the input state for a loop run: system prompt, conversation
// messages, and the tools available to the model.
type AgentContext struct {
	SystemPrompt string      `json:"systemPrompt"`
	Messages     MessageList `json:"messages"`
	Tools        []AgentTool `json:"-"`
}

// ToolExecutionMode selects how a tool is executed relative to others in a batch.
type ToolExecutionMode string

const (
	// ToolExecutionParallel allows the tool to run concurrently with others.
	ToolExecutionParallel ToolExecutionMode = "parallel"
	// ToolExecutionSequential forces the whole batch to run serially.
	ToolExecutionSequential ToolExecutionMode = "sequential"
)

// ToolUpdateFunc receives a partial result during tool execution; the loop
// turns each call into a tool_execution_update event.
type ToolUpdateFunc func(partial AgentToolResult)

// AgentTool is a tool the model can invoke. Schema is the JSON Schema used to
// validate arguments before execution (US-014).
type AgentTool interface {
	Name() string
	Description() string
	// Schema returns the JSON Schema (as raw JSON) for the tool's arguments.
	Schema() json.RawMessage
	// ExecutionMode reports whether this tool forces sequential execution.
	ExecutionMode() ToolExecutionMode
	// Execute runs the tool. onUpdate may be nil.
	Execute(ctx context.Context, id string, args json.RawMessage, onUpdate ToolUpdateFunc) (AgentToolResult, error)
}

// AgentToolCall is a decoded request to invoke a tool (the loop-level view of a
// ToolCallContent block).
type AgentToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// AgentToolResult is the outcome of executing a tool.
//
// Details uses `any` in the first version (matching pi's internal
// AgentToolResult<any>); a generic form can be added later. Terminate is a
// *bool so "not set" is distinguishable from an explicit false — the loop only
// signals early termination when every result in a batch has Terminate=true.
type AgentToolResult struct {
	Content ContentList `json:"content"`
	Details any         `json:"details,omitempty"`
	// StructuredContent is the machine-readable result matching a tool's output
	// schema. It is not sent to the model; Content remains the model-facing
	// result (pi's AgentToolResult.structuredContent).
	StructuredContent any `json:"structuredContent,omitempty"`
	// IsError reports a failure without throwing: the model sees Content as an
	// error result, but Details/StructuredContent are kept for the UI and
	// programmatic callers (pi's AgentToolResult.isError).
	IsError   *bool `json:"isError,omitempty"`
	Terminate *bool `json:"terminate,omitempty"`
}

// AgentToolCallOutcome is the final outcome of one tool call after the
// prepare/execute/finalize phases and hooks ran (pi's AgentToolCallOutcome).
type AgentToolCallOutcome struct {
	ToolCall AgentToolCall
	Result   AgentToolResult
	IsError  bool
	// DurationMs is how long execute() took, measured with a monotonic clock.
	// nil when the tool did not run (unknown tool, invalid arguments, blocked or
	// aborted call); hook time is excluded.
	DurationMs *int64
}
