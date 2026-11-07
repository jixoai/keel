package externalgw

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/gateway"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/servicegraph"
)

// ToolService is the loadout's ToolProvider service: stateless external tools
// registered into the run-path tool registry. The result of a call is returned
// to the runner, which commits it through the contract kernel; the provider
// never touches the ledger.
type ToolService struct {
	opts      Options
	log       *slog.Logger
	providers map[string]*toolRuntime

	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	closed bool
	wg     sync.WaitGroup
	// tools is the published catalogue, rebuilt whenever a provider connects
	// or its catalogue changes.
	tools []agentcore.AgentTool
}

var _ servicegraph.Instance = (*ToolService)(nil)

// NewToolService resolves the tenant document's tool providers. It does not
// connect; Start owns every side effect.
func NewToolService(doc host.ExternalConfig, opts Options) (*ToolService, error) {
	config, err := ResolveConfig(doc, opts)
	if err != nil {
		return nil, err
	}
	if opts.Restart.MaxRestarts == 0 {
		opts.Restart = gateway.RestartPolicy{MaxRestarts: -1}
	}
	service := &ToolService{
		opts:      opts,
		log:       opts.logger(),
		providers: make(map[string]*toolRuntime, len(config.Tools)),
	}
	for _, provider := range config.Tools {
		service.providers[provider.Name] = &toolRuntime{name: provider.Name, cfg: provider, svc: service}
	}
	return service, nil
}

// Start connects every tool provider and publishes the resulting catalogue. A
// provider that cannot connect is retried; its tools are simply absent until it
// recovers, and the run path keeps working.
func (s *ToolService) Start(ctx context.Context, effects *servicegraph.EffectScope) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	base, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.ctx, s.cancel = base, cancel
	s.mu.Unlock()

	var wg sync.WaitGroup
	for _, provider := range s.providers {
		provider := provider
		wg.Add(1)
		go func() {
			defer wg.Done()
			attemptCtx, attemptCancel := context.WithTimeout(base, s.opts.connectTimeout())
			_ = provider.connect(attemptCtx)
			attemptCancel()
		}()
	}
	wg.Wait()
	s.publish()
	for _, provider := range s.providers {
		providerCtx, providerCancel := context.WithCancel(base)
		provider.cancel = providerCancel
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			provider.supervise(providerCtx)
		}()
		if effects != nil {
			name := provider.name
			if err := effects.Register("external-tool["+name+"]", func() error {
				provider.shutdown(context.WithoutCancel(context.Background()))
				return nil
			}); err != nil {
				provider.shutdown(context.WithoutCancel(context.Background()))
				return err
			}
		}
	}
	for _, provider := range s.providers {
		if provider.ready() {
			s.log.Info("external tool provider ready", "provider", provider.name, "tools", len(provider.toolDefs()))
		} else {
			s.log.Warn("external tool provider not ready; retrying with backoff", "provider", provider.name, "error", provider.lastError())
		}
	}
	return nil
}

// Stop closes every tool provider and waits briefly for the supervisors.
func (s *ToolService) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, provider := range s.providers {
		provider.shutdown(ctx)
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(15 * time.Second):
		s.log.Warn("external tool supervisors did not stop within 15s")
		return nil
	}
}

// Tools returns the published catalogue. The adapters are stable across a
// reconnect: a call resolves the provider's current connection by name, so a
// tool that was registered at boot works again after the provider recovers.
// A changed catalogue is applied on the next loadout reload.
func (s *ToolService) Tools() []agentcore.AgentTool {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agentcore.AgentTool(nil), s.tools...)
}

// publish rebuilds the catalogue from every connected provider.
func (s *ToolService) publish() {
	taken := make(map[string]bool)
	var out []agentcore.AgentTool
	names := make([]string, 0, len(s.providers))
	for name := range s.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		provider := s.providers[name]
		for _, def := range provider.toolDefs() {
			toolName := externalToolName(provider.name, def.Name, taken)
			taken[toolName] = true
			out = append(out, &toolAdapter{
				svc:      s,
				provider: provider.name,
				def:      def,
				name:     toolName,
			})
		}
	}
	s.mu.Lock()
	s.tools = out
	s.mu.Unlock()
}

// callTool invokes one provider tool through the provider's current connection.
func (s *ToolService) callTool(ctx context.Context, providerName, tool string, arguments json.RawMessage) (gateway.ToolResult, error) {
	if s == nil {
		return gateway.ToolResult{}, fmt.Errorf("%w: no external tool providers are configured", ErrBackendUnavailable)
	}
	provider, ok := s.providers[providerName]
	if !ok {
		return gateway.ToolResult{}, fmt.Errorf("%w: %s", ErrBackendUnknown, providerName)
	}
	client, err := provider.clientOrErr()
	if err != nil {
		return gateway.ToolResult{}, err
	}
	result, err := client.CallTool(ctx, tool, arguments)
	if err != nil {
		return gateway.ToolResult{}, fmt.Errorf("external tool %s/%s: %w", providerName, tool, err)
	}
	return result, nil
}

// --- provider runtime -----------------------------------------------------

type toolRuntime struct {
	name string
	cfg  ProviderConfig
	svc  *ToolService

	mu        sync.Mutex
	client    *gateway.ToolClient
	conn      *gateway.Conn
	transport gateway.Transport
	readyNow  bool
	lastErr   string
	attempts  int
	defs      []gateway.ToolDefinition
	cancel    context.CancelFunc
}

func (p *toolRuntime) ready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.readyNow
}

func (p *toolRuntime) lastError() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastErr
}

func (p *toolRuntime) toolDefs() []gateway.ToolDefinition {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]gateway.ToolDefinition(nil), p.defs...)
}

func (p *toolRuntime) clientOrErr() (*gateway.ToolClient, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client == nil || !p.readyNow {
		detail := p.lastErr
		if detail == "" {
			detail = "connecting"
		}
		return nil, fmt.Errorf("%w: tool provider %s (%s)", ErrBackendUnavailable, p.name, detail)
	}
	return p.client, nil
}

func (p *toolRuntime) connect(ctx context.Context) error {
	var transport gateway.Transport
	switch p.cfg.Transport {
	case host.ExternalTransportStdio:
		stdio, err := gateway.SpawnStdio(gateway.SpawnOptions{
			Command: p.cfg.Command,
			Args:    p.cfg.Args,
			Dir:     p.cfg.Dir,
			Env:     p.cfg.Env,
			Stderr:  p.svc.opts.stderr(),
		})
		if err != nil {
			p.fail(err)
			return err
		}
		transport = stdio
	case host.ExternalTransportHTTP:
		transport = gateway.NewHTTPTransport(gateway.HTTPTransportOptions{
			URL:     p.cfg.URL,
			Headers: p.cfg.Headers,
			Client:  p.svc.opts.HTTPClient,
		})
	default:
		err := fmt.Errorf("externalgw: unsupported transport %q", p.cfg.Transport)
		p.fail(err)
		return err
	}
	conn := gateway.NewConn(gateway.ConnOptions{
		ClientInfo:      gateway.Implementation{Name: "keel-runtime", Version: "1.0.0"},
		AcceptProviders: []gateway.ProviderType{gateway.ProviderTypeToolProvider},
		RequestTimeout:  p.cfg.Timeout,
		OnCall: func(record gateway.CallRecord) {
			p.svc.log.Info("external call", "provider", p.name,
				"method", record.Method, "target", record.Target, "status", record.Status,
				"error_code", record.ErrorCode, "duration_ms", record.Duration.Milliseconds())
		},
		OnDiagnostic: func(diag gateway.Diagnostic) {
			p.svc.log.Warn("external diagnostic", "provider", p.name, "kind", diag.Kind, "detail", diag.Detail)
		},
	})
	if _, err := conn.Connect(ctx, transport); err != nil {
		_ = transport.Close()
		p.fail(err)
		return err
	}
	client := gateway.NewToolProvider(conn)
	tools, err := client.ListTools(ctx)
	if err != nil {
		_ = conn.Close()
		p.fail(err)
		return err
	}
	p.mu.Lock()
	p.transport = transport
	p.conn = conn
	p.client = client
	p.readyNow = true
	p.lastErr = ""
	p.attempts = 0
	p.defs = tools
	p.mu.Unlock()
	return nil
}

func (p *toolRuntime) fail(err error) {
	p.mu.Lock()
	p.readyNow = false
	p.lastErr = err.Error()
	p.attempts++
	p.mu.Unlock()
	p.svc.log.Warn("external tool provider connect failed", "provider", p.name, "error", err)
}

func (p *toolRuntime) supervise(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			p.shutdown(context.Background())
			return
		}
		p.mu.Lock()
		conn := p.conn
		ready := p.readyNow
		attempts := p.attempts
		p.mu.Unlock()
		if ready && conn != nil {
			select {
			case <-ctx.Done():
				p.shutdown(context.Background())
				return
			case <-conn.Done():
			}
			p.mu.Lock()
			p.client = nil
			p.conn = nil
			p.transport = nil
			p.readyNow = false
			p.mu.Unlock()
			_ = conn.Close()
			p.svc.publish()
			continue
		}
		attempts++
		if !p.svc.opts.Restart.Allowed(attempts) {
			p.svc.log.Error("external tool provider restart budget exhausted", "provider", p.name)
			<-ctx.Done()
			p.shutdown(context.Background())
			return
		}
		p.mu.Lock()
		p.attempts = attempts
		p.mu.Unlock()
		delay := p.svc.opts.Restart.Delay(attempts)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			p.shutdown(context.Background())
			return
		case <-timer.C:
		}
		connectCtx, cancel := context.WithTimeout(ctx, p.svc.opts.connectTimeout())
		err := p.connect(connectCtx)
		cancel()
		if err == nil {
			p.mu.Lock()
			p.attempts = 0
			p.mu.Unlock()
			p.svc.publish()
		}
	}
}

func (p *toolRuntime) shutdown(ctx context.Context) {
	p.mu.Lock()
	cancel := p.cancel
	conn := p.conn
	p.client = nil
	p.conn = nil
	p.transport = nil
	p.readyNow = false
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if conn != nil {
		_ = conn.Close()
	}
}

// --- tool adapter ---------------------------------------------------------

// toolAdapter exposes one ToolProvider tool to the native run path. The result
// it returns is committed by the runner through the contract kernel, so the
// external process never writes an event.
type toolAdapter struct {
	svc      *ToolService
	provider string
	def      gateway.ToolDefinition
	name     string
}

var _ agentcore.AgentTool = (*toolAdapter)(nil)

func (t *toolAdapter) Name() string { return t.name }

func (t *toolAdapter) Description() string {
	if t.def.Description != "" {
		return t.def.Description
	}
	if t.def.Title != "" {
		return t.def.Title
	}
	return "external tool " + t.provider + "/" + t.def.Name
}

func (t *toolAdapter) Schema() json.RawMessage { return t.def.InputSchema }

func (t *toolAdapter) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}

func (t *toolAdapter) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	result, err := t.svc.callTool(ctx, t.provider, t.def.Name, args)
	if err != nil {
		return agentcore.AgentToolResult{}, err
	}
	out := agentcore.AgentToolResult{Content: make(agentcore.ContentList, 0, len(result.Content))}
	for _, block := range result.Content {
		switch block.Type {
		case "text":
			out.Content = append(out.Content, agentcore.NewTextContent(block.Text))
		case "image":
			out.Content = append(out.Content, agentcore.NewImageContent(block.Data, block.MimeType))
		default:
			raw, _ := json.Marshal(block)
			out.Content = append(out.Content, agentcore.NewTextContent(string(raw)))
		}
	}
	if len(result.StructuredContent) > 0 {
		var structured any
		if err := json.Unmarshal(result.StructuredContent, &structured); err == nil {
			out.StructuredContent = structured
		}
	}
	if result.IsError {
		isError := true
		out.IsError = &isError
	}
	if onUpdate != nil && out.IsError != nil && *out.IsError {
		onUpdate(out)
	}
	return out, nil
}

// externalToolName builds a namespaced, sanitized tool name. The namespace
// keeps external tools from shadowing builtins/MCP tools; a long name is
// shortened with a hash suffix, and a collision (two tools sanitizing to the
// same string) gets a second hash so registration cannot fail silently.
var externalToolNamePattern = regexp.MustCompile(`[^a-zA-Z0-9_]+`)

func externalToolName(provider, tool string, taken map[string]bool) string {
	base := "ext__" + externalToolNamePattern.ReplaceAllString(provider, "_") + "__" +
		externalToolNamePattern.ReplaceAllString(tool, "_")
	const maxLen = 64
	if len(base) > maxLen {
		base = base[:maxLen-9] + "_" + shortHash(base)
	}
	if taken[base] {
		base = base[:maxLen-9] + "_" + shortHash(provider+"/"+tool)
	}
	return base
}

func shortHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:4])
}
