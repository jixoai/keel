package toolsurface

import (
	"sort"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/skills"
)

// Backends supplies the live services the aggregate tools drive. A nil backend
// keeps the tool present (the surface is constant) and makes its actions answer
// with a typed not_wired error.
type Backends struct {
	MCP      MCPBackend
	Agents   AgentBackend
	Creation CreationBackend
}

// BuiltinNames returns the eleven builtin tool names, sorted.
func BuiltinNames() []string {
	names := []string{Bash, Read, Write, Edit, MCP, Skills, Tasks, Agent, Approve, Notify, Creation}
	sort.Strings(names)
	return names
}

// BuiltinTools builds the constant harness-owned tool set. The disabled skills
// are filtered here: a skill with disable-model-invocation never appears in the
// `skills` tool's ls output.
func BuiltinTools(discovered []skills.Skill, backends Backends) []agentcore.AgentTool {
	return []agentcore.AgentTool{
		newBashTool(),
		newReadTool(),
		newWriteTool(),
		newEditTool(),
		newMCPTool(backends.MCP),
		newSkillsTool(discovered),
		newTasksTool(),
		newAgentTool(backends.Agents),
		newApproveTool(),
		newNotifyTool(),
		newCreationTool(backends.Creation),
	}
}
