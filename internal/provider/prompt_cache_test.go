package provider

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/promptcache"
)

type cacheTestTool struct {
	name   string
	schema string
}

func (t cacheTestTool) Name() string            { return t.name }
func (t cacheTestTool) Description() string     { return "tool " + t.name }
func (t cacheTestTool) Schema() json.RawMessage { return json.RawMessage(t.schema) }
func (t cacheTestTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}
func (t cacheTestTool) Execute(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	return agentcore.AgentToolResult{}, nil
}

func cacheTools() []agentcore.AgentTool {
	return []agentcore.AgentTool{
		cacheTestTool{name: "bash", schema: `{"type":"object","properties":{"cmd":{"type":"string"}}}`},
		cacheTestTool{name: "read", schema: `{"type":"object","properties":{"path":{"type":"string"}}}`},
	}
}

// TestAnthropicPromptCacheBoundaryMarksTheLastTool: the breakpoint sits at the
// end of the system/tool prefix, so the transcript after it can be pruned
// without invalidating the cached prefix.
func TestAnthropicPromptCacheBoundaryMarksTheLastTool(t *testing.T) {
	req := CompletionRequest{
		Model: "claude-sonnet-4-5",
		Context: LlmContext{
			SystemPrompt: "system",
			Messages:     agentcore.MessageList{agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("hi")}}},
			Tools:        cacheTools(),
		},
		Config: StreamConfig{PromptCache: true},
	}
	body := anthropicWireBody(t, req)
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools = %#v", body["tools"])
	}
	first, _ := tools[0].(map[string]any)
	if _, ok := first["cache_control"]; ok {
		t.Fatalf("an earlier tool carries a breakpoint: %#v", first)
	}
	last, _ := tools[1].(map[string]any)
	if _, ok := last["cache_control"].(map[string]any); !ok {
		t.Fatalf("last tool = %#v, want cache_control", last)
	}
	// The system prompt stays a plain string when a tool anchor exists.
	if body["system"] != "system" {
		t.Fatalf("system = %#v, want the unchanged string", body["system"])
	}
}

// TestAnthropicPromptCacheBoundaryIsOptIn: without the request bit the wire
// body is unchanged (a gateway that does not implement the field never sees it).
func TestAnthropicPromptCacheBoundaryIsOptIn(t *testing.T) {
	req := CompletionRequest{
		Model: "claude-sonnet-4-5",
		Context: LlmContext{
			SystemPrompt: "system",
			Messages:     agentcore.MessageList{agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("hi")}}},
			Tools:        cacheTools(),
		},
	}
	body := anthropicWireBody(t, req)
	tools, _ := body["tools"].([]any)
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if _, ok := tool["cache_control"]; ok {
			t.Fatalf("unrequested breakpoint on %#v", tool)
		}
	}
	if body["system"] != "system" {
		t.Fatalf("system = %#v", body["system"])
	}
}

// TestAnthropicPromptCacheBoundaryWithoutTools: with no tool anchor the system
// prompt becomes the one cacheable content block.
func TestAnthropicPromptCacheBoundaryWithoutTools(t *testing.T) {
	req := CompletionRequest{
		Model: "claude-sonnet-4-5",
		Context: LlmContext{
			SystemPrompt: "system",
			Messages:     agentcore.MessageList{agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("hi")}}},
		},
		Config: StreamConfig{PromptCache: true},
	}
	body := anthropicWireBody(t, req)
	blocks, ok := body["system"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("system = %#v, want one content block", body["system"])
	}
	block, _ := blocks[0].(map[string]any)
	if block["text"] != "system" {
		t.Fatalf("block = %#v", block)
	}
	if _, ok := block["cache_control"].(map[string]any); !ok {
		t.Fatalf("block = %#v, want cache_control", block)
	}
}

// TestCanonicalToolsProduceByteIdenticalAnthropicBodies pins the other half of
// keel.prompt-cache: registration order must not change the request bytes.
func TestCanonicalToolsProduceByteIdenticalAnthropicBodies(t *testing.T) {
	encode := func(tools []agentcore.AgentTool) []byte {
		req := CompletionRequest{
			Model: "claude-sonnet-4-5",
			Context: LlmContext{
				SystemPrompt: "system",
				Messages:     agentcore.MessageList{agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent("hi")}}},
				Tools:        promptcache.CanonicalTools(tools),
			},
			Config: StreamConfig{PromptCache: true},
		}
		raw, err := encodeAnthropicRequest(req, nil)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return raw
	}
	forward := encode(cacheTools())
	reversed := encode([]agentcore.AgentTool{cacheTools()[1], cacheTools()[0]})
	if string(forward) != string(reversed) {
		t.Fatalf("tool order changed the prefix bytes:\n%s\n%s", forward, reversed)
	}
}
