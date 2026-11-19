package toolsurface

import (
	"encoding/json"

	"github.com/jixoai/keel/internal/agentcore"
)

func newEditTool() agentcore.AgentTool {
	return &singleTool{
		name:        Edit,
		description: "Replace a range in a file through the file-plugin. The file-plugin lands in a later wave.",
		schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"oldText":{"type":"string"},"newText":{"type":"string"}},"required":["path"],"additionalProperties":true}`),
	}
}
