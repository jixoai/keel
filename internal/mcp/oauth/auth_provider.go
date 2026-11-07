// Behaviour ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/src/oauth/auth-provider.ts (MIT License); implemented
// independently in Go. Contract kept identical; see docs/UPSTREAM.md.
//
// The auth provider for connections to an OAuth-protected MCP server: it sends
// the stored access token and refreshes it, but never starts a browser flow.
// When the user has to sign in it fails with AuthorizationRequiredError and the
// host runs a sign-in.

package oauth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// RefreshSkewMS is how close to expiry an access token is refreshed before it
// is sent.
const RefreshSkewMS int64 = 30_000

// StoredAuthProviderOptions configures the stored-token auth provider.
type StoredAuthProviderOptions struct {
	ServerURL               string
	ClientName              string
	Settings                Settings
	Store                   ServerStore
	ClientMetadataDocuments *ClientMetadataDocuments
	// RedirectURL is a redirect URI the application signs in with, such as its
	// https callback route. A refresh never redirects, so any listed URI works.
	RedirectURL string
	HTTPClient  *http.Client
	// OnChallenge receives the server's WWW-Authenticate challenge so a host
	// can remember the resource metadata URL and scope for sign-in.
	OnChallenge func(challenge Challenge)
}

// StoredAuthProvider sends stored tokens, refreshing them when they are about
// to expire or after a 401.
type StoredAuthProvider struct {
	options StoredAuthProviderOptions

	mu         sync.Mutex
	refreshing *refreshCall
}

type refreshCall struct {
	done chan struct{}
	err  error
}

// NewStoredAuthProvider builds the provider.
func NewStoredAuthProvider(options StoredAuthProviderOptions) *StoredAuthProvider {
	return &StoredAuthProvider{options: options}
}

// Token returns the access token for the next request. It waits for a running
// refresh, then refreshes an expiring token; a failure falls through and the
// request goes out with the old token, so a 401 decides what happens.
func (p *StoredAuthProvider) Token(ctx context.Context) (string, error) {
	if err := p.waitRefreshing(ctx); err != nil {
		return "", err
	}
	state, err := p.options.Store.Load(ctx)
	if err != nil {
		return "", err
	}
	if state == nil || state.Tokens == nil {
		return "", nil
	}
	token := state.Tokens.AccessToken
	expiring := state.TokensExpireAt != 0 && state.TokensExpireAt-RefreshSkewMS <= time.Now().UnixMilli()
	if !expiring || state.Tokens.RefreshToken == "" {
		return token, nil
	}
	_ = p.refresh(ctx, token, nil)
	reloaded, err := p.options.Store.Load(ctx)
	if err != nil {
		return "", err
	}
	if reloaded == nil || reloaded.Tokens == nil {
		return "", nil
	}
	return reloaded.Tokens.AccessToken, nil
}

// OnUnauthorized refreshes a rejected token, or fails with
// AuthorizationRequiredError when the user has to sign in (including a server
// asking for more scope: a refresh keeps the granted scope).
func (p *StoredAuthProvider) OnUnauthorized(ctx context.Context, event UnauthorizedEvent) error {
	challenge := ParseWWWAuthenticate(event.WWWAuthenticate)
	if p.options.OnChallenge != nil {
		p.options.OnChallenge(challenge)
	}
	if challenge.Error == "insufficient_scope" {
		return &AuthorizationRequiredError{Reason: "insufficient_scope"}
	}
	return p.refresh(ctx, event.Token, &challenge)
}

// Settled resolves when no refresh is running, so shutdown does not drop
// rotated tokens before they are saved.
func (p *StoredAuthProvider) Settled(ctx context.Context) error {
	return p.waitRefreshing(ctx)
}

func (p *StoredAuthProvider) waitRefreshing(ctx context.Context) error {
	p.mu.Lock()
	call := p.refreshing
	p.mu.Unlock()
	if call == nil {
		return nil
	}
	select {
	case <-call.done:
		return call.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// refresh replaces staleToken under the store's refresh lock. Requests in this
// process share one refresh; the lock keeps other processes out, and tokens
// that changed meanwhile are used without refreshing.
func (p *StoredAuthProvider) refresh(ctx context.Context, staleToken string, challenge *Challenge) error {
	p.mu.Lock()
	if p.refreshing != nil {
		call := p.refreshing
		p.mu.Unlock()
		<-call.done
		return call.err
	}
	call := &refreshCall{done: make(chan struct{})}
	p.refreshing = call
	p.mu.Unlock()

	// A refresh already sent must save its rotated tokens, but it still needs a
	// hard deadline so a stuck server cannot pin the lock (and the connection).
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RefreshBudget)
	defer cancel()
	call.err = p.options.Store.WithRefreshLock(refreshCtx, func(lockCtx context.Context) error {
		state, err := p.options.Store.Load(lockCtx)
		if err != nil {
			return err
		}
		current := ""
		if state != nil && state.Tokens != nil {
			current = state.Tokens.AccessToken
		}
		// Another process (or a completed sign-in) may already have replaced the
		// rejected token; then there is nothing to refresh.
		if current != staleToken {
			return nil
		}
		if state == nil || state.Tokens == nil || state.Tokens.RefreshToken == "" {
			return &AuthorizationRequiredError{Reason: "no refresh token stored"}
		}
		result, err := RefreshStoredTokens(lockCtx, p.options, state, RefreshOptions{
			Challenge:   challenge,
			RedirectURL: p.options.RedirectURL,
		})
		if err != nil {
			return err
		}
		if result == FlowRedirect {
			return &AuthorizationRequiredError{Reason: "the refresh needs a new sign-in"}
		}
		return nil
	})
	p.mu.Lock()
	p.refreshing = nil
	p.mu.Unlock()
	close(call.done)
	return call.err
}

// RefreshOptions configures RefreshStoredTokens.
type RefreshOptions struct {
	Challenge *Challenge
	// RedirectURL is a redirect URI the application signs in with, tried before
	// the loopback ones and the stored client's.
	RedirectURL string
}

// RefreshStoredTokens refreshes state's tokens with the stored client (or the
// document it signed in with), or reports FlowRedirect when that is not
// possible. It never registers a client or prepares an authorization request,
// so a pending sign-in stays usable. Call it while holding the refresh lock.
func RefreshStoredTokens(ctx context.Context, options StoredAuthProviderOptions, state *OAuthState, refresh RefreshOptions) (FlowResult, error) {
	candidates := []string{refresh.RedirectURL, LoopbackSettingsOf(options.Settings).FixedRedirectURL}
	if state != nil {
		if registered := RegisteredRedirectURLs(state.ClientInformation); len(registered) > 0 {
			candidates = append(candidates, registered[0])
		}
	}
	redirectURL := ""
	documents := options.ClientMetadataDocuments
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if documents != nil && documents.ListsRedirectURL != nil && documents.ListsRedirectURL(candidate) {
			redirectURL = candidate
			break
		}
	}
	if redirectURL == "" {
		redirectURL = DefaultRedirectURL
	}
	provider := NewClientProvider(options, redirectURL, nil)
	flowOptions := FlowOptions{
		ServerURL:                      options.ServerURL,
		AuthorizationServerMetadataURL: options.Settings.AuthServerMetadataURL,
		RefreshOnly:                    true,
		HTTPClient:                     options.HTTPClient,
	}
	if refresh.Challenge != nil {
		flowOptions.ResourceMetadataURL = refresh.Challenge.ResourceMetadataURL
	}
	return AuthorizeMCP(ctx, provider, flowOptions)
}

// NewClientProvider builds the flow provider of one OAuth client, redirecting
// to redirectURL or the document's variant of it.
func NewClientProvider(options StoredAuthProviderOptions, redirectURL string, onRedirect func(context.Context, *url.URL) error) *ClientProvider {
	clientName := options.Settings.ClientName
	if clientName == "" {
		clientName = options.ClientName
	}
	metadata := ClientMetadata{ClientName: clientName}
	documents := options.ClientMetadataDocuments
	var document func(*AuthorizationServerMetadata) (*ClientMetadataDocument, error)
	if OffersClientMetadataDocument(options.Settings, documents, redirectURL) {
		document = func(metadata *AuthorizationServerMetadata) (*ClientMetadataDocument, error) {
			if documents == nil || documents.ListsRedirectURL == nil || !documents.ListsRedirectURL(redirectURL) {
				return nil, fmt.Errorf(`oauth.clientRegistration "cimd" needs Client ID Metadata Documents that list the redirect URI %s`, redirectURL)
			}
			picked := clientMetadataDocument(options.ServerURL, redirectURL, metadata, documents.BaseURL)
			if picked == nil && options.Settings.ClientRegistration == "cimd" {
				return nil, fmt.Errorf(`The authorization server does not support Client ID Metadata Documents for public clients; remove oauth.clientRegistration "cimd"`)
			}
			return picked, nil
		}
	}
	provider := NewProvider(ProviderOptions{
		ServerURL:              options.ServerURL,
		RedirectURL:            redirectURL,
		ClientMetadata:         metadata,
		ClientMetadataDocument: document,
		ClientID:               options.Settings.ClientID,
		ClientSecret:           options.Settings.ClientSecret,
		Store:                  options.Store,
		OnRedirect:             onRedirect,
	})
	return provider.ClientProvider()
}
