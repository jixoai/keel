package architecture

// MCP events isolation (change feat-mcp-apps-events, P4.3/P4.4). The MCP
// notification surface is server-scoped and lives in internal/mcphost;
// the product SSE surface is session-scoped and lives in internal/httpapi. They
// must never share a handler, a hub or a recovery semantic, so the import graph
// keeps them apart:
//
//   - internal/mcphost must not import internal/httpapi, internal/agentapi or
//     the cmd/keel wiring;
//   - internal/mcp (the client/transport layer) must not import the host
//     dispatcher (internal/mcphost): the dependency only points host → client.

import "testing"

const (
	mcpHostImportPath  = "github.com/jixoai/keel/internal/mcphost"
	mcpImportPath      = "github.com/jixoai/keel/internal/mcp"
	httpapiImportPath  = "github.com/jixoai/keel/internal/httpapi"
	agentapiImportPath = "github.com/jixoai/keel/internal/agentapi"
)

func TestMCPHostDoesNotImportProductSSE(t *testing.T) {
	graph := packageGraph(t, mcpHostImportPath)
	for _, forbidden := range []string{httpapiImportPath, agentapiImportPath} {
		requireNoImport(t, graph, mcpHostImportPath, forbidden)
	}
}

func TestMCPClientDoesNotImportHostDispatcher(t *testing.T) {
	graph := packageGraph(t, mcpImportPath)
	requireNoImport(t, graph, mcpImportPath, mcpHostImportPath)
	requireNoImport(t, graph, mcpImportPath, httpapiImportPath)
}
