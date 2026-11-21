// Login service (change keel-bot-identity-access, A2).
//
// The service owns the rules that sit between the HTTP surface and the store:
// password verification with uniform failures, the username+IP limiter, session
// issuance and revocation, bootstrap/reset policy and the super-admin guard.
// HTTP handlers stay a thin translation of these results into status codes and
// cookies; they never touch a password hash.
//
// Two login methods share one authority model:
//   - a human presents username+password and gets a server-side session;
//   - a bot presents a key bound to (account, bot) and never gets a session.
//
// The principal in both cases is an Account. A bot principal is explicitly not
// a super admin: the spec forbids delegating deployment authority to a bot even
// if its account row somehow says otherwise.
package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/store"
)

// Principal kinds.
const (
	PrincipalHuman = "human"
	PrincipalBot   = "bot"
	PrincipalLocal = "local"
)

// Service errors. HTTP maps each to one status; the response body never says
// which credential component was wrong.
var (
	ErrInvalidCredentials    = errors.New("auth: invalid credentials")
	ErrSessionInvalid        = errors.New("auth: session is invalid")
	ErrBootstrapTokenInvalid = errors.New("auth: setup token is invalid")
	ErrBootstrapComplete     = errors.New("auth: bootstrap is already complete")
	ErrUsernameTaken         = errors.New("auth: username is already taken")
	ErrPasswordReused        = errors.New("auth: new password must differ from the current one")
	ErrForbidden             = errors.New("auth: operation is not permitted")
	ErrTenantNotConfigured   = errors.New("auth: tenant is not configured")
	ErrInvalidBotKey         = errors.New("auth: invalid bot key")
	ErrInvalidAccount        = errors.New("auth: invalid account input")
)

// RateLimitedError carries the wait the caller must observe. It is returned
// before any account lookup, so a locked username and a locked IP answer
// identically.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("auth: too many login attempts; retry in %s", e.RetryAfter.Round(time.Second))
}

// Principal is the authenticated caller. SessionID is empty for a bot key and
// BotID is empty for a human/local caller.
type Principal struct {
	Kind         string
	TenantID     uuid.UUID
	AccountID    uuid.UUID
	SessionID    uuid.UUID
	BotID        string
	KeyID        uuid.UUID
	KeyPrefix    string
	Username     string
	DisplayName  string
	IsSuperAdmin bool
	// MustChangePassword restricts the caller to the password-change surface
	// until it changes its credential.
	MustChangePassword bool
}

// IsHuman reports whether the principal authenticated with a browser session.
func (p Principal) IsHuman() bool { return p.Kind == PrincipalHuman || p.Kind == PrincipalLocal }

// IsBot reports whether the principal is a bot key.
func (p Principal) IsBot() bool { return p.Kind == PrincipalBot }

// Service is the login/auth policy over one store and one deployment tenant.
type Service struct {
	Store      *store.Store
	TenantID   uuid.UUID
	SessionTTL time.Duration
	Now        func() time.Time
	Limiter    *Limiter

	dummyOnce sync.Once
	dummyHash string
	dummyErr  error
}

// NewService builds the login service with a 12h session TTL.
func NewService(st *store.Store, tenantID uuid.UUID) *Service {
	return &Service{
		Store: st, TenantID: tenantID, SessionTTL: 12 * time.Hour,
		Now: func() time.Time { return time.Now().UTC() }, Limiter: NewLimiter(),
	}
}

// LoginInput is one password login attempt.
type LoginInput struct {
	Username  string
	Password  string
	UserAgent string
	IP        string
}

// SessionResult is the outcome of a successful login or password change.
type SessionResult struct {
	Principal Principal
	Session   store.AuthSession
}

// Login verifies a password and issues a persisted session. Every failure that
// concerns the credential is ErrInvalidCredentials; only a rate lock and an
// internal failure are distinguishable.
func (s *Service) Login(ctx context.Context, input LoginInput) (SessionResult, error) {
	if s == nil || s.Store == nil {
		return SessionResult{}, errors.New("auth: login service is not configured")
	}
	tenantID := s.TenantID
	if tenantID == uuid.Nil {
		return SessionResult{}, ErrTenantNotConfigured
	}
	username := strings.TrimSpace(input.Username)
	ip := strings.TrimSpace(input.IP)
	if wait := s.Limiter.Check(username, ip); wait > 0 {
		s.audit(ctx, store.AuthEvent{
			TenantID: tenantID, Kind: store.AuthEventLoginRateLimit, Result: store.AuthResultRefused,
			Detail: map[string]any{
				"username":    truncate(username, 128),
				"ip_hash":     hashHex(ip),
				"retry_after": int(wait.Round(time.Second).Seconds()),
			},
		})
		return SessionResult{}, &RateLimitedError{RetryAfter: wait}
	}
	account, err := s.Store.GetAccountByUsername(ctx, tenantID, username)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return SessionResult{}, err
		}
		s.verifyDummy(input.Password)
		s.limiterFailure(ctx, username, ip, "unknown_account")
		return SessionResult{}, ErrInvalidCredentials
	}
	if account.Kind == "bot" || strings.TrimSpace(account.PasswordHash) == "" {
		s.verifyDummy(input.Password)
		s.limiterFailure(ctx, account.Username, ip, "password_login_not_allowed")
		return SessionResult{}, ErrInvalidCredentials
	}
	if err := VerifyPassword(account.PasswordHash, input.Password); err != nil {
		reason := "password_mismatch"
		if errors.Is(err, ErrInvalidPasswordHash) {
			reason = "stored_hash_invalid"
		}
		s.limiterFailure(ctx, account.Username, ip, reason)
		return SessionResult{}, ErrInvalidCredentials
	}
	if account.DisabledAt != nil {
		s.limiterFailure(ctx, account.Username, ip, "account_disabled")
		return SessionResult{}, ErrInvalidCredentials
	}
	// A verified weaker/older hash is upgraded silently. It is not a password
	// change: no password.changed event is written, so a pending must-change
	// fact survives.
	if NeedsRehash(account.PasswordHash) {
		if upgraded, err := HashPassword(input.Password); err == nil {
			_ = s.Store.SetAccountPasswordHash(ctx, tenantID, account.ID, upgraded)
		}
	}
	s.Limiter.Success(account.Username, ip)
	mustChange, err := s.Store.AccountMustChangePassword(ctx, tenantID, account.ID)
	if err != nil {
		return SessionResult{}, err
	}
	session, err := s.createSession(ctx, account, input.UserAgent, ip)
	if err != nil {
		return SessionResult{}, err
	}
	s.audit(ctx, store.AuthEvent{
		TenantID: tenantID, AccountID: &account.ID, Kind: store.AuthEventLoginSucceeded,
		Detail: map[string]any{
			"username":             account.Username,
			"session_id":           session.SessionID.String(),
			"ip_hash":              hashHex(ip),
			"user_agent_hash":      hashHex(input.UserAgent),
			"must_change_password": mustChange,
		},
	})
	return SessionResult{
		Principal: Principal{
			Kind: PrincipalHuman, TenantID: tenantID, AccountID: account.ID,
			SessionID: session.SessionID, Username: account.Username,
			DisplayName: account.DisplayName, IsSuperAdmin: account.IsSuperAdmin,
			MustChangePassword: mustChange,
		},
		Session: session,
	}, nil
}

// AuthenticateSession resolves a cookie's session id to a principal. A missing,
// revoked or expired row, a disabled account and a deleted account all answer
// ErrSessionInvalid; the middleware must not distinguish them.
func (s *Service) AuthenticateSession(ctx context.Context, sessionID uuid.UUID) (Principal, error) {
	if s == nil || s.Store == nil || sessionID == uuid.Nil {
		return Principal{}, ErrSessionInvalid
	}
	session, err := s.Store.GetAuthSessionByID(ctx, sessionID)
	if err != nil || !session.Active(s.now()) {
		return Principal{}, ErrSessionInvalid
	}
	// The serving process acts as one tenant (KEEL_SERVE_TENANT). A session
	// minted for another tenant is refused here rather than injecting that
	// tenant's id into this server's kernel boundary.
	if session.TenantID != s.TenantID {
		return Principal{}, ErrSessionInvalid
	}
	account, err := s.Store.GetAccount(ctx, session.TenantID, session.AccountID)
	if err != nil {
		return Principal{}, ErrSessionInvalid
	}
	if account.DisabledAt != nil {
		_ = s.Store.RevokeAuthSession(ctx, session.TenantID, session.SessionID)
		s.audit(ctx, store.AuthEvent{
			TenantID: session.TenantID, AccountID: &account.ID, Kind: store.AuthEventSessionReject,
			Result: store.AuthResultDenied,
			Detail: map[string]any{"reason": "account_disabled", "session_id": session.SessionID.String()},
		})
		return Principal{}, ErrSessionInvalid
	}
	mustChange, err := s.Store.AccountMustChangePassword(ctx, session.TenantID, account.ID)
	if err != nil {
		return Principal{}, err
	}
	return Principal{
		Kind: PrincipalHuman, TenantID: session.TenantID, AccountID: account.ID,
		SessionID: session.SessionID, Username: account.Username, DisplayName: account.DisplayName,
		IsSuperAdmin: account.IsSuperAdmin, MustChangePassword: mustChange,
	}, nil
}

// Logout revokes the caller's session and records the fact. It is idempotent:
// logging out twice succeeds.
func (s *Service) Logout(ctx context.Context, principal Principal) error {
	if principal.SessionID == uuid.Nil {
		return nil
	}
	if err := s.Store.RevokeAuthSession(ctx, principal.TenantID, principal.SessionID); err != nil &&
		!errors.Is(err, store.ErrNotFound) {
		return err
	}
	s.audit(ctx, store.AuthEvent{
		TenantID: principal.TenantID, AccountID: &principal.AccountID, Kind: store.AuthEventLogout,
		Detail: map[string]any{"session_id": principal.SessionID.String()},
	})
	return nil
}

// ChangePassword verifies the current password, stores the new hash, revokes
// every existing session (including this one) and issues a fresh session for
// the caller. The new session is the only live cookie after the call.
func (s *Service) ChangePassword(ctx context.Context, principal Principal, current, next, userAgent, ip string) (SessionResult, error) {
	if principal.Kind != PrincipalHuman && principal.Kind != PrincipalLocal {
		return SessionResult{}, ErrForbidden
	}
	account, err := s.Store.GetAccount(ctx, principal.TenantID, principal.AccountID)
	if err != nil {
		return SessionResult{}, ErrSessionInvalid
	}
	if account.DisabledAt != nil {
		return SessionResult{}, ErrSessionInvalid
	}
	if strings.TrimSpace(account.PasswordHash) == "" {
		return SessionResult{}, ErrInvalidCredentials
	}
	if err := VerifyPassword(account.PasswordHash, current); err != nil {
		s.audit(ctx, store.AuthEvent{
			TenantID: principal.TenantID, AccountID: &account.ID, Kind: store.AuthEventPasswordChange,
			Result: store.AuthResultFailed, Detail: map[string]any{"reason": "current_password_mismatch"},
		})
		return SessionResult{}, ErrInvalidCredentials
	}
	if current == next {
		return SessionResult{}, ErrPasswordReused
	}
	hash, err := HashPassword(next)
	if err != nil {
		return SessionResult{}, err
	}
	if _, err := s.Store.ChangeAccountPassword(ctx, principal.TenantID, account.ID, hash, account.ID); err != nil {
		return SessionResult{}, err
	}
	if account.IsSuperAdmin {
		s.audit(ctx, store.AuthEvent{
			TenantID: principal.TenantID, AccountID: &account.ID, ActorAccountID: &account.ID,
			Kind:   store.AuthEventSuperAdmin,
			Detail: map[string]any{"action": "password.change"},
		})
	}
	session, err := s.createSession(ctx, account, userAgent, ip)
	if err != nil {
		return SessionResult{}, err
	}
	return SessionResult{
		Principal: Principal{
			Kind: PrincipalHuman, TenantID: principal.TenantID, AccountID: account.ID,
			SessionID: session.SessionID, Username: account.Username, DisplayName: account.DisplayName,
			IsSuperAdmin: account.IsSuperAdmin, MustChangePassword: false,
		},
		Session: session,
	}, nil
}

// Bootstrap redeems a one-time setup token and creates the first super admin.
// It does not log the browser in: the account deliberately starts in the
// must-change state, so the first login is followed by a password change.
func (s *Service) Bootstrap(ctx context.Context, token, username, password, displayName string) (store.AccountRecord, error) {
	if s == nil || s.Store == nil {
		return store.AccountRecord{}, errors.New("auth: login service is not configured")
	}
	if s.TenantID == uuid.Nil {
		return store.AccountRecord{}, ErrTenantNotConfigured
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return store.AccountRecord{}, ErrBootstrapTokenInvalid
	}
	tokenHash := HashToken(token)
	// Validate the setup token before hashing the password: an invalid token
	// must not cost a 64MiB Argon2 attempt. The authoritative redemption below
	// remains atomic.
	state, err := s.Store.BootstrapTokenState(ctx, s.TenantID, tokenHash, s.now())
	if err != nil {
		return store.AccountRecord{}, err
	}
	switch state {
	case store.BootstrapTokenValid:
		// proceed
	case store.BootstrapTokenUsed, store.BootstrapTokenExpired:
		return store.AccountRecord{}, ErrBootstrapTokenInvalid
	default:
		has, err := s.Store.HasSuperAdmin(ctx)
		if err != nil {
			return store.AccountRecord{}, err
		}
		if has {
			return store.AccountRecord{}, ErrBootstrapComplete
		}
		return store.AccountRecord{}, ErrBootstrapTokenInvalid
	}
	hash, err := HashPassword(password)
	if err != nil {
		return store.AccountRecord{}, err
	}
	account, err := s.Store.BootstrapSuperAdmin(ctx, store.BootstrapParams{
		TenantID: s.TenantID, TokenHash: tokenHash,
		Username: username, DisplayName: displayName, PasswordHash: hash, Now: s.now(),
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrBootstrapAlreadyComplete):
			return store.AccountRecord{}, ErrBootstrapComplete
		case errors.Is(err, store.ErrConflict):
			return store.AccountRecord{}, ErrUsernameTaken
		case errors.Is(err, store.ErrInvalid):
			return store.AccountRecord{}, ErrBootstrapTokenInvalid
		default:
			return store.AccountRecord{}, err
		}
	}
	return account, nil
}

// BootstrapRequired reports whether the deployment has no enabled super admin.
func (s *Service) BootstrapRequired(ctx context.Context) (bool, error) {
	if s == nil || s.Store == nil {
		return false, errors.New("auth: login service is not configured")
	}
	has, err := s.Store.HasSuperAdmin(ctx)
	if err != nil {
		return false, err
	}
	return !has, nil
}

// AccountInput is one admin-created account.
type AccountInput struct {
	Username    string
	DisplayName string
	Kind        string
	Password    string
}

// CreateAccount creates a human/bot/service account on behalf of a super admin.
// A bot account cannot have a password: its credential is a bot key.
func (s *Service) CreateAccount(ctx context.Context, actor Principal, input AccountInput) (store.AccountRecord, error) {
	if err := s.requireSuperAdmin(ctx, actor); err != nil {
		return store.AccountRecord{}, err
	}
	kind := strings.TrimSpace(input.Kind)
	if kind == "" {
		kind = "human"
	}
	if kind != "human" && kind != "bot" && kind != "service" {
		return store.AccountRecord{}, ErrInvalidAccount
	}
	passwordHash := ""
	if strings.TrimSpace(input.Password) != "" {
		if kind == "bot" {
			return store.AccountRecord{}, ErrInvalidAccount
		}
		hash, err := HashPassword(input.Password)
		if err != nil {
			return store.AccountRecord{}, err
		}
		passwordHash = hash
	} else if kind == "human" {
		return store.AccountRecord{}, ErrInvalidAccount
	}
	displayName := strings.TrimSpace(input.DisplayName)
	if displayName == "" {
		displayName = strings.TrimSpace(input.Username)
	}
	return s.Store.CreateAccountAudited(ctx, store.AccountRecord{
		TenantID: actor.TenantID, ID: uuid.New(), Username: strings.TrimSpace(input.Username),
		DisplayName: displayName, Kind: kind, PasswordHash: passwordHash,
	}, actor.AccountID)
}

// DisableAccount disables or re-enables an account. A super admin cannot be
// disabled; the store enforces the same invariant.
func (s *Service) DisableAccount(ctx context.Context, actor Principal, accountID uuid.UUID, disable bool, reason string) (store.AccountRecord, error) {
	if err := s.requireSuperAdmin(ctx, actor); err != nil {
		return store.AccountRecord{}, err
	}
	record, err := s.Store.DisableAccountAudited(ctx, actor.TenantID, accountID, actor.AccountID, disable, reason)
	if errors.Is(err, store.ErrSuperAdminProtected) {
		return store.AccountRecord{}, ErrForbidden
	}
	return record, err
}

// BotKeyIssue is the result of issuing one credential. Plaintext is the only
// copy and must be returned to the caller immediately.
type BotKeyIssue struct {
	Plaintext string
	Key       store.BotAPIKey
}

// IssueBotKey mints a credential for (account, bot). The bot's profile must name
// the same account: a key may never let one account act as another's bot.
func (s *Service) IssueBotKey(ctx context.Context, actor Principal, accountID uuid.UUID, botID, label string) (BotKeyIssue, error) {
	if err := s.requireSuperAdmin(ctx, actor); err != nil {
		return BotKeyIssue{}, err
	}
	account, err := s.Store.GetAccount(ctx, actor.TenantID, accountID)
	if err != nil {
		return BotKeyIssue{}, err
	}
	if account.DisabledAt != nil {
		return BotKeyIssue{}, ErrForbidden
	}
	bot, err := s.Store.GetBot(ctx, actor.TenantID, botID)
	if err != nil {
		return BotKeyIssue{}, err
	}
	if bot.Archived {
		return BotKeyIssue{}, ErrForbidden
	}
	profile, err := s.Store.GetProfile(ctx, actor.TenantID, bot.ProfileID)
	if err != nil {
		return BotKeyIssue{}, err
	}
	if profile.AccountID != accountID {
		return BotKeyIssue{}, ErrForbidden
	}
	minted, err := MintBotKey()
	if err != nil {
		return BotKeyIssue{}, err
	}
	key, err := s.Store.CreateBotAPIKey(ctx, actor.TenantID, uuid.New(), minted.Hash,
		minted.Prefix, accountID, bot.ID, label)
	if err != nil {
		return BotKeyIssue{}, err
	}
	s.audit(ctx, store.AuthEvent{
		TenantID: actor.TenantID, AccountID: &accountID, ActorAccountID: &actor.AccountID,
		Kind: store.AuthEventBotKeyIssued,
		Detail: map[string]any{
			"key_id": key.KeyID.String(), "key_prefix": key.Prefix,
			"bot_id": bot.ID, "label": key.Label,
		},
	})
	return BotKeyIssue{Plaintext: minted.Plaintext, Key: key}, nil
}

// AuthenticateBotKey resolves a presented credential to a bot principal. The
// key itself names the tenant: a request never chooses its tenant by header.
// Revoked keys, disabled accounts and archived bots are all one 401; a key for
// another bot is a 403 at the middleware, not here.
func (s *Service) AuthenticateBotKey(ctx context.Context, plaintext string) (Principal, error) {
	if s == nil || s.Store == nil || !LooksLikeBotKey(plaintext) {
		return Principal{}, ErrInvalidBotKey
	}
	key, err := s.Store.LookupBotAPIKey(ctx, HashBotKey(plaintext))
	if err != nil {
		return Principal{}, ErrInvalidBotKey
	}
	if key.TenantID != s.TenantID {
		return Principal{}, ErrInvalidBotKey
	}
	account, err := s.Store.GetAccount(ctx, key.TenantID, key.AccountID)
	if err != nil || account.DisabledAt != nil {
		return Principal{}, ErrInvalidBotKey
	}
	bot, err := s.Store.GetBot(ctx, key.TenantID, key.BotID)
	if err != nil || bot.Archived {
		return Principal{}, ErrInvalidBotKey
	}
	profile, err := s.Store.GetProfile(ctx, key.TenantID, bot.ProfileID)
	if err != nil || profile.AccountID != key.AccountID {
		return Principal{}, ErrInvalidBotKey
	}
	_ = s.Store.TouchBotAPIKey(ctx, key.TenantID, key.KeyID, s.now())
	return Principal{
		Kind: PrincipalBot, TenantID: key.TenantID, AccountID: key.AccountID,
		BotID: key.BotID, KeyID: key.KeyID, KeyPrefix: key.Prefix,
		Username: account.Username, DisplayName: account.DisplayName,
		// A bot never inherits deployment super-admin authority (spec: 超级管理员
		// 权限 MUST NOT 委托给 Bot), even if its account row was marked.
		IsSuperAdmin: false,
	}, nil
}

// RevokeBotKey revokes one credential on behalf of a super admin.
func (s *Service) RevokeBotKey(ctx context.Context, actor Principal, keyID uuid.UUID) error {
	if err := s.requireSuperAdmin(ctx, actor); err != nil {
		return err
	}
	if err := s.Store.RevokeBotAPIKey(ctx, actor.TenantID, keyID); err != nil {
		return err
	}
	s.audit(ctx, store.AuthEvent{
		TenantID: actor.TenantID, ActorAccountID: &actor.AccountID, Kind: store.AuthEventBotKeyRevoked,
		Detail: map[string]any{"key_id": keyID.String()},
	})
	return nil
}

// ListBotKeys returns credential metadata for a super admin. The plaintext is
// not part of the projection and cannot be recovered.
func (s *Service) ListBotKeys(ctx context.Context, actor Principal, botID string) ([]store.BotAPIKey, error) {
	if err := s.requireSuperAdmin(ctx, actor); err != nil {
		return nil, err
	}
	return s.Store.ListBotAPIKeys(ctx, actor.TenantID, botID, 0)
}

// requireSuperAdmin re-reads the actor's account: a session that was issued
// while the account was a super admin must stop acting as one after a change.
func (s *Service) requireSuperAdmin(ctx context.Context, actor Principal) error {
	if actor.IsBot() || actor.AccountID == uuid.Nil {
		return ErrForbidden
	}
	account, err := s.Store.GetAccount(ctx, actor.TenantID, actor.AccountID)
	if err != nil {
		return ErrForbidden
	}
	if !account.IsSuperAdmin || account.DisabledAt != nil {
		return ErrForbidden
	}
	return nil
}

func (s *Service) createSession(ctx context.Context, account store.AccountRecord, userAgent, ip string) (store.AuthSession, error) {
	now := s.now()
	ttl := s.SessionTTL
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return s.Store.CreateAuthSession(ctx, store.AuthSession{
		TenantID: account.TenantID, SessionID: uuid.New(), AccountID: account.ID,
		IssuedAt: now, ExpiresAt: now.Add(ttl),
		UserAgentHash: hashBytes(userAgent), IPHash: hashBytes(ip),
	})
}

func (s *Service) limiterFailure(ctx context.Context, username, ip, reason string) {
	s.Limiter.Failure(username, ip)
	s.audit(ctx, store.AuthEvent{
		TenantID: s.TenantID, Kind: store.AuthEventLoginFailed, Result: store.AuthResultFailed,
		Detail: map[string]any{
			"username": truncate(username, 128), "reason": reason,
			"ip_hash": hashHex(ip),
		},
	})
}

func (s *Service) verifyDummy(password string) {
	s.dummyOnce.Do(func() {
		s.dummyHash, s.dummyErr = HashPassword("keel-dummy-password-not-an-account")
	})
	if s.dummyErr == nil {
		_ = VerifyPassword(s.dummyHash, password)
	}
}

func (s *Service) audit(ctx context.Context, event store.AuthEvent) {
	if s == nil || s.Store == nil {
		return
	}
	_, _ = s.Store.RecordAuthEvent(ctx, event)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func hashBytes(value string) []byte {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

func hashHex(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return fmt.Sprintf("%x", hashBytes(value))
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}
