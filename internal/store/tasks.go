package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Durable task and submission rows.
//
// This file is the PostgreSQL projection of the task/submission state machine
// implemented in internal/durable. The durable core itself is storage-agnostic
// (it runs on an in-memory store in tests); these rows are what a Keel worker
// persists between processes. Event semantics are untouched: tasks and
// submissions are ordinary bookkeeping rows, never new event kinds.

// TaskRow is one durable task record. Input, Checkpoint and Outcome are JSON
// documents; nil means "absent" (jsonb NULL).
type TaskRow struct {
	TenantID         uuid.UUID
	SessionID        uuid.UUID
	TaskID           uuid.UUID
	Kind             string
	Version          int
	Input            json.RawMessage
	Checkpoint       json.RawMessage
	Outcome          json.RawMessage
	Status           string
	Owner            *uuid.UUID
	Restarts         int
	StartedAt        *time.Time
	EndedAt          *time.Time
	AbortRequested   bool
	Background       bool
	AbandonOnRestart bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// CreateTask inserts a task. It is idempotent by (tenant, session, task): a
// replay of the same creation returns the stored row.
func (s *Store) CreateTask(ctx context.Context, row TaskRow) (TaskRow, error) {
	if row.Version == 0 {
		row.Version = 1
	}
	if row.Status == "" {
		row.Status = "pending"
	}
	if len(row.Input) == 0 {
		row.Input = json.RawMessage(`{}`)
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO tasks (tenant_id, session_id, task_id, kind, version, input, checkpoint,
		                    outcome, status, owner, restarts, started_at, ended_at,
		                    abort_requested, background, abandon_on_restart, state)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,'{}'::jsonb)
		 ON CONFLICT (tenant_id, session_id, task_id) DO NOTHING`,
		row.TenantID, row.SessionID, row.TaskID, row.Kind, row.Version, jsonOrNil(row.Input),
		jsonOrNil(row.Checkpoint), jsonOrNil(row.Outcome), row.Status, row.Owner, row.Restarts,
		row.StartedAt, row.EndedAt, row.AbortRequested, row.Background, row.AbandonOnRestart)
	if err != nil {
		return TaskRow{}, fmt.Errorf("store: create task: %w", err)
	}
	return s.GetTask(ctx, row.TenantID, row.SessionID, row.TaskID)
}

// GetTask reads one task row.
func (s *Store) GetTask(ctx context.Context, tenantID, sessionID, taskID uuid.UUID) (TaskRow, error) {
	var row TaskRow
	var input, checkpoint, outcome []byte
	err := s.pool.QueryRow(ctx,
		`SELECT tenant_id, session_id, task_id, kind, version,
		        COALESCE(input, '{}'::jsonb), checkpoint, outcome, status, owner, restarts,
		        started_at, ended_at, abort_requested, background, abandon_on_restart,
		        created_at, updated_at
		 FROM tasks WHERE tenant_id=$1 AND session_id=$2 AND task_id=$3`,
		tenantID, sessionID, taskID).
		Scan(&row.TenantID, &row.SessionID, &row.TaskID, &row.Kind, &row.Version,
			&input, &checkpoint, &outcome, &row.Status, &row.Owner, &row.Restarts,
			&row.StartedAt, &row.EndedAt, &row.AbortRequested, &row.Background,
			&row.AbandonOnRestart, &row.CreatedAt, &row.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskRow{}, ErrTaskNotFound
	}
	if err != nil {
		return TaskRow{}, fmt.Errorf("store: get task: %w", err)
	}
	row.Input = input
	row.Checkpoint = checkpoint
	row.Outcome = outcome
	return row, nil
}

// ErrTaskNotFound means the task row does not exist for the tenant/session.
var ErrTaskNotFound = errors.New("store: task not found")

// SaveTask replaces every mutable field of an existing task row.
func (s *Store) SaveTask(ctx context.Context, row TaskRow) error {
	if len(row.Input) == 0 {
		row.Input = json.RawMessage(`{}`)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE tasks SET kind=$4, version=$5, input=$6, checkpoint=$7, outcome=$8,
		                 status=$9, owner=$10, restarts=$11, started_at=$12, ended_at=$13,
		                 abort_requested=$14, background=$15, abandon_on_restart=$16, updated_at=now()
		 WHERE tenant_id=$1 AND session_id=$2 AND task_id=$3`,
		row.TenantID, row.SessionID, row.TaskID, row.Kind, row.Version, jsonOrNil(row.Input),
		jsonOrNil(row.Checkpoint), jsonOrNil(row.Outcome), row.Status, row.Owner, row.Restarts,
		row.StartedAt, row.EndedAt, row.AbortRequested, row.Background, row.AbandonOnRestart)
	if err != nil {
		return fmt.Errorf("store: save task: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTaskNotFound
	}
	return nil
}

// ListTasks returns a session's tasks in creation order. liveOnly restricts the
// result to schedulable states.
func (s *Store) ListTasks(ctx context.Context, tenantID, sessionID uuid.UUID, liveOnly bool) ([]TaskRow, error) {
	q := `SELECT tenant_id, session_id, task_id, kind, version,
	             COALESCE(input, '{}'::jsonb), checkpoint, outcome, status, owner, restarts,
	             started_at, ended_at, abort_requested, background, abandon_on_restart,
	             created_at, updated_at
	      FROM tasks WHERE tenant_id=$1 AND session_id=$2`
	if liveOnly {
		q += ` AND status IN ('pending','running','waiting','sleeping','completing')`
	}
	q += ` ORDER BY created_at, task_id`
	rows, err := s.pool.Query(ctx, q, tenantID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: list tasks: %w", err)
	}
	defer rows.Close()
	var out []TaskRow
	for rows.Next() {
		var row TaskRow
		var input, checkpoint, outcome []byte
		if err := rows.Scan(&row.TenantID, &row.SessionID, &row.TaskID, &row.Kind, &row.Version,
			&input, &checkpoint, &outcome, &row.Status, &row.Owner, &row.Restarts,
			&row.StartedAt, &row.EndedAt, &row.AbortRequested, &row.Background,
			&row.AbandonOnRestart, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan task: %w", err)
		}
		row.Input, row.Checkpoint, row.Outcome = input, checkpoint, outcome
		out = append(out, row)
	}
	return out, rows.Err()
}

// SubmissionRow is one durable submission. EntryID/AnswerID are transcript
// entry sequences supplied by the durable core.
type SubmissionRow struct {
	TenantID     uuid.UUID
	SessionID    uuid.UUID
	SubmissionID uuid.UUID
	RequestID    string
	Type         string
	Status       string
	EntryID      *int64
	AnswerID     *int64
	Reason       string
	Detail       json.RawMessage
	Seq          *int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// CreateSubmission inserts a submission. A non-empty RequestID deduplicates per
// session; an empty RequestID gets a synthetic key so rows never collide. The
// returned row is the stored one (an existing row on a replay).
func (s *Store) CreateSubmission(ctx context.Context, row SubmissionRow) (SubmissionRow, bool, error) {
	if row.Type == "" {
		row.Type = "input"
	}
	if row.Status == "" {
		row.Status = "queued"
	}
	requestID := row.RequestID
	if requestID == "" {
		requestID = row.SubmissionID.String()
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO submissions (tenant_id, request_id, session_id, submission_id, status,
		                          type, entry_id, answer_id, reason, detail, seq)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10,$11)
		 ON CONFLICT (tenant_id, session_id, request_id) DO NOTHING`,
		row.TenantID, requestID, row.SessionID, row.SubmissionID, row.Status, row.Type,
		row.EntryID, row.AnswerID, row.Reason, jsonOrNil(row.Detail), row.Seq)
	if err != nil {
		return SubmissionRow{}, false, fmt.Errorf("store: create submission: %w", err)
	}
	created := tag.RowsAffected() == 1
	stored, err := s.GetSubmissionByRequest(ctx, row.TenantID, row.SessionID, requestID)
	if err != nil {
		return SubmissionRow{}, created, err
	}
	return stored, created, nil
}

const submissionColumns = `tenant_id, session_id, submission_id, request_id, COALESCE(type,'input'),
	status, entry_id, answer_id, COALESCE(reason,''), detail, seq, created_at, updated_at`

func scanSubmission(row pgx.Row) (SubmissionRow, error) {
	var out SubmissionRow
	var detail []byte
	err := row.Scan(&out.TenantID, &out.SessionID, &out.SubmissionID, &out.RequestID, &out.Type,
		&out.Status, &out.EntryID, &out.AnswerID, &out.Reason, &detail, &out.Seq,
		&out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return SubmissionRow{}, err
	}
	out.Detail = detail
	return out, nil
}

// GetSubmission reads one submission for a tenant.
func (s *Store) GetSubmission(ctx context.Context, tenantID, submissionID uuid.UUID) (SubmissionRow, error) {
	row, err := scanSubmission(s.pool.QueryRow(ctx,
		`SELECT `+submissionColumns+` FROM submissions
		 WHERE tenant_id=$1 AND submission_id=$2`, tenantID, submissionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return SubmissionRow{}, ErrSubmissionNotFound
	}
	if err != nil {
		return SubmissionRow{}, fmt.Errorf("store: get submission: %w", err)
	}
	return row, nil
}

// GetSubmissionByRequest reads a submission by its per-session request key.
func (s *Store) GetSubmissionByRequest(ctx context.Context, tenantID, sessionID uuid.UUID, requestID string) (SubmissionRow, error) {
	row, err := scanSubmission(s.pool.QueryRow(ctx,
		`SELECT `+submissionColumns+` FROM submissions
		 WHERE tenant_id=$1 AND session_id=$2 AND request_id=$3`, tenantID, sessionID, requestID))
	if errors.Is(err, pgx.ErrNoRows) {
		return SubmissionRow{}, ErrSubmissionNotFound
	}
	if err != nil {
		return SubmissionRow{}, fmt.Errorf("store: get submission by request: %w", err)
	}
	return row, nil
}

// ErrSubmissionNotFound means the submission does not exist for the tenant.
var ErrSubmissionNotFound = errors.New("store: submission not found")

// SaveSubmission replaces every mutable field of an existing submission.
func (s *Store) SaveSubmission(ctx context.Context, row SubmissionRow) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE submissions SET status=$3, type=$4, entry_id=$5, answer_id=$6,
		                       reason=NULLIF($7,''), detail=$8, seq=$9, updated_at=now()
		 WHERE tenant_id=$1 AND submission_id=$2`,
		row.TenantID, row.SubmissionID, row.Status, row.Type, row.EntryID, row.AnswerID,
		row.Reason, jsonOrNil(row.Detail), row.Seq)
	if err != nil {
		return fmt.Errorf("store: save submission: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSubmissionNotFound
	}
	return nil
}

// ListSubmissions returns a session's submissions in creation order.
func (s *Store) ListSubmissions(ctx context.Context, tenantID, sessionID uuid.UUID) ([]SubmissionRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+submissionColumns+` FROM submissions
		 WHERE tenant_id=$1 AND session_id=$2 ORDER BY created_at, submission_id`,
		tenantID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: list submissions: %w", err)
	}
	defer rows.Close()
	var out []SubmissionRow
	for rows.Next() {
		row, err := scanSubmission(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan submission: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func jsonOrNil(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}
