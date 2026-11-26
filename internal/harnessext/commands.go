package harnessext

// Plugin command registry (change keel-plugin-commands, tasks 1.3 and 3.1).
//
// A plugin's `provides.commands` declarations are not a second execution
// primitive: every command names a tool the same manifest already declares, and
// the host executes it through Manager.InvokePluginTool. This file is the *live*
// registry the product layer reads: which plugin commands exist right now, in
// which lifecycle/runtime state, and whether they can be invoked.
//
// The registry is derived, not stored: it scans the manager's entries and the
// runtime's current snapshot on every read. That is what makes enable/disable/
// uninstall/update take effect in the same generation — there is no second index
// that could go stale (the failure mode the change explicitly forbids).

import (
	"sort"

	"github.com/jixoai/keel/internal/pluginpkg"
)

// Command availability reasons. They are stable strings a client renders; the
// catalog only names a reason when Available is false.
const (
	// CommandReasonPluginPending reports an enabled plugin whose runtime has not
	// reached active (a dependency wait, a stopped runtime).
	CommandReasonPluginPending = "plugin_pending"
	// CommandReasonPluginFailed reports an enabled plugin whose assembly or
	// runtime failed (including a package that no longer validates).
	CommandReasonPluginFailed = "plugin_failed"
)

// PluginCommand is one live plugin command declaration.
type PluginCommand struct {
	// PluginID owns the command.
	PluginID string `json:"pluginId"`
	// Name is the plugin-local name (the declaration's name).
	Name string `json:"name"`
	// Published is the session-facing name `<plugin-id>:<name>` (the client
	// renders it with a leading '/').
	Published string `json:"published"`
	// Tool is the local backend tool spelling passed to InvokePluginTool.
	Tool string `json:"tool"`
	// DeclaredTool is the provides.tools spelling the manifest declared.
	DeclaredTool string `json:"declaredTool"`
	// PublishedTool is the wire-facing spelling of the backend tool; the product
	// layer derives the required permission keys from it.
	PublishedTool string `json:"publishedTool"`
	// Decl is the manifest declaration (validation ran at install).
	Decl pluginpkg.CommandDecl `json:"decl"`
	// State is the plugin's lifecycle intent at read time.
	State LifecycleState `json:"state"`
	// Available reports whether an invocation can reach the tool now.
	Available bool `json:"available"`
	// Reason is plugin_pending/plugin_failed when Available is false.
	Reason string `json:"reason,omitempty"`
}

// CommandRegistry is one read of the live plugin command surface.
type CommandRegistry struct {
	// Generation is the runtime assembly generation the snapshot came from.
	Generation int64 `json:"generation"`
	// Commands are ordered by plugin id, then declaration order.
	Commands []PluginCommand `json:"commands"`
}

// ListCommands renders the live plugin command registry. Disabled, installed
// (never enabled) and uninstalled plugins contribute nothing; an enabled plugin
// whose runtime is pending/failed stays listed as unavailable so a client can
// explain why a command it remembered is not runnable.
func (m *Manager) ListCommands() CommandRegistry {
	// Read the runtime snapshot before taking m.mu: the manager's own Snapshot
	// takes the same order, while lifecycle writes hold m.mu and then reach the
	// runtime. Keeping the order fixed avoids an ABBA deadlock.
	runtimeSnapshot := m.runtime.Snapshot()
	statuses := make(map[string]PluginStatus, len(runtimeSnapshot.Plugins))
	for _, status := range runtimeSnapshot.Plugins {
		statuses[status.ID] = status
	}

	m.mu.Lock()
	type snapshotEntry struct {
		id       string
		state    LifecycleState
		manifest *pluginpkg.Manifest
		commands []pluginpkg.PublishedCommand
	}
	snapshots := make([]snapshotEntry, 0, len(m.entries))
	for id, entry := range m.entries {
		switch entry.state {
		case LifecycleEnabled, LifecycleFailed, LifecycleInvalid:
		default:
			continue
		}
		if entry.manifest == nil || len(entry.manifest.Provides.Commands) == 0 {
			continue
		}
		snapshots = append(snapshots, snapshotEntry{
			id:       id,
			state:    entry.state,
			manifest: entry.manifest,
			commands: entry.manifest.PublishedCommands(),
		})
	}
	m.mu.Unlock()

	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].id < snapshots[j].id })
	registry := CommandRegistry{Generation: runtimeSnapshot.Generation, Commands: []PluginCommand{}}
	for _, entry := range snapshots {
		available, reason := commandAvailability(entry.state, statuses[entry.id])
		for _, command := range entry.commands {
			registry.Commands = append(registry.Commands, PluginCommand{
				PluginID:      entry.id,
				Name:          command.Local,
				Published:     command.Published,
				Tool:          command.Tool,
				DeclaredTool:  command.DeclaredTool,
				PublishedTool: command.PublishedTool,
				Decl:          command.Decl,
				State:         entry.state,
				Available:     available,
				Reason:        reason,
			})
		}
	}
	return registry
}

// commandAvailability maps the manager state and the runtime status onto the
// catalog's availability fact.
func commandAvailability(state LifecycleState, status PluginStatus) (bool, string) {
	switch state {
	case LifecycleFailed, LifecycleInvalid:
		return false, CommandReasonPluginFailed
	case LifecycleEnabled:
	default:
		return false, CommandReasonPluginFailed
	}
	switch status.State {
	case PluginActive:
		return true, ""
	case PluginFailed:
		return false, CommandReasonPluginFailed
	default:
		// No runtime status (not assembled yet) or a pending/stopped runtime.
		return false, CommandReasonPluginPending
	}
}

// FindCommand resolves one published plugin command from the live registry.
func (m *Manager) FindCommand(published string) (PluginCommand, bool) {
	for _, command := range m.ListCommands().Commands {
		if command.Published == published {
			return command, true
		}
	}
	return PluginCommand{}, false
}
