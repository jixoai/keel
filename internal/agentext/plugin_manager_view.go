package agentext

import (
	"sort"
	"time"

	"github.com/jixoai/keel/internal/pluginpkg"
)

// PluginView is one row of GET /v1/plugins: the manifest, the lifecycle state
// and the live runtime surface. `bundled`, `source` and `version` are the
// fields the client needs to tell an official plugin from a local one.
type PluginView struct {
	// ID is the plugin id.
	ID string `json:"id"`
	// Version is the installed version.
	Version string `json:"version,omitempty"`
	// Kind is in-process|sidecar|webview.
	Kind string `json:"kind,omitempty"`
	// Description is the manifest's one-liner.
	Description string `json:"description,omitempty"`
	// State is the lifecycle state (installed|enabled|disabled|failed|invalid).
	State LifecycleState `json:"state"`
	// Bundled marks a plugin compiled into the binary.
	Bundled bool `json:"bundled"`
	// Source is where the package came from.
	Source pluginpkg.Source `json:"source"`
	// Path is the managed package directory (empty for bundled plugins).
	Path string `json:"path,omitempty"`
	// ContentHash is the package tree hash (empty for bundled plugins).
	ContentHash string `json:"contentHash,omitempty"`
	// Engines is the declared host compatibility range.
	Engines pluginpkg.Engines `json:"engines,omitempty"`
	// Provides is the manifest's declared surface.
	Provides pluginpkg.Provides `json:"provides"`
	// Requires lists the requirements of the plugin.
	Requires []string `json:"requires,omitempty"`
	// Permissions is the declared capability set.
	Permissions pluginpkg.Permissions `json:"permissions,omitempty"`
	// Diagnostics are the reasons attached to the state (validation problems,
	// load failures, content changes).
	Diagnostics []string `json:"diagnostics,omitempty"`
	// Problem is set when the package is invalid: why, and which fields.
	Problem *pluginpkg.Problem `json:"problem,omitempty"`
	// Shadowed lists other versions of the same id found under the root (they are
	// kept for update/rollback and never loaded).
	Shadowed []ShadowedPackage `json:"shadowed,omitempty"`
	// Tools, Hooks, Triggers and Effects mirror the live surface at the top
	// level: the model-facing names this plugin contributes to the current
	// assembly (empty when it is not loaded). They are the fields a listing
	// renders without descending into `runtime`.
	Tools    []string    `json:"tools,omitempty"`
	Hooks    []HookEvent `json:"hooks,omitempty"`
	Triggers []string    `json:"triggers,omitempty"`
	Effects  []string    `json:"effects,omitempty"`
	// StartedAt and StoppedAt stamp the last activation cycle (top-level mirror
	// of the same runtime fields).
	StartedAt *time.Time `json:"startedAt,omitempty"`
	StoppedAt *time.Time `json:"stoppedAt,omitempty"`
	// Runtime is the live assembly state (nil when the plugin is not loaded).
	Runtime *PluginRuntimeView `json:"runtime,omitempty"`
	// InstalledAt, UpdatedAt, EnabledAt and DisabledAt are UTC timestamps.
	InstalledAt *time.Time `json:"installedAt,omitempty"`
	UpdatedAt   *time.Time `json:"updatedAt,omitempty"`
	EnabledAt   *time.Time `json:"enabledAt,omitempty"`
	DisabledAt  *time.Time `json:"disabledAt,omitempty"`
}

// ShadowedPackage is a package of the same id that lost the version conflict.
type ShadowedPackage struct {
	Version string `json:"version"`
	Path    string `json:"path"`
	Hash    string `json:"hash,omitempty"`
}

// PluginRuntimeView is the live state of one loaded plugin.
type PluginRuntimeView struct {
	// State is the runtime state: active|pending|failed|stopped.
	State PluginState `json:"state"`
	// Tools are the model-facing tools the plugin contributed to the current
	// generation.
	Tools []string `json:"tools,omitempty"`
	// Hooks are the subscribed events.
	Hooks []HookEvent `json:"hooks,omitempty"`
	// Triggers are the trigger ids the plugin owns.
	Triggers []string `json:"triggers,omitempty"`
	// Effects are the named cleanups the runtime will run on unload.
	Effects []string `json:"effects,omitempty"`
	// Diagnostics are the runtime's own reasons (dependency waits, tool
	// conflicts, activation errors).
	Diagnostics []string `json:"diagnostics,omitempty"`
	// StartedAt and StoppedAt stamp the last activation cycle.
	StartedAt *time.Time `json:"startedAt,omitempty"`
	StoppedAt *time.Time `json:"stoppedAt,omitempty"`
}

// PluginInventory is the GET /v1/plugins body: every plugin the deployment
// knows, the effective tool surface and two change identities — the runtime
// generation and the manager revision. A client that only needs "did anything
// change" compares those two numbers.
type PluginInventory struct {
	// Root is the managed plugins root.
	Root string `json:"root,omitempty"`
	// Revision advances on every inventory change (install/enable/disable/...).
	Revision int64 `json:"revision"`
	// Generation is the runtime assembly generation.
	Generation int64 `json:"generation"`
	// Plugins is the inventory.
	Plugins []PluginView `json:"plugins"`
	// Tools is the effective model-facing tool surface.
	Tools []string `json:"tools"`
	// Enabled lists the enabled plugin ids.
	Enabled []string `json:"enabled"`
	// Hooks is the hook-bus summary.
	Hooks StatsSummary `json:"hooks"`
}

// Snapshot renders the inventory. It is safe to call at any time and never
// blocks on the runtime: the runtime's own snapshot is read once.
func (m *Manager) Snapshot() PluginInventory {
	runtimeSnapshot := m.runtime.Snapshot()
	statuses := make(map[string]PluginStatus, len(runtimeSnapshot.Plugins))
	for _, status := range runtimeSnapshot.Plugins {
		statuses[status.ID] = status
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	views := make([]PluginView, 0, len(m.entries))
	enabled := make([]string, 0, len(m.entries))
	for id, entry := range m.entries {
		view := m.viewWithStatus(entry, statuses[id])
		if entry.state == LifecycleEnabled {
			enabled = append(enabled, id)
		}
		views = append(views, view)
	}
	sortPluginViews(views)
	sortStrings(enabled)
	return PluginInventory{
		Root:       m.root,
		Revision:   m.revision,
		Generation: runtimeSnapshot.Generation,
		Plugins:    views,
		Tools:      runtimeSnapshot.Tools,
		Enabled:    enabled,
		Hooks:      runtimeSnapshot.Hooks,
	}
}

// viewLocked renders one entry with the current runtime status.
func (m *Manager) viewLocked(entry *managerEntry) PluginView {
	statuses := make(map[string]PluginStatus)
	for _, status := range m.runtime.Snapshot().Plugins {
		statuses[status.ID] = status
	}
	return m.viewWithStatus(entry, statuses[entry.id])
}

func (m *Manager) viewWithStatus(entry *managerEntry, status PluginStatus) PluginView {
	view := PluginView{
		ID:          entry.id,
		State:       entry.state,
		Bundled:     entry.bundled,
		Source:      entry.source,
		Path:        entry.dir,
		ContentHash: entry.hash,
		Diagnostics: append([]string(nil), entry.diagnostics...),
		Problem:     entry.problem,
	}
	if entry.manifest != nil {
		view.Version = entry.manifest.Version
		view.Kind = string(entry.manifest.Kind)
		view.Description = entry.manifest.Description
		view.Engines = entry.manifest.Engines
		view.Provides = entry.manifest.Provides
		view.Requires = append([]string(nil), entry.manifest.Requires...)
		view.Permissions = entry.manifest.Permissions
	}
	if !entry.installedAt.IsZero() {
		installedAt := entry.installedAt
		view.InstalledAt = &installedAt
	}
	if !entry.updatedAt.IsZero() {
		updatedAt := entry.updatedAt
		view.UpdatedAt = &updatedAt
	}
	view.EnabledAt = entry.enabledAt
	view.DisabledAt = entry.disabledAt
	for _, pkg := range entry.shadowed {
		view.Shadowed = append(view.Shadowed, ShadowedPackage{Version: pkg.Version(), Path: pkg.Dir, Hash: pkg.Hash})
	}
	if status.ID != "" {
		view.Tools = append([]string(nil), status.Tools...)
		view.Hooks = append([]HookEvent(nil), status.Hooks...)
		view.Effects = append([]string(nil), status.Effects...)
		for _, trigger := range status.Triggers {
			view.Triggers = append(view.Triggers, trigger.ID)
		}
		view.StartedAt = status.StartedAt
		view.StoppedAt = status.StoppedAt
		runtime := &PluginRuntimeView{
			State:       status.State,
			Tools:       append([]string(nil), status.Tools...),
			Hooks:       append([]HookEvent(nil), status.Hooks...),
			Effects:     append([]string(nil), status.Effects...),
			Diagnostics: append([]string(nil), status.Diagnostics...),
			StartedAt:   status.StartedAt,
			StoppedAt:   status.StoppedAt,
		}
		for _, trigger := range status.Triggers {
			runtime.Triggers = append(runtime.Triggers, trigger.ID)
		}
		view.Runtime = runtime
		// A sidecar (or a webview backend) speaks its own local tool names; the
		// host publishes them as <plugin>.<tool>. `provides` is the manifest's
		// declaration, so the host-qualified names the assembly actually
		// publishes are added: that is the name the model calls and the plugin
		// bridge dispatches, and a client that only reads `provides` must see it.
		if len(view.Provides.Tools) > 0 {
			merged := append([]string(nil), view.Provides.Tools...)
			for _, tool := range status.Tools {
				merged = appendUnique(merged, tool)
			}
			sort.Strings(merged)
			view.Provides.Tools = merged
		}
		// An enabled plugin the runtime could not activate is reported as failed:
		// the persisted intent says enabled, the truth says otherwise.
		if entry.state == LifecycleEnabled && status.State == PluginFailed {
			view.State = LifecycleFailed
			view.Diagnostics = appendUnique(view.Diagnostics, status.Diagnostics...)
		}
	}
	// A loader that supervises a live process reports its own deaths: a crashed
	// sidecar is failed even though the database still says enabled (its
	// re-enable is the reconnect).
	if entry.state == LifecycleEnabled {
		if reason, failed := m.runtimeFailure(entry); failed {
			view.State = LifecycleFailed
			view.Diagnostics = appendUnique(view.Diagnostics, "runtime failed: "+reason)
		}
	}
	return view
}

func sortPluginViews(views []PluginView) {
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
}

func sortStrings(values []string) { sort.Strings(values) }
