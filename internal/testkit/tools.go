// Package testkit holds the development tools that used to be built into the
// harness. They are no longer part of the model-visible builtin surface: the
// `keel.testkit` plugin owns them and is disabled by default, so they appear
// only after an operator installs/enables the plugin and (for a bound run)
// grants them to the agent.
//
// The tools are real implementations on purpose: E2E uses `clock` to prove a
// plugin-provided tool still passes the agent grant filter, and tests use
// `echo` for deterministic tool round-trips without a network.
package testkit

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
)

// PluginID is the builtin plugin that contributes these tools.
const PluginID = "keel.testkit"

// ClockTool returns the current time as an ISO 8601 string.
type ClockTool struct{}

func (ClockTool) Name() string        { return "clock" }
func (ClockTool) Description() string { return "Return the current time as an ISO 8601 string." }
func (ClockTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}
func (ClockTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}
func (ClockTool) Execute(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	return agentcore.AgentToolResult{Content: agentcore.ContentList{
		agentcore.NewTextContent(time.Now().UTC().Format(time.RFC3339)),
	}}, nil
}

// EchoTool echoes its argument. KEEL_TOOL_DELAY_MS makes it sleep inside the
// tool call, which the crash-matrix tests use to kill the process mid-tool.
type EchoTool struct{}

func (EchoTool) Name() string        { return "echo" }
func (EchoTool) Description() string { return "Echo the given text back." }
func (EchoTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}},"required":["x"],"additionalProperties":false}`)
}
func (EchoTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}
func (EchoTool) Execute(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	if ms, _ := strconv.Atoi(os.Getenv("KEEL_TOOL_DELAY_MS")); ms > 0 {
		select {
		case <-ctx.Done():
			return agentcore.AgentToolResult{}, ctx.Err()
		case <-time.After(time.Duration(ms) * time.Millisecond):
		}
	}
	var in struct {
		X string `json:"x"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return agentcore.AgentToolResult{
			Content: agentcore.ContentList{agentcore.NewTextContent("invalid arguments: " + err.Error())},
		}, nil
	}
	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent(in.X)},
	}, nil
}
