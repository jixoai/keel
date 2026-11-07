// MCP OAuth HTTP surface: sign-in start, browser callback, pasted redirect,
// and configuration errors. Skipped without KEEL_TEST_DSN.

package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/mcp/oauth"
	"github.com/jixoai/keel/internal/store"
)

const testAdminKey = "test-admin-key"

type oauthTestAuthorizationServer struct {
	server *httptest.Server
	origin string

	mu            sync.Mutex
	registrations int
	tokens        int
	refreshes     int
	codeExchanges int
	accessToken   string
	challenges    map[string]string
	refreshTokens map[string]bool
}

func startOAuthTestAuthorizationServer(t *testing.T) *oauthTestAuthorizationServer {
	t.Helper()
	as := &oauthTestAuthorizationServer{challenges: map[string]string{}, refreshTokens: map[string]bool{}}
	as.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := "http://" + r.Host
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			writeJSON(w, 200, map[string]any{
				"issuer":                                origin,
				"authorization_endpoint":                origin + "/authorize",
				"token_endpoint":                        origin + "/token",
				"registration_endpoint":                 origin + "/register",
				"response_types_supported":              []string{"code"},
				"code_challenge_methods_supported":      []string{"S256"},
				"token_endpoint_auth_methods_supported": []string{"none"},
			})
		case "/register":
			var metadata map[string]any
			_ = json.Unmarshal([]byte(readAll(r)), &metadata)
			as.mu.Lock()
			as.registrations++
			id := "client-" + uuid.NewString()[:8]
			as.mu.Unlock()
			metadata["client_id"] = id
			writeJSON(w, 201, metadata)
		case "/authorize":
			code := "code-" + uuid.NewString()[:8]
			as.mu.Lock()
			as.challenges[code] = r.URL.Query().Get("code_challenge")
			as.mu.Unlock()
			redirect, _ := url.Parse(r.URL.Query().Get("redirect_uri"))
			query := redirect.Query()
			query.Set("code", code)
			query.Set("state", r.URL.Query().Get("state"))
			redirect.RawQuery = query.Encode()
			w.Header().Set("Location", redirect.String())
			w.WriteHeader(http.StatusFound)
		case "/token":
			params, _ := url.ParseQuery(readAll(r))
			as.mu.Lock()
			defer as.mu.Unlock()
			if params.Get("grant_type") == "refresh_token" {
				as.refreshes++
				if !as.refreshTokens[params.Get("refresh_token")] {
					writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
					return
				}
				delete(as.refreshTokens, params.Get("refresh_token"))
				as.tokens++
				refresh := "refresh-" + uuid.NewString()[:8]
				as.refreshTokens[refresh] = true
				access := as.accessToken
				if access == "" {
					access = "access-" + uuid.NewString()[:8]
				}
				writeJSON(w, 200, map[string]any{
					"access_token": access, "refresh_token": refresh, "token_type": "Bearer",
				})
				return
			}
			digest := sha256.Sum256([]byte(params.Get("code_verifier")))
			if as.challenges[params.Get("code")] != base64.RawURLEncoding.EncodeToString(digest[:]) {
				writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
				return
			}
			delete(as.challenges, params.Get("code"))
			as.codeExchanges++
			as.tokens++
			refresh := "refresh-" + uuid.NewString()[:8]
			as.refreshTokens[refresh] = true
			access := as.accessToken
			if access == "" {
				access = "access-" + uuid.NewString()[:8]
			}
			writeJSON(w, 200, map[string]any{
				"access_token": access, "refresh_token": refresh, "token_type": "Bearer",
			})
		default:
			w.WriteHeader(404)
		}
	}))
	as.origin = as.server.URL
	t.Cleanup(as.server.Close)
	return as
}

func readAll(r *http.Request) string {
	data, _ := io.ReadAll(r.Body)
	return string(data)
}

func newOAuthAPIServer(t *testing.T, as *oauthTestAuthorizationServer) (*httptest.Server, *store.Store, uuid.UUID, string, *store.SecretBox) {
	t.Helper()
	return newOAuthAPIServerWithAdminKey(t, as, nil, testAdminKey)
}

func newOAuthAPIServerWithReconnect(t *testing.T, as *oauthTestAuthorizationServer, reconnect func(context.Context, uuid.UUID, string) error) (*httptest.Server, *store.Store, uuid.UUID, string, *store.SecretBox) {
	t.Helper()
	return newOAuthAPIServerWithAdminKey(t, as, reconnect, testAdminKey)
}

func newOAuthAPIServerWithAdminKey(t *testing.T, as *oauthTestAuthorizationServer, reconnect func(context.Context, uuid.UUID, string) error, adminKey string) (*httptest.Server, *store.Store, uuid.UUID, string, *store.SecretBox) {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "mcp-oauth-api-test"); err != nil {
		t.Fatal(err)
	}
	box, err := store.NewSecretBox(bytes.Repeat([]byte{0x33}, 32))
	if err != nil {
		t.Fatal(err)
	}
	serverName := "demo-" + uuid.NewString()[:8]
	serverURL := as.origin + "/mcp"
	config := &MCPOAuth{
		Servers: func(_ uuid.UUID, name string) (MCPOAuthServer, error) {
			if name != serverName {
				return MCPOAuthServer{}, errors.New("unknown MCP server")
			}
			return MCPOAuthServer{URL: serverURL, Settings: oauth.Settings{}}, nil
		},
		NewStore: func(tenantID uuid.UUID, name string) *oauth.PostgresStore {
			return oauth.NewPostgresStore(st, box, tenantID, name)
		},
		Reconnect:  reconnect,
		ClientName: "keel-test",
		AdminKey:   adminKey,
		// Tests reach the loopback authorization server; production passes the
		// SSRF-guarded client.
		HTTPClient: &http.Client{},
	}
	srv := New(Config{Store: st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MCPOAuth: config})
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	return hs, st, tenant, serverName, box
}

func postJSON(t *testing.T, client *http.Client, target, tenant string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	request, _ := http.NewRequest(http.MethodPost, target, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	if tenant != "" {
		request.Header.Set("X-Tenant-ID", tenant)
		request.Header.Set("X-Admin-Key", testAdminKey)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	var decoded map[string]any
	_ = json.Unmarshal(data, &decoded)
	return response.StatusCode, decoded
}

var noFollowClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func TestMCPOAuthSignInAndCallback(t *testing.T) {
	as := startOAuthTestAuthorizationServer(t)
	hs, st, tenant, serverName, box := newOAuthAPIServer(t, as)
	ctx := context.Background()

	status, signIn := postJSON(t, hs.Client(), hs.URL+"/v1/mcp/servers/"+serverName+"/signin", tenant.String(), map[string]any{})
	if status != 200 || signIn["kind"] != "redirect" {
		t.Fatalf("signin = %d %#v", status, signIn)
	}
	authorizationURL, _ := signIn["authorizationUrl"].(string)
	state, _ := signIn["state"].(string)
	if authorizationURL == "" || state == "" {
		t.Fatalf("signin body = %#v", signIn)
	}
	if _, ok := signIn["expiresAt"].(string); !ok {
		t.Errorf("expiresAt = %#v", signIn["expiresAt"])
	}

	// Follow the authorization redirect like a browser.
	response, err := noFollowClient.Get(authorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	location := response.Header.Get("Location")
	if !strings.Contains(location, "tenant="+tenant.String()) || !strings.Contains(location, "server="+serverName) {
		t.Fatalf("callback location = %q", location)
	}
	callbackResponse, err := hs.Client().Get(location)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(callbackResponse.Body)
	callbackResponse.Body.Close()
	if callbackResponse.StatusCode != 200 || !strings.Contains(string(body), "Sign-in complete") {
		t.Fatalf("callback = %d %s", callbackResponse.StatusCode, body)
	}

	credentials, err := st.LoadMCPOAuthCredentials(ctx, tenant, serverName, box)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(credentials.Payload), `"tokens"`) {
		t.Errorf("payload = %s", credentials.Payload)
	}
	pending, err := st.GetMCPOAuthPending(ctx, tenant, serverName, state)
	if err != nil {
		t.Fatal(err)
	}
	if pending.ConsumedAt == nil {
		t.Error("pending sign-in was not consumed")
	}

	// A later sign-in refreshes the stored grant instead of redirecting.
	status, again := postJSON(t, hs.Client(), hs.URL+"/v1/mcp/servers/"+serverName+"/signin", tenant.String(), map[string]any{})
	if status != 200 || again["kind"] != "authorized" {
		t.Fatalf("second signin = %d %#v", status, again)
	}
	as.mu.Lock()
	refreshes := as.refreshes
	as.mu.Unlock()
	if refreshes != 1 {
		t.Errorf("refreshes = %d", refreshes)
	}
}

func TestMCPOAuthCallbackWithPastedRedirect(t *testing.T) {
	as := startOAuthTestAuthorizationServer(t)
	hs, _, tenant, serverName, _ := newOAuthAPIServer(t, as)

	_, signIn := postJSON(t, hs.Client(), hs.URL+"/v1/mcp/servers/"+serverName+"/signin", tenant.String(), map[string]any{})
	authorizationURL, _ := signIn["authorizationUrl"].(string)
	response, err := noFollowClient.Get(authorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	location := response.Header.Get("Location")

	status, body := postJSON(t, hs.Client(), hs.URL+"/v1/mcp/callback", tenant.String(), map[string]any{
		"server":      serverName,
		"redirectUrl": location,
	})
	if status != 200 || body["kind"] != "authorized" {
		t.Fatalf("callback = %d %#v", status, body)
	}
}

func TestMCPOAuthRejectsInsecureRedirectURL(t *testing.T) {
	as := startOAuthTestAuthorizationServer(t)
	hs, _, tenant, serverName, _ := newOAuthAPIServer(t, as)
	status, body := postJSON(t, hs.Client(), hs.URL+"/v1/mcp/servers/"+serverName+"/signin", tenant.String(), map[string]any{
		"redirectUrl": "http://attacker.example/callback",
	})
	if status != 400 {
		t.Fatalf("status = %d %#v", status, body)
	}
}

func TestMCPOAuthNotConfigured(t *testing.T) {
	hs := httptest.NewServer(New(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}))
	defer hs.Close()
	tenant := uuid.NewString()
	status, body := postJSON(t, hs.Client(), hs.URL+"/v1/mcp/servers/demo/signin", tenant, map[string]any{})
	if status != http.StatusServiceUnavailable || body["code"] != "mcp_oauth_not_configured" {
		t.Fatalf("signin = %d %#v", status, body)
	}
	response, err := hs.Client().Get(hs.URL + "/v1/mcp/callback?tenant=" + tenant + "&server=demo&state=x&code=y")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("callback status = %d", response.StatusCode)
	}
}

func TestMCPOAuthSignInRequiresAdminKey(t *testing.T) {
	as := startOAuthTestAuthorizationServer(t)
	hs, _, tenant, serverName, _ := newOAuthAPIServer(t, as)
	body, _ := json.Marshal(map[string]any{})
	makeRequest := func(adminKey, tenantHeader string) (int, map[string]any) {
		request, _ := http.NewRequest(http.MethodPost, hs.URL+"/v1/mcp/servers/"+serverName+"/signin", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		if tenantHeader != "" {
			request.Header.Set("X-Tenant-ID", tenantHeader)
		}
		if adminKey != "" {
			request.Header.Set("X-Admin-Key", adminKey)
		}
		response, err := hs.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var decoded map[string]any
		raw, _ := io.ReadAll(response.Body)
		_ = json.Unmarshal(raw, &decoded)
		return response.StatusCode, decoded
	}
	if status, _ := makeRequest("", tenant.String()); status != http.StatusUnauthorized {
		t.Errorf("missing key status = %d", status)
	}
	if status, _ := makeRequest("wrong", tenant.String()); status != http.StatusUnauthorized {
		t.Errorf("wrong key status = %d", status)
	}
	if status, _ := makeRequest(testAdminKey, uuid.NewString()); status != http.StatusNotFound {
		t.Errorf("unknown tenant status = %d", status)
	}
	// The zero tenant is never a real tenant row; it must not become a shared bucket.
	if status, _ := makeRequest(testAdminKey, uuid.Nil.String()); status != http.StatusNotFound {
		t.Errorf("nil tenant status = %d", status)
	}
	if status, _ := makeRequest(testAdminKey, tenant.String()); status != 200 {
		t.Errorf("valid sign-in status = %d", status)
	}
}

// signInWithAdminKey starts a sign-in with a specific operator key, bypassing
// the fixed key postJSON adds.
func signInWithAdminKey(t *testing.T, hs *httptest.Server, tenant, serverName, key string) int {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{})
	request, _ := http.NewRequest(http.MethodPost, hs.URL+"/v1/mcp/servers/"+serverName+"/signin", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Tenant-ID", tenant)
	if key != "" {
		request.Header.Set("X-Admin-Key", key)
	}
	response, err := hs.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

// TestMCPOAuthAdminKeyRotatesThroughEnvironment is the P1-10 regression for the
// sign-in endpoint: with no explicit MCPOAuth.AdminKey the operator key is read
// from KEEL_ADMIN_KEY per request, so a rotated key works without a restart.
func TestMCPOAuthAdminKeyRotatesThroughEnvironment(t *testing.T) {
	as := startOAuthTestAuthorizationServer(t)
	hs, _, tenant, serverName, _ := newOAuthAPIServerWithAdminKey(t, as, nil, "")
	t.Setenv("KEEL_ADMIN_KEY", "signin-one")
	if code := signInWithAdminKey(t, hs, tenant.String(), serverName, "signin-one"); code != 200 {
		t.Fatalf("first key = %d, want 200", code)
	}
	t.Setenv("KEEL_ADMIN_KEY", "signin-two")
	if code := signInWithAdminKey(t, hs, tenant.String(), serverName, "signin-one"); code != http.StatusUnauthorized {
		t.Fatalf("rotated-out key = %d, want 401", code)
	}
	if code := signInWithAdminKey(t, hs, tenant.String(), serverName, "signin-two"); code != 200 {
		t.Fatalf("rotated-in key = %d, want 200", code)
	}
}

// TestMCPOAuthConfiguredAdminKeyWinsOverEnvironment keeps the embedded
// override authoritative: an explicit MCPOAuth.AdminKey is not shadowed by the
// environment.
func TestMCPOAuthConfiguredAdminKeyWinsOverEnvironment(t *testing.T) {
	as := startOAuthTestAuthorizationServer(t)
	t.Setenv("KEEL_ADMIN_KEY", "env-key")
	hs, _, tenant, serverName, _ := newOAuthAPIServerWithAdminKey(t, as, nil, testAdminKey)
	if code := signInWithAdminKey(t, hs, tenant.String(), serverName, testAdminKey); code != 200 {
		t.Fatalf("configured key = %d, want 200", code)
	}
	if code := signInWithAdminKey(t, hs, tenant.String(), serverName, "env-key"); code != http.StatusUnauthorized {
		t.Fatalf("environment key overrode MCPOAuth.AdminKey: %d, want 401", code)
	}
}

func TestMCPOAuthDuplicateCallbackIsIdempotent(t *testing.T) {
	as := startOAuthTestAuthorizationServer(t)
	hs, _, tenant, serverName, _ := newOAuthAPIServer(t, as)
	_, signIn := postJSON(t, hs.Client(), hs.URL+"/v1/mcp/servers/"+serverName+"/signin", tenant.String(), map[string]any{})
	authorizationURL, _ := signIn["authorizationUrl"].(string)
	response, err := noFollowClient.Get(authorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	location := response.Header.Get("Location")
	for attempt := 0; attempt < 2; attempt++ {
		callbackResponse, err := hs.Client().Get(location)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(callbackResponse.Body)
		callbackResponse.Body.Close()
		if callbackResponse.StatusCode != 200 || !strings.Contains(string(body), "Sign-in complete") {
			t.Fatalf("attempt %d = %d %s", attempt, callbackResponse.StatusCode, body)
		}
	}
	as.mu.Lock()
	exchanges := as.codeExchanges
	as.mu.Unlock()
	if exchanges != 1 {
		t.Errorf("code exchanges = %d, want 1", exchanges)
	}
}

func TestMCPOAuthConcurrentCallbacksExchangeOnce(t *testing.T) {
	as := startOAuthTestAuthorizationServer(t)
	hs, st, tenant, serverName, box := newOAuthAPIServer(t, as)
	_, signIn := postJSON(t, hs.Client(), hs.URL+"/v1/mcp/servers/"+serverName+"/signin", tenant.String(), map[string]any{})
	authorizationURL, _ := signIn["authorizationUrl"].(string)
	response, err := noFollowClient.Get(authorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	location := response.Header.Get("Location")

	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			callbackResponse, err := hs.Client().Get(location)
			if err != nil {
				codes <- 0
				return
			}
			_, _ = io.Copy(io.Discard, callbackResponse.Body)
			callbackResponse.Body.Close()
			codes <- callbackResponse.StatusCode
		}()
	}
	for i := 0; i < 2; i++ {
		status := <-codes
		// The loser either sees the winner's tokens (200) or still in-flight (409).
		if status != 200 && status != http.StatusConflict {
			t.Errorf("callback status = %d", status)
		}
	}
	as.mu.Lock()
	exchanges := as.codeExchanges
	as.mu.Unlock()
	if exchanges != 1 {
		t.Errorf("code exchanges = %d, want 1", exchanges)
	}
	credentials, err := st.LoadMCPOAuthCredentials(context.Background(), tenant, serverName, box)
	if err != nil || len(credentials.Payload) == 0 {
		t.Errorf("credentials = %#v, %v", credentials, err)
	}
}

func TestMCPOAuthCallbackReconnects(t *testing.T) {
	as := startOAuthTestAuthorizationServer(t)
	var mu sync.Mutex
	var reconnects []string
	var reconnectedTenants []uuid.UUID
	hs, _, tenant, serverName, _ := newOAuthAPIServerWithReconnect(t, as, func(_ context.Context, gotTenant uuid.UUID, server string) error {
		mu.Lock()
		reconnectedTenants = append(reconnectedTenants, gotTenant)
		reconnects = append(reconnects, server)
		mu.Unlock()
		return nil
	})
	_, signIn := postJSON(t, hs.Client(), hs.URL+"/v1/mcp/servers/"+serverName+"/signin", tenant.String(), map[string]any{})
	authorizationURL, _ := signIn["authorizationUrl"].(string)
	response, err := noFollowClient.Get(authorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	location := response.Header.Get("Location")
	callbackResponse, err := hs.Client().Get(location)
	if err != nil {
		t.Fatal(err)
	}
	callbackResponse.Body.Close()
	if callbackResponse.StatusCode != 200 {
		t.Fatalf("callback status = %d", callbackResponse.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reconnects) != 1 || reconnects[0] != serverName {
		t.Errorf("reconnects = %#v", reconnects)
	}
	if len(reconnectedTenants) != 1 || reconnectedTenants[0] != tenant {
		t.Errorf("reconnected tenants = %#v, want %s", reconnectedTenants, tenant)
	}
}

// TestMCPOAuthCallbackCannotWriteAnotherTenantsCredentials is the cross-tenant
// negative case. The state is the capability, but it is only a capability for
// the tenant/server pair that created it: pointing either callback shape at
// tenant B must not store a grant under B, and must leave A's pending step
// completable.
func TestMCPOAuthCallbackCannotWriteAnotherTenantsCredentials(t *testing.T) {
	as := startOAuthTestAuthorizationServer(t)
	hs, st, tenantA, serverName, box := newOAuthAPIServer(t, as)
	ctx := context.Background()
	tenantB := uuid.New()
	if err := st.CreateTenant(ctx, tenantB, "mcp-oauth-other"); err != nil {
		t.Fatal(err)
	}

	_, signIn := postJSON(t, hs.Client(), hs.URL+"/v1/mcp/servers/"+serverName+"/signin", tenantA.String(), map[string]any{})
	authorizationURL, _ := signIn["authorizationUrl"].(string)
	response, err := noFollowClient.Get(authorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	location := response.Header.Get("Location")
	if location == "" {
		t.Fatal("authorization redirect has no Location")
	}

	// Same state, foreign tenant: rewrite only the tenant query parameter.
	evilURL, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	query := evilURL.Query()
	query.Set("tenant", tenantB.String())
	evilURL.RawQuery = query.Encode()
	foreign, err := hs.Client().Get(evilURL.String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, foreign.Body)
	foreign.Body.Close()
	if foreign.StatusCode == 200 {
		t.Fatalf("cross-tenant GET callback = %d, want a rejection", foreign.StatusCode)
	}
	if _, err := st.LoadMCPOAuthCredentials(ctx, tenantB, serverName, box); !errors.Is(err, store.ErrMCPOAuthNotFound) {
		t.Fatalf("tenant B credentials exist after a cross-tenant GET callback: %v", err)
	}

	// The application callback path (POST) with A's state and B's header.
	status, body := postJSON(t, hs.Client(), hs.URL+"/v1/mcp/callback", tenantB.String(), map[string]any{
		"server":      serverName,
		"redirectUrl": location,
	})
	if status == 200 {
		t.Fatalf("cross-tenant POST callback = %d %#v, want a rejection", status, body)
	}
	if _, err := st.LoadMCPOAuthCredentials(ctx, tenantB, serverName, box); !errors.Is(err, store.ErrMCPOAuthNotFound) {
		t.Fatalf("tenant B credentials exist after a cross-tenant POST callback: %v", err)
	}

	// Neither attempt consumed A's pending step: the legitimate callback still
	// exchanges the code exactly once and stores the grant under A.
	good, err := hs.Client().Get(location)
	if err != nil {
		t.Fatal(err)
	}
	goodBody, _ := io.ReadAll(good.Body)
	good.Body.Close()
	if good.StatusCode != 200 || !strings.Contains(string(goodBody), "Sign-in complete") {
		t.Fatalf("legitimate callback after foreign attempts = %d %s", good.StatusCode, goodBody)
	}
	credentials, err := st.LoadMCPOAuthCredentials(ctx, tenantA, serverName, box)
	if err != nil || len(credentials.Payload) == 0 {
		t.Fatalf("tenant A credentials = %#v, %v", credentials, err)
	}
	if _, err := st.LoadMCPOAuthCredentials(ctx, tenantB, serverName, box); !errors.Is(err, store.ErrMCPOAuthNotFound) {
		t.Fatalf("tenant B credentials exist after the legitimate callback: %v", err)
	}
	as.mu.Lock()
	exchanges := as.codeExchanges
	as.mu.Unlock()
	if exchanges != 1 {
		t.Errorf("code exchanges = %d, want 1", exchanges)
	}
}

func TestMCPOAuthCallbackURLDerivationIsNotForwardedHeaderTrusted(t *testing.T) {
	as := startOAuthTestAuthorizationServer(t)
	hs, st, tenant, serverName, box := newOAuthAPIServer(t, as)
	config := &MCPOAuth{
		Servers: func(_ uuid.UUID, name string) (MCPOAuthServer, error) {
			return MCPOAuthServer{URL: as.origin + "/mcp", Settings: oauth.Settings{}}, nil
		},
		NewStore: func(tenantID uuid.UUID, name string) *oauth.PostgresStore {
			return oauth.NewPostgresStore(st, box, tenantID, name)
		},
		AdminKey:   testAdminKey,
		HTTPClient: &http.Client{},
	}
	srv := New(Config{Store: st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MCPOAuth: config})
	apiServer := httptest.NewServer(srv)
	defer apiServer.Close()

	signInAt := func(host string) (int, map[string]any) {
		body, _ := json.Marshal(map[string]any{})
		request, _ := http.NewRequest(http.MethodPost, apiServer.URL+"/v1/mcp/servers/"+serverName+"/signin", bytes.NewReader(body))
		request.Host = host
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Tenant-ID", tenant.String())
		request.Header.Set("X-Admin-Key", testAdminKey)
		request.Header.Set("X-Forwarded-Proto", "https")
		response, err := apiServer.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var decoded map[string]any
		raw, _ := io.ReadAll(response.Body)
		_ = json.Unmarshal(raw, &decoded)
		return response.StatusCode, decoded
	}
	if status, _ := signInAt("evil.example"); status != 400 {
		t.Errorf("unlisted host status = %d", status)
	}
	config.AllowedHosts = []string{"app.example"}
	if status, body := signInAt("app.example"); status != 200 {
		t.Fatalf("allowed host status = %d %#v", status, body)
	} else if redirect, _ := body["redirectUrl"].(string); !strings.HasPrefix(redirect, "http://app.example/v1/mcp/callback") {
		t.Errorf("redirectUrl = %q, want http scheme (X-Forwarded-Proto ignored)", redirect)
	}
	_ = hs
}
