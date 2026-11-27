package boardintegration

// Sync entry management and runtime assembly (keel-board-v1 tasks 10.6,
// 12.1). Entry documents live in keel's board_tenant_sync_entries table; the
// board module's sync store carries only the enabled/platform view its own API
// serves.
//
// Runtime effect is explicit, never pretended:
//   - create and structural changes (secret rotation, routes, settings) are
//     persisted and the board sync store row is disabled until the process
//     restarts, so an old credential or route can never keep serving;
//   - an enable/disable toggle is immediate for the connection that was
//     loaded at boot (webhook acceptance follows the board sync store row, and
//     this process's workers start/stop);
//   - enabling a connection this process never loaded is safe but inert: the
//     webhook answers 503 until a restart, so the platform retries instead of
//     the event being lost.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/store"

	boardaudit "github.com/jixoai/keel-board/audit"
	boardlog "github.com/jixoai/keel-board/log"
	boardmetrics "github.com/jixoai/keel-board/metrics"
	boardserver "github.com/jixoai/keel-board/server"
	boardsync "github.com/jixoai/keel-board/sync"
	boardsyncstore "github.com/jixoai/keel-board/syncstore"
)

// Audit kinds keel adds for the credential/entry lifecycle. board_audit stores
// bounded tokens; the detail carries names/status only, never a value.
// errSyncEntryUnavailable marks a storage failure on the entry path. The HTTP
// layer answers 503, never 400 or a success.
var errSyncEntryUnavailable = errors.New("boardintegration: sync entry storage is unavailable")

// syncEntryStoreError distinguishes an entry storage failure (503) from a
// validation failure (400). The domain sentinels keep their own HTTP mapping.
func syncEntryStoreError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, store.ErrBoardSyncEntryMissing),
		errors.Is(err, store.ErrBoardSyncEntryExists),
		errors.Is(err, store.ErrBoardSyncEntryConflict):
		return err
	}
	if errors.Is(err, errSyncEntryUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %v", errSyncEntryUnavailable, err)
}

const (
	auditKindEntryCreated   = "sync.entry.created"
	auditKindEntryUpdated   = "sync.entry.updated"
	auditKindCredentialSeen = "sync.credential.status"

	// defaultSyncMonitor bounds how long a flag switch, a durable disable or a
	// credential rotation can go unnoticed by this process's workers.
	defaultSyncMonitor = 5 * time.Second
)

// syncBoot is the immutable result of reading configuration at start. The
// board server consumes Registry/Owned/StartChannels/BridgeSecret; the manager
// keeps Subscribed and Statuses for runtime decisions and telemetry.
type syncBoot struct {
	registry      *boardsync.Registry
	owned         []boardserver.SyncKey
	startChannels map[boardserver.SyncKey][]string
	bridgeSecrets map[boardsync.Key]boardsync.Secret
	subscribed    map[boardsync.Key]bool
	statuses      map[boardsync.Key]CredentialStatus
	gate          *runtimeGate
	// loaded is the configuration and credential fingerprint this process
	// actually built. A durable entry that no longer matches it is pending a
	// restart and must not serve through the old provider.
	loaded map[boardsync.Key]loadedSyncEntry
}

// loadedSyncEntry is the boot snapshot of one configured connection. The
// fingerprint covers the resolved secret values; it is compared in-process and
// never rendered or logged.
type loadedSyncEntry struct {
	record      store.BoardSyncEntryRecord
	fingerprint string
}

// syncManager owns the runtime entry set: listing, credential status, the
// enable/disable path, and the workers.
type syncManager struct {
	store    *store.Store
	syncdb   *boardsyncstore.Postgres
	audit    *boardaudit.Postgres
	board    *boardlog.Board
	tenant   uuid.UUID
	resolver SecretResolver
	logger   *slog.Logger

	registry      *boardsync.Registry
	bridgeSecrets map[boardsync.Key]boardsync.Secret
	subscribed    map[boardsync.Key]bool
	statuses      map[boardsync.Key]CredentialStatus
	loaded        map[boardsync.Key]loadedSyncEntry
	gauge         *credentialGauge

	flags         BoardFlagReader
	monitor       time.Duration
	workerPoll    time.Duration
	providerHosts []string
	// externalRegistry means an embedding test/deployment supplied its own
	// registry; keel has no durable entry document to compare, so the durable
	// gate is not applied.
	externalRegistry bool

	// stateMu serializes entry mutations (create/update/enable) and the
	// monitor's decisions. A structural change and an enable racing on one
	// process must not both decide from the same pre-change snapshot.
	stateMu sync.Mutex
	runCtx  context.Context

	server *boardserver.Server
	pool   *syncWorkerPool
}

// loadSyncBoot reads the tenant's entries and builds the registry. It never
// fails the board surface for one bad entry: a bad entry is logged (without a
// credential) and disabled.
func loadSyncBoot(ctx context.Context, opts Options, syncEnabled bool, syncdb *boardsyncstore.Postgres, board *boardlog.Board) (*syncBoot, error) {
	boot := &syncBoot{
		registry:      boardsync.NewRegistry(),
		startChannels: make(map[boardserver.SyncKey][]string),
		bridgeSecrets: make(map[boardsync.Key]boardsync.Secret),
		subscribed:    make(map[boardsync.Key]bool),
		statuses:      make(map[boardsync.Key]CredentialStatus),
		gate:          newRuntimeGate(),
		loaded:        make(map[boardsync.Key]loadedSyncEntry),
	}
	if !syncEnabled || opts.TenantID == uuid.Nil || opts.Store == nil {
		return boot, nil
	}
	records, err := opts.Store.ListBoardSyncEntries(ctx, opts.TenantID)
	if err != nil {
		return nil, fmt.Errorf("boardintegration: list sync entries: %w", err)
	}
	resolver := SecretResolver{Store: opts.Store, Box: opts.SecretBox}
	tenant := opts.TenantID.String()
	for _, record := range records {
		if err := validateSyncEntry(&record); err != nil {
			opts.Logger.Warn("board sync entry configuration is invalid",
				"tenant", tenant, "key", record.Key, "error", err)
			boot.statuses[boardsync.Key(record.Key)] = CredentialInvalid
			_ = upsertSyncStoreEntry(ctx, syncdb, tenant, record.Key, record.Platform, false, opts.Logger)
			continue
		}
		if err := validateProviderEndpoints(record, opts.ProviderHosts); err != nil {
			opts.Logger.Warn("board sync entry endpoint is not allowed",
				"tenant", tenant, "key", record.Key, "error", err)
			boot.statuses[boardsync.Key(record.Key)] = CredentialInvalid
			_ = upsertSyncStoreEntry(ctx, syncdb, tenant, record.Key, record.Platform, false, opts.Logger)
			continue
		}
		// P0-1: a historical entry that names an absent or relation-free board
		// channel must not start a worker. It stays disabled (fail closed) and
		// the log names the exact create call; one bad entry never crashes the
		// process. The relation is checked at boot because a platform actor can
		// never be mapped to a keel Account by the owner reconciler.
		if report, aclErr := checkRouteACL(ctx, opts.Store, board, opts.TenantID, record.Routes); aclErr != nil || !report.Ready() {
			issue := report.Error()
			if issue == "" && aclErr != nil {
				issue = "the route ACL could not be read"
			}
			opts.Logger.Warn("board sync entry route is not provisioned; the entry stays disabled",
				"tenant", tenant, "key", record.Key, "reason", issue)
			_ = upsertSyncStoreEntry(ctx, syncdb, tenant, record.Key, record.Platform, false, opts.Logger)
			continue
		}
		fingerprint, _ := resolver.entryFingerprint(ctx, opts.TenantID, record.Platform, record.SecretRef)
		boot.loaded[boardsync.Key(record.Key)] = loadedSyncEntry{record: record, fingerprint: fingerprint}
		provider, report, buildErr := buildSyncProvider(ctx, opts.TenantID, record, resolver)
		boot.statuses[boardsync.Key(record.Key)] = report.Status
		if buildErr != nil || provider == nil {
			opts.Logger.Warn("board sync entry is not runnable",
				"tenant", tenant, "key", record.Key, "platform", record.Platform,
				"credential", report.Status, "error", sanitizedBuildError(buildErr))
			_ = upsertSyncStoreEntry(ctx, syncdb, tenant, record.Key, effectivePlatform(record, provider), false, opts.Logger)
			continue
		}
		provider = wrapSyncProvider(provider, boardsync.Key(record.Key), boot.gate)
		platform := provider.Platform()
		fn, routes := syncFunctions(record.Routes)
		if err := boot.registry.Add(boardsync.Entry{
			Tenant: tenant, Key: boardsync.Key(record.Key), Platform: platform,
			Provider: provider, Sync: fn, Routes: routes,
		}); err != nil {
			opts.Logger.Warn("board sync entry could not register",
				"tenant", tenant, "key", record.Key, "error", err)
			_ = upsertSyncStoreEntry(ctx, syncdb, tenant, record.Key, string(platform), false, opts.Logger)
			continue
		}
		if secret, ok := bridgeSecretOf(provider, report); ok {
			boot.bridgeSecrets[boardsync.Key(record.Key)] = secret
		}
		key := boardserver.SyncKey{Tenant: tenant, Key: boardsync.Key(record.Key)}
		if len(record.StartChannels) > 0 {
			boot.startChannels[key] = append([]string(nil), record.StartChannels...)
		}
		if record.Enabled {
			boot.subscribed[boardsync.Key(record.Key)] = true
			boot.owned = append(boot.owned, key)
		}
		// A valid but disabled entry stays registered, so a later enable can
		// take effect without rebuilding the provider; it owns no workers and
		// its board sync row is disabled.
		if err := upsertSyncStoreEntry(ctx, syncdb, tenant, record.Key, string(platform), record.Enabled, opts.Logger); err != nil {
			opts.Logger.Warn("board sync store entry could not be aligned",
				"tenant", tenant, "key", record.Key, "error", err)
		}
	}
	return boot, nil
}

func effectivePlatform(record store.BoardSyncEntryRecord, provider boardsync.Provider) string {
	if provider != nil {
		return string(provider.Platform())
	}
	return effectiveStoredPlatform(record)
}

// effectiveStoredPlatform is the platform name the board's own sync store
// records. A chatsdk entry is configured as "chatsdk" but registers under its
// sidecar's platform, exactly like cmd/board does; the board's webhook/bridge
// route compares the stored platform with the provider's, so the stored value
// must be the effective one.
func effectiveStoredPlatform(record store.BoardSyncEntryRecord) string {
	if record.Platform != syncPlatformChatSDK {
		return record.Platform
	}
	settings, err := parsePlatformSettings(record.Platform, record.Settings)
	if err != nil {
		return record.Platform
	}
	if platform := strings.TrimSpace(settings.chatsdk.Platform); platform != "" {
		return platform
	}
	return record.Platform
}

// syncFunctions maps platform channel -> board channel, all mirror entries. A
// platform channel can also be imported-only later; the current schema has no
// per-route mode, so every configured route mirrors.
func syncFunctions(routes map[string]string) (boardsync.SyncFunc, boardsync.RouteFunc) {
	functions := make(map[string]boardsync.SyncFunc, len(routes))
	for platformChannel, boardChannel := range routes {
		functions[platformChannel] = boardsync.Mirror(boardChannel)
	}
	return boardsync.Channels(functions)
}

// upsertSyncStoreEntry creates/aligns the board module's local entry row. An
// existing row's enabled flag is operator state, so UpsertEntry preserves it;
// SetEntryEnabled then applies the desired state explicitly.
func upsertSyncStoreEntry(ctx context.Context, syncdb *boardsyncstore.Postgres, tenant, key, platform string, enabled bool, logger *slog.Logger) error {
	if syncdb == nil {
		return errors.New("boardintegration: sync store is not wired")
	}
	if !boardsync.Key(key).Valid() || strings.TrimSpace(platform) == "" {
		return errors.New("boardintegration: entry key/platform is not usable")
	}
	entry, err := syncdb.UpsertEntry(ctx, boardsyncstore.Entry{
		Tenant: tenant, Key: boardsync.Key(key), Platform: boardsync.Platform(platform), Enabled: enabled,
	})
	if err != nil {
		return err
	}
	if entry.Enabled != enabled {
		if _, err := syncdb.SetEntryEnabled(ctx, tenant, boardsync.Key(key), enabled); err != nil {
			return err
		}
	}
	return nil
}

// sanitizedBuildError reduces a construction error to a bounded class. Provider
// validation errors name configuration fields, never a credential value, but
// a future adapter could quote one, so the log keeps only the field shape.
func sanitizedBuildError(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if len(text) > 200 {
		text = text[:200]
	}
	return text
}

// newSyncManager wraps a boot result with the process store, audit, tenant
// switches and gauge.
func newSyncManager(boot *syncBoot, st *store.Store, syncdb *boardsyncstore.Postgres, audit *boardaudit.Postgres, board *boardlog.Board, tenant uuid.UUID, box *store.SecretBox, logger *slog.Logger, metricsRegistry *boardmetrics.Registry, flags BoardFlagReader, monitor time.Duration, providerHosts []string) *syncManager {
	if boot == nil {
		boot = &syncBoot{
			registry:      boardsync.NewRegistry(),
			startChannels: make(map[boardserver.SyncKey][]string),
			bridgeSecrets: make(map[boardsync.Key]boardsync.Secret),
			subscribed:    make(map[boardsync.Key]bool),
			statuses:      make(map[boardsync.Key]CredentialStatus),
			loaded:        make(map[boardsync.Key]loadedSyncEntry),
		}
	}
	if boot.loaded == nil {
		boot.loaded = make(map[boardsync.Key]loadedSyncEntry)
	}
	if monitor <= 0 {
		monitor = defaultSyncMonitor
	}
	manager := &syncManager{
		store: st, syncdb: syncdb, audit: audit, board: board,
		tenant: tenant, logger: logger, flags: flags, monitor: monitor,
		providerHosts: append([]string(nil), providerHosts...),
		resolver:      SecretResolver{Store: st, Box: box},
		registry:      boot.registry,
		bridgeSecrets: boot.bridgeSecrets,
		subscribed:    boot.subscribed,
		statuses:      boot.statuses,
		loaded:        boot.loaded,
	}
	if metricsRegistry != nil {
		manager.gauge = newCredentialGauge(metricsRegistry)
	}
	for key, status := range manager.statuses {
		manager.gauge.set(key, status)
	}
	if boot.gate != nil {
		boot.gate.set(func(ctx context.Context, key boardsync.Key) error {
			if manager.flags != nil {
				flags, flagErr := manager.flags.Get(ctx, manager.tenant)
				if flagErr != nil || !flags.FullyEnabled() {
					return errSyncPaused
				}
			}
			state, _, err := manager.connectionState(ctx, key)
			if err != nil {
				return errSyncPaused
			}
			switch state {
			case connectionOK:
				return nil
			case connectionDisabled, connectionPending, connectionMissing:
				return errSyncPaused
			default:
				return errSyncPaused
			}
		})
	}
	return manager
}

// Attach binds the board server and its worker pool after server construction.
func (m *syncManager) Attach(server *boardserver.Server, tenant string) {
	m.server = server
	if server == nil {
		return
	}
	m.pool = newSyncWorkerPool(server, &gatedSyncStore{Store: m.syncdb, allowed: m.workerAllowed}, m.registry, tenant, m.workerPoll, m.logger)
}

// Start prepares the sync cursors, then starts the board subscription, the
// workers for every connection enabled at boot, and the monitor that pauses
// them when the tenant flags close, the durable entry is disabled/rotated, or
// a credential changes. ctx is the process lifetime, never a request context:
// an enable PATCH must start workers that outlive the HTTP request.
func (m *syncManager) Start(ctx context.Context) error {
	if m == nil || m.server == nil {
		return nil
	}
	if err := m.server.PrepareSync(ctx); err != nil {
		return err
	}
	m.stateMu.Lock()
	m.runCtx = ctx
	m.stateMu.Unlock()
	if m.pool == nil {
		return nil
	}
	for key := range m.subscribed {
		if err := m.pool.Start(ctx, key); err != nil {
			return fmt.Errorf("boardintegration: start sync workers for %q: %w", key, err)
		}
	}
	if len(m.subscribed) > 0 {
		go m.pool.RunBoardSubscription(ctx)
	}
	go m.monitorLoop(ctx)
	return nil
}

// Stop stops the monitor and workers. The Surface calls it before closing the
// stores.
func (m *syncManager) Stop() {
	if m == nil {
		return
	}
	if m.pool != nil {
		m.pool.StopAll()
	}
}

// monitorLoop reconciles this process's workers with the durable configuration
// and tenant switches. It is the cross-replica half of disable/rotation: every
// process observes the same keel row, so a change made through one replica
// pauses the others within one interval.
func (m *syncManager) monitorLoop(ctx context.Context) {
	ticker := time.NewTicker(m.monitor)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reconcile(ctx)
		}
	}
}

// reconcile pauses or resumes workers. A flag read or store failure pauses
// everything: fail closed beats a stale credential or a disabled board still
// writing.
func (m *syncManager) reconcile(ctx context.Context) {
	if m == nil || m.pool == nil || m.flags == nil {
		return
	}
	flags, err := m.flags.Get(ctx, m.tenant)
	if err != nil || !flags.FullyEnabled() {
		if err != nil {
			m.logger.Warn("board sync monitor could not read tenant flags; pausing workers", "tenant", m.tenant, "error", err)
		}
		m.pool.StopAll()
		return
	}
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	for key := range m.subscribed {
		state, record, _, stateErr := m.connectionStateLocked(ctx, key)
		if stateErr != nil {
			m.logger.Warn("board sync monitor could not read its entry; pausing the key",
				"tenant", m.tenant, "key", key, "error", stateErr)
			m.pool.Stop(key)
			continue
		}
		status := m.credentialStatus(ctx, record)
		m.gauge.set(key, status)
		m.statuses[key] = status
		switch state {
		case connectionOK:
			if err := m.pool.Start(ctx, key); err != nil {
				m.logger.Warn("board sync monitor could not restart workers",
					"tenant", m.tenant, "key", key, "error", err)
				m.pool.Stop(key)
			}
		case connectionPending:
			m.pool.Stop(key)
			if err := upsertSyncStoreEntry(ctx, m.syncdb, m.tenant.String(), record.Key, effectiveStoredPlatform(record), false, m.logger); err != nil {
				m.logger.Warn("board sync monitor could not disable a pending entry",
					"tenant", m.tenant, "key", key, "error", err)
			}
		case connectionDisabled, connectionMissing:
			m.pool.Stop(key)
			if state == connectionMissing {
				continue
			}
			if err := upsertSyncStoreEntry(ctx, m.syncdb, m.tenant.String(), record.Key, effectiveStoredPlatform(record), false, m.logger); err != nil {
				m.logger.Warn("board sync monitor could not keep a disabled entry disabled",
					"tenant", m.tenant, "key", key, "error", err)
			}
		}
	}
}

// workerAllowed is the store gate's durable check before a lease. The tenant
// switches are part of the check so a closed board/sync pauses new leases even
// before the monitor's next tick (within the flags cache window).
func (m *syncManager) workerAllowed(ctx context.Context, key boardsync.Key) bool {
	if m.flags != nil {
		flags, err := m.flags.Get(ctx, m.tenant)
		if err != nil || !flags.FullyEnabled() {
			return false
		}
	}
	// The board sync row is the durable enabled view every replica shares. A
	// row disabled directly (structural change, credential rotation, operator
	// action) must stop leases without waiting for the monitor's Stop.
	if m.syncdb != nil {
		entry, found, err := m.syncdb.Entry(ctx, m.tenant.String(), key)
		if err != nil || !found || !entry.Enabled {
			return false
		}
	}
	state, _, err := m.connectionState(ctx, key)
	return err == nil && state == connectionOK
}

// connectionState is the durable answer for one live connection. It is the
// gate every platform-facing path (webhook, attach, source read, worker start)
// shares, so a stale process cannot keep using an old provider.
type connectionState int

const (
	connectionOK connectionState = iota
	connectionDisabled
	connectionPending
	connectionMissing
)

// connectionState reports whether this process may serve key right now. A
// missing row is a 404; a disabled row is a 404; a row whose enabled flag or
// configuration differs from this process's boot snapshot, or whose resolved
// credential no longer matches the fingerprint the provider was built with, is
// pending a restart.
func (m *syncManager) connectionState(ctx context.Context, key boardsync.Key) (connectionState, store.BoardSyncEntryRecord, error) {
	// loaded and subscribed are immutable after Start; only the store lookup is
	// live. Do not take stateMu here: a worker gated store may call this while
	// an update holds stateMu and waits for that worker to stop.
	state, record, _, err := m.connectionStateLocked(ctx, key)
	return state, record, err
}

func (m *syncManager) connectionStateLocked(ctx context.Context, key boardsync.Key) (connectionState, store.BoardSyncEntryRecord, string, error) {
	if m != nil && m.externalRegistry {
		return connectionOK, store.BoardSyncEntryRecord{Key: string(key), Enabled: true}, "", nil
	}
	if m == nil || m.store == nil || m.tenant == uuid.Nil {
		return connectionMissing, store.BoardSyncEntryRecord{}, "", nil
	}
	record, found, err := m.store.GetBoardSyncEntry(ctx, m.tenant, string(key))
	if err != nil {
		return connectionPending, store.BoardSyncEntryRecord{}, "", err
	}
	if !found {
		return connectionMissing, store.BoardSyncEntryRecord{}, "", nil
	}
	if !record.Enabled {
		return connectionDisabled, record, "", nil
	}
	// Compare the durable document after the same normalization the boot
	// loader applied (default start channels, lowercase platform). A row
	// written by an older/direct path must not look structurally different
	// forever just because the loader fills a derived default.
	normalized := cloneEntryRecord(record)
	if err := validateSyncEntry(&normalized); err != nil {
		return connectionPending, record, "", nil
	}
	loaded, ok := m.loaded[key]
	if !ok || !sameEntryDocument(loaded.record, normalized) || !m.subscribed[key] {
		return connectionPending, record, "", nil
	}
	fingerprint, status := m.resolver.entryFingerprint(ctx, m.tenant, normalized.Platform, normalized.SecretRef)
	if status != CredentialConfigured || fingerprint != loaded.fingerprint {
		return connectionPending, record, fingerprint, nil
	}
	return connectionOK, record, fingerprint, nil
}

// List returns every configured entry for the tenant with its current
// credential status. The credential status is recomputed from the secret store
// on read; it never decrypts into the response.
func (m *syncManager) List(ctx context.Context) ([]SyncEntryView, error) {
	if m == nil || m.store == nil || m.tenant == uuid.Nil {
		return []SyncEntryView{}, nil
	}
	records, err := m.store.ListBoardSyncEntries(ctx, m.tenant)
	if err != nil {
		return nil, err
	}
	views := make([]SyncEntryView, 0, len(records))
	for _, record := range records {
		status := m.credentialStatus(ctx, record)
		m.gauge.set(boardsync.Key(record.Key), status)
		view := m.decorateView(ctx, record, viewOf(record, status))
		if state, _, err := m.connectionState(ctx, boardsync.Key(record.Key)); err == nil && record.Enabled {
			view.RestartRequired = state == connectionPending
		}
		views = append(views, view)
	}
	return views, nil
}

// Get returns one entry.
func (m *syncManager) Get(ctx context.Context, key string) (SyncEntryView, bool, error) {
	record, found, err := m.store.GetBoardSyncEntry(ctx, m.tenant, key)
	if err != nil || !found {
		return SyncEntryView{}, found, err
	}
	status := m.credentialStatus(ctx, record)
	m.gauge.set(boardsync.Key(record.Key), status)
	view := m.decorateView(ctx, record, viewOf(record, status))
	if state, _, err := m.connectionState(ctx, boardsync.Key(record.Key)); err == nil && record.Enabled {
		view.RestartRequired = state == connectionPending
	}
	return view, true, nil
}

// Create persists a new entry. The running process intentionally does not
// adopt it: the response says a restart is required, and an enabled webhook
// answers 503 (retry) until the process loads the configured provider.
func (m *syncManager) Create(ctx context.Context, input SyncEntryInput) (SyncEntryView, bool, error) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	secretRef, err := input.secretRef()
	if err != nil {
		return SyncEntryView{}, false, err
	}
	record := store.BoardSyncEntryRecord{
		TenantID: m.tenant, Key: input.Key, Platform: input.Platform,
		SecretRef: secretRef, Settings: input.Settings, Routes: input.Routes,
		StartChannels: input.StartChannels, Enabled: enabled,
	}
	if err := validateSyncEntry(&record); err != nil {
		return SyncEntryView{}, false, err
	}
	if err := m.validateProviderConfig(ctx, record); err != nil {
		return SyncEntryView{}, false, err
	}
	// A disabled entry is safe staging: it never serves a webhook and its
	// board sync row stays disabled, so its route may not exist yet. Enabling
	// it (or a later routes change) re-checks the target.
	if enabled {
		if err := m.validateRouteACL(ctx, m.tenant, record.Routes); err != nil {
			return SyncEntryView{}, false, err
		}
	}
	created, err := m.store.CreateBoardSyncEntry(ctx, record)
	if err != nil {
		return SyncEntryView{}, false, syncEntryStoreError(err)
	}
	// P2 (hardening): a new entry is pending adoption. This process has not
	// loaded its provider, so the board sync store row must stay disabled until
	// a restart loads and aligns it; an enabled row here would advertise a
	// connection no process serves.
	if err := upsertSyncStoreEntry(ctx, m.syncdb, m.tenant.String(), created.Key, effectiveStoredPlatform(created), false, m.logger); err != nil {
		// The keel row and the board's local row are two stores. Do not leave a
		// configuration row whose board half was never written: remove the keel
		// row and answer 503 so the operator can retry.
		if _, delErr := m.store.DeleteBoardSyncEntry(ctx, m.tenant, created.Key); delErr != nil {
			m.logger.Error("board sync entry rollback failed after a board sync store error",
				"tenant", m.tenant, "key", created.Key, "error", delErr)
		}
		return SyncEntryView{}, false, fmt.Errorf("%w: %v", errSyncEntryUnavailable, err)
	}
	status := m.credentialStatus(ctx, created)
	m.gauge.set(boardsync.Key(created.Key), status)
	m.auditEntry(ctx, auditKindEntryCreated, created, status)
	return m.decorateView(ctx, created, viewOf(created, status)), true, nil
}

// Update applies a PATCH. A structural change is persisted but disabled at the
// board sync layer until restart; an enable/disable toggle is immediate for a
// connection this process loaded at boot and serves with its process-lifetime
// context.
func (m *syncManager) Update(ctx context.Context, key string, patch SyncEntryPatch) (SyncEntryView, bool, bool, error) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	current, found, err := m.store.GetBoardSyncEntry(ctx, m.tenant, key)
	if err != nil {
		return SyncEntryView{}, false, false, syncEntryStoreError(err)
	}
	if !found {
		return SyncEntryView{}, false, false, store.ErrBoardSyncEntryMissing
	}
	patchSecretRef, err := patch.secretRef()
	if err != nil {
		return SyncEntryView{}, false, false, err
	}
	next := cloneEntryRecord(current)
	if patch.Platform != nil {
		next.Platform = *patch.Platform
	}
	if patchSecretRef != nil {
		next.SecretRef = cloneStringMap(*patchSecretRef)
	}
	if patch.Settings != nil {
		next.Settings = append(json.RawMessage(nil), (*patch.Settings)...)
	}
	if patch.Routes != nil {
		next.Routes = cloneStringMap(*patch.Routes)
	}
	if patch.StartChannels != nil {
		next.StartChannels = append([]string(nil), (*patch.StartChannels)...)
	}
	if patch.Enabled != nil {
		next.Enabled = *patch.Enabled
	}
	if err := validateSyncEntry(&next); err != nil {
		return SyncEntryView{}, false, false, err
	}
	// Disabling must always work: an operator has to be able to stop a
	// connection whose route channel was deleted or lost its relation. Enabling
	// and a routes change re-check the target; a secret/settings/platform change
	// may be saved while unhealthy because it keeps the entry pending/disabled
	// until restart, and boot re-validates every route before a worker starts.
	turningOn := patch.Enabled != nil && *patch.Enabled
	routesChanged := patch.Routes != nil && !reflect.DeepEqual(current.Routes, next.Routes)
	if turningOn || routesChanged {
		if err := m.validateRouteACL(ctx, m.tenant, next.Routes); err != nil {
			return SyncEntryView{}, false, false, err
		}
	}
	if !sameEntryDocument(current, next) {
		return m.structuralUpdate(ctx, current, next)
	}
	// Only Enabled may have changed (or nothing at all).
	if patch.Enabled == nil {
		status := m.credentialStatus(ctx, current)
		m.gauge.set(boardsync.Key(current.Key), status)
		return viewOf(current, status), true, false, nil
	}
	if !*patch.Enabled {
		if !current.Enabled {
			status := m.credentialStatus(ctx, current)
			m.gauge.set(boardsync.Key(current.Key), status)
			return viewOf(current, status), true, false, nil
		}
		updated, err := m.store.SetBoardSyncEntryEnabled(ctx, m.tenant, key, false)
		if err != nil {
			return SyncEntryView{}, false, false, syncEntryStoreError(err)
		}
		if m.pool != nil {
			m.pool.Stop(boardsync.Key(key))
		}
		if err := upsertSyncStoreEntry(ctx, m.syncdb, m.tenant.String(), updated.Key, effectiveStoredPlatform(updated), false, m.logger); err != nil {
			return SyncEntryView{}, false, false, fmt.Errorf("%w: %v", errSyncEntryUnavailable, err)
		}
		status := m.credentialStatus(ctx, updated)
		m.gauge.set(boardsync.Key(updated.Key), status)
		m.auditEntry(ctx, auditKindEntryUpdated, updated, status)
		return viewOf(updated, status), true, false, nil
	}
	// Enable (or confirm an already-enabled entry). It may only start workers
	// when the durable document and this process's boot snapshot agree.
	updated := current
	if !current.Enabled {
		var err error
		updated, err = m.store.SetBoardSyncEntryEnabled(ctx, m.tenant, key, true)
		if err != nil {
			return SyncEntryView{}, false, false, syncEntryStoreError(err)
		}
	}
	status := m.credentialStatus(ctx, updated)
	m.gauge.set(boardsync.Key(updated.Key), status)
	state, _, _, stateErr := m.connectionStateLocked(ctx, boardsync.Key(key))
	if stateErr != nil {
		return SyncEntryView{}, false, false, fmt.Errorf("%w: %v", errSyncEntryUnavailable, stateErr)
	}
	if state != connectionOK || m.pool == nil || m.runCtx == nil || m.runCtx.Err() != nil {
		if err := upsertSyncStoreEntry(ctx, m.syncdb, m.tenant.String(), updated.Key, effectiveStoredPlatform(updated), false, m.logger); err != nil {
			return SyncEntryView{}, false, false, fmt.Errorf("%w: %v", errSyncEntryUnavailable, err)
		}
		m.auditEntry(ctx, auditKindEntryUpdated, updated, status)
		return viewOf(updated, status), true, true, nil
	}
	if err := upsertSyncStoreEntry(ctx, m.syncdb, m.tenant.String(), updated.Key, effectiveStoredPlatform(updated), true, m.logger); err != nil {
		return SyncEntryView{}, false, false, fmt.Errorf("%w: %v", errSyncEntryUnavailable, err)
	}
	if err := m.pool.Start(m.runCtx, boardsync.Key(key)); err != nil {
		// The worker failed to start after the entry was enabled. Disable both
		// the row and the desired flag, so a retry can try again cleanly and the
		// platform never believes a dead worker is serving.
		_, _ = m.store.SetBoardSyncEntryEnabled(ctx, m.tenant, key, false)
		_ = upsertSyncStoreEntry(ctx, m.syncdb, m.tenant.String(), updated.Key, effectiveStoredPlatform(updated), false, m.logger)
		return SyncEntryView{}, false, false, fmt.Errorf("%w: start workers: %v", errSyncEntryUnavailable, err)
	}
	m.auditEntry(ctx, auditKindEntryUpdated, updated, status)
	return viewOf(updated, status), true, false, nil
}

// structuralUpdate applies a rotation/route/settings change. The old provider
// must stop before the response: the durable row is disabled and any failure is
// surfaced, never reported as a successful live change.
func (m *syncManager) structuralUpdate(ctx context.Context, current, next store.BoardSyncEntryRecord) (SyncEntryView, bool, bool, error) {
	if err := m.validateProviderConfig(ctx, next); err != nil {
		return SyncEntryView{}, false, false, err
	}
	updated, err := m.store.UpdateBoardSyncEntry(ctx, next, current.Revision)
	if err != nil {
		return SyncEntryView{}, false, false, syncEntryStoreError(err)
	}
	if m.pool != nil {
		m.pool.Stop(boardsync.Key(updated.Key))
	}
	if err := upsertSyncStoreEntry(ctx, m.syncdb, m.tenant.String(), updated.Key, effectiveStoredPlatform(updated), false, m.logger); err != nil {
		status := m.credentialStatus(ctx, updated)
		m.gauge.set(boardsync.Key(updated.Key), status)
		m.auditEntry(ctx, auditKindEntryUpdated, updated, status)
		return SyncEntryView{}, true, false, fmt.Errorf("%w: disable pending entry: %v", errSyncEntryUnavailable, err)
	}
	status := m.credentialStatus(ctx, updated)
	m.gauge.set(boardsync.Key(updated.Key), status)
	m.auditEntry(ctx, auditKindEntryUpdated, updated, status)
	return viewOf(updated, status), true, true, nil
}

// validateProviderConfig builds the provider without I/O when the credentials
// happen to be configured. A missing credential is not a 400 (the operator may
// be about to store it); a malformed platform setting is.
func (m *syncManager) validateProviderConfig(ctx context.Context, record store.BoardSyncEntryRecord) error {
	if err := validateProviderEndpoints(record, m.providerHosts); err != nil {
		return err
	}
	provider, report, err := buildSyncProvider(ctx, m.tenant, record, m.resolver)
	if err != nil {
		if report.Status == CredentialMissing {
			// The configuration is syntactically valid; the credential is
			// reported as missing and the entry stays disabled.
			return nil
		}
		return fmt.Errorf("entry configuration: %w", err)
	}
	if provider == nil {
		return errors.New("entry configuration: no provider was built")
	}
	return nil
}

// routeStatus resolves the view fields for an entry's static routes. A read
// failure is a not-ready status, never a silent "ready".
func (m *syncManager) routeStatus(ctx context.Context, record store.BoardSyncEntryRecord) (bool, string) {
	report, err := checkRouteACL(ctx, m.store, m.board, m.tenant, record.Routes)
	if err != nil || !report.Ready() {
		issue := report.Error()
		if issue == "" {
			issue = "the route ACL could not be read"
		}
		return false, issue
	}
	return true, ""
}

func (m *syncManager) decorateView(ctx context.Context, record store.BoardSyncEntryRecord, view SyncEntryView) SyncEntryView {
	view.RoutesReady, view.RouteIssue = m.routeStatus(ctx, record)
	return view
}

// credentialStatus resolves the entry's credential roles now, without
// rendering a value. A malformed document or unavailable store is invalid or
// missing, never "configured".
func (m *syncManager) credentialStatus(ctx context.Context, record store.BoardSyncEntryRecord) CredentialStatus {
	if err := validateSyncEntry(&record); err != nil {
		return CredentialInvalid
	}
	report := m.resolver.resolveEntryCredential(ctx, m.tenant, record.Platform, record.SecretRef)
	return report.Status
}

// bridgeSecretFunc is the board server's per-entry HMAC resolver over the boot
// snapshot. A rotated secret is inert until restart; the structural PATCH
// disables the entry in the meantime.
func bridgeSecretFunc(secrets map[boardsync.Key]boardsync.Secret) func(string, boardsync.Key) (boardsync.Secret, bool) {
	return func(_ string, key boardsync.Key) (boardsync.Secret, bool) {
		secret, ok := secrets[key]
		return secret, ok && !secret.Empty()
	}
}

// BridgeSecret is the manager's copy of the same resolver (used by tests).
func (m *syncManager) BridgeSecret(tenant string, key boardsync.Key) (boardsync.Secret, bool) {
	if m == nil || tenant != m.tenant.String() {
		return "", false
	}
	return bridgeSecretFunc(m.bridgeSecrets)(tenant, key)
}

func (m *syncManager) auditEntry(ctx context.Context, kind string, record store.BoardSyncEntryRecord, status CredentialStatus) {
	if m.audit == nil {
		return
	}
	event := boardaudit.Event{
		Tenant: m.tenant.String(), Kind: kind,
		Key: record.Key, Platform: record.Platform,
		Status: string(status),
		Detail: map[string]any{"revision": record.Revision, "enabled": record.Enabled},
	}
	if _, err := m.audit.Append(ctx, event); err != nil {
		m.logger.Warn("board sync credential audit write failed",
			"tenant", m.tenant, "key", record.Key, "error", err)
	}
}

func cloneEntryRecord(record store.BoardSyncEntryRecord) store.BoardSyncEntryRecord {
	record.SecretRef = cloneStringMap(record.SecretRef)
	record.Routes = cloneStringMap(record.Routes)
	record.StartChannels = append([]string(nil), record.StartChannels...)
	record.Settings = append(json.RawMessage(nil), record.Settings...)
	return record
}

func cloneStringMap(value map[string]string) map[string]string {
	if value == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(value))
	for key, item := range value {
		out[key] = item
	}
	return out
}

// sameEntryDocument compares the fields a provider/worker construction reads.
// Enabled is deliberately not part of it: a toggle is not a rebuild.
func sameEntryDocument(a, b store.BoardSyncEntryRecord) bool {
	if a.Platform != b.Platform || a.Key != b.Key {
		return false
	}
	if !reflect.DeepEqual(a.SecretRef, b.SecretRef) || !reflect.DeepEqual(a.Routes, b.Routes) {
		return false
	}
	if !reflect.DeepEqual(a.StartChannels, b.StartChannels) {
		return false
	}
	return canonicalJSON(a.Settings) == canonicalJSON(b.Settings)
}

func canonicalJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return string(raw)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return string(raw)
	}
	return string(canonical)
}

// credentialGauge exports board_sync_credential_status{key,status} with 1 for
// the current status and 0 for the other two. Labels are bounded by the key
// grammar and the status vocabulary; no secret or message body can label a
// series.
type credentialGauge struct {
	gauge *boardmetrics.Gauge
	mu    sync.Mutex
	last  map[boardsync.Key]CredentialStatus
}

func newCredentialGauge(registry *boardmetrics.Registry) *credentialGauge {
	if registry == nil {
		return &credentialGauge{}
	}
	return &credentialGauge{
		gauge: registry.Gauge("board_sync_credential_status",
			"Configured sync credential status by connection key (1 for the current status).",
			boardmetrics.LabelKey, boardmetrics.LabelStatus),
		last: make(map[boardsync.Key]CredentialStatus),
	}
}

func (g *credentialGauge) set(key boardsync.Key, status CredentialStatus) {
	if g == nil || g.gauge == nil || key == "" {
		return
	}
	if status != CredentialConfigured && status != CredentialMissing && status != CredentialInvalid {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, candidate := range []CredentialStatus{CredentialConfigured, CredentialMissing, CredentialInvalid} {
		value := 0.0
		if candidate == status {
			value = 1
		}
		g.gauge.Set(value, boardmetrics.KeyLabel(string(key)), boardmetrics.StatusLabel(string(candidate)))
	}
	if g.last == nil {
		g.last = make(map[boardsync.Key]CredentialStatus)
	}
	g.last[key] = status
}
