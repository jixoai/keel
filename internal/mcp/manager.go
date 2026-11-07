// Behaviour ported from @earendil-works/pi coding-agent, the built-in MCP
// extension (packages/coding-agent/src/extensions/mcp, MIT License); implemented
// independently in Go. Contract kept identical; see docs/UPSTREAM.md.
//
// Manager connects the configured MCP servers, lists their tools, and exposes
// them as agent tools. Servers connect concurrently; one failing server is
// reported on its entry and never blocks the others.

package mcp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/mcp/oauth"
)

// OAuthOptions configures OAuth for Streamable HTTP entries. The runtime
// replaces PI's JSON state files with the PostgreSQL store: Storage builds the
// per-tenant/server state document and refresh lock.
type OAuthOptions struct {
	// Storage builds the OAuth state store of one server. Nil disables OAuth.
	Storage func(tenantID uuid.UUID, server string) oauth.ServerStore
	// ClientName is client_name for dynamic client registration unless the
	// entry names another.
	ClientName string
	// MetadataBaseURL publishes Client ID Metadata Documents (CIMD). Empty
	// makes "auto" use dynamic client registration and fails explicit "cimd".
	MetadataBaseURL string
	// MetadataRedirectURLs lists the redirect URIs the published documents
	// contain.
	MetadataRedirectURLs []string
	// RedirectURL is the host's callback route used for refreshes when the
	// entry configures no loopback callbackUrl, so tokens signed in through the
	// host's https callback can still be refreshed with a Client ID Metadata
	// Document.
	RedirectURL string
	// HTTPClient is used for authorization-server requests. Nil uses the
	// SSRF-guarded client.
	HTTPClient *http.Client
	// OnChallenge observes the server's bearer challenge, so a host can record
	// the scope and resource metadata URL for a sign-in.
	OnChallenge func(server string, challenge oauth.Challenge)
}

// ConnectOptions configures Manager connection attempts.
type ConnectOptions struct {
	ClientName     string
	ClientVersion  string
	RequestTimeout time.Duration
	// DisableGetStream keeps Streamable HTTP servers' server-to-client streams
	// closed (tests, or servers known not to support GET).
	DisableGetStream bool
	// AuthProvider builds the auth provider for one HTTP entry. Nil means OAuth
	// is built from OAuth (when configured) or the entry's headers carry all
	// credentials.
	AuthProvider func(entry ServerEntry, config map[string]any) AuthProvider
	// TenantID scopes OAuth credentials. The zero UUID is the process-global
	// tenant (the boot-time MCP configuration is per process, not per request).
	TenantID uuid.UUID
	// OAuth enables the PostgreSQL-backed OAuth provider for entries with an
	// "oauth" configuration.
	OAuth *OAuthOptions
	// Stderr receives stdio servers' stderr. Nil uses os.Stderr.
	Stderr io.Writer
	Logger *slog.Logger
	// Concurrency bounds simultaneous connection attempts. Zero uses 8.
	Concurrency int
}

// Server is one connected (or failed) MCP server.
type Server struct {
	Entry     ServerEntry
	Client    *Client
	Transport Transport
	Tools     []agentcore.AgentTool
	Err       error
}

// Name is the configured server name.
func (s *Server) Name() string { return s.Entry.Name }

// Manager owns every MCP connection of one host.
type Manager struct {
	mu      sync.Mutex
	servers []*Server
	entries map[string]ServerEntry
	options ConnectOptions
	logger  *slog.Logger
}

// Connect connects the enabled entries. It always returns a manager so callers
// can inspect partial failures and close what did connect.
func Connect(ctx context.Context, entries []ServerEntry, options ConnectOptions) *Manager {
	if options.ClientName == "" {
		options.ClientName = "keel"
	}
	if options.ClientVersion == "" {
		options.ClientVersion = "1"
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.Concurrency <= 0 {
		options.Concurrency = 8
	}
	manager := &Manager{logger: options.Logger, entries: map[string]ServerEntry{}, options: options}
	var mu sync.Mutex
	semaphore := make(chan struct{}, options.Concurrency)
	var wg sync.WaitGroup
	for _, entry := range entries {
		if !ServerBool(entry.Config, "enabled", true) {
			continue
		}
		manager.entries[entry.Name] = entry
		entry := entry
		wg.Add(1)
		semaphore <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-semaphore }()
			server := connectOne(ctx, entry, options)
			mu.Lock()
			manager.servers = append(manager.servers, server)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return manager
}

// Reconnect rebuilds one server's connection, for example after an OAuth
// sign-in stored a grant in another request. The old connection stays until the
// new one succeeds, so a failed attempt does not remove a working server.
func (m *Manager) Reconnect(ctx context.Context, name string) error {
	if m == nil {
		return fmt.Errorf("mcp: reconnect: no manager")
	}
	m.mu.Lock()
	entry, ok := m.entries[name]
	options := m.options
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("mcp: reconnect: unknown server %q", name)
	}
	server := connectOne(ctx, entry, options)
	if server.Err != nil {
		return server.Err
	}
	m.mu.Lock()
	replaced := false
	var previous *Server
	for index, existing := range m.servers {
		if existing.Name() == name {
			previous = existing
			m.servers[index] = server
			replaced = true
			break
		}
	}
	if !replaced {
		m.servers = append(m.servers, server)
	}
	m.mu.Unlock()
	if previous != nil && previous.Client != nil {
		_ = previous.Client.Close()
	}
	m.logger.Info("mcp server reconnected", "server", name, "tools", len(server.Tools))
	return nil
}

func connectOne(ctx context.Context, entry ServerEntry, options ConnectOptions) *Server {
	server := &Server{Entry: entry}
	transport, err := transportFor(entry, options)
	if err != nil {
		server.Err = err
		return server
	}
	timeout := options.RequestTimeout
	if seconds, ok := entry.Config["timeout"].(float64); ok && seconds > 0 {
		timeout = time.Duration(seconds * float64(time.Second))
	}
	client := NewClient(ClientOptions{
		Name:           options.ClientName,
		Version:        options.ClientVersion,
		RequestTimeout: timeout,
	})
	if _, err := client.Connect(ctx, transport); err != nil {
		_ = client.Close()
		server.Err = fmt.Errorf("mcp: server %q: %w", entry.Name, err)
		return server
	}
	tools, err := client.AgentTools(entry.Name)
	if err != nil {
		_ = client.Close()
		server.Err = fmt.Errorf("mcp: server %q: %w", entry.Name, err)
		return server
	}
	server.Client = client
	server.Transport = transport
	server.Tools = tools
	options.Logger.Info("mcp server connected", "server", entry.Name, "tools", len(tools), "source", entry.Source)
	return server
}

func transportFor(entry ServerEntry, options ConnectOptions) (Transport, error) {
	if url := ServerField(entry.Config, "url"); url != "" {
		httpOptions := HTTPTransportOptions{
			URL:              url,
			Headers:          stringMap(entry.Config["headers"]),
			DisableGetStream: options.DisableGetStream,
		}
		if options.AuthProvider != nil {
			httpOptions.AuthProvider = options.AuthProvider(entry, entry.Config)
		} else if options.OAuth != nil && entry.Config["oauth"] != nil {
			provider, err := oauthProviderFor(entry, url, options)
			if err != nil {
				return nil, err
			}
			httpOptions.AuthProvider = provider
		}
		return NewHTTPTransport(httpOptions), nil
	}
	command := ServerField(entry.Config, "command")
	if command == "" {
		return nil, fmt.Errorf("mcp: server %q has neither a command nor a url", entry.Name)
	}
	return NewStdioTransport(StdioTransportOptions{
		Command: command,
		Args:    stringSlice(entry.Config["args"]),
		Dir:     ServerField(entry.Config, "cwd"),
		Env:     mergedEnv(entry.Config["env"]),
		Stderr:  options.Stderr,
		OnError: func(err error) {
			options.Logger.Warn("mcp stdio", "server", entry.Name, "err", err)
		},
	}), nil
}

// oauthAuthAdapter maps the oauth package's provider contract onto the
// transport's AuthProvider. Both carry the same fields; the adapter keeps the
// oauth package independent of the transport package.
type oauthAuthAdapter struct {
	inner *oauth.StoredAuthProvider
}

func (a oauthAuthAdapter) Token(ctx context.Context) (string, error) {
	return a.inner.Token(ctx)
}

func (a oauthAuthAdapter) OnUnauthorized(ctx context.Context, event UnauthorizedEvent) error {
	return a.inner.OnUnauthorized(ctx, oauth.UnauthorizedEvent{
		Status:          event.Status,
		WWWAuthenticate: event.WWWAuthenticate,
		Body:            event.Body,
		ServerURL:       event.ServerURL,
		Token:           event.Token,
		HTTPClient:      event.Client,
	})
}

// oauthProviderFor builds the stored-token provider of one HTTP entry. A server
// without a stored grant fails its connection with
// oauth.AuthorizationRequiredError, which a host turns into a sign-in prompt.
func oauthProviderFor(entry ServerEntry, serverURL string, options ConnectOptions) (AuthProvider, error) {
	if options.OAuth == nil || options.OAuth.Storage == nil {
		return nil, nil
	}
	// Credentials must never fall into an implicit shared tenant.
	if options.TenantID == uuid.Nil {
		return nil, fmt.Errorf("mcp: server %q: OAuth requires an explicit tenant (set KEEL_MCP_OAUTH_TENANT)", entry.Name)
	}
	parsed, err := url.Parse(serverURL)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("mcp: server %q: invalid url", entry.Name)
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackOAuthHost(parsed.Hostname())) {
		return nil, fmt.Errorf("mcp: server %q: OAuth requires an https URL (http is only allowed on loopback)", entry.Name)
	}
	stateStore := options.OAuth.Storage(options.TenantID, entry.Name)
	if stateStore == nil {
		return nil, nil
	}
	settings := OAuthSettingsFromConfig(entry.Config, options.OAuth.MetadataBaseURL, options.OAuth.MetadataRedirectURLs)
	documents := oauth.DocumentsFromSettings(settings)
	redirectURL := settings.CallbackURL
	if redirectURL == "" {
		redirectURL = options.OAuth.RedirectURL
	}
	if redirectURL == "" {
		redirectURL = oauth.DefaultRedirectURL
	}
	var onChallenge func(oauth.Challenge)
	if options.OAuth.OnChallenge != nil {
		onChallenge = func(challenge oauth.Challenge) {
			options.OAuth.OnChallenge(entry.Name, challenge)
		}
	}
	return oauthAuthAdapter{inner: oauth.NewStoredAuthProvider(oauth.StoredAuthProviderOptions{
		ServerURL:               serverURL,
		ClientName:              options.OAuth.ClientName,
		Settings:                settings,
		Store:                   stateStore,
		ClientMetadataDocuments: documents,
		RedirectURL:             redirectURL,
		HTTPClient:              options.OAuth.HTTPClient,
		OnChallenge:             onChallenge,
	})}, nil
}

func isLoopbackOAuthHost(host string) bool {
	switch strings.Trim(host, "[]") {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// OAuthSettingsFromConfig reads the oauth block of one mcp.json entry. The
// shared configuration shape uses the same camelCase names as PI. The host's
// defaults (client name, public CIMD metadata base URL) fill the gaps.
func OAuthSettingsFromConfig(config map[string]any, metadataBaseURL string, metadataRedirectURLs []string) oauth.Settings {
	// ClientName is the entry's own clientName, not the host default: the host
	// default is passed separately so "auto" can still choose CIMD.
	settings := oauth.Settings{
		MetadataBaseURL:      metadataBaseURL,
		MetadataRedirectURLs: metadataRedirectURLs,
	}
	block, _ := config["oauth"].(map[string]any)
	if block == nil {
		return settings
	}
	stringField := func(name string) string {
		value, _ := block[name].(string)
		return value
	}
	settings.ClientID = stringField("clientId")
	settings.ClientSecret = stringField("clientSecret")
	settings.CallbackURL = stringField("callbackUrl")
	settings.Scope = stringField("scope")
	settings.ClientRegistration = stringField("clientRegistration")
	settings.AuthServerMetadataURL = stringField("authServerMetadataUrl")
	if value, ok := block["clientName"].(string); ok && value != "" {
		settings.ClientName = value
	}
	if value, ok := block["callbackPort"].(float64); ok && value >= 1 && value <= 65535 {
		port := int(value)
		settings.CallbackPort = &port
	}
	return settings
}

func stringMap(value any) map[string]string {
	record, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(record))
	for key, entry := range record {
		if text, ok := entry.(string); ok {
			out[key] = text
		}
	}
	return out
}

func stringSlice(value any) []string {
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, entry := range list {
		if text, ok := entry.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

// mergedEnv appends the configured variables to the current environment.
func mergedEnv(value any) []string {
	extra := stringMap(value)
	if len(extra) == 0 {
		return nil
	}
	env := os.Environ()
	for key, entry := range extra {
		env = append(env, key+"="+entry)
	}
	return env
}

// Servers returns every attempted server, connected or failed, by name.
func (m *Manager) Servers() []*Server {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*Server(nil), m.servers...)
}

// Tools flattens the tools of every connected server.
func (m *Manager) Tools() []agentcore.AgentTool {
	if m == nil {
		return nil
	}
	var tools []agentcore.AgentTool
	for _, server := range m.Servers() {
		tools = append(tools, server.Tools...)
	}
	return tools
}

// ToolNames lists the model-facing names of every connected tool.
func (m *Manager) ToolNames() []string {
	tools := m.Tools()
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	return names
}

// Errors lists the connection failures as "name: error" strings.
func (m *Manager) Errors() []string {
	if m == nil {
		return nil
	}
	var errors []string
	for _, server := range m.Servers() {
		if server.Err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", server.Entry.Name, server.Err))
		}
	}
	return errors
}

// PromptSection renders the mcp_servers section pi's host adds to the system
// prompt: one line per connected server, omitted when no server connected.
func (m *Manager) PromptSection() string {
	if m == nil {
		return ""
	}
	var lines []string
	for _, server := range m.Servers() {
		if server.Err != nil {
			continue
		}
		description := ServerField(server.Entry.Config, "description")
		if strings.TrimSpace(description) == "" {
			description = fmt.Sprintf("%d tool(s)", len(server.Tools))
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", server.Entry.Name, description))
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// Close closes every connection; failures are joined.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var failures []string
	for _, server := range m.servers {
		if server.Client != nil {
			if err := server.Client.Close(); err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", server.Entry.Name, err))
			}
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("mcp: close: %s", strings.Join(failures, "; "))
	}
	return nil
}
