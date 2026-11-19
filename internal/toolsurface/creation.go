package toolsurface

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentext"
)

// CreationBackend is the keel.creation plugin's action backend. The aggregate
// tool is owned by the harness and always present; the plugin only supplies the
// implementation, so disabling keel.creation yields plugin_disabled instead of
// removing the tool name.
type CreationBackend interface {
	Invoke(ctx context.Context, action string, args json.RawMessage) (agentcore.AgentToolResult, error)
}

var creationActions = []string{ActionCatalog, ActionScaffold, ActionValidate, ActionInstall, ActionList, ActionRemove}

const creationToolDescription = "Creation mode: build, validate, install and remove real plugin packages. Actions: " +
	"catalog (read-only aggregate of installed plugins and their declared surface); scaffold {id, purpose, title?, tool?, version?}; " +
	"validate {id, path?}; install {id, requestId?, decisionId?} (durable approval, call again with decisionId); " +
	"list; remove {id, requestId?, decisionId?}. The keel.creation plugin supplies the backend."

func creationSchema() json.RawMessage {
	return schema(`, "id":{"type":"string"}, "purpose":{"type":"string"}, "title":{"type":"string"}, ` +
		`"tool":{"type":"string"}, "version":{"type":"string"}, "kind":{"type":"string"}, "path":{"type":"string"}, ` +
		`"requestId":{"type":"string"}, "decisionId":{"type":"string"}`)
}

func newCreationTool(backend CreationBackend) *actionTool {
	return &actionTool{
		name:        Creation,
		description: creationToolDescription,
		schema:      creationSchema(),
		actions:     creationActions,
		sequential:  true,
		run: func(ctx context.Context, action string, raw json.RawMessage) (agentcore.AgentToolResult, error) {
			if backend == nil {
				return notWired(Creation, "no creation backend is wired in this deployment")
			}
			result, err := backend.Invoke(ctx, action, raw)
			if err != nil {
				switch {
				case errors.Is(err, agentext.ErrNotWired):
					// The manager itself is absent: a deployment wiring gap, not a
					// disabled plugin. Keep the two facts distinguishable.
					return notWired(Creation, err.Error())
				case errors.Is(err, agentext.ErrPluginNotFound):
					return ErrorResult(CodePluginDisabled, Creation,
						"keel.creation is not installed; install the plugin to use creation actions", nil)
				case errors.Is(err, agentext.ErrPluginState):
					return ErrorResult(CodePluginDisabled, Creation,
						"keel.creation is not enabled; enable the plugin to use creation actions", nil)
				default:
					return ErrorResult(CodeBackendFailed, Creation, err.Error(), map[string]any{"action": action})
				}
			}
			return result, nil
		},
	}
}
