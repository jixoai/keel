package host

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/skills"
)

// Config describes one host assembly. It is the static part of a tenant's
// agent configuration; model credentials are injected separately.
type Config struct {
	Preamble     string
	Sections     []Section
	SkillRoots   []string
	FileReadTool string
	// ExtraTools are host-provided tools (MCP adapters, tenant tools).
	ExtraTools []agentcore.AgentTool
	// IncludeBuiltins adds the clock/echo development tools.
	IncludeBuiltins bool
}

// Host composes the system prompt, skills, and tools for a run. The tool
// registry and the dynamic prompt sections can be refreshed after an MCP
// sign-in or a plugin change; an in-flight run keeps using the registry it
// already resolved.
type Host struct {
	mu       sync.RWMutex
	cfg      Config
	skills   []skills.Skill
	warnings []skills.Warning
	tools    *agenttool.ToolRegistry
	// dynamic holds the sections the refresh seam owns: the mcp_servers section
	// and the sections plugins contribute. SystemPrompt renders cfg.Sections
	// (the preamble's static and tenant sections) followed by these, so a refresh
	// replaces exactly the dynamic surface and never touches the sections the
	// configuration assembled.
	dynamic []Section
}

// New discovers skills and builds the tool registry. Skill discovery failures
// are returned as warnings, not errors: a broken skill must not stop the host.
func New(cfg Config) (*Host, []skills.Warning, error) {
	discovered, warnings := skills.Discover(cfg.SkillRoots)
	reg, err := buildRegistry(cfg, discovered)
	if err != nil {
		return nil, warnings, err
	}
	return &Host{cfg: cfg, skills: discovered, warnings: warnings, tools: reg}, warnings, nil
}

// buildRegistry compiles the builtin, host-provided, and skill tools. It is
// shared by boot and RefreshMCP so a refresh has exactly the same validation.
func buildRegistry(cfg Config, discovered []skills.Skill) (*agenttool.ToolRegistry, error) {
	reg := agenttool.NewToolRegistry()
	if cfg.IncludeBuiltins {
		for _, tool := range []agentcore.AgentTool{ClockTool{}, EchoTool{}} {
			if err := reg.Register(tool); err != nil {
				return nil, fmt.Errorf("host: register %s: %w", tool.Name(), err)
			}
		}
	}
	for _, tool := range cfg.ExtraTools {
		if err := reg.Register(tool); err != nil {
			return nil, fmt.Errorf("host: register %s: %w", tool.Name(), err)
		}
	}
	visible := visibleSkills(discovered)
	if len(visible) > 0 {
		if err := reg.Register(SkillTool{skills: visible}); err != nil {
			return nil, fmt.Errorf("host: register skill tool: %w", err)
		}
	}
	return reg, nil
}

// RefreshMCP replaces the MCP-provided tools and prompt section. The new
// registry is built and validated before it is published; callers already
// holding the old registry (an in-flight run) continue safely.
func (h *Host) RefreshMCP(extra []agentcore.AgentTool, section string) error {
	return h.RefreshSections(extra, section, nil)
}

// RefreshSections replaces the host's dynamic surface: the extra tools, the
// mcp_servers section and any additional named sections the serving process
// publishes (plugin prompt contributions). It is RefreshMCP generalized to more
// than one dynamic section, so a caller republishing because of a change in one
// of them (a plugin enable/disable) never drops the others (the MCP section).
// The caller passes the complete dynamic set — exactly like the tool registry
// is republished — so a refresh replaces that set wholesale and never leaves a
// retired section behind. The new registry is built and validated before it is
// published; callers already holding the old registry (an in-flight run)
// continue safely.
func (h *Host) RefreshSections(extra []agentcore.AgentTool, mcpSection string, pluginSections []Section) error {
	h.mu.RLock()
	cfg := h.cfg
	discovered := h.skills
	h.mu.RUnlock()

	cfg.ExtraTools = append([]agentcore.AgentTool(nil), extra...)
	// The construction-time mcp section (when the assembly was built with one) is
	// retired here: the dynamic set owns it from now on.
	static := make([]Section, 0, len(cfg.Sections))
	for _, s := range cfg.Sections {
		if s.Name != "mcp_servers" {
			static = append(static, s)
		}
	}
	cfg.Sections = static
	dynamic := make([]Section, 0, len(pluginSections)+1)
	if mcpSection != "" {
		dynamic = append(dynamic, Section{Name: "mcp_servers", Content: mcpSection})
	}
	dynamic = append(dynamic, pluginSections...)

	reg, err := buildRegistry(cfg, discovered)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.cfg = cfg
	h.dynamic = dynamic
	h.tools = reg
	h.mu.Unlock()
	return nil
}

// Skills returns the discovered skills.
func (h *Host) Skills() []skills.Skill { return h.skills }

// VisibleSkills returns the skills advertised to the model.
func (h *Host) VisibleSkills() []skills.Skill {
	return visibleSkills(h.skills)
}

func visibleSkills(discovered []skills.Skill) []skills.Skill {
	out := make([]skills.Skill, 0, len(discovered))
	for _, skill := range discovered {
		if !skill.DisableModelInvocation {
			out = append(out, skill)
		}
	}
	return out
}

// SystemPrompt renders the PI-shaped sections including the skills index. The
// dynamic sections (mcp_servers and plugin contributions) render after the
// configured ones and before the skills index.
func (h *Host) SystemPrompt() string {
	h.mu.RLock()
	cfg := h.cfg
	discovered := h.skills
	dynamic := append([]Section(nil), h.dynamic...)
	h.mu.RUnlock()
	sections := make([]Section, 0, len(cfg.Sections)+len(dynamic))
	sections = append(sections, cfg.Sections...)
	sections = append(sections, dynamic...)
	return BuildSystemPrompt(PromptConfig{
		Preamble:     cfg.Preamble,
		Sections:     sections,
		Skills:       discovered,
		FileReadTool: cfg.FileReadTool,
	})
}

// Tools returns the registry the loop executes.
func (h *Host) Tools() *agenttool.ToolRegistry {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.tools
}

// SkillTool loads a skill's full instructions on demand (progressive
// disclosure). It is registered only when a visible skill exists.
type SkillTool struct {
	skills []skills.Skill
}

func (SkillTool) Name() string        { return "skill" }
func (SkillTool) Description() string { return "Load a skill's full instructions by name." }
func (SkillTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`)
}
func (SkillTool) ExecutionMode() agentcore.ToolExecutionMode { return agentcore.ToolExecutionParallel }
func (t SkillTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("invalid arguments: " + err.Error())}}, nil
	}
	for _, skill := range t.skills {
		if skill.Name == in.Name {
			content := fmt.Sprintf("Skill %s (%s)\n\n%s", skill.Name, skill.Path, skill.Body)
			return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent(content)}}, nil
		}
	}
	return agentcore.AgentToolResult{Content: agentcore.ContentList{
		agentcore.NewTextContent(fmt.Sprintf("unknown skill %q", in.Name)),
	}}, nil
}

// ClockTool is the development clock tool (same contract as internal/agent's).
type ClockTool struct{}

func (ClockTool) Name() string        { return "clock" }
func (ClockTool) Description() string { return "Return the current time as an ISO 8601 string." }
func (ClockTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}
func (ClockTool) ExecutionMode() agentcore.ToolExecutionMode { return agentcore.ToolExecutionParallel }
func (ClockTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	return agentcore.AgentToolResult{Content: agentcore.ContentList{
		agentcore.NewTextContent(timeNow()),
	}}, nil
}

// EchoTool echoes its argument.
type EchoTool struct{}

func (EchoTool) Name() string        { return "echo" }
func (EchoTool) Description() string { return "Echo the given text back." }
func (EchoTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}},"required":["x"],"additionalProperties":false}`)
}
func (EchoTool) ExecutionMode() agentcore.ToolExecutionMode { return agentcore.ToolExecutionParallel }
func (EchoTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	var in struct {
		X string `json:"x"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("invalid arguments: " + err.Error())}}, nil
	}
	return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent(in.X)}}, nil
}
