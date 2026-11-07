// Ported from PI, packages/mcp/test/auth-provider.test.ts (MIT License). Test
// titles and expectations follow the upstream cases.

package oauth_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	mcpoauth "github.com/jixoai/keel/internal/mcp/oauth"
)

func storedOptionsFor(as *authorizationServer, store mcpoauth.ServerStore, settings mcpoauth.Settings, redirectURL string, documents *mcpoauth.ClientMetadataDocuments) mcpoauth.StoredAuthProviderOptions {
	if store == nil {
		store = memoryStore()
	}
	if redirectURL == "" {
		redirectURL = signInRoute
	}
	return mcpoauth.StoredAuthProviderOptions{
		ServerURL:               as.serverURL(),
		ClientName:              "app",
		Settings:                settings,
		Store:                   store,
		ClientMetadataDocuments: documents,
		RedirectURL:             redirectURL,
		HTTPClient:              testHTTPClient,
	}
}

func signInOptionsFromStored(as *authorizationServer, base mcpoauth.StoredAuthProviderOptions, challenge *mcpoauth.Challenge) mcpoauth.SignInOptions {
	return mcpoauth.SignInOptions{
		ServerURL:               base.ServerURL,
		RedirectURL:             base.RedirectURL,
		ClientName:              base.ClientName,
		Settings:                base.Settings,
		ClientMetadataDocuments: base.ClientMetadataDocuments,
		Store:                   base.Store,
		Challenge:               challenge,
		HTTPClient:              base.HTTPClient,
	}
}

func unauthorizedEvent(as *authorizationServer, token string) mcpoauth.UnauthorizedEvent {
	return mcpoauth.UnauthorizedEvent{Status: 401, ServerURL: as.serverURL(), Token: token, HTTPClient: testHTTPClient}
}

func TestStoredAuthProviderRefreshesRejectedToken(t *testing.T) {
	server := startAuthorizationServer(t)
	base := storedOptionsFor(server, nil, mcpoauth.Settings{}, "", nil)
	signIn(t, signInOptionsFromStored(server, base, nil))
	provider := mcpoauth.NewStoredAuthProvider(base)
	if err := provider.OnUnauthorized(context.Background(), unauthorizedEvent(server, "access-1")); err != nil {
		t.Fatal(err)
	}
	token, err := provider.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "access-2" {
		t.Errorf("token = %q", token)
	}
	if got := server.tokenRequests[1].Get("client_id"); got != "client-1" {
		t.Errorf("client_id = %q", got)
	}
}

func TestStoredAuthProviderLeavesPendingSignInUsableWhenRefreshFails(t *testing.T) {
	server := startAuthorizationServer(t)
	base := storedOptionsFor(server, nil, mcpoauth.Settings{}, "", nil)
	signOptions := signInOptionsFromStored(server, base, nil)
	signIn(t, signOptions)
	// A step-up keeps the tokens while the user is in the browser.
	stepUp := signInOptionsFromStored(server, base, &mcpoauth.Challenge{Error: "insufficient_scope", Scope: "more"})
	authorization := startRedirect(t, stepUp)
	pending, _ := base.Store.Load(context.Background())

	server.revokeRefreshTokens()
	provider := mcpoauth.NewStoredAuthProvider(base)
	err := provider.OnUnauthorized(context.Background(), unauthorizedEvent(server, "access-1"))
	var required *mcpoauth.AuthorizationRequiredError
	if !errors.As(err, &required) {
		t.Fatalf("error = %v", err)
	}
	state, _ := base.Store.Load(context.Background())
	if state.OAuthState != pending.OAuthState || state.CodeVerifier != pending.CodeVerifier {
		t.Errorf("state = %#v, want the pending sign-in kept", state)
	}
	if len(server.registrations) != 1 {
		t.Errorf("registrations = %d", len(server.registrations))
	}
	response := routeResponse(t, authorizeInBrowser(t, authorization.AuthorizationURL))
	if err := mcpoauth.CompleteSignIn(context.Background(), stepUp, *authorization, response); err != nil {
		t.Fatal(err)
	}
	state, _ = base.Store.Load(context.Background())
	if state.Tokens.AccessToken != "access-2" {
		t.Errorf("access token = %q", state.Tokens.AccessToken)
	}
}

func TestStoredAuthProviderDoesNotRegisterClientToRefresh(t *testing.T) {
	server := startAuthorizationServer(t, authorizationServerOptions{cimd: true, issParameter: true})
	base := storedOptionsFor(server, nil, mcpoauth.Settings{}, signInLoopback, &mcpoauth.ClientMetadataDocuments{
		BaseURL:          "https://app.example/oauth",
		ListsRedirectURL: func(string) bool { return true },
	})
	signIn(t, signInOptionsFromStored(server, base, nil))
	state, _ := base.Store.Load(context.Background())
	if state.ClientInformation != nil {
		t.Fatalf("client = %#v", state.ClientInformation)
	}
	// Without the documents, the tokens have no client to refresh them with.
	withoutDocuments := base
	withoutDocuments.ClientMetadataDocuments = nil
	provider := mcpoauth.NewStoredAuthProvider(withoutDocuments)
	err := provider.OnUnauthorized(context.Background(), unauthorizedEvent(server, "access-1"))
	var required *mcpoauth.AuthorizationRequiredError
	if !errors.As(err, &required) {
		t.Fatalf("error = %v", err)
	}
	if len(server.registrations) != 0 {
		t.Errorf("registrations = %#v", server.registrations)
	}
	if len(server.authorizations) != 1 {
		t.Errorf("authorizations = %d", len(server.authorizations))
	}
}

func TestStoredAuthProviderRefreshesDocumentTokens(t *testing.T) {
	for _, registration := range []string{"auto", "cimd"} {
		registration := registration
		t.Run("refreshes the tokens of a document that lists only the redirect URI passed ("+registration+")", func(t *testing.T) {
			server := startAuthorizationServer(t, authorizationServerOptions{cimd: true, issParameter: true})
			base := storedOptionsFor(server, nil, mcpoauth.Settings{ClientRegistration: registration}, signInRoute, routeDocuments)
			signIn(t, signInOptionsFromStored(server, base, nil))
			state, _ := base.Store.Load(context.Background())
			if state.ClientInformation != nil {
				t.Fatalf("client = %#v", state.ClientInformation)
			}
			provider := mcpoauth.NewStoredAuthProvider(base)
			if err := provider.OnUnauthorized(context.Background(), unauthorizedEvent(server, "access-1")); err != nil {
				t.Fatal(err)
			}
			token, err := provider.Token(context.Background())
			if err != nil || token != "access-2" {
				t.Fatalf("token = %q, %v", token, err)
			}
			if got := server.tokenRequests[1].Get("client_id"); got != "https://app.example/oauth/client.json" {
				t.Errorf("client_id = %q", got)
			}
			if len(server.authorizations) != 1 {
				t.Errorf("authorizations = %d", len(server.authorizations))
			}
		})
	}
}

// offlineRoundTripper fails token requests with a network error.
type offlineRoundTripper struct {
	base *http.Transport
}

func (o offlineRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if strings.HasSuffix(request.URL.Path, "/token") {
		return nil, errors.New("fetch failed")
	}
	return o.base.RoundTrip(request)
}

func TestStoredAuthProviderPendingSignIn(t *testing.T) {
	// pendingSignIn signs in, starts a step-up whose browser step is waiting, and
	// builds the provider.
	pendingSignIn := func(t *testing.T) (*authorizationServer, mcpoauth.StoredAuthProviderOptions, *mcpoauth.SignInOptions, *mcpoauth.SignInRedirect, *mcpoauth.OAuthState, *mcpoauth.StoredAuthProvider) {
		server := startAuthorizationServer(t)
		base := storedOptionsFor(server, nil, mcpoauth.Settings{}, "", nil)
		signIn(t, signInOptionsFromStored(server, base, nil))
		stepUp := signInOptionsFromStored(server, base, &mcpoauth.Challenge{Error: "insufficient_scope", Scope: "more"})
		authorization := startRedirect(t, stepUp)
		pending, _ := base.Store.Load(context.Background())
		provider := mcpoauth.NewStoredAuthProvider(base)
		return server, base, &stepUp, authorization, pending, provider
	}

	t.Run("drops a rejected client and its tokens but keeps the pending sign-in", func(t *testing.T) {
		server, base, _, _, pending, provider := pendingSignIn(t)
		server.failNextRefresh("invalid_client", nil)
		err := provider.OnUnauthorized(context.Background(), unauthorizedEvent(server, "access-1"))
		var required *mcpoauth.AuthorizationRequiredError
		if !errors.As(err, &required) {
			t.Fatalf("error = %v", err)
		}
		state, _ := base.Store.Load(context.Background())
		if state.OAuthState != pending.OAuthState || state.CodeVerifier != pending.CodeVerifier {
			t.Errorf("state = %#v, want the pending sign-in kept", state)
		}
		if state.ClientInformation != nil || state.Tokens != nil {
			t.Errorf("state = %#v, want client and tokens dropped", state)
		}
		if len(server.registrations) != 1 {
			t.Errorf("registrations = %d", len(server.registrations))
		}
	})

	t.Run("keeps a client stored while the rejected refresh ran", func(t *testing.T) {
		server, base, _, _, _, provider := pendingSignIn(t)
		// Another sign-in completes with a new client while the server rejects the
		// old one.
		server.failNextRefresh("invalid_client", func() {
			state, err := base.Store.Load(context.Background())
			if err != nil || state == nil {
				return
			}
			state.ClientInformation = &mcpoauth.ClientInformation{ClientID: "client-new", RedirectURIs: []string{signInRoute}}
			_ = base.Store.Save(context.Background(), state)
		})
		if err := provider.OnUnauthorized(context.Background(), unauthorizedEvent(server, "access-1")); err != nil {
			t.Fatal(err)
		}
		state, _ := base.Store.Load(context.Background())
		if state.ClientInformation == nil || state.ClientInformation.ClientID != "client-new" {
			t.Errorf("client = %#v", state.ClientInformation)
		}
		if got := server.tokenRequests[len(server.tokenRequests)-1].Get("client_id"); got != "client-new" {
			t.Errorf("last client_id = %q", got)
		}
		token, err := provider.Token(context.Background())
		if err != nil || token != "access-2" {
			t.Errorf("token = %q, %v", token, err)
		}
	})

	t.Run("keeps the tokens and the pending sign-in after a server error", func(t *testing.T) {
		server, base, _, _, pending, provider := pendingSignIn(t)
		server.failNextRefresh("server_error", nil)
		err := provider.OnUnauthorized(context.Background(), unauthorizedEvent(server, "access-1"))
		var required *mcpoauth.AuthorizationRequiredError
		if !errors.As(err, &required) {
			t.Fatalf("error = %v", err)
		}
		state, _ := base.Store.Load(context.Background())
		if state.OAuthState != pending.OAuthState || state.CodeVerifier != pending.CodeVerifier {
			t.Errorf("state = %#v", state)
		}
		if state.Tokens == nil || state.Tokens.AccessToken != "access-1" {
			t.Errorf("tokens = %#v", state.Tokens)
		}
	})

	t.Run("keeps the tokens and the pending sign-in after a network error", func(t *testing.T) {
		server, base, _, _, pending, provider := pendingSignIn(t)
		offline := base
		offline.HTTPClient = &http.Client{Transport: offlineRoundTripper{base: http.DefaultTransport.(*http.Transport)}}
		provider = mcpoauth.NewStoredAuthProvider(offline)
		event := unauthorizedEvent(server, "access-1")
		event.HTTPClient = offline.HTTPClient
		err := provider.OnUnauthorized(context.Background(), event)
		var required *mcpoauth.AuthorizationRequiredError
		if !errors.As(err, &required) {
			t.Fatalf("error = %v", err)
		}
		state, _ := base.Store.Load(context.Background())
		if state.OAuthState != pending.OAuthState || state.CodeVerifier != pending.CodeVerifier {
			t.Errorf("state = %#v", state)
		}
		if state.Tokens == nil || state.Tokens.AccessToken != "access-1" {
			t.Errorf("tokens = %#v", state.Tokens)
		}
	})
}
