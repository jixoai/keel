package toolsurface

import (
	"sort"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/skills"
)

// Backends supplies the live services the aggregate tools drive. A nil backend
// keeps the tool present (the surface is constant) and makes its actions answer
// with a typed not_wired error. Tasks is also the attribution backend: when it
// is present, every builtin call is stamped/resolved to a task card before it
// executes (spec task-log: 每次工具调用必须带 taskId).
type Backends struct {
	MCP      MCPBackend
	Agents   AgentBackend
	Creation CreationBackend
	Tasks    TasksBackend
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
	tools := []agentcore.AgentTool{
		newBashTool(),
		newReadTool(),
		newWriteTool(),
		newEditTool(),
		newMCPTool(backends.MCP),
		newSkillsTool(discovered),
		newTasksTool(backends.Tasks),
		newAgentTool(backends.Agents),
		newApproveTool(),
		newNotifyTool(),
		newCreationTool(backends.Creation),
	}
	return wrapWithTaskGate(tools, backends.Tasks)
}
