package hooks

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
)

// Action is a hook decision. PI's vocabulary is allow / deny / modify /
// continue; the zero value is allow.
type Action string

const (
	ActionAllow    Action = "allow"
	ActionDeny     Action = "deny"
	ActionModify   Action = "modify"
	ActionContinue Action = "continue"
)

// Normalized returns the action with the zero value resolved to allow.
func (a Action) Normalized() Action {
	if a == "" {
		return ActionAllow
	}
	return a
}

// Decision is what a hook returns. Only fields meaningful for the event being
// dispatched are read; the runner documents per-event handling and validates
// them.
type Decision struct {
	Action Action
	// Reason explains a deny or is recorded in the audit trail.
	Reason string
	// Terminate marks a denied tool call as eligible for batch early
	// termination (PI: ToolCallEventResult.terminate). It matters only when
	// Action is deny.
	Terminate bool

	// Input replaces tool_call arguments when Action is modify.
	Input json.RawMessage
	// Result patches a tool result when Action is modify.
	Result *ToolResultPatch
	// Message replaces a finalized message when Action is modify
	// (message_end). The replacement must keep the same role.
	Message agentcore.Message
	// Messages replaces the conversation when Action is modify (context).
	Messages []agentcore.Message
	// SystemPrompt replaces the chained system prompt (before_agent_start).
	SystemPrompt *string
	// Guidance injects messages into the run (before_agent_start).
	Guidance []GuidanceMessage
	// Compaction provides a custom summary (session_before_compact).
	Compaction *CompactionResult
	// Entries replace the boundary proposal (turn_end / agent_before_settle).
	Entries []BoundaryDraft
	// ContinueOverride explicitly sets the boundary continuation decision.
	// Nil leaves the current decision unchanged; ActionContinue is shorthand
	// for true. A later handler can therefore suppress an earlier request with
	// a false override, matching PI.
	ContinueOverride *bool
}

// Allow returns the no-op decision.
func Allow() Decision { return Decision{Action: ActionAllow} }

// Deny blocks the current operation (tool_call, session_before_compact).
func Deny(reason string) Decision { return Decision{Action: ActionDeny, Reason: reason} }

// DenyTerminate blocks a tool call and marks it terminating. The batch stops
// early only when every finalized result in the batch terminates (PI rule).
func DenyTerminate(reason string) Decision {
	return Decision{Action: ActionDeny, Reason: reason, Terminate: true}
}

// ModifyInput rewrites tool_call arguments. No re-validation is performed.
func ModifyInput(input json.RawMessage) Decision {
	return Decision{Action: ActionModify, Input: input}
}

// ModifyResult patches a tool result.
func ModifyResult(patch ToolResultPatch) Decision {
	return Decision{Action: ActionModify, Result: &patch}
}

// ReplaceMessage replaces a finalized message (message_end).
func ReplaceMessage(message agentcore.Message) Decision {
	return Decision{Action: ActionModify, Message: message}
}

// ReplaceMessages rewrites the context (context).
func ReplaceMessages(messages []agentcore.Message) Decision {
	return Decision{Action: ActionModify, Messages: messages}
}

// SetSystemPrompt replaces the chained system prompt (before_agent_start).
func SetSystemPrompt(prompt string) Decision {
	return Decision{Action: ActionModify, SystemPrompt: &prompt}
}

// InjectGuidance injects messages into the run (before_agent_start).
func InjectGuidance(messages ...GuidanceMessage) Decision {
	return Decision{Action: ActionModify, Guidance: messages}
}

// ContinueBoundary proposes entries and requests one continuation
// (turn_end / agent_before_settle).
func ContinueBoundary(entries ...BoundaryDraft) Decision {
	yes := true
	return Decision{Action: ActionContinue, Entries: entries, ContinueOverride: &yes}
}

// SetContinue explicitly sets the boundary continuation decision. It lets a
// later handler suppress an earlier continuation request.
func SetContinue(continuation bool) Decision {
	return Decision{Action: ActionModify, ContinueOverride: &continuation}
}

// CustomCompaction provides a handler-generated compaction summary
// (session_before_compact).
func CustomCompaction(result CompactionResult) Decision {
	return Decision{Action: ActionModify, Compaction: &result}
}

// ToolResultPatch is a partial tool-result update. Nil fields keep the current
// value, matching PI's field-level replacement (no deep merge). DetailsSet
// distinguishes an explicitly provided nil Details from an omitted field.
type ToolResultPatch struct {
	Content    *agentcore.ContentList `json:"content,omitempty"`
	Details    any                    `json:"details,omitempty"`
	DetailsSet bool                   `json:"-"`
	IsError    *bool                  `json:"isError,omitempty"`
	Terminate  *bool                  `json:"terminate,omitempty"`
}

// ErrorPolicy decides what a handler error does to the run.
type ErrorPolicy string

const (
	// ErrorPolicyFailOpen records the error and continues. This is the default
	// for notification and transform events (PI: extension errors are logged,
	// the agent continues).
	ErrorPolicyFailOpen ErrorPolicy = "fail_open"
	// ErrorPolicyFailClosed turns the error into a deny. This is the default
	// for tool_call: PI documents "tool_call errors block the tool (fail-safe)".
	ErrorPolicyFailClosed ErrorPolicy = "fail_closed"
	// ErrorPolicyIgnore continues without reporting the error to listeners.
	ErrorPolicyIgnore ErrorPolicy = "ignore"
)

// DefaultErrorPolicy returns the policy applied to a handler that did not set
// one. Only tool_call fails closed; every other event logs and continues.
func DefaultErrorPolicy(event string) ErrorPolicy {
	if event == EventToolCall {
		return ErrorPolicyFailClosed
	}
	return ErrorPolicyFailOpen
}

// AuditRecord is one hook execution. The host persists it as the
// hook.executed event; it never enters the model context. Duration covers only
// the hook invocation (command hooks include process spawn and I/O).
type AuditRecord struct {
	Hook      string        `json:"hook"`
	Event     string        `json:"event"`
	Duration  time.Duration `json:"duration"`
	Decision  Action        `json:"decision"`
	Reason    string        `json:"reason,omitempty"`
	Error     string        `json:"error,omitempty"`
	Observer  bool          `json:"observer,omitempty"`
	Timestamp time.Time     `json:"timestamp"`
}

// AuditSink receives one record per hook execution. Calls are serialized by
// the registry's audit lock, so the sink does not need its own synchronization;
// it must not call back into the registry that is recording (that would
// deadlock). Tool batches may dispatch in parallel, but audit records are
// delivered one at a time.
type AuditSink interface {
	Record(AuditRecord)
}

// AuditFunc adapts a function to AuditSink.
type AuditFunc func(AuditRecord)

// Record implements AuditSink.
func (f AuditFunc) Record(record AuditRecord) { f(record) }

// HookError reports a hook failure to the host (ExtensionError equivalent).
type HookError struct {
	Hook  string
	Event string
	Err   error
	// Stderr is the command hook's stderr, when any.
	Stderr string
}

func (e HookError) Error() string {
	if e.Hook == "" {
		return fmt.Sprintf("%s hook failed: %v", e.Event, e.Err)
	}
	return fmt.Sprintf("%s hook %q failed: %v", e.Event, e.Hook, e.Err)
}

func (e HookError) Unwrap() error { return e.Err }
