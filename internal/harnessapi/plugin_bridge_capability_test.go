package harnessapi

// W12: the bridge is gated by the plugin's own contribution capabilities.
// A contribution that names `state` only cannot call tools, ask for approvals
// or subscribe; a legacy webview with no contributions keeps today's bridge.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/harnessext"
)

func bridgeViewWithContributes(capabilities ...string) harnessext.PluginView {
	view := bridgePluginView()
	view.Layer = "ui"
	view.Slots = []harnessext.ExtensionSlot{{
		ID: "vendor.panel.widget", Forms: []string{"chunk"}, Arity: "multi", Targets: []string{"web", "native", "tui"},
	}}
	view.Contributes = []harnessext.ExtensionContribution{{
		ID: "refresh", Slot: "keel.ui.action", Form: "chunk", Entry: "ui/panel.js",
		Title: "Refresh", Capabilities: capabilities,
	}}
	view.Extensibility = &harnessext.ExtensionDoc{Usable: true, Schema: "extensibility.json"}
	return view
}

func TestPluginBridgeStateCarriesTheDeclaration(t *testing.T) {
	harness := newBridgeHarness(t, nil, bridgeViewWithContributes("state", "call"))
	tenant := uuid.New()
	var state pluginBridgeStateResponse
	if code := harness.write(t, http.MethodGet, "/v1/plugins/vendor.panel/bridge/state", tenant, nil, &state); code != http.StatusOK {
		t.Fatalf("state = %d", code)
	}
	if state.Layer != "ui" || len(state.Slots) != 1 || len(state.Contributes) != 1 {
		t.Fatalf("state declaration = %+v", state)
	}
	if state.Extensibility == nil || !state.Extensibility.Usable {
		t.Fatalf("extensibility = %+v", state.Extensibility)
	}
	if !state.Bridge.Call {
		t.Fatalf("a contribution that declares call must announce it: %+v", state.Bridge)
	}
	if state.Bridge.Approve || state.Bridge.Subscribe {
		t.Fatalf("verbs the contribution did not declare must not be announced: %+v", state.Bridge)
	}
}

func TestPluginBridgeGatesUndeclaredVerbs(t *testing.T) {
	harness := newBridgeHarness(t, nil, bridgeViewWithContributes("state"))
	tenant := uuid.New()
	cases := []struct {
		name string
		path string
		body any
	}{
		{"call", "/v1/plugins/vendor.panel/bridge/call", map[string]any{"tool": "example_status", "args": map[string]any{}}},
		{"approve", "/v1/plugins/vendor.panel/bridge/approve", map[string]any{"prompt": "ok?"}},
		{"subscribe", "/v1/plugins/vendor.panel/bridge/subscribe", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			method := http.MethodPost
			if tc.name == "subscribe" {
				method = http.MethodGet
			}
			var body map[string]any
			code := harness.write(t, method, tc.path, tenant, tc.body, &body)
			if code != http.StatusForbidden {
				t.Fatalf("%s = %d %+v", tc.name, code, body)
			}
			if body["code"] != "ERR_ACCESS_DENIED" {
				t.Fatalf("%s code = %v", tc.name, body["code"])
			}
		})
	}
}

func TestPluginBridgeDeclaredCallStillOwnsItsTool(t *testing.T) {
	view := bridgeViewWithContributes("state", "call")
	view.Provides.Tools = []string{"example_status"}
	view.Tools = []string{"example_status"}
	harness := newBridgeHarness(t, nil, view)
	tenant := uuid.New()
	var body map[string]any
	code := harness.write(t, http.MethodPost, "/v1/plugins/vendor.panel/bridge/call", tenant,
		map[string]any{"tool": "example_status", "args": map[string]any{}}, &body)
	if code != http.StatusOK {
		t.Fatalf("declared call = %d %+v", code, body)
	}
	if len(harness.tools.calls) != 1 {
		t.Fatalf("calls = %+v", harness.tools.calls)
	}
}

func TestPluginBridgeLegacyViewKeepsTheFullBridge(t *testing.T) {
	harness := newBridgeHarness(t, nil, bridgePluginView()) // no contributes
	tenant := uuid.New()
	var state pluginBridgeStateResponse
	if code := harness.write(t, http.MethodGet, "/v1/plugins/vendor.panel/bridge/state", tenant, nil, &state); code != http.StatusOK {
		t.Fatalf("state = %d", code)
	}
	if !state.Bridge.Call || !state.Bridge.Approve || !state.Bridge.Subscribe {
		t.Fatalf("a view without contributions must keep the legacy bridge: %+v", state.Bridge)
	}
}

func TestPluginBridgeUndeclaredToolKeepsItsCode(t *testing.T) {
	harness := newBridgeHarness(t, nil, bridgeViewWithContributes("state", "call"))
	tenant := uuid.New()
	var body map[string]any
	code := harness.write(t, http.MethodPost, "/v1/plugins/vendor.panel/bridge/call", tenant,
		map[string]any{"tool": "other_plugin_tool", "args": map[string]any{}}, &body)
	if code != http.StatusForbidden || body["code"] != "ERR_ACCESS_DENIED" {
		t.Fatalf("undeclared tool = %d %+v", code, body)
	}
	if len(harness.audit.list()) == 0 {
		t.Fatal("the refusal must be audited")
	}
}

// The response type is the allowlist; a declaration is not a host secret.
func TestPluginBridgeStateJSONHasNoPackagePath(t *testing.T) {
	harness := newBridgeHarness(t, nil, bridgeViewWithContributes("state"))
	tenant := uuid.New()
	var raw json.RawMessage
	if code := harness.write(t, http.MethodGet, "/v1/plugins/vendor.panel/bridge/state", tenant, nil, &raw); code != http.StatusOK {
		t.Fatalf("state = %d", code)
	}
	for _, needle := range []string{"/Users/operator", "secret-checkout"} {
		if strings.Contains(string(raw), needle) {
			t.Fatalf("state leaked %q: %s", needle, raw)
		}
	}
}
