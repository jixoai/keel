// Behaviour ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/src/oauth/sign-in.ts (MIT License); implemented independently
// in Go. Contract kept identical; see docs/UPSTREAM.md.
//
// Sign-in to an OAuth-protected MCP server, in two halves so an application
// can complete it in another request with the same state store:
// StartSignIn refreshes stored tokens when it can, or prepares the browser step
// and returns the authorization URL; CompleteSignIn exchanges the code.

package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultPendingTTL bounds how long a browser step may be completed.
const DefaultPendingTTL = 10 * time.Minute

// SignInOptions are the options of both halves of a sign-in. Pass the same
// values to StartSignIn and CompleteSignIn.
type SignInOptions struct {
	ServerURL string
	// RedirectURL is a loopback redirect URI or an https callback route of the
	// application. Pass the same value to both halves: the authorization request
	// may use a variant specific to the MCP server.
	RedirectURL             string
	ClientName              string
	Settings                Settings
	ClientMetadataDocuments *ClientMetadataDocuments
	Store                   ServerStore
	// Challenge is the server's challenge that asked for the sign-in; its
	// resource metadata URL and scope apply.
	Challenge  *Challenge
	HTTPClient *http.Client
	// Pending records the browser step, so a later request can list or complete
	// it. Nil keeps it only in the state store.
	Pending    PendingStore
	PendingTTL time.Duration
}

// PendingSignIn is a persisted browser step.
type PendingSignIn struct {
	State            string    `json:"state"`
	CodeVerifier     string    `json:"codeVerifier"`
	RedirectURL      string    `json:"redirectUrl"`
	AuthorizationURL string    `json:"authorizationUrl"`
	Scope            string    `json:"scope,omitempty"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

// PendingStore persists browser steps.
type PendingStore interface {
	// StartPending records a new pending authorization, superseding older ones.
	StartPending(ctx context.Context, pending PendingSignIn) error
	// Pending returns the pending authorization with state, without consuming it.
	Pending(ctx context.Context, state string) (*PendingSignIn, error)
	// ConsumePending atomically marks the pending authorization used and returns
	// it, or returns nil when another caller already consumed it. Exactly one
	// concurrent completion can exchange the code.
	ConsumePending(ctx context.Context, state string) (*PendingSignIn, error)
}

// SignInRedirect is the browser step of a sign-in. Plain data, so an
// application can persist it.
type SignInRedirect struct {
	// AuthorizationURL is opened in the user's browser.
	AuthorizationURL string
	// RedirectURL is the redirect URI of the authorization request; the
	// authorization response must arrive on it.
	RedirectURL string
	// State is the `state` parameter the authorization response must carry.
	State string
	// Scope is the scope requested, if any. Saved with the tokens when the
	// token response names none.
	Scope string
	// ExpiresAt bounds completion of this browser step.
	ExpiresAt time.Time
}

// SignInStart is the result of StartSignIn: signed in by refreshing, or the
// browser step.
type SignInStart struct {
	Authorized bool
	Redirect   *SignInRedirect
}

// StartSignIn is the first half of a sign-in. Without a step-up
// (insufficient_scope), it first tries to refresh the stored tokens with the
// stored client. Otherwise it prepares the browser step: a fresh state, and a
// stored client that cannot use RedirectURL is dropped with its tokens.
func StartSignIn(ctx context.Context, options SignInOptions) (SignInStart, error) {
	if ctx.Err() != nil {
		return SignInStart{}, ErrSignInCancelled
	}
	stepUp := options.Challenge != nil && options.Challenge.Error == "insufficient_scope"
	if !stepUp {
		result, err := refreshBeforeSignIn(ctx, options)
		if err != nil {
			return SignInStart{}, err
		}
		if result == FlowAuthorized {
			return SignInStart{Authorized: true}, nil
		}
	}
	redirect, err := startBrowserStep(ctx, options, stepUp)
	if err != nil {
		if ctx.Err() != nil {
			return SignInStart{}, ErrSignInCancelled
		}
		return SignInStart{}, err
	}
	return SignInStart{Redirect: redirect}, nil
}

// RefreshBudget bounds one sign-in refresh: lock wait plus discovery and the
// token request. Without it a stuck authorization server could hold the
// refresh lock forever.
const RefreshBudget = 60 * time.Second

// refreshBeforeSignIn refreshes the stored tokens, unless `cimd` is to replace
// a registered client. A busy refresh lock means "go to the browser step"; any
// other failure fails the sign-in. Tokens are checked once without the lock, so
// the common "no stored grant" sign-in never contends for it.
func refreshBeforeSignIn(ctx context.Context, options SignInOptions) (FlowResult, error) {
	if options.Store == nil {
		return FlowRedirect, nil
	}
	precheck, err := options.Store.Load(ctx)
	if err != nil {
		return "", err
	}
	if precheck == nil || precheck.Tokens == nil || precheck.Tokens.RefreshToken == "" {
		return FlowRedirect, nil
	}
	if options.Settings.ClientRegistration == "cimd" && precheck.ClientInformation != nil && options.Settings.ClientID == "" {
		return FlowRedirect, nil
	}
	type outcome struct {
		authorized bool
		err        error
	}
	results := make(chan outcome, 1)
	go func() {
		authorized := false
		// The refresh itself is not cancellable by the caller: a refresh the
		// server answered must save its rotated tokens. It still has a hard
		// deadline, so a stuck server cannot pin the lock.
		background, cancel := context.WithTimeout(context.WithoutCancel(ctx), RefreshBudget)
		defer cancel()
		err := options.Store.WithRefreshLock(background, func(lockCtx context.Context) error {
			// The lock may arrive after the sign-in was cancelled: return at once,
			// so it is released.
			if ctx.Err() != nil {
				return nil
			}
			if lockCtx.Err() != nil {
				return lockCtx.Err()
			}
			state, err := options.Store.Load(lockCtx)
			if err != nil {
				return err
			}
			if state == nil || state.Tokens == nil || state.Tokens.RefreshToken == "" {
				return nil
			}
			if options.Settings.ClientRegistration == "cimd" && state.ClientInformation != nil && options.Settings.ClientID == "" {
				return nil
			}
			result, err := RefreshStoredTokens(lockCtx, storedProviderOptions(options), state, RefreshOptions{
				Challenge:   options.Challenge,
				RedirectURL: options.RedirectURL,
			})
			if err != nil {
				return err
			}
			authorized = result == FlowAuthorized
			return nil
		})
		results <- outcome{authorized: authorized, err: err}
	}()
	select {
	case result := <-results:
		if result.err != nil {
			if IsRefreshLockBusy(result.err) {
				return FlowRedirect, nil
			}
			return "", result.err
		}
		if result.authorized {
			return FlowAuthorized, nil
		}
		return FlowRedirect, nil
	case <-ctx.Done():
		return "", ErrSignInCancelled
	}
}

func storedProviderOptions(options SignInOptions) StoredAuthProviderOptions {
	return StoredAuthProviderOptions{
		ServerURL:               options.ServerURL,
		ClientName:              options.ClientName,
		Settings:                options.Settings,
		Store:                   options.Store,
		ClientMetadataDocuments: options.ClientMetadataDocuments,
		RedirectURL:             options.RedirectURL,
		HTTPClient:              options.HTTPClient,
	}
}

func startBrowserStep(ctx context.Context, options SignInOptions, stepUp bool) (*SignInRedirect, error) {
	stored, err := options.Store.Load(ctx)
	if err != nil {
		return nil, err
	}
	if stored != nil {
		if err := options.Store.Save(ctx, stateForSignIn(stored, options)); err != nil {
			return nil, err
		}
	}
	var authorizationURL *url.URL
	providerOptions := storedProviderOptions(options)
	provider := NewClientProvider(providerOptions, options.RedirectURL, func(ctx context.Context, value *url.URL) error {
		authorizationURL = value
		return nil
	})
	scope := mergeScopes(options.Settings.Scope, "")
	if stepUp {
		granted := ""
		if stored != nil && stored.Tokens != nil {
			granted = stored.Tokens.Scope
		}
		challenged := ""
		if options.Challenge != nil {
			challenged = options.Challenge.Scope
		}
		scope = mergeScopes(options.Settings.Scope, StepUpScope(granted, challenged))
	} else if options.Challenge != nil {
		scope = mergeScopes(options.Settings.Scope, options.Challenge.Scope)
	}
	flowOptions := FlowOptions{
		ServerURL:                      options.ServerURL,
		Scope:                          scope,
		AuthorizationServerMetadataURL: options.Settings.AuthServerMetadataURL,
		SkipRefresh:                    true,
		HTTPClient:                     options.HTTPClient,
	}
	if options.Challenge != nil {
		flowOptions.ResourceMetadataURL = options.Challenge.ResourceMetadataURL
	}
	if _, err := AuthorizeMCP(ctx, provider, flowOptions); err != nil {
		return nil, err
	}
	if authorizationURL == nil {
		return nil, errors.New("OAuth flow did not produce an authorization URL")
	}
	redirectURL := authorizationURL.Query().Get("redirect_uri")
	if redirectURL == "" {
		redirectURL = options.RedirectURL
	}
	state, err := provider.State(ctx)
	if err != nil {
		return nil, err
	}
	verifier, err := provider.CodeVerifier(ctx)
	if err != nil {
		return nil, err
	}
	ttl := options.PendingTTL
	if ttl <= 0 {
		ttl = DefaultPendingTTL
	}
	redirect := &SignInRedirect{
		AuthorizationURL: authorizationURL.String(),
		RedirectURL:      redirectURL,
		State:            state,
		Scope:            authorizationURL.Query().Get("scope"),
		ExpiresAt:        time.Now().Add(ttl),
	}
	if options.Pending != nil {
		if err := options.Pending.StartPending(ctx, PendingSignIn{
			State:            redirect.State,
			CodeVerifier:     verifier,
			RedirectURL:      redirect.RedirectURL,
			AuthorizationURL: redirect.AuthorizationURL,
			Scope:            redirect.Scope,
			ExpiresAt:        redirect.ExpiresAt,
		}); err != nil {
			return nil, err
		}
	}
	return redirect, nil
}

// stateForSignIn is the stored state for a browser step: without the last
// sign-in's state, and without a client that cannot use redirectURL, along with
// its tokens.
func stateForSignIn(stored *OAuthState, options SignInOptions) *OAuthState {
	next := stored.Clone()
	next.OAuthState = ""
	keepClient := options.Settings.ClientID != ""
	if !keepClient {
		if stored.ClientInformation != nil {
			keepClient = options.Settings.ClientRegistration != "cimd" &&
				contains(RegisteredRedirectURLs(stored.ClientInformation), options.RedirectURL)
		} else {
			keepClient = OffersClientMetadataDocument(options.Settings, options.ClientMetadataDocuments, options.RedirectURL)
		}
	}
	if !keepClient {
		next.ClientInformation = nil
		next.Tokens = nil
		next.TokensExpireAt = 0
	}
	return next
}

func mergeScopes(scopes ...string) string {
	seen := map[string]bool{}
	values := make([]string, 0, 8)
	for _, scope := range scopes {
		for _, word := range strings.Fields(scope) {
			if !seen[word] {
				seen[word] = true
				values = append(values, word)
			}
		}
	}
	return strings.Join(values, " ")
}

// CompleteSignIn is the second half of a sign-in: it exchanges the
// authorization code of response and saves the tokens. Pass the options given
// to StartSignIn, including its RedirectURL (not authorization.RedirectURL).
func CompleteSignIn(ctx context.Context, options SignInOptions, authorization SignInRedirect, response Callback) error {
	err := completeSignIn(ctx, options, authorization, response)
	if err != nil && ctx.Err() != nil {
		return ErrSignInCancelled
	}
	return err
}

func completeSignIn(ctx context.Context, options SignInOptions, authorization SignInRedirect, response Callback) error {
	if response.State != authorization.State {
		return errors.New("The authorization response belongs to a different sign-in")
	}
	var pending *PendingSignIn
	if options.Pending != nil {
		// Consume first: exactly one concurrent callback may exchange the code.
		value, err := options.Pending.ConsumePending(ctx, authorization.State)
		if err != nil {
			return err
		}
		if value == nil {
			// A duplicate callback after a successful exchange is an idempotent
			// success; a callback whose pending row was superseded or expired is not.
			state, loadErr := options.Store.Load(ctx)
			if loadErr != nil {
				return loadErr
			}
			// The stored state still names this sign-in only when it completed;
			// a newer sign-in clears it, so old tokens cannot mask a superseded one.
			if state != nil && state.OAuthState != authorization.State {
				return errors.New("Another sign-in to this MCP server started since; sign in again")
			}
			if state != nil && state.Tokens != nil && state.Tokens.AccessToken != "" {
				return nil
			}
			return ErrSignInInProgress
		}
		if value.RedirectURL != authorization.RedirectURL || value.State != authorization.State {
			return errors.New("The authorization response belongs to a different sign-in")
		}
		if !value.ExpiresAt.IsZero() && time.Now().After(value.ExpiresAt) {
			return errors.New("The authorization request expired; sign in again")
		}
		pending = value
	}
	// The stored PKCE verifier belongs to the latest sign-in; exchanging with it
	// would fail with invalid_grant, which discards the stored tokens.
	state, err := options.Store.Load(ctx)
	if err != nil {
		return err
	}
	if state == nil || state.OAuthState != authorization.State {
		return errors.New("Another sign-in to this MCP server started since; sign in again")
	}
	providerOptions := storedProviderOptions(options)
	provider := NewClientProvider(providerOptions, options.RedirectURL, nil)
	if pending != nil && pending.CodeVerifier != "" {
		verifier := pending.CodeVerifier
		provider.CodeVerifier = func(context.Context) (string, error) { return verifier, nil }
	}
	flowOptions := FlowOptions{
		ServerURL:                      options.ServerURL,
		Scope:                          authorization.Scope,
		AuthorizationServerMetadataURL: options.Settings.AuthServerMetadataURL,
		AuthorizationRedirectURL:       &authorization.RedirectURL,
		AuthorizationCode:              response.Code,
		HTTPClient:                     options.HTTPClient,
	}
	if options.Challenge != nil {
		flowOptions.ResourceMetadataURL = options.Challenge.ResourceMetadataURL
	}
	if response.ISS != "" {
		iss := response.ISS
		flowOptions.ISS = &iss
	}
	if _, err := AuthorizeMCP(ctx, provider, flowOptions); err != nil {
		return err
	}
	return nil
}

// ResponseFromRedirectURL reads the authorization response from a redirect URL
// the user pasted from the browser address bar. It fails when the URL is not on
// the sign-in's redirect URI, carries another state, or reports an error.
func ResponseFromRedirectURL(input string, authorization SignInRedirect) (Callback, error) {
	parsed, err := url.Parse(strings.TrimSpace(input))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return Callback{}, errors.New("Expected the full redirect URL from the browser address bar")
	}
	redirectURL, err := url.Parse(authorization.RedirectURL)
	if err != nil {
		return Callback{}, fmt.Errorf("Invalid sign-in redirect URI %s", authorization.RedirectURL)
	}
	// A server-specific redirect URI tells authorization servers apart, so it
	// must match exactly.
	if parsed.Scheme != redirectURL.Scheme || parsed.Host != redirectURL.Host || parsed.Path != redirectURL.Path {
		return Callback{}, errors.New("The redirect URL does not match this sign-in's redirect URI")
	}
	query := parsed.Query()
	if errorCode := query.Get("error"); errorCode != "" {
		description := query.Get("error_description")
		if description == "" {
			description = errorCode
		}
		return Callback{}, errors.New(description)
	}
	state := query.Get("state")
	if state != authorization.State {
		return Callback{}, errors.New("The redirect URL belongs to a different sign-in")
	}
	code := query.Get("code")
	if code == "" {
		return Callback{}, errors.New("The redirect URL does not contain an authorization code")
	}
	return Callback{Code: code, State: state, ISS: query.Get("iss")}, nil
}
