package harnessext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/pluginpkg"
	"github.com/jixoai/keel/internal/store"
)

// Plugin lifecycle (change keel-plugin-core): the state machine an operator
// drives through /v1/plugins, on top of the assembly semantics of plugin.go.
//
//	installed -> enabled -> disabled -> uninstalled
//	                \-> failed (an enable that fell over; enable retries it)
//	installed/disabled -> invalid (the package on disk stopped validating)
//
// The manager owns *intent* (what the database says), the Runtime owns
// *assembly* (what is loaded right now). Every mutation goes through the
// runtime, so a plugin that is enabled in the database but could not load is
// reported with the runtime's own diagnostics instead of pretending to work.
type LifecycleState string

const (
	// LifecycleInstalled is a valid package that is not enabled yet.
	LifecycleInstalled LifecycleState = "installed"
	// LifecycleEnabled means the operator asked for it and the runtime loaded it.
	LifecycleEnabled LifecycleState = "enabled"
	// LifecycleDisabled is a previously enabled plugin that is fully unloaded.
	LifecycleDisabled LifecycleState = "disabled"
	// LifecycleFailed is an enable that did not reach the active state.
	LifecycleFailed LifecycleState = "failed"
	// LifecycleInvalid is a package that no longer validates on disk.
	LifecycleInvalid LifecycleState = "invalid"
	// LifecycleUninstalled is the terminal state of a removed plugin; rows in
	// this state are deleted, so it appears only in the audit trail.
	LifecycleUninstalled LifecycleState = "uninstalled"
)

// LifecycleStates returns the closed set in declaration order.
func LifecycleStates() []LifecycleState {
	return []LifecycleState{LifecycleInstalled, LifecycleEnabled, LifecycleDisabled, LifecycleFailed, LifecycleInvalid, LifecycleUninstalled}
}

// Lifecycle transitions the operator may drive.
var (
	// ErrPluginNotFound is the 404 of the plugin API: no such id is installed.
	ErrPluginNotFound = errors.New("harnessext: plugin not found")
	// ErrPluginExists reports an install of an id that is already installed.
	ErrPluginExists = errors.New("harnessext: plugin is already installed")
	// ErrPluginState reports a transition the state machine does not allow.
	ErrPluginState = errors.New("harnessext: plugin state does not allow this action")
	// ErrPluginDependency reports a missing requirement (enable) or an enabled
	// dependent (disable/uninstall).
	ErrPluginDependency = errors.New("harnessext: plugin dependency")
	// ErrPluginKind reports a kind this build has no loader for.
	ErrPluginKind = errors.New("harnessext: plugin kind has no loader in this build")
	// ErrPluginInvalid reports a package that does not validate.
	ErrPluginInvalid = errors.New("harnessext: plugin package is invalid")
	// ErrPluginGate reports an engines.keel mismatch (the version gate).
	ErrPluginGate = errors.New("harnessext: plugin version gate")
	// ErrPluginBundled reports an action that does not apply to a plugin
	// compiled into the binary.
	ErrPluginBundled = errors.New("harnessext: bundled plugin")
	// ErrPluginPath reports a package location the manager refuses to touch.
	ErrPluginPath = errors.New("harnessext: plugin package path is not allowed")
	// ErrPluginTenant reports a lifecycle action whose caller names a tenant
	// other than the manager's. It is a cross-tenant refusal, never a lookup.
	ErrPluginTenant = errors.New("harnessext: plugin lifecycle belongs to another tenant")
	// ErrPluginHashMismatch reports a package whose content changed after the
	// approval was granted (or a caller-supplied expected hash that does not
	// match).
	ErrPluginHashMismatch = errors.New("harnessext: plugin package content does not match the approved hash")
)

// PluginError is the structured failure of a plugin action: a sentinel for the
// HTTP status plus the field-level detail a client renders. It never carries a
// credential (manifests hold none by construction).
type PluginError struct {
	// Op is the action: install, enable, disable, uninstall, update, boot.
	Op string `json:"op"`
	// ID is the plugin id the action was about (empty for an install whose
	// manifest never parsed).
	ID string `json:"id,omitempty"`
	// Message is the human-readable reason.
	Message string `json:"message"`
	// Fields are field-level validation errors, when the failure is about a
	// manifest or a request body.
	Fields []pluginpkg.FieldError `json:"fields,omitempty"`
	// Missing lists unsatisfied requirements (enable).
	Missing []string `json:"missing,omitempty"`
	// Dependents lists enabled plugins that require this one (disable/uninstall).
	Dependents []string `json:"dependents,omitempty"`
	// Diagnostics carries the runtime's own reasons for a failed load.
	Diagnostics []string `json:"diagnostics,omitempty"`
	// cause is the sentinel errors.Is matches.
	cause error
}

func (e *PluginError) Error() string {
	if e.ID == "" {
		return fmt.Sprintf("plugin %s: %s", e.Op, e.Message)
	}
	return fmt.Sprintf("plugin %s %s: %s", e.Op, e.ID, e.Message)
}

// Unwrap exposes the sentinel for errors.Is.
func (e *PluginError) Unwrap() error { return e.cause }

// NewPluginError builds a structured plugin failure: a sentinel (errors.Is)
// plus the field-level detail a client renders. Loaders and other layers use it
// to report a lifecycle failure in the same shape the manager does.
func NewPluginError(op, id string, cause error, format string, args ...any) *PluginError {
	return &PluginError{Op: op, ID: id, Message: fmt.Sprintf(format, args...), cause: cause}
}

func pluginErrf(cause error, op, id, format string, args ...any) *PluginError {
	return NewPluginError(op, id, cause, format, args...)
}

// Builtin is a plugin compiled into the binary.
type Builtin struct {
	// Manifest is the embedded plugin manifest (internal/pluginpkg/builtin).
	Manifest *pluginpkg.Manifest
	// Factory builds a fresh runtime plugin on every activation, so a
	// disable/enable cycle is a clean restart of the plugin's own state.
	Factory func() *Plugin
	// DefaultEnabled enables the plugin on first registration. A persisted
	// disable wins: the operator's decision is never overridden by a boot.
	DefaultEnabled bool
}

// PluginLoader materializes a runtime plugin for a kind that is not compiled
// in (sidecar/webview backends). The manager only requires that a loader exists
// for the kind before it enables such a plugin, so a host without one refuses
// with a readable reason instead of half-loading.
type PluginLoader interface {
	// Load builds the runtime plugin for a validated package.
	Load(ctx context.Context, pkg pluginpkg.Package, manifest *pluginpkg.Manifest) (*Plugin, error)
}

// RuntimeHealthReporter is an optional PluginLoader capability: a loader that
// supervises live resources (a sidecar process) reports which plugins' runtimes
// died under it. The manager consults it when it renders a plugin and when an
// enable arrives for a plugin whose process is gone: "the persisted intent says
// enabled, the truth says failed" is the same rule the runtime's own activation
// failures follow, and re-enabling is the reconnect.
type RuntimeHealthReporter interface {
	// RuntimeFailure reports the reason a loaded plugin's runtime died.
	RuntimeFailure(pluginID string) (reason string, failed bool)
}

// DenialSink is an optional PluginLoader capability: the loader hands the
// manager every permission denial it refuses at its own boundary (an undeclared
// sidecar tool), which turns into an audit row (spec P3).
type DenialSink interface {
	SetDenialSink(sink func(Denial))
}

// FailureSink is an optional PluginLoader capability: the loader reports the
// death of a runtime it supervises the moment it happens, so the audit trail
// carries the crash even though the plugin's persisted intent is unchanged
// (the view reports failed; the row still says what the operator asked for).
type FailureSink interface {
	SetFailureSink(sink func(pluginID, version, reason string))
}

// PluginPersistence is the persistence seam. *store.Store satisfies it
// verbatim; tests use an in-memory fake. Every inventory and audit operation is
// tenant-scoped: the manager is bound to one tenant and passes it explicitly,
// so a missed predicate cannot read another tenant's plugin state.
type PluginPersistence interface {
	UpsertPlugin(ctx context.Context, record store.PluginRecord) error
	GetPlugin(ctx context.Context, tenantID uuid.UUID, id string) (store.PluginRecord, bool, error)
	ListPlugins(ctx context.Context, tenantID uuid.UUID) ([]store.PluginRecord, error)
	DeletePlugin(ctx context.Context, tenantID uuid.UUID, id string) (bool, error)
	AppendPluginAudit(ctx context.Context, entry store.PluginAuditEntry) (store.PluginAuditEntry, error)
	ListPluginAudit(ctx context.Context, tenantID uuid.UUID, pluginID string, after int64, limit int) ([]store.PluginAuditEntry, error)
}

// ManagerOptions wires the plugin manager.
type ManagerOptions struct {
	// TenantID is the tenant this manager owns (required). A single-tenant
	// deployment passes its one tenant; the manager never has a
	// deployment-level mode.
	TenantID uuid.UUID
	// Root is the plugins root (the managed tree). Bundled plugins do not need
	// one, but install/enable of discovered kinds do; an empty root disables
	// installation and discovery.
	Root string
	// Runtime is the assembly engine (required).
	Runtime *Runtime
	// Persistence is the inventory/audit store. Nil keeps the manager in memory:
	// useful for a test host, never used by keel serve.
	Persistence PluginPersistence
	// Builtins are the compiled-in plugins.
	Builtins []Builtin
	// Loaders materialize non-compiled kinds (sidecar/webview backends).
	Loaders map[pluginpkg.Kind]PluginLoader
	// KeelVersion is the running keel version (the engines.keel gate).
	KeelVersion string
	Logger      *slog.Logger
	Now         func() time.Time
	// KnownHookEvent is the closed hook-event set (manifest validation).
	KnownHookEvent func(string) bool
	// AuditDetail augments every lifecycle audit row with plugin-specific,
	// already-sanitized facts (keel.telemetry reports signals/endpoint
	// host/sample ratio there; never header values or payloads). Nil adds none.
	AuditDetail func(pluginID string) map[string]any
}

// managerEntry is the manager's view of one plugin: intent state, package
// provenance and the factory/loader that materializes it.
type managerEntry struct {
	id             string
	state          LifecycleState
	manifest       *pluginpkg.Manifest
	rawManifest    json.RawMessage
	dir            string
	hash           string
	bundled        bool
	defaultEnabled bool
	source         pluginpkg.Source
	factory        func() *Plugin
	loader         PluginLoader
	diagnostics    []string
	problem        *pluginpkg.Problem
	shadowed       []pluginpkg.Package
	installedAt    time.Time
	updatedAt      time.Time
	enabledAt      *time.Time
	disabledAt     *time.Time
	// stateBeforeInvalid remembers the intent an invalid package had, so a
	// package that validates again restores it (a disable stays a disable).
	stateBeforeInvalid LifecycleState
	// extension is the plugin's UI extension declaration (slots, contributions,
	// extensibility.json), decoded from the package and validated at install.
	extension ExtensionDeclaration
}

// Manager is the plugin lifecycle authority: discovery, install, enable,
// disable, uninstall, update, audit — all through the harnessext Runtime.
type Manager struct {
	tenantID  uuid.UUID
	root      string
	runtime   *Runtime
	store     PluginPersistence
	loaders   map[pluginpkg.Kind]PluginLoader
	version   string
	log       *slog.Logger
	now       func() time.Time
	knownHook func(string) bool
	auditInfo func(string) map[string]any

	mu       sync.Mutex
	entries  map[string]*managerEntry
	revision int64
	// changeSink, when set, is told that the tenant's inventory changed. It is
	// the product's invalidation seam (GET /v1/identity/events kind=plugins); the
	// manager never blocks on it and never sends rows.
	changeSink func(uuid.UUID)
}

// SetChangeSink installs the product invalidation sink. It is set once at boot,
// after the API server exists; a nil sink is valid (tests, hosts without SSE).
func (m *Manager) SetChangeSink(sink func(uuid.UUID)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.changeSink = sink
}

// bumpRevisionLocked advances the inventory revision and publishes the
// invalidation fact. The caller holds m.mu; the sink is a non-blocking publish.
func (m *Manager) bumpRevisionLocked() {
	m.revision++
	if m.changeSink != nil {
		m.changeSink(m.tenantID)
	}
}

// TenantID is the tenant this manager owns. It is the same value the store
// filters by, so callers compare against it before serving a request.
func (m *Manager) TenantID() uuid.UUID { return m.tenantID }

// NewPluginManager builds the manager. It does not touch disk or the runtime:
// Boot does.
func NewPluginManager(opts ManagerOptions) (*Manager, error) {
	if opts.Runtime == nil {
		return nil, errors.New("harnessext: plugin manager needs the plugin runtime")
	}
	if opts.TenantID == uuid.Nil {
		return nil, errors.New("harnessext: plugin manager needs the owning tenant")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.KnownHookEvent == nil {
		opts.KnownHookEvent = ValidHookEvent
	}
	root := opts.Root
	if root != "" {
		absolute, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("harnessext: plugins root: %w", err)
		}
		root = absolute
	}
	loaders := make(map[pluginpkg.Kind]PluginLoader, len(opts.Loaders))
	for kind, loader := range opts.Loaders {
		if loader != nil {
			loaders[kind] = loader
		}
	}

	manager := &Manager{
		tenantID:  opts.TenantID,
		root:      root,
		runtime:   opts.Runtime,
		store:     opts.Persistence,
		loaders:   loaders,
		version:   opts.KeelVersion,
		log:       opts.Logger,
		now:       opts.Now,
		knownHook: opts.KnownHookEvent,
		auditInfo: opts.AuditDetail,
		entries:   make(map[string]*managerEntry),
	}
	// A loader that refuses permissions at its own boundary hands the manager
	// every denial, so refusals reach the audit trail even when the dispatch
	// point is inside the loader (spec P3). The same wiring carries a
	// supervised runtime's death into the trail.
	for _, loader := range loaders {
		if sink, ok := loader.(DenialSink); ok {
			sink.SetDenialSink(manager.recordDenial)
		}
		if sink, ok := loader.(FailureSink); ok {
			sink.SetFailureSink(manager.recordRuntimeFailure)
		}
	}
	for _, builtin := range opts.Builtins {
		if builtin.Manifest == nil {
			return nil, errors.New("harnessext: builtin plugin without a manifest")
		}
		manifest := builtin.Manifest
		manifest.Dir = ""
		if err := manifest.CheckEngines(manager.version); err != nil {
			return nil, fmt.Errorf("harnessext: builtin manifest %s: %w", manifest.ID, err)
		}
		manager.entries[manifest.ID] = &managerEntry{
			id:             manifest.ID,
			state:          LifecycleInstalled,
			manifest:       manifest,
			bundled:        true,
			defaultEnabled: builtin.DefaultEnabled,
			source:         pluginpkg.Source{Type: pluginpkg.SourceBuiltin},
			factory:        builtin.Factory,
		}
	}
	return manager, nil
}

func (m *Manager) environment() pluginpkg.Environment {
	return pluginpkg.Environment{KeelVersion: m.version, KnownHookEvent: m.knownHook}
}

// loaderFor resolves the loader that materializes an entry (nil for a builtin
// or a kind this build has no loader for).
func (m *Manager) loaderFor(entry *managerEntry) PluginLoader {
	if entry == nil {
		return nil
	}
	if entry.loader != nil {
		return entry.loader
	}
	return m.loaderForKind(entry.manifest)
}

// runtimeFailure asks the entry's loader whether the runtime it supervises has
// died, so a crashed sidecar is reported as failed while the persisted intent
// still says enabled. It never blocks: a loader that reports nothing answers
// immediately.
func (m *Manager) runtimeFailure(entry *managerEntry) (string, bool) {
	reporter, ok := m.loaderFor(entry).(RuntimeHealthReporter)
	if !ok {
		return "", false
	}
	return reporter.RuntimeFailure(entry.id)
}
