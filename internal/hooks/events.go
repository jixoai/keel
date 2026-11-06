// Package hooks implements the PI extension/hook host model for Keel.
//
// Event names and payload shapes follow PI's coding-agent extension model
// verbatim (packages/coding-agent/docs/extensions.md and
// packages/coding-agent/src/core/extensions/types.ts). The host owns the
// session; this package only dispatches hooks, applies their decisions, and
// emits audit records for the caller to persist.
//
// Two hook implementations share one registry:
//
//   - in-process Go hooks (Handler), registered with On/Add;
//   - command hooks, which run a child process with a JSON stdin/stdout
//     protocol (see command.go), loaded from configuration with
//     LoadCommandHooks.
//
// Decisions use PI's vocabulary: allow, deny, modify, continue. A deny on
// tool_call prevents execution; a continue on turn_end / agent_before_settle
// asks the host for exactly one more provider request. Observers are invoked in
// registration order but can neither gate nor fail the run. Async hooks (only
// valid for notification events) run in their own goroutine and do not delay
// the caller. Every hook execution produces an AuditRecord through the
// configured sink; this package never writes storage itself and audit records
// never enter the model context. The host persists a record as
// EventHookExecuted:
//
//	registry.SetAuditSink(hooks.AuditFunc(func(record hooks.AuditRecord) {
//		session.Append(hooks.EventHookExecuted, record)
//	}))
//
// Keel keeps the system prompt outside agentcore.AgentContext.Messages, so PI's
// "strip system messages and restore prompt/tool state" step for the context
// event is unnecessary: context handlers only ever see the conversation and
// cannot drop the prompt or tool declarations.
package hooks

import (
	"encoding/json"

	"github.com/jixoai/keel/internal/agentcore"
)

// Event type discriminants. These strings are PI's extension event names and
// must not be renamed: sessions, command hooks, and audit records all key on
// them.
const (
	EventBeforeAgentStart     = "before_agent_start"
	EventAgentStart           = "agent_start"
	EventAgentEnd             = "agent_end"
	EventAgentBeforeSettle    = "agent_before_settle"
	EventAgentSettled         = "agent_settled"
	EventTurnStart            = "turn_start"
	EventTurnEnd              = "turn_end"
	EventMessageStart         = "message_start"
	EventMessageUpdate        = "message_update"
	EventMessageEnd           = "message_end"
	EventToolExecutionStart   = "tool_execution_start"
	EventToolExecutionUpdate  = "tool_execution_update"
	EventToolExecutionEnd     = "tool_execution_end"
	EventContext              = "context"
	EventSessionBeforeCompact = "session_before_compact"
	EventSessionCompact       = "session_compact"
	EventSessionCompactFailed = "session_compact_failed"

	// EventToolCall and EventToolResult are the two gate/transform events.
	// They are dispatched by EmitToolCall / EmitToolResult and are not part of
	// PI's notification event list (they carry decisions).
	EventToolCall   = "tool_call"
	EventToolResult = "tool_result"

	// EventHookExecuted is the audit event name the host persists when it
	// consumes AuditRecord values. It is never dispatched to hooks and never
	// enters the model context.
	EventHookExecuted = "hook.executed"
)

// Event is implemented by every hook event payload.
type Event interface {
	EventType() string
}

// KnownEvent reports whether name is one of the PI extension event names this
// package dispatches (including tool_call/tool_result).
func KnownEvent(name string) bool {
	switch name {
	case EventBeforeAgentStart,
		EventAgentStart,
		EventAgentEnd,
		EventAgentBeforeSettle,
		EventAgentSettled,
		EventTurnStart,
		EventTurnEnd,
		EventMessageStart,
		EventMessageUpdate,
		EventMessageEnd,
		EventToolExecutionStart,
		EventToolExecutionUpdate,
		EventToolExecutionEnd,
		EventContext,
		EventSessionBeforeCompact,
		EventSessionCompact,
		EventSessionCompactFailed,
		EventToolCall,
		EventToolResult:
		return true
	default:
		return false
	}
}

// IsNotificationEvent reports whether name is a notification-only event. Such
// events ignore handler decisions and are the only ones that may be registered
// async.
func IsNotificationEvent(name string) bool {
	switch name {
	case EventAgentStart,
		EventAgentEnd,
		EventAgentSettled,
		EventTurnStart,
		EventMessageStart,
		EventMessageUpdate,
		EventToolExecutionStart,
		EventToolExecutionUpdate,
		EventToolExecutionEnd,
		EventSessionCompact,
		EventSessionCompactFailed:
		return true
	default:
		return false
	}
}

// MessageType values for BeforeAgentStartEvent, matching PI.
const (
	MessageTypeUser   = "user"
	MessageTypeCustom = "custom"
)

// Compaction reasons, matching PI.
const (
	CompactReasonManual    = "manual"
	CompactReasonThreshold = "threshold"
	CompactReasonOverflow  = "overflow"
)

// BoundaryDraftType values for BoundaryDraft, matching PI's
// SessionBoundaryDraft union.
const (
	BoundaryDraftCustom        = "custom"
	BoundaryDraftCustomMessage = "custom_message"
	BoundaryDraftContextEdit   = "context_edit"
	BoundaryDraftCompaction    = "compaction"
)

// GuidanceMessage is a message a before_agent_start hook injects into the run.
// It maps to PI's BeforeAgentStartEventResult.message (a durable custom message
// stored in the session and sent to the model).
type GuidanceMessage struct {
	CustomType string          `json:"customType"`
	Content    string          `json:"content"`
	Display    bool            `json:"display"`
	Details    json.RawMessage `json:"details,omitempty"`
}

// BeforeAgentStartEvent is fired before a run starts, after input handling and
// expansion. SystemPrompt is chained: each handler observes the prompt produced
// by earlier handlers, and a handler may replace it for the run. Returning a
// Guidance message injects it into the run (PI: result.message).
type BeforeAgentStartEvent struct {
	// MessageType is MessageTypeUser or MessageTypeCustom.
	MessageType string `json:"messageType"`
	// Prompt is the raw user prompt (after expansion) for MessageTypeUser.
	Prompt string `json:"prompt,omitempty"`
	// Images are attached to the user prompt, if any.
	Images []agentcore.ImageContent `json:"images,omitempty"`
	// Message is the custom message that starts the run for MessageTypeCustom.
	Message json.RawMessage `json:"message,omitempty"`
	// SystemPrompt is the current chained system prompt for this handler.
	SystemPrompt string `json:"systemPrompt"`
	// Options is host-defined system-prompt construction data (PI's
	// systemPromptOptions), available to hooks for inspection/mutation.
	Options map[string]any `json:"systemPromptOptions,omitempty"`
}

func (*BeforeAgentStartEvent) EventType() string { return EventBeforeAgentStart }

// BeforeAgentStartResult is the combined result of every before_agent_start
// handler.
type BeforeAgentStartResult struct {
	// SystemPrompt is the final chained system prompt for the run.
	SystemPrompt string
	// Guidance are the messages handlers injected, in handler order.
	Guidance []GuidanceMessage
}

// AgentStartEvent is fired when a low-level agent run begins.
type AgentStartEvent struct{}

func (*AgentStartEvent) EventType() string { return EventAgentStart }

// AgentEndEvent is fired when a low-level agent run ends. The host may still
// retry, compact, or continue with queued messages afterwards.
type AgentEndEvent struct {
	Messages []agentcore.Message `json:"messages"`
}

func (*AgentEndEvent) EventType() string { return EventAgentEnd }

// AgentSettledEvent is fired after a run has fully settled and no automatic
// retry, compaction, or queued continuation will run. Notification-only.
type AgentSettledEvent struct {
	Aborted bool `json:"aborted"`
}

func (*AgentSettledEvent) EventType() string { return EventAgentSettled }

// BoundaryContext is the host-projected context a boundary handler sees. It is
// rebuilt from the current draft entries after every handler, so each handler
// observes the proposals made so far.
type BoundaryContext struct {
	// ContextEntries are the projected session entries (host-defined shape).
	ContextEntries []json.RawMessage `json:"contextEntries"`
	// ContextMessages are the messages of the active context.
	ContextMessages []agentcore.Message `json:"contextMessages"`
	// LLMMessages are the messages that would be sent to the provider.
	LLMMessages []agentcore.Message `json:"llmMessages"`
	// PendingMessages are queued messages not yet part of the context.
	PendingMessages []agentcore.Message `json:"pendingMessages"`
	// CanContinue reports whether the host can run one more provider request.
	CanContinue bool `json:"canContinue"`
}

// BoundaryDraft is one session entry a boundary handler proposes. It mirrors
// PI's SessionBoundaryDraft union; exactly the fields of the selected Type are
// meaningful.
type BoundaryDraft struct {
	Type       string          `json:"type"`
	CustomType string          `json:"customType,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
	Content    string          `json:"content,omitempty"`
	Display    bool            `json:"display,omitempty"`
	Details    json.RawMessage `json:"details,omitempty"`
	TargetID   string          `json:"targetId,omitempty"`
	// Replacement is the replacement content for a context_edit draft; the
	// literal null (cleared/replaced with nothing) is represented by
	// ReplacementSet=true with empty Replacement.
	Replacement    json.RawMessage `json:"replacement,omitempty"`
	ReplacementSet bool            `json:"-"`
	Summary        string          `json:"summary,omitempty"`
	// FirstKeptEntryID is nil for a self-retaining compaction that keeps no
	// preceding entries.
	FirstKeptEntryID *string         `json:"firstKeptEntryId,omitempty"`
	Usage            json.RawMessage `json:"usage,omitempty"`
}

// BoundaryState is the actionable boundary shared by turn_end and
// agent_before_settle. Handlers see the proposals so far and may replace
// Entries and request one continuation.
type BoundaryState struct {
	Entries  []BoundaryDraft `json:"entries"`
	Continue bool            `json:"continue"`
	Context  BoundaryContext `json:"context"`
	// Outcome is "completed", "aborted", or "error".
	Outcome string `json:"outcome"`
}

// TurnEndEvent is fired after the assistant and tool-result messages of a turn
// were persisted. It is an actionable boundary (PI: TurnEndEvent extends
// BoundaryState).
type TurnEndEvent struct {
	BoundaryState
	TurnIndex          int                           `json:"turnIndex"`
	Message            agentcore.Message             `json:"message"`
	ToolResults        []agentcore.ToolResultMessage `json:"toolResults"`
	MessageEntryID     string                        `json:"messageEntryId"`
	ToolResultEntryIDs []string                      `json:"toolResultEntryIds"`
}

func (*TurnEndEvent) EventType() string { return EventTurnEnd }

// AgentBeforeSettleEvent is the final actionable boundary: it may append
// entries and request one continuation before the run settles.
type AgentBeforeSettleEvent struct {
	BoundaryState
}

func (*AgentBeforeSettleEvent) EventType() string { return EventAgentBeforeSettle }

// TurnStartEvent is fired at the start of each turn.
type TurnStartEvent struct {
	TurnIndex int   `json:"turnIndex"`
	Timestamp int64 `json:"timestamp"`
}

func (*TurnStartEvent) EventType() string { return EventTurnStart }

// MessageStartEvent is fired when a message starts (user, assistant,
// toolResult, or custom).
type MessageStartEvent struct {
	Message agentcore.Message `json:"message"`
}

func (*MessageStartEvent) EventType() string { return EventMessageStart }

// MessageUpdateEvent is fired during assistant message streaming.
type MessageUpdateEvent struct {
	Message               agentcore.Message `json:"message"`
	AssistantMessageEvent any               `json:"assistantMessageEvent,omitempty"`
}

func (*MessageUpdateEvent) EventType() string { return EventMessageUpdate }

// MessageEndEvent is fired when a message ends. Returning a replacement through
// Decision.Message (ActionModify) replaces the finalized message; the
// replacement must keep the same role.
type MessageEndEvent struct {
	Message agentcore.Message `json:"message"`
}

func (*MessageEndEvent) EventType() string { return EventMessageEnd }

// ToolExecutionStartEvent is fired before a tool executes. It is emitted in
// assistant source order during preflight, before tool_call.
type ToolExecutionStartEvent struct {
	ToolCallID       string          `json:"toolCallId"`
	ToolName         string          `json:"toolName"`
	Args             json.RawMessage `json:"args"`
	ParentToolCallID string          `json:"parentToolCallId,omitempty"`
}

func (*ToolExecutionStartEvent) EventType() string { return EventToolExecutionStart }

// ToolExecutionUpdateEvent carries a partial result while a tool executes.
type ToolExecutionUpdateEvent struct {
	ToolCallID       string                    `json:"toolCallId"`
	ToolName         string                    `json:"toolName"`
	Args             json.RawMessage           `json:"args"`
	PartialResult    agentcore.AgentToolResult `json:"partialResult"`
	ParentToolCallID string                    `json:"parentToolCallId,omitempty"`
}

func (*ToolExecutionUpdateEvent) EventType() string { return EventToolExecutionUpdate }

// ToolExecutionEndEvent is fired when a tool finishes. DurationMs is nil when
// the tool did not run (unknown tool, invalid arguments, a blocked call).
type ToolExecutionEndEvent struct {
	ToolCallID       string                    `json:"toolCallId"`
	ToolName         string                    `json:"toolName"`
	Result           agentcore.AgentToolResult `json:"result"`
	IsError          bool                      `json:"isError"`
	DurationMs       *int64                    `json:"durationMs,omitempty"`
	ParentToolCallID string                    `json:"parentToolCallId,omitempty"`
}

func (*ToolExecutionEndEvent) EventType() string { return EventToolExecutionEnd }

// ToolCallEvent is fired before a tool executes and can block it. Input is
// mutable: a later handler sees mutations made by an earlier handler, and no
// re-validation is performed after mutation (PI semantics).
type ToolCallEvent struct {
	ToolCallID       string          `json:"toolCallId"`
	ToolName         string          `json:"toolName"`
	Input            json.RawMessage `json:"input"`
	ParentToolCallID string          `json:"parentToolCallId,omitempty"`
}

func (*ToolCallEvent) EventType() string { return EventToolCall }

// ToolResultEvent is fired after tool execution and before tool_execution_end.
// Handlers chain partial patches: content, details, isError, terminate.
type ToolResultEvent struct {
	ToolCallID       string                `json:"toolCallId"`
	ToolName         string                `json:"toolName"`
	ParentToolCallID string                `json:"parentToolCallId,omitempty"`
	Input            json.RawMessage       `json:"input"`
	Content          agentcore.ContentList `json:"content"`
	Details          any                   `json:"details,omitempty"`
	// DetailsSet distinguishes an explicitly provided nil Details from an
	// omitted one.
	DetailsSet bool  `json:"-"`
	IsError    bool  `json:"isError"`
	Terminate  *bool `json:"terminate,omitempty"`
}

func (*ToolResultEvent) EventType() string { return EventToolResult }

// ContextEvent is fired before each LLM call. Messages holds the conversation
// without the system prompt (which belongs to the host). Handlers may mutate
// Messages in place or return a replacement; replacements chain across
// handlers.
type ContextEvent struct {
	Messages []agentcore.Message `json:"messages"`
}

func (*ContextEvent) EventType() string { return EventContext }

// SessionBeforeCompactEvent is fired before context compaction. A handler may
// cancel (ActionDeny) or provide a custom compaction (ActionModify with
// Decision.Compaction).
type SessionBeforeCompactEvent struct {
	// Preparation is the host's compaction preparation (opaque here).
	Preparation any `json:"preparation,omitempty"`
	// BranchEntries are the active branch entries being compacted.
	BranchEntries []json.RawMessage `json:"branchEntries,omitempty"`
	// CustomInstructions is the user's /compact instruction, if any.
	CustomInstructions string `json:"customInstructions,omitempty"`
	// Reason is manual, threshold, or overflow.
	Reason string `json:"reason"`
	// WillRetry reports whether the aborted turn is retried after compaction
	// (overflow recovery).
	WillRetry bool `json:"willRetry"`
}

func (*SessionBeforeCompactEvent) EventType() string { return EventSessionBeforeCompact }

// CompactionResult is a handler-provided compaction summary.
type CompactionResult struct {
	Summary          string `json:"summary"`
	FirstKeptEntryID string `json:"firstKeptEntryId"`
	TokensBefore     int    `json:"tokensBefore,omitempty"`
	// Usage is opaque host usage data included in session totals when set.
	Usage json.RawMessage `json:"usage,omitempty"`
}

// SessionBeforeCompactResult is the combined result for
// session_before_compact.
type SessionBeforeCompactResult struct {
	// Cancel stops the compaction (the first cancel wins).
	Cancel bool
	// Compaction, when set, replaces the host-generated summary (the last
	// custom result wins).
	Compaction *CompactionResult
}

// SessionCompactEvent is fired after compaction succeeds.
type SessionCompactEvent struct {
	CompactionEntry json.RawMessage `json:"compactionEntry,omitempty"`
	FromExtension   bool            `json:"fromExtension"`
	Reason          string          `json:"reason"`
	WillRetry       bool            `json:"willRetry"`
}

func (*SessionCompactEvent) EventType() string { return EventSessionCompact }

// SessionCompactFailedEvent is fired after compaction fails or is aborted.
type SessionCompactFailedEvent struct {
	Reason        string `json:"reason"`
	ErrorMessage  string `json:"errorMessage,omitempty"`
	Aborted       bool   `json:"aborted"`
	WillRetry     bool   `json:"willRetry"`
	FromExtension bool   `json:"fromExtension"`
}

func (*SessionCompactFailedEvent) EventType() string { return EventSessionCompactFailed }
