package toolsurface

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/mcp"
)

type realMCPBackend struct {
	client  *mcp.Client
	servers []*mcp.Server
}

func (b *realMCPBackend) Servers() []*mcp.Server                 { return b.servers }
func (b *realMCPBackend) CatalogueSnapshot(string) []*mcp.Server { return b.servers }
func (b *realMCPBackend) CallTool(ctx context.Context, server, tool string, args json.RawMessage) (mcp.CallToolResult, error) {
	return b.client.CallTool(ctx, tool, args)
}
func (b *realMCPBackend) CallToolWithOptions(ctx context.Context, server, tool string, args json.RawMessage, options mcp.CallOptions) (mcp.CallToolResult, error) {
	return b.client.CallToolWithOptions(ctx, tool, args, options)
}

// TestRealFilesystemAggregateTool exercises the same real server through the
// aggregate `mcp` tool the model sees, not just the low-level client.
func TestRealFilesystemAggregateTool(t *testing.T) {
	if os.Getenv("KEEL_REAL_MCP") != "1" {
		t.Skip("set KEEL_REAL_MCP=1 to run the real filesystem MCP server")
	}
	command, err := exec.LookPath("pnpm")
	if err != nil {
		t.Skip("pnpm not available")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(file, []byte("hello through the aggregate tool\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	transport := mcp.NewStdioTransport(mcp.StdioTransportOptions{
		Command: command,
		Args:    []string{"dlx", "@modelcontextprotocol/server-filesystem@2.4.0", dir},
	})
	client := mcp.NewClient(mcp.ClientOptions{Name: "keel-real-aggregate", Version: "1.0.0"})
	if _, err := client.Connect(ctx, transport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	server := &mcp.Server{Entry: mcp.ServerEntry{Name: "filesystem"}, Client: client, Tools: tools}
	tool := newMCPTool(&realMCPBackend{client: client, servers: []*mcp.Server{server}}, nil, nil)
	if tool.Name() != MCP {
		t.Fatalf("aggregate tool name = %q", tool.Name())
	}

	list, err := tool.Execute(ctx, "c1", json.RawMessage(`{"action":"list"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if listText := resultText(list); !strings.Contains(listText, "filesystem") {
		t.Fatalf("list = %q", listText)
	}

	var readTool string
	for _, candidate := range tools {
		if strings.Contains(candidate.Name, "read") && !strings.Contains(candidate.Name, "multiple") {
			readTool = candidate.Name
			break
		}
	}
	if readTool == "" {
		t.Fatalf("no read tool in %+v", tools)
	}
	args, _ := json.Marshal(map[string]any{
		"action": "call", "server": "filesystem", "tool": readTool,
		"arguments": map[string]string{"path": file},
	})
	result, err := tool.Execute(ctx, "c2", args, nil)
	if err != nil {
		t.Fatal(err)
	}
	if text := resultText(result); !strings.Contains(text, "hello through the aggregate tool") {
		t.Fatalf("call = %q", text)
	}
}

func resultText(result agentcore.AgentToolResult) string {
	var text strings.Builder
	for _, block := range result.Content {
		if value, ok := block.(agentcore.TextContent); ok {
			text.WriteString(value.Text)
		}
	}
	return text.String()
}
