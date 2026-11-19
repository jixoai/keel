package toolsurface

import (
	"context"
	"encoding/json"

	"github.com/jixoai/keel/internal/agentcore"
)

func newEditTool(backend FileBackend) agentcore.AgentTool {
	return &fileTool{
		name: Edit,
		description: "Replace exact text in a file through the keel.file plugin. expectedHash is required (read first). " +
			"oldText must occur exactly once unless replaceAll is true; zero or multiple matches are typed conflicts. " +
			"The edit is streamed into layerfs, appends one fs.write event and writes through to the execution space.",
		schema:    json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"oldText":{"type":"string"},"newText":{"type":"string"},"expectedHash":{"type":"string"},"replaceAll":{"type":"boolean"},"taskId":{"type":"string"}},"required":["path","oldText","newText"],"additionalProperties":true}`),
		available: backend != nil,
		run: func(ctx context.Context, args json.RawMessage) (agentcore.AgentToolResult, error) {
			if backend == nil {
				return notWired(Edit, "no file backend is wired in this deployment")
			}
			return backend.Edit(ctx, args)
		},
	}
}
