// Behaviour ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/src/oauth/flow.ts (MIT License); implemented independently in
// Go. Contract kept identical; see docs/UPSTREAM.md.
//
// Textual OAuth flow: discovery, dynamic client registration, PKCE
// authorization, code exchange, refresh, and step-up scope. It is transport
// agnostic: callers supply the provider callbacks that persist state and open
// the authorization URL.

package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// requestTimeout bounds one request to the authorization server, so an
// unresponsive one cannot hold the refresh lock or a sign-in.
const requestTimeout = 15 * time.Second

const tokenBodyLimit = 256 << 10

// AddClientAuthentication customizes client authentication on a token request.
// When nil, client_id/client_secret are applied per the server's supported
// methods.
type AddClientAuthentication func(headers http.Header, params url.Values, endpoint string, metadata *AuthorizationServerMetadata) error

// ClientProvider is the application side of the flow. Optional callbacks are
// nil when the application does not support that part, mirroring PI's optional
// methods.
type ClientProvider struct {
	RedirectURL    string
	ClientMetadata ClientMetadata

	// ClientMetadataDocument returns the document to identify as instead of
	// registering, or nil to register. It is only called when no client
	// information is stored.
	ClientMetadataDocument  func(metadata *AuthorizationServerMetadata) (*ClientMetadataDocument, error)
	State                   func(ctx context.Context) (string, error)
	ClientInformation       func(ctx context.Context) (*ClientInformation, error)
	SaveClientInformation   func(ctx context.Context, information ClientInformation) error
	Tokens                  func(ctx context.Context) (*Tokens, error)
	SaveTokens              func(ctx context.Context, tokens Tokens) error
	RedirectToAuthorization func(ctx context.Context, authorizationURL *url.URL) error
	SaveCodeVerifier        func(ctx context.Context, verifier string) error
	CodeVerifier            func(ctx context.Context) (string, error)
	AddClientAuthentication AddClientAuthentication
	InvalidateCredentials   func(ctx context.Context, kind InvalidateKind) error
	SaveDiscoveryState      func(ctx context.Context, state DiscoveryState) error
	DiscoveryState          func(ctx context.Context) (*DiscoveryState, error)
}

// InvalidateKind names the stored credentials to drop.
type InvalidateKind string

const (
	InvalidateAll       InvalidateKind = "all"
	InvalidateClient    InvalidateKind = "client"
	InvalidateTokens    InvalidateKind = "tokens"
	InvalidateVerifier  InvalidateKind = "verifier"
	InvalidateDiscovery InvalidateKind = "discovery"
)

func (provider *ClientProvider) clientInformation(ctx context.Context) (*ClientInformation, error) {
	if provider.ClientInformation == nil {
		return nil, nil
	}
	return provider.ClientInformation(ctx)
}

func (provider *ClientProvider) tokens(ctx context.Context) (*Tokens, error) {
	if provider.Tokens == nil {
		return nil, nil
	}
	return provider.Tokens(ctx)
}

func (provider *ClientProvider) saveClientInformation(ctx context.Context, information ClientInformation) error {
	if provider.SaveClientInformation == nil {
		return nil
	}
	return provider.SaveClientInformation(ctx, information)
}

func (provider *ClientProvider) saveTokens(ctx context.Context, tokens Tokens) error {
	if provider.SaveTokens == nil {
		return errors.New("OAuth tokens cannot be persisted")
	}
	return provider.SaveTokens(ctx, tokens)
}

func (provider *ClientProvider) saveCodeVerifier(ctx context.Context, verifier string) error {
	if provider.SaveCodeVerifier == nil {
		return errors.New("OAuth code verifier cannot be persisted")
	}
	return provider.SaveCodeVerifier(ctx, verifier)
}

func (provider *ClientProvider) codeVerifier(ctx context.Context) (string, error) {
	if provider.CodeVerifier == nil {
		return "", errors.New("No OAuth PKCE code verifier is stored")
	}
	verifier, err := provider.CodeVerifier(ctx)
	if err != nil {
		return "", err
	}
	if verifier == "" {
		return "", errors.New("No OAuth PKCE code verifier is stored")
	}
	return verifier, nil
}

func (provider *ClientProvider) invalidate(ctx context.Context, kind InvalidateKind) error {
	if provider.InvalidateCredentials == nil {
		return nil
	}
	return provider.InvalidateCredentials(ctx, kind)
}

// FlowOptions configures one authorization flow.
type FlowOptions struct {
	ServerURL         string
	AuthorizationCode string
	// ISS is the `iss` parameter of the authorization response (RFC 9207).
	ISS                 *string
	Scope               string
	ResourceMetadataURL string
	// AuthorizationServerMetadataURL is trusted as configured. It must use
	// https, except on loopback.
	AuthorizationServerMetadataURL string
	HTTPClient                     *http.Client
	SkipIssuerValidation           bool
	// SkipRefresh goes straight to the authorization redirect instead of
	// refreshing stored tokens, for a step-up authorization.
	SkipRefresh bool
	// RefreshOnly only refreshes stored tokens; it returns FlowRedirect instead
	// of registering a client or preparing an authorization request, so a
	// pending authorization stays usable.
	RefreshOnly bool
	// AuthorizationRedirectURL is the redirect URI the authorization request
	// used. With AuthorizationCode the exchange fails before sending the code
	// when the flow would send another one.
	AuthorizationRedirectURL *string
}

// FlowResult reports whether the flow authorized or needs a browser step.
type FlowResult string

const (
	FlowAuthorized FlowResult = "AUTHORIZED"
	FlowRedirect   FlowResult = "REDIRECT"
)

// TokenRequestOptions configures a token request.
type TokenRequestOptions struct {
	Metadata                *AuthorizationServerMetadata
	ClientInformation       ClientInformation
	Resource                string
	AddClientAuthentication AddClientAuthentication
	HTTPClient              *http.Client
}

type clientAuthMethod string

const (
	clientSecretBasic clientAuthMethod = "client_secret_basic"
	clientSecretPost  clientAuthMethod = "client_secret_post"
	clientAuthNone    clientAuthMethod = "none"
)

func secureEndpoint(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("Invalid OAuth endpoint %s", value)
	}
	if parsed.Scheme != "https" && !isLoopbackHost(parsed.Hostname()) {
		return nil, &InsecureEndpointError{Endpoint: parsed.String()}
	}
	return parsed, nil
}

func selectClientAuthMethod(information ClientInformation, supported []string) clientAuthMethod {
	hinted := information.TokenEndpointAuthMethod
	if hinted != "" &&
		(hinted == string(clientSecretBasic) || hinted == string(clientSecretPost) || hinted == string(clientAuthNone)) &&
		(len(supported) == 0 || contains(supported, hinted)) {
		return clientAuthMethod(hinted)
	}
	hasSecret := information.ClientSecret != ""
	if len(supported) == 0 {
		if hasSecret {
			return clientSecretBasic
		}
		return clientAuthNone
	}
	if hasSecret && contains(supported, string(clientSecretBasic)) {
		return clientSecretBasic
	}
	if hasSecret && contains(supported, string(clientSecretPost)) {
		return clientSecretPost
	}
	if contains(supported, string(clientAuthNone)) {
		return clientAuthNone
	}
	if hasSecret {
		return clientSecretPost
	}
	return clientAuthNone
}

func applyClientAuthentication(method clientAuthMethod, information ClientInformation, headers http.Header, params url.Values) error {
	if method == clientSecretBasic {
		if information.ClientSecret == "" {
			return errors.New("client_secret_basic requires a client secret")
		}
		credentials := information.ClientID + ":" + information.ClientSecret
		headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(credentials)))
		return nil
	}
	params.Set("client_id", information.ClientID)
	if method == clientSecretPost && information.ClientSecret != "" {
		params.Set("client_secret", information.ClientSecret)
	}
	return nil
}

// StartAuthorization prepares the browser step: a PKCE verifier and the
// authorization URL.
func StartAuthorization(authorizationServerURL string, metadata *AuthorizationServerMetadata, clientInformation ClientInformation, redirectURL, scope, state, resource string) (*url.URL, string, error) {
	if metadata != nil && !contains(metadata.ResponseTypesSupported, "code") {
		return nil, "", errors.New("Authorization server does not support authorization codes")
	}
	if metadata != nil && metadata.CodeChallengeMethodsSupported != nil && !contains(metadata.CodeChallengeMethodsSupported, "S256") {
		return nil, "", errors.New("Authorization server does not support PKCE S256")
	}
	endpoint := ""
	if metadata != nil && metadata.AuthorizationEndpoint != "" {
		endpoint = metadata.AuthorizationEndpoint
	} else {
		parsed, err := url.Parse(authorizationServerURL)
		if err != nil {
			return nil, "", fmt.Errorf("Invalid authorization server URL %s", authorizationServerURL)
		}
		endpoint = parsed.ResolveReference(&url.URL{Path: "/authorize"}).String()
	}
	authorizationURL, err := url.Parse(endpoint)
	if err != nil || authorizationURL.Host == "" || !IsHTTPURL(authorizationURL) {
		return nil, "", fmt.Errorf("Invalid authorization endpoint %s", endpoint)
	}
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		return nil, "", err
	}
	query := authorizationURL.Query()
	query.Set("response_type", "code")
	query.Set("client_id", clientInformation.ClientID)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	query.Set("redirect_uri", redirectURL)
	if state != "" {
		query.Set("state", state)
	}
	if scope != "" {
		query.Set("scope", scope)
	}
	if hasWord(scope, "offline_access") {
		query.Set("prompt", "consent")
	}
	if resource != "" {
		query.Set("resource", resource)
	}
	authorizationURL.RawQuery = query.Encode()
	return authorizationURL, verifier, nil
}

func hasWord(list, word string) bool {
	for _, value := range strings.Fields(list) {
		if value == word {
			return true
		}
	}
	return false
}

func resolveEndpoint(base string, metadata *AuthorizationServerMetadata, field, fallbackPath string) (string, error) {
	if metadata != nil {
		switch field {
		case "authorization":
			if metadata.AuthorizationEndpoint != "" {
				return metadata.AuthorizationEndpoint, nil
			}
		case "token":
			if metadata.TokenEndpoint != "" {
				return metadata.TokenEndpoint, nil
			}
		case "registration":
			if metadata.RegistrationEndpoint != "" {
				return metadata.RegistrationEndpoint, nil
			}
		}
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("Invalid authorization server URL %s", base)
	}
	return parsed.ResolveReference(&url.URL{Path: fallbackPath}).String(), nil
}

func requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, requestTimeout)
}

// doRequest performs one HTTP request with the flow's per-request timeout and
// body limit, returning the status and body.
func doRequest(ctx context.Context, client *http.Client, request *http.Request) (int, []byte, error) {
	requestCtx, cancel := requestContext(ctx)
	defer cancel()
	request = request.WithContext(requestCtx)
	response, err := clientOr(client).Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, tokenBodyLimit))
	if err != nil {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, body, nil
}

// authorizationSecret returns the base64 part of a client_secret_basic header,
// so a server echoing the Authorization header cannot leak it into an error.
func authorizationSecret(headers http.Header) []string {
	value := headers.Get("Authorization")
	if value == "" {
		return nil
	}
	return []string{value, strings.TrimPrefix(value, "Basic ")}
}

// redactSecrets removes request credentials from a response excerpt before it
// is embedded in an error a host may log.
func redactSecrets(text string, secrets ...string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		text = strings.ReplaceAll(text, secret, "[REDACTED]")
	}
	return text
}

func tokenRequest(ctx context.Context, authorizationServerURL string, options TokenRequestOptions, params url.Values) (Tokens, error) {
	endpoint, err := resolveEndpoint(authorizationServerURL, options.Metadata, "token", "/token")
	if err != nil {
		return Tokens{}, err
	}
	secure, err := secureEndpoint(endpoint)
	if err != nil {
		return Tokens{}, err
	}
	headers := http.Header{"Accept": {"application/json"}, "Content-Type": {"application/x-www-form-urlencoded"}}
	if options.Resource != "" {
		params.Set("resource", options.Resource)
	}
	if options.AddClientAuthentication != nil {
		if err := options.AddClientAuthentication(headers, params, secure.String(), options.Metadata); err != nil {
			return Tokens{}, err
		}
	} else {
		supported := []string(nil)
		if options.Metadata != nil {
			supported = options.Metadata.TokenEndpointAuthMethodsSupported
		}
		if err := applyClientAuthentication(selectClientAuthMethod(options.ClientInformation, supported), options.ClientInformation, headers, params); err != nil {
			return Tokens{}, err
		}
	}
	request, err := http.NewRequest(http.MethodPost, secure.String(), strings.NewReader(params.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	request.Header = headers
	status, body, err := doRequest(ctx, options.HTTPClient, request)
	if err != nil {
		return Tokens{}, err
	}
	var value map[string]json.RawMessage
	_ = json.Unmarshal(body, &value)
	if value != nil {
		var errorCode string
		if raw, ok := value["error"]; ok {
			_ = json.Unmarshal(raw, &errorCode)
		}
		if errorCode != "" {
			description := errorCode
			if raw, ok := value["error_description"]; ok {
				_ = json.Unmarshal(raw, &description)
			}
			errorURI := ""
			if raw, ok := value["error_uri"]; ok {
				_ = json.Unmarshal(raw, &errorURI)
			}
			// Error bodies are logged by hosts: never let a secret the request
			// sent (or a server echoes) appear in them.
			redact := func(text string) string {
				return redactSecrets(text, append([]string{
					params.Get("client_secret"), params.Get("code_verifier"),
					params.Get("refresh_token"), params.Get("code"),
				}, authorizationSecret(headers)...)...)
			}
			return Tokens{}, &Error{Code: errorCode, Message: redact(description), ErrorURI: errorURI, HTTPStatus: status, ResponseBody: redact(string(body))}
		}
	}
	if status < 200 || status >= 300 {
		redact := func(text string) string {
			return redactSecrets(text, append([]string{
				params.Get("client_secret"), params.Get("code_verifier"),
				params.Get("refresh_token"), params.Get("code"),
			}, authorizationSecret(headers)...)...)
		}
		return Tokens{}, &Error{Code: "server_error", Message: fmt.Sprintf("HTTP %d: %s", status, redact(string(body))), HTTPStatus: status, ResponseBody: redact(string(body))}
	}
	return ParseTokens(body)
}

// RegisterClient performs RFC 7591 dynamic client registration.
func RegisterClient(ctx context.Context, authorizationServerURL string, metadata *AuthorizationServerMetadata, clientMetadata ClientMetadata, scope string, client *http.Client) (ClientInformation, error) {
	if metadata != nil && metadata.RegistrationEndpoint == "" {
		return ClientInformation{}, errors.New("Authorization server does not support dynamic client registration")
	}
	endpoint, err := resolveEndpoint(authorizationServerURL, metadata, "registration", "/register")
	if err != nil {
		return ClientInformation{}, err
	}
	if err := validateHTTPURL(endpoint); err != nil {
		return ClientInformation{}, err
	}
	body, err := marshalClientMetadata(clientMetadata, scope)
	if err != nil {
		return ClientInformation{}, err
	}
	request, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return ClientInformation{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	status, responseBody, err := doRequest(ctx, client, request)
	if err != nil {
		return ClientInformation{}, err
	}
	if status < 200 || status >= 300 {
		return ClientInformation{}, &RegistrationError{Status: status, Body: string(responseBody)}
	}
	return ParseClientInformation(responseBody)
}

// ExchangeAuthorizationCode exchanges an authorization code plus PKCE verifier.
func ExchangeAuthorizationCode(ctx context.Context, authorizationServerURL string, options TokenRequestOptions, code, codeVerifier, redirectURL string) (Tokens, error) {
	params := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {codeVerifier},
		"redirect_uri":  {redirectURL},
	}
	return tokenRequest(ctx, authorizationServerURL, options, params)
}

// RefreshAuthorization refreshes a grant. A response without a new refresh
// token keeps the old one.
func RefreshAuthorization(ctx context.Context, authorizationServerURL string, options TokenRequestOptions, refreshToken string) (Tokens, error) {
	tokens, err := tokenRequest(ctx, authorizationServerURL, options, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
	if err != nil {
		return Tokens{}, err
	}
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = refreshToken
	}
	return tokens, nil
}

func withScope(tokens Tokens, scope string) Tokens {
	if tokens.Scope == "" && scope != "" {
		tokens.Scope = scope
	}
	return tokens
}

// StepUpScope combines the granted scopes with the challenged ones: a challenge
// may list only the missing scopes, and a token with just those would lose
// access the old one had (SEP-2350). Without challenged scopes it returns "".
func StepUpScope(granted, challenged string) string {
	if challenged == "" {
		return ""
	}
	seen := map[string]bool{}
	values := make([]string, 0, 8)
	for _, scope := range []string{granted, challenged} {
		for _, word := range strings.Fields(scope) {
			if !seen[word] {
				seen[word] = true
				values = append(values, word)
			}
		}
	}
	return strings.Join(values, " ")
}

type flowUse struct {
	storedClientID string
}

func runFlow(ctx context.Context, provider *ClientProvider, options FlowOptions, use *flowUse) (FlowResult, error) {
	metadataURL := ""
	if options.AuthorizationServerMetadataURL != "" {
		secure, err := secureEndpoint(options.AuthorizationServerMetadataURL)
		if err != nil {
			return "", err
		}
		metadataURL = secure.String()
	}
	discoveryOptions := DiscoveryOptions{
		ResourceMetadataURL:            options.ResourceMetadataURL,
		AuthorizationServerMetadataURL: metadataURL,
		SkipIssuerValidation:           options.SkipIssuerValidation,
		HTTPClient:                     options.HTTPClient,
	}
	var cached *DiscoveryState
	if metadataURL == "" && provider.DiscoveryState != nil {
		value, err := provider.DiscoveryState(ctx)
		if err != nil {
			return "", err
		}
		cached = value
	}
	var discovered ServerInfo
	if cached != nil && cached.AuthorizationServerURL != "" {
		discovered.AuthorizationServerURL = cached.AuthorizationServerURL
		discovered.ResourceMetadata = cached.ResourceMetadata
		if cached.AuthorizationServerMetadata != nil {
			discovered.AuthorizationServerMetadata = cached.AuthorizationServerMetadata
		} else {
			metadata, err := DiscoverAuthorizationServerMetadata(ctx, cached.AuthorizationServerURL, discoveryOptions)
			if err != nil {
				return "", err
			}
			discovered.AuthorizationServerMetadata = metadata
		}
	} else {
		info, err := DiscoverOAuthServerInfo(ctx, options.ServerURL, discoveryOptions)
		if err != nil {
			return "", err
		}
		discovered = info
	}
	if metadataURL == "" && provider.SaveDiscoveryState != nil {
		state := DiscoveryState{
			AuthorizationServerURL:      discovered.AuthorizationServerURL,
			AuthorizationServerMetadata: discovered.AuthorizationServerMetadata,
			ResourceMetadata:            discovered.ResourceMetadata,
		}
		if options.ResourceMetadataURL != "" {
			state.ResourceMetadataURL = options.ResourceMetadataURL
		}
		if err := provider.SaveDiscoveryState(ctx, state); err != nil {
			return "", err
		}
	}
	metadata := discovered.AuthorizationServerMetadata
	resource, err := SelectResource(options.ServerURL, discovered.ResourceMetadata)
	if err != nil {
		return "", err
	}
	scope := options.Scope
	if scope == "" && discovered.ResourceMetadata != nil {
		scope = strings.Join(discovered.ResourceMetadata.ScopesSupported, " ")
	}
	if scope == "" {
		scope = provider.ClientMetadata.Scope
	}
	stored, err := provider.clientInformation(ctx)
	if err != nil {
		return "", err
	}
	if stored != nil {
		use.storedClientID = stored.ClientID
	}
	var document *ClientMetadataDocument
	if stored == nil && provider.ClientMetadataDocument != nil {
		document, err = provider.ClientMetadataDocument(metadata)
		if err != nil {
			return "", err
		}
	}
	if document != nil {
		parsed, err := url.Parse(document.URL)
		if err != nil || parsed.Scheme != "https" || parsed.Path == "/" || parsed.Path == "" {
			return "", ErrClientMetadataURL
		}
	}
	redirectURL := provider.RedirectURL
	if document != nil {
		redirectURL = document.RedirectURL
	}
	if options.AuthorizationCode != "" && options.AuthorizationRedirectURL != nil && redirectURL != *options.AuthorizationRedirectURL {
		return "", fmt.Errorf("The code exchange would use the redirect URI %s, but the authorization request used %s",
			redirectURL, *options.AuthorizationRedirectURL)
	}
	var client *ClientInformation
	if stored != nil {
		client = stored
	} else if document != nil {
		client = &ClientInformation{ClientID: document.URL}
	}
	if client == nil {
		if options.RefreshOnly {
			return FlowRedirect, nil
		}
		if options.AuthorizationCode != "" {
			return "", errors.New("OAuth client information is missing during code exchange: the authorization request's client was removed, or the client settings changed")
		}
		if provider.SaveClientInformation == nil {
			return "", errors.New("OAuth client information cannot be persisted")
		}
		registered, err := RegisterClient(ctx, discovered.AuthorizationServerURL, metadata, provider.ClientMetadata, scope, options.HTTPClient)
		if err != nil {
			return "", err
		}
		if err := provider.saveClientInformation(ctx, registered); err != nil {
			return "", err
		}
		client = &registered
	}
	tokenOptions := TokenRequestOptions{
		Metadata:                metadata,
		ClientInformation:       *client,
		Resource:                resource,
		AddClientAuthentication: provider.AddClientAuthentication,
		HTTPClient:              options.HTTPClient,
	}
	if options.AuthorizationCode != "" {
		// RFC 9207: never send a code from another authorization server to this
		// one. Without metadata there is no issuer to compare with, so any `iss`
		// is rejected.
		if options.ISS != nil || (metadata != nil && metadata.AuthorizationResponseIssParameter) {
			if options.ISS == nil || metadata == nil || *options.ISS != metadata.Issuer {
				var expected *string
				var expectedValue string
				if metadata != nil {
					expectedValue = metadata.Issuer
					expected = &expectedValue
				}
				return "", &IssuerMismatchError{Expected: expected, Received: options.ISS}
			}
		}
		verifier, err := provider.codeVerifier(ctx)
		if err != nil {
			return "", err
		}
		tokens, err := ExchangeAuthorizationCode(ctx, discovered.AuthorizationServerURL, tokenOptions, options.AuthorizationCode, verifier, redirectURL)
		if err != nil {
			return "", err
		}
		if err := provider.saveTokens(ctx, withScope(tokens, scope)); err != nil {
			return "", err
		}
		return FlowAuthorized, nil
	}
	var existing *Tokens
	if !options.SkipRefresh {
		existing, err = provider.tokens(ctx)
		if err != nil {
			return "", err
		}
	}
	if existing != nil && existing.RefreshToken != "" {
		tokens, err := RefreshAuthorization(ctx, discovered.AuthorizationServerURL, tokenOptions, existing.RefreshToken)
		if err == nil {
			if err := provider.saveTokens(ctx, withScope(tokens, existing.Scope)); err != nil {
				return "", err
			}
			return FlowAuthorized, nil
		}
		if ctx.Err() != nil {
			return "", err
		}
		var insecure *InsecureEndpointError
		if errors.As(err, &insecure) {
			return "", err
		}
		var oauthErr *Error
		if errors.As(err, &oauthErr) && oauthErr.Code != "server_error" {
			return "", err
		}
	}
	if options.RefreshOnly {
		return FlowRedirect, nil
	}
	state := ""
	if provider.State != nil {
		state, err = provider.State(ctx)
		if err != nil {
			return "", err
		}
	}
	authorizationURL, verifier, err := StartAuthorization(discovered.AuthorizationServerURL, metadata, *client, redirectURL, scope, state, resource)
	if err != nil {
		return "", err
	}
	if err := provider.saveCodeVerifier(ctx, verifier); err != nil {
		return "", err
	}
	if provider.RedirectToAuthorization != nil {
		if err := provider.RedirectToAuthorization(ctx, authorizationURL); err != nil {
			return "", err
		}
	}
	return FlowRedirect, nil
}

// AuthorizeMCP runs the flow. A rejected stored client is invalidated and the
// flow retried, so a server that forgot a registration recovers on its own.
func AuthorizeMCP(ctx context.Context, provider *ClientProvider, options FlowOptions) (FlowResult, error) {
	use := &flowUse{}
	result, err := runFlow(ctx, provider, options, use)
	if err == nil {
		return result, nil
	}
	var oauthErr *Error
	if errors.As(err, &oauthErr) {
		// A code exchange never invalidates stored credentials: a concurrent
		// callback may already have saved the winner's tokens, and a failed
		// step-up must keep the working grant.
		if options.AuthorizationCode != "" {
			return "", err
		}
		switch oauthErr.Code {
		case "invalid_client", "unauthorized_client":
			// A refresh keeps a pending authorization's state and verifier: they
			// are the user's sign-in. A client stored meanwhile (by a sign-in that
			// completed) is not the one the server rejected, so it stays.
			if options.RefreshOnly {
				current, loadErr := provider.clientInformation(ctx)
				if loadErr == nil && (current == nil || current.ClientID == use.storedClientID) {
					_ = provider.invalidate(ctx, InvalidateClient)
					_ = provider.invalidate(ctx, InvalidateTokens)
					_ = provider.invalidate(ctx, InvalidateDiscovery)
				}
			} else {
				_ = provider.invalidate(ctx, InvalidateAll)
			}
			return runFlow(ctx, provider, options, &flowUse{})
		case "invalid_grant":
			_ = provider.invalidate(ctx, InvalidateTokens)
			return runFlow(ctx, provider, options, &flowUse{})
		}
	}
	return "", err
}

// UnauthorizedEvent describes an authorization failure handed to an
// AuthProvider. It mirrors mcp.UnauthorizedEvent while keeping the oauth
// package independent of the transport package.
type UnauthorizedEvent struct {
	Status          int
	WWWAuthenticate string
	Body            string
	ServerURL       string
	// Token is the access token the rejected request carried, if any.
	Token string
	// HTTPClient is the client of the rejected request, so flow requests share
	// its transport when the provider was not configured with one.
	HTTPClient *http.Client
}

// AuthProvider supplies bearer tokens to the MCP transport.
type AuthProvider interface {
	Token(ctx context.Context) (string, error)
	OnUnauthorized(ctx context.Context, event UnauthorizedEvent) error
}

// AdaptOAuthProvider adapts a ClientProvider to the transport: after a 401 it
// refreshes the tokens, or fails with AuthorizationRequiredError when the user
// has to authorize (again). Concurrent 401s share one refresh, and a request
// whose token was already replaced is just retried.
func AdaptOAuthProvider(provider *ClientProvider) AuthProvider {
	return AdaptOAuthProviderWithClient(provider, nil)
}

// AdaptOAuthProviderWithClient is AdaptOAuthProvider with an HTTP client for
// authorization-server requests (the transport's client usually comes with the
// UnauthorizedEvent; this one applies when it does not).
func AdaptOAuthProviderWithClient(provider *ClientProvider, client *http.Client) AuthProvider {
	return &adaptedProvider{provider: provider, client: client}
}

type adaptedProvider struct {
	provider *ClientProvider
	client   *http.Client

	mu       sync.Mutex
	inFlight *unauthorizedCall
}

type unauthorizedCall struct {
	done chan struct{}
	err  error
}

func (a *adaptedProvider) Token(ctx context.Context) (string, error) {
	tokens, err := a.provider.tokens(ctx)
	if err != nil || tokens == nil {
		return "", err
	}
	return tokens.AccessToken, nil
}

func (a *adaptedProvider) OnUnauthorized(ctx context.Context, event UnauthorizedEvent) error {
	challenge := ParseWWWAuthenticate(event.WWWAuthenticate)
	insufficientScope := challenge.Error == "insufficient_scope"
	a.mu.Lock()
	if !insufficientScope && a.inFlight == nil && event.Token != "" {
		current, err := a.provider.tokens(ctx)
		if err != nil {
			a.mu.Unlock()
			return err
		}
		if current != nil && current.AccessToken != "" && current.AccessToken != event.Token {
			a.mu.Unlock()
			return nil
		}
	}
	if a.inFlight != nil {
		call := a.inFlight
		a.mu.Unlock()
		<-call.done
		return call.err
	}
	call := &unauthorizedCall{done: make(chan struct{})}
	a.inFlight = call
	a.mu.Unlock()

	var granted *Tokens
	if insufficientScope {
		value, err := a.provider.tokens(ctx)
		if err != nil {
			call.err = err
		} else {
			granted = value
		}
	}
	if call.err == nil {
		scope := challenge.Scope
		if insufficientScope {
			grantedScope := ""
			if granted != nil {
				grantedScope = granted.Scope
			}
			scope = StepUpScope(grantedScope, challenge.Scope)
		}
		client := event.HTTPClient
		if client == nil {
			client = a.client
		}
		result, err := AuthorizeMCP(ctx, a.provider, FlowOptions{
			ServerURL:           event.ServerURL,
			ResourceMetadataURL: challenge.ResourceMetadataURL,
			Scope:               scope,
			SkipRefresh:         insufficientScope,
			HTTPClient:          client,
		})
		switch {
		case err != nil:
			call.err = err
		case result == FlowRedirect:
			call.err = &AuthorizationRequiredError{}
		}
	}
	a.mu.Lock()
	a.inFlight = nil
	a.mu.Unlock()
	close(call.done)
	return call.err
}
