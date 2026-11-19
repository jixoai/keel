package servicehost

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/externalgw"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/loadout"
	"github.com/jixoai/keel/internal/mcp"
	"github.com/jixoai/keel/internal/mcphost"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/servicegraph"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/telemetry"

	"github.com/google/uuid"
)

// newProvider maps the default blueprint's service names to the concrete
// factories of this hosting process. A name with no factory is omitted from
// the loadout (sandbox has no implementation; MCP is omitted when nothing is
// configured), which is exactly the "no fake factory" rule.
//
// tenant is the document the loadout is being resolved from, passed explicitly
// rather than read off cfg: a reassembly (Reload) must build its
// document-derived services from the document being applied, not from the
// document this process booted with.
func newProvider(tenant host.TenantConfig, cfg Config, engine *servicegraph.Engine) func(string) servicegraph.Factory {
	factories := map[string]servicegraph.Factory{
		loadout.ServiceStore:             newStoreFactory(cfg),
		loadout.ServiceLLM:               newLLMFactory(tenant, cfg),
		loadout.ServicePrompt:            newPromptFactory(cfg),
		loadout.ServiceTools:             newToolsFactory(cfg),
		loadout.ServicePiLoop:            newPiLoopFactory(cfg),
		loadout.ServiceSessionProjection: newProjectionFactory(cfg),
		loadout.ServiceTelemetry:         newTelemetryFactory(cfg),
		loadout.ServiceCompaction:        newCompactionFactory(cfg),
		loadout.ServiceWorker:            newWorkerFactory(cfg, engine),
		loadout.ServiceExternalAgent:     newExternalAgentFactory(cfg),
		loadout.ServiceExternalTools:     newExternalToolsFactory(cfg),
	}
	if factory := newMCPFactory(cfg); factory != nil {
		factories[loadout.ServiceMCP] = factory
	}
	return func(name string) servicegraph.Factory { return factories[name] }
}

// --- store ---------------------------------------------------------------

// storeService is the store capability's published view: the read-only run-row
// mirror the worker logs from. The raw *store.Store never leaves the hosting
// process — a service cannot append an event or reach a table through this
// binding.
type storeService struct {
	mirror agent.RunMirror
}

func (s *storeService) GetRun(ctx context.Context, tenantID, sessionID uuid.UUID) (store.Run, error) {
	return s.mirror.GetRun(ctx, tenantID, sessionID)
}

func (s *storeService) Start(context.Context, *servicegraph.EffectScope) error { return nil }
func (s *storeService) Stop(context.Context) error                             { return nil }

func newStoreFactory(cfg Config) servicegraph.Factory {
	return func(context.Context, servicegraph.Resolved) (servicegraph.Instance, error) {
		if cfg.Seams.Runs == nil {
			return nil, fmt.Errorf("store service requires the run-row mirror seam")
		}
		return &storeService{mirror: cfg.Seams.Runs}, nil
	}
}

// --- llm -----------------------------------------------------------------

// LLMService is the llm capability: the provider stream plus the model identity
// captured into telemetry and logs. It is the narrow binding the loop consumes.
type LLMService interface {
	StreamFn() provider.StreamFn
	ProviderName() string
	Model() string
	APIKey() string
	// Models is the provider configuration of this generation (nil when the
	// tenant declares none): the per-attempt resolver and the views the API
	// surface serves. The loop wires it into the runner so an attempt's
	// execution identity comes from the run's selection fact.
	Models() ModelService
}

type llmService struct {
	stream       provider.StreamFn
	providerName string
	model        string
	apiKey       string
	models       ModelService
}

func (l *llmService) StreamFn() provider.StreamFn { return l.stream }
func (l *llmService) ProviderName() string        { return l.providerName }
func (l *llmService) Model() string               { return l.model }
func (l *llmService) APIKey() string              { return l.apiKey }
func (l *llmService) Models() ModelService        { return l.models }

func (l *llmService) Start(context.Context, *servicegraph.EffectScope) error { return nil }
func (l *llmService) Stop(context.Context) error                             { return nil }

func newLLMFactory(tenant host.TenantConfig, cfg Config) servicegraph.Factory {
	return func(context.Context, servicegraph.Resolved) (servicegraph.Instance, error) {
		lookup := func(string) string { return "" }
		if cfg.LLM.LookupEnv != nil {
			lookup = func(name string) string {
				value, _ := cfg.LLM.LookupEnv(name)
				return value
			}
		}
		svc := &llmService{stream: agent.FakeStream(), providerName: "fake", model: "fake", apiKey: cfg.LLM.APIKey}
		agentCfg := tenant.Agent
		if strings.TrimSpace(agentCfg.Model) != "" {
			resolved, _, err := provider.ResolveProvider(agentCfg.Model, agentCfg.BaseURL, agentCfg.Protocol, agentCfg.Provider, lookup)
			if err != nil {
				return nil, fmt.Errorf("resolve provider %q: %w", agentCfg.Model, err)
			}
			svc.stream = provider.StreamFnFromProvider(resolved)
			svc.providerName = resolved.Name()
			// ResolveProvider's second return is the provider name, not the wire
			// model id; the runner sends Model() to the provider, so it must be
			// the model id the provider actually serves (a provider name here is
			// rejected by every gateway with model_not_found).
			if models := resolved.Models(); len(models) > 0 && strings.TrimSpace(models[0].ID) != "" {
				svc.model = models[0].ID
			} else {
				svc.model = agentCfg.Model
			}
		}
		// The provider configuration (the tenant document's models slice)
		// resolves the execution identity per attempt; its absence leaves the
		// static path above authoritative. A document the resolver cannot use
		// fails this factory, which fails boot/reassembly.
		if modelsConfigured(tenant) {
			models, err := newModelsService(tenant, cfg.LLM, cfg.Product.WrapStream)
			if err != nil {
				return nil, fmt.Errorf("resolve model configuration: %w", err)
			}
			svc.models = models
			// The advertised identity follows the document's default selection:
			// a document-driven deployment must not report itself as the fake
			// provider in boot logs, telemetry and the loadout summary. The
			// construction above already proved the default resolves, so this
			// is the identity every bare run of this generation uses.
			if models.DefaultSelection().ModelID != "" {
				if active, err := models.resolveActive(nil); err == nil {
					svc.providerName = active.ProviderID
					svc.model = active.ModelID
				}
			}
		}
		// Product-layer observation (token partials): the decorator runs on the
		// hot path for every model request, so it must stay allocation-light.
		if cfg.Product.WrapStream != nil && svc.stream != nil {
			svc.stream = cfg.Product.WrapStream(svc.stream)
		}
		return svc, nil
	}
}

// --- prompt (host) -------------------------------------------------------

// promptService is the host assembly: system prompt, skills and the one tool
// registry every dependent shares. It provides ctx.prompt.
type promptService struct {
	host *host.Host
}

func (p *promptService) SystemPrompt() string { return p.host.SystemPrompt() }
func (p *promptService) RenderSystemPrompt(preamble string) string {
	return p.host.RenderSystemPrompt(preamble)
}
func (p *promptService) Tools() *agenttool.ToolRegistry { return p.host.Tools() }
func (p *promptService) RefreshSections(extra []agentcore.AgentTool, mcpSection string, pluginSections []host.Section) error {
	return p.host.RefreshSections(extra, mcpSection, pluginSections)
}
func (p *promptService) Start(context.Context, *servicegraph.EffectScope) error { return nil }
func (p *promptService) Stop(context.Context) error                             { return nil }

func newPromptFactory(cfg Config) servicegraph.Factory {
	return func(_ context.Context, resolved servicegraph.Resolved) (servicegraph.Instance, error) {
		sections := append([]host.Section(nil), cfg.Prompt.Sections...)
		for _, section := range cfg.Tenant.Agent.Sections {
			sections = append(sections, host.Section{Name: section.Name, Content: section.Content})
		}
		var extra []agentcore.AgentTool
		backends := cfg.Product.ToolBackends
		if bound, ok := resolved.Bind(loadout.CapMCP); ok {
			mcpService, ok := bound.(*mcpService)
			if !ok {
				return nil, fmt.Errorf("ctx.mcp is bound to %T, want *mcpService", bound)
			}
			// MCP capability enters through the builtin `mcp` tool only. The
			// manager's per-server catalogue must not be flattened into tools:
			// adding a server would change the model-visible manifest.
			backends.MCP = mcpService.manager
			// mcp_servers and mcp_apps are NOT read here: the assembly-time read
			// was the one-shot seam that made an MCP refresh invisible. They are
			// published by the MCP registry through PublishMCPSurface (mcpService
			// refreshes the registry when it starts) and republished on every
			// change, composed with the plugin half.
		}
		// External tool providers join the same registry (their names are
		// namespaced ext__<provider>__<tool>). A duplicate name fails host
		// assembly, which is the fail-closed answer: two tools cannot share a
		// model-facing name.
		if bound, ok := resolved.Bind(loadout.CapExternalTools); ok {
			toolsService, ok := bound.(*externalgw.ToolService)
			if !ok {
				return nil, fmt.Errorf("ctx.external-tools is bound to %T, want *externalgw.ToolService", bound)
			}
			extra = append(extra, toolsService.Tools()...)
		}
		// Static product tools (keel-agent): plugin tools are added later through
		// Runtime.RefreshProductTools.
		extra = append(extra, cfg.Product.ExtraTools...)
		preamble := cfg.Tenant.Agent.SystemPrompt
		if preamble == "" {
			preamble = cfg.Prompt.Preamble
		}
		var roots []string
		if cfg.Tenant.Skills.Enabled == nil || *cfg.Tenant.Skills.Enabled {
			roots = cfg.Tenant.Skills.Roots
		}
		asm, warnings, err := host.New(host.Config{
			Preamble:        preamble,
			Sections:        sections,
			SkillRoots:      roots,
			FileReadTool:    cfg.Tenant.Skills.FileReadTool,
			ExtraTools:      extra,
			IncludeBuiltins: true,
			ToolBackends:    backends,
			// The tenant document's tools.allow is the one filter over the
			// assembled surface (builtins and plugin/external tools alike).
			ToolsAllow: cfg.Tenant.Tools.Allow,
		})
		if err != nil {
			return nil, fmt.Errorf("assemble host: %w", err)
		}
		for _, w := range warnings {
			cfg.Seams.Logger.Warn("skill discovery", "warning", w.String())
		}
		cfg.Seams.Logger.Info("host assembled",
			"skills", len(asm.Skills()),
			"visible", len(asm.VisibleSkills()),
			"tools", len(asm.Tools().List()),
			"extra_tools", len(extra))
		return &promptService{host: asm}, nil
	}
}

// --- tools ---------------------------------------------------------------

// toolsService exposes the host's tool registry as the ctx.tools capability
// (and satisfies agent.Assembly for the loop). It is a view, not a second
// registry: skill, builtin and MCP tools keep living in the prompt service.
type toolsService struct {
	prompt *promptService
}

func (t *toolsService) SystemPrompt() string { return t.prompt.SystemPrompt() }
func (t *toolsService) RenderSystemPrompt(preamble string) string {
	return t.prompt.RenderSystemPrompt(preamble)
}
func (t *toolsService) Tools() *agenttool.ToolRegistry { return t.prompt.Tools() }
func (t *toolsService) RefreshSections(extra []agentcore.AgentTool, mcpSection string, pluginSections []host.Section) error {
	return t.prompt.RefreshSections(extra, mcpSection, pluginSections)
}
func (t *toolsService) Start(context.Context, *servicegraph.EffectScope) error { return nil }
func (t *toolsService) Stop(context.Context) error                             { return nil }

func newToolsFactory(Config) servicegraph.Factory {
	return func(_ context.Context, resolved servicegraph.Resolved) (servicegraph.Instance, error) {
		prompt, err := servicegraph.BindAs[*promptService](resolved, loadout.CapPrompt)
		if err != nil {
			return nil, err
		}
		return &toolsService{prompt: prompt}, nil
	}
}

// --- mcp -----------------------------------------------------------------

// mcpService owns the MCP connections: they are opened when the service is
// constructed and closed by Stop, so a reassembly that replaces the service
// releases every subprocess and HTTP connection with it.
type mcpService struct {
	manager  *mcp.Manager
	entries  []mcp.ServerEntry
	registry *mcphost.Registry
	log      *slog.Logger
}

func (m *mcpService) PromptSection() string      { return m.manager.PromptSection() }
func (m *mcpService) AppsPromptSection() string  { return m.manager.AppsPromptSection() }
func (m *mcpService) Entries() []mcp.ServerEntry { return append([]mcp.ServerEntry(nil), m.entries...) }

// Reconnect rebuilds one connection and re-publishes the app surface: a
// reconnected server may serve different tools and resources, and a stale
// catalogue must not survive it. A registry failure is reported but does not
// fail the reconnect itself (the connection is already live).
func (m *mcpService) Reconnect(ctx context.Context, name string) error {
	if err := m.manager.Reconnect(ctx, name); err != nil {
		return err
	}
	return m.refreshRegistry(ctx)
}

// refreshRegistry re-reads the app documents of the connected fleet. It runs
// before the first publication (Start) and after a reconnect; plugin changes do
// not re-fetch documents (the published surface is reused).
func (m *mcpService) refreshRegistry(ctx context.Context) error {
	if m.registry == nil {
		return nil
	}
	if _, err := m.registry.Refresh(ctx, m.manager); err != nil {
		m.log.Error("mcp apps refresh", "err", err)
		return err
	}
	return nil
}

func (m *mcpService) Start(ctx context.Context, _ *servicegraph.EffectScope) error {
	for _, message := range m.manager.Errors() {
		m.log.Error("mcp server", "err", message)
	}
	// The first app publication: fetch and validate every declared document
	// before the surface is assembled. A failed fetch is a diagnostic (the app
	// stays out, its tools keep their text results), never a start failure.
	if err := m.refreshRegistry(ctx); err != nil {
		m.log.Warn("mcp apps registry refresh at start", "err", err)
	}
	return nil
}

func (m *mcpService) Stop(context.Context) error {
	return m.manager.Close()
}

func newMCPFactory(cfg Config) servicegraph.Factory {
	if len(cfg.MCP.Entries) == 0 {
		return nil
	}
	return func(ctx context.Context, _ servicegraph.Resolved) (servicegraph.Instance, error) {
		options := cfg.MCP.ConnectOptions
		if cfg.MCP.Registry != nil {
			// The host registry owns "which app has a validated document": while
			// an app is absent from the published catalogue, its tools carry no
			// app binding and no mcp_apps line.
			options.AppReadiness = cfg.MCP.Registry.AppReady
		}
		manager := mcp.Connect(ctx, cfg.MCP.Entries, options)
		return &mcpService{manager: manager, entries: cfg.MCP.Entries, registry: cfg.MCP.Registry, log: cfg.Seams.Logger}, nil
	}
}

// --- compaction ----------------------------------------------------------

type compactionService struct {
	options agent.CompactionOptions
}

func (c *compactionService) Options() agent.CompactionOptions                       { return c.options }
func (c *compactionService) Start(context.Context, *servicegraph.EffectScope) error { return nil }
func (c *compactionService) Stop(context.Context) error                             { return nil }

func newCompactionFactory(cfg Config) servicegraph.Factory {
	return func(context.Context, servicegraph.Resolved) (servicegraph.Instance, error) {
		return &compactionService{options: compactionOptions(cfg.Tenant)}, nil
	}
}

// compactionOptions derives the loop's compaction knobs from the tenant
// document. The threshold is the fraction of the context window at which
// blocking compaction starts; the loop expresses it as reserve tokens.
func compactionOptions(tenant host.TenantConfig) agent.CompactionOptions {
	settings := compaction.DefaultCompactionSettings
	settings.Enabled = true
	if tenant.Compaction.KeepRecentTokens > 0 {
		settings.KeepRecentTokens = tenant.Compaction.KeepRecentTokens
	}
	window := tenant.Agent.ContextWindow
	if tenant.Compaction.Threshold > 0 && window > 0 {
		reserve := window - int(float64(window)*tenant.Compaction.Threshold)
		if reserve > 0 {
			settings.ReserveTokens = reserve
		}
	}
	return agent.CompactionOptions{ContextWindow: window, Settings: settings}
}

// --- session projection --------------------------------------------------

// projectionService is the run/read projection: the kernel Reader with the
// tenant boundary. The HTTP edge reads committed events through it, and its
// dependency on the store documents that the projection is only meaningful
// while the durable store is live.
type projectionService struct {
	reader kernel.Reader
}

func (p *projectionService) Start(context.Context, *servicegraph.EffectScope) error { return nil }
func (p *projectionService) Stop(context.Context) error                             { return nil }

func newProjectionFactory(cfg Config) servicegraph.Factory {
	return func(_ context.Context, resolved servicegraph.Resolved) (servicegraph.Instance, error) {
		if _, ok := resolved.Bind(loadout.CapStore); !ok {
			return nil, fmt.Errorf("session-projection requires the store capability")
		}
		if cfg.Seams.Reader == nil {
			return nil, fmt.Errorf("session-projection requires the kernel reader seam")
		}
		return &projectionService{reader: cfg.Seams.Reader}, nil
	}
}

// --- telemetry -----------------------------------------------------------

type telemetryService struct {
	run *agent.RunTelemetry
}

func (t *telemetryService) RunTelemetry() *agent.RunTelemetry                      { return t.run }
func (t *telemetryService) Start(context.Context, *servicegraph.EffectScope) error { return nil }
func (t *telemetryService) Stop(context.Context) error                             { return nil }

func newTelemetryFactory(cfg Config) servicegraph.Factory {
	return func(context.Context, servicegraph.Resolved) (servicegraph.Instance, error) {
		memory := telemetry.NewMemory()
		runContext := telemetry.Context(memory)
		if cfg.Product.WrapTelemetry != nil {
			runContext = cfg.Product.WrapTelemetry(runContext)
		}
		return &telemetryService{run: &agent.RunTelemetry{Context: runContext, Metrics: memory}}, nil
	}
}

// --- pi-loop -------------------------------------------------------------

// loopService is the "pi-loop" instance in production. It owns the run
// coordinator (agent.Runner): the loop's PI behavior plus the kernel commit
// path for one generation. A reassembly rebuilds it against the new llm/tools
// bindings; runs pin the instance through their graph lease.
type loopService struct {
	runner *agent.Runner
}

func (l *loopService) Runner() *agent.Runner                                  { return l.runner }
func (l *loopService) Start(context.Context, *servicegraph.EffectScope) error { return nil }
func (l *loopService) Stop(context.Context) error                             { return nil }

func newPiLoopFactory(cfg Config) servicegraph.Factory {
	return func(_ context.Context, resolved servicegraph.Resolved) (servicegraph.Instance, error) {
		llm, err := servicegraph.BindAs[LLMService](resolved, loadout.CapLLM)
		if err != nil {
			return nil, err
		}
		assembly, err := servicegraph.BindAs[agent.Assembly](resolved, loadout.CapTools)
		if err != nil {
			return nil, err
		}
		runner := agent.NewRunner(cfg.Seams.Committer, cfg.Seams.Reader,
			llm.StreamFn(), llm.ProviderName(), llm.Model(), llm.APIKey(), cfg.Seams.Logger)
		// The attempt's execution identity comes from the provider configuration
		// when the tenant declares one; without it the runner's static fields
		// (the fake/legacy path) stay authoritative.
		if models := llm.Models(); models != nil {
			runner.Models = models
		}
		runner.Assembly = assembly
		// The execution binding (Agent/AgentTeam the input selected) resolves per
		// attempt through this seam: the runner reads the binding the selection
		// fact names and freezes its identity into the run summary. A zero
		// BindingsOptions keeps the pre-binding behaviour.
		if cfg.Bindings.Reader != nil {
			runner.Bindings = &bindingResolver{
				reader: cfg.Bindings.Reader,
				root:   cfg.Bindings.Root,
				log:    cfg.Seams.Logger,
			}
		}
		if cfg.Tenant.Agent.MaxTurns > 0 {
			runner.MaxTurns = cfg.Tenant.Agent.MaxTurns
		}
		if bound, ok := resolved.Bind(loadout.CapCompaction); ok {
			policy, ok := bound.(*compactionService)
			if !ok {
				return nil, fmt.Errorf("ctx.compaction is bound to %T, want *compactionService", bound)
			}
			opts := policy.Options()
			runner.Compaction = &opts
		}
		// The outbound image pipeline (keel-ga §3 provider-files): the runner
		// resolves a transcript image reference (blobId) into the wire form the
		// attempt's provider accepts, right before each request. A deployment
		// without a blob source keeps the nil resolver, and a referenced image
		// degrades to a text notice naming why (never a silent drop).
		if resolver := newImageResolver(cfg); resolver != nil {
			// Assigned only when non-nil: a typed nil in the interface would look
			// wired to every nil check downstream.
			runner.Images = resolver
		}
		return &loopService{runner: runner}, nil
	}
}

// newImageResolver builds the outbound image resolver from the hosting process
// inputs. Uploaders are created per attempt from the attempt's own endpoint and
// credential (see provider.ImageTarget): a provider file handle is only usable by
// the account that uploaded it, so the resolver never borrows another key. The
// file cache dedupes uploads by (sha256, provider, workspace) and refreshes an
// expired handle lazily (Gemini's 48h window).
//
// A deployment with no blob source returns nil: the runner's degradation path is
// the honest answer there, and constructing uploaders nothing can feed would only
// hide the missing wiring.
func newImageResolver(cfg Config) *provider.ImageResolver {
	if cfg.LLM.Images == nil {
		return nil
	}
	resolver := &provider.ImageResolver{
		Blobs: cfg.LLM.Images,
		Files: provider.NewFileCache(0),
	}
	if base := strings.TrimSpace(cfg.LLM.PublicBaseURL); base != "" {
		resolver.URLs = &provider.PathURLSigner{Base: base}
	}
	return resolver
}

// --- worker --------------------------------------------------------------

// workerService owns the claim loop: Start launches it, Stop cancels and
// waits, Drain only stops new claims (SIGTERM drain leaves in-flight attempts
// to settle; Close still tears the generation down afterwards).
type workerService struct {
	worker *agent.Worker
	log    *slog.Logger

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func (w *workerService) Start(ctx context.Context, effects *servicegraph.EffectScope) error {
	w.mu.Lock()
	if w.cancel != nil {
		w.mu.Unlock()
		return nil
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	w.cancel, w.done = cancel, done
	w.mu.Unlock()

	go func() {
		defer close(done)
		w.worker.Run(runCtx)
	}()
	// A cancelled activation context (process shutdown) stops claiming even if
	// nobody calls Drain/Stop.
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-done:
		}
	}()
	if effects != nil {
		if err := effects.Register("worker-pool", func() error {
			cancel()
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (w *workerService) Stop(context.Context) error {
	w.Drain()
	w.mu.Lock()
	done := w.done
	w.mu.Unlock()
	if done != nil {
		<-done
	}
	return nil
}

// Drain cancels the worker's context: claiming stops and in-flight attempts are
// abandoned immediately (each gives its lease back so another runtime can claim
// it, or the next process sees the run as pending). It is not a graceful
// "continue running what started" drain; handoff-safe drainage is the
// coordinator's job, not this service's.
func (w *workerService) Drain() {
	w.mu.Lock()
	cancel := w.cancel
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func newWorkerFactory(cfg Config, engine *servicegraph.Engine) servicegraph.Factory {
	return func(_ context.Context, resolved servicegraph.Resolved) (servicegraph.Instance, error) {
		loop, err := servicegraph.BindAs[*loopService](resolved, loadout.CapLoop)
		if err != nil {
			return nil, err
		}
		mirror, err := servicegraph.BindAs[agent.RunMirror](resolved, loadout.CapStore)
		if err != nil {
			return nil, err
		}
		// The worker's descriptor requires prompt and tools: binding them here
		// proves the run path exists in this generation (the loop already
		// consumed tools; prompt is the assembly the tools view delegates to).
		if _, err := servicegraph.BindAs[*promptService](resolved, loadout.CapPrompt); err != nil {
			return nil, err
		}
		runner := loop.Runner()
		if bound, ok := resolved.Bind(loadout.CapTelemetry); ok {
			if telemetryService, ok := bound.(*telemetryService); ok && telemetryService.RunTelemetry() != nil {
				runner.Telemetry = telemetryService.RunTelemetry()
			}
		}
		worker := agent.NewWorker(cfg.Seams.Leases, mirror, runner, cfg.RuntimeID, cfg.Concurrency, cfg.Seams.Logger)
		worker.Metrics = cfg.Metrics
		worker.DrainGate = cfg.DrainGate
		worker.Control = cfg.Product.RunControl
		// The attempt summary is a durable fact (run.finished) written under the
		// run's lease; without the commit seam the worker settles without it.
		worker.Committer = cfg.Seams.Committer
		worker.Binding = &graphBinding{engine: engine}
		return &workerService{worker: worker, log: cfg.Seams.Logger}, nil
	}
}

// --- external gateway ----------------------------------------------------

// externalOptions maps the hosting process's external inputs onto the service
// package's options. The kernel seams are the only handles the services get;
// no store or table handle crosses this boundary.
func externalOptions(cfg Config) externalgw.Options {
	return externalgw.Options{
		Committer:      cfg.Seams.Committer,
		Sessions:       cfg.Seams.External,
		LookupEnv:      cfg.External.LookupEnv,
		Environ:        cfg.External.Environ,
		Logger:         cfg.Seams.Logger,
		Stderr:         cfg.External.Stderr,
		Restart:        cfg.External.Restart,
		ConnectTimeout: cfg.External.ConnectTimeout,
		HTTPClient:     cfg.External.HTTPClient,
	}
}

// newExternalAgentFactory builds the AgentBackend service from the tenant
// document's external.backends. A document that declares backends without the
// external-session seam fails activation with a readable diagnostic instead of
// running half-wired.
func newExternalAgentFactory(cfg Config) servicegraph.Factory {
	return func(_ context.Context, resolved servicegraph.Resolved) (servicegraph.Instance, error) {
		if _, ok := resolved.Bind(loadout.CapStore); !ok {
			return nil, fmt.Errorf("external-agent requires the store capability")
		}
		if cfg.Seams.External == nil {
			return nil, fmt.Errorf("external-agent requires the external-session seam")
		}
		return externalgw.NewAgentService(cfg.Tenant.External, externalOptions(cfg))
	}
}

// newExternalToolsFactory builds the ToolProvider service from the tenant
// document's external.tools.
func newExternalToolsFactory(cfg Config) servicegraph.Factory {
	return func(context.Context, servicegraph.Resolved) (servicegraph.Instance, error) {
		return externalgw.NewToolService(cfg.Tenant.External, externalOptions(cfg))
	}
}
