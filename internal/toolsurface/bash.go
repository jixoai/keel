package toolsurface

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/harnessext"
)

// BashBackend is the keel.shell plugin's action backend. The aggregate `bash`
// tool is owned by the harness and always present; the plugin supplies the
// implementation, so disabling keel.shell yields plugin_disabled instead of
// removing the tool name.
type BashBackend interface {
	Invoke(ctx context.Context, action string, args json.RawMessage) (agentcore.AgentToolResult, error)
}

// bashSchemas / tool descriptions. The action set and the schema stay exactly
// as the constant surface defined them; only the backend changed. `run` may
// turn into a background job when timeoutMs expires, in which case the result
// carries a jobId for the `jobs` action.
var bashTool = &actionTool{
	name: Bash,
	description: "Run commands and manage background jobs. Actions: run {command, cwd, timeoutMs, env}; " +
		"jobs {operation: list|status|adopt|kill, jobId}. A run whose timeoutMs expires keeps running " +
		"as a background job; `adopt` restores a job's facts and settles its terminal state after a restart. " +
		"env accepts only session variables (PATH, HOME, locale, TZ, TERM); use script-local assignments for other values.",
	schema:     schema(`, "command":{"type":"string"}, "cwd":{"type":"string"}, "timeoutMs":{"type":"integer","minimum":0}, "env":{"type":"object"}, "operation":{"type":"string"}, "jobId":{"type":"string"}`),
	actions:    []string{ActionRun, ActionJobs},
	sequential: true,
}

func newBashTool(backend BashBackend) *actionTool {
	// Return a copy so a caller cannot mutate the package-level template.
	copied := *bashTool
	copied.run = func(ctx context.Context, action string, raw json.RawMessage) (agentcore.AgentToolResult, error) {
		if backend == nil {
			return notWired(Bash, "no bash backend is wired in this deployment")
		}
		result, err := backend.Invoke(ctx, action, raw)
		if err != nil {
			switch {
			case errors.Is(err, harnessext.ErrPluginState), errors.Is(err, harnessext.ErrPluginNotFound):
				return ErrorResult(CodePluginDisabled, Bash,
					"keel.shell is not enabled; enable the plugin to use bash", nil)
			default:
				return ErrorResult(CodeBackendFailed, Bash, err.Error(), map[string]any{"action": action})
			}
		}
		return result, nil
	}
	return &copied
}
