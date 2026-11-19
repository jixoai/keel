// MCP progress/snapshot tests (P4.5 / P4.4 run boundary). They drive the
// aggregate tool the way the runner does: the attempt scope and call id travel
// on the context, the catalogue is frozen per attempt id.

package toolsurface_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/mcp"
	"github.com/jixoai/keel/internal/toolsurface"
)

func TestMCPToolInjectsProgressTokenAndAttempt(t *testing.T) {
	backend := &fakeMCP{
		servers: []*mcp.Server{mcpServer("fixture", mcp.Tool{Name: "work", InputSchema: json.RawMessage(`{"type":"object"}`)})},
		result:  mcp.CallToolResult{Content: []mcp.ContentBlock{}},
	}
	scope := agentcore.ToolRunScope{TenantID: "tenant-1", SessionID: "session-1"}
	var publishedScope agentcore.ToolRunScope
	var publishedCall string
	var published mcp.ProgressNotification
	progresses := 0
	registry := builtinRegistry(t, toolsurface.Backends{
		MCP: backend,
		MCPProgress: func(callScope agentcore.ToolRunScope, toolCallID string, progress mcp.ProgressNotification) {
			progresses++
			publishedScope, publishedCall, published = callScope, toolCallID, progress
		},
	})
	ctx := agentcore.WithToolRunScope(context.Background(), scope)
	ctx = agentcore.WithMCPAttempt(ctx, "attempt-1")
	// The executor passes the provider's call id as the second argument.
	result, err := registry[toolsurface.MCP].Execute(ctx, "call-1",
		json.RawMessage(`{"action":"call","server":"fixture","tool":"work","arguments":{}}`), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(backend.options) != 1 {
		t.Fatalf("backend calls = %d", len(backend.options))
	}
	options := backend.options[0]
	if options.ProgressToken != "call-1" {
		t.Fatalf("progress token = %q, want the toolCallId", options.ProgressToken)
	}
	if options.AttemptID != "attempt-1" {
		t.Fatalf("attempt id = %q", options.AttemptID)
	}
	if options.OnProgress == nil {
		t.Fatal("no progress sink was bound")
	}
	total := 2.0
	options.OnProgress(mcp.ProgressNotification{Progress: 1, Total: &total, Message: "half"})
	if progresses != 1 || publishedScope != scope || publishedCall != "call-1" {
		t.Fatalf("published = %+v/%q after %d frames", publishedScope, publishedCall, progresses)
	}
	if published.Progress != 1 || published.Total == nil || *published.Total != 2 || published.Message != "half" {
		t.Fatalf("progress payload = %+v", published)
	}
	_ = result
}

func TestMCPToolFreezesCataloguePerAttempt(t *testing.T) {
	old := mcpServer("fixture", mcp.Tool{Name: "old", InputSchema: json.RawMessage(`{"type":"object"}`)})
	backend := &fakeMCP{servers: []*mcp.Server{old}, result: mcp.CallToolResult{Content: []mcp.ContentBlock{}}}
	registry := builtinRegistry(t, toolsurface.Backends{MCP: backend})
	tool := registry[toolsurface.MCP]

	describe := func(attempt string, toolName string) agentcore.AgentToolResult {
		ctx := agentcore.WithMCPAttempt(context.Background(), attempt)
		args := json.RawMessage(`{"action":"describe","server":"fixture","tool":"` + toolName + `"}`)
		result, err := tool.Execute(ctx, "call-1", args, nil)
		if err != nil {
			t.Fatalf("describe: %v", err)
		}
		return result
	}
	// The first action of attempt-1 freezes the catalogue (old tool present).
	describe("attempt-1", "old")
	// A republish replaces the live server with one that only has "new".
	backend.servers = []*mcp.Server{mcpServer("fixture", mcp.Tool{Name: "new", InputSchema: json.RawMessage(`{"type":"object"}`)})}
	// The running attempt still sees the tool it started with.
	if result := describe("attempt-1", "old"); result.IsError != nil && *result.IsError {
		t.Fatalf("running attempt lost its frozen tool: %+v", result)
	}
	// The next attempt sees the republished catalogue: the old name is unknown,
	// the new one is present.
	next := describe("attempt-2", "old")
	if next.IsError == nil || !*next.IsError {
		t.Fatalf("old tool is still callable in the next attempt: %+v", next)
	}
	if result := describe("attempt-2", "new"); result.IsError != nil && *result.IsError {
		t.Fatalf("new tool is not visible in the next attempt: %+v", result)
	}
}
