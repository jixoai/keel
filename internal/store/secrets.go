package store

// Tenant secrets: named credentials the tenant's model configuration references
// with `secret:NAME`. The value is only ever handled as ciphertext at rest and
// as an in-memory plaintext for the resolving path; no method here returns a
// value to a caller that renders it, and the list projection deliberately
// carries nothing but the name and the timestamps.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrNoTenantSecret means no secret row exists for the tenant/name pair.
var ErrNoTenantSecret = errors.New("store: tenant secret not found")

// TenantSecretMeta is the non-sensitive projection of one stored secret: what
// a list may show. It deliberately has no length, prefix, hash or any other
// datum that could help guess the value.
type TenantSecretMeta struct {
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TenantSecretAAD binds a sealed secret to its tenant and name, exactly like
// MCPOAuthAAD does for OAuth credentials: a ciphertext cannot be moved between
// rows or tenants.
func TenantSecretAAD(tenantID uuid.UUID, name string) []byte {
	aad := make([]byte, 0, len(tenantID)+1+len(name))
	aad = append(aad, tenantID[:]...)
	aad = append(aad, 0)
	aad = append(aad, name...)
	return aad
}

// ListTenantSecrets returns the names and timestamps of a tenant's secrets in
// name order. A tenant with none is an empty slice, not an error.
func (s *Store) ListTenantSecrets(ctx context.Context, tenantID uuid.UUID) ([]TenantSecretMeta, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT name, created_at, updated_at FROM tenant_secrets
		 WHERE tenant_id = $1 ORDER BY name`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: list tenant secrets: %w", err)
	}
	defer rows.Close()
	out := make([]TenantSecretMeta, 0, 8)
	for rows.Next() {
		var meta TenantSecretMeta
		if err := rows.Scan(&meta.Name, &meta.CreatedAt, &meta.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan tenant secret: %w", err)
		}
		out = append(out, meta)
	}
	return out, rows.Err()
}

// GetTenantSecret decrypts one secret for the resolving path (an attempt, a
// selection validation). found=false means the tenant never stored that name.
// A nil box is ErrSecretKeyMissing: a deployment without KEEL_SECRET_KEY has no
// secret storage, and a reference it cannot resolve must fail closed rather
// than fall back to reading plaintext from anywhere.
func (s *Store) GetTenantSecret(ctx context.Context, tenantID uuid.UUID, name string, box *SecretBox) ([]byte, bool, error) {
	if box == nil {
		return nil, false, ErrSecretKeyMissing
	}
	var sealed []byte
	err := s.pool.QueryRow(ctx,
		`SELECT ciphertext FROM tenant_secrets WHERE tenant_id = $1 AND name = $2`,
		tenantID, name).Scan(&sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: get tenant secret: %w", err)
	}
	plaintext, err := box.Open(sealed, TenantSecretAAD(tenantID, name))
	if err != nil {
		return nil, false, err
	}
	return plaintext, true, nil
}

// PutTenantSecret encrypts and upserts one secret, returning the row's
// updated_at. created_at is preserved across an overwrite, so the list still
// distinguishes "first written" from "last changed".
func (s *Store) PutTenantSecret(ctx context.Context, tenantID uuid.UUID, name string, plaintext []byte, box *SecretBox) (time.Time, error) {
	if box == nil {
		return time.Time{}, ErrSecretKeyMissing
	}
	sealed, err := box.Seal(plaintext, TenantSecretAAD(tenantID, name))
	if err != nil {
		return time.Time{}, err
	}
	var updatedAt time.Time
	err = s.pool.QueryRow(ctx,
		`INSERT INTO tenant_secrets (tenant_id, name, ciphertext)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (tenant_id, name) DO UPDATE
		 SET ciphertext = EXCLUDED.ciphertext, updated_at = now()
		 RETURNING updated_at`,
		tenantID, name, sealed).Scan(&updatedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: put tenant secret: %w", err)
	}
	return updatedAt, nil
}

// DeleteTenantSecret drops one secret. The bool reports whether a row existed:
// a delete of a name that was never stored is not an error, and the caller may
// answer idempotently.
func (s *Store) DeleteTenantSecret(ctx context.Context, tenantID uuid.UUID, name string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM tenant_secrets WHERE tenant_id = $1 AND name = $2`,
		tenantID, name)
	if err != nil {
		return false, fmt.Errorf("store: delete tenant secret: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
