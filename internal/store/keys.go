// Tenant API keys: the credential that resolves a request to a tenant.
//
// Only the SHA-256 hash of the key and a display prefix are persisted. The
// plaintext exists exactly once — in the answer of the provisioning call — and
// is never stored, logged or recoverable. A key belongs to one tenant and
// carries a role ('tenant' or 'admin'); the role is a property of the
// credential, not a second account model.
package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// API key roles. A tenant key acts inside one tenant; an admin key may act on
// any tenant (operator API surface).
const (
	APIKeyRoleTenant = "tenant"
	APIKeyRoleAdmin  = "admin"
)

// ErrNoAPIKey is returned when no live key matches a hash. It deliberately does
// not distinguish "unknown" from "revoked": the caller answers 401 either way
// and must not leak which one happened.
var ErrNoAPIKey = errors.New("store: api key not found")

// APIKey is one credential's metadata. Hash is the lookup identity; Prefix is
// the only part of the plaintext that survives.
type APIKey struct {
	TenantID  uuid.UUID
	KeyID     uuid.UUID
	Prefix    string
	Role      string
	Label     string
	CreatedAt time.Time
	RevokedAt *time.Time
}

// APIKeyHash is the lookup identity of a plaintext key.
func APIKeyHash(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// ValidAPIKeyRole reports whether role is a known role.
func ValidAPIKeyRole(role string) bool {
	return role == APIKeyRoleTenant || role == APIKeyRoleAdmin
}

// CreateAPIKey persists one key hash and returns the stored metadata. The
// caller generated the plaintext; this method never sees it.
func (s *Store) CreateAPIKey(ctx context.Context, tenantID, keyID uuid.UUID, hash []byte, prefix, role, label string) (APIKey, error) {
	if tenantID == uuid.Nil || keyID == uuid.Nil {
		return APIKey{}, errors.New("store: api key needs a tenant and a key id")
	}
	if len(hash) != sha256.Size {
		return APIKey{}, fmt.Errorf("store: api key hash must be %d bytes", sha256.Size)
	}
	if role == "" {
		role = APIKeyRoleTenant
	}
	if !ValidAPIKeyRole(role) {
		return APIKey{}, fmt.Errorf("store: unknown api key role %q", role)
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO api_keys (tenant_id, key_id, key_hash, key_prefix, role, label)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING tenant_id, key_id, key_prefix, role, label, created_at, revoked_at`,
		tenantID, keyID, hash, prefix, role, label)
	key, err := scanAPIKey(row)
	if err != nil {
		return APIKey{}, fmt.Errorf("store: create api key: %w", err)
	}
	return key, nil
}

// LookupAPIKey resolves a key hash to its live credential. A revoked key is
// ErrNoAPIKey, exactly like an unknown one. The tenant must still exist: an
// orphaned key (tenant row deleted) resolves to ErrNoAPIKey rather than to a
// tenant that owns nothing.
func (s *Store) LookupAPIKey(ctx context.Context, hash []byte) (APIKey, error) {
	if len(hash) != sha256.Size {
		return APIKey{}, ErrNoAPIKey
	}
	row := s.pool.QueryRow(ctx,
		`SELECT k.tenant_id, k.key_id, k.key_prefix, k.role, k.label, k.created_at, k.revoked_at
		 FROM api_keys k
		 JOIN tenants t ON t.tenant_id = k.tenant_id
		 WHERE k.key_hash = $1 AND k.revoked_at IS NULL`, hash)
	key, err := scanAPIKey(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIKey{}, ErrNoAPIKey
	}
	if err != nil {
		return APIKey{}, fmt.Errorf("store: lookup api key: %w", err)
	}
	return key, nil
}

// ListAPIKeys returns a tenant's keys, newest first. Hashes never leave the
// database: the projection carries the display prefix only.
func (s *Store) ListAPIKeys(ctx context.Context, tenantID uuid.UUID) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, key_id, key_prefix, role, label, created_at, revoked_at
		 FROM api_keys WHERE tenant_id = $1
		 ORDER BY created_at DESC, key_id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: list api keys: %w", err)
	}
	defer rows.Close()
	out := make([]APIKey, 0, 4)
	for rows.Next() {
		var key APIKey
		if err := rows.Scan(&key.TenantID, &key.KeyID, &key.Prefix, &key.Role,
			&key.Label, &key.CreatedAt, &key.RevokedAt); err != nil {
			return nil, fmt.Errorf("store: scan api key: %w", err)
		}
		out = append(out, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list api keys: %w", err)
	}
	return out, nil
}

// RevokeAPIKey marks a key revoked. It reports false when the key does not
// exist for this tenant. Revocation is idempotent: an already-revoked key
// reports true and keeps its first revocation time.
func (s *Store) RevokeAPIKey(ctx context.Context, tenantID, keyID uuid.UUID) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE api_keys SET revoked_at = COALESCE(revoked_at, now())
		 WHERE tenant_id = $1 AND key_id = $2`, tenantID, keyID)
	if err != nil {
		return false, fmt.Errorf("store: revoke api key: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return true, nil
	}
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM api_keys WHERE tenant_id = $1 AND key_id = $2)`,
		tenantID, keyID).Scan(&exists); err != nil {
		return false, fmt.Errorf("store: revoke api key: %w", err)
	}
	return exists, nil
}

// DeleteAPIKey removes a key row. Used by tests and by an operator cleaning a
// mis-provisioned key; production revocation keeps the audit row.
func (s *Store) DeleteAPIKey(ctx context.Context, tenantID, keyID uuid.UUID) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM api_keys WHERE tenant_id = $1 AND key_id = $2`, tenantID, keyID)
	if err != nil {
		return false, fmt.Errorf("store: delete api key: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// CountAPIKeys reports how many live keys a tenant holds (provisioning output
// and tests).
func (s *Store) CountAPIKeys(ctx context.Context, tenantID uuid.UUID) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM api_keys WHERE tenant_id = $1 AND revoked_at IS NULL`,
		tenantID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count api keys: %w", err)
	}
	return n, nil
}

// TenantByKeyPrefix is a display helper: the tenant a prefix belongs to. It
// never resolves a credential (the prefix is not a secret and is not unique by
// construction); operators use it to identify a key they printed once.
func (s *Store) TenantByKeyPrefix(ctx context.Context, prefix string) ([]APIKey, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil, errors.New("store: a key prefix is required")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, key_id, key_prefix, role, label, created_at, revoked_at
		 FROM api_keys WHERE key_prefix = $1 ORDER BY created_at DESC`, prefix)
	if err != nil {
		return nil, fmt.Errorf("store: keys by prefix: %w", err)
	}
	defer rows.Close()
	out := make([]APIKey, 0, 2)
	for rows.Next() {
		var key APIKey
		if err := rows.Scan(&key.TenantID, &key.KeyID, &key.Prefix, &key.Role,
			&key.Label, &key.CreatedAt, &key.RevokedAt); err != nil {
			return nil, fmt.Errorf("store: scan api key: %w", err)
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

func scanAPIKey(row rowScanner) (APIKey, error) {
	var key APIKey
	err := row.Scan(&key.TenantID, &key.KeyID, &key.Prefix, &key.Role,
		&key.Label, &key.CreatedAt, &key.RevokedAt)
	return key, err
}
