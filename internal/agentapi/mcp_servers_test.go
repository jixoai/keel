// P5.6: GET /v1/mcp/servers carries the era, protocol version and the bounded
// capability document, so an operator (or the UI) needs no second endpoint to
// tell which generation a peer speaks.

package agentapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/mcp"
	"github.com/jixoai/keel/internal/mcphost"
)

func TestListMCPServersReportsEraVersionCapabilities(t *testing.T) {
	tenant := uuid.New()
	surface := testSurface(t)
	surface.Servers = []mcphost.ServerState{
		{
			Name:            "modern",
			Transport:       "streamable-http",
			Era:             "modern",
			ProtocolVersion: mcp.ModernProtocolVersion,
			Connected:       true,
			Capabilities:    json.RawMessage(`{"tools":{"listChanged":true},"extensions":{"io.modelcontextprotocol/tasks":{}}}`),
			Tools:           3,
			Apps:            1,
		},
		{
			Name:            "legacy",
			Transport:       "stdio",
			Era:             "legacy",
			ProtocolVersion: mcp.LatestProtocolVersion,
			Connected:       true,
			Capabilities:    json.RawMessage(`{"tools":{"listChanged":true}}`),
		},
		{
			Name:                  "huge",
			Transport:             "stdio",
			Era:                   "legacy",
			Connected:             true,
			Capabilities:          json.RawMessage(`{"truncated":true}`),
			CapabilitiesTruncated: true,
		},
	}
	server := testAgentAPI(t, Config{
		MCPApps: &fakeMCPApps{tenant: tenant, published: true, surface: surface},
	})
	res := rawGet(t, server, "/v1/mcp/servers", tenant)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	var body mcpServersResponse
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Generation != surface.Generation {
		t.Fatalf("generation = %d", body.Generation)
	}
	byName := map[string]mcphost.ServerState{}
	for _, state := range body.Servers {
		byName[state.Name] = state
	}
	modern := byName["modern"]
	if modern.Era != "modern" || modern.ProtocolVersion != mcp.ModernProtocolVersion {
		t.Fatalf("modern = %+v", modern)
	}
	if len(modern.Capabilities) == 0 {
		t.Fatal("modern capabilities are missing")
	}
	legacy := byName["legacy"]
	if legacy.Era != "legacy" || legacy.ProtocolVersion != mcp.LatestProtocolVersion {
		t.Fatalf("legacy = %+v", legacy)
	}
	huge := byName["huge"]
	if !huge.CapabilitiesTruncated || string(huge.Capabilities) != `{"truncated":true}` {
		t.Fatalf("huge = %+v", huge)
	}
}
