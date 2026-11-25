package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/superagent"
)

// Super-agent persistence (change keel-collab-superagent).
//
// The methods below make *Store implement superagent.Store (which embeds
// collab.WorkStore) and collab.SessionIndex. They are deliberately plain SQL
// over the collab_* tables in schema.sql: the generic core shape stays generic,
// and no super-agent-only column is added to sessions or tasks.
var _ superagent.Store = (*Store)(nil)
var _ collab.SessionIndex = (*Store)(nil)

// --- WorkItem -------------------------------------------------------------

// CreateWorkItem inserts one work item. A repeated non-empty idempotency key
// returns the original row, which is what makes a replayed Source event a
// no-op before the seen-event log is even consulted.
func (s *Store) CreateWorkItem(ctx context.Context, item collab.WorkItem) (collab.WorkItem, error) {
	if item.TenantID == uuid.Nil || item.ID == uuid.Nil {
		return collab.WorkItem{}, collabError(collab.ErrTenantRequired, collab.CodeTenantRequired, "create_work_item", "tenant and work item ids are required")
	}
	source, scope, limits, err := workItemJSON(item)
	if err != nil {
		return collab.WorkItem{}, err
	}
	now := time.Now().UTC()
	if item.CreatedAt.IsZero() {
		item.CreatedAt = now
	}
	item.UpdatedAt = now
	row, err := scanWorkItem(s.pool.QueryRow(ctx, `
		INSERT INTO collab_work_items
		    (tenant_id, work_item_id, mode_id, title, input, source, scope, limits,
		     status, idempotency_key, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5::jsonb,$6::jsonb,$7::jsonb,$8::jsonb,$9,$10,$11,$12)
		ON CONFLICT (tenant_id, mode_id, idempotency_key)
		    WHERE idempotency_key <> '' DO NOTHING
		RETURNING tenant_id, work_item_id, mode_id, title, input, source, scope, limits,
		          status, idempotency_key, created_at, updated_at`,
		item.TenantID, item.ID, item.ModeID, item.Title, rawInput(item.Input), source, scope, limits,
		string(item.Status), item.IdempotencyKey, item.CreatedAt, item.UpdatedAt))
	if errors.Is(err, pgx.ErrNoRows) && item.IdempotencyKey != "" {
		return s.workItemByIdempotency(ctx, item.TenantID, item.ModeID, item.IdempotencyKey)
	}
	if err != nil {
		return collab.WorkItem{}, fmt.Errorf("store: create work item: %w", err)
	}
	return row, nil
}

func (s *Store) workItemByIdempotency(ctx context.Context, tenantID uuid.UUID, modeID, key string) (collab.WorkItem, error) {
	row, err := scanWorkItem(s.pool.QueryRow(ctx, `
		SELECT tenant_id, work_item_id, mode_id, title, input, source, scope, limits,
		       status, idempotency_key, created_at, updated_at
		  FROM collab_work_items
		 WHERE tenant_id = $1 AND mode_id = $2 AND idempotency_key = $3`, tenantID, modeID, key))
	if err != nil {
		return collab.WorkItem{}, fmt.Errorf("store: read idempotent work item: %w", err)
	}
	return row, nil
}

// GetWorkItem reads one work item. A foreign tenant's row is not found.
func (s *Store) GetWorkItem(ctx context.Context, tenantID, workItemID uuid.UUID) (collab.WorkItem, error) {
	row, err := scanWorkItem(s.pool.QueryRow(ctx, `
		SELECT tenant_id, work_item_id, mode_id, title, input, source, scope, limits,
		       status, idempotency_key, created_at, updated_at
		  FROM collab_work_items WHERE tenant_id = $1 AND work_item_id = $2`, tenantID, workItemID))
	if errors.Is(err, pgx.ErrNoRows) {
		return collab.WorkItem{}, collabError(collab.ErrWorkItemNotFound, collab.CodeWorkItemNotFound, "get_work_item", "work item not found")
	}
	if err != nil {
		return collab.WorkItem{}, fmt.Errorf("store: get work item: %w", err)
	}
	return row, nil
}

// ListWorkItems lists one tenant's work items, oldest first.
func (s *Store) ListWorkItems(ctx context.Context, tenantID uuid.UUID, filter collab.WorkFilter) ([]collab.WorkItem, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tenant_id, work_item_id, mode_id, title, input, source, scope, limits,
		       status, idempotency_key, created_at, updated_at
		  FROM collab_work_items
		 WHERE tenant_id = $1
		   AND ($2 = '' OR mode_id = $2)
		   AND ($3 = '' OR status = $3)
		 ORDER BY created_at ASC, work_item_id ASC`,
		tenantID, string(filter.ModeID), string(filter.Status))
	if err != nil {
		return nil, fmt.Errorf("store: list work items: %w", err)
	}
	defer rows.Close()
	out := make([]collab.WorkItem, 0)
	for rows.Next() {
		item, err := scanWorkItem(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan work item: %w", err)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// UpdateWorkItem replaces the mutable fields of a work item.
func (s *Store) UpdateWorkItem(ctx context.Context, item collab.WorkItem) (collab.WorkItem, error) {
	source, scope, limits, err := workItemJSON(item)
	if err != nil {
		return collab.WorkItem{}, err
	}
	row, err := scanWorkItem(s.pool.QueryRow(ctx, `
		UPDATE collab_work_items
		   SET title = $3, input = $4::jsonb, source = $5::jsonb, scope = $6::jsonb,
		       limits = $7::jsonb, status = $8, updated_at = now()
		 WHERE tenant_id = $1 AND work_item_id = $2
		RETURNING tenant_id, work_item_id, mode_id, title, input, source, scope, limits,
		          status, idempotency_key, created_at, updated_at`,
		item.TenantID, item.ID, item.Title, rawInput(item.Input), source, scope, limits, string(item.Status)))
	if errors.Is(err, pgx.ErrNoRows) {
		return collab.WorkItem{}, collabError(collab.ErrWorkItemNotFound, collab.CodeWorkItemNotFound, "update_work_item", "work item not found")
	}
	if err != nil {
		return collab.WorkItem{}, fmt.Errorf("store: update work item: %w", err)
	}
	return row, nil
}

type workItemScanner interface {
	Scan(dest ...any) error
}

func scanWorkItem(row workItemScanner) (collab.WorkItem, error) {
	var item collab.WorkItem
	var input, source, scope, limits []byte
	var status string
	if err := row.Scan(&item.TenantID, &item.ID, &item.ModeID, &item.Title, &input, &source, &scope, &limits,
		&status, &item.IdempotencyKey, &item.CreatedAt, &item.UpdatedAt); err != nil {
		return collab.WorkItem{}, err
	}
	item.Status = collab.WorkItemStatus(status)
	item.Input = append(json.RawMessage(nil), input...)
	_ = json.Unmarshal(source, &item.Source)
	_ = json.Unmarshal(scope, &item.Scope)
	_ = json.Unmarshal(limits, &item.Limits)
	return item, nil
}

func workItemJSON(item collab.WorkItem) ([]byte, []byte, []byte, error) {
	source, err := json.Marshal(item.Source)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("store: encode work item source: %w", err)
	}
	scope, err := json.Marshal(item.Scope)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("store: encode work item scope: %w", err)
	}
	limits, err := json.Marshal(item.Limits)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("store: encode work item limits: %w", err)
	}
	return source, scope, limits, nil
}

func rawInput(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// --- Assignment -----------------------------------------------------------

// CreateAssignment inserts one assignment. A repeated idempotency key returns
// the original row.
func (s *Store) CreateAssignment(ctx context.Context, assignment collab.Assignment) (collab.Assignment, error) {
	if assignment.TenantID == uuid.Nil || assignment.ID == uuid.Nil {
		return collab.Assignment{}, collabError(collab.ErrTenantRequired, collab.CodeTenantRequired, "create_assignment", "tenant and assignment ids are required")
	}
	now := time.Now().UTC()
	if assignment.CreatedAt.IsZero() {
		assignment.CreatedAt = now
	}
	assignment.UpdatedAt = now
	row, err := scanAssignment(s.pool.QueryRow(ctx, `
		INSERT INTO collab_assignments
		    (tenant_id, assignment_id, work_item_id, mode_id, bot_id, session_id, caller_session_id,
		     status, title, input, result, error, idempotency_key, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11::jsonb,$12,$13,$14,$15)
		ON CONFLICT (tenant_id, work_item_id, idempotency_key)
		    WHERE idempotency_key <> '' DO NOTHING
		RETURNING tenant_id, assignment_id, work_item_id, mode_id, bot_id, session_id, caller_session_id,
		          status, title, input, result, error, idempotency_key, created_at, updated_at`,
		assignment.TenantID, assignment.ID, assignment.WorkItemID, assignment.ModeID, assignment.BotID,
		nullableUUID(assignment.SessionID), nullableUUID(assignment.CallerSessionID), string(assignment.Status),
		assignment.Title, rawInput(assignment.Input), rawInput(assignment.Result), assignment.Error,
		assignment.IdempotencyKey, assignment.CreatedAt, assignment.UpdatedAt))
	if errors.Is(err, pgx.ErrNoRows) && assignment.IdempotencyKey != "" {
		return s.assignmentByIdempotency(ctx, assignment.TenantID, assignment.WorkItemID, assignment.IdempotencyKey)
	}
	if err != nil {
		return collab.Assignment{}, fmt.Errorf("store: create assignment: %w", err)
	}
	return row, nil
}

func (s *Store) assignmentByIdempotency(ctx context.Context, tenantID, workItemID uuid.UUID, key string) (collab.Assignment, error) {
	row, err := scanAssignment(s.pool.QueryRow(ctx, `
		SELECT tenant_id, assignment_id, work_item_id, mode_id, bot_id, session_id, caller_session_id,
		       status, title, input, result, error, idempotency_key, created_at, updated_at
		  FROM collab_assignments
		 WHERE tenant_id = $1 AND work_item_id = $2 AND idempotency_key = $3`, tenantID, workItemID, key))
	if err != nil {
		return collab.Assignment{}, fmt.Errorf("store: read idempotent assignment: %w", err)
	}
	return row, nil
}

// GetAssignment reads one assignment.
func (s *Store) GetAssignment(ctx context.Context, tenantID, assignmentID uuid.UUID) (collab.Assignment, error) {
	row, err := scanAssignment(s.pool.QueryRow(ctx, `
		SELECT tenant_id, assignment_id, work_item_id, mode_id, bot_id, session_id, caller_session_id,
		       status, title, input, result, error, idempotency_key, created_at, updated_at
		  FROM collab_assignments WHERE tenant_id = $1 AND assignment_id = $2`, tenantID, assignmentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return collab.Assignment{}, collabError(collab.ErrAssignmentNotFound, collab.CodeAssignmentNotFound, "get_assignment", "assignment not found")
	}
	if err != nil {
		return collab.Assignment{}, fmt.Errorf("store: get assignment: %w", err)
	}
	return row, nil
}

// ListAssignments lists one work item's assignments, oldest first.
func (s *Store) ListAssignments(ctx context.Context, tenantID, workItemID uuid.UUID) ([]collab.Assignment, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tenant_id, assignment_id, work_item_id, mode_id, bot_id, session_id, caller_session_id,
		       status, title, input, result, error, idempotency_key, created_at, updated_at
		  FROM collab_assignments
		 WHERE tenant_id = $1 AND work_item_id = $2
		 ORDER BY created_at ASC, assignment_id ASC`, tenantID, workItemID)
	if err != nil {
		return nil, fmt.Errorf("store: list assignments: %w", err)
	}
	defer rows.Close()
	out := make([]collab.Assignment, 0)
	for rows.Next() {
		assignment, err := scanAssignment(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan assignment: %w", err)
		}
		out = append(out, assignment)
	}
	return out, rows.Err()
}

// UpdateAssignment replaces the mutable fields of an assignment.
func (s *Store) UpdateAssignment(ctx context.Context, assignment collab.Assignment) (collab.Assignment, error) {
	row, err := scanAssignment(s.pool.QueryRow(ctx, `
		UPDATE collab_assignments
		   SET bot_id = $3, session_id = $4, caller_session_id = $5, status = $6, title = $7,
		       input = $8::jsonb, result = $9::jsonb, error = $10, updated_at = now()
		 WHERE tenant_id = $1 AND assignment_id = $2
		RETURNING tenant_id, assignment_id, work_item_id, mode_id, bot_id, session_id, caller_session_id,
		          status, title, input, result, error, idempotency_key, created_at, updated_at`,
		assignment.TenantID, assignment.ID, assignment.BotID, nullableUUID(assignment.SessionID),
		nullableUUID(assignment.CallerSessionID), string(assignment.Status), assignment.Title,
		rawInput(assignment.Input), rawInput(assignment.Result), assignment.Error))
	if errors.Is(err, pgx.ErrNoRows) {
		return collab.Assignment{}, collabError(collab.ErrAssignmentNotFound, collab.CodeAssignmentNotFound, "update_assignment", "assignment not found")
	}
	if err != nil {
		return collab.Assignment{}, fmt.Errorf("store: update assignment: %w", err)
	}
	return row, nil
}

func scanAssignment(row workItemScanner) (collab.Assignment, error) {
	var assignment collab.Assignment
	var sessionID, callerSessionID uuid.NullUUID
	var input, result []byte
	var status string
	if err := row.Scan(&assignment.TenantID, &assignment.ID, &assignment.WorkItemID, &assignment.ModeID,
		&assignment.BotID, &sessionID, &callerSessionID, &status, &assignment.Title,
		&input, &result, &assignment.Error, &assignment.IdempotencyKey,
		&assignment.CreatedAt, &assignment.UpdatedAt); err != nil {
		return collab.Assignment{}, err
	}
	assignment.Status = collab.AssignmentStatus(status)
	if sessionID.Valid {
		assignment.SessionID = sessionID.UUID
	}
	if callerSessionID.Valid {
		assignment.CallerSessionID = callerSessionID.UUID
	}
	assignment.Input = append(json.RawMessage(nil), input...)
	assignment.Result = append(json.RawMessage(nil), result...)
	return assignment, nil
}

// --- Attention ------------------------------------------------------------

// CreateAttention inserts one attention row. A repeated request id returns the
// original row.
func (s *Store) CreateAttention(ctx context.Context, attention collab.Attention) (collab.Attention, error) {
	if attention.TenantID == uuid.Nil || attention.ID == uuid.Nil {
		return collab.Attention{}, collabError(collab.ErrTenantRequired, collab.CodeTenantRequired, "create_attention", "tenant and attention ids are required")
	}
	options, err := json.Marshal(attention.Options)
	if err != nil {
		return collab.Attention{}, fmt.Errorf("store: encode attention options: %w", err)
	}
	answer, err := answerJSON(attention.Answer)
	if err != nil {
		return collab.Attention{}, err
	}
	row, err := scanAttention(s.pool.QueryRow(ctx, `
		INSERT INTO collab_attentions
		    (tenant_id, attention_id, work_item_id, assignment_id, mode_id, session_id, request_id,
		     kind, prompt, options, allow_free_text, route, channel, status, answer, created_at, deadline)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11,$12,$13,$14,$15::jsonb,$16,$17)
		ON CONFLICT (tenant_id, work_item_id, request_id)
		    WHERE request_id <> '' DO NOTHING
		RETURNING tenant_id, attention_id, work_item_id, assignment_id, mode_id, session_id, request_id,
		          kind, prompt, options, allow_free_text, route, channel, status, answer, created_at, deadline`,
		attention.TenantID, attention.ID, attention.WorkItemID, nullableUUID(attention.AssignmentID),
		attention.ModeID, nullableUUID(attention.SessionID), attention.RequestID, attention.Kind,
		attention.Prompt, string(options), attention.AllowFreeText, string(attention.Route),
		attention.Channel, string(attention.Status), rawInput(answer), attention.CreatedAt, attention.Deadline))
	if errors.Is(err, pgx.ErrNoRows) && attention.RequestID != "" {
		return s.attentionByRequest(ctx, attention.TenantID, attention.WorkItemID, attention.RequestID)
	}
	if err != nil {
		return collab.Attention{}, fmt.Errorf("store: create attention: %w", err)
	}
	return row, nil
}

func (s *Store) attentionByRequest(ctx context.Context, tenantID, workItemID uuid.UUID, requestID string) (collab.Attention, error) {
	row, err := scanAttention(s.pool.QueryRow(ctx, `
		SELECT tenant_id, attention_id, work_item_id, assignment_id, mode_id, session_id, request_id,
		       kind, prompt, options, allow_free_text, route, channel, status, answer, created_at, deadline
		  FROM collab_attentions
		 WHERE tenant_id = $1 AND work_item_id = $2 AND request_id = $3`, tenantID, workItemID, requestID))
	if err != nil {
		return collab.Attention{}, fmt.Errorf("store: read idempotent attention: %w", err)
	}
	return row, nil
}

// UpdateAttention replaces the mutable fields of an attention.
func (s *Store) UpdateAttention(ctx context.Context, attention collab.Attention) (collab.Attention, error) {
	options, err := json.Marshal(attention.Options)
	if err != nil {
		return collab.Attention{}, fmt.Errorf("store: encode attention options: %w", err)
	}
	answer, err := answerJSON(attention.Answer)
	if err != nil {
		return collab.Attention{}, err
	}
	row, err := scanAttention(s.pool.QueryRow(ctx, `
		UPDATE collab_attentions
		   SET options = $3::jsonb, status = $4, answer = $5::jsonb
		 WHERE tenant_id = $1 AND attention_id = $2
		RETURNING tenant_id, attention_id, work_item_id, assignment_id, mode_id, session_id, request_id,
		          kind, prompt, options, allow_free_text, route, channel, status, answer, created_at, deadline`,
		attention.TenantID, attention.ID, string(options), string(attention.Status), rawInput(answer)))
	if errors.Is(err, pgx.ErrNoRows) {
		return collab.Attention{}, collabError(collab.ErrAttentionNotFound, collab.CodeAttentionNotFound, "update_attention", "attention not found")
	}
	if err != nil {
		return collab.Attention{}, fmt.Errorf("store: update attention: %w", err)
	}
	return row, nil
}

// GetAttention reads one attention.
func (s *Store) GetAttention(ctx context.Context, tenantID, attentionID uuid.UUID) (collab.Attention, error) {
	row, err := scanAttention(s.pool.QueryRow(ctx, `
		SELECT tenant_id, attention_id, work_item_id, assignment_id, mode_id, session_id, request_id,
		       kind, prompt, options, allow_free_text, route, channel, status, answer, created_at, deadline
		  FROM collab_attentions WHERE tenant_id = $1 AND attention_id = $2`, tenantID, attentionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return collab.Attention{}, collabError(collab.ErrAttentionNotFound, collab.CodeAttentionNotFound, "get_attention", "attention not found")
	}
	if err != nil {
		return collab.Attention{}, fmt.Errorf("store: get attention: %w", err)
	}
	return row, nil
}

// ListAttentions lists one work item's attention requests.
func (s *Store) ListAttentions(ctx context.Context, tenantID, workItemID uuid.UUID) ([]collab.Attention, error) {
	return s.listAttentions(ctx, `
		SELECT tenant_id, attention_id, work_item_id, assignment_id, mode_id, session_id, request_id,
		       kind, prompt, options, allow_free_text, route, channel, status, answer, created_at, deadline
		  FROM collab_attentions
		 WHERE tenant_id = $1 AND work_item_id = $2
		 ORDER BY created_at ASC, attention_id ASC`, tenantID, workItemID)
}

// ListOpenAttentions lists every open attention of one tenant (the inbox).
func (s *Store) ListOpenAttentions(ctx context.Context, tenantID uuid.UUID) ([]collab.Attention, error) {
	return s.listAttentions(ctx, `
		SELECT tenant_id, attention_id, work_item_id, assignment_id, mode_id, session_id, request_id,
		       kind, prompt, options, allow_free_text, route, channel, status, answer, created_at, deadline
		  FROM collab_attentions
		 WHERE tenant_id = $1 AND status = 'open'
		 ORDER BY created_at ASC, attention_id ASC`, tenantID)
}

func (s *Store) listAttentions(ctx context.Context, query string, args ...any) ([]collab.Attention, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list attentions: %w", err)
	}
	defer rows.Close()
	out := make([]collab.Attention, 0)
	for rows.Next() {
		attention, err := scanAttention(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan attention: %w", err)
		}
		out = append(out, attention)
	}
	return out, rows.Err()
}

func scanAttention(row workItemScanner) (collab.Attention, error) {
	var attention collab.Attention
	var assignmentID, sessionID uuid.NullUUID
	var options, answer []byte
	var route, status string
	if err := row.Scan(&attention.TenantID, &attention.ID, &attention.WorkItemID, &assignmentID,
		&attention.ModeID, &sessionID, &attention.RequestID, &attention.Kind, &attention.Prompt,
		&options, &attention.AllowFreeText, &route, &attention.Channel,
		&status, &answer, &attention.CreatedAt, &attention.Deadline); err != nil {
		return collab.Attention{}, err
	}
	attention.Route = collab.AttentionRoute(route)
	attention.Status = collab.AttentionStatus(status)
	if assignmentID.Valid {
		attention.AssignmentID = assignmentID.UUID
	}
	if sessionID.Valid {
		attention.SessionID = sessionID.UUID
	}
	if len(options) > 0 {
		_ = json.Unmarshal(options, &attention.Options)
	}
	if len(answer) > 0 {
		var decoded collab.AttentionAnswer
		if err := json.Unmarshal(answer, &decoded); err == nil {
			attention.Answer = &decoded
		}
	}
	return attention, nil
}

func answerJSON(answer *collab.AttentionAnswer) ([]byte, error) {
	if answer == nil {
		return nil, nil
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		return nil, fmt.Errorf("store: encode attention answer: %w", err)
	}
	return raw, nil
}

// ListAssignmentsByStatus lists the tenant's assignments in the named states.
// An empty status list returns every assignment.
func (s *Store) ListAssignmentsByStatus(ctx context.Context, tenantID uuid.UUID, statuses ...collab.AssignmentStatus) ([]collab.Assignment, error) {
	values := make([]string, 0, len(statuses))
	for _, status := range statuses {
		values = append(values, string(status))
	}
	rows, err := s.pool.Query(ctx, `
		SELECT tenant_id, assignment_id, work_item_id, mode_id, bot_id, session_id, caller_session_id,
		       status, title, input, result, error, idempotency_key, created_at, updated_at
		  FROM collab_assignments
		 WHERE tenant_id = $1 AND (cardinality($2::text[]) = 0 OR status = ANY($2::text[]))
		 ORDER BY created_at ASC, assignment_id ASC`, tenantID, values)
	if err != nil {
		return nil, fmt.Errorf("store: list assignments by status: %w", err)
	}
	defer rows.Close()
	out := make([]collab.Assignment, 0)
	for rows.Next() {
		assignment, err := scanAssignment(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan assignment: %w", err)
		}
		out = append(out, assignment)
	}
	return out, rows.Err()
}

// --- Links ----------------------------------------------------------------

// AddLink inserts one semantic link.
func (s *Store) AddLink(ctx context.Context, link collab.Link) (collab.Link, error) {
	if link.TenantID == uuid.Nil || link.ID == uuid.Nil {
		return collab.Link{}, collabError(collab.ErrTenantRequired, collab.CodeTenantRequired, "add_link", "tenant and link ids are required")
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO collab_links
		    (tenant_id, link_id, work_item_id, kind, from_type, from_id, to_type, to_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (tenant_id, link_id) DO NOTHING`,
		link.TenantID, link.ID, nullableUUID(link.WorkItemID), string(link.Kind),
		string(link.From.Type), link.From.ID, string(link.To.Type), link.To.ID, link.CreatedAt); err != nil {
		return collab.Link{}, fmt.Errorf("store: add link: %w", err)
	}
	return link, nil
}

// ListLinks lists one work item's links.
func (s *Store) ListLinks(ctx context.Context, tenantID, workItemID uuid.UUID) ([]collab.Link, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tenant_id, link_id, work_item_id, kind, from_type, from_id, to_type, to_id, created_at
		  FROM collab_links
		 WHERE tenant_id = $1 AND work_item_id = $2
		 ORDER BY created_at ASC, link_id ASC`, tenantID, workItemID)
	if err != nil {
		return nil, fmt.Errorf("store: list links: %w", err)
	}
	defer rows.Close()
	out := make([]collab.Link, 0)
	for rows.Next() {
		var link collab.Link
		var itemID uuid.NullUUID
		if err := rows.Scan(&link.TenantID, &link.ID, &itemID, &link.Kind,
			&link.From.Type, &link.From.ID, &link.To.Type, &link.To.ID, &link.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan link: %w", err)
		}
		if itemID.Valid {
			link.WorkItemID = itemID.UUID
		}
		out = append(out, link)
	}
	return out, rows.Err()
}

// --- Source inbox and cursor ---------------------------------------------

// SourceEventSeen reports whether the event was consumed.
func (s *Store) SourceEventSeen(ctx context.Context, tenantID uuid.UUID, sourceID, eventID string) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM collab_source_events
			 WHERE tenant_id = $1 AND source_id = $2 AND event_id = $3
		)`, tenantID, sourceID, eventID).Scan(&exists); err != nil {
		return false, fmt.Errorf("store: source event seen: %w", err)
	}
	return exists, nil
}

// RecordSourceEvent inserts the seen fact once. A duplicate is a no-op.
func (s *Store) RecordSourceEvent(ctx context.Context, event superagent.SeenEvent) error {
	if event.TenantID == uuid.Nil || event.SourceID == "" || event.EventID == "" {
		return collabError(collab.ErrTenantRequired, collab.CodeTenantRequired, "record_source_event", "tenant, source and event ids are required")
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO collab_source_events
		    (tenant_id, source_id, event_id, kind, actor_id, thread_id, payload, decision, work_item_id, received_at, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$10,$11)
		ON CONFLICT (tenant_id, source_id, event_id) DO NOTHING`,
		event.TenantID, event.SourceID, event.EventID, event.Kind, event.ActorID, event.ThreadID,
		rawInput(event.Payload), event.Decision, nullableUUID(event.WorkItemID), event.ReceivedAt, event.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: record source event: %w", err)
	}
	return nil
}

// GetSourceCursor returns the durable read position ("" when none exists).
func (s *Store) GetSourceCursor(ctx context.Context, tenantID uuid.UUID, sourceID string) (string, error) {
	var cursor string
	err := s.pool.QueryRow(ctx, `
		SELECT cursor FROM collab_source_cursors WHERE tenant_id = $1 AND source_id = $2`,
		tenantID, sourceID).Scan(&cursor)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: get source cursor: %w", err)
	}
	return cursor, nil
}

// SetSourceCursor upserts the read position.
func (s *Store) SetSourceCursor(ctx context.Context, tenantID uuid.UUID, sourceID, cursor string) error {
	if tenantID == uuid.Nil || sourceID == "" {
		return collabError(collab.ErrTenantRequired, collab.CodeTenantRequired, "set_source_cursor", "tenant and source ids are required")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO collab_source_cursors (tenant_id, source_id, cursor, updated_at)
		VALUES ($1,$2,$3,now())
		ON CONFLICT (tenant_id, source_id) DO UPDATE SET cursor = EXCLUDED.cursor, updated_at = now()`,
		tenantID, sourceID, cursor)
	if err != nil {
		return fmt.Errorf("store: set source cursor: %w", err)
	}
	return nil
}

// --- Subscriptions --------------------------------------------------------

// CreateSubscription adds a Ticket ↔ Thread subscription. The unique thread key
// makes a repeated call return the original row.
func (s *Store) CreateSubscription(ctx context.Context, subscription superagent.Subscription) (superagent.Subscription, error) {
	if subscription.TenantID == uuid.Nil || subscription.WorkItemID == uuid.Nil || !subscription.Thread.Valid() {
		return superagent.Subscription{}, collabError(collab.ErrTenantRequired, collab.CodeTenantRequired, "create_subscription", "tenant, work item and thread are required")
	}
	if subscription.ID == uuid.Nil {
		subscription.ID = uuid.New()
	}
	now := time.Now().UTC()
	if subscription.CreatedAt.IsZero() {
		subscription.CreatedAt = now
	}
	subscription.UpdatedAt = now
	row := s.pool.QueryRow(ctx, `
		INSERT INTO collab_subscriptions
		    (tenant_id, subscription_id, work_item_id, source_id, thread_kind, thread_id, active, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (tenant_id, work_item_id, source_id, thread_id) DO NOTHING
		RETURNING tenant_id, subscription_id, work_item_id, source_id, thread_kind, thread_id, active, created_at, updated_at`,
		subscription.TenantID, subscription.ID, subscription.WorkItemID, subscription.Thread.Source,
		subscription.Thread.Kind, subscription.Thread.ID, subscription.Active, subscription.CreatedAt, subscription.UpdatedAt)
	stored, err := scanSubscription(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.subscriptionByThread(ctx, subscription.TenantID, subscription.WorkItemID, subscription.Thread)
	}
	if err != nil {
		return superagent.Subscription{}, fmt.Errorf("store: create subscription: %w", err)
	}
	return stored, nil
}

func (s *Store) subscriptionByThread(ctx context.Context, tenantID, workItemID uuid.UUID, thread superagent.ThreadRef) (superagent.Subscription, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT tenant_id, subscription_id, work_item_id, source_id, thread_kind, thread_id, active, created_at, updated_at
		  FROM collab_subscriptions
		 WHERE tenant_id = $1 AND work_item_id = $2 AND source_id = $3 AND thread_id = $4`,
		tenantID, workItemID, thread.Source, thread.ID)
	stored, err := scanSubscription(row)
	if err != nil {
		return superagent.Subscription{}, fmt.Errorf("store: read idempotent subscription: %w", err)
	}
	return stored, nil
}

// UpdateSubscription replaces the active state of a subscription.
func (s *Store) UpdateSubscription(ctx context.Context, subscription superagent.Subscription) (superagent.Subscription, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE collab_subscriptions
		   SET active = $3, thread_kind = $4, updated_at = now()
		 WHERE tenant_id = $1 AND subscription_id = $2
		RETURNING tenant_id, subscription_id, work_item_id, source_id, thread_kind, thread_id, active, created_at, updated_at`,
		subscription.TenantID, subscription.ID, subscription.Active, subscription.Thread.Kind)
	stored, err := scanSubscription(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return superagent.Subscription{}, fmt.Errorf("store: subscription not found")
	}
	if err != nil {
		return superagent.Subscription{}, fmt.Errorf("store: update subscription: %w", err)
	}
	return stored, nil
}

// ListSubscriptions lists one ticket's subscriptions.
func (s *Store) ListSubscriptions(ctx context.Context, tenantID, workItemID uuid.UUID) ([]superagent.Subscription, error) {
	return s.listSubscriptions(ctx, `
		SELECT tenant_id, subscription_id, work_item_id, source_id, thread_kind, thread_id, active, created_at, updated_at
		  FROM collab_subscriptions
		 WHERE tenant_id = $1 AND work_item_id = $2
		 ORDER BY created_at ASC, subscription_id ASC`, tenantID, workItemID)
}

// ListThreadSubscriptions lists active subscriptions to one thread.
func (s *Store) ListThreadSubscriptions(ctx context.Context, tenantID uuid.UUID, thread superagent.ThreadRef) ([]superagent.Subscription, error) {
	return s.listSubscriptions(ctx, `
		SELECT tenant_id, subscription_id, work_item_id, source_id, thread_kind, thread_id, active, created_at, updated_at
		  FROM collab_subscriptions
		 WHERE tenant_id = $1 AND source_id = $2 AND thread_id = $3 AND active
		 ORDER BY created_at ASC, subscription_id ASC`, tenantID, thread.Source, thread.ID)
}

func (s *Store) listSubscriptions(ctx context.Context, query string, args ...any) ([]superagent.Subscription, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list subscriptions: %w", err)
	}
	defer rows.Close()
	out := make([]superagent.Subscription, 0)
	for rows.Next() {
		subscription, err := scanSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, subscription)
	}
	return out, rows.Err()
}

func scanSubscription(row workItemScanner) (superagent.Subscription, error) {
	var subscription superagent.Subscription
	if err := row.Scan(&subscription.TenantID, &subscription.ID, &subscription.WorkItemID,
		&subscription.Thread.Source, &subscription.Thread.Kind, &subscription.Thread.ID,
		&subscription.Active, &subscription.CreatedAt, &subscription.UpdatedAt); err != nil {
		return superagent.Subscription{}, fmt.Errorf("store: scan subscription: %w", err)
	}
	return subscription, nil
}

// --- Outbox ---------------------------------------------------------------

// EnqueueOutbox inserts one durable message. A repeated idempotency key returns
// the original row, including a terminal one.
func (s *Store) EnqueueOutbox(ctx context.Context, message superagent.OutboxMessage) (superagent.OutboxMessage, error) {
	if message.TenantID == uuid.Nil || message.ChannelID == "" {
		return superagent.OutboxMessage{}, collabError(collab.ErrTenantRequired, collab.CodeTenantRequired, "enqueue_outbox", "tenant and channel are required")
	}
	if message.ID == uuid.Nil {
		message.ID = uuid.New()
	}
	now := time.Now().UTC()
	if message.CreatedAt.IsZero() {
		message.CreatedAt = now
	}
	if message.NextAttemptAt.IsZero() {
		message.NextAttemptAt = now
	}
	if message.Status == "" {
		message.Status = superagent.OutboxPending
	}
	message.UpdatedAt = now
	row := s.pool.QueryRow(ctx, `
		INSERT INTO collab_outbox
		    (tenant_id, message_id, work_item_id, attention_id, channel_id, thread_kind, thread_id,
		     body, data, idempotency_key, status, attempts, next_attempt_at, last_error, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (tenant_id, idempotency_key)
		    WHERE idempotency_key <> '' DO NOTHING
		RETURNING tenant_id, message_id, work_item_id, attention_id, channel_id, thread_kind, thread_id,
		          body, data, idempotency_key, status, attempts, next_attempt_at, last_error, created_at, updated_at`,
		message.TenantID, message.ID, nullableUUID(message.WorkItemID), nullableUUID(message.AttentionID),
		message.ChannelID, message.Thread.Kind, message.Thread.ID, message.Text, rawInput(message.Data),
		message.IdempotencyKey, string(message.Status), message.Attempts, message.NextAttemptAt,
		message.LastError, message.CreatedAt, message.UpdatedAt)
	stored, err := scanOutbox(row)
	if errors.Is(err, pgx.ErrNoRows) && message.IdempotencyKey != "" {
		return s.outboxByIdempotency(ctx, message.TenantID, message.IdempotencyKey)
	}
	if err != nil {
		return superagent.OutboxMessage{}, fmt.Errorf("store: enqueue outbox: %w", err)
	}
	return stored, nil
}

func (s *Store) outboxByIdempotency(ctx context.Context, tenantID uuid.UUID, key string) (superagent.OutboxMessage, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT tenant_id, message_id, work_item_id, attention_id, channel_id, thread_kind, thread_id,
		       body, data, idempotency_key, status, attempts, next_attempt_at, last_error, created_at, updated_at
		  FROM collab_outbox WHERE tenant_id = $1 AND idempotency_key = $2`, tenantID, key)
	stored, err := scanOutbox(row)
	if err != nil {
		return superagent.OutboxMessage{}, fmt.Errorf("store: read idempotent outbox: %w", err)
	}
	return stored, nil
}

// ListOutbox lists one tenant's messages in one state, oldest first.
func (s *Store) ListOutbox(ctx context.Context, tenantID uuid.UUID, status superagent.OutboxStatus, limit int) ([]superagent.OutboxMessage, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT tenant_id, message_id, work_item_id, attention_id, channel_id, thread_kind, thread_id,
		       body, data, idempotency_key, status, attempts, next_attempt_at, last_error, created_at, updated_at
		  FROM collab_outbox
		 WHERE tenant_id = $1 AND ($2 = '' OR status = $2)
		 ORDER BY created_at ASC, message_id ASC
		 LIMIT $3`, tenantID, string(status), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list outbox: %w", err)
	}
	defer rows.Close()
	out := make([]superagent.OutboxMessage, 0)
	for rows.Next() {
		message, err := scanOutbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, message)
	}
	return out, rows.Err()
}

// UpdateOutbox replaces the delivery state of one message.
func (s *Store) UpdateOutbox(ctx context.Context, message superagent.OutboxMessage) (superagent.OutboxMessage, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE collab_outbox
		   SET status = $3, attempts = $4, next_attempt_at = $5, last_error = $6, data = $7::jsonb, updated_at = now()
		 WHERE tenant_id = $1 AND message_id = $2
		RETURNING tenant_id, message_id, work_item_id, attention_id, channel_id, thread_kind, thread_id,
		          body, data, idempotency_key, status, attempts, next_attempt_at, last_error, created_at, updated_at`,
		message.TenantID, message.ID, string(message.Status), message.Attempts, message.NextAttemptAt,
		message.LastError, rawInput(message.Data))
	stored, err := scanOutbox(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return superagent.OutboxMessage{}, fmt.Errorf("store: outbox message not found")
	}
	if err != nil {
		return superagent.OutboxMessage{}, fmt.Errorf("store: update outbox: %w", err)
	}
	return stored, nil
}

func scanOutbox(row workItemScanner) (superagent.OutboxMessage, error) {
	var message superagent.OutboxMessage
	var workItemID, attentionID uuid.NullUUID
	var data []byte
	var status string
	if err := row.Scan(&message.TenantID, &message.ID, &workItemID, &attentionID, &message.ChannelID,
		&message.Thread.Kind, &message.Thread.ID, &message.Text, &data, &message.IdempotencyKey,
		&status, &message.Attempts, &message.NextAttemptAt, &message.LastError,
		&message.CreatedAt, &message.UpdatedAt); err != nil {
		return superagent.OutboxMessage{}, fmt.Errorf("store: scan outbox: %w", err)
	}
	message.Status = superagent.OutboxStatus(status)
	if workItemID.Valid {
		message.WorkItemID = workItemID.UUID
	}
	if attentionID.Valid {
		message.AttentionID = attentionID.UUID
	}
	message.Data = append(json.RawMessage(nil), data...)
	return message, nil
}

// --- Session index --------------------------------------------------------

// Session reads one Host-created session from the collab projection.
func (s *Store) Session(ctx context.Context, tenantID, sessionID uuid.UUID) (collab.SessionRef, error) {
	var ref collab.SessionRef
	var parentID, workItemID uuid.NullUUID
	var state string
	err := s.pool.QueryRow(ctx, `
		SELECT tenant_id, session_id, parent_session_id, bot_id, mode_id, work_item_id, depth, state, created_at
		  FROM collab_session_index WHERE tenant_id = $1 AND session_id = $2`,
		tenantID, sessionID).Scan(&ref.TenantID, &ref.SessionID, &parentID, &ref.BotID, &ref.ModeID,
		&workItemID, &ref.Depth, &state, &ref.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return collab.SessionRef{}, collabError(collab.ErrSessionNotFound, collab.CodeSessionNotFound, "session", "session not found")
	}
	if err != nil {
		return collab.SessionRef{}, fmt.Errorf("store: get collab session: %w", err)
	}
	if parentID.Valid {
		ref.ParentSessionID = parentID.UUID
	}
	if workItemID.Valid {
		ref.WorkItemID = workItemID.UUID
	}
	ref.State = collab.SessionState(state)
	return ref, nil
}

// Put records one Host-created session. A duplicate is a contract violation,
// not an upsert: two launchers must not claim the same child.
func (s *Store) Put(ctx context.Context, ref collab.SessionRef) error {
	if ref.TenantID == uuid.Nil || ref.SessionID == uuid.Nil {
		return collabError(collab.ErrInvalidRequest, collab.CodeInvalidRequest, "put_session", "tenant and session are required")
	}
	if ref.CreatedAt.IsZero() {
		ref.CreatedAt = time.Now().UTC()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO collab_session_index
		    (tenant_id, session_id, parent_session_id, bot_id, mode_id, work_item_id, depth, state, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,now())`,
		ref.TenantID, ref.SessionID, nullableUUID(ref.ParentSessionID), ref.BotID, ref.ModeID,
		nullableUUID(ref.WorkItemID), ref.Depth, string(ref.State), ref.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return collabError(collab.ErrSessionExists, collab.CodeSessionExists, "put_session", "session is already tracked")
		}
		return fmt.Errorf("store: put collab session: %w", err)
	}
	return nil
}

// SetState updates one tracked session's state.
func (s *Store) SetState(ctx context.Context, tenantID, sessionID uuid.UUID, state collab.SessionState) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE collab_session_index SET state = $3, updated_at = now()
		 WHERE tenant_id = $1 AND session_id = $2`, tenantID, sessionID, string(state))
	if err != nil {
		return fmt.Errorf("store: set collab session state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return collabError(collab.ErrSessionNotFound, collab.CodeSessionNotFound, "set_session_state", "session not found")
	}
	return nil
}

// ActiveChildren counts one parent's active children.
func (s *Store) ActiveChildren(ctx context.Context, tenantID, parentSessionID uuid.UUID) (int, error) {
	var count int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM collab_session_index
		 WHERE tenant_id = $1 AND parent_session_id = $2 AND state = 'active'`,
		tenantID, parentSessionID).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count active children: %w", err)
	}
	return count, nil
}

// ActiveInMode counts one tenant's active sessions in a mode.
func (s *Store) ActiveInMode(ctx context.Context, tenantID uuid.UUID, modeID string) (int, error) {
	var count int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM collab_session_index
		 WHERE tenant_id = $1 AND mode_id = $2 AND state = 'active'`,
		tenantID, modeID).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count active mode sessions: %w", err)
	}
	return count, nil
}

func collabError(kind error, code collab.ErrorCode, operation, detail string) *collab.Error {
	return &collab.Error{Kind: kind, Code: code, Operation: operation, Detail: detail}
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
