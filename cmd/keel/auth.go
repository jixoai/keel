// Command keel is the keel agent product binary: `keel serve` hosts the
// AgentAPI, the static web UI and the SSE edge on one origin; `keel tui` runs the
// NSH TUI client against it; `keel version` and `keel doctor` are diagnostics.
//
// # Authentication boundary
//
// The browser never holds the kernel's X-Tenant-ID or an admin key. `keel serve`
// authenticates the client at the service layer (a local passphrase, injected
// through an environment variable) and injects the tenant header towards the
// kernel. Internal packages (httpapi, agentapi, kernel) only ever see
// X-Tenant-ID — exactly the contract the kernel documents.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

// authCookieName is the session cookie of the local passphrase login.
const authCookieName = "keel_session"

// authConfig is the service-layer authentication policy.
type authConfig struct {
	// Password is the local passphrase (KEEL_SERVE_PASSWORD). Empty disables
	// authentication: only acceptable for loopback development, and the server
	// logs it loudly.
	Password string
	// TenantID is the tenant the server acts as (KEEL_SERVE_TENANT). Required
	// whenever authentication is enabled: the middleware replaces any
	// client-supplied X-Tenant-ID with it.
	TenantID uuid.UUID
	// SessionTTL bounds one login (default 12h).
	SessionTTL time.Duration
	// SecureCookies marks the cookie Secure (set behind TLS).
	SecureCookies bool
	// Now overrides the clock (tests).
	Now func() time.Time
}

// authExempt reports routes reachable without a session: probes, metrics, the
// login/session endpoints and every non-API path (the static app must load so it
// can show the login form).
func authExempt(path string) bool {
	switch path {
	case "/v1/health", "/v1/ready", "/v1/auth/login", "/v1/auth/session", "/v1/auth/logout", "/metrics":
		return true
	}
	return !strings.HasPrefix(path, "/v1/")
}

// authHandler is the cookie/passphrase middleware. It is deliberately small and
// boring: constant-time comparisons, an HMAC-signed cookie, and the tenant
// injection the kernel needs.
type authHandler struct {
	cfg    authConfig
	next   http.Handler
	logger interface {
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
	}
	key []byte
	now func() time.Time
}

func newAuthHandler(cfg authConfig, next http.Handler, logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}) *authHandler {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 12 * time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	sum := sha256.Sum256([]byte("keel-serve-cookie-v1|" + cfg.Password))
	h := &authHandler{cfg: cfg, next: next, logger: logger, key: sum[:], now: cfg.Now}
	if cfg.Password == "" {
		logger.Warn("keel serve authentication is disabled: set KEEL_SERVE_PASSWORD before exposing the port")
	}
	return h
}

func (h *authHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Password == "" {
		// No passphrase: the port is a development surface, so it must not be
		// reachable from another origin. The Host check blocks DNS rebinding (a
		// page on evil.test resolving to 127.0.0.1 still sends its own Host).
		if !loopbackHost(r.Host) {
			writeAuthJSON(w, http.StatusForbidden, map[string]any{
				"error": "authentication is disabled: keel serve only answers loopback hosts",
			})
			return
		}
		if authExempt(r.URL.Path) {
			h.next.ServeHTTP(w, r)
			return
		}
		// The bundled web UI never sends X-Tenant-ID, so a configured tenant is
		// injected here too — otherwise the UI is unusable with auth disabled.
		// Without a configured tenant the header-driven development mode stays.
		if h.cfg.TenantID != uuid.Nil {
			r.Header.Del("X-Tenant-ID")
			r.Header.Set("X-Tenant-ID", h.cfg.TenantID.String())
		}
		h.next.ServeHTTP(w, r)
		return
	}
	if authExempt(r.URL.Path) {
		h.next.ServeHTTP(w, r)
		return
	}
	if !h.authenticated(r) {
		w.Header().Set("WWW-Authenticate", `Cookie realm="keel"`)
		writeAuthJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "authentication required: POST /v1/auth/login with the server passphrase",
		})
		return
	}
	if h.cfg.TenantID == uuid.Nil {
		writeAuthJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "server is not configured: KEEL_SERVE_TENANT must be a tenant UUID",
		})
		return
	}
	// The tenant is the server's, never the client's: drop anything the caller
	// sent and inject the authenticated tenant for the kernel boundary.
	r.Header.Del("X-Tenant-ID")
	r.Header.Set("X-Tenant-ID", h.cfg.TenantID.String())
	h.next.ServeHTTP(w, r)
}

func (h *authHandler) authenticated(r *http.Request) bool {
	if header := strings.TrimSpace(r.Header.Get("Authorization")); header != "" {
		parts := strings.SplitN(header, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
			if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(parts[1])), []byte(h.cfg.Password)) == 1 {
				return true
			}
		}
	}
	if cookie, err := r.Cookie(authCookieName); err == nil {
		return h.verifyCookie(cookie.Value)
	}
	return false
}

// cookiePayload is the signed session content.
type cookiePayload struct {
	Tenant string `json:"tenant"`
	Expiry int64  `json:"expiry"`
}

func (h *authHandler) sign(payload []byte) string {
	mac := hmac.New(sha256.New, h.key)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (h *authHandler) issueCookie(tenant uuid.UUID) string {
	payload, _ := json.Marshal(cookiePayload{Tenant: tenant.String(), Expiry: h.now().Add(h.cfg.SessionTTL).Unix()})
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + h.sign(payload)
}

func (h *authHandler) verifyCookie(value string) bool {
	encoded, signature, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}
	expected := h.sign(payload)
	if subtle.ConstantTimeCompare([]byte(signature), []byte(expected)) != 1 {
		return false
	}
	var content cookiePayload
	if err := json.Unmarshal(payload, &content); err != nil {
		return false
	}
	if h.now().Unix() >= content.Expiry {
		return false
	}
	// A cookie issued for another tenant (server reconfiguration) is invalid.
	return content.Tenant == h.cfg.TenantID.String()
}

// authRoutes registers the login surface. They are mounted on the API server
// (same origin, no CORS).
func (h *authHandler) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/auth/login", h.login)
	mux.HandleFunc("POST /v1/auth/logout", h.logout)
	mux.HandleFunc("GET /v1/auth/session", h.session)
}

type loginRequest struct {
	Password string `json:"password"`
}

func (h *authHandler) login(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Password == "" {
		writeAuthJSON(w, http.StatusOK, map[string]any{
			"authenticated": true,
			"tenantId":      h.cfg.TenantID.String(),
			"mode":          "disabled",
		})
		return
	}
	body := loginRequest{Password: strings.TrimSpace(r.Header.Get("X-Keel-Password"))}
	if r.ContentLength != 0 {
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeAuthJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid login body"})
			return
		}
	}
	if !passwordEqual(strings.TrimSpace(body.Password), h.cfg.Password) {
		// A tiny delay makes an offline guess loop expensive without adding a
		// stateful throttle to the local server.
		time.Sleep(150 * time.Millisecond)
		writeAuthJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid passphrase"})
		return
	}
	if h.cfg.TenantID == uuid.Nil {
		writeAuthJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "server is not configured: KEEL_SERVE_TENANT must be a tenant UUID",
		})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    h.issueCookie(h.cfg.TenantID),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.SecureCookies,
		MaxAge:   int(h.cfg.SessionTTL.Seconds()),
	})
	writeAuthJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"tenantId":      h.cfg.TenantID.String(),
		"expiresIn":     int(h.cfg.SessionTTL.Seconds()),
	})
}

func (h *authHandler) logout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.cfg.SecureCookies,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
	writeAuthJSON(w, http.StatusOK, map[string]any{"authenticated": false})
}

func (h *authHandler) session(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Password == "" {
		writeAuthJSON(w, http.StatusOK, map[string]any{
			"authenticated": true,
			"tenantId":      h.cfg.TenantID.String(),
			"mode":          "disabled",
		})
		return
	}
	if !h.authenticated(r) {
		writeAuthJSON(w, http.StatusOK, map[string]any{"authenticated": false, "mode": "password"})
		return
	}
	writeAuthJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"tenantId":      h.cfg.TenantID.String(),
		"mode":          "password",
	})
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

// passwordEqual compares two passphrases in constant time regardless of their
// lengths (a plain ConstantTimeCompare leaks the expected length through its
// early return).
func passwordEqual(got, want string) bool {
	gotSum := sha256.Sum256([]byte(got))
	wantSum := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(gotSum[:], wantSum[:]) == 1
}

// loopbackHost reports whether the request Host names the local machine. Used
// only when authentication is disabled.
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
