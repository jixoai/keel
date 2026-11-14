// Non-token usage facts. Token accounting has exactly one home, usage_ledger
// (usage.go); this file records the byte dimensions the ledger has no column
// for: image bytes and total storage bytes a tenant placed. There is
// deliberately no price, amount or currency column anywhere in this schema —
// prices live in contracts, the runtime records objective quantities only.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Usage kinds of a byte event.
const (
	// UsageImageBytes is the byte size of an image blob a tenant stored.
	UsageImageBytes = "image_bytes"
	// UsageStorageBytes is the byte size of any blob a tenant stored. An image
	// upload records both rows (image_bytes for the image dimension,
	// storage_bytes for the storage dimension).
	UsageStorageBytes = "storage_bytes"
)

// ValidUsageKind reports whether kind is a known byte-usage dimension.
func ValidUsageKind(kind string) bool {
	return kind == UsageImageBytes || kind == UsageStorageBytes
}

// ByteUsageRecord is one objective byte event.
//
// DedupeKey makes recording idempotent (the same blob recorded twice counts
// once); an empty key means "always a new fact" and is reserved for callers
// that have no natural identity. SessionID and BlobID are optional context,
// never part of the identity.
type ByteUsageRecord struct {
	TenantID  uuid.UUID
	Kind      string
	Quantity  int64
	Source    string
	SessionID *uuid.UUID
	BlobID    *uuid.UUID
	DedupeKey string
}

// AppendByteUsage records one byte event. It reports whether a new row was
// written: a duplicate dedupe key is idempotent and returns false.
func (s *Store) AppendByteUsage(ctx context.Context, record ByteUsageRecord) (bool, error) {
	if record.TenantID == uuid.Nil {
		return false, errors.New("store: usage event needs a tenant")
	}
	if !ValidUsageKind(record.Kind) {
		return false, fmt.Errorf("store: unknown usage kind %q", record.Kind)
	}
	if record.Quantity < 0 {
		return false, errors.New("store: usage quantity must be >= 0")
	}
	var dedupe *string
	if record.DedupeKey != "" {
		key := record.DedupeKey
		dedupe = &key
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO tenant_usage (tenant_id, kind, quantity, source, session_id, blob_id, dedupe_key)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (tenant_id, kind, dedupe_key) WHERE dedupe_key IS NOT NULL DO NOTHING`,
		record.TenantID, record.Kind, record.Quantity, record.Source,
		record.SessionID, record.BlobID, dedupe)
	if err != nil {
		return false, fmt.Errorf("store: append usage event: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ByteUsageQuery filters an aggregation. A zero Since/Until means "unbounded";
// an empty Kind means "every kind".
type ByteUsageQuery struct {
	TenantID uuid.UUID
	Kind     string
	Since    time.Time
	Until    time.Time
}

// ByteUsageTotals is one aggregation group (kind) or, when Kind is empty, the
// per-kind breakdown.
type ByteUsageTotals struct {
	TenantID uuid.UUID
	Kind     string
	Entries  int64
	Bytes    int64
}

// SumByteUsage aggregates the byte events by kind, optionally narrowed to a
// time window. The tenant id is always required.
func (s *Store) SumByteUsage(ctx context.Context, query ByteUsageQuery) ([]ByteUsageTotals, error) {
	conditions := []string{"tenant_id = $1"}
	args := []any{query.TenantID}
	add := func(condition string, value any) {
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf(condition, len(args)))
	}
	if query.Kind != "" {
		add("kind = $%d", query.Kind)
	}
	if !query.Since.IsZero() {
		add("recorded_at >= $%d", query.Since)
	}
	if !query.Until.IsZero() {
		add("recorded_at < $%d", query.Until)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, kind, COUNT(*), COALESCE(SUM(quantity), 0)
		 FROM tenant_usage
		 WHERE `+strings.Join(conditions, " AND ")+`
		 GROUP BY tenant_id, kind
		 ORDER BY kind`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: sum usage events: %w", err)
	}
	defer rows.Close()
	out := make([]ByteUsageTotals, 0, 2)
	for rows.Next() {
		var totals ByteUsageTotals
		if err := rows.Scan(&totals.TenantID, &totals.Kind, &totals.Entries, &totals.Bytes); err != nil {
			return nil, fmt.Errorf("store: scan usage event: %w", err)
		}
		out = append(out, totals)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: sum usage events: %w", err)
	}
	return out, nil
}

// TenantStoredBytes reports the tenant's current stored bytes (the storage
// quota input): the sum of complete blob sizes. It is a live measurement, not a
// ledger sum: deleting a blob releases its quota, which a monotonic counter
// could not express.
func (s *Store) TenantStoredBytes(ctx context.Context, tenantID uuid.UUID) (int64, error) {
	var total int64
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(size), 0) FROM blobs WHERE tenant_id = $1`, tenantID).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: tenant stored bytes: %w", err)
	}
	return total, nil
}

// TenantQuota is the per-tenant quota row the process edge enforces. Storage is
// a ceiling on TenantStoredBytes; the stream and bandwidth limits bound what
// one tenant may hold open and consume. Zero means unlimited in every field.
type TenantQuota struct {
	TenantID     uuid.UUID
	Streams      int
	BytesPerSec  int64
	StorageBytes int64
}

// GetTenantQuota reads the quota row. A missing tenant is ErrNoTenant, so an
// unknown tenant is a 404/401 rather than a zero quota that admits everything.
func (s *Store) GetTenantQuota(ctx context.Context, tenantID uuid.UUID) (TenantQuota, error) {
	var quota TenantQuota
	err := s.pool.QueryRow(ctx,
		`SELECT tenant_id, quota_streams, quota_bytes_per_sec, quota_storage_bytes
		 FROM tenants WHERE tenant_id = $1`, tenantID).
		Scan(&quota.TenantID, &quota.Streams, &quota.BytesPerSec, &quota.StorageBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantQuota{}, ErrNoTenant
	}
	if err != nil {
		return TenantQuota{}, fmt.Errorf("store: get tenant quota: %w", err)
	}
	return quota, nil
}

// SetTenantQuota writes the quota dimensions this table owns. A negative value
// is rejected; zero means unlimited. The run-concurrency and event-rate budgets
// are NOT here: they live in the tenant configuration document
// (host.QuotaConfig) so there is exactly one quota vocabulary per dimension.
func (s *Store) SetTenantQuota(ctx context.Context, tenantID uuid.UUID, streams int, bytesPerSec, storageBytes int64) error {
	if streams < 0 || bytesPerSec < 0 || storageBytes < 0 {
		return errors.New("store: quota values must be >= 0")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE tenants SET quota_streams = $2, quota_bytes_per_sec = $3, quota_storage_bytes = $4
		 WHERE tenant_id = $1`, tenantID, streams, bytesPerSec, storageBytes)
	if err != nil {
		return fmt.Errorf("store: set tenant quota: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoTenant
	}
	return nil
}
