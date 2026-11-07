// Behaviour ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/src/oauth/provider.ts (MIT License); implemented independently
// in Go. Contract kept identical; see docs/UPSTREAM.md.
//
// The stateful OAuth client of one exact MCP server URL: a plain state
// document, a storage interface applications can back with PostgreSQL, and the
// ClientProvider callbacks the flow uses. Stored state for another server URL
// is ignored so credentials never leak across servers.

package oauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"
)

// OAuthState is one server's stored OAuth state.
type OAuthState struct {
	ServerURL         string             `json:"serverUrl"`
	ClientInformation *ClientInformation `json:"clientInformation,omitempty"`
	Tokens            *Tokens            `json:"tokens,omitempty"`
	// TokensExpireAt is when the access token expires, in milliseconds since the
	// epoch, from expires_in at the time it was saved. Zero means no expiry.
	TokensExpireAt int64           `json:"tokensExpireAt,omitempty"`
	CodeVerifier   string          `json:"codeVerifier,omitempty"`
	OAuthState     string          `json:"oauthState,omitempty"`
	Discovery      *DiscoveryState `json:"discovery,omitempty"`
}

// Clone returns a deep copy (the state is small and all fields are values).
func (s *OAuthState) Clone() *OAuthState {
	if s == nil {
		return nil
	}
	copy := *s
	if s.ClientInformation != nil {
		client := *s.ClientInformation
		client.RedirectURIs = append([]string(nil), s.ClientInformation.RedirectURIs...)
		copy.ClientInformation = &client
	}
	if s.Tokens != nil {
		tokens := *s.Tokens
		copy.Tokens = &tokens
	}
	if s.Discovery != nil {
		discovery := *s.Discovery
		copy.Discovery = &discovery
	}
	return &copy
}

// StateStore persists one server's OAuth state.
type StateStore interface {
	Load(ctx context.Context) (*OAuthState, error)
	Save(ctx context.Context, state *OAuthState) error
}

// ServerStore is a StateStore with a refresh lock that keeps other processes
// from refreshing at the same time. It is not reentrant: sign-in and the auth
// provider take it themselves.
type ServerStore interface {
	StateStore
	WithRefreshLock(ctx context.Context, fn func(ctx context.Context) error) error
}

// MemoryStateStore is an in-process StateStore.
type MemoryStateStore struct {
	mu    sync.Mutex
	state *OAuthState
}

// NewMemoryStateStore returns an empty in-memory store.
func NewMemoryStateStore() *MemoryStateStore { return &MemoryStateStore{} }

func (m *MemoryStateStore) Load(ctx context.Context) (*OAuthState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.Clone(), nil
}

func (m *MemoryStateStore) Save(ctx context.Context, state *OAuthState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = state.Clone()
	return nil
}

// MemoryServerStore is a MemoryStateStore whose refresh lock only runs fn:
// there is one process.
type MemoryServerStore struct {
	*MemoryStateStore
}

// NewMemoryServerStore returns an empty in-memory server store.
func NewMemoryServerStore() *MemoryServerStore {
	return &MemoryServerStore{MemoryStateStore: NewMemoryStateStore()}
}

func (m *MemoryServerStore) WithRefreshLock(ctx context.Context, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

// ProviderOptions configures the stateful provider.
type ProviderOptions struct {
	ServerURL              string
	RedirectURL            string
	ClientMetadata         ClientMetadata
	ClientMetadataDocument func(metadata *AuthorizationServerMetadata) (*ClientMetadataDocument, error)
	ClientID               string
	ClientSecret           string
	Store                  StateStore
	OnRedirect             func(ctx context.Context, authorizationURL *url.URL) error
}

// Provider is the stateful OAuth client of one exact MCP server URL.
type Provider struct {
	serverURL      string
	redirectURL    string
	clientMetadata ClientMetadata
	document       func(*AuthorizationServerMetadata) (*ClientMetadataDocument, error)
	configured     *ClientInformation
	store          StateStore
	onRedirect     func(context.Context, *url.URL) error

	mu sync.Mutex
}

// NewProvider builds the provider. redirectURL is what the caller signs in
// with.
func NewProvider(options ProviderOptions) *Provider {
	serverURL := options.ServerURL
	if parsed, err := url.Parse(serverURL); err == nil && parsed.Path == "" {
		parsed.Path = "/"
		serverURL = parsed.String()
	}
	metadata := options.ClientMetadata
	if len(metadata.RedirectURIs) == 0 {
		metadata.RedirectURIs = []string{options.RedirectURL}
	}
	if len(metadata.GrantTypes) == 0 {
		metadata.GrantTypes = []string{"authorization_code", "refresh_token"}
	}
	if len(metadata.ResponseTypes) == 0 {
		metadata.ResponseTypes = []string{"code"}
	}
	if metadata.TokenEndpointAuthMethod == "" {
		if options.ClientSecret != "" {
			metadata.TokenEndpointAuthMethod = "client_secret_post"
		} else {
			metadata.TokenEndpointAuthMethod = "none"
		}
	}
	provider := &Provider{
		serverURL:      serverURL,
		redirectURL:    options.RedirectURL,
		clientMetadata: metadata,
		document:       options.ClientMetadataDocument,
		store:          options.Store,
		onRedirect:     options.OnRedirect,
	}
	if options.ClientID != "" {
		client := &ClientInformation{ClientID: options.ClientID, ClientSecret: options.ClientSecret}
		provider.configured = client
	}
	if provider.store == nil {
		provider.store = NewMemoryStateStore()
	}
	if provider.onRedirect == nil {
		provider.onRedirect = func(context.Context, *url.URL) error { return nil }
	}
	return provider
}

// RedirectURL is the redirect URI this provider signs in with.
func (p *Provider) RedirectURL() string { return p.redirectURL }

// ClientMetadata is the registration metadata of this client.
func (p *Provider) ClientMetadata() ClientMetadata { return p.clientMetadata }

// ClientProvider returns the flow callbacks bound to this provider.
func (p *Provider) ClientProvider() *ClientProvider {
	return &ClientProvider{
		RedirectURL:             p.redirectURL,
		ClientMetadata:          p.clientMetadata,
		ClientMetadataDocument:  p.document,
		State:                   p.State,
		ClientInformation:       p.ClientInformation,
		SaveClientInformation:   p.SaveClientInformation,
		Tokens:                  p.Tokens,
		SaveTokens:              p.SaveTokens,
		RedirectToAuthorization: p.RedirectToAuthorization,
		SaveCodeVerifier:        p.SaveCodeVerifier,
		CodeVerifier:            p.CodeVerifier,
		InvalidateCredentials:   p.InvalidateCredentials,
		SaveDiscoveryState:      p.SaveDiscoveryState,
		DiscoveryState:          p.DiscoveryState,
	}
}

func (p *Provider) State(ctx context.Context) (string, error) {
	current, err := p.load(ctx)
	if err != nil {
		return "", err
	}
	if current.OAuthState != "" {
		return current.OAuthState, nil
	}
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	state := hex.EncodeToString(bytes)
	err = p.update(ctx, func(value OAuthState) OAuthState {
		value.OAuthState = state
		return value
	})
	if err != nil {
		return "", err
	}
	return state, nil
}

func (p *Provider) ClientInformation(ctx context.Context) (*ClientInformation, error) {
	if p.configured != nil {
		return p.configured, nil
	}
	state, err := p.load(ctx)
	if err != nil {
		return nil, err
	}
	return state.ClientInformation, nil
}

func (p *Provider) SaveClientInformation(ctx context.Context, information ClientInformation) error {
	if p.configured != nil {
		return nil
	}
	return p.update(ctx, func(value OAuthState) OAuthState {
		value.ClientInformation = &information
		return value
	})
}

func (p *Provider) Tokens(ctx context.Context) (*Tokens, error) {
	state, err := p.load(ctx)
	if err != nil {
		return nil, err
	}
	return state.Tokens, nil
}

func (p *Provider) SaveTokens(ctx context.Context, tokens Tokens) error {
	var expiresAt int64
	if tokens.ExpiresIn != nil {
		expiresAt = time.Now().UnixMilli() + int64(*tokens.ExpiresIn*1000)
	}
	return p.update(ctx, func(value OAuthState) OAuthState {
		value.Tokens = &tokens
		value.TokensExpireAt = expiresAt
		return value
	})
}

func (p *Provider) RedirectToAuthorization(ctx context.Context, authorizationURL *url.URL) error {
	return p.onRedirect(ctx, authorizationURL)
}

func (p *Provider) SaveCodeVerifier(ctx context.Context, verifier string) error {
	return p.update(ctx, func(value OAuthState) OAuthState {
		value.CodeVerifier = verifier
		return value
	})
}

func (p *Provider) CodeVerifier(ctx context.Context) (string, error) {
	state, err := p.load(ctx)
	if err != nil {
		return "", err
	}
	if state.CodeVerifier == "" {
		return "", errors.New("No OAuth PKCE code verifier is stored")
	}
	return state.CodeVerifier, nil
}

func (p *Provider) InvalidateCredentials(ctx context.Context, kind InvalidateKind) error {
	return p.update(ctx, func(value OAuthState) OAuthState {
		if kind == InvalidateAll || kind == InvalidateClient {
			value.ClientInformation = nil
		}
		if kind == InvalidateAll || kind == InvalidateTokens {
			value.Tokens = nil
			value.TokensExpireAt = 0
		}
		if kind == InvalidateAll || kind == InvalidateVerifier {
			value.CodeVerifier = ""
		}
		if kind == InvalidateAll || kind == InvalidateDiscovery {
			value.Discovery = nil
		}
		if kind == InvalidateAll {
			value.OAuthState = ""
		}
		return value
	})
}

func (p *Provider) SaveDiscoveryState(ctx context.Context, discovery DiscoveryState) error {
	return p.update(ctx, func(value OAuthState) OAuthState {
		value.Discovery = &discovery
		return value
	})
}

func (p *Provider) DiscoveryState(ctx context.Context) (*DiscoveryState, error) {
	state, err := p.load(ctx)
	if err != nil {
		return nil, err
	}
	return state.Discovery, nil
}

// TokensExpireAt returns the stored expiry in milliseconds, 0 when absent.
func (p *Provider) TokensExpireAt(ctx context.Context) (int64, error) {
	state, err := p.load(ctx)
	if err != nil {
		return 0, err
	}
	return state.TokensExpireAt, nil
}

func (p *Provider) load(ctx context.Context) (*OAuthState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	stored, err := p.store.Load(ctx)
	if err != nil {
		return nil, err
	}
	return p.own(stored), nil
}

func (p *Provider) update(ctx context.Context, update func(OAuthState) OAuthState) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	stored, err := p.store.Load(ctx)
	if err != nil {
		return err
	}
	next := update(*p.own(stored))
	return p.store.Save(ctx, &next)
}

// own ignores state stored for another server URL.
func (p *Provider) own(state *OAuthState) *OAuthState {
	if state == nil || state.ServerURL != p.serverURL {
		return &OAuthState{ServerURL: p.serverURL}
	}
	copy := *state
	return &copy
}

// Describe is a short, secret-free description used in errors and logs.
func (p *Provider) Describe() string {
	return fmt.Sprintf("oauth %s", p.serverURL)
}
