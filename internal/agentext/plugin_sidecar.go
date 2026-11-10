package agentext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/gateway"
	"github.com/jixoai/keel/internal/pluginpkg"
)

// Sidecar plugin loader (change keel-plugin-core, spec S1-S3: `kind: sidecar`).
//
// A sidecar plugin is an external process that speaks the gateway's stdio
// line-JSON-RPC protocol (internal/gateway): the child's stdin/stdout carry
// newline-delimited JSON-RPC 2.0 frames, and the host is the client. Nothing
// about the wire is re-invented here:
//
//	transport  gateway.SpawnStdio  (newline framing, bounded queues, backpressure,
//	                                process-group leadership, shutdown ladder)
//	handshake  gateway.Conn.Connect (initialize + protocol-revision/provider-kind/
//	                                capability validation: a sidecar that does not
//	                                speak the contract does not activate)
//	tools      gateway.ToolClient  (tools/list, tools/call — MCP-verbatim, the
//	                                protocol's tool surface)
//	env        gateway.FilterEnv   (the allowlist that keeps host credentials out)
//	teardown   gateway process-group primitives, through the transport's Close
//
// The verbs of the spec's S1 list map onto that contract as follows:
//
//	initialize  the handshake (Conn.Connect): the sidecar answers with
//	            protocolVersion, provider{type: tool_provider}, capabilities.tools
//	            and serverInfo; anything else is refused before any tool runs.
//	tools.list  tools/list (ToolClient.ListTools, paging included).
//	tools.call  tools/call (ToolClient.CallTool): request timeout, caller
//	            cancellation (propagated as notifications/cancelled) and a dead
//	            connection all surface as definite errors.
//	health      supervision-level: the loader's own health report per plugin
//	            (connection health + process liveness + the recorded failure),
//	            which the manager surfaces as the plugin's state. The reused
//	            protocol has no health method on the tool surface — a
//	            tool_provider connection rejects the agent surface's methods by
//	            construction (see internal/gateway/README.md §1.2/§1.3), and
//	            inventing one would be a second protocol.
//	close       supervision-level, and it does go on the wire: the loader sends
//	            the protocol's `close` method as a best-effort notification
//	            (a sidecar that implements it exits on its own), then runs the
//	            transport's documented shutdown ladder: stdin close → SIGTERM to
//	            the whole process group → SIGKILL to the whole process group.
//
// Isolation is honest: the host controls what the process *receives* (argv, cwd,
// environment) and what it *speaks* (the protocol), and it reaps the whole
// process group on teardown. It does not sandbox the process's own syscalls —
// an in-process plugin is not sandboxed either (spec: 诚实边界); both are
// trusted-by-installation code whose *capability requests* go through the
// permission gate (plugin_permissions.go).

// SidecarOptions configures a SidecarLoader.
type SidecarOptions struct {
	// BaseEnv is the host environment the sidecar's environment is filtered
	// from (normally os.Environ()). A nil value uses os.Environ(): unlike a
	// hand-built child environment, this one can only ever grow by names the
	// manifest declared.
	BaseEnv []string
	// Stderr receives the sidecar's stderr (diagnostics must not corrupt the
	// protocol stream). Nil discards it into the loader's logger at debug level.
	Stderr io.Writer
	// KeelVersion is reported to the sidecar in initialize and in KEEL_VERSION.
	KeelVersion string
	// ConnectTimeout bounds spawn + initialize + tools/list for one activation.
	// Zero uses 15s.
	ConnectTimeout time.Duration
	// RequestTimeout bounds one tools/call when the caller's context carries no
	// deadline of its own. Zero uses 60s.
	RequestTimeout time.Duration
	// CloseGrace bounds each step of the shutdown ladder. Zero uses the
	// transport default (2s); negative kills immediately.
	CloseGrace time.Duration
	// CloseNoticeGrace is how long the loader lets the sidecar act on the
	// best-effort `close` notification before the ladder starts. Zero uses
	// 250ms; negative skips the notification.
	CloseNoticeGrace time.Duration
	// OnDenied, when set, receives every permission denial at the sidecar
	// boundary (an undeclared tool). The plugin manager installs it (through
	// SetDenialSink) so every refusal lands in the audit trail (spec P3).
	OnDenied func(Denial)
	Logger   *slog.Logger
	Now      func() time.Time
}

// SidecarHealth is the supervision view of one loaded sidecar.
type SidecarHealth struct {
	// PluginID is the plugin the runtime belongs to.
	PluginID string `json:"pluginId"`
	// PID is the sidecar's process id (0 when no process is managed).
	PID int `json:"pid,omitempty"`
	// State is "running" or "failed".
	State string `json:"state"`
	// Reason carries the failure reason (empty while running).
	Reason string `json:"reason,omitempty"`
	// StartedAt stamps the activation.
	StartedAt time.Time `json:"startedAt"`
}

// Sidecar process states.
const (
	sidecarRunning = "running"
	sidecarFailed  = "failed"
)

// SidecarLoader materializes `kind: sidecar` packages into runtime plugins. One
// loader supervises every sidecar it loaded and is safe for concurrent use.
type SidecarLoader struct {
	opts SidecarOptions
	log  *slog.Logger

	mu        sync.Mutex
	instances map[string]*sidecarInstance
	denials   func(Denial)
	failures  func(pluginID, version, reason string)
}

// sidecarInstance is one supervision record: the live connection, the process
// it owns and what happened to it.
type sidecarInstance struct {
	id        string
	version   string
	conn      *gateway.Conn
	transport *gateway.StdioTransport
	client    *gateway.ToolClient

	mu        sync.Mutex
	stopped   bool
	failed    bool
	reason    string
	startedAt time.Time
	pid       int
}

// NewSidecarLoader builds the loader for `kind: sidecar` packages. It touches
// nothing until a plugin is loaded.
func NewSidecarLoader(opts SidecarOptions) *SidecarLoader {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = 15 * time.Second
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = 60 * time.Second
	}
	if opts.CloseNoticeGrace == 0 {
		opts.CloseNoticeGrace = 250 * time.Millisecond
	}
	return &SidecarLoader{
		opts:      opts,
		log:       opts.Logger,
		instances: make(map[string]*sidecarInstance),
		denials:   opts.OnDenied,
	}
}

// SetDenialSink installs the audit sink for permission denials. The plugin
// manager wires it to the plugin audit trail; the latest sink wins (a
// deployment builds one loader and one manager).
func (l *SidecarLoader) SetDenialSink(sink func(Denial)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.denials = sink
}

// SetFailureSink installs the sink that receives the death of a supervised
// sidecar (plugin id, version, reason) the moment it happens.
func (l *SidecarLoader) SetFailureSink(sink func(pluginID, version, reason string)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures = sink
}

// Load implements PluginLoader: it spawns the package's sidecar, negotiates the
// handshake, reads the tool catalogue and returns the runtime plugin. Any
// failure tears the process down before returning, so a refused plugin never
// leaves a process behind (spec S2: no orphans).
func (l *SidecarLoader) Load(ctx context.Context, pkg pluginpkg.Package, manifest *pluginpkg.Manifest) (*Plugin, error) {
	if manifest == nil {
		return nil, errors.New("agentext: sidecar loader needs the plugin manifest")
	}
	if manifest.Kind != pluginpkg.KindSidecar && manifest.Kind != pluginpkg.KindWebview {
		return nil, fmt.Errorf("agentext: sidecar loader does not load kind %s", manifest.Kind)
	}
	command, args, err := l.resolveCommand(pkg, manifest)
	if err != nil {
		return nil, err
	}
	connectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.opts.ConnectTimeout)
	defer cancel()

	transport, err := gateway.SpawnStdio(gateway.SpawnOptions{
		Command:    command,
		Args:       args,
		Dir:        pkg.Dir,
		Env:        l.environment(pkg, manifest),
		Stderr:     l.stderr(manifest.ID),
		CloseGrace: l.opts.CloseGrace,
	})
	if err != nil {
		return nil, fmt.Errorf("agentext: plugin %s: spawn sidecar: %w", manifest.ID, err)
	}
	// A refused plugin must not leave a process behind: every early return
	// below closes the transport, which is the documented shutdown ladder.
	conn := gateway.NewConn(gateway.ConnOptions{
		ClientInfo:      gateway.Implementation{Name: "keel-plugin-host", Version: l.version()},
		AcceptProviders: []gateway.ProviderType{gateway.ProviderTypeToolProvider},
		RequestTimeout:  l.opts.RequestTimeout,
		OnDiagnostic: func(diagnostic gateway.Diagnostic) {
			l.log.Warn("plugin sidecar diagnostic", "plugin", manifest.ID, "kind", diagnostic.Kind, "detail", diagnostic.Detail)
		},
		OnCall: func(record gateway.CallRecord) {
			l.log.Debug("plugin sidecar call", "plugin", manifest.ID, "method", record.Method,
				"target", record.Target, "status", record.Status, "duration_ms", record.Duration.Milliseconds())
		},
	})
	if _, err := conn.Connect(connectCtx, transport); err != nil {
		_ = transport.Close()
		return nil, NewPluginError("assemble", manifest.ID, ErrPluginKind,
			"sidecar handshake failed: %v", err)
	}
	client := gateway.NewToolProvider(conn)
	definitions, err := client.ListTools(connectCtx)
	if err != nil {
		_ = conn.Close()
		return nil, NewPluginError("assemble", manifest.ID, ErrPluginKind,
			"sidecar tools/list failed: %v", err)
	}

	instance := &sidecarInstance{
		id:        manifest.ID,
		version:   manifest.Version,
		conn:      conn,
		transport: transport,
		client:    client,
		startedAt: l.now(),
		pid:       transport.PID(),
	}
	l.register(instance)

	tools, refused := l.catalog(manifest, definitions)
	if len(refused) > 0 {
		l.reportDenials(manifest, refused)
	}
	plugin := &Plugin{
		ID:       manifest.ID,
		Version:  manifest.Version,
		Provides: []string{"cap:plugin.sidecar"},
		Tools:    tools,
		// The process is the plugin's side effect: it is cleaned up exactly once,
		// before the plugin's own Stop runs (plugin.go's reverse-order contract),
		// so a half-assembled plugin never keeps a process alive.
		Effects: []Effect{{
			Name: "sidecar-process",
			Stop: func(context.Context) error {
				l.stop(instance, "unloaded")
				return nil
			},
		}},
		Stop: func(context.Context) error {
			l.stop(instance, "stopped")
			return nil
		},
	}
	go l.watch(instance)
	l.log.Info("plugin sidecar loaded", "plugin", manifest.ID, "pid", instance.pid,
		"tools", len(tools), "refused", len(refused))
	return plugin, nil
}

// RuntimeFailure implements RuntimeHealthReporter: it reports the reason a
// previously loaded sidecar died, so the manager can show the plugin as failed
// while the persisted intent still says enabled (the same rule the runtime's
// own activation failures follow).
func (l *SidecarLoader) RuntimeFailure(pluginID string) (string, bool) {
	l.mu.Lock()
	instance := l.instances[pluginID]
	l.mu.Unlock()
	if instance == nil {
		return "", false
	}
	instance.mu.Lock()
	defer instance.mu.Unlock()
	if !instance.failed {
		return "", false
	}
	return instance.reason, true
}

// Health reports every sidecar the loader supervises (diagnostics surface).
func (l *SidecarLoader) Health() []SidecarHealth {
	l.mu.Lock()
	instances := make([]*sidecarInstance, 0, len(l.instances))
	for _, instance := range l.instances {
		instances = append(instances, instance)
	}
	l.mu.Unlock()
	out := make([]SidecarHealth, 0, len(instances))
	for _, instance := range instances {
		instance.mu.Lock()
		state, reason := sidecarRunning, ""
		if instance.failed {
			state, reason = sidecarFailed, instance.reason
		}
		health := SidecarHealth{PluginID: instance.id, PID: instance.pid, State: state, Reason: reason, StartedAt: instance.startedAt}
		instance.mu.Unlock()
		out = append(out, health)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PluginID < out[j].PluginID })
	return out
}

// Close stops every sidecar the loader still supervises. It is the loader's
// own teardown, for hosts that drop the runtime without unloading plugins.
func (l *SidecarLoader) Close() {
	l.mu.Lock()
	instances := make([]*sidecarInstance, 0, len(l.instances))
	for _, instance := range l.instances {
		instances = append(instances, instance)
	}
	l.mu.Unlock()
	for _, instance := range instances {
		l.stop(instance, "loader closed")
	}
}

// --- catalogue ------------------------------------------------------------

// sidecarRefusal is one tool the sidecar offered but its manifest did not
// declare.
type sidecarRefusal struct {
	name   string
	reason string
}

// catalog turns the sidecar's tool definitions into runtime tool specs. The
// manifest's provides.tools is the declared surface (spec: 工具白名单在派发处校验):
// a sidecar that lists a tool its manifest never declared gets that tool
// REFUSED — it is not published to the model, and the refusal is audited. A
// plugin cannot widen its own surface at run time.
//
// Published names are `<plugin>.<tool>` (spec S3), normalized to the wire
// grammar (spec: 发布名必须是 wire 安全的 `<a-zA-Z0-9_->{1,64}`): the sidecar
// speaks its own local names, the host namespaces them, so two plugins can both
// ship a `search` tool without a global-uniqueness accident, and `demo.sidecar`
// + `greet` is published as `demo_sidecar_greet` — a name a real model can call.
// A declared entry matches either the local name or the already-namespaced one,
// because both readings of "provides.tools" are natural in a manifest.
func (l *SidecarLoader) catalog(manifest *pluginpkg.Manifest, definitions []gateway.ToolDefinition) ([]ToolSpec, []sidecarRefusal) {
	declared := make(map[string]bool, len(manifest.Provides.Tools))
	for _, name := range manifest.Provides.Tools {
		declared[name] = true
	}
	tools := make([]ToolSpec, 0, len(definitions))
	var refused []sidecarRefusal
	seen := make(map[string]bool, len(definitions))
	for _, definition := range definitions {
		local := definition.Name
		qualified := qualifiedToolName(manifest.ID, local)
		if !declaredTool(declared, manifest.ID, local) {
			refused = append(refused, sidecarRefusal{name: local,
				reason: fmt.Sprintf("the sidecar offers tool %q but the manifest does not declare it in provides.tools (expected %q)", local, qualified)})
			continue
		}
		// A published name outside the wire grammar cannot be called by a model
		// (manifest validation already refuses such a declaration; this is the
		// runtime half of the same rule, for a package edited on disk).
		if !pluginpkg.ValidPublishedToolName(qualified) {
			refused = append(refused, sidecarRefusal{name: local,
				reason: fmt.Sprintf("the published tool name %q (from %q) must match ^[A-Za-z0-9_-]{1,%d}$",
					qualified, local, pluginpkg.MaxPublishedToolName)})
			continue
		}
		if seen[qualified] {
			refused = append(refused, sidecarRefusal{name: local,
				reason: fmt.Sprintf("the sidecar lists tool %q twice", local)})
			continue
		}
		seen[qualified] = true
		tools = append(tools, l.toolSpec(manifest, definition, qualified))
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools, refused
}

// qualifiedToolName is the model-facing name of a sidecar's local tool: the
// host namespaces it, so two plugins can both ship a `search` tool, and the
// result is normalized to the wire grammar (`.` is not a callable function name
// on an OpenAI-compatible endpoint). A sidecar that already namespaces its own
// tools keeps its name; both spellings normalize to the same publication name.
func qualifiedToolName(pluginID, local string) string {
	if strings.HasPrefix(local, pluginID+".") {
		return pluginpkg.NormalizePublishedToolName(local)
	}
	return pluginpkg.NamespacedPublishedToolName(pluginID, local)
}

// declaredTool reports whether the manifest declares this sidecar tool, in
// either spelling: the local name, the qualified name, or the local name of an
// already-qualified listing.
func declaredTool(declared map[string]bool, pluginID, local string) bool {
	if declared[local] || declared[pluginID+"."+local] {
		return true
	}
	if bare, ok := strings.CutPrefix(local, pluginID+"."); ok && declared[bare] {
		return true
	}
	return false
}

// toolSpec wraps one sidecar tool as a runtime ToolSpec. The dispatch is bound
// to the caller's context (so a canceled turn cancels the call and the gateway
// propagates notifications/cancelled) and to the connection of *this*
// activation; a dead process surfaces as a definite error, never as a hang.
func (l *SidecarLoader) toolSpec(manifest *pluginpkg.Manifest, definition gateway.ToolDefinition, qualified string) ToolSpec {
	local := definition.Name
	schema := definition.InputSchema
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object","additionalProperties":true}`)
	}
	description := definition.Description
	if description == "" {
		description = definition.Title
	}
	if description == "" {
		description = fmt.Sprintf("sidecar tool %s of plugin %s", local, manifest.ID)
	}
	return ToolSpec{
		Name:        qualified,
		Description: description,
		Schema:      schema,
		Execute: func(ctx context.Context, id string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
			client, err := l.clientOf(manifest.ID)
			if err != nil {
				return agentcore.AgentToolResult{}, fmt.Errorf("plugin %s tool %s: %w", manifest.ID, local, err)
			}
			result, err := client.CallTool(ctx, local, args)
			if err != nil {
				return agentcore.AgentToolResult{}, fmt.Errorf("plugin %s tool %s: %w", manifest.ID, local, err)
			}
			return sidecarToolResult(result), nil
		},
	}
}

// clientOf resolves the live connection for a plugin. It fails fast when the
// sidecar process is gone (the model then sees a definite, named error).
func (l *SidecarLoader) clientOf(pluginID string) (*gateway.ToolClient, error) {
	l.mu.Lock()
	instance := l.instances[pluginID]
	l.mu.Unlock()
	if instance == nil {
		return nil, errors.New("the plugin is not loaded")
	}
	instance.mu.Lock()
	failed, reason := instance.failed, instance.reason
	instance.mu.Unlock()
	if failed {
		return nil, fmt.Errorf("the plugin process is not available: %s", reason)
	}
	return instance.client, nil
}

// sidecarToolResult maps the protocol's tool result onto the runtime's.
func sidecarToolResult(result gateway.ToolResult) agentcore.AgentToolResult {
	out := agentcore.AgentToolResult{Content: make(agentcore.ContentList, 0, len(result.Content))}
	for _, block := range result.Content {
		switch block.Type {
		case "text":
			out.Content = append(out.Content, agentcore.NewTextContent(block.Text))
		case "image":
			out.Content = append(out.Content, agentcore.NewImageContent(block.Data, block.MimeType))
		default:
			raw, marshalErr := json.Marshal(block)
			if marshalErr != nil {
				continue
			}
			out.Content = append(out.Content, agentcore.NewTextContent(string(raw)))
		}
	}
	if len(result.StructuredContent) > 0 {
		var structured any
		if err := json.Unmarshal(result.StructuredContent, &structured); err == nil {
			out.StructuredContent = structured
		}
	}
	if result.IsError {
		isError := true
		out.IsError = &isError
	}
	return out
}

// --- supervision ----------------------------------------------------------

// watch records the plugin's death the moment the connection ends, unless the
// loader itself stopped it. The record is what RuntimeFailure reports and what
// the manager renders as the failed state.
func (l *SidecarLoader) watch(instance *sidecarInstance) {
	<-instance.conn.Done()
	instance.mu.Lock()
	if instance.stopped {
		instance.mu.Unlock()
		return
	}
	reason := "the plugin process exited"
	if terminal := instance.conn.Terminal(); terminal != nil {
		reason = terminal.Error()
	}
	if state := instance.transport.ExitState(); state != nil {
		reason = "the plugin process exited: " + state.String()
	}
	instance.failed = true
	instance.reason = reason
	instance.mu.Unlock()
	l.log.Warn("plugin sidecar died", "plugin", instance.id, "pid", instance.pid, "reason", reason)
	l.mu.Lock()
	sink := l.failures
	l.mu.Unlock()
	if sink != nil {
		sink(instance.id, instance.version, reason)
	}
}

// stop shuts one sidecar down: the best-effort `close` notification, then the
// transport's ladder (stdin close, then SIGTERM/SIGKILL to the whole process
// group, so nothing the sidecar spawned survives it).
func (l *SidecarLoader) stop(instance *sidecarInstance, why string) {
	instance.mu.Lock()
	if instance.stopped {
		instance.mu.Unlock()
		return
	}
	instance.stopped = true
	// A connection that already ended (a crash) gets no `close` notification and
	// no grace: writing to a dead stream only produces a spurious diagnostic.
	alreadyFailed := instance.failed
	instance.mu.Unlock()
	l.unregister(instance)

	if !alreadyFailed && l.opts.CloseNoticeGrace > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		// Best effort: a sidecar that implements the notification exits on its
		// own; one that does not never sees the ladder differently.
		_ = instance.transport.Send(ctx, &gateway.Notification{JSONRPC: gateway.JSONRPCVersion, Method: gateway.MethodClose})
		cancel()
		deadline := time.Now().Add(l.opts.CloseNoticeGrace)
		for time.Now().Before(deadline) {
			if instance.transport.ExitState() != nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	_ = instance.conn.Close()
	l.log.Info("plugin sidecar stopped", "plugin", instance.id, "pid", instance.pid, "why", why)
}

func (l *SidecarLoader) register(instance *sidecarInstance) {
	l.mu.Lock()
	previous := l.instances[instance.id]
	l.instances[instance.id] = instance
	l.mu.Unlock()
	if previous != nil && previous != instance {
		// A re-activation while an old instance is still winding down: take the
		// old one down so two processes never serve one plugin id.
		l.stop(previous, "replaced")
	}
}

// unregister drops an instance, but only when the map still holds *that*
// instance: a re-activated plugin replaces its record, and stopping the old
// process must not unregister the new one.
func (l *SidecarLoader) unregister(instance *sidecarInstance) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if current := l.instances[instance.id]; current == instance {
		delete(l.instances, instance.id)
	}
}

// reportDenials sends every refused tool to the audit sink (spec P3: 越权拒绝写审计).
func (l *SidecarLoader) reportDenials(manifest *pluginpkg.Manifest, refusals []sidecarRefusal) {
	l.mu.Lock()
	sink := l.denials
	l.mu.Unlock()
	for _, refusal := range refusals {
		l.log.Warn("plugin sidecar tool refused", "plugin", manifest.ID, "tool", refusal.name, "reason", refusal.reason)
		if sink == nil {
			continue
		}
		sink(Denial{
			PluginID: manifest.ID,
			Version:  manifest.Version,
			Kind:     PermissionTool,
			Target:   refusal.name,
			Reason:   refusal.reason,
			At:       l.now(),
		})
	}
}

// --- process plumbing -----------------------------------------------------

// resolveCommand turns the manifest's entry into an argv. A command that names
// a path (`./bin/sidecar`, `bin/sidecar`) resolves inside the package and must
// exist; a bare name (`node`, `python3`) is left to PATH. The package is never
// allowed to point at a binary outside itself by accident of the working
// directory: the child's cwd is the package directory either way.
func (l *SidecarLoader) resolveCommand(pkg pluginpkg.Package, manifest *pluginpkg.Manifest) (string, []string, error) {
	if len(manifest.Entry.Command) == 0 {
		return "", nil, NewPluginError("assemble", manifest.ID, ErrPluginKind,
			"kind sidecar requires entry.command")
	}
	argv := append(append([]string(nil), manifest.Entry.Command...), manifest.Entry.Args...)
	head := argv[0]
	if strings.TrimSpace(head) == "" {
		return "", nil, NewPluginError("assemble", manifest.ID, ErrPluginKind, "entry.command[0] is empty")
	}
	if !filepath.IsAbs(head) && strings.ContainsRune(head, '/') {
		if pkg.Dir == "" {
			return "", nil, NewPluginError("assemble", manifest.ID, ErrPluginKind,
				"entry.command %q is relative to the package, which has no directory", head)
		}
		head = filepath.Clean(filepath.Join(pkg.Dir, head))
	}
	if filepath.IsAbs(head) {
		info, err := os.Stat(head)
		if err != nil {
			return "", nil, NewPluginError("assemble", manifest.ID, ErrPluginKind,
				"entry.command %q: %v", head, err)
		}
		if info.IsDir() {
			return "", nil, NewPluginError("assemble", manifest.ID, ErrPluginKind,
				"entry.command %q is a directory", head)
		}
	}
	return head, argv[1:], nil
}

// environment is the sidecar's complete environment: the host's safe defaults
// plus exactly the variable *names* the manifest declared (values always come
// from the host, never from the manifest), plus the host's own identity
// variables. A host secret the manifest did not declare cannot reach the child
// even if it lives in os.Environ().
func (l *SidecarLoader) environment(pkg pluginpkg.Package, manifest *pluginpkg.Manifest) []string {
	base := l.opts.BaseEnv
	if base == nil {
		base = os.Environ()
	}
	allowed := append([]string(nil), gateway.DefaultEnvAllowlist...)
	allowed = append(allowed, manifest.Entry.Env...)
	env := gateway.FilterEnv(base, allowed...)
	env = append(env,
		"KEEL_PLUGIN_ID="+manifest.ID,
		"KEEL_PLUGIN_VERSION="+manifest.Version,
		"KEEL_PLUGIN_DIR="+pkg.Dir,
		"KEEL_PLUGIN_PROTOCOL="+gateway.ProtocolVersion,
		"KEEL_VERSION="+l.version(),
	)
	return env
}

// stderr routes the sidecar's stderr. Without an explicit writer the bytes go
// to the logger at debug level instead of the host's stderr, so a chatty plugin
// cannot scribble over the product's own output.
func (l *SidecarLoader) stderr(pluginID string) io.Writer {
	if l.opts.Stderr != nil {
		return l.opts.Stderr
	}
	return &logWriter{log: l.log, pluginID: pluginID}
}

// logWriter turns a child's stderr stream into log records.
type logWriter struct {
	log      *slog.Logger
	pluginID string
	mu       sync.Mutex
	line     []byte
}

func (w *logWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.line = append(w.line, data...)
	for {
		index := -1
		for i, b := range w.line {
			if b == '\n' {
				index = i
				break
			}
		}
		if index < 0 {
			break
		}
		line := strings.TrimRight(string(w.line[:index]), "\r")
		w.line = append(w.line[:0], w.line[index+1:]...)
		if line != "" {
			w.log.Debug("plugin sidecar stderr", "plugin", w.pluginID, "line", line)
		}
	}
	if len(w.line) > 8<<10 {
		w.line = w.line[len(w.line)-(8<<10):]
	}
	return len(data), nil
}

func (l *SidecarLoader) version() string {
	if l.opts.KeelVersion == "" {
		return "0.0.0"
	}
	return l.opts.KeelVersion
}

func (l *SidecarLoader) now() time.Time { return l.opts.Now() }
