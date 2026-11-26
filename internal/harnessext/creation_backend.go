package harnessext

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jixoai/keel/internal/agentcore"
)

// CreationBackend adapts the keel.creation plugin's internal action handlers to
// the harness-owned `creation` aggregate tool. The Manager getter is lazy because
// the builtin factories are registered before the manager exists; at call time it
// always resolves.
type CreationBackend struct {
	Manager func() *Manager
}

var creationBackendActions = map[string]string{
	"catalog":  CreationToolCatalog,
	"scaffold": CreationToolScaffold,
	"read":     CreationToolRead,
	"write":    CreationToolWrite,
	"validate": CreationToolValidate,
	"install":  CreationToolInstall,
	"list":     CreationToolList,
	"remove":   CreationToolRemove,
}

// Invoke runs one creation action through the plugin manager. A missing manager
// is a wiring fact (ErrNotWired); once a manager exists, the plugin's lifecycle
// gate is the Manager's: an uninstalled keel.creation returns ErrPluginNotFound
// and a disabled one ErrPluginState. The aggregate tool maps those to
// not_wired and plugin_disabled respectively — the two facts stay distinct.
func (b CreationBackend) Invoke(ctx context.Context, action string, args json.RawMessage) (agentcore.AgentToolResult, error) {
	tool, ok := creationBackendActions[action]
	if !ok {
		return agentcore.AgentToolResult{}, fmt.Errorf("harnessext: unknown creation action %q", action)
	}
	manager := (*Manager)(nil)
	if b.Manager != nil {
		manager = b.Manager()
	}
	if manager == nil {
		return agentcore.AgentToolResult{}, fmt.Errorf("%w: no plugin manager is configured", ErrNotWired)
	}
	return manager.InvokeToolResult(ctx, CreationPluginID, tool, args)
}
