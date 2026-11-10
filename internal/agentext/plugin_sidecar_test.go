package agentext

// Sidecar loader tests (change keel-plugin-core, spec S1-S3).
//
// The sidecar under test is this test binary re-executed as a child process
// (the same fixture technique internal/gateway uses): with KEEL_TEST_SIDECAR set
// it serves the provider side of the gateway's stdio JSON-RPC protocol and
// prints nothing on stdout. Every test drives the real loader, the real
// transport, the real process supervision and a real process group.
//
// The negative controls the spec asks for are here by name:
//
//	TestSidecarLoaderEnvWhitelistKeepsHostSecretsOut  越权 env
//	TestSidecarLoaderRefusesUndeclaredTools           越权工具（未声明）
//	TestSidecarLoaderCrashesIsolateAndReconnect       崩溃隔离 + 可重连
//	TestSidecarLoaderTimeoutAndCancellation           超时与取消
//	TestSidecarLoaderStopReapsTheProcessGroup         孤儿进程

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/gateway"
	"github.com/jixoai/keel/internal/pluginpkg"
	"github.com/jixoai/keel/internal/store"
)

// --- the sidecar helper process -------------------------------------------

// TestSidecarHelperProcess is not a test: it is the sidecar's main, entered only
// when KEEL_TEST_SIDECAR is set on a re-executed test binary.
func TestSidecarHelperProcess(t *testing.T) {
	if os.Getenv("KEEL_TEST_SIDECAR") == "" {
		t.Skip("not a sidecar helper invocation")
	}
	serveSidecarHelper()
	os.Exit(0)
}

func serveSidecarHelper() {
	if path := os.Getenv("KEEL_TEST_SIDECAR_PIDFILE"); path != "" {
		_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644)
	}
	reader := gateway.NewFrameReader(os.Stdin, gateway.DefaultMaxFrameBytes)
	writer := gateway.NewFrameWriter(os.Stdout, gateway.DefaultMaxFrameBytes)
	write := func(message gateway.Message) bool {
		raw, err := json.Marshal(message)
		if err != nil {
			return false
		}
		return writer.WriteFrame(raw) == nil
	}
	for {
		data, err := reader.ReadFrame()
		if err != nil {
			return
		}
		message, parseErr := gateway.ParseMessage(data)
		if parseErr != nil {
			continue
		}
		switch typed := message.(type) {
		case *gateway.Notification:
			if typed.Method == gateway.MethodClose {
				if path := os.Getenv("KEEL_TEST_SIDECAR_CLOSEFILE"); path != "" {
					_ = os.WriteFile(path, []byte("close\n"), 0o644)
				}
				return
			}
		case *gateway.Request:
			result, failure := sidecarHelperHandle(typed)
			if failure != nil {
				if !write(&gateway.ErrorResponse{JSONRPC: gateway.JSONRPCVersion, ID: typed.ID, Error: *failure}) {
					return
				}
				continue
			}
			raw, err := json.Marshal(result)
			if err != nil {
				continue
			}
			if !write(&gateway.SuccessResponse{JSONRPC: gateway.JSONRPCVersion, ID: typed.ID, Result: raw}) {
				return
			}
		}
	}
}

func sidecarHelperToolNames() []string {
	raw := os.Getenv("KEEL_TEST_SIDECAR_TOOLS")
	if raw == "" {
		raw = "greet"
	}
	out := make([]string, 0, 4)
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func sidecarHelperHandle(request *gateway.Request) (any, *gateway.ErrorObject) {
	switch request.Method {
	case gateway.MethodInitialize:
		return sidecarHelperInitialize(), nil
	case gateway.MethodToolsList:
		return sidecarHelperTools(), nil
	case gateway.MethodToolsCall:
		return sidecarHelperCall(request)
	default:
		return nil, &gateway.ErrorObject{Code: gateway.CodeMethodNotFound, Message: "no such method: " + request.Method}
	}
}

func sidecarHelperInitialize() map[string]any {
	if os.Getenv("KEEL_TEST_SIDECAR_DECLARE") == "agent_backend" {
		return map[string]any{
			"protocolVersion": gateway.ProtocolVersion,
			"capabilities":    map[string]any{"agent": map[string]any{"events": true}},
			"serverInfo":      map[string]any{"name": "keel-test-sidecar", "version": "1.0.0"},
			"provider":        map[string]any{"type": "agent_backend"},
		}
	}
	version := gateway.ProtocolVersion
	if negotiated := os.Getenv("KEEL_TEST_SIDECAR_VERSION"); negotiated != "" {
		version = negotiated
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": "keel-test-sidecar", "version": "1.0.0"},
		"provider":        map[string]any{"type": "tool_provider"},
	}
}

func sidecarHelperTools() map[string]any {
	tools := make([]map[string]any, 0, 4)
	for _, name := range sidecarHelperToolNames() {
		tools = append(tools, map[string]any{
			"name":        name,
			"description": "sidecar tool " + name,
			"inputSchema": map[string]any{"type": "object", "additionalProperties": true},
		})
	}
	return map[string]any{"tools": tools}
}

func sidecarHelperCall(request *gateway.Request) (any, *gateway.ErrorObject) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(request.Params, &call); err != nil {
		return nil, &gateway.ErrorObject{Code: gateway.CodeInvalidParams, Message: err.Error()}
	}
	if name := os.Getenv("KEEL_TEST_SIDECAR_HANG_ON"); name != "" && name == call.Name {
		// Never answer: the host's request timeout is what ends this. A timer
		// (not `select{}`) keeps the Go runtime from declaring a deadlock and
		// exiting the helper before the host gives up.
		time.Sleep(10 * time.Minute)
	}
	if name := os.Getenv("KEEL_TEST_SIDECAR_CRASH_ON"); name != "" && name == call.Name {
		os.Exit(9)
	}
	switch call.Name {
	case "env":
		return textToolResult(strings.Join(os.Environ(), "\n")), nil
	case "pid":
		return textToolResult(fmt.Sprintf("pid=%d", os.Getpid())), nil
	case "spawn":
		child := exec.Command("sleep", "300")
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: false}
		if err := child.Start(); err != nil {
			return nil, &gateway.ErrorObject{Code: gateway.CodeInternalError, Message: err.Error()}
		}
		go func() { _ = child.Wait() }()
		return textToolResult(fmt.Sprintf("pid=%d child=%d", os.Getpid(), child.Process.Pid)), nil
	case "fail":
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": "the tool refused"}},
			"isError": true,
		}, nil
	case "image":
		return map[string]any{
			"content": []map[string]any{{"type": "image", "data": "aGVsbG8=", "mimeType": "image/png"}},
		}, nil
	default:
		return map[string]any{
			"content":           []map[string]any{{"type": "text", "text": "answered " + call.Name + " with " + string(call.Arguments)}},
			"structuredContent": map[string]any{"tool": call.Name},
		}, nil
	}
}

func textToolResult(text string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
}

// --- fixtures --------------------------------------------------------------

// sidecarHelperEnv is the environment every fixture passes to the helper. The
// *values* come from the host (t.Setenv); the manifest only declares the names
// it wants, which is exactly the contract the loader enforces.
var sidecarHelperEnvNames = []string{
	"KEEL_TEST_SIDECAR",
	"KEEL_TEST_SIDECAR_TOOLS",
	"KEEL_TEST_SIDECAR_DECLARE",
	"KEEL_TEST_SIDECAR_VERSION",
	"KEEL_TEST_SIDECAR_HANG_ON",
	"KEEL_TEST_SIDECAR_CRASH_ON",
	"KEEL_TEST_SIDECAR_PIDFILE",
	"KEEL_TEST_SIDECAR_CLOSEFILE",
	"KEEL_TEST_DECLARED_TOKEN",
}

// sidecarManifest renders a sidecar plugin.yaml. tools are the manifest's
// declared provides.tools; permissionsTools is permissions.tools.
func sidecarManifest(t *testing.T, id, version string, tools, permissionsTools []string) string {
	t.Helper()
	quote := func(values []string) string {
		quoted := make([]string, 0, len(values))
		for _, value := range values {
			quoted = append(quoted, "    - "+value)
		}
		return strings.Join(quoted, "\n")
	}
	body := fmt.Sprintf(`id: %s
version: %s
kind: sidecar
description: "test sidecar plugin"
engines:
  keel: ">=0.1.0 <1.0.0"
provides:
  tools:
%s
permissions:
  tools:
%s
entry:
  command: [%s, -test.run=^TestSidecarHelperProcess$]
`,
		id, version, quote(tools), quote(permissionsTools), sidecarHelperBinary(t))
	body += "  env:\n"
	for _, name := range sidecarHelperEnvNames {
		body += "    - " + name + "\n"
	}
	return body
}

// sidecarHelperBinary is the test binary itself, re-executed as the sidecar.
func sidecarHelperBinary(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// sidecarPackage writes a package directory and loads it through the real
// package loader, so the fixture is validated exactly like an installed plugin.
func sidecarPackage(t *testing.T, dir string, manifest string) pluginpkg.Package {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded := pluginpkg.LoadDir(dir, pluginpkg.Environment{KeelVersion: testKeelVersion(), KnownHookEvent: ValidHookEvent})
	if !loaded.Usable() {
		t.Fatalf("fixture package is invalid: %+v", loaded.Problem)
	}
	return loaded
}

func testKeelVersion() string { return "0.1.0" }

// newTestSidecarLoader builds a loader with short budgets, so a broken sidecar
// fails a test in seconds instead of a minute, and registers the environment
// every fixture needs.
func newTestSidecarLoader(t *testing.T, opts SidecarOptions) *SidecarLoader {
	t.Helper()
	t.Setenv("KEEL_TEST_SIDECAR", "1")
	if opts.Logger == nil {
		opts.Logger = testLogger()
	}
	opts.KeelVersion = testKeelVersion()
	opts.ConnectTimeout = 10 * time.Second
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = 5 * time.Second
	}
	loader := NewSidecarLoader(opts)
	t.Cleanup(loader.Close)
	return loader
}

func callTool(t *testing.T, plugin *Plugin, name string, args string) (string, error) {
	t.Helper()
	var spec *ToolSpec
	for i := range plugin.Tools {
		if plugin.Tools[i].Name == name {
			spec = &plugin.Tools[i]
			break
		}
	}
	if spec == nil {
		t.Fatalf("plugin %s has no tool %s (tools: %v)", plugin.ID, name, pluginToolNames(plugin))
	}
	result, err := spec.Execute(context.Background(), "call-1", json.RawMessage(args), nil)
	if err != nil {
		return "", err
	}
	var builder strings.Builder
	for _, block := range result.Content {
		if text, ok := block.(agentcore.TextContent); ok {
			builder.WriteString(text.Text)
		}
	}
	return builder.String(), nil
}

func pluginToolNames(plugin *Plugin) []string {
	out := make([]string, 0, len(plugin.Tools))
	for _, tool := range plugin.Tools {
		out = append(out, tool.Name)
	}
	return out
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func waitUntil(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", message)
}

// --- S1/S3: load, catalogue, call -----------------------------------------

func TestSidecarLoaderLoadsToolsAndCalls(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pkg := sidecarPackage(t, filepath.Join(dir, "demo.sidecar"), sidecarManifest(t, "demo.sidecar", "1.2.3", []string{"greet"}, []string{"greet"}))
	loader := newTestSidecarLoader(t, SidecarOptions{})
	t.Setenv("KEEL_TEST_SIDECAR_TOOLS", "greet")

	plugin, err := loader.Load(ctx, pkg, pkg.Manifest)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if plugin.ID != "demo.sidecar" || plugin.Version != "1.2.3" {
		t.Fatalf("plugin identity = %s@%s", plugin.ID, plugin.Version)
	}
	if got := pluginToolNames(plugin); len(got) != 1 || got[0] != "demo_sidecar_greet" {
		t.Fatalf("tools = %v, want [demo_sidecar_greet]", got)
	}
	if !contains(plugin.Provides, "cap:plugin.sidecar") {
		t.Fatalf("provides = %v, want the sidecar capability", plugin.Provides)
	}
	text, err := callTool(t, plugin, "demo_sidecar_greet", `{"who":"world"}`)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(text, "answered greet") || !strings.Contains(text, `{"who":"world"}`) {
		t.Fatalf("tool result = %q", text)
	}
	// The health surface reports the live process.
	health := loader.Health()
	if len(health) != 1 || health[0].PluginID != "demo.sidecar" || health[0].State != sidecarRunning || health[0].PID <= 0 {
		t.Fatalf("health = %+v", health)
	}
	pid := health[0].PID
	// Teardown: the plugin's own Effect stops the process (its Stop is the
	// mirror image), and the process really is gone.
	stopPlugin(t, plugin)
	waitUntil(t, 5*time.Second, func() bool { return !processAlive(pid) }, "the sidecar process is still alive after Stop")
	if len(loader.Health()) != 0 {
		t.Fatalf("health after stop = %+v, want empty", loader.Health())
	}
}

func stopPlugin(t *testing.T, plugin *Plugin) {
	t.Helper()
	for i := len(plugin.Effects) - 1; i >= 0; i-- {
		if err := plugin.Effects[i].Stop(context.Background()); err != nil {
			t.Fatalf("effect %s: %v", plugin.Effects[i].Name, err)
		}
	}
	if plugin.Stop != nil {
		if err := plugin.Stop(context.Background()); err != nil {
			t.Fatalf("stop: %v", err)
		}
	}
}

func TestSidecarLoaderRefusesNonToolProviders(t *testing.T) {
	for _, mode := range []string{"agent_backend", "version"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "pid")
			t.Setenv("KEEL_TEST_SIDECAR_PIDFILE", pidFile)
			if mode == "agent_backend" {
				t.Setenv("KEEL_TEST_SIDECAR_DECLARE", "agent_backend")
			} else {
				t.Setenv("KEEL_TEST_SIDECAR_VERSION", "1999-01-01")
			}
			pkg := sidecarPackage(t, filepath.Join(dir, "demo.sidecar"),
				sidecarManifest(t, "demo.sidecar", "1.0.0", []string{"greet"}, []string{"greet"}))
			loader := newTestSidecarLoader(t, SidecarOptions{})
			_, err := loader.Load(context.Background(), pkg, pkg.Manifest)
			if err == nil {
				t.Fatal("Load succeeded for a sidecar that does not speak the tool_provider contract")
			}
			if !errors.Is(err, ErrPluginKind) {
				t.Fatalf("error = %v, want ErrPluginKind", err)
			}
			// A refused plugin leaves no process behind.
			raw, readErr := os.ReadFile(pidFile)
			if readErr != nil {
				t.Fatalf("the helper never wrote its pid (%v): the spawn itself failed", readErr)
			}
			pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
			waitUntil(t, 5*time.Second, func() bool { return !processAlive(pid) }, "the refused sidecar is still running")
		})
	}
}

func TestSidecarLoaderRefusesUndeclaredTools(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("KEEL_TEST_SIDECAR_TOOLS", "greet,exfiltrate")
	pkg := sidecarPackage(t, filepath.Join(dir, "demo.sidecar"),
		sidecarManifest(t, "demo.sidecar", "1.0.0", []string{"greet"}, []string{"greet"}))

	var (
		mu      sync.Mutex
		denials []Denial
	)
	loader := newTestSidecarLoader(t, SidecarOptions{OnDenied: func(denial Denial) {
		mu.Lock()
		denials = append(denials, denial)
		mu.Unlock()
	}})
	plugin, err := loader.Load(ctx, pkg, pkg.Manifest)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer stopPlugin(t, plugin)
	if got := pluginToolNames(plugin); len(got) != 1 || got[0] != "demo_sidecar_greet" {
		t.Fatalf("tools = %v, want only the declared tool", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(denials) != 1 {
		t.Fatalf("denials = %+v, want exactly the undeclared tool", denials)
	}
	denial := denials[0]
	if denial.PluginID != "demo.sidecar" || denial.Kind != PermissionTool || denial.Target != "exfiltrate" {
		t.Fatalf("denial = %+v", denial)
	}
	if !strings.Contains(denial.Reason, "does not declare") {
		t.Fatalf("denial reason = %q", denial.Reason)
	}
}

func TestSidecarLoaderEnvWhitelistKeepsHostSecretsOut(t *testing.T) {
	ctx := context.Background()
	hostSecret := "keel-sidecar-host-secret-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Setenv("KEEL_TEST_HOST_SECRET", hostSecret)
	t.Setenv("KEEL_TEST_DECLARED_TOKEN", "declared-token-value")
	t.Setenv("KEEL_TEST_SIDECAR_TOOLS", "env")
	dir := t.TempDir()
	pkg := sidecarPackage(t, filepath.Join(dir, "demo.sidecar"),
		sidecarManifest(t, "demo.sidecar", "1.0.0", []string{"env"}, []string{"env"}))
	loader := newTestSidecarLoader(t, SidecarOptions{})

	plugin, err := loader.Load(ctx, pkg, pkg.Manifest)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer stopPlugin(t, plugin)
	text, err := callTool(t, plugin, "demo_sidecar_env", `{}`)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if strings.Contains(text, hostSecret) || strings.Contains(text, "KEEL_TEST_HOST_SECRET=") {
		t.Fatalf("the sidecar received a host secret its manifest never declared:\n%s", text)
	}
	if !strings.Contains(text, "KEEL_TEST_DECLARED_TOKEN=declared-token-value") {
		t.Fatalf("the declared variable did not reach the sidecar:\n%s", text)
	}
	// The host's own identity variables are always present, and they are the
	// host's, not the manifest's.
	for _, want := range []string{
		"KEEL_PLUGIN_ID=demo.sidecar",
		"KEEL_PLUGIN_VERSION=1.0.0",
		"KEEL_PLUGIN_PROTOCOL=" + gateway.ProtocolVersion,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the sidecar is missing %q:\n%s", want, text)
		}
	}
}

// --- S2: crash isolation, reconnect, teardown ------------------------------

func TestSidecarCrashIsolatesAndReconnects(t *testing.T) {
	ctx := context.Background()
	t.Setenv("KEEL_TEST_SIDECAR_TOOLS", "greet,crashme")
	t.Setenv("KEEL_TEST_SIDECAR_CRASH_ON", "crashme")
	dir := t.TempDir()
	pkg := sidecarPackage(t, filepath.Join(dir, "demo.sidecar"),
		sidecarManifest(t, "demo.sidecar", "1.0.0", []string{"greet", "crashme"}, []string{"greet", "crashme"}))
	var failures []string
	var failureMu sync.Mutex
	loader := newTestSidecarLoader(t, SidecarOptions{})
	loader.SetFailureSink(func(pluginID, version, reason string) {
		failureMu.Lock()
		failures = append(failures, pluginID+"@"+version+": "+reason)
		failureMu.Unlock()
	})

	plugin, err := loader.Load(ctx, pkg, pkg.Manifest)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := callTool(t, plugin, "demo_sidecar_greet", `{}`); err != nil {
		t.Fatalf("greet before the crash: %v", err)
	}
	if _, err := callTool(t, plugin, "demo_sidecar_crashme", `{}`); err == nil {
		t.Fatal("a tool whose sidecar process dies answered without an error")
	}
	// The supervision record reports the death, and the crash is audited once.
	waitUntil(t, 5*time.Second, func() bool {
		_, failed := loader.RuntimeFailure("demo.sidecar")
		return failed
	}, "the loader never reported the crashed sidecar")
	waitUntil(t, 5*time.Second, func() bool {
		failureMu.Lock()
		defer failureMu.Unlock()
		return len(failures) == 1
	}, "the failure sink never saw the crash")
	reason, _ := loader.RuntimeFailure("demo.sidecar")
	if !strings.Contains(reason, "exit") && !strings.Contains(reason, "exit status") {
		t.Fatalf("failure reason = %q, want the process exit status", reason)
	}
	// The dead plugin's tools now fail with a definite, named error instead of
	// hanging: this is what the model sees for a crashed sidecar.
	spec := plugin.Tools[0]
	if _, err := spec.Execute(ctx, "call-2", json.RawMessage(`{}`), nil); err == nil {
		t.Fatal("a tool of a crashed sidecar answered without an error")
	}

	// Reconnect: enabling the plugin again loads a fresh process (the manager's
	// enable path is the reconnect; Load is what it calls).
	reconnected, err := loader.Load(ctx, pkg, pkg.Manifest)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer stopPlugin(t, reconnected)
	if _, err := callTool(t, reconnected, "demo_sidecar_greet", `{}`); err != nil {
		t.Fatalf("greet after reconnect: %v", err)
	}
	if _, failed := loader.RuntimeFailure("demo.sidecar"); failed {
		t.Fatal("the reconnected sidecar is still reported as failed")
	}
}

func TestSidecarLoaderStopReapsTheProcessGroup(t *testing.T) {
	ctx := context.Background()
	t.Setenv("KEEL_TEST_SIDECAR_TOOLS", "spawn")
	dir := t.TempDir()
	pkg := sidecarPackage(t, filepath.Join(dir, "demo.sidecar"),
		sidecarManifest(t, "demo.sidecar", "1.0.0", []string{"spawn"}, []string{"spawn"}))
	loader := newTestSidecarLoader(t, SidecarOptions{})

	plugin, err := loader.Load(ctx, pkg, pkg.Manifest)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	text, err := callTool(t, plugin, "demo_sidecar_spawn", `{}`)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	var leader, child int
	if _, err := fmt.Sscanf(text, "pid=%d child=%d", &leader, &child); err != nil {
		t.Fatalf("cannot read the pids from %q: %v", text, err)
	}
	if !processAlive(leader) || !processAlive(child) {
		t.Fatalf("the sidecar tree is not alive: %q", text)
	}
	// The hostile case: the leader dies without shutdown, the grandchild
	// survives it (that is the orphan the negative control is about).
	if err := syscall.Kill(leader, syscall.SIGKILL); err != nil {
		t.Fatalf("kill the sidecar: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool { return !processAlive(leader) }, "the leader is still alive")
	if !processAlive(child) {
		t.Skip("the grandchild died with its parent: nothing left to reap (environment dependent)")
	}
	// Resuming the plugin tears the whole group down: no orphan survives.
	stopPlugin(t, plugin)
	waitUntil(t, 5*time.Second, func() bool { return !processAlive(child) }, "the sidecar's grandchild survived teardown (orphan)")
}

// --- S3: timeout and cancellation -----------------------------------------

func TestSidecarLoaderTimeoutAndCancellation(t *testing.T) {
	ctx := context.Background()
	t.Setenv("KEEL_TEST_SIDECAR_TOOLS", "greet,slowme")
	t.Setenv("KEEL_TEST_SIDECAR_HANG_ON", "slowme")
	dir := t.TempDir()
	pkg := sidecarPackage(t, filepath.Join(dir, "demo.sidecar"),
		sidecarManifest(t, "demo.sidecar", "1.0.0", []string{"greet", "slowme"}, []string{"greet", "slowme"}))
	loader := newTestSidecarLoader(t, SidecarOptions{RequestTimeout: 400 * time.Millisecond})

	plugin, err := loader.Load(ctx, pkg, pkg.Manifest)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer stopPlugin(t, plugin)

	// A sidecar that never answers is ended by the request budget.
	started := time.Now()
	spec := toolSpecNamed(t, plugin, "demo_sidecar_slowme")
	_, err = spec.Execute(ctx, "call-1", json.RawMessage(`{}`), nil)
	if err == nil {
		t.Fatal("a request that never answers succeeded")
	}
	var timeoutErr *gateway.TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("error = %v (%T), want a gateway timeout", err, err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the request budget did not bound the call: %s", elapsed)
	}

	// A canceled caller context ends the call immediately, and the process
	// survives: cancellation aborts one request, it does not kill the plugin.
	cancelCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, callErr := spec.Execute(cancelCtx, "call-2", json.RawMessage(`{}`), nil)
		done <- callErr
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case callErr := <-done:
		if callErr == nil {
			t.Fatal("a canceled request succeeded")
		}
		var canceled *gateway.CanceledError
		if !errors.As(callErr, &canceled) {
			t.Fatalf("error = %v (%T), want a gateway cancellation", callErr, callErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a canceled request did not return")
	}
	health := loader.Health()
	if len(health) != 1 || health[0].State != sidecarRunning {
		t.Fatalf("health after cancellation = %+v, want the plugin still running", health)
	}
	if !processAlive(health[0].PID) {
		t.Fatalf("pid %d died from a canceled request", health[0].PID)
	}
}

func TestSidecarLoaderPoliteCloseNotification(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	closeFile := filepath.Join(dir, "closed")
	t.Setenv("KEEL_TEST_SIDECAR_CLOSEFILE", closeFile)
	t.Setenv("KEEL_TEST_SIDECAR_TOOLS", "greet")
	pkg := sidecarPackage(t, filepath.Join(dir, "demo.sidecar"),
		sidecarManifest(t, "demo.sidecar", "1.0.0", []string{"greet"}, []string{"greet"}))
	loader := newTestSidecarLoader(t, SidecarOptions{CloseNoticeGrace: 3 * time.Second})

	plugin, err := loader.Load(ctx, pkg, pkg.Manifest)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	stopPlugin(t, plugin)
	if _, err := os.Stat(closeFile); err != nil {
		t.Fatalf("the sidecar never received the close notification: %v", err)
	}
}

// --- sealed behaviours ----------------------------------------------------

func TestSidecarLoaderRefusesPackagesWithoutACommand(t *testing.T) {
	dir := t.TempDir()
	raw := `id: demo.sidecar
version: 1.0.0
kind: sidecar
description: "no command"
`
	path := filepath.Join(dir, "demo.sidecar")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "plugin.yaml"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	// The manifest itself is invalid (a sidecar requires entry.command), so the
	// package never reaches the loader; the manifest check is pluginpkg's.
	loaded := pluginpkg.LoadDir(path, pluginpkg.Environment{KeelVersion: testKeelVersion(), KnownHookEvent: ValidHookEvent})
	if loaded.Usable() {
		t.Fatal("a sidecar manifest without entry.command validated")
	}
}

func TestSidecarLoaderRelativeCommandResolvesInsideThePackage(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "demo.sidecar", "bin", "sidecar.sh")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`id: demo.sidecar
version: 1.0.0
kind: sidecar
description: "relative command"
entry:
  command: [./bin/sidecar.sh]
  args: [--serve]
`)
	pkg := sidecarPackage(t, filepath.Join(dir, "demo.sidecar"), manifest)
	loader := newTestSidecarLoader(t, SidecarOptions{ConnectTimeout: 2 * time.Second})
	_, err := loader.Load(context.Background(), pkg, pkg.Manifest)
	// The script exits immediately, so the handshake cannot complete: the point
	// of the test is that the relative command resolved (the failure is the
	// handshake, not "no such file").
	if err == nil {
		t.Fatal("a sidecar that exits immediately completed a handshake")
	}
	if strings.Contains(err.Error(), "no such file") {
		t.Fatalf("the relative command did not resolve inside the package: %v", err)
	}
	if !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("error = %v, want a handshake failure", err)
	}
}

func TestSidecarToolResultsMapOntoTheRuntime(t *testing.T) {
	ctx := context.Background()
	t.Setenv("KEEL_TEST_SIDECAR_TOOLS", "fail,image")
	dir := t.TempDir()
	pkg := sidecarPackage(t, filepath.Join(dir, "demo.sidecar"),
		sidecarManifest(t, "demo.sidecar", "1.0.0", []string{"fail", "image"}, []string{"fail", "image"}))
	loader := newTestSidecarLoader(t, SidecarOptions{})
	plugin, err := loader.Load(ctx, pkg, pkg.Manifest)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer stopPlugin(t, plugin)

	fail := toolSpecNamed(t, plugin, "demo_sidecar_fail")
	result, err := fail.Execute(ctx, "call-1", json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("fail tool: %v", err)
	}
	if result.IsError == nil || !*result.IsError {
		t.Fatalf("a sidecar isError result did not become a tool error: %+v", result)
	}
	image := toolSpecNamed(t, plugin, "demo_sidecar_image")
	result, err = image.Execute(ctx, "call-2", json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("image tool: %v", err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("image result content = %+v", result.Content)
	}
}

func toolSpecNamed(t *testing.T, plugin *Plugin, name string) ToolSpec {
	t.Helper()
	for _, tool := range plugin.Tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("plugin %s has no tool %s (tools: %v)", plugin.ID, name, pluginToolNames(plugin))
	return ToolSpec{}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// --- manager integration ---------------------------------------------------

// TestManagerSidecarLifecycle drives the whole kernel with a real sidecar: the
// manager installs the package into its managed root, enables it (which spawns
// the process and publishes the tools), survives a crash (state failed, tools
// error, audit row), reconnects on the next enable, and on disable removes the
// tool from the model's surface and reaps the process.
func TestManagerSidecarLifecycle(t *testing.T) {
	ctx := context.Background()
	t.Setenv("KEEL_TEST_SIDECAR_TOOLS", "greet,crashme")
	t.Setenv("KEEL_TEST_SIDECAR_CRASH_ON", "crashme")
	root := t.TempDir()
	sidecarLoader := newTestSidecarLoader(t, SidecarOptions{})
	harness := newSidecarHarness(t, root, sidecarLoader)

	source := sidecarPackage(t, t.TempDir(),
		sidecarManifest(t, "demo.sidecar", "1.0.0", []string{"greet", "crashme"}, []string{"greet", "crashme"})).Dir
	view, err := harness.manager.Install(ctx, InstallRequest{Path: source}, AuditContext{Actor: "test"})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if view.State != LifecycleInstalled || view.Kind != "sidecar" {
		t.Fatalf("installed view = %+v", view)
	}
	view, err = harness.manager.Enable(ctx, "demo.sidecar", AuditContext{Actor: "test"})
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if view.State != LifecycleEnabled {
		t.Fatalf("enabled view = %+v", view)
	}
	if !contains(harness.tools.list(), "demo_sidecar_greet") {
		t.Fatalf("the model's tool surface = %v, want the sidecar tool", harness.tools.list())
	}
	if !contains(view.Provides.Tools, "demo_sidecar_greet") {
		t.Fatalf("view provides = %v, want the published tool name", view.Provides.Tools)
	}
	if view.Runtime == nil || view.Runtime.State != PluginActive {
		t.Fatalf("runtime view = %+v", view.Runtime)
	}
	health := sidecarLoader.Health()
	if len(health) != 1 || health[0].PID <= 0 {
		t.Fatalf("health = %+v", health)
	}
	pid := health[0].PID

	// A crash is reported as failed with the reason, and the crash is audited.
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool { return !processAlive(pid) }, "the sidecar is still alive")
	waitUntil(t, 5*time.Second, func() bool {
		for _, plugin := range harness.manager.Snapshot().Plugins {
			if plugin.ID == "demo.sidecar" {
				return plugin.State == LifecycleFailed
			}
		}
		return false
	}, "the inventory never reported the crashed sidecar as failed")
	waitUntil(t, 5*time.Second, func() bool {
		return containsAction(harness.store.auditActions(t), "enable:failed")
	}, "the crash was not audited")

	// Reconnect: enable again loads a fresh process.
	view, err = harness.manager.Enable(ctx, "demo.sidecar", AuditContext{Actor: "test"})
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if view.State != LifecycleEnabled {
		t.Fatalf("reconnected view = %+v", view)
	}
	health = sidecarLoader.Health()
	if len(health) != 1 || health[0].PID == pid {
		t.Fatalf("the reconnect did not start a new process: %+v", health)
	}
	newPID := health[0].PID

	// Disable: the model's surface loses the tool and the process is gone.
	view, err = harness.manager.Disable(ctx, "demo.sidecar", AuditContext{Actor: "test"})
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if view.State != LifecycleDisabled || len(view.Tools) != 0 {
		t.Fatalf("disabled view = %+v", view)
	}
	if contains(harness.tools.list(), "demo_sidecar_greet") {
		t.Fatalf("the model's tool surface still has the disabled plugin's tool: %v", harness.tools.list())
	}
	waitUntil(t, 5*time.Second, func() bool { return !processAlive(newPID) }, "the disabled plugin's process is still alive")

	if _, err := harness.manager.Uninstall(ctx, "demo.sidecar", AuditContext{Actor: "test"}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	for _, plugin := range harness.manager.Snapshot().Plugins {
		if plugin.ID == "demo.sidecar" {
			t.Fatalf("the uninstalled plugin is still in the inventory: %+v", plugin)
		}
	}
}

// TestManagerAuditsUndeclaredSidecarTools is the P3 negative control at the
// manager level: a sidecar that offers a tool its manifest never declared has the
// tool refused and an audit row written.
func TestManagerAuditsUndeclaredSidecarTools(t *testing.T) {
	ctx := context.Background()
	t.Setenv("KEEL_TEST_SIDECAR_TOOLS", "greet,exfiltrate")
	root := t.TempDir()
	sidecarLoader := newTestSidecarLoader(t, SidecarOptions{})
	harness := newSidecarHarness(t, root, sidecarLoader)

	source := sidecarPackage(t, t.TempDir(),
		sidecarManifest(t, "demo.sidecar", "1.0.0", []string{"greet"}, []string{"greet"})).Dir
	if _, err := harness.manager.Install(ctx, InstallRequest{Path: source}, AuditContext{Actor: "test"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := harness.manager.Enable(ctx, "demo.sidecar", AuditContext{Actor: "test"}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	rows, err := harness.manager.Audit(ctx, nil, "demo.sidecar", 0, 100)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	var denial *store.PluginAuditEntry
	for i := range rows {
		if rows[i].Action == PluginActionDeny {
			denial = &rows[i]
		}
	}
	if denial == nil {
		t.Fatalf("no denial row in %+v", rows)
	}
	if denial.Result != store.PluginResultRefused || denial.Actor != "plugin-gate" {
		t.Fatalf("denial row = %+v", denial)
	}
	var detail struct {
		Kind   string `json:"kind"`
		Target string `json:"target"`
	}
	if err := json.Unmarshal(denial.Detail, &detail); err != nil {
		t.Fatalf("denial detail = %s", denial.Detail)
	}
	if detail.Kind != PermissionTool || detail.Target != "exfiltrate" {
		t.Fatalf("denial detail = %+v", detail)
	}
	if contains(harness.tools.list(), "demo_sidecar_exfiltrate") || contains(harness.tools.list(), "demo.sidecar.exfiltrate") {
		t.Fatalf("the undeclared tool reached the model: %v", harness.tools.list())
	}
	if !contains(harness.tools.list(), "demo_sidecar_greet") {
		t.Fatalf("the declared tool is not published: %v", harness.tools.list())
	}
}

// newSidecarHarness builds the manager with a real runtime and the sidecar
// loader under test; the store is the in-memory fake the manager tests use.
func newSidecarHarness(t *testing.T, root string, loader PluginLoader) *managerHarness {
	t.Helper()
	hooks := NewHookBus(HookBusOptions{Logger: testLogger(), Queue: 64, Workers: 1})
	triggers := NewTriggerEngine(TriggerEngineOptions{Starter: &countingStarter{}, Logger: testLogger()})
	recorder := &toolRecorder{}
	clock := &testClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	runtime := NewRuntime(RuntimeOptions{
		Hooks:    hooks,
		Triggers: triggers,
		Tools:    recorder.sink,
		Logger:   testLogger(),
		Now:      clock.tick,
	})
	persistence := newFakePluginStore()
	manager, err := NewPluginManager(ManagerOptions{
		Root:        root,
		Runtime:     runtime,
		Persistence: persistence,
		Loaders:     map[pluginpkg.Kind]PluginLoader{pluginpkg.KindSidecar: loader},
		KeelVersion: testKeelVersion(),
		Logger:      testLogger(),
		Now:         clock.tick,
	})
	if err != nil {
		t.Fatal(err)
	}
	harness := &managerHarness{manager: manager, runtime: runtime, store: persistence, root: root, hooks: hooks, tools: recorder, clock: clock}
	t.Cleanup(func() {
		runtime.Close(context.Background())
		triggers.Close()
		hooks.Close()
	})
	return harness
}
