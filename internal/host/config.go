package host

// Tenant configuration is the v1 document described in docs/HOST.md §2. This
// file is a pure data contract: it parses, merges, and validates configuration.
// It deliberately does not connect MCP servers, discover skills, read secrets,
// or reach a provider — assembly stays in the host/runner wiring so a bad
// config can be rejected before any side effect happens.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/provider"
)

// identifierPattern is the grammar for names that become prompt structure
// (section tags) or tool namespaces (MCP server names). Restricting it to
// lowercase identifiers prevents markup injection into the system prompt.
var identifierPattern = regexp.MustCompile(`^[a-z0-9_-]+$`)

// secretRefPattern accepts only indirections, never literal credentials:
// ${NAME}, env:NAME, or secret:NAME.
var secretRefPattern = regexp.MustCompile(`^(?:\$\{[A-Za-z_][A-Za-z0-9_]*\}|(?:env|secret):[A-Za-z_][A-Za-z0-9_]*)$`)

// toolPattern accepts registry names and trailing-* wildcards (mcp__*).
var toolPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+\*?$`)

// TenantConfig is one tenant's agent configuration. Every field is optional:
// a zero value inherits from the defaults passed to Merge.
type TenantConfig struct {
	Agent AgentConfig `json:"agent"`
	// Models carries the provider-configuration slice of the tenant document
	// (providers / modelRules / modelApiRules / siteRules / providerModelRules /
	// selection). It is passed verbatim to the model configuration resolver, so
	// the host vocabulary stays one document.
	Models     json.RawMessage  `json:"models,omitempty"`
	Skills     SkillsConfig     `json:"skills"`
	MCP        MCPConfig        `json:"mcp"`
	Hooks      HooksConfig      `json:"hooks"`
	Compaction CompactionConfig `json:"compaction"`
	Telemetry  TelemetryConfig  `json:"telemetry"`
	Tools      ToolsConfig      `json:"tools"`
	Quota      QuotaConfig      `json:"quota"`
	// External declares the process-external providers of the tenant's
	// loadout (agent backends and tool providers). It is the single
	// configuration vocabulary for the external gateway: the loadout decides
	// which services exist from this document and nothing else.
	External ExternalConfig `json:"external"`
}

// AgentConfig selects the model and shapes the base system prompt. APIKeyRef
// names a secret; the key itself never appears in this document.
type AgentConfig struct {
	Name         string `json:"name,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Model        string `json:"model,omitempty"`
	BaseURL      string `json:"baseUrl,omitempty"`
	Protocol     string `json:"protocol,omitempty"`
	APIKeyRef    string `json:"apiKeyRef,omitempty"`
	SystemPrompt string `json:"systemPrompt,omitempty"`
	// Sections has no omitempty: a nil slice means "inherit the default
	// sections" while an explicit [] means "clear them". Both must survive the
	// canonical JSON stored by the admin API.
	Sections      []PromptSection `json:"sections"`
	Thinking      string          `json:"thinking,omitempty"`
	MaxTurns      int             `json:"maxTurns,omitempty"`
	ContextWindow int             `json:"contextWindow,omitempty"`
}

// PromptSection is one named system-prompt section, rendered as
// <name>content</name> after the preamble.
type PromptSection struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// SkillsConfig points the host at skill roots and selects the PI file-read
// wording used in the skills index. Enabled is a pointer so an explicit false
// can disable discovery when a default enables it.
type SkillsConfig struct {
	Enabled      *bool    `json:"enabled,omitempty"`
	Roots        []string `json:"roots"`
	FileReadTool string   `json:"fileReadTool,omitempty"`
}

// MCPConfig lists MCP servers by name. Servers are validated here but connected
// by the host assembly, not by this package.
type MCPConfig struct {
	Servers map[string]MCPServerConfig `json:"servers,omitempty"`
}

// MCPServerConfig accepts the shared mcpServers shape: stdio (command/args/env/
// cwd) or Streamable HTTP (url/headers), plus the common timeout/enabled flags.
type MCPServerConfig struct {
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	CWD         string            `json:"cwd,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Timeout     int               `json:"timeout,omitempty"` // seconds
	Enabled     *bool             `json:"enabled,omitempty"`
	Description string            `json:"description,omitempty"`
}

// HooksConfig lists command hooks keyed by PI extension event name. Commands
// has no omitempty so an explicit [] can clear defaults.
type HooksConfig struct {
	Commands []HookCommand `json:"commands"`
}

// HookCommand runs Command when Event fires on the loop. TimeoutMS is a
// per-command wall clock bound; 0 means the hooks package default.
type HookCommand struct {
	Event     string   `json:"event"`
	Command   []string `json:"command"`
	TimeoutMS int      `json:"timeoutMs,omitempty"`
}

// CompactionConfig tunes context compaction. Enabled is a pointer so an
// explicit false can override a default of true; nil inherits. Threshold is a
// fraction of the context window in (0, 1]; 0 means unset.
type CompactionConfig struct {
	Enabled          *bool   `json:"enabled,omitempty"`
	Threshold        float64 `json:"threshold,omitempty"`
	KeepRecentTokens int     `json:"keepRecentTokens,omitempty"`
}

// TelemetryConfig selects the telemetry adapter ("memory", "prometheus", or
// "otlp"; empty inherits the default).
type TelemetryConfig struct {
	Adapter string `json:"adapter,omitempty"`
}

// ToolsConfig filters the assembled tool registry. Allow holds tool names and
// trailing-* wildcards (for example "clock" or "mcp__*"); nil inherits the
// default allow-list and [] explicitly clears it.
type ToolsConfig struct {
	Allow []string `json:"allow"`
}

// QuotaConfig bounds one tenant's kernel-enforced runtime consumption. Zero
// means unlimited.
//
// This is the single quota vocabulary: the streaming edge keeps enforcing the
// tenant row's quota_streams / quota_bytes_per_sec columns, while the contract
// kernel consults these limits on the claim and commit paths (per-tenant
// concurrent runs, admitted events per minute). Administrators write this
// document through PUT /v1/tenants/{id}/config like every other tenant setting;
// no second quota schema exists.
type QuotaConfig struct {
	// RunConcurrency caps concurrently running runs for the tenant. 0 = no
	// limit beyond the process-wide worker concurrency.
	RunConcurrency int `json:"runConcurrency,omitempty"`
	// EventsPerMinute caps admitted events per tenant per minute. The limit is
	// a token bucket shared by every session of the tenant; 0 = unlimited.
	EventsPerMinute int `json:"eventsPerMinute,omitempty"`
}

// External provider transports.
const (
	// ExternalTransportStdio spawns a sidecar process and speaks the protocol
	// over its stdin/stdout.
	ExternalTransportStdio = "stdio"
	// ExternalTransportHTTP talks Streamable HTTP to a provider endpoint.
	ExternalTransportHTTP = "http"
)

// External unmetered policies (external.quota.unmetered). The policy must be
// chosen explicitly when the document declares backends: a backend whose usage
// Keel cannot see is never silently admitted, and its usage is never recorded
// as zero (external-gateway spec: 外部用量计量边界).
const (
	ExternalUnmeteredAllow = "allow"
	ExternalUnmeteredDeny  = "deny"
)

// ExternalConfig declares the process-external providers a tenant's loadout
// assembles: agent backends (whole external runtimes such as DSH, which own
// their session state) and tool providers (stateless JSON tools). This is the
// one configuration vocabulary for the external gateway; the loadout maps it to
// services and the runtime only speaks these shapes.
type ExternalConfig struct {
	// Backends are AgentBackend providers keyed by provider name.
	Backends map[string]ExternalProviderConfig `json:"backends,omitempty"`
	// Tools are ToolProvider providers keyed by provider name.
	Tools map[string]ExternalProviderConfig `json:"tools,omitempty"`
	// Quota is the explicit external-session policy.
	Quota ExternalQuotaConfig `json:"quota"`
}

// ExternalProviderConfig is one external provider endpoint.
//
// Transport selects stdio ("command" plus optional args/dir) or http ("url"
// plus optional headers); when it is empty the endpoint shape decides. Env and
// Headers values are secret references only (${NAME}, env:NAME, secret:NAME):
// a literal credential in the tenant document is rejected, exactly like MCP
// server configuration. Nothing from the host environment reaches a spawned
// provider except the gateway's minimal allowlist plus the variables listed
// here.
type ExternalProviderConfig struct {
	Transport string            `json:"transport,omitempty"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Dir       string            `json:"dir,omitempty"`
	URL       string            `json:"url,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	// TimeoutMS bounds one request to the provider (0 = gateway default).
	TimeoutMS int `json:"timeoutMs,omitempty"`
	// Enabled is a pointer so an explicit false disables a default provider;
	// nil means enabled when the provider is declared.
	Enabled *bool `json:"enabled,omitempty"`
	// MaxSessions caps concurrently open sessions of this backend. Agent
	// backends only; tool providers ignore it.
	MaxSessions int `json:"maxSessions,omitempty"`
}

// ExternalQuotaConfig is the explicit policy for external sessions, which have
// no native lease and whose model usage may be invisible to Keel.
type ExternalQuotaConfig struct {
	// Unmetered decides what happens when a backend reports metering=none:
	// "allow" admits the session and records metering=none; "deny" refuses to
	// open it. Empty is rejected when backends are declared, so the policy is
	// always an explicit choice.
	Unmetered string `json:"unmetered,omitempty"`
	// MaxSessionsPerBackend caps concurrently open sessions of every backend.
	// 0 = unlimited; a backend's own maxSessions is an additional, tighter cap.
	MaxSessionsPerBackend int `json:"maxSessionsPerBackend,omitempty"`
}

// ProviderEnabled reports whether the provider is turned on: declared and not
// explicitly disabled.
func (p ExternalProviderConfig) ProviderEnabled() bool {
	return p.Enabled == nil || *p.Enabled
}

// TransportKind resolves the effective transport ("stdio" or "http") and
// validates that the endpoint fields match it.
func (p ExternalProviderConfig) TransportKind() (string, error) {
	command := strings.TrimSpace(p.Command)
	rawURL := strings.TrimSpace(p.URL)
	transport := strings.ToLower(strings.TrimSpace(p.Transport))
	if transport == "" {
		switch {
		case command != "" && rawURL == "":
			transport = ExternalTransportStdio
		case rawURL != "" && command == "":
			transport = ExternalTransportHTTP
		case command == "" && rawURL == "":
			return "", fmt.Errorf(`either "command" (stdio) or "url" (http) is required`)
		default:
			return "", fmt.Errorf(`"command" and "url" are mutually exclusive`)
		}
	}
	switch transport {
	case ExternalTransportStdio:
		if command == "" {
			return "", fmt.Errorf(`transport "stdio" requires "command"`)
		}
		if rawURL != "" {
			return "", fmt.Errorf(`transport "stdio" must not carry "url"`)
		}
	case ExternalTransportHTTP:
		if rawURL == "" {
			return "", fmt.Errorf(`transport "http" requires "url"`)
		}
		if command != "" {
			return "", fmt.Errorf(`transport "http" must not carry "command"`)
		}
	default:
		return "", fmt.Errorf(`transport %q (want stdio|http)`, p.Transport)
	}
	return transport, nil
}

// validate checks one external provider endpoint.
func (p ExternalProviderConfig) validate() error {
	transport, err := p.TransportKind()
	if err != nil {
		return err
	}
	if transport == ExternalTransportHTTP {
		parsed, err := url.Parse(strings.TrimSpace(p.URL))
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("url must be an http or https URL")
		}
		if parsed.User != nil {
			return fmt.Errorf("url must not embed credentials; use a header secret reference")
		}
	}
	if p.TimeoutMS < 0 {
		return fmt.Errorf("timeoutMs must be >= 0")
	}
	if p.MaxSessions < 0 {
		return fmt.Errorf("maxSessions must be >= 0")
	}
	for i, arg := range p.Args {
		if strings.TrimSpace(arg) == "" {
			return fmt.Errorf("args[%d] is empty", i)
		}
	}
	for name, value := range p.Env {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("env contains an empty variable name")
		}
		if err := validateSecretRef(value); err != nil {
			return fmt.Errorf("env[%q]: %v", name, err)
		}
	}
	for name, value := range p.Headers {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("headers contains an empty header name")
		}
		if err := validateSecretRef(value); err != nil {
			return fmt.Errorf("headers[%q]: %v", name, err)
		}
	}
	return nil
}

// validate rejects external configuration that cannot be assembled. Agent
// backends additionally require the explicit unmetered policy: whether an
// invisible-usage session may be opened is a tenant decision, not a default.
func (c ExternalConfig) validate() error {
	problem := func(format string, args ...any) error {
		return fmt.Errorf("external: "+format, args...)
	}
	for name, provider := range c.Backends {
		if strings.TrimSpace(name) == "" {
			return problem("backends contains an empty provider name")
		}
		if !identifierPattern.MatchString(name) {
			return problem(`backends name %q (want lowercase [a-z0-9_-]+)`, name)
		}
		if err := provider.validate(); err != nil {
			return problem("backends[%q]: %v", name, err)
		}
	}
	for name, provider := range c.Tools {
		if strings.TrimSpace(name) == "" {
			return problem("tools contains an empty provider name")
		}
		if !identifierPattern.MatchString(name) {
			return problem(`tools name %q (want lowercase [a-z0-9_-]+)`, name)
		}
		if err := provider.validate(); err != nil {
			return problem("tools[%q]: %v", name, err)
		}
	}
	switch c.Quota.Unmetered {
	case "":
		if len(c.Backends) > 0 {
			return problem(`quota.unmetered must be chosen explicitly ("allow" or "deny") when backends are declared`)
		}
	case ExternalUnmeteredAllow, ExternalUnmeteredDeny:
	default:
		return problem(`quota.unmetered %q (want allow|deny)`, c.Quota.Unmetered)
	}
	if c.Quota.MaxSessionsPerBackend < 0 {
		return problem("quota.maxSessionsPerBackend must be >= 0")
	}
	return nil
}

// ParseTenantConfig decodes one tenant configuration document strictly: the
// body must be a JSON object and unknown fields are rejected so typos surface
// at the write boundary instead of being silently dropped. Validation is a
// separate step (Validate) because callers usually merge defaults first.
func ParseTenantConfig(raw []byte) (TenantConfig, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return TenantConfig{}, fmt.Errorf("tenant config: body must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var cfg TenantConfig
	if err := dec.Decode(&cfg); err != nil {
		return TenantConfig{}, fmt.Errorf("tenant config: %w", err)
	}
	return cfg, nil
}

// LoadModelsDocument reads the provider-configuration fragment named by a
// tenant-document path: the file IS the models section (providers / rules /
// selection), not an envelope. An empty path means "no models document"
// (nil, nil); an unreadable or malformed file is an error the caller turns
// into a boot failure.
//
// The bytes travel verbatim (the resolver owns the vocabulary); only the
// JSON-object shape is checked here so a boot failure names the file.
func LoadModelsDocument(path string) (json.RawMessage, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil, fmt.Errorf("%s: models document must be a JSON object", path)
	}
	return json.RawMessage(trimmed), nil
}

// Merge overlays a tenant document on defaults. Scalars and the compaction
// Enabled flag take the tenant value when explicitly set (non-zero / non-nil);
// Sections, skill Roots, hook Commands, and the tools Allow-list replace the
// defaults only when the tenant slice is non-nil (an empty slice clears them);
// MCP servers merge by name so a tenant can add or override a single server.
// The result owns copies of every slice, map, and pointer, so mutating it never
// changes an input.
func Merge(defaults, tenant TenantConfig) TenantConfig {
	out := TenantConfig{
		Agent: AgentConfig{
			Name:          pickString(defaults.Agent.Name, tenant.Agent.Name),
			Provider:      pickString(defaults.Agent.Provider, tenant.Agent.Provider),
			Model:         pickString(defaults.Agent.Model, tenant.Agent.Model),
			BaseURL:       pickString(defaults.Agent.BaseURL, tenant.Agent.BaseURL),
			Protocol:      pickString(defaults.Agent.Protocol, tenant.Agent.Protocol),
			APIKeyRef:     pickString(defaults.Agent.APIKeyRef, tenant.Agent.APIKeyRef),
			SystemPrompt:  pickString(defaults.Agent.SystemPrompt, tenant.Agent.SystemPrompt),
			Sections:      cloneSections(defaults.Agent.Sections),
			Thinking:      pickString(defaults.Agent.Thinking, tenant.Agent.Thinking),
			MaxTurns:      pickInt(defaults.Agent.MaxTurns, tenant.Agent.MaxTurns),
			ContextWindow: pickInt(defaults.Agent.ContextWindow, tenant.Agent.ContextWindow),
		},
		Models: cloneRawMessage(defaults.Models),
		Skills: SkillsConfig{
			Enabled:      cloneBool(defaults.Skills.Enabled),
			Roots:        cloneStrings(defaults.Skills.Roots),
			FileReadTool: pickString(defaults.Skills.FileReadTool, tenant.Skills.FileReadTool),
		},
		MCP: MCPConfig{Servers: cloneServers(defaults.MCP.Servers)},
		Hooks: HooksConfig{
			Commands: cloneHookCommands(defaults.Hooks.Commands),
		},
		Compaction: CompactionConfig{
			Enabled:          cloneBool(defaults.Compaction.Enabled),
			Threshold:        pickFloat(defaults.Compaction.Threshold, tenant.Compaction.Threshold),
			KeepRecentTokens: pickInt(defaults.Compaction.KeepRecentTokens, tenant.Compaction.KeepRecentTokens),
		},
		Telemetry: TelemetryConfig{
			Adapter: pickString(defaults.Telemetry.Adapter, tenant.Telemetry.Adapter),
		},
		Tools: ToolsConfig{
			Allow: cloneStrings(defaults.Tools.Allow),
		},
		Quota: QuotaConfig{
			RunConcurrency:  pickInt(defaults.Quota.RunConcurrency, tenant.Quota.RunConcurrency),
			EventsPerMinute: pickInt(defaults.Quota.EventsPerMinute, tenant.Quota.EventsPerMinute),
		},
		External: ExternalConfig{
			Backends: cloneExternalProviders(defaults.External.Backends),
			Tools:    cloneExternalProviders(defaults.External.Tools),
			Quota: ExternalQuotaConfig{
				Unmetered:             pickString(defaults.External.Quota.Unmetered, tenant.External.Quota.Unmetered),
				MaxSessionsPerBackend: pickInt(defaults.External.Quota.MaxSessionsPerBackend, tenant.External.Quota.MaxSessionsPerBackend),
			},
		},
	}
	if tenant.Agent.Sections != nil {
		out.Agent.Sections = cloneSections(tenant.Agent.Sections)
	}
	// A nil models section inherits the defaults; a non-nil one (including an
	// explicitly empty object) replaces them, exactly like Sections.
	if tenant.Models != nil {
		out.Models = cloneRawMessage(tenant.Models)
	}
	if tenant.Skills.Roots != nil {
		out.Skills.Roots = cloneStrings(tenant.Skills.Roots)
	}
	if tenant.Skills.Enabled != nil {
		out.Skills.Enabled = cloneBool(tenant.Skills.Enabled)
	}
	if tenant.MCP.Servers != nil {
		if out.MCP.Servers == nil {
			out.MCP.Servers = make(map[string]MCPServerConfig, len(tenant.MCP.Servers))
		}
		for name, server := range tenant.MCP.Servers {
			out.MCP.Servers[name] = cloneServer(server)
		}
	}
	// External providers merge by name exactly like MCP servers: a tenant can
	// add or override one provider without restating the defaults. A default
	// provider is removed with "enabled": false, not by omission.
	if tenant.External.Backends != nil {
		if out.External.Backends == nil {
			out.External.Backends = make(map[string]ExternalProviderConfig, len(tenant.External.Backends))
		}
		for name, provider := range tenant.External.Backends {
			out.External.Backends[name] = cloneExternalProvider(provider)
		}
	}
	if tenant.External.Tools != nil {
		if out.External.Tools == nil {
			out.External.Tools = make(map[string]ExternalProviderConfig, len(tenant.External.Tools))
		}
		for name, provider := range tenant.External.Tools {
			out.External.Tools[name] = cloneExternalProvider(provider)
		}
	}
	if tenant.Hooks.Commands != nil {
		out.Hooks.Commands = cloneHookCommands(tenant.Hooks.Commands)
	}
	if tenant.Tools.Allow != nil {
		out.Tools.Allow = cloneStrings(tenant.Tools.Allow)
	}
	if tenant.Compaction.Enabled != nil {
		out.Compaction.Enabled = cloneBool(tenant.Compaction.Enabled)
	}
	return out
}

// Validate rejects configuration that cannot be assembled, reporting the
// dotted field path. Empty documents are valid: an empty model/provider means
// the fake provider, and every other dimension falls back to its default.
func (c TenantConfig) Validate() error {
	problem := func(format string, args ...any) error {
		return fmt.Errorf("tenant config: "+format, args...)
	}
	if c.Agent.MaxTurns < 0 {
		return problem("agent.maxTurns must be >= 0")
	}
	if c.Agent.ContextWindow < 0 {
		return problem("agent.contextWindow must be >= 0")
	}
	if c.Agent.Protocol != "" {
		if _, err := provider.NormalizeProtocol(c.Agent.Protocol); err != nil {
			return problem("agent.protocol: %v", err)
		}
	}
	if err := validateBaseURL(c.Agent.BaseURL); err != nil {
		return problem("agent.baseUrl: %v", err)
	}
	if c.Agent.APIKeyRef != "" {
		if err := validateSecretRef(c.Agent.APIKeyRef); err != nil {
			return problem("agent.apiKeyRef: %v", err)
		}
	}
	if c.Agent.Thinking != "" && !validThinkingLevel(c.Agent.Thinking) {
		return problem("agent.thinking %q (want off|minimal|low|medium|high|xhigh|max)", c.Agent.Thinking)
	}
	seenSections := make(map[string]struct{}, len(c.Agent.Sections))
	for i, section := range c.Agent.Sections {
		name := strings.TrimSpace(section.Name)
		if name == "" {
			return problem("agent.sections[%d].name is required", i)
		}
		if !identifierPattern.MatchString(name) {
			return problem(`agent.sections[%d].name %q (want lowercase [a-z0-9_-]+)`, i, name)
		}
		if section.Name != name {
			return problem("agent.sections[%d].name must not have surrounding whitespace", i)
		}
		if _, duplicate := seenSections[name]; duplicate {
			return problem("agent.sections[%d].name %q is duplicated", i, name)
		}
		seenSections[name] = struct{}{}
	}
	switch c.Skills.FileReadTool {
	case "", "read", "bash", "indirect":
	default:
		return problem("skills.fileReadTool %q (want read|bash|indirect)", c.Skills.FileReadTool)
	}
	for i, root := range c.Skills.Roots {
		if strings.TrimSpace(root) == "" {
			return problem("skills.roots[%d] is empty", i)
		}
	}
	for name, server := range c.MCP.Servers {
		if strings.TrimSpace(name) == "" {
			return problem("mcp.servers contains an empty server name")
		}
		if !identifierPattern.MatchString(name) {
			return problem(`mcp.servers name %q (want lowercase [a-z0-9_-]+)`, name)
		}
		if err := server.validate(); err != nil {
			return problem("mcp.servers[%q]: %v", name, err)
		}
	}
	for i, command := range c.Hooks.Commands {
		if strings.TrimSpace(command.Event) == "" {
			return problem("hooks.commands[%d].event is required", i)
		}
		if len(command.Command) == 0 || strings.TrimSpace(command.Command[0]) == "" {
			return problem("hooks.commands[%d].command must start with an executable", i)
		}
		if command.TimeoutMS < 0 {
			return problem("hooks.commands[%d].timeoutMs must be >= 0", i)
		}
	}
	if c.Compaction.Threshold != 0 && (c.Compaction.Threshold < 0 || c.Compaction.Threshold > 1) {
		return problem("compaction.threshold must be in (0, 1]")
	}
	if c.Compaction.KeepRecentTokens < 0 {
		return problem("compaction.keepRecentTokens must be >= 0")
	}
	for i, pattern := range c.Tools.Allow {
		if strings.TrimSpace(pattern) == "" {
			return problem("tools.allow[%d] is empty", i)
		}
		if !toolPattern.MatchString(pattern) {
			return problem(`tools.allow[%d] %q (want a tool name or trailing-* wildcard)`, i, pattern)
		}
	}
	switch c.Telemetry.Adapter {
	case "", "memory", "prometheus", "otlp":
	default:
		return problem("telemetry.adapter %q (want memory|prometheus|otlp)", c.Telemetry.Adapter)
	}
	if c.Quota.RunConcurrency < 0 {
		return problem("quota.runConcurrency must be >= 0")
	}
	if c.Quota.EventsPerMinute < 0 {
		return problem("quota.eventsPerMinute must be >= 0")
	}
	if err := c.External.validate(); err != nil {
		return problem("%v", err)
	}
	return nil
}

func (s MCPServerConfig) validate() error {
	command := strings.TrimSpace(s.Command)
	rawURL := strings.TrimSpace(s.URL)
	switch {
	case command == "" && rawURL == "":
		return fmt.Errorf(`either "command" (stdio) or "url" (streamable HTTP) is required`)
	case command != "" && rawURL != "":
		return fmt.Errorf(`"command" and "url" are mutually exclusive`)
	}
	if s.Timeout < 0 {
		return fmt.Errorf("timeout must be >= 0 seconds")
	}
	if rawURL != "" {
		parsed, err := url.Parse(rawURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("url must be an http or https URL")
		}
		if parsed.User != nil {
			return fmt.Errorf("url must not embed credentials; use a header secret reference")
		}
	}
	for name, value := range s.Env {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("env contains an empty variable name")
		}
		if err := validateSecretRef(value); err != nil {
			return fmt.Errorf("env[%q]: %v", name, err)
		}
	}
	for name, value := range s.Headers {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("headers contains an empty header name")
		}
		if err := validateSecretRef(value); err != nil {
			return fmt.Errorf("headers[%q]: %v", name, err)
		}
	}
	return nil
}

// validateSecretRef accepts only indirections. Literal credentials (including
// "Bearer <token>") are rejected so no secret can be persisted in the tenant
// config document.
func validateSecretRef(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("secret reference is required")
	}
	if !secretRefPattern.MatchString(value) {
		return fmt.Errorf(`must be a secret reference (${NAME}, env:NAME, or secret:NAME); literal secrets are not allowed`)
	}
	return nil
}

func validateBaseURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("must be an http or https URL")
	}
	return nil
}

func validThinkingLevel(level string) bool {
	switch agentcore.ThinkingLevel(level) {
	case agentcore.ThinkingOff, agentcore.ThinkingMinimal, agentcore.ThinkingLow,
		agentcore.ThinkingMedium, agentcore.ThinkingHigh, agentcore.ThinkingXHigh,
		agentcore.ThinkingMax:
		return true
	default:
		return false
	}
}

func pickString(defaults, tenant string) string {
	if tenant != "" {
		return tenant
	}
	return defaults
}

func pickInt(defaults, tenant int) int {
	if tenant != 0 {
		return tenant
	}
	return defaults
}

func pickFloat(defaults, tenant float64) float64 {
	if tenant != 0 {
		return tenant
	}
	return defaults
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

// cloneStrings and cloneSections use slices.Clone so a non-nil empty slice
// stays non-nil: "explicitly cleared" must survive a merge, otherwise the next
// merge would treat it as "inherit defaults".
func cloneStrings(values []string) []string {
	return slices.Clone(values)
}

func cloneSections(sections []PromptSection) []PromptSection {
	return slices.Clone(sections)
}

// cloneRawMessage copies raw JSON so the merged document owns its bytes. A
// non-nil empty document stays non-nil: "explicitly cleared" must survive the
// merge, otherwise the next merge would treat it as "inherit defaults".
func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	out := make(json.RawMessage, len(raw))
	copy(out, raw)
	return out
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	copied := make(map[string]string, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

func cloneServer(server MCPServerConfig) MCPServerConfig {
	server.Args = cloneStrings(server.Args)
	server.Env = cloneStringMap(server.Env)
	server.Headers = cloneStringMap(server.Headers)
	server.Enabled = cloneBool(server.Enabled)
	return server
}

func cloneServers(servers map[string]MCPServerConfig) map[string]MCPServerConfig {
	if servers == nil {
		return nil
	}
	copied := make(map[string]MCPServerConfig, len(servers))
	for name, server := range servers {
		copied[name] = cloneServer(server)
	}
	return copied
}

func cloneExternalProvider(provider ExternalProviderConfig) ExternalProviderConfig {
	provider.Args = cloneStrings(provider.Args)
	provider.Env = cloneStringMap(provider.Env)
	provider.Headers = cloneStringMap(provider.Headers)
	provider.Enabled = cloneBool(provider.Enabled)
	return provider
}

func cloneExternalProviders(providers map[string]ExternalProviderConfig) map[string]ExternalProviderConfig {
	if providers == nil {
		return nil
	}
	copied := make(map[string]ExternalProviderConfig, len(providers))
	for name, provider := range providers {
		copied[name] = cloneExternalProvider(provider)
	}
	return copied
}

func cloneHookCommands(commands []HookCommand) []HookCommand {
	if commands == nil {
		return nil
	}
	copied := make([]HookCommand, len(commands))
	for i, command := range commands {
		command.Command = cloneStrings(command.Command)
		copied[i] = command
	}
	return copied
}
