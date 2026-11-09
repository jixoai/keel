// Package servicehost assembles the production runtime through the service
// graph: it turns the hosting process's tenant document into the default
// loadout (internal/loadout), attaches the concrete service factories, applies
// the loadout to a servicegraph.Engine, and hands the running instances back to
// cmd/runtime.
//
// The package is the wiring layer, not a second runtime: the HTTP edge, the
// store and the contract kernel are created by the caller and injected through
// narrow seams (kernel.Committer/Reader/LeaseManager, agent.RunMirror) — no
// service ever receives a *store.Store write handle or a table handle.
//
// # Run pinning
//
// The engine generation is pinned per run: Runtime.Binding (installed on the
// worker) acquires a servicegraph lease for every claimed run, resolves the
// `pi-loop` instance of that generation and releases the pin when the attempt
// settles. The worker records the pinned loadout revision/generation with the
// run (kernel.FinishRequest.Loadout). Reload applies a new loadout; an
// in-flight run keeps its old generation, which enters RETIRING and is unloaded
// in reverse dependency order once its references drain.
//
// # Idle collection
//
// A host ticker calls Engine.CollectExpired so a RETIRING generation is
// finished even when no run is claimed or loadout applied (the engine alone
// only collects on Apply/Acquire). The collector is started by Boot and
// stopped by Close.
package servicehost

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/externalgw"
	"github.com/jixoai/keel/internal/gateway"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/loadout"
	"github.com/jixoai/keel/internal/mcp"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/quota"
	"github.com/jixoai/keel/internal/servicegraph"
	"github.com/jixoai/keel/internal/telemetry"
)

// Default collector timing. The retire timeout bounds how long an old
// generation may keep running after a reload; the interval bounds how long an
// idle process may keep its effects alive past that deadline.
const (
	DefaultRetireTimeout   = 2 * time.Minute
	DefaultCollectInterval = 5 * time.Second
)

// Seams are the kernel and process seams the services may use. Every field is
// a narrow interface by design (see internal/kernel/seams.go): a service
// factory below receives these through the hosting process, never a *Kernel,
// *store.Store or table handle.
type Seams struct {
	Committer kernel.Committer
	Reader    kernel.Reader
	Leases    kernel.LeaseManager
	// External is the external-session mark seam the external gateway uses
	// (nil disables the external services: a document that declares providers
	// then records an activation diagnostic instead of half-wiring them).
	External kernel.ExternalSessions
	// Runs is the run-row mirror the worker reads for logs/metrics only.
	Runs agent.RunMirror
	// Limits is the kernel admission seam; Reload updates it from the new
	// tenant document (nil disables the update).
	Limits *quota.Limits
	Logger *slog.Logger
}

// PromptOptions carries the hosting process's host-assembly inputs that are not
// part of the tenant document: the paths MCP/skills resolution already loaded,
// and the "default MCP section" the prompt appends before the graph runs.
type PromptOptions struct {
	// Preamble overrides Tenant.Agent.SystemPrompt when non-empty (the runtime
	// default prompt). It is kept separate because an empty tenant
	// systemPrompt means "inherit the runtime default".
	Preamble string
	// Sections prepends extra sections to the tenant document's sections.
	Sections []host.Section
}

// MCPOptions configures the mcp service. Entries are the already-loaded
// mcp.json servers (the runtime keeps mcp.json as the connection source; the
// tenant document only names the enabled servers). A nil/empty Entries
// disables the service entirely.
type MCPOptions struct {
	Entries        []mcp.ServerEntry
	ConnectOptions mcp.ConnectOptions
}

// LLMOptions carries the resolved model selection inputs. The tenant document
// names provider/model/baseURL/protocol; the hosting process supplies the
// credential and the environment lookup the provider resolver expects.
type LLMOptions struct {
	APIKey string
	// LookupEnv resolves provider credentials referenced by apiKeyRef or by the
	// provider's own env conventions. Defaults to a lookup that finds nothing.
	LookupEnv func(string) (string, bool)
}

// ExternalOptions carries the hosting-process inputs the external gateway
// needs beyond the kernel seams: secret resolution for the declared provider
// endpoints, the base environment a spawned provider may derive its minimal
// environment from, and the process supervision policy.
type ExternalOptions struct {
	// LookupEnv resolves ${NAME}/env:NAME/secret:NAME in the tenant document.
	LookupEnv func(string) (string, bool)
	// Environ is the host environment; nil means os.Environ().
	Environ []string
	// Restart bounds backend/tool-provider reconnect attempts.
	Restart gateway.RestartPolicy
	// ConnectTimeout bounds one provider connect attempt.
	ConnectTimeout time.Duration
	// HTTPClient is used for http providers (nil means http.DefaultClient).
	HTTPClient *http.Client
	// Stderr receives spawned providers' stderr (nil means os.Stderr).
	Stderr io.Writer
}

// ProductOptions is the product layer's (keel-agent) assembly hook. It is the
// only place the extension surface touches the run path, and every field is
// optional: a kernel-only deployment passes none of them.
type ProductOptions struct {
	// ExtraTools are static product tools appended to the host registry (plugin
	// tools added later go through Runtime.RefreshProductTools).
	ExtraTools []agentcore.AgentTool
	// WrapTelemetry decorates the run telemetry context. The runner opens one
	// keel.agent.run span per attempt; the product layer turns that span into
	// the turn.started/turn.finished hooks without a worker change.
	WrapTelemetry func(base telemetry.Context) telemetry.Context
	// WrapStream decorates every provider stream (the run's model calls). It is
	// the product layer's observation point for token-level partials.
	WrapStream func(base provider.StreamFn) provider.StreamFn
	// RunControl registers each executing attempt so the product layer can stop
	// it (POST /v1/sessions/{id}/cancel). The worker consults it per claim; nil
	// keeps cancellation outside the process (the endpoint answers 501).
	RunControl agent.RunController
}

// Config is the hosting-process input for Boot.
type Config struct {
	Seams  Seams
	Tenant host.TenantConfig
	// Options is the loadout snapshot header and the process-level worker
	// switch.
	Options loadout.Options
	// RuntimeID is the worker owner identity written into every claim.
	RuntimeID string
	// Concurrency caps the process-wide worker (0 = agent default).
	Concurrency int
	// Metrics observes worker runs (optional).
	Metrics agent.RunObserver
	// DrainGate, when set, closes the worker's claim path during a drain and
	// reports the in-flight run count (internal/handoff's Coordinator).
	DrainGate agent.DrainGate

	Prompt PromptOptions
	MCP    MCPOptions
	LLM    LLMOptions
	// External carries the external gateway's hosting-process inputs. The
	// tenant document (Tenant.External) declares the providers; this is only
	// how the process resolves their secrets and supervises them.
	External ExternalOptions
	// Product is the keel-agent assembly hook (extra tools, telemetry and
	// provider-stream decorators). All fields are optional.
	Product ProductOptions

	RetireTimeout   time.Duration
	CollectInterval time.Duration
}

// Runtime is one assembled service graph plus the handles the HTTP shell needs.
type Runtime struct {
	engine   *servicegraph.Engine
	cfg      Config
	tenant   host.TenantConfig
	opts     loadout.Options
	provider func(string) servicegraph.Factory

	mu       sync.Mutex
	snapshot *servicegraph.Snapshot
	closed   bool

	collectCancel context.CancelFunc
	collectDone   chan struct{}
}

// Boot resolves the tenant document into the default loadout, applies it, and
// returns the live runtime. A failed activation of a run-path service is a boot
// error (the runtime cannot serve); services that are optional by document
// (MCP, compaction, telemetry) are absent by construction.
func Boot(ctx context.Context, cfg Config) (*Runtime, error) {
	if cfg.Seams.Committer == nil || cfg.Seams.Reader == nil || cfg.Seams.Leases == nil || cfg.Seams.Runs == nil {
		return nil, fmt.Errorf("servicehost: kernel seams (committer, reader, leases, run mirror) are required")
	}
	if cfg.Seams.Logger == nil {
		cfg.Seams.Logger = slog.Default()
	}
	if cfg.RetireTimeout <= 0 {
		cfg.RetireTimeout = DefaultRetireTimeout
	}
	if cfg.CollectInterval <= 0 {
		cfg.CollectInterval = DefaultCollectInterval
	}

	engine := servicegraph.NewEngine(
		servicegraph.WithLogger(cfg.Seams.Logger),
		servicegraph.WithRetireTimeout(cfg.RetireTimeout),
	)
	provide := newProvider(cfg.Tenant, cfg, engine)
	lo, err := loadout.FromTenantConfig(cfg.Tenant, cfg.Options, provide)
	if err != nil {
		return nil, fmt.Errorf("servicehost: resolve loadout: %w", err)
	}
	snap, err := engine.Apply(ctx, lo)
	if err != nil {
		if activation, ok := err.(*servicegraph.ActivationError); ok {
			// Keep the activation error visible, but only fail boot when a
			// service the run path depends on did not activate.
			cfg.Seams.Logger.Error("loadout activation incomplete", "generation", activation.Generation, "err", err)
		} else {
			engine.Close(context.WithoutCancel(ctx))
			return nil, fmt.Errorf("servicehost: apply loadout: %w", err)
		}
	}
	if err := requireActive(snap, cfg.Options.Worker); err != nil {
		engine.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	rt := &Runtime{
		engine:   engine,
		cfg:      cfg,
		tenant:   cfg.Tenant,
		opts:     cfg.Options,
		provider: provide,
		snapshot: snap,
	}
	rt.startCollector(ctx)
	cfg.Seams.Logger.Info("service loadout active",
		"loadout", snap.LoadoutID,
		"revision", snap.Revision,
		"generation", snap.Generation,
		"summary", snap.Summary,
		"services", len(snap.Services),
		"retiring", len(snap.Retiring))
	return rt, nil
}

// requireActive fails boot when a service on the run path is not ACTIVE. A
// document-disabled optional service (mcp/compaction/telemetry/sandbox) is
// simply absent and is not checked.
func requireActive(snap *servicegraph.Snapshot, worker bool) error {
	required := []string{
		loadout.ServiceStore,
		loadout.ServiceLLM,
		loadout.ServicePrompt,
		loadout.ServiceTools,
		loadout.ServicePiLoop,
		loadout.ServiceSessionProjection,
	}
	if worker {
		required = append(required, loadout.ServiceWorker)
	}
	for _, name := range required {
		if snap.State(name) != servicegraph.StateActive {
			return fmt.Errorf("servicehost: service %q is %s, not ACTIVE: %s", name, snap.State(name), snap.Explain(name))
		}
	}
	return nil
}

// Snapshot returns the current graph snapshot.
func (r *Runtime) Snapshot() *servicegraph.Snapshot {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshot
}

// Assembly returns the host assembly (prompt + tool registry + MCP refresh)
// that the HTTP shell exposes to the MCP sign-in endpoint.
func (r *Runtime) Assembly() RefreshableAssembly {
	if r == nil {
		return nil
	}
	if svc, ok := r.engine.Lookup(loadout.ServiceTools); ok {
		if tools, ok := svc.(*toolsService); ok {
			return tools
		}
	}
	return nil
}

// Host exposes the prompt service for diagnostics/tests.
func (r *Runtime) Host() *host.Host {
	if r == nil {
		return nil
	}
	if svc, ok := r.engine.Lookup(loadout.ServicePrompt); ok {
		if prompt, ok := svc.(*promptService); ok {
			return prompt.host
		}
	}
	return nil
}

// Projection returns the read seam of the session-projection service (the
// kernel Reader with the tenant boundary). The HTTP edge reads events through
// it.
func (r *Runtime) Projection() kernel.Reader {
	if r == nil {
		return nil
	}
	if svc, ok := r.engine.Lookup(loadout.ServiceSessionProjection); ok {
		if projection, ok := svc.(*projectionService); ok {
			return projection.reader
		}
	}
	return nil
}

// ProviderName and Model describe the llm service in use.
func (r *Runtime) ProviderName() string { return r.llm().ProviderName() }
func (r *Runtime) Model() string        { return r.llm().Model() }

// Models returns the provider configuration of the current generation, or nil
// when the tenant declares none (no models document and no legacy model): the
// hosting process then exposes no selection vocabulary at all.
func (r *Runtime) Models() ModelService {
	if r == nil {
		return nil
	}
	if svc, ok := r.engine.Lookup(loadout.ServiceLLM); ok {
		if llm, ok := svc.(*llmService); ok {
			return llm.models
		}
	}
	return nil
}

// ExternalAgents returns the AgentBackend service of the current generation
// (nil when the tenant document declares no agent backend). The HTTP edge uses
// it for the external-session endpoints.
func (r *Runtime) ExternalAgents() *externalgw.AgentService {
	if r == nil {
		return nil
	}
	if svc, ok := r.engine.Lookup(loadout.ServiceExternalAgent); ok {
		if agents, ok := svc.(*externalgw.AgentService); ok {
			return agents
		}
	}
	return nil
}

// ExternalTools returns the ToolProvider service of the current generation
// (nil when no tool provider is declared).
func (r *Runtime) ExternalTools() *externalgw.ToolService {
	if r == nil {
		return nil
	}
	if svc, ok := r.engine.Lookup(loadout.ServiceExternalTools); ok {
		if tools, ok := svc.(*externalgw.ToolService); ok {
			return tools
		}
	}
	return nil
}

func (r *Runtime) llm() *llmService {
	if r == nil {
		return &llmService{}
	}
	if svc, ok := r.engine.Lookup(loadout.ServiceLLM); ok {
		if llm, ok := svc.(*llmService); ok {
			return llm
		}
	}
	return &llmService{}
}

// MCPEntries returns the loaded MCP server entries (nil when the service is
// disabled). The HTTP shell uses them for the sign-in endpoint.
func (r *Runtime) MCPEntries() []mcp.ServerEntry {
	if r == nil {
		return nil
	}
	if svc, ok := r.engine.Lookup(loadout.ServiceMCP); ok {
		if mcpService, ok := svc.(*mcpService); ok {
			return mcpService.Entries()
		}
	}
	return nil
}

// ReconnectMCP reconnects one MCP server and republishes its tools and prompt
// section on the live host, so a sign-in takes effect without a restart.
func (r *Runtime) ReconnectMCP(ctx context.Context, name string) error {
	if r == nil {
		return fmt.Errorf("servicehost: runtime closed")
	}
	svc, ok := r.engine.Lookup(loadout.ServiceMCP)
	if !ok {
		return fmt.Errorf("servicehost: MCP is not enabled")
	}
	mcpService, ok := svc.(*mcpService)
	if !ok {
		return fmt.Errorf("servicehost: MCP service has unexpected type %T", svc)
	}
	if err := mcpService.Reconnect(ctx, name); err != nil {
		return err
	}
	assembly := r.Assembly()
	if assembly == nil {
		return fmt.Errorf("servicehost: host assembly is not available")
	}
	if err := assembly.RefreshMCP(mcpService.manager.Tools(), mcpService.manager.PromptSection()); err != nil {
		return fmt.Errorf("refresh host after MCP reconnect: %w", err)
	}
	return nil
}

// RefreshProductTools republishes the host tool registry with the product
// layer's dynamic tool set (plugin tools). It preserves the MCP and external
// tool providers, which share the one registry, so a refresh never silently
// drops a capability. The registry swap is live: an already running attempt
// resolves tools per model call, so a newly assembled plugin is visible without
// a restart (and an unloaded one disappears).
func (r *Runtime) RefreshProductTools(dynamic []agentcore.AgentTool) error {
	if r == nil {
		return fmt.Errorf("servicehost: runtime closed")
	}
	assembly := r.Assembly()
	if assembly == nil {
		return fmt.Errorf("servicehost: host assembly is not available")
	}
	var extra []agentcore.AgentTool
	section := ""
	if svc, ok := r.engine.Lookup(loadout.ServiceMCP); ok {
		if mcpService, ok := svc.(*mcpService); ok {
			extra = append(extra, mcpService.Tools()...)
			section = mcpService.PromptSection()
		}
	}
	if svc, ok := r.engine.Lookup(loadout.ServiceExternalTools); ok {
		if toolsService, ok := svc.(*externalgw.ToolService); ok {
			extra = append(extra, toolsService.Tools()...)
		}
	}
	extra = append(extra, r.cfg.Product.ExtraTools...)
	extra = append(extra, dynamic...)
	return assembly.RefreshMCP(extra, section)
}

// StopWorker stops claiming new runs (SIGTERM drain) without unloading the
// graph; in-flight attempts continue and Close is still required.
func (r *Runtime) StopWorker() {
	if r == nil {
		return
	}
	if svc, ok := r.engine.Lookup(loadout.ServiceWorker); ok {
		if worker, ok := svc.(*workerService); ok {
			worker.Drain()
		}
	}
}

// Reload resolves and applies a new tenant document (the next-run picture): a
// new snapshot revision/generation becomes current, while runs already pinned
// to the previous generation keep their instances. On a rejected loadout the
// current generation stays untouched.
//
// The factories are rebuilt from the incoming document, so a document-derived
// service (the model configuration) is assembled from the configuration being
// applied. A document that leaves a required run-path service failed is
// reported as a reload error instead of being adopted silently.
func (r *Runtime) Reload(ctx context.Context, tenant host.TenantConfig, opts loadout.Options) (*servicegraph.Snapshot, error) {
	if r == nil {
		return nil, fmt.Errorf("servicehost: runtime closed")
	}
	provide := newProvider(tenant, r.cfg, r.engine)
	lo, err := loadout.FromTenantConfig(tenant, opts, provide)
	if err != nil {
		return nil, fmt.Errorf("servicehost: reload resolve: %w", err)
	}
	snap, err := r.engine.Apply(ctx, lo)
	if err != nil {
		if _, ok := err.(*servicegraph.ActivationError); !ok {
			return nil, fmt.Errorf("servicehost: reload apply: %w", err)
		}
		r.cfg.Seams.Logger.Error("reload activation incomplete", "err", err)
	}
	if err := requireActive(snap, opts.Worker); err != nil {
		return nil, fmt.Errorf("servicehost: reload rejected: %w", err)
	}
	r.mu.Lock()
	r.tenant, r.opts, r.snapshot, r.provider = tenant, opts, snap, provide
	r.mu.Unlock()
	if r.cfg.Seams.Limits != nil {
		r.cfg.Seams.Limits.Update(tenant.Quota)
	}
	r.cfg.Seams.Logger.Info("service loadout reloaded",
		"loadout", snap.LoadoutID, "revision", snap.Revision, "generation", snap.Generation,
		"summary", snap.Summary, "retiring", len(snap.Retiring))
	return snap, nil
}

// Binding returns the per-run service pin installed on the worker (also
// exposed for tests that exercise reassembly).
func (r *Runtime) Binding() agent.RunBinding {
	return &graphBinding{engine: r.engine}
}

func (r *Runtime) startCollector(ctx context.Context) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r.collectCancel = cancel
	r.collectDone = make(chan struct{})
	interval := r.cfg.CollectInterval
	go func() {
		defer close(r.collectDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// CollectExpired only force-unloads generations past their
				// drain deadline and invalidates the leases still holding
				// them; an idle process therefore still finishes RETIRING.
				if n, err := r.engine.CollectExpired(ctx); err != nil {
					r.cfg.Seams.Logger.Warn("collect expired loadout generations", "error", err)
				} else if n > 0 {
					r.cfg.Seams.Logger.Info("expired loadout generation collected", "instances", n)
				}
			}
		}
	}()
}

// Close stops the collector and unloads every service in reverse dependency
// order (worker first, store last). It is idempotent.
func (r *Runtime) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	if r.collectCancel != nil {
		r.collectCancel()
		<-r.collectDone
	}
	return r.engine.Close(context.WithoutCancel(ctx))
}

// graphBinding implements agent.RunBinding over one engine generation.
type graphBinding struct {
	engine *servicegraph.Engine
}

func (b *graphBinding) Acquire(ctx context.Context) (agent.BoundRun, error) {
	lease, err := b.engine.Acquire()
	if err != nil {
		return nil, err
	}
	instance, ok := lease.Lookup(loadout.ServicePiLoop)
	if !ok {
		lease.Release(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("servicehost: generation %d has no %s instance", lease.Generation(), loadout.ServicePiLoop)
	}
	loop, ok := instance.(*loopService)
	if !ok {
		lease.Release(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("servicehost: %s instance has unexpected type %T", loadout.ServicePiLoop, instance)
	}
	return &boundRun{
		lease:   lease,
		runner:  loop.Runner(),
		loadout: agent.LoadoutRef{ID: lease.LoadoutID(), Revision: lease.Revision(), Generation: lease.Generation()},
	}, nil
}

type boundRun struct {
	lease   *servicegraph.Lease
	runner  *agent.Runner
	loadout agent.LoadoutRef
}

func (b *boundRun) Runner() *agent.Runner             { return b.runner }
func (b *boundRun) Loadout() agent.LoadoutRef         { return b.loadout }
func (b *boundRun) Release(ctx context.Context) error { return b.lease.Release(ctx) }

var _ agent.RunBinding = (*graphBinding)(nil)
var _ agent.BoundRun = (*boundRun)(nil)

// RefreshableAssembly is the host view the HTTP shell uses; it is agent.Assembly
// plus the MCP refresh seam.
type RefreshableAssembly interface {
	agent.Assembly
	RefreshMCP(extra []agentcore.AgentTool, section string) error
}

var _ RefreshableAssembly = (*toolsService)(nil)
