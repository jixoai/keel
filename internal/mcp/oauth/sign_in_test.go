// Ported from PI, packages/mcp/test/sign-in.test.ts (MIT License). Test titles
// and expectations follow the upstream cases.

package oauth_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	mcpoauth "github.com/jixoai/keel/internal/mcp/oauth"
)

// Callback routes of the application on two origins, like p2 through a relay
// and on its public URL.
const (
	signInRoute      = "https://app.example/mcp/callback"
	otherSignInRoute = "https://relay.example/mcp/callback"
	// A loopback redirect URI; tests read the redirect from the authorization
	// server instead of following it.
	signInLoopback = "http://127.0.0.1:1/callback"
)

// loopbackDocuments list /callback on 127.0.0.1, like pi's on pi.dev.
var loopbackDocuments = &mcpoauth.ClientMetadataDocuments{
	BaseURL: "https://app.example/oauth",
	ListsRedirectURL: func(redirectURL string) bool {
		parsed, err := url.Parse(redirectURL)
		return err == nil && parsed.Hostname() == "127.0.0.1" && parsed.Path == "/callback"
	},
}

// routeDocuments list only the application's https callback route.
var routeDocuments = &mcpoauth.ClientMetadataDocuments{
	BaseURL:          "https://app.example/oauth",
	ListsRedirectURL: func(redirectURL string) bool { return redirectURL == signInRoute },
}

func signInOptionsFor(as *authorizationServer, store mcpoauth.ServerStore, settings mcpoauth.Settings, redirectURL string, documents *mcpoauth.ClientMetadataDocuments, challenge *mcpoauth.Challenge) mcpoauth.SignInOptions {
	if store == nil {
		store = memoryStore()
	}
	if redirectURL == "" {
		redirectURL = signInRoute
	}
	return mcpoauth.SignInOptions{
		ServerURL:               as.serverURL(),
		RedirectURL:             redirectURL,
		ClientName:              "app",
		Settings:                settings,
		ClientMetadataDocuments: documents,
		Store:                   store,
		Challenge:               challenge,
		HTTPClient:              testHTTPClient,
	}
}

func startRedirect(t *testing.T, options mcpoauth.SignInOptions) *mcpoauth.SignInRedirect {
	t.Helper()
	started, err := mcpoauth.StartSignIn(context.Background(), options)
	if err != nil {
		t.Fatalf("start sign-in: %v", err)
	}
	if started.Redirect == nil {
		t.Fatalf("expected the browser step, got %#v", started)
	}
	return started.Redirect
}

func routeResponse(t *testing.T, location string) mcpoauth.Callback {
	t.Helper()
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	return mcpoauth.Callback{
		Code:  parsed.Query().Get("code"),
		State: parsed.Query().Get("state"),
		ISS:   parsed.Query().Get("iss"),
	}
}

func signIn(t *testing.T, options mcpoauth.SignInOptions) *mcpoauth.SignInRedirect {
	t.Helper()
	authorization := startRedirect(t, options)
	response := routeResponse(t, authorizeInBrowser(t, authorization.AuthorizationURL))
	if err := mcpoauth.CompleteSignIn(context.Background(), options, *authorization, response); err != nil {
		t.Fatalf("complete sign-in: %v", err)
	}
	return authorization
}

func TestSignInCompletesInSecondCallWithPersistedRedirect(t *testing.T) {
	server := startAuthorizationServer(t)
	options := signInOptionsFor(server, nil, mcpoauth.Settings{Scope: "issues:read"}, "", nil, nil)
	authorization := startRedirect(t, options)
	if authorization.RedirectURL != signInRoute || authorization.Scope != "issues:read" {
		t.Errorf("authorization = %#v", authorization)
	}
	parsed, _ := url.Parse(authorization.AuthorizationURL)
	if parsed.Query().Get("state") != authorization.State {
		t.Errorf("state in URL = %q", parsed.Query().Get("state"))
	}
	if len(server.registrations) != 1 {
		t.Fatalf("registrations = %#v", server.registrations)
	}
	registration := server.registrations[0]
	if registration["client_name"] != "app" || registration["application_type"] != "web" {
		t.Errorf("registration = %#v", registration)
	}
	redirects, _ := registration["redirect_uris"].([]any)
	if len(redirects) != 1 || redirects[0] != signInRoute {
		t.Errorf("redirect_uris = %#v", registration["redirect_uris"])
	}

	response := routeResponse(t, authorizeInBrowser(t, authorization.AuthorizationURL))
	if err := mcpoauth.CompleteSignIn(context.Background(), options, *authorization, response); err != nil {
		t.Fatal(err)
	}
	state, _ := options.Store.Load(context.Background())
	if state.Tokens == nil || state.Tokens.AccessToken != "access-1" || state.Tokens.Scope != "issues:read" {
		t.Errorf("tokens = %#v", state.Tokens)
	}
	if got := server.tokenRequests[0].Get("redirect_uri"); got != signInRoute {
		t.Errorf("token redirect_uri = %q", got)
	}
	if got := server.tokenRequests[0].Get("client_id"); got != "client-1" {
		t.Errorf("token client_id = %q", got)
	}
}

func TestSignInRefreshesStoredTokens(t *testing.T) {
	server := startAuthorizationServer(t)
	options := signInOptionsFor(server, nil, mcpoauth.Settings{}, "", nil, nil)
	signIn(t, options)
	started, err := mcpoauth.StartSignIn(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !started.Authorized {
		t.Fatalf("start = %#v", started)
	}
	if len(server.authorizations) != 1 {
		t.Errorf("authorizations = %d", len(server.authorizations))
	}
	if got := server.tokenRequests[1].Get("grant_type"); got != "refresh_token" {
		t.Errorf("grant type = %q", got)
	}
	state, _ := options.Store.Load(context.Background())
	if state.Tokens.AccessToken != "access-2" {
		t.Errorf("access token = %q", state.Tokens.AccessToken)
	}
}

func TestSignInRefreshesUnderRefreshLock(t *testing.T) {
	server := startAuthorizationServer(t)
	recording := newRecordingStore(nil)
	options := signInOptionsFor(server, recording, mcpoauth.Settings{}, "", nil, nil)
	signIn(t, options)
	started, err := mcpoauth.StartSignIn(context.Background(), options)
	if err != nil || !started.Authorized {
		t.Fatalf("start = %#v, %v", started, err)
	}
	saves := recording.savedWithAccess("access-2")
	if len(saves) != 1 || !saves[0].locked {
		t.Errorf("saves = %#v", saves)
	}
}

func TestSignInGoesToBrowserWhenRefreshLockBusy(t *testing.T) {
	server := startAuthorizationServer(t)
	recording := newRecordingStore(&mcpoauth.RefreshLockBusyError{})
	options := signInOptionsFor(server, recording, mcpoauth.Settings{}, "", nil, nil)
	signIn(t, options)
	started, err := mcpoauth.StartSignIn(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if started.Redirect == nil {
		t.Fatalf("start = %#v, want the browser step", started)
	}
	if len(server.tokenRequests) != 1 || server.tokenRequests[0].Get("grant_type") != "authorization_code" {
		t.Errorf("token requests = %#v", server.tokenRequests)
	}
}

func TestSignInFailsWhenRefreshLockFailsForAnotherReason(t *testing.T) {
	server := startAuthorizationServer(t)
	lockError := errors.New("EACCES: permission denied")
	recording := newRecordingStore(lockError)
	options := signInOptionsFor(server, recording, mcpoauth.Settings{}, "", nil, nil)
	// Stored tokens make the sign-in take the refresh path (and its lock).
	provider := mcpoauth.NewProvider(mcpoauth.ProviderOptions{
		ServerURL: server.serverURL(),
		Store:     recording,
	})
	if err := provider.SaveTokens(context.Background(), mcpoauth.Tokens{
		AccessToken: "a1", RefreshToken: "r1", TokenType: "Bearer",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := mcpoauth.StartSignIn(context.Background(), options)
	if !errors.Is(err, lockError) {
		t.Fatalf("error = %v", err)
	}
	if len(server.registrations) != 0 || len(server.authorizations) != 0 {
		t.Errorf("registrations = %#v authorizations = %#v", server.registrations, server.authorizations)
	}
}

func TestSignInWithoutStoredTokensNeverTakesRefreshLock(t *testing.T) {
	server := startAuthorizationServer(t)
	// A store whose lock always fails: no tokens means no refresh, so the lock
	// must not be touched at all.
	recording := newRecordingStore(errors.New("lock must not be taken"))
	options := signInOptionsFor(server, recording, mcpoauth.Settings{}, "", nil, nil)
	started, err := mcpoauth.StartSignIn(context.Background(), options)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if started.Redirect == nil {
		t.Fatalf("start = %#v, want the browser step", started)
	}
	recording.mu.Lock()
	runs := len(recording.lockRuns)
	recording.mu.Unlock()
	if runs != 0 {
		t.Errorf("refresh lock runs = %d, want 0", runs)
	}
}

func TestSignInRefusesCompleteWithAnotherRedirectURI(t *testing.T) {
	server := startAuthorizationServer(t, authorizationServerOptions{cimd: true})
	options := signInOptionsFor(server, nil, mcpoauth.Settings{}, signInLoopback, loopbackDocuments, nil)
	authorization := startRedirect(t, options)
	response := routeResponse(t, authorizeInBrowser(t, authorization.AuthorizationURL))
	wrong := options
	wrong.RedirectURL = "http://127.0.0.1:2/callback"
	err := mcpoauth.CompleteSignIn(context.Background(), wrong, *authorization, response)
	if err == nil || !strings.Contains(err.Error(), "but the authorization request used "+authorization.RedirectURL) {
		t.Fatalf("error = %v", err)
	}
	if len(server.tokenRequests) != 0 {
		t.Errorf("token requests = %#v", server.tokenRequests)
	}
}

func TestSignInFreshStateAndRefusesSuperseded(t *testing.T) {
	server := startAuthorizationServer(t)
	options := signInOptionsFor(server, nil, mcpoauth.Settings{}, "", nil, nil)
	first := startRedirect(t, options)
	firstResponse := routeResponse(t, authorizeInBrowser(t, first.AuthorizationURL))
	second := startRedirect(t, options)
	if second.State == first.State {
		t.Error("second sign-in reused the first state")
	}
	err := mcpoauth.CompleteSignIn(context.Background(), options, *first, firstResponse)
	if err == nil || !strings.Contains(err.Error(), "Another sign-in to this MCP server started since") {
		t.Fatalf("error = %v", err)
	}
	if len(server.tokenRequests) != 0 {
		t.Errorf("token requests = %#v", server.tokenRequests)
	}
}

func TestSignInRefusesResponseWithAnotherState(t *testing.T) {
	server := startAuthorizationServer(t)
	options := signInOptionsFor(server, nil, mcpoauth.Settings{}, "", nil, nil)
	authorization := startRedirect(t, options)
	response := routeResponse(t, authorizeInBrowser(t, authorization.AuthorizationURL))
	response.State = "other"
	err := mcpoauth.CompleteSignIn(context.Background(), options, *authorization, response)
	if err == nil || !strings.Contains(err.Error(), "belongs to a different sign-in") {
		t.Fatalf("error = %v", err)
	}
	if len(server.tokenRequests) != 0 {
		t.Errorf("token requests = %#v", server.tokenRequests)
	}
}

func TestSignInStoredClient(t *testing.T) {
	t.Run("refreshes with the stored client from another redirect URI", func(t *testing.T) {
		server := startAuthorizationServer(t)
		options := signInOptionsFor(server, nil, mcpoauth.Settings{}, "", nil, nil)
		signIn(t, options)
		other := options
		other.RedirectURL = otherSignInRoute
		started, err := mcpoauth.StartSignIn(context.Background(), other)
		if err != nil || !started.Authorized {
			t.Fatalf("start = %#v, %v", started, err)
		}
		if len(server.registrations) != 1 {
			t.Errorf("registrations = %d", len(server.registrations))
		}
		if got := server.tokenRequests[1].Get("client_id"); got != "client-1" {
			t.Errorf("client_id = %q", got)
		}
		state, _ := options.Store.Load(context.Background())
		if state.ClientInformation == nil || state.ClientInformation.ClientID != "client-1" {
			t.Errorf("client = %#v", state.ClientInformation)
		}
	})

	t.Run("keeps the client for a browser step on its redirect URI", func(t *testing.T) {
		server := startAuthorizationServer(t)
		options := signInOptionsFor(server, nil, mcpoauth.Settings{}, "", nil, nil)
		signIn(t, options)
		server.revokeRefreshTokens()
		authorization := startRedirect(t, options)
		parsed, _ := url.Parse(authorization.AuthorizationURL)
		if got := parsed.Query().Get("client_id"); got != "client-1" {
			t.Errorf("client_id = %q", got)
		}
		if len(server.registrations) != 1 {
			t.Errorf("registrations = %d", len(server.registrations))
		}
	})

	t.Run("drops the client and its tokens for a browser step on another redirect URI", func(t *testing.T) {
		server := startAuthorizationServer(t)
		options := signInOptionsFor(server, nil, mcpoauth.Settings{}, "", nil, nil)
		signIn(t, options)
		server.revokeRefreshTokens()
		other := options
		other.RedirectURL = otherSignInRoute
		authorization := startRedirect(t, other)
		parsed, _ := url.Parse(authorization.AuthorizationURL)
		if got := parsed.Query().Get("client_id"); got != "client-2" {
			t.Errorf("client_id = %q", got)
		}
		if len(server.registrations) < 2 {
			t.Fatalf("registrations = %#v", server.registrations)
		}
		redirects, _ := server.registrations[1]["redirect_uris"].([]any)
		if len(redirects) != 1 || redirects[0] != otherSignInRoute {
			t.Errorf("second registration = %#v", server.registrations[1])
		}
		state, _ := options.Store.Load(context.Background())
		if state.ClientInformation == nil || state.ClientInformation.ClientID != "client-2" {
			t.Errorf("client = %#v", state.ClientInformation)
		}
		if state.Tokens != nil {
			t.Errorf("tokens = %#v, want dropped", state.Tokens)
		}
	})

	t.Run("does not refresh with a registered client that cimd replaces", func(t *testing.T) {
		server := startAuthorizationServer(t, authorizationServerOptions{cimd: true, issParameter: true})
		options := signInOptionsFor(server, nil, mcpoauth.Settings{ClientRegistration: "dcr"}, signInLoopback, loopbackDocuments, nil)
		signIn(t, options)
		state, _ := options.Store.Load(context.Background())
		if state.ClientInformation == nil || state.ClientInformation.ClientID != "client-1" {
			t.Fatalf("client = %#v", state.ClientInformation)
		}
		cimd := options
		cimd.Settings = mcpoauth.Settings{ClientRegistration: "cimd"}
		authorization := startRedirect(t, cimd)
		parsed, _ := url.Parse(authorization.AuthorizationURL)
		if got := parsed.Query().Get("client_id"); got != "https://app.example/oauth/client.json" {
			t.Errorf("client_id = %q", got)
		}
		if len(server.registrations) != 1 {
			t.Errorf("registrations = %d", len(server.registrations))
		}
		for _, request := range server.tokenRequests {
			if request.Get("grant_type") != "authorization_code" {
				t.Errorf("token request = %#v", request)
			}
		}
		state, _ = options.Store.Load(context.Background())
		if state.ClientInformation != nil || state.Tokens != nil {
			t.Errorf("state = %#v, want client and tokens dropped", state)
		}
	})

	t.Run("asks for the granted scope plus the challenged one on a step-up, without refreshing", func(t *testing.T) {
		server := startAuthorizationServer(t)
		options := signInOptionsFor(server, nil, mcpoauth.Settings{}, "", nil, &mcpoauth.Challenge{Scope: "issues:read"})
		signIn(t, options)
		state, _ := options.Store.Load(context.Background())
		if state.Tokens.Scope != "issues:read" {
			t.Fatalf("scope = %q", state.Tokens.Scope)
		}
		// The challenge lists only the missing scope; requesting just that would
		// lose issues:read.
		stepUp := options
		stepUp.Challenge = &mcpoauth.Challenge{Error: "insufficient_scope", Scope: "issues:write"}
		authorization := signIn(t, stepUp)
		if authorization.Scope != "issues:read issues:write" {
			t.Errorf("scope = %q", authorization.Scope)
		}
		if len(server.tokenRequests) != 2 {
			t.Fatalf("token requests = %#v", server.tokenRequests)
		}
		for _, request := range server.tokenRequests {
			if request.Get("grant_type") != "authorization_code" {
				t.Errorf("grant type = %q", request.Get("grant_type"))
			}
		}
		state, _ = options.Store.Load(context.Background())
		if state.Tokens.Scope != "issues:read issues:write" {
			t.Errorf("scope = %q", state.Tokens.Scope)
		}
	})
}

func TestSignInClientMetadataDocuments(t *testing.T) {
	loopback := func(server *authorizationServer, settings mcpoauth.Settings) mcpoauth.SignInOptions {
		return signInOptionsFor(server, nil, settings, signInLoopback, loopbackDocuments, nil)
	}

	t.Run("uses a document when the server supports them and the documents list the redirect URI", func(t *testing.T) {
		server := startAuthorizationServer(t, authorizationServerOptions{cimd: true, issParameter: true})
		options := loopback(server, mcpoauth.Settings{})
		authorization := signIn(t, options)
		parsed, _ := url.Parse(authorization.AuthorizationURL)
		if got := parsed.Query().Get("client_id"); got != "https://app.example/oauth/client.json" {
			t.Errorf("client_id = %q", got)
		}
		if authorization.RedirectURL != signInLoopback {
			t.Errorf("redirect = %q", authorization.RedirectURL)
		}
		if len(server.registrations) != 0 {
			t.Errorf("registrations = %#v", server.registrations)
		}
		if got := server.tokenRequests[0].Get("client_id"); got != "https://app.example/oauth/client.json" {
			t.Errorf("token client_id = %q", got)
		}
		state, _ := options.Store.Load(context.Background())
		if state.ClientInformation != nil {
			t.Errorf("client = %#v", state.ClientInformation)
		}
	})

	t.Run("uses a document and redirect URI specific to the MCP server without the iss parameter", func(t *testing.T) {
		server := startAuthorizationServer(t, authorizationServerOptions{cimd: true})
		authorization := signIn(t, loopback(server, mcpoauth.Settings{}))
		id := mcpoauth.CallbackID(server.serverURL())
		parsed, _ := url.Parse(authorization.AuthorizationURL)
		if got := parsed.Query().Get("client_id"); got != "https://app.example/oauth/"+id+"/client.json" {
			t.Errorf("client_id = %q", got)
		}
		if authorization.RedirectURL != signInLoopback+"/"+id {
			t.Errorf("redirect = %q", authorization.RedirectURL)
		}
		if got := server.tokenRequests[0].Get("redirect_uri"); got != signInLoopback+"/"+id {
			t.Errorf("token redirect_uri = %q", got)
		}
	})

	t.Run("appends the server-specific path without doubling a trailing slash", func(t *testing.T) {
		server := startAuthorizationServer(t, authorizationServerOptions{cimd: true})
		options := loopback(server, mcpoauth.Settings{})
		options.RedirectURL = signInLoopback + "/"
		options.ClientMetadataDocuments = &mcpoauth.ClientMetadataDocuments{
			BaseURL:          "https://app.example/oauth",
			ListsRedirectURL: func(string) bool { return true },
		}
		authorization := startRedirect(t, options)
		id := mcpoauth.CallbackID(server.serverURL())
		if authorization.RedirectURL != signInLoopback+"/"+id {
			t.Errorf("redirect = %q", authorization.RedirectURL)
		}
	})

	t.Run("registers dynamically when the server does not support documents", func(t *testing.T) {
		server := startAuthorizationServer(t, authorizationServerOptions{issParameter: true})
		signIn(t, loopback(server, mcpoauth.Settings{}))
		if len(server.registrations) != 1 {
			t.Fatalf("registrations = %d", len(server.registrations))
		}
		if got := server.authorizations[0].Get("client_id"); got != "client-1" {
			t.Errorf("client_id = %q", got)
		}
	})

	t.Run("registers dynamically when the documents do not list the redirect URI", func(t *testing.T) {
		server := startAuthorizationServer(t, authorizationServerOptions{cimd: true, issParameter: true})
		options := loopback(server, mcpoauth.Settings{})
		options.RedirectURL = signInRoute
		signIn(t, options)
		if len(server.registrations) != 1 {
			t.Fatalf("registrations = %d", len(server.registrations))
		}
	})

	t.Run("registers dynamically without documents or with a client name", func(t *testing.T) {
		server := startAuthorizationServer(t, authorizationServerOptions{cimd: true, issParameter: true})
		withoutDocuments := loopback(server, mcpoauth.Settings{})
		withoutDocuments.ClientMetadataDocuments = nil
		signIn(t, withoutDocuments)
		signIn(t, loopback(server, mcpoauth.Settings{ClientName: "Other"}))
		if len(server.registrations) != 2 {
			t.Fatalf("registrations = %d", len(server.registrations))
		}
		if server.registrations[0]["client_name"] != "app" || server.registrations[1]["client_name"] != "Other" {
			t.Errorf("registrations = %#v", server.registrations)
		}
	})

	for _, registration := range []string{"auto", "cimd"} {
		registration := registration
		t.Run("refreshes the tokens of a document that lists only an https route ("+registration+")", func(t *testing.T) {
			server := startAuthorizationServer(t, authorizationServerOptions{cimd: true, issParameter: true})
			options := signInOptionsFor(server, nil, mcpoauth.Settings{ClientRegistration: registration}, "", routeDocuments, nil)
			authorization := signIn(t, options)
			parsed, _ := url.Parse(authorization.AuthorizationURL)
			if got := parsed.Query().Get("client_id"); got != "https://app.example/oauth/client.json" {
				t.Errorf("client_id = %q", got)
			}
			state, _ := options.Store.Load(context.Background())
			if state.ClientInformation != nil {
				t.Errorf("client = %#v", state.ClientInformation)
			}
			started, err := mcpoauth.StartSignIn(context.Background(), options)
			if err != nil || !started.Authorized {
				t.Fatalf("start = %#v, %v", started, err)
			}
			if len(server.authorizations) != 1 {
				t.Errorf("authorizations = %d", len(server.authorizations))
			}
			if got := server.tokenRequests[1].Get("grant_type"); got != "refresh_token" {
				t.Errorf("grant type = %q", got)
			}
			if got := server.tokenRequests[1].Get("client_id"); got != "https://app.example/oauth/client.json" {
				t.Errorf("client_id = %q", got)
			}
			state, _ = options.Store.Load(context.Background())
			if state.Tokens.AccessToken != "access-2" {
				t.Errorf("access token = %q", state.Tokens.AccessToken)
			}
		})
	}

	t.Run("fails with cimd when the server does not support documents", func(t *testing.T) {
		server := startAuthorizationServer(t, authorizationServerOptions{issParameter: true})
		_, err := mcpoauth.StartSignIn(context.Background(), loopback(server, mcpoauth.Settings{ClientRegistration: "cimd"}))
		if err == nil || !strings.Contains(err.Error(), "does not support Client ID Metadata Documents") {
			t.Fatalf("error = %v", err)
		}
		if len(server.registrations) != 0 {
			t.Errorf("registrations = %#v", server.registrations)
		}
	})

	t.Run("fails with cimd when the documents do not list the redirect URI", func(t *testing.T) {
		server := startAuthorizationServer(t, authorizationServerOptions{cimd: true, issParameter: true})
		options := loopback(server, mcpoauth.Settings{ClientRegistration: "cimd"})
		options.RedirectURL = signInRoute
		_, err := mcpoauth.StartSignIn(context.Background(), options)
		if err == nil || !strings.Contains(err.Error(), "needs Client ID Metadata Documents that list the redirect URI") {
			t.Fatalf("error = %v", err)
		}
		if len(server.registrations) != 0 {
			t.Errorf("registrations = %#v", server.registrations)
		}
	})
}

func TestSignInCancellation(t *testing.T) {
	t.Run("cancels a start whose request does not answer", func(t *testing.T) {
		server := startAuthorizationServer(t)
		server.stallPath("/register")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		time.AfterFunc(50*time.Millisecond, cancel)
		_, err := mcpoauth.StartSignIn(ctx, signInOptionsFor(server, nil, mcpoauth.Settings{}, "", nil, nil))
		if !errors.Is(err, mcpoauth.ErrSignInCancelled) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("cancels a completion whose token request does not answer", func(t *testing.T) {
		server := startAuthorizationServer(t)
		options := signInOptionsFor(server, nil, mcpoauth.Settings{}, "", nil, nil)
		authorization := startRedirect(t, options)
		response := routeResponse(t, authorizeInBrowser(t, authorization.AuthorizationURL))
		server.stallPath("/token")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		time.AfterFunc(50*time.Millisecond, cancel)
		err := mcpoauth.CompleteSignIn(ctx, options, *authorization, response)
		if !errors.Is(err, mcpoauth.ErrSignInCancelled) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("cancels at once while another process holds the refresh lock, and refreshes nothing", func(t *testing.T) {
		server := startAuthorizationServer(t)
		recording := newRecordingStore(nil)
		options := signInOptionsFor(server, recording, mcpoauth.Settings{}, "", nil, nil)
		signIn(t, options)
		release := recording.hold()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := make(chan error, 1)
		go func() {
			_, err := mcpoauth.StartSignIn(ctx, options)
			started <- err
		}()
		time.AfterFunc(50*time.Millisecond, cancel)
		err := <-started
		if !errors.Is(err, mcpoauth.ErrSignInCancelled) {
			t.Fatalf("error = %v", err)
		}
		// The lock arrives after the cancellation: nothing runs under it, and it is
		// released.
		release()
		recording.waitLockRuns(t)
		if recording.isLocked() {
			t.Error("refresh lock still held")
		}
		if len(server.tokenRequests) != 1 || server.tokenRequests[0].Get("grant_type") != "authorization_code" {
			t.Errorf("token requests = %#v", server.tokenRequests)
		}
		if len(server.authorizations) != 1 {
			t.Errorf("authorizations = %d", len(server.authorizations))
		}
	})

	t.Run("fails at once with an aborted context", func(t *testing.T) {
		server := startAuthorizationServer(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := mcpoauth.StartSignIn(ctx, signInOptionsFor(server, nil, mcpoauth.Settings{}, "", nil, nil))
		if !errors.Is(err, mcpoauth.ErrSignInCancelled) {
			t.Fatalf("error = %v", err)
		}
		if len(server.registrations) != 0 {
			t.Errorf("registrations = %#v", server.registrations)
		}
	})
}

// TestSignInStepUpFailureKeepsWorkingGrant is the token-clearing regression for
// the interactive path: a failed step-up exchange must keep the old grant.
func TestSignInStepUpFailureKeepsWorkingGrant(t *testing.T) {
	server := startAuthorizationServer(t)
	options := signInOptionsFor(server, nil, mcpoauth.Settings{}, "", nil, nil)
	signIn(t, options)

	stepUp := options
	stepUp.Challenge = &mcpoauth.Challenge{Error: "insufficient_scope", Scope: "admin"}
	authorization := startRedirect(t, stepUp)
	response := routeResponse(t, authorizeInBrowser(t, authorization.AuthorizationURL))
	// Make the code exchange fail like a replayed or expired code.
	server.mu.Lock()
	delete(server.challenges, response.Code)
	server.mu.Unlock()
	err := mcpoauth.CompleteSignIn(context.Background(), stepUp, *authorization, response)
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("error = %v", err)
	}
	after, _ := options.Store.Load(context.Background())
	if after.Tokens == nil || after.Tokens.AccessToken != "access-1" || after.Tokens.RefreshToken == "" {
		t.Fatalf("tokens = %#v, want the working grant kept", after.Tokens)
	}
	if after.ClientInformation == nil {
		t.Error("client information was invalidated by the failed step-up completion")
	}
}
