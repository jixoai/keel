package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jixoai/keel/internal/store"
)

// MemoryStore is the test double and the degraded no-database mode.
type MemoryStore struct {
	mu   sync.Mutex
	rows map[string]Notification
}

// NewMemoryStore builds an empty in-memory notification store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: make(map[string]Notification)}
}

// Create inserts a row unless the notification id exists.
func (s *MemoryStore) Create(_ context.Context, row Notification) (Notification, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := storeKey(row.TenantID, row.NotificationID)
	if existing, ok := s.rows[key]; ok {
		return copyNotification(existing), false, nil
	}
	s.rows[key] = copyNotification(row)
	return copyNotification(row), true, nil
}

// Get reads one row of a tenant.
func (s *MemoryStore) Get(_ context.Context, tenantID, notificationID uuid.UUID) (Notification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[storeKey(tenantID.String(), notificationID.String())]
	if !ok {
		return Notification{}, ErrNotFound
	}
	return copyNotification(row), nil
}

// GetAny reads one row by id alone.
func (s *MemoryStore) GetAny(_ context.Context, notificationID uuid.UUID) (Notification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.rows {
		if row.NotificationID == notificationID.String() {
			return copyNotification(row), nil
		}
	}
	return Notification{}, ErrNotFound
}

// List returns a tenant's rows, newest first.
func (s *MemoryStore) List(_ context.Context, tenantID uuid.UUID, sessionID *uuid.UUID, limit int) ([]Notification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Notification, 0)
	for _, row := range s.rows {
		if row.TenantID != tenantID.String() {
			continue
		}
		if sessionID != nil && row.SessionID != sessionID.String() {
			continue
		}
		out = append(out, copyNotification(row))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].NotificationID > out[j].NotificationID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListDue returns due notifications and claimed-but-unrecorded pushes.
func (s *MemoryStore) ListDue(_ context.Context, now time.Time, limit int) ([]Notification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Notification, 0)
	for _, row := range s.rows {
		if row.Status == StatusConsumed {
			continue
		}
		due := row.PushAttemptedAt == nil && !now.Before(row.Deadline)
		unrecorded := row.PushAttemptedAt != nil && row.Push == nil
		if due || unrecorded {
			out = append(out, copyNotification(row))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Deadline.Before(out[j].Deadline) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Save replaces a row.
func (s *MemoryStore) Save(_ context.Context, row Notification) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := storeKey(row.TenantID, row.NotificationID)
	if _, ok := s.rows[key]; !ok {
		return ErrNotFound
	}
	s.rows[key] = copyNotification(row)
	return nil
}

// ClaimPush marks the single push attempt atomically.
func (s *MemoryStore) ClaimPush(_ context.Context, tenantID, notificationID uuid.UUID, at time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := storeKey(tenantID.String(), notificationID.String())
	row, ok := s.rows[key]
	if !ok {
		return false, ErrNotFound
	}
	if row.Status == StatusConsumed || row.PushAttemptedAt != nil || row.Push != nil {
		return false, nil
	}
	row.PushAttemptedAt = &at
	row.UpdatedAt = at
	s.rows[key] = row
	return true, nil
}

// PostgresStore persists notifications in the notifications table.
type PostgresStore struct {
	store *store.Store
}

// NewPostgresStore wraps a store.
func NewPostgresStore(st *store.Store) *PostgresStore {
	return &PostgresStore{store: st}
}

const notificationColumns = `tenant_id, notification_id, session_id, title, body, status,
	created_at, deadline, consumers, push_attempted_at, push, updated_at`

// Create inserts a row unless the notification id exists.
func (s *PostgresStore) Create(ctx context.Context, row Notification) (Notification, bool, error) {
	tenantID, notificationID, sessionID, err := parseNotificationIDs(row)
	if err != nil {
		return Notification{}, false, err
	}
	consumers, err := json.Marshal(row.Consumers)
	if err != nil {
		return Notification{}, false, fmt.Errorf("notify: encode consumers: %w", err)
	}
	push, err := marshalPush(row.Push)
	if err != nil {
		return Notification{}, false, err
	}
	tag, err := s.store.Pool().Exec(ctx,
		`INSERT INTO notifications
		   (tenant_id, notification_id, session_id, title, body, status,
		    created_at, deadline, consumers, push_attempted_at, push, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 ON CONFLICT (tenant_id, notification_id) DO NOTHING`,
		tenantID, notificationID, sessionID, row.Title, row.Text, row.Status,
		row.CreatedAt, row.Deadline, consumers, row.PushAttemptedAt, push, row.UpdatedAt)
	if err != nil {
		return Notification{}, false, fmt.Errorf("notify: create notification: %w", err)
	}
	stored, err := s.Get(ctx, tenantID, notificationID)
	if err != nil {
		return Notification{}, false, err
	}
	return stored, tag.RowsAffected() == 1, nil
}

// Get reads one row of a tenant.
func (s *PostgresStore) Get(ctx context.Context, tenantID, notificationID uuid.UUID) (Notification, error) {
	return s.scanRow(s.store.Pool().QueryRow(ctx,
		`SELECT `+notificationColumns+` FROM notifications WHERE tenant_id = $1 AND notification_id = $2`,
		tenantID, notificationID))
}

// GetAny reads one row by id alone.
func (s *PostgresStore) GetAny(ctx context.Context, notificationID uuid.UUID) (Notification, error) {
	return s.scanRow(s.store.Pool().QueryRow(ctx,
		`SELECT `+notificationColumns+` FROM notifications WHERE notification_id = $1`, notificationID))
}

// List returns a tenant's rows, newest first.
func (s *PostgresStore) List(ctx context.Context, tenantID uuid.UUID, sessionID *uuid.UUID, limit int) ([]Notification, error) {
	query := `SELECT ` + notificationColumns + ` FROM notifications WHERE tenant_id = $1`
	args := []any{tenantID}
	if sessionID != nil {
		query += ` AND session_id = $2`
		args = append(args, *sessionID)
	}
	query += fmt.Sprintf(` ORDER BY created_at DESC, notification_id DESC LIMIT %d`, clampLimit(limit))
	return s.scanRows(s.store.Pool().Query(ctx, query, args...))
}

// ListDue returns due notifications and claimed-but-unrecorded pushes.
func (s *PostgresStore) ListDue(ctx context.Context, now time.Time, limit int) ([]Notification, error) {
	query := `SELECT ` + notificationColumns + ` FROM notifications
	          WHERE status <> 'consumed'
	            AND ((push_attempted_at IS NULL AND deadline <= $1)
	              OR (push_attempted_at IS NOT NULL AND push IS NULL))
	          ORDER BY deadline ASC LIMIT ` + fmt.Sprintf("%d", clampLimit(limit))
	return s.scanRows(s.store.Pool().Query(ctx, query, now))
}

// Save rewrites a row.
func (s *PostgresStore) Save(ctx context.Context, row Notification) error {
	tenantID, notificationID, sessionID, err := parseNotificationIDs(row)
	if err != nil {
		return err
	}
	consumers, err := json.Marshal(row.Consumers)
	if err != nil {
		return fmt.Errorf("notify: encode consumers: %w", err)
	}
	push, err := marshalPush(row.Push)
	if err != nil {
		return err
	}
	tag, err := s.store.Pool().Exec(ctx,
		`UPDATE notifications
		    SET session_id = $3, title = $4, body = $5, status = $6, created_at = $7,
		        deadline = $8, consumers = $9, push_attempted_at = $10, push = $11, updated_at = $12
		  WHERE tenant_id = $1 AND notification_id = $2`,
		tenantID, notificationID, sessionID, row.Title, row.Text, row.Status, row.CreatedAt,
		row.Deadline, consumers, row.PushAttemptedAt, push, row.UpdatedAt)
	if err != nil {
		return fmt.Errorf("notify: save notification: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ClaimPush marks the single push attempt atomically.
func (s *PostgresStore) ClaimPush(ctx context.Context, tenantID, notificationID uuid.UUID, at time.Time) (bool, error) {
	tag, err := s.store.Pool().Exec(ctx,
		`UPDATE notifications
		    SET push_attempted_at = $3, updated_at = $3
		  WHERE tenant_id = $1 AND notification_id = $2
		    AND status <> 'consumed' AND push_attempted_at IS NULL AND push IS NULL`,
		tenantID, notificationID, at)
	if err != nil {
		return false, fmt.Errorf("notify: claim push: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *PostgresStore) scanRow(row pgx.Row) (Notification, error) {
	var out Notification
	var consumers []byte
	var push []byte
	if err := row.Scan(&out.TenantID, &out.NotificationID, &out.SessionID, &out.Title, &out.Text,
		&out.Status, &out.CreatedAt, &out.Deadline, &consumers, &out.PushAttemptedAt, &push,
		&out.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Notification{}, ErrNotFound
		}
		return Notification{}, fmt.Errorf("notify: read notification: %w", err)
	}
	if err := decodeConsumers(consumers, &out.Consumers); err != nil {
		return Notification{}, err
	}
	out.Push = decodePush(push)
	return out, nil
}

func (s *PostgresStore) scanRows(rows pgx.Rows, err error) ([]Notification, error) {
	if err != nil {
		return nil, fmt.Errorf("notify: list notifications: %w", err)
	}
	defer rows.Close()
	out := make([]Notification, 0)
	for rows.Next() {
		var row Notification
		var consumers []byte
		var push []byte
		if err := rows.Scan(&row.TenantID, &row.NotificationID, &row.SessionID, &row.Title,
			&row.Text, &row.Status, &row.CreatedAt, &row.Deadline, &consumers,
			&row.PushAttemptedAt, &push, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("notify: scan notification: %w", err)
		}
		if err := decodeConsumers(consumers, &row.Consumers); err != nil {
			return nil, err
		}
		row.Push = decodePush(push)
		out = append(out, row)
	}
	return out, rows.Err()
}

func decodeConsumers(raw []byte, out *[]Consumer) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("notify: decode consumers: %w", err)
	}
	return nil
}

func marshalPush(push *PushRecord) (any, error) {
	if push == nil {
		return nil, nil
	}
	raw, err := json.Marshal(push)
	if err != nil {
		return nil, fmt.Errorf("notify: encode push: %w", err)
	}
	return raw, nil
}

func decodePush(raw []byte) *PushRecord {
	if len(raw) == 0 {
		return nil
	}
	var push PushRecord
	if err := json.Unmarshal(raw, &push); err != nil {
		return nil
	}
	return &push
}

func parseNotificationIDs(row Notification) (uuid.UUID, uuid.UUID, uuid.UUID, error) {
	tenantID, err := uuid.Parse(row.TenantID)
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, fmt.Errorf("%w: tenantId must be a UUID", ErrInvalid)
	}
	notificationID, err := uuid.Parse(row.NotificationID)
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, fmt.Errorf("%w: notificationId must be a UUID", ErrInvalid)
	}
	sessionID, err := uuid.Parse(row.SessionID)
	if err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, fmt.Errorf("%w: sessionId must be a UUID", ErrInvalid)
	}
	return tenantID, notificationID, sessionID, nil
}

func storeKey(tenantID, notificationID string) string {
	return tenantID + ":" + notificationID
}

func clampLimit(limit int) int {
	if limit <= 0 {
		return 20
	}
	if limit > 100 {
		return 100
	}
	return limit
}

func copyNotification(row Notification) Notification {
	if row.Consumers != nil {
		consumers := make([]Consumer, len(row.Consumers))
		copy(consumers, row.Consumers)
		for i := range consumers {
			if consumers[i].DeliveredAt != nil {
				at := *consumers[i].DeliveredAt
				consumers[i].DeliveredAt = &at
			}
			if consumers[i].AckedAt != nil {
				at := *consumers[i].AckedAt
				consumers[i].AckedAt = &at
			}
			if consumers[i].TokenExpiresAt != nil {
				at := *consumers[i].TokenExpiresAt
				consumers[i].TokenExpiresAt = &at
			}
		}
		row.Consumers = consumers
	}
	if row.PushAttemptedAt != nil {
		at := *row.PushAttemptedAt
		row.PushAttemptedAt = &at
	}
	if row.Push != nil {
		push := *row.Push
		push.Consumers = append([]string(nil), row.Push.Consumers...)
		row.Push = &push
	}
	return row
}
