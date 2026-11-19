package host

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
)

func writeSkill(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + name + " skill\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHostAssemblesPromptAndTools(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "pdf", "Read the PDF body.")
	h, warnings, err := New(Config{
		Preamble:        "You are a test agent.",
		SkillRoots:      []string{root},
		IncludeBuiltins: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	prompt := h.SystemPrompt()
	if !strings.Contains(prompt, "<skills>") || !strings.Contains(prompt, "<name>pdf</name>") {
		t.Fatalf("prompt = %q", prompt)
	}
	// The builtin surface is the constant eleven-tool set; a skill is content
	// reached through skills.view, never a tool name of its own.
	wantBuiltins := []string{"agent", "approve", "bash", "creation", "edit", "mcp", "notify", "read", "skills", "tasks", "write"}
	if got := strings.Join(h.Tools().Names(), ","); got != strings.Join(wantBuiltins, ",") {
		t.Fatalf("builtin tools = %v, want %v", h.Tools().Names(), wantBuiltins)
	}
	for _, removed := range []string{"skill", "clock", "echo", "spawn_subagent"} {
		if _, ok := h.Tools().Get(removed); ok {
			t.Fatalf("removed tool %q is still registered", removed)
		}
	}
	if _, ok := h.Tools().Get("pdf"); ok {
		t.Fatal("skill body should not be a tool")
	}
	// skills.view loads the body on demand.
	tool, _ := h.Tools().Get("skills")
	result, err := tool.Execute(context.Background(), "call-1", json.RawMessage(`{"action":"view","name":"pdf"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content[0].(agentcore.TextContent).Text, "Read the PDF body.") {
		t.Fatalf("skill body = %#v", result.Content)
	}
}

func TestHostHidesDisabledSkillFromPrompt(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "hidden")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: hidden\ndescription: hidden skill\ndisable-model-invocation: true\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	h, _, err := New(Config{Preamble: "base", SkillRoots: []string{root}, IncludeBuiltins: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.SystemPrompt(), "hidden") {
		t.Fatalf("disabled skill advertised: %q", h.SystemPrompt())
	}
	// The skills tool is constant; a disabled skill is simply absent from ls/view.
	tool, ok := h.Tools().Get("skills")
	if !ok {
		t.Fatal("the skills tool must exist even with no visible skills")
	}
	result, err := tool.Execute(context.Background(), "call-1", json.RawMessage(`{"action":"ls"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Content[0].(agentcore.TextContent).Text, "hidden") {
		t.Fatalf("disabled skill leaked into ls: %#v", result.Content)
	}
}

type namedTestTool struct{ name string }

func (n namedTestTool) Name() string          { return n.name }
func (namedTestTool) Description() string     { return "test tool" }
func (namedTestTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (namedTestTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}
func (namedTestTool) Execute(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	return agentcore.AgentToolResult{}, nil
}

func TestRefreshMCPReplacesToolsAndPromptSection(t *testing.T) {
	h, _, err := New(Config{
		Preamble:   "base",
		Sections:   []Section{{Name: "base", Content: "BASE"}},
		ExtraTools: []agentcore.AgentTool{namedTestTool{name: "old_mcp"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.Tools().Get("old_mcp"); !ok {
		t.Fatal("old tool missing before refresh")
	}
	if err := h.RefreshMCP([]agentcore.AgentTool{namedTestTool{name: "new_mcp"}}, "MCP SERVERS"); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.Tools().Get("old_mcp"); ok {
		t.Fatal("old tool survived refresh")
	}
	if _, ok := h.Tools().Get("new_mcp"); !ok {
		t.Fatal("new tool missing after refresh")
	}
	prompt := h.SystemPrompt()
	if !strings.Contains(prompt, "MCP SERVERS") || !strings.Contains(prompt, "BASE") {
		t.Fatalf("prompt = %q", prompt)
	}
}

// TestToolsAllowFiltersBuiltinsAndPluginTools pins the tenant tools.allow
// filter: it applies to the builtin surface and to host-provided (plugin)
// tools alike, and a trailing "*" is a prefix wildcard. RefreshSections builds
// through the same path, so a plugin installed after boot cannot bypass it.
func TestToolsAllowFiltersBuiltinsAndPluginTools(t *testing.T) {
	h, _, err := New(Config{
		IncludeBuiltins: true,
		ExtraTools: []agentcore.AgentTool{
			namedTestTool{name: "vendor_panel_refresh"},
			namedTestTool{name: "unlisted_tool"},
		},
		ToolsAllow: []string{"bash", "agent", "vendor_*"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(h.Tools().Names(), ","), "agent,bash,vendor_panel_refresh"; got != want {
		t.Fatalf("filtered tool surface = %s, want %s", got, want)
	}
	if err := h.RefreshSections([]agentcore.AgentTool{
		namedTestTool{name: "vendor_catalog"},
		namedTestTool{name: "unlisted_tool"},
	}, "", nil); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(h.Tools().Names(), ","), "agent,bash,vendor_catalog"; got != want {
		t.Fatalf("refreshed tool surface = %s, want %s", got, want)
	}
}

// TestToolsAllowExplicitEmptyKeepsNoTools pins the nil/non-nil distinction the
// tenant document needs: an explicit [] clears the surface instead of meaning
// "inherit everything".
func TestToolsAllowExplicitEmptyKeepsNoTools(t *testing.T) {
	h, _, err := New(Config{
		IncludeBuiltins: true,
		ExtraTools:      []agentcore.AgentTool{namedTestTool{name: "plugin_tool"}},
		ToolsAllow:      []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if names := h.Tools().Names(); len(names) != 0 {
		t.Fatalf("explicit empty tools.allow kept %v", names)
	}
}

func TestToolAllowListMatching(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		tool     string
		allow    bool
	}{
		{name: "nil keeps everything", patterns: nil, tool: "anything", allow: true},
		{name: "empty keeps nothing", patterns: []string{}, tool: "bash", allow: false},
		{name: "exact match", patterns: []string{"bash"}, tool: "bash", allow: true},
		{name: "exact miss", patterns: []string{"read"}, tool: "bash", allow: false},
		{name: "prefix wildcard match", patterns: []string{"keel_*"}, tool: "keel_echo", allow: true},
		{name: "prefix wildcard does not overreach", patterns: []string{"keel_*"}, tool: "other", allow: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			allow := newToolAllowList(test.patterns)
			if got := allow.allows(test.tool); got != test.allow {
				t.Fatalf("allows(%q) with %v = %v, want %v", test.tool, test.patterns, got, test.allow)
			}
		})
	}
}

func TestRefreshMCPConcurrentReads(t *testing.T) {
	h, _, err := New(Config{Preamble: "base", IncludeBuiltins: true})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			_ = h.Tools().List()
			_ = h.SystemPrompt()
		}(i)
	}
	for i := 0; i < 8; i++ {
		if err := h.RefreshMCP([]agentcore.AgentTool{namedTestTool{name: "mcp_" + string(rune('a'+i))}}, "section"); err != nil {
			t.Error(err)
		}
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
