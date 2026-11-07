// MCP OAuth sign-in endpoints. They are the host side of internal/mcp/oauth:
// POST /v1/mcp/servers/{name}/signin starts a sign-in and returns the
// authorization URL, and GET/POST /v1/mcp/callback completes it. Tokens,
// verifiers, and client secrets never appear in responses or logs.

package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/mcp/oauth"
	"github.com/jixoai/keel/internal/store"
)

// MCPOAuth wires the MCP OAuth endpoints. Nil disables them (routes answer
// 503), which is also what a missing KEEL_SECRET_KEY means.
type MCPOAuth struct {
	// Servers resolves one configured MCP server. Returning an error makes the
	// endpoint answer 404; the runtime indexes its loaded mcp.json entries here.
	Servers func(tenantID uuid.UUID, name string) (MCPOAuthServer, error)
	// NewStore builds the tenant/server OAuth store (PostgreSQL-backed). It
	// returns nil when OAuth is not configured (no secret key).
	NewStore func(tenantID uuid.UUID, name string) *oauth.PostgresStore
	// HTTPClient is used for authorization-server requests. Nil uses the SSRF
	// guarded client.
	HTTPClient *http.Client
	// CallbackURL is the public callback route, for example
	// https://app.example/v1/mcp/callback. Empty derives it from the request.
	CallbackURL string
	// PendingTTL bounds completion of a browser step. Zero uses 10 minutes.
	PendingTTL time.Duration
	// ClientMetadataDocuments are the published CIMD documents, when any.
	ClientMetadataDocuments *oauth.ClientMetadataDocuments
	// ClientName is client_name for dynamic client registration.
	ClientName string
	// AllowLoopbackRedirect lets a caller pass a loopback redirect URL, for a
	// local CLI helper talking to this API. Server deployments leave it off.
	AllowLoopbackRedirect bool
	// AdminKey authenticates sign-in start. Empty disables the endpoints: a
	// tenant header is not authentication, and accepting one would let any
	// client store credentials the worker later uses.
	AdminKey string
	// AllowedHosts restricts the Host used to derive the callback URL when
	// CallbackURL is empty. Empty allows loopback hosts only.
	AllowedHosts []string
	// Reconnect rebuilds the process's MCP connection after a sign-in stored a
	// grant, so the server is usable without a restart.
	Reconnect func(ctx context.Context, tenantID uuid.UUID, server string) error
}

// MCPOAuthServer is one configured MCP server visible to the endpoints.
type MCPOAuthServer struct {
	URL      string
	Settings oauth.Settings
}

// signInRequest is the body of POST /v1/mcp/servers/{name}/signin.
type signInRequest struct {
	// RedirectURL overrides the configured callback route. https, or http
	// loopback with AllowLoopbackRedirect.
	RedirectURL string `json:"redirectUrl"`
	// Challenge is the server's bearer challenge that caused the sign-in.
	Challenge *struct {
		Error               string `json:"error"`
		Scope               string `json:"scope"`
		ResourceMetadataURL string `json:"resourceMetadataUrl"`
	} `json:"challenge"`
}

func (s *Server) mcpSignIn(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, 401, errors.New("X-Tenant-ID is required"))
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" || len(name) > 128 {
		writeErr(w, 400, errors.New("invalid MCP server name"))
		return
	}
	config := s.cfg.MCPOAuth
	adminKey := s.mcpAdminKey(config)
	if config == nil || config.Servers == nil || config.NewStore == nil || adminKey == "" {
		writeOAuthConfigError(w)
		return
	}
	if !adminKeyValid(r, adminKey) {
		writeErr(w, http.StatusUnauthorized, errors.New("admin authentication is required to start an MCP OAuth sign-in"))
		return
	}
	if s.cfg.Store == nil {
		writeOAuthConfigError(w)
		return
	}
	exists, err := s.cfg.Store.TenantExists(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !exists {
		writeErr(w, http.StatusNotFound, errors.New("unknown tenant"))
		return
	}
	var body signInRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, 400, err)
		return
	}
	server, err := config.Servers(tenantID, name)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if server.URL == "" {
		writeErr(w, 500, errors.New("MCP server has no url"))
		return
	}
	stateStore := config.NewStore(tenantID, name)
	if stateStore == nil {
		writeOAuthConfigError(w)
		return
	}
	callbackURL, err := s.mcpCallbackURL(r, tenantID, name, body.RedirectURL, config)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	challenge := &oauth.Challenge{}
	if body.Challenge != nil {
		challenge.Error = body.Challenge.Error
		challenge.Scope = body.Challenge.Scope
		challenge.ResourceMetadataURL = body.Challenge.ResourceMetadataURL
	}
	start, err := oauth.StartSignIn(r.Context(), oauth.SignInOptions{
		ServerURL:               server.URL,
		RedirectURL:             callbackURL,
		ClientName:              config.ClientName,
		Settings:                server.Settings,
		ClientMetadataDocuments: config.ClientMetadataDocuments,
		Store:                   stateStore,
		Challenge:               challenge,
		HTTPClient:              config.HTTPClient,
		Pending:                 stateStore,
		PendingTTL:              config.PendingTTL,
	})
	if err != nil {
		writeOAuthFlowError(w, err)
		return
	}
	if start.Authorized {
		writeJSON(w, 200, map[string]any{"kind": "authorized"})
		return
	}
	redirect := start.Redirect
	writeJSON(w, 200, map[string]any{
		"kind":             "redirect",
		"authorizationUrl": redirect.AuthorizationURL,
		"state":            redirect.State,
		"redirectUrl":      redirect.RedirectURL,
		"scope":            redirect.Scope,
		"expiresAt":        redirect.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// mcpCallbackURL is the redirect URI of the browser step: the configured
// callback route (or one derived from the request) with tenant and server
// appended, so the unauthenticated browser redirect can find its pending row.
func (s *Server) mcpCallbackURL(r *http.Request, tenantID uuid.UUID, name, requested string, config *MCPOAuth) (string, error) {
	base := config.CallbackURL
	if requested != "" {
		parsed, err := url.Parse(requested)
		if err != nil || parsed.Host == "" {
			return "", errors.New("redirectUrl must be an absolute URL")
		}
		if parsed.Scheme != "https" {
			if !(config.AllowLoopbackRedirect && parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
				return "", errors.New("redirectUrl must use https (http is only allowed for an explicit loopback helper)")
			}
		}
		base = requested
	}
	if base == "" {
		// Only the transport security and an explicit host allowlist decide the
		// scheme and host; X-Forwarded-Proto is attacker-controlled.
		host := r.Host
		hostname := host
		if colon := strings.LastIndex(host, ":"); colon >= 0 && !strings.HasSuffix(host, "]") {
			hostname = host[:colon]
		}
		if !config.hostAllowed(hostname) {
			return "", fmt.Errorf("host %q is not allowed to derive the MCP OAuth callback URL; set KEEL_MCP_OAUTH_CALLBACK_URL", hostname)
		}
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		base = scheme + "://" + host + "/v1/mcp/callback"
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("the MCP OAuth callback URL is not an http(s) URL")
	}
	query := parsed.Query()
	query.Set("tenant", tenantID.String())
	query.Set("server", name)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// mcpAdminKey preserves the explicit MCPOAuth override used by embedders and
// tests, but falls back to the server's per-request operator key so rotation
// through KEEL_ADMIN_KEY also applies to sign-in.
func (s *Server) mcpAdminKey(config *MCPOAuth) string {
	if config != nil && config.AdminKey != "" {
		return config.AdminKey
	}
	return s.adminKey()
}

func adminKeyValid(r *http.Request, key string) bool {
	provided := r.Header.Get("X-Admin-Key")
	if provided == "" {
		authorization := r.Header.Get("Authorization")
		if strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
			provided = strings.TrimSpace(authorization[len("Bearer "):])
		}
	}
	if provided == "" || len(provided) != len(key) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(key)) == 1
}

func (config *MCPOAuth) hostAllowed(hostname string) bool {
	if isLoopbackHost(hostname) || hostname == "" {
		return true
	}
	for _, allowed := range config.AllowedHosts {
		if strings.EqualFold(allowed, hostname) {
			return true
		}
	}
	return false
}

func isLoopbackHost(host string) bool {
	switch strings.Trim(host, "[]") {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// mcpCallback serves the browser redirect (GET) and an application callback
// (POST). GET carries tenant and server in its query, added by
// mcpCallbackURL; POST takes them from the path/body.
func (s *Server) mcpCallback(w http.ResponseWriter, r *http.Request) {
	config := s.cfg.MCPOAuth
	if config == nil || config.Servers == nil || config.NewStore == nil {
		writeOAuthConfigError(w)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.mcpCallbackGet(w, r, config)
	case http.MethodPost:
		s.mcpCallbackPost(w, r, config)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeErr(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	}
}

func (s *Server) mcpCallbackGet(w http.ResponseWriter, r *http.Request, config *MCPOAuth) {
	query := r.URL.Query()
	tenantID, tenantErr := uuid.Parse(query.Get("tenant"))
	name := strings.TrimSpace(query.Get("server"))
	state := query.Get("state")
	if tenantErr != nil || name == "" || state == "" {
		writeOAuthPage(w, 400, false, "Invalid or expired sign-in", "")
		return
	}
	callback := oauth.Callback{Code: query.Get("code"), State: state, ISS: query.Get("iss")}
	_, err := s.completeMCPCallback(r.Context(), config, tenantID, name, callback, "")
	if err != nil {
		status := 400
		message := "Sign-in failed"
		switch {
		case errors.Is(err, oauth.ErrSignInCancelled):
			message = "Sign-in cancelled"
		case errors.Is(err, oauth.ErrSignInInProgress):
			status = http.StatusConflict
			message = "Sign-in already in progress"
		}
		writeOAuthPage(w, status, false, message, err.Error())
		return
	}
	writeOAuthPage(w, 200, true, "Sign-in complete", "")
}

// mcpCallbackPost completes with a code, or with a redirect URL the user pasted
// (redirectUrl), for a host whose browser cannot reach this callback route.
func (s *Server) mcpCallbackPost(w http.ResponseWriter, r *http.Request, config *MCPOAuth) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, 401, errors.New("X-Tenant-ID is required"))
		return
	}
	var body struct {
		Server      string `json:"server"`
		State       string `json:"state"`
		Code        string `json:"code"`
		ISS         string `json:"iss"`
		RedirectURL string `json:"redirectUrl"`
	}
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	name := strings.TrimSpace(body.Server)
	if name == "" {
		writeErr(w, 400, errors.New("server is required"))
		return
	}
	callback := oauth.Callback{Code: body.Code, State: body.State, ISS: body.ISS}
	if body.RedirectURL != "" {
		state, err := stateFromRedirectURL(body.RedirectURL)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		callback.State = state
	}
	if callback.State == "" {
		writeErr(w, 400, errors.New("state is required"))
		return
	}
	reconnected, err := s.completeMCPCallback(r.Context(), config, tenantID, name, callback, body.RedirectURL)
	if err != nil {
		writeOAuthFlowError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"kind": "authorized", "reconnected": reconnected})
}

func stateFromRedirectURL(input string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(input))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("Expected the full redirect URL from the browser address bar")
	}
	state := parsed.Query().Get("state")
	if state == "" {
		return "", errors.New("The redirect URL does not contain a state")
	}
	return state, nil
}

// completeMCPCallback loads the pending browser step and exchanges the code.
func (s *Server) completeMCPCallback(ctx context.Context, config *MCPOAuth, tenantID uuid.UUID, name string, callback oauth.Callback, pastedRedirectURL string) (bool, error) {
	server, err := config.Servers(tenantID, name)
	if err != nil {
		return false, err
	}
	stateStore := config.NewStore(tenantID, name)
	if stateStore == nil {
		return false, store.ErrSecretKeyMissing
	}
	pending, err := stateStore.Pending(ctx, callback.State)
	if err != nil {
		return false, err
	}
	if pending == nil {
		// A duplicate callback after a successful exchange is an idempotent
		// success if credentials exist; a consumed/superseded state otherwise fails.
		state, loadErr := stateStore.Load(ctx)
		if loadErr != nil {
			return false, loadErr
		}
		if state != nil && state.OAuthState == callback.State {
			if state.Tokens != nil && state.Tokens.AccessToken != "" {
				return s.reconnectAfterSignIn(ctx, config, tenantID, name)
			}
			return false, oauth.ErrSignInInProgress
		}
		return false, errors.New("No pending sign-in for this server and state")
	}
	authorization := oauth.SignInRedirect{
		AuthorizationURL: pending.AuthorizationURL,
		RedirectURL:      pending.RedirectURL,
		State:            pending.State,
		Scope:            pending.Scope,
		ExpiresAt:        pending.ExpiresAt,
	}
	if pastedRedirectURL != "" {
		parsed, err := oauth.ResponseFromRedirectURL(pastedRedirectURL, authorization)
		if err != nil {
			return false, err
		}
		callback = parsed
	}
	if callback.Code == "" {
		return false, errors.New("The authorization response does not contain an authorization code")
	}
	if err := oauth.CompleteSignIn(ctx, oauth.SignInOptions{
		ServerURL:               server.URL,
		RedirectURL:             pending.RedirectURL,
		ClientName:              config.ClientName,
		Settings:                server.Settings,
		ClientMetadataDocuments: config.ClientMetadataDocuments,
		Store:                   stateStore,
		HTTPClient:              config.HTTPClient,
		Pending:                 stateStore,
		PendingTTL:              config.PendingTTL,
	}, authorization, callback); err != nil {
		return false, err
	}
	return s.reconnectAfterSignIn(ctx, config, tenantID, name)
}

// reconnectAfterSignIn rebuilds the process's connection so the new grant takes
// effect without a restart. A missing hook is reported as "not reconnected"; a
// failing hook is an error (the grant is stored but not active yet).
func (s *Server) reconnectAfterSignIn(ctx context.Context, config *MCPOAuth, tenantID uuid.UUID, name string) (bool, error) {
	if config.Reconnect == nil {
		return false, nil
	}
	if err := config.Reconnect(ctx, tenantID, name); err != nil {
		// The grant is stored, but the running host did not pick it up. Surface
		// this as an error instead of a false "authorized" success; a retry is
		// idempotent and can reconnect again.
		return false, fmt.Errorf("mcp sign-in stored but reconnect failed: %w", err)
	}
	return true, nil
}

func writeOAuthConfigError(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": "MCP OAuth is not configured: set KEEL_SECRET_KEY and an MCP server oauth block",
		"code":  "mcp_oauth_not_configured",
	})
}

func writeOAuthFlowError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrSecretKeyMissing):
		writeOAuthConfigError(w)
	case errors.Is(err, oauth.ErrSignInInProgress):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "another callback is completing this sign-in", "code": "sign_in_in_progress"})
	case errors.Is(err, oauth.ErrSignInCancelled), errors.Is(err, context.Canceled):
		writeJSON(w, http.StatusRequestTimeout, map[string]any{"error": "sign-in cancelled"})
	default:
		writeErr(w, http.StatusBadGateway, err)
	}
}

// writeOAuthPage renders the browser page of the callback route. Values from the
// authorization response are escaped.
func writeOAuthPage(w http.ResponseWriter, status int, ok bool, title, details string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	message := html.EscapeString(title)
	extra := ""
	if details != "" {
		extra = "<p><code>" + html.EscapeString(details) + "</code></p>"
	}
	_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>%s</title></head><body><h1>%s</h1>%s</body></html>`, message, message, extra)
}
