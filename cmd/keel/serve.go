package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentapi"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/blob"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/httpapi"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/loadout"
	"github.com/jixoai/keel/internal/mcp"
	"github.com/jixoai/keel/internal/modelcfg"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/quota"
	"github.com/jixoai/keel/internal/servicehost"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/stream"
	"github.com/jixoai/keel/internal/telemetry"
)

// serveOptions is the resolved `keel serve` configuration (flags + environment).
type serveOptions struct {
	Addr            string
	DSN             string
	WebDir          string
	CASDir          string
	TenantID        uuid.UUID
	Password        string
	SecureCookies   bool
	Runs            int
	NoWorker        bool
	Model           string
	Provider        string
	BaseURL         string
	Protocol        string
	APIKey          string
	SystemPrompt    string
	SkillsRoots     []string
	LoadoutID       string
	LoadoutRevision int64
	MCPConfig       string
	MCPProject      string
	// ModelsConfig is the path of the tenant models document
	// (providers/rules/selection); empty leaves provider configuration off.
	ModelsConfig    string
	ExampleInterval time.Duration
	Log             *slog.Logger
}

const (
	defaultServeAddr     = "127.0.0.1:13750"
	defaultSystemPrompt  = "You are an AI employee running on the Keel agent runtime. Use the available tools when they help."
	defaultServeWebDir   = "web/dist"
	serveShutdownTimeout = 10 * time.Second
)

// runServe assembles the product server and blocks until the process is
// signalled. It is the only place that knows about every layer; the packages it
// wires stay unaware of each other's policy.
func runServe(parent context.Context, opts serveOptions) error {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	if opts.DSN == "" {
		return errors.New("KEEL_DSN (or --dsn) is required")
	}
	if opts.Addr == "" {
		opts.Addr = defaultServeAddr
	}
	if opts.Runs <= 0 {
		opts.Runs = 64
	}
	if opts.LoadoutID == "" {
		opts.LoadoutID = loadout.DefaultID
	}
	if opts.LoadoutRevision <= 0 {
		opts.LoadoutRevision = 1
	}
	if opts.SystemPrompt == "" {
		opts.SystemPrompt = defaultSystemPrompt
	}
	if opts.TenantID == uuid.Nil && opts.Password != "" {
		return errors.New("KEEL_SERVE_TENANT is required when authentication is enabled")
	}

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, store.Config{DSN: opts.DSN})
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	kernelLog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		return fmt.Errorf("kernel migrate: %w", err)
	}
	limits := quota.New(host.QuotaConfig{
		RunConcurrency:  envInt("KEEL_QUOTA_RUNS", 0),
		EventsPerMinute: envInt("KEEL_QUOTA_EVENTS_PER_MINUTE", 0),
	})
	kern := kernel.New(kernelLog, kernel.Options{
		Limits: limits,
		Audit:  servicehost.NewAuditSink(log, servicehost.AuditOptions{}),
	})

	hub := httpapi.NewHub(st, log)
	go hub.Run(ctx)

	// Static product tools are empty at boot: plugin tools are published after
	// the plugin runtime assembles, through Runtime.RefreshProductTools.
	// The hook bus exists before the service graph so the run telemetry
	// decorator can be installed at assembly time: the runner's
	// keel.agent.run span becomes turn.started/turn.finished with no worker
	// change.
	hooks := agentext.NewHookBus(agentext.HookBusOptions{Logger: log})
	// The run-control seam is created before the service graph so the worker
	// registers every attempt: the cancel endpoint can then stop an in-process
	// run instead of answering 501.
	runControl := agentext.NewRunControl(storeSessions{Store: st}, log)
	// Partials are created before the service graph: the provider-stream tee
	// publishes the run's text deltas into this hub, and the SSE endpoint reads
	// them for clients that ask (?partials=1).
	partials := agentext.NewPartialHub()

	var mcpEntries []mcp.ServerEntry
	if opts.MCPConfig != "" {
		loaded := mcp.LoadConfigFiles(opts.MCPConfig, opts.MCPProject)
		mcpEntries = loaded.Servers
		for _, message := range loaded.Errors {
			log.Warn("mcp config", "err", message)
		}
	}
	// The tenant models document is the provider configuration source; an empty
	// path leaves provider configuration off (the legacy flat agent config
	// still normalizes to one provider + selection).
	modelsDoc, err := host.LoadModelsDocument(opts.ModelsConfig)
	if err != nil {
		return fmt.Errorf("load models config: %w", err)
	}
	tenantCfg := host.TenantConfig{
		Agent: host.AgentConfig{
			Provider:     opts.Provider,
			Model:        opts.Model,
			BaseURL:      opts.BaseURL,
			Protocol:     opts.Protocol,
			SystemPrompt: opts.SystemPrompt,
		},
		Models: modelsDoc,
		Skills: host.SkillsConfig{Roots: opts.SkillsRoots},
		MCP:    host.MCPConfig{Servers: mcpServerEntries(mcpEntries)},
		Quota:  host.QuotaConfig{RunConcurrency: envInt("KEEL_QUOTA_RUNS", 0), EventsPerMinute: envInt("KEEL_QUOTA_EVENTS_PER_MINUTE", 0)},
	}
	limits.Update(tenantCfg.Quota)

	rt, err := servicehost.Boot(ctx, servicehost.Config{
		Seams: servicehost.Seams{
			Committer: kern,
			Reader:    kern,
			Leases:    kern,
			External:  kern,
			Runs:      st,
			Limits:    limits,
			Logger:    log,
		},
		Tenant: tenantCfg,
		Options: loadout.Options{
			ID:             opts.LoadoutID,
			Revision:       opts.LoadoutRevision,
			Worker:         !opts.NoWorker,
			RunConcurrency: opts.Runs,
		},
		RuntimeID:   runtimeID(),
		Concurrency: opts.Runs,
		Prompt:      servicehost.PromptOptions{Preamble: opts.SystemPrompt},
		MCP: servicehost.MCPOptions{
			Entries:        mcpEntries,
			ConnectOptions: mcp.ConnectOptions{ClientName: "keel", ClientVersion: version, Logger: log},
		},
		LLM: servicehost.LLMOptions{APIKey: opts.APIKey, LookupEnv: os.LookupEnv},
		External: servicehost.ExternalOptions{
			LookupEnv: os.LookupEnv,
			Environ:   os.Environ(),
			Stderr:    os.Stderr,
		},
		Product: servicehost.ProductOptions{
			WrapTelemetry: func(base telemetry.Context) telemetry.Context {
				return &agentext.TurnTelemetry{Base: base, Hooks: hooks}
			},
			WrapStream: func(base provider.StreamFn) provider.StreamFn {
				return agentext.StreamTee(base, partials)
			},
			RunControl: runControl,
		},
	})
	if err != nil {
		hooks.Close()
		return fmt.Errorf("assemble service loadout: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), serveShutdownTimeout)
		defer cancel()
		if err := rt.Close(closeCtx); err != nil {
			log.Warn("close service loadout", "err", err)
		}
	}()

	// --- product layer (keel-agent) ---
	product, err := newProductLayer(ctx, st, kern, rt, hooks, runControl, partials, opts, log)
	if err != nil {
		return err
	}
	defer product.close()

	// The resolved provider configuration is product configuration: a
	// deployment whose service assembly carries none must refuse selections at
	// the input edge instead of storing a fact it cannot honour.
	var validateSelection func(json.RawMessage) error
	if svc := rt.Models(); svc != nil {
		validateSelection = svc.ValidateSelection
	}

	// Drain is the same edge contract the runtime uses: new requests get 503 +
	// Retry-After, every open SSE stream receives the drain frame and closes at
	// the configured rate. keel serve owns the worker, so draining also stops
	// claiming.
	drainer := httpapi.NewDrainer(3000, 200)
	streamMetrics := &stream.Metrics{}
	api := httpapi.New(httpapi.Config{
		Store:     st,
		Hub:       hub,
		Edge:      kern,
		Events:    rt.Projection(),
		Blobs:     blob.NewReader(st, 256, 1),
		Uploader:  &blob.Uploader{Store: st, CASDir: opts.CASDir, TempDir: os.TempDir()},
		Scheduler: stream.NewScheduler(256),
		Admitter:  stream.NewAdmitter(200, 400),
		Limiter:   stream.NewLimiter(0),
		Metrics:   streamMetrics,
		Ready:     st,
		Drain:     drainer,
		Logger:    log,
		CASDir:    opts.CASDir,
		Product:   product.api,
		Partials:  agentapi.PartialSource(product.partials),
		// The selection vocabulary is injected here; a nil seam (no provider
		// configuration) makes the input edge refuse every selection.
		ValidateSelection: validateSelection,
	})

	// Auth routes live next to the API (same origin). The middleware wraps the
	// dispatcher, not the API alone, so a static request is still servable (the
	// login form must load) while /v1 requires a session.
	apiMux := http.NewServeMux()
	product.auth.register(apiMux)
	apiMux.Handle("/", api)

	static := newStaticHandler(opts.WebDir, log)
	if !static.available() {
		log.Warn("web build not found; keel serve answers 404 for non-API paths", "web_dir", opts.WebDir,
			"hint", "build the NSH web target into web/dist")
	}

	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1" || strings.HasPrefix(r.URL.Path, "/v1/") || r.URL.Path == "/metrics" {
			apiMux.ServeHTTP(w, r)
			return
		}
		static.ServeHTTP(w, r)
	})
	handler := product.auth.wrap(dispatch)

	hs := &http.Server{
		Addr:              opts.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       75 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", opts.Addr, err)
	}
	log.Info("keel serve listening", "addr", opts.Addr, "tenant", opts.TenantID.String(),
		"auth", product.auth.mode(), "web", webBuildSummary(opts.WebDir), "web_ready", static.available(),
		"worker", !opts.NoWorker, "provider", rt.ProviderName(), "model", rt.Model(),
		"plugins", len(product.pluginSnapshot().Plugins), "triggers", len(product.triggers.List()))
	go func() {
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	// Drain: stop claiming immediately, wake SSE streams, give them a bounded
	// window to close before shutting the listener down.
	drainer.Begin()
	rt.StopWorker()
	drainDeadline := time.NewTimer(3 * time.Second)
	drainTick := time.NewTicker(50 * time.Millisecond)
drainWait:
	for {
		select {
		case <-drainDeadline.C:
			log.Warn("drain window elapsed", "active_streams", streamMetrics.StreamsActive.Load())
			break drainWait
		case <-drainTick.C:
			if streamMetrics.StreamsActive.Load() == 0 {
				break drainWait
			}
		}
	}
	drainDeadline.Stop()
	drainTick.Stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), serveShutdownTimeout)
	defer cancel()
	if err := hs.Shutdown(shutdownCtx); err != nil {
		log.Warn("shutdown", "err", err)
	}
	rt.StopWorker()
	log.Info("keel serve stopped")
	return nil
}

// productLayer owns every product-layer service and its shutdown order.
type productLayer struct {
	hooks     *agentext.HookBus
	decisions *agentext.DecisionBroker
	partials  *agentext.PartialHub
	runCtrl   *agentext.RunControl
	starter   *agentext.TurnStarter
	subagents *agentext.SubagentRunner
	triggers  *agentext.TriggerEngine
	plugins   *agentext.Runtime
	api       *agentapi.Server
	auth      *authController

	closeFns []func(context.Context)
}

func (p *productLayer) close() {
	ctx, cancel := context.WithTimeout(context.Background(), serveShutdownTimeout)
	defer cancel()
	for i := len(p.closeFns) - 1; i >= 0; i-- {
		p.closeFns[i](ctx)
	}
}

func (p *productLayer) pluginSnapshot() agentext.Snapshot {
	if p.plugins == nil {
		return agentext.Snapshot{}
	}
	return p.plugins.Snapshot()
}

// newProductLayer assembles the extension surface over an already-booted
// service runtime.
func newProductLayer(ctx context.Context, st *store.Store, kern *kernel.Kernel, rt *servicehost.Runtime, hooks *agentext.HookBus, runControl *agentext.RunControl, partials *agentext.PartialHub, opts serveOptions, log *slog.Logger) (*productLayer, error) {
	sessions := storeSessions{Store: st}
	decisions := agentext.NewDecisionBroker(agentext.DecisionBrokerOptions{
		Store:  agentext.NewTasksDecisionStore(st),
		Hooks:  hooks,
		Logger: log,
	})
	starter := agentext.NewTurnStarter(sessions, kern, hooks, log)
	subagents := agentext.NewSubagentRunner(agentext.SubagentOptions{
		Gate:    sessions,
		Edge:    kern,
		Events:  kern,
		Runs:    sessions,
		Control: runControl,
		Hooks:   hooks,
		Logger:  log,
	})
	triggers := agentext.NewTriggerEngine(agentext.TriggerEngineOptions{
		Starter: starter,
		Hooks:   hooks,
		Logger:  log,
	})

	plugin := agentext.NewExamplePlugin(agentext.ExamplePluginOptions{
		TenantID: opts.TenantID.String(),
		Interval: opts.ExampleInterval,
	})
	plugins := agentext.NewRuntime(agentext.RuntimeOptions{
		Plugins:   []*agentext.Plugin{plugin, agentext.NewSubagentPlugin(subagents)},
		Hooks:     hooks,
		Triggers:  triggers,
		Decisions: decisions,
		Subagents: subagents,
		Partials:  partials,
		Tools:     rt.RefreshProductTools,
		Logger:    log,
	})
	applyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err := plugins.Apply(applyCtx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("assemble plugins: %w", err)
	}

	cancelFn := func(cancelCtx context.Context, tenantID, sessionID uuid.UUID) (agentext.CancelResult, error) {
		owner, exists, err := st.SessionOwner(cancelCtx, tenantID, sessionID)
		if err != nil {
			return agentext.CancelResult{}, err
		}
		if !exists {
			return agentext.CancelResult{}, agentapi.ErrSessionNotFound
		}
		if owner == store.StateOwnerExternal {
			return agentext.CancelResult{Status: "idle", RunStatus: "external",
				Detail: "external sessions have no native run to cancel"}, agentapi.ErrExternalSession
		}
		return runControl.Cancel(cancelCtx, kernel.Scope{TenantID: tenantID, SessionID: sessionID})
	}

	// The settings/registry projection (GET /v1/providers, GET /v1/models) comes
	// from the same assembly as the selection vocabulary; a nil catalog keeps
	// both endpoints at 501.
	var modelCatalog agentapi.ModelCatalog
	if svc := rt.Models(); svc != nil {
		modelCatalog = modelSeam{svc: svc}
	}
	api := agentapi.New(agentapi.Config{
		Sessions: agentapi.NewPGSessionLister(st),
		Cancel:   cancelFn,
		// A product write must land on a session this tenant owns: a decision
		// opened on a missing session would be an orphan row.
		SessionExists: func(ctx context.Context, tenantID, sessionID uuid.UUID) (bool, error) {
			_, exists, err := st.SessionOwner(ctx, tenantID, sessionID)
			return exists, err
		},
		Decisions: decisions,
		Partials:  partials,
		Plugins:   plugins,
		Triggers:  triggers,
		Subagents: subagents,
		Hooks:     hooks,
		Models:    modelCatalog,
		RunStatus: func(tenantID, sessionID uuid.UUID) (string, bool) {
			return runControl.Status(kernel.Scope{TenantID: tenantID, SessionID: sessionID})
		},
		Logger: log,
	})
	auth := newAuthController(authConfig{
		Password:      opts.Password,
		TenantID:      opts.TenantID,
		SecureCookies: opts.SecureCookies,
	}, log)

	layer := &productLayer{
		hooks:     hooks,
		decisions: decisions,
		partials:  partials,
		runCtrl:   runControl,
		starter:   starter,
		subagents: subagents,
		triggers:  triggers,
		plugins:   plugins,
		api:       api,
		auth:      auth,
	}
	layer.closeFns = []func(context.Context){
		func(ctx context.Context) { plugins.Close(ctx) },
		func(context.Context) { triggers.Close() },
		func(context.Context) { decisions.Close() },
		func(context.Context) { hooks.Close() },
	}
	return layer, nil
}

// storeSessions adapts the durable store to the product seams: the session gate
// (create + classify), the run-status reader and the sub-agent parent link.
type storeSessions struct {
	*store.Store
}

// modelSeam adapts the service assembly's resolved provider configuration to
// the agentapi.ModelCatalog seam. It lives in cmd/keel because that is the only
// package that knows both sides: agentapi must not depend on servicehost.
type modelSeam struct {
	svc interface {
		ProviderViews() []modelcfg.ProviderView
		ModelViews() []modelcfg.ModelView
	}
}

// ProviderViews implements agentapi.ModelCatalog.
func (m modelSeam) ProviderViews() []modelcfg.ProviderView { return m.svc.ProviderViews() }

// ModelViews implements agentapi.ModelCatalog.
func (m modelSeam) ModelViews() []modelcfg.ModelView { return m.svc.ModelViews() }

// RunState implements agentext.RunStatusReader.
func (s storeSessions) RunState(ctx context.Context, tenantID, sessionID uuid.UUID) (agentext.RunState, error) {
	run, err := s.Store.GetRun(ctx, tenantID, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNoRun) {
			return agentext.RunState{}, agentext.ErrNoRun
		}
		return agentext.RunState{}, err
	}
	return agentext.RunState{Status: run.Status, Attempts: run.Attempts, CursorSeq: run.CursorSeq, LastError: run.LastError}, nil
}

// authController owns the middleware so routes and wrapping share one policy
// (and one "auth disabled" warning at boot).
type authController struct {
	cfg    authConfig
	logger *slog.Logger
	h      *authHandler
}

func newAuthController(cfg authConfig, log *slog.Logger) *authController {
	return &authController{cfg: cfg, logger: log}
}

func (a *authController) handler() *authHandler {
	if a.h == nil {
		a.h = newAuthHandler(a.cfg, http.NotFoundHandler(), a.logger)
	}
	return a.h
}

func (a *authController) register(mux *http.ServeMux) {
	a.handler().register(mux)
}

func (a *authController) wrap(next http.Handler) http.Handler {
	h := a.handler()
	h.next = next
	return h
}

func (a *authController) mode() string {
	if a.cfg.Password == "" {
		return "disabled"
	}
	return "password"
}

func runtimeID() string {
	if id := os.Getenv("KEEL_RUNTIME_ID"); id != "" {
		return id
	}
	hostname, _ := os.Hostname()
	return fmt.Sprintf("%s-%d", hostname, os.Getpid())
}

// mcpServerEntries projects the loaded MCP servers onto the tenant document's
// server map (the one vocabulary the prompt assembly reads).
func mcpServerEntries(entries []mcp.ServerEntry) map[string]host.MCPServerConfig {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string]host.MCPServerConfig, len(entries))
	for _, entry := range entries {
		server := host.MCPServerConfig{
			Command: mcp.ServerField(entry.Config, "command"),
			URL:     mcp.ServerField(entry.Config, "url"),
		}
		if server.Command == "" && server.URL == "" {
			continue
		}
		out[entry.Name] = server
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildInfo returns the module version and VCS revision for `keel version`.
func buildInfo() (module, revision string, modified bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", "", false
	}
	revision = ""
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	return info.Main.Version, revision, modified
}
