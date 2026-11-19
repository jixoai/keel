// P5.6: GET /v1/mcp/servers reports the negotiated era, protocol version and
// the bounded capability document. The projection must follow the client's
// actual era, not re-derive it from the version string.

package mcphost

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/mcp"
)

// connectFakeServer returns a connected mcp.Client whose peer answers with the
// given initialize/discover result and an empty tools/list.
func connectFakeServer(t *testing.T, era mcp.ProtocolEra, capabilities map[string]any, version string) *mcp.Client {
	t.Helper()
	clientTransport, serverTransport := mcp.NewMemoryTransportPair()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	if err := serverTransport.Start(ctx); err != nil {
		t.Fatalf("start server transport: %v", err)
	}
	go func() {
		for {
			message, err := serverTransport.Receive(context.Background())
			if err != nil {
				return
			}
			request, ok := message.(*mcp.Request)
			if !ok {
				continue
			}
			var result any
			switch request.Method {
			case "initialize", "server/discover":
				result = map[string]any{
					"protocolVersion": version,
					"capabilities":    capabilities,
					"serverInfo":      map[string]any{"name": "state-fixture", "version": "1"},
				}
			case "tools/list":
				result = map[string]any{"tools": []any{}}
			default:
				result = map[string]any{}
			}
			encoded, _ := json.Marshal(result)
			_ = serverTransport.Send(context.Background(), &mcp.SuccessResponse{
				JSONRPC: mcp.JSONRPCVersion,
				ID:      request.ID,
				Result:  encoded,
			})
		}
	}()
	t.Cleanup(func() { _ = serverTransport.Close() })
	client := mcp.NewClient(mcp.ClientOptions{Name: "state-test", Version: "1", Era: era})
	if _, err := client.Connect(ctx, clientTransport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestServerStateReportsEraVersionAndCapabilities(t *testing.T) {
	modern := connectFakeServer(t, mcp.EraModern, map[string]any{
		"tools":      map[string]any{"listChanged": true},
		"extensions": map[string]any{"io.modelcontextprotocol/tasks": map[string]any{}},
	}, mcp.ModernProtocolVersion)
	legacy := connectFakeServer(t, mcp.EraLegacy, map[string]any{
		"tools": map[string]any{"listChanged": true},
	}, mcp.LatestProtocolVersion)
	fleet := &fakeFleet{servers: []*mcp.Server{
		{Entry: mcp.ServerEntry{Name: "modern"}, Client: modern},
		{Entry: mcp.ServerEntry{Name: "legacy"}, Client: legacy},
	}}
	surface := (&Registry{}).buildSurface(fleet, fleet.servers, nil, 7)
	if surface.Generation != 7 {
		t.Fatalf("generation = %d", surface.Generation)
	}
	byName := map[string]ServerState{}
	for _, state := range surface.Servers {
		byName[state.Name] = state
	}
	modernState := byName["modern"]
	if modernState.Era != "modern" || modernState.ProtocolVersion != mcp.ModernProtocolVersion {
		t.Fatalf("modern state = %+v", modernState)
	}
	if !strings.Contains(string(modernState.Capabilities), "extensions") {
		t.Fatalf("modern capabilities = %s, want extensions", modernState.Capabilities)
	}
	if modernState.CapabilitiesTruncated {
		t.Fatal("small capabilities were marked truncated")
	}
	legacyState := byName["legacy"]
	if legacyState.Era != "legacy" || legacyState.ProtocolVersion != mcp.LatestProtocolVersion {
		t.Fatalf("legacy state = %+v", legacyState)
	}
}

func TestServerStateBoundsCapabilities(t *testing.T) {
	huge := strings.Repeat("x", ServerStateCapabilitiesMaxBytes+1024)
	client := connectFakeServer(t, mcp.EraLegacy, map[string]any{
		"experimental": map[string]any{"huge": huge},
	}, mcp.LatestProtocolVersion)
	fleet := &fakeFleet{servers: []*mcp.Server{{Entry: mcp.ServerEntry{Name: "huge"}, Client: client}}}
	surface := (&Registry{}).buildSurface(fleet, fleet.servers, nil, 1)
	state := surface.Servers[0]
	if !state.CapabilitiesTruncated {
		t.Fatalf("large capabilities were not marked truncated: %d bytes", len(state.Capabilities))
	}
	if string(state.Capabilities) != `{"truncated":true}` {
		t.Fatalf("capabilities marker = %s", state.Capabilities)
	}
	if len(state.Capabilities) > ServerStateCapabilitiesMaxBytes {
		t.Fatalf("capabilities projection = %d bytes, over the bound", len(state.Capabilities))
	}
}

func TestServerStateDisconnectedServerIsLegacy(t *testing.T) {
	fleet := &fakeFleet{servers: []*mcp.Server{{Entry: mcp.ServerEntry{Name: "down"}, Err: context.DeadlineExceeded}}}
	surface := (&Registry{}).buildSurface(fleet, fleet.servers, nil, 1)
	state := surface.Servers[0]
	if state.Era != "legacy" || state.Connected {
		t.Fatalf("state = %+v", state)
	}
}
