package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// cleanupTenant removes the accounts of a test tenant so the deployment-wide
// "no super admin" predicate is repeatable across runs on one test database.
// Audit rows deliberately stay: auth_events is append-only.
func cleanupTenant(t *testing.T, st *Store, tenantID uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := st.Pool().Exec(context.Background(),
			`DELETE FROM accounts WHERE tenant_id = $1`, tenantID); err != nil {
			t.Logf("cleanup accounts for %s: %v", tenantID, err)
		}
	})
}

// authFixture creates one tenant and one human account.
func authFixture(t *testing.T, st *Store, super bool) (uuid.UUID, AccountRecord) {
	t.Helper()
	ctx := context.Background()
	tenantID := uuid.New()
	if err := st.CreateTenant(ctx, tenantID, "auth-"+tenantID.String()[:8]); err != nil {
		t.Fatal(err)
	}
	cleanupTenant(t, st, tenantID)
	account, err := st.CreateAccount(ctx, AccountRecord{
		TenantID: tenantID, ID: uuid.New(), Username: "user-" + tenantID.String()[:8],
		DisplayName: "User", Kind: "human", PasswordHash: "argon2id-test",
		IsSuperAdmin: super,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tenantID, account
}

func digest(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

func TestAuthSessionStoreRoundTrip(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenantID, account := authFixture(t, st, false)
	otherTenant, otherAccount := authFixture(t, st, false)

	now := time.Now().UTC().Truncate(time.Microsecond)
	record := AuthSession{
		TenantID: tenantID, SessionID: uuid.New(), AccountID: account.ID,
		IssuedAt: now, ExpiresAt: now.Add(time.Hour),
		UserAgentHash: digest("agent"), IPHash: digest("127.0.0.1"),
	}
	created, err := st.CreateAuthSession(ctx, record)
	if err != nil {
		t.Fatal(err)
	}
	if created.SessionID != record.SessionID || !created.Active(now) || created.RevokedAt != nil {
		t.Fatalf("created = %+v", created)
	}
	if len(created.UserAgentHash) != 32 || len(created.IPHash) != 32 {
		t.Fatalf("hashes not persisted: %+v", created)
	}

	byID, err := st.GetAuthSessionByID(ctx, created.SessionID)
	if err != nil || byID.AccountID != account.ID || byID.TenantID != tenantID {
		t.Fatalf("get by id = %+v err=%v", byID, err)
	}
	scoped, err := st.GetAuthSession(ctx, tenantID, created.SessionID)
	if err != nil || scoped.SessionID != created.SessionID {
		t.Fatalf("get scoped = %+v err=%v", scoped, err)
	}
	// Another tenant cannot read the row (the unique index is not a tenant scope).
	if _, err := st.GetAuthSession(ctx, otherTenant, created.SessionID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant read err = %v, want ErrNotFound", err)
	}

	// A second session for the same account, plus one for another account.
	second, err := st.CreateAuthSession(ctx, AuthSession{
		TenantID: tenantID, SessionID: uuid.New(), AccountID: account.ID,
		IssuedAt: now.Add(time.Second), ExpiresAt: now.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAuthSession(ctx, AuthSession{
		TenantID: otherTenant, SessionID: uuid.New(), AccountID: otherAccount.ID,
		IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	sessions, err := st.ListAuthSessionsForAccount(ctx, tenantID, account.ID, 0)
	if err != nil || len(sessions) != 2 {
		t.Fatalf("list = %d %v", len(sessions), err)
	}
	if sessions[0].SessionID != second.SessionID {
		t.Fatalf("list is not newest first: %+v", sessions)
	}
	limited, err := st.ListAuthSessionsForAccount(ctx, tenantID, account.ID, 1)
	if err != nil || len(limited) != 1 {
		t.Fatalf("bounded list = %d %v", len(limited), err)
	}

	// Revoke is immediate and idempotent; the row keeps the first mark.
	if err := st.RevokeAuthSession(ctx, tenantID, created.SessionID); err != nil {
		t.Fatal(err)
	}
	revoked, err := st.GetAuthSessionByID(ctx, created.SessionID)
	if err != nil || revoked.RevokedAt == nil || revoked.Active(now.Add(time.Minute)) {
		t.Fatalf("revoked = %+v err=%v", revoked, err)
	}
	firstMark := *revoked.RevokedAt
	time.Sleep(5 * time.Millisecond)
	if err := st.RevokeAuthSession(ctx, tenantID, created.SessionID); err != nil {
		t.Fatal(err)
	}
	again, _ := st.GetAuthSessionByID(ctx, created.SessionID)
	if !again.RevokedAt.Equal(firstMark) {
		t.Fatalf("revocation time moved: %v -> %v", firstMark, again.RevokedAt)
	}
	if err := st.RevokeAuthSession(ctx, tenantID, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke missing err = %v, want ErrNotFound", err)
	}

	// Revoking all sessions reports the live count and leaves revoked rows alone.
	extra, err := st.CreateAuthSession(ctx, AuthSession{
		TenantID: tenantID, SessionID: uuid.New(), AccountID: account.ID,
		IssuedAt: now.Add(3 * time.Second), ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = extra
	revokedCount, err := st.RevokeAuthSessionsForAccount(ctx, tenantID, account.ID)
	if err != nil || revokedCount != 2 { // second + extra; created was already revoked
		t.Fatalf("revoke all = %d err=%v", revokedCount, err)
	}
	if count, err := st.RevokeAuthSessionsForAccount(ctx, tenantID, account.ID); err != nil || count != 0 {
		t.Fatalf("second revoke all = %d err=%v", count, err)
	}
}

func TestAuthSessionRejectsInvalidInput(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenantID, account := authFixture(t, st, false)
	now := time.Now().UTC()
	cases := []struct {
		name   string
		record AuthSession
	}{
		{"missing tenant", AuthSession{SessionID: uuid.New(), AccountID: account.ID, IssuedAt: now, ExpiresAt: now.Add(time.Hour)}},
		{"missing session", AuthSession{TenantID: tenantID, AccountID: account.ID, IssuedAt: now, ExpiresAt: now.Add(time.Hour)}},
		{"missing account", AuthSession{TenantID: tenantID, SessionID: uuid.New(), IssuedAt: now, ExpiresAt: now.Add(time.Hour)}},
		{"expiry before issue", AuthSession{TenantID: tenantID, SessionID: uuid.New(), AccountID: account.ID, IssuedAt: now, ExpiresAt: now.Add(-time.Second)}},
		{"short digest", AuthSession{TenantID: tenantID, SessionID: uuid.New(), AccountID: account.ID, IssuedAt: now, ExpiresAt: now.Add(time.Hour), IPHash: []byte("short")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.CreateAuthSession(ctx, tc.record); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

// TestBootstrapTokenExpiry runs before the successful bootstrap: the bootstrap
// predicate is deployment-wide, so this order keeps the "no super admin" state.
func TestBootstrapTokenExpiry(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	if err := st.CreateTenant(ctx, tenantID, "bootstrap-expiry"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	hash := digest("expiring-token")
	if _, err := st.CreateBootstrapToken(ctx, tenantID, hash, now.Add(time.Minute), "cli"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BootstrapSuperAdmin(ctx, BootstrapParams{
		TenantID: tenantID, TokenHash: hash, Username: "root",
		PasswordHash: "hash", Now: now.Add(2 * time.Minute),
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expired token err = %v, want ErrInvalid", err)
	}
	// The failed attempt did not consume the token.
	if has, err := st.HasSuperAdmin(ctx); err != nil || has {
		t.Fatalf("expired attempt created a super admin: %v err=%v", has, err)
	}
	if state, err := st.BootstrapTokenState(ctx, tenantID, hash, now.Add(2*time.Minute)); err != nil || state != BootstrapTokenExpired {
		t.Fatalf("expired token state = %q err=%v", state, err)
	}
}

func TestBootstrapTokenOnceAndAtomicSuperAdmin(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	if err := st.CreateTenant(ctx, tenantID, "bootstrap"); err != nil {
		t.Fatal(err)
	}
	cleanupTenant(t, st, tenantID)
	now := time.Now().UTC()
	hash := digest("setup-token")
	if _, err := st.CreateBootstrapToken(ctx, tenantID, hash, now.Add(10*time.Minute), "cli"); err != nil {
		t.Fatal(err)
	}
	// The plaintext never reaches the database; only its hash is in the detail.
	events, err := st.ListAuthEvents(ctx, tenantID, AuthEventFilter{Kind: AuthEventBootstrapToken}, 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("token events = %d err=%v", len(events), err)
	}
	encoded, _ := events[0].Detail["token_hash"].(string)
	if encoded != hex.EncodeToString(hash) || strings.Contains(encoded, "setup-token") {
		t.Fatalf("stored token detail = %+v", events[0].Detail)
	}
	if live, err := st.LiveBootstrapToken(ctx, tenantID, now); err != nil || !live {
		t.Fatalf("live token = %v err=%v", live, err)
	}
	if state, err := st.BootstrapTokenState(ctx, tenantID, hash, now); err != nil || state != BootstrapTokenValid {
		t.Fatalf("valid token state = %q err=%v", state, err)
	}
	if state, err := st.BootstrapTokenState(ctx, tenantID, digest("unknown-token"), now); err != nil || state != BootstrapTokenUnknown {
		t.Fatalf("unknown token state = %q err=%v", state, err)
	}

	passwordHash := "$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHRzYWx0c2FsdA$a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5MzI"
	if _, err := st.BootstrapSuperAdmin(ctx, BootstrapParams{
		TenantID: tenantID, TokenHash: digest("wrong-token"), Username: "root",
		PasswordHash: passwordHash, Now: now,
	}); err == nil {
		t.Fatal("wrong token was accepted")
	}
	account, err := st.BootstrapSuperAdmin(ctx, BootstrapParams{
		TenantID: tenantID, TokenHash: hash, Username: "root", DisplayName: "Root",
		PasswordHash: passwordHash, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !account.IsSuperAdmin || account.Kind != "human" || account.PasswordHash != passwordHash {
		t.Fatalf("bootstrapped account = %+v", account)
	}
	if must, err := st.AccountMustChangePassword(ctx, tenantID, account.ID); err != nil || !must {
		t.Fatalf("bootstrapped must-change = %v err=%v", must, err)
	}
	if has, err := st.HasSuperAdmin(ctx); err != nil || !has {
		t.Fatalf("HasSuperAdmin = %v err=%v", has, err)
	}
	// The token is one-time: replay fails, and now that a super admin exists the
	// second bootstrap cannot create another one.
	if _, err := st.BootstrapSuperAdmin(ctx, BootstrapParams{
		TenantID: tenantID, TokenHash: hash, Username: "root2", PasswordHash: passwordHash, Now: now,
	}); err == nil {
		t.Fatal("replayed setup token was accepted")
	}
	if live, err := st.LiveBootstrapToken(ctx, tenantID, now); err != nil || live {
		t.Fatalf("used token still live = %v err=%v", live, err)
	}
	if state, err := st.BootstrapTokenState(ctx, tenantID, hash, now); err != nil || state != BootstrapTokenUsed {
		t.Fatalf("used token state = %q err=%v", state, err)
	}
	used, err := st.ListAuthEvents(ctx, tenantID, AuthEventFilter{Kind: AuthEventBootstrapUsed}, 0)
	if err != nil || len(used) != 1 {
		t.Fatalf("bootstrap.used rows = %d err=%v", len(used), err)
	}
}

func TestAuthEventsAuditAndDerivedFacts(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenantID, account := authFixture(t, st, false)

	if must, err := st.AccountMustChangePassword(ctx, tenantID, account.ID); err != nil || must {
		t.Fatalf("plain account must-change = %v err=%v", must, err)
	}

	id, err := st.RecordAuthEvent(ctx, AuthEvent{
		TenantID: tenantID, AccountID: &account.ID, Kind: AuthEventLoginSucceeded,
		Detail: map[string]any{"ip_hash": hex.EncodeToString(digest("ip"))},
	})
	if err != nil || id == 0 {
		t.Fatalf("record = %d err=%v", id, err)
	}
	if _, err := st.RecordAuthEvent(ctx, AuthEvent{
		TenantID: tenantID, Kind: AuthEventLoginFailed, Result: AuthResultFailed,
		Detail: map[string]any{"username": "missing"},
	}); err != nil {
		t.Fatal(err)
	}
	events, err := st.ListAuthEvents(ctx, tenantID, AuthEventFilter{}, 0)
	if err != nil || len(events) != 2 {
		t.Fatalf("list = %d err=%v", len(events), err)
	}
	if events[0].Kind != AuthEventLoginFailed || events[0].Result != AuthResultFailed {
		t.Fatalf("newest event = %+v", events[0])
	}
	if events[0].AccountID != nil {
		t.Fatalf("failed login must not name an account: %+v", events[0])
	}
	byAccount, err := st.ListAuthEvents(ctx, tenantID, AuthEventFilter{AccountID: account.ID}, 0)
	if err != nil || len(byAccount) != 1 || byAccount[0].Kind != AuthEventLoginSucceeded {
		t.Fatalf("by account = %+v err=%v", byAccount, err)
	}
	if byAccount[0].Detail["ip_hash"] == nil {
		t.Fatalf("detail lost: %+v", byAccount[0].Detail)
	}

	// A bootstrap event makes the account owe a password change until a later
	// password.changed event lands.
	if _, err := st.RecordAuthEvent(ctx, AuthEvent{
		TenantID: tenantID, AccountID: &account.ID, Kind: AuthEventAccountBoot,
		Detail: map[string]any{"must_change_password": true},
	}); err != nil {
		t.Fatal(err)
	}
	if must, err := st.AccountMustChangePassword(ctx, tenantID, account.ID); err != nil || !must {
		t.Fatalf("after bootstrap must-change = %v err=%v", must, err)
	}
	// A failed password-change attempt must not lift the gate: only an
	// ok password.changed event counts as the rotation.
	if _, err := st.RecordAuthEvent(ctx, AuthEvent{
		TenantID: tenantID, AccountID: &account.ID, Kind: AuthEventPasswordChange,
		Result: AuthResultFailed, Detail: map[string]any{"reason": "current_password_mismatch"},
	}); err != nil {
		t.Fatal(err)
	}
	if must, err := st.AccountMustChangePassword(ctx, tenantID, account.ID); err != nil || !must {
		t.Fatalf("after failed change must-change = %v err=%v", must, err)
	}
	if _, err := st.RecordAuthEvent(ctx, AuthEvent{
		TenantID: tenantID, AccountID: &account.ID, Kind: AuthEventPasswordChange,
	}); err != nil {
		t.Fatal(err)
	}
	if must, err := st.AccountMustChangePassword(ctx, tenantID, account.ID); err != nil || must {
		t.Fatalf("after change must-change = %v err=%v", must, err)
	}
}

func TestResetChangeDisableLifecycle(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenantID, account := authFixture(t, st, false)
	now := time.Now().UTC()

	session, err := st.CreateAuthSession(ctx, AuthSession{
		TenantID: tenantID, SessionID: uuid.New(), AccountID: account.ID,
		IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	// CLI reset: new hash, all sessions dead, must-change set.
	reset, err := st.ResetAccountPassword(ctx, tenantID, account.Username, "hash-reset", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if reset.PasswordHash != "hash-reset" {
		t.Fatalf("reset account = %+v", reset)
	}
	stale, err := st.GetAuthSessionByID(ctx, session.SessionID)
	if err != nil || stale.RevokedAt == nil {
		t.Fatalf("reset did not revoke the session: %+v err=%v", stale, err)
	}
	if must, err := st.AccountMustChangePassword(ctx, tenantID, account.ID); err != nil || !must {
		t.Fatalf("after reset must-change = %v err=%v", must, err)
	}

	// Login-time change: hash replaced, sessions revoked, must-change cleared.
	changed, err := st.ChangeAccountPassword(ctx, tenantID, account.ID, "hash-changed", account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if changed.PasswordHash != "hash-changed" {
		t.Fatalf("changed account = %+v", changed)
	}
	if must, err := st.AccountMustChangePassword(ctx, tenantID, account.ID); err != nil || must {
		t.Fatalf("after change must-change = %v err=%v", must, err)
	}

	// Disable: session (a new one) is revoked immediately, audit recorded.
	live, err := st.CreateAuthSession(ctx, AuthSession{
		TenantID: tenantID, SessionID: uuid.New(), AccountID: account.ID,
		IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := st.DisableAccountAudited(ctx, tenantID, account.ID, uuid.Nil, true, "test")
	if err != nil {
		t.Fatal(err)
	}
	if disabled.DisabledAt == nil {
		t.Fatalf("account not disabled: %+v", disabled)
	}
	stale, err = st.GetAuthSessionByID(ctx, live.SessionID)
	if err != nil || stale.RevokedAt == nil {
		t.Fatalf("disable did not revoke the session: %+v err=%v", stale, err)
	}
	events, err := st.ListAuthEvents(ctx, tenantID, AuthEventFilter{AccountID: account.ID, Kind: AuthEventAccountDisable}, 0)
	if err != nil || len(events) != 1 || events[0].Detail["reason"] != "test" {
		t.Fatalf("disable audit = %+v err=%v", events, err)
	}
	enabled, err := st.DisableAccountAudited(ctx, tenantID, account.ID, uuid.Nil, false, "test")
	if err != nil || enabled.DisabledAt != nil {
		t.Fatalf("re-enable = %+v err=%v", enabled, err)
	}
}

func TestSuperAdminCannotBeDisabled(t *testing.T) {
	st := readyTestStore(t)
	ctx := context.Background()
	tenantID, root := authFixture(t, st, true)
	if _, err := st.DisableAccountAudited(ctx, tenantID, root.ID, uuid.Nil, true, "test"); !errors.Is(err, ErrSuperAdminProtected) {
		t.Fatalf("disable super admin err = %v, want ErrSuperAdminProtected", err)
	}
	// The low-level write carries the same invariant: no caller can bypass it.
	if err := st.SetAccountDisabled(ctx, tenantID, root.ID, true); !errors.Is(err, ErrSuperAdminProtected) {
		t.Fatalf("raw disable super admin err = %v, want ErrSuperAdminProtected", err)
	}
	after, err := st.GetAccount(ctx, tenantID, root.ID)
	if err != nil || after.DisabledAt != nil {
		t.Fatalf("super admin changed: %+v err=%v", after, err)
	}
	if has, err := st.HasSuperAdmin(ctx); err != nil || !has {
		t.Fatalf("HasSuperAdmin = %v err=%v", has, err)
	}
}
