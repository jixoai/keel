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

// --- task-log read models (change keel-harness-v2, spec task-log) -----------
//
// task_cards and task_events are projections of the session ledger, owned by
// internal/tasklog. The store deliberately exposes no "update the card title"
// operation: a projection row is replaced from the log (Rebuild) or advanced by
// the one event that just committed (Upsert/Insert). Nothing here is a second
// source of truth, and a stray row written around the ledger is removed by the
// next rebuild (the consistency test pins that).

// TaskCardRow is one current-state task card.
type TaskCardRow struct {
	TenantID     uuid.UUID
	SessionID    uuid.UUID
	TaskID       uuid.UUID
	ParentTaskID *uuid.UUID
	Title        string
	Status       string
	Origin       string
	OriginSeq    int64
	CreatedSeq   int64
	UpdatedSeq   int64
	ClosedSeq    int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// TaskEventRow is one materialized task.* event. Seq is the ledger sequence.
type TaskEventRow struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	Seq       int64
	TaskID    uuid.UUID
	Kind      string
	Payload   json.RawMessage
	CreatedAt time.Time
}

// ErrTaskCardNotFound means the card does not exist for the tenant scope.
var ErrTaskCardNotFound = errors.New("store: task card not found")

const taskCardColumns = `tenant_id, session_id, task_id, parent_task_id, title, status,
	origin, origin_seq, created_seq, updated_seq, closed_seq, created_at, updated_at`

func scanTaskCardRow(row pgx.Row) (TaskCardRow, error) {
	var out TaskCardRow
	err := row.Scan(&out.TenantID, &out.SessionID, &out.TaskID, &out.ParentTaskID, &out.Title,
		&out.Status, &out.Origin, &out.OriginSeq, &out.CreatedSeq, &out.UpdatedSeq,
		&out.ClosedSeq, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return TaskCardRow{}, err
	}
	return out, nil
}

// GetTaskCardRow reads one card inside a session.
func (s *Store) GetTaskCardRow(ctx context.Context, tenantID, sessionID, taskID uuid.UUID) (TaskCardRow, error) {
	row, err := scanTaskCardRow(s.pool.QueryRow(ctx,
		`SELECT `+taskCardColumns+` FROM task_cards
		 WHERE tenant_id=$1 AND session_id=$2 AND task_id=$3`,
		tenantID, sessionID, taskID))
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskCardRow{}, ErrTaskCardNotFound
	}
	if err != nil {
		return TaskCardRow{}, fmt.Errorf("store: get task card: %w", err)
	}
	return row, nil
}

// GetTaskCardRowByID reads one card by its tenant-wide task id (a parent may
// live in another session, so the parent lookup cannot assume the session).
func (s *Store) GetTaskCardRowByID(ctx context.Context, tenantID, taskID uuid.UUID) (TaskCardRow, error) {
	row, err := scanTaskCardRow(s.pool.QueryRow(ctx,
		`SELECT `+taskCardColumns+` FROM task_cards WHERE tenant_id=$1 AND task_id=$2`,
		tenantID, taskID))
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskCardRow{}, ErrTaskCardNotFound
	}
	if err != nil {
		return TaskCardRow{}, fmt.Errorf("store: get task card by id: %w", err)
	}
	return row, nil
}

// ListTaskCardRows lists cards of one tenant. sessionID/parentTaskID nil mean
// "any"; status "" means "any". The order is stable (updated_at, task_id).
func (s *Store) ListTaskCardRows(ctx context.Context, tenantID uuid.UUID, sessionID, parentTaskID *uuid.UUID, status string) ([]TaskCardRow, error) {
	q := `SELECT ` + taskCardColumns + ` FROM task_cards WHERE tenant_id=$1`
	args := []any{tenantID}
	if sessionID != nil {
		args = append(args, *sessionID)
		q += fmt.Sprintf(" AND session_id=$%d", len(args))
	}
	if parentTaskID != nil {
		args = append(args, *parentTaskID)
		q += fmt.Sprintf(" AND parent_task_id=$%d", len(args))
	}
	if status != "" {
		args = append(args, status)
		q += fmt.Sprintf(" AND status=$%d", len(args))
	}
	q += ` ORDER BY updated_at DESC, task_id`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list task cards: %w", err)
	}
	defer rows.Close()
	var out []TaskCardRow
	for rows.Next() {
		row, err := scanTaskCardRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan task card: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

const childTaskCardColumns = `c.tenant_id, c.session_id, c.task_id, c.parent_task_id, c.title, c.status,
	c.origin, c.origin_seq, c.created_seq, c.updated_seq, c.closed_seq, c.created_at, c.updated_at`

// taskCardQuerier is the query seam both the operator pool and a tenant
// transaction satisfy.
type taskCardQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ListChildTaskCardRows lists the cards whose parentTaskId points at a card of
// parentSessionID. The child row keeps living in the child session; the join is
// the projection relation, not a copy. Ordering is newest activity first, with
// a keyset cursor on (updated_at, task_id).
//
// When the deployment carries the RLS tenant pool the read runs inside a
// tenant-scoped transaction: the tenant predicate and the policy both apply.
func (s *Store) ListChildTaskCardRows(ctx context.Context, tenantID, parentSessionID uuid.UUID, limit int, afterUpdatedAt *time.Time, afterTaskID *uuid.UUID) ([]TaskCardRow, error) {
	if limit <= 0 {
		limit = 1
	}
	q := `SELECT ` + childTaskCardColumns + `
	      FROM task_cards c
	      JOIN task_cards p ON p.tenant_id = c.tenant_id AND p.task_id = c.parent_task_id
	     WHERE c.tenant_id = $1 AND p.session_id = $2`
	args := []any{tenantID, parentSessionID}
	if afterUpdatedAt != nil && afterTaskID != nil {
		args = append(args, *afterUpdatedAt, *afterTaskID)
		q += fmt.Sprintf(" AND (c.updated_at, c.task_id) < ($%d::timestamptz, $%d::uuid)", len(args)-1, len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY c.updated_at DESC, c.task_id DESC LIMIT $%d", len(args))
	collect := func(db taskCardQuerier) ([]TaskCardRow, error) {
		rows, err := db.Query(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []TaskCardRow
		for rows.Next() {
			row, err := scanTaskCardRow(rows)
			if err != nil {
				return nil, err
			}
			out = append(out, row)
		}
		return out, rows.Err()
	}
	if s.RLSEnabled() {
		var out []TaskCardRow
		if err := s.TenantScoped(ctx, tenantID, func(tx TenantTx) error {
			var err error
			out, err = collect(tx)
			return err
		}); err != nil {
			return nil, fmt.Errorf("store: list child task cards: %w", err)
		}
		return out, nil
	}
	out, err := collect(s.pool)
	if err != nil {
		return nil, fmt.Errorf("store: list child task cards: %w", err)
	}
	return out, nil
}

// ListTaskEventRows returns the events of one card in ledger order.
func (s *Store) ListTaskEventRows(ctx context.Context, tenantID, sessionID, taskID uuid.UUID) ([]TaskEventRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, session_id, seq, task_id, kind, payload, created_at
		   FROM task_events WHERE tenant_id=$1 AND session_id=$2 AND task_id=$3
		  ORDER BY seq`, tenantID, sessionID, taskID)
	if err != nil {
		return nil, fmt.Errorf("store: list task events: %w", err)
	}
	defer rows.Close()
	var out []TaskEventRow
	for rows.Next() {
		var row TaskEventRow
		var payload []byte
		if err := rows.Scan(&row.TenantID, &row.SessionID, &row.Seq, &row.TaskID, &row.Kind,
			&payload, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan task event: %w", err)
		}
		row.Payload = payload
		out = append(out, row)
	}
	return out, rows.Err()
}

// UpsertTaskCardRow advances one card. An event out of order (an older seq than
// the row already carries) never rewrites newer state: the row is the
// projection of the highest sequence applied so far.
func (s *Store) UpsertTaskCardRow(ctx context.Context, row TaskCardRow) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO task_cards (tenant_id, session_id, task_id, parent_task_id, title, status,
		                         origin, origin_seq, created_seq, updated_seq, closed_seq)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		 ON CONFLICT (tenant_id, session_id, task_id) DO UPDATE SET
		   parent_task_id = COALESCE(EXCLUDED.parent_task_id, task_cards.parent_task_id),
		   title          = CASE WHEN EXCLUDED.updated_seq >= task_cards.updated_seq THEN EXCLUDED.title ELSE task_cards.title END,
		   status         = CASE WHEN EXCLUDED.updated_seq >= task_cards.updated_seq THEN EXCLUDED.status ELSE task_cards.status END,
		   origin_seq     = CASE WHEN EXCLUDED.updated_seq >= task_cards.updated_seq THEN EXCLUDED.origin_seq ELSE task_cards.origin_seq END,
		   created_seq    = CASE WHEN EXCLUDED.created_seq > 0 AND task_cards.created_seq = 0 THEN EXCLUDED.created_seq ELSE task_cards.created_seq END,
		   updated_seq    = GREATEST(task_cards.updated_seq, EXCLUDED.updated_seq),
		   closed_seq     = GREATEST(task_cards.closed_seq, EXCLUDED.closed_seq),
		   updated_at     = now()`,
		row.TenantID, row.SessionID, row.TaskID, row.ParentTaskID, row.Title, row.Status,
		row.Origin, row.OriginSeq, row.CreatedSeq, row.UpdatedSeq, row.ClosedSeq)
	if err != nil {
		return fmt.Errorf("store: upsert task card: %w", err)
	}
	return nil
}

// InsertTaskEventRow records the projection row of one already-committed
// ledger event. Repeats are no-ops (the primary key is the ledger sequence).
func (s *Store) InsertTaskEventRow(ctx context.Context, row TaskEventRow) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO task_events (tenant_id, session_id, seq, task_id, kind, payload)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (tenant_id, session_id, seq) DO NOTHING`,
		row.TenantID, row.SessionID, row.Seq, row.TaskID, row.Kind, jsonOrNil(row.Payload))
	if err != nil {
		return fmt.Errorf("store: insert task event: %w", err)
	}
	return nil
}

// ReplaceTaskSessionProjection swaps one session's projection for a fresh one in
// a single transaction: delete, then insert the rebuilt rows. Rebuilds are
// therefore idempotent (running twice yields the same rows), and a crash can
// leave either the old or the new projection, never a mixture.
func (s *Store) ReplaceTaskSessionProjection(ctx context.Context, tenantID, sessionID uuid.UUID, cards []TaskCardRow, events []TaskEventRow) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin task projection tx: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`DELETE FROM task_events WHERE tenant_id=$1 AND session_id=$2`, tenantID, sessionID); err != nil {
		return fmt.Errorf("store: clear task events: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM task_cards WHERE tenant_id=$1 AND session_id=$2`, tenantID, sessionID); err != nil {
		return fmt.Errorf("store: clear task cards: %w", err)
	}
	batch := &pgx.Batch{}
	for _, card := range cards {
		batch.Queue(
			`INSERT INTO task_cards (tenant_id, session_id, task_id, parent_task_id, title, status,
			                         origin, origin_seq, created_seq, updated_seq, closed_seq)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			card.TenantID, card.SessionID, card.TaskID, card.ParentTaskID, card.Title, card.Status,
			card.Origin, card.OriginSeq, card.CreatedSeq, card.UpdatedSeq, card.ClosedSeq)
	}
	for _, event := range events {
		batch.Queue(
			`INSERT INTO task_events (tenant_id, session_id, seq, task_id, kind, payload)
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			event.TenantID, event.SessionID, event.Seq, event.TaskID, event.Kind, jsonOrNil(event.Payload))
	}
	if batch.Len() > 0 {
		results := tx.SendBatch(ctx, batch)
		for i := 0; i < batch.Len(); i++ {
			if _, err := results.Exec(); err != nil {
				results.Close()
				return fmt.Errorf("store: rewrite task projection: %w", err)
			}
		}
		if err := results.Close(); err != nil {
			return fmt.Errorf("store: close task projection batch: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit task projection: %w", err)
	}
	return nil
}

// CountTaskEvents returns how many task events the projection holds for a
// session (the consistency test compares it with the ledger count).
func (s *Store) CountTaskEvents(ctx context.Context, tenantID, sessionID uuid.UUID) (int, error) {
	var count int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM task_events WHERE tenant_id=$1 AND session_id=$2`,
		tenantID, sessionID).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count task events: %w", err)
	}
	return count, nil
}

func jsonOrNil(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}
