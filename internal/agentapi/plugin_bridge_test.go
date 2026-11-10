package agentapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/pluginpkg"
	"github.com/jixoai/keel/internal/store"
)

// The plugin UI bridge (change keel-plugin-core, W3): four verbs, the plugin's
// own capabilities only, and never a host secret in the answer.

// fakePluginTools is the bridge's tool seam double.
type fakePluginTools struct {
	calls  []string
	result json.RawMessage
	err    error
}

func (f *fakePluginTools) InvokePluginTool(_ context.Context, pluginID, tool string, args json.RawMessage) (json.RawMessage, error) {
	f.calls = append(f.calls, pluginID+"/"+tool+"/"+string(args))
	if f.err != nil {
		return nil, f.err
	}
	if f.result == nil {
		return json.RawMessage(`{"ok":true}`), nil
	}
	return f.result, nil
}

// bridgeHarness is the bridge-specific server harness: the fake manager, the
// real decision broker and hook bus, and the tool seam double.
type bridgeHarness struct {
	server  *httptest.Server
	manager *fakePluginManager
	audit   *fakePluginAuditWriter
	hooks   *agentext.HookBus
	tools   *fakePluginTools
}

// write performs one bridge call. path is relative to the test server.
func (h *bridgeHarness) write(t *testing.T, method, path string, tenant uuid.UUID, body any, out any) int {
	t.Helper()
	return doJSON(t, h.server.Client(), method, h.server.URL+path, tenant, body, out)
}

func (h *bridgeHarness) getRaw(t *testing.T, path string, tenant uuid.UUID) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tenant != uuid.Nil {
		req.Header.Set("X-Tenant-ID", tenant.String())
	}
	res, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func newBridgeHarness(t *testing.T, tune func(*Config), views ...agentext.PluginView) *bridgeHarness {
	t.Helper()
	manager := &fakePluginManager{inventory: agentext.PluginInventory{
		Root:       "/Users/operator/.keel/plugins",
		Revision:   11,
		Generation: 5,
		Plugins:    views,
		Tools:      []string{"example_status"},
		Enabled:    []string{"vendor.panel"},
	}}
	audit := &fakePluginAuditWriter{}
	broker := agentext.NewDecisionBroker(agentext.DecisionBrokerOptions{Logger: testLogger()})
	t.Cleanup(broker.Close)
	hooks := agentext.NewHookBus(agentext.HookBusOptions{Logger: testLogger()})
	t.Cleanup(hooks.Close)
	tools := &fakePluginTools{}
	cfg := Config{
		PluginManager:     manager,
		PluginAuditWriter: audit,
		Decisions:         broker,
		Hooks:             hooks,
		PluginTools:       tools,
		// The host theme tokens the bridge pushes down to plugin UIs.
		Theme:  json.RawMessage(`{"name":"Keel Solid Glass","cssVars":{"light":{"--bg":"oklch(1 0 0)"}}}`),
		Logger: testLogger(),
	}
	if tune != nil {
		tune(&cfg)
	}
	server := httptest.NewServer(New(cfg))
	t.Cleanup(server.Close)
	return &bridgeHarness{server: server, manager: manager, audit: audit, hooks: hooks, tools: tools}
}

// bridgePluginView is an enabled webview plugin with a live tool surface.
func bridgePluginView() agentext.PluginView {
	return agentext.PluginView{
		ID:          "vendor.panel",
		Version:     "1.2.3",
		Kind:        "webview",
		Description: "a panel",
		State:       agentext.LifecycleEnabled,
		// Fields a plugin UI must never receive: the managed package path and the
		// install provenance (they reveal the host's filesystem layout).
		Path:        "/Users/operator/.keel/plugins/vendor.panel",
		Source:      pluginpkg.Source{Type: pluginpkg.SourceLocal, Path: "/Users/operator/secret-checkout"},
		ContentHash: "sha256:abc",
		Provides: pluginpkg.Provides{
			Tools: []string{"example_status"},
			UI:    &pluginpkg.UI{Entry: "ui/index.html", Title: "panel"},
		},
		Permissions: pluginpkg.Permissions{Secrets: []string{"PANEL_TOKEN"}},
		Tools:       []string{"example_status"},
		Runtime:     &agentext.PluginRuntimeView{State: agentext.PluginActive, Tools: []string{"example_status"}},
		InstalledAt: timePtr(time.Unix(100, 0).UTC()),
	}
}

func timePtr(at time.Time) *time.Time { return &at }

// TestPluginBridgeStateIsAnAllowlist: the state document carries the plugin's
// own row and the host theme tokens, and nothing host-private.
func TestPluginBridgeStateIsAnAllowlist(t *testing.T) {
	harness := newBridgeHarness(t, nil, bridgePluginView())
	tenant := uuid.New()
	res := harness.getRaw(t, "/v1/plugins/vendor.panel/bridge/state", tenant)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("state = %d %s", res.StatusCode, raw)
	}
	for _, needle := range []string{
		"/Users/operator", // the managed package path
		"secret-checkout", // the install provenance
		"\"path\"",        // the view's path field
		"\"source\"",      // the provenance document
		"\"contentHash\"", // the package hash
	} {
		if strings.Contains(string(raw), needle) {
			t.Fatalf("the state answer leaks %q: %s", needle, raw)
		}
	}
	var state pluginBridgeStateResponse
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.ID != "vendor.panel" || state.State != agentext.LifecycleEnabled || state.Version != "1.2.3" {
		t.Fatalf("state = %+v", state)
	}
	if state.Revision != 11 || state.Generation != 5 {
		t.Fatalf("the change identities must travel: %+v", state)
	}
	if len(state.Provides.Tools) != 1 || state.Provides.Tools[0] != "example_status" {
		t.Fatalf("provides = %+v", state.Provides)
	}
	if len(state.Theme) == 0 || !strings.Contains(string(state.Theme), "Keel Solid Glass") {
		t.Fatalf("the host theme tokens must reach the plugin UI: %s", state.Theme)
	}
	if !state.Bridge.Call || !state.Bridge.Approve || !state.Bridge.Subscribe {
		t.Fatalf("the wired verbs must be announced: %+v", state.Bridge)
	}
}

// TestPluginBridgeStateGate: only an enabled, installed plugin is bridged.
func TestPluginBridgeStateGate(t *testing.T) {
	disabled := bridgePluginView()
	disabled.State = agentext.LifecycleDisabled
	harness := newBridgeHarness(t, nil, disabled)
	tenant := uuid.New()
	for _, tc := range []struct {
		name string
		path string
		want int
	}{
		{"disabled", "/v1/plugins/vendor.panel/bridge/state", http.StatusForbidden},
		{"not installed", "/v1/plugins/other.plugin/bridge/state", http.StatusNotFound},
		{"malformed id", "/v1/plugins/NOT-AN-ID/bridge/state", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := harness.getRaw(t, tc.path, tenant)
			defer res.Body.Close()
			if res.StatusCode != tc.want {
				t.Fatalf("%s = %d, want %d", tc.path, res.StatusCode, tc.want)
			}
		})
	}
	// Every bridge route needs a tenant.
	if code := harness.write(t, http.MethodGet, "/v1/plugins/vendor.panel/bridge/state", uuid.Nil, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("missing tenant = %d", code)
	}
}

// TestPluginBridgeCallOwnTools: a plugin calls its own tool; anything else is
// refused with an audit row.
func TestPluginBridgeCallOwnTools(t *testing.T) {
	harness := newBridgeHarness(t, nil, bridgePluginView())
	tenant := uuid.New()

	var ok struct {
		Tool   string          `json:"tool"`
		Result json.RawMessage `json:"result"`
	}
	if code := harness.write(t, http.MethodPost, "/v1/plugins/vendor.panel/bridge/call", tenant,
		map[string]any{"tool": "example_status", "args": map[string]any{"verbose": true}}, &ok); code != http.StatusOK {
		t.Fatalf("call = %d", code)
	}
	if ok.Tool != "example_status" || !strings.Contains(string(ok.Result), "ok") {
		t.Fatalf("call result = %+v", ok)
	}
	if len(harness.tools.calls) != 1 || !strings.Contains(harness.tools.calls[0], `"verbose":true`) {
		t.Fatalf("the args must travel unchanged: %+v", harness.tools.calls)
	}

	// Another plugin's tool is out of the bridge's vocabulary, and the attempt is
	// audited (spec: 越权被拒 MUST 写审计).
	var refusal map[string]any
	if code := harness.write(t, http.MethodPost, "/v1/plugins/vendor.panel/bridge/call", tenant,
		map[string]any{"tool": "other_plugin_tool"}, &refusal); code != http.StatusForbidden {
		t.Fatalf("foreign tool = %d %+v", code, refusal)
	}
	if len(harness.tools.calls) != 1 {
		t.Fatalf("a refused call must not execute: %+v", harness.tools.calls)
	}
	rows := harness.audit.list()
	if len(rows) != 1 || rows[0].Result != store.PluginResultRefused || rows[0].PluginID != "vendor.panel" {
		t.Fatalf("audit = %+v", rows)
	}
	if rows[0].Action != pluginBridgeCallAction {
		t.Fatalf("the refusal action must be distinguishable: %+v", rows[0])
	}
	// A missing seam is a deployment gap, not a refusal.
	unwired := newBridgeHarness(t, func(cfg *Config) { cfg.PluginTools = nil }, bridgePluginView())
	if code := unwired.write(t, http.MethodPost, "/v1/plugins/vendor.panel/bridge/call", tenant,
		map[string]any{"tool": "example_status"}, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("unwired seam = %d", code)
	}
	// A UI-only plugin (enabled, never assembled) has nothing to call.
	uiOnly := bridgePluginView()
	uiOnly.Runtime = nil
	uiOnly.Tools = nil
	onlyUI := newBridgeHarness(t, nil, uiOnly)
	if code := onlyUI.write(t, http.MethodPost, "/v1/plugins/vendor.panel/bridge/call", tenant,
		map[string]any{"tool": "example_status"}, nil); code != http.StatusForbidden {
		t.Fatalf("ui-only plugin call = %d", code)
	}
}

// TestPluginBridgeApproveRoundTrip: the plugin asks, the host answers, the
// plugin observes the answer; the question is namespaced per plugin.
func TestPluginBridgeApproveRoundTrip(t *testing.T) {
	harness := newBridgeHarness(t, nil, bridgePluginView())
	tenant := uuid.New()
	var asked struct {
		Decision    agentext.Decision `json:"decision"`
		Status      string            `json:"status"`
		ApprovalURL string            `json:"approvalUrl"`
	}
	if code := harness.write(t, http.MethodPost, "/v1/plugins/vendor.panel/bridge/approve", tenant,
		map[string]any{"prompt": "Install the panel?", "requestId": "q1"}, &asked); code != http.StatusAccepted {
		t.Fatalf("approve = %d", code)
	}
	if asked.Decision.Status != agentext.DecisionOpen || asked.Decision.Kind != pluginBridgeKindDecision {
		t.Fatalf("decision = %+v", asked.Decision)
	}
	if !strings.HasPrefix(asked.Decision.RequestID, pluginBridgeRequestPrefix+"vendor.panel:") {
		t.Fatalf("the question must be attributable to its plugin: %q", asked.Decision.RequestID)
	}
	if asked.ApprovalURL != "/v1/plugins/vendor.panel/bridge/approvals/"+asked.Decision.DecisionID {
		t.Fatalf("approvalUrl = %q", asked.ApprovalURL)
	}
	if len(asked.Decision.Options) != 2 {
		t.Fatalf("the default options must be allow/deny: %+v", asked.Decision.Options)
	}
	// The host answers.
	if code := harness.write(t, http.MethodPost, asked.ApprovalURL, tenant, map[string]any{"optionId": pluginApprovalAllow}, nil); code != http.StatusOK {
		t.Fatalf("answer = %d", code)
	}
	// The plugin reads it back, and a repeat ask with the same key observes the
	// already-answered question (the waiting shape the UI template awaits).
	var read struct {
		Decision agentext.Decision `json:"decision"`
	}
	if code := harness.write(t, http.MethodGet, asked.ApprovalURL, tenant, nil, &read); code != http.StatusOK || read.Decision.Status != agentext.DecisionAnswered {
		t.Fatalf("read = %d %+v", code, read)
	}
	var waited struct {
		Decision agentext.Decision `json:"decision"`
		Status   string            `json:"status"`
	}
	if code := harness.write(t, http.MethodPost, "/v1/plugins/vendor.panel/bridge/approve", tenant,
		map[string]any{"prompt": "Install the panel?", "requestId": "q1", "waitMs": 5000}, &waited); code != http.StatusOK {
		t.Fatalf("waiting ask = %d", code)
	}
	if waited.Decision.Status != agentext.DecisionAnswered || waited.Decision.Answer == nil || waited.Decision.Answer.OptionID != pluginApprovalAllow {
		t.Fatalf("the wait must return the answer: %+v", waited)
	}
	// Bounds.
	var bad map[string]any
	for _, body := range []map[string]any{
		{"prompt": ""},
		{"prompt": "x", "waitMs": 600001},
		{"prompt": "x", "options": make([]map[string]any, 9)},
		{"prompt": "x", "options": []map[string]any{{"id": ""}}},
	} {
		if code := harness.write(t, http.MethodPost, "/v1/plugins/vendor.panel/bridge/approve", tenant, body, &bad); code != http.StatusBadRequest {
			t.Fatalf("bad approval body %+v = %d", body, code)
		}
	}
	// A question of another deployment is not readable through this plugin's route.
	otherView := bridgePluginView()
	otherView.ID = "vendor.other"
	other := newBridgeHarness(t, nil, otherView)
	if code := other.write(t, http.MethodGet, "/v1/plugins/vendor.other/bridge/approvals/"+asked.Decision.DecisionID, tenant, nil, &bad); code != http.StatusNotFound {
		t.Fatalf("another plugin must not see the question = %d", code)
	}
}

// TestPluginBridgeSubscribeAttribute: the stream carries this plugin's own
// events, and only those.
func TestPluginBridgeSubscribeAttribute(t *testing.T) {
	harness := newBridgeHarness(t, nil, bridgePluginView())
	tenant := uuid.New()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		harness.server.URL+"/v1/plugins/vendor.panel/bridge/subscribe?events=trigger.before", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", tenant.String())
	res, err := harness.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("subscribe = %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	frames := make(chan string, 32)
	go func() {
		scanner := bufio.NewScanner(res.Body)
		for scanner.Scan() {
			frames <- scanner.Text()
		}
		close(frames)
	}()
	// The ready frame proves the subscription is live before anything is emitted.
	waitForLine(t, frames, "event: ready")
	// Another plugin's trigger is not this plugin's business.
	harness.hooks.Emit(agentext.HookPayload{Event: agentext.HookTriggerBefore, TenantID: tenant.String(), TriggerID: "other.plugin.webhook"})
	if line, ok := waitForMaybeLine(t, frames, 300*time.Millisecond, "event: trigger.before"); ok {
		t.Fatalf("a foreign event must not be forwarded: %q", line)
	}
	// Its own trigger is.
	harness.hooks.Emit(agentext.HookPayload{Event: agentext.HookTriggerBefore, TenantID: tenant.String(), TriggerID: "vendor.panel.webhook", Status: "manual"})
	waitForLine(t, frames, "event: trigger.before")
	payload := waitForLine(t, frames, "data:")
	if !strings.Contains(payload, "vendor.panel.webhook") {
		t.Fatalf("own event payload = %q", payload)
	}
	// An unknown event name is a 400 before any stream starts.
	if code := harness.write(t, http.MethodGet, "/v1/plugins/vendor.panel/bridge/subscribe?events=nope", tenant, nil, nil); code != http.StatusBadRequest {
		t.Fatalf("unknown event = %d", code)
	}
}

// TestPluginBridgeCapabilitiesDiscoverable: the deployment announces the bridge
// so a client can hide what is not served here.
func TestPluginBridgeCapabilitiesDiscoverable(t *testing.T) {
	harness := newBridgeHarness(t, nil, bridgePluginView())
	var caps Capabilities
	if code := doJSON(t, harness.server.Client(), http.MethodGet, harness.server.URL+"/v1/capabilities", uuid.New(), nil, &caps); code != 200 {
		t.Fatalf("capabilities = %d", code)
	}
	if !caps.Features["plugins.bridge"] || !caps.Features["plugins.approvals"] {
		t.Fatalf("features = %+v", caps.Features)
	}
	unwired := newBridgeHarness(t, func(cfg *Config) { cfg.Decisions = nil }, bridgePluginView())
	if code := doJSON(t, unwired.server.Client(), http.MethodGet, unwired.server.URL+"/v1/capabilities", uuid.New(), nil, &caps); code != 200 {
		t.Fatalf("capabilities = %d", code)
	}
	if caps.Features["plugins.approvals"] {
		t.Fatalf("a deployment that cannot ask must say so: %+v", caps.Features)
	}
}

// --- stream helpers -------------------------------------------------------

func waitForLine(t *testing.T, frames <-chan string, prefix string) string {
	t.Helper()
	line, ok := waitForMaybeLine(t, frames, 5*time.Second, prefix)
	if !ok {
		t.Fatalf("no frame with prefix %q", prefix)
	}
	return line
}

func waitForMaybeLine(t *testing.T, frames <-chan string, within time.Duration, prefix string) (string, bool) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case line, ok := <-frames:
			if !ok {
				return "", false
			}
			if strings.HasPrefix(line, prefix) {
				return line, true
			}
		case <-deadline:
			return "", false
		}
	}
}
