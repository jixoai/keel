package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Aggregate dispatch for the model's `mcp` tool. MCP servers are discovered
// dynamically; the model-visible tool set is not. The aggregate tool lists
// servers/tools, describes one tool's schema, and calls it by (server, tool),
// so no mcp__<server>__<tool> name is ever generated.

// ErrServerNotFound is the typed "no such configured server" failure.
var ErrServerNotFound = errors.New("mcp: server not found")

// ErrToolNotFound is the typed "the server has no such tool" failure.
var ErrToolNotFound = errors.New("mcp: tool not found")

// FindTool returns one tool's raw catalogue entry.
func (m *Manager) FindTool(server, tool string) (Tool, *Server, bool) {
	return findToolIn(m.Servers(), server, tool)
}

func findToolIn(servers []*Server, server, tool string) (Tool, *Server, bool) {
	for _, candidate := range servers {
		if candidate.Entry.Name != server {
			continue
		}
		for _, entry := range candidate.Tools {
			if entry.Name == tool {
				return entry, candidate, true
			}
		}
		return Tool{}, candidate, false
	}
	return Tool{}, nil, false
}

// maxCatalogueSnapshots bounds the per-attempt snapshot cache. An attempt that
// never ends (a crashed process) leaves at most this many entries behind; the
// value is small because a snapshot is a slice of pointers to immutable
// per-server values.
const maxCatalogueSnapshots = 256

// CatalogueSnapshot returns the catalogue frozen for one attempt. The first call
// for an attempt records the current servers; every later call returns the same
// value, so a tools/list_changed republish cannot change a running attempt's
// tools (spec「热更的 attempt 边界与失败保面」). An empty attemptID returns the
// live catalogue (tests and callers outside a run).
func (m *Manager) CatalogueSnapshot(attemptID string) []*Server {
	if m == nil {
		return nil
	}
	if attemptID == "" {
		return m.Servers()
	}
	m.snapshotMu.Lock()
	defer m.snapshotMu.Unlock()
	if servers, ok := m.snapshots[attemptID]; ok {
		return servers
	}
	servers := m.Servers()
	if m.snapshots == nil {
		m.snapshots = make(map[string][]*Server)
	}
	if len(m.snapshotOrder) >= maxCatalogueSnapshots {
		oldest := m.snapshotOrder[0]
		m.snapshotOrder = m.snapshotOrder[1:]
		delete(m.snapshots, oldest)
	}
	// The server values are immutable (a refresh replaces the pointer), so the
	// snapshot can share them.
	m.snapshots[attemptID] = servers
	m.snapshotOrder = append(m.snapshotOrder, attemptID)
	return servers
}

// CallTool invokes one tool of one connected server. The model faces this
// through the aggregate `mcp` tool, never through a generated per-tool name.
func (m *Manager) CallTool(ctx context.Context, server, tool string, args json.RawMessage) (CallToolResult, error) {
	return m.CallToolWithOptions(ctx, server, tool, args, CallOptions{})
}

// CallToolWithOptions is CallTool with the per-call notification surface (P4:
// the progress token and its sink). The zero options behave exactly like
// CallTool.
func (m *Manager) CallToolWithOptions(ctx context.Context, server, tool string, args json.RawMessage, options CallOptions) (CallToolResult, error) {
	if m == nil {
		return CallToolResult{}, fmt.Errorf("%w: no MCP manager", ErrServerNotFound)
	}
	entry, candidate, found := findToolIn(m.CatalogueSnapshot(options.AttemptID), server, tool)
	if candidate == nil {
		return CallToolResult{}, fmt.Errorf("%w: %q", ErrServerNotFound, server)
	}
	if candidate.Err != nil {
		return CallToolResult{}, fmt.Errorf("mcp: server %q: %w", server, candidate.Err)
	}
	if !found {
		return CallToolResult{}, fmt.Errorf("%w: %s/%s", ErrToolNotFound, server, tool)
	}
	if candidate.Client == nil {
		return CallToolResult{}, fmt.Errorf("mcp: server %q is not connected", server)
	}
	return candidate.Client.CallToolWithOptions(ctx, entry.Name, args, options)
}
