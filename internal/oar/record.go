package oar

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// RecordSchema is the schema id of one OAR record.
const RecordSchema = "keel.oar.record/1"

// RootAgentPath is the agent path of the runtime's main agent. Child agents use
// paths below it ("/main/worker-1").
const RootAgentPath = "/main"

// MaxRecordBytes bounds one record (encoded) on the wire and in the journal.
// The gateway frame cap is larger; this is the OAR-specific bound so one
// pathological tool result never becomes a multi-megabyte record.
const MaxRecordBytes = 1 << 20

// RecordKind is the record's role in the stream.
type RecordKind string

const (
	// KindCall is a call that started: a tool invocation, a model call or a
	// runtime action. CallID links it to its result.
	KindCall RecordKind = "call"
	// KindResult is a call's result (or a standalone result when the runtime
	// reported no call).
	KindResult RecordKind = "result"
	// KindStatus is a lifecycle or health fact: session/turn started/completed/
	// failed/cancelled/degraded.
	KindStatus RecordKind = "status"
	// KindMessage is assistant/reasoning/error text. It is not a call.
	KindMessage RecordKind = "message"
	// KindUsage is a token-usage fact. Its absence means not measurable, never
	// zero.
	KindUsage RecordKind = "usage"
	// KindError is a runtime error that is not a call result.
	KindError RecordKind = "error"
)

// recordKinds is the closed set in stable order.
var recordKinds = []RecordKind{KindCall, KindResult, KindStatus, KindMessage, KindUsage, KindError}

// KnownRecordKinds returns the closed kind vocabulary.
func KnownRecordKinds() []RecordKind {
	out := make([]RecordKind, len(recordKinds))
	copy(out, recordKinds)
	return out
}

// ParseRecordKind validates a kind name.
func ParseRecordKind(raw string) (RecordKind, bool) {
	for _, kind := range recordKinds {
		if string(kind) == raw {
			return kind, true
		}
	}
	return "", false
}

// Record statuses. They are the values of Record.Status.
const (
	StatusStarted   = "started"
	StatusRunning   = "running"
	StatusDegraded  = "degraded"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

var recordStatuses = map[string]bool{
	StatusStarted:   true,
	StatusRunning:   true,
	StatusDegraded:  true,
	StatusCompleted: true,
	StatusFailed:    true,
	StatusCancelled: true,
}

// Attribution is the out-of-band ownership of an external call. It is set by
// the host in the session-open config and echoed on every record; the runtime
// never creates a task card and the host never injects "please record a task
// card" into the prompt.
type Attribution struct {
	// SessionID is the Keel (host) session that started the external call.
	SessionID string `json:"sessionId,omitempty"`
	// TaskID is the task card the external call belongs to. External calls are
	// attributed to the card that started them; they never create a child card.
	TaskID string `json:"taskId,omitempty"`
}

// RecordError is an error attached to a record.
type RecordError struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// RecordUsage is runtime-reported usage. A missing Usage is "not measurable";
// it is never synthesized as zero.
type RecordUsage struct {
	Metering     string `json:"metering,omitempty"`
	Model        string `json:"model,omitempty"`
	InputTokens  int64  `json:"inputTokens,omitempty"`
	OutputTokens int64  `json:"outputTokens,omitempty"`
	TotalTokens  int64  `json:"totalTokens,omitempty"`
}

// Record is one fact of the out-of-band record stream.
type Record struct {
	// Schema is RecordSchema.
	Schema string `json:"schema"`
	// Seq is monotonic per backend session (strictly increasing across every
	// agentPath of that session).
	Seq int64 `json:"seq"`
	// AgentPath is the agent that produced the record ("/main",
	// "/main/worker-1").
	AgentPath string `json:"agentPath"`
	// SessionID is the backend's opaque session handle (the gateway session id).
	SessionID string `json:"sessionId"`
	// TurnID is the runtime turn that produced the record, when it has one.
	TurnID string `json:"turnId,omitempty"`
	// Kind is the record's role.
	Kind RecordKind `json:"kind"`
	// CallID links a call and its result.
	CallID string `json:"callId,omitempty"`
	// Name is the tool/model/action name (for example "shell" or
	// "agent_message").
	Name string `json:"name,omitempty"`
	// Status is a lifecycle/result state.
	Status string `json:"status,omitempty"`
	// Input is the call input (an opaque JSON value).
	Input json.RawMessage `json:"input,omitempty"`
	// Output is the result output (an opaque JSON value).
	Output json.RawMessage `json:"output,omitempty"`
	// Message is message text or a human-readable status detail.
	Message string `json:"message,omitempty"`
	// Error is a typed runtime error.
	Error *RecordError `json:"error,omitempty"`
	// Usage is runtime-reported usage; nil means not measurable.
	Usage *RecordUsage `json:"usage,omitempty"`
	// Runtime is the adapter id that produced the record.
	Runtime string `json:"runtime,omitempty"`
	// Attribution is the host-supplied ownership block.
	Attribution Attribution `json:"attribution"`
	// Timestamp is when the runtime produced the record (RFC3339Nano on the
	// wire).
	Timestamp time.Time `json:"ts"`
}

// agentPathPattern bounds an agent path: one or more "/segment" parts with a
// safe alphabet, no trailing slash and no "..".
var agentPathPattern = regexp.MustCompile(`^/[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

// Validate checks one record against the schema. The returned error is a typed
// *Error with CodeInvalidRecord (or CodeRecordTooLarge).
func (r Record) Validate() error {
	invalid := func(format string, args ...any) error {
		return &Error{Code: CodeInvalidRecord, Runtime: r.Runtime, Detail: fmt.Sprintf(format, args...)}
	}
	if r.Schema != RecordSchema {
		return invalid("schema is %q, want %q", r.Schema, RecordSchema)
	}
	if r.Seq <= 0 {
		return invalid("seq must be positive, got %d", r.Seq)
	}
	if !validAgentPath(r.AgentPath) {
		return invalid("agentPath %q must be /segment(/segment)* with [A-Za-z0-9._-]", r.AgentPath)
	}
	if strings.TrimSpace(r.SessionID) == "" {
		return invalid("sessionId is required")
	}
	if _, ok := ParseRecordKind(string(r.Kind)); !ok {
		return invalid("kind %q is not one of %v", r.Kind, KnownRecordKinds())
	}
	if r.Timestamp.IsZero() {
		return invalid("ts is required")
	}
	if r.Status != "" && !recordStatuses[r.Status] {
		return invalid("status %q is not one of started|running|degraded|completed|failed|cancelled", r.Status)
	}
	if r.Error != nil && strings.TrimSpace(r.Error.Message) == "" {
		return invalid("error.message is required when error is present")
	}
	switch r.Kind {
	case KindCall:
		if r.CallID == "" {
			return invalid("a call record requires callId")
		}
	case KindResult:
		if r.CallID == "" {
			return invalid("a result record requires callId")
		}
		if len(r.Output) == 0 && r.Error == nil {
			return invalid("a result record requires output or error")
		}
	case KindStatus:
		if r.Status == "" {
			return invalid("a status record requires status")
		}
	case KindMessage:
		if strings.TrimSpace(r.Message) == "" && len(r.Output) == 0 {
			return invalid("a message record requires message text or output")
		}
	case KindUsage:
		if r.Usage == nil {
			return invalid("a usage record requires usage")
		}
	case KindError:
		if r.Error == nil {
			return invalid("an error record requires error")
		}
	}
	if len(r.Input) > 0 && !json.Valid(r.Input) {
		return invalid("input is not valid JSON")
	}
	if len(r.Output) > 0 && !json.Valid(r.Output) {
		return invalid("output is not valid JSON")
	}
	if r.Error != nil && len(r.Error.Data) > 0 && !json.Valid(r.Error.Data) {
		return invalid("error.data is not valid JSON")
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return invalid("record cannot be encoded: %v", err)
	}
	if len(raw) > MaxRecordBytes {
		return &Error{
			Code:    CodeRecordTooLarge,
			Runtime: r.Runtime,
			Detail:  fmt.Sprintf("record is %d bytes, limit %d", len(raw), MaxRecordBytes),
		}
	}
	return nil
}

func validAgentPath(path string) bool {
	if path == "" || len(path) > 256 {
		return false
	}
	if !agentPathPattern.MatchString(path) {
		return false
	}
	for _, segment := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// ChildAgentPath returns the path of a child agent below parent.
func ChildAgentPath(parent, child string) string {
	if parent == "" {
		parent = RootAgentPath
	}
	child = strings.TrimSpace(child)
	if child == "" {
		return parent
	}
	if !strings.HasPrefix(child, "/") {
		child = "/" + child
	}
	return parent + child
}

// SeqTracker validates monotonic seq per backend session. It is the in-memory
// half of the ordering rule; the journal adds the cross-restart half.
type SeqTracker struct {
	last map[string]int64
	gaps int64
}

// NewSeqTracker returns an empty tracker.
func NewSeqTracker() *SeqTracker {
	return &SeqTracker{last: make(map[string]int64)}
}

// Observe records one seq. A duplicate (same seq as the last one) is reported
// as duplicate and is not an error; a regression is *Error{CodeRecordOrder}.
// A forward jump is allowed (a crash may lose in-flight records) and counted as
// a gap for observability.
func (t *SeqTracker) Observe(sessionID string, seq int64) (duplicate bool, err error) {
	last := t.last[sessionID]
	switch {
	case seq == last:
		return true, nil
	case seq < last:
		return false, &Error{
			Code:   CodeRecordOrder,
			Detail: fmt.Sprintf("session %s seq %d is not greater than the last seq %d", sessionID, seq, last),
		}
	case last > 0 && seq > last+1:
		t.gaps += seq - last - 1
	}
	t.last[sessionID] = seq
	return false, nil
}

// Last returns the last observed seq of a session (0 when none).
func (t *SeqTracker) Last(sessionID string) int64 { return t.last[sessionID] }

// Gaps returns how many seq numbers were skipped (observability only).
func (t *SeqTracker) Gaps() int64 { return t.gaps }

// EqualRecordBytes reports whether two encoded records are byte-identical.
// It is used to make duplicate delivery idempotent: the same seq with the same
// bytes is a replay, the same seq with different bytes is an ordering error.
func EqualRecordBytes(a, b []byte) bool { return bytes.Equal(a, b) }
