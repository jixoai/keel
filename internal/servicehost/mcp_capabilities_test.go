// MCP Apps capability gating (change feat-mcp-apps-events, P6E/A13).
//
// The initialize declaration is assembled where the MCP fleet is wired: the
// MRTR executor supplies sampling/elicitation, and only a deployment with the
// apps registry (catalogue + document + proxy routes) may claim the UI
// extension. These tests pin that gate without booting the whole service graph.

package servicehost

import (
	"bytes"
	"testing"

	"github.com/jixoai/keel/internal/mcphost"
)

func TestMCPClientCapabilitiesDeclareAppsOnlyWhenRegistryWired(t *testing.T) {
	// No registry: no app surface exists, so neither position may appear.
	unwired := mcpClientCapabilities(Config{MCP: MCPOptions{}})
	if _, ok := unwired.Extensions["io.modelcontextprotocol/ui"]; ok {
		t.Fatalf("extensions declared without an apps registry: %+v", unwired.Extensions)
	}
	if _, ok := unwired.Experimental["io.modelcontextprotocol/ui"]; ok {
		t.Fatalf("experimental declared without an apps registry: %+v", unwired.Experimental)
	}

	// With the registry the same payload must appear in both positions.
	registry := mcphost.New(mcphost.Options{})
	wired := mcpClientCapabilities(Config{MCP: MCPOptions{Registry: registry}})
	extension, hasExtension := wired.Extensions["io.modelcontextprotocol/ui"]
	experimental, hasExperimental := wired.Experimental["io.modelcontextprotocol/ui"]
	if !hasExtension || !hasExperimental {
		t.Fatalf("extensions/experimental = %+v / %+v, want both", wired.Extensions, wired.Experimental)
	}
	if !bytes.Equal(extension, experimental) {
		t.Fatalf("payloads differ: extensions=%s experimental=%s", extension, experimental)
	}
	if string(extension) != `{"mimeTypes":["text/html;profile=mcp-app"]}` {
		t.Fatalf("payload = %s", extension)
	}

	// The MRTR declaration stays intact when apps are added.
	mrtr := mcphost.NewMRTR(mcphost.MRTROptions{SamplingMode: mcphost.SamplingSession})
	wiredWithMRTR := mcpClientCapabilities(Config{MCP: MCPOptions{Registry: registry, MRTR: mrtr}})
	if wiredWithMRTR.Sampling == nil || wiredWithMRTR.Elicitation == nil {
		t.Fatalf("MRTR capabilities lost: %+v", wiredWithMRTR)
	}
	if _, ok := wiredWithMRTR.Extensions["io.modelcontextprotocol/ui"]; !ok {
		t.Fatalf("apps declaration lost with MRTR wired: %+v", wiredWithMRTR)
	}
}
