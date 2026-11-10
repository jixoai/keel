package agentext

// Permission enforcement tests (change keel-plugin-core, spec P1/P3). The
// negative controls are the point of this file: an undeclared tool, host,
// secret or URL must be refused AT THE DISPATCH POINT, the delegate must never
// be reached, and every refusal must leave exactly one audit row.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/pluginpkg"
	"github.com/jixoai/keel/internal/store"
)

func permissionManifest(t *testing.T, permissions string) *pluginpkg.Manifest {
	t.Helper()
	raw := `id: demo.rules
version: 1.0.0
kind: in-process
description: "permission fixture"
provides:
  tools:
    - own_tool
` + permissions
	manifest, err := pluginpkg.ParseManifest([]byte(raw))
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if err := manifest.Validate(pluginpkg.Environment{KeelVersion: testKeelVersion(), KnownHookEvent: ValidHookEvent}); err != nil {
		t.Fatalf("validate manifest: %v", err)
	}
	return manifest
}

func TestPermissionPolicyToolWhitelist(t *testing.T) {
	policy := NewPermissionPolicy(permissionManifest(t, `permissions:
  tools:
    - remote_tool
    - keel.example.example_status
`))
	for _, name := range []string{"own_tool", "demo.rules.own_tool", "remote_tool", "keel.example.example_status"} {
		if reason, allowed := policy.AllowTool(name); !allowed {
			t.Fatalf("tool %s was refused: %s", name, reason)
		}
	}
	for _, name := range []string{"host_shell", "keel.subagents.spawn_subagent", "demo.rules.exfiltrate"} {
		reason, allowed := policy.AllowTool(name)
		if allowed {
			t.Fatalf("tool %q was allowed", name)
		}
		if !strings.Contains(reason, "permissions.tools") {
			t.Fatalf("refusal reason for %q = %q", name, reason)
		}
	}
	if reason, allowed := policy.AllowTool(""); allowed || reason == "" {
		t.Fatalf("an empty tool name: allowed=%v reason=%q", allowed, reason)
	}
}

func TestPermissionPolicyEmptyManifestAllowsNothing(t *testing.T) {
	policy := NewPermissionPolicy(nil)
	if _, allowed := policy.AllowTool("anything"); allowed {
		t.Fatal("a plugin without a manifest was allowed to call a tool")
	}
	if _, allowed := policy.AllowHost("example.com"); allowed {
		t.Fatal("a plugin without a manifest was allowed to reach a host")
	}
	if _, allowed := policy.AllowSecret("TOKEN"); allowed {
		t.Fatal("a plugin without a manifest was allowed to read a secret")
	}
}

func TestPermissionPolicyNetHosts(t *testing.T) {
	// The manifest grammar (pluginpkg) allows '*' inside a host but not as its
	// first character, so the validated fixture uses a grammar-legal glob; the
	// canonical "*.suffix" form is pinned below on a hand-built policy.
	policy := NewPermissionPolicy(permissionManifest(t, `permissions:
  net:
    - api.example.com
    - "docs-*.internal.test"
    - "localhost:8080"
`))
	allowed := []string{
		"api.example.com",
		"API.EXAMPLE.COM",
		"api.example.com:443", // a portless declaration admits any port
		"docs-1.internal.test",
		"docs-a.b.internal.test",
		"localhost:8080",
	}
	for _, host := range allowed {
		if reason, ok := policy.AllowHost(host); !ok {
			t.Fatalf("host %s was refused: %s", host, reason)
		}
	}
	refused := []string{
		"example.com",
		"evil-api.example.com",   // only the exact host is declared
		"api.example.com.evil.x", // suffix attacks are not a match
		"internal.test",          // the glob needs its literal prefix
		"docs.internal.test",     // ... and the literal dot after it
		"localhost",              // the declaration pins the port
		"localhost:9090",
	}
	for _, host := range refused {
		if reason, ok := policy.AllowHost(host); ok {
			t.Fatalf("host %s was allowed", host)
		} else if !strings.Contains(reason, "permissions.net") {
			t.Fatalf("refusal reason for %s = %q", host, reason)
		}
	}
	if reason, ok := policy.AllowHost(""); ok || reason == "" {
		t.Fatalf("an empty host: allowed=%v reason=%q", ok, reason)
	}

	// The canonical subdomain wildcard, compiled directly (manifest validation
	// refuses a leading '*': a net permission must look like a host).
	wildcard := NewPermissionPolicy(&pluginpkg.Manifest{
		ID:          "demo.rules",
		Version:     "1.0.0",
		Permissions: pluginpkg.Permissions{Net: []string{"*.internal.test"}},
	})
	for _, host := range []string{"a.internal.test", "a.b.internal.test"} {
		if _, ok := wildcard.AllowHost(host); !ok {
			t.Fatalf("wildcard refused %s", host)
		}
	}
	for _, host := range []string{"internal.test", "notinternal.test", "evil.com"} {
		if _, ok := wildcard.AllowHost(host); ok {
			t.Fatalf("wildcard allowed %s", host)
		}
	}
}

func TestPermissionPolicyURLs(t *testing.T) {
	policy := NewPermissionPolicy(permissionManifest(t, `permissions:
  net:
    - api.example.com
`))
	if _, ok := policy.AllowURL("https://api.example.com/v1/items"); !ok {
		t.Fatal("a declared https URL was refused")
	}
	for _, raw := range []string{
		"http://evil.example.com/",
		"file:///etc/passwd",
		"",
		"not a url",
	} {
		if reason, ok := policy.AllowURL(raw); ok {
			t.Fatalf("URL %q was allowed", raw)
		} else if reason == "" {
			t.Fatalf("URL %q was refused without a reason", raw)
		}
	}
	// A declaration without a port admits any port of that host.
	if _, ok := policy.AllowURL("https://api.example.com:8443/"); !ok {
		t.Fatal("a portless declaration refused another port of the declared host")
	}
}

func TestPermissionPolicySecrets(t *testing.T) {
	policy := NewPermissionPolicy(permissionManifest(t, `permissions:
  secrets:
    - GITHUB_TOKEN
`))
	if _, ok := policy.AllowSecret("GITHUB_TOKEN"); !ok {
		t.Fatal("a declared secret was refused")
	}
	for _, name := range []string{"OPENAI_API_KEY", "github_token"} {
		if reason, ok := policy.AllowSecret(name); ok {
			t.Fatalf("secret %q was allowed", name)
		} else if !strings.Contains(reason, "permissions.secrets") {
			t.Fatalf("refusal reason for %q = %q", name, reason)
		}
	}
	if reason, ok := policy.AllowSecret(""); ok || reason == "" {
		t.Fatalf("an empty secret name: allowed=%v reason=%q", ok, reason)
	}
}

// --- the gate: every refusal is an error and an audit row -----------------

func TestPermissionGateAuditsEveryRefusal(t *testing.T) {
	var (
		mu      sync.Mutex
		denials []Denial
	)
	gate := NewPermissionGate(
		NewPermissionPolicy(permissionManifest(t, `permissions:
  tools: [declared_tool]
  net: [api.example.com]
  secrets: [DECLARED_SECRET]
`)),
		func(denial Denial) {
			mu.Lock()
			denials = append(denials, denial)
			mu.Unlock()
		},
		func() time.Time { return time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC) },
	)
	ctx := context.Background()

	// Allowed requests are silent.
	if err := gate.CheckTool(ctx, "declared_tool"); err != nil {
		t.Fatalf("declared tool: %v", err)
	}
	if err := gate.CheckTool(ctx, "demo.rules.own_tool"); err != nil {
		t.Fatalf("own tool: %v", err)
	}
	if err := gate.CheckURL(ctx, "https://api.example.com/x"); err != nil {
		t.Fatalf("declared host: %v", err)
	}
	if err := gate.CheckSecret(ctx, "DECLARED_SECRET"); err != nil {
		t.Fatalf("declared secret: %v", err)
	}
	mu.Lock()
	if len(denials) != 0 {
		mu.Unlock()
		t.Fatalf("allowed requests were audited: %+v", denials)
	}
	mu.Unlock()

	refusals := []struct {
		call   func() error
		kind   string
		target string
	}{
		{func() error { return gate.CheckTool(ctx, "undeclared_tool") }, PermissionTool, "undeclared_tool"},
		{func() error { return gate.CheckHost(ctx, "evil.example.com") }, PermissionNet, "evil.example.com"},
		{func() error { return gate.CheckSecret(ctx, "OPENAI_API_KEY") }, PermissionSecret, "OPENAI_API_KEY"},
	}
	for _, refusal := range refusals {
		err := refusal.call()
		if err == nil {
			t.Fatalf("%s %s was allowed", refusal.kind, refusal.target)
		}
		if !errors.Is(err, ErrPermissionDenied) {
			t.Fatalf("%s refusal = %v, want ErrPermissionDenied", refusal.kind, err)
		}
		var denied *PermissionDeniedError
		if !errors.As(err, &denied) {
			t.Fatalf("%s refusal is not a *PermissionDeniedError: %T", refusal.kind, err)
		}
		if denied.PluginID != "demo.rules" || denied.Kind != refusal.kind || denied.Target != refusal.target || denied.Reason == "" {
			t.Fatalf("refusal = %+v", denied)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(denials) != len(refusals) {
		t.Fatalf("audited denials = %+v, want %d", denials, len(refusals))
	}
	for i, denial := range denials {
		if denial.PluginID != "demo.rules" || denial.Kind != refusals[i].kind || denial.Target != refusals[i].target {
			t.Fatalf("denial[%d] = %+v", i, denial)
		}
		if denial.At.IsZero() {
			t.Fatalf("denial[%d] has no timestamp", i)
		}
	}
}

func TestPermissionGateGuardDoesNotRunDeniedWork(t *testing.T) {
	gate := NewPermissionGate(NewPermissionPolicy(permissionManifest(t, "permissions:\n  tools: [declared_tool]\n")), nil, nil)
	ran := false
	if err := gate.Guard(context.Background(), PermissionTool, "declared_tool", func() error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("declared tool guard: %v", err)
	}
	if !ran {
		t.Fatal("an allowed request did not run")
	}
	ran = false
	if err := gate.Guard(context.Background(), PermissionTool, "other_tool", func() error {
		ran = true
		return nil
	}); err == nil {
		t.Fatal("an undeclared tool guard succeeded")
	}
	if ran {
		t.Fatal("the denied work ran anyway")
	}
}

// --- the gated broker ------------------------------------------------------

type recordingBroker struct {
	mu      sync.Mutex
	call    int
	fetch   int
	secret  int
	lastURL string
}

func (b *recordingBroker) CallTool(_ context.Context, pluginID, tool string, args json.RawMessage) (agentcore.AgentToolResult, error) {
	b.mu.Lock()
	b.call++
	b.mu.Unlock()
	return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("delegated " + tool)}}, nil
}

func (b *recordingBroker) Fetch(_ context.Context, pluginID string, request FetchRequest) (FetchResponse, error) {
	b.mu.Lock()
	b.fetch++
	b.lastURL = request.URL
	b.mu.Unlock()
	return FetchResponse{Status: 200, Body: []byte("ok")}, nil
}

func (b *recordingBroker) ReadSecret(_ context.Context, pluginID, name string) (string, error) {
	b.mu.Lock()
	b.secret++
	b.mu.Unlock()
	return "value-of-" + name, nil
}

func (b *recordingBroker) counts() (int, int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.call, b.fetch, b.secret
}

func TestPermissionedBrokerRefusesBeforeDelegating(t *testing.T) {
	manifest := permissionManifest(t, `permissions:
  tools: [declared_tool]
  net: [api.example.com]
  secrets: [DECLARED_SECRET]
`)
	policy := NewPermissionPolicy(manifest)
	broker := &recordingBroker{}
	var (
		mu      sync.Mutex
		denials []Denial
	)
	// The broker is wired to a policy directly: the manager's own wiring (the
	// audit trail) is covered by the manager-level test below.
	gated := &PermissionedBroker{broker: broker, gates: map[string]*PermissionGate{
		"demo.rules": NewPermissionGate(policy, func(denial Denial) {
			mu.Lock()
			denials = append(denials, denial)
			mu.Unlock()
		}, nil),
	}}
	ctx := context.Background()

	if _, err := gated.CallTool(ctx, "demo.rules", "declared_tool", nil); err != nil {
		t.Fatalf("declared tool: %v", err)
	}
	if _, err := gated.Fetch(ctx, "demo.rules", FetchRequest{URL: "https://api.example.com/x"}); err != nil {
		t.Fatalf("declared host: %v", err)
	}
	if _, err := gated.ReadSecret(ctx, "demo.rules", "DECLARED_SECRET"); err != nil {
		t.Fatalf("declared secret: %v", err)
	}
	if calls, fetches, secrets := broker.counts(); calls != 1 || fetches != 1 || secrets != 1 {
		t.Fatalf("delegate counts = %d/%d/%d, want 1/1/1", calls, fetches, secrets)
	}

	if _, err := gated.CallTool(ctx, "demo.rules", "undeclared_tool", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("undeclared tool = %v", err)
	}
	if _, err := gated.Fetch(ctx, "demo.rules", FetchRequest{URL: "https://evil.example.com/"}); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("undeclared host = %v", err)
	}
	if _, err := gated.ReadSecret(ctx, "demo.rules", "OPENAI_API_KEY"); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("undeclared secret = %v", err)
	}
	// The delegate was never reached and every refusal was audited.
	if calls, fetches, secrets := broker.counts(); calls != 1 || fetches != 1 || secrets != 1 {
		t.Fatalf("a refused request reached the delegate: counts = %d/%d/%d", calls, fetches, secrets)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(denials) != 3 {
		t.Fatalf("audited denials = %+v, want 3", denials)
	}
}

func TestPermissionedBrokerWithoutBrokerRefusesAfterTheGate(t *testing.T) {
	// A deployment without a broker must not answer a *permitted* request either:
	// the gate passes, the seam reports itself missing.
	gate := NewPermissionedBroker(nil, nil)
	if _, err := gate.CallTool(context.Background(), "demo.rules", "anything", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("a plugin with no policy was allowed: %v", err)
	}
}

// --- the manager's dispatch point -----------------------------------------

// permissionHarness adds one plugin that owns a tool of its own and declares
// another plugin's tool in permissions.tools.
func permissionHarness(t *testing.T) *managerHarness {
	t.Helper()
	root := t.TempDir()
	harness := newSidecarHarness(t, root, nil)
	// The harness' runtime is built without loaders; add the two builtins the
	// dispatch tests need.
	addBuiltinPlugin(t, harness, &pluginpkg.Manifest{
		ID:          "demo.rules",
		Version:     "1.0.0",
		Kind:        pluginpkg.KindInProcess,
		Description: "rules fixture",
		Provides:    pluginpkg.Provides{Tools: []string{"demo.rules.own_tool"}},
		Permissions: pluginpkg.Permissions{Tools: []string{"keel.other.other_tool"}},
	}, "demo.rules.own_tool")
	addBuiltinPlugin(t, harness, &pluginpkg.Manifest{
		ID:          "keel.other",
		Version:     "1.0.0",
		Kind:        pluginpkg.KindInProcess,
		Description: "other fixture",
		Provides:    pluginpkg.Provides{Tools: []string{"keel.other.other_tool"}},
	}, "keel.other.other_tool")
	return harness
}

func addBuiltinPlugin(t *testing.T, harness *managerHarness, manifest *pluginpkg.Manifest, toolName string) {
	t.Helper()
	harness.manager.entries[manifest.ID] = &managerEntry{
		id:             manifest.ID,
		state:          LifecycleEnabled,
		manifest:       manifest,
		bundled:        true,
		defaultEnabled: true,
		source:         pluginpkg.Source{Type: pluginpkg.SourceBuiltin},
		dir:            "",
	}
	tool := toolName
	harness.manager.entries[manifest.ID].factory = func() *Plugin {
		return &Plugin{
			ID:      manifest.ID,
			Version: manifest.Version,
			Tools: []ToolSpec{{
				Name:        tool,
				Description: "fixture tool " + tool,
				Schema:      json.RawMessage(`{"type":"object"}`),
				Execute: func(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
					return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("ran " + tool)}}, nil
				},
			}},
		}
	}
}

func TestManagerInvokePluginToolEnforcesTheWhitelist(t *testing.T) {
	harness := permissionHarness(t)
	ctx := context.Background()
	if _, _, err := harness.manager.applyEnabledLocked(ctx); err != nil {
		t.Fatalf("assemble: %v", err)
	}

	// Own tool: allowed by ownership.
	raw, err := harness.manager.InvokePluginTool(ctx, "demo.rules", "demo.rules.own_tool", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("own tool: %v", err)
	}
	if !strings.Contains(string(raw), "ran demo.rules.own_tool") {
		t.Fatalf("own tool result = %s", raw)
	}
	// Declared foreign tool: allowed by permissions.tools.
	if _, err := harness.manager.InvokePluginTool(ctx, "demo.rules", "keel.other.other_tool", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("declared foreign tool: %v", err)
	}
	// Designated refusal: the other plugin does not declare the rules plugin's
	// tool, so the call is refused and audited.
	_, err = harness.manager.InvokePluginTool(ctx, "keel.other", "demo.rules.own_tool", json.RawMessage(`{}`))
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("undeclared tool = %v", err)
	}
	rows, auditErr := harness.manager.Audit(ctx, nil, "keel.other", 0, 100)
	if auditErr != nil {
		t.Fatalf("audit: %v", auditErr)
	}
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want one denial", rows)
	}
	row := rows[0]
	if row.Action != PluginActionDeny || row.Result != store.PluginResultRefused || row.Version != "1.0.0" {
		t.Fatalf("denial row = %+v", row)
	}
	var detail struct {
		Kind   string `json:"kind"`
		Target string `json:"target"`
	}
	if err := json.Unmarshal(row.Detail, &detail); err != nil {
		t.Fatalf("denial detail: %v", err)
	}
	if detail.Kind != PermissionTool || detail.Target != "demo_rules_own_tool" {
		t.Fatalf("denial detail = %+v (the audit names the published wire name)", detail)
	}
	// A tool nothing provides is not a permission decision.
	if _, err := harness.manager.InvokePluginTool(ctx, "demo.rules", "no.such.tool", json.RawMessage(`{}`)); err == nil {
		t.Fatal("an unknown tool was invoked")
	} else if !errors.Is(err, ErrPluginState) {
		t.Fatalf("unknown tool error = %v, want ErrPluginState", err)
	}
	// A disabled plugin has no dispatch surface at all.
	harness.manager.entries["demo.rules"].state = LifecycleDisabled
	if _, err := harness.manager.InvokePluginTool(ctx, "demo.rules", "demo.rules.own_tool", json.RawMessage(`{}`)); !errors.Is(err, ErrPluginState) {
		t.Fatalf("disabled plugin call = %v, want ErrPluginState", err)
	}
}

// TestManagerViewPublishesSidecarToolNamesAndCrashState pins the two view
// behaviours the sidecar path depends on: the host-qualified tool names appear
// in `provides` (that is the name the model and the bridge use), and a crashed
// supervised runtime shows as failed with its reason.
func TestManagerViewPublishesSidecarToolNamesAndCrashState(t *testing.T) {
	ctx := context.Background()
	t.Setenv("KEEL_TEST_SIDECAR_TOOLS", "greet")
	root := t.TempDir()
	sidecarLoader := newTestSidecarLoader(t, SidecarOptions{})
	harness := newSidecarHarness(t, root, sidecarLoader)

	source := sidecarPackage(t, t.TempDir(),
		sidecarManifest(t, "demo.sidecar", "1.0.0", []string{"greet"}, []string{"greet"})).Dir
	if _, err := harness.manager.Install(ctx, InstallRequest{Path: source}, AuditContext{Actor: "test"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	view, err := harness.manager.Enable(ctx, "demo.sidecar", AuditContext{Actor: "test"})
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !contains(view.Provides.Tools, "demo_sidecar_greet") {
		t.Fatalf("provides = %v, want the qualified tool name", view.Provides.Tools)
	}
	// The bridge's own rule (a tool is callable when it is declared AND live)
	// holds with this view: both lists carry the same name.
	if !contains(view.Tools, "demo_sidecar_greet") {
		t.Fatalf("live tools = %v", view.Tools)
	}

	health := sidecarLoader.Health()
	if len(health) != 1 {
		t.Fatalf("health = %+v", health)
	}
	if err := syscall.Kill(health[0].PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		for _, plugin := range harness.manager.Snapshot().Plugins {
			if plugin.ID == "demo.sidecar" {
				return plugin.State == LifecycleFailed
			}
		}
		return false
	}, "the crashed sidecar was never reported as failed")
	for _, plugin := range harness.manager.Snapshot().Plugins {
		if plugin.ID != "demo.sidecar" {
			continue
		}
		found := false
		for _, diagnostic := range plugin.Diagnostics {
			if strings.Contains(diagnostic, "runtime failed") {
				found = true
			}
		}
		if !found {
			t.Fatalf("diagnostics = %v, want the crash reason", plugin.Diagnostics)
		}
	}
}

// TestPermissionedBrokerWithAManagerAuditsThroughTheTrail pins the wiring the
// host uses: the broker resolves the plugin's policy from the manager and a
// refusal lands in the same audit trail the lifecycle actions use.
func TestPermissionedBrokerWithAManagerAuditsThroughTheTrail(t *testing.T) {
	harness := permissionHarness(t)
	ctx := context.Background()
	if _, _, err := harness.manager.applyEnabledLocked(ctx); err != nil {
		t.Fatalf("assemble: %v", err)
	}
	broker := &recordingBroker{}
	gated := NewPermissionedBroker(harness.manager, broker)

	// The plugin's own tool: allowed, delegated.
	if _, err := gated.CallTool(ctx, "demo.rules", "demo.rules.own_tool", nil); err != nil {
		t.Fatalf("own tool through the broker: %v", err)
	}
	// A declared foreign tool: allowed, delegated.
	if _, err := gated.CallTool(ctx, "demo.rules", "keel.other.other_tool", nil); err != nil {
		t.Fatalf("declared foreign tool through the broker: %v", err)
	}
	// An undeclared host and secret: refused, audited, never delegated.
	if _, err := gated.Fetch(ctx, "demo.rules", FetchRequest{URL: "https://api.example.com/x"}); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("undeclared net = %v", err)
	}
	if _, err := gated.ReadSecret(ctx, "demo.rules", "OPENAI_API_KEY"); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("undeclared secret = %v", err)
	}
	if calls, fetches, secrets := broker.counts(); calls != 2 || fetches != 0 || secrets != 0 {
		t.Fatalf("delegate counts = %d/%d/%d, want 2/0/0", calls, fetches, secrets)
	}
	rows, err := harness.manager.Audit(ctx, nil, "demo.rules", 0, 100)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("audit rows = %+v, want two denials", rows)
	}
	kinds := map[string]bool{}
	for _, row := range rows {
		if row.Action != PluginActionDeny || row.Result != store.PluginResultRefused || row.Version != "1.0.0" {
			t.Fatalf("denial row = %+v", row)
		}
		var detail struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(row.Detail, &detail); err != nil {
			t.Fatalf("detail: %v", err)
		}
		kinds[detail.Kind] = true
	}
	if !kinds[PermissionNet] || !kinds[PermissionSecret] {
		t.Fatalf("denial kinds = %v, want net and secret", kinds)
	}
	// A plugin nobody installed is refused too, and that refusal is audited as
	// well (the fail-closed default).
	if _, err := gated.CallTool(ctx, "ghost.plugin", "anything", nil); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("unknown plugin = %v", err)
	}
	rows, err = harness.manager.Audit(ctx, nil, "ghost.plugin", 0, 100)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(rows) != 1 || rows[0].Action != PluginActionDeny {
		t.Fatalf("unknown-plugin audit rows = %+v", rows)
	}
}
