package agentext

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
	"validate": CreationToolValidate,
	"install":  CreationToolInstall,
	"list":     CreationToolList,
	"remove":   CreationToolRemove,
}

// Invoke runs one creation action through the plugin manager. The plugin's
// lifecycle gate is the Manager's: a disabled or missing keel.creation returns
// ErrPluginState/ErrPluginNotFound, which the aggregate tool maps to
// plugin_disabled.
func (b CreationBackend) Invoke(ctx context.Context, action string, args json.RawMessage) (agentcore.AgentToolResult, error) {
	tool, ok := creationBackendActions[action]
	if !ok {
		return agentcore.AgentToolResult{}, fmt.Errorf("agentext: unknown creation action %q", action)
	}
	manager := (*Manager)(nil)
	if b.Manager != nil {
		manager = b.Manager()
	}
	if manager == nil {
		return agentcore.AgentToolResult{}, fmt.Errorf("agentext: %w: the plugin manager is not wired", ErrPluginState)
	}
	return manager.InvokeToolResult(ctx, CreationPluginID, tool, args)
}
