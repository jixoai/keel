package toolsurface_test

import (
	"testing"

	"github.com/jixoai/keel/internal/mcp"
	"github.com/jixoai/keel/internal/toolsurface"
)

// TestMCPCallCrashIsTypedToolError pins the crash contract of the pool: a
// server that exited during the call answers one typed backend_failed tool
// result. The tool returns a result (not a Go error), so the loop records a
// failed tool call and the run remains resumable.
func TestMCPCallCrashIsTypedToolError(t *testing.T) {
	backend := destructiveServer(mcp.Tool{Name: "read_file"})
	backend.err = mcp.ErrServerUnavailable
	registry := builtinRegistry(t, toolsurface.Backends{MCP: backend})
	result := runAction(t, registry[toolsurface.MCP],
		`{"action":"call","server":"files","tool":"read_file","arguments":{"path":"/tmp/x"}}`)
	problem := errorDocument(t, result)
	if problem["code"] != toolsurface.CodeBackendFailed {
		t.Fatalf("problem = %v, want %s", problem, toolsurface.CodeBackendFailed)
	}
	if backend.calls != 1 {
		t.Fatalf("backend calls = %d, want the attempted call", backend.calls)
	}
}
