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
	for _, candidate := range m.Servers() {
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

// CallTool invokes one tool of one connected server. The model faces this
// through the aggregate `mcp` tool, never through a generated per-tool name.
func (m *Manager) CallTool(ctx context.Context, server, tool string, args json.RawMessage) (CallToolResult, error) {
	if m == nil {
		return CallToolResult{}, fmt.Errorf("%w: no MCP manager", ErrServerNotFound)
	}
	entry, candidate, found := m.FindTool(server, tool)
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
	return candidate.Client.CallTool(ctx, entry.Name, args)
}
