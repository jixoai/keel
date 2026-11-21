// Account authentication at the serving edge (change keel-bot-identity-access,
// A2).
//
// The old model was one deployment passphrase (`KEEL_SERVE_PASSWORD`) exchanged
// for a self-contained HMAC cookie. This file replaces it, with no compatibility
// path:
//
//   - humans log in with username+password; the cookie names a row in
//     auth_sessions and every request re-checks that row;
//   - bots present a key bound to (account, bot); they never get a session;
//   - every non-GET /v1 write must present X-Keel-CSRF matching the signed
//     keel_csrf cookie;
//   - auth=required|local-only|off. local-only and off answer loopback only and
//     map anonymous requests to the built-in local-admin super admin; off
//     additionally needs an explicit --insecure at boot.
//
// Old cookies fail the "v1." signature format; old passphrase bearers and old
// keel_tenant_/keel_admin_ API keys are rejected before any store lookup.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/store"
)

// authMode is the deployment authentication policy.
type authMode string

const (
	authModeRequired  authMode = "required"
	authModeLocalOnly authMode = "local-only"
	authModeOff       authMode = "off"

	// Local-only and off map anonymous requests to this built-in super admin.
	// The ids are fixed so several replicas converge on one row.
	localTenantIDString = "00000000-0000-0000-0000-00000000a001"
	localAdminIDString  = "00000000-0000-0000-0000-00000000a002"
	localAdminUsername  = "local-admin"
)

var (
	localTenantID = uuid.MustParse(localTenantIDString)
	localAdminID  = uuid.MustParse(localAdminIDString)
)

func parseAuthMode(raw string) (authMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(authModeRequired):
		return authModeRequired, nil
	case string(authModeLocalOnly):
		return authModeLocalOnly, nil
	case string(authModeOff):
		return authModeOff, nil
	default:
		return "", fmt.Errorf("auth mode %q (want required, local-only or off)", raw)
	}
}

func (m authMode) usesLocalAdmin() bool { return m == authModeLocalOnly || m == authModeOff }

// validateAuthBinding refuses a configuration that would expose a no-password
// mode beyond loopback. It checks the planned address before net.Listen and the
// actual listener afterwards (validateListener).
func validateAuthBinding(mode authMode, insecure bool, addr string) error {
	if mode == authModeOff && !insecure {
		return errors.New("auth=off requires the explicit --insecure flag (loopback only, never expose it)")
	}
	if !mode.usesLocalAdmin() {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("auth=%s requires a host:port loopback address: %w", mode, err)
	}
	if !loopbackHost(host) {
		return fmt.Errorf("auth=%s refuses to bind %q: only loopback addresses are allowed", mode, addr)
	}
	return nil
}

// validateListener re-checks the resolved listener: a hostname that passed the
// textual check must not resolve to a wildcard interface.
func validateListener(mode authMode, ln net.Listener) error {
	if !mode.usesLocalAdmin() {
		return nil
	}
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok || tcp.IP == nil || !tcp.IP.IsLoopback() {
		return fmt.Errorf("auth=%s listener %s is not loopback", mode, ln.Addr())
	}
	return nil
}

// authConfig is the resolved edge policy. SecretKey is the 32-byte
// KEEL_SECRET_KEY (or an ephemeral key in local-only/off when it is unset).
type authConfig struct {
	Mode       authMode
	Store      *store.Store
	TenantID   uuid.UUID
	SecretKey  []byte
	SessionTTL time.Duration
	Now        func() time.Time
	// LocalAdmin is the principal a local-only/off request receives when it has
	// no session. ensureLocalIdentity builds it at boot.
	LocalAdmin auth.Principal
}

// authController is the serving middleware plus the /v1/auth route set. The
// routes share one policy object, so a handler can never be mounted behind a
// different authentication mode than the middleware.
type authController struct {
	cfg     authConfig
	logger  *slog.Logger
	service *auth.Service
	cookie  *auth.SessionCookie
	csrf    *auth.CSRF
}

func newAuthController(cfg authConfig, log *slog.Logger) (*authController, error) {
	if cfg.Store == nil {
		return nil, errors.New("auth controller requires a store")
	}
	if cfg.TenantID == uuid.Nil {
		return nil, errors.New("auth controller requires a tenant id")
	}
	if len(cfg.SecretKey) != 32 {
		return nil, errors.New("auth controller requires a 32-byte secret key")
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 12 * time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	service := auth.NewService(cfg.Store, cfg.TenantID)
	service.SessionTTL = cfg.SessionTTL
	service.Now = cfg.Now
	if cfg.Mode.usesLocalAdmin() && cfg.LocalAdmin.AccountID == uuid.Nil {
		return nil, errors.New("auth local-only/off requires the local-admin principal")
	}
	cookie, err := auth.NewSessionCookie(cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	csrf, err := auth.NewCSRF(cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	return &authController{cfg: cfg, logger: log, service: service, cookie: cookie, csrf: csrf}, nil
}

// register mounts the auth surface on the API mux.
func (a *authController) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/auth/login", a.login)
	mux.HandleFunc("POST /v1/auth/logout", a.logout)
	mux.HandleFunc("GET /v1/auth/session", a.session)
	mux.HandleFunc("POST /v1/auth/password", a.changePassword)
	mux.HandleFunc("POST /v1/auth/bootstrap", a.bootstrap)
	// Bot credentials are the A2 bridge until A4 mounts them under /v1/bots.
	// They are super-admin-only here; A3/A4 will replace the check with the
	// permission registry.
	mux.HandleFunc("POST /v1/auth/bot-keys", a.issueBotKey)
	mux.HandleFunc("GET /v1/auth/bot-keys", a.listBotKeys)
	mux.HandleFunc("DELETE /v1/auth/bot-keys/{keyId}", a.revokeBotKey)
}

// wrap returns the edge middleware around next (the quota edge + dispatcher).
func (a *authController) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.serve(w, r, next)
	})
}

// mode is the boot log and doctor label.
func (a *authController) mode() string { return string(a.cfg.Mode) }

func (a *authController) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	path := r.URL.Path
	if a.cfg.Mode.usesLocalAdmin() && !loopbackRequest(r) {
		writeAuthError(w, http.StatusForbidden, "loopback_only", "auth="+string(a.cfg.Mode)+" only answers loopback requests")
		return
	}
	// Probes and the static app do not require a session: the login form must be
	// able to load. /metrics follows the pre-existing operator contract.
	if path == "/v1/health" || path == "/v1/ready" || path == "/metrics" || !strings.HasPrefix(path, "/v1") {
		next.ServeHTTP(w, r)
		return
	}

	if principal, handled := a.machinePrincipal(w, r, next); handled {
		if principal != nil {
			a.servePrincipal(w, r, *principal, next)
		}
		return
	}

	principal, ok, err := a.sessionPrincipal(r)
	if err != nil {
		clearSessionCookies(w)
		writeAuthError(w, http.StatusUnauthorized, "invalid_session", "authentication required")
		return
	}
	if ok {
		a.servePrincipal(w, r, principal, next)
		return
	}

	if a.cfg.Mode == authModeRequired {
		if !anonymousAuthRoute(path) {
			required, err := a.service.BootstrapRequired(r.Context())
			if err != nil {
				writeAuthError(w, http.StatusServiceUnavailable, "auth_unavailable", "authentication is not ready")
				return
			}
			if required {
				writeAuthError(w, http.StatusServiceUnavailable, "bootstrap_required",
					"no super admin exists: run `keel admin bootstrap` and complete setup")
				return
			}
			w.Header().Set("WWW-Authenticate", `Cookie realm="keel"`)
			writeAuthError(w, http.StatusUnauthorized, "authentication_required", "authentication required")
			return
		}
		if !a.checkCSRF(w, r, auth.AnonymousCSRFSubject) {
			return
		}
		next.ServeHTTP(w, r)
		return
	}

	// local-only / off: an unauthenticated loopback request is the local admin.
	a.servePrincipal(w, r, a.cfg.LocalAdmin, next)
}

// servePrincipal applies the principal's restrictions, injects the identity
// headers and forwards. Client-supplied identity headers are always replaced.
func (a *authController) servePrincipal(w http.ResponseWriter, r *http.Request, principal auth.Principal, next http.Handler) {
	if principal.IsBot() {
		if strings.HasPrefix(r.URL.Path, "/v1/auth/") {
			writeAuthError(w, http.StatusForbidden, "bot_browser_forbidden", "a bot key cannot use the browser auth surface")
			return
		}
		if target := botIDFromPath(r.URL.Path); target != "" && target != principal.BotID {
			writeAuthError(w, http.StatusForbidden, "bot_mismatch", "this key is bound to another bot")
			return
		}
		if claimed := strings.TrimSpace(r.Header.Get("X-Bot-ID")); claimed != "" && claimed != principal.BotID {
			writeAuthError(w, http.StatusForbidden, "bot_mismatch", "this key is bound to another bot")
			return
		}
	} else {
		subject := auth.AnonymousCSRFSubject
		if principal.SessionID != uuid.Nil {
			subject = principal.SessionID.String()
		}
		if !a.checkCSRF(w, r, subject) {
			return
		}
		if principal.MustChangePassword && !passwordChangeAllowed(r) {
			writeAuthError(w, http.StatusForbidden, "password_change_required",
				"change the password before using the API")
			return
		}
	}
	setIdentityHeaders(r, principal)
	r = r.WithContext(withPrincipal(r.Context(), principal))
	next.ServeHTTP(w, r)
}

// machinePrincipal resolves X-API-Key / Bearer keel_bot_*. handled=false means
// no machine credential was present and the session path still owns the
// request. Every other Authorization value is rejected rather than forwarded:
// the old passphrase and old api_keys must not reach a handler.
func (a *authController) machinePrincipal(w http.ResponseWriter, r *http.Request, next http.Handler) (*auth.Principal, bool) {
	if value := strings.TrimSpace(r.Header.Get("X-API-Key")); value != "" {
		principal, err := a.service.AuthenticateBotKey(r.Context(), value)
		if err != nil {
			writeAuthError(w, http.StatusUnauthorized, "invalid_api_key", "invalid api key")
			return nil, true
		}
		r.Header.Del("X-API-Key")
		r.Header.Del("Authorization")
		return &principal, true
	}
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return nil, false
	}
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		writeAuthError(w, http.StatusUnauthorized, "invalid_credential", "invalid credentials")
		return nil, true
	}
	token = strings.TrimSpace(token)
	switch {
	case auth.LooksLikeBotKey(token) || strings.HasPrefix(token, auth.BotKeyScheme):
		principal, err := a.service.AuthenticateBotKey(r.Context(), token)
		if err != nil {
			writeAuthError(w, http.StatusUnauthorized, "invalid_api_key", "invalid api key")
			return nil, true
		}
		r.Header.Del("Authorization")
		return &principal, true
	case strings.HasPrefix(token, auth.KeyScheme):
		// keel_tenant_* / keel_admin_* are the retired api_keys shape (L19).
		writeAuthError(w, http.StatusUnauthorized, "invalid_api_key", "invalid api key")
		return nil, true
	}
	// The operator provisioning surface keeps KEEL_ADMIN_KEY as a deployment
	// credential. It is not a human or bot login: it reaches only /v1/admin/*
	// and the handler re-checks it in constant time.
	if strings.HasPrefix(r.URL.Path, "/v1/admin/") && adminKeyMatches(token) {
		principal := auth.Principal{
			Kind: auth.PrincipalLocal, TenantID: a.cfg.TenantID, IsSuperAdmin: true,
		}
		setIdentityHeaders(r, principal)
		// Keep the Authorization header: the provisioning handler is the one
		// that re-checks the operator key. This path is a deployment credential,
		// not a human/bot login, so CSRF (an ambient-cookie defence) does not
		// apply.
		next.ServeHTTP(w, r)
		return nil, true
	}
	// Any other bearer is the retired passphrase or an unknown credential.
	writeAuthError(w, http.StatusUnauthorized, "invalid_credential", "invalid credentials")
	return nil, true
}

// adminKeyMatches is the constant-time KEEL_ADMIN_KEY comparison.
func adminKeyMatches(token string) bool {
	expected := strings.TrimSpace(os.Getenv("KEEL_ADMIN_KEY"))
	if expected == "" || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}

func (a *authController) sessionPrincipal(r *http.Request) (auth.Principal, bool, error) {
	cookie, err := r.Cookie(auth.SessionCookieName)
	if err != nil {
		return auth.Principal{}, false, nil
	}
	sessionID, err := a.cookie.Verify(cookie.Value)
	if err != nil {
		return auth.Principal{}, false, err
	}
	principal, err := a.service.AuthenticateSession(r.Context(), sessionID)
	if err != nil {
		return auth.Principal{}, false, err
	}
	return principal, true, nil
}

func (a *authController) checkCSRF(w http.ResponseWriter, r *http.Request, subject string) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	header := strings.TrimSpace(r.Header.Get(auth.CSRFHeaderName))
	cookie, err := r.Cookie(auth.CSRFCookieName)
	if err != nil || header == "" || !auth.Equal(header, cookie.Value) || !a.csrf.Verify(subject, header) {
		writeAuthError(w, http.StatusForbidden, "csrf_failed", "a valid X-Keel-CSRF header is required for writes")
		return false
	}
	return true
}

// --- handlers ---------------------------------------------------------------

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *authController) login(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Mode == authModeRequired {
		required, err := a.service.BootstrapRequired(r.Context())
		if err != nil {
			writeAuthError(w, http.StatusServiceUnavailable, "auth_unavailable", "authentication is not ready")
			return
		}
		if required {
			writeAuthError(w, http.StatusUnauthorized, "bootstrap_required",
				"no super admin exists: complete `keel admin bootstrap` first")
			return
		}
	}
	var body loginRequest
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Username) == "" || body.Password == "" {
		writeAuthError(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password")
		return
	}
	result, err := a.service.Login(r.Context(), auth.LoginInput{
		Username: body.Username, Password: body.Password,
		UserAgent: r.UserAgent(), IP: remoteIP(r),
	})
	if err != nil {
		a.writeLoginError(w, err)
		return
	}
	a.writeSessionResult(w, result, http.StatusOK)
}

func (a *authController) writeLoginError(w http.ResponseWriter, err error) {
	var limited *auth.RateLimitedError
	switch {
	case errors.As(err, &limited):
		seconds := int(limited.RetryAfter.Round(time.Second).Seconds())
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", fmt.Sprint(seconds))
		writeAuthError(w, http.StatusTooManyRequests, "rate_limited", "too many login attempts")
	case errors.Is(err, auth.ErrTenantNotConfigured):
		writeAuthError(w, http.StatusServiceUnavailable, "tenant_not_configured", "the deployment tenant is not configured")
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeAuthError(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password")
	case errors.Is(err, auth.ErrBootstrapRequired):
		writeAuthError(w, http.StatusUnauthorized, "bootstrap_required", "bootstrap is required")
	default:
		a.logger.Error("login", "err", err)
		writeAuthError(w, http.StatusInternalServerError, "internal_error", "login failed")
	}
}

func (a *authController) logout(w http.ResponseWriter, r *http.Request) {
	if principal, ok := principalFrom(r.Context()); ok && principal.SessionID != uuid.Nil {
		if err := a.service.Logout(r.Context(), principal); err != nil {
			a.logger.Error("logout", "err", err)
			writeAuthError(w, http.StatusInternalServerError, "internal_error", "logout failed")
			return
		}
	}
	clearSessionCookies(w)
	writeAuthJSON(w, http.StatusOK, map[string]any{"authenticated": false})
}

func (a *authController) session(w http.ResponseWriter, r *http.Request) {
	required := false
	if a.cfg.Mode == authModeRequired {
		var err error
		required, err = a.service.BootstrapRequired(r.Context())
		if err != nil {
			writeAuthError(w, http.StatusServiceUnavailable, "auth_unavailable", "authentication is not ready")
			return
		}
	}
	principal, ok, err := a.sessionPrincipal(r)
	if err != nil {
		clearSessionCookies(w)
		writeAuthError(w, http.StatusUnauthorized, "invalid_session", "authentication required")
		return
	}
	if !ok && a.cfg.Mode.usesLocalAdmin() {
		principal, ok = a.cfg.LocalAdmin, true
	}
	subject := auth.AnonymousCSRFSubject
	if ok && principal.SessionID != uuid.Nil {
		subject = principal.SessionID.String()
	}
	token, err := a.issueCSRF(w, subject)
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "internal_error", "could not issue a csrf token")
		return
	}
	if !ok {
		writeAuthJSON(w, http.StatusOK, map[string]any{
			"authenticated": false, "mode": string(a.cfg.Mode),
			"bootstrapRequired": required, "csrfToken": token,
		})
		return
	}
	writeAuthJSON(w, http.StatusOK, a.sessionBody(principal, token, principal.MustChangePassword))
}

type passwordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

func (a *authController) changePassword(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok || !principal.IsHuman() || principal.Kind == auth.PrincipalLocal {
		writeAuthError(w, http.StatusForbidden, "forbidden", "a human session is required")
		return
	}
	var body passwordRequest
	if !decodeJSONBody(w, r, &body) {
		return
	}
	result, err := a.service.ChangePassword(r.Context(), principal,
		body.CurrentPassword, body.NewPassword, r.UserAgent(), remoteIP(r))
	switch {
	case err == nil:
		a.writeSessionResult(w, result, http.StatusOK)
	case errors.Is(err, auth.ErrPasswordPolicy):
		writeAuthError(w, http.StatusBadRequest, "weak_password",
			fmt.Sprintf("password must be %d..%d bytes", auth.MinPasswordBytes, auth.MaxPasswordBytes))
	case errors.Is(err, auth.ErrPasswordReused):
		writeAuthError(w, http.StatusBadRequest, "password_reused", "the new password must differ from the current one")
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeAuthError(w, http.StatusUnauthorized, "invalid_credentials", "the current password is not valid")
	case errors.Is(err, auth.ErrSessionInvalid):
		clearSessionCookies(w)
		writeAuthError(w, http.StatusUnauthorized, "invalid_session", "authentication required")
	default:
		a.logger.Error("change password", "err", err)
		writeAuthError(w, http.StatusInternalServerError, "internal_error", "password change failed")
	}
}

type bootstrapRequest struct {
	Token       string `json:"token"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"displayName"`
}

func (a *authController) bootstrap(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Mode != authModeRequired {
		writeAuthError(w, http.StatusConflict, "bootstrap_not_needed",
			"auth="+string(a.cfg.Mode)+" maps anonymous requests to the local admin")
		return
	}
	var body bootstrapRequest
	if !decodeJSONBody(w, r, &body) {
		return
	}
	account, err := a.service.Bootstrap(r.Context(), body.Token, body.Username, body.Password, body.DisplayName)
	switch {
	case err == nil:
		a.logger.Info("super admin bootstrapped", "account", account.ID.String(), "username", account.Username)
		writeAuthJSON(w, http.StatusOK, map[string]any{
			"bootstrapped": true, "mustChangePassword": true,
			"account": map[string]any{"id": account.ID.String(), "username": account.Username},
		})
	case errors.Is(err, auth.ErrBootstrapComplete):
		writeAuthError(w, http.StatusConflict, "bootstrap_complete", "a super admin already exists")
	case errors.Is(err, auth.ErrUsernameTaken):
		writeAuthError(w, http.StatusConflict, "username_taken", "the username is already taken")
	case errors.Is(err, auth.ErrBootstrapTokenInvalid):
		writeAuthError(w, http.StatusUnauthorized, "invalid_setup_token", "the setup token is invalid, expired or already used")
	case errors.Is(err, auth.ErrPasswordPolicy):
		writeAuthError(w, http.StatusBadRequest, "weak_password",
			fmt.Sprintf("password must be %d..%d bytes", auth.MinPasswordBytes, auth.MaxPasswordBytes))
	case errors.Is(err, auth.ErrTenantNotConfigured):
		writeAuthError(w, http.StatusServiceUnavailable, "tenant_not_configured", "the deployment tenant is not configured")
	default:
		a.logger.Error("bootstrap", "err", err)
		writeAuthError(w, http.StatusInternalServerError, "internal_error", "bootstrap failed")
	}
}

type issueBotKeyRequest struct {
	AccountID string `json:"accountId"`
	BotID     string `json:"botId"`
	Label     string `json:"label"`
}

func (a *authController) issueBotKey(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		writeAuthError(w, http.StatusUnauthorized, "authentication_required", "authentication required")
		return
	}
	var body issueBotKeyRequest
	if !decodeJSONBody(w, r, &body) {
		return
	}
	accountID, err := uuid.Parse(strings.TrimSpace(body.AccountID))
	if err != nil || accountID == uuid.Nil || strings.TrimSpace(body.BotID) == "" {
		writeAuthError(w, http.StatusBadRequest, "invalid_request", "accountId and botId are required")
		return
	}
	issued, err := a.service.IssueBotKey(r.Context(), principal, accountID, body.BotID, body.Label)
	if err != nil {
		a.writeBotKeyError(w, err)
		return
	}
	writeAuthJSON(w, http.StatusCreated, map[string]any{
		"key": map[string]any{
			"keyId": issued.Key.KeyID.String(), "prefix": issued.Key.Prefix,
			"accountId": issued.Key.AccountID.String(), "botId": issued.Key.BotID,
			"label": issued.Key.Label, "createdAt": issued.Key.CreatedAt.UTC(),
		},
		"plaintext": issued.Plaintext,
		"notice":    "this key is shown once; it cannot be read again",
	})
}

func (a *authController) listBotKeys(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		writeAuthError(w, http.StatusUnauthorized, "authentication_required", "authentication required")
		return
	}
	keys, err := a.service.ListBotKeys(r.Context(), principal, r.URL.Query().Get("botId"))
	if err != nil {
		a.writeBotKeyError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		out = append(out, map[string]any{
			"keyId": key.KeyID.String(), "prefix": key.Prefix,
			"accountId": key.AccountID.String(), "botId": key.BotID,
			"label": key.Label, "createdAt": key.CreatedAt.UTC(),
			"revokedAt": key.RevokedAt, "lastUsedAt": key.LastUsedAt,
		})
	}
	writeAuthJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (a *authController) revokeBotKey(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		writeAuthError(w, http.StatusUnauthorized, "authentication_required", "authentication required")
		return
	}
	keyID, err := uuid.Parse(strings.TrimSpace(r.PathValue("keyId")))
	if err != nil || keyID == uuid.Nil {
		writeAuthError(w, http.StatusBadRequest, "invalid_request", "keyId must be a UUID")
		return
	}
	if err := a.service.RevokeBotKey(r.Context(), principal, keyID); err != nil {
		a.writeBotKeyError(w, err)
		return
	}
	writeAuthJSON(w, http.StatusOK, map[string]any{"revoked": keyID.String()})
}

func (a *authController) writeBotKeyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrForbidden):
		writeAuthError(w, http.StatusForbidden, "forbidden", "a super admin is required, and the bot must belong to the named account")
	case errors.Is(err, store.ErrNotFound):
		writeAuthError(w, http.StatusNotFound, "not_found", "account or bot not found")
	case errors.Is(err, auth.ErrInvalidAccount):
		writeAuthError(w, http.StatusBadRequest, "invalid_request", "invalid account or bot")
	default:
		a.logger.Error("bot key", "err", err)
		writeAuthError(w, http.StatusInternalServerError, "internal_error", "bot key operation failed")
	}
}

// --- response/cookie helpers ------------------------------------------------

func (a *authController) writeSessionResult(w http.ResponseWriter, result auth.SessionResult, status int) {
	principal := result.Principal
	subject := auth.AnonymousCSRFSubject
	if principal.SessionID != uuid.Nil {
		subject = principal.SessionID.String()
	}
	token, err := a.issueCSRF(w, subject)
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "internal_error", "could not issue a csrf token")
		return
	}
	a.setSessionCookie(w, result.Session)
	body := a.sessionBody(principal, token, principal.MustChangePassword)
	body["expiresAt"] = result.Session.ExpiresAt.UTC()
	writeAuthJSON(w, status, body)
}

func (a *authController) sessionBody(principal auth.Principal, csrfToken string, mustChange bool) map[string]any {
	body := map[string]any{
		"authenticated": true,
		"mode":          string(a.cfg.Mode),
		"csrfToken":     csrfToken,
		"tenantId":      principal.TenantID.String(),
		"account": map[string]any{
			"id": principal.AccountID.String(), "username": principal.Username,
			"displayName": principal.DisplayName, "isSuperAdmin": principal.IsSuperAdmin,
		},
		"mustChangePassword": mustChange,
	}
	if principal.IsBot() {
		body["botId"] = principal.BotID
		body["keyId"] = principal.KeyID.String()
	}
	if principal.SessionID != uuid.Nil {
		body["sessionId"] = principal.SessionID.String()
		body["expiresAt"] = a.service.Now().Add(a.cfg.SessionTTL).UTC()
	}
	if principal.Kind == auth.PrincipalLocal {
		body["localAdmin"] = true
	}
	return body
}

func (a *authController) setSessionCookie(w http.ResponseWriter, session store.AuthSession) {
	maxAge := int(session.ExpiresAt.Sub(a.cfg.Now()).Seconds())
	if maxAge < 1 {
		maxAge = int(a.cfg.SessionTTL.Seconds())
	}
	http.SetCookie(w, &http.Cookie{
		Name: auth.SessionCookieName, Value: a.cookie.Sign(session.SessionID),
		Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *authController) issueCSRF(w http.ResponseWriter, subject string) (string, error) {
	token, err := a.csrf.Issue(subject)
	if err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{
		Name: auth.CSRFCookieName, Value: token, Path: "/",
		MaxAge: int(a.cfg.SessionTTL.Seconds()), HttpOnly: false, Secure: true,
		SameSite: http.SameSiteLaxMode,
	})
	return token, nil
}

func clearSessionCookies(w http.ResponseWriter) {
	for _, name := range []string{auth.SessionCookieName, auth.CSRFCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/", MaxAge: -1,
			Expires: time.Unix(0, 0), HttpOnly: name == auth.SessionCookieName,
			Secure: true, SameSite: http.SameSiteLaxMode,
		})
	}
}

// --- request helpers ---------------------------------------------------------

type principalContextKey struct{}

func withPrincipal(ctx context.Context, principal auth.Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

func principalFrom(ctx context.Context) (auth.Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(auth.Principal)
	return principal, ok
}

func setIdentityHeaders(r *http.Request, principal auth.Principal) {
	r.Header.Del("X-Tenant-ID")
	r.Header.Del("X-Account-ID")
	r.Header.Del("X-Bot-ID")
	if principal.TenantID != uuid.Nil {
		r.Header.Set("X-Tenant-ID", principal.TenantID.String())
	}
	if principal.AccountID != uuid.Nil {
		r.Header.Set("X-Account-ID", principal.AccountID.String())
	}
	if principal.BotID != "" {
		r.Header.Set("X-Bot-ID", principal.BotID)
	}
}

func anonymousAuthRoute(path string) bool {
	switch path {
	case "/v1/auth/login", "/v1/auth/logout", "/v1/auth/session", "/v1/auth/bootstrap":
		return true
	}
	return false
}

func passwordChangeAllowed(r *http.Request) bool {
	switch r.URL.Path {
	case "/v1/auth/session", "/v1/auth/password", "/v1/auth/logout":
		return true
	}
	return false
}

// botIDFromPath extracts {id} from /v1/bots/{id}[/...]. A key bound to another
// bot is a 403, never a silent substitution.
func botIDFromPath(path string) string {
	const prefix = "/v1/bots/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	if rest == "" {
		return ""
	}
	id, _, _ := strings.Cut(rest, "/")
	return id
}

// loopbackRequest requires both the Host header and the transport peer to be
// loopback, so a DNS-rebinding page and a proxied connection are both refused.
func loopbackRequest(r *http.Request) bool {
	if !loopbackHost(r.Host) {
		return false
	}
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func remoteIP(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.Trim(host, "[]")
}

// errAuthConfig marks an unusable authentication configuration at boot.
var errAuthConfig = errors.New("invalid authentication configuration")

func parseTenant(raw string) (uuid.UUID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return uuid.Nil, nil
	}
	tenantID, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: KEEL_SERVE_TENANT %q is not a UUID", errAuthConfig, raw)
	}
	return tenantID, nil
}

// envBool reads a boolean environment flag ("1"/"true" enable).
func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// loopbackHost reports whether a host (with or without port) names the local
// machine. Used by local-only/off mode admission, never as a credential.
func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, body any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(body); err != nil {
		writeAuthError(w, http.StatusBadRequest, "invalid_body", "invalid JSON body")
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeAuthError(w, http.StatusBadRequest, "invalid_body", "invalid JSON body")
		return false
	}
	return true
}

func writeAuthJSON(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeAuthError(w http.ResponseWriter, status int, code, message string) {
	writeAuthJSON(w, status, map[string]any{"error": message, "code": code})
}

// --- boot helpers ------------------------------------------------------------

// ensureAuthSecret reads KEEL_SECRET_KEY. In required mode the deployment must
// configure it; in local-only/off an ephemeral key keeps zero-config
// development working but logs that sessions do not survive a restart.
func ensureAuthSecret(mode authMode, log *slog.Logger) ([]byte, error) {
	key, err := store.SecretKeyFromEnv(os.Getenv)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, store.ErrSecretKeyMissing) {
		return nil, err
	}
	if mode == authModeRequired {
		return nil, errors.New("KEEL_SECRET_KEY is required for auth=required (32 bytes, base64 or hex)")
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate ephemeral auth key: %w", err)
	}
	log.Warn("KEEL_SECRET_KEY is unset: using an ephemeral key (sessions and CSRF tokens expire on restart)")
	return key, nil
}

// ensureLocalIdentity provisions the tenant and the built-in local-admin super
// admin for local-only/off. It never creates a super admin in required mode:
// "the first visitor becomes super admin" must not exist.
func ensureLocalIdentity(ctx context.Context, st *store.Store, tenantID uuid.UUID, log *slog.Logger) (auth.Principal, error) {
	if tenantID == uuid.Nil {
		tenantID = localTenantID
	}
	if _, err := st.CreateTenantReport(ctx, tenantID, "local"); err != nil {
		return auth.Principal{}, err
	}
	account, err := st.GetAccountByUsername(ctx, tenantID, localAdminUsername)
	if errors.Is(err, store.ErrNotFound) {
		account, err = st.CreateAccountAudited(ctx, store.AccountRecord{
			TenantID: tenantID, ID: localAdminID, Username: localAdminUsername,
			DisplayName: "Local Admin", Kind: "service", IsSuperAdmin: true,
		}, uuid.Nil)
		if errors.Is(err, store.ErrConflict) {
			account, err = st.GetAccountByUsername(ctx, tenantID, localAdminUsername)
		}
	}
	if err != nil {
		return auth.Principal{}, err
	}
	if !account.IsSuperAdmin || account.DisabledAt != nil {
		return auth.Principal{}, errors.New("the built-in local-admin account is not an enabled super admin")
	}
	log.Info("local authentication mode", "mode", "local-only/off", "account", account.ID.String(), "tenant", tenantID.String())
	return auth.Principal{
		Kind: auth.PrincipalLocal, TenantID: tenantID, AccountID: account.ID,
		Username: account.Username, DisplayName: account.DisplayName, IsSuperAdmin: true,
	}, nil
}

// ensureBootstrapEnvToken installs KEEL_BOOTSTRAP_TOKEN as a one-time setup
// token. It is the container-friendly alternative to running the CLI inside the
// container; the plaintext still lives only in the environment and the hash in
// auth_events.
func ensureBootstrapEnvToken(ctx context.Context, st *store.Store, tenantID uuid.UUID, log *slog.Logger) error {
	token := strings.TrimSpace(os.Getenv("KEEL_BOOTSTRAP_TOKEN"))
	if token == "" || tenantID == uuid.Nil {
		return nil
	}
	has, err := st.HasSuperAdmin(ctx)
	if err != nil {
		return err
	}
	if has {
		log.Info("KEEL_BOOTSTRAP_TOKEN ignored: a super admin already exists")
		return nil
	}
	created, err := st.EnsureBootstrapToken(ctx, tenantID, auth.HashToken(token),
		time.Now().UTC().Add(store.BootstrapTokenTTL), "environment")
	if err != nil {
		return err
	}
	if created {
		log.Info("bootstrap token configured from environment", "ttl", store.BootstrapTokenTTL.String())
	}
	return nil
}
