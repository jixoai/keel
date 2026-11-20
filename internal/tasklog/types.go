// Package tasklog implements the task-card memory of the Keel Harness (spec:
// keel-harness-v2 / task-log).
//
// A task card is a DB row plus an append-only event history. The ledger is the
// only truth: every change is committed as a task.created / task.updated /
// task.attached / task.detached / task.closed / task.note event, and both read
// models (task_cards and task_events) are projections rebuilt from the owning
// session's ledger. Nothing in this package mutates a projection without first
// appending the event it projects.
//
// Task ids are the attribution handle of a run: every tool call is resolved to
// a card before it executes (see toolsurface's TaskGate), and a sub-agent's card
// is linked to the card that spawned it through parentTaskId. The tree is a
// projection of those links, which is why a session migration cannot break it:
// the relation lives in the database, not in a process.
package tasklog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
)

// Card status vocabulary. A card's status is a projection field, not a state
// machine: closing is terminal in practice (close on a closed card is a no-op)
// while every transition is still recorded as an event.
const (
	StatusOpen       = "open"
	StatusInProgress = "in_progress"
	StatusBlocked    = "blocked"
	StatusDone       = "done"
	StatusClosed     = "closed"
)

// Card origins: how the card came into existence. The initial card of a session
// is "session", a card created through the tasks tool is "tool", a sub-agent's
// card is "subagent" and a fork's card is "fork" (originSeq then names the
// fork's eventSeq origin).
const (
	OriginSession  = "session"
	OriginTool     = "tool"
	OriginSubagent = "subagent"
	OriginFork     = "fork"
)

// ValidStatus reports whether status belongs to the closed status vocabulary.
func ValidStatus(status string) bool {
	switch status {
	case StatusOpen, StatusInProgress, StatusBlocked, StatusDone, StatusClosed:
		return true
	default:
		return false
	}
}

// Card is one task card as the projection currently sees it.
type Card struct {
	TenantID     uuid.UUID  `json:"-"`
	SessionID    uuid.UUID  `json:"-"`
	TaskID       uuid.UUID  `json:"taskId"`
	ParentTaskID *uuid.UUID `json:"parentTaskId,omitempty"`
	Title        string     `json:"title"`
	Status       string     `json:"status"`
	Origin       string     `json:"origin"`
	OriginSeq    int64      `json:"originSeq,omitempty"`
	CreatedSeq   int64      `json:"createdSeq"`
	UpdatedSeq   int64      `json:"updatedSeq"`
	ClosedSeq    int64      `json:"closedSeq,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

// Event is one materialized task.* event. Seq is its ledger sequence, so the
// projection can always be reconciled against the log.
type Event struct {
	TaskID    uuid.UUID       `json:"taskId"`
	Kind      string          `json:"kind"`
	Seq       int64           `json:"seq"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"createdAt"`
}

// Attachment is one (path, hash) reference a card carries. The bytes stay in
// layerfs; the card only records the reference.
type Attachment struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
}

// View is what tasks.show answers: the card, its history, its children (cards
// whose parentTaskId points at it, possibly from another session) and its live
// attachment set.
type View struct {
	Card        Card         `json:"card"`
	Events      []Event      `json:"events"`
	Children    []Card       `json:"children"`
	Attachments []Attachment `json:"attachments"`
}

// CreateInput is the validated input of tasks.create.
type CreateInput struct {
	Title        string
	Status       string
	ParentTaskID *uuid.UUID
}

// UpdateInput is the field set of tasks.update. Nil pointers are "unchanged".
type UpdateInput struct {
	Title        *string
	Status       *string
	ParentTaskID *uuid.UUID
}

// ListFilter narrows tasks.list. A nil SessionID means the caller's session.
type ListFilter struct {
	SessionID    *uuid.UUID
	ParentTaskID *uuid.UUID
	Status       string
}

// Stats is the outcome of one projection rebuild.
type Stats struct {
	Events int `json:"events"`
	Cards  int `json:"cards"`
}

// Typed errors. Callers branch with errors.Is: the tools surface maps them onto
// typed tool-result codes instead of parsing prose.
var (
	// ErrTaskNotFound: the card does not exist inside the caller's tenant.
	ErrTaskNotFound = errors.New("tasklog: task card not found")
	// ErrTaskRequired: attribution is impossible because the session has no
	// card and one could not be created.
	ErrTaskRequired = errors.New("tasklog: a task id is required")
	// ErrInvalidTaskID: the id is not a UUID.
	ErrInvalidTaskID = errors.New("tasklog: invalid task id")
	// ErrInvalidEvent: the payload does not satisfy the kind's contract.
	ErrInvalidEvent = errors.New("tasklog: invalid task event payload")
	// ErrInvalidStatus: the status is outside the closed vocabulary.
	ErrInvalidStatus = errors.New("tasklog: invalid task status")
	// ErrTaskCycle: a parentTaskId would make the tree cyclic.
	ErrTaskCycle = errors.New("tasklog: parentTaskId would create a cycle")
	// ErrInvalidCursor: the child-card page cursor is malformed.
	ErrInvalidCursor = errors.New("tasklog: malformed task card cursor")
	// ErrNoBackend: the service was built without a ledger or projection.
	ErrNoBackend = errors.New("tasklog: ledger or projection backend is not wired")
)

// taskEventPayload is the durable payload shape of every task.* kind. One
// structure for six kinds keeps the projection decoder and the writers from
// drifting; each kind validates the fields it needs.
type taskEventPayload struct {
	TaskID       string `json:"taskId"`
	Title        string `json:"title,omitempty"`
	Status       string `json:"status,omitempty"`
	ParentTaskID string `json:"parentTaskId,omitempty"`
	Origin       string `json:"origin,omitempty"`
	OriginSeq    int64  `json:"originSeq,omitempty"`
	Path         string `json:"path,omitempty"`
	Hash         string `json:"hash,omitempty"`
	Text         string `json:"text,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// decodeTaskPayload parses a stored payload and checks the fields every task
// event carries. taskId is mandatory: the event key alone cannot be the
// projection's input because replays and rebuilds read only the payload.
func decodeTaskPayload(kind string, raw json.RawMessage) (taskEventPayload, error) {
	if len(raw) == 0 {
		return taskEventPayload{}, fmt.Errorf("%w: %s payload is empty", ErrInvalidEvent, kind)
	}
	var payload taskEventPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return taskEventPayload{}, fmt.Errorf("%w: %s payload is not an object: %v", ErrInvalidEvent, kind, err)
	}
	if _, err := uuid.Parse(payload.TaskID); err != nil {
		return taskEventPayload{}, fmt.Errorf("%w: %s payload has no valid taskId", ErrInvalidEvent, kind)
	}
	switch kind {
	case string(kernel.KindTaskCreated):
		if strings.TrimSpace(payload.Title) == "" {
			return taskEventPayload{}, fmt.Errorf("%w: task.created needs a title", ErrInvalidEvent)
		}
		if payload.Status == "" {
			payload.Status = StatusOpen
		}
		if !ValidStatus(payload.Status) {
			return taskEventPayload{}, fmt.Errorf("%w: %q", ErrInvalidStatus, payload.Status)
		}
		if payload.ParentTaskID != "" {
			if _, err := uuid.Parse(payload.ParentTaskID); err != nil {
				return taskEventPayload{}, fmt.Errorf("%w: task.created parentTaskId %q", ErrInvalidEvent, payload.ParentTaskID)
			}
		}
	case string(kernel.KindTaskUpdated):
		if payload.Title == "" && payload.Status == "" && payload.ParentTaskID == "" {
			return taskEventPayload{}, fmt.Errorf("%w: task.updated carries no change", ErrInvalidEvent)
		}
		if payload.Status != "" && !ValidStatus(payload.Status) {
			return taskEventPayload{}, fmt.Errorf("%w: %q", ErrInvalidStatus, payload.Status)
		}
		if payload.ParentTaskID != "" {
			if _, err := uuid.Parse(payload.ParentTaskID); err != nil {
				return taskEventPayload{}, fmt.Errorf("%w: task.updated parentTaskId %q", ErrInvalidEvent, payload.ParentTaskID)
			}
		}
	case string(kernel.KindTaskAttached):
		if strings.TrimSpace(payload.Path) == "" || strings.TrimSpace(payload.Hash) == "" {
			return taskEventPayload{}, fmt.Errorf("%w: task.attached needs path and hash", ErrInvalidEvent)
		}
	case string(kernel.KindTaskDetached):
		if strings.TrimSpace(payload.Path) == "" {
			return taskEventPayload{}, fmt.Errorf("%w: task.detached needs a path", ErrInvalidEvent)
		}
	case string(kernel.KindTaskClosed):
		// reason is optional.
	case string(kernel.KindTaskNote):
		if strings.TrimSpace(payload.Text) == "" {
			return taskEventPayload{}, fmt.Errorf("%w: task.note needs text", ErrInvalidEvent)
		}
	default:
		return taskEventPayload{}, fmt.Errorf("%w: %q is not a task event kind", ErrInvalidEvent, kind)
	}
	return payload, nil
}

// IsTaskKind reports whether the ledger kind belongs to the task log.
func IsTaskKind(kind kernel.EventKind) bool {
	switch kind {
	case kernel.KindTaskCreated, kernel.KindTaskUpdated, kernel.KindTaskAttached,
		kernel.KindTaskDetached, kernel.KindTaskClosed, kernel.KindTaskNote:
		return true
	default:
		return false
	}
}

// InitialTaskID derives the deterministic card id of a session's initial card.
// Determinism is what makes "会话起步即有卡" idempotent without a global lock: two
// processes creating the card converge on the same id and the same ledger key.
func InitialTaskID(tenantID, sessionID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("keel/task/initial/"+tenantID.String()+"/"+sessionID.String()))
}

// callIDKey carries the tool-call id into the plugin backend so a retried tool
// call dedupes its task event instead of appending a second copy.
type callIDKey struct{}

// WithCallID attaches the idempotency suffix of one tool call.
func WithCallID(ctx context.Context, callID string) context.Context {
	if strings.TrimSpace(callID) == "" {
		return ctx
	}
	return context.WithValue(ctx, callIDKey{}, callID)
}

// CallIDFromContext reads the ambient tool-call id ("" when absent).
func CallIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(callIDKey{}).(string)
	return value
}
