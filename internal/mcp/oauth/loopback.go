// Behaviour ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/src/oauth/loopback.ts (MIT License); implemented independently
// in Go. Contract kept identical; see docs/UPSTREAM.md.
//
// How the authorization response of a sign-in reaches a local application: a
// loopback callback server the browser redirects to, or a redirect URL the user
// pastes from the browser address bar.

package oauth

import (
	"context"
	"net/url"
	"strconv"
	"strings"
)

// LoopbackCallback is a loopback callback server and the redirect URI it
// serves.
type LoopbackCallback struct {
	Callback    *CallbackServer
	RedirectURL string
}

// LoopbackListenOptions configures ListenForLoopbackSignIn.
type LoopbackListenOptions struct {
	ServerURL               string
	Store                   StateStore
	Settings                Settings
	ClientMetadataDocuments *ClientMetadataDocuments
	// RenderPage renders the browser page after the redirect.
	RenderPage func(page CallbackPage) string
}

// ListenForLoopbackSignIn listens for the authorization response of a sign-in
// on loopback, where settings say: CallbackURL and CallbackPort, else the port
// of the stored client's redirect URI (so that client stays valid), or a free
// one. Close the callback server when the sign-in ends.
func ListenForLoopbackSignIn(options LoopbackListenOptions) (*LoopbackCallback, error) {
	loopback := LoopbackSettingsOf(options.Settings)
	port := 0
	portConfigured := false
	if loopback.Port != nil {
		port = *loopback.Port
		portConfigured = true
	} else if options.Store != nil {
		state, err := options.Store.Load(context.Background())
		if err != nil {
			return nil, err
		}
		if state != nil {
			if registered := RegisteredRedirectURLs(state.ClientInformation); len(registered) > 0 {
				if parsed, err := url.Parse(registered[0]); err == nil && parsed.Port() != "" {
					if value, err := strconv.Atoi(parsed.Port()); err == nil {
						port = value
					}
				}
			}
		}
	}
	extraPaths := []string(nil)
	if OffersClientMetadataDocument(options.Settings, options.ClientMetadataDocuments, loopback.URL) {
		extraPaths = append(extraPaths, ServerSpecificPath(loopback.Path, options.ServerURL))
	}
	listenOptions := CallbackServerOptions{
		Host:         loopback.Host,
		RedirectHost: loopback.RedirectHost,
		Path:         loopback.Path,
		ExtraPaths:   extraPaths,
		RenderPage:   options.RenderPage,
	}
	listenOptions.Port = port
	callback, err := ListenCallbackServer(listenOptions)
	if err != nil {
		// A configured port is required; the stored client's port is only
		// preferred.
		if portConfigured || port == 0 {
			return nil, err
		}
		listenOptions.Port = 0
		callback, err = ListenCallbackServer(listenOptions)
		if err != nil {
			return nil, err
		}
	}
	redirectURL := loopback.FixedRedirectURL
	if redirectURL == "" {
		redirectURL = callback.RedirectURL()
	}
	return &LoopbackCallback{Callback: callback, RedirectURL: redirectURL}, nil
}

// LoopbackWaitOptions configures WaitForLoopbackSignIn.
type LoopbackWaitOptions struct {
	Callback *CallbackServer
	// Authorization is the browser step of StartSignIn.
	Authorization SignInRedirect
	// PromptForRedirectURL asks for the redirect URL, for when the browser cannot
	// reach the loopback callback (for example over SSH). Resolving to "" cancels
	// the sign-in.
	PromptForRedirectURL func(ctx context.Context) (string, error)
	Context              context.Context
}

// WaitForLoopbackSignIn waits for the authorization response on a loopback
// callback server, or for a redirect URL the user pastes, whichever comes
// first. Cancelling the context stops the wait with ErrSignInCancelled.
func WaitForLoopbackSignIn(options LoopbackWaitOptions) (Callback, error) {
	parent := options.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	redirectURL, err := url.Parse(options.Authorization.RedirectURL)
	if err != nil {
		return Callback{}, err
	}
	future, err := options.Callback.WaitForCallback(options.Authorization.State, redirectURL.Path)
	if err != nil {
		return Callback{}, err
	}
	results := make(chan CallbackResult, 2)
	go func() {
		callback, err := future.Await(ctx)
		results <- CallbackResult{Callback: callback, Err: err}
	}()
	if options.PromptForRedirectURL != nil {
		go func() {
			input, err := options.PromptForRedirectURL(ctx)
			if err != nil {
				results <- CallbackResult{Err: err}
				return
			}
			if strings.TrimSpace(input) == "" {
				results <- CallbackResult{Err: ErrSignInCancelled}
				return
			}
			callback, err := ResponseFromRedirectURL(input, options.Authorization)
			results <- CallbackResult{Callback: callback, Err: err}
		}()
	}
	select {
	case result := <-results:
		if result.Err != nil && parent.Err() != nil {
			return Callback{}, ErrSignInCancelled
		}
		return result.Callback, result.Err
	case <-parent.Done():
		return Callback{}, ErrSignInCancelled
	}
}
