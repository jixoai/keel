package toolsurface

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/harnessext"
)

// ApproveBackend is the keel.approval plugin's action backend. The aggregate
// tool is owned by the harness and always present; the plugin only supplies the
// implementation, so disabling keel.approval yields plugin_disabled instead of
// removing the tool name.
type ApproveBackend interface {
	Invoke(ctx context.Context, action string, args json.RawMessage) (agentcore.AgentToolResult, error)
}

var approveActions = []string{ActionConfirm, ActionPrompt, ActionChoice}

const approveToolDescription = "Ask a human and wait for the durable answer (human-in-the-loop). " +
	"Actions: confirm {prompt, yesLabel?, noLabel?} — approve or refuse; prompt {prompt} — a free-text answer; " +
	"choice {prompt, options:[{id,label,description?}], allowOther?} — pick one option, where the 'other' escape is " +
	"free text returned with validated:false. All actions accept timeoutMs (default 5m, max 24h) and requestId; " +
	"a retry with the same requestId addresses the same question, so after a timeout pass a new requestId to ask again. " +
	"The call blocks until answered, times out (the persisted timed_out state; confirm's safe side is 'no') or the " +
	"run is cancelled; the result carries the provenance (who answered, over which channel, validated/escape). " +
	"The model cannot answer its own question. The keel.approval plugin supplies the backend."

func approveSchema() json.RawMessage {
	return schema(`, "prompt":{"type":"string"}, ` +
		`"options":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"label":{"type":"string"},"description":{"type":"string"}},"required":["id"]}}, ` +
		`"yesLabel":{"type":"string"}, "noLabel":{"type":"string"}, "allowOther":{"type":"boolean"}, ` +
		`"timeoutMs":{"type":"integer","minimum":0}, "requestId":{"type":"string"}`)
}

func newApproveTool(backend ApproveBackend) *actionTool {
	return &actionTool{
		name:        Approve,
		description: approveToolDescription,
		schema:      approveSchema(),
		actions:     approveActions,
		// Waiting for a human is a barrier: the rest of the batch must not race
		// ahead of the question.
		sequential: true,
		run: func(ctx context.Context, action string, raw json.RawMessage) (agentcore.AgentToolResult, error) {
			if backend == nil {
				return notWired(Approve, "no approval backend is wired in this deployment")
			}
			result, err := backend.Invoke(ctx, action, raw)
			if err != nil {
				switch {
				case errors.Is(err, harnessext.ErrPluginState), errors.Is(err, harnessext.ErrPluginNotFound):
					return ErrorResult(CodePluginDisabled, Approve,
						"keel.approval is not enabled; enable the plugin to use approve actions", map[string]any{"action": action})
				default:
					return ErrorResult(CodeBackendFailed, Approve, err.Error(), map[string]any{"action": action})
				}
			}
			return result, nil
		},
	}
}
