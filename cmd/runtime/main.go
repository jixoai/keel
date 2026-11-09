// Command runtime is the Keel agent runtime server: a stateless HTTP/SSE shell
// over PostgreSQL.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/blob"
	"github.com/jixoai/keel/internal/handoff"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/httpapi"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/loadout"
	"github.com/jixoai/keel/internal/logsafe"
	"github.com/jixoai/keel/internal/mcp"
	"github.com/jixoai/keel/internal/mcp/oauth"
	keelmetrics "github.com/jixoai/keel/internal/metrics"
	"github.com/jixoai/keel/internal/quota"
	"github.com/jixoai/keel/internal/servicehost"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/stream"
)

func main() {
	var (
		addr       = flag.String("addr", ":13740", "listen address")
		dsn        = flag.String("dsn", os.Getenv("KEEL_DSN"), "PostgreSQL DSN")
		casDir     = flag.String("cas", os.Getenv("KEEL_CAS_DIR"), "content-addressed store directory (large originals)")
		budget     = flag.Int64("bandwidth", envInt64("KEEL_BANDWIDTH", 0), "outbound bytes/sec budget (0 = unlimited)")
		streamCap  = flag.Int("streams", envInt("KEEL_STREAMS", 1000), "max concurrent streams")
		batch      = flag.Int("chunk-batch", envInt("KEEL_CHUNK_BATCH", 1), "chunks per PG query (2 = 1Gbps mode)")
		admitRate  = flag.Int("admit-rate", envInt("KEEL_ADMIT_RATE", 200), "new streams per second")
		memLimit   = flag.Int64("memlimit", envInt64("GOMEMLIMIT", 0), "soft memory limit in bytes (0 = Go default)")
		runs       = flag.Int("runs", envInt("KEEL_RUNS", 64), "max concurrent agent runs")
		noWorker   = flag.Bool("no-worker", false, "do not start the agent worker")
		mcpConfig  = flag.String("mcp", os.Getenv("KEEL_MCP_CONFIG"), "global mcp.json path (empty disables MCP)")
		mcpProject = flag.String("mcp-project", os.Getenv("KEEL_MCP_PROJECT"), "project mcp.json path (trusted projects only)")

		drainRetryMS    = flag.Int("drain-retry-ms", envInt("KEEL_DRAIN_RETRY_MS", 3000), "SSE reconnect hint advertised during drain")
		drainCloseRate  = flag.Int("drain-close-rate", envInt("KEEL_DRAIN_CLOSE_RATE", 200), "streams closed per second while draining")
		drainWindow     = flag.Duration("drain-window", envDuration("KEEL_DRAIN_WINDOW", 5*time.Second), "max time to wait for streams to close on SIGTERM")
		handoffWindow   = flag.Duration("handoff-window", envDuration("KEEL_HANDOFF_WINDOW", 30*time.Second), "max time to wait for in-flight runs to drain during a handoff")
		healthcheck     = flag.Bool("healthcheck", false, "probe the local /v1/health endpoint and exit 0/1")
		healthcheckAddr = flag.String("healthcheck-addr", os.Getenv("KEEL_HEALTH_ADDR"), "address the -healthcheck probe connects to (default: -addr)")
	)
	flag.Parse()
	if *healthcheck {
		target := *healthcheckAddr
		if target == "" {
			target = *addr
		}
		os.Exit(runHealthcheck(target))
	}
	if *dsn == "" {
		slog.Error("KEEL_DSN (or -dsn) is required")
		os.Exit(2)
	}
	if *memLimit > 0 {
		debug.SetMemoryLimit(*memLimit)
	}

	// Redaction is part of the sink, not of individual log statements: values
	// under credential/body keys never reach stdout.
	log := slog.New(logsafe.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	// SIGINT keeps the old behavior (immediate shutdown). SIGTERM is handled
	// separately so the process can drain first.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	defer signal.Stop(term)

	st, err := store.Open(ctx, store.Config{DSN: *dsn})
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()
	// MCP OAuth browser steps are transient; prune dead rows so an always-on
	// process does not accumulate them.
	go runMCPOAuthPendingPruner(ctx, st, mcpOAuthPendingPruneInterval, log)

	// Contract kernel (change keel-service-graph, W1): the single writer of the
	// event log and the only issuer of run leases. It applies its additive
	// migration on the shared pool (same advisory lock as store.Migrate) and is
	// injected into the runner, the worker and the HTTP edge; none of them gets
	// a raw store write handle.
	kernelLog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		log.Error("kernel migrate", "err", err)
		os.Exit(1)
	}

	// The metric set is fixed at boot. Queue depth, active streams, pgxpool
	// state, and the Go runtime are scraped through gauge functions; everything
	// else is an atomic counter updated by the worker, the kernel and the store
	// observer.
	streamMetrics := &stream.Metrics{}
	reg := keelmetrics.NewRegistry()
	runtimeMetrics := keelmetrics.NewSet(reg)
	keelmetrics.BindGoRuntime(reg)
	keelmetrics.BindQueueDepth(reg, st)
	keelmetrics.BindStreams(reg, func() int64 { return streamMetrics.StreamsActive.Load() })
	keelmetrics.BindPoolStats(reg, func() keelmetrics.PoolStats { return st.Pool().Stat() })
	reg.SetErrorHandler(func(name string, err error) {
		log.Warn("metrics collect", "metric", name, "err", err)
	})
	// Events are committed through the kernel now, so the observer that used to
	// live on store.AppendEvent moves onto the kernel's commit hook.
	//
	// The kernel's admission seam is a real per-tenant policy: the hosting
	// default comes from the same tenant document the admin API writes (a
	// KEEL_QUOTA_* default here, the stored tenant document merged over it per
	// tenant), and every rejection is projected onto structured logs.
	bootQuota := host.QuotaConfig{
		RunConcurrency:  envInt("KEEL_QUOTA_RUNS", 0),
		EventsPerMinute: envInt("KEEL_QUOTA_EVENTS_PER_MINUTE", 0),
	}
	limits := quota.New(bootQuota).WithSource(tenantDocumentSource{store: st})
	kern := kernel.New(kernelLog, kernel.Options{
		OnCommit: func(kind kernel.EventKind) { runtimeMetrics.EventCommitted(string(kind)) },
		Limits:   limits,
		Audit:    servicehost.NewAuditSink(log, servicehost.AuditOptions{}),
	})

	// --- keel-service-graph W4: runtime identity, drain modes, handoff audit ---
	//
	// The runtime id is persistent: it names this process in runtime_instances,
	// is written into every claim as the owner and appears in the handoff
	// audit, so a drained runtime can never be mistaken for its successor. The
	// coordinator owns the drain state machine (claim gate, mode, deadline,
	// in-flight runs) that the HTTP gate and the worker consult, and it is the
	// single entry point for both drain triggers: SIGTERM (shutdown) and the
	// admin route (handoff or shutdown).
	runtimeID := os.Getenv("KEEL_RUNTIME_ID")
	if runtimeID == "" {
		hostname, _ := os.Hostname()
		runtimeID = fmt.Sprintf("%s-%d", hostname, os.Getpid())
	}
	handoffRegistry, err := handoff.NewPGRegistry(ctx, st.Pool())
	if err != nil {
		log.Error("open handoff registry", "err", err)
		os.Exit(1)
	}
	runtimeDrain := httpapi.NewModeDrainer(*drainRetryMS, *drainCloseRate)
	stopWorker := func() {} // assigned once the worker context exists
	var coord *handoff.Coordinator
	coord, err = handoff.New(handoff.Config{
		Identity: handoff.Identity{
			ID:        runtimeID,
			LoadoutID: envOr("KEEL_LOADOUT_ID", "default"),
			Revision:  envInt64("KEEL_LOADOUT_REVISION", 1),
		},
		Registry: handoffRegistry,
		Kernel:   kern,
		Logger:   log,
		OnDrain: func(mode handoff.Mode, deadline time.Duration) {
			// Wake the SSE drainer (flush committed events, hint a reconnect,
			// close at the configured rate) and stop claiming.
			runtimeDrain.BeginMode(mode, deadline)
			if mode == handoff.ModeShutdown {
				// Shutdown: in-flight runs may be abandoned; their leases
				// expire and the next process recovers the sessions.
				stopWorker()
				return
			}
			// Handoff: let in-flight runs drain to a safe point, bounded by the
			// deadline; whatever is left when it passes is abandoned and the
			// lease expires (recorded as a timeout).
			go func() {
				if err := coord.WaitDrain(context.Background()); err != nil && !errors.Is(err, handoff.ErrDrainTimeout) {
					log.Warn("handoff drain wait", "err", err)
				}
				stopWorker()
			}()
		},
	})
	if err != nil {
		log.Error("handoff coordinator", "err", err)
		os.Exit(1)
	}
	if err := coord.Register(ctx); err != nil {
		log.Error("register runtime", "err", err)
		os.Exit(1)
	}
	if err := coord.Ready(ctx); err != nil {
		log.Error("runtime ready", "err", err)
		os.Exit(1)
	}

	hub := httpapi.NewHub(st, log)
	go hub.Run(ctx)

	// MCP OAuth credentials are encrypted with KEEL_SECRET_KEY. Without it the
	// feature is disabled explicitly: nothing is stored in plaintext and the
	// sign-in endpoints answer 503.
	var secretBox *store.SecretBox
	if key, err := store.SecretKeyFromEnv(os.Getenv); err != nil {
		if !errors.Is(err, store.ErrSecretKeyMissing) {
			log.Error("mcp oauth disabled", "err", err)
		} else {
			log.Info("mcp oauth disabled", "reason", "KEEL_SECRET_KEY is not set")
		}
	} else if box, err := store.NewSecretBox(key); err != nil {
		log.Error("mcp oauth disabled", "err", err)
	} else {
		secretBox = box
	}
	oauthTenant := uuid.Nil
	if raw := os.Getenv("KEEL_MCP_OAUTH_TENANT"); raw != "" {
		if parsed, err := uuid.Parse(raw); err == nil {
			oauthTenant = parsed
		} else {
			log.Error("KEEL_MCP_OAUTH_TENANT is not a UUID", "err", err)
		}
	}
	// Credentials are tenant-scoped. Without an explicit tenant, OAuth is off:
	// silently falling back to a shared tenant would let any caller inject
	// credentials every run then uses.
	if secretBox != nil && oauthTenant == uuid.Nil {
		log.Warn("mcp oauth disabled", "reason", "KEEL_MCP_OAUTH_TENANT must name an explicit tenant UUID")
		secretBox = nil
	}
	allowLoopback := os.Getenv("KEEL_MCP_OAUTH_ALLOW_LOOPBACK") == "1"
	oauthCallbackURL := os.Getenv("KEEL_MCP_OAUTH_CALLBACK_URL")
	oauthMetadataBase := os.Getenv("KEEL_MCP_OAUTH_METADATA_BASE_URL")
	oauthAdminKey := os.Getenv("KEEL_ADMIN_KEY")
	oauthRedirectURLs := []string(nil)
	if oauthCallbackURL != "" {
		parsed, err := url.Parse(oauthCallbackURL)
		valid := err == nil && parsed.Host != "" &&
			(parsed.Scheme == "https" || (parsed.Scheme == "http" && allowLoopback && isLoopbackHostname(parsed.Hostname())))
		if !valid {
			log.Error("mcp oauth sign-in endpoints disabled", "reason", "KEEL_MCP_OAUTH_CALLBACK_URL must be an https URL (http only for an explicit loopback helper)")
			oauthCallbackURL = ""
		} else {
			oauthRedirectURLs = append(oauthRedirectURLs, oauthCallbackURL)
		}
	}
	if secretBox != nil && oauthCallbackURL == "" {
		log.Warn("mcp oauth sign-in endpoints disabled", "reason", "KEEL_MCP_OAUTH_CALLBACK_URL is required")
	}
	if secretBox != nil && oauthAdminKey == "" {
		log.Warn("mcp oauth sign-in endpoints disabled", "reason", "KEEL_ADMIN_KEY is required")
	}
	oauthHTTPClient := oauth.NewHTTPClient(oauth.HTTPClientOptions{AllowLoopback: allowLoopback})
	mcpOAuthStorage := func(tenantID uuid.UUID, server string) oauth.ServerStore {
		return oauth.NewPostgresStore(st, secretBox, tenantID, server)
	}

	// Host layer: tenant prompt, skills (progressive disclosure), and tools. The
	// runtime stays stateless: the host is assembled at boot from the tenant
	// document (env defaults here; the admin API writes the same document), and
	// the service graph below owns the assembly lifecycle.
	sections, err := hostSections()
	if err != nil {
		log.Error("parse KEEL_SYSTEM_SECTIONS", "err", err)
		os.Exit(2)
	}
	// MCP servers: configuration is the shared mcpServers shape; the loaded
	// entries become the mcp service's connection input, and the tenant document
	// records the server names it enables (single quota/loadout vocabulary).
	var mcpEntries []mcp.ServerEntry
	mcpOptions := servicehost.MCPOptions{}
	if *mcpConfig != "" {
		loadOptions := mcp.McpServerConfigOptions{}
		if oauthMetadataBase != "" {
			documents := oauth.DocumentsFromSettings(oauth.Settings{
				CallbackURL:          oauthCallbackURL,
				MetadataBaseURL:      oauthMetadataBase,
				MetadataRedirectURLs: oauthRedirectURLs,
			})
			loadOptions.CheckMetadataDocumentCallback = func(callbackURL string) string {
				if callbackURL != "" && documents != nil && documents.ListsRedirectURL != nil && !documents.ListsRedirectURL(callbackURL) {
					return "oauth.callbackUrl is not listed by the configured Client ID Metadata Documents"
				}
				return ""
			}
		}
		loaded := mcp.LoadConfigFilesWithOptions(*mcpConfig, *mcpProject, loadOptions)
		mcpEntries = loaded.Servers
		for _, message := range loaded.Errors {
			log.Warn("mcp config", "err", message)
		}
		connectOptions := mcp.ConnectOptions{
			ClientName:    "keel",
			ClientVersion: "1.0.0",
			Logger:        log,
			TenantID:      oauthTenant,
		}
		if secretBox != nil {
			connectOptions.OAuth = &mcp.OAuthOptions{
				Storage:              mcpOAuthStorage,
				ClientName:           "keel",
				MetadataBaseURL:      oauthMetadataBase,
				MetadataRedirectURLs: oauthRedirectURLs,
				RedirectURL:          oauthCallbackURL,
				HTTPClient:           oauthHTTPClient,
				// A 401 challenge is the only sign that a browser sign-in is
				// needed; log it so the operator can act without a debugger.
				OnChallenge: logMCPChallenge(log),
			}
		}
		mcpOptions = servicehost.MCPOptions{Entries: mcpEntries, ConnectOptions: connectOptions}
	}

	// External provider declarations are part of the same tenant document. The
	// boot default comes from KEEL_EXTERNAL_CONFIG (a tenant-document fragment,
	// parsed with the one host vocabulary); an admin-written tenant document
	// carries the same field for the next boot. Nothing here is a second config
	// format.
	externalCfg, err := externalConfigFromFile(os.Getenv("KEEL_EXTERNAL_CONFIG"))
	if err != nil {
		log.Error("load KEEL_EXTERNAL_CONFIG", "err", err)
		os.Exit(2)
	}
	// The provider configuration is the same tenant document's models slice, so
	// its boot default comes from KEEL_MODELS_CONFIG (the file IS the models
	// section: providers / rules / selection). The bytes travel verbatim; the
	// resolver owns the vocabulary and fails boot on a document it cannot use.
	modelsDoc, err := host.LoadModelsDocument(os.Getenv("KEEL_MODELS_CONFIG"))
	if err != nil {
		log.Error("load KEEL_MODELS_CONFIG", "err", err)
		os.Exit(2)
	}
	// The runtime's default tenant document. Everything a loadout needs comes
	// from this one struct (host.TenantConfig): model selection, prompt, skills,
	// MCP servers, telemetry, quota and external providers. There is no second
	// configuration vocabulary between here and the service graph.
	tenantCfg := host.TenantConfig{
		Agent: host.AgentConfig{
			Provider:     os.Getenv("KEEL_PROVIDER"),
			Model:        os.Getenv("KEEL_MODEL"),
			BaseURL:      os.Getenv("KEEL_BASE_URL"),
			Protocol:     os.Getenv("KEEL_PROTOCOL"),
			SystemPrompt: envOr("KEEL_SYSTEM_PROMPT", defaultSystemPrompt),
			Sections:     promptSections(sections),
		},
		Models: modelsDoc,
		Skills: host.SkillsConfig{
			Roots:        skillRoots(),
			FileReadTool: os.Getenv("KEEL_FILE_READ_TOOL"),
		},
		MCP:       host.MCPConfig{Servers: mcpServersFromEntries(mcpEntries)},
		Telemetry: host.TelemetryConfig{Adapter: telemetryAdapter(os.Getenv("KEEL_TELEMETRY"))},
		Quota:     bootQuota,
		External:  externalCfg,
	}
	limits.Update(tenantCfg.Quota)

	// Assemble the default loadout through the service graph. Every service gets
	// narrow seams only (kernel Committer/Reader/LeaseManager/ExternalSessions,
	// the read-only run mirror, its own interfaces); the raw store and kernel
	// stay in this file.
	rt, err := servicehost.Boot(ctx, servicehost.Config{
		Seams: servicehost.Seams{
			Committer: kern,
			Reader:    kern,
			Leases:    kern,
			External:  kern,
			Runs:      runMirror{store: st},
			Limits:    limits,
			Logger:    log,
		},
		Tenant: tenantCfg,
		Options: loadout.Options{
			ID:             envOr("KEEL_LOADOUT_ID", loadout.DefaultID),
			Revision:       envInt64("KEEL_LOADOUT_REVISION", 1),
			Worker:         !*noWorker,
			RunConcurrency: *runs,
		},
		RuntimeID:   runtimeID,
		Concurrency: *runs,
		Metrics:     runtimeMetrics,
		Prompt:      servicehost.PromptOptions{Preamble: envOr("KEEL_SYSTEM_PROMPT", defaultSystemPrompt)},
		MCP:         mcpOptions,
		LLM:         servicehost.LLMOptions{APIKey: os.Getenv("KEEL_API_KEY"), LookupEnv: os.LookupEnv},
		External: servicehost.ExternalOptions{
			LookupEnv: os.LookupEnv,
			Environ:   os.Environ(),
			Stderr:    os.Stderr,
		},
		DrainGate: coord,
	})
	if err != nil {
		log.Error("assemble service loadout", "err", err)
		os.Exit(1)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := rt.Close(closeCtx); err != nil {
			log.Warn("close service loadout", "err", err)
		}
	}()
	if !*noWorker {
		log.Info("agent worker started", "provider", rt.ProviderName(), "model", rt.Model(),
			"concurrency", *runs, "runtime_id", runtimeID,
			"loadout", rt.Snapshot().LoadoutID, "revision", rt.Snapshot().Revision,
			"generation", rt.Snapshot().Generation)
	}
	// Drain now goes through the handoff coordinator: the worker's gate closes
	// claims and the mode drainer wakes SSE streams. stopWorker is the service
	// graph's worker stop (cancel claims; in-flight attempts keep settling).
	stopWorker = rt.StopWorker

	var mcpOAuthConfig *httpapi.MCPOAuth
	if secretBox != nil && len(mcpEntries) > 0 && oauthCallbackURL != "" && oauthAdminKey != "" {
		entries := make(map[string]mcp.ServerEntry, len(mcpEntries))
		for _, entry := range mcpEntries {
			entries[entry.Name] = entry
		}
		documents := oauth.DocumentsFromSettings(oauth.Settings{
			CallbackURL:          oauthCallbackURL,
			MetadataBaseURL:      oauthMetadataBase,
			MetadataRedirectURLs: oauthRedirectURLs,
		})
		allowedHosts := []string(nil)
		for _, host := range strings.Split(os.Getenv("KEEL_MCP_OAUTH_ALLOWED_HOSTS"), ",") {
			if trimmed := strings.TrimSpace(host); trimmed != "" {
				allowedHosts = append(allowedHosts, trimmed)
			}
		}
		mcpOAuthConfig = &httpapi.MCPOAuth{
			Servers: func(tenantID uuid.UUID, name string) (httpapi.MCPOAuthServer, error) {
				entry, ok := entries[name]
				if !ok {
					return httpapi.MCPOAuthServer{}, fmt.Errorf("unknown MCP server %q", name)
				}
				serverURL := mcp.ServerField(entry.Config, "url")
				if serverURL == "" || entry.Config["oauth"] == nil {
					return httpapi.MCPOAuthServer{}, fmt.Errorf("MCP server %q has no OAuth configuration", name)
				}
				return httpapi.MCPOAuthServer{
					URL:      serverURL,
					Settings: mcp.OAuthSettingsFromConfig(entry.Config, oauthMetadataBase, oauthRedirectURLs),
				}, nil
			},
			NewStore: func(tenantID uuid.UUID, name string) *oauth.PostgresStore {
				return oauth.NewPostgresStore(st, secretBox, tenantID, name)
			},
			HTTPClient:              oauthHTTPClient,
			CallbackURL:             oauthCallbackURL,
			ClientMetadataDocuments: documents,
			ClientName:              "keel",
			AllowLoopbackRedirect:   allowLoopback,
			AllowedHosts:            allowedHosts,
			Reconnect: func(ctx context.Context, tenantID uuid.UUID, name string) error {
				if tenantID != oauthTenant || len(rt.MCPEntries()) == 0 {
					return fmt.Errorf("no MCP manager for tenant %s", tenantID)
				}
				// Reconnect the server and republish its tools/section on the
				// live host so sign-in takes effect without a restart;
				// in-flight runs keep their previous registry.
				return rt.ReconnectMCP(ctx, name)
			},
		}
	}
	// A nil *externalgw.AgentService must stay a nil interface: the HTTP edge
	// answers 503 on the external routes when no backend is declared, and a
	// typed-nil interface would panic instead.
	var externalSessions httpapi.ExternalSessions
	if agents := rt.ExternalAgents(); agents != nil {
		externalSessions = agents
		for _, backend := range agents.Backends() {
			log.Info("external agent backend", "name", backend.Name, "state", backend.State, "health", backend.Health)
		}
	}
	// A nil validator must stay a nil interface: the edge accepts a selection
	// exactly when the tenant declares a provider configuration, and an unwired
	// validator refuses every selection instead of storing one nothing honours.
	var validateSelection func(json.RawMessage) error
	if models := rt.Models(); models != nil {
		validateSelection = models.ValidateSelection
	}
	srv := httpapi.New(httpapi.Config{
		Store:             st,
		Hub:               hub,
		Edge:              kern,
		Events:            rt.Projection(),
		Loadouts:          kern,
		ValidateSelection: validateSelection,
		Blobs:             blob.NewReader(st, *streamCap, *batch),
		Uploader:          &blob.Uploader{Store: st, CASDir: *casDir, TempDir: os.TempDir()},
		Scheduler:         stream.NewScheduler(*streamCap),
		Admitter:          stream.NewAdmitter(*admitRate, *admitRate*2),
		Limiter:           stream.NewLimiter(*budget),
		Metrics:           streamMetrics,
		MetricsExporter:   reg,
		Ready:             st,
		Drain:             runtimeDrain,
		Control:           runtimeControl{Coordinator: coord, window: *handoffWindow},
		External:          externalSessions,
		Logger:            log,
		CASDir:            *casDir,
		MCPOAuth:          mcpOAuthConfig,
	})

	hs := &http.Server{
		Addr:              *addr,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
		// SSE and file streams must not be cut by a global WriteTimeout; every
		// individual write carries its own deadline instead.
		ReadTimeout:    0,
		WriteTimeout:   0,
		IdleTimeout:    75 * time.Second,
		MaxHeaderBytes: 16 << 10,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			setSendBuffer(c, 64<<10)
			return ctx
		},
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
	log.Info("runtime listening", "addr", *addr, "streams", *streamCap, "batch", *batch,
		"bandwidth", *budget, "pid", os.Getpid())

	go func() {
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err)
			stop()
		}
	}()

	// SIGTERM drains before shutting down: stop admitting work, wake every SSE
	// stream with a reconnect hint, and wait (bounded) for rate-limited closes.
	// SIGINT cancels ctx and keeps the immediate-shutdown behavior.
	go func() {
		select {
		case <-term:
			log.Info("SIGTERM received, draining", "mode", string(handoff.ModeShutdown))
			runtimeMetrics.SetDraining(true)
			if _, err := coord.BeginDrain(context.Background(), handoff.ModeShutdown, *drainWindow); err != nil && !errors.Is(err, handoff.ErrAlreadyDraining) {
				log.Warn("shutdown drain", "err", err)
			}
			deadline := time.NewTimer(*drainWindow)
			tick := time.NewTicker(50 * time.Millisecond)
			defer deadline.Stop()
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-deadline.C:
					log.Warn("drain window elapsed", "active_streams", streamMetrics.StreamsActive.Load())
					stop()
					return
				case <-tick.C:
					if streamMetrics.StreamsActive.Load() == 0 {
						log.Info("drain complete")
						stop()
						return
					}
				}
			}
		case <-ctx.Done():
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hs.Shutdown(shutdownCtx); err != nil {
		log.Warn("shutdown", "err", err)
	}
	log.Info("stopped")
	_ = coord.Stop(context.Background(), "process exit")
}

func runHealthcheck(addr string) int {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: bad addr %q: %v\n", addr, err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Get("http://" + net.JoinHostPort(host, port) + "/v1/health")
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: status %d\n", res.StatusCode)
		return 1
	}
	return 0
}

func isLoopbackHostname(host string) bool {
	switch strings.Trim(host, "[]") {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// setSendBuffer caps the kernel send buffer. Left at the OS autotuned default
// (up to 4MB/connection on Linux), 1000 streams would hold gigabytes of kernel
// memory that GOMEMLIMIT cannot see.
func setSendBuffer(c net.Conn, size int) {
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	_ = tcp.SetWriteBuffer(size)
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envInt64(name string, def int64) int64 {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envDuration(name string, def time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

const defaultSystemPrompt = "You are an AI agent running on the Keel runtime. Use the available tools when they help."

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// skillRoots reads the PI-style skill search path (OS path list).
func skillRoots() []string {
	raw := os.Getenv("KEEL_SKILLS_ROOTS")
	if raw == "" {
		return nil
	}
	return filepath.SplitList(raw)
}

// hostSections parses additional prompt sections as a JSON array of
// {"name": "...", "content": "..."} objects.
func hostSections() ([]host.Section, error) {
	raw := os.Getenv("KEEL_SYSTEM_SECTIONS")
	if raw == "" {
		return nil, nil
	}
	var sections []host.Section
	if err := json.Unmarshal([]byte(raw), &sections); err != nil {
		return nil, err
	}
	return sections, nil
}

// runtimeControl adapts the handoff coordinator to the HTTP control surface and
// applies the process's default handoff window when a drain request carries no
// explicit deadline.
type runtimeControl struct {
	*handoff.Coordinator
	window time.Duration
}

var _ httpapi.RuntimeControl = runtimeControl{}

func (r runtimeControl) BeginDrain(ctx context.Context, mode handoff.Mode, deadline time.Duration) (handoff.Status, error) {
	if deadline <= 0 {
		deadline = r.window
	}
	return r.Coordinator.BeginDrain(ctx, mode, deadline)
}
