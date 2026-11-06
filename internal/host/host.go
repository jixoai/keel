package host

import (
	"context"
	"encoding/json"
	"fmt"

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

// Host composes the system prompt, skills, and tools for a run.
type Host struct {
	cfg      Config
	skills   []skills.Skill
	warnings []skills.Warning
	tools    *agenttool.ToolRegistry
}

// New discovers skills and builds the tool registry. Skill discovery failures
// are returned as warnings, not errors: a broken skill must not stop the host.
func New(cfg Config) (*Host, []skills.Warning, error) {
	discovered, warnings := skills.Discover(cfg.SkillRoots)
	h := &Host{cfg: cfg, skills: discovered, warnings: warnings, tools: agenttool.NewToolRegistry()}
	if cfg.IncludeBuiltins {
		for _, tool := range []agentcore.AgentTool{ClockTool{}, EchoTool{}} {
			if err := h.tools.Register(tool); err != nil {
				return nil, warnings, fmt.Errorf("host: register %s: %w", tool.Name(), err)
			}
		}
	}
	for _, tool := range cfg.ExtraTools {
		if err := h.tools.Register(tool); err != nil {
			return nil, warnings, fmt.Errorf("host: register %s: %w", tool.Name(), err)
		}
	}
	// The skill loader is only useful when at least one skill is visible.
	visible := h.VisibleSkills()
	if len(visible) > 0 {
		if err := h.tools.Register(SkillTool{skills: visible}); err != nil {
			return nil, warnings, fmt.Errorf("host: register skill tool: %w", err)
		}
	}
	return h, warnings, nil
}

// Skills returns the discovered skills.
func (h *Host) Skills() []skills.Skill { return h.skills }

// VisibleSkills returns the skills advertised to the model.
func (h *Host) VisibleSkills() []skills.Skill {
	out := make([]skills.Skill, 0, len(h.skills))
	for _, skill := range h.skills {
		if !skill.DisableModelInvocation {
			out = append(out, skill)
		}
	}
	return out
}

// SystemPrompt renders the PI-shaped sections including the skills index.
func (h *Host) SystemPrompt() string {
	return BuildSystemPrompt(PromptConfig{
		Preamble:     h.cfg.Preamble,
		Sections:     h.cfg.Sections,
		Skills:       h.skills,
		FileReadTool: h.cfg.FileReadTool,
	})
}

// Tools returns the registry the loop executes.
func (h *Host) Tools() *agenttool.ToolRegistry { return h.tools }

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
