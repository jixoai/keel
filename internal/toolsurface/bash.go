package toolsurface

import (
	"context"
	"encoding/json"

	"github.com/jixoai/keel/internal/agentcore"
)

// bashSchemas / tool descriptions. The action exists now so the model-visible
// surface is constant; the call returns the typed not_implemented error until
// the bash bridge + JobManager land (W2.2).
var bashTool = &actionTool{
	name: Bash,
	description: "Run commands and manage background jobs. Actions: run {command, cwd, timeoutMs, env}; " +
		"jobs {operation: list|status|adopt|kill, jobId}. The bash bridge and JobManager land in a later wave.",
	schema:  schema(`, "command":{"type":"string"}, "cwd":{"type":"string"}, "timeoutMs":{"type":"integer","minimum":0}, "env":{"type":"object"}, "operation":{"type":"string"}, "jobId":{"type":"string"}`),
	actions: []string{ActionRun, ActionJobs},
	run: func(_ context.Context, action string, _ json.RawMessage) (agentcore.AgentToolResult, error) {
		return NotImplemented(Bash, action)
	},
}

func newBashTool() *actionTool {
	// Return a copy so a caller cannot mutate the package-level template.
	copied := *bashTool
	copied.run = func(_ context.Context, action string, _ json.RawMessage) (agentcore.AgentToolResult, error) {
		return NotImplemented(Bash, action)
	}
	return &copied
}
