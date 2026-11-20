// MCP Apps client capability declaration (change feat-mcp-apps-events, P6E/A13).
//
// The shared spec requirement「降级路径与能力协商」is a wire contract: the
// initialize request MUST carry the extension under BOTH `capabilities.extensions`
// (2026-07-28) and `capabilities.experimental` (the pre-revision position), with
// the identical payload. These tests capture the request the server actually
// receives, not the struct the client built.

package mcp

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestInitializeDeclaresMCPAppsInBothPositions(t *testing.T) {
	clientTransport, server := newTestServer(t)
	capabilities := ClientCapabilities{}
	capabilities.DeclareMCPApps()
	client := NewClient(ClientOptions{
		Name:         "apps-client",
		Version:      "1.0.0",
		Capabilities: capabilities,
	})
	if _, err := client.Connect(testContext(t), clientTransport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if !waitFor(t, 2*time.Second, func() bool { return len(server.requests("initialize")) == 1 }) {
		t.Fatalf("server saw %d initialize requests, want 1", len(server.requests("initialize")))
	}
	initialize := server.requests("initialize")[0]
	var params struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(initialize.Params, &params); err != nil {
		t.Fatalf("decode initialize params: %v", err)
	}
	extensionsRaw, hasExtensions := params.Capabilities["extensions"]
	experimentalRaw, hasExperimental := params.Capabilities["experimental"]
	if !hasExtensions || !hasExperimental {
		t.Fatalf("capabilities = %s, want both extensions and experimental", initialize.Params)
	}

	var extensions, experimental map[string]json.RawMessage
	if err := json.Unmarshal(extensionsRaw, &extensions); err != nil {
		t.Fatalf("decode extensions: %v", err)
	}
	if err := json.Unmarshal(experimentalRaw, &experimental); err != nil {
		t.Fatalf("decode experimental: %v", err)
	}
	extensionPayload, ok := extensions[AppExtensionID]
	if !ok {
		t.Fatalf("extensions = %s, want %q", extensionsRaw, AppExtensionID)
	}
	experimentalPayload, ok := experimental[AppExtensionID]
	if !ok {
		t.Fatalf("experimental = %s, want %q", experimentalRaw, AppExtensionID)
	}
	// Deep equality first: both positions must carry the same declaration.
	var extensionValue, experimentalValue any
	if err := json.Unmarshal(extensionPayload, &extensionValue); err != nil {
		t.Fatalf("decode extension payload: %v", err)
	}
	if err := json.Unmarshal(experimentalPayload, &experimentalValue); err != nil {
		t.Fatalf("decode experimental payload: %v", err)
	}
	if !reflect.DeepEqual(extensionValue, experimentalValue) {
		t.Fatalf("payloads differ: extensions=%s experimental=%s", extensionPayload, experimentalPayload)
	}
	if !bytes.Equal(extensionPayload, experimentalPayload) {
		t.Fatalf("payload bytes differ: extensions=%s experimental=%s", extensionPayload, experimentalPayload)
	}
	var payload struct {
		MimeTypes []string `json:"mimeTypes"`
	}
	if err := json.Unmarshal(extensionPayload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(payload.MimeTypes) != 1 || payload.MimeTypes[0] != AppMIMEType {
		t.Fatalf("mimeTypes = %v, want [%q]", payload.MimeTypes, AppMIMEType)
	}
}

func TestInitializeOmitsMCPAppsWithoutDeclaration(t *testing.T) {
	clientTransport, server := newTestServer(t)
	client := NewClient(ClientOptions{Name: "plain-client", Version: "1.0.0"})
	if _, err := client.Connect(testContext(t), clientTransport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if !waitFor(t, 2*time.Second, func() bool { return len(server.requests("initialize")) == 1 }) {
		t.Fatalf("server saw %d initialize requests, want 1", len(server.requests("initialize")))
	}
	initialize := server.requests("initialize")[0]
	var params struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(initialize.Params, &params); err != nil {
		t.Fatalf("decode initialize params: %v", err)
	}
	if _, ok := params.Capabilities["extensions"]; ok {
		t.Fatalf("capabilities declared extensions without an apps deployment: %s", initialize.Params)
	}
	if _, ok := params.Capabilities["experimental"]; ok {
		t.Fatalf("capabilities declared experimental without an apps deployment: %s", initialize.Params)
	}
}

// An empty but non-nil map is a declaration, not silence: it marshals as `{}`
// exactly like Sampling/Elicitation. A nil map stays omitted.
func TestClientCapabilitiesPreservesEmptyExtensionsMap(t *testing.T) {
	declared, err := json.Marshal(ClientCapabilities{Extensions: map[string]json.RawMessage{}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(declared) != `{"extensions":{}}` {
		t.Fatalf("empty extensions = %s, want {\"extensions\":{}}", declared)
	}
	plain, err := json.Marshal(ClientCapabilities{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(plain) != `{}` {
		t.Fatalf("zero capabilities = %s, want {}", plain)
	}
}
