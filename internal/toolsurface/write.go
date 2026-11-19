package toolsurface

import (
	"context"
	"encoding/json"

	"github.com/jixoai/keel/internal/agentcore"
)

func newWriteTool(backend FileBackend) agentcore.AgentTool {
	return &fileTool{
		name: Write,
		description: "Write a file through the keel.file plugin. Creating a new file omits expectedHash; " +
			"overwriting an existing file requires the hash read returned, otherwise the write is a typed conflict. " +
			"The write is streamed into layerfs, appends one fs.write event and writes through to the execution space.",
		schema:    json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"},"expectedHash":{"type":"string"},"mode":{"type":"integer","minimum":0},"taskId":{"type":"string"}},"required":["path","content"],"additionalProperties":true}`),
		available: backend != nil,
		run: func(ctx context.Context, args json.RawMessage) (agentcore.AgentToolResult, error) {
			if backend == nil {
				return notWired(Write, "no file backend is wired in this deployment")
			}
			return backend.Write(ctx, args)
		},
	}
}
