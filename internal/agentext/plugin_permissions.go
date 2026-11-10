package agentext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/pluginpkg"
	"github.com/jixoai/keel/internal/store"
)

// Plugin permissions: enforcement (change keel-plugin-core, spec P1/P3).
//
// The manifest declares `permissions.{fs,net,secrets,tools}`; pluginpkg parses
// and shape-validates them. This file is the other half: the *decision* taken
// when a capability is actually requested on a plugin's behalf, and the audit
// row a refusal leaves behind.
//
// The enforcement model, stated once so it cannot drift:
//
//	tool dispatch  a capability call made on behalf of plugin P may name a tool
//	               only if P provides it (a plugin always owns its own tools —
//	               spec: 调用自身工具) or the manifest declares it in
//	               permissions.tools. The check runs AT THE DISPATCH POINT
//	               (Manager.InvokePluginTool, PermissionedBroker.CallTool), not at
//	               registration, so a call that names something new is refused
//	               even though the plugin is already loaded.
//	net            a host-mediated fetch may name a host only if
//	               permissions.net declares it (exact, `*.suffix`, optional
//	               :port; http/https only).
//	secrets        a host-mediated secret read may name only a declared secret.
//	fs             declared and published, but keel mediates no file access for
//	               a plugin yet, so there is no dispatch point to enforce it at
//	               (the honest boundary below).
//	sidecar env    enforced by the loader (plugin_sidecar.go): the child gets the
//	               host's safe defaults plus exactly the *names* the manifest
//	               declared, values always from the host.
//
// Honest boundary (spec: 诚实边界): an in-process plugin runs in the host
// process and a sidecar is a host-controlled process; neither is sandboxed.
// Enforced is every capability request that crosses the host's own boundary:
// tool dispatch, host-mediated fetch, secret reads, the child environment. A
// sidecar that opens its own socket is not stopped by `permissions.net`; the
// rule a plugin author follows is "ask the host through the broker", and the
// loader's env allowlist keeps host credentials out of the process regardless.
//
// Every refusal is a *PermissionDeniedError (errors.Is ErrPermissionDenied) and
// writes one audit row (action "deny", result "refused", detail
// {kind,target,reason}) — the same table and the same GET /v1/plugins/audit
// surface the lifecycle actions use (spec P3: 越权拒绝 MUST 记录时间、操作者、
// 插件 id、版本、动作、结果).

// Permission kinds, as they appear in an audit detail document.
const (
	// PermissionTool is a tool invocation.
	PermissionTool = "tool"
	// PermissionNet is a host-mediated network request.
	PermissionNet = "net"
	// PermissionSecret is a secret reference.
	PermissionSecret = "secret"
)

// PluginActionDeny is the audit action of a refusal. It lives here rather than
// in internal/store, whose vocabulary covers lifecycle actions only; the action
// column is free text and the detail carries the kind and the target.
const PluginActionDeny = "deny"

// ErrPermissionDenied is the sentinel of a refused capability request.
var ErrPermissionDenied = errors.New("agentext: plugin permission denied")

// PermissionDeniedError is one refusal: which plugin asked for what, and why it
// was not allowed.
type PermissionDeniedError struct {
	PluginID string `json:"pluginId"`
	// Kind is tool|net|secret.
	Kind string `json:"kind"`
	// Target is the refused tool, host or secret name.
	Target string `json:"target"`
	// Reason is the human-readable rule that refused it.
	Reason string `json:"reason"`
}

func (e *PermissionDeniedError) Error() string {
	return fmt.Sprintf("plugin %s: %s %q is not permitted: %s", e.PluginID, e.Kind, e.Target, e.Reason)
}

// Unwrap exposes the sentinel for errors.Is / errors.As.
func (e *PermissionDeniedError) Unwrap() error { return ErrPermissionDenied }

// Denial is one refusal as the audit trail receives it.
type Denial struct {
	PluginID string
	Version  string
	Kind     string
	Target   string
	Reason   string
	At       time.Time
}

// PermissionPolicy is one plugin's compiled permissions. It is immutable after
// construction and safe for concurrent use.
type PermissionPolicy struct {
	pluginID string
	// version is the plugin version the policy was compiled from; a denial
	// carries it into the audit row.
	version string
	// tools is permissions.tools: what the plugin may call.
	tools map[string]bool
	// own is the plugin's own surface: provides.tools plus the host-qualified
	// form the assembly publishes for a sidecar.
	own map[string]bool
	// net is the declared host patterns.
	net []string
	// secrets is permissions.secrets.
	secrets map[string]bool
}

// NewPermissionPolicy compiles a manifest's permissions. A nil manifest (a row
// whose package never parsed) compiles to a policy that allows nothing, which
// is the fail-closed default.
func NewPermissionPolicy(manifest *pluginpkg.Manifest) *PermissionPolicy {
	policy := &PermissionPolicy{
		tools:   make(map[string]bool),
		own:     make(map[string]bool),
		secrets: make(map[string]bool),
	}
	if manifest == nil {
		return policy
	}
	policy.pluginID = manifest.ID
	policy.version = manifest.Version
	for _, name := range manifest.Permissions.Tools {
		policy.tools[name] = true
		// The dispatch point checks the PUBLISHED name of the resolved tool, so a
		// permission declared in the manifest's spelling (`keel.other.other_tool`)
		// must also match its wire form (`keel_other_other_tool`). Both name the
		// same declared tool.
		policy.tools[pluginpkg.NormalizePublishedToolName(name)] = true
	}
	for _, tool := range manifest.PublishedTools() {
		// A plugin's own surface is every spelling of its tools that can arrive
		// at a dispatch point: the declared name, the local name a sidecar speaks,
		// the host-qualified `<id>.<local>`, and the normalized wire name the model
		// and the bridge actually call (`demo_sidecar_greet`). Without the last one
		// a plugin could not call its own tool by its published name.
		policy.own[tool.Declared] = true
		policy.own[tool.Published] = true
		if manifest.NamespacesTools() {
			policy.own[manifest.ID+"."+tool.Local] = true
			policy.own[manifest.ID+"."+tool.Declared] = true
		}
	}
	policy.net = append([]string(nil), manifest.Permissions.Net...)
	for _, name := range manifest.Permissions.Secrets {
		policy.secrets[name] = true
	}
	return policy
}

// emptyPolicy compiles a policy that allows nothing for a plugin id that has no
// manifest (or no row): the fail-closed default, still identified so the
// refusal it produces lands in the audit trail under the right plugin.
func emptyPolicy(pluginID string) *PermissionPolicy {
	return &PermissionPolicy{
		pluginID: pluginID,
		tools:    map[string]bool{},
		own:      map[string]bool{},
		secrets:  map[string]bool{},
	}
}

// PluginID is the policy's plugin.
func (p *PermissionPolicy) PluginID() string { return p.pluginID }

// Version is the plugin version the policy was compiled from.
func (p *PermissionPolicy) Version() string { return p.version }

// DeclaredTools is permissions.tools, sorted (diagnostics and tests).
func (p *PermissionPolicy) DeclaredTools() []string { return sortedKeys(p.tools) }

// DeclaredHosts is permissions.net, sorted.
func (p *PermissionPolicy) DeclaredHosts() []string {
	out := append([]string(nil), p.net...)
	sort.Strings(out)
	return out
}

// DeclaredSecrets is permissions.secrets, sorted.
func (p *PermissionPolicy) DeclaredSecrets() []string { return sortedKeys(p.secrets) }

// AllowTool decides a tool invocation: the manifest's permissions.tools, or the
// plugin's own surface. The returned string is the reason of a refusal.
func (p *PermissionPolicy) AllowTool(name string) (string, bool) {
	if name == "" {
		return "the tool name is empty", false
	}
	if p.tools[name] || p.own[name] {
		return "", true
	}
	// A bare declaration of our own tool also permits the qualified call.
	if strings.HasPrefix(name, p.pluginID+".") && p.own[strings.TrimPrefix(name, p.pluginID+".")] {
		return "", true
	}
	return fmt.Sprintf("tool %q is neither declared in permissions.tools nor provided by this plugin", name), false
}

// AllowHost decides a host-mediated request for a host (`host` or `host:port`).
func (p *PermissionPolicy) AllowHost(host string) (string, bool) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return "the host is empty", false
	}
	if len(p.net) == 0 {
		return "the manifest declares no permissions.net hosts", false
	}
	for _, pattern := range p.net {
		if matchHost(pattern, host) {
			return "", true
		}
	}
	return fmt.Sprintf("host %q is not declared in permissions.net", host), false
}

// AllowURL decides a host-mediated fetch: the URL must be http(s) and its host
// (with its port, when it carries one) must be declared.
func (p *PermissionPolicy) AllowURL(rawURL string) (string, bool) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return "the URL is empty", false
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Sprintf("the URL %q cannot be parsed: %v", rawURL, err), false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Sprintf("scheme %q is not allowed (only http and https)", parsed.Scheme), false
	}
	if parsed.Hostname() == "" {
		return fmt.Sprintf("the URL %q has no host", rawURL), false
	}
	return p.AllowHost(strings.ToLower(parsed.Host))
}

// AllowSecret decides a secret reference.
func (p *PermissionPolicy) AllowSecret(name string) (string, bool) {
	if name == "" {
		return "the secret name is empty", false
	}
	if p.secrets[name] {
		return "", true
	}
	return fmt.Sprintf("secret %q is not declared in permissions.secrets", name), false
}

// matchHost implements the declared-host grammar: an exact host, `*` for any
// host, or a `*` glob inside the host (`*.example.com` matches one or more
// leading labels but never the bare suffix; `api-*.example.com` matches what it
// reads). A pattern without a port matches any port; a pattern with a port
// matches only that port, and never matches a request whose port is unknown.
func matchHost(pattern, host string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	patternHost, patternPort, patternHasPort := splitHostPort(pattern)
	hostName, hostPort, hostHasPort := splitHostPort(host)
	if patternHasPort && (!hostHasPort || patternPort != hostPort) {
		return false
	}
	return globHost(patternHost, hostName)
}

// globHost matches a host against a pattern where `*` stands for any run of
// characters, anchored at both ends. A pattern that starts with "*." therefore
// requires at least one extra label before the suffix.
func globHost(pattern, host string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == host
	}
	// Standard backtracking glob (patterns are short and never adversarial).
	patternIndex, hostIndex := 0, 0
	starIndex, starHostIndex := -1, 0
	for hostIndex < len(host) {
		switch {
		case patternIndex < len(pattern) && pattern[patternIndex] == host[hostIndex]:
			patternIndex++
			hostIndex++
		case patternIndex < len(pattern) && pattern[patternIndex] == '*':
			starIndex = patternIndex
			starHostIndex = hostIndex
			patternIndex++
		case starIndex >= 0:
			starHostIndex++
			hostIndex = starHostIndex
			patternIndex = starIndex + 1
		default:
			return false
		}
	}
	for patternIndex < len(pattern) && pattern[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(pattern)
}

// splitHostPort splits "host", "host:port" and "[v6]:port" without net.SplitHostPort's
// insistence on a port being present.
func splitHostPort(value string) (host, port string, hasPort bool) {
	if strings.HasPrefix(value, "[") {
		if end := strings.Index(value, "]"); end >= 0 {
			host = value[1:end]
			rest := value[end+1:]
			if strings.HasPrefix(rest, ":") {
				return host, rest[1:], true
			}
			return host, "", false
		}
	}
	if strings.Count(value, ":") == 1 {
		if index := strings.Index(value, ":"); index >= 0 {
			candidate := value[index+1:]
			if candidate == "" {
				return value[:index], "", false
			}
			return value[:index], candidate, true
		}
	}
	return value, "", false
}

func sortedKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// --- the gate -------------------------------------------------------------

// PermissionGate enforces one plugin's policy at a dispatch point and audits
// every refusal. A nil gate — or a gate without a policy — allows nothing:
// callers get a definite denial instead of an accidental allow.
type PermissionGate struct {
	policy *PermissionPolicy
	sink   func(Denial)
	now    func() time.Time
}

// NewPermissionGate builds a gate over a policy. The sink receives every
// refusal (the manager wires it to the audit trail); a nil sink still refuses,
// it just cannot record (tests and hosts without persistence).
func NewPermissionGate(policy *PermissionPolicy, sink func(Denial), now func() time.Time) *PermissionGate {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &PermissionGate{policy: policy, sink: sink, now: now}
}

// Policy is the gate's compiled policy (nil when the gate has none).
func (g *PermissionGate) Policy() *PermissionPolicy {
	if g == nil {
		return nil
	}
	return g.policy
}

// PluginID is the plugin the gate speaks for (empty without a policy).
func (g *PermissionGate) PluginID() string {
	if g == nil || g.policy == nil {
		return ""
	}
	return g.policy.PluginID()
}

// CheckTool decides a tool invocation and audits a refusal.
func (g *PermissionGate) CheckTool(_ context.Context, name string) error {
	if g == nil || g.policy == nil {
		return gateDeny(g, PermissionTool, name, "the plugin has no permission policy")
	}
	if reason, allowed := g.policy.AllowTool(name); !allowed {
		return gateDeny(g, PermissionTool, name, reason)
	}
	return nil
}

// CheckHost decides a host-mediated request and audits a refusal.
func (g *PermissionGate) CheckHost(_ context.Context, host string) error {
	if g == nil || g.policy == nil {
		return gateDeny(g, PermissionNet, host, "the plugin has no permission policy")
	}
	if reason, allowed := g.policy.AllowHost(host); !allowed {
		return gateDeny(g, PermissionNet, host, reason)
	}
	return nil
}

// CheckURL decides a host-mediated fetch and audits a refusal.
func (g *PermissionGate) CheckURL(_ context.Context, rawURL string) error {
	if g == nil || g.policy == nil {
		return gateDeny(g, PermissionNet, rawURL, "the plugin has no permission policy")
	}
	if reason, allowed := g.policy.AllowURL(rawURL); !allowed {
		return gateDeny(g, PermissionNet, rawURL, reason)
	}
	return nil
}

// CheckSecret decides a secret reference and audits a refusal.
func (g *PermissionGate) CheckSecret(_ context.Context, name string) error {
	if g == nil || g.policy == nil {
		return gateDeny(g, PermissionSecret, name, "the plugin has no permission policy")
	}
	if reason, allowed := g.policy.AllowSecret(name); !allowed {
		return gateDeny(g, PermissionSecret, name, reason)
	}
	return nil
}

// Guard runs fn only after the check passes: the shape every mediated
// capability uses, so a request can never be checked-but-still-executed.
func (g *PermissionGate) Guard(_ context.Context, kind, target string, fn func() error) error {
	switch kind {
	case PermissionTool:
		if err := g.CheckTool(context.Background(), target); err != nil {
			return err
		}
	case PermissionNet:
		if err := g.CheckURL(context.Background(), target); err != nil {
			return err
		}
	case PermissionSecret:
		if err := g.CheckSecret(context.Background(), target); err != nil {
			return err
		}
	default:
		return gateDeny(g, kind, target, fmt.Sprintf("unknown permission kind %q", kind))
	}
	return fn()
}

// gateDeny builds the refusal and records it. It tolerates a nil gate so the
// "no policy" path is a denial rather than a panic.
func gateDeny(g *PermissionGate, kind, target, reason string) error {
	var pluginID, version string
	if g != nil && g.policy != nil {
		pluginID = g.policy.PluginID()
		version = g.policy.Version()
	}
	at := time.Now().UTC()
	if g != nil && g.now != nil {
		at = g.now()
	}
	if g != nil && g.sink != nil {
		g.sink(Denial{PluginID: pluginID, Version: version, Kind: kind, Target: target, Reason: reason, At: at})
	}
	return &PermissionDeniedError{PluginID: pluginID, Kind: kind, Target: target, Reason: reason}
}

// GateFor returns the gate of one plugin id (nil when the plugin is unknown).
func (m *Manager) GateFor(pluginID string) *PermissionGate {
	m.mu.Lock()
	entry := m.entries[pluginID]
	m.mu.Unlock()
	if entry == nil {
		return nil
	}
	return m.gateFor(entry)
}

// gateFor builds the gate of an entry. The gate writes through the manager's
// audit path, so a refusal appears in GET /v1/plugins/audit with the plugin id,
// the kind, the target and the result — the P3 contract.
func (m *Manager) gateFor(entry *managerEntry) *PermissionGate {
	return NewPermissionGate(NewPermissionPolicy(entryManifest(entry)), m.recordDenial, m.now)
}

func entryManifest(entry *managerEntry) *pluginpkg.Manifest {
	if entry == nil {
		return nil
	}
	return entry.manifest
}

// recordDenial writes one refusal to the audit trail. It runs on dispatch paths
// (including the sidecar loader's supervision goroutines AND the loader's own
// Load, which the manager calls while it holds its lock), so it must never take
// the plugin manager's lock: the denial carries the plugin id and version it
// needs. It never fails the caller either — the refusal is already the outcome,
// and a store failure must be loud rather than silent.
func (m *Manager) recordDenial(denial Denial) {
	if m.store == nil {
		return
	}
	at := denial.At
	if at.IsZero() {
		at = m.now()
	}
	detail := mustJSON(map[string]any{"kind": denial.Kind, "target": denial.Target, "reason": denial.Reason})
	entry := store.PluginAuditEntry{
		TS:       at,
		Actor:    "plugin-gate",
		PluginID: denial.PluginID,
		Version:  denial.Version,
		Action:   PluginActionDeny,
		Result:   store.PluginResultRefused,
		Detail:   detail,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.store.AppendPluginAudit(ctx, entry); err != nil {
		m.log.Error("plugin permission denial audit write failed", "plugin", denial.PluginID,
			"kind", denial.Kind, "target", denial.Target, "err", err)
	}
}

// recordRuntimeFailure writes the crash of a supervised runtime into the audit
// trail. It is called from the loader's supervision goroutine, so it never takes
// the plugin manager's lock; the row mirrors the shape an activation failure
// uses (action enable, result failed, the reason in the detail).
func (m *Manager) recordRuntimeFailure(pluginID, version, reason string) {
	if m.store == nil {
		return
	}
	if version == "" {
		m.mu.Lock()
		if entry := m.entries[pluginID]; entry != nil {
			version = entry.manifestVersion()
		}
		m.mu.Unlock()
	}
	entry := store.PluginAuditEntry{
		TS:       m.now(),
		Actor:    "plugin-runtime",
		PluginID: pluginID,
		Version:  version,
		Action:   store.PluginActionEnable,
		Result:   store.PluginResultFailed,
		Detail:   mustJSON(map[string]any{"runtime": "failed", "reason": reason}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.store.AppendPluginAudit(ctx, entry); err != nil {
		m.log.Error("plugin runtime failure audit write failed", "plugin", pluginID, "err", err)
	}
}

// --- the mediated capability seam -----------------------------------------

// FetchRequest is one host-mediated HTTP request a plugin may ask for. The
// broker decides the transport details; the gate decides the host.
type FetchRequest struct {
	Method string
	URL    string
	Body   []byte
}

// FetchResponse is the host's answer to a mediated fetch.
type FetchResponse struct {
	Status int
	Body   []byte
}

// CapabilityBroker is the host-supplied capability surface a plugin reaches
// through agentext: tools, host-mediated fetches and secret reads. The host
// implements it (its tool registry, its HTTP client, its secret store); the gate
// in front of it decides, so a broker never duplicates permission rules.
type CapabilityBroker interface {
	// CallTool runs one host tool with the plugin's arguments.
	CallTool(ctx context.Context, pluginID, tool string, args json.RawMessage) (agentcore.AgentToolResult, error)
	// Fetch performs one host-mediated HTTP request.
	Fetch(ctx context.Context, pluginID string, request FetchRequest) (FetchResponse, error)
	// ReadSecret resolves one secret reference into its value.
	ReadSecret(ctx context.Context, pluginID, name string) (string, error)
}

// PermissionedBroker is the enforced dispatcher: every request arrives with the
// requesting plugin's id, is checked against that plugin's manifest permissions
// and audited when refused, and only then reaches the underlying broker. It is
// the single place a host surface (the plugin bridge, a creation flow, a future
// host-call verb) should reach capabilities through.
type PermissionedBroker struct {
	manager *Manager
	broker  CapabilityBroker

	mu    sync.Mutex
	gates map[string]*PermissionGate
}

// NewPermissionedBroker wraps a broker with the manager's policies.
func NewPermissionedBroker(manager *Manager, broker CapabilityBroker) *PermissionedBroker {
	return &PermissionedBroker{manager: manager, broker: broker, gates: make(map[string]*PermissionGate)}
}

// Broker is the wrapped delegate (diagnostics and tests).
func (b *PermissionedBroker) Broker() CapabilityBroker { return b.broker }

// Gate returns the gate of a plugin, one per plugin id (a policy is immutable
// for a version; an update replaces it on the next lookup).
func (b *PermissionedBroker) Gate(pluginID string) *PermissionGate {
	b.mu.Lock()
	defer b.mu.Unlock()
	if gate, ok := b.gates[pluginID]; ok {
		return gate
	}
	// An unknown plugin compiles to a policy that allows nothing; with a manager
	// behind it the refusal still reaches the audit trail, under the requested
	// plugin id.
	if b.manager != nil {
		if gate := b.manager.GateFor(pluginID); gate != nil {
			b.gates[pluginID] = gate
			return gate
		}
		gate := NewPermissionGate(emptyPolicy(pluginID), b.manager.recordDenial, b.manager.now)
		b.gates[pluginID] = gate
		return gate
	}
	gate := NewPermissionGate(NewPermissionPolicy(nil), nil, nil)
	b.gates[pluginID] = gate
	return gate
}

// CallTool enforces the tool whitelist at the dispatch point (spec P1) and
// delegates.
func (b *PermissionedBroker) CallTool(ctx context.Context, pluginID, tool string, args json.RawMessage) (agentcore.AgentToolResult, error) {
	if err := b.Gate(pluginID).CheckTool(ctx, tool); err != nil {
		return agentcore.AgentToolResult{}, err
	}
	if b.broker == nil {
		return agentcore.AgentToolResult{}, fmt.Errorf("plugin %s: no capability broker is wired in this deployment", pluginID)
	}
	return b.broker.CallTool(ctx, pluginID, tool, args)
}

// Fetch enforces the net host whitelist and delegates.
func (b *PermissionedBroker) Fetch(ctx context.Context, pluginID string, request FetchRequest) (FetchResponse, error) {
	if err := b.Gate(pluginID).CheckURL(ctx, request.URL); err != nil {
		return FetchResponse{}, err
	}
	if b.broker == nil {
		return FetchResponse{}, fmt.Errorf("plugin %s: no capability broker is wired in this deployment", pluginID)
	}
	return b.broker.Fetch(ctx, pluginID, request)
}

// ReadSecret enforces the secret whitelist and delegates.
func (b *PermissionedBroker) ReadSecret(ctx context.Context, pluginID, name string) (string, error) {
	if err := b.Gate(pluginID).CheckSecret(ctx, name); err != nil {
		return "", err
	}
	if b.broker == nil {
		return "", fmt.Errorf("plugin %s: no capability broker is wired in this deployment", pluginID)
	}
	return b.broker.ReadSecret(ctx, pluginID, name)
}

// --- the tool dispatch point ----------------------------------------------

// activeTool is one tool of the live assembly, with its owner.
type activeTool struct {
	// name is the model-facing (published) name.
	name string
	// owner is the plugin that contributed it.
	owner string
	// tool is the runtime tool itself.
	tool agentcore.AgentTool
}

// activeTools lists the live assembly's tools. It is the read side the dispatch
// point resolves against: a plugin may only call what the assembly currently
// publishes.
func (r *Runtime) activeTools() []activeTool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]activeTool, 0, 8)
	for _, entry := range r.active {
		for _, tool := range entry.tools {
			out = append(out, activeTool{name: tool.Name(), owner: entry.plugin.ID, tool: tool})
		}
	}
	return out
}

// errToolNotActive reports that no live tool matches the request.
var errToolNotActive = errors.New("the tool is not part of the current assembly")

// resolveActiveTool finds a tool in the runtime's current assembly: the exact
// published name, the plugin-qualified form of a bare request, or the normalized
// wire form the model and the bridge call (`demo_sidecar_greet` for a sidecar
// tool `greet` of plugin `demo.sidecar`).
func (m *Manager) resolveActiveTool(pluginID, tool string) (activeTool, error) {
	// The request may name the tool in any spelling that can reach a caller: the
	// published wire name, the declared `<id>.<local>` form of this plugin or of
	// ANOTHER plugin (`permissions.tools` lets a plugin call a declared foreign
	// tool by its declaration), the bare local name, or an already-normalized
	// form of any of them. Trimming the plugin prefix first keeps a local name
	// from being qualified twice.
	local := strings.TrimPrefix(tool, pluginID+".")
	normalizedLocal := pluginpkg.NamespacedPublishedToolName(pluginID, local)
	normalizedDeclared := pluginpkg.NormalizePublishedToolName(tool)
	for _, candidate := range m.runtime.activeTools() {
		if candidate.name == tool ||
			candidate.name == pluginID+"."+tool ||
			candidate.name == normalizedLocal ||
			candidate.name == normalizedDeclared {
			return candidate, nil
		}
	}
	return activeTool{}, fmt.Errorf("%w: tool %q", errToolNotActive, tool)
}

// InvokePluginTool runs a tool from the current assembly on behalf of a plugin.
// It is the dispatch point the spec's tool whitelist is enforced at (P1):
//
//  1. the plugin must exist and be enabled (a disabled plugin has no surface);
//  2. the tool must resolve inside the live assembly — the plugin's own tool,
//     or one the manifest declares in permissions.tools;
//  3. only then is the tool's Execute called, with the caller's context.
//
// A refusal is a *PermissionDeniedError and leaves an audit row. A name nothing
// provides is not a permission decision and is reported as an unknown tool.
func (m *Manager) InvokePluginTool(ctx context.Context, pluginID, tool string, args json.RawMessage) (json.RawMessage, error) {
	if strings.TrimSpace(pluginID) == "" || strings.TrimSpace(tool) == "" {
		return nil, errors.New("agentext: InvokePluginTool requires a plugin id and a tool name")
	}
	m.mu.Lock()
	entry := m.entries[pluginID]
	switch {
	case entry == nil:
		m.mu.Unlock()
		return nil, pluginErrf(ErrPluginNotFound, "call", pluginID, "plugin is not installed")
	case entry.state != LifecycleEnabled:
		state := entry.state
		m.mu.Unlock()
		return nil, pluginErrf(ErrPluginState, "call", pluginID, "plugin is %s", state)
	}
	gate := m.gateFor(entry)
	m.mu.Unlock()

	execution, err := m.resolveActiveTool(pluginID, tool)
	if err != nil {
		return nil, pluginErrf(ErrPluginState, "call", pluginID, "%v", err)
	}
	// The whitelist is checked here, at the dispatch point: the plugin's own
	// tools are its own surface (always allowed); anything owned by someone else
	// must be declared in permissions.tools.
	if err := gate.CheckTool(ctx, execution.name); err != nil {
		return nil, err
	}
	result, err := execution.tool.Execute(ctx, "plugin-call", args, nil)
	if err != nil {
		return nil, fmt.Errorf("plugin %s tool %s: %w", pluginID, execution.name, err)
	}
	raw, err := json.Marshal(toolResultDocument(result))
	if err != nil {
		return nil, fmt.Errorf("agentext: encode tool result: %w", err)
	}
	return raw, nil
}

// toolResultDocument renders a tool result as the JSON document a bridge hands
// back to a plugin UI.
func toolResultDocument(result agentcore.AgentToolResult) map[string]any {
	content := make([]map[string]any, 0, len(result.Content))
	for _, block := range result.Content {
		switch typed := block.(type) {
		case agentcore.TextContent:
			content = append(content, map[string]any{"type": "text", "text": typed.Text})
		case agentcore.ImageContent:
			content = append(content, map[string]any{"type": "image", "data": typed.Data, "mimeType": typed.MimeType})
		default:
			raw, err := json.Marshal(block)
			if err != nil {
				continue
			}
			content = append(content, map[string]any{"type": "json", "value": json.RawMessage(raw)})
		}
	}
	document := map[string]any{"content": content}
	if result.StructuredContent != nil {
		document["structuredContent"] = result.StructuredContent
	}
	if result.IsError != nil {
		document["isError"] = *result.IsError
	}
	return document
}
