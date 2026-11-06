// Copied from github.com/smallnest/pigo@891d1f3 (MIT License).
// Upstream: internal/agentcore/event.go
// Modified for keel: module import paths rewritten; see docs/UPSTREAM.md.

package agentcore

// AgentEvent is the sealed interface implemented by every event the loop emits.
// Consumers dispatch with a type switch, consistent with Content. pigo covers
// all 10 of pi's event types (PRD FR-24).
type AgentEvent interface {
	isAgentEvent()
	// EventType returns the discriminant string, useful for logging and for
	// serialising events to the stream-json/stdio protocol (US-020).
	EventType() string
}

// Event type discriminants.
const (
	EventAgentStart          = "agent_start"
	EventAgentEnd            = "agent_end"
	EventAgentBeforeSettle   = "agent_before_settle"
	EventAgentSettled        = "agent_settled"
	EventTurnStart           = "turn_start"
	EventTurnEnd             = "turn_end"
	EventMessageStart        = "message_start"
	EventMessageUpdate       = "message_update"
	EventMessageEnd          = "message_end"
	EventToolExecutionStart  = "tool_execution_start"
	EventToolExecutionUpdate = "tool_execution_update"
	EventToolExecutionEnd    = "tool_execution_end"
	EventCompaction          = "compaction"
	EventCompactionStart     = "compaction_start"
	EventTelemetry           = "telemetry"
	EventSubAgentProgress    = "subagent_progress"
)

// AgentStartEvent is emitted once when a loop run begins. SessionID, when set,
// is the id of the session backing this run; it is carried in the first
// stream-json event so a caller can associate output with a session and resume
// it later (mirrors pi/Claude Code, which put a session id in the first event).
type AgentStartEvent struct {
	SessionID string
}

// AgentEndEvent is emitted once when a loop run ends, carrying the messages
// newly produced during this run (the EventStream result).
//
// agent_end is the terminal event of pi's core AgentEvent
// (packages/agent/src/types.ts). The host layer that mirrors pi's coding-agent
// session (docs/extensions.md, "agent_start / agent_end / agent_before_settle /
// agent_settled") may additionally emit agent_before_settle / agent_settled
// after it; those two are host-level extensions, not kernel events, and are
// opt-in in the loop (RunConfig.EmitSettleEvents).
type AgentEndEvent struct {
	Messages []AgentMessage
}

// Outcomes carried by AgentBeforeSettleEvent.Outcome.
const (
	SettleOutcomeCompleted = "completed"
	SettleOutcomeError     = "error"
	SettleOutcomeAborted   = "aborted"
)

// AgentBeforeSettleEvent is the final actionable settle boundary of the host
// layer (pi's coding-agent session/extension event, not part of the core
// AgentEvent set): it is emitted after agent_end when the run is about to settle
// and the host opted in. A boundary handler may still request one more turn
// (for example retry recovery or queued work), which starts another low-level
// run. An aborted run skips this event, going straight from agent_end to
// agent_settled.
type AgentBeforeSettleEvent struct {
	// Outcome reports how the low-level run ended: "completed", "error", or
	// "aborted".
	Outcome string
}

// AgentSettledEvent is the final, notification-only event of a run: the agent is
// idle and will not continue on its own. Aborted distinguishes a cancelled run
// from a finished one.
type AgentSettledEvent struct {
	Aborted bool
}

// TurnStartEvent marks the start of a turn (a single assistant response cycle).
type TurnStartEvent struct{}

// TurnEndEvent marks the end of a turn, with the assistant message and any tool
// results produced during it.
type TurnEndEvent struct {
	Message     AssistantMessage
	ToolResults []ToolResultMessage
}

// MessageStartEvent is emitted when a message begins streaming.
type MessageStartEvent struct {
	Message AgentMessage
}

// MessageUpdateEvent is emitted for each streaming delta, carrying the current
// partial message and the raw provider-level event that produced it.
type MessageUpdateEvent struct {
	Message               AgentMessage
	AssistantMessageEvent any
}

// MessageEndEvent is emitted when a message finishes streaming.
type MessageEndEvent struct {
	Message AgentMessage
}

// ToolExecutionStartEvent is emitted before a tool runs.
type ToolExecutionStartEvent struct {
	ToolCallID string
	ToolName   string
	Args       any
}

// ToolExecutionUpdateEvent carries a partial result during tool execution.
type ToolExecutionUpdateEvent struct {
	ToolCallID    string
	ToolName      string
	PartialResult AgentToolResult
}

// ToolExecutionEndEvent is emitted when a tool finishes.
type ToolExecutionEndEvent struct {
	ToolCallID string
	ToolName   string
	Result     AgentToolResult
	IsError    bool
	// DurationMs is how long execute() took, measured with a monotonic clock.
	// nil when the tool did not run (unknown tool, invalid arguments, a blocked
	// or aborted call); hook time (beforeToolCall / afterToolCall) is excluded,
	// matching pi's AgentToolCallOutcome.durationMs.
	DurationMs *int64
}

// CompactionEvent is emitted when the loop compacts the context window, either
// automatically (threshold/overflow) or on an explicit /compact request. It
// carries before/after token counts and how many messages were summarized vs.
// retained. When compaction fails it is still emitted with ErrorMessage set and
// the token/count fields describing the unchanged context, so consumers can
// surface the failure without the session aborting (US-004).
type CompactionEvent struct {
	// Reason is why compaction ran: "manual", "threshold", or "overflow".
	Reason string
	// TokensBefore is the estimated context tokens prior to compaction.
	TokensBefore int
	// TokensAfter is the estimated context tokens after compaction (equals
	// TokensBefore when compaction failed or was a no-op).
	TokensAfter int
	// SummarizedCount is the number of messages folded into the summary.
	SummarizedCount int
	// KeptCount is the number of recent messages retained verbatim.
	KeptCount int
	// ErrorMessage is non-empty when compaction failed; the original context is
	// preserved in that case.
	ErrorMessage string
}

// CompactionStartEvent is emitted immediately before the loop runs compaction,
// so a front-end can show an in-progress "Compacting conversation…" indicator
// while the summarization request is in flight. The matching CompactionEvent is
// emitted when it completes (or fails). Reason mirrors CompactionEvent.Reason.
type CompactionStartEvent struct {
	// Reason is why compaction is running: "manual", "threshold", or "overflow".
	Reason string
	// TokensBefore is the estimated context tokens that triggered compaction.
	TokensBefore int
}

// SubAgentProgressEvent carries structured progress from a running sub-agent
// (dispatched by the task tool). It is reported at the sub-agent's tool
// execution / turn boundaries so a TUI (multi-line status panel) or headless
// mode (stderr line) can display live progress. Elapsed time is intentionally
// omitted: consumers compute it themselves (TUI from tool-start time, headless
// from when the id was first seen) to avoid emitting an event per frame.
type SubAgentProgressEvent struct {
	// ToolCallID is the parent task call's tool-call id, used as the key for
	// the status line.
	ToolCallID string
	// Description is the task call's description, for display (may be empty).
	Description string
	// Activity is the current activity: tool name / phase, e.g. "Editing",
	// "Running bash", "Thinking".
	Activity string
	// Tokens is the estimated sub-agent output token count (0 = unknown).
	Tokens int
}

// ToolTiming records how long one tool invocation took, keyed by tool name in
// TelemetryEvent.ToolDurationsMs. It aggregates repeated calls of the same tool
// so a summary stays compact regardless of turn count.
type ToolTiming struct {
	// Count is how many times the tool was invoked over the run.
	Count int
	// TotalMs is the summed wall-clock duration of every invocation, in
	// milliseconds.
	TotalMs int64
}

// TelemetryEvent is a lightweight, additive observability summary emitted once
// at run end (just before agent_end) so scripts consuming the stream-json
// output can read structured metrics without a new dependency (no
// Prometheus/OTLP). It is purely observational: consumers that ignore it behave
// exactly as before. Metrics covered (observability — structured telemetry collection):
//   - per-tool wall-clock durations (ToolDurationsMs, aggregated by tool name),
//   - how many turns ran (Turns),
//   - how many assistant responses were truncated by the output cap
//     (TruncationCount),
//   - how many times the context was compacted (CompactionCount),
//   - the latest context-utilization ratio (ContextUtilization = used tokens /
//     ContextWindow) and the raw numbers behind it.
type TelemetryEvent struct {
	// Turns is the number of turns (turn_start events) the run executed.
	Turns int
	// ToolDurationsMs maps a tool name to its aggregated timing over the run.
	ToolDurationsMs map[string]ToolTiming
	// TruncationCount is how many assistant responses stopped with reason
	// "length" (truncated by the output token cap), each triggering a resend.
	TruncationCount int
	// CompactionCount is how many successful context compactions occurred.
	CompactionCount int
	// ContextUtilization is the latest used/window ratio in [0,1], or 0 when the
	// context window is unknown. Computed as ContextTokens / ContextWindow.
	ContextUtilization float64
	// ContextTokens is the most recently observed estimated context-token usage.
	ContextTokens int
	// ContextWindow is the model's total context-token budget (0 when unknown).
	ContextWindow int
}

func (AgentStartEvent) isAgentEvent()          {}
func (AgentEndEvent) isAgentEvent()            {}
func (AgentBeforeSettleEvent) isAgentEvent()   {}
func (AgentSettledEvent) isAgentEvent()        {}
func (TurnStartEvent) isAgentEvent()           {}
func (TurnEndEvent) isAgentEvent()             {}
func (MessageStartEvent) isAgentEvent()        {}
func (MessageUpdateEvent) isAgentEvent()       {}
func (MessageEndEvent) isAgentEvent()          {}
func (ToolExecutionStartEvent) isAgentEvent()  {}
func (ToolExecutionUpdateEvent) isAgentEvent() {}
func (ToolExecutionEndEvent) isAgentEvent()    {}
func (CompactionEvent) isAgentEvent()          {}
func (CompactionStartEvent) isAgentEvent()     {}
func (TelemetryEvent) isAgentEvent()           {}
func (SubAgentProgressEvent) isAgentEvent()    {}

func (AgentStartEvent) EventType() string          { return EventAgentStart }
func (AgentEndEvent) EventType() string            { return EventAgentEnd }
func (AgentBeforeSettleEvent) EventType() string   { return EventAgentBeforeSettle }
func (AgentSettledEvent) EventType() string        { return EventAgentSettled }
func (TurnStartEvent) EventType() string           { return EventTurnStart }
func (TurnEndEvent) EventType() string             { return EventTurnEnd }
func (MessageStartEvent) EventType() string        { return EventMessageStart }
func (MessageUpdateEvent) EventType() string       { return EventMessageUpdate }
func (MessageEndEvent) EventType() string          { return EventMessageEnd }
func (ToolExecutionStartEvent) EventType() string  { return EventToolExecutionStart }
func (ToolExecutionUpdateEvent) EventType() string { return EventToolExecutionUpdate }
func (ToolExecutionEndEvent) EventType() string    { return EventToolExecutionEnd }
func (CompactionEvent) EventType() string          { return EventCompaction }
func (CompactionStartEvent) EventType() string     { return EventCompactionStart }
func (TelemetryEvent) EventType() string           { return EventTelemetry }
func (SubAgentProgressEvent) EventType() string    { return EventSubAgentProgress }
