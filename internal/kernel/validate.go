package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// kindSpec is the kernel's contract for one event kind.
type kindSpec struct {
	// class is the commit category: owner-scoped kinds carry the fence,
	// edge-scoped kinds are client input and never see the lease.
	class CommitClass
	// key is the idempotency space the kind carries ("" = none).
	key KeyKind
}

// kindRegistry is the registered event kinds. A kind absent here cannot enter
// the log. The string values are pinned against internal/store by a test, and
// the edge whitelist (EdgeKinds) is exactly the ClassEdge entries — an edge
// endpoint must accept nothing else.
//
// run.started / run.failed are *reserved*: the vocabulary and their fences are
// implemented, but no runtime emits them today (the run row already records
// start/attempts/outcome, and emitting them would change the client-visible
// event stream, which the R1 parity gate pins). Do not treat their presence
// here as "the loop emits lifecycle events".
//
// run.finished is different: the worker DOES emit it once per settled attempt
// (the attempt-level summary), because a client needs turns/tokens/stop reason
// without reconstructing them from the transcript. It is bookkeeping, not a
// message: the transcript decoder skips it.
var kindRegistry = map[EventKind]kindSpec{
	KindUserMessage:      {class: ClassEdge, key: KeyRequest},
	KindAssistantMessage: {class: ClassOwner, key: KeyGeneration},
	KindToolResult:       {class: ClassOwner, key: KeyToolCall},
	KindCompaction:       {class: ClassOwner},
	KindRunStarted:       {class: ClassOwner},
	KindRunFailed:        {class: ClassOwner},
	KindRunFinished:      {class: ClassOwner},
	KindExternalEvent:    {class: ClassExternal, key: KeyRequest},
}

// RegisteredKind reports whether the kind may be committed.
func RegisteredKind(kind EventKind) bool {
	_, ok := kindRegistry[kind]
	return ok
}

// KindClass reports the commit class of a registered kind.
func KindClass(kind EventKind) (CommitClass, bool) {
	spec, ok := kindRegistry[kind]
	if !ok {
		return "", false
	}
	return spec.class, true
}

// EdgeKinds returns the client-writable whitelist: exactly the kinds an edge
// endpoint may accept from a client. Everything else is 400 there, and the
// kernel rejects it without a fence anyway.
func EdgeKinds() []EventKind {
	var out []EventKind
	for _, kind := range Kinds() {
		if kindRegistry[kind].class == ClassEdge {
			out = append(out, kind)
		}
	}
	return out
}

// Kinds returns the registered kinds in stable order.
func Kinds() []EventKind {
	return []EventKind{
		KindUserMessage,
		KindAssistantMessage,
		KindToolResult,
		KindCompaction,
		KindRunStarted,
		KindRunFailed,
		KindRunFinished,
		KindExternalEvent,
	}
}

// keyFor validates the idempotency key shape of a request against its kind.
func (s kindSpec) keyFor(req CommitRequest) (KeyRef, error) {
	present := make(map[KeyKind]string, 3)
	for _, kv := range []struct {
		kind  KeyKind
		value string
	}{
		{KeyRequest, req.RequestID},
		{KeyGeneration, req.GenerationID},
		{KeyToolCall, req.ToolCallID},
	} {
		if kv.value != "" {
			present[kv.kind] = kv.value
		}
	}
	if s.key == "" {
		if len(present) > 0 {
			return KeyRef{}, fmt.Errorf("kind %q carries no idempotency key, got %s", req.Kind, keyList(present))
		}
		return KeyRef{}, nil
	}
	if len(present) != 1 {
		if len(present) == 0 {
			return KeyRef{}, fmt.Errorf("kind %q requires a %s", req.Kind, s.key)
		}
		return KeyRef{}, fmt.Errorf("kind %q takes exactly one idempotency key (%s), got %s", req.Kind, s.key, keyList(present))
	}
	if _, ok := present[s.key]; !ok {
		return KeyRef{}, fmt.Errorf("kind %q takes %s, got %s", req.Kind, s.key, keyList(present))
	}
	return KeyRef{Kind: s.key, Value: present[s.key]}, nil
}

func keyList(present map[KeyKind]string) string {
	out := ""
	for _, k := range []KeyKind{KeyRequest, KeyGeneration, KeyToolCall} {
		if _, ok := present[k]; !ok {
			continue
		}
		if out != "" {
			out += ", "
		}
		out += string(k)
	}
	return out
}

// contentBlock is the minimal shape the kernel reads out of a message payload:
// only the fields the pairing contract depends on.
type contentBlock struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// toolCallBlocks decodes the toolCall blocks of an assistant payload. A payload
// that is not a message document is a defect the kernel refuses to store.
func toolCallBlocks(payload json.RawMessage) ([]contentBlock, error) {
	if len(payload) == 0 {
		return nil, errors.New("payload is empty")
	}
	var msg struct {
		Content []contentBlock `json:"content"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil {
		return nil, fmt.Errorf("payload is not a message document: %w", err)
	}
	var calls []contentBlock
	for _, c := range msg.Content {
		if c.Type == "toolCall" {
			calls = append(calls, c)
		}
	}
	return calls, nil
}

// validateAssistantPayload checks the tool-call contract of an assistant
// message: every call has an id and a name, and no id is repeated inside the
// same message (a result could no longer be attached to a unique call).
func validateAssistantPayload(payload json.RawMessage) error {
	calls, err := toolCallBlocks(payload)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(calls))
	for _, c := range calls {
		if c.ID == "" {
			return errors.New("assistant tool call without an id cannot be paired with a result")
		}
		if c.Name == "" {
			return fmt.Errorf("assistant tool call %q has no name", c.ID)
		}
		if seen[c.ID] {
			return fmt.Errorf("%w: %q", ErrDuplicateToolCall, c.ID)
		}
		seen[c.ID] = true
	}
	return nil
}

// validateToolResultPayload reads the tool.result contract: the payload must
// carry the same toolCallId as the request, and the tool name it reports is
// used to detect a result hung on the wrong call.
func validateToolResultPayload(req CommitRequest) (ToolCallRef, error) {
	if len(req.Payload) == 0 {
		return ToolCallRef{}, errors.New("payload is empty")
	}
	var p struct {
		ToolCallID string `json:"toolCallId"`
		ToolName   string `json:"toolName"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		return ToolCallRef{}, fmt.Errorf("payload is not a tool result document: %w", err)
	}
	if p.ToolCallID == "" {
		return ToolCallRef{}, errors.New("payload carries no toolCallId")
	}
	if p.ToolCallID != req.ToolCallID {
		return ToolCallRef{}, fmt.Errorf("payload toolCallId %q does not match request toolCallId %q", p.ToolCallID, req.ToolCallID)
	}
	return ToolCallRef{ToolCallID: p.ToolCallID, ToolName: p.ToolName}, nil
}

// findToolCall locates the committed assistant tool call with this id. The
// implementation is the store's (indexed lookup in PostgreSQL, bounded reverse
// scan in MemStore); see SessionTxn.FindToolCall for the cost contract.
func findToolCall(ctx context.Context, txn SessionTxn, callID string) (ToolCallRef, bool, error) {
	return txn.FindToolCall(ctx, callID)
}

// finishStatus mirrors store.FinishRun's outcome policy: a failed run goes back
// to pending unless the attempt budget is spent; a successful run goes back to
// pending when newer input arrived while it ran, otherwise it is done.
func finishStatus(runErr error, attempts, maxAttempts int, moreInput, cancelled bool) string {
	switch {
	case cancelled:
		// A cancelled attempt is terminal: the operator asked for it to stop, so
		// re-queueing the same input would make the cancel a no-op. The input
		// remains unanswered in the log and the next run (started by later
		// input) answers it.
		return RunStatusFailed
	case runErr != nil:
		if maxAttempts > 0 && attempts >= maxAttempts {
			return RunStatusFailed
		}
		return RunStatusPending
	case moreInput:
		return RunStatusPending
	default:
		return RunStatusDone
	}
}

// Delivery values of user.message.payload.delivery. They are the durable
// contract between the edge (which writes the intent) and the loop (which
// consumes it): queue input waits for the next run, guide input is injected
// into the running turn at a tool boundary. The names follow PI's
// followUp/steer and zcode's queue/guide wording.
const (
	DeliveryQueue = "queue"
	DeliveryGuide = "guide"
)

// Summary status values of a run.finished payload: the attempt-level outcome.
// "cancelled" is a deliberate stop, distinct from a failure.
const (
	RunSummaryDone      = "done"
	RunSummaryFailed    = "failed"
	RunSummaryCancelled = "cancelled"
)

// validateUserMessagePayload pins the shape the transcript rebuild depends on.
// user.message is the only kind a client can write; a payload the runner cannot
// decode would poison the session permanently, because the event is already in
// the log and every later rebuild of the transcript fails the same way. Unknown
// fields stay allowed so the edge can carry metadata without a kernel change.
func validateUserMessagePayload(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return errors.New("user.message payload must be a JSON object")
	}
	// A field that is present must have the declared type: null is not a
	// string, and "close enough" shapes are how a session gets poisoned.
	if v, ok := payload["text"]; ok && !isJSONString(v) {
		return errors.New(`user.message payload "text" must be a string`)
	}
	if v, ok := payload["requestId"]; ok && !isJSONString(v) {
		return errors.New(`user.message payload "requestId" must be a string`)
	}
	// delivery selects how the loop consumes this input (PI steer/followUp,
	// zcode guide/queue). It is persisted in the payload because a successor
	// process must see the intent from the log alone. An absent field means
	// "queue" (the pre-existing behaviour), so old clients and old dumps keep
	// working.
	if v, ok := payload["delivery"]; ok {
		var delivery string
		if !isJSONString(v) || json.Unmarshal(v, &delivery) != nil {
			return errors.New(`user.message payload "delivery" must be a string`)
		}
		if delivery != DeliveryQueue && delivery != DeliveryGuide {
			return fmt.Errorf(`user.message payload "delivery" must be "queue" or "guide", got %q`, delivery)
		}
	}
	return nil
}

// validateRunFinishedPayload pins the shape of the attempt summary. The event is
// the durable answer to "what did this attempt do?" — the run row only carries
// the session-level status — so the status must be one of the three terminal
// attempt outcomes and the counters must be numbers when present. Unknown
// fields stay allowed: a newer worker may add a metric without a kernel change.
func validateRunFinishedPayload(raw json.RawMessage) error {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
		return errors.New("run.finished payload must be a JSON object")
	}
	status, ok := payload["status"]
	if !ok {
		return errors.New(`run.finished payload requires a "status"`)
	}
	if !isJSONString(status) {
		return errors.New(`run.finished payload "status" must be a string`)
	}
	var s string
	_ = json.Unmarshal(status, &s)
	switch s {
	case RunSummaryDone, RunSummaryFailed, RunSummaryCancelled:
	default:
		return fmt.Errorf(`run.finished payload "status" must be %q, %q or %q, got %q`,
			RunSummaryDone, RunSummaryFailed, RunSummaryCancelled, s)
	}
	for _, field := range []string{"turns", "durationMs"} {
		if v, ok := payload[field]; ok && !isJSONCount(v) {
			return fmt.Errorf(`run.finished payload %q must be a non-negative integer`, field)
		}
	}
	if v, ok := payload["stopReason"]; ok && !isJSONString(v) {
		return errors.New(`run.finished payload "stopReason" must be a string`)
	}
	if v, ok := payload["error"]; ok && !isJSONString(v) {
		return errors.New(`run.finished payload "error" must be a string`)
	}
	for _, field := range []string{"tokens", "context"} {
		v, ok := payload[field]
		if !ok {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(v, &obj); err != nil || obj == nil {
			return fmt.Errorf(`run.finished payload %q must be a JSON object`, field)
		}
		for name, value := range obj {
			if !isJSONCount(value) {
				return fmt.Errorf(`run.finished payload %q field %q must be a non-negative integer`, field, name)
			}
		}
	}
	if v, ok := payload["toolTimings"]; ok {
		var entries []map[string]json.RawMessage
		if err := json.Unmarshal(v, &entries); err != nil {
			return errors.New(`run.finished payload "toolTimings" must be an array of objects`)
		}
		for _, entry := range entries {
			if entry == nil {
				return errors.New(`run.finished payload "toolTimings" entries must be objects`)
			}
			if name, ok := entry["tool"]; ok && !isJSONString(name) {
				return errors.New(`run.finished payload toolTimings "tool" must be a string`)
			}
			if d, ok := entry["durationMs"]; ok && !isJSONCount(d) {
				return errors.New(`run.finished payload toolTimings "durationMs" must be a non-negative integer`)
			}
			if e, ok := entry["isError"]; ok && !isJSONBool(e) {
				return errors.New(`run.finished payload toolTimings "isError" must be a boolean`)
			}
		}
	}
	return nil
}

// isJSONCount reports whether raw is a non-negative JSON integer (null and
// floats are not counters).
func isJSONCount(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	var n int64
	return json.Unmarshal(trimmed, &n) == nil && n >= 0
}

func isJSONBool(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return bytes.Equal(trimmed, []byte("true")) || bytes.Equal(trimmed, []byte("false"))
}

func isJSONString(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	// json.Unmarshal accepts JSON null into a string without error, so the
	// literal has to be rejected explicitly.
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	var s string
	return json.Unmarshal(trimmed, &s) == nil
}

// isJSONNull reports whether raw is the JSON literal null.
func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// validateExternalEventPayload pins the provenance marks of an external.event:
// the payload must be a JSON object naming the backend and the backend's own
// opaque session ref. Without those marks the log could not tell the record
// apart from a native message, which is exactly what the compatibility
// boundary forbids (external-gateway spec: 兼容模式标记).
func validateExternalEventPayload(raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return errors.New("external.event payload is required")
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
		return errors.New("external.event payload must be a JSON object")
	}
	requireString := func(field string) error {
		value, ok := payload[field]
		if !ok || !isJSONString(value) {
			return fmt.Errorf(`external.event payload %q must be a non-empty string`, field)
		}
		var s string
		if err := json.Unmarshal(value, &s); err != nil || strings.TrimSpace(s) == "" {
			return fmt.Errorf(`external.event payload %q must be a non-empty string`, field)
		}
		return nil
	}
	if err := requireString("backend"); err != nil {
		return err
	}
	if err := requireString("externalRef"); err != nil {
		return err
	}
	if value, ok := payload["stateOwner"]; ok && !isJSONNull(value) {
		var owner string
		if err := json.Unmarshal(value, &owner); err != nil || owner != StateOwnerExternal {
			return fmt.Errorf(`external.event payload "stateOwner" must be %q`, StateOwnerExternal)
		}
	}
	if value, ok := payload["seq"]; ok && !isJSONNull(value) {
		var seq int64
		if err := json.Unmarshal(value, &seq); err != nil || seq < 0 {
			return errors.New(`external.event payload "seq" must be a non-negative integer`)
		}
	}
	return nil
}

// samePayload compares two JSON payloads by content, not by formatting: the
// same request retried with re-ordered keys or different whitespace is still
// the same request.
func samePayload(a, b json.RawMessage) bool {
	ca, errA := canonicalJSON(a)
	cb, errB := canonicalJSON(b)
	if errA != nil || errB != nil {
		// A payload that cannot be canonicalized (should not happen: the
		// kernel validated it) falls back to a byte comparison.
		return string(a) == string(b)
	}
	return ca == cb
}

func canonicalJSON(raw json.RawMessage) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
