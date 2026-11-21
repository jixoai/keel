package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/store"
)

// These tests exercise the real auth controller over HTTP against a real
// PostgreSQL database. They are skipped unless KEEL_TEST_DSN names a dedicated
// database; the test tenant's accounts are deleted on cleanup so the
// deployment-wide "no super admin" predicate is repeatable.
type authTestEnv struct {
	t        *testing.T
	st       *store.Store
	ctl      *authController
	server   *httptest.Server
	client   *http.Client
	tenantID uuid.UUID
	secret   []byte
	mode     authMode
}

func testSecretKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(0xA0 + i)
	}
	return key
}

func newAuthTestEnv(t *testing.T, mode authMode, tenantID uuid.UUID) *authTestEnv {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if tenantID == uuid.Nil {
		tenantID = uuid.New()
	}
	if err := st.CreateTenant(ctx, tenantID, "auth-e2e-"+tenantID.String()[:8]); err != nil {
		t.Fatal(err)
	}
	cleanupAuthTenant(t, st, tenantID)
	env := &authTestEnv{t: t, st: st, tenantID: tenantID, secret: testSecretKey(), mode: mode}
	var localAdmin auth.Principal
	if mode.usesLocalAdmin() {
		localAdmin, err = ensureLocalIdentity(ctx, st, tenantID, discardLogger())
		if err != nil {
			t.Fatalf("local identity: %v", err)
		}
	}
	ctl, err := newAuthController(authConfig{
		Mode: mode, Store: st, TenantID: tenantID, SecretKey: env.secret,
		SessionTTL: time.Hour, LocalAdmin: localAdmin,
	}, discardLogger())
	if err != nil {
		t.Fatalf("auth controller: %v", err)
	}
	env.ctl = ctl
	mux := http.NewServeMux()
	ctl.register(mux)
	mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeAuthJSON(w, http.StatusOK, map[string]any{
			"tenantId":  r.Header.Get("X-Tenant-ID"),
			"accountId": r.Header.Get("X-Account-ID"),
			"botId":     r.Header.Get("X-Bot-ID"),
		})
	})
	mux.HandleFunc("GET /v1/auth/whoami", func(w http.ResponseWriter, r *http.Request) {
		writeAuthJSON(w, http.StatusOK, map[string]any{
			"tenantId": r.Header.Get("X-Tenant-ID"), "botId": r.Header.Get("X-Bot-ID"),
			"accountId": r.Header.Get("X-Account-ID"),
		})
	})
	mux.HandleFunc("GET /v1/bots/{id}/echo", func(w http.ResponseWriter, r *http.Request) {
		writeAuthJSON(w, http.StatusOK, map[string]any{
			"tenantId":  r.Header.Get("X-Tenant-ID"),
			"accountId": r.Header.Get("X-Account-ID"),
			"botId":     r.Header.Get("X-Bot-ID"),
			"pathId":    r.PathValue("id"),
		})
	})
	server := httptest.NewUnstartedServer(ctl.wrap(mux))
	server.StartTLS()
	t.Cleanup(server.Close)
	env.server = server
	env.client = env.newClient()
	return env
}

// newClient returns an independent cookie jar over the test server's trusted
// TLS transport. Secure cookies work because the server is HTTPS.
func (e *authTestEnv) newClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Transport: e.server.Client().Transport, Jar: jar}
}

func (e *authTestEnv) do(client *http.Client, method, path string, body any, headers map[string]string) (int, map[string]any, http.Header) {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.server.URL+path, reader)
	if err != nil {
		e.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	res, err := client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var decoded map[string]any
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			e.t.Fatalf("%s %s returned non-JSON %q", method, path, raw)
		}
	}
	return res.StatusCode, decoded, res.Header
}

func (e *authTestEnv) csrf(client *http.Client) string {
	e.t.Helper()
	parsed, _ := url.Parse(e.server.URL)
	for _, cookie := range client.Jar.Cookies(parsed) {
		if cookie.Name == auth.CSRFCookieName {
			return cookie.Value
		}
	}
	return ""
}

func (e *authTestEnv) csrfHeader(client *http.Client) map[string]string {
	e.t.Helper()
	token := e.csrf(client)
	if token == "" {
		e.t.Fatal("no csrf cookie in the jar")
	}
	return map[string]string{auth.CSRFHeaderName: token}
}

func (e *authTestEnv) getSession(client *http.Client) (int, map[string]any) {
	e.t.Helper()
	status, body, _ := e.do(client, http.MethodGet, "/v1/auth/session", nil, nil)
	return status, body
}

func (e *authTestEnv) login(client *http.Client, username, password string) (int, map[string]any) {
	e.t.Helper()
	status, body, _ := e.loginWithHeaders(client, username, password)
	return status, body
}

func (e *authTestEnv) loginWithHeaders(client *http.Client, username, password string) (int, map[string]any, http.Header) {
	e.t.Helper()
	if status, body := e.getSession(client); status != http.StatusOK {
		e.t.Fatalf("session probe: %d %v", status, body)
	}
	return e.do(client, http.MethodPost, "/v1/auth/login",
		map[string]any{"username": username, "password": password}, e.csrfHeader(client))
}

func (e *authTestEnv) changePassword(client *http.Client, current, next string) (int, map[string]any) {
	e.t.Helper()
	status, body, _ := e.do(client, http.MethodPost, "/v1/auth/password",
		map[string]any{"currentPassword": current, "newPassword": next}, e.csrfHeader(client))
	return status, body
}

func (e *authTestEnv) accountID(body map[string]any) uuid.UUID {
	e.t.Helper()
	account, _ := body["account"].(map[string]any)
	id, _ := account["id"].(string)
	parsed, err := uuid.Parse(id)
	if err != nil {
		e.t.Fatalf("response has no account id: %v", body)
	}
	return parsed
}

// bootstrapAdmin runs the whole first-admin journey: token, setup, login and the
// forced password change. It returns the admin account id and the client's
// session-bound CSRF header helper.
func (e *authTestEnv) bootstrapAdmin(initialPassword, finalPassword string) (uuid.UUID, map[string]string) {
	e.t.Helper()
	token := auth.SetupTokenScheme + uuid.NewString()
	if _, err := e.st.CreateBootstrapToken(context.Background(), e.tenantID,
		auth.HashToken(token), time.Now().UTC().Add(10*time.Minute), "e2e"); err != nil {
		e.t.Fatal(err)
	}
	if status, body := e.getSession(e.client); status != http.StatusOK {
		e.t.Fatalf("anonymous session: %d %v", status, body)
	}
	status, body, _ := e.do(e.client, http.MethodPost, "/v1/auth/bootstrap",
		map[string]any{"token": token, "username": "root", "password": initialPassword, "displayName": "Root"},
		e.csrfHeader(e.client))
	if status != http.StatusOK {
		e.t.Fatalf("bootstrap: %d %v", status, body)
	}
	// Replay must fail before the login stage.
	if status, _, _ := e.do(e.client, http.MethodPost, "/v1/auth/bootstrap",
		map[string]any{"token": token, "username": "other", "password": initialPassword},
		e.csrfHeader(e.client)); status != http.StatusUnauthorized {
		e.t.Fatalf("replayed setup token: %d", status)
	}
	status, loginBody, loginHeaders := e.loginWithHeaders(e.client, "root", initialPassword)
	if status != http.StatusOK || loginBody["mustChangePassword"] != true {
		e.t.Fatalf("first login: %d %v", status, loginBody)
	}
	assertSessionCookieAttributes(e.t, loginHeaders)
	adminID := e.accountID(loginBody)
	status, changed := e.changePassword(e.client, initialPassword, finalPassword)
	if status != http.StatusOK || changed["mustChangePassword"] != false {
		e.t.Fatalf("forced password change: %d %v", status, changed)
	}
	return adminID, e.csrfHeader(e.client)
}

func cleanupAuthTenant(t *testing.T, st *store.Store, tenantID uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		// Delete in FK order: bots -> profiles -> roles -> accounts. Audit rows
		// deliberately stay (auth_events is append-only).
		for _, statement := range []string{
			`DELETE FROM bot_api_keys WHERE tenant_id = $1`,
			`DELETE FROM bots WHERE tenant_id = $1`,
			`DELETE FROM profiles WHERE tenant_id = $1`,
			`DELETE FROM roles WHERE tenant_id = $1`,
			`DELETE FROM permission_group_members WHERE tenant_id = $1`,
			`DELETE FROM accounts WHERE tenant_id = $1`,
		} {
			if _, err := st.Pool().Exec(ctx, statement, tenantID); err != nil {
				t.Logf("cleanup %q: %v", statement, err)
			}
		}
	})
}

func TestAuthRequiredLifecycleE2E(t *testing.T) {
	env := newAuthTestEnv(t, authModeRequired, uuid.Nil)

	// No super admin: /v1 refuses anonymous entry and points at bootstrap.
	status, body, _ := env.do(env.client, http.MethodGet, "/v1/sessions", nil, nil)
	if status != http.StatusServiceUnavailable || body["code"] != "bootstrap_required" {
		t.Fatalf("pre-bootstrap /v1: %d %v", status, body)
	}
	status, sessionBody := env.getSession(env.client)
	if status != http.StatusOK || sessionBody["authenticated"] != false || sessionBody["bootstrapRequired"] != true {
		t.Fatalf("anonymous session: %d %v", status, sessionBody)
	}

	// CSRF negative control on the setup write.
	token := auth.SetupTokenScheme + uuid.NewString()
	if _, err := env.st.CreateBootstrapToken(context.Background(), env.tenantID,
		auth.HashToken(token), time.Now().UTC().Add(10*time.Minute), "e2e"); err != nil {
		t.Fatal(err)
	}
	if status, body, _ := env.do(env.client, http.MethodPost, "/v1/auth/bootstrap",
		map[string]any{"token": token, "username": "root", "password": "admin-password-123"}, nil); status != http.StatusForbidden {
		t.Fatalf("bootstrap without csrf: %d %v", status, body)
	}
	if status, body, _ := env.do(env.client, http.MethodPost, "/v1/auth/bootstrap",
		map[string]any{"token": token, "username": "root", "password": "admin-password-123"},
		map[string]string{auth.CSRFHeaderName: "wrong." + strings.Repeat("A", 43)}); status != http.StatusForbidden {
		t.Fatalf("bootstrap with forged csrf: %d %v", status, body)
	}
	status, body, _ = env.do(env.client, http.MethodPost, "/v1/auth/bootstrap",
		map[string]any{"token": token, "username": "root", "password": "admin-password-123", "displayName": "Root"},
		env.csrfHeader(env.client))
	if status != http.StatusOK || body["mustChangePassword"] != true {
		t.Fatalf("bootstrap: %d %v", status, body)
	}
	// The token is one-time.
	if status, body, _ := env.do(env.client, http.MethodPost, "/v1/auth/bootstrap",
		map[string]any{"token": token, "username": "other", "password": "admin-password-123"},
		env.csrfHeader(env.client)); status != http.StatusUnauthorized {
		t.Fatalf("replayed token: %d %v", status, body)
	}

	// Unknown user and wrong password have one response shape.
	statusUnknown, bodyUnknown := env.login(env.client, "nobody", "admin-password-123")
	statusWrong, bodyWrong := env.login(env.client, "root", "wrong-password-123")
	if statusUnknown != http.StatusUnauthorized || statusWrong != http.StatusUnauthorized {
		t.Fatalf("uniform failure status: %d/%d", statusUnknown, statusWrong)
	}
	if bodyUnknown["error"] != bodyWrong["error"] || bodyUnknown["code"] != bodyWrong["code"] {
		t.Fatalf("failure shape leaks account existence: %v vs %v", bodyUnknown, bodyWrong)
	}

	status, loginBody := env.login(env.client, "root", "admin-password-123")
	if status != http.StatusOK || loginBody["authenticated"] != true || loginBody["mustChangePassword"] != true {
		t.Fatalf("first login: %d %v", status, loginBody)
	}
	adminID := env.accountID(loginBody)
	oldCookie := sessionCookieValue(t, env.client, env.server.URL)
	if oldCookie == "" {
		t.Fatal("login did not set a session cookie")
	}

	// The must-change principal may only reach the password surface.
	if status, body, _ := env.do(env.client, http.MethodGet, "/v1/sessions", nil, nil); status != http.StatusForbidden || body["code"] != "password_change_required" {
		t.Fatalf("must-change restriction: %d %v", status, body)
	}
	// A failed change attempt must not lift the gate: only a successful
	// rotation does. Regression for the failed password.changed row.
	if status, body := env.changePassword(env.client, "wrong-current-123", "admin-password-456"); status != http.StatusUnauthorized {
		t.Fatalf("failed password change: %d %v", status, body)
	}
	if status, sessionBody := env.getSession(env.client); status != http.StatusOK || sessionBody["mustChangePassword"] != true {
		t.Fatalf("failed change lifted must-change: %d %v", status, sessionBody)
	}
	// Repeated failed current-password attempts hit the change-password limiter
	// (separate key from login) instead of burning Argon2 forever.
	for i := 0; i < 2; i++ {
		if status, body := env.changePassword(env.client, "wrong-current-123", "admin-password-456"); status != http.StatusUnauthorized {
			t.Fatalf("failed password change %d: %d %v", i, status, body)
		}
	}
	if status, body := env.changePassword(env.client, "wrong-current-123", "admin-password-456"); status != http.StatusTooManyRequests {
		t.Fatalf("rate-limited password change: %d %v", status, body)
	}
	time.Sleep(1100 * time.Millisecond)
	if status, body := env.changePassword(env.client, "admin-password-123", "admin-password-456"); status != http.StatusOK || body["mustChangePassword"] != false {
		t.Fatalf("password change: %d %v", status, body)
	}
	// The pre-change cookie is dead.
	req, _ := http.NewRequest(http.MethodGet, env.server.URL+"/v1/sessions", nil)
	req.Header.Set("Cookie", auth.SessionCookieName+"="+oldCookie)
	res, err := env.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old cookie after password change: %d", res.StatusCode)
	}
	// The new cookie works and the kernel header is the server tenant.
	if status, body, _ := env.do(env.client, http.MethodGet, "/v1/sessions", nil, nil); status != http.StatusOK ||
		body["tenantId"] != env.tenantID.String() || body["accountId"] != adminID.String() {
		t.Fatalf("post-change session: %d %v", status, body)
	}

	// Logout needs CSRF; without it the session survives.
	if status, body, _ := env.do(env.client, http.MethodPost, "/v1/auth/logout", nil, nil); status != http.StatusForbidden {
		t.Fatalf("logout without csrf: %d %v", status, body)
	}
	if status, _, _ := env.do(env.client, http.MethodGet, "/v1/sessions", nil, nil); status != http.StatusOK {
		t.Fatalf("session did not survive the refused logout: %d", status)
	}
	// A second controller over the same store accepts the persisted session.
	ctl2, err := newAuthController(authConfig{
		Mode: authModeRequired, Store: env.st, TenantID: env.tenantID,
		SecretKey: env.secret, SessionTTL: time.Hour,
	}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	server2 := httptest.NewServer(ctl2.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeAuthJSON(w, http.StatusOK, map[string]any{"tenantId": r.Header.Get("X-Tenant-ID")})
	})))
	defer server2.Close()
	newCookie := sessionCookieValue(t, env.client, env.server.URL)
	req, _ = http.NewRequest(http.MethodGet, server2.URL+"/v1/sessions", nil)
	req.Header.Set("Cookie", auth.SessionCookieName+"="+newCookie)
	res, err = env.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("session did not survive a controller restart: %d", res.StatusCode)
	}
	if status, _, _ := env.do(env.client, http.MethodPost, "/v1/auth/logout", nil, env.csrfHeader(env.client)); status != http.StatusOK {
		t.Fatalf("logout: %d", status)
	}
	if status, _, _ := env.do(env.client, http.MethodGet, "/v1/sessions", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("post-logout request: %d", status)
	}

	// An admin-created member is disabled and its live session dies at once.
	admin := auth.Principal{Kind: auth.PrincipalHuman, TenantID: env.tenantID, AccountID: adminID, IsSuperAdmin: true}
	member, err := env.ctl.service.CreateAccount(context.Background(), admin, auth.AccountInput{
		Username: "member", DisplayName: "Member", Kind: "human", Password: "member-password-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	memberClient := env.newClient()
	if status, body := env.login(memberClient, "member", "member-password-123"); status != http.StatusOK {
		t.Fatalf("member login: %d %v", status, body)
	}
	if _, err := env.ctl.service.DisableAccount(context.Background(), admin, member.ID, true, "e2e"); err != nil {
		t.Fatalf("disable member: %v", err)
	}
	if status, _, _ := env.do(memberClient, http.MethodGet, "/v1/sessions", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("disabled member session: %d", status)
	}
	if status, body := env.login(memberClient, "member", "member-password-123"); status != http.StatusUnauthorized || body["code"] != "invalid_credentials" {
		t.Fatalf("disabled member login: %d %v", status, body)
	}
	if _, err := env.ctl.service.DisableAccount(context.Background(), admin, adminID, true, "e2e"); err == nil {
		t.Fatal("a super admin was disabled")
	}

	// Retired credentials: old passphrase bearer, old cookie format and old
	// api_keys rows are all 401 and never establish a session.
	for _, request := range []struct {
		name    string
		headers map[string]string
	}{
		{"old passphrase", map[string]string{"Authorization": "Bearer old-serve-password"}},
		{"old cookie", map[string]string{"Cookie": auth.SessionCookieName + "=AAAA.BBBB"}},
	} {
		if status, _, _ := env.do(env.newClient(), http.MethodGet, "/v1/sessions", nil, request.headers); status != http.StatusUnauthorized {
			t.Fatalf("%s: %d", request.name, status)
		}
	}
	minted, err := auth.MintKey(auth.RoleTenant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.st.CreateAPIKey(context.Background(), env.tenantID, uuid.New(), minted.Hash, minted.Prefix, minted.Role, "legacy"); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := env.do(env.newClient(), http.MethodGet, "/v1/sessions", nil,
		map[string]string{"Authorization": "Bearer " + minted.Plaintext}); status != http.StatusUnauthorized {
		t.Fatalf("legacy api key bearer: %d", status)
	}
	if status, _, _ := env.do(env.newClient(), http.MethodGet, "/v1/sessions", nil,
		map[string]string{"X-API-Key": minted.Plaintext}); status != http.StatusUnauthorized {
		t.Fatalf("legacy api key header: %d", status)
	}

	// Audit trail: every required event exists and no detail leaks a secret.
	events, err := env.st.ListAuthEvents(context.Background(), env.tenantID, store.AuthEventFilter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, event := range events {
		seen[event.Kind] = true
		raw, _ := json.Marshal(event.Detail)
		for _, secret := range []string{"admin-password-123", "admin-password-456", "member-password-123", token} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("auth_events detail leaked a secret: %s", raw)
			}
		}
	}
	for _, kind := range []string{
		store.AuthEventBootstrapUsed, store.AuthEventAccountBoot, store.AuthEventLoginSucceeded,
		store.AuthEventLoginFailed, store.AuthEventPasswordChange, store.AuthEventAccountDisable,
		store.AuthEventLogout,
	} {
		if !seen[kind] {
			t.Fatalf("missing audit event %q in %v", kind, seen)
		}
	}
}

func sessionCookieValue(t *testing.T, client *http.Client, base string) string {
	t.Helper()
	parsed, _ := url.Parse(base)
	for _, cookie := range client.Jar.Cookies(parsed) {
		if cookie.Name == auth.SessionCookieName {
			return cookie.Value
		}
	}
	return ""
}

// assertSessionCookieAttributes pins the spec's cookie flags: HttpOnly, Secure
// and SameSite=Lax, with the CSRF cookie readable by the app.
func assertSessionCookieAttributes(t *testing.T, headers http.Header) {
	t.Helper()
	var session, csrf string
	for _, value := range headers.Values("Set-Cookie") {
		if strings.HasPrefix(value, auth.SessionCookieName+"=") {
			session = value
		}
		if strings.HasPrefix(value, auth.CSRFCookieName+"=") {
			csrf = value
		}
	}
	if session == "" || csrf == "" {
		t.Fatalf("login did not set both cookies: %v", headers.Values("Set-Cookie"))
	}
	for _, flag := range []string{"HttpOnly", "Secure", "SameSite=Lax"} {
		if !strings.Contains(session, flag) {
			t.Fatalf("session cookie lacks %s: %s", flag, session)
		}
	}
	if !strings.Contains(csrf, "Secure") || !strings.Contains(csrf, "SameSite=Lax") {
		t.Fatalf("csrf cookie lacks Secure/SameSite: %s", csrf)
	}
	if strings.Contains(csrf, "HttpOnly") {
		t.Fatalf("csrf cookie must be readable by the app: %s", csrf)
	}
}

func TestLoopbackRequestRequiresBothHostAndPeer(t *testing.T) {
	loopback := func(host, remote string) bool {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/v1/sessions", nil)
		req.Host = host
		req.RemoteAddr = remote
		return loopbackRequest(req)
	}
	if !loopback("127.0.0.1:13750", "127.0.0.1:2222") {
		t.Fatal("loopback host and peer were refused")
	}
	if loopback("evil.test", "127.0.0.1:2222") {
		t.Fatal("a foreign Host header was admitted from loopback")
	}
	if loopback("127.0.0.1:13750", "203.0.113.5:2222") {
		t.Fatal("a non-loopback peer was admitted")
	}
}

func TestAuthRateLimitE2E(t *testing.T) {
	env := newAuthTestEnv(t, authModeRequired, uuid.Nil)
	env.bootstrapAdmin("admin-password-123", "admin-password-456")
	spray := env.newClient()
	for i := 1; i <= 4; i++ {
		status, body := env.login(spray, "spray-user", "wrong-password-123")
		if status != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d %v", i, status, body)
		}
	}
	// The fifth attempt is locked. X-Forwarded-For is ignored, so spoofing it
	// does not reset the bucket.
	status, body, headers := env.do(spray, http.MethodPost, "/v1/auth/login",
		map[string]any{"username": "spray-user", "password": "wrong-password-123"},
		mergeHeaders(env.csrfHeader(spray), map[string]string{"X-Forwarded-For": "198.51.100.9"}))
	if status != http.StatusTooManyRequests || body["code"] != "rate_limited" {
		t.Fatalf("rate limit: %d %v", status, body)
	}
	if headers.Get("Retry-After") == "" {
		t.Fatal("rate limit answer has no Retry-After")
	}
	// After the lock expires the username is accepted again (still a wrong
	// password, so 401 rather than 429).
	now := time.Now().UTC().Add(2 * time.Second)
	env.ctl.service.Now = func() time.Time { return now }
	env.ctl.service.Limiter.SetClock(func() time.Time { return now })
	status, body = env.login(spray, "spray-user", "wrong-password-123")
	if status != http.StatusUnauthorized {
		t.Fatalf("post-expiry attempt: %d %v", status, body)
	}
}

func mergeHeaders(left, right map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range left {
		out[key] = value
	}
	for key, value := range right {
		out[key] = value
	}
	return out
}

func TestBotKeyLifecycleE2E(t *testing.T) {
	env := newAuthTestEnv(t, authModeRequired, uuid.Nil)
	adminID, adminCSRF := env.bootstrapAdmin("admin-password-123", "admin-password-456")
	admin := auth.Principal{Kind: auth.PrincipalHuman, TenantID: env.tenantID, AccountID: adminID, IsSuperAdmin: true}

	botAccount, err := env.ctl.service.CreateAccount(context.Background(), admin, auth.AccountInput{
		Username: "bot-account", DisplayName: "Bot Account", Kind: "bot",
	})
	if err != nil {
		t.Fatal(err)
	}
	role, err := env.st.CreateRole(context.Background(), store.RoleRecord{
		TenantID: env.tenantID, ID: uuid.New(), Name: "Support",
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := env.st.CreateProfile(context.Background(), store.ProfileRecord{
		TenantID: env.tenantID, ID: uuid.New(), AccountID: botAccount.ID, RoleID: role.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.st.CreateBot(context.Background(), store.BotRecord{
		TenantID: env.tenantID, ID: "support-1", Name: "Support One", ProfileID: profile.ID,
		ModelKind: "text", DefaultModel: json.RawMessage(`{"providerId":"fake","modelId":"fake"}`),
		Harness: json.RawMessage(`{"tools":[],"context":{"permissions":[]}}`),
	}); err != nil {
		t.Fatal(err)
	}

	// The issuance surface is /v1/bots/{id}/keys (A4.1); the A2 bridge route is
	// gone with no alias. Mint through the store here: this test proves the bot
	// key AUTHENTICATION path, while the HTTP issuance contract is covered by the
	// agentapi identity tests.
	// The removed route answers a plain 404 (no JSON error body to decode).
	removedReq, err := http.NewRequest(http.MethodPost, env.server.URL+"/v1/auth/bot-keys",
		strings.NewReader(`{"accountId":"`+botAccount.ID.String()+`","botId":"support-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	removedReq.Header.Set("Content-Type", "application/json")
	for key, value := range adminCSRF {
		removedReq.Header.Set(key, value)
	}
	removedRes, err := env.client.Do(removedReq)
	if err != nil {
		t.Fatal(err)
	}
	removedRes.Body.Close()
	if removedRes.StatusCode != http.StatusNotFound {
		t.Fatalf("removed bot-key bridge route: %d, want 404", removedRes.StatusCode)
	}
	minted, err := auth.MintBotKey()
	if err != nil {
		t.Fatal(err)
	}
	key, err := env.st.CreateBotAPIKey(context.Background(), env.tenantID, uuid.New(), minted.Hash,
		minted.Prefix, botAccount.ID, "support-1", "ci")
	if err != nil {
		t.Fatal(err)
	}
	plaintext := minted.Plaintext
	keyID := key.KeyID.String()
	if !strings.HasPrefix(plaintext, auth.BotKeyScheme) {
		t.Fatalf("plaintext = %q", plaintext)
	}

	// The key acts as its bot; another bot id is a 403.
	status, echo, _ := env.do(env.newClient(), http.MethodGet, "/v1/bots/support-1/echo", nil,
		map[string]string{"Authorization": "Bearer " + plaintext})
	if status != http.StatusOK || echo["botId"] != "support-1" ||
		echo["accountId"] != botAccount.ID.String() || echo["tenantId"] != env.tenantID.String() {
		t.Fatalf("bot key echo: %d %v", status, echo)
	}
	if status, body, _ := env.do(env.newClient(), http.MethodGet, "/v1/bots/support-2/echo", nil,
		map[string]string{"Authorization": "Bearer " + plaintext}); status != http.StatusForbidden || body["code"] != "bot_mismatch" {
		t.Fatalf("wrong bot: %d %v", status, body)
	}
	// X-API-Key is the same credential path.
	if status, echo, _ := env.do(env.newClient(), http.MethodGet, "/v1/bots/support-1/echo", nil,
		map[string]string{"X-API-Key": plaintext}); status != http.StatusOK || echo["botId"] != "support-1" {
		t.Fatalf("bot key via X-API-Key: %d %v", status, echo)
	}
	// A bot never uses the browser auth surface...
	if status, _, _ := env.do(env.newClient(), http.MethodGet, "/v1/auth/session", nil,
		map[string]string{"Authorization": "Bearer " + plaintext}); status != http.StatusForbidden {
		t.Fatalf("bot on browser auth surface: %d", status)
	}
	// ...but whoami is identity introspection and answers for a bot key.
	if status, who, _ := env.do(env.newClient(), http.MethodGet, "/v1/auth/whoami", nil,
		map[string]string{"Authorization": "Bearer " + plaintext}); status != http.StatusOK || who["botId"] != "support-1" {
		t.Fatalf("bot whoami: %d %v", status, who)
	}
	// last_used_at is updated by authentication.
	keyUUID, _ := uuid.Parse(keyID)
	stored, err := env.st.GetBotAPIKey(context.Background(), env.tenantID, keyUUID)
	if err != nil || stored.LastUsedAt == nil {
		t.Fatalf("last_used_at: %+v err=%v", stored, err)
	}

	// Revocation is immediate.
	if err := env.st.RevokeBotAPIKey(context.Background(), env.tenantID, keyUUID); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := env.do(env.newClient(), http.MethodGet, "/v1/bots/support-1/echo", nil,
		map[string]string{"Authorization": "Bearer " + plaintext}); status != http.StatusUnauthorized {
		t.Fatalf("revoked key: %d", status)
	}
}

func authMustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAuthLocalOnlyE2E(t *testing.T) {
	env := newAuthTestEnv(t, authModeLocalOnly, uuid.Nil)
	status, body, _ := env.do(env.client, http.MethodGet, "/v1/sessions", nil, nil)
	if status != http.StatusOK || body["accountId"] == "" {
		t.Fatalf("local-only anonymous request: %d %v", status, body)
	}
	status, sessionBody := env.getSession(env.client)
	if status != http.StatusOK || sessionBody["authenticated"] != true ||
		sessionBody["localAdmin"] != true || sessionBody["mode"] != "local-only" {
		t.Fatalf("local-only session: %d %v", status, sessionBody)
	}
	// Writes still need the double-submit token.
	if status, _, _ := env.do(env.client, http.MethodPost, "/v1/auth/logout", nil, nil); status != http.StatusForbidden {
		t.Fatalf("local-only logout without csrf: %d", status)
	}
	if status, _, _ := env.do(env.client, http.MethodPost, "/v1/auth/logout", nil, env.csrfHeader(env.client)); status != http.StatusOK {
		t.Fatalf("local-only logout: %d", status)
	}
	// With no session the next request is the local admin again.
	if status, body, _ := env.do(env.client, http.MethodGet, "/v1/sessions", nil, nil); status != http.StatusOK || body["accountId"] == "" {
		t.Fatalf("local-only after logout: %d %v", status, body)
	}
	if status, body := env.getSession(env.client); status != http.StatusOK || body["authenticated"] != true {
		t.Fatalf("local-only session after logout: %d %v", status, body)
	}
	if status, body, _ := env.do(env.client, http.MethodPost, "/v1/auth/bootstrap",
		map[string]any{"token": "x", "username": "root", "password": "admin-password-123"}, env.csrfHeader(env.client)); status != http.StatusConflict || body["code"] != "bootstrap_not_needed" {
		t.Fatalf("bootstrap in local-only: %d %v", status, body)
	}
}

func TestAuthRejectsCrossTenantSessionAndKeyE2E(t *testing.T) {
	env := newAuthTestEnv(t, authModeRequired, uuid.Nil)
	env.bootstrapAdmin("admin-password-123", "admin-password-456")

	// A session minted for another tenant (same signing key) must not inject
	// that tenant into this server.
	otherTenant := uuid.New()
	if err := env.st.CreateTenant(context.Background(), otherTenant, "other"); err != nil {
		t.Fatal(err)
	}
	cleanupAuthTenant(t, env.st, otherTenant)
	otherAccount, err := env.st.CreateAccount(context.Background(), store.AccountRecord{
		TenantID: otherTenant, ID: uuid.New(), Username: "other-user",
		DisplayName: "Other", Kind: "human", PasswordHash: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	otherSession, err := env.st.CreateAuthSession(context.Background(), store.AuthSession{
		TenantID: otherTenant, SessionID: uuid.New(), AccountID: otherAccount.ID,
		IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie, _ := auth.NewSessionCookie(env.secret)
	req, _ := http.NewRequest(http.MethodGet, env.server.URL+"/v1/sessions", nil)
	req.Header.Set("Cookie", auth.SessionCookieName+"="+cookie.Sign(otherSession.SessionID))
	res, err := env.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cross-tenant session: %d", res.StatusCode)
	}

	// A bot key from another tenant is equally invalid on this server.
	role, _ := env.st.CreateRole(context.Background(), store.RoleRecord{TenantID: otherTenant, ID: uuid.New(), Name: "R"})
	botAccount, _ := env.st.CreateAccount(context.Background(), store.AccountRecord{
		TenantID: otherTenant, ID: uuid.New(), Username: "other-bot", DisplayName: "Other Bot", Kind: "bot",
	})
	profile, _ := env.st.CreateProfile(context.Background(), store.ProfileRecord{
		TenantID: otherTenant, ID: uuid.New(), AccountID: botAccount.ID, RoleID: role.ID,
	})
	if _, err := env.st.CreateBot(context.Background(), store.BotRecord{
		TenantID: otherTenant, ID: "other-bot-1", Name: "Other", ProfileID: profile.ID,
		ModelKind: "text", DefaultModel: json.RawMessage(`{}`), Harness: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	minted, _ := auth.MintBotKey()
	if _, err := env.st.CreateBotAPIKey(context.Background(), otherTenant, uuid.New(), minted.Hash,
		minted.Prefix, botAccount.ID, "other-bot-1", ""); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := env.do(env.newClient(), http.MethodGet, "/v1/bots/other-bot-1/echo", nil,
		map[string]string{"Authorization": "Bearer " + minted.Plaintext}); status != http.StatusUnauthorized {
		t.Fatalf("cross-tenant bot key: %d", status)
	}
}
