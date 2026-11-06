// Package durable reproduces the core persistent-execution semantics of the
// official PI harness (packages/durable/src/harness) in Go:
//
//   - durable tasks with pending/running/waiting/sleeping/completing/terminal
//     states, checkpoints, attempts, restart limits and outcomes;
//   - submissions (input/write/continue) with queued/placed/done/unanswered
//     states, request-id deduplication, an inbox, cancellation and failed-run
//     redelivery policies;
//   - generation idempotency: a generation attempt is named by task id +
//     attempt, a committed answer is reused, and a crash counts as a failed
//     attempt under the retry policy;
//   - tool tasks with beforeTool/afterTool hooks (allow/deny/modify/terminate),
//     replay policy, output bounds, interruption/abandon/restart-limit results;
//   - nested tool calls (own task, key, parent, reattachment) and structured
//     output validation.
//
// The storage layer is intentionally not PI's SQLite/JSONL implementation: the
// core runs against the small Store interface below, with an in-memory
// implementation used by the ported tests. internal/store/tasks.go is the
// PostgreSQL projection for Keel workers.
package durable

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
)

// TaskID identifies one durable task.
type TaskID string

// SubmissionID identifies one durable submission.
type SubmissionID string

// ConversationID identifies one transcript scope.
type ConversationID string

// EntryID is a per-conversation, strictly increasing transcript sequence.
type EntryID int64

// RootConversationID is the reserved id of the harness's root conversation.
const RootConversationID ConversationID = "root"

// Task status values, matching PI's TaskState.
const (
	TaskPending    = "pending"
	TaskRunning    = "running"
	TaskWaiting    = "waiting"
	TaskSleeping   = "sleeping"
	TaskCompleting = "completing"
	TaskTerminal   = "terminal"
)

// Task outcome values, matching PI's TaskOutcome.
const (
	OutcomeCompleted = "completed"
	OutcomeFailed    = "failed"
	OutcomeAborted   = "aborted"
	OutcomeOrphaned  = "orphaned"
	OutcomeFaulted   = "faulted"
)

// Submission states.
const (
	SubmissionQueued     = "queued"
	SubmissionPlaced     = "placed"
	SubmissionDone       = "done"
	SubmissionUnanswered = "unanswered"
)

// Submission types.
const (
	SubmissionInput    = "input"
	SubmissionWrite    = "write"
	SubmissionContinue = "continue"
)

// Task kinds.
const (
	GenerationTaskKind = "pi.generation"
	ToolTaskKind       = "pi.tool"
)

// Queue modes: how many queued items of one mode a boundary places.
const (
	QueueAll                = "all"
	QueueOneAtATime         = "one-at-a-time"
	ToolExecutionParallel   = "parallel"
	ToolExecutionSequential = "sequential"
	ReplaySafe              = "safe"
	ReplayUnsafe            = "unsafe"
)

// After-failed-run policies.
const (
	AfterFailedKeep     = "keep"
	AfterFailedContinue = "continue"
	AfterFailedWithdraw = "withdraw"
)

// Run failure reasons, matching pi.live.runFailure.
const (
	ReasonModelError      = "model_error"
	ReasonNoModel         = "no_model"
	ReasonAborted         = "aborted"
	ReasonReset           = "reset"
	ReasonFaulted         = "faulted"
	ReasonOrphaned        = "orphaned"
	ReasonInterrupted     = "interrupted"
	ReasonRestartLimit    = "restart_limit"
	ReasonNothingToAnswer = "nothing_to_answer"
	ReasonTurnLimit       = "turn_limit"
	ReasonRunFailed       = "run_failed"
)

// Diagnostic is a model-visible remark about a call (truncation, spill path,
// blocked call, ...); never part of the tool's data.
type Diagnostic struct {
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Code     string `json:"code,omitempty"`
}

func diag(code, message string) Diagnostic {
	return Diagnostic{Severity: "error", Code: code, Message: message}
}

// TaskError is a JSON-safe error snapshot.
type TaskError struct {
	Message string          `json:"message"`
	Detail  json.RawMessage `json:"detail,omitempty"`
}

// Outcome is a terminal task outcome.
type Outcome struct {
	Status string          `json:"status"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *TaskError      `json:"error,omitempty"`
	Reason string          `json:"reason,omitempty"`
}

// TaskState is the complete durable execution state of a task. Checkpoint is
// the task-kind-specific JSON blob; Status is one of the Task* constants.
type TaskState struct {
	Status     string          `json:"status"`
	Checkpoint json.RawMessage `json:"checkpoint,omitempty"`
	On         []TaskID        `json:"on,omitempty"`
	Policy     string          `json:"policy,omitempty"`
	Until      int64           `json:"until,omitempty"`
	Outcome    *Outcome        `json:"outcome,omitempty"`
}

// TaskRecord is the durable record of one task.
type TaskRecord struct {
	ID             TaskID          `json:"id"`
	ConversationID ConversationID  `json:"conversationId"`
	Kind           string          `json:"kind"`
	Version        int             `json:"version"`
	Input          json.RawMessage `json:"input,omitempty"`
	Owner          TaskID          `json:"owner,omitempty"`
	State          TaskState       `json:"state"`
	Restarts       int             `json:"restarts,omitempty"`
	StartedAt      int64           `json:"startedAt,omitempty"`
	EndedAt        int64           `json:"endedAt,omitempty"`
}

// StatusWord maps the PI task state to the four-word lifecycle the Keel task
// spec uses: pending, running, done, failed.
func (t TaskRecord) StatusWord() string {
	switch t.State.Status {
	case TaskPending:
		return "pending"
	case TaskRunning, TaskWaiting, TaskSleeping, TaskCompleting:
		// A task that is parked is still live work; "running" is the closest
		// one-word state the four-word contract has.
		if t.State.Status == TaskCompleting {
			return "running"
		}
		return "running"
	case TaskTerminal:
		if t.State.Outcome != nil && t.State.Outcome.Status == OutcomeCompleted {
			return "done"
		}
		return "failed"
	}
	return t.State.Status
}

// Live reports whether the task can still run.
func (t TaskRecord) Live() bool {
	switch t.State.Status {
	case TaskPending, TaskRunning, TaskWaiting, TaskSleeping, TaskCompleting:
		return true
	}
	return false
}

// Terminal reports whether the task has settled.
func (t TaskRecord) Terminal() bool { return t.State.Status == TaskTerminal }

// InputEntry is the application entry an input is placed as (instead of
// pi.user). Its kind must not start with "pi.".
type InputEntry struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data,omitempty"`
}

// ContextEdit is an immutable override of one visible entry's model messages.
type ContextEdit struct {
	Target EntryID `json:"target"`
	Action string  `json:"action"` // omit | replace
}

// EntryDraft is an entry a write submission appends.
type EntryDraft struct {
	Kind  string                `json:"kind"`
	Model agentcore.MessageList `json:"model,omitempty"`
	Data  json.RawMessage       `json:"data,omitempty"`
	// Head "self" starts a new active context at this entry (a reset).
	Head  string        `json:"head,omitempty"`
	Edits []ContextEdit `json:"edits,omitempty"`
}

// DisplayOnly reports whether a draft changes nothing the model sees.
func (d EntryDraft) DisplayOnly() bool {
	return len(d.Model) == 0 && d.Head == "" && len(d.Edits) == 0
}

// IsReset reports whether the draft starts a new context.
func (d EntryDraft) IsReset() bool { return d.Head == "self" }

// EntryRecord is one committed transcript entry.
type EntryRecord struct {
	ID             EntryID               `json:"id"`
	ConversationID ConversationID        `json:"conversationId"`
	Kind           string                `json:"kind"`
	Model          agentcore.MessageList `json:"model,omitempty"`
	Data           json.RawMessage       `json:"data,omitempty"`
	Head           bool                  `json:"head,omitempty"`
	Edits          []ContextEdit         `json:"edits,omitempty"`
}

// SubmissionRecord is the durable state of one submission. Content is the
// model-facing user input (a JSON string or a content list).
type SubmissionRecord struct {
	ID             SubmissionID    `json:"id"`
	ConversationID ConversationID  `json:"conversationId"`
	Type           string          `json:"type"`
	Status         string          `json:"status"`
	RequestID      string          `json:"requestId,omitempty"`
	Content        json.RawMessage `json:"content,omitempty"`
	Entry          *InputEntry     `json:"entry,omitempty"`
	Write          *EntryDraft     `json:"write,omitempty"`
	EntryID        EntryID         `json:"entryId,omitempty"`
	AnswerID       EntryID         `json:"answerId,omitempty"`
	Reason         string          `json:"reason,omitempty"`
	Detail         string          `json:"detail,omitempty"`
	Error          string          `json:"error,omitempty"`
}

// Settled reports whether a submission reached a terminal state.
func (s SubmissionRecord) Settled() bool {
	return s.Status == SubmissionDone || s.Status == SubmissionUnanswered
}

// InboxItem is one queued submission: user input (steer/followUp) or a write.
type InboxItem struct {
	ID      SubmissionID    `json:"id"`
	Mode    string          `json:"mode"` // steer | followUp | write
	Content json.RawMessage `json:"content,omitempty"`
	Entry   *InputEntry     `json:"entry,omitempty"`
	Write   *EntryDraft     `json:"write,omitempty"`
}

// LiveRun is the run control of a busy conversation.
type LiveRun struct {
	TaskID TaskID         `json:"taskId"`
	Inputs []SubmissionID `json:"inputs"`
	Turns  int            `json:"turns"`
	First  EntryID        `json:"first,omitempty"`
}

// RunFailure is why the newest run ended without an answer.
type RunFailure struct {
	TaskID TaskID         `json:"taskId"`
	Inputs []SubmissionID `json:"inputs"`
	Reason string         `json:"reason"`
	Detail string         `json:"detail,omitempty"`
	First  EntryID        `json:"first,omitempty"`
}

// ToolSlot is the live presentation of one model-issued tool call.
type ToolSlot struct {
	CallID      string          `json:"callId"`
	Name        string          `json:"name"`
	TaskID      TaskID          `json:"taskId,omitempty"`
	Status      string          `json:"status"`
	Unoffered   bool            `json:"unoffered,omitempty"`
	Output      string          `json:"output,omitempty"`
	Details     json.RawMessage `json:"details,omitempty"`
	Diagnostics []Diagnostic    `json:"diagnostics,omitempty"`
	Entry       EntryID         `json:"entry,omitempty"`
}

// NestedToolSlot is the live presentation of one nested call.
type NestedToolSlot struct {
	CallID       string          `json:"callId"`
	ParentCallID string          `json:"parentCallId"`
	ParentTaskID TaskID          `json:"parentTaskId"`
	Name         string          `json:"name"`
	TaskID       TaskID          `json:"taskId"`
	Arguments    json.RawMessage `json:"arguments,omitempty"`
	Status       string          `json:"status"`
	Output       string          `json:"output,omitempty"`
	Details      json.RawMessage `json:"details,omitempty"`
	Diagnostics  []Diagnostic    `json:"diagnostics,omitempty"`
	Summary      *NestedSummary  `json:"summary,omitempty"`
}

// NestedSummary is how a nested call ended, for its slot.
type NestedSummary struct {
	IsError    bool             `json:"isError"`
	DurationMs *int64           `json:"durationMs,omitempty"`
	Usage      *agentcore.Usage `json:"usage,omitempty"`
	Error      string           `json:"error,omitempty"`
}

// LiveState is the per-conversation live document.
type LiveState struct {
	Run         *LiveRun         `json:"run,omitempty"`
	Generation  *GenerationLive  `json:"generation,omitempty"`
	Tools       []ToolSlot       `json:"tools,omitempty"`
	NestedTools []NestedToolSlot `json:"nestedTools,omitempty"`
	RunFailure  *RunFailure      `json:"runFailure,omitempty"`
}

// GenerationLive is the presentation of the current generation attempt.
type GenerationLive struct {
	Attempt int `json:"attempt"`
	// Retry is the durable backoff before the next attempt.
	Retry *RetryLive `json:"retry,omitempty"`
}

// RetryLive records a retry's deadline and error text.
type RetryLive struct {
	At    int64  `json:"at"`
	Error string `json:"error"`
}

// AgentState is the stored per-conversation agent configuration. A nil Model
// means the harness default; a non-nil but unavailable model fails no_model.
type AgentState struct {
	Model        *ModelRef `json:"model,omitempty"`
	Instructions string    `json:"instructions,omitempty"`
	// Tools, when non-nil, is the exact set of tool names offered to the model.
	// A nil pointer offers every registered tool.
	Tools *[]string `json:"tools,omitempty"`
}

// ModelRef names a provider/model pair.
type ModelRef struct {
	Provider string `json:"provider"`
	ModelID  string `json:"modelId"`
}

// NestedCallDoc is the task-scoped record of one nested call, keyed by its key.
type NestedCallDoc struct {
	TaskID TaskID        `json:"taskId,omitempty"`
	Result *NestedResult `json:"result,omitempty"`
}

// NestedResult is what executeTool() returns.
type NestedResult struct {
	TaskID           TaskID           `json:"taskId"`
	StructuredOutput json.RawMessage  `json:"structuredOutput,omitempty"`
	IsError          bool             `json:"isError"`
	Details          json.RawMessage  `json:"details,omitempty"`
	Diagnostics      []Diagnostic     `json:"diagnostics"`
	Usage            *agentcore.Usage `json:"usage,omitempty"`
	DurationMs       *int64           `json:"durationMs,omitempty"`
}

// cloneNestedResult returns a deep copy a tool may mutate.
func cloneNestedResult(r NestedResult) NestedResult {
	out := r
	out.StructuredOutput = cloneRaw(r.StructuredOutput)
	out.Details = cloneRaw(r.Details)
	out.Diagnostics = append([]Diagnostic(nil), r.Diagnostics...)
	return out
}

// cloneRaw copies a raw JSON blob.
func cloneRaw(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

// RetryPolicy matches PI's ConversationRetryPolicy defaults.
type RetryPolicy struct {
	Enabled         bool
	MaxRetries      int
	BaseDelayMs     int64
	MaxAgentDelayMs int64
}

// ProgressPolicy matches PI's ProgressPolicy defaults.
type ProgressPolicy struct {
	PartialIntervalMs int64
	OutputIntervalMs  int64
	PersistWithinMs   int64
}

// Settings is the durable run policy. The zero value resolves to PI defaults
// through ResolveSettings.
type Settings struct {
	Retry          *RetryPolicy
	Progress       *ProgressPolicy
	ToolExecution  string
	SteeringMode   string
	FollowUpMode   string
	AfterFailedRun string
	// RestartLimit: how many consecutive crashes may cut a task off without
	// state progress before it ends instead of running again. Default 5.
	RestartLimit int
	// MaxTurns bounds tool rounds per run; 0 means no limit.
	MaxTurns int
}

// DefaultRetryPolicy mirrors PI's DEFAULT_RETRY_POLICY.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{Enabled: true, MaxRetries: 3, BaseDelayMs: 2000, MaxAgentDelayMs: 60000}
}

// DefaultProgressPolicy mirrors PI's DEFAULT_PROGRESS_POLICY.
func DefaultProgressPolicy() ProgressPolicy {
	return ProgressPolicy{PartialIntervalMs: 100, OutputIntervalMs: 100, PersistWithinMs: 1000}
}

// ResolveSettings fills every unset field with PI's default.
func ResolveSettings(s Settings) Settings {
	if s.Retry == nil {
		r := DefaultRetryPolicy()
		s.Retry = &r
	}
	if s.Progress == nil {
		p := DefaultProgressPolicy()
		s.Progress = &p
	}
	if s.ToolExecution == "" {
		s.ToolExecution = ToolExecutionParallel
	}
	if s.SteeringMode == "" {
		s.SteeringMode = QueueOneAtATime
	}
	if s.FollowUpMode == "" {
		s.FollowUpMode = QueueOneAtATime
	}
	if s.AfterFailedRun == "" {
		s.AfterFailedRun = AfterFailedKeep
	}
	if s.RestartLimit == 0 {
		s.RestartLimit = 5
	}
	if s.RestartLimit < 0 {
		s.RestartLimit = 1 << 30 // Infinity: never end for restarts.
	}
	return s
}

// retryDelayMs mirrors PI's retryDelayMs: base * 2^(attempt-1), capped.
func retryDelayMs(p RetryPolicy, attempt int) int64 {
	delay := p.BaseDelayMs
	for i := 1; i < attempt; i++ {
		if delay > (1<<62)/2 {
			delay = 1 << 62
			break
		}
		delay *= 2
	}
	if p.MaxAgentDelayMs > 0 && delay > p.MaxAgentDelayMs {
		delay = p.MaxAgentDelayMs
	}
	return delay
}

// ConversationBusy is returned when a submission needs an idle conversation.
type ConversationBusy struct{ ConversationID ConversationID }

func (e *ConversationBusy) Error() string {
	return fmt.Sprintf("conversation %s is busy", e.ConversationID)
}

// ConversationIdle is returned for whenIdle: "reject" input on an idle conversation.
type ConversationIdle struct{ ConversationID ConversationID }

func (e *ConversationIdle) Error() string {
	return fmt.Sprintf("conversation %s is idle", e.ConversationID)
}

// ErrHarnessClosed is returned by operations after Close or Kill.
type ErrHarnessClosed struct{}

func (ErrHarnessClosed) Error() string { return "Harness is closed" }

// ErrSubmissionNotFound is returned when a submission id does not exist.
type ErrSubmissionNotFound struct{ ID SubmissionID }

func (e ErrSubmissionNotFound) Error() string {
	return fmt.Sprintf("Submission %s does not exist", e.ID)
}

// AbortResult reports what aborting a submission did.
type AbortResult string

const (
	AbortAborted       AbortResult = "aborted"
	AbortAlreadyPlaced AbortResult = "already_placed"
	AbortSettled       AbortResult = "settled"
	AbortNotFound      AbortResult = "not_found"
)

// waiters is a broadcast channel: every state change replaces it.
type notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

func newNotifier() *notifier { return &notifier{ch: make(chan struct{})} }

func (n *notifier) notify() {
	n.mu.Lock()
	close(n.ch)
	n.ch = make(chan struct{})
	n.mu.Unlock()
}

func (n *notifier) wait(ctx context.Context) error {
	return waitChan(ctx, n.current())
}

func (n *notifier) current() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ch
}

func waitChan(ctx context.Context, ch <-chan struct{}) error {
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// contentText flattens a JSON user content value into text, the way the tests
// compare it. Content may be a JSON string or an array of content blocks.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks agentcore.ContentList
	if err := json.Unmarshal(raw, &blocks); err == nil {
		out := ""
		for _, c := range blocks {
			if t, ok := c.(agentcore.TextContent); ok {
				out += t.Text
			}
		}
		return out
	}
	return ""
}

// contentMessages converts a user content value into a single UserMessage.
func contentMessages(raw json.RawMessage, ts int64) agentcore.MessageList {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return agentcore.MessageList{agentcore.UserMessage{
			RoleField: agentcore.RoleUser,
			Content:   agentcore.ContentList{agentcore.NewTextContent(s)},
			Timestamp: ts,
		}}
	}
	var blocks agentcore.ContentList
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return agentcore.MessageList{agentcore.UserMessage{
			RoleField: agentcore.RoleUser,
			Content:   blocks,
			Timestamp: ts,
		}}
	}
	return nil
}

// rawJSON marshals v, panicking only on programmer error.
func rawJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("durable: marshal: %v", err))
	}
	return b
}

// nowMillis is the default harness clock.
func nowMillis() int64 { return time.Now().UnixMilli() }
