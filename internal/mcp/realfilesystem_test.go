package mcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRealFilesystemServer is the G4 real-server check: a pinned upstream
// @modelcontextprotocol/server-filesystem over stdio, with no Keel-specific
// adapter or fixture. It is opt-in because it downloads the npm package; CI keeps
// the hermetic fixtures. Run with:
//
//	KEEL_REAL_MCP=1 go test ./internal/mcp -run TestRealFilesystemServer -v
func TestRealFilesystemServer(t *testing.T) {
	if os.Getenv("KEEL_REAL_MCP") != "1" {
		t.Skip("set KEEL_REAL_MCP=1 to run the real filesystem MCP server")
	}
	// The upstream package pins pnpm as its dev package manager; npx refuses it.
	command, err := exec.LookPath("pnpm")
	if err != nil {
		command, err = exec.LookPath("npx")
	}
	if err != nil {
		t.Skip("neither pnpm nor npx is available")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(file, []byte("hello from a real MCP server\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	serverArgs := []string{"dlx", "@modelcontextprotocol/server-filesystem@2.4.0", dir}
	if filepath.Base(command) == "npx" {
		serverArgs = []string{"-y", "@modelcontextprotocol/server-filesystem@2.4.0", dir}
	}
	stderr := &lockedBuffer{}
	transport := NewStdioTransport(StdioTransportOptions{
		Command: command,
		Args:    serverArgs,
		Stderr:  stderr,
	})
	client := NewClient(ClientOptions{Name: "keel-real-filesystem", Version: "1.0.0"})
	if _, err := client.Connect(ctx, transport); err != nil {
		t.Fatalf("connect: %v (stderr: %s)", err, stderr.String())
	}
	t.Cleanup(func() { _ = client.Close() })

	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var readTool string
	for _, tool := range tools {
		if strings.Contains(tool.Name, "read") && !strings.Contains(tool.Name, "multiple") {
			readTool = tool.Name
			break
		}
	}
	if readTool == "" {
		t.Fatalf("no read tool in %v", toolNames(tools))
	}
	args, _ := json.Marshal(map[string]string{"path": file})
	result, err := client.CallTool(ctx, readTool, args)
	if err != nil {
		t.Fatalf("call %s: %v", readTool, err)
	}
	if result.IsError {
		t.Fatalf("%s returned an error: %+v", readTool, result.Content)
	}
	var text strings.Builder
	for _, block := range result.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	if !strings.Contains(text.String(), "hello from a real MCP server") {
		t.Fatalf("%s content = %q", readTool, text.String())
	}
}

func toolNames(tools []Tool) []string {
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	return names
}
