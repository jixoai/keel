package toolsurface

import (
	"encoding/json"

	"github.com/jixoai/keel/internal/agentcore"
)

func newWriteTool() agentcore.AgentTool {
	return &singleTool{
		name:        Write,
		description: "Write a file through the file-plugin. The file-plugin lands in a later wave.",
		schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"],"additionalProperties":true}`),
	}
}
