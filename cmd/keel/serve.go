package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/agentapi"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/approval"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/bashbridge"
	"github.com/jixoai/keel/internal/blob"
	"github.com/jixoai/keel/internal/fileplugin"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/httpapi"
	"github.com/jixoai/keel/internal/jobqueue"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/layerfs"
	"github.com/jixoai/keel/internal/loadout"
	"github.com/jixoai/keel/internal/mcp"
	"github.com/jixoai/keel/internal/mcphost"
	"github.com/jixoai/keel/internal/mediaadapter"
	"github.com/jixoai/keel/internal/mediawiring"
	keelmetrics "github.com/jixoai/keel/internal/metrics"
	"github.com/jixoai/keel/internal/modelcfg"
	"github.com/jixoai/keel/internal/notify"
	"github.com/jixoai/keel/internal/placement"
	"github.com/jixoai/keel/internal/pluginpkg"
	"github.com/jixoai/keel/internal/pluginpkg/builtin"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/quota"
	"github.com/jixoai/keel/internal/servicehost"
	"github.com/jixoai/keel/internal/sessionbinding"
	"github.com/jixoai/keel/internal/skilllib"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/stream"
	"github.com/jixoai/keel/internal/tasklog"
	"github.com/jixoai/keel/internal/telemetry"
	"github.com/jixoai/keel/internal/testkit"
	"github.com/jixoai/keel/internal/toolsurface"
)

// serveOptions is the resolved `keel serve` configuration (flags + environment).
type serveOptions struct {
	Addr          string
	DSN           string
	WebDir        string
	CASDir        string
	TenantID      uuid.UUID
	Password      string
	SecureCookies bool
	Runs          int
	NoWorker      bool
	Model         string
	Provider      string
	BaseURL       string
	Protocol      string
	APIKey        string
	SystemPrompt  string
	SkillsRoots   []string
	// SkillsRoot is the managed skill library root ($KEEL_HOME/skills). The
	// tenant's active/ directory below it is part of skill discovery, so an
	// installed skill reaches the next run without restarting the process.
	SkillsRoot string
	// SkillsSourceRoot is the only directory a skill subscription may point at
	// (default <SkillsRoot>/sources).
	SkillsSourceRoot string
	// Skills is the built library (set by runServe before the product layer).
	Skills          *skilllib.Library
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
	// Bash is the keel.shell job manager (timeout-to-background jobs). It is
	// built before the service graph so restart recovery runs before the first
	// run can start.
	Bash *jobqueue.Manager
	// FileService is the keel.file plugin's backend. It is built before the
	// service graph so the constant read/write/edit tools have a backend at
	// assembly time; nil keeps them present and answering not_wired.
	FileService *fileplugin.Service
}

const (
	defaultServeAddr     = "127.0.0.1:13750"
	defaultSystemPrompt  = "You are an AI employee running on the Keel Harness. Use the available tools when they help."
	defaultServeWebDir   = "web/dist"
	serveShutdownTimeout = 10 * time.Second
)

// bashSessionBackend stamps the run identity on the bash tool's context before
// the keel.shell plugin reaches the bridge. The task gate has already resolved
// the attribution card and stamped it into the arguments; this wrapper forwards
// that same fact (plus the tool-call id) so the workspace provider keys the
// session workspace and the run's fs.capture is attributed without inventing a
// second attribution path.
type bashSessionBackend struct {
	inner toolsurface.BashBackend
}

func (b bashSessionBackend) Invoke(ctx context.Context, action string, args json.RawMessage) (agentcore.AgentToolResult, error) {
	if scope, ok := agentext.RunScopeFromContext(ctx); ok && scope.TenantID != uuid.Nil && scope.SessionID != uuid.Nil {
		var fields struct {
			TaskID string `json:"taskId"`
		}
		_ = json.Unmarshal(args, &fields)
		ctx = bashbridge.WithSessionIdentity(ctx, bashbridge.SessionIdentity{
			TenantID:  scope.TenantID,
			SessionID: scope.SessionID,
			TaskID:    strings.TrimSpace(fields.TaskID),
			CallID:    strings.TrimSpace(tasklog.CallIDFromContext(ctx)),
		})
	}
	return b.inner.Invoke(ctx, action, args)
}

// bashJobManager is the slice of *jobqueue.Manager the sandbox job commands
// need. It is an interface so the adapter's mapping stays testable without a
// database.
type bashJobManager interface {
	List(ctx context.Context, tenantID uuid.UUID, limit int) ([]jobqueue.JobView, error)
	Run(ctx context.Context, tenantID, sessionID uuid.UUID, req jobqueue.RunRequest) (jobqueue.RunResult, error)
}

// bashJobCommands adapts the JobManager to the sandbox's jobs/ps/nohup
// commands. The view is the persisted job fact, not a host process: job ids are
// keel job ids, and no numeric host pid is ever rendered.
type bashJobCommands struct {
	manager func() bashJobManager
}

func (b bashJobCommands) managerValue() bashJobManager {
	if b.manager == nil {
		return nil
	}
	return b.manager()
}

// ListJobs implements bashbridge.JobBackend.
func (b bashJobCommands) ListJobs(ctx context.Context, tenantID uuid.UUID, limit int) ([]bashbridge.JobInfo, error) {
	manager := b.managerValue()
	if manager == nil {
		return nil, errors.New("the bash job manager is not wired")
	}
	jobs, err := manager.List(ctx, tenantID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]bashbridge.JobInfo, 0, len(jobs))
	for _, job := range jobs {
		info := bashbridge.JobInfo{
			ID:        job.JobID.String(),
			Command:   job.Command,
			Cwd:       job.Cwd,
			Status:    job.Status,
			Reason:    job.Reason,
			ExitCode:  job.ExitCode,
			CreatedAt: job.CreatedAt,
			Live:      job.Live,
		}
		if job.SessionID != nil {
			info.SessionID = job.SessionID.String()
		}
		out = append(out, info)
	}
	return out, nil
}

// StartJob implements bashbridge.JobBackend. A positive timeout is what makes
// JobManager.Run register a background job and return instead of running
// synchronously; one nanosecond keeps nohup's own wait negligible while the
// command runs in its own session and settles its own fs.capture.
func (b bashJobCommands) StartJob(ctx context.Context, tenantID, sessionID uuid.UUID, command string) (bashbridge.JobInfo, error) {
	manager := b.managerValue()
	if manager == nil {
		return bashbridge.JobInfo{}, errors.New("the bash job manager is not wired")
	}
	result, err := manager.Run(ctx, tenantID, sessionID, jobqueue.RunRequest{
		Command: command,
		Timeout: time.Nanosecond,
	})
	if err != nil {
		return bashbridge.JobInfo{}, err
	}
	info := bashbridge.JobInfo{
		Command:  command,
		Cwd:      result.Cwd,
		Status:   result.Status,
		Reason:   result.Reason,
		ExitCode: result.ExitCode,
		Live:     result.Status == store.BashJobRunning,
	}
	if result.JobID != nil {
		info.ID = result.JobID.String()
	}
	if sessionID != uuid.Nil {
		info.SessionID = sessionID.String()
	}
	return info, nil
}

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

	st, err := store.Open(ctx, store.Config{
		DSN: opts.DSN,
		// A cluster that forbids CREATE ROLE (a managed service) has to provision
		// the tenant role out of band; KEEL_RLS=off is the explicit switch for
		// that deployment. The default is on, because RLS is the schema contract.
		DisableRLS: strings.EqualFold(strings.TrimSpace(os.Getenv("KEEL_RLS")), "off"),
	})
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	// The file plugin's byte layer and its per-session execution space are built
	// before the bash manager: the bridge's workspace provider is the same object
	// the file plugin writes through to and captures from, so read/write/edit and
	// bash see one session view. The service itself is built after the kernel
	// (it commits fs.write/fs.capture); the provider resolves it lazily.
	fileStore, err := newFileStore(opts, log)
	if err != nil {
		return err
	}
	defer fileStore.Close()
	var fileService *fileplugin.Service
	sandbox, err := bashbridge.NewLayerWorkspaces(bashbridge.LayerWorkspaceOptions{
		Store:   fileStore,
		Service: func() *fileplugin.Service { return fileService },
		Logger:  log,
	})
	if err != nil {
		return fmt.Errorf("bash workspace provider: %w", err)
	}

	// Bash background jobs: the manager is a process-level singleton. Recovery
	// runs before the service graph, so no run can observe a stale "running"
	// row from a previous process. Its output uploader is separate from the
	// image uploader: bash output may be far larger than an image. The manager is
	// also the data source of the sandbox's jobs/ps/nohup commands; the bridge is
	// built first, so the adapter resolves the manager lazily.
	var bashManager *jobqueue.Manager
	jobCommands := bashbridge.NewJobCommands(bashJobCommands{manager: func() bashJobManager {
		if bashManager == nil {
			return nil
		}
		return bashManager
	}})
	bashManager, err = jobqueue.New(jobqueue.Options{
		Store:  st,
		Bridge: bashbridge.New(bashbridge.Config{Workspace: sandbox, Commands: jobCommands}),
		Uploader: &blob.Uploader{
			Store:       st,
			CASDir:      opts.CASDir,
			TempDir:     os.TempDir(),
			PGThreshold: 1 << 20,
			MaxBytes:    1 << 30,
		},
		TenantID:    opts.TenantID,
		MaxGlobal:   envInt("KEEL_BASH_MAX_JOBS", 8),
		MaxPerGroup: envInt("KEEL_BASH_MAX_JOBS_PER_SESSION", 4),
		Logger:      log,
	})
	if err != nil {
		return fmt.Errorf("bash job manager: %w", err)
	}
	if err := bashManager.Start(ctx); err != nil {
		return fmt.Errorf("bash job recovery: %w", err)
	}
	defer bashManager.Close()
	opts.Bash = bashManager

	kernelLog, err := kernel.NewPG(ctx, st.Pool(), kernel.WithBlobRefs(st))
	if err != nil {
		return fmt.Errorf("kernel migrate: %w", err)
	}
	limits := quota.New(host.QuotaConfig{
		RunConcurrency:  envInt("KEEL_QUOTA_RUNS", 0),
		EventsPerMinute: envInt("KEEL_QUOTA_EVENTS_PER_MINUTE", 0),
	})
	// Run concurrency is admitted durably by the placement gate below; the
	// kernel keeps the event-rate dimension so a run is never counted twice.
	kern := kernel.New(kernelLog, kernel.Options{
		Limits: limits.EventsOnly(),
		Audit:  servicehost.NewAuditSink(log, servicehost.AuditOptions{}),
	})
	// The harness admission gate is part of the boot contract: without it a run
	// would only consume the process-local worker semaphore and the group /
	// tenant durable quota would not exist. A misconfigured gate fails the boot.
	placementRT, err := newPlacementRuntime(st, limits, kern, runtimeID(), log)
	if err != nil {
		return fmt.Errorf("placement admission gate: %w", err)
	}

	// The task log is a harness service, not a plugin: its attribution gate must
	// keep working when keel.tasks is disabled. The plugin only supplies the
	// `tasks` tool's action backend.
	taskLog := tasklog.New(tasklog.Options{
		Committer:  kern,
		Reader:     kern,
		Projection: tasklog.NewPGProjection(st),
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
	// The MCP Apps host (P2): the registry the mcp service refreshes when the
	// fleet connects, the publication bridge that republishes the MCP half of
	// the assembly, and the API surface's read model. It is created even when
	// no server is configured: an empty catalogue is a valid answer, and the
	// capability bits then honestly report an unwired-app surface.
	mcpAppsPublication := newDynamicSurface(log)
	// The MCP metric set (P4.7) is registered once at boot: /metrics exports
	// the fixed series, and the registry/bridge report into the same set.
	metricsReg := keelmetrics.NewRegistry()
	keelmetrics.BindGoRuntime(metricsReg)
	// The runtime metric set (G3/G5): the worker's run-concurrency gauge and
	// completion counters are the scale evidence. `cmd/runtime` has wired the
	// same set since its W6 acceptance; `keel serve` owns a worker too, so the
	// same facts must be visible here.
	runMetrics := keelmetrics.NewSet(metricsReg)
	mcpMetrics := mcphost.NewMetrics(metricsReg)
	mcpApps := mcphost.New(mcphost.Options{
		TenantID: opts.TenantID,
		Logger:   log,
		Sinks:    mcpAppsPublication.sinks(),
		Metrics:  mcpMetrics,
		// Document fetch/validation outcomes are durable audit rows (P3.7): the
		// registry and the bridge share one table, one shape and one bound.
		Auditor: mcpAppAuditor{store: st, log: log},
	})
	mcpAppsSandboxOrigin := configuredMCPAppOrigin(log)
	// Establish the fixed series before the first server connects or a change
	// publishes, so a scrape always sees the same set.
	mcpMetrics.ServerStates(0, 0)
	mcpMetrics.SurfaceGeneration(mcpApps.Generation())
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
	// The managed skill library: the tenant-scoped registry behind
	// /v1/settings/skills, `keel skills` and the `skills` tool's manage/sources
	// actions. Its active/ directory joins skill discovery, so an install reaches
	// the next run's prompt; the OnChange callback (installed after boot, when the
	// runtime exists) republishes the registry.
	skillLibrary := skilllib.New(skilllib.Options{
		Root:       opts.SkillsRoot,
		SourceRoot: opts.SkillsSourceRoot,
		Logger:     log,
	})
	opts.Skills = skillLibrary
	skillsRoots := append([]string(nil), opts.SkillsRoots...)
	if managed := skillLibrary.ActiveRoot(opts.TenantID); managed != "" {
		skillsRoots = append(skillsRoots, managed)
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
		Skills: host.SkillsConfig{Roots: skillsRoots},
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

	// Tenant secret resolution is one closure shared by the chat provider path
	// and the media wiring: both resolve access.ref / access.env the same way,
	// so "settings says available" and "the call authenticates" cannot drift.
	tenantSecrets := tenantSecretLookup(st, secretBox, opts.TenantID, log)
	// mediaOptions is the product-layer media seam input for one generation.
	// servicehost calls the builders per host assembly, so a settings write that
	// changes a provider reaches the next run without a restart. The HTTP client
	// and asset fetcher are shared per process: a republish must not mint a new
	// transport (and a new idle connection pool) per generation.
	mediaClient := mediaadapter.BoundedHTTPClient(0)
	mediaFetcher := mediaadapter.NewHTTPFetcher(0)
	mediaOptions := func(tenant host.TenantConfig) mediawiring.Options {
		return mediawiring.Options{
			Models: tenant.Models,
			Legacy: modelcfg.LegacyAgent{
				Provider:  tenant.Agent.Provider,
				Model:     tenant.Agent.Model,
				BaseURL:   tenant.Agent.BaseURL,
				Protocol:  tenant.Agent.Protocol,
				APIKeyRef: tenant.Agent.APIKeyRef,
			},
			Credential: func(access modelcfg.Access) (string, string) {
				return servicehost.ResolveAccessCredential(access, opts.APIKey, os.LookupEnv, tenantSecrets)
			},
			Artifacts:  fileStore,
			HTTPClient: mediaClient,
			Fetcher:    mediaFetcher,
		}
	}

	// The file plugin's manifest is session-scoped; its bytes are the process-wide
	// store built above and its execution space is the same per-session workspace
	// provider the bash bridge mounts.
	fileService, err = newFileService(st, kern, fileStore, log, sandbox, sandbox)
	if err != nil {
		return err
	}
	opts.FileService = fileService

	// The constant tool surface needs the sub-agent runner and the plugin
	// manager, both created after Boot. The getters are captured now and resolve
	// at tool-call time; no run executes before newProductLayer returns.
	var toolSubagents *agentext.SubagentRunner
	var toolPluginManager *agentext.Manager

	// MRTR / legacy server-request executor (P5). It is constructed before the
	// MCP connections so initialize declares exactly what this host can answer:
	// sampling is advertised only when the deployment has a model configuration,
	// elicitation only while the decision capability is enabled. The decision
	// bridge and sampler depend on services built later, so they are installed
	// after Boot and after the product layer assembles (no tool call can run
	// before then).
	samplingMode := mcphost.SamplingModeFromEnvironment(os.Getenv)
	if samplingMode != mcphost.SamplingOff && len(modelsDoc) == 0 && strings.TrimSpace(opts.Model) == "" {
		samplingMode = mcphost.SamplingOff
	}
	mrtr := mcphost.NewMRTR(mcphost.MRTROptions{
		Logger:       log,
		Auditor:      mcpAppAuditor{store: st, log: log},
		Metrics:      mcpMetrics,
		SamplingMode: samplingMode,
	})

	rt, err := servicehost.Boot(ctx, servicehost.Config{
		Metrics: runMetrics,
		Seams: servicehost.Seams{
			// The run loop keeps the raw kernel: its read-marker/checkpoint
			// capabilities are derived from the concrete committer, and the
			// placement gate only owns native input, not run.finished commits.
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
		// The MCP app context (P5.5): read per attempt, merged into the last user
		// message as <mcp_app_context>, never written to the ledger.
		SessionContexts: func(ctx context.Context, scope kernel.Scope) []agentcore.Content {
			rows, err := st.ListMCPAppContexts(ctx, scope.TenantID, scope.SessionID)
			if err != nil {
				log.Warn("load mcp app context", "session_id", scope.SessionID, "err", err)
				return nil
			}
			contexts := make([]mcphost.AppContext, 0, len(rows))
			for _, row := range rows {
				contexts = append(contexts, mcphost.AppContext{
					AppID:      row.AppID,
					Content:    row.Content,
					Structured: row.Structured,
				})
			}
			return mcphost.RenderAppContexts(contexts)
		},
		// A forked session's transcript prefix is read from the parent log (a
		// reference, never a copy). The store owns the durable fork fact.
		ForkPrefixes: st,
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
			Entries: mcpEntries,
			ConnectOptions: mcp.ConnectOptions{
				ClientName:    "keel",
				ClientVersion: version,
				Logger:        log,
				// P5.4: the configured dialect gate. Empty means the deployment
				// default (auto unless KEEL_MCP_PROTOCOL_ERA says otherwise; an
				// invalid value fails closed to legacy).
				Era: mcp.EraFromEnvironment(os.Getenv),
			},
			Registry: mcpApps,
			// P5.2/P5.3: the shared MRTR executor. The mcp service derives the
			// declared capabilities from it and uses it for legacy server
			// requests.
			MRTR: mrtr,
			// P4: the notification surface. The dispatcher classifies every
			// notification, coalesces catalogue changes into one republish and
			// keeps one listen stream per modern server. The hook event makes a
			// republish observable without changing a running run's tool set.
			Events: &mcphost.EventsOptions{
				Logger:  log,
				Auditor: mcpAppAuditor{store: st, log: log},
				Metrics: mcpMetrics,
				OnSurfaceChanged: func(change mcphost.SurfaceChange) {
					payload := agentext.HookPayload{
						Event:    agentext.HookMCPSurfaceChanged,
						TenantID: opts.TenantID.String(),
					}
					payload.SetData("generation", fmt.Sprintf("%d", change.Generation))
					payload.SetData("servers", strings.Join(change.Servers, ","))
					payload.SetData("kinds", strings.Join(change.Kinds, ","))
					payload.SetData("added", strings.Join(change.Added, ","))
					payload.SetData("removed", strings.Join(change.Removed, ","))
					hooks.Emit(payload)
				},
				ListenOptions: mcp.ListenOptions{OnDiagnostic: func(message string) {
					log.Warn("mcp listen", "detail", message)
				}},
			},
		},
		LLM: servicehost.LLMOptions{
			APIKey:       opts.APIKey,
			LookupEnv:    os.LookupEnv,
			LookupSecret: tenantSecrets,
			// Outbound image resolution (keel-ga §3 provider-files): events carry
			// only a blobId, and the worker streams the bytes here before the
			// request leaves. Without it an image reference degrades to a named
			// text notice instead of a silent drop.
			Images: &blob.ImageSource{Store: st, Reader: blob.NewReader(st, 256, 1)},
			// Prefix under which a provider may fetch images itself; empty means
			// the url form degrades to file_id/base64 rather than emitting a URL
			// the provider cannot reach.
			PublicBaseURL: os.Getenv("KEEL_PUBLIC_BASE_URL"),
		},
		// The execution binding reads the tenant's Agent/AgentTeam documents.
		// Prompt-file references resolve against the process working directory
		// (the deployment root the CLI and the web app share).
		Bindings: servicehost.BindingsOptions{
			Reader:   st,
			TenantID: opts.TenantID,
			Root:     deploymentRoot(),
		},
		External: servicehost.ExternalOptions{
			LookupEnv: os.LookupEnv,
			Environ:   os.Environ(),
			Stderr:    os.Stderr,
		},
		Product: servicehost.ProductOptions{
			// Media tools: image_generate/video_generate/speech_tts/speech_stt/
			// decision_ask over the tenant's providers, with generated bytes in
			// this process's layerfs. Build reports per-family state; a
			// configured-but-unusable family is logged here and still fails
			// closed at the tool boundary.
			MediaTools: func(tenant host.TenantConfig) ([]agentcore.AgentTool, error) {
				result := mediawiring.Build(mediaOptions(tenant))
				for _, issue := range result.Issues {
					log.Warn("media wiring", "field", issue.Field, "reason", issue.Message)
				}
				if summary := result.Summary(); summary != "" {
					log.Info("media tools assembled", "families", summary)
				}
				return result.Tools, nil
			},
			// The settings write path's media/decision check: a decision binding
			// that fails Preflight is refused before the document is stored.
			ValidateMedia: func(tenant host.TenantConfig) []modelcfg.Issue {
				return mediawiring.ValidateModels(mediaOptions(tenant))
			},
			WrapTelemetry: func(base telemetry.Context) telemetry.Context {
				return &agentext.TurnTelemetry{Base: base, Hooks: hooks}
			},
			WrapStream: func(base provider.StreamFn) provider.StreamFn {
				return agentext.StreamTee(base, partials)
			},
			RunControl: runControl,
			ToolBackends: toolsurface.Backends{
				Bash:     bashSessionBackend{inner: agentext.ShellBackend{Manager: func() *agentext.Manager { return toolPluginManager }}},
				Agents:   agentext.AgentBackend{Runner: func() *agentext.SubagentRunner { return toolSubagents }},
				Creation: agentext.CreationBackend{Manager: func() *agentext.Manager { return toolPluginManager }},
				Tasks: agentext.TasksBackend{
					Manager: func() *agentext.Manager { return toolPluginManager },
					Service: taskLog,
				},
				Approve: agentext.ApproveBackend{Manager: func() *agentext.Manager { return toolPluginManager }},
				Notify:  agentext.NotifyBackend{Manager: func() *agentext.Manager { return toolPluginManager }},
				Files:   agentext.FileBackend{Manager: func() *agentext.Manager { return toolPluginManager }},
				// The skill library is the tenant-scoped registry behind
				// `skills manage`/`sources` and the settings page.
				Skills: skillLibrary,
				// MCP notifications/progress reach the tool row through the same
				// transient bus as token deltas (P4.5). The frame is advisory and
				// never enters the ledger.
				MCPProgress: func(scope agentcore.ToolRunScope, toolCallID string, progress mcp.ProgressNotification) {
					tenantID, tenantErr := uuid.Parse(scope.TenantID)
					sessionID, sessionErr := uuid.Parse(scope.SessionID)
					if tenantErr != nil || sessionErr != nil {
						return
					}
					partials.PublishMCPProgress(tenantID, sessionID, toolCallID, progress)
				},
			},
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
	// The runtime is up: the sampling executor now points at this generation's
	// model service (the same seam the runner uses, never a second provider
	// path). A deployment without a model leaves it nil and sampling fails
	// closed.
	mrtr.SetSampler(rt.MCPSampler())
	// The skill library's writes (install/update/pin/remove) republish the
	// prompt registry: the active/ directory is a discovery root, so a new skill
	// is indexed before the next run claims. The callback runs after the
	// library's write lock is released from the caller's perspective (the write
	// has landed), and a refresh failure leaves the previous registry in place —
	// the settings write itself still succeeded and says so.
	skillLibrary.SetOnChange(func() {
		warnings, err := rt.RefreshSkills()
		if err != nil {
			log.Warn("skill refresh failed; the previous registry keeps serving", "err", err)
			return
		}
		for _, warning := range warnings {
			log.Warn("skill discovery", "warning", warning.String())
		}
	})
	// The runtime is up: the SSE hub may hold its pool connection now.
	go hub.Run(ctx)
	// The MCP half was buffered while the graph activated (mcpService.Start
	// publishes before the runtime value exists): replay it now, so the first
	// assembly the plugin layer builds already composes both halves.
	mcpAppsPublication.attach(rt)
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
	// The edge quota enforcer is built before the product layer: the tenant's
	// quota projection reports the same counters the stream and upload paths
	// enforce, so "my usage" and "why was I refused" cannot disagree.
	edge := newEdgeQuota(st)
	hostTheme := loadHostTheme(static.root, log)
	product, err := newProductLayer(ctx, st, kern, placementRT.Edge, placementRT.Gate, rt, hooks, runControl, partials, taskLog, opts, secretBox, secretReason,
		mcpApps, mcpAppsSandboxOrigin, hostTheme, edge, mcpMetrics, mrtr, log)
	if err != nil {
		return err
	}
	toolSubagents = product.subagents
	toolPluginManager = product.pluginManager
	defer product.close()

	// Drain is the same edge contract the runtime uses: new requests get 503 +
	// Retry-After, every open SSE stream receives the drain frame and closes at
	// the configured rate. keel serve owns the worker, so draining also stops
	// claiming.
	drainer := httpapi.NewDrainer(3000, 200)
	streamMetrics := &stream.Metrics{}
	keelmetrics.BindQueueDepth(metricsReg, st)
	keelmetrics.BindStreams(metricsReg, func() int64 { return streamMetrics.StreamsActive.Load() })
	// Blob lifecycle (keel-ga §5). The uploader queues supported images and
	// wakes the generator; the generator's own uploader is bare so a thumbnail
	// never queues itself. The sweeper reaps TTL-expired, unreferenced blobs
	// (metadata -> chunks -> CAS) and retries a crashed unlink through its
	// orphan scan. Both loops stop with the serve context.
	blobReader := blob.NewReader(st, 256, 1)
	blobUploader := &blob.Uploader{Store: st, CASDir: opts.CASDir, TempDir: os.TempDir()}
	thumbnails := blob.NewThumbnailer(st, blob.NewReader(st, 4, 1), blobUploader, log)
	thumbnails.Interval = envDuration("KEEL_BLOB_THUMB_INTERVAL", blob.ThumbDefaultInterval)
	blobUploader.Thumbnails = thumbnails
	go thumbnails.Run(ctx)
	blobGC := blob.NewSweeper(st, opts.CASDir, log)
	blobGC.Interval = envDuration("KEEL_BLOB_GC_INTERVAL", blob.DefaultGCInterval)
	blobGC.OrphanInterval = envDuration("KEEL_BLOB_ORPHAN_INTERVAL", blob.DefaultOrphanInterval)
	go blobGC.Run(ctx)
	api := httpapi.New(httpapi.Config{
		Store:     st,
		Hub:       hub,
		Edge:      placementRT.Edge,
		Placement: placementRT.Gate,
		Events:    rt.Projection(),
		Blobs:     blobReader,
		Uploader:  blobUploader,
		Scheduler: stream.NewScheduler(256),
		Admitter:  stream.NewAdmitter(200, 400),
		Limiter:   stream.NewLimiter(0),
		Metrics:   streamMetrics,
		// Prometheus text at /metrics: the fixed runtime/MCP set registered at
		// boot (P4.7).
		MetricsExporter: metricsReg,
		Ready:           st,
		Drain:           drainer,
		Logger:          log,
		CASDir:          opts.CASDir,
		Product:         product.api,
		Partials:        agentapi.PartialSource(product.partials),
		// Agent registry and agent teams: the store owns the documents, the HTTP
		// surface owns the rules (change keel-agent-team-v1, P2/P3).
		AgentTeams: st,
		// A saved agent may only grant tools the current assembled surface
		// actually serves. The lookup is resolved per request, so a plugin
		// install/uninstall moves what a new save may reference without a restart
		// (spec: 保存 Agent 时校验 toolId 存在且当前租户可用).
		ToolExists: func(toolID string) bool {
			_, ok := rt.Assembly().Tools().Get(toolID)
			return ok
		},
		// The selection vocabulary is read from the CURRENT generation at call
		// time: a settings write installs a new model service, and the input edge
		// must accept exactly what that generation can run. A runtime without
		// provider configuration still refuses every selection.
		//
		// The execution binding (agentId/teamId) is part of that same gate: the
		// submit path resolves it exactly as an attempt would (existence, archive,
		// seal) and merges the agent's default model into a fact that named none,
		// so "what the edge accepts" stays "what the next attempt runs".
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
	// Credential order matters: a machine credential (Bearer keel_* / X-API-Key)
	// is resolved first and wins over the cookie/passphrase session, because a
	// key names its own tenant and a client holding one must never have the
	// server's configured tenant substituted for it. Every request without a
	// key-shaped credential falls through to the cookie handler unchanged, so a
	// deployment whose tenants use the browser session is unaffected.
	//
	// The quota middleware sits inside the cookie handler, after the tenant is
	// known: it needs the resolved X-Tenant-ID to charge the right tenant.
	quotaEdge := &edgeQuotaMiddleware{Edge: edge, Next: dispatch}
	// A verified key goes straight to the dispatch (through the quota check): the
	// cookie handler would refuse a key-only request for lacking a session, which
	// is exactly the shape a machine client has. Everything else keeps the
	// passphrase path unchanged.
	authorizer := &auth.Authorizer{
		Keys:     apiKeyStore{store: st},
		Verified: quotaEdge,
		Next:     product.auth.wrap(quotaEdge),
	}
	var handler http.Handler = authorizer

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

// forkSnapshots adapts the file plugin's manifest to the sub-agent fork seam.
// The tree hash travels as its prefixed string form because agentext treats it
// as an opaque reference.
type forkSnapshots struct{ manifest fileplugin.Manifest }

func (f forkSnapshots) SnapshotAtOrBefore(ctx context.Context, tenantID, sessionID uuid.UUID, seq int64) (string, int64, bool, error) {
	tree, snapshotSeq, ok, err := f.manifest.SnapshotAtOrBefore(ctx, tenantID, sessionID, seq)
	if err != nil || !ok {
		return "", snapshotSeq, ok, err
	}
	return tree.Prefixed(), snapshotSeq, true, nil
}

// forkCards adapts the task log to the fork card seam (origin resolution and
// the new card's parentTaskId).
type forkCards struct{ service *tasklog.Service }

func (f forkCards) OriginCard(ctx context.Context, scope kernel.Scope, seq int64) (uuid.UUID, error) {
	card, err := f.service.OriginCard(ctx, scope, seq)
	if err != nil {
		return uuid.Nil, err
	}
	return card.TaskID, nil
}

func (f forkCards) ForkCard(ctx context.Context, origin kernel.Scope, originTaskID uuid.UUID, originSeq int64, target kernel.Scope, title, callID string) (uuid.UUID, error) {
	card, err := f.service.ForkCard(ctx, origin, originTaskID, originSeq, target, title, callID)
	if err != nil {
		return uuid.Nil, err
	}
	return card.TaskID, nil
}

func (f forkCards) CloseForkCard(ctx context.Context, scope kernel.Scope, childSessionID uuid.UUID, reason, callID string) error {
	taskID := tasklog.InitialTaskID(scope.TenantID, childSessionID)
	_, err := f.service.Close(ctx, scope, taskID, reason, callID)
	if errors.Is(err, tasklog.ErrTaskNotFound) {
		return nil
	}
	return err
}

// forkAdmission compensates a failed fork's placement reservation. Release
// drops the pending run slot; Leave removes the placement EnsurePlacement may
// have created before admission failed.
type forkAdmission struct{ gate *placement.Gate }

func (f forkAdmission) Release(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	if f.gate == nil {
		return nil
	}
	return f.gate.Release(ctx, tenantID, sessionID)
}

func (f forkAdmission) Leave(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	if f.gate == nil {
		return nil
	}
	service := f.gate.Service()
	current, err := service.Store().GetPlacement(ctx, tenantID, sessionID)
	if errors.Is(err, placement.ErrPlacementNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = service.Leave(ctx, placement.LeaveRequest{
		TenantID: tenantID, SessionID: sessionID,
		WorkerID: current.WorkerID, ExpectedEpoch: current.Epoch,
	})
	return err
}

// runRequeuer adapts the store's run requeue to the sub-agent resume seam.
type runRequeuer struct{ store *store.Store }

func (r runRequeuer) RequeueRun(ctx context.Context, tenantID, sessionID uuid.UUID) (agentext.RequeueOutcome, error) {
	outcome, err := r.store.RequeueRun(ctx, tenantID, sessionID)
	return agentext.RequeueOutcome{Status: outcome.Status, Attempts: outcome.Attempts}, err
}

// newProductLayer assembles the extension surface over an already-booted
// service runtime. secretBox (nil when KEEL_SECRET_KEY is unset) enables the
// settings secret store; secretReason explains its absence to an operator;
// theme is the host theme token document handed to plugin UIs through the
// bridge (nil when the web build has none).
func newProductLayer(ctx context.Context, st *store.Store, kern *kernel.Kernel, admission kernel.Committer, admissionGate *placement.Gate, rt *servicehost.Runtime, hooks *agentext.HookBus, runControl *agentext.RunControl, partials *agentext.PartialHub, taskLog *tasklog.Service, opts serveOptions, secretBox *store.SecretBox, secretReason string, mcpApps *mcphost.Registry, mcpAppsSandboxOrigin string, theme json.RawMessage, edge *quota.Edge, mcpMetrics *mcphost.Metrics, mrtr *mcphost.MRTR, log *slog.Logger) (*productLayer, error) {
	sessions := storeSessions{Store: st}
	decisions := agentext.NewDecisionBroker(agentext.DecisionBrokerOptions{
		Store:  agentext.NewTasksDecisionStore(st),
		Hooks:  hooks,
		Logger: log,
	})
	// The MRTR executor opens elicitations through the same decision primitive
	// the product UI answers. No tool call can run before this point, so the
	// capability declared at initialize is honest by the time it is used.
	mrtr.SetDecisions(decisions)
	// Approval provenance and notifications are durable product facts. The
	// provenance rows are the audit of who answered; the notification rows carry
	// the consumer set, while notification.ack / notification.push_sent are the
	// event-ized truth (the kernel is the committer).
	approvals := approval.NewService(approval.Options{
		Store:  approval.NewPostgresStore(st),
		Logger: log,
	})
	notifications := notify.NewService(notify.Options{
		Store:     notify.NewPostgresStore(st),
		Committer: kern,
		Logger:    log,
		BaseURL:   os.Getenv("KEEL_PUBLIC_BASE_URL"),
	})
	// The binding projection is one recorder shared by every TurnStarter path
	// (triggers, sub-agent spawn, manual fire) and by the HTTP input paths. It
	// only writes the derived sessions.bound_* index; the event ledger stays the
	// truth.
	bindingRecorder := sessionbinding.New(st)
	starter := agentext.NewTurnStarter(sessions, admission, hooks, log)
	starter.Bindings = bindingRecorder
	var forkSnapshotReader agentext.ForkSnapshotReader
	if opts.FileService != nil {
		forkSnapshotReader = forkSnapshots{manifest: opts.FileService.Manifest()}
	}
	var forkAdmissionOpt agentext.ForkAdmission
	if admissionGate != nil {
		forkAdmissionOpt = forkAdmission{gate: admissionGate}
	}
	subagents := agentext.NewSubagentRunner(agentext.SubagentOptions{
		Gate:    sessions,
		Edge:    admission,
		Events:  kern,
		Runs:    sessions,
		Control: runControl,
		Hooks:   hooks,
		Logger:  log,
		// A spawned child's input carries a selection fact exactly like a
		// session input; the spawn must refuse an unexecutable one at the call,
		// before the child session or its user.message exists. Bind the same
		// per-generation gate the /input edge uses so the two cannot drift.
		ValidateSelection: selectFromCurrentGeneration(rt),
		// A live child session must block its Agent's delete/archive exactly like
		// a session started through POST /input (session reference protection).
		Bindings: bindingRecorder,
		// Fork/resume (W8): the fork origin is a durable session fact (tree hash
		// + ledger origin), the card writer points the new card at the origin
		// card, and resume re-enqueues the same run row.
		Forks:     st,
		Snapshots: forkSnapshotReader,
		Cards:     forkCards{service: taskLog},
		Admission: forkAdmissionOpt,
		Cleanup:   st,
		Requeue:   runRequeuer{store: st},
		Notes:     taskLog,
		ConfigVersion: func() (string, int64) {
			snapshot := rt.Snapshot()
			if snapshot == nil {
				return "", 0
			}
			return agent.ConfigHashForLoadout(snapshot.LoadoutID, snapshot.Revision), snapshot.Revision
		},
	})
	triggers := agentext.NewTriggerEngine(agentext.TriggerEngineOptions{
		Starter: starter,
		Hooks:   hooks,
		Logger:  log,
	})

	surface, err := newPluginSurface(ctx, rt, st, opts, hooks, triggers, decisions, approvals, notifications, subagents, partials, taskLog, log)
	if err != nil {
		return nil, err
	}
	manager, toolIndex, plugins := surface.manager, surface.toolIndex, surface.runtime

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
		// The member-task projection of a captain session: the child cards stay
		// in their own ledgers and are read through parentTaskId. Read-only.
		TeamTasks: taskLog,
		// DELETE /v1/sessions/{id} is the soft delete: the session leaves the
		// list, its ledger stays (see store.ArchiveSession).
		ArchiveSession: st.ArchiveSession,
		Cancel:         cancelFn,
		// A product write must land on a session this tenant owns: a decision
		// opened on a missing session would be an orphan row.
		SessionExists: func(ctx context.Context, tenantID, sessionID uuid.UUID) (bool, error) {
			_, exists, err := st.SessionOwner(ctx, tenantID, sessionID)
			return exists, err
		},
		Decisions:     decisions,
		Approvals:     approvals,
		Notifications: notifications,
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
		// MCP Apps (P2): the catalogue/document surface and the two capability
		// inputs (the configured sandbox origin and the host theme palettes the
		// View maps onto the apps standard variables). Nil registry answers 501
		// on the MCP app routes and reports mcp.apps=false.
		MCPApps:              mcpApps,
		MCPAppsSandboxOrigin: mcpAppsSandboxOrigin,
		MCPAppsTheme:         theme,
		// The app bridge (P3): tools/call and resources/read execute over the
		// same MCP connections the model uses; a confirmed ui/message commits
		// through the runtime's shared input path; every action reaches the
		// durable audit table.
		MCPAppRuntime:     mcpAppRuntimeOrNil(rt),
		MCPAppAudit:       st,
		MCPAppInputCommit: mcpAppInputCommit(st, admission),
		// P5.5: ui/update-model-context persists into mcp_app_contexts (never the
		// ledger); the runner reads the same table for the next attempt.
		MCPAppContexts: st,
		// P4: the dispatcher is wired by the MCP service (Events above), so the
		// events capability is honest for clients that gate on it.
		MCPEvents:     true,
		MCPAppMetrics: mcpMetrics,
		Triggers:      triggers,
		Subagents:     subagents,
		Hooks:         hooks,
		ModelsLive: func() agentapi.ModelCatalog {
			if rt.Models() == nil {
				return nil
			}
			return modelSeam
		},
		ModelSettingsStore:   st,
		ModelSettingsRuntime: modelSeam,
		// A provider test is one minimal upstream request through the same
		// credential resolution an attempt uses; the CLI never sees the value.
		ProviderProber: modelSeam,
		// Provider deletion protection reads the agent/team registry (a provider
		// an agent or a team still binds is refused, with the referrers listed).
		ProviderReferences: st,
		SecretStore:        secretStore,
		SecretStoreReason:  secretReason,
		// The skill settings surface and the session-local completion sources:
		// both read services the tools already use, never a second copy.
		Skills:       opts.Skills,
		SessionFiles: opts.FileService,
		SessionTasks: taskLog,
		// Tenancy surface (change keel-ga §2): the operator provisioning path and
		// the tenant's own quota projection. Both read the same store the request
		// path uses; neither is reachable with a tenant credential alone.
		Provisioner: apiKeyStore{store: st},
		Quota:       quotaReporter{store: st, edge: edge},
		AdminKey:    adminKeyFromEnv,
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

// pluginSurface is the product's plugin layer: the lifecycle authority, the
// tool index the UI bridge shares with the model registry, and the runtime the
// api reads snapshots from.
type pluginSurface struct {
	manager   *agentext.Manager
	toolIndex *productToolIndex
	runtime   *agentext.Runtime
}

// newPluginSurface builds the plugin lifecycle authority over the live host
// assembly: the bundled plugin set this product ships (ordinary plugin packages
// whose manifests live in the binary — the manager drives their enablement like
// any other plugin, so `keel.example` can be disabled from the API and stays
// disabled), the runtime publication sinks, and the sidecar loader of this
// build. persistence is the durable inventory/audit store (nil keeps the
// manager in memory). The manager is booted — the inventory is reconciled
// against what is on disk and assembled from persisted intent, so a plugin an
// operator disabled stays disabled — before it is returned.
//
// It is a constructor rather than inline code so the serving path and its tests
// share exactly this wiring: the model-side proof that a plugin's prompt
// section reaches the system prompt rides on these sinks.
func newPluginSurface(ctx context.Context, rt *servicehost.Runtime, persistence agentext.PluginPersistence, opts serveOptions, hooks *agentext.HookBus, triggers *agentext.TriggerEngine, decisions *agentext.DecisionBroker, approvals *approval.Service, notifications *notify.Service, subagents *agentext.SubagentRunner, partials *agentext.PartialHub, tasks *tasklog.Service, log *slog.Logger) (*pluginSurface, error) {
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
		testkit.PluginID: func() *agentext.Plugin { return agentext.NewTestkitPlugin() },
		agentext.FilePluginID: func() *agentext.Plugin {
			return agentext.NewFilePlugin(agentext.FilePluginOptions{Service: opts.FileService})
		},
		agentext.ModelPluginID: func() *agentext.Plugin { return agentext.NewModelPlugin() },
		agentext.AgentPluginID: func() *agentext.Plugin { return agentext.NewAgentPlugin() },
		agentext.TeamPluginID:  func() *agentext.Plugin { return agentext.NewTeamPlugin() },
		agentext.CreationPluginID: func() *agentext.Plugin {
			return agentext.NewCreationPlugin(agentext.CreationOptions{
				Manager:  func() *agentext.Manager { return pluginManager },
				TenantID: opts.TenantID,
			})
		},
		agentext.TasksPluginID: func() *agentext.Plugin {
			return agentext.NewTasksPlugin(agentext.TasksOptions{Service: tasks})
		},
		agentext.ShellPluginID: func() *agentext.Plugin {
			return agentext.NewShellPlugin(agentext.ShellOptions{
				Service:  func() *jobqueue.Manager { return opts.Bash },
				TenantID: opts.TenantID,
			})
		},
		agentext.ApprovalPluginID: func() *agentext.Plugin {
			return agentext.NewApprovalPlugin(agentext.ApprovalOptions{
				Decisions:  decisions,
				Provenance: approvals,
				Logger:     log,
			})
		},
		agentext.NotifyPluginID: func() *agentext.Plugin {
			return agentext.NewNotifyPlugin(agentext.NotifyOptions{
				Service: notifications,
				Logger:  log,
			})
		},
	}
	builtins := make([]agentext.Builtin, 0, len(builtinManifests))
	for _, manifest := range builtinManifests {
		factory, ok := builtinFactories[manifest.ID]
		if !ok {
			return nil, fmt.Errorf("no built-in plugin is registered for the manifest %s", manifest.ID)
		}
		defaultEnabled := manifest.ID != testkit.PluginID
		builtins = append(builtins, agentext.Builtin{Manifest: manifest, Factory: factory, DefaultEnabled: defaultEnabled})
	}
	// The tool index mirrors the dynamic tool set the plugin runtime publishes:
	// the plugin UI bridge calls a plugin's own tools through it, so a bridge call
	// executes exactly what the model would execute.
	toolIndex := newProductToolIndex()
	plugins := newPluginRuntime(rt, toolIndex, hooks, triggers, decisions, subagents, partials, log)
	manager, err := agentext.NewPluginManager(agentext.ManagerOptions{
		Root:        opts.PluginsRoot,
		Runtime:     plugins,
		Persistence: persistence,
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
	applyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err = manager.Boot(applyCtx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("assemble plugins: %w", err)
	}
	return &pluginSurface{manager: manager, toolIndex: toolIndex, runtime: plugins}, nil
}

// newPluginRuntime wires the plugin runtime's publication sinks to the live
// host assembly. Both are the republish form — whatever the runtime currently
// publishes becomes the surface — so a plugin that starts or stops is effective
// on the next model call, without a restart:
//
//	Tools  → the bridge's tool index + the host registry (servicehost composes
//	         the MCP, external, static and plugin tools);
//	Prompt → the host's dynamic prompt sections (servicehost composes the MCP
//	         section and the plugin sections; the tenant/static sections stay).
//
// The prompt sink is what makes a plugin's PromptSections reach the model:
// RuntimeOptions.Prompt is the runtime's seam, and this is its wiring.
func newPluginRuntime(rt *servicehost.Runtime, toolIndex *productToolIndex, hooks *agentext.HookBus, triggers *agentext.TriggerEngine, decisions *agentext.DecisionBroker, subagents *agentext.SubagentRunner, partials *agentext.PartialHub, log *slog.Logger) *agentext.Runtime {
	return agentext.NewRuntime(agentext.RuntimeOptions{
		Hooks:     hooks,
		Triggers:  triggers,
		Decisions: decisions,
		Subagents: subagents,
		Partials:  partials,
		Tools: func(tools []agentcore.AgentTool) error {
			toolIndex.publish(tools)
			return rt.RefreshProductTools(tools)
		},
		Prompt: func(sections []agentext.PromptSection) error {
			return rt.RefreshProductPrompt(pluginPromptSections(sections))
		},
		Logger: log,
	})
}

// pluginPromptSections projects the plugin runtime's published prompt sections
// onto the host assembly's section type. Section names carry over verbatim: the
// name is what the model sees (<name>...</name>), and the runtime has already
// dropped empty contributions.
func pluginPromptSections(sections []agentext.PromptSection) []host.Section {
	if len(sections) == 0 {
		return nil
	}
	out := make([]host.Section, 0, len(sections))
	for _, section := range sections {
		out = append(out, host.Section{Name: section.Name, Content: section.Content})
	}
	return out
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

// ProbeProvider implements agentapi.ProviderProber.
func (m modelRuntimeSeam) ProbeProvider(ctx context.Context, providerID string) (modelcfg.ProviderProbe, bool, error) {
	service := m.rt.Models()
	if service == nil {
		return modelcfg.ProviderProbe{}, false, nil
	}
	return service.ProbeProvider(ctx, providerID)
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
		ProviderIDs:         candidate.ProviderIDs,
	}, nil, nil
}

// ApplyModels implements agentapi.ModelSettingsRuntime.
func (m modelRuntimeSeam) ApplyModels(ctx context.Context, document json.RawMessage) error {
	_, err := m.rt.ApplyModels(ctx, document)
	return err
}

// newFileStore builds the process-wide layerfs byte store. The CAS root is
// shared with the blob uploader's directory but isolated in a layerfs/ subtree:
// layerfs GC must never delete an object the blob table still references.
// Without KEEL_CAS_DIR the root is ephemeral (Phase A); the log line makes that
// visible instead of silently losing files on restart.
func newFileStore(opts serveOptions, log *slog.Logger) (*layerfs.Store, error) {
	root := strings.TrimSpace(opts.CASDir)
	if root == "" {
		root = filepath.Join(os.TempDir(), "keel-layerfs")
		log.Warn("KEEL_CAS_DIR is not set; layerfs uses an ephemeral temp root", "root", root)
	} else {
		root = filepath.Join(root, "layerfs")
	}
	store, err := layerfs.Open(root, layerfs.Options{})
	if err != nil {
		return nil, fmt.Errorf("open layerfs: %w", err)
	}
	return store, nil
}

// newFileService builds the file plugin service over the process-wide store.
// The execution space is session-scoped (the bash bridge's workspace provider):
// it is passed as both the write-through surface and the capture snapshot
// source, so "write then bash read" and "bash write then capture" act on one
// view. The directory Surface/CaptureRoot pair remains for the local/test
// adapter and is deliberately left unset here.
func newFileService(st *store.Store, kern *kernel.Kernel, fileStore *layerfs.Store, log *slog.Logger,
	surface fileplugin.SessionSurface, snapshots fileplugin.SnapshotSource) (*fileplugin.Service, error) {
	manifest, err := fileplugin.NewPostgresManifest(st.Pool())
	if err != nil {
		return nil, fmt.Errorf("file manifest: %w", err)
	}
	service, err := fileplugin.New(fileplugin.Options{
		Store:          fileStore,
		Manifest:       manifest,
		Committer:      kern,
		Leases:         kern,
		SessionSurface: surface,
		SnapshotSource: snapshots,
		Forks:          st,
		Logger:         log,
	})
	if err != nil {
		return nil, fmt.Errorf("file plugin: %w", err)
	}
	return service, nil
}

// deploymentRoot is the directory an agent's prompt-file reference resolves
// against: the process working directory, which is the deployment root the CLI
// and the web app share. An unresolvable cwd yields "" (file prompts are then
// refused with a diagnostic instead of guessing).
func deploymentRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	return dir
}

// selectFromCurrentGeneration is the input edge's selection validator: it
// validates against the model service the runtime is serving right now, so a
// settings write changes what the next input may ask for without a restart. A
// runtime without provider configuration refuses every selection, exactly like
// a boot without a models document always did.
//
// It delegates to the runtime's own gate so the execution binding (agentId/
// teamId) travels the same path: resolving it here as well would be a second
// rule that could drift from what the attempt runs.
func selectFromCurrentGeneration(rt *servicehost.Runtime) func(json.RawMessage) error {
	return func(selection json.RawMessage) error {
		if rt == nil {
			return errors.New("selection is not supported by this server")
		}
		return rt.ValidateSelection(context.Background(), selection)
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

// adminKeyFromEnv decides whether a request carries the deployment's operator
// key. It reads KEEL_ADMIN_KEY per request (the key may be rotated without a
// restart) and compares in constant time; the value is never logged.
func adminKeyFromEnv(r *http.Request) bool {
	expected := strings.TrimSpace(os.Getenv("KEEL_ADMIN_KEY"))
	if expected == "" {
		return false
	}
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), []byte(expected)) == 1
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
