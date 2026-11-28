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
	MCP MCPBackend
	// MCPProgress publishes a tool call's notifications/progress frames on the
	// host's transient channel (P4.5). Nil means progress is not shown.
	MCPProgress MCPProgressPublisher
	// MCPApproval gates destructive MCP tools on the model path. Nil is
	// fail-closed: such a call answers mcp_approval_required and never executes.
	MCPApproval MCPApprovalFunc
	Agents      AgentBackend
	Creation    CreationBackend
	Tasks       TasksBackend
	Bash        BashBackend
	Approve     ApproveBackend
	Notify      NotifyBackend
	Files       FileBackend
	// Skills is the tenant-scoped skill library behind the `skills` tool's
	// manage/sources actions and the merge behind ls/view. Nil keeps the tool
	// constant and answers manage/sources with not_wired.
	Skills SkillsBackend
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
		newBashTool(backends.Bash),
		newReadTool(backends.Files),
		newWriteTool(backends.Files),
		newEditTool(backends.Files),
		newMCPTool(backends.MCP, backends.MCPProgress, backends.MCPApproval),
		newSkillsTool(discovered, backends.Skills),
		newTasksTool(backends.Tasks),
		newAgentTool(backends.Agents),
		newApproveTool(backends.Approve),
		newNotifyTool(backends.Notify),
		newCreationTool(backends.Creation),
	}
	return wrapWithTaskGate(tools, backends.Tasks)
}
