package host

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func boolPtr(value bool) *bool { return &value }

func TestMergeOverlaysTenantOnDefaults(t *testing.T) {
	defaults := TenantConfig{
		Agent: AgentConfig{
			Provider:      "openai",
			Model:         "gpt-5",
			SystemPrompt:  "base prompt",
			Sections:      []PromptSection{{Name: "tenant", Content: "default section"}},
			MaxTurns:      10,
			ContextWindow: 128000,
		},
		Skills: SkillsConfig{Enabled: boolPtr(true), Roots: []string{"/global/skills"}, FileReadTool: "read"},
		MCP: MCPConfig{Servers: map[string]MCPServerConfig{
			"filesystem": {Command: "npx", Args: []string{"-y", "fs"}},
			"shared":     {URL: "https://mcp.example/mcp"},
		}},
		Hooks: HooksConfig{Commands: []HookCommand{{
			Event: "tool_execution_start", Command: []string{"./deny.sh"}, TimeoutMS: 5000,
		}}},
		Compaction: CompactionConfig{Enabled: boolPtr(true), Threshold: 0.8, KeepRecentTokens: 20000},
		Telemetry:  TelemetryConfig{Adapter: "memory"},
		Tools:      ToolsConfig{Allow: []string{"clock"}},
	}
	tenant := TenantConfig{
		Agent: AgentConfig{
			Model:    "gpt-5-mini",
			Sections: []PromptSection{{Name: "tenant", Content: "tenant section"}},
			Thinking: "high",
		},
		Skills: SkillsConfig{Roots: []string{"/tenant/skills"}},
		MCP: MCPConfig{Servers: map[string]MCPServerConfig{
			"filesystem": {Command: "npx", Args: []string{"-y", "tenant-fs"}},
			"extra":      {URL: "https://extra.example/mcp"},
		}},
		Hooks: HooksConfig{Commands: []HookCommand{{
			Event: "tool_execution_end", Command: []string{"./log.sh"},
		}}},
		Compaction: CompactionConfig{Enabled: boolPtr(false)},
		Tools:      ToolsConfig{Allow: []string{"echo", "mcp__*"}},
	}

	merged := Merge(defaults, tenant)
	if merged.Agent.Provider != "openai" || merged.Agent.Model != "gpt-5-mini" {
		t.Fatalf("agent model/provider = %q/%q", merged.Agent.Provider, merged.Agent.Model)
	}
	if merged.Agent.SystemPrompt != "base prompt" || merged.Agent.Thinking != "high" {
		t.Fatalf("agent prompt/thinking = %q/%q", merged.Agent.SystemPrompt, merged.Agent.Thinking)
	}
	if merged.Agent.MaxTurns != 10 || merged.Agent.ContextWindow != 128000 {
		t.Fatalf("agent limits = %d/%d", merged.Agent.MaxTurns, merged.Agent.ContextWindow)
	}
	if len(merged.Agent.Sections) != 1 || merged.Agent.Sections[0].Content != "tenant section" {
		t.Fatalf("sections = %+v", merged.Agent.Sections)
	}
	if len(merged.Skills.Roots) != 1 || merged.Skills.Roots[0] != "/tenant/skills" || merged.Skills.FileReadTool != "read" {
		t.Fatalf("skills = %+v", merged.Skills)
	}
	if merged.Skills.Enabled == nil || !*merged.Skills.Enabled {
		t.Fatalf("skills.enabled = %v, want inherited true", merged.Skills.Enabled)
	}
	if len(merged.MCP.Servers) != 3 {
		t.Fatalf("servers = %+v", merged.MCP.Servers)
	}
	if got := merged.MCP.Servers["filesystem"]; len(got.Args) != 2 || got.Args[1] != "tenant-fs" {
		t.Fatalf("filesystem = %+v", got)
	}
	if got := merged.MCP.Servers["shared"]; got.URL != "https://mcp.example/mcp" {
		t.Fatalf("shared = %+v", got)
	}
	if got := merged.MCP.Servers["extra"]; got.URL != "https://extra.example/mcp" {
		t.Fatalf("extra = %+v", got)
	}
	if len(merged.Hooks.Commands) != 1 || merged.Hooks.Commands[0].Event != "tool_execution_end" {
		t.Fatalf("hooks = %+v", merged.Hooks)
	}
	if merged.Compaction.Enabled == nil || *merged.Compaction.Enabled {
		t.Fatalf("compaction enabled = %v, want explicit false", merged.Compaction.Enabled)
	}
	if merged.Compaction.Threshold != 0.8 || merged.Compaction.KeepRecentTokens != 20000 {
		t.Fatalf("compaction = %+v", merged.Compaction)
	}
	if merged.Telemetry.Adapter != "memory" {
		t.Fatalf("telemetry = %+v", merged.Telemetry)
	}
	if len(merged.Tools.Allow) != 2 || merged.Tools.Allow[0] != "echo" || merged.Tools.Allow[1] != "mcp__*" {
		t.Fatalf("tools = %+v", merged.Tools)
	}

	// The merged value owns its nested data; mutating it must not reach either
	// input (a runner may keep the merged snapshot for several turns).
	merged.Agent.Sections[0].Content = "mutated"
	merged.Skills.Roots[0] = "mutated"
	merged.MCP.Servers["filesystem"].Args[1] = "mutated"
	merged.Hooks.Commands[0].Command[0] = "mutated"
	merged.Tools.Allow[0] = "mutated"
	*merged.Compaction.Enabled = true
	if defaults.Agent.Sections[0].Content != "default section" ||
		defaults.Skills.Roots[0] != "/global/skills" ||
		defaults.MCP.Servers["filesystem"].Args[1] != "fs" ||
		defaults.Hooks.Commands[0].Command[0] != "./deny.sh" ||
		defaults.Tools.Allow[0] != "clock" ||
		*defaults.Compaction.Enabled != true {
		t.Fatal("merge aliased a default value")
	}
	if tenant.Agent.Sections[0].Content != "tenant section" ||
		tenant.MCP.Servers["filesystem"].Args[1] != "tenant-fs" ||
		tenant.Tools.Allow[0] != "echo" ||
		*tenant.Compaction.Enabled != false {
		t.Fatal("merge aliased a tenant value")
	}
}

func TestMergeEmptyTenantInheritsEverything(t *testing.T) {
	defaults := TenantConfig{
		Agent:  AgentConfig{Provider: "anthropic", Model: "claude", Sections: []PromptSection{{Name: "a", Content: "b"}}},
		Skills: SkillsConfig{Roots: []string{"/skills"}, FileReadTool: "bash"},
		MCP:    MCPConfig{Servers: map[string]MCPServerConfig{"s": {Command: "cmd"}}},
		Hooks:  HooksConfig{Commands: []HookCommand{{Event: "e", Command: []string{"c"}}}},
	}
	merged := Merge(defaults, TenantConfig{})
	if merged.Agent.Provider != "anthropic" || merged.Agent.Model != "claude" ||
		len(merged.Agent.Sections) != 1 || len(merged.Skills.Roots) != 1 ||
		merged.Skills.FileReadTool != "bash" || len(merged.MCP.Servers) != 1 ||
		len(merged.Hooks.Commands) != 1 {
		t.Fatalf("merged = %+v", merged)
	}
}

func TestMergeExplicitEmptySlicesClearDefaults(t *testing.T) {
	defaults := TenantConfig{
		Agent:  AgentConfig{Sections: []PromptSection{{Name: "a", Content: "b"}}},
		Skills: SkillsConfig{Roots: []string{"/skills"}},
		Hooks:  HooksConfig{Commands: []HookCommand{{Event: "e", Command: []string{"c"}}}},
	}
	tenant := TenantConfig{
		Agent:  AgentConfig{Sections: []PromptSection{}},
		Skills: SkillsConfig{Roots: []string{}},
		Hooks:  HooksConfig{Commands: []HookCommand{}},
	}
	merged := Merge(defaults, tenant)
	if len(merged.Agent.Sections) != 0 || len(merged.Skills.Roots) != 0 || len(merged.Hooks.Commands) != 0 {
		t.Fatalf("explicit empty slices did not clear: %+v", merged)
	}
	// A cleared slice must stay non-nil; nil would mean "inherit" on the next
	// merge.
	if merged.Agent.Sections == nil || merged.Skills.Roots == nil || merged.Hooks.Commands == nil {
		t.Fatalf("explicit empty slices became nil: %+v", merged)
	}
	// Chaining the same defaults must not resurrect them.
	chained := Merge(defaults, merged)
	if len(chained.Agent.Sections) != 0 || len(chained.Skills.Roots) != 0 || len(chained.Hooks.Commands) != 0 {
		t.Fatalf("chained merge resurrected defaults: %+v", chained)
	}
	if len(defaults.Agent.Sections) != 1 || len(defaults.Skills.Roots) != 1 || len(defaults.Hooks.Commands) != 1 {
		t.Fatal("clearing the tenant value mutated the defaults")
	}
}

func TestValidateAcceptsDocumentedConfig(t *testing.T) {
	raw := []byte(`{
	  "agent": {
	    "name": "tenant-agent",
	    "provider": "openai",
	    "model": "gpt-5",
	    "baseUrl": "https://api.example/v1",
	    "protocol": "openai/resp_api",
	    "apiKeyRef": "env:OPENAI_API_KEY",
	    "systemPrompt": "You are a tenant agent.",
	    "sections": [{"name": "tenant", "content": "acme"}],
	    "thinking": "high",
	    "maxTurns": 8,
	    "contextWindow": 128000
	  },
	  "skills": {"enabled": true, "roots": ["~/.agents/skills", ".agents/skills"], "fileReadTool": "read"},
	  "mcp": {"servers": {
	    "filesystem": {"command": "npx", "args": ["-y", "server-filesystem", "."], "timeout": 60},
	    "remote": {"url": "https://mcp.example/mcp", "headers": {"Authorization": "env:MCP_TOKEN"}}
	  }},
	  "hooks": {"commands": [
	    {"event": "tool_execution_start", "command": ["./hooks/deny-rm.sh"], "timeoutMs": 5000}
	  ]},
	  "compaction": {"enabled": true, "threshold": 0.8, "keepRecentTokens": 20000},
	  "telemetry": {"adapter": "memory"},
	  "tools": {"allow": ["clock", "echo", "skill", "mcp__*"]}
	}`)
	cfg, err := ParseTenantConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Agent.Name != "tenant-agent" || cfg.Agent.Protocol != "openai/resp_api" ||
		cfg.Agent.Thinking != "high" || cfg.Agent.MaxTurns != 8 ||
		cfg.Skills.Enabled == nil || !*cfg.Skills.Enabled ||
		cfg.Skills.FileReadTool != "read" || len(cfg.MCP.Servers) != 2 ||
		len(cfg.Hooks.Commands) != 1 || cfg.Compaction.Threshold != 0.8 ||
		cfg.Telemetry.Adapter != "memory" ||
		len(cfg.Tools.Allow) != 4 || cfg.Tools.Allow[3] != "mcp__*" {
		t.Fatalf("parsed config = %+v", cfg)
	}
}

func TestValidateRejectsBadFields(t *testing.T) {
	cases := []struct {
		name string
		cfg  TenantConfig
		want string
	}{
		{"negative max turns", TenantConfig{Agent: AgentConfig{MaxTurns: -1}}, "agent.maxTurns"},
		{"negative context window", TenantConfig{Agent: AgentConfig{ContextWindow: -1}}, "agent.contextWindow"},
		{"unknown protocol", TenantConfig{Agent: AgentConfig{Protocol: "grpc"}}, "agent.protocol"},
		{"bad base url", TenantConfig{Agent: AgentConfig{BaseURL: "ftp://example"}}, "agent.baseUrl"},
		{"unknown thinking", TenantConfig{Agent: AgentConfig{Thinking: "extreme"}}, "agent.thinking"},
		{"literal api key ref", TenantConfig{Agent: AgentConfig{APIKeyRef: "sk-live-not-a-real-key"}}, "agent.apiKeyRef"},
		{"bad section name", TenantConfig{Agent: AgentConfig{Sections: []PromptSection{{Name: "Bad Name"}}}}, "lowercase [a-z0-9_-]+"},
		{"section name whitespace", TenantConfig{Agent: AgentConfig{Sections: []PromptSection{{Name: " tenant "}}}}, "surrounding whitespace"},
		{"empty section name", TenantConfig{Agent: AgentConfig{Sections: []PromptSection{{Content: "x"}}}}, "sections[0].name"},
		{"duplicate section", TenantConfig{Agent: AgentConfig{Sections: []PromptSection{
			{Name: "a"}, {Name: "a"},
		}}}, "duplicated"},
		{"unknown file read tool", TenantConfig{Skills: SkillsConfig{FileReadTool: "cat"}}, "skills.fileReadTool"},
		{"empty skill root", TenantConfig{Skills: SkillsConfig{Roots: []string{""}}}, "skills.roots[0]"},
		{"mcp no transport", TenantConfig{MCP: MCPConfig{Servers: map[string]MCPServerConfig{"s": {}}}}, "mcp.servers"},
		{"mcp both transports", TenantConfig{MCP: MCPConfig{Servers: map[string]MCPServerConfig{
			"s": {Command: "cmd", URL: "https://example/mcp"},
		}}}, "mutually exclusive"},
		{"mcp bad url", TenantConfig{MCP: MCPConfig{Servers: map[string]MCPServerConfig{
			"s": {URL: "ftp://example"},
		}}}, "http or https"},
		{"mcp negative timeout", TenantConfig{MCP: MCPConfig{Servers: map[string]MCPServerConfig{
			"s": {Command: "cmd", Timeout: -1},
		}}}, "timeout"},
		{"mcp bad server name", TenantConfig{MCP: MCPConfig{Servers: map[string]MCPServerConfig{
			"Server": {Command: "cmd"},
		}}}, "lowercase [a-z0-9_-]+"},
		{"mcp literal header", TenantConfig{MCP: MCPConfig{Servers: map[string]MCPServerConfig{
			"s": {URL: "https://example/mcp", Headers: map[string]string{"Authorization": "Bearer x"}},
		}}}, "secret reference"},
		{"mcp literal env", TenantConfig{MCP: MCPConfig{Servers: map[string]MCPServerConfig{
			"s": {Command: "cmd", Env: map[string]string{"TOKEN": "plain"}},
		}}}, "secret reference"},
		{"mcp url userinfo", TenantConfig{MCP: MCPConfig{Servers: map[string]MCPServerConfig{
			"s": {URL: "https://user:hunter2@example/mcp"},
		}}}, "must not embed credentials"},
		{"hook without event", TenantConfig{Hooks: HooksConfig{Commands: []HookCommand{{Command: []string{"c"}}}}}, "event is required"},
		{"hook without command", TenantConfig{Hooks: HooksConfig{Commands: []HookCommand{{Event: "e"}}}}, "executable"},
		{"hook negative timeout", TenantConfig{Hooks: HooksConfig{Commands: []HookCommand{
			{Event: "e", Command: []string{"c"}, TimeoutMS: -1},
		}}}, "timeoutMs"},
		{"threshold above one", TenantConfig{Compaction: CompactionConfig{Threshold: 1.5}}, "compaction.threshold"},
		{"threshold below zero", TenantConfig{Compaction: CompactionConfig{Threshold: -0.1}}, "compaction.threshold"},
		{"negative keep recent", TenantConfig{Compaction: CompactionConfig{KeepRecentTokens: -1}}, "keepRecentTokens"},
		{"unknown telemetry", TenantConfig{Telemetry: TelemetryConfig{Adapter: "datadog"}}, "telemetry.adapter"},
		{"bad tool allow", TenantConfig{Tools: ToolsConfig{Allow: []string{"bad name"}}}, "tools.allow[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %+v", tc.cfg)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err, tc.want)
			}
		})
	}
}

func TestValidateEmptyConfigIsValid(t *testing.T) {
	var cfg TenantConfig
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty config rejected: %v", err)
	}
}

// A nil slice means "inherit"; an explicit empty slice means "clear". The
// canonical JSON stored by the admin API must preserve that distinction, so the
// three slice fields have no omitempty (nil marshals to null, [] to []).
func TestMarshalPreservesNilVersusEmptyCollections(t *testing.T) {
	nilConfig, err := json.Marshal(TenantConfig{})
	if err != nil {
		t.Fatal(err)
	}
	var nilDoc map[string]any
	if err := json.Unmarshal(nilConfig, &nilDoc); err != nil {
		t.Fatal(err)
	}
	if agent := nilDoc["agent"].(map[string]any); agent["sections"] != nil {
		t.Fatalf("nil sections = %#v, want null", agent["sections"])
	}
	if skills := nilDoc["skills"].(map[string]any); skills["roots"] != nil {
		t.Fatalf("nil roots = %#v, want null", skills["roots"])
	}
	if hooks := nilDoc["hooks"].(map[string]any); hooks["commands"] != nil {
		t.Fatalf("nil commands = %#v, want null", hooks["commands"])
	}
	if tools := nilDoc["tools"].(map[string]any); tools["allow"] != nil {
		t.Fatalf("nil allow = %#v, want null", tools["allow"])
	}

	emptyConfig, err := json.Marshal(TenantConfig{
		Agent:  AgentConfig{Sections: []PromptSection{}},
		Skills: SkillsConfig{Roots: []string{}},
		Hooks:  HooksConfig{Commands: []HookCommand{}},
		Tools:  ToolsConfig{Allow: []string{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var emptyDoc map[string]any
	if err := json.Unmarshal(emptyConfig, &emptyDoc); err != nil {
		t.Fatal(err)
	}
	requireEmpty := func(object map[string]any, key string) {
		t.Helper()
		array, ok := object[key].([]any)
		if !ok || len(array) != 0 {
			t.Fatalf("%s = %#v, want []", key, object[key])
		}
	}
	requireEmpty(emptyDoc["agent"].(map[string]any), "sections")
	requireEmpty(emptyDoc["skills"].(map[string]any), "roots")
	requireEmpty(emptyDoc["hooks"].(map[string]any), "commands")
	requireEmpty(emptyDoc["tools"].(map[string]any), "allow")

	// Parsing the canonical form preserves the empty slices (no inheritance).
	reparsed, err := ParseTenantConfig(emptyConfig)
	if err != nil {
		t.Fatal(err)
	}
	if reparsed.Agent.Sections == nil || reparsed.Skills.Roots == nil ||
		reparsed.Hooks.Commands == nil || reparsed.Tools.Allow == nil {
		t.Fatalf("empty slices lost on reparse: %+v", reparsed)
	}
	merged := Merge(TenantConfig{
		Agent:  AgentConfig{Sections: []PromptSection{{Name: "default"}}},
		Skills: SkillsConfig{Roots: []string{"/default"}},
		Hooks:  HooksConfig{Commands: []HookCommand{{Event: "e", Command: []string{"c"}}}},
		Tools:  ToolsConfig{Allow: []string{"clock"}},
	}, reparsed)
	if len(merged.Agent.Sections) != 0 || len(merged.Skills.Roots) != 0 ||
		len(merged.Hooks.Commands) != 0 || len(merged.Tools.Allow) != 0 {
		t.Fatalf("explicit empty slices did not clear defaults: %+v", merged)
	}
	if merged.Agent.Sections == nil || merged.Skills.Roots == nil ||
		merged.Hooks.Commands == nil || merged.Tools.Allow == nil {
		t.Fatalf("explicit empty slices became nil: %+v", merged)
	}
}

func TestParseTenantConfigRejectsNonObjectAndUnknownFields(t *testing.T) {
	bad := []string{
		``,
		`null`,
		`[]`,
		`"text"`,
		`{"unknownField": true}`,
		`{"agent":{"model":"a"}} {"agent":{"model":"b"}}`,
		`{"agent": {"unknown": 1}}`,
	}
	for _, raw := range bad {
		if _, err := ParseTenantConfig([]byte(raw)); err == nil {
			t.Fatalf("ParseTenantConfig accepted %q", raw)
		}
	}
}

func TestParseTenantConfigRoundTripsCanonicalJSON(t *testing.T) {
	cfg, err := ParseTenantConfig([]byte(`{"agent":{"model":"m"},"mcp":{"servers":{"s":{"command":"c"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reparsed, err := ParseTenantConfig(encoded)
	if err != nil {
		t.Fatalf("canonical JSON did not reparse: %v (%s)", err, encoded)
	}
	if reparsed.Agent.Model != "m" || reparsed.MCP.Servers["s"].Command != "c" {
		t.Fatalf("reparsed = %+v", reparsed)
	}
}

// TestExternalConfigValidation pins the external-provider document rules: the
// transport shape decides stdio vs http, secret references are mandatory in
// env/headers, and the unmetered policy is an explicit choice.
func TestExternalConfigValidation(t *testing.T) {
	valid := TenantConfig{
		External: ExternalConfig{
			Backends: map[string]ExternalProviderConfig{
				"dsh": {
					Command:     "/usr/local/bin/dsh-sidecar",
					Args:        []string{"--stdio"},
					Env:         map[string]string{"DSH_API_KEY": "env:DSH_API_KEY"},
					MaxSessions: 8,
				},
			},
			Tools: map[string]ExternalProviderConfig{
				"docs": {
					Transport: "http",
					URL:       "https://tools.example/mcp",
					Headers:   map[string]string{"Authorization": "secret:TOOLS_TOKEN"},
				},
			},
			Quota: ExternalQuotaConfig{Unmetered: ExternalUnmeteredAllow, MaxSessionsPerBackend: 16},
		},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid external document = %v", err)
	}
	transport, err := valid.External.Backends["dsh"].TransportKind()
	if err != nil || transport != ExternalTransportStdio {
		t.Fatalf("dsh transport = %q, %v", transport, err)
	}
	transport, err = valid.External.Tools["docs"].TransportKind()
	if err != nil || transport != ExternalTransportHTTP {
		t.Fatalf("docs transport = %q, %v", transport, err)
	}

	cases := []struct {
		name    string
		config  TenantConfig
		wantErr string
	}{
		{
			name: "literal env secret",
			config: TenantConfig{External: ExternalConfig{
				Backends: map[string]ExternalProviderConfig{"dsh": {Command: "/bin/dsh", Env: map[string]string{"TOKEN": "sk-live-123"}}},
				Quota:    ExternalQuotaConfig{Unmetered: ExternalUnmeteredDeny},
			}},
			wantErr: "literal secrets are not allowed",
		},
		{
			name: "missing unmetered policy",
			config: TenantConfig{External: ExternalConfig{
				Backends: map[string]ExternalProviderConfig{"dsh": {Command: "/bin/dsh"}},
			}},
			wantErr: "quota.unmetered must be chosen explicitly",
		},
		{
			name: "unknown unmetered policy",
			config: TenantConfig{External: ExternalConfig{
				Backends: map[string]ExternalProviderConfig{"dsh": {Command: "/bin/dsh"}},
				Quota:    ExternalQuotaConfig{Unmetered: "maybe"},
			}},
			wantErr: "want allow|deny",
		},
		{
			name: "command and url",
			config: TenantConfig{External: ExternalConfig{
				Backends: map[string]ExternalProviderConfig{"dsh": {Command: "/bin/dsh", URL: "https://x.example"}},
				Quota:    ExternalQuotaConfig{Unmetered: ExternalUnmeteredAllow},
			}},
			wantErr: "mutually exclusive",
		},
		{
			name: "stdio with url",
			config: TenantConfig{External: ExternalConfig{
				Backends: map[string]ExternalProviderConfig{"dsh": {Transport: "stdio", Command: "/bin/dsh", URL: "https://x.example"}},
				Quota:    ExternalQuotaConfig{Unmetered: ExternalUnmeteredAllow},
			}},
			wantErr: `must not carry "url"`,
		},
		{
			name: "http url with credentials",
			config: TenantConfig{External: ExternalConfig{
				Tools: map[string]ExternalProviderConfig{"docs": {Transport: "http", URL: "https://user:pw@x.example/mcp"}},
			}},
			wantErr: "must not embed credentials",
		},
		{
			name: "illegal provider name",
			config: TenantConfig{External: ExternalConfig{
				Backends: map[string]ExternalProviderConfig{"DSH Backend": {Command: "/bin/dsh"}},
				Quota:    ExternalQuotaConfig{Unmetered: ExternalUnmeteredAllow},
			}},
			wantErr: "want lowercase",
		},
		{
			name: "negative max sessions",
			config: TenantConfig{External: ExternalConfig{
				Backends: map[string]ExternalProviderConfig{"dsh": {Command: "/bin/dsh", MaxSessions: -1}},
				Quota:    ExternalQuotaConfig{Unmetered: ExternalUnmeteredAllow},
			}},
			wantErr: "maxSessions must be >= 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.config.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestExternalConfigMerge pins the by-name merge and the ownership of copies:
// a tenant document overrides one provider without restating the defaults, and
// mutating the result never mutates an input.
func TestExternalConfigMerge(t *testing.T) {
	defaults := TenantConfig{External: ExternalConfig{
		Backends: map[string]ExternalProviderConfig{
			"dsh":   {Command: "/bin/dsh", Env: map[string]string{"DSH_API_KEY": "env:KEY"}},
			"local": {Command: "/bin/local", Enabled: boolPtr(true)},
		},
		Quota: ExternalQuotaConfig{Unmetered: ExternalUnmeteredDeny, MaxSessionsPerBackend: 4},
	}}
	tenant := TenantConfig{External: ExternalConfig{
		Backends: map[string]ExternalProviderConfig{
			"dsh":   {Command: "/opt/dsh", Args: []string{"--fast"}},
			"local": {Command: "/bin/local", Enabled: boolPtr(false)},
		},
		Tools: map[string]ExternalProviderConfig{"docs": {URL: "https://docs.example/mcp"}},
		Quota: ExternalQuotaConfig{Unmetered: ExternalUnmeteredAllow},
	}}
	merged := Merge(defaults, tenant)
	if got := merged.External.Backends["dsh"].Command; got != "/opt/dsh" {
		t.Fatalf("dsh command = %q", got)
	}
	if len(merged.External.Backends["dsh"].Args) != 1 {
		t.Fatalf("dsh args = %+v", merged.External.Backends["dsh"].Args)
	}
	if merged.External.Backends["local"].ProviderEnabled() {
		t.Fatal("explicit enabled=false was lost")
	}
	if _, ok := merged.External.Tools["docs"]; !ok {
		t.Fatal("tenant tool provider missing")
	}
	if merged.External.Quota.Unmetered != ExternalUnmeteredAllow || merged.External.Quota.MaxSessionsPerBackend != 4 {
		t.Fatalf("quota = %+v", merged.External.Quota)
	}
	merged.External.Backends["dsh"] = ExternalProviderConfig{Command: "mutated"}
	if defaults.External.Backends["dsh"].Command != "/bin/dsh" {
		t.Fatal("merge did not copy the provider map")
	}
	provider := merged.External.Backends["dsh"]
	provider.Command = "mutated-again"
	if defaults.External.Backends["dsh"].Command != "/bin/dsh" {
		t.Fatal("defaults were mutated through a copied provider")
	}
}

// TestModelsDocumentMerge pins the pass-through contract of the provider
// configuration slice: it merges like the other sections (nil inherits, an
// explicit value replaces) and the merged document owns its bytes.
func TestModelsDocumentMerge(t *testing.T) {
	defaults := TenantConfig{Models: json.RawMessage(`{"selection":{"modelId":"a"}}`)}
	if got := string(Merge(defaults, TenantConfig{}).Models); got != `{"selection":{"modelId":"a"}}` {
		t.Fatalf("a nil models section must inherit the defaults: %s", got)
	}
	tenant := json.RawMessage(`{"selection":{"modelId":"b"}}`)
	merged := Merge(defaults, TenantConfig{Models: tenant})
	if string(merged.Models) != `{"selection":{"modelId":"b"}}` {
		t.Fatalf("an explicit models section must replace the defaults: %s", merged.Models)
	}
	merged.Models[0] = '['
	if tenant[0] != '{' || defaults.Models[0] != '{' {
		t.Fatal("merge did not copy the models bytes")
	}
}

// TestLoadModelsDocument pins the file contract: the file IS the models
// section, an empty path or file means "no models document", and a malformed
// one names the file so boot fails readably.
func TestLoadModelsDocument(t *testing.T) {
	if raw, err := LoadModelsDocument(""); raw != nil || err != nil {
		t.Fatalf("empty path = %s, %v; want no document", raw, err)
	}
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	path := write("models.json", `{"providers":[],"selection":{"modelId":"m"}}`)
	raw, err := LoadModelsDocument(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// The loaded bytes are exactly what a tenant document carries, so the file,
	// the admin API and both commands speak one shape.
	doc, err := ParseTenantConfig([]byte(`{"models":` + string(raw) + `}`))
	if err != nil {
		t.Fatalf("the fragment must parse as the document's models section: %v", err)
	}
	if string(doc.Models) != string(raw) {
		t.Fatalf("models round trip = %s", doc.Models)
	}
	if raw, err := LoadModelsDocument(write("empty.json", " \n")); raw != nil || err != nil {
		t.Fatalf("an empty file = %s, %v; want no document", raw, err)
	}
	if _, err := LoadModelsDocument(write("array.json", `[1,2]`)); err == nil || !strings.Contains(err.Error(), "array.json") {
		t.Fatalf("a non-object must be an error naming the file, got %v", err)
	}
	if _, err := LoadModelsDocument(write("broken.json", `{`)); err == nil {
		t.Fatal("a malformed file must be an error")
	}
	if _, err := LoadModelsDocument(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("an unreadable file must be an error")
	}
}
