package agent

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
)

// ClockTool returns the current time. It is the smallest possible real tool and
// gives the loop something deterministic to call in development.
type ClockTool struct{}

func (ClockTool) Name() string        { return "clock" }
func (ClockTool) Description() string { return "Return the current time as an ISO 8601 string." }
func (ClockTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}
func (ClockTool) ExecutionMode() agentcore.ToolExecutionMode { return agentcore.ToolExecutionParallel }
func (ClockTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent(time.Now().UTC().Format(time.RFC3339))},
	}, nil
}

// EchoTool echoes its argument. It is used by the fake provider and by E2E to
// exercise the tool round-trip without external dependencies.
type EchoTool struct{}

func (EchoTool) Name() string        { return "echo" }
func (EchoTool) Description() string { return "Echo the given text back." }
func (EchoTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}},"required":["x"],"additionalProperties":false}`)
}
func (EchoTool) ExecutionMode() agentcore.ToolExecutionMode { return agentcore.ToolExecutionParallel }
func (EchoTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	// E2E uses this knob to kill the process mid-tool; production leaves it unset.
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
