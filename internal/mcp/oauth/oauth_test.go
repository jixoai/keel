// Ported from PI, packages/mcp/test/oauth.test.ts (MIT License). Test titles
// and expectations follow the upstream cases.

package oauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/mcp"
	mcpoauth "github.com/jixoai/keel/internal/mcp/oauth"
)

func TestOAuthDiscoversRegistersAuthorizesAndRefreshesOn401(t *testing.T) {
	var mu sync.Mutex
	var expectedChallenge string
	refreshes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp":
			writeJSON(w, 200, map[string]any{
				"resource":              originOf(r) + "/mcp",
				"authorization_servers": []string{originOf(r)},
				"scopes_supported":      []string{"org:read"},
			})
		case "/.well-known/oauth-authorization-server":
			writeJSON(w, 200, map[string]any{
				"issuer":                                originOf(r),
				"authorization_endpoint":                originOf(r) + "/authorize",
				"token_endpoint":                        originOf(r) + "/token",
				"registration_endpoint":                 originOf(r) + "/register",
				"response_types_supported":              []string{"code"},
				"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
				"token_endpoint_auth_methods_supported": []string{"none"},
				"code_challenge_methods_supported":      []string{"S256"},
			})
		case "/register":
			var metadata map[string]any
			_ = json.Unmarshal([]byte(readBody(r)), &metadata)
			metadata["client_id"] = "test-client"
			metadata["client_secret"] = ""
			writeJSON(w, 201, metadata)
		case "/authorize":
			mu.Lock()
			expectedChallenge = r.URL.Query().Get("code_challenge")
			mu.Unlock()
			redirect, _ := url.Parse(r.URL.Query().Get("redirect_uri"))
			query := redirect.Query()
			query.Set("code", "test-code")
			query.Set("state", r.URL.Query().Get("state"))
			redirect.RawQuery = query.Encode()
			w.Header().Set("Location", redirect.String())
			w.WriteHeader(http.StatusFound)
		case "/token":
			params, _ := url.ParseQuery(readBody(r))
			if params.Get("grant_type") == "refresh_token" {
				mu.Lock()
				refreshes++
				mu.Unlock()
				writeJSON(w, 200, map[string]any{
					"access_token":  "refreshed-token",
					"token_type":    "Bearer",
					"refresh_token": "",
					"expires_in":    nil,
				})
				return
			}
			digest := sha256.Sum256([]byte(params.Get("code_verifier")))
			challenge := base64.RawURLEncoding.EncodeToString(digest[:])
			mu.Lock()
			expected := expectedChallenge
			mu.Unlock()
			if params.Get("code") != "test-code" || challenge != expected {
				writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
				return
			}
			writeJSON(w, 200, map[string]any{
				"access_token":  "first-token",
				"refresh_token": "refresh-token",
				"token_type":    "Bearer",
				"scope":         "",
			})
		case "/mcp":
			if r.Method == http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if r.Method == http.MethodDelete {
				w.WriteHeader(200)
				return
			}
			authorization := r.Header.Get("Authorization")
			if authorization != "Bearer first-token" && authorization != "Bearer refreshed-token" {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("www-authenticate", `Bearer resource_metadata="`+originOf(r)+`/.well-known/oauth-protected-resource/mcp", scope=""`)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte("Unauthorized"))
				return
			}
			var message map[string]any
			_ = json.Unmarshal([]byte(readBody(r)), &message)
			if _, ok := message["id"]; !ok {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			var result any
			if message["method"] == "initialize" {
				result = map[string]any{
					"protocolVersion": "2025-06-18",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "oauth-test", "version": "1.0.0"},
				}
			} else {
				result = map[string]any{"tools": []any{map[string]any{
					"name": "issues", "inputSchema": map[string]any{"type": "object"},
				}}}
			}
			writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": message["id"], "result": result})
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	origin := server.URL

	callback, err := mcpoauth.ListenCallbackServer(mcpoauth.CallbackServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer callback.Close()
	provider := newTestOAuthProvider(callback.RedirectURL())
	authProvider := transportAuthAdapter{mcpoauth.AdaptOAuthProviderWithClient(provider.ClientProvider(), testHTTPClient)}

	connect := func(tokenHeader string) error {
		client := mcp.NewClient(mcp.ClientOptions{Name: "oauth-test", Version: "1.0.0"})
		t.Cleanup(func() { _ = client.Close() })
		_, err := client.Connect(context.Background(), mcp.NewHTTPTransport(mcp.HTTPTransportOptions{
			URL:              origin + "/mcp",
			Headers:          map[string]string{"Authorization": "Bearer " + tokenHeader},
			AuthProvider:     authProvider,
			DisableGetStream: true,
		}))
		return err
	}

	err = connect("caller-supplied-stale-token")
	var required *mcpoauth.AuthorizationRequiredError
	if !errors.As(err, &required) {
		t.Fatalf("connect error = %v, want AuthorizationRequiredError", err)
	}
	authorizationURL := provider.authorizationURL()
	if authorizationURL == nil {
		t.Fatal("no authorization URL")
	}
	if got := authorizationURL.Query().Get("scope"); got != "org:read" {
		t.Errorf("scope = %q", got)
	}
	if got := authorizationURL.Query().Get("resource"); got != origin+"/mcp" {
		t.Errorf("resource = %q", got)
	}

	future, err := callback.WaitForCallback("expected-state", "")
	if err != nil {
		t.Fatal(err)
	}
	location := authorizeInBrowser(t, authorizationURL.String())
	response, err := testHTTPClient.Get(location)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	callbackResult, err := future.Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result, err := mcpoauth.AuthorizeMCP(context.Background(), provider.ClientProvider(), mcpoauth.FlowOptions{
		ServerURL:         origin + "/mcp",
		AuthorizationCode: callbackResult.Code,
		HTTPClient:        testHTTPClient,
	})
	if err != nil || result != mcpoauth.FlowAuthorized {
		t.Fatalf("authorize = %q, %v", result, err)
	}

	client := mcp.NewClient(mcp.ClientOptions{Name: "oauth-test", Version: "1.0.0"})
	if _, err := client.Connect(context.Background(), mcp.NewHTTPTransport(mcp.HTTPTransportOptions{
		URL:              origin + "/mcp",
		Headers:          map[string]string{"Authorization": "Bearer caller-supplied-stale-token"},
		AuthProvider:     authProvider,
		DisableGetStream: true,
	})); err != nil {
		t.Fatal(err)
	}
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "issues" {
		t.Errorf("tools = %#v", tools)
	}
	_ = client.Close()

	stale := *provider.tokens()
	stale.AccessToken = "stale-token"
	provider.setTokens(stale)
	refreshedClient := mcp.NewClient(mcp.ClientOptions{Name: "oauth-test", Version: "1.0.0"})
	if _, err := refreshedClient.Connect(context.Background(), mcp.NewHTTPTransport(mcp.HTTPTransportOptions{
		URL:              origin + "/mcp",
		Headers:          map[string]string{"Authorization": "Bearer caller-supplied-stale-token"},
		AuthProvider:     authProvider,
		DisableGetStream: true,
	})); err != nil {
		t.Fatal(err)
	}
	// Neither token response names a scope, so the grant has the requested scope.
	tokens := provider.tokens()
	if tokens == nil || tokens.AccessToken != "refreshed-token" || tokens.RefreshToken != "refresh-token" || tokens.TokenType != "Bearer" || tokens.Scope != "org:read" {
		t.Errorf("tokens = %#v", tokens)
	}
	mu.Lock()
	gotRefreshes := refreshes
	mu.Unlock()
	if gotRefreshes != 1 {
		t.Errorf("refreshes = %d, want 1", gotRefreshes)
	}
	_ = refreshedClient.Close()
}

func TestOAuthSharesOneRefreshBetweenConcurrent401s(t *testing.T) {
	var mu sync.Mutex
	grants := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp":
			// Invalid resource metadata falls back to the server origin.
			writeJSON(w, 200, map[string]any{
				"resource":              originOf(r) + "/mcp",
				"authorization_servers": []string{"not a url"},
			})
		case "/.well-known/oauth-authorization-server":
			writeJSON(w, 200, map[string]any{
				"issuer":                   originOf(r),
				"authorization_endpoint":   originOf(r) + "/authorize",
				"token_endpoint":           originOf(r) + "/token",
				"response_types_supported": []string{"code"},
			})
		case "/token":
			params, _ := url.ParseQuery(readBody(r))
			refreshToken := params.Get("refresh_token")
			mu.Lock()
			grants = append(grants, refreshToken)
			mu.Unlock()
			if refreshToken != "r1" {
				writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
				return
			}
			time.Sleep(20 * time.Millisecond)
			writeJSON(w, 200, map[string]any{
				"access_token": "a2", "refresh_token": "r2", "token_type": "Bearer", "expires_in": 3600,
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	origin := server.URL

	store := mcpoauth.NewMemoryStateStore()
	provider := mcpoauth.NewProvider(mcpoauth.ProviderOptions{
		ServerURL:      origin + "/mcp",
		RedirectURL:    "http://127.0.0.1/callback",
		ClientMetadata: mcpoauth.ClientMetadata{ClientName: "test"},
		ClientID:       "client",
		Store:          store,
	})
	if err := provider.SaveTokens(context.Background(), mcpoauth.Tokens{
		AccessToken: "a1", RefreshToken: "r1", TokenType: "Bearer",
	}); err != nil {
		t.Fatal(err)
	}
	auth := mcpoauth.AdaptOAuthProviderWithClient(provider.ClientProvider(), testHTTPClient)
	unauthorized := func() mcpoauth.UnauthorizedEvent {
		return mcpoauth.UnauthorizedEvent{Status: 401, WWWAuthenticate: "Bearer", ServerURL: origin + "/mcp", Token: "a1"}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- auth.OnUnauthorized(context.Background(), unauthorized())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("onUnauthorized: %v", err)
		}
	}
	// A late 401 for a request that still carried the old token must not refresh.
	if err := auth.OnUnauthorized(context.Background(), unauthorized()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotGrants := append([]string(nil), grants...)
	mu.Unlock()
	if len(gotGrants) != 1 || gotGrants[0] != "r1" {
		t.Errorf("grants = %#v", gotGrants)
	}
	token, err := auth.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "a2" {
		t.Errorf("token = %q", token)
	}
	state, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Tokens.RefreshToken != "r2" {
		t.Errorf("refresh token = %q", state.Tokens.RefreshToken)
	}
	if state.TokensExpireAt <= time.Now().UnixMilli()+3_500_000 {
		t.Errorf("tokensExpireAt = %d, want > now+3500s", state.TokensExpireAt)
	}
}

func TestOAuthAsksForAuthorizationOnInsufficientScope(t *testing.T) {
	provider := newTestOAuthProvider("http://127.0.0.1/callback")
	provider.setClient(mcpoauth.ClientInformation{ClientID: "client"})
	provider.setTokens(mcpoauth.Tokens{AccessToken: "a1", RefreshToken: "r1", TokenType: "Bearer", Scope: "repo read:org"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-authorization-server" {
			writeJSON(w, 200, map[string]any{
				"issuer":                   originOf(r),
				"authorization_endpoint":   originOf(r) + "/authorize",
				"token_endpoint":           originOf(r) + "/token",
				"response_types_supported": []string{"code"},
			})
			return
		}
		if r.URL.Path == "/token" {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(404)
	}))
	defer server.Close()
	auth := mcpoauth.AdaptOAuthProviderWithClient(provider.ClientProvider(), testHTTPClient)
	err := auth.OnUnauthorized(context.Background(), mcpoauth.UnauthorizedEvent{
		Status:          403,
		WWWAuthenticate: `Bearer error="insufficient_scope", scope="repo admin"`,
		ServerURL:       server.URL + "/mcp",
		Token:           "a1",
	})
	var required *mcpoauth.AuthorizationRequiredError
	if !errors.As(err, &required) {
		t.Fatalf("error = %v, want AuthorizationRequiredError", err)
	}
	// The challenge may list only the missing scopes; the new grant keeps the old ones too.
	if got := provider.authorizationURL().Query().Get("scope"); got != "repo read:org admin" {
		t.Errorf("scope = %q", got)
	}
	// The working grant is kept until the user authorizes the new scope.
	if got := provider.tokens().AccessToken; got != "a1" {
		t.Errorf("access token = %q", got)
	}
}

func TestOAuthBindsCredentialsToExactServerURL(t *testing.T) {
	store := mcpoauth.NewMemoryStateStore()
	first := mcpoauth.NewProvider(mcpoauth.ProviderOptions{
		ServerURL:      "https://one.example/mcp",
		RedirectURL:    "http://127.0.0.1/callback",
		ClientMetadata: mcpoauth.ClientMetadata{ClientName: "test"},
		Store:          store,
	})
	if err := first.SaveTokens(context.Background(), mcpoauth.Tokens{AccessToken: "secret", TokenType: "Bearer"}); err != nil {
		t.Fatal(err)
	}
	tokens, err := first.Tokens(context.Background())
	if err != nil || tokens == nil || tokens.AccessToken != "secret" {
		t.Fatalf("tokens = %#v, %v", tokens, err)
	}
	second := mcpoauth.NewProvider(mcpoauth.ProviderOptions{
		ServerURL:      "https://two.example/mcp",
		RedirectURL:    "http://127.0.0.1/callback",
		ClientMetadata: mcpoauth.ClientMetadata{ClientName: "test"},
		Store:          store,
	})
	tokens, err = second.Tokens(context.Background())
	if err != nil || tokens != nil {
		t.Fatalf("second tokens = %#v, %v", tokens, err)
	}
}

func TestOAuthRegistrationApplicationType(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var metadata map[string]any
		_ = json.Unmarshal([]byte(readBody(r)), &metadata)
		mu.Lock()
		bodies = append(bodies, metadata)
		mu.Unlock()
		metadata["client_id"] = "client"
		writeJSON(w, 201, metadata)
	}))
	defer server.Close()
	register := func(redirects []string, applicationType string) {
		metadata := mcpoauth.ClientMetadata{RedirectURIs: redirects}
		if applicationType != "" {
			metadata.ApplicationType = applicationType
		}
		if _, err := mcpoauth.RegisterClient(context.Background(), server.URL, nil, metadata, "", testHTTPClient); err != nil {
			t.Fatal(err)
		}
	}
	register([]string{"http://127.0.0.1:1234/callback"}, "")
	register([]string{"http://[::1]/callback"}, "")
	register([]string{"com.example.app:/callback"}, "")
	register([]string{"https://app.example/callback"}, "")
	register([]string{"http://localhost/callback"}, "web")
	mu.Lock()
	defer mu.Unlock()
	want := []string{"native", "native", "native", "web", "web"}
	for i, body := range bodies {
		if body["application_type"] != want[i] {
			t.Errorf("body %d application_type = %v, want %s", i, body["application_type"], want[i])
		}
	}
}

func TestOAuthRejectsIssuerMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-authorization-server" {
			writeJSON(w, 200, map[string]any{
				"issuer":                   "https://attacker.example",
				"authorization_endpoint":   originOf(r) + "/authorize",
				"token_endpoint":           originOf(r) + "/token",
				"response_types_supported": []string{"code"},
			})
			return
		}
		w.WriteHeader(404)
	}))
	defer server.Close()
	_, err := mcpoauth.DiscoverAuthorizationServerMetadata(context.Background(), server.URL, mcpoauth.DiscoveryOptions{HTTPClient: testHTTPClient})
	var mismatch *mcpoauth.IssuerMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("error = %v, want IssuerMismatchError", err)
	}
}

func TestOAuthRejectsNonHTTPAuthorizationEndpoints(t *testing.T) {
	endpoints := []string{"file://attacker.example/share/x.exe", "search-ms:query=x", "ms-msdt:/id"}
	endpoint := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"issuer":                   originOf(r),
			"authorization_endpoint":   endpoint,
			"token_endpoint":           originOf(r) + "/token",
			"response_types_supported": []string{"code"},
		})
	}))
	defer server.Close()
	for _, value := range endpoints {
		endpoint = value
		_, err := mcpoauth.DiscoverAuthorizationServerMetadata(context.Background(), server.URL, mcpoauth.DiscoveryOptions{HTTPClient: testHTTPClient})
		if err == nil || !strings.Contains(err.Error(), "Invalid authorization endpoint") {
			t.Errorf("discover %q error = %v", value, err)
		}
		metadata := &mcpoauth.AuthorizationServerMetadata{
			Issuer:                 server.URL,
			AuthorizationEndpoint:  value,
			TokenEndpoint:          server.URL + "/token",
			ResponseTypesSupported: []string{"code"},
		}
		_, _, err = mcpoauth.StartAuthorization(server.URL, metadata, mcpoauth.ClientInformation{ClientID: "client"}, "http://127.0.0.1/callback", "", "", "")
		if err == nil || !strings.Contains(err.Error(), "Invalid authorization endpoint") {
			t.Errorf("start %q error = %v", value, err)
		}
	}
}

func TestOAuthUsesConfiguredAuthorizationServerMetadataDocument(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp":
			writeJSON(w, 200, map[string]any{
				"resource":              originOf(r) + "/mcp",
				"authorization_servers": []string{originOf(r)},
			})
		case "/idp/metadata.json":
			writeJSON(w, 200, map[string]any{
				"issuer":                   "https://idp.example",
				"authorization_endpoint":   originOf(r) + "/idp/authorize",
				"token_endpoint":           originOf(r) + "/idp/token",
				"response_types_supported": []string{"code"},
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	provider := newTestOAuthProvider("http://127.0.0.1/callback")
	provider.setClient(mcpoauth.ClientInformation{ClientID: "client"})
	result, err := mcpoauth.AuthorizeMCP(context.Background(), provider.ClientProvider(), mcpoauth.FlowOptions{
		ServerURL:                      server.URL + "/mcp",
		AuthorizationServerMetadataURL: server.URL + "/idp/metadata.json",
		HTTPClient:                     testHTTPClient,
	})
	if err != nil || result != mcpoauth.FlowRedirect {
		t.Fatalf("authorize = %q, %v", result, err)
	}
	authorizationURL := provider.authorizationURL()
	if got := authorizationURL.Scheme + "://" + authorizationURL.Host + authorizationURL.Path; got != server.URL+"/idp/authorize" {
		t.Errorf("authorization endpoint = %s", got)
	}
	if got := authorizationURL.Query().Get("resource"); got != server.URL+"/mcp" {
		t.Errorf("resource = %q", got)
	}
	_, err = mcpoauth.AuthorizeMCP(context.Background(), provider.ClientProvider(), mcpoauth.FlowOptions{
		ServerURL:                      server.URL + "/mcp",
		AuthorizationServerMetadataURL: "http://idp.example/metadata.json",
		HTTPClient:                     testHTTPClient,
	})
	var insecure *mcpoauth.InsecureEndpointError
	if !errors.As(err, &insecure) {
		t.Fatalf("insecure error = %v", err)
	}
}

func TestOAuthExchangesCodeOnlyWhenIssParameterMatches(t *testing.T) {
	var mu sync.Mutex
	var codes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(404)
			return
		}
		params, _ := url.ParseQuery(readBody(r))
		mu.Lock()
		codes = append(codes, params.Get("code"))
		mu.Unlock()
		writeJSON(w, 200, map[string]any{"access_token": "token", "token_type": "Bearer"})
	}))
	defer server.Close()
	origin := server.URL
	exchange := func(code string, iss *string, issParameter bool, metadata bool) (mcpoauth.FlowResult, error) {
		provider := newTestOAuthProvider("http://127.0.0.1/callback")
		provider.setClient(mcpoauth.ClientInformation{ClientID: "client"})
		provider.setVerifier("verifier")
		discovery := mcpoauth.DiscoveryState{AuthorizationServerURL: origin}
		if metadata {
			discovery.AuthorizationServerMetadata = &mcpoauth.AuthorizationServerMetadata{
				Issuer:                            origin,
				AuthorizationEndpoint:             origin + "/authorize",
				TokenEndpoint:                     origin + "/token",
				ResponseTypesSupported:            []string{"code"},
				AuthorizationResponseIssParameter: issParameter,
			}
		}
		provider.setDiscovery(discovery)
		return mcpoauth.AuthorizeMCP(context.Background(), provider.ClientProvider(), mcpoauth.FlowOptions{
			ServerURL:         origin + "/mcp",
			AuthorizationCode: code,
			ISS:               iss,
			HTTPClient:        testHTTPClient,
		})
	}
	attacker := "https://attacker.example"
	if _, err := exchange("other", &attacker, false, true); !errors.As(err, new(*mcpoauth.IssuerMismatchError)) {
		t.Errorf("iss from another server: err = %v", err)
	}
	if _, err := exchange("missing", nil, true, true); !errors.As(err, new(*mcpoauth.IssuerMismatchError)) {
		t.Errorf("missing iss: err = %v", err)
	}
	if result, err := exchange("matching", &origin, true, true); err != nil || result != mcpoauth.FlowAuthorized {
		t.Errorf("matching iss = %q, %v", result, err)
	}
	// Servers that do not promise the parameter may omit it.
	if result, err := exchange("omitted", nil, false, true); err != nil || result != mcpoauth.FlowAuthorized {
		t.Errorf("omitted iss = %q, %v", result, err)
	}
	// Without metadata there is no issuer to compare with, so a server cannot skip
	// the check by hiding it.
	if _, err := exchange("unverifiable", &attacker, false, false); !errors.As(err, new(*mcpoauth.IssuerMismatchError)) {
		t.Errorf("unverifiable iss: err = %v", err)
	}
	if result, err := exchange("no-metadata", nil, false, false); err != nil || result != mcpoauth.FlowAuthorized {
		t.Errorf("no metadata iss = %q, %v", result, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(codes) != 3 || codes[0] != "matching" || codes[1] != "omitted" || codes[2] != "no-metadata" {
		t.Errorf("codes = %#v", codes)
	}
}

func TestOAuthStopsWhenContextIsCancelled(t *testing.T) {
	var mu sync.Mutex
	var stalled []string
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		stalled = append(stalled, r.URL.Path)
		mu.Unlock()
		// Accept and never answer until the test ends.
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	run := func(provider *testOAuthProvider) (string, error) {
		ctx, cancel := context.WithCancel(context.Background())
		mu.Lock()
		count := len(stalled)
		mu.Unlock()
		done := make(chan error, 1)
		go func() {
			_, err := mcpoauth.AuthorizeMCP(ctx, provider.ClientProvider(), mcpoauth.FlowOptions{
				ServerURL:  server.URL + "/mcp",
				HTTPClient: testHTTPClient,
			})
			done <- err
		}()
		deadline := time.Now().Add(2 * time.Second)
		for {
			mu.Lock()
			current := len(stalled)
			mu.Unlock()
			if current > count || time.Now().After(deadline) {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		err := <-done
		if provider.authorizationURL() != nil {
			t.Errorf("authorization URL set after cancellation")
		}
		mu.Lock()
		defer mu.Unlock()
		if len(stalled) == 0 {
			return "", err
		}
		return stalled[len(stalled)-1], err
	}

	// Discovery.
	path, err := run(newTestOAuthProvider("http://127.0.0.1/callback"))
	if path != "/.well-known/oauth-protected-resource/mcp" {
		t.Errorf("stalled path = %q", path)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("discovery cancellation error = %v", err)
	}

	// A failed refresh otherwise falls back to a new authorization.
	refreshing := newTestOAuthProvider("http://127.0.0.1/callback")
	refreshing.setClient(mcpoauth.ClientInformation{ClientID: "client"})
	refreshing.setTokens(mcpoauth.Tokens{AccessToken: "a1", RefreshToken: "r1", TokenType: "Bearer"})
	refreshing.setDiscovery(mcpoauth.DiscoveryState{
		AuthorizationServerURL: server.URL,
		AuthorizationServerMetadata: &mcpoauth.AuthorizationServerMetadata{
			Issuer:                 server.URL,
			AuthorizationEndpoint:  server.URL + "/authorize",
			TokenEndpoint:          server.URL + "/token",
			ResponseTypesSupported: []string{"code"},
		},
	})
	path, _ = run(refreshing)
	if path != "/token" {
		t.Errorf("stalled refresh path = %q", path)
	}
}

func TestOAuthCallbackServerPages(t *testing.T) {
	t.Run("renders plain text by default", func(t *testing.T) {
		callback, err := mcpoauth.ListenCallbackServer(mcpoauth.CallbackServerOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer callback.Close()
		future, err := callback.WaitForCallback("s1", "")
		if err != nil {
			t.Fatal(err)
		}
		response, err := testHTTPClient.Get(callback.RedirectURL() + "?code=abc&state=s1")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if got := response.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
			t.Errorf("content type = %q", got)
		}
		body, _ := io.ReadAll(response.Body)
		if string(body) != "Authorization complete. You may close this window." {
			t.Errorf("body = %q", body)
		}
		result, err := future.Await(context.Background())
		if err != nil || result.Code != "abc" {
			t.Errorf("callback = %#v, %v", result, err)
		}
	})

	t.Run("rejects a response on another path than the expected one", func(t *testing.T) {
		callback, err := mcpoauth.ListenCallbackServer(mcpoauth.CallbackServerOptions{ExtraPaths: []string{"/callback/server-id"}})
		if err != nil {
			t.Fatal(err)
		}
		defer callback.Close()
		origin := strings.TrimSuffix(callback.RedirectURL(), "/callback")
		mixedUp, err := callback.WaitForCallback("s1", "/callback/server-id")
		if err != nil {
			t.Fatal(err)
		}
		wrong, err := testHTTPClient.Get(origin + "/callback?code=abc&state=s1")
		if err != nil {
			t.Fatal(err)
		}
		wrong.Body.Close()
		if wrong.StatusCode != 400 {
			t.Errorf("wrong path status = %d", wrong.StatusCode)
		}
		if _, err := mixedUp.Await(context.Background()); err == nil || !strings.Contains(err.Error(), "arrived on another redirect URI") {
			t.Errorf("mixed-up error = %v", err)
		}
		pending, err := callback.WaitForCallback("s2", "/callback/server-id")
		if err != nil {
			t.Fatal(err)
		}
		right, err := testHTTPClient.Get(origin + "/callback/server-id?code=abc&state=s2")
		if err != nil {
			t.Fatal(err)
		}
		right.Body.Close()
		if right.StatusCode != 200 {
			t.Errorf("right path status = %d", right.StatusCode)
		}
		result, err := pending.Await(context.Background())
		if err != nil || result.Code != "abc" {
			t.Errorf("callback = %#v, %v", result, err)
		}
	})

	t.Run("renders pages through RenderPage", func(t *testing.T) {
		var mu sync.Mutex
		var pages []mcpoauth.CallbackPage
		callback, err := mcpoauth.ListenCallbackServer(mcpoauth.CallbackServerOptions{
			RenderPage: func(page mcpoauth.CallbackPage) string {
				mu.Lock()
				pages = append(pages, page)
				mu.Unlock()
				if page.OK {
					return "<p>ok</p>"
				}
				return "<p>" + page.Message + "</p>"
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer callback.Close()
		denied, err := callback.WaitForCallback("s1", "")
		if err != nil {
			t.Fatal(err)
		}
		failure, err := testHTTPClient.Get(callback.RedirectURL() + "?error=access_denied&error_description=Denied&state=s1")
		if err != nil {
			t.Fatal(err)
		}
		failure.Body.Close()
		if got := failure.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Errorf("content type = %q", got)
		}
		if _, err := denied.Await(context.Background()); err == nil || !strings.Contains(err.Error(), "Denied") {
			t.Errorf("denied error = %v", err)
		}
		mu.Lock()
		last := pages[len(pages)-1]
		mu.Unlock()
		if last.OK || last.Message != "Authorization failed. You may close this window." || last.Details != "Denied" {
			t.Errorf("page = %#v", last)
		}
		pending, err := callback.WaitForCallback("s2", "")
		if err != nil {
			t.Fatal(err)
		}
		success, err := testHTTPClient.Get(callback.RedirectURL() + "?code=abc&state=s2")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(success.Body)
		success.Body.Close()
		if string(body) != "<p>ok</p>" {
			t.Errorf("body = %q", body)
		}
		if _, err := pending.Await(context.Background()); err != nil {
			t.Errorf("pending = %v", err)
		}
	})
}

// originOf reconstructs the test server origin from a request.
func originOf(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// TestOAuthCodeExchangeFailureKeepsStoredTokens is the token-clearing
// regression: a losing concurrent callback (or a failed step-up completion)
// must never wipe the winner's grant.
func TestOAuthCodeExchangeFailureKeepsStoredTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
	}))
	defer server.Close()
	provider := newTestOAuthProvider("http://127.0.0.1/callback")
	provider.setClient(mcpoauth.ClientInformation{ClientID: "client"})
	provider.setVerifier("verifier")
	provider.setTokens(mcpoauth.Tokens{AccessToken: "working-token", RefreshToken: "working-refresh", TokenType: "Bearer", Scope: "read"})
	provider.setDiscovery(mcpoauth.DiscoveryState{
		AuthorizationServerURL: server.URL,
		AuthorizationServerMetadata: &mcpoauth.AuthorizationServerMetadata{
			Issuer:                 server.URL,
			AuthorizationEndpoint:  server.URL + "/authorize",
			TokenEndpoint:          server.URL + "/token",
			ResponseTypesSupported: []string{"code"},
		},
	})
	_, err := mcpoauth.AuthorizeMCP(context.Background(), provider.ClientProvider(), mcpoauth.FlowOptions{
		ServerURL:         server.URL + "/mcp",
		AuthorizationCode: "losing-code",
		HTTPClient:        testHTTPClient,
	})
	var oauthErr *mcpoauth.Error
	if !errors.As(err, &oauthErr) || oauthErr.Code != "invalid_grant" {
		t.Fatalf("error = %v", err)
	}
	tokens := provider.tokens()
	if tokens == nil || tokens.AccessToken != "working-token" || tokens.RefreshToken != "working-refresh" {
		t.Fatalf("tokens = %#v, want the stored grant kept", tokens)
	}
	if provider.discoveryState() == nil {
		t.Error("discovery state was invalidated by a code exchange failure")
	}
}
