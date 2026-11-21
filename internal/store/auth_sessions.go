// Browser auth sessions (change keel-bot-identity-access, A2).
//
// A session is a server-side row, not a self-contained token: the cookie names
// the session id and carries an HMAC, and every request re-reads this row. That
// is what makes logout, password change and account disable revoke access
// immediately — there is no "valid until expiry" cached authority.
//
// The row is tenant-scoped, but the cookie lookup happens before the tenant is
// known (the session id is globally unique). GetAuthSessionByID therefore runs
// on the operator pool, whose connections carry app.bypass_rls='on' — exactly
// the contract LookupAPIKey and LookupBotAPIKey already follow. The tenant
// returned by that lookup is the answer, never a caller-supplied header.
package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// AuthSession is one browser login. UserAgentHash/IPHash are SHA-256 digests;
// the raw user agent and address are never stored (design §2.2).
type AuthSession struct {
	TenantID      uuid.UUID
	SessionID     uuid.UUID
	AccountID     uuid.UUID
	IssuedAt      time.Time
	ExpiresAt     time.Time
	RevokedAt     *time.Time
	UserAgentHash []byte
	IPHash        []byte
}

// Active reports whether the session may authenticate a request at now.
func (s AuthSession) Active(now time.Time) bool {
	return s.RevokedAt == nil && now.Before(s.ExpiresAt)
}

// CreateAuthSession inserts one session. The caller minted the session id and
// chose the expiry; this layer only persists the fact.
func (s *Store) CreateAuthSession(ctx context.Context, record AuthSession) (AuthSession, error) {
	if record.TenantID == uuid.Nil || record.SessionID == uuid.Nil || record.AccountID == uuid.Nil {
		return AuthSession{}, invalidIdentity("create auth session", "tenant, session and account ids are required")
	}
	if record.IssuedAt.IsZero() {
		record.IssuedAt = time.Now().UTC()
	}
	if !record.ExpiresAt.After(record.IssuedAt) {
		return AuthSession{}, invalidIdentity("create auth session", "expiry must be after issue time")
	}
	if err := validateDigest("user agent hash", record.UserAgentHash); err != nil {
		return AuthSession{}, err
	}
	if err := validateDigest("ip hash", record.IPHash); err != nil {
		return AuthSession{}, err
	}
	created, err := scanAuthSessionRow(s.pool.QueryRow(ctx,
		`INSERT INTO auth_sessions
		    (tenant_id, session_id, account_id, issued_at, expires_at, user_agent_hash, ip_hash)
		 VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''::bytea), NULLIF($7, ''::bytea))
		 RETURNING tenant_id, session_id, account_id, issued_at, expires_at, revoked_at,
		           user_agent_hash, ip_hash`,
		record.TenantID, record.SessionID, record.AccountID, record.IssuedAt, record.ExpiresAt,
		nullableDigest(record.UserAgentHash), nullableDigest(record.IPHash)))
	if err != nil {
		return AuthSession{}, identityWriteError("create auth session", err)
	}
	return created, nil
}

// GetAuthSessionByID resolves the cookie's session id before the tenant is
// known. A missing row is ErrNotFound; the caller answers 401 and must not say
// whether the session was unknown, expired or revoked.
func (s *Store) GetAuthSessionByID(ctx context.Context, sessionID uuid.UUID) (AuthSession, error) {
	if sessionID == uuid.Nil {
		return AuthSession{}, invalidIdentity("get auth session", "session id is required")
	}
	record, err := scanAuthSessionRow(s.pool.QueryRow(ctx,
		`SELECT tenant_id, session_id, account_id, issued_at, expires_at, revoked_at,
		        user_agent_hash, ip_hash
		   FROM auth_sessions WHERE session_id = $1`, sessionID))
	if err != nil {
		return AuthSession{}, identityWriteError("get auth session", err)
	}
	return record, nil
}

// GetAuthSession reads one session inside a tenant (management/diagnostics).
func (s *Store) GetAuthSession(ctx context.Context, tenantID, sessionID uuid.UUID) (AuthSession, error) {
	if tenantID == uuid.Nil || sessionID == uuid.Nil {
		return AuthSession{}, invalidIdentity("get auth session", "tenant and session ids are required")
	}
	record, err := scanAuthSessionRow(s.pool.QueryRow(ctx,
		`SELECT tenant_id, session_id, account_id, issued_at, expires_at, revoked_at,
		        user_agent_hash, ip_hash
		   FROM auth_sessions WHERE tenant_id = $1 AND session_id = $2`, tenantID, sessionID))
	if err != nil {
		return AuthSession{}, identityWriteError("get auth session", err)
	}
	return record, nil
}

// RevokeAuthSession marks one session revoked. It is idempotent: revoking an
// already-revoked session succeeds and keeps the first revocation time. A
// session that does not exist in the tenant is ErrNotFound.
func (s *Store) RevokeAuthSession(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	if tenantID == uuid.Nil || sessionID == uuid.Nil {
		return invalidIdentity("revoke auth session", "tenant and session ids are required")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE auth_sessions SET revoked_at = COALESCE(revoked_at, now())
		  WHERE tenant_id = $1 AND session_id = $2`, tenantID, sessionID)
	if err != nil {
		return identityWriteError("revoke auth session", err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	if _, err := s.GetAuthSession(ctx, tenantID, sessionID); err != nil {
		return err
	}
	return nil
}

// RevokeAuthSessionsForAccount revokes every live session of one account and
// returns how many were newly revoked. A disabled account or a password change
// calls this before answering, so the next request fails even if the client
// still holds a cookie.
func (s *Store) RevokeAuthSessionsForAccount(ctx context.Context, tenantID, accountID uuid.UUID) (int64, error) {
	if tenantID == uuid.Nil || accountID == uuid.Nil {
		return 0, invalidIdentity("revoke account sessions", "tenant and account ids are required")
	}
	return revokeAccountSessions(ctx, s.pool, tenantID, accountID)
}

// revokeAccountSessions is the executor-neutral form: the pool (ordinary
// revocation) and a transaction (bootstrap/reset) both satisfy execer.
func revokeAccountSessions(ctx context.Context, q execer, tenantID, accountID uuid.UUID) (int64, error) {
	tag, err := q.Exec(ctx,
		`UPDATE auth_sessions SET revoked_at = now()
		  WHERE tenant_id = $1 AND account_id = $2 AND revoked_at IS NULL`,
		tenantID, accountID)
	if err != nil {
		return 0, identityWriteError("revoke account sessions", err)
	}
	return tag.RowsAffected(), nil
}

// ListAuthSessionsForAccount returns up to limit sessions of one account,
// newest first. Revoked sessions are included so an operator can see history;
// the bound is part of the contract (identityListLimit).
func (s *Store) ListAuthSessionsForAccount(ctx context.Context, tenantID, accountID uuid.UUID, limit int) ([]AuthSession, error) {
	if tenantID == uuid.Nil || accountID == uuid.Nil {
		return nil, invalidIdentity("list auth sessions", "tenant and account ids are required")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, session_id, account_id, issued_at, expires_at, revoked_at,
		        user_agent_hash, ip_hash
		   FROM auth_sessions
		  WHERE tenant_id = $1 AND account_id = $2
		  ORDER BY issued_at DESC, session_id
		  LIMIT $3`, tenantID, accountID, identityListLimit(limit))
	if err != nil {
		return nil, identityWriteError("list auth sessions", err)
	}
	defer rows.Close()
	out := make([]AuthSession, 0, 8)
	for rows.Next() {
		record, err := scanAuthSessionRow(rows)
		if err != nil {
			return nil, identityWriteError("scan auth session", err)
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, identityWriteError("list auth sessions", err)
	}
	return out, nil
}

func validateDigest(name string, digest []byte) error {
	if len(digest) != 0 && len(digest) != 32 {
		return invalidIdentity("auth session", "%s must be 32 bytes or empty", name)
	}
	return nil
}

func nullableDigest(digest []byte) []byte {
	if len(digest) == 0 {
		return nil
	}
	return digest
}

func scanAuthSessionRow(row rowScanner) (AuthSession, error) {
	var record AuthSession
	err := row.Scan(&record.TenantID, &record.SessionID, &record.AccountID,
		&record.IssuedAt, &record.ExpiresAt, &record.RevokedAt,
		&record.UserAgentHash, &record.IPHash)
	return record, err
}
