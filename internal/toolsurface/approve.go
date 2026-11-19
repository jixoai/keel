package toolsurface

import (
	"context"
	"encoding/json"

	"github.com/jixoai/keel/internal/agentcore"
)

var approveTool = &actionTool{
	name: Approve,
	description: "Ask the user to confirm, choose or answer. Actions: confirm, prompt, choice. " +
		"The approval service lands in a later wave.",
	schema:  schema(`, "prompt":{"type":"string"}, "options":{"type":"array","items":{"type":"object"}}`),
	actions: []string{ActionConfirm, ActionPrompt, ActionChoice},
	run: func(_ context.Context, action string, _ json.RawMessage) (agentcore.AgentToolResult, error) {
		return NotImplemented(Approve, action)
	},
}

func newApproveTool() *actionTool {
	copied := *approveTool
	copied.run = approveTool.run
	return &copied
}
