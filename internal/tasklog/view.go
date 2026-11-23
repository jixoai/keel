package tasklog

// The panel's read-side projection: one task event decoded into the fields a
// read-only surface may show. The materialized `task_events` payload is the
// only input (spec: 任务树与面板 MUST 只读该投影) — the panel never reads the
// raw payload itself, and this is the one decoder that turns it into those
// fields. No writer is touched here, and no new truth is derived: a field that
// the event did not carry stays absent.

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
)

// DescribedEvent is one task event as a projection surface reads it. The
// fields mirror taskEventPayload one-to-one; Kind and Seq always come from the
// materialized row, so a payload cannot relabel its own event.
type DescribedEvent struct {
	TaskID       uuid.UUID `json:"taskId"`
	Kind         string    `json:"kind"`
	Seq          int64     `json:"seq"`
	At           time.Time `json:"at"`
	Title        string    `json:"title,omitempty"`
	Status       string    `json:"status,omitempty"`
	ParentTaskID string    `json:"parentTaskId,omitempty"`
	Origin       string    `json:"origin,omitempty"`
	OriginSeq    int64     `json:"originSeq,omitempty"`
	Path         string    `json:"path,omitempty"`
	Hash         string    `json:"hash,omitempty"`
	Text         string    `json:"text,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	// Unparsed marks an event whose kind this build does not decode (a newer
	// server's kind). The event is still shown with its kind and sequence
	// instead of being silently dropped.
	Unparsed bool `json:"unparsed,omitempty"`
}

// DescribeEvent decodes one materialized task event. A payload that does not
// satisfy its kind's contract is an error: the projection guarantees decodable
// rows, so a failure means the row is corrupt and the caller surfaces it
// rather than pretend the event carries nothing.
func DescribeEvent(event Event) (DescribedEvent, error) {
	described := DescribedEvent{
		TaskID: event.TaskID,
		Kind:   event.Kind,
		Seq:    event.Seq,
		At:     event.CreatedAt,
	}
	if !IsTaskKind(kernel.EventKind(event.Kind)) {
		// An unknown kind is not corrupt data: a newer build wrote it.
		described.Unparsed = true
		return described, nil
	}
	payload, err := decodeTaskPayload(event.Kind, event.Payload)
	if err != nil {
		return DescribedEvent{}, err
	}
	described.Title = payload.Title
	described.Status = payload.Status
	described.ParentTaskID = payload.ParentTaskID
	described.Origin = payload.Origin
	described.OriginSeq = payload.OriginSeq
	described.Path = payload.Path
	described.Hash = payload.Hash
	described.Text = payload.Text
	described.Reason = payload.Reason
	return described, nil
}

// DecodeEventPayload exposes the projection payload shape to tests and to a
// future caller that needs a field this view does not name yet. It performs no
// kind contract check.
func DecodeEventPayload(raw json.RawMessage) (map[string]any, error) {
	var payload map[string]any
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}
