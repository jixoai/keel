// Auth audit events (change keel-bot-identity-access, A2).
//
// auth_events is append-only: one row per login attempt, logout, credential
// change, account lifecycle change and super-admin action. It is also the
// durable state behind two derived facts that deliberately have no column in
// the A1 schema:
//
//   - "the account must change its password" — an account.bootstrap or
//     password.reset event with no later password.changed event;
//   - "this bootstrap token was used" — a bootstrap.used event naming the
//     token event id. Tokens themselves are stored as hashes in a
//     bootstrap.token event, so no schema change is needed and the whole
//     story stays queryable.
//
// Details never carry a password, a password hash, a token plaintext or a raw
// IP/user-agent: hashes and identifiers only.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Auth event kinds. The column is free text (A1 schema); these constants are
// the one vocabulary the store, service and CLI share.
const (
	AuthEventLoginSucceeded = "login.succeeded"
	AuthEventLoginFailed    = "login.failed"
	AuthEventLoginRateLimit = "login.rate_limited"
	AuthEventSessionReject  = "session.rejected"
	AuthEventLogout         = "logout"
	AuthEventPasswordChange = "password.changed"
	AuthEventPasswordReset  = "password.reset"
	AuthEventAccountCreated = "account.created"
	AuthEventAccountDisable = "account.disabled"
	AuthEventAccountEnable  = "account.enabled"
	AuthEventAccountBoot    = "account.bootstrap"
	AuthEventBootstrapToken = "bootstrap.token"
	AuthEventBootstrapUsed  = "bootstrap.used"
	AuthEventBotKeyIssued   = "bot_key.issued"
	AuthEventBotKeyRevoked  = "bot_key.revoked"
	AuthEventSuperAdmin     = "superadmin.action"
)

// Auth result values.
const (
	AuthResultOK      = "ok"
	AuthResultFailed  = "failed"
	AuthResultDenied  = "denied"
	AuthResultRefused = "refused"
)

// ErrSuperAdminProtected is returned by the account lifecycle writes when the
// target is (or would cease to be) an enabled super admin. It wraps
// ErrConflict, so an HTTP layer can answer 409/403 without parsing prose.
var ErrSuperAdminProtected = errors.New("store: super admin is protected")

// AuthEvent is one audit row. AccountID is nil for a failed login that named
// no account; ActorAccountID is the operator for super-admin actions.
type AuthEvent struct {
	TenantID       uuid.UUID
	EventID        int64
	AccountID      *uuid.UUID
	ActorAccountID *uuid.UUID
	Kind           string
	Result         string
	Detail         map[string]any
	CreatedAt      time.Time
}

// RecordAuthEvent appends one audit row and returns its id. An empty result is
// AuthResultOK; an empty kind is invalid input.
func (s *Store) RecordAuthEvent(ctx context.Context, event AuthEvent) (int64, error) {
	if event.TenantID == uuid.Nil {
		return 0, invalidIdentity("record auth event", "tenant id is required")
	}
	event.Kind = strings.TrimSpace(event.Kind)
	if event.Kind == "" {
		return 0, invalidIdentity("record auth event", "kind is required")
	}
	if event.Result == "" {
		event.Result = AuthResultOK
	}
	detail := event.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return 0, invalidIdentity("record auth event", "detail is not JSON: %v", err)
	}
	var id int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO auth_events
		    (tenant_id, account_id, actor_account_id, kind, result, detail)
		 VALUES ($1, $2, $3, $4, $5, $6::jsonb)
		 RETURNING event_id`,
		event.TenantID, event.AccountID, event.ActorAccountID, event.Kind, event.Result, raw).Scan(&id); err != nil {
		return 0, identityWriteError("record auth event", err)
	}
	return id, nil
}

// AuthEventFilter bounds a read of the audit trail. A zero field is "any".
type AuthEventFilter struct {
	AccountID uuid.UUID
	Kind      string
	Since     time.Time
}

// ListAuthEvents returns up to limit audit rows, newest first. The result is
// always bounded; there is no unbounded audit export in the request path.
func (s *Store) ListAuthEvents(ctx context.Context, tenantID uuid.UUID, filter AuthEventFilter, limit int) ([]AuthEvent, error) {
	if tenantID == uuid.Nil {
		return nil, invalidIdentity("list auth events", "tenant id is required")
	}
	var since any
	if !filter.Since.IsZero() {
		since = filter.Since
	}
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, event_id, account_id, actor_account_id, kind, result, detail, created_at
		   FROM auth_events
		  WHERE tenant_id = $1
		    AND ($2::uuid IS NULL OR account_id = $2)
		    AND ($3 = '' OR kind = $3)
		    AND ($4::timestamptz IS NULL OR created_at >= $4)
		  ORDER BY event_id DESC
		  LIMIT $5`,
		tenantID, nullableUUID(filter.AccountID), strings.TrimSpace(filter.Kind), since,
		identityListLimit(limit))
	if err != nil {
		return nil, identityWriteError("list auth events", err)
	}
	defer rows.Close()
	out := make([]AuthEvent, 0, 16)
	for rows.Next() {
		event, err := scanAuthEventRow(rows)
		if err != nil {
			return nil, identityWriteError("scan auth event", err)
		}
		out = append(out, event)
	}
	if err := rows.Err(); err != nil {
		return nil, identityWriteError("list auth events", err)
	}
	return out, nil
}

// HasSuperAdmin reports whether the deployment has at least one enabled super
// admin. It is the "bootstrap required" predicate, and the account lifecycle
// writes use the same predicate to protect the last one.
func (s *Store) HasSuperAdmin(ctx context.Context) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM accounts
		                 WHERE is_super_admin AND disabled_at IS NULL)`).Scan(&exists); err != nil {
		return false, identityWriteError("has super admin", err)
	}
	return exists, nil
}

// AccountMustChangePassword reports whether the account's newest credential
// fact is a bootstrap/reset that no later password change has superseded.
func (s *Store) AccountMustChangePassword(ctx context.Context, tenantID, accountID uuid.UUID) (bool, error) {
	if tenantID == uuid.Nil || accountID == uuid.Nil {
		return false, invalidIdentity("account must change password", "tenant and account ids are required")
	}
	var must bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM auth_events b
		     WHERE b.tenant_id = $1 AND b.account_id = $2
		       AND b.kind IN ($3, $4)
		       AND NOT EXISTS (
		           SELECT 1 FROM auth_events c
		            WHERE c.tenant_id = b.tenant_id AND c.account_id = b.account_id
		              AND c.kind = $5 AND c.event_id > b.event_id
		       )
		 )`,
		tenantID, accountID, AuthEventAccountBoot, AuthEventPasswordReset,
		AuthEventPasswordChange).Scan(&must); err != nil {
		return false, identityWriteError("account must change password", err)
	}
	return must, nil
}

func scanAuthEventRow(row rowScanner) (AuthEvent, error) {
	var (
		event AuthEvent
		raw   []byte
	)
	if err := row.Scan(&event.TenantID, &event.EventID, &event.AccountID,
		&event.ActorAccountID, &event.Kind, &event.Result, &raw, &event.CreatedAt); err != nil {
		return AuthEvent{}, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &event.Detail); err != nil {
			return AuthEvent{}, err
		}
	}
	if event.Detail == nil {
		event.Detail = map[string]any{}
	}
	return event, nil
}
