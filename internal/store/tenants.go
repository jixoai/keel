package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrNoTenant is returned when a tenant-scoped row does not exist.
var ErrNoTenant = errors.New("store: tenant not found")

// TenantConfigRecord is the persisted configuration document and its revision. The
// store treats Config as opaque JSON: the document schema lives in
// internal/host, not in SQL, so adding a config field never needs a migration.
type TenantConfigRecord struct {
	Config   json.RawMessage
	Revision int64
}

// GetTenantConfig reads a tenant's configuration document. A tenant created
// before the config column existed (or without a write) reports "{}" at
// revision 1.
func (s *Store) GetTenantConfig(ctx context.Context, tenantID uuid.UUID) (TenantConfigRecord, error) {
	var (
		raw []byte
		rev int64
	)
	err := s.pool.QueryRow(ctx,
		`SELECT config, config_revision FROM tenants WHERE tenant_id = $1`,
		tenantID).Scan(&raw, &rev)
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantConfigRecord{}, ErrNoTenant
	}
	if err != nil {
		return TenantConfigRecord{}, fmt.Errorf("store: get tenant config: %w", err)
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	return TenantConfigRecord{Config: json.RawMessage(raw), Revision: rev}, nil
}

// SetTenantConfig replaces a tenant's configuration and bumps the revision in
// one statement. It returns the new revision; concurrent writers therefore see
// distinct, monotonically increasing values (the revision is an update counter,
// not a compare-and-swap token).
func (s *Store) SetTenantConfig(ctx context.Context, tenantID uuid.UUID, config json.RawMessage) (int64, error) {
	if len(config) == 0 {
		config = json.RawMessage("{}")
	}
	if !json.Valid(config) {
		return 0, fmt.Errorf("store: tenant config is not valid JSON")
	}
	var revision int64
	err := s.pool.QueryRow(ctx,
		`UPDATE tenants
		 SET config = $2::jsonb, config_revision = config_revision + 1
		 WHERE tenant_id = $1
		 RETURNING config_revision`,
		tenantID, string(config)).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNoTenant
	}
	if err != nil {
		return 0, fmt.Errorf("store: set tenant config: %w", err)
	}
	return revision, nil
}

// GetTenantRevision returns just the config revision, for callers that poll
// for changes without loading the document.
func (s *Store) GetTenantRevision(ctx context.Context, tenantID uuid.UUID) (int64, error) {
	var revision int64
	err := s.pool.QueryRow(ctx,
		`SELECT config_revision FROM tenants WHERE tenant_id = $1`,
		tenantID).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNoTenant
	}
	if err != nil {
		return 0, fmt.Errorf("store: get tenant revision: %w", err)
	}
	return revision, nil
}
