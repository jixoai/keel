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
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
)

// ConnectOptions configures Manager connection attempts.
type ConnectOptions struct {
	ClientName     string
	ClientVersion  string
	RequestTimeout time.Duration
	// DisableGetStream keeps Streamable HTTP servers' server-to-client streams
	// closed (tests, or servers known not to support GET).
	DisableGetStream bool
	// AuthProvider builds the auth provider for one HTTP entry. Nil means the
	// entry's headers carry all credentials.
	AuthProvider func(entry ServerEntry, config map[string]any) AuthProvider
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
	manager := &Manager{logger: options.Logger}
	var mu sync.Mutex
	semaphore := make(chan struct{}, options.Concurrency)
	var wg sync.WaitGroup
	for _, entry := range entries {
		if !ServerBool(entry.Config, "enabled", true) {
			continue
		}
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
