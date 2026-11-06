// Package host assembles PI capabilities into a runnable agent: system prompt
// sections, tools, skills, hooks, MCP servers, compaction and telemetry. It
// does not implement new protocol semantics; it only composes the packages
// that do.
package host

import (
	"strings"

	"github.com/jixoai/keel/internal/skills"
)

// Section is one named system-prompt section. Order is preserved; PI wraps
// every section except the preamble in <name>...</name>.
type Section struct {
	Name    string
	Content string
}

// PromptConfig describes the system prompt the host assembles.
type PromptConfig struct {
	// Preamble is the agent's base instruction. It is emitted verbatim, without
	// an XML wrapper, like PI's preamble section.
	Preamble string
	// Sections are additional named sections in order.
	Sections []Section
	// Skills are advertised by name/description/location only; the body is
	// loaded on demand by the skill tool.
	Skills []skills.Skill
	// FileReadTool selects the PI intro wording: "read", "bash", or "indirect".
	FileReadTool string
}

// BuildSystemPrompt renders the PI-shaped sections. The skills index is only
// included when there is at least one visible skill, and it is emitted as the
// <skills> section after the explicit sections.
func BuildSystemPrompt(cfg PromptConfig) string {
	var b strings.Builder
	preamble := strings.TrimSpace(cfg.Preamble)
	if preamble != "" {
		b.WriteString(preamble)
	}
	writeSection := func(name, content string) {
		content = strings.TrimSpace(content)
		if content == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("<")
		b.WriteString(name)
		b.WriteString(">\n")
		b.WriteString(content)
		b.WriteString("\n</")
		b.WriteString(name)
		b.WriteString(">")
	}
	for _, section := range cfg.Sections {
		writeSection(section.Name, section.Content)
	}
	if len(cfg.Skills) > 0 {
		tool := cfg.FileReadTool
		if tool == "" {
			tool = "read"
		}
		writeSection("skills", skills.FormatForPrompt(cfg.Skills, tool))
	}
	return b.String()
}
