// Bot API keys (change keel-bot-identity-access, A1).
//
// A key binds to one (account, bot) pair. Only the SHA-256 hash and a display
// prefix are stored; the plaintext exists once, in the issue response A2 will
// build, and is never stored, logged or recoverable. The global unique hash
// index is the lookup path: a request presents a key before its tenant is
// known, so LookupBotAPIKey deliberately runs on the operator pool (bypass
// marker), exactly like LookupAPIKey.
package store

import (
	"context"
	"crypto/sha256"
	"strings"
	"time"

	"github.com/google/uuid"
)

// BotAPIKey is one credential's metadata. The hash never leaves the database
// through this type; the prefix is the only part of the plaintext that
// survives.
type BotAPIKey struct {
	TenantID   uuid.UUID
	KeyID      uuid.UUID
	Prefix     string
	AccountID  uuid.UUID
	BotID      string
	Label      string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
}

// BotAPIKeyHash is the lookup identity of a plaintext bot key.
func BotAPIKeyHash(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// CreateBotAPIKey persists one key hash and returns the stored metadata. The
// caller generated and returned the plaintext; this method never sees it.
func (s *Store) CreateBotAPIKey(ctx context.Context, tenantID, keyID uuid.UUID, hash []byte,
	prefix string, accountID uuid.UUID, botID, label string) (BotAPIKey, error) {
	prefix = strings.TrimSpace(prefix)
	botID = strings.TrimSpace(botID)
	label = strings.TrimSpace(label)
	switch {
	case tenantID == uuid.Nil || keyID == uuid.Nil:
		return BotAPIKey{}, invalidIdentity("create bot api key", "tenant and key ids are required")
	case len(hash) != sha256.Size:
		return BotAPIKey{}, invalidIdentity("create bot api key", "key hash must be %d bytes", sha256.Size)
	case prefix == "":
		return BotAPIKey{}, invalidIdentity("create bot api key", "key prefix is required")
	case accountID == uuid.Nil:
		return BotAPIKey{}, invalidIdentity("create bot api key", "account id is required")
	case botID == "":
		return BotAPIKey{}, invalidIdentity("create bot api key", "bot id is required (a key always binds to a bot)")
	}
	created, err := scanBotAPIKeyRow(s.pool.QueryRow(ctx,
		`INSERT INTO bot_api_keys
		    (tenant_id, key_id, key_hash, key_prefix, account_id, bot_id, label)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING tenant_id, key_id, key_prefix, account_id, bot_id, label,
		           created_at, updated_at, revoked_at, last_used_at`,
		tenantID, keyID, hash, prefix, accountID, botID, label))
	if err != nil {
		return BotAPIKey{}, identityWriteError("create bot api key", err)
	}
	return created, nil
}

// LookupBotAPIKey resolves a key hash to its live credential. A revoked key is
// ErrNotFound, exactly like an unknown one: the caller answers 401 either way
// and must not leak which happened. A2 adds the account-disabled and
// bot-archived checks; the store only answers "is this a live credential row?".
func (s *Store) LookupBotAPIKey(ctx context.Context, hash []byte) (BotAPIKey, error) {
	if len(hash) != sha256.Size {
		return BotAPIKey{}, invalidIdentity("lookup bot api key", "key hash must be %d bytes", sha256.Size)
	}
	record, err := scanBotAPIKeyRow(s.pool.QueryRow(ctx,
		`SELECT tenant_id, key_id, key_prefix, account_id, bot_id, label,
		        created_at, updated_at, revoked_at, last_used_at
		   FROM bot_api_keys
		  WHERE key_hash = $1 AND revoked_at IS NULL`, hash))
	if err != nil {
		return BotAPIKey{}, identityWriteError("lookup bot api key", err)
	}
	return record, nil
}

// RevokeBotAPIKey marks a key revoked. It is idempotent: an already-revoked key
// keeps its first revocation time and reports success.
func (s *Store) RevokeBotAPIKey(ctx context.Context, tenantID, keyID uuid.UUID) error {
	if tenantID == uuid.Nil || keyID == uuid.Nil {
		return invalidIdentity("revoke bot api key", "tenant and key ids are required")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE bot_api_keys SET revoked_at = COALESCE(revoked_at, now()), updated_at = now()
		  WHERE tenant_id = $1 AND key_id = $2`, tenantID, keyID)
	if err != nil {
		return identityWriteError("revoke bot api key", err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	if _, err := s.GetBotAPIKey(ctx, tenantID, keyID); err != nil {
		return err
	}
	return nil
}

// GetBotAPIKey reads one key's metadata without resolving the credential (the
// management/list path).
func (s *Store) GetBotAPIKey(ctx context.Context, tenantID, keyID uuid.UUID) (BotAPIKey, error) {
	if tenantID == uuid.Nil || keyID == uuid.Nil {
		return BotAPIKey{}, invalidIdentity("get bot api key", "tenant and key ids are required")
	}
	record, err := scanBotAPIKeyRow(s.pool.QueryRow(ctx,
		`SELECT tenant_id, key_id, key_prefix, account_id, bot_id, label,
		        created_at, updated_at, revoked_at, last_used_at
		   FROM bot_api_keys WHERE tenant_id = $1 AND key_id = $2`, tenantID, keyID))
	if err != nil {
		return BotAPIKey{}, identityWriteError("get bot api key", err)
	}
	return record, nil
}

// TouchBotAPIKey records the last successful use. The write never moves the
// timestamp backwards: a delayed call with an older timestamp is ignored.
func (s *Store) TouchBotAPIKey(ctx context.Context, tenantID, keyID uuid.UUID, at time.Time) error {
	if tenantID == uuid.Nil || keyID == uuid.Nil || at.IsZero() {
		return invalidIdentity("touch bot api key", "tenant, key id and timestamp are required")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE bot_api_keys
		    SET last_used_at = GREATEST(COALESCE(last_used_at, $3), $3),
		        updated_at = now()
		  WHERE tenant_id = $1 AND key_id = $2`, tenantID, keyID, at)
	if err != nil {
		return identityWriteError("touch bot api key", err)
	}
	return identityRowsResult("touch bot api key", tag.RowsAffected())
}

// ListBotAPIKeys returns up to limit key metadata rows. An empty botID means
// every bot of the tenant; revoked keys are included so management can see
// them.
func (s *Store) ListBotAPIKeys(ctx context.Context, tenantID uuid.UUID, botID string, limit int) ([]BotAPIKey, error) {
	if tenantID == uuid.Nil {
		return nil, invalidIdentity("list bot api keys", "tenant id is required")
	}
	botID = strings.TrimSpace(botID)
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, key_id, key_prefix, account_id, bot_id, label,
		        created_at, updated_at, revoked_at, last_used_at
		   FROM bot_api_keys
		  WHERE tenant_id = $1 AND ($2 = '' OR bot_id = $2)
		  ORDER BY created_at DESC, key_id
		  LIMIT $3`, tenantID, botID, identityListLimit(limit))
	if err != nil {
		return nil, identityWriteError("list bot api keys", err)
	}
	defer rows.Close()
	out := make([]BotAPIKey, 0, 8)
	for rows.Next() {
		record, err := scanBotAPIKeyRow(rows)
		if err != nil {
			return nil, identityWriteError("scan bot api key", err)
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, identityWriteError("list bot api keys", err)
	}
	return out, nil
}

func scanBotAPIKeyRow(row rowScanner) (BotAPIKey, error) {
	var record BotAPIKey
	err := row.Scan(&record.TenantID, &record.KeyID, &record.Prefix, &record.AccountID,
		&record.BotID, &record.Label, &record.CreatedAt, &record.UpdatedAt,
		&record.RevokedAt, &record.LastUsedAt)
	return record, err
}
