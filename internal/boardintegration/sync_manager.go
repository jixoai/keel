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

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/store"

	boardaudit "github.com/jixoai/keel-board/audit"
	boardmetrics "github.com/jixoai/keel-board/metrics"
	boardserver "github.com/jixoai/keel-board/server"
	boardsync "github.com/jixoai/keel-board/sync"
	boardsyncstore "github.com/jixoai/keel-board/syncstore"
)

// Audit kinds keel adds for the credential/entry lifecycle. board_audit stores
// bounded tokens; the detail carries names/status only, never a value.
const (
	auditKindEntryCreated   = "sync.entry.created"
	auditKindEntryUpdated   = "sync.entry.updated"
	auditKindCredentialSeen = "sync.credential.status"
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
}

// syncManager owns the runtime entry set: listing, credential status, the
// enable/disable path, and the workers.
type syncManager struct {
	store    *store.Store
	syncdb   *boardsyncstore.Postgres
	audit    *boardaudit.Postgres
	tenant   uuid.UUID
	resolver SecretResolver
	logger   *slog.Logger

	registry      *boardsync.Registry
	bridgeSecrets map[boardsync.Key]boardsync.Secret
	subscribed    map[boardsync.Key]bool
	statuses      map[boardsync.Key]CredentialStatus
	// pendingRestart marks a key whose configured provider is not the one this
	// process holds (create/rotation/routes). Enabling it must not fall back to
	// the old provider: the board sync row stays disabled until a restart.
	pendingMu      sync.Mutex
	pendingRestart map[boardsync.Key]bool
	gauge          *credentialGauge

	server *boardserver.Server
	pool   *syncWorkerPool
}

// loadSyncBoot reads the tenant's entries and builds the registry. It never
// fails the board surface for one bad entry: a bad entry is logged (without a
// credential) and disabled.
func loadSyncBoot(ctx context.Context, opts Options, syncEnabled bool, syncdb *boardsyncstore.Postgres) (*syncBoot, error) {
	boot := &syncBoot{
		registry:      boardsync.NewRegistry(),
		startChannels: make(map[boardserver.SyncKey][]string),
		bridgeSecrets: make(map[boardsync.Key]boardsync.Secret),
		subscribed:    make(map[boardsync.Key]bool),
		statuses:      make(map[boardsync.Key]CredentialStatus),
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
		provider, report, buildErr := buildSyncProvider(ctx, opts.TenantID, record, resolver)
		boot.statuses[boardsync.Key(record.Key)] = report.Status
		if buildErr != nil || provider == nil {
			opts.Logger.Warn("board sync entry is not runnable",
				"tenant", tenant, "key", record.Key, "platform", record.Platform,
				"credential", report.Status, "error", sanitizedBuildError(buildErr))
			_ = upsertSyncStoreEntry(ctx, syncdb, tenant, record.Key, effectivePlatform(record, provider), false, opts.Logger)
			continue
		}
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

// newSyncManager wraps a boot result with the process store, audit and gauge.
func newSyncManager(boot *syncBoot, st *store.Store, syncdb *boardsyncstore.Postgres, audit *boardaudit.Postgres, tenant uuid.UUID, box *store.SecretBox, logger *slog.Logger, metricsRegistry *boardmetrics.Registry) *syncManager {
	if boot == nil {
		boot = &syncBoot{
			registry:      boardsync.NewRegistry(),
			startChannels: make(map[boardserver.SyncKey][]string),
			bridgeSecrets: make(map[boardsync.Key]boardsync.Secret),
			subscribed:    make(map[boardsync.Key]bool),
			statuses:      make(map[boardsync.Key]CredentialStatus),
		}
	}
	manager := &syncManager{
		store: st, syncdb: syncdb, audit: audit,
		tenant: tenant, logger: logger,
		resolver:       SecretResolver{Store: st, Box: box},
		registry:       boot.registry,
		bridgeSecrets:  boot.bridgeSecrets,
		subscribed:     boot.subscribed,
		statuses:       boot.statuses,
		pendingRestart: make(map[boardsync.Key]bool),
	}
	if metricsRegistry != nil {
		manager.gauge = newCredentialGauge(metricsRegistry)
	}
	for key, status := range manager.statuses {
		manager.gauge.set(key, status)
	}
	return manager
}

// Attach binds the board server and its worker pool after server construction.
func (m *syncManager) Attach(server *boardserver.Server, tenant string) {
	m.server = server
	if server == nil {
		return
	}
	m.pool = newSyncWorkerPool(server, m.syncdb, m.registry, tenant, m.logger)
}

// Start prepares the sync cursors, then starts the board subscription and the
// workers for every connection enabled at boot. It must run before the HTTP
// listener accepts writes, or a root posted in the gap is skipped as history.
func (m *syncManager) Start(ctx context.Context) error {
	if m == nil || m.server == nil {
		return nil
	}
	if err := m.server.PrepareSync(ctx); err != nil {
		return err
	}
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
	return nil
}

// Stop stops workers. The Surface calls it before closing the stores.
func (m *syncManager) Stop() {
	if m == nil || m.pool == nil {
		return
	}
	m.pool.StopAll()
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
		views = append(views, viewOf(record, status))
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
	return viewOf(record, status), true, nil
}

// Create persists a new entry. The running process intentionally does not
// adopt it: the response says a restart is required, and the board sync row is
// enabled as requested only for the next boot. Until then a webhook for an
// enabled key answers 503 (retry), never 200 with a silent drop.
func (m *syncManager) Create(ctx context.Context, input SyncEntryInput) (SyncEntryView, bool, error) {
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
	created, err := m.store.CreateBoardSyncEntry(ctx, record)
	if err != nil {
		return SyncEntryView{}, false, err
	}
	status := m.credentialStatus(ctx, created)
	m.gauge.set(boardsync.Key(created.Key), status)
	if err := upsertSyncStoreEntry(ctx, m.syncdb, m.tenant.String(), created.Key, effectiveStoredPlatform(created), created.Enabled, m.logger); err != nil {
		m.logger.Warn("board sync store entry could not be created",
			"tenant", m.tenant, "key", created.Key, "error", err)
	}
	m.markPending(boardsync.Key(created.Key))
	m.auditEntry(ctx, auditKindEntryCreated, created, status)
	return viewOf(created, status), true, nil
}

// Update applies a PATCH. A structural change is persisted but disabled at the
// board sync layer until restart; an enable/disable toggle is immediate for a
// connection this process loaded at boot.
func (m *syncManager) Update(ctx context.Context, key string, patch SyncEntryPatch) (SyncEntryView, bool, bool, error) {
	current, found, err := m.store.GetBoardSyncEntry(ctx, m.tenant, key)
	if err != nil {
		return SyncEntryView{}, false, false, err
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
	structural := !sameEntryDocument(current, next)
	restartRequired := false
	if structural {
		if err := m.validateProviderConfig(ctx, next); err != nil {
			return SyncEntryView{}, false, false, err
		}
		updated, err := m.store.UpdateBoardSyncEntry(ctx, next, current.Revision)
		if err != nil {
			return SyncEntryView{}, false, false, err
		}
		// Fail closed: the running provider still holds the old credential and
		// routes. Disable the board sync row and stop this key's workers until
		// a restart loads the new document.
		m.markPending(boardsync.Key(updated.Key))
		if m.pool != nil {
			m.pool.Stop(boardsync.Key(updated.Key))
		}
		if err := upsertSyncStoreEntry(ctx, m.syncdb, m.tenant.String(), updated.Key, effectiveStoredPlatform(updated), false, m.logger); err != nil {
			m.logger.Warn("board sync store entry could not be disabled after a structural change",
				"tenant", m.tenant, "key", updated.Key, "error", err)
		}
		status := m.credentialStatus(ctx, updated)
		m.gauge.set(boardsync.Key(updated.Key), status)
		m.auditEntry(ctx, auditKindEntryUpdated, updated, status)
		return viewOf(updated, status), true, true, nil
	}
	// A pending-restart key must never revive the provider this process holds.
	// Its desired enabled flag is persisted, but the board sync row stays
	// disabled until a restart loads the configured provider.
	if patch.Enabled != nil && *patch.Enabled && m.isPending(boardsync.Key(key)) {
		updated := current
		if !current.Enabled {
			var err error
			updated, err = m.store.SetBoardSyncEntryEnabled(ctx, m.tenant, key, true)
			if err != nil {
				return SyncEntryView{}, false, false, err
			}
		}
		if err := upsertSyncStoreEntry(ctx, m.syncdb, m.tenant.String(), updated.Key, effectiveStoredPlatform(updated), false, m.logger); err != nil {
			return SyncEntryView{}, false, false, err
		}
		status := m.credentialStatus(ctx, updated)
		m.gauge.set(boardsync.Key(updated.Key), status)
		m.auditEntry(ctx, auditKindEntryUpdated, updated, status)
		return viewOf(updated, status), true, true, nil
	}
	// Only Enabled may have changed (or nothing at all).
	if patch.Enabled != nil && *patch.Enabled != current.Enabled {
		updated, err := m.store.SetBoardSyncEntryEnabled(ctx, m.tenant, key, *patch.Enabled)
		if err != nil {
			return SyncEntryView{}, false, false, err
		}
		if *patch.Enabled {
			registered := false
			if _, ok := m.registry.Get(m.tenant.String(), boardsync.Key(key)); ok {
				registered = true
			}
			switch {
			case registered && m.subscribed[boardsync.Key(key)]:
				if err := upsertSyncStoreEntry(ctx, m.syncdb, m.tenant.String(), updated.Key, effectiveStoredPlatform(updated), true, m.logger); err != nil {
					return SyncEntryView{}, false, false, err
				}
				if m.pool != nil {
					if err := m.pool.Start(ctx, boardsync.Key(key)); err != nil {
						return SyncEntryView{}, false, false, err
					}
				}
			default:
				// No worker subscription exists in this process. Keep the
				// board sync row disabled so a webhook is acknowledged without
				// a side effect; the entry becomes live only at restart. The
				// response says restartRequired.
				m.markPending(boardsync.Key(key))
				if err := upsertSyncStoreEntry(ctx, m.syncdb, m.tenant.String(), updated.Key, effectiveStoredPlatform(updated), false, m.logger); err != nil {
					return SyncEntryView{}, false, false, err
				}
				restartRequired = true
			}
		} else {
			if m.pool != nil {
				m.pool.Stop(boardsync.Key(key))
			}
			if err := upsertSyncStoreEntry(ctx, m.syncdb, m.tenant.String(), updated.Key, effectiveStoredPlatform(updated), false, m.logger); err != nil {
				return SyncEntryView{}, false, false, err
			}
		}
		status := m.credentialStatus(ctx, updated)
		m.gauge.set(boardsync.Key(updated.Key), status)
		m.auditEntry(ctx, auditKindEntryUpdated, updated, status)
		return viewOf(updated, status), true, restartRequired, nil
	}
	status := m.credentialStatus(ctx, current)
	m.gauge.set(boardsync.Key(current.Key), status)
	return viewOf(current, status), true, false, nil
}

// validateProviderConfig builds the provider without I/O when the credentials
// happen to be configured. A missing credential is not a 400 (the operator may
// be about to store it); a malformed platform setting is.
func (m *syncManager) validateProviderConfig(ctx context.Context, record store.BoardSyncEntryRecord) error {
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

func (m *syncManager) markPending(key boardsync.Key) {
	m.pendingMu.Lock()
	if m.pendingRestart == nil {
		m.pendingRestart = make(map[boardsync.Key]bool)
	}
	m.pendingRestart[key] = true
	m.pendingMu.Unlock()
}

func (m *syncManager) isPending(key boardsync.Key) bool {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	return m.pendingRestart[key]
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
