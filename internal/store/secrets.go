package store

// Tenant secrets: named credentials the tenant's model configuration references
// with `secret:NAME`. The value is only ever handled as ciphertext at rest and
// as an in-memory plaintext for the resolving path; no method here returns a
// value to a caller that renders it, and the list projection deliberately
// carries nothing but the name, the masked preview and the timestamps.

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

// MaskedSecretPreview is the fixed stand-in for a credential too short to mask
// with a prefix and a suffix. It deliberately carries no character of the value
// and no length fact.
const MaskedSecretPreview = "••••••••"

// previewVisibleRunes is the rune count above which a preview keeps the first
// six and the last six characters. At exactly 18 runes, six + six would be the
// whole value, so the fixed mask applies instead.
const previewVisibleRunes = 18

// SecretPreview computes the masked preview of one credential value. The rule
// is exact: a value longer than 18 runes previews as first6 + "…" + last6; any
// shorter value (including 18) previews as the fixed mask alone, so a short
// credential cannot be reconstructed and its length cannot be probed from the
// preview. A rune-counted boundary matters: an emoji or CJK secret counts by
// code point, not UTF-16 units or bytes.
func SecretPreview(value []byte) string {
	runes := []rune(string(value))
	if len(runes) <= previewVisibleRunes {
		return MaskedSecretPreview
	}
	return string(runes[:6]) + "…" + string(runes[len(runes)-6:])
}

// TenantSecretMeta is the non-sensitive projection of one stored secret: what
// a list may show. The preview is the masked form computed at write time (see
// SecretPreview); it deliberately has no length, full prefix, hash or any other
// datum that could help guess the value. An empty preview means the row
// predates the column and no masked form was recorded — a reader shows
// "已保存" instead of inventing one.
type TenantSecretMeta struct {
	Name      string
	Preview   string
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

// ListTenantSecrets returns the names, masked previews and timestamps of a
// tenant's secrets in name order. A tenant with none is an empty slice, not an
// error.
func (s *Store) ListTenantSecrets(ctx context.Context, tenantID uuid.UUID) ([]TenantSecretMeta, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT name, preview, created_at, updated_at FROM tenant_secrets
		 WHERE tenant_id = $1 ORDER BY name`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: list tenant secrets: %w", err)
	}
	defer rows.Close()
	out := make([]TenantSecretMeta, 0, 8)
	for rows.Next() {
		var meta TenantSecretMeta
		var preview *string
		if err := rows.Scan(&meta.Name, &preview, &meta.CreatedAt, &meta.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan tenant secret: %w", err)
		}
		if preview != nil {
			meta.Preview = *preview
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
// updated_at. The masked preview is computed from the plaintext here — at write
// time, by the same process that held the value — so no later read ever needs
// the plaintext to render it. created_at is preserved across an overwrite, so
// the list still distinguishes "first written" from "last changed".
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
		`INSERT INTO tenant_secrets (tenant_id, name, ciphertext, preview)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (tenant_id, name) DO UPDATE
		 SET ciphertext = EXCLUDED.ciphertext, preview = EXCLUDED.preview, updated_at = now()
		 RETURNING updated_at`,
		tenantID, name, sealed, SecretPreview(plaintext)).Scan(&updatedAt)
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
