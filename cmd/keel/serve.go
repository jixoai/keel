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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/accessgate"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/approval"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/bashbridge"
	"github.com/jixoai/keel/internal/blob"
	"github.com/jixoai/keel/internal/capturepolicy"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/configfile"
	"github.com/jixoai/keel/internal/fileplugin"
	"github.com/jixoai/keel/internal/harnessapi"
	"github.com/jixoai/keel/internal/harnessext"
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
	placementwiring "github.com/jixoai/keel/internal/placement/wiring"
	"github.com/jixoai/keel/internal/pluginpkg"
	"github.com/jixoai/keel/internal/pluginpkg/builtin"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/quota"
	"github.com/jixoai/keel/internal/servicehost"
	"github.com/jixoai/keel/internal/sessionbinding"
	"github.com/jixoai/keel/internal/sessionpolicy"
	"github.com/jixoai/keel/internal/skilllib"
	"github.com/jixoai/keel/internal/spillblob"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/stream"
	"github.com/jixoai/keel/internal/tasklog"
	"github.com/jixoai/keel/internal/telemetry"
	"github.com/jixoai/keel/internal/testkit"
	"github.com/jixoai/keel/internal/toolauth"
	"github.com/jixoai/keel/internal/toolsurface"
)

// serveOptions is the resolved `keel serve` configuration (flags + environment).
type serveOptions struct {
	Addr   string
	DSN    string
	WebDir string
	CASDir string
	// OARJournalDir is the directory of the OAR record journals
	// (session-<ref>.ndjson). The external-run projection route reads it; empty
	// leaves that route answering 501 oar_not_wired (a deployment without OAR
	// must not serve an empty projection as if the capability were present).
	OARJournalDir string
	TenantID      uuid.UUID
	AuthMode      authMode
	Insecure      bool
	// SecretKey is the 32-byte KEEL_SECRET_KEY (or an ephemeral key in
	// local-only/off when it is unset). It signs the session cookie and CSRF.
	SecretKey []byte
	// LocalAdmin is the built-in principal for local-only/off.
	LocalAdmin   auth.Principal
	Runs         int
	NoWorker     bool
	Model        string
	Provider     string
	BaseURL      string
	Protocol     string
	APIKey       string
	SystemPrompt string
	SkillsRoots  []string
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
	// ConfigFile is the config.toml path loaded at startup and rotated
	// atomically by the host. Missing is an empty optional document; invalid
	// fails startup.
	ConfigFile string
	// ConfigLoader is the loaded config.toml version (set by runServe before
	// the product layer), so every consumer pins the same version.
	ConfigLoader *configfile.Loader
	// PluginsRoot is the managed plugin directory tree
	// ($KEEL_HOME/plugins/<id>/plugin.yaml). An empty root disables install and
	// discovery; bundled plugins still load.
	PluginsRoot string
	// ExampleBotID is the Bot the built-in example triggers fire as. Empty
	// leaves the example plugin without triggers (every trigger needs a Bot
	// owner); the plugin's tools/hooks still load.
	ExampleBotID    string
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
	// Workspace is the per-session execution space the bash bridge mounts. It
	// is also the explicit checkpoint surface behind bash
	// {action:"checkpoint"} and the run-completion capture hook.
	Workspace *bashbridge.LayerWorkspaces
	// SubagentMaxDepth is the deployment's sub-agent depth setting
	// (KEEL_SUBAGENT_MAX_DEPTH): -1 unlimited, 0 forbidden, positive a cap.
	// Nil keeps the mode's three-level default.
	SubagentMaxDepth *int
	// SubagentMaxChildren is the direct-children cap
	// (KEEL_SUBAGENT_MAX_CHILDREN). Nil keeps the mode default (8). The same
	// value reaches the runner's defensive check and the mode descriptor the
	// Host enforces.
	SubagentMaxChildren *int
	// SuperagentWorkerTools bridges the model-facing report/ask tools to the
	// super-agent orchestrator assembled later in this process. It is created
	// before servicehost.Boot (the host assembly captures the tool instances)
	// and receives the orchestrator after newSuperagentRuntime succeeds.
	SuperagentWorkerTools *superagentToolBackend
	// TelemetryConfig is the tenant telemetry section in the plugin vocabulary.
	// It is snapshotted after the config.toml overlay and shared by the service
	// factory, the plugin factory, the approval preview and the status hub.
	TelemetryConfig telemetry.Config
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
	if scope, ok := harnessext.RunScopeFromContext(ctx); ok && scope.TenantID != uuid.Nil && scope.SessionID != uuid.Nil {
		var fields struct {
			TaskID string `json:"taskId"`
		}
		_ = json.Unmarshal(args, &fields)
		mode, _ := capturepolicy.CaptureModeFromContext(ctx)
		ctx = bashbridge.WithSessionIdentity(ctx, bashbridge.SessionIdentity{
			TenantID:    scope.TenantID,
			SessionID:   scope.SessionID,
			TaskID:      strings.TrimSpace(fields.TaskID),
			CallID:      strings.TrimSpace(tasklog.CallIDFromContext(ctx)),
			CaptureMode: mode,
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
		return errors.New("KEEL_DSN (or --dsn) is required; for a local cluster without Docker run keel db start")
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
	if opts.AuthMode == "" {
		opts.AuthMode = authModeRequired
	}
	if err := validateAuthBinding(opts.AuthMode, opts.Insecure, opts.Addr); err != nil {
		return err
	}
	if opts.AuthMode == authModeRequired && opts.TenantID == uuid.Nil {
		return errors.New("KEEL_SERVE_TENANT is required for auth=required")
	}
	if opts.AuthMode.usesLocalAdmin() && opts.TenantID == uuid.Nil {
		opts.TenantID = localTenantID
	}
	secretKey, err := ensureAuthSecret(opts.AuthMode, log)
	if err != nil {
		return err
	}
	opts.SecretKey = secretKey
	// The super-agent worker tools are captured by the host assembly during
	// Boot; the orchestrator they call is assembled later and installed through
	// this holder. A default deployment registers no report/ask tool.
	if opts.SuperagentWorkerTools == nil {
		opts.SuperagentWorkerTools = newSuperagentToolBackend()
	}

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, store.Config{
		DSN: opts.DSN,
		// A cluster that forbids CREATE ROLE (a managed service) has to provision
		// the tenant role out of band; KEEL_RLS=off is the explicit switch for
		// that deployment. The default is on, because RLS is the schema contract.
		DisableRLS: strings.EqualFold(strings.TrimSpace(os.Getenv("KEEL_RLS")), "off"),
		// An upgrade from the deployment-level plugin inventory adopts those rows
		// into the serving tenant (one process = one tenant).
		PluginTenant: opts.TenantID,
	})
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()
	if opts.AuthMode.usesLocalAdmin() {
		localAdmin, err := ensureLocalIdentity(ctx, st, opts.TenantID, log)
		if err != nil {
			return fmt.Errorf("local admin identity: %w", err)
		}
		opts.LocalAdmin = localAdmin
	} else {
		if err := ensureServeTenant(ctx, st, opts.TenantID, log); err != nil {
			return fmt.Errorf("serve tenant: %w", err)
		}
		if err := ensureBootstrapEnvToken(ctx, st, opts.TenantID, log); err != nil {
			return fmt.Errorf("bootstrap token: %w", err)
		}
		if hasSuper, err := st.HasSuperAdmin(ctx); err != nil {
			return fmt.Errorf("bootstrap state: %w", err)
		} else if !hasSuper {
			log.Warn("no super admin exists: run `keel admin bootstrap` (or set KEEL_BOOTSTRAP_TOKEN) before signing in")
		}
	}
	// The built-in permission-group presets are part of the deployment's
	// authority surface: seed (idempotently) before any request can be served. A
	// failure is fatal, so the product never runs with a half-seeded preset.
	if err := accesswire.EnsureBuiltinGroups(ctx, st, opts.TenantID); err != nil {
		return fmt.Errorf("seed built-in permission groups: %w", err)
	}

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
	opts.Workspace = sandbox

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
	// Crash recovery for the placement queue: a queued run whose promotion was
	// lost with a previous process is promoted by this sweep (the settlement and
	// release paths handle the in-process cases).
	go placementwiring.RunQueueSweeper(ctx, placementRT, log)

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

	// Product tools are static at boot: the super-agent worker tools are added
	// when the mode is enabled (their orchestrator is installed later through
	// the same holder). Plugin tools are published after the plugin runtime
	// assembles, through Runtime.RefreshProductTools.
	// The hook bus exists before the service graph so the run telemetry
	// decorator can be installed at assembly time: the runner's
	// keel.agent.run span becomes turn.started/turn.finished with no worker
	// change.
	hooks := harnessext.NewHookBus(harnessext.HookBusOptions{Logger: log})
	// The run-control seam is created before the service graph so the worker
	// registers every attempt: the cancel endpoint can then stop an in-process
	// run instead of answering 501.
	runControl := harnessext.NewRunControlWithCanceller(storeSessions{Store: st}, storeSessions{Store: st}, log)
	// Partials are created before the service graph: the provider-stream tee
	// publishes the run's text deltas into this hub, and the SSE endpoint reads
	// them for clients that ask (?partials=1).
	partials := harnessext.NewPartialHub()

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
	// The config.toml file is loaded once at startup and fails closed: an
	// unreadable or invalid document stops the process instead of leaving a
	// half-applied policy. A missing file is an empty optional document. The
	// active file version/hash and rotation facts are logged and exposed on the
	// capability document; the per-attempt capture resolver pins the file pin it
	// starts under. The run's loadout configHash is a separate identity.
	// captureDefaults is the boot tenant document BEFORE the config.toml overlay.
	// The per-attempt resolver applies the file pin itself, so a rotation that
	// removes filesystem.capture.mode must fall back through the stored document
	// to this document. Passing the already-overlaid tenantCfg here would leak
	// the removed file value into every later attempt as "boot-default".
	captureDefaults := tenantCfg
	var configFile *configfile.Loader
	if path := strings.TrimSpace(opts.ConfigFile); path != "" {
		configFile, err = configfile.Open(configfile.Options{Path: path})
		if err != nil {
			return fmt.Errorf("load %s: %w", path, err)
		}
		opts.ConfigLoader = configFile
		configStatus := configFile.Status()
		log.Info("config.toml loaded",
			"exists", configStatus.Exists, "revision", configStatus.Revision,
			"hash", configStatus.Hash, "anchorEventSeq", configStatus.AnchorEventSeq,
			"captureMode", configStatus.CaptureMode)
		if configStatus.LastRotation != nil {
			log.Info("config.toml rotation fact",
				"revision", configStatus.LastRotation.Revision,
				"hash", configStatus.LastRotation.Hash,
				"anchorEventSeq", configStatus.LastRotation.AnchorEventSeq,
				"previousRevision", configStatus.LastRotation.PreviousRevision,
				"previousHash", configStatus.LastRotation.PreviousHash)
		}
		if overlaid, applied, err := configfile.OverlayTenant(configFile.Pin(), tenantCfg); err != nil {
			return fmt.Errorf("apply %s: %w", path, err)
		} else if applied {
			tenantCfg = overlaid
		}
	} else {
		log.Info("config.toml disabled: no path resolved")
	}
	limits.Update(tenantCfg.Quota)

	// Tenant secret resolution is one closure shared by the chat provider path
	// and the media wiring: both resolve access.ref / access.env the same way,
	// so "settings says available" and "the call authenticates" cannot drift.
	tenantSecrets := tenantSecretLookup(st, secretBox, opts.TenantID, log)
	// The process telemetry hub is created before Boot so the plugin surface and
	// GET /v1/telemetry/status read the same object the run path writes to. The
	// configured values are recorded for status/approval but stay inert: the
	// OTLP exporter only exists while keel.telemetry is enabled.
	opts.TelemetryConfig = servicehost.TelemetryPluginConfig(tenantCfg)
	opts.TelemetryConfig.TenantID = opts.TenantID.String()
	telemetryHub := telemetry.NewHub(telemetry.HubOptions{
		TenantID: opts.TenantID.String(),
		Base:     telemetry.NoopContext,
		Logger:   log,
	})
	telemetryHub.Configure(telemetry.SummarizeConfig(opts.TelemetryConfig))
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
	var toolSubagents *harnessext.SubagentRunner
	var toolPluginManager *harnessext.Manager

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

	// The access bridge is shared by the runtime tool surface and the product
	// HTTP surface: one authorizer, one grant/relation read path, one audit
	// stream. The tool seam must exist before Boot because the runner pins it
	// with the generation.
	accessBridge, err := accesswire.NewBridge(st, log)
	if err != nil {
		return fmt.Errorf("access bridge: %w", err)
	}
	toolAccess, err := toolauth.New(accessBridge, log)
	if err != nil {
		return fmt.Errorf("tool access: %w", err)
	}

	// The recovery/guardrail policy is read per attempt by the runner and per
	// scan by the supervisor. One reader keeps both halves on the same document;
	// a rotation therefore changes later attempts only.
	policyReader := newSupervisorPolicyReader(log)
	// manual/never must win before the worker starts claiming; this pass is a
	// no-op for auto.
	if err := parkDisabledBeforeBoot(ctx, st, kern, policyReader, log); err != nil {
		return fmt.Errorf("supervisor pre-boot park: %w", err)
	}
	// keel.spill's durable seam: pruned tool results are tenant blobs, stored and
	// read through the same uploader/reader as attachments. It is built before
	// Boot because the runner pins the seam with the loadout generation.
	spillStore := spillblob.New(
		&blob.Uploader{Store: st, CASDir: opts.CASDir, TempDir: os.TempDir()},
		// The reader must see the same CAS directory as the uploader: payloads
		// above the uploader's PG threshold are stored as CAS files, and a
		// reader without CASDir cannot recover them.
		&blob.ImageSource{Store: st, Reader: blob.NewReaderWithCAS(st, 256, 1, opts.CASDir)},
	)
	// The session checkpoint policy service is built before Boot (the runner
	// pins the resolver with the generation) and follows the live runtime once
	// Boot returns, so a tenant reload is visible to the next policy read.
	var liveCheckpointRuntime atomic.Pointer[servicehost.Runtime]
	initialCheckpointPolicy := servicehost.CheckpointPolicyFor(tenantCfg)
	sessionPolicies := sessionpolicy.New(st, func() compaction.CheckpointPolicy {
		if current := liveCheckpointRuntime.Load(); current != nil {
			return current.CheckpointPolicy()
		}
		return initialCheckpointPolicy
	})
	rt, err := servicehost.Boot(ctx, servicehost.Config{
		Metrics: runMetrics,
		// The session checkpoint policy is the tenant ceiling narrowed by the
		// session's stored override. The closure follows the live generation, so
		// a tenant reload changes later decisions without a restart.
		CheckpointPolicies: sessionPolicies,
		Memory:             servicehost.MemoryOptions{Spill: spillStore},
		// Every model tool call is authorized against the session's principal
		// (Bot Account ∩ Harness) before it executes (A4.2b1).
		ToolAccess:      toolAccess,
		GuardrailPolicy: policyReader.guardrails,
		// The capture policy is resolved once per attempt from the stored tenant
		// document over the PRE-overlay boot document, and the config.toml pin
		// is applied last by the resolver. The generation tenant keeps the boot
		// overlay, but it must not become the resolver's default: a rotation that
		// removes the file setting falls back to the stored/boot document.
		CapturePolicy: newRunCaptureResolver(st, captureDefaults, opts.Workspace, configFile),
		// A settled attempt frees a placement slot: the gate promotes the FIFO
		// queue head and records the observed duration for the ETA.
		Settlements: placementRT.Gate,
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
		// The telemetry hub is process-level: its exporters survive a loadout
		// reload, and the plugin/serve layers hold the same pointer.
		TenantID:  opts.TenantID,
		Telemetry: servicehost.TelemetryOptions{Hub: telemetryHub},
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
					payload := harnessext.HookPayload{
						Event:    harnessext.HookMCPSurfaceChanged,
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
			Images: &blob.ImageSource{Store: st, Reader: blob.NewReaderWithCAS(st, 256, 1, opts.CASDir)},
			// Prefix under which a provider may fetch images itself; empty means
			// the url form degrades to file_id/base64 rather than emitting a URL
			// the provider cannot reach.
			PublicBaseURL: os.Getenv("KEEL_PUBLIC_BASE_URL"),
		},
		// The execution binding reads the tenant's Bot/TeamBot/Profile/Role graph.
		// A run is always owned by a Bot; there is no deployment-prompt fallback
		// for an unowned session (fail closed).
		Bindings: servicehost.BindingsOptions{
			Reader:   st,
			TenantID: opts.TenantID,
		},
		External: servicehost.ExternalOptions{
			LookupEnv: os.LookupEnv,
			Environ:   os.Environ(),
			Stderr:    os.Stderr,
		},
		Product: servicehost.ProductOptions{
			// The super-agent worker tools are product extras, not part of the
			// constant eleven-tool builtin surface: they are registered only
			// when KEEL_SUPERAGENT_SOURCE enables the mode. A non-worker
			// session in an enabled deployment still sees them and gets a
			// typed fail-closed refusal.
			ExtraTools: superagentWorkerModelTools(opts.SuperagentWorkerTools),
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
				return &harnessext.TurnTelemetry{Base: base, Hooks: hooks}
			},
			WrapStream: func(base provider.StreamFn) provider.StreamFn {
				return harnessext.StreamTee(base, partials)
			},
			RunControl: runControl,
			ToolBackends: toolsurface.Backends{
				Bash:     bashSessionBackend{inner: harnessext.ShellBackend{Manager: func() *harnessext.Manager { return toolPluginManager }}},
				Agents:   harnessext.AgentBackend{Runner: func() *harnessext.SubagentRunner { return toolSubagents }},
				Creation: harnessext.CreationBackend{Manager: func() *harnessext.Manager { return toolPluginManager }},
				Tasks: harnessext.TasksBackend{
					Manager: func() *harnessext.Manager { return toolPluginManager },
					Service: taskLog,
				},
				Approve: harnessext.ApproveBackend{Manager: func() *harnessext.Manager { return toolPluginManager }},
				Notify:  harnessext.NotifyBackend{Manager: func() *harnessext.Manager { return toolPluginManager }},
				Files:   harnessext.FileBackend{Manager: func() *harnessext.Manager { return toolPluginManager }},
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
	liveCheckpointRuntime.Store(rt)
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
	// One upload bound for every input path: the HTTP upload handler, the
	// attachment re-check and the MCP ui/message bridge read the same number.
	uploadLimit := httpapi.DefaultMaxUpload
	hostTheme := loadHostTheme(static.root, log)
	product, err := newProductLayer(ctx, st, kern, placementRT.Edge, placementRT.Gate, rt, hooks, runControl, partials, taskLog, opts, secretBox, secretReason,
		accessBridge, mcpApps, mcpAppsSandboxOrigin, hostTheme, edge, uploadLimit, mcpMetrics, mrtr,
		newManualCompactor(rt.Binding(), st, kern, log), sessionPolicies, log)
	if err != nil {
		return err
	}
	toolSubagents = product.subagents
	toolPluginManager = product.pluginManager
	defer product.close()

	// Automatic recovery is default behavior, not a button: scan every
	// non-terminal run before the listener opens, record the recovery facts and
	// keep reconciling failed runs in the background. The worker's claim/lease
	// path remains the execution engine; this only orchestrates and audits.
	if _, err := newRecoverySupervisor(ctx, st, kern, rt, product, policyReader, log); err != nil {
		return fmt.Errorf("recovery supervisor: %w", err)
	}

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
	blobReader := blob.NewReaderWithCAS(st, 256, 1, opts.CASDir)
	blobUploader := &blob.Uploader{Store: st, CASDir: opts.CASDir, TempDir: os.TempDir()}
	thumbnails := blob.NewThumbnailer(st, blob.NewReaderWithCAS(st, 4, 1, opts.CASDir), blobUploader, log)
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
		// The input path re-checks the tenant's stored bytes before accepting an
		// attachment reference. The upload middleware already enforces the same
		// ceiling; this seam keeps the two from disagreeing (a lowered quota).
		Quota:     edge,
		MaxUpload: uploadLimit,
		// Prometheus text at /metrics: the fixed runtime/MCP set registered at
		// boot (P4.7).
		MetricsExporter: metricsReg,
		Ready:           st,
		Drain:           drainer,
		Logger:          log,
		CASDir:          opts.CASDir,
		Product:         product.api,
		SessionOwner:    newSessionOwner(st, product.access),
		SessionCreator:  newAccountSessionOwner(st, product.access),
		// Plugin approvals live in a per-tenant derived session; a client must
		// not be able to create that row and mint questions there.
		ReservedSessionID: func(tenantID, sessionID uuid.UUID) bool {
			return sessionID == harnessext.PluginApprovalSessionID(tenantID)
		},
		// A tenant created through the operator API gets the same built-in
		// permission-group presets as the serving tenant at boot (idempotent).
		TenantCreated: func(ctx context.Context, tenantID uuid.UUID) error {
			return accesswire.EnsureBuiltinGroups(ctx, st, tenantID)
		},
		// A4.2a: every /v1 route is gated by the single route permission matrix
		// before its handler runs. The startup check below refuses to serve when a
		// registered route is missing from the matrix.
		Access:        product.access,
		AuditRelation: product.access.AuditRelation,
		Partials:      harnessapi.PartialSource(product.partials),
		// The selection vocabulary is read from the CURRENT generation at call
		// time: a settings write installs a new model service, and the input edge
		// must accept exactly what that generation can run. A runtime without
		// provider configuration still refuses every selection.
		//
		// The execution binding (botId/teamBotId) is part of that same gate: the
		// submit path resolves it exactly as an attempt would (existence, archive,
		// seal) and merges the agent's default model into a fact that named none,
		// so "what the edge accepts" stays "what the next attempt runs".
		ValidateSelection: selectFromCurrentGeneration(rt),
	})
	// A4.2a route permission coverage: enumerate both muxes (the runtime edge
	// and the nested product API) plus the A2 auth surface and refuse to start
	// when a /v1 route has no declaration. The gate itself is wired above
	// (Config.Access); this check is what makes a forgotten route a boot error
	// instead of a silently unscoped handler.
	if !api.AccessWired() {
		return errors.New("route permission gate is not wired: refusing to serve /v1 without authorization")
	}
	routePatterns := append(api.Routes(), product.api.Routes()...)
	routePatterns = append(routePatterns, accessgate.AuthRoutes()...)
	if err := accessgate.CheckRoutes(routePatterns); err != nil {
		return fmt.Errorf("route permission matrix: %w", err)
	}
	log.Info("route permission matrix verified", "rules", accessgate.RuleCount(), "routes", len(routePatterns))

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
		case strings.HasPrefix(r.URL.Path, "/plugins/"):
			// Only the namespace with an id is a plugin UI. The bare /plugins is
			// the web client's own route: a reload there must serve the SPA, not
			// the plugin handler's "a plugin id is required" JSON.
			pluginUI.ServeHTTP(w, r)
		default:
			static.ServeHTTP(w, r)
		}
	})
	// The quota middleware sits inside the auth handler, after the principal is
	// known: it needs the resolved X-Tenant-ID to charge the right tenant. Bot
	// keys and cookie sessions are both resolved by the same auth controller, so
	// there is exactly one place that decides "who is calling".
	quotaEdge := &edgeQuotaMiddleware{Edge: edge, Next: dispatch}
	var handler http.Handler = product.auth.wrap(quotaEdge)

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
	if err := validateListener(opts.AuthMode, ln); err != nil {
		_ = ln.Close()
		return err
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
	hooks     *harnessext.HookBus
	decisions *harnessext.DecisionBroker
	partials  *harnessext.PartialHub
	runCtrl   *harnessext.RunControl
	starter   *harnessext.TurnStarter
	subagents *harnessext.SubagentRunner
	// collabModes is the deployment's collaboration mode catalogue. It is
	// read-only at this layer; mode registration happens during assembly.
	collabModes *collab.ModeRegistry
	triggers    *harnessext.TriggerEngine
	plugins     *harnessext.Runtime
	// pluginManager is the lifecycle authority over plugins; api serves it and
	// the runtime is only ever told what to load through it.
	pluginManager *harnessext.Manager
	// notifications and taskLog are the recovery supervisor's owner/parent
	// rungs. They stay product-layer services; the supervisor receives narrow
	// adapters.
	notifications *notify.Service
	taskLog       *tasklog.Service
	api           *harnessapi.Server
	auth          *authController
	// access is the product authorization bridge shared by the identity routes,
	// the bot invoke/start seams and the generic session-owner callback.
	access *accesswire.Bridge

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
// The tree hash travels as its prefixed string form because harnessext treats it
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

// filePreviewReader adapts the file service to the panel's preview seam. The
// preview is a read, so it never touches the ledger or the manifest; a nil
// service answers the route as not wired.
type filePreviewReader struct{ service *fileplugin.Service }

func (f filePreviewReader) ReadEntry(ctx context.Context, scope fileplugin.Scope, path string, offset, limit int) (fileplugin.ReadResult, error) {
	if f.service == nil {
		return fileplugin.ReadResult{}, fileplugin.Errorf(fileplugin.CodeUnavailable, "the file service is not wired")
	}
	return f.service.ReadEntry(ctx, scope, path, offset, limit)
}

// filePreviewFor keeps the "not wired" state honest: a typed nil service must
// not become a non-nil interface value, or the capability probe would advertise
// session.filePreview and the route would answer 500 instead of 404.
func filePreviewFor(service *fileplugin.Service) harnessapi.SessionFileReader {
	if service == nil {
		return nil
	}
	return filePreviewReader{service: service}
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

func (r runRequeuer) RequeueRun(ctx context.Context, tenantID, sessionID uuid.UUID) (harnessext.RequeueOutcome, error) {
	outcome, err := r.store.RequeueRun(ctx, tenantID, sessionID)
	return harnessext.RequeueOutcome{Status: outcome.Status, Attempts: outcome.Attempts}, err
}

// newProductLayer assembles the extension surface over an already-booted
// service runtime. secretBox (nil when KEEL_SECRET_KEY is unset) enables the
// settings secret store; secretReason explains its absence to an operator;
// theme is the host theme token document handed to plugin UIs through the
// bridge (nil when the web build has none).
func newProductLayer(ctx context.Context, st *store.Store, kern *kernel.Kernel, admission kernel.Committer, admissionGate *placement.Gate, rt *servicehost.Runtime, hooks *harnessext.HookBus, runControl *harnessext.RunControl, partials *harnessext.PartialHub, taskLog *tasklog.Service, opts serveOptions, secretBox *store.SecretBox, secretReason string, accessBridge *accesswire.Bridge, mcpApps *mcphost.Registry, mcpAppsSandboxOrigin string, theme json.RawMessage, edge *quota.Edge, maxUpload int64, mcpMetrics *mcphost.Metrics, mrtr *mcphost.MRTR, compactSession harnessapi.CompactSessionFunc, checkpointPolicies harnessapi.SessionCheckpointPolicyService, log *slog.Logger) (*productLayer, error) {
	sessions := storeSessions{Store: st}
	decisions := harnessext.NewDecisionBroker(harnessext.DecisionBrokerOptions{
		Store:  harnessext.NewTasksDecisionStore(st),
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
		Store: approval.NewPostgresStore(st),
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
	starter := harnessext.NewTurnStarter(sessions, admission, hooks, log)
	starter.Bindings = bindingRecorder
	var forkSnapshotReader harnessext.ForkSnapshotReader
	if opts.FileService != nil {
		forkSnapshotReader = forkSnapshots{manifest: opts.FileService.Manifest()}
	}
	var forkAdmissionOpt harnessext.ForkAdmission
	if admissionGate != nil {
		forkAdmissionOpt = forkAdmission{gate: admissionGate}
	}
	subagentMaxDepth := resolveSubagentMaxDepth(opts.SubagentMaxDepth)
	subagentMaxChildren := resolveSubagentMaxChildren(opts.SubagentMaxChildren)
	// The same bot.invoke checker serves the Host and the runner's defensive
	// spawnChild check, so a direct launcher call cannot authorize one policy
	// while the Host enforces another.
	subagentPermissions := collabPermissionChecker{store: st, access: accessBridge}
	subagents := harnessext.NewSubagentRunner(harnessext.SubagentOptions{
		Gate:    sessions,
		Edge:    admission,
		Events:  kern,
		Runs:    sessions,
		Control: runControl,
		Hooks:   hooks,
		Logger:  log,
		// The mode's depth setting lives in the request layer: -1/0 are
		// interpreted there, and the Host only enforces the explicit DepthPolicy.
		MaxDepth: &subagentMaxDepth,
		// The same direct-children cap reaches the descriptor (the Host's guard)
		// and the runner's defensive check.
		MaxChildren: subagentMaxChildren,
		// The spawn link projection (parent/depth/caps/budget) makes width
		// counting, restart recovery and a cold idempotent retry queryable; the
		// first user.message stays the semantic truth.
		Links: collabSubagentLinks{store: st},
		// Runner-side refusals and cancellations land in the same auth_events
		// stream as the Host's rows (ids/codes/numbers, no task text).
		Audit: collabAuditSink{store: st, log: log},
		BotInvoker: func(ctx context.Context, tenantID, parentSessionID uuid.UUID, botID string) error {
			return subagentPermissions.CheckBotInvoke(ctx, tenantID,
				collab.SessionRef{TenantID: tenantID, SessionID: parentSessionID}, botID)
		},
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
		// A spawned/forked child is a session resource: inherit the parent's
		// owner relation before the child's first input, so its routes authorize
		// exactly like the parent's. A spawn naming a member Bot owns the child
		// as that member (its Profile's Account), so the member's tools run under
		// the member's Account ∩ Harness, not the captain's.
		InheritOwnership: newSessionOwnershipInheritor(st),
		OwnSpawnedChild:  st.CreateSpawnedSession,
	})
	// The collaboration mode registry is completed before the product API is
	// constructed: duplicate ids and unknown disable targets must fail startup,
	// and the read-only catalogue is served from this same object. The guarded
	// collab.Host is then built over the same runner, so the `agent` tool and
	// the HTTP sub-agent routes share one enforced creation gate.
	collabModes, err := newCollabModes(opts.TenantID, subagentMaxDepth, subagentMaxChildren)
	if err != nil {
		return nil, fmt.Errorf("collaboration modes: %w", err)
	}
	if _, err := newCollabHost(opts.TenantID, collabModes, st, kern, accessBridge, subagents, log); err != nil {
		return nil, err
	}
	// Triggers fire as a Bot: the store-backed resolver validates the Bot
	// (existence/archive/tenant) at registration and before every fire, and
	// the ownership writer creates the session with its Bot/account/created-by
	// tuple in one transaction. Without these seams a fire is fail closed.
	triggerOwnership := triggerBotOwner{store: st}
	triggers := harnessext.NewTriggerEngine(harnessext.TriggerEngineOptions{
		Starter: starter,
		Hooks:   hooks,
		Logger:  log,
		// The durable cursor makes an interval/cron trigger replay what it
		// missed while the process was down; the task log creates the initial
		// card as part of a fire (task-log: 会话起步即有卡).
		State:     st,
		Tasks:     taskLog,
		Bots:      triggerOwnership,
		Ownership: triggerOwnership,
	})

	// The super-agent queue (change keel-collab-superagent) is assembled only
	// when KEEL_SUPERAGENT_SOURCE names a Source and both role Bots. It enables
	// the mode and starts the poller; with no configuration the descriptor stays
	// disabled exactly as the core change left it.
	superagentChanges := newSuperagentChangeSink()
	superagentRuntime, err := newSuperagentRuntime(ctx, opts.TenantID, st, collabModes, accessBridge, subagents, starter, superagentChanges.Publish, log)
	if err != nil {
		return nil, fmt.Errorf("super-agent: %w", err)
	}
	// The model-facing report/ask tools were captured at Boot; install the
	// orchestrator they call. Nil (disabled mode) leaves them answering the
	// typed unavailable refusal.
	opts.SuperagentWorkerTools.set(superagentRuntime)

	surface, err := newPluginSurface(ctx, rt, st, opts, hooks, triggers, decisions, approvals, notifications, subagents, partials, taskLog, tenantSecretLookup(st, secretBox, opts.TenantID, log), log)
	if err != nil {
		return nil, err
	}
	manager, toolIndex, plugins := surface.manager, surface.toolIndex, surface.runtime

	cancelFn := func(cancelCtx context.Context, tenantID, sessionID uuid.UUID) (harnessext.CancelResult, error) {
		owner, exists, err := st.SessionOwner(cancelCtx, tenantID, sessionID)
		if err != nil {
			return harnessext.CancelResult{}, err
		}
		if !exists {
			return harnessext.CancelResult{}, harnessapi.ErrSessionNotFound
		}
		if owner == store.StateOwnerExternal {
			return harnessext.CancelResult{Status: "idle", RunStatus: "external",
				Detail: "external sessions have no native run to cancel"}, harnessapi.ErrExternalSession
		}
		// One subtree entry for the whole stop: it cancels this session (a local
		// attempt or a durable queued row) and every descendant, persists the
		// cancelled state and audits each outcome. A child failure is reported,
		// never hidden.
		result, err := subagents.CancelSubtree(cancelCtx, tenantID, sessionID, "parent session cancelled")
		if err != nil {
			return result, err
		}
		if len(result.FailedIDs) > 0 {
			result.Detail = joinDetail(result.Detail, fmt.Sprintf("sub-agent tree cancel: %d cancelled, %d failed", len(result.CancelledIDs), len(result.FailedIDs)))
		} else if len(result.CancelledIDs) > 0 {
			result.Detail = joinDetail(result.Detail, fmt.Sprintf("sub-agent tree cancel: %d cancelled", len(result.CancelledIDs)))
		}
		return result, nil
	}

	// The settings/registry projection (GET /v1/providers, GET /v1/models) comes
	// from the same assembly as the selection vocabulary. It is read per request
	// off the live runtime (ModelsLive): a settings write installs a new model
	// service without a restart, and a generation without provider
	// configuration answers 501 exactly like a boot without a models document.
	modelSeam := modelRuntimeSeam{rt: rt}
	// The product identity surface shares one authorizer across HTTP, the Bot
	// invoke/start seams, whoami and the runtime tool surface. It reads grants
	// and relations from the same store the request path uses; there is no
	// second permission cache.
	if accessBridge == nil {
		return nil, errors.New("access bridge is required")
	}
	var secretStore harnessapi.SecretStore
	if secretBox != nil {
		secretStore = tenantSecretSeam{store: st, box: secretBox}
	}
	// The bounded transcript reads (W12 session-query) run over the same store
	// and tenant scope as the session list; they never become a second ledger.
	sessionQuery := harnessapi.NewPGSessionQuery(st)
	// A typed nil *superagent.Orchestrator in an interface would advertise the
	// board as wired; keep the interface nil unless the queue is assembled.
	var superagentService harnessapi.SuperagentService
	if superagentRuntime != nil {
		superagentService = superagentRuntime
	}
	api := harnessapi.New(harnessapi.Config{
		Sessions: harnessapi.NewPGSessionLister(st),
		// Manual compaction and the session policy surface (change
		// keel-checkpoint). Manual compaction runs the live runner under the
		// session's run lease; the policy service reads/writes the session
		// override and always narrows it against the current tenant policy.
		CompactSession:     compactSession,
		CheckpointPolicies: checkpointPolicies,
		// Identity/access surface (change keel-bot-identity-access, A4.1): the
		// same store and authorizer the auth middleware uses. A bot invoke/start
		// creates an owned session and commits the turn through the one starter.
		Identity:        st,
		Access:          accessBridge,
		StartBotSession: newBotSessionStarter(st, starter),
		// A Bot's Harness tool list is a whitelist of tools the deployment
		// actually serves. The lookup is resolved per request, so a plugin
		// install/uninstall moves what a new save may name without a restart.
		ToolExists: func(toolID string) bool {
			_, ok := rt.Assembly().Tools().Get(toolID)
			return ok
		},
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
		// Telemetry status and the enable question read the process hub and the
		// snapshotted tenant section; the question never renders header values.
		Telemetry: rt.TelemetryHub(),
		TelemetryApproval: func() []string {
			return telemetryApprovalLines(opts.TelemetryConfig)
		},
		Theme: theme,
		// Chat commands (change keel-plugin-commands): the durable invocation
		// state machine is the same store; plugin commands come from the live
		// manager registry and execute through Manager.InvokePluginTool (never
		// the UI bridge's index); skills are the tenant library. A command is
		// idle-only, so the idle seam reads the durable run row: queued/pending/
		// running means a turn is owed or in flight.
		CommandStore:   st,
		CommandPlugins: manager,
		CommandTools:   manager,
		CommandSkills:  opts.Skills,
		SessionIdle: func(ctx context.Context, tenantID, sessionID uuid.UUID) (bool, error) {
			run, err := st.GetRun(ctx, tenantID, sessionID)
			if errors.Is(err, store.ErrNoRun) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			switch run.Status {
			case store.RunQueued, store.RunPending, store.RunRunning:
				return false, nil
			default:
				return true, nil
			}
		},
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
		MCPAppInputCommit: mcpAppInputCommit(st, admission, edge, maxUpload),
		// P5.5: ui/update-model-context persists into mcp_app_contexts (never the
		// ledger); the runner reads the same table for the next attempt.
		MCPAppContexts: st,
		// P4: the dispatcher is wired by the MCP service (Events above), so the
		// events capability is honest for clients that gate on it.
		MCPEvents:     true,
		MCPAppMetrics: mcpMetrics,
		Triggers:      triggers,
		Subagents:     subagents,
		// The durable spawn-link projection: the registry is a warm index, this
		// is what GET /v1/subagents?rootSessionId= and the single-record read
		// answer after a restart.
		SubagentLinks: collabSubagentLinks{store: st},
		// The read-only collaboration catalogue (change keel-collab-core):
		// enabled and disabled modes alike, so a client can keep existing work
		// visible after a disable.
		CollabModes: collabModes.List,
		// The read-only super-agent ticket board / attention inbox. Nil keeps
		// both routes at 503 and superagent.board=false.
		Superagent: superagentService,
		Hooks:      hooks,
		ModelsLive: func() harnessapi.ModelCatalog {
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
		// The active config.toml file version/hash and rotation facts are a
		// capability fact. They describe the file and the capture policy source,
		// not the run's loadout-derived configHash.
		ConfigFileStatus: func() *harnessapi.ConfigFileCapabilities {
			if opts.ConfigLoader == nil {
				return nil
			}
			status := opts.ConfigLoader.Status()
			block := harnessapi.ConfigFileCapabilities{
				Exists:         status.Exists,
				Revision:       status.Revision,
				Hash:           status.Hash,
				AnchorEventSeq: status.AnchorEventSeq,
				CaptureMode:    status.CaptureMode,
				Rotations:      status.Rotations,
			}
			if fact := status.LastRotation; fact != nil {
				block.LastRotation = &harnessapi.ConfigFileRotation{
					Revision:         fact.Revision,
					Hash:             fact.Hash,
					AnchorEventSeq:   fact.AnchorEventSeq,
					PreviousRevision: fact.PreviousRevision,
					PreviousHash:     fact.PreviousHash,
					RotatedAt:        fact.RotatedAt.Format(time.RFC3339Nano),
				}
			}
			return &block
		},
		// The skill settings surface and the session-local completion sources:
		// both read services the tools already use, never a second copy.
		Skills:       opts.Skills,
		SessionFiles: opts.FileService,
		SessionTasks: taskLog,
		// Bounded transcript search/export (W12 `keel.session-query`). The
		// handler filters every candidate through the same access relation the
		// session list uses.
		SessionSearch: sessionQuery,
		SessionExport: sessionQuery,
		// The run surface's task panel (W4): the task-log projection read and
		// the file manifest's bounded read behind the resource preview. Both
		// are the services the tools already use; neither writes task state.
		TaskPanel:   taskLog,
		FilePreview: filePreviewFor(opts.FileService),
		// The external-run projection (change keel-oar-renderer): the session's
		// external marks come from the same store the rest of the request path
		// reads, and the journal directory is the deployment's OAR journal
		// (-oar-journal-dir > KEEL_OAR_JOURNAL_DIR, resolved in main.go). Empty
		// keeps the route registered and answering 501 oar_not_wired.
		ExternalSessions: st,
		OARJournalDir:    opts.OARJournalDir,
		// Tenancy surface (change keel-ga §2): the operator provisioning path and
		// the tenant's own quota projection. Both read the same store the request
		// path uses; neither is reachable with a tenant credential alone.
		Provisioner: tenantProvisioner{store: st},
		Quota:       quotaReporter{store: st, edge: edge},
		AdminKey:    adminKeyFromEnv,
		RunStatus: func(tenantID, sessionID uuid.UUID) (string, bool) {
			return runControl.Status(kernel.Scope{TenantID: tenantID, SessionID: sessionID})
		},
	})
	// A lifecycle write (API or creation tool) invalidates the tenant's plugin
	// inventory over the same SSE tail the identity pages use; the client
	// re-reads GET /v1/plugins, never a row from the event.
	manager.SetChangeSink(api.PublishPluginChange)
	// A successful super-agent ticket/Attention write invalidates the board and
	// Attention inbox over the same SSE tail; the client re-reads the board,
	// never a row from the event.
	superagentChanges.set(api)
	auth, err := newAuthController(authConfig{
		Mode:       opts.AuthMode,
		Store:      st,
		TenantID:   opts.TenantID,
		SecretKey:  opts.SecretKey,
		LocalAdmin: opts.LocalAdmin,
	}, log)
	if err != nil {
		return nil, err
	}

	layer := &productLayer{
		hooks:         hooks,
		decisions:     decisions,
		partials:      partials,
		runCtrl:       runControl,
		starter:       starter,
		subagents:     subagents,
		collabModes:   collabModes,
		triggers:      triggers,
		plugins:       plugins,
		pluginManager: manager,
		notifications: notifications,
		taskLog:       taskLog,
		api:           api,
		auth:          auth,
		access:        accessBridge,
	}
	layer.closeFns = []func(context.Context){
		func(ctx context.Context) { plugins.Close(ctx) },
		func(context.Context) { triggers.Close() },
		func(context.Context) { decisions.Close() },
		func(context.Context) { hooks.Close() },
	}
	return layer, nil
}

// newBotSessionStarter is the bot invoke/team start seam. It writes the session
// ownership (bot → account, initiator, optional team mission) before committing
// the turn, so a new session is private from its first row. The turn itself goes
// through the same TurnStarter triggers and sub-agents use.
func newBotSessionStarter(st *store.Store, starter *harnessext.TurnStarter) func(context.Context, harnessapi.BotSessionRequest) (harnessapi.BotSessionResult, error) {
	return func(ctx context.Context, request harnessapi.BotSessionRequest) (harnessapi.BotSessionResult, error) {
		if st == nil || starter == nil {
			return harnessapi.BotSessionResult{}, errors.New("bot session starter is not configured")
		}
		sessionID := request.SessionID
		if sessionID == uuid.Nil {
			sessionID = uuid.New()
		}
		if err := st.CreateOwnedSession(ctx, store.SessionOwnership{
			TenantID: request.TenantID, SessionID: sessionID, BotID: request.BotID,
			AccountID: request.AccountID, CreatedByAccountID: request.ActorAccountID,
			TeamBotID: request.TeamBotID,
		}); err != nil {
			return harnessapi.BotSessionResult{}, err
		}
		var extras map[string]any
		if request.TeamBotID != "" {
			// A team start names the TeamBot in the input's selection fact: the
			// attempt resolves the captain/mission/roster from it, and the session
			// ownership below records the same team as the mission snapshot.
			extras = map[string]any{
				"selection": map[string]string{"teamBotId": request.TeamBotID},
				"teamBotId": request.TeamBotID,
			}
			if request.Mission != "" {
				extras["teamMission"] = request.Mission
			}
		} else {
			// A bot invoke names the Bot in the selection fact so the attempt uses
			// that Bot's Role/Harness prompt, tool whitelist and default model.
			extras = map[string]any{
				"selection": map[string]string{"botId": request.BotID},
			}
		}
		result, err := starter.StartTurn(ctx, harnessext.StartTurnRequest{
			TenantID: request.TenantID, SessionID: sessionID,
			Text: request.Text, RequestID: request.RequestID, Extras: extras,
		})
		if err != nil {
			return harnessapi.BotSessionResult{}, err
		}
		return harnessapi.BotSessionResult{
			SessionID: result.SessionID, Seq: result.Seq,
			RequestID: result.RequestID, Deduped: result.Deduped,
		}, nil
	}
}

// pluginSurface is the product's plugin layer: the lifecycle authority, the
// tool index the UI bridge shares with the model registry, and the runtime the
// api reads snapshots from.
type pluginSurface struct {
	manager   *harnessext.Manager
	toolIndex *productToolIndex
	runtime   *harnessext.Runtime
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
func newPluginSurface(ctx context.Context, rt *servicehost.Runtime, persistence harnessext.PluginPersistence, opts serveOptions, hooks *harnessext.HookBus, triggers *harnessext.TriggerEngine, decisions *harnessext.DecisionBroker, approvals *approval.Service, notifications *notify.Service, subagents *harnessext.SubagentRunner, partials *harnessext.PartialHub, tasks *tasklog.Service, lookupSecret func(string) (string, bool), log *slog.Logger) (*pluginSurface, error) {
	builtinManifests, err := builtin.Manifests()
	if err != nil {
		return nil, fmt.Errorf("builtin plugin manifests: %w", err)
	}
	// Creation mode drives the plugin manager itself, so its factory resolves the
	// manager lazily: the manager is built with (and after) this builtin set.
	var pluginManager *harnessext.Manager
	builtinFactories := map[string]func() *harnessext.Plugin{
		harnessext.ExamplePluginID: func() *harnessext.Plugin {
			return harnessext.NewExamplePlugin(harnessext.ExamplePluginOptions{
				TenantID: opts.TenantID.String(),
				Interval: opts.ExampleInterval,
				// A signed webhook delivery proves its origin with this key. The
				// value is resolved from the environment at registration; without
				// it the trigger stays fail-closed for signed deliveries and the
				// manual fire route remains available.
				WebhookSecretEnv: "KEEL_EXAMPLE_WEBHOOK_SECRET",
				BotID:            opts.ExampleBotID,
			})
		},
		testkit.PluginID: func() *harnessext.Plugin { return harnessext.NewTestkitPlugin() },
		harnessext.FilePluginID: func() *harnessext.Plugin {
			return harnessext.NewFilePlugin(harnessext.FilePluginOptions{Service: opts.FileService})
		},
		harnessext.ModelPluginID:   func() *harnessext.Plugin { return harnessext.NewModelPlugin() },
		harnessext.BotPluginID:     func() *harnessext.Plugin { return harnessext.NewBotPlugin() },
		harnessext.TeamBotPluginID: func() *harnessext.Plugin { return harnessext.NewTeamBotPlugin() },
		harnessext.SpillPluginID:   func() *harnessext.Plugin { return harnessext.NewSpillPlugin() },
		harnessext.CreationPluginID: func() *harnessext.Plugin {
			return harnessext.NewCreationPlugin(harnessext.CreationOptions{
				Manager:  func() *harnessext.Manager { return pluginManager },
				TenantID: opts.TenantID,
			})
		},
		harnessext.TasksPluginID: func() *harnessext.Plugin {
			return harnessext.NewTasksPlugin(harnessext.TasksOptions{Service: tasks})
		},
		harnessext.ShellPluginID: func() *harnessext.Plugin {
			return harnessext.NewShellPlugin(harnessext.ShellOptions{
				Service:  func() *jobqueue.Manager { return opts.Bash },
				TenantID: opts.TenantID,
				Checkpoint: func(ctx context.Context) (map[string]any, error) {
					return checkpointDocument(ctx, opts.Workspace)
				},
			})
		},
		harnessext.ApprovalPluginID: func() *harnessext.Plugin {
			return harnessext.NewApprovalPlugin(harnessext.ApprovalOptions{
				Decisions:  decisions,
				Provenance: approvals,
				Logger:     log,
			})
		},
		harnessext.NotifyPluginID: func() *harnessext.Plugin {
			return harnessext.NewNotifyPlugin(harnessext.NotifyOptions{
				Service: notifications,
				Logger:  log,
			})
		},
		harnessext.TelemetryPluginID: func() *harnessext.Plugin {
			return harnessext.NewTelemetryPlugin(harnessext.TelemetryPluginOptions{
				TenantID:     opts.TenantID.String(),
				Config:       opts.TelemetryConfig,
				LookupSecret: lookupSecret,
				Logger:       log,
			})
		},
	}
	builtins := make([]harnessext.Builtin, 0, len(builtinManifests))
	for _, manifest := range builtinManifests {
		if manifest.ID == harnessext.ExamplePluginID && strings.TrimSpace(opts.ExampleBotID) == "" {
			// The example factory declares its triggers only when a Bot is
			// configured (an unowned trigger cannot register). Keep the
			// advertised manifest consistent with what actually loads.
			manifest.Provides.Triggers = nil
		}
		factory, ok := builtinFactories[manifest.ID]
		if !ok {
			return nil, fmt.Errorf("no built-in plugin is registered for the manifest %s", manifest.ID)
		}
		defaultEnabled := manifest.ID != testkit.PluginID && manifest.ID != harnessext.TelemetryPluginID
		builtins = append(builtins, harnessext.Builtin{Manifest: manifest, Factory: factory, DefaultEnabled: defaultEnabled})
	}
	// The tool index mirrors the dynamic tool set the plugin runtime publishes:
	// the plugin UI bridge calls a plugin's own tools through it, so a bridge call
	// executes exactly what the model would execute.
	toolIndex := newProductToolIndex()
	auditSink := func(ctx context.Context, pluginID, version, action, result string, detail map[string]any) {
		if pluginManager == nil {
			return
		}
		tenantID := opts.TenantID
		pluginManager.RecordAudit(ctx, harnessext.AuditContext{TenantID: &tenantID, Actor: "system:plugin-start"},
			pluginID, version, action, result, detail)
	}
	plugins := newPluginRuntime(rt, toolIndex, hooks, triggers, decisions, subagents, partials, auditSink, log)
	manager, err := harnessext.NewPluginManager(harnessext.ManagerOptions{
		// Plugin install/state/audit is tenant-scoped (keel-creation). `keel serve`
		// is a single-tenant process, so this is the one tenant and the only code
		// path.
		TenantID:    opts.TenantID,
		Root:        opts.PluginsRoot,
		Runtime:     plugins,
		Persistence: persistence,
		Builtins:    builtins,
		// The sidecar loader is the one loading strategy this build ships for
		// non-compiled kinds; the manager installs its denial sink so a sidecar's
		// permission refusals land in the audit trail (spec P3).
		Loaders: map[pluginpkg.Kind]harnessext.PluginLoader{
			pluginpkg.KindSidecar: harnessext.NewSidecarLoader(harnessext.SidecarOptions{
				KeelVersion: version,
				Logger:      log,
			}),
		},
		KeelVersion: version,
		Logger:      log,
		// Telemetry lifecycle rows carry the exporter/signals/endpoint/sampling
		// projection; header values and payloads never enter audit.
		AuditDetail: func(pluginID string) map[string]any {
			hub := rt.TelemetryHub()
			if pluginID != harnessext.TelemetryPluginID || hub == nil {
				return nil
			}
			status := hub.Status()
			detail := map[string]any{
				"telemetryState":   string(status.State),
				"adapter":          status.Config.Adapter,
				"signals":          telemetrySignalStrings(status.Signals),
				"endpointHost":     status.EndpointHost,
				"sampleRatio":      status.SampleRatio,
				"logRatio":         status.LogRatio,
				"headerNames":      status.HeaderNames,
				"headerCount":      status.HeaderCount,
				"queueDepth":       status.QueueDepth,
				"dropped":          status.Dropped,
				"exported":         status.Exported,
				"lastErrorClass":   status.LastErrorClass,
				"metricSeriesDrop": status.MetricSeriesDropped,
			}
			if status.Reason != "" {
				detail["reason"] = status.Reason
			}
			return detail
		},
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
func newPluginRuntime(rt *servicehost.Runtime, toolIndex *productToolIndex, hooks *harnessext.HookBus, triggers *harnessext.TriggerEngine, decisions *harnessext.DecisionBroker, subagents *harnessext.SubagentRunner, partials *harnessext.PartialHub, audit func(ctx context.Context, pluginID, version, action, result string, detail map[string]any), log *slog.Logger) *harnessext.Runtime {
	return harnessext.NewRuntime(harnessext.RuntimeOptions{
		Hooks:     hooks,
		Triggers:  triggers,
		Decisions: decisions,
		Subagents: subagents,
		Partials:  partials,
		// The telemetry hub is the same process object the run path uses; a
		// plugin registering an exporter later is exactly the late-registration
		// seam the hub exists for.
		Telemetry: rt.TelemetryHub(),
		Audit:     audit,
		Tools: func(tools []agentcore.AgentTool) error {
			toolIndex.publish(tools)
			return rt.RefreshProductTools(tools)
		},
		Prompt: func(sections []harnessext.PromptSection) error {
			return rt.RefreshProductPrompt(pluginPromptSections(sections))
		},
	})
}

func telemetrySignalStrings(signals []telemetry.Signal) []string {
	out := make([]string, 0, len(signals))
	for _, signal := range signals {
		out = append(out, string(signal))
	}
	return out
}

// telemetryApprovalLines renders the sanitized telemetry configuration for the
// keel.telemetry enable question: exporter, signals, endpoint host, sampling and
// header names/count. It never renders a header value or a secret reference.
func telemetryApprovalLines(config telemetry.Config) []string {
	summary := telemetry.SummarizeConfig(config)
	lines := []string{
		"exporter=" + summary.Adapter,
		"signals=" + strings.Join(telemetrySignalStrings(summary.Signals), ","),
		"sampleRatio=" + telemetry.FormatRatio(summary.SampleRatio),
		"logRatio=" + telemetry.FormatRatio(summary.LogRatio),
	}
	if summary.EndpointHost != "" {
		lines = append(lines, "endpointHost="+summary.EndpointHost)
	}
	if len(summary.HeaderNames) > 0 {
		lines = append(lines, "headers="+strings.Join(summary.HeaderNames, ","))
	}
	if !summary.Valid && summary.Reason != "" {
		lines = append(lines, "configProblem="+summary.Reason)
	}
	return lines
}

// pluginPromptSections projects the plugin runtime's published prompt sections
// onto the host assembly's section type. Section names carry over verbatim: the
// name is what the model sees (<name>...</name>), and the runtime has already
// dropped empty contributions.
func pluginPromptSections(sections []harnessext.PromptSection) []host.Section {
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
// that knows both sides: harnessapi must not depend on servicehost.
type modelRuntimeSeam struct {
	rt *servicehost.Runtime
}

// ProviderViews implements harnessapi.ModelCatalog.
func (m modelRuntimeSeam) ProviderViews() []modelcfg.ProviderView {
	if service := m.rt.Models(); service != nil {
		return service.ProviderViews()
	}
	return nil
}

// ModelViews implements harnessapi.ModelCatalog.
func (m modelRuntimeSeam) ModelViews() []modelcfg.ModelView {
	if service := m.rt.Models(); service != nil {
		return service.ModelViews()
	}
	return nil
}

// ResolutionIssues implements harnessapi.ModelCatalog.
func (m modelRuntimeSeam) ResolutionIssues() []modelcfg.Issue {
	if service := m.rt.Models(); service != nil {
		return service.ResolutionIssues()
	}
	return nil
}

// DefaultSelection implements harnessapi.ModelCatalog.
func (m modelRuntimeSeam) DefaultSelection() modelcfg.Selection {
	if service := m.rt.Models(); service != nil {
		return service.DefaultSelection()
	}
	return modelcfg.Selection{}
}

// ProbeProvider implements harnessapi.ProviderProber.
func (m modelRuntimeSeam) ProbeProvider(ctx context.Context, providerID string) (modelcfg.ProviderProbe, bool, error) {
	service := m.rt.Models()
	if service == nil {
		return modelcfg.ProviderProbe{}, false, nil
	}
	return service.ProbeProvider(ctx, providerID)
}

// DeploymentModels implements harnessapi.ModelSettingsRuntime.
func (m modelRuntimeSeam) DeploymentModels() json.RawMessage { return m.rt.DeploymentModels() }

// RejectedModelsOverride implements harnessapi.ModelSettingsRuntime.
func (m modelRuntimeSeam) RejectedModelsOverride() (int64, string, bool) {
	rejected, ok := m.rt.RejectedModelsOverride()
	if !ok {
		return 0, "", false
	}
	return rejected.Revision, rejected.Reason, true
}

// ValidateModelsDocument implements harnessapi.ModelSettingsRuntime.
func (m modelRuntimeSeam) ValidateModelsDocument(document json.RawMessage) (harnessapi.ModelCandidate, []modelcfg.Issue, error) {
	candidate, issues, err := m.rt.ValidateModelsDocument(document)
	if err != nil || len(issues) > 0 {
		return harnessapi.ModelCandidate{}, issues, err
	}
	return harnessapi.ModelCandidate{
		Selection:           candidate.Selection,
		ProvidersExecutable: candidate.ProvidersExecutable,
		ModelsSelectable:    candidate.ModelsSelectable,
		ProviderIDs:         candidate.ProviderIDs,
	}, nil, nil
}

// ApplyModels implements harnessapi.ModelSettingsRuntime.
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

// selectFromCurrentGeneration is the input edge's selection validator: it
// validates against the model service the runtime is serving right now, so a
// settings write changes what the next input may ask for without a restart. A
// runtime without provider configuration refuses every selection, exactly like
// a boot without a models document always did.
//
// It delegates to the runtime's own gate so the execution binding (botId/
// teamBotId) travels the same path: resolving it here as well would be a second
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
// harnessapi.SecretStore. The value crosses this boundary exactly once, on the
// way in; no method here returns one to a caller.
type tenantSecretSeam struct {
	store *store.Store
	box   *store.SecretBox
}

func (s tenantSecretSeam) List(ctx context.Context, tenantID uuid.UUID) ([]harnessapi.SecretMeta, error) {
	metas, err := s.store.ListTenantSecrets(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]harnessapi.SecretMeta, 0, len(metas))
	for _, meta := range metas {
		out = append(out, harnessapi.SecretMeta{
			Name:      meta.Name,
			Preview:   meta.Preview,
			CreatedAt: meta.CreatedAt,
			UpdatedAt: meta.UpdatedAt,
		})
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

// RunState implements harnessext.RunStatusReader.
// CancelQueuedRun durably stops a queued/pending run for RunControl (the
// session cancel path uses CancelSubtree, which reaches this through the
// control seam).
func (s storeSessions) CancelQueuedRun(ctx context.Context, tenantID, sessionID uuid.UUID, reason string) (bool, string, error) {
	return s.Store.CancelQueuedRun(ctx, tenantID, sessionID, reason)
}

func (s storeSessions) RunState(ctx context.Context, tenantID, sessionID uuid.UUID) (harnessext.RunState, error) {
	run, err := s.Store.GetRun(ctx, tenantID, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNoRun) {
			return harnessext.RunState{}, harnessext.ErrNoRun
		}
		return harnessext.RunState{}, err
	}
	return harnessext.RunState{Status: run.Status, Attempts: run.Attempts, CursorSeq: run.CursorSeq, LastError: run.LastError}, nil
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

// joinDetail appends one cancellation detail without losing the earlier one.
func joinDetail(existing, added string) string {
	existing = strings.TrimSpace(existing)
	added = strings.TrimSpace(added)
	switch {
	case existing == "":
		return added
	case added == "":
		return existing
	default:
		return existing + "; " + added
	}
}
