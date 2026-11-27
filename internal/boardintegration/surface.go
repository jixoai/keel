package boardintegration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jixoai/keel/internal/accesswire"
	keelstore "github.com/jixoai/keel/internal/store"

	boardaudit "github.com/jixoai/keel-board/audit"
	"github.com/jixoai/keel-board/automation"
	boardblob "github.com/jixoai/keel-board/blob"
	boardlog "github.com/jixoai/keel-board/log"
	boardmetrics "github.com/jixoai/keel-board/metrics"
	boardserver "github.com/jixoai/keel-board/server"
	boardstore "github.com/jixoai/keel-board/store"
	boardsync "github.com/jixoai/keel-board/sync"
	boardsyncstore "github.com/jixoai/keel-board/syncstore"
)

// Options configure the keel-owned board surface.
type Options struct {
	// DSN is the PostgreSQL database the board's own migrations and RLS use.
	// Production points it at the keel database (or a dedicated database/role);
	// tests point it at the test database with Schema set for isolation.
	DSN string
	// Schema confines board tables to one PostgreSQL schema. Empty uses the
	// database default (normally public).
	Schema string
	// Access is the edge-resolved authorization bridge. Required when enabled.
	Access *accesswire.Bridge
	// Store is keel's identity store, used for bot display names and the
	// channel-owner relation writes. Defaults to Access.Store.
	Store *keelstore.Store
	// TenantID is the deployment tenant a platform webhook belongs to. Sync
	// webhooks carry no board session; the deployment tenant plus the entry's
	// provider signature are the authority. Zero refuses webhooks.
	TenantID uuid.UUID
	// Logger receives operational failures. Nil uses slog.Default().
	Logger *slog.Logger
	// Heartbeat is the SSE keepalive interval. Zero uses 15s; tests may lower
	// it.
	Heartbeat time.Duration
	// StreamLifetime bounds one SSE connection (the client reconnects with
	// Last-Event-ID). Zero uses 55s; a negative value disables the rollover.
	StreamLifetime time.Duration
	// ChannelScanInterval bounds how often the owner reconciler repairs
	// channels whose creating request did not record its owner relation. Zero
	// uses 5 minutes; negative disables the periodic scan (the startup scan
	// still runs).
	ChannelScanInterval time.Duration
	// SyncRegistry is a deployment/test-provided registry. Nil loads the
	// tenant's board_tenant_sync_entries configuration (the production path);
	// a non-nil registry is used as-is and no entry document is loaded.
	SyncRegistry *boardsync.Registry
	// BoardMetrics is an existing board metric registry. Nil creates one.
	BoardMetrics *boardmetrics.Registry
	// SecretBox decrypts tenant secrets for sync providers. Nil means the
	// deployment has no KEEL_SECRET_KEY: entry credentials report missing and
	// nothing is ever read from plaintext.
	SecretBox *keelstore.SecretBox
	// BlobDir is the local CAS root keel's blob store uses. Empty keeps blobs
	// in PostgreSQL chunks.
	BlobDir string
	// FileHosts is the inbound file/link URL host allowlist. Empty refuses
	// every URL, which is the safe default.
	FileHosts []string
	// TenantFlagsTTL bounds how long a tenant switch read is cached. Zero uses
	// 5s; a negative value disables the cache (tests).
	TenantFlagsTTL time.Duration
	// SyncStoreConfig bounds the board sync store's retry/lease policy. Zero
	// uses the board module's defaults; tests shorten the delays.
	SyncStoreConfig boardsyncstore.Config
	// MaxWebhookBody, MaxFileBytes, MaxFiles, MaxLinks and FileFetchTimeout
	// bound untrusted platform input. Zero keeps the board defaults.
	MaxWebhookBody   int64
	MaxFileBytes     int64
	MaxFiles         int
	MaxLinks         int
	FileFetchTimeout time.Duration
	// DisableAutomation omits the watcher/schedule consumers and their
	// management routes. The default is to mount both.
	DisableAutomation bool
}

// TextExporter is a Prometheus text exporter (keel's metrics.Registry and the
// test seam both satisfy it).
type TextExporter interface {
	WriteText(ctx context.Context, w io.Writer) error
}

// Surface owns the board's PostgreSQL pools, log, HTTP handlers and background
// repair loop. Close releases them in dependency order.
type Surface struct {
	handler       *handler
	webhook       *boardserver.Server
	board         *boardlog.Board
	store         *boardstore.Postgres
	syncdb        *boardsyncstore.Postgres
	audit         *boardaudit.Postgres
	automation    *automation.Postgres
	owners        *ChannelOwners
	metrics       *boardmetrics.Registry
	identity      *Identity
	authorizer    *Authorizer
	flags         *TenantFlags
	manager       *syncManager
	webhookTenant uuid.UUID
	stopAuto      func()
	cancel        context.CancelFunc
	done          chan struct{}
	closeOnce     sync.Once
	logger        *slog.Logger
}

// New builds the board surface. It runs the board's idempotent migrations
// (core, sync, audit) through the board's own store, then wires identity,
// authorization, audit and metrics into keel.
func New(ctx context.Context, opts Options) (*Surface, error) {
	if opts.DSN == "" {
		return nil, errors.New("boardintegration: a board PostgreSQL DSN is required")
	}
	if opts.Access == nil || opts.Access.Store == nil {
		return nil, errors.New("boardintegration: the edge access bridge is required")
	}
	if opts.Store == nil {
		opts.Store = opts.Access.Store
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Heartbeat == 0 {
		opts.Heartbeat = defaultHeartbeat
	}
	if opts.StreamLifetime == 0 {
		opts.StreamLifetime = defaultStreamLifetime
	}
	if opts.ChannelScanInterval == 0 {
		opts.ChannelScanInterval = defaultChannelScan
	}
	if opts.Schema != "" {
		if err := ensureSchema(ctx, opts.DSN, opts.Schema); err != nil {
			return nil, openError("create board schema", err, opts.DSN)
		}
	}

	store, err := boardstore.OpenPostgres(ctx, boardstore.PostgresOptions{DSN: opts.DSN, SearchPath: opts.Schema})
	if err != nil {
		return nil, openError("open board store", err, opts.DSN)
	}
	board := boardlog.New(boardlog.Options{Store: store})

	syncdb, err := boardsyncstore.OpenPostgres(ctx, boardsyncstore.PostgresOptions{
		DSN: opts.DSN, SearchPath: opts.Schema, Config: opts.SyncStoreConfig,
	})
	if err != nil {
		store.Close()
		return nil, openError("open sync store", err, opts.DSN)
	}
	auditStore, err := boardaudit.OpenPostgres(ctx, boardaudit.PostgresOptions{DSN: opts.DSN, SearchPath: opts.Schema})
	if err != nil {
		syncdb.Close()
		store.Close()
		return nil, openError("open board audit", err, opts.DSN)
	}
	var automationStore *automation.Postgres
	if !opts.DisableAutomation {
		automationStore, err = automation.OpenPostgres(ctx, automation.PostgresOptions{DSN: opts.DSN, SearchPath: opts.Schema})
		if err != nil {
			auditStore.Close()
			syncdb.Close()
			store.Close()
			return nil, openError("open automation store", err, opts.DSN)
		}
	}

	authorizer, err := NewAuthorizer(opts.Access, nil, board, opts.Logger)
	if err != nil {
		if automationStore != nil {
			automationStore.Close()
		}
		auditStore.Close()
		syncdb.Close()
		store.Close()
		return nil, err
	}
	identity := &Identity{Store: opts.Store}

	metricsRegistry := opts.BoardMetrics
	if metricsRegistry == nil {
		metricsRegistry = boardmetrics.NewRegistry()
	}
	flags := NewTenantFlags(opts.Store, opts.TenantFlagsTTL)
	// The deployment tenant's switches decide whether this process assembles
	// platform connections. A read failure is not fatal for the board core:
	// the per-request gate will answer 503 until the document is fixed.
	bootFlags, flagErr := flags.Get(ctx, opts.TenantID)
	if flagErr != nil {
		opts.Logger.Warn("board tenant flags could not be read at boot; sync stays off", "error", flagErr)
		bootFlags = BoardFlags{}
	}
	syncEnabled := bootFlags.FullyEnabled()
	var boot *syncBoot
	if opts.SyncRegistry != nil {
		// A caller-provided registry (tests / an embedding deployment) owns the
		// entry set; configuration loading is skipped deliberately.
		boot = &syncBoot{
			registry:      opts.SyncRegistry,
			startChannels: make(map[boardserver.SyncKey][]string),
			bridgeSecrets: make(map[boardsync.Key]boardsync.Secret),
			subscribed:    make(map[boardsync.Key]bool),
			statuses:      make(map[boardsync.Key]CredentialStatus),
		}
	} else {
		boot, err = loadSyncBoot(ctx, opts, syncEnabled, syncdb)
		if err != nil {
			if automationStore != nil {
				automationStore.Close()
			}
			auditStore.Close()
			syncdb.Close()
			store.Close()
			return nil, err
		}
	}
	deploymentTenant := opts.TenantID
	syncTenant := func(*http.Request) (string, error) {
		if deploymentTenant == uuid.Nil {
			return "", errors.New("boardintegration: no deployment tenant for webhooks")
		}
		return deploymentTenant.String(), nil
	}
	var blobs boardblob.Store
	if adapter := newKeelBlobs(opts.Store, opts.BlobDir); adapter != nil {
		blobs = adapter
	}
	serverOptions := boardserver.Options{
		Board:     board,
		Authorize: authorizer,
		Actor:     identity.Actor,
		Tenant:    identity.Tenant,
		Logger:    opts.Logger,
		Metrics:   metricsRegistry,
		Audit:     auditStore,
		Blobs:     blobs,
		Sync: &boardserver.SyncOptions{
			Store:            syncdb,
			Registry:         boot.registry,
			Tenant:           syncTenant,
			Owned:            boot.owned,
			StartChannels:    boot.startChannels,
			BridgeSecret:     bridgeSecretFunc(boot.bridgeSecrets),
			FileHosts:        append([]string(nil), opts.FileHosts...),
			MaxFileBytes:     opts.MaxFileBytes,
			MaxFiles:         opts.MaxFiles,
			MaxLinks:         opts.MaxLinks,
			FileFetchTimeout: opts.FileFetchTimeout,
			MaxWebhookBody:   opts.MaxWebhookBody,
			Logger:           opts.Logger,
		},
	}
	if automationStore != nil {
		actions, actionErr := automation.NewRegistryWith(automation.PostAction{})
		if actionErr != nil {
			if automationStore != nil {
				automationStore.Close()
			}
			auditStore.Close()
			syncdb.Close()
			store.Close()
			return nil, actionErr
		}
		serverOptions.Automation = &boardserver.AutomationOptions{
			Store: automationStore, Actions: actions, Logger: opts.Logger,
		}
	}
	server := boardserver.New(serverOptions)

	owners := &ChannelOwners{
		Store: opts.Store, Audit: opts.Access, Schema: opts.Schema, DSN: opts.DSN,
		Logger: opts.Logger,
	}
	manager := newSyncManager(boot, opts.Store, syncdb, auditStore, deploymentTenant, opts.SecretBox, opts.Logger, metricsRegistry)
	manager.Attach(server, deploymentTenant.String())
	runCtx, cancel := context.WithCancel(context.Background())
	admin := &syncAdmin{manager: manager, bridge: opts.Access, tenant: deploymentTenant, logger: opts.Logger}
	handler := &handler{
		next:      server,
		board:     board,
		owners:    owners,
		sse:       &sseHandler{next: server, heartbeat: opts.Heartbeat, lifetime: opts.StreamLifetime, logger: opts.Logger},
		syncAdmin: admin,
		logger:    opts.Logger,
	}
	surface := &Surface{
		handler:       handler,
		webhook:       server,
		board:         board,
		store:         store,
		syncdb:        syncdb,
		audit:         auditStore,
		automation:    automationStore,
		owners:        owners,
		metrics:       metricsRegistry,
		identity:      identity,
		authorizer:    authorizer,
		flags:         flags,
		manager:       manager,
		webhookTenant: deploymentTenant,
		cancel:        cancel,
		done:          make(chan struct{}),
		logger:        opts.Logger,
	}
	// Sync cursors are fixed before the listener accepts writes, then the
	// workers start. A failure here is a boot failure: half-running sync is
	// worse than not starting.
	if err := manager.Start(runCtx); err != nil {
		cancel()
		manager.Stop()
		if automationStore != nil {
			automationStore.Close()
		}
		auditStore.Close()
		syncdb.Close()
		store.Close()
		return nil, err
	}
	if automationStore != nil && deploymentTenant != uuid.Nil {
		workerAuthorizer := &WorkerAuthorizer{Bridge: opts.Access, Store: opts.Store, Decision: authorizer}
		stop, autoErr := startAutomationWorkers(runCtx, opts.Logger, automationStore, board, workerAuthorizer, deploymentTenant.String())
		if autoErr != nil {
			cancel()
			manager.Stop()
			if automationStore != nil {
				automationStore.Close()
			}
			auditStore.Close()
			syncdb.Close()
			store.Close()
			return nil, autoErr
		}
		surface.stopAuto = stop
	}
	go func() {
		defer close(surface.done)
		owners.Run(runCtx, opts.ChannelScanInterval)
	}()
	return surface, nil
}

// ServeHTTP implements the board API handler mounted at /v1/board. It applies
// the tenant switches before any board route: a disabled tenant sees 404
// board_disabled, and the sync/source half additionally requires
// board.sync.enabled. A request without an edge principal is passed to the
// board, whose authorizer answers 401.
func (s *Surface) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.checkBoardFlags(w, r) {
		return
	}
	s.handler.ServeHTTP(w, r)
}

// checkBoardFlags answers false when the request was already refused.
func (s *Surface) checkBoardFlags(w http.ResponseWriter, r *http.Request) bool {
	if s == nil || s.flags == nil {
		return true
	}
	tenantID, ok := requestTenant(r)
	if boardBridgePath(r.URL.Path) && s.webhookTenant != uuid.Nil {
		// The bridge route's HMAC is the authority and its board tenant is the
		// deployment tenant; a session principal must not choose the switches.
		tenantID, ok = s.webhookTenant, true
	}
	if !ok {
		return true
	}
	flags, err := s.flags.Get(r.Context(), tenantID)
	if err != nil {
		s.logger.Error("board tenant flags read failed", "tenant", tenantID, "error", err)
		writeAdminError(w, http.StatusServiceUnavailable, "unavailable", "board availability is unavailable")
		return false
	}
	if !flags.Enabled {
		writeDisabled(w, "the board is disabled in this deployment", "board_disabled")
		return false
	}
	if boardSyncPath(r.URL.Path) && !flags.Sync {
		syncDisabledHandler().ServeHTTP(w, r)
		return false
	}
	return true
}

// Webhook serves the platform webhook namespace. It is mounted outside the
// board's session authentication; each provider verifies its own signature. The
// deployment tenant's switches gate the namespace before a byte of the body is
// read, so a disabled board or sync has no side effect. The guard also keeps
// the handler's other routes (/v1/board, /metrics) unreachable even if a proxy
// hands it a path outside the namespace.
func (s *Surface) Webhook() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/webhooks/") {
			DisabledHandler().ServeHTTP(w, r)
			return
		}
		if !s.checkWebhookFlags(w, r) {
			return
		}
		s.webhook.ServeHTTP(w, r)
	})
}

func (s *Surface) checkWebhookFlags(w http.ResponseWriter, r *http.Request) bool {
	if s == nil || s.flags == nil || s.webhookTenant == uuid.Nil {
		return true
	}
	flags, err := s.flags.Get(r.Context(), s.webhookTenant)
	if err != nil {
		s.logger.Error("board webhook tenant flags read failed", "tenant", s.webhookTenant, "error", err)
		writeAdminError(w, http.StatusServiceUnavailable, "unavailable", "board availability is unavailable")
		return false
	}
	if !flags.Enabled {
		writeDisabled(w, "the board is disabled in this deployment", "board_disabled")
		return false
	}
	if !flags.Sync {
		syncDisabledHandler().ServeHTTP(w, r)
		return false
	}
	return true
}

// Metrics returns the board metric registry.
func (s *Surface) Metrics() *boardmetrics.Registry { return s.metrics }

// Board returns the board log the surface serves. It is the write seam the
// superagent mirror needs (change keel-board-v1, 11.2): the adapter appends
// through the board's own log API and projection, never raw SQL against
// board_events. Callers outside this package must treat it as the board's API,
// including its locking and idempotency rules.
func (s *Surface) Board() *boardlog.Board { return s.board }

// Identity returns the board's keel principal resolver. The superagent command
// bridge reuses it; a second resolver would be a second authentication path.
func (s *Surface) Identity() *Identity { return s.identity }

// Authorizer returns the board's keel authorizer. The command bridge asks it
// before routing a mirrored task action to the supervisor.
func (s *Surface) Authorizer() *Authorizer { return s.authorizer }

// SyncStore returns the durable sync store. The tenant entry manager writes
// the board module's local entry rows through it.
func (s *Surface) SyncStore() *boardsyncstore.Postgres { return s.syncdb }

// AuditStore returns the board audit store (operational audit; the domain
// audit is board_events).
func (s *Surface) AuditStore() *boardaudit.Postgres { return s.audit }

// Expo forms one Prometheus exporter over keel's registry followed by the
// board's. Both use disjoint family names (keel_* and board_*), so the
// concatenation is a valid exposition.
func (s *Surface) Expo(base TextExporter) TextExporter {
	return &combinedExporter{base: base, board: s.metrics}
}

// Close stops the reconciler, workers and automation consumers, then releases
// every pool.
func (s *Surface) Close() {
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.manager != nil {
			s.manager.Stop()
		}
		if s.stopAuto != nil {
			s.stopAuto()
		}
		if s.done != nil {
			<-s.done
		}
		if s.automation != nil {
			s.automation.Close()
		}
		if s.audit != nil {
			s.audit.Close()
		}
		if s.syncdb != nil {
			s.syncdb.Close()
		}
		if s.store != nil {
			s.store.Close()
		}
	})
}

// combinedExporter writes the deployment registry and then the board's.
type combinedExporter struct {
	base  TextExporter
	board *boardmetrics.Registry
}

func (e *combinedExporter) WriteText(ctx context.Context, w io.Writer) error {
	if e.base != nil {
		if err := e.base.WriteText(ctx, w); err != nil {
			return err
		}
	}
	if e.board == nil {
		return nil
	}
	return e.board.WritePrometheus(w)
}

// ensureSchema creates the board's schema when a deployment isolates it. The
// board's migrations do not create a schema; a missing one would otherwise fail
// inside the first migration with a less useful error.
func ensureSchema(ctx context.Context, dsn, schema string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize()); err != nil {
		return err
	}
	return nil
}

// openError keeps a database error's cause while removing the DSN and its
// password: pgx includes the connection string in parse failures, and the
// board's DSN is deployment configuration.
func openError(what string, err error, dsn string) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	if dsn != "" {
		text = strings.ReplaceAll(text, dsn, "[redacted-dsn]")
	}
	if parsed, parseErr := url.Parse(dsn); parseErr == nil && parsed.User != nil {
		if password, ok := parsed.User.Password(); ok && password != "" {
			text = strings.ReplaceAll(text, password, "[redacted]")
		}
	}
	return fmt.Errorf("boardintegration: %s: %s", what, text)
}

// RoleBypassesRLS reports whether the board DSN's current role is a superuser
// or has BYPASSRLS. Both bypass FORCE ROW LEVEL SECURITY; a deployment using
// one still works, but its board tables are not protected at the database layer.
// The caller logs a warning and does not refuse to start.
func RoleBypassesRLS(ctx context.Context, dsn string) (bool, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return false, err
	}
	defer conn.Close(context.Background())
	var bypass bool
	if err := conn.QueryRow(ctx,
		`SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass); err != nil {
		return false, err
	}
	return bypass, nil
}

// DisabledHandler answers every board path with 404 board_disabled. It is the
// feature-flag seam: a deployment that turns the board off must not reveal
// whether a channel, message or task exists.
func DisabledHandler() http.Handler { return disabledHandler{} }

type disabledHandler struct{}

func (disabledHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	writeDisabled(w, "the board is disabled in this deployment", "board_disabled")
}

const (
	defaultHeartbeat       = 15 * time.Second
	defaultStreamLifetime  = 55 * time.Second
	defaultChannelScan     = 5 * time.Minute
	channelCreateBodyLimit = 64 << 10
)
