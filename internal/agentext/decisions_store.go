package agentext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/store"
)

// decisionTaskKind is the bookkeeping kind used for decision rows. It is not an
// event kind: decisions never enter the kernel's event log. Reusing the tasks
// table keeps the schema untouched (no migration, no new table) while giving
// the answer journal a durable home.
const decisionTaskKind = "keel.decision"

// decisionDoc is the Input document of a decision task row.
type decisionDoc struct {
	RequestID     string           `json:"requestId"`
	Kind          string           `json:"kind"`
	Prompt        string           `json:"prompt"`
	Options       []DecisionOption `json:"options,omitempty"`
	AllowFreeText bool             `json:"allowFreeText,omitempty"`
	CreatedAt     time.Time        `json:"createdAt"`
	Deadline      time.Time        `json:"deadline"`
}

// decisionJournal is the Checkpoint document of a decision task row.
type decisionJournal struct {
	Events []DecisionEvent `json:"events"`
}

// MemoryDecisionStore keeps decisions in process memory. It is the test double
// and the degraded no-database mode; a restart loses its rows.
type MemoryDecisionStore struct {
	mu   sync.Mutex
	rows map[string]DecisionRow
}

// NewMemoryDecisionStore builds an empty in-memory store.
func NewMemoryDecisionStore() *MemoryDecisionStore {
	return &MemoryDecisionStore{rows: make(map[string]DecisionRow)}
}

// Create inserts a row unless the same decision already exists.
func (s *MemoryDecisionStore) Create(_ context.Context, row DecisionRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := decisionKey(row.TenantID, row.SessionID, row.DecisionID)
	if _, exists := s.rows[key]; exists {
		return nil
	}
	s.rows[key] = copyDecisionRow(row)
	return nil
}

// Get reads one row.
func (s *MemoryDecisionStore) Get(_ context.Context, tenantID, sessionID, decisionID uuid.UUID) (DecisionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[decisionKey(tenantID, sessionID, decisionID)]
	if !ok {
		return DecisionRow{}, ErrDecisionNotFound
	}
	return copyDecisionRow(row), nil
}

// Save replaces an existing row.
func (s *MemoryDecisionStore) Save(_ context.Context, row DecisionRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := decisionKey(row.TenantID, row.SessionID, row.DecisionID)
	if _, ok := s.rows[key]; !ok {
		return ErrDecisionNotFound
	}
	s.rows[key] = copyDecisionRow(row)
	return nil
}

// List returns the session's rows.
func (s *MemoryDecisionStore) List(_ context.Context, tenantID, sessionID uuid.UUID) ([]DecisionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DecisionRow, 0)
	for _, row := range s.rows {
		if row.TenantID == tenantID && row.SessionID == sessionID {
			out = append(out, copyDecisionRow(row))
		}
	}
	return out, nil
}

func copyDecisionRow(row DecisionRow) DecisionRow {
	row.Options = append([]DecisionOption(nil), row.Options...)
	row.Events = append([]DecisionEvent(nil), row.Events...)
	if row.Answer != nil {
		answer := *row.Answer
		row.Answer = &answer
	}
	if row.AnsweredAt != nil {
		at := *row.AnsweredAt
		row.AnsweredAt = &at
	}
	return row
}

// TasksDecisionStore persists decision rows through internal/store's tasks
// bookkeeping table. It is the production store: durable answers, no schema
// change, no event-kind change.
type TasksDecisionStore struct {
	store *store.Store
}

// NewTasksDecisionStore wraps a store.
func NewTasksDecisionStore(st *store.Store) *TasksDecisionStore {
	return &TasksDecisionStore{store: st}
}

// Create inserts the decision as a task row (idempotent by decision id).
func (s *TasksDecisionStore) Create(ctx context.Context, row DecisionRow) error {
	input, err := encodeDecisionDoc(row)
	if err != nil {
		return err
	}
	checkpoint, err := json.Marshal(decisionJournal{Events: row.Events})
	if err != nil {
		return fmt.Errorf("agentext: encode decision journal: %w", err)
	}
	_, err = s.store.CreateTask(ctx, store.TaskRow{
		TenantID:   row.TenantID,
		SessionID:  row.SessionID,
		TaskID:     row.DecisionID,
		Kind:       decisionTaskKind,
		Version:    1,
		Input:      input,
		Checkpoint: checkpoint,
		Outcome:    encodeAnswer(row.Answer),
		Status:     string(row.Status),
		StartedAt:  &row.CreatedAt,
		EndedAt:    row.AnsweredAt,
	})
	if err != nil {
		return fmt.Errorf("agentext: create decision row: %w", err)
	}
	return nil
}

// Get reads one decision row, reconstructing it from the task columns.
func (s *TasksDecisionStore) Get(ctx context.Context, tenantID, sessionID, decisionID uuid.UUID) (DecisionRow, error) {
	task, err := s.store.GetTask(ctx, tenantID, sessionID, decisionID)
	if err != nil {
		if errors.Is(err, store.ErrTaskNotFound) {
			return DecisionRow{}, ErrDecisionNotFound
		}
		return DecisionRow{}, err
	}
	if task.Kind != decisionTaskKind {
		return DecisionRow{}, ErrDecisionNotFound
	}
	return decodeDecisionTask(task)
}

// Save rewrites the mutable columns of the decision row.
func (s *TasksDecisionStore) Save(ctx context.Context, row DecisionRow) error {
	// SaveTask replaces every mutable column, so the open document must be
	// re-encoded here too: omitting it would wipe requestId/prompt/options/
	// deadline on the first answer (the row would still say "answered" while
	// losing what was asked).
	input, err := encodeDecisionDoc(row)
	if err != nil {
		return err
	}
	checkpoint, err := json.Marshal(decisionJournal{Events: row.Events})
	if err != nil {
		return fmt.Errorf("agentext: encode decision journal: %w", err)
	}
	if err := s.store.SaveTask(ctx, store.TaskRow{
		TenantID:   row.TenantID,
		SessionID:  row.SessionID,
		TaskID:     row.DecisionID,
		Kind:       decisionTaskKind,
		Version:    1,
		Input:      input,
		Checkpoint: checkpoint,
		Outcome:    encodeAnswer(row.Answer),
		Status:     string(row.Status),
		StartedAt:  &row.CreatedAt,
		EndedAt:    row.AnsweredAt,
	}); err != nil {
		if errors.Is(err, store.ErrTaskNotFound) {
			return ErrDecisionNotFound
		}
		return err
	}
	return nil
}

// List returns every decision row of the session.
func (s *TasksDecisionStore) List(ctx context.Context, tenantID, sessionID uuid.UUID) ([]DecisionRow, error) {
	tasks, err := s.store.ListTasks(ctx, tenantID, sessionID, false)
	if err != nil {
		return nil, err
	}
	out := make([]DecisionRow, 0, len(tasks))
	for _, task := range tasks {
		if task.Kind != decisionTaskKind {
			continue
		}
		row, err := decodeDecisionTask(task)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// encodeDecisionDoc renders the immutable question (the task row's Input
// document).
func encodeDecisionDoc(row DecisionRow) (json.RawMessage, error) {
	raw, err := json.Marshal(decisionDoc{
		RequestID:     row.RequestID,
		Kind:          row.Kind,
		Prompt:        row.Prompt,
		Options:       row.Options,
		AllowFreeText: row.AllowFreeText,
		CreatedAt:     row.CreatedAt,
		Deadline:      row.Deadline,
	})
	if err != nil {
		return nil, fmt.Errorf("agentext: encode decision: %w", err)
	}
	return raw, nil
}

func encodeAnswer(answer *DecisionAnswer) json.RawMessage {
	if answer == nil {
		return nil
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		return nil
	}
	return raw
}

func decodeDecisionTask(task store.TaskRow) (DecisionRow, error) {
	var doc decisionDoc
	if len(task.Input) > 0 {
		if err := json.Unmarshal(task.Input, &doc); err != nil {
			return DecisionRow{}, fmt.Errorf("agentext: decode decision %s: %w", task.TaskID, err)
		}
	}
	var journal decisionJournal
	if len(task.Checkpoint) > 0 {
		if err := json.Unmarshal(task.Checkpoint, &journal); err != nil {
			return DecisionRow{}, fmt.Errorf("agentext: decode decision journal %s: %w", task.TaskID, err)
		}
	}
	row := DecisionRow{
		TenantID:      task.TenantID,
		SessionID:     task.SessionID,
		DecisionID:    task.TaskID,
		RequestID:     doc.RequestID,
		Kind:          doc.Kind,
		Prompt:        doc.Prompt,
		Options:       doc.Options,
		AllowFreeText: doc.AllowFreeText,
		Status:        DecisionStatus(task.Status),
		CreatedAt:     doc.CreatedAt,
		Deadline:      doc.Deadline,
		AnsweredAt:    task.EndedAt,
		Events:        journal.Events,
	}
	if row.CreatedAt.IsZero() && task.StartedAt != nil {
		row.CreatedAt = *task.StartedAt
	}
	if len(task.Outcome) > 0 {
		var answer DecisionAnswer
		if err := json.Unmarshal(task.Outcome, &answer); err != nil {
			return DecisionRow{}, fmt.Errorf("agentext: decode decision answer %s: %w", task.TaskID, err)
		}
		row.Answer = &answer
	}
	if row.Kind == "" {
		row.Kind = "approval"
	}
	if row.Status == "" {
		row.Status = DecisionOpen
	}
	return row, nil
}
