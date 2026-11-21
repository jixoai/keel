// Account bootstrap, reset and audited lifecycle writes (A2).
//
// The A1 schema deliberately has no "must change password" flag and no
// bootstrap-token table. This file uses auth_events as the durable fact instead
// of adding columns: a token is a hashed bootstrap.token event, consumption is
// a bootstrap.used event naming it, and "must change password" is the absence
// of a later password.changed event. That keeps the schema frozen while still
// surviving restarts and staying auditable.
package store

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BootstrapTokenTTL bounds a setup token's life. The CLI passes an explicit
// expiry; this constant is the default the caller uses.
const BootstrapTokenTTL = 30 * time.Minute

// BootstrapParams is one atomic setup-token redemption. The token hash must be
// 32 bytes (SHA-256 of the plaintext token).
type BootstrapParams struct {
	TenantID      uuid.UUID
	TokenHash     []byte
	Username      string
	DisplayName   string
	PasswordHash  string
	Now           time.Time
	UserAgentHash []byte
	IPHash        []byte
}

// scaffoldTokenDetail is the JSON stored in a bootstrap.token event.
type scaffoldTokenDetail struct {
	TokenHash string `json:"token_hash"`
	ExpiresAt string `json:"expires_at"`
	Actor     string `json:"actor,omitempty"`
}

// CreateBootstrapToken records one one-time setup token hash. The plaintext is
// never passed here: the CLI mints it, hashes it, prints it once and forgets it.
func (s *Store) CreateBootstrapToken(ctx context.Context, tenantID uuid.UUID, tokenHash []byte,
	expiresAt time.Time, actor string) (int64, error) {
	if tenantID == uuid.Nil {
		return 0, invalidIdentity("create bootstrap token", "tenant id is required")
	}
	if len(tokenHash) != 32 {
		return 0, invalidIdentity("create bootstrap token", "token hash must be 32 bytes")
	}
	if !expiresAt.After(time.Now().UTC()) {
		return 0, invalidIdentity("create bootstrap token", "expiry must be in the future")
	}
	detail, err := json.Marshal(scaffoldTokenDetail{
		TokenHash: hex.EncodeToString(tokenHash),
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339Nano),
		Actor:     strings.TrimSpace(actor),
	})
	if err != nil {
		return 0, err
	}
	var id int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO auth_events (tenant_id, kind, result, detail)
		 VALUES ($1, $2, $3, $4::jsonb) RETURNING event_id`,
		tenantID, AuthEventBootstrapToken, AuthResultOK, detail).Scan(&id); err != nil {
		return 0, identityWriteError("create bootstrap token", err)
	}
	return id, nil
}

// EnsureBootstrapToken records the token hash only when no bootstrap.token
// event with the same hash exists. It is the KEEL_BOOTSTRAP_TOKEN path: several
// replicas booting with the same env token converge on one event.
func (s *Store) EnsureBootstrapToken(ctx context.Context, tenantID uuid.UUID, tokenHash []byte,
	expiresAt time.Time, actor string) (bool, error) {
	if tenantID == uuid.Nil || len(tokenHash) != 32 {
		return false, invalidIdentity("ensure bootstrap token", "tenant and a 32-byte token hash are required")
	}
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM auth_events
		     WHERE tenant_id = $1 AND kind = $2 AND detail->>'token_hash' = $3
		 )`, tenantID, AuthEventBootstrapToken, hex.EncodeToString(tokenHash)).Scan(&exists); err != nil {
		return false, identityWriteError("ensure bootstrap token", err)
	}
	if exists {
		return false, nil
	}
	if _, err := s.CreateBootstrapToken(ctx, tenantID, tokenHash, expiresAt, actor); err != nil {
		return false, err
	}
	return true, nil
}

// LiveBootstrapToken reports whether an unconsumed, unexpired setup token
// exists. The CLI uses it to tell the operator a token is already outstanding
// instead of silently minting a second one.
func (s *Store) LiveBootstrapToken(ctx context.Context, tenantID uuid.UUID, now time.Time) (bool, error) {
	if tenantID == uuid.Nil {
		return false, invalidIdentity("live bootstrap token", "tenant id is required")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT t.event_id, t.detail
		   FROM auth_events t
		  WHERE t.tenant_id = $1 AND t.kind = $2
		    AND NOT EXISTS (
		        SELECT 1 FROM auth_events u
		         WHERE u.tenant_id = t.tenant_id AND u.kind = $3
		           AND u.detail->>'token_event_id' = t.event_id::text
		    )
		  ORDER BY t.event_id DESC LIMIT 20`,
		tenantID, AuthEventBootstrapToken, AuthEventBootstrapUsed)
	if err != nil {
		return false, identityWriteError("live bootstrap token", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id     int64
			raw    []byte
			detail scaffoldTokenDetail
		)
		if err := rows.Scan(&id, &raw); err != nil {
			return false, identityWriteError("live bootstrap token", err)
		}
		if err := json.Unmarshal(raw, &detail); err != nil {
			continue
		}
		expires, err := time.Parse(time.RFC3339Nano, detail.ExpiresAt)
		if err != nil || !now.Before(expires) {
			continue
		}
		return true, nil
	}
	return false, rows.Err()
}

// BootstrapSuperAdmin redeems one setup token and creates the first super
// admin atomically. The transaction locks the tenant's bootstrap events, so two
// concurrent redemptions cannot both consume one token or both create the
// first super admin.
func (s *Store) BootstrapSuperAdmin(ctx context.Context, params BootstrapParams) (AccountRecord, error) {
	username := strings.TrimSpace(params.Username)
	displayName := strings.TrimSpace(params.DisplayName)
	if displayName == "" {
		displayName = username
	}
	switch {
	case params.TenantID == uuid.Nil:
		return AccountRecord{}, invalidIdentity("bootstrap", "tenant id is required")
	case len(params.TokenHash) != 32:
		return AccountRecord{}, invalidIdentity("bootstrap", "token hash must be 32 bytes")
	case username == "":
		return AccountRecord{}, invalidIdentity("bootstrap", "username is required")
	case strings.TrimSpace(params.PasswordHash) == "":
		return AccountRecord{}, invalidIdentity("bootstrap", "password hash is required")
	}
	now := params.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AccountRecord{}, identityWriteError("bootstrap", err)
	}
	defer tx.Rollback(ctx)

	// Serialize bootstrap per tenant: the token row lock below is enough for a
	// single token, but the "no super admin exists" check needs the same gate.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(714325, hashtext($1::text))`,
		params.TenantID.String()); err != nil {
		return AccountRecord{}, identityWriteError("bootstrap lock", err)
	}

	// Validate the token before the "already bootstrapped" check: replaying a
	// used token must answer "invalid token" (401), not "already complete" (409).
	tokenEventID, expiresAt, err := findBootstrapToken(ctx, tx, params.TenantID, params.TokenHash)
	if err != nil {
		if errors.Is(err, ErrInvalid) {
			var hasSuper bool
			if scanErr := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM accounts WHERE is_super_admin AND disabled_at IS NULL)`).Scan(&hasSuper); scanErr != nil {
				return AccountRecord{}, identityWriteError("bootstrap super admin check", scanErr)
			}
			if hasSuper {
				return AccountRecord{}, conflictIdentity("bootstrap", "a super admin already exists")
			}
		}
		return AccountRecord{}, err
	}
	if !now.Before(expiresAt) {
		return AccountRecord{}, invalidIdentity("bootstrap", "setup token has expired")
	}
	var used bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM auth_events
		     WHERE tenant_id = $1 AND kind = $2 AND detail->>'token_event_id' = $3
		 )`, params.TenantID, AuthEventBootstrapUsed, fmt.Sprint(tokenEventID)).Scan(&used); err != nil {
		return AccountRecord{}, identityWriteError("bootstrap token used check", err)
	}
	if used {
		return AccountRecord{}, invalidIdentity("bootstrap", "setup token has already been used")
	}
	var hasSuper bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM accounts WHERE is_super_admin AND disabled_at IS NULL)`).Scan(&hasSuper); err != nil {
		return AccountRecord{}, identityWriteError("bootstrap super admin check", err)
	}
	if hasSuper {
		return AccountRecord{}, conflictIdentity("bootstrap", "a super admin already exists")
	}

	accountID := uuid.New()
	created, err := scanAccountRow(tx.QueryRow(ctx,
		`INSERT INTO accounts
		    (tenant_id, account_id, username, display_name, kind, password_hash, is_super_admin)
		 VALUES ($1, $2, $3, $4, 'human', $5, true)
		 RETURNING tenant_id, account_id, username, display_name, kind,
		           COALESCE(password_hash, ''), is_super_admin, disabled_at,
		           created_at, updated_at`,
		params.TenantID, accountID, username, displayName, params.PasswordHash))
	if err != nil {
		return AccountRecord{}, identityWriteError("bootstrap create account", err)
	}
	bootDetail, _ := json.Marshal(map[string]any{
		"must_change_password":     true,
		"username":                 created.Username,
		"bootstrap_token_event_id": tokenEventID,
	})
	if _, err := tx.Exec(ctx,
		`INSERT INTO auth_events (tenant_id, account_id, kind, result, detail)
		 VALUES ($1, $2, $3, $4, $5::jsonb)`,
		params.TenantID, created.ID, AuthEventAccountBoot, AuthResultOK, bootDetail); err != nil {
		return AccountRecord{}, identityWriteError("bootstrap audit account", err)
	}
	usedDetail, _ := json.Marshal(map[string]any{
		"token_event_id": tokenEventID,
		"account_id":     created.ID.String(),
		"username":       created.Username,
	})
	if _, err := tx.Exec(ctx,
		`INSERT INTO auth_events (tenant_id, account_id, kind, result, detail)
		 VALUES ($1, $2, $3, $4, $5::jsonb)`,
		params.TenantID, created.ID, AuthEventBootstrapUsed, AuthResultOK, usedDetail); err != nil {
		return AccountRecord{}, identityWriteError("bootstrap audit token use", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AccountRecord{}, identityWriteError("bootstrap commit", err)
	}
	return created, nil
}

// findBootstrapToken locks the tenant's recent token events and returns the one
// whose hash matches. Hashes are compared in constant time in Go; the SQL
// predicate would otherwise short-circuit on the first differing byte.
func findBootstrapToken(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, hash []byte) (int64, time.Time, error) {
	rows, err := tx.Query(ctx,
		`SELECT event_id, detail FROM auth_events
		  WHERE tenant_id = $1 AND kind = $2
		  ORDER BY event_id DESC LIMIT 50
		  FOR UPDATE`, tenantID, AuthEventBootstrapToken)
	if err != nil {
		return 0, time.Time{}, identityWriteError("bootstrap find token", err)
	}
	defer rows.Close()
	want := []byte(hex.EncodeToString(hash))
	for rows.Next() {
		var (
			id     int64
			raw    []byte
			detail scaffoldTokenDetail
		)
		if err := rows.Scan(&id, &raw); err != nil {
			return 0, time.Time{}, identityWriteError("bootstrap scan token", err)
		}
		if err := json.Unmarshal(raw, &detail); err != nil {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(detail.TokenHash), want) != 1 {
			continue
		}
		expires, err := time.Parse(time.RFC3339Nano, detail.ExpiresAt)
		if err != nil {
			return 0, time.Time{}, invalidIdentity("bootstrap", "stored token expiry is invalid")
		}
		return id, expires, nil
	}
	if err := rows.Err(); err != nil {
		return 0, time.Time{}, identityWriteError("bootstrap find token", err)
	}
	return 0, time.Time{}, invalidIdentity("bootstrap", "setup token is invalid")
}

// CreateAccountAudited inserts a human/bot/service account and its
// account.created event in one transaction.
func (s *Store) CreateAccountAudited(ctx context.Context, record AccountRecord, actor uuid.UUID) (AccountRecord, error) {
	if err := validateAccountRecord(record); err != nil {
		return AccountRecord{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AccountRecord{}, identityWriteError("create account", err)
	}
	defer tx.Rollback(ctx)
	created, err := scanAccountRow(tx.QueryRow(ctx,
		`INSERT INTO accounts
		    (tenant_id, account_id, username, display_name, kind, password_hash, is_super_admin)
		 VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7)
		 RETURNING tenant_id, account_id, username, display_name, kind,
		           COALESCE(password_hash, ''), is_super_admin, disabled_at,
		           created_at, updated_at`,
		record.TenantID, record.ID, record.Username, record.DisplayName, record.Kind,
		record.PasswordHash, record.IsSuperAdmin))
	if err != nil {
		return AccountRecord{}, identityWriteError("create account", err)
	}
	detail, _ := json.Marshal(map[string]any{
		"username":       created.Username,
		"kind":           created.Kind,
		"is_super_admin": created.IsSuperAdmin,
	})
	var actorPtr *uuid.UUID
	if actor != uuid.Nil {
		actorPtr = &actor
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO auth_events (tenant_id, account_id, actor_account_id, kind, result, detail)
		 VALUES ($1, $2, $3, $4, $5, $6::jsonb)`,
		created.TenantID, created.ID, actorPtr, AuthEventAccountCreated, AuthResultOK, detail); err != nil {
		return AccountRecord{}, identityWriteError("audit account create", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AccountRecord{}, identityWriteError("create account commit", err)
	}
	return created, nil
}

// ChangeAccountPassword stores a new hash, revokes every session of the account
// and records password.changed — all in one transaction. The login that
// performed the change then gets a fresh session; every older cookie is dead.
func (s *Store) ChangeAccountPassword(ctx context.Context, tenantID, accountID uuid.UUID,
	passwordHash string, actor uuid.UUID) (AccountRecord, error) {
	if tenantID == uuid.Nil || accountID == uuid.Nil || strings.TrimSpace(passwordHash) == "" {
		return AccountRecord{}, invalidIdentity("change password", "tenant, account and password hash are required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AccountRecord{}, identityWriteError("change password", err)
	}
	defer tx.Rollback(ctx)
	record, err := scanAccountRow(tx.QueryRow(ctx,
		`UPDATE accounts SET password_hash = $3, updated_at = now()
		  WHERE tenant_id = $1 AND account_id = $2
		  RETURNING tenant_id, account_id, username, display_name, kind,
		            COALESCE(password_hash, ''), is_super_admin, disabled_at,
		            created_at, updated_at`, tenantID, accountID, passwordHash))
	if err != nil {
		return AccountRecord{}, identityWriteError("change password", err)
	}
	if _, err := revokeAccountSessions(ctx, tx, tenantID, accountID); err != nil {
		return AccountRecord{}, err
	}
	detail, _ := json.Marshal(map[string]any{"by": "account"})
	var actorPtr *uuid.UUID
	if actor != uuid.Nil {
		actorPtr = &actor
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO auth_events (tenant_id, account_id, actor_account_id, kind, result, detail)
		 VALUES ($1, $2, $3, $4, $5, $6::jsonb)`,
		tenantID, accountID, actorPtr, AuthEventPasswordChange, AuthResultOK, detail); err != nil {
		return AccountRecord{}, identityWriteError("audit password change", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AccountRecord{}, identityWriteError("change password commit", err)
	}
	return record, nil
}

// ResetAccountPassword is the local-CLI recovery path: set a temporary hash,
// revoke every session and record password.reset (which makes the next login
// require a change). It is deliberately not reachable over HTTP.
func (s *Store) ResetAccountPassword(ctx context.Context, tenantID uuid.UUID, username,
	passwordHash, actor string) (AccountRecord, error) {
	username = strings.TrimSpace(username)
	if tenantID == uuid.Nil || username == "" || strings.TrimSpace(passwordHash) == "" {
		return AccountRecord{}, invalidIdentity("reset password", "tenant, username and password hash are required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AccountRecord{}, identityWriteError("reset password", err)
	}
	defer tx.Rollback(ctx)
	record, err := scanAccountRow(tx.QueryRow(ctx,
		`UPDATE accounts SET password_hash = $3, updated_at = now()
		  WHERE tenant_id = $1 AND lower(username) = lower($2)
		  RETURNING tenant_id, account_id, username, display_name, kind,
		            COALESCE(password_hash, ''), is_super_admin, disabled_at,
		            created_at, updated_at`, tenantID, username, passwordHash))
	if err != nil {
		return AccountRecord{}, identityWriteError("reset password", err)
	}
	if _, err := revokeAccountSessions(ctx, tx, tenantID, record.ID); err != nil {
		return AccountRecord{}, err
	}
	detail, _ := json.Marshal(map[string]any{
		"by":                   "local-cli",
		"actor":                strings.TrimSpace(actor),
		"must_change_password": true,
	})
	if _, err := tx.Exec(ctx,
		`INSERT INTO auth_events (tenant_id, account_id, kind, result, detail)
		 VALUES ($1, $2, $3, $4, $5::jsonb)`,
		tenantID, record.ID, AuthEventPasswordReset, AuthResultOK, detail); err != nil {
		return AccountRecord{}, identityWriteError("audit password reset", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AccountRecord{}, identityWriteError("reset password commit", err)
	}
	return record, nil
}

// DisableAccountAudited disables or re-enables an account, revokes its sessions
// when disabling and records the change. A super admin cannot be disabled: the
// deployment must always keep at least one enabled super admin, and this is the
// only lifecycle write that could remove one.
func (s *Store) DisableAccountAudited(ctx context.Context, tenantID, accountID, actor uuid.UUID,
	disable bool, reason string) (AccountRecord, error) {
	if tenantID == uuid.Nil || accountID == uuid.Nil {
		return AccountRecord{}, invalidIdentity("disable account", "tenant and account ids are required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AccountRecord{}, identityWriteError("disable account", err)
	}
	defer tx.Rollback(ctx)
	current, err := scanAccountRow(tx.QueryRow(ctx,
		`SELECT tenant_id, account_id, username, display_name, kind,
		        COALESCE(password_hash, ''), is_super_admin, disabled_at,
		        created_at, updated_at
		   FROM accounts WHERE tenant_id = $1 AND account_id = $2 FOR UPDATE`,
		tenantID, accountID))
	if err != nil {
		return AccountRecord{}, identityWriteError("disable account", err)
	}
	if disable && current.IsSuperAdmin {
		return AccountRecord{}, fmt.Errorf("%w: %w: a super admin cannot be disabled", ErrConflict, ErrSuperAdminProtected)
	}
	record, err := scanAccountRow(tx.QueryRow(ctx,
		`UPDATE accounts
		    SET disabled_at = CASE WHEN $3 THEN COALESCE(disabled_at, now()) ELSE NULL END,
		        updated_at = now()
		  WHERE tenant_id = $1 AND account_id = $2
		  RETURNING tenant_id, account_id, username, display_name, kind,
		            COALESCE(password_hash, ''), is_super_admin, disabled_at,
		            created_at, updated_at`, tenantID, accountID, disable))
	if err != nil {
		return AccountRecord{}, identityWriteError("disable account", err)
	}
	if disable {
		if _, err := revokeAccountSessions(ctx, tx, tenantID, accountID); err != nil {
			return AccountRecord{}, err
		}
	}
	kind := AuthEventAccountEnable
	if disable {
		kind = AuthEventAccountDisable
	}
	detail, _ := json.Marshal(map[string]any{
		"reason":   strings.TrimSpace(reason),
		"username": record.Username,
	})
	var actorPtr *uuid.UUID
	if actor != uuid.Nil {
		actorPtr = &actor
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO auth_events (tenant_id, account_id, actor_account_id, kind, result, detail)
		 VALUES ($1, $2, $3, $4, $5, $6::jsonb)`,
		tenantID, accountID, actorPtr, kind, AuthResultOK, detail); err != nil {
		return AccountRecord{}, identityWriteError("audit account disable", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AccountRecord{}, identityWriteError("disable account commit", err)
	}
	return record, nil
}

// IsSuperAdmin reports whether one account is an enabled super admin. The
// policy layer (A3) will replace this with the full permission evaluator; A2
// uses it only for the bootstrap endpoints' fail-closed checks.
func (s *Store) IsSuperAdmin(ctx context.Context, tenantID, accountID uuid.UUID) (bool, error) {
	record, err := s.GetAccount(ctx, tenantID, accountID)
	if err != nil {
		return false, err
	}
	return record.IsSuperAdmin && record.DisabledAt == nil, nil
}
