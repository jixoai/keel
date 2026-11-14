// Tenant listing for the operator CLI and the admin surface. Kept separate from
// tenants.go (configuration document) because these are the tenant's identity
// and its quota row — the two facts an operator lists without loading a config
// document.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TenantRecord is a tenant's identity plus its quota row.
type TenantRecord struct {
	TenantID  uuid.UUID
	Name      string
	Quota     TenantQuota
	CreatedAt time.Time
}

// ListTenants returns every tenant ordered by creation time. The operator
// surface lists tenants; tenancy is small (100-200 tenants by architecture
// design), so this is deliberately unpaginated.
func (s *Store) ListTenants(ctx context.Context) ([]TenantRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, name, quota_streams, quota_bytes_per_sec, quota_storage_bytes, created_at
		 FROM tenants ORDER BY created_at, tenant_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list tenants: %w", err)
	}
	defer rows.Close()
	out := make([]TenantRecord, 0, 16)
	for rows.Next() {
		var record TenantRecord
		if err := rows.Scan(&record.TenantID, &record.Name, &record.Quota.Streams,
			&record.Quota.BytesPerSec, &record.Quota.StorageBytes, &record.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan tenant: %w", err)
		}
		record.Quota.TenantID = record.TenantID
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list tenants: %w", err)
	}
	return out, nil
}

// GetTenant reads one tenant. found=false is a missing tenant (not an error):
// the CLI reports it and exits non-zero, the HTTP surface answers 404.
func (s *Store) GetTenant(ctx context.Context, tenantID uuid.UUID) (TenantRecord, bool, error) {
	var record TenantRecord
	err := s.pool.QueryRow(ctx,
		`SELECT tenant_id, name, quota_streams, quota_bytes_per_sec, quota_storage_bytes, created_at
		 FROM tenants WHERE tenant_id = $1`, tenantID).
		Scan(&record.TenantID, &record.Name, &record.Quota.Streams,
			&record.Quota.BytesPerSec, &record.Quota.StorageBytes, &record.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantRecord{}, false, nil
	}
	if err != nil {
		return TenantRecord{}, false, fmt.Errorf("store: get tenant: %w", err)
	}
	record.Quota.TenantID = record.TenantID
	return record, true, nil
}
