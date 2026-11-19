package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
)

// TestToolResultDetailsNeverReachTheWire pins the boundary P1.4 relies on: the
// durable tool.result payload carries Details (the MCP app binding and the
// bounded structured content), but neither wire encoder copies them into a
// model request. Only Content is model-facing.
func TestToolResultDetailsNeverReachTheWire(t *testing.T) {
	message := agentcore.ToolResultMessage{
		RoleField:  agentcore.RoleToolResult,
		ToolCallID: "call-1",
		ToolName:   "mcp",
		Content:    agentcore.ContentList{agentcore.NewTextContent("visible result text")},
		Details: map[string]any{
			"app": map[string]any{
				"appId":       "mcpapp_details_marker",
				"resourceUri": "ui://docs/panel",
			},
			"structuredContent": map[string]any{"secret": "structured-details-marker"},
		},
	}

	openAI, err := json.Marshal(encodeOpenAIMessage(message))
	if err != nil {
		t.Fatalf("encode openai: %v", err)
	}
	anthropic, err := json.Marshal(encodeAnthropicMessage(message))
	if err != nil {
		t.Fatalf("encode anthropic: %v", err)
	}
	for name, wire := range map[string]string{"openai": string(openAI), "anthropic": string(anthropic)} {
		for _, forbidden := range []string{"mcpapp_details_marker", "structured-details-marker", "ui://docs/panel", "structuredContent"} {
			if strings.Contains(wire, forbidden) {
				t.Fatalf("%s wire leaked %q: %s", name, forbidden, wire)
			}
		}
		if !strings.Contains(wire, "visible result text") {
			t.Fatalf("%s wire dropped the model-facing content: %s", name, wire)
		}
	}
}
