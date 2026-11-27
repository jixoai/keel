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
	"github.com/jixoai/keel/internal/store"

	boardaudit "github.com/jixoai/keel-board/audit"
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
	Store *store.Store
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
	// SyncRegistry is the process's sync entries. Nil starts empty; Phase 1
	// leaves it empty until the tenant secret store lane (10.6) registers
	// entries. Tests inject entries here.
	SyncRegistry *boardsync.Registry
	// BoardMetrics is an existing board metric registry. Nil creates one.
	BoardMetrics *boardmetrics.Registry
}

// TextExporter is a Prometheus text exporter (keel's metrics.Registry and the
// test seam both satisfy it).
type TextExporter interface {
	WriteText(ctx context.Context, w io.Writer) error
}

// Surface owns the board's PostgreSQL pools, log, HTTP handlers and background
// repair loop. Close releases them in dependency order.
type Surface struct {
	handler   *handler
	webhook   *boardserver.Server
	board     *boardlog.Board
	store     *boardstore.Postgres
	syncdb    *boardsyncstore.Postgres
	audit     *boardaudit.Postgres
	owners    *ChannelOwners
	metrics   *boardmetrics.Registry
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
	logger    *slog.Logger
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

	syncdb, err := boardsyncstore.OpenPostgres(ctx, boardsyncstore.PostgresOptions{DSN: opts.DSN, SearchPath: opts.Schema})
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

	authorizer, err := NewAuthorizer(opts.Access, nil, board, opts.Logger)
	if err != nil {
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
	syncRegistry := opts.SyncRegistry
	if syncRegistry == nil {
		syncRegistry = boardsync.NewRegistry()
	}
	deploymentTenant := opts.TenantID
	syncTenant := func(*http.Request) (string, error) {
		if deploymentTenant == uuid.Nil {
			return "", errors.New("boardintegration: no deployment tenant for webhooks")
		}
		return deploymentTenant.String(), nil
	}

	server := boardserver.New(boardserver.Options{
		Board:     board,
		Authorize: authorizer,
		Actor:     identity.Actor,
		Tenant:    identity.Tenant,
		Logger:    opts.Logger,
		Metrics:   metricsRegistry,
		Audit:     auditStore,
		Sync: &boardserver.SyncOptions{
			Store:    syncdb,
			Registry: syncRegistry,
			Tenant:   syncTenant,
			Logger:   opts.Logger,
		},
	})

	owners := &ChannelOwners{
		Store: opts.Store, Audit: opts.Access, Schema: opts.Schema, DSN: opts.DSN,
		Logger: opts.Logger,
	}
	runCtx, cancel := context.WithCancel(context.Background())
	surface := &Surface{
		handler: &handler{
			next:   server,
			board:  board,
			owners: owners,
			sse:    &sseHandler{next: server, heartbeat: opts.Heartbeat, lifetime: opts.StreamLifetime, logger: opts.Logger},
			logger: opts.Logger,
		},
		webhook: server,
		board:   board,
		store:   store,
		syncdb:  syncdb,
		audit:   auditStore,
		owners:  owners,
		metrics: metricsRegistry,
		cancel:  cancel,
		done:    make(chan struct{}),
		logger:  opts.Logger,
	}
	go func() {
		defer close(surface.done)
		owners.Run(runCtx, opts.ChannelScanInterval)
	}()
	return surface, nil
}

// ServeHTTP implements the board API handler mounted at /v1/board.
func (s *Surface) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// Webhook serves the platform webhook namespace. It is mounted outside the
// board's session authentication; each provider verifies its own signature. The
// guard keeps the handler's other routes (/v1/board, /metrics) unreachable even
// if a proxy hands it a path outside the namespace.
func (s *Surface) Webhook() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/webhooks/") {
			DisabledHandler().ServeHTTP(w, r)
			return
		}
		s.webhook.ServeHTTP(w, r)
	})
}

// Metrics returns the board metric registry.
func (s *Surface) Metrics() *boardmetrics.Registry { return s.metrics }

// SyncStore returns the durable sync store. The secret-store lane (10.6)
// registers tenant entries through it; this lane leaves the registry empty.
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

// Close stops the reconciler and releases every pool.
func (s *Surface) Close() {
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.done != nil {
			<-s.done
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
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"the board is disabled in this deployment","code":"board_disabled"}`))
}

const (
	defaultHeartbeat       = 15 * time.Second
	defaultStreamLifetime  = 55 * time.Second
	defaultChannelScan     = 5 * time.Minute
	channelCreateBodyLimit = 64 << 10
)
