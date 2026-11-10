package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentapi"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/store"
)

// Host-side adapters of the plugin product surface (change keel-plugin-core):
// they live in cmd/keel because that is the only place that knows both the
// product API (internal/agentapi) and the live host registry.

// productToolIndex remembers the dynamic tool set the plugin runtime publishes
// through servicehost.Runtime.RefreshProductTools. The bridge's `call` verb
// executes a tool the plugin owns; the published registry is exactly what the
// model calls, so the bridge cannot reach a capability the model would not
// reach either.
type productToolIndex struct {
	mu    sync.RWMutex
	tools map[string]agentcore.AgentTool
}

func newProductToolIndex() *productToolIndex {
	return &productToolIndex{tools: make(map[string]agentcore.AgentTool)}
}

// publish replaces the indexed tool set. Called from the plugin runtime's tool
// sink, on every assembly.
func (i *productToolIndex) publish(tools []agentcore.AgentTool) {
	indexed := make(map[string]agentcore.AgentTool, len(tools))
	for _, tool := range tools {
		if tool == nil || tool.Name() == "" {
			continue
		}
		indexed[tool.Name()] = tool
	}
	i.mu.Lock()
	i.tools = indexed
	i.mu.Unlock()
}

func (i *productToolIndex) find(name string) agentcore.AgentTool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.tools[name]
}

// pluginToolBridge implements agentapi.PluginToolInvoker over the published
// registry. Ownership is re-checked against the manager's live view, so a call
// naming another plugin's tool fails even if the caller's own gate was bypassed.
type pluginToolBridge struct {
	index   *productToolIndex
	manager *agentext.Manager
}

// InvokePluginTool runs one tool the plugin provides right now.
func (b *pluginToolBridge) InvokePluginTool(ctx context.Context, pluginID, tool string, args json.RawMessage) (json.RawMessage, error) {
	if b == nil || b.index == nil || b.manager == nil {
		return nil, errors.New("the plugin tool bridge is not wired")
	}
	owned := false
	for _, view := range b.manager.Snapshot().Plugins {
		if view.ID != pluginID {
			continue
		}
		for _, name := range view.Tools {
			if name == tool {
				owned = true
			}
		}
	}
	if !owned {
		return nil, fmt.Errorf("plugin %s does not provide the tool %s", pluginID, tool)
	}
	executable := b.index.find(tool)
	if executable == nil {
		return nil, fmt.Errorf("tool %s is not in the live tool registry", tool)
	}
	// The call id is a provenance tag in the tool's own logs/updates: "bridge-"
	// tells the operator this call came from the plugin's UI, not from a model.
	result, err := executable.Execute(ctx, "bridge-"+uuid.NewString(), args, nil)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode tool result: %w", err)
	}
	return raw, nil
}

// pluginAuditStore adapts the durable store to agentapi.PluginAuditWriter: the
// audit row of a lifecycle write the manager never saw (an approval refusal)
// lands in the same trail as the manager's own rows.
type pluginAuditStore struct {
	store *store.Store
}

// RecordPluginAudit implements agentapi.PluginAuditWriter.
func (a pluginAuditStore) RecordPluginAudit(ctx context.Context, entry store.PluginAuditEntry) error {
	if a.store == nil {
		return errors.New("no store is wired")
	}
	_, err := a.store.AppendPluginAudit(ctx, entry)
	return err
}

// hostThemeMaxBytes bounds the theme document read from the web build.
const hostThemeMaxBytes = 256 << 10

// loadHostTheme reads the host theme token document (the "Keel Solid Glass"
// tokens the web client renders with: web/theme.json, published next to the web
// build). The plugin bridge hands it to plugin UIs so a plugin inherits the
// host's light/dark and spacing instead of shipping its own design system. A
// missing or unreadable document is not fatal: the bridge simply omits it.
func loadHostTheme(webRoot string, log *slog.Logger) json.RawMessage {
	if webRoot == "" {
		return nil
	}
	path := filepath.Join(webRoot, "theme.json")
	info, err := os.Stat(path)
	if err != nil {
		log.Info("host theme tokens not found; plugin UIs will not receive them", "path", path)
		return nil
	}
	if info.Size() > hostThemeMaxBytes {
		log.Warn("host theme document ignored: too large", "path", path, "bytes", info.Size())
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		log.Warn("host theme document unreadable", "path", path, "err", err)
		return nil
	}
	if !json.Valid(raw) {
		log.Warn("host theme document is not valid JSON", "path", path)
		return nil
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		log.Warn("host theme document is not a JSON object", "path", path, "err", err)
		return nil
	}
	return json.RawMessage(raw)
}

// Compile-time assertions of the product-API seams this file implements.
var (
	_ agentapi.PluginToolInvoker = (*pluginToolBridge)(nil)
	_ agentapi.PluginAuditWriter = pluginAuditStore{}
)
