package agentext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/pluginpkg"
	"github.com/jixoai/keel/internal/store"
)

// AuditContext is who performed a lifecycle action (the audit row's tenant and
// actor).
type AuditContext struct {
	// TenantID is the calling tenant (nil for a boot-time action).
	TenantID *uuid.UUID
	// Actor is the operator label ("api", a user id, "boot").
	Actor string
}

// Boot reconciles the inventory with what is on disk and assembles the plugin
// set the database says should be enabled. It is the only place a plugin is
// loaded because of persisted intent, and it never overrides a persisted
// decision: a plugin disabled by an operator stays disabled across restarts.
func (m *Manager) Boot(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store != nil {
		rows, err := m.store.ListPlugins(ctx)
		if err != nil {
			return fmt.Errorf("agentext: read plugin inventory: %w", err)
		}
		for _, row := range rows {
			entry := &managerEntry{
				id:          row.ID,
				state:       LifecycleState(row.State),
				bundled:     row.Bundled,
				dir:         row.Path,
				hash:        row.ContentHash,
				diagnostics: append([]string(nil), row.Diagnostics...),
				installedAt: row.InstalledAt,
				updatedAt:   row.UpdatedAt,
				enabledAt:   row.EnabledAt,
				disabledAt:  row.DisabledAt,
			}
			if existing, ok := m.entries[row.ID]; ok {
				// The builtin registration wins for identity (factory, manifest);
				// the row wins for state.
				existing.state = entry.state
				existing.installedAt = entry.installedAt
				existing.updatedAt = entry.updatedAt
				existing.enabledAt = entry.enabledAt
				existing.disabledAt = entry.disabledAt
				if len(entry.diagnostics) > 0 && len(existing.diagnostics) == 0 {
					existing.diagnostics = entry.diagnostics
				}
				continue
			}
			entry.rawManifest = row.Manifest
			entry.source = decodeSource(row.Source)
			m.entries[row.ID] = entry
		}
	}
	if err := m.reconcileLocked(ctx); err != nil {
		return err
	}
	enabled := m.enabledIDsLocked()
	m.log.Info("plugin manager boot", "root", m.root, "plugins", len(m.entries), "enabled", len(enabled), "revision", m.revision)
	return nil
}

// Refresh re-runs discovery and reassembles from persisted intent. It is how a
// package dropped into the plugins root by hand becomes visible without a
// restart.
func (m *Manager) Refresh(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reconcileLocked(ctx)
}

// reconcileLocked folds the package root into the inventory, persists the
// result and reassembles the runtime. The caller holds m.mu.
func (m *Manager) reconcileLocked(ctx context.Context) error {
	now := m.now()
	scan := pluginpkg.Scan{}
	if m.root != "" {
		scan = pluginpkg.Discover(m.root, m.environment())
	}
	for _, problem := range scan.Errors {
		m.log.Warn("plugin discovery problem", "root", m.root, "err", problem)
	}

	// 1. Bundled plugins: refresh their metadata and register the row on first
	// boot with the plugin's default enablement. A persisted decision always
	// wins — a disable is never undone by a restart.
	for _, entry := range m.entries {
		if !entry.bundled {
			continue
		}
		entry.rawManifest = mustJSON(entry.manifest)
		entry.dir = ""
		entry.hash = ""
		entry.problem = nil
		entry.updatedAt = now
		if row, found := m.rowLocked(ctx, entry.id); found {
			entry.state = LifecycleState(row.State)
			entry.installedAt = row.InstalledAt
			entry.enabledAt = row.EnabledAt
			entry.disabledAt = row.DisabledAt
			if len(row.Diagnostics) > 0 {
				entry.diagnostics = append([]string(nil), row.Diagnostics...)
			}
		} else {
			entry.installedAt = now
			if entry.defaultEnabled {
				entry.state = LifecycleEnabled
				entry.enabledAt = timePointer(now)
			} else {
				entry.state = LifecycleInstalled
			}
			m.auditLocked(ctx, AuditContext{Actor: "boot"}, entry.id, entry.manifestVersion(), store.PluginActionDiscover, store.PluginResultOK,
				map[string]any{"bundled": true, "defaultEnabled": entry.defaultEnabled, "state": string(entry.state)})
		}
		if err := m.persistLocked(ctx, entry); err != nil {
			return err
		}
	}

	// 2. Discovered packages. The version conflict rule is the spec's: the
	// installed version wins, otherwise the highest version does.
	seen := make(map[string]bool, len(m.entries))
	for id, entry := range m.entries {
		if entry.bundled {
			seen[id] = true
		}
	}
	conflicts, single := scan.ResolveConflicts(func(id string) string {
		entry, ok := m.entries[id]
		if !ok {
			return ""
		}
		return entry.manifestVersion()
	})
	packages := append([]pluginpkg.Package(nil), single...)
	shadowedByID := make(map[string][]pluginpkg.Package)
	for _, conflict := range conflicts {
		packages = append(packages, conflict.Chosen)
		shadowedByID[conflict.ID] = conflict.Shadowed
	}
	sort.Slice(packages, func(i, j int) bool { return packages[i].ID() < packages[j].ID() })
	for _, pkg := range packages {
		id := pkg.ID()
		entry, ok := m.entries[id]
		if ok && entry.bundled {
			// A package on disk that claims a bundled id shadows nothing: the
			// compiled-in plugin is the registered one. Record the fact.
			entry.diagnostics = appendUnique(entry.diagnostics,
				fmt.Sprintf("package %s declares the bundled id %s and is ignored", pkg.Dir, id))
			seen[id] = true
			m.markIfChangedInPlaceLocked(ctx, entry, pkg)
			continue
		}
		if !ok {
			entry = &managerEntry{id: id, state: LifecycleInstalled, installedAt: now}
			m.entries[id] = entry
			m.auditLocked(ctx, AuditContext{Actor: "boot"}, id, pkg.Version(), store.PluginActionDiscover, store.PluginResultOK,
				map[string]any{"path": pkg.Dir, "hash": pkg.Hash})
		}
		previousHash := entry.hash
		previouslyInvalid := entry.state == LifecycleInvalid
		entry.manifest = pkg.Manifest
		entry.rawManifest = mustJSON(pkg.Manifest)
		entry.dir = pkg.Dir
		entry.hash = pkg.Hash
		entry.problem = nil
		entry.shadowed = shadowedByID[id]
		entry.source = sourceOf(pkg)
		entry.updatedAt = now
		seen[id] = true
		if previouslyInvalid {
			// The package validates again: restore the intent it had before it
			// went invalid (a disable stays a disable).
			entry.state = entry.stateBeforeInvalid
			if entry.state == "" {
				entry.state = LifecycleInstalled
			}
			entry.diagnostics = nil
			m.auditLocked(ctx, AuditContext{Actor: "boot"}, id, pkg.Version(), store.PluginActionDiscover, store.PluginResultOK,
				map[string]any{"recovered": true, "state": string(entry.state)})
		}
		if err := pkg.Manifest.CheckEngines(m.version); err != nil {
			entry.diagnostics = appendUnique(entry.diagnostics,
				fmt.Sprintf("version gate: %s does not admit this host (keel %s)", pkg.Manifest.Engines.Keel, m.version))
		}
		if previousHash != "" && previousHash != pkg.Hash {
			entry.diagnostics = appendUnique(entry.diagnostics,
				fmt.Sprintf("package content changed on disk (hash %s -> %s)", shortHash(previousHash), shortHash(pkg.Hash)))
			m.auditLocked(ctx, AuditContext{Actor: "boot"}, id, pkg.Version(), store.PluginActionDiscover, store.PluginResultOK,
				map[string]any{"contentChanged": true, "hash": pkg.Hash})
		}
		if err := m.persistLocked(ctx, entry); err != nil {
			return err
		}
	}

	// 3. Isolated invalid packages: a broken manifest is never fatal, and a
	// package that declares a known id marks that id invalid rather than
	// disappearing from the inventory.
	for _, pkg := range scan.Invalid {
		// The display id of a package whose manifest never parsed is the
		// directory name; only a grammar-legal id is persisted, so a junk
		// directory cannot write a junk row (the listing is recomputed from disk
		// on every reconcile anyway).
		id := pkg.ID()
		if id == "" {
			id = filepath.Base(pkg.Dir)
		}
		entry, ok := m.entries[id]
		if !ok {
			entry = &managerEntry{id: id, installedAt: now, state: LifecycleInstalled}
			m.entries[id] = entry
		} else if entry.bundled {
			entry.diagnostics = appendUnique(entry.diagnostics, "package at "+pkg.Dir+" is invalid: "+pkg.Problem.Reason)
			seen[id] = true
			continue
		}
		if entry.state != LifecycleInvalid {
			entry.stateBeforeInvalid = entry.state
		}
		entry.problem = pkg.Problem
		entry.dir = pkg.Dir
		entry.state = LifecycleInvalid
		entry.diagnostics = appendUnique(entry.diagnostics, pkg.Problem.Reason)
		entry.updatedAt = now
		seen[id] = true
		if !pluginpkg.ValidatePluginID(id) {
			continue
		}
		if err := m.persistLocked(ctx, entry); err != nil {
			return err
		}
	}

	// 4. Rows whose package is gone: bundled plugins never are; a managed plugin
	// whose directory disappeared is invalid with that reason (never silently
	// dropped — the audit trail must line up with the inventory).
	for id, entry := range m.entries {
		if entry.bundled || seen[id] || entry.rawManifest == nil || entry.problem != nil {
			continue
		}
		if _, err := os.Stat(entry.dir); err == nil {
			continue
		}
		if entry.state != LifecycleInvalid {
			entry.stateBeforeInvalid = entry.state
		}
		entry.problem = &pluginpkg.Problem{Path: entry.dir, Reason: "the plugin package directory is missing"}
		entry.state = LifecycleInvalid
		entry.diagnostics = appendUnique(entry.diagnostics, entry.problem.Reason)
		entry.updatedAt = now
		if err := m.persistLocked(ctx, entry); err != nil {
			return err
		}
	}

	failures, err := m.assembleLocked(ctx)
	if err != nil {
		return err
	}
	for id, failure := range failures {
		entry := m.entries[id]
		if entry == nil || entry.state != LifecycleEnabled {
			continue
		}
		entry.state = LifecycleFailed
		entry.diagnostics = appendUnique(entry.diagnostics, failure.Error())
		entry.updatedAt = m.now()
		m.auditLocked(ctx, AuditContext{Actor: "boot"}, id, entry.manifestVersion(), store.PluginActionEnable, store.PluginResultFailed,
			map[string]any{"reason": failure.Error()})
		if err := m.persistLocked(ctx, entry); err != nil {
			return err
		}
	}
	m.revision++
	return nil
}

// markIfChangedInPlaceLocked records a content change of a package whose id is
// claimed by a bundled plugin (the package is ignored either way).
func (m *Manager) markIfChangedInPlaceLocked(ctx context.Context, entry *managerEntry, pkg pluginpkg.Package) {
	if entry.hash == pkg.Hash {
		return
	}
	entry.hash = pkg.Hash
	entry.dir = pkg.Dir
	entry.updatedAt = m.now()
	m.auditLocked(ctx, AuditContext{Actor: "boot"}, entry.id, pkg.Version(), store.PluginActionDiscover, store.PluginResultOK,
		map[string]any{"ignoredPackage": pkg.Dir, "hash": pkg.Hash})
}

// rowLocked reads the persisted row of an id (used to tell "never installed"
// from "installed and disabled").
func (m *Manager) rowLocked(ctx context.Context, id string) (store.PluginRecord, bool) {
	if m.store == nil {
		return store.PluginRecord{}, false
	}
	row, found, err := m.store.GetPlugin(ctx, id)
	if err != nil {
		m.log.Warn("plugin inventory read failed", "plugin", id, "err", err)
		return store.PluginRecord{}, false
	}
	return row, found
}

// assembleLocked builds the runtime plugin set from the enabled entries and
// applies it. It is the only place the runtime is told what to load.
//
// Materialization failures (no factory, no loader, a loader that refused) are
// returned per id: the assembly simply does not contain that plugin, and the
// caller decides what the failure means for the entry's state. The runtime's own
// activation failures (dependency waits, tool conflicts) are visible in the
// snapshot afterwards.
func (m *Manager) assembleLocked(ctx context.Context) (map[string]error, error) {
	plugins := make([]*Plugin, 0, len(m.entries))
	failures := make(map[string]error)
	for _, id := range m.enabledIDsLocked() {
		entry := m.entries[id]
		if !entry.hasRuntimeSurface() {
			// A UI-only webview plugin contributes nothing to the in-process
			// runtime: enabling it gates its hosted UI, nothing else.
			continue
		}
		plugin, err := m.materializeLocked(ctx, entry)
		if err != nil {
			failures[id] = err
			continue
		}
		plugins = append(plugins, plugin)
	}
	if err := m.runtime.ApplyPlugins(ctx, plugins); err != nil {
		return failures, fmt.Errorf("agentext: assemble plugins: %w", err)
	}
	return failures, nil
}

// materializeLocked builds the runtime plugin for one entry: the builtin
// factory, or the loader registered for its kind.
func (m *Manager) materializeLocked(ctx context.Context, entry *managerEntry) (*Plugin, error) {
	if entry.manifest == nil {
		return nil, pluginErrf(ErrPluginInvalid, "assemble", entry.id, "the plugin manifest is not available")
	}
	var (
		plugin *Plugin
		err    error
	)
	switch {
	case entry.factory != nil:
		plugin = entry.factory()
	case entry.loader != nil:
		plugin, err = entry.loader.Load(ctx, pluginpkg.Package{Dir: entry.dir, Manifest: entry.manifest, Hash: entry.hash}, entry.manifest)
		if err != nil {
			return nil, pluginErrf(ErrPluginKind, "assemble", entry.id, "loader failed: %v", err)
		}
	default:
		loader := m.loaderForKind(entry.manifest)
		if loader == nil {
			return nil, pluginErrf(ErrPluginKind, "assemble", entry.id,
				"kind %s has no loader in this build (%s)", entry.manifest.Kind, entry.kindHint())
		}
		plugin, err = loader.Load(ctx, pluginpkg.Package{Dir: entry.dir, Manifest: entry.manifest, Hash: entry.hash}, entry.manifest)
		if err != nil {
			return nil, pluginErrf(ErrPluginKind, "assemble", entry.id, "loader failed: %v", err)
		}
	}
	if plugin == nil {
		return nil, pluginErrf(ErrPluginKind, "assemble", entry.id, "the %s loader returned no plugin", entry.manifest.Kind)
	}
	if plugin.ID == "" {
		plugin.ID = entry.id
	}
	if plugin.ID != entry.id {
		return nil, pluginErrf(ErrPluginInvalid, "assemble", entry.id, "the loaded plugin reports id %q", plugin.ID)
	}
	if plugin.Version == "" {
		plugin.Version = entry.manifest.Version
	}
	plugin.Requires = translateRequires(entry.manifest.Requires, plugin.Requires)
	plugin.Provides = dedupeStrings(append([]string{"plugin:" + entry.id}, plugin.Provides...))
	return plugin, nil
}

// loaderForKind resolves the loader of a package by its manifest. A webview that
// declares an `entry.command` is loaded by the sidecar loader: the spec makes a
// webview's optional backend a sidecar ("webview 可带 sidecar 后端"), so a host
// registers that one loader and both kinds use it. Without the fallback a host
// that wires only `KindSidecar` (the production wiring) could never enable a
// webview package with a backend.
func (m *Manager) loaderForKind(manifest *pluginpkg.Manifest) PluginLoader {
	if manifest == nil {
		return nil
	}
	if loader, ok := m.loaders[manifest.Kind]; ok {
		return loader
	}
	if manifest.Kind == pluginpkg.KindWebview && len(manifest.Entry.Command) > 0 {
		return m.loaders[pluginpkg.KindSidecar]
	}
	return nil
}

// kindHint explains what to do about a missing loader, per kind.
func (entry *managerEntry) kindHint() string {
	if entry.manifest == nil {
		return "no manifest"
	}
	switch entry.manifest.Kind {
	case pluginpkg.KindInProcess:
		return "in-process plugins are compiled into the binary; a package of this kind cannot be loaded at runtime"
	case pluginpkg.KindSidecar:
		return "the sidecar loader (agentext.NewSidecarLoader) is not wired into this deployment's ManagerOptions.Loaders"
	case pluginpkg.KindWebview:
		if len(entry.manifest.Entry.Command) > 0 {
			return "this webview package declares a sidecar backend, which needs the sidecar loader (agentext.NewSidecarLoader) registered under kind sidecar"
		}
		return "a UI-only webview plugin has no runtime surface to load"
	default:
		return "unknown kind"
	}
}

// hasRuntimeSurface reports whether a plugin contributes anything to the
// in-process runtime. A UI-only webview package does not: its enablement is a
// lifecycle fact (it gates the hosted UI under /plugins/<id>) with no assembly
// entry, and the spec's "disable revokes every registration" is trivially true
// for it — there is nothing registered.
func (entry *managerEntry) hasRuntimeSurface() bool {
	if entry.factory != nil {
		return true
	}
	if entry.manifest == nil {
		return false
	}
	switch entry.manifest.Kind {
	case pluginpkg.KindInProcess, pluginpkg.KindSidecar:
		return true
	case pluginpkg.KindWebview:
		return len(entry.manifest.Entry.Command) > 0
	default:
		return false
	}
}

// translateRequires converts manifest requirements into the runtime's
// capability vocabulary: a bare id means "the plugin whose id this is"
// (plugin:<id>), a colon-bearing token is a service capability passed through
// (cap:fs, tool:whatever — the plugin's own Provides).
func translateRequires(requires []string, existing []string) []string {
	out := make([]string, 0, len(requires)+len(existing))
	for _, requirement := range requires {
		if strings.Contains(requirement, ":") {
			out = append(out, requirement)
			continue
		}
		out = append(out, "plugin:"+requirement)
	}
	out = append(out, existing...)
	return dedupeStrings(out)
}

// enabledIDsLocked returns the ids the database says should be loaded, sorted.
func (m *Manager) enabledIDsLocked() []string {
	out := make([]string, 0, len(m.entries))
	for id, entry := range m.entries {
		if entry.state == LifecycleEnabled && entry.manifest != nil {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// --- actions --------------------------------------------------------------

// InstallRequest is POST /v1/plugins/install.
type InstallRequest struct {
	// Path is the local package directory to install from. The content is
	// validated, hashed and copied into the managed root; the stored plugin never
	// points at the source directory.
	Path string
}

// Install validates a package and copies it into the managed root.
func (m *Manager) Install(ctx context.Context, req InstallRequest, audit AuditContext) (PluginView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, pkg, err := m.stagePackageLocked(req.Path, "")
	if err != nil {
		id := pkg.ID()
		if id == "" {
			id = packageID(req.Path)
		}
		m.auditLocked(ctx, audit, id, pkg.Version(), store.PluginActionInstall, store.PluginResultFailed, detailOf(err))
		return PluginView{}, err
	}
	if existing, ok := m.entries[entry.id]; ok {
		err := pluginErrf(ErrPluginExists, "install", entry.id,
			"plugin %s %s is already installed (%s); use update to replace it", entry.id, existing.manifestVersion(), existing.state)
		m.auditLocked(ctx, audit, entry.id, pkg.Version(), store.PluginActionInstall, store.PluginResultRefused, detailOf(err))
		return PluginView{}, err
	}
	if err := m.commitPackageLocked(entry, pkg); err != nil {
		m.auditLocked(ctx, audit, entry.id, pkg.Version(), store.PluginActionInstall, store.PluginResultFailed, detailOf(err))
		return PluginView{}, err
	}
	now := m.now()
	entry.state = LifecycleInstalled
	entry.installedAt = now
	entry.updatedAt = now
	entry.rawManifest = mustJSON(entry.manifest)
	entry.source = m.sourceOf(entry, req.Path)
	m.entries[entry.id] = entry
	if err := m.persistLocked(ctx, entry); err != nil {
		return PluginView{}, err
	}
	m.auditLocked(ctx, audit, entry.id, entry.manifestVersion(), store.PluginActionInstall, store.PluginResultOK,
		map[string]any{"path": entry.dir, "hash": entry.hash, "source": req.Path, "bundled": false})
	m.revision++
	m.log.Info("plugin installed", "plugin", entry.id, "version", entry.manifestVersion(), "hash", shortHash(entry.hash), "actor", audit.Actor)
	return m.viewLocked(entry), nil
}

// stagePackageLocked loads and gates a package from a source directory. It does
// not touch the managed root beyond reading: the caller commits (copy) or
// discards. expectedID, when set, is the id the package must declare (update).
func (m *Manager) stagePackageLocked(path, expectedID string) (*managerEntry, pluginpkg.Package, error) {
	if strings.TrimSpace(path) == "" {
		return nil, pluginpkg.Package{}, &PluginError{Op: actionFor(expectedID), Message: "path is required", cause: ErrPluginPath,
			Fields: []pluginpkg.FieldError{{Field: "path", Message: "path is required"}}}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, pluginpkg.Package{}, pluginErrf(ErrPluginPath, actionFor(expectedID), "", "resolve path %q: %v", path, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, pluginpkg.Package{}, &PluginError{Op: actionFor(expectedID), Message: "read path: " + err.Error(), cause: ErrPluginPath,
			Fields: []pluginpkg.FieldError{{Field: "path", Message: "directory does not exist: " + err.Error()}}}
	}
	if !info.IsDir() {
		return nil, pluginpkg.Package{}, &PluginError{Op: actionFor(expectedID), Message: "path is not a directory", cause: ErrPluginPath,
			Fields: []pluginpkg.FieldError{{Field: "path", Message: "path is not a directory"}}}
	}
	pkg := pluginpkg.LoadDir(absolute, m.environment())
	if !pkg.Usable() {
		problem := pkg.Problem
		fields := problem.Fields
		if len(fields) == 0 {
			fields = []pluginpkg.FieldError{{Field: "", Message: problem.Reason}}
		}
		return nil, pkg, &PluginError{Op: actionFor(expectedID), ID: pkg.ID(), Message: problem.Reason, Fields: fields, cause: ErrPluginInvalid}
	}
	if expectedID != "" && pkg.ID() != expectedID {
		return nil, pkg, &PluginError{Op: "update", ID: pkg.ID(), Message: "package declares a different plugin id", cause: ErrPluginInvalid,
			Fields: []pluginpkg.FieldError{{Field: "id", Message: fmt.Sprintf("package declares id %q, expected %q", pkg.ID(), expectedID)}}}
	}
	if err := pkg.Manifest.CheckEngines(m.version); err != nil {
		return nil, pkg, &PluginError{Op: actionFor(expectedID), ID: pkg.ID(), Message: err.Error(), Fields: pluginpkg.FieldErrors(err), cause: ErrPluginGate}
	}
	entry := &managerEntry{
		id:       pkg.ID(),
		manifest: pkg.Manifest,
		hash:     pkg.Hash,
		dir:      pkg.Dir,
		state:    LifecycleInstalled,
	}
	return entry, pkg, nil
}

func actionFor(expectedID string) string {
	if expectedID == "" {
		return "install"
	}
	return "update"
}

// commitPackageLocked copies a staged package into the managed root (unless it
// already is the managed directory) and verifies the copy hashes identically.
func (m *Manager) commitPackageLocked(entry *managerEntry, pkg pluginpkg.Package) error {
	if m.root == "" {
		return pluginErrf(ErrPluginPath, "install", entry.id, "this deployment has no plugins root configured")
	}
	if err := os.MkdirAll(m.root, 0o755); err != nil {
		return pluginErrf(ErrPluginPath, "install", entry.id, "create plugins root: %v", err)
	}
	dest := pluginpkg.PackageDirFor(m.root, entry.id)
	if filepath.Clean(pkg.Dir) == filepath.Clean(dest) {
		// The operator dropped the package into its managed directory already:
		// nothing to copy, the hash above is the managed content.
		entry.dir = dest
		entry.hash = pkg.Hash
		return nil
	}
	if _, err := os.Stat(dest); err == nil {
		return pluginErrf(ErrPluginExists, "install", entry.id, "the managed directory %s already exists", dest)
	}
	staging := filepath.Join(m.root, fmt.Sprintf(".staging-%s-%d", entry.id, m.now().UnixNano()))
	if err := pluginpkg.CopyTree(pkg.Dir, staging); err != nil {
		_ = os.RemoveAll(staging)
		return pluginErrf(ErrPluginPath, "install", entry.id, "copy package: %v", err)
	}
	copied := pluginpkg.LoadDir(staging, m.environment())
	if !copied.Usable() || copied.Hash != pkg.Hash {
		_ = os.RemoveAll(staging)
		return pluginErrf(ErrPluginPath, "install", entry.id,
			"the copied package does not hash like its source (%s vs %s)", copied.Hash, pkg.Hash)
	}
	if err := os.Rename(staging, dest); err != nil {
		_ = os.RemoveAll(staging)
		return pluginErrf(ErrPluginPath, "install", entry.id, "move package into place: %v", err)
	}
	entry.dir = dest
	entry.hash = copied.Hash
	entry.manifest = copied.Manifest
	return nil
}

// Enable loads a plugin through the runtime's dependency topology. A failure
// leaves the previous assembly untouched (no half-loaded plugin) and records
// why.
func (m *Manager) Enable(ctx context.Context, id string, audit AuditContext) (PluginView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[id]
	if !ok {
		return PluginView{}, pluginErrf(ErrPluginNotFound, "enable", id, "plugin is not installed")
	}
	if entry.state == LifecycleEnabled {
		// A plugin whose loaded runtime died (a crashed sidecar) still says
		// enabled in the database, but it is not serving: enabling it again is
		// the reconnect the spec calls for, not a no-op.
		if reason, failed := m.runtimeFailure(entry); failed {
			m.log.Warn("plugin enable reconnects a failed runtime", "plugin", id, "reason", reason)
		} else {
			return m.viewLocked(entry), pluginErrf(ErrPluginState, "enable", id, "plugin is already enabled")
		}
	}
	if entry.manifest == nil {
		// A row whose package never validated: nothing to load, and the reason is
		// the stored problem.
		view := m.viewLocked(entry)
		return view, pluginErrf(ErrPluginInvalid, "enable", id, "the plugin manifest is not available")
	}
	if entry.state == LifecycleInvalid || entry.problem != nil {
		reason := "the package is invalid"
		if entry.problem != nil {
			reason = entry.problem.Reason
		}
		view := m.viewLocked(entry)
		return view, &PluginError{Op: "enable", ID: id, Message: reason, Fields: problemFields(entry.problem), cause: ErrPluginInvalid}
	}
	if err := entry.manifest.CheckEngines(m.version); err != nil {
		return m.viewLocked(entry), &PluginError{Op: "enable", ID: id, Message: err.Error(), Fields: pluginpkg.FieldErrors(err), cause: ErrPluginGate}
	}
	if missing := m.missingRequirementsLocked(entry, id); len(missing) > 0 {
		view := m.viewLocked(entry)
		view.Diagnostics = appendUnique(view.Diagnostics, "missing dependencies: "+strings.Join(missing, ", "))
		err := &PluginError{Op: "enable", ID: id, Message: "unsatisfied requirements: " + strings.Join(missing, ", "), Missing: missing, cause: ErrPluginDependency}
		m.auditLocked(ctx, audit, id, entry.manifestVersion(), store.PluginActionEnable, store.PluginResultRefused, detailOf(err))
		return view, err
	}
	uiOnly := !entry.hasRuntimeSurface()
	previous := entry.state
	entry.state = LifecycleEnabled
	applied, materializeFailures, err := m.applyEnabledLocked(ctx)
	if err != nil {
		entry.state = previous
		return m.viewLocked(entry), err
	}
	status, loaded := applied[id]
	if failure, failed := materializeFailures[id]; failed {
		// The plugin could not even be materialized (no factory, no loader): the
		// assembly does not contain it, so this is not a half-load.
		entry.state = previous
		if _, _, rollbackErr := m.applyEnabledLocked(ctx); rollbackErr != nil {
			return m.viewLocked(entry), fmt.Errorf("agentext: enable %s: rollback: %w", id, rollbackErr)
		}
		entry.state = LifecycleFailed
		entry.diagnostics = appendUnique(entry.diagnostics, failure.Error())
		if err := m.persistLocked(ctx, entry); err != nil {
			return m.viewLocked(entry), err
		}
		m.auditLocked(ctx, audit, id, entry.manifestVersion(), store.PluginActionEnable, store.PluginResultFailed, detailOf(failure))
		m.revision++
		return m.viewLocked(entry), failure
	}
	if !uiOnly && (!loaded || status.State != PluginActive) {
		// Roll back: re-assemble without the plugin, then report the runtime's own
		// reason. Nothing is left half-loaded.
		entry.state = previous
		if _, _, rollbackErr := m.applyEnabledLocked(ctx); rollbackErr != nil {
			return m.viewLocked(entry), fmt.Errorf("agentext: enable %s: rollback: %w", id, rollbackErr)
		}
		diagnostics := append([]string(nil), status.Diagnostics...)
		state := LifecycleFailed
		message := "the plugin did not reach the active state"
		if len(diagnostics) > 0 {
			message = diagnostics[len(diagnostics)-1]
		}
		if status.State == PluginPending {
			// A pending plugin is not half-loaded, it is not loaded at all: the
			// honest state is disabled with the reason.
			state = LifecycleDisabled
		}
		entry.state = state
		if state == LifecycleDisabled {
			entry.disabledAt = timePointer(m.now())
		}
		failure := &PluginError{Op: "enable", ID: id, Message: message, Diagnostics: diagnostics, cause: ErrPluginDependency}
		if state == LifecycleFailed {
			failure.cause = nil
			failure.Message = "the plugin failed to load: " + message
		}
		entry.diagnostics = appendUnique(entry.diagnostics, message)
		if err := m.persistLocked(ctx, entry); err != nil {
			return m.viewLocked(entry), err
		}
		m.auditLocked(ctx, audit, id, entry.manifestVersion(), store.PluginActionEnable, store.PluginResultFailed,
			map[string]any{"state": string(state), "diagnostics": diagnostics})
		m.revision++
		return m.viewLocked(entry), failure
	}
	entry.enabledAt = timePointer(m.now())
	entry.disabledAt = nil
	entry.diagnostics = nil
	entry.updatedAt = m.now()
	if err := m.persistLocked(ctx, entry); err != nil {
		return m.viewLocked(entry), err
	}
	surface := "runtime"
	if uiOnly {
		surface = "ui"
	}
	m.auditLocked(ctx, audit, id, entry.manifestVersion(), store.PluginActionEnable, store.PluginResultOK,
		map[string]any{"surface": surface, "generation": m.runtime.Generation()})
	m.revision++
	m.log.Info("plugin enabled", "plugin", id, "surface", surface, "generation", m.runtime.Generation(), "actor", audit.Actor)
	return m.viewLocked(entry), nil
}

// Disable unloads a plugin and every registration it made. It refuses when an
// enabled plugin depends on it, so nothing is left pending by a disable.
func (m *Manager) Disable(ctx context.Context, id string, audit AuditContext) (PluginView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[id]
	if !ok {
		return PluginView{}, pluginErrf(ErrPluginNotFound, "disable", id, "plugin is not installed")
	}
	if entry.state != LifecycleEnabled && entry.state != LifecycleFailed {
		return m.viewLocked(entry), pluginErrf(ErrPluginState, "disable", id, "plugin is %s, not enabled", entry.state)
	}
	if dependents := m.enabledDependentsLocked(id); len(dependents) > 0 {
		err := &PluginError{Op: "disable", ID: id,
			Message:    "enabled plugins depend on this one; disable them first: " + strings.Join(dependents, ", "),
			Dependents: dependents, cause: ErrPluginDependency}
		m.auditLocked(ctx, audit, id, entry.manifestVersion(), store.PluginActionDisable, store.PluginResultRefused, detailOf(err))
		return m.viewLocked(entry), err
	}
	previous := entry.state
	entry.state = LifecycleDisabled
	if _, _, err := m.applyEnabledLocked(ctx); err != nil {
		entry.state = previous
		return m.viewLocked(entry), err
	}
	entry.disabledAt = timePointer(m.now())
	entry.updatedAt = m.now()
	entry.diagnostics = nil
	if err := m.persistLocked(ctx, entry); err != nil {
		return m.viewLocked(entry), err
	}
	m.auditLocked(ctx, audit, id, entry.manifestVersion(), store.PluginActionDisable, store.PluginResultOK, nil)
	m.revision++
	m.log.Info("plugin disabled", "plugin", id, "generation", m.runtime.Generation(), "actor", audit.Actor)
	return m.viewLocked(entry), nil
}

// Uninstall removes a disabled plugin: its row, its managed directory and its
// state. An enabled plugin must be disabled first (the state machine is
// explicit about the order, and a live plugin must not be deleted underneath
// the runtime).
func (m *Manager) Uninstall(ctx context.Context, id string, audit AuditContext) (PluginView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[id]
	if !ok {
		return PluginView{}, pluginErrf(ErrPluginNotFound, "uninstall", id, "plugin is not installed")
	}
	if entry.bundled {
		err := pluginErrf(ErrPluginBundled, "uninstall", id, "bundled plugins ship with the binary: disable it instead")
		m.auditLocked(ctx, audit, id, entry.manifestVersion(), store.PluginActionUninstall, store.PluginResultRefused, detailOf(err))
		return m.viewLocked(entry), err
	}
	if entry.state == LifecycleEnabled {
		err := pluginErrf(ErrPluginState, "uninstall", id, "disable the plugin before uninstalling it")
		m.auditLocked(ctx, audit, id, entry.manifestVersion(), store.PluginActionUninstall, store.PluginResultRefused, detailOf(err))
		return m.viewLocked(entry), err
	}
	if dependents := m.enabledDependentsLocked(id); len(dependents) > 0 {
		err := &PluginError{Op: "uninstall", ID: id,
			Message:    "enabled plugins depend on this one; disable them first: " + strings.Join(dependents, ", "),
			Dependents: dependents, cause: ErrPluginDependency}
		m.auditLocked(ctx, audit, id, entry.manifestVersion(), store.PluginActionUninstall, store.PluginResultRefused, detailOf(err))
		return m.viewLocked(entry), err
	}
	view := m.viewLocked(entry)
	if entry.dir != "" {
		if m.root == "" || !pluginpkg.ManagedDir(m.root, entry.dir) {
			return view, pluginErrf(ErrPluginPath, "uninstall", id, "refuse to remove %s: it is outside the plugins root", entry.dir)
		}
		if err := pluginpkg.RemoveTree(m.root, entry.dir); err != nil {
			m.auditLocked(ctx, audit, id, entry.manifestVersion(), store.PluginActionUninstall, store.PluginResultFailed,
				map[string]any{"error": err.Error()})
			return view, pluginErrf(ErrPluginPath, "uninstall", id, "remove package: %v", err)
		}
	}
	// Versions archived by an update go with the plugin: uninstall means "remove
	// it", not "keep its history on disk".
	if m.root != "" {
		archive := filepath.Join(m.root, ".versions", id)
		if _, err := os.Stat(archive); err == nil {
			if err := os.RemoveAll(archive); err != nil {
				m.log.Warn("plugin uninstall: removing archived versions failed", "plugin", id, "err", err)
			}
		}
	}
	if m.store != nil {
		found, err := m.store.DeletePlugin(ctx, id)
		if err != nil {
			return view, err
		}
		if !found && entry.dir == "" {
			// Nothing was ever persisted and there is no package: the id is not
			// installed, whatever the caller saw.
			return view, pluginErrf(ErrPluginNotFound, "uninstall", id, "the plugin row is already gone")
		}
	}
	delete(m.entries, id)
	if _, _, err := m.applyEnabledLocked(ctx); err != nil {
		return view, err
	}
	m.auditLocked(ctx, audit, id, view.Version, store.PluginActionUninstall, store.PluginResultOK,
		map[string]any{"removed": entry.dir != ""})
	m.revision++
	m.log.Info("plugin uninstalled", "plugin", id, "actor", audit.Actor)
	return view, nil
}

// applyEnabledLocked reassembles the runtime from the currently enabled
// entries; callers mutate entry.state first and roll it back if this fails. It
// returns the runtime status per plugin id plus the per-id materialization
// failures.
func (m *Manager) applyEnabledLocked(ctx context.Context) (map[string]PluginStatus, map[string]error, error) {
	failures, err := m.assembleLocked(ctx)
	if err != nil {
		return nil, failures, err
	}
	statuses := make(map[string]PluginStatus)
	for _, status := range m.runtime.Snapshot().Plugins {
		statuses[status.ID] = status
	}
	return statuses, failures, nil
}

// missingRequirementsLocked resolves the manifest's requires against the
// enabled set (plus the plugin being enabled).
func (m *Manager) missingRequirementsLocked(entry *managerEntry, self string) []string {
	var missing []string
	for _, requirement := range entry.manifest.Requires {
		if requirement == self {
			continue
		}
		if strings.Contains(requirement, ":") {
			if !m.capabilityProvidedLocked(requirement, self) {
				missing = append(missing, requirement)
			}
			continue
		}
		dependency, ok := m.entries[requirement]
		if !ok || (dependency.state != LifecycleEnabled && dependency.id != self) {
			missing = append(missing, requirement)
		}
	}
	return missing
}

// capabilityProvidedLocked reports whether any enabled plugin declares the
// capability token (a service id like cap:fs, or a plugin's own Provides).
func (m *Manager) capabilityProvidedLocked(capability, self string) bool {
	for id, entry := range m.entries {
		if id == self || entry.state != LifecycleEnabled || entry.manifest == nil {
			continue
		}
		if strings.HasPrefix(capability, "plugin:") && "plugin:"+id == capability {
			return true
		}
		if entry.factory != nil {
			plugin := entry.factory()
			for _, provided := range plugin.Provides {
				if provided == capability {
					return true
				}
			}
			for _, tool := range plugin.Tools {
				if "tool:"+tool.Name == capability {
					return true
				}
			}
		}
	}
	return false
}

// enabledDependentsLocked returns every enabled plugin that (transitively)
// requires id, so a disable or uninstall can refuse with the full list.
func (m *Manager) enabledDependentsLocked(id string) []string {
	found := map[string]bool{}
	frontier := []string{id}
	for len(frontier) > 0 {
		current := frontier[0]
		frontier = frontier[1:]
		for otherID, entry := range m.entries {
			if otherID == id || entry.state != LifecycleEnabled || entry.manifest == nil || found[otherID] {
				continue
			}
			for _, requirement := range entry.manifest.Requires {
				if requirement == current || (strings.Contains(requirement, ":") && requirement == "plugin:"+current) {
					found[otherID] = true
					frontier = append(frontier, otherID)
					break
				}
			}
		}
	}
	out := make([]string, 0, len(found))
	for dependent := range found {
		out = append(out, dependent)
	}
	sort.Strings(out)
	return out
}

// UpdateRequest is POST /v1/plugins/{id}/update.
type UpdateRequest struct {
	// Path is a new package directory. Empty picks the highest version different
	// from the installed one from the plugins root (the multi-version layout).
	Path string
}

// Update replaces a plugin's version: disable, swap the package, enable.
// A failure restores the previous version and its state.
func (m *Manager) Update(ctx context.Context, id string, req UpdateRequest, audit AuditContext) (PluginView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[id]
	if !ok {
		return PluginView{}, pluginErrf(ErrPluginNotFound, "update", id, "plugin is not installed")
	}
	if entry.bundled {
		err := pluginErrf(ErrPluginBundled, "update", id, "bundled plugins are updated with the binary, not from a package")
		m.auditLocked(ctx, audit, id, entry.manifestVersion(), store.PluginActionUpdate, store.PluginResultRefused, detailOf(err))
		return m.viewLocked(entry), err
	}
	staged, pkg, err := m.stageUpdateLocked(id, req.Path)
	if err != nil {
		m.auditLocked(ctx, audit, id, entry.manifestVersion(), store.PluginActionUpdate, store.PluginResultFailed, detailOf(err))
		return m.viewLocked(entry), err
	}
	previous := *entry
	wasEnabled := entry.state == LifecycleEnabled || entry.state == LifecycleFailed
	if wasEnabled {
		entry.state = LifecycleDisabled
		if _, _, err := m.applyEnabledLocked(ctx); err != nil {
			entry.state = previous.state
			return m.viewLocked(entry), err
		}
	}
	archived, err := m.swapPackageLocked(entry, staged, pkg)
	if err != nil {
		entry.state = previous.state
		rollbackView, rollbackErr := m.restoreLocked(ctx, entry, previous, archived, wasEnabled)
		if rollbackErr != nil {
			return rollbackView, fmt.Errorf("agentext: update %s: %w (rollback: %v)", id, err, rollbackErr)
		}
		m.auditLocked(ctx, audit, id, previous.manifestVersion(), store.PluginActionUpdate, store.PluginResultFailed, detailOf(err))
		return rollbackView, err
	}
	entry.updatedAt = m.now()
	entry.problem = nil
	entry.diagnostics = nil
	entry.disabledAt = timePointer(m.now())
	if err := m.persistLocked(ctx, entry); err != nil {
		return m.viewLocked(entry), err
	}
	uiOnly := !entry.hasRuntimeSurface()
	if wasEnabled {
		entry.state = LifecycleEnabled
		applied, materializeFailures, err := m.applyEnabledLocked(ctx)
		if err != nil {
			entry.state = previous.state
			return m.viewLocked(entry), err
		}
		if failure, failed := materializeFailures[id]; failed {
			entry.state = previous.state
			rollbackView, rollbackErr := m.restoreLocked(ctx, entry, previous, archived, wasEnabled)
			if rollbackErr != nil {
				return rollbackView, fmt.Errorf("agentext: update %s: %v (rollback: %v)", id, failure, rollbackErr)
			}
			m.auditLocked(ctx, audit, id, previous.manifestVersion(), store.PluginActionUpdate, store.PluginResultFailed, detailOf(failure))
			return rollbackView, failure
		}
		status, loaded := applied[id]
		if !uiOnly && (!loaded || status.State != PluginActive) {
			// Roll back to the old version (package and state).
			message := "the updated plugin did not reach the active state"
			if len(status.Diagnostics) > 0 {
				message = status.Diagnostics[len(status.Diagnostics)-1]
			}
			entry.state = previous.state
			rollbackView, rollbackErr := m.restoreLocked(ctx, entry, previous, archived, wasEnabled)
			failure := &PluginError{Op: "update", ID: id, Message: message, Diagnostics: status.Diagnostics, cause: ErrPluginDependency}
			if rollbackErr != nil {
				return rollbackView, fmt.Errorf("agentext: update %s: %s (rollback: %v)", id, message, rollbackErr)
			}
			m.auditLocked(ctx, audit, id, previous.manifestVersion(), store.PluginActionUpdate, store.PluginResultFailed,
				map[string]any{"from": previous.manifestVersion(), "to": entry.manifestVersion(), "rolledBack": true, "diagnostics": status.Diagnostics})
			return rollbackView, failure
		}
	}
	entry.state = wasEnabledState(wasEnabled, previous.state)
	entry.updatedAt = m.now()
	if entry.state == LifecycleEnabled {
		entry.enabledAt = timePointer(m.now())
		entry.disabledAt = nil
	}
	if err := m.persistLocked(ctx, entry); err != nil {
		return m.viewLocked(entry), err
	}
	m.auditLocked(ctx, audit, id, entry.manifestVersion(), store.PluginActionUpdate, store.PluginResultOK,
		map[string]any{"from": previous.manifestVersion(), "to": entry.manifestVersion(), "hash": entry.hash, "enabled": entry.state == LifecycleEnabled})
	m.revision++
	m.log.Info("plugin updated", "plugin", id, "from", previous.manifestVersion(), "to", entry.manifestVersion(), "actor", audit.Actor)
	return m.viewLocked(entry), nil
}

func wasEnabledState(wasEnabled bool, previous LifecycleState) LifecycleState {
	if wasEnabled {
		return LifecycleEnabled
	}
	return previous
}

// stageUpdateLocked resolves the replacement package (explicit path or the
// highest other version under the root).
func (m *Manager) stageUpdateLocked(id, path string) (*managerEntry, pluginpkg.Package, error) {
	if strings.TrimSpace(path) != "" {
		return m.stagePackageLocked(path, id)
	}
	if m.root == "" {
		return nil, pluginpkg.Package{}, pluginErrf(ErrPluginState, "update", id, "no package path was given and this deployment has no plugins root")
	}
	scan := pluginpkg.Discover(m.root, m.environment())
	current, ok := m.entries[id]
	if !ok {
		return nil, pluginpkg.Package{}, pluginErrf(ErrPluginNotFound, "update", id, "plugin is not installed")
	}
	best := pluginpkg.Package{}
	for _, pkg := range scan.Packages {
		if pkg.ID() != id || pkg.Version() == current.manifestVersion() {
			continue
		}
		if best.Manifest == nil || pkg.Manifest.ParsedVersion.Compare(best.Manifest.ParsedVersion) > 0 {
			best = pkg
		}
	}
	if best.Manifest == nil {
		return nil, pluginpkg.Package{}, pluginErrf(ErrPluginState, "update", id,
			"no other version of %s is available under %s: give a path or drop the new version there", id, m.root)
	}
	if err := best.Manifest.CheckEngines(m.version); err != nil {
		return nil, best, &PluginError{Op: "update", ID: id, Message: err.Error(), Fields: pluginpkg.FieldErrors(err), cause: ErrPluginGate}
	}
	return &managerEntry{id: id, manifest: best.Manifest, hash: best.Hash, dir: best.Dir}, best, nil
}

// archivedPackage remembers where the replaced version was moved (an empty dir
// means the previous content was replaced in place and cannot be restored).
type archivedPackage struct {
	dir string
}

// swapPackageLocked moves the new package into the managed directory, archiving
// the old one under `<root>/.versions/<id>/<version>-<unix>`.
func (m *Manager) swapPackageLocked(entry *managerEntry, staged *managerEntry, pkg pluginpkg.Package) (archivedPackage, error) {
	if m.root == "" {
		return archivedPackage{}, pluginErrf(ErrPluginPath, "update", entry.id, "this deployment has no plugins root configured")
	}
	dest := pluginpkg.PackageDirFor(m.root, entry.id)
	archive := archivedPackage{dir: entry.dir}
	if filepath.Clean(pkg.Dir) == filepath.Clean(dest) {
		// The new version already *is* the managed directory (an in-place update by
		// hand): there is nothing to copy and nothing to archive — the previous
		// content is already gone.
		archive.dir = ""
		entry.hash = pkg.Hash
		entry.manifest = pkg.Manifest
		entry.rawManifest = mustJSON(pkg.Manifest)
		entry.source = sourceOf(pkg)
		return archive, nil
	}
	if strings.TrimSpace(pkg.Dir) != "" && filepath.Clean(pkg.Dir) != filepath.Clean(dest) {
		staging := filepath.Join(m.root, fmt.Sprintf(".staging-%s-%d", entry.id, m.now().UnixNano()))
		if err := pluginpkg.CopyTree(pkg.Dir, staging); err != nil {
			_ = os.RemoveAll(staging)
			return archivedPackage{}, pluginErrf(ErrPluginPath, "update", entry.id, "copy package: %v", err)
		}
		copied := pluginpkg.LoadDir(staging, m.environment())
		if !copied.Usable() || copied.Hash != pkg.Hash {
			_ = os.RemoveAll(staging)
			return archivedPackage{}, pluginErrf(ErrPluginPath, "update", entry.id,
				"the copied package does not hash like its source (%s vs %s)", copied.Hash, pkg.Hash)
		}
		dest = staging
	}
	if entry.dir != "" {
		if err := os.MkdirAll(filepath.Join(m.root, ".versions", entry.id), 0o755); err != nil {
			return archivedPackage{}, pluginErrf(ErrPluginPath, "update", entry.id, "create archive directory: %v", err)
		}
		archivedDir := filepath.Join(m.root, ".versions", entry.id, fmt.Sprintf("%s-%d", entry.manifestVersion(), m.now().UnixNano()))
		if err := os.Rename(entry.dir, archivedDir); err != nil {
			return archivedPackage{}, pluginErrf(ErrPluginPath, "update", entry.id, "archive the previous version: %v", err)
		}
		archive.dir = archivedDir
	}
	if err := os.Rename(dest, pluginpkg.PackageDirFor(m.root, entry.id)); err != nil {
		// Put the old version back so the plugin is not left without a package.
		if archive.dir != "" {
			_ = os.Rename(archive.dir, pluginpkg.PackageDirFor(m.root, entry.id))
		}
		return archivedPackage{}, pluginErrf(ErrPluginPath, "update", entry.id, "move the new version into place: %v", err)
	}
	entry.dir = pluginpkg.PackageDirFor(m.root, entry.id)
	entry.hash = pkg.Hash
	entry.manifest = pkg.Manifest
	entry.rawManifest = mustJSON(pkg.Manifest)
	entry.source = sourceOf(pkg)
	return archive, nil
}

// restoreLocked puts a failed update back the way it was.
func (m *Manager) restoreLocked(ctx context.Context, entry *managerEntry, previous managerEntry, archived archivedPackage, wasEnabled bool) (PluginView, error) {
	if archived.dir == "" && previous.dir != "" && filepath.Clean(previous.dir) == filepath.Clean(entry.dir) {
		// The replaced content was an in-place replacement: the old package bytes
		// are gone, so the honest outcome is a failed plugin that names the
		// reason instead of a silent half-rollback.
		*entry = previous
		entry.state = LifecycleFailed
		entry.diagnostics = appendUnique(entry.diagnostics,
			"the update failed and the previous package content was replaced in place; install the previous version again")
		if err := m.persistLocked(ctx, entry); err != nil {
			return m.viewLocked(entry), err
		}
		return m.viewLocked(entry), nil
	}
	if archived.dir != "" {
		if err := pluginpkg.RemoveTree(m.root, entry.dir); err != nil {
			m.log.Warn("plugin update rollback: remove the new package failed", "plugin", entry.id, "err", err)
		}
		if err := os.Rename(archived.dir, pluginpkg.PackageDirFor(m.root, entry.id)); err != nil {
			m.log.Warn("plugin update rollback: restore the old package failed", "plugin", entry.id, "err", err)
		}
	}
	*entry = previous
	if wasEnabled {
		entry.state = LifecycleEnabled
		if _, _, err := m.applyEnabledLocked(ctx); err != nil {
			entry.state = LifecycleFailed
		}
	}
	if err := m.persistLocked(ctx, entry); err != nil {
		return m.viewLocked(entry), err
	}
	m.revision++
	return m.viewLocked(entry), nil
}

// --- persistence helpers --------------------------------------------------

func (m *Manager) persistLocked(ctx context.Context, entry *managerEntry) error {
	if m.store == nil {
		return nil
	}
	if entry.installedAt.IsZero() {
		entry.installedAt = m.now()
	}
	if entry.updatedAt.IsZero() {
		entry.updatedAt = m.now()
	}
	record := store.PluginRecord{
		ID:          entry.id,
		Version:     entry.manifestVersion(),
		Kind:        entry.kind(),
		State:       string(entry.state),
		Bundled:     entry.bundled,
		Source:      mustJSON(entry.source),
		Manifest:    entry.rawManifest,
		ContentHash: entry.hash,
		Path:        entry.dir,
		Diagnostics: append([]string(nil), entry.diagnostics...),
		InstalledAt: entry.installedAt,
		UpdatedAt:   entry.updatedAt,
		EnabledAt:   entry.enabledAt,
		DisabledAt:  entry.disabledAt,
	}
	if err := m.store.UpsertPlugin(ctx, record); err != nil {
		return fmt.Errorf("agentext: persist plugin %s: %w", entry.id, err)
	}
	return nil
}

func (m *Manager) auditLocked(ctx context.Context, audit AuditContext, id, version, action, result string, detail map[string]any) {
	if m.store == nil {
		return
	}
	actor := audit.Actor
	if actor == "" {
		actor = "unknown"
	}
	entry := store.PluginAuditEntry{
		TS:       m.now(),
		TenantID: audit.TenantID,
		Actor:    actor,
		PluginID: id,
		Version:  version,
		Action:   action,
		Result:   result,
	}
	if detail != nil {
		entry.Detail = mustJSON(detail)
	}
	if _, err := m.store.AppendPluginAudit(ctx, entry); err != nil {
		// The audit trail must not be silently lost; the action itself already
		// happened, so it is logged loudly instead of failing the request.
		m.log.Error("plugin audit write failed", "plugin", id, "action", action, "result", result, "err", err)
	}
}

// Audit returns lifecycle audit rows in chronological order.
func (m *Manager) Audit(ctx context.Context, tenantID *uuid.UUID, pluginID string, after int64, limit int) ([]store.PluginAuditEntry, error) {
	if m.store == nil {
		return []store.PluginAuditEntry{}, nil
	}
	return m.store.ListPluginAudit(ctx, tenantID, pluginID, after, limit)
}

// RecordAudit appends one lifecycle audit row on behalf of a product flow that
// drives the manager indirectly. It exists for the outcomes the manager itself
// never sees: a creation flow that refused to install (the approver said no, or
// the generated package failed validation) still has to leave a row, and the
// row must use the same vocabulary and the same table as every lifecycle
// action. The action/result strings are the store's (install/uninstall/... and
// ok/refused/failed); the detail carries the flow-specific facts.
func (m *Manager) RecordAudit(ctx context.Context, audit AuditContext, pluginID, version, action, result string, detail map[string]any) {
	m.auditLocked(ctx, audit, pluginID, version, action, result, detail)
}

// Root is the managed plugins root (empty when this deployment has none: a host
// that only ships builtins does not need one).
func (m *Manager) Root() string { return m.root }

// --- small helpers --------------------------------------------------------

func (entry *managerEntry) manifestVersion() string {
	if entry.manifest == nil {
		return ""
	}
	return entry.manifest.Version
}

func (entry *managerEntry) kind() string {
	if entry.manifest == nil {
		return ""
	}
	return string(entry.manifest.Kind)
}

func sourceOf(pkg pluginpkg.Package) pluginpkg.Source {
	if pkg.Manifest != nil && pkg.Manifest.Source.Type != "" {
		return pkg.Manifest.Source
	}
	return pluginpkg.Source{Type: pluginpkg.SourceLocal, Path: pkg.Dir}
}

// sourceOf on the manager keeps the manifest's declared provenance, falling
// back to "local" with the path it was installed from.
func (m *Manager) sourceOf(entry *managerEntry, origin string) pluginpkg.Source {
	if entry.manifest != nil && entry.manifest.Source.Type != "" {
		return entry.manifest.Source
	}
	return pluginpkg.Source{Type: pluginpkg.SourceLocal, Path: origin}
}

func decodeSource(raw json.RawMessage) pluginpkg.Source {
	var source pluginpkg.Source
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &source)
	}
	return source
}

func problemFields(problem *pluginpkg.Problem) []pluginpkg.FieldError {
	if problem == nil {
		return nil
	}
	return problem.Fields
}

func detailOf(err error) map[string]any {
	detail := map[string]any{"error": err.Error()}
	var pluginErr *PluginError
	if errors.As(err, &pluginErr) {
		if len(pluginErr.Fields) > 0 {
			detail["fields"] = pluginErr.Fields
		}
		if len(pluginErr.Missing) > 0 {
			detail["missing"] = pluginErr.Missing
		}
		if len(pluginErr.Dependents) > 0 {
			detail["dependents"] = pluginErr.Dependents
		}
		if len(pluginErr.Diagnostics) > 0 {
			detail["diagnostics"] = pluginErr.Diagnostics
		}
	}
	return detail
}

func packageID(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Base(path)
	}
	return filepath.Base(absolute)
}

func mustJSON(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func appendUnique(values []string, additions ...string) []string {
	for _, addition := range additions {
		if addition == "" {
			continue
		}
		found := false
		for _, value := range values {
			if value == addition {
				found = true
				break
			}
		}
		if !found {
			values = append(values, addition)
		}
	}
	return values
}

func timePointer(at time.Time) *time.Time { return &at }

func shortHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}
