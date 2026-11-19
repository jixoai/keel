package toolsurface

import (
	"encoding/json"

	"github.com/jixoai/keel/internal/agentcore"
)

func newReadTool() agentcore.AgentTool {
	return &singleTool{
		name:        Read,
		description: "Read a file from the session's layerfs view with offset/limit paging. The file-plugin lands in a later wave.",
		schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":0}},"required":["path"],"additionalProperties":true}`),
	}
}
