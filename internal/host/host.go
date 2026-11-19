package host

import (
	"fmt"
	"strings"
	"sync"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/skills"
	"github.com/jixoai/keel/internal/toolsurface"
)

// Config describes one host assembly. It is the static part of a tenant's
// agent configuration; model credentials are injected separately.
type Config struct {
	Preamble     string
	Sections     []Section
	SkillRoots   []string
	FileReadTool string
	// ExtraTools are host-provided tools (external providers, tenant tools and
	// the tools approved plugins publish). MCP tools are no longer flattened
	// into extras: the builtin `mcp` aggregate tool is the only MCP entry.
	ExtraTools []agentcore.AgentTool
	// IncludeBuiltins adds the constant eleven-tool builtin surface.
	IncludeBuiltins bool
	// ToolsAllow is the tenant document's tools.allow filter. Nil keeps every
	// assembled tool; a non-nil list keeps only names matching one of its
	// entries, where a trailing "*" is a prefix wildcard ("keel_*"); an explicit
	// empty list keeps none. It applies to the builtin surface and ExtraTools
	// alike, so a plugin cannot bypass it by publishing a tool.
	ToolsAllow []string
	// ToolBackends wires the services the aggregate builtins drive (MCP, internal
	// sub-agents, creation). Nil backends keep the tool present and answer
	// not_wired.
	ToolBackends toolsurface.Backends
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
// shared by boot and RefreshMCP so a refresh has exactly the same validation
// and the same tools.allow filter.
func buildRegistry(cfg Config, discovered []skills.Skill) (*agenttool.ToolRegistry, error) {
	reg := agenttool.NewToolRegistry()
	allow := newToolAllowList(cfg.ToolsAllow)
	if cfg.IncludeBuiltins {
		for _, tool := range toolsurface.BuiltinTools(visibleSkills(discovered), cfg.ToolBackends) {
			if !allow.allows(tool.Name()) {
				continue
			}
			if err := reg.Register(tool); err != nil {
				return nil, fmt.Errorf("host: register %s: %w", tool.Name(), err)
			}
		}
	}
	for _, tool := range cfg.ExtraTools {
		if !allow.allows(tool.Name()) {
			continue
		}
		if err := reg.Register(tool); err != nil {
			return nil, fmt.Errorf("host: register %s: %w", tool.Name(), err)
		}
	}
	return reg, nil
}

// toolAllowList is the compiled tools.allow filter. A nil pattern list means
// "no filter"; a non-nil empty list means "keep nothing" (the document
// explicitly cleared the surface). Keeping the nil/non-nil distinction is what
// lets the tenant document express both.
type toolAllowList struct {
	patterns []string
	active   bool
}

func newToolAllowList(patterns []string) toolAllowList {
	return toolAllowList{patterns: patterns, active: patterns != nil}
}

func (a toolAllowList) allows(name string) bool {
	if !a.active {
		return true
	}
	for _, pattern := range a.patterns {
		if prefix, wildcard := strings.CutSuffix(pattern, "*"); wildcard {
			if strings.HasPrefix(name, prefix) {
				return true
			}
			continue
		}
		if pattern == name {
			return true
		}
	}
	return false
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
	return h.RenderSystemPrompt("")
}

// RenderSystemPrompt renders the assembly with preamble as the base instruction;
// an empty preamble keeps the configured one. It exists for the execution
// binding (change keel-agent-team-v1): an Agent's system prompt replaces the
// deployment preamble while the dynamic surface (mcp_servers, plugin sections,
// skills index) keeps rendering around it, so selecting an Agent never silently
// drops those sections.
func (h *Host) RenderSystemPrompt(preamble string) string {
	h.mu.RLock()
	cfg := h.cfg
	discovered := h.skills
	dynamic := append([]Section(nil), h.dynamic...)
	h.mu.RUnlock()
	if strings.TrimSpace(preamble) == "" {
		preamble = cfg.Preamble
	}
	sections := make([]Section, 0, len(cfg.Sections)+len(dynamic))
	sections = append(sections, cfg.Sections...)
	sections = append(sections, dynamic...)
	return BuildSystemPrompt(PromptConfig{
		Preamble:     preamble,
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
