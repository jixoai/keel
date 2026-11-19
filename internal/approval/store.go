package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jixoai/keel/internal/store"
)

// MemoryStore keeps provenance rows in process memory. It is the test double
// and the degraded no-database mode; a restart loses its rows.
type MemoryStore struct {
	mu   sync.Mutex
	rows map[string]Record
}

// NewMemoryStore builds an empty in-memory provenance store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: make(map[string]Record)}
}

// Create inserts a row unless the decision already has one.
func (s *MemoryStore) Create(_ context.Context, row Record) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := recordKey(row.TenantID, row.SessionID, row.DecisionID)
	if existing, ok := s.rows[key]; ok {
		return copyRecord(existing), false, nil
	}
	s.rows[key] = copyRecord(row)
	return copyRecord(row), true, nil
}

// Get reads one row.
func (s *MemoryStore) Get(_ context.Context, tenantID, sessionID, decisionID uuid.UUID) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[recordKey(tenantID.String(), sessionID.String(), decisionID.String())]
	if !ok {
		return Record{}, ErrNotFound
	}
	return copyRecord(row), nil
}

// List returns the session's rows, newest first.
func (s *MemoryStore) List(_ context.Context, tenantID, sessionID uuid.UUID) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0)
	for _, row := range s.rows {
		if row.TenantID == tenantID.String() && row.SessionID == sessionID.String() {
			out = append(out, copyRecord(row))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// Save replaces an existing row.
func (s *MemoryStore) Save(_ context.Context, row Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := recordKey(row.TenantID, row.SessionID, row.DecisionID)
	if _, ok := s.rows[key]; !ok {
		return ErrNotFound
	}
	s.rows[key] = copyRecord(row)
	return nil
}

// PostgresStore persists provenance rows in approval_provenance (schema.sql).
type PostgresStore struct {
	store *store.Store
}

// NewPostgresStore wraps a store.
func NewPostgresStore(st *store.Store) *PostgresStore {
	return &PostgresStore{store: st}
}

// Create inserts a row unless the decision already has one.
func (s *PostgresStore) Create(ctx context.Context, row Record) (Record, bool, error) {
	tenantID, sessionID, decisionID, err := parseIDs(row)
	if err != nil {
		return Record{}, false, err
	}
	ctxJSON, err := marshalContext(row.Context)
	if err != nil {
		return Record{}, false, err
	}
	tag, err := s.store.Pool().Exec(ctx,
		`INSERT INTO approval_provenance
		   (tenant_id, session_id, decision_id, kind, mode, status, option_id, answer_text,
		    validated, escape, actor, channel, context, opened_at, delivered_at, answered_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		 ON CONFLICT (tenant_id, session_id, decision_id) DO NOTHING`,
		tenantID, sessionID, decisionID, row.Kind, row.Mode, row.Status, row.OptionID, row.Text,
		row.Validated, row.Escape, row.Actor, row.Channel, ctxJSON, row.OpenedAt, row.DeliveredAt,
		row.AnsweredAt, row.UpdatedAt)
	if err != nil {
		return Record{}, false, fmt.Errorf("approval: create provenance: %w", err)
	}
	stored, err := s.Get(ctx, tenantID, sessionID, decisionID)
	if err != nil {
		return Record{}, false, err
	}
	return stored, tag.RowsAffected() == 1, nil
}

// Get reads one row.
func (s *PostgresStore) Get(ctx context.Context, tenantID, sessionID, decisionID uuid.UUID) (Record, error) {
	var row Record
	var ctxJSON []byte
	err := s.store.Pool().QueryRow(ctx,
		`SELECT tenant_id, session_id, decision_id, kind, mode, status, option_id, answer_text,
		        validated, escape, actor, channel, context, opened_at, delivered_at, answered_at, updated_at
		   FROM approval_provenance
		  WHERE tenant_id = $1 AND session_id = $2 AND decision_id = $3`,
		tenantID, sessionID, decisionID).Scan(
		&row.TenantID, &row.SessionID, &row.DecisionID, &row.Kind, &row.Mode, &row.Status,
		&row.OptionID, &row.Text, &row.Validated, &row.Escape, &row.Actor, &row.Channel,
		&ctxJSON, &row.OpenedAt, &row.DeliveredAt, &row.AnsweredAt, &row.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("approval: get provenance: %w", err)
	}
	row.Context = DecodeContext(ctxJSON)
	return row, nil
}

// List returns the session's rows, newest first.
func (s *PostgresStore) List(ctx context.Context, tenantID, sessionID uuid.UUID) ([]Record, error) {
	rows, err := s.store.Pool().Query(ctx,
		`SELECT tenant_id, session_id, decision_id, kind, mode, status, option_id, answer_text,
		        validated, escape, actor, channel, context, opened_at, delivered_at, answered_at, updated_at
		   FROM approval_provenance
		  WHERE tenant_id = $1 AND session_id = $2
		  ORDER BY updated_at DESC, decision_id DESC`,
		tenantID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("approval: list provenance: %w", err)
	}
	defer rows.Close()
	out := make([]Record, 0)
	for rows.Next() {
		var row Record
		var ctxJSON []byte
		if err := rows.Scan(
			&row.TenantID, &row.SessionID, &row.DecisionID, &row.Kind, &row.Mode, &row.Status,
			&row.OptionID, &row.Text, &row.Validated, &row.Escape, &row.Actor, &row.Channel,
			&ctxJSON, &row.OpenedAt, &row.DeliveredAt, &row.AnsweredAt, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("approval: scan provenance: %w", err)
		}
		row.Context = DecodeContext(ctxJSON)
		out = append(out, row)
	}
	return out, rows.Err()
}

// Save rewrites the mutable columns of a row.
func (s *PostgresStore) Save(ctx context.Context, row Record) error {
	tenantID, sessionID, decisionID, err := parseIDs(row)
	if err != nil {
		return err
	}
	ctxJSON, err := marshalContext(row.Context)
	if err != nil {
		return err
	}
	tag, err := s.store.Pool().Exec(ctx,
		`UPDATE approval_provenance
		    SET kind = $4, mode = $5, status = $6, option_id = $7, answer_text = $8,
		        validated = $9, escape = $10, actor = $11, channel = $12, context = $13,
		        opened_at = $14, delivered_at = $15, answered_at = $16, updated_at = $17
		  WHERE tenant_id = $1 AND session_id = $2 AND decision_id = $3`,
		tenantID, sessionID, decisionID, row.Kind, row.Mode, row.Status, row.OptionID, row.Text,
		row.Validated, row.Escape, row.Actor, row.Channel, ctxJSON, row.OpenedAt, row.DeliveredAt,
		row.AnsweredAt, row.UpdatedAt)
	if err != nil {
		return fmt.Errorf("approval: save provenance: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func parseIDs(row Record) (uuid.UUID, uuid.UUID, uuid.UUID, error) {
	tenantID, err := uuid.Parse(row.TenantID)
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, fmt.Errorf("%w: tenantId must be a UUID", ErrInvalid)
	}
	sessionID, err := uuid.Parse(row.SessionID)
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, fmt.Errorf("%w: sessionId must be a UUID", ErrInvalid)
	}
	decisionID, err := uuid.Parse(row.DecisionID)
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, fmt.Errorf("%w: decisionId must be a UUID", ErrInvalid)
	}
	return tenantID, sessionID, decisionID, nil
}

func marshalContext(context map[string]string) (any, error) {
	if len(context) == 0 {
		return []byte("{}"), nil
	}
	raw, err := json.Marshal(context)
	if err != nil {
		return nil, fmt.Errorf("approval: encode context: %w", err)
	}
	return raw, nil
}

func recordKey(tenantID, sessionID, decisionID string) string {
	return tenantID + ":" + sessionID + ":" + decisionID
}

func copyRecord(row Record) Record {
	if row.Context != nil {
		context := make(map[string]string, len(row.Context))
		for key, value := range row.Context {
			context[key] = value
		}
		row.Context = context
	}
	if row.OpenedAt != nil {
		at := *row.OpenedAt
		row.OpenedAt = &at
	}
	if row.DeliveredAt != nil {
		at := *row.DeliveredAt
		row.DeliveredAt = &at
	}
	if row.AnsweredAt != nil {
		at := *row.AnsweredAt
		row.AnsweredAt = &at
	}
	return row
}
