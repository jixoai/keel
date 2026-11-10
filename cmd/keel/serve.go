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
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/blob"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/httpapi"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/loadout"
	"github.com/jixoai/keel/internal/mcp"
	"github.com/jixoai/keel/internal/modelcfg"
	"github.com/jixoai/keel/internal/pluginpkg"
	"github.com/jixoai/keel/internal/pluginpkg/builtin"
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
	ModelsConfig string
	// PluginsRoot is the managed plugin directory tree
	// ($KEEL_HOME/plugins/<id>/plugin.yaml). An empty root disables install and
	// discovery; bundled plugins still load.
	PluginsRoot     string
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

	// The SSE hub is started once the runtime is up (below): on a boot failure
	// it would hold a pool connection and park store.Close, turning a reported
	// error into a silent hang.
	hub := httpapi.NewHub(st, log)

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
	// Tenant secret storage (settings > 密钥) is AES-256-GCM through the same
	// SecretBox the MCP OAuth path uses. Without KEEL_SECRET_KEY the feature is
	// disabled explicitly: the settings page reports it, writes are refused, and
	// a `secret:` model reference fails closed — nothing is ever stored in
	// plaintext.
	var secretBox *store.SecretBox
	secretReason := ""
	if key, err := store.SecretKeyFromEnv(os.Getenv); err != nil {
		if errors.Is(err, store.ErrSecretKeyMissing) {
			secretReason = "tenant secret storage is not enabled: KEEL_SECRET_KEY is not configured"
			log.Info("tenant secret storage disabled", "reason", "KEEL_SECRET_KEY is not set")
		} else {
			secretReason = "tenant secret storage is not enabled: " + err.Error()
			log.Error("tenant secret storage disabled", "err", err)
		}
	} else if box, err := store.NewSecretBox(key); err != nil {
		secretReason = "tenant secret storage is not enabled: " + err.Error()
		log.Error("tenant secret storage disabled", "err", err)
	} else {
		secretBox = box
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
	// Boot precedence: a stored override (the settings write path's row) wins
	// over KEEL_MODELS_CONFIG; the deployment document stays the fallback a
	// reset returns to — and the fallback boot takes when the override cannot
	// be served at all (deleted secret, unset environment variable): a settings
	// row must not be able to brick the next boot.
	var overrideRevision int64
	if opts.TenantID != uuid.Nil {
		document, revision, _, found, err := st.GetModelSettings(ctx, opts.TenantID)
		if err != nil {
			return fmt.Errorf("load stored model settings: %w", err)
		}
		if found {
			log.Info("stored model settings override in effect", "revision", revision, "bytes", len(document))
			tenantCfg.Models = document
			overrideRevision = revision
		}
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
		// The deployment document stays the boot fallback: a stored override
		// whose credentials no longer resolve falls back to it instead of
		// failing the deployment (the rejection is surfaced on the settings
		// read).
		DeploymentModels: modelsDoc,
		OverrideRevision: overrideRevision,
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
		LLM: servicehost.LLMOptions{
			APIKey:       opts.APIKey,
			LookupEnv:    os.LookupEnv,
			LookupSecret: tenantSecretLookup(st, secretBox, opts.TenantID, log),
		},
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
	// The runtime is up: the SSE hub may hold its pool connection now.
	go hub.Run(ctx)
	if rejected, ok := rt.RejectedModelsOverride(); ok {
		log.Warn("the stored model settings override was rejected at boot; serving the deployment document",
			"revision", rejected.Revision, "reason", rejected.Reason)
	}

	// --- product layer (keel-agent) ---
	// The web build is resolved before the product layer so the layer can hand
	// the host theme tokens (web/theme.json) to plugin UIs through the bridge.
	static := newStaticHandler(opts.WebDir, log)
	if !static.available() {
		log.Warn("web build not found; keel serve answers 404 for non-API paths", "web_dir", opts.WebDir,
			"hint", "build the NSH web target into web/dist")
	}
	product, err := newProductLayer(ctx, st, kern, rt, hooks, runControl, partials, opts, secretBox, secretReason,
		loadHostTheme(static.root, log), log)
	if err != nil {
		return err
	}
	defer product.close()

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
		// The selection vocabulary is read from the CURRENT generation at call
		// time: a settings write installs a new model service, and the input edge
		// must accept exactly what that generation can run. A runtime without
		// provider configuration still refuses every selection.
		ValidateSelection: selectFromCurrentGeneration(rt),
	})

	// Auth routes live next to the API (same origin). The middleware wraps the
	// dispatcher, not the API alone, so a static request is still servable (the
	// login form must load) while /v1 requires a session.
	apiMux := http.NewServeMux()
	product.auth.register(apiMux)
	apiMux.Handle("/", api)

	// Plugin UIs are hosted on the same origin (the bridge and the static app
	// share it), and they are not part of the /v1 API surface: the dispatcher
	// routes /plugins/<id>/ to the plugin UI handler before the static app, which
	// serves the same path from the web build only if no plugin claims it.
	pluginUI := newPluginUIHandler(product.pluginManager, log)

	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1" || strings.HasPrefix(r.URL.Path, "/v1/") || r.URL.Path == "/metrics":
			apiMux.ServeHTTP(w, r)
		case r.URL.Path == "/plugins" || strings.HasPrefix(r.URL.Path, "/plugins/"):
			pluginUI.ServeHTTP(w, r)
		default:
			static.ServeHTTP(w, r)
		}
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
	inventory := product.pluginManager.Snapshot()
	log.Info("keel serve listening", "addr", opts.Addr, "tenant", opts.TenantID.String(),
		"auth", product.auth.mode(), "web", webBuildSummary(opts.WebDir), "web_ready", static.available(),
		"worker", !opts.NoWorker, "provider", rt.ProviderName(), "model", rt.Model(),
		"plugins", len(inventory.Plugins), "plugins_enabled", len(inventory.Enabled),
		"plugins_root", opts.PluginsRoot, "triggers", len(product.triggers.List()))
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
	// pluginManager is the lifecycle authority over plugins; api serves it and
	// the runtime is only ever told what to load through it.
	pluginManager *agentext.Manager
	api           *agentapi.Server
	auth          *authController

	closeFns []func(context.Context)
}

func (p *productLayer) close() {
	ctx, cancel := context.WithTimeout(context.Background(), serveShutdownTimeout)
	defer cancel()
	for i := len(p.closeFns) - 1; i >= 0; i-- {
		p.closeFns[i](ctx)
	}
}

// newProductLayer assembles the extension surface over an already-booted
// service runtime. secretBox (nil when KEEL_SECRET_KEY is unset) enables the
// settings secret store; secretReason explains its absence to an operator;
// theme is the host theme token document handed to plugin UIs through the
// bridge (nil when the web build has none).
func newProductLayer(ctx context.Context, st *store.Store, kern *kernel.Kernel, rt *servicehost.Runtime, hooks *agentext.HookBus, runControl *agentext.RunControl, partials *agentext.PartialHub, opts serveOptions, secretBox *store.SecretBox, secretReason string, theme json.RawMessage, log *slog.Logger) (*productLayer, error) {
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

	// Bundled plugins are ordinary plugin packages whose manifest lives in the
	// binary; the manager drives their enablement like any other plugin, so
	// `keel.example` can be disabled from the API and stays disabled.
	builtinManifests, err := builtin.Manifests()
	if err != nil {
		return nil, fmt.Errorf("builtin plugin manifests: %w", err)
	}
	// Creation mode drives the plugin manager itself, so its factory resolves the
	// manager lazily: the manager is built with (and after) this builtin set.
	var pluginManager *agentext.Manager
	builtinFactories := map[string]func() *agentext.Plugin{
		agentext.ExamplePluginID: func() *agentext.Plugin {
			return agentext.NewExamplePlugin(agentext.ExamplePluginOptions{
				TenantID: opts.TenantID.String(),
				Interval: opts.ExampleInterval,
			})
		},
		"keel.subagents": func() *agentext.Plugin { return agentext.NewSubagentPlugin(subagents) },
		agentext.CreationPluginID: func() *agentext.Plugin {
			return agentext.NewCreationPlugin(agentext.CreationOptions{
				Manager:  func() *agentext.Manager { return pluginManager },
				TenantID: opts.TenantID,
			})
		},
	}
	builtins := make([]agentext.Builtin, 0, len(builtinManifests))
	for _, manifest := range builtinManifests {
		factory, ok := builtinFactories[manifest.ID]
		if !ok {
			return nil, fmt.Errorf("no built-in plugin is registered for the manifest %s", manifest.ID)
		}
		builtins = append(builtins, agentext.Builtin{Manifest: manifest, Factory: factory, DefaultEnabled: true})
	}
	// The tool index mirrors the dynamic tool set the plugin runtime publishes:
	// the plugin UI bridge calls a plugin's own tools through it, so a bridge call
	// executes exactly what the model would execute.
	toolIndex := newProductToolIndex()
	plugins := agentext.NewRuntime(agentext.RuntimeOptions{
		Hooks:     hooks,
		Triggers:  triggers,
		Decisions: decisions,
		Subagents: subagents,
		Partials:  partials,
		Tools: func(tools []agentcore.AgentTool) error {
			toolIndex.publish(tools)
			return rt.RefreshProductTools(tools)
		},
		Logger: log,
	})
	manager, err := agentext.NewPluginManager(agentext.ManagerOptions{
		Root:        opts.PluginsRoot,
		Runtime:     plugins,
		Persistence: st,
		Builtins:    builtins,
		// The sidecar loader is the one loading strategy this build ships for
		// non-compiled kinds; the manager installs its denial sink so a sidecar's
		// permission refusals land in the audit trail (spec P3).
		Loaders: map[pluginpkg.Kind]agentext.PluginLoader{
			pluginpkg.KindSidecar: agentext.NewSidecarLoader(agentext.SidecarOptions{
				KeelVersion: version,
				Logger:      log,
			}),
		},
		KeelVersion: version,
		Logger:      log,
	})
	if err != nil {
		return nil, fmt.Errorf("plugin manager: %w", err)
	}
	pluginManager = manager
	// The plugin inventory is reconciled against what is on disk and assembled
	// from persisted intent: a plugin an operator disabled stays disabled.
	applyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err = manager.Boot(applyCtx)
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
	// from the same assembly as the selection vocabulary. It is read per request
	// off the live runtime (ModelsLive): a settings write installs a new model
	// service without a restart, and a generation without provider
	// configuration answers 501 exactly like a boot without a models document.
	modelSeam := modelRuntimeSeam{rt: rt}
	var secretStore agentapi.SecretStore
	if secretBox != nil {
		secretStore = tenantSecretSeam{store: st, box: secretBox}
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
		Decisions:     decisions,
		Partials:      partials,
		Plugins:       plugins,
		PluginManager: manager,
		// The approval refusal of a lifecycle write never reaches the manager, so
		// the product API writes its audit row through the store directly; the
		// bridge's tool calls execute over the same registry the model uses, and
		// plugin UIs receive the host theme tokens with their state.
		PluginAuditWriter: pluginAuditStore{store: st},
		PluginTools:       &pluginToolBridge{index: toolIndex, manager: manager},
		Theme:             theme,
		Triggers:          triggers,
		Subagents:         subagents,
		Hooks:             hooks,
		ModelsLive: func() agentapi.ModelCatalog {
			if rt.Models() == nil {
				return nil
			}
			return modelSeam
		},
		ModelSettingsStore:   st,
		ModelSettingsRuntime: modelSeam,
		SecretStore:          secretStore,
		SecretStoreReason:    secretReason,
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
		hooks:         hooks,
		decisions:     decisions,
		partials:      partials,
		runCtrl:       runControl,
		starter:       starter,
		subagents:     subagents,
		triggers:      triggers,
		plugins:       plugins,
		pluginManager: manager,
		api:           api,
		auth:          auth,
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

// modelRuntimeSeam adapts the live service runtime to the product API's model
// seams. Both sides read the CURRENT generation per call (never a boot capture):
// the registry projections the picker consumes, and the settings write path's
// validate/apply pair. It lives in cmd/keel because that is the only package
// that knows both sides: agentapi must not depend on servicehost.
type modelRuntimeSeam struct {
	rt *servicehost.Runtime
}

// ProviderViews implements agentapi.ModelCatalog.
func (m modelRuntimeSeam) ProviderViews() []modelcfg.ProviderView {
	if service := m.rt.Models(); service != nil {
		return service.ProviderViews()
	}
	return nil
}

// ModelViews implements agentapi.ModelCatalog.
func (m modelRuntimeSeam) ModelViews() []modelcfg.ModelView {
	if service := m.rt.Models(); service != nil {
		return service.ModelViews()
	}
	return nil
}

// ResolutionIssues implements agentapi.ModelCatalog.
func (m modelRuntimeSeam) ResolutionIssues() []modelcfg.Issue {
	if service := m.rt.Models(); service != nil {
		return service.ResolutionIssues()
	}
	return nil
}

// DefaultSelection implements agentapi.ModelCatalog.
func (m modelRuntimeSeam) DefaultSelection() modelcfg.Selection {
	if service := m.rt.Models(); service != nil {
		return service.DefaultSelection()
	}
	return modelcfg.Selection{}
}

// DeploymentModels implements agentapi.ModelSettingsRuntime.
func (m modelRuntimeSeam) DeploymentModels() json.RawMessage { return m.rt.DeploymentModels() }

// RejectedModelsOverride implements agentapi.ModelSettingsRuntime.
func (m modelRuntimeSeam) RejectedModelsOverride() (int64, string, bool) {
	rejected, ok := m.rt.RejectedModelsOverride()
	if !ok {
		return 0, "", false
	}
	return rejected.Revision, rejected.Reason, true
}

// ValidateModelsDocument implements agentapi.ModelSettingsRuntime.
func (m modelRuntimeSeam) ValidateModelsDocument(document json.RawMessage) (agentapi.ModelCandidate, []modelcfg.Issue, error) {
	candidate, issues, err := m.rt.ValidateModelsDocument(document)
	if err != nil || len(issues) > 0 {
		return agentapi.ModelCandidate{}, issues, err
	}
	return agentapi.ModelCandidate{
		Selection:           candidate.Selection,
		ProvidersExecutable: candidate.ProvidersExecutable,
		ModelsSelectable:    candidate.ModelsSelectable,
	}, nil, nil
}

// ApplyModels implements agentapi.ModelSettingsRuntime.
func (m modelRuntimeSeam) ApplyModels(ctx context.Context, document json.RawMessage) error {
	_, err := m.rt.ApplyModels(ctx, document)
	return err
}

// selectFromCurrentGeneration is the input edge's selection validator: it
// validates against the model service the runtime is serving right now, so a
// settings write changes what the next input may ask for without a restart. A
// runtime without provider configuration refuses every selection, exactly like
// a boot without a models document always did.
func selectFromCurrentGeneration(rt *servicehost.Runtime) func(json.RawMessage) error {
	return func(selection json.RawMessage) error {
		service := rt.Models()
		if service == nil {
			return errors.New("selection is not supported by this server")
		}
		return service.ValidateSelection(selection)
	}
}

// tenantSecretSeam adapts the store's encrypted tenant secrets to
// agentapi.SecretStore. The value crosses this boundary exactly once, on the
// way in; no method here returns one to a caller.
type tenantSecretSeam struct {
	store *store.Store
	box   *store.SecretBox
}

func (s tenantSecretSeam) List(ctx context.Context, tenantID uuid.UUID) ([]agentapi.SecretMeta, error) {
	metas, err := s.store.ListTenantSecrets(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]agentapi.SecretMeta, 0, len(metas))
	for _, meta := range metas {
		out = append(out, agentapi.SecretMeta{Name: meta.Name, CreatedAt: meta.CreatedAt, UpdatedAt: meta.UpdatedAt})
	}
	return out, nil
}

func (s tenantSecretSeam) Put(ctx context.Context, tenantID uuid.UUID, name string, value []byte) (time.Time, error) {
	return s.store.PutTenantSecret(ctx, tenantID, name, value, s.box)
}

func (s tenantSecretSeam) Delete(ctx context.Context, tenantID uuid.UUID, name string) (bool, error) {
	return s.store.DeleteTenantSecret(ctx, tenantID, name)
}

// tenantSecretLookup resolves `secret:NAME` model credentials from the boot
// tenant's secret store. It is nil when this deployment has no secret storage
// (or no configured tenant): a secret: reference then fails closed in the
// resolver, naming the secret. The read gets its own bounded context because
// the resolution pipeline is also called from ctx-free projections (the
// settings views) — it must never hand an unbounded store query to them. Only
// the name and the failure are ever logged.
func tenantSecretLookup(st *store.Store, box *store.SecretBox, tenantID uuid.UUID, log *slog.Logger) func(string) (string, bool) {
	if st == nil || box == nil || tenantID == uuid.Nil {
		return nil
	}
	return func(name string) (string, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		value, found, err := st.GetTenantSecret(ctx, tenantID, name, box)
		if err != nil {
			log.Warn("model settings: read tenant secret failed", "secret", name, "err", err)
			return "", false
		}
		if !found || len(value) == 0 {
			return "", false
		}
		return string(value), true
	}
}

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
