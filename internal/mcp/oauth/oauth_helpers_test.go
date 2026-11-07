// Ported test fixture from PI, packages/mcp/test/authorization-server.ts and
// helpers.ts (MIT License), reimplemented with net/http/httptest.

package oauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/jixoai/keel/internal/mcp"
	mcpoauth "github.com/jixoai/keel/internal/mcp/oauth"
)

// transportAuthAdapter maps the oauth package's AuthProvider onto the
// transport's interface (the mcp package has the production adapter).
type transportAuthAdapter struct {
	inner mcpoauth.AuthProvider
}

func (a transportAuthAdapter) Token(ctx context.Context) (string, error) {
	return a.inner.Token(ctx)
}

func (a transportAuthAdapter) OnUnauthorized(ctx context.Context, event mcp.UnauthorizedEvent) error {
	return a.inner.OnUnauthorized(ctx, mcpoauth.UnauthorizedEvent{
		Status:          event.Status,
		WWWAuthenticate: event.WWWAuthenticate,
		Body:            event.Body,
		ServerURL:       event.ServerURL,
		Token:           event.Token,
		HTTPClient:      event.Client,
	})
}

// testHTTPClient reaches loopback test servers. Production uses
// mcpoauth.NewHTTPClient, which blocks private addresses.
var testHTTPClient = &http.Client{}

// noRedirectClient follows the authorization redirect manually, like a browser
// whose address bar the test reads.
var noRedirectClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func readBody(r *http.Request) string {
	data, _ := io.ReadAll(r.Body)
	return string(data)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// authorizationServer is the PI test authorization server of an MCP server at
// <origin>/mcp: discovery, dynamic client registration, PKCE, and rotating
// refresh tokens. Token responses name no scope.
type authorizationServer struct {
	t      *testing.T
	server *httptest.Server
	origin string

	mu             sync.Mutex
	registrations  []map[string]any
	authorizations []url.Values
	tokenRequests  []url.Values
	refreshTokens  map[string]bool
	challenges     map[string]string
	stall          map[string]bool
	refreshFailure *refreshFailure
	issued         int
	cimd           bool
	issParameter   bool
	// releaseStall unblocks stalled handlers at cleanup, so httptest.Server can
	// close.
	releaseStall chan struct{}
}

type refreshFailure struct {
	error     string
	onRequest func()
}

type authorizationServerOptions struct {
	cimd         bool
	issParameter bool
}

func startAuthorizationServer(t *testing.T, options ...authorizationServerOptions) *authorizationServer {
	t.Helper()
	config := authorizationServerOptions{}
	if len(options) > 0 {
		config = options[0]
	}
	as := &authorizationServer{
		t:             t,
		refreshTokens: map[string]bool{},
		challenges:    map[string]string{},
		stall:         map[string]bool{},
		cimd:          config.cimd,
		issParameter:  config.issParameter,
		releaseStall:  make(chan struct{}),
	}
	as.server = httptest.NewServer(http.HandlerFunc(as.handle))
	as.origin = as.server.URL
	t.Cleanup(func() {
		close(as.releaseStall)
		as.server.Close()
	})
	return as
}

func (as *authorizationServer) serverURL() string { return as.origin + "/mcp" }

func (as *authorizationServer) issueTokens() map[string]any {
	as.issued++
	refresh := fmt.Sprintf("refresh-%d", as.issued)
	as.refreshTokens[refresh] = true
	return map[string]any{
		"access_token":  fmt.Sprintf("access-%d", as.issued),
		"refresh_token": refresh,
		"token_type":    "Bearer",
	}
}

func (as *authorizationServer) handle(w http.ResponseWriter, r *http.Request) {
	requestURL, _ := url.Parse(r.URL.String())
	as.mu.Lock()
	stalled := as.stall[requestURL.Path]
	as.mu.Unlock()
	if stalled {
		// Accept and never answer, like PI's stall fixture.
		<-as.releaseStall
		return
	}
	switch requestURL.Path {
	case "/.well-known/oauth-protected-resource/mcp":
		writeJSON(w, 200, map[string]any{
			"resource":              as.origin + "/mcp",
			"authorization_servers": []string{as.origin},
		})
	case "/.well-known/oauth-authorization-server":
		body := map[string]any{
			"issuer":                                as.origin,
			"authorization_endpoint":                as.origin + "/authorize",
			"token_endpoint":                        as.origin + "/token",
			"registration_endpoint":                 as.origin + "/register",
			"response_types_supported":              []string{"code"},
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none"},
		}
		if as.cimd {
			body["client_id_metadata_document_supported"] = true
		}
		if as.issParameter {
			body["authorization_response_iss_parameter_supported"] = true
		}
		writeJSON(w, 200, body)
	case "/register":
		var metadata map[string]any
		_ = json.Unmarshal([]byte(readBody(r)), &metadata)
		as.mu.Lock()
		as.registrations = append(as.registrations, metadata)
		id := fmt.Sprintf("client-%d", len(as.registrations))
		as.mu.Unlock()
		metadata["client_id"] = id
		writeJSON(w, 201, metadata)
	case "/authorize":
		as.mu.Lock()
		as.authorizations = append(as.authorizations, r.URL.Query())
		code := fmt.Sprintf("code-%d", len(as.authorizations))
		as.challenges[code] = r.URL.Query().Get("code_challenge")
		as.mu.Unlock()
		redirect, _ := url.Parse(r.URL.Query().Get("redirect_uri"))
		query := redirect.Query()
		query.Set("code", code)
		query.Set("state", r.URL.Query().Get("state"))
		if as.issParameter {
			query.Set("iss", as.origin)
		}
		redirect.RawQuery = query.Encode()
		w.Header().Set("Location", redirect.String())
		w.WriteHeader(http.StatusFound)
	case "/token":
		params, _ := url.ParseQuery(readBody(r))
		as.mu.Lock()
		as.tokenRequests = append(as.tokenRequests, params)
		as.mu.Unlock()
		if params.Get("grant_type") == "authorization_code" {
			digest := sha256.Sum256([]byte(params.Get("code_verifier")))
			verifier := base64.RawURLEncoding.EncodeToString(digest[:])
			as.mu.Lock()
			expected := as.challenges[params.Get("code")]
			as.mu.Unlock()
			if expected != verifier {
				writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
				return
			}
			as.mu.Lock()
			delete(as.challenges, params.Get("code"))
			tokens := as.issueTokens()
			as.mu.Unlock()
			writeJSON(w, 200, tokens)
			return
		}
		as.mu.Lock()
		failure := as.refreshFailure
		as.refreshFailure = nil
		as.mu.Unlock()
		if failure != nil {
			if failure.onRequest != nil {
				failure.onRequest()
			}
			writeJSON(w, 400, map[string]any{"error": failure.error})
			return
		}
		as.mu.Lock()
		valid := as.refreshTokens[params.Get("refresh_token")]
		delete(as.refreshTokens, params.Get("refresh_token"))
		var tokens map[string]any
		if valid {
			tokens = as.issueTokens()
		}
		as.mu.Unlock()
		if !valid {
			writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
			return
		}
		writeJSON(w, 200, tokens)
	default:
		w.WriteHeader(404)
	}
}

func (as *authorizationServer) revokeRefreshTokens() {
	as.mu.Lock()
	defer as.mu.Unlock()
	as.refreshTokens = map[string]bool{}
}

func (as *authorizationServer) failNextRefresh(errorCode string, onRequest func()) {
	as.mu.Lock()
	defer as.mu.Unlock()
	as.refreshFailure = &refreshFailure{error: errorCode, onRequest: onRequest}
}

func (as *authorizationServer) stallPath(path string) {
	as.mu.Lock()
	defer as.mu.Unlock()
	as.stall[path] = true
}

// authorizeInBrowser follows the authorization URL like a browser, up to the
// redirect back to the application.
func authorizeInBrowser(t *testing.T, authorizationURL string) string {
	t.Helper()
	response, err := noRedirectClient.Get(authorizationURL)
	if err != nil {
		t.Fatalf("browse authorization URL: %v", err)
	}
	defer response.Body.Close()
	return response.Header.Get("Location")
}

// memoryStore is an in-memory server store; its refresh lock only runs fn.
func memoryStore() mcpoauth.ServerStore { return mcpoauth.NewMemoryServerStore() }

// recordedSave is one Save under the recording store.
type recordedSave struct {
	locked      bool
	accessToken string
}

// recordingStore records whether each save ran under the refresh lock. With
// lockError, taking the lock fails; hold() stands for another process holding
// the lock until the returned function releases it.
type recordingStore struct {
	mu        sync.Mutex
	memory    *mcpoauth.MemoryStateStore
	saves     []recordedSave
	locked    bool
	lockError error
	held      chan struct{}
	lockRuns  []chan struct{}
}

func newRecordingStore(lockError error) *recordingStore {
	return &recordingStore{memory: mcpoauth.NewMemoryStateStore(), lockError: lockError}
}

func (r *recordingStore) Load(ctx context.Context) (*mcpoauth.OAuthState, error) {
	return r.memory.Load(ctx)
}

func (r *recordingStore) Save(ctx context.Context, state *mcpoauth.OAuthState) error {
	r.mu.Lock()
	access := ""
	if state != nil && state.Tokens != nil {
		access = state.Tokens.AccessToken
	}
	r.saves = append(r.saves, recordedSave{locked: r.locked, accessToken: access})
	r.mu.Unlock()
	return r.memory.Save(ctx, state)
}

func (r *recordingStore) WithRefreshLock(ctx context.Context, fn func(ctx context.Context) error) error {
	done := make(chan struct{})
	r.mu.Lock()
	r.lockRuns = append(r.lockRuns, done)
	held := r.held
	lockError := r.lockError
	r.mu.Unlock()
	defer close(done)
	if held != nil {
		<-held
	}
	if lockError != nil {
		return lockError
	}
	r.mu.Lock()
	r.locked = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.locked = false
		r.mu.Unlock()
	}()
	return fn(ctx)
}

func (r *recordingStore) hold() func() {
	release := make(chan struct{})
	r.mu.Lock()
	r.held = release
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			r.held = nil
			r.mu.Unlock()
			close(release)
		})
	}
}

func (r *recordingStore) savedWithAccess(token string) []recordedSave {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []recordedSave
	for _, save := range r.saves {
		if save.accessToken == token {
			out = append(out, save)
		}
	}
	return out
}

func (r *recordingStore) isLocked() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.locked
}

func (r *recordingStore) waitLockRuns(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	runs := append([]chan struct{}(nil), r.lockRuns...)
	r.mu.Unlock()
	for _, run := range runs {
		<-run
	}
}

// testOAuthProvider is the PI test's TestOAuthProvider: a ClientProvider with
// in-memory fields, for tests that need to inspect or preset state.
type testOAuthProvider struct {
	mu        sync.Mutex
	redirect  string
	client    *mcpoauth.ClientInformation
	tokenSet  *mcpoauth.Tokens
	verifier  string
	discovery *mcpoauth.DiscoveryState
	authURL   *url.URL
}

func newTestOAuthProvider(redirectURL string) *testOAuthProvider {
	return &testOAuthProvider{redirect: redirectURL}
}

func (p *testOAuthProvider) ClientProvider() *mcpoauth.ClientProvider {
	return &mcpoauth.ClientProvider{
		RedirectURL: p.redirect,
		ClientMetadata: mcpoauth.ClientMetadata{
			RedirectURIs:            []string{p.redirect},
			ClientName:              "pi-mcp-test",
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			ResponseTypes:           []string{"code"},
			TokenEndpointAuthMethod: "none",
		},
		State: func(context.Context) (string, error) { return "expected-state", nil },
		ClientInformation: func(context.Context) (*mcpoauth.ClientInformation, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			return p.client, nil
		},
		SaveClientInformation: func(_ context.Context, information mcpoauth.ClientInformation) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.client = &information
			return nil
		},
		Tokens: func(context.Context) (*mcpoauth.Tokens, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			return p.tokenSet, nil
		},
		SaveTokens: func(_ context.Context, tokens mcpoauth.Tokens) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.tokenSet = &tokens
			return nil
		},
		RedirectToAuthorization: func(_ context.Context, authorizationURL *url.URL) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.authURL = authorizationURL
			return nil
		},
		SaveCodeVerifier: func(_ context.Context, verifier string) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.verifier = verifier
			return nil
		},
		CodeVerifier: func(context.Context) (string, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.verifier == "" {
				return "", fmt.Errorf("Missing code verifier")
			}
			return p.verifier, nil
		},
		InvalidateCredentials: func(_ context.Context, kind mcpoauth.InvalidateKind) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			if kind == mcpoauth.InvalidateAll || kind == mcpoauth.InvalidateClient {
				p.client = nil
			}
			if kind == mcpoauth.InvalidateAll || kind == mcpoauth.InvalidateTokens {
				p.tokenSet = nil
			}
			if kind == mcpoauth.InvalidateAll || kind == mcpoauth.InvalidateVerifier {
				p.verifier = ""
			}
			if kind == mcpoauth.InvalidateAll || kind == mcpoauth.InvalidateDiscovery {
				p.discovery = nil
			}
			return nil
		},
		SaveDiscoveryState: func(_ context.Context, state mcpoauth.DiscoveryState) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.discovery = &state
			return nil
		},
		DiscoveryState: func(context.Context) (*mcpoauth.DiscoveryState, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			return p.discovery, nil
		},
	}
}

func (p *testOAuthProvider) setClient(client mcpoauth.ClientInformation) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.client = &client
}

func (p *testOAuthProvider) setTokens(tokens mcpoauth.Tokens) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tokenSet = &tokens
}

func (p *testOAuthProvider) setDiscovery(state mcpoauth.DiscoveryState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.discovery = &state
}

func (p *testOAuthProvider) setVerifier(verifier string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.verifier = verifier
}

func (p *testOAuthProvider) authorizationURL() *url.URL {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.authURL
}

func (p *testOAuthProvider) tokens() *mcpoauth.Tokens {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tokenSet
}

func (p *testOAuthProvider) discoveryState() *mcpoauth.DiscoveryState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.discovery
}
