package toolsurface

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/mcp"
)

// MCPBackend is the narrow view of the MCP manager the aggregate tool needs.
// *mcp.Manager implements it. The model never sees per-server tool names: it
// lists, describes and calls through this one tool.
type MCPBackend interface {
	Servers() []*mcp.Server
	CallTool(ctx context.Context, server, tool string, args json.RawMessage) (mcp.CallToolResult, error)
}

var mcpToolActions = []string{ActionList, ActionDesc, ActionCall}

const mcpToolDescription = "MCP tool ecosystem. Actions: list (servers and their tools); " +
	"describe {server, tool} (input schema); call {server, tool, arguments}. " +
	"Tool names are never hardcoded as mcp__<server>__<tool>; server changes do not change the tool list."

func mcpSchema() json.RawMessage {
	return schema(`, "server":{"type":"string"}, "tool":{"type":"string"}, "arguments":{"type":"object","description":"Arguments passed to the MCP tool."}`)
}

func newMCPTool(backend MCPBackend) *actionTool {
	return &actionTool{
		name:        MCP,
		description: mcpToolDescription,
		schema:      mcpSchema(),
		actions:     mcpToolActions,
		run: func(ctx context.Context, action string, raw json.RawMessage) (agentcore.AgentToolResult, error) {
			switch action {
			case ActionList:
				return mcpList(backend)
			case ActionDesc:
				return mcpDescribe(backend, raw)
			case ActionCall:
				return mcpCall(ctx, backend, raw)
			default:
				return unknownAction(MCP, action, mcpToolActions)
			}
		},
	}
}

func mcpList(backend MCPBackend) (agentcore.AgentToolResult, error) {
	if backend == nil {
		return notWired(MCP, "no MCP service is configured in this deployment")
	}
	servers := make([]map[string]any, 0)
	for _, server := range backend.Servers() {
		description := ""
		if server.Entry.Config != nil {
			description = mcp.ServerField(server.Entry.Config, "description")
		}
		entry := map[string]any{
			"name":      server.Entry.Name,
			"connected": server.Err == nil,
		}
		if description != "" {
			entry["description"] = description
		}
		if server.Err != nil {
			entry["error"] = server.Err.Error()
		}
		tools := make([]map[string]string, 0, len(server.Tools))
		for _, tool := range server.Tools {
			item := map[string]string{"name": tool.Name}
			if tool.Title != "" {
				item["title"] = tool.Title
			}
			if tool.Description != "" {
				item["description"] = tool.Description
			}
			tools = append(tools, item)
		}
		entry["tools"] = tools
		servers = append(servers, entry)
	}
	document := map[string]any{"servers": servers}
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(marshalDocument(document))},
		StructuredContent: document,
	}, nil
}

func mcpDescribe(backend MCPBackend, raw json.RawMessage) (agentcore.AgentToolResult, error) {
	if backend == nil {
		return notWired(MCP, "no MCP service is configured in this deployment")
	}
	var input struct {
		Server string `json:"server"`
		Tool   string `json:"tool"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return invalidArguments(MCP, err.Error())
	}
	if input.Server == "" || input.Tool == "" {
		return invalidArguments(MCP, "describe requires server and tool")
	}
	for _, server := range backend.Servers() {
		if server.Entry.Name != input.Server {
			continue
		}
		if server.Err != nil {
			return notFound(MCP, fmt.Sprintf("server %q is not connected: %v", input.Server, server.Err))
		}
		for _, tool := range server.Tools {
			if tool.Name != input.Tool {
				continue
			}
			inputSchema := tool.InputSchema
			if normalized, err := mcp.NormalizeInputSchema(tool.InputSchema); err == nil {
				inputSchema = normalized
			}
			document := map[string]any{
				"server":      input.Server,
				"tool":        tool.Name,
				"title":       tool.Title,
				"description": tool.Description,
				"inputSchema": json.RawMessage(inputSchema),
			}
			return agentcore.AgentToolResult{
				Content:           agentcore.ContentList{agentcore.NewTextContent(marshalDocument(document))},
				StructuredContent: document,
			}, nil
		}
		return notFound(MCP, fmt.Sprintf("server %q has no tool %q", input.Server, input.Tool))
	}
	return notFound(MCP, fmt.Sprintf("no MCP server named %q", input.Server))
}

func mcpCall(ctx context.Context, backend MCPBackend, raw json.RawMessage) (agentcore.AgentToolResult, error) {
	if backend == nil {
		return notWired(MCP, "no MCP service is configured in this deployment")
	}
	var input struct {
		Server    string          `json:"server"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return invalidArguments(MCP, err.Error())
	}
	if input.Server == "" || input.Tool == "" {
		return invalidArguments(MCP, "call requires server and tool")
	}
	result, err := backend.CallTool(ctx, input.Server, input.Tool, input.Arguments)
	if err != nil {
		switch {
		case errors.Is(err, mcp.ErrServerNotFound), errors.Is(err, mcp.ErrToolNotFound):
			return notFound(MCP, err.Error())
		default:
			return ErrorResult(CodeBackendFailed, MCP, err.Error(), map[string]any{"server": input.Server, "tool": input.Tool})
		}
	}
	content := mcp.ToLLMContent(result)
	if result.IsError && len(content) == 0 {
		content = append(content, agentcore.NewTextContent(
			fmt.Sprintf("MCP tool %s/%s returned an error", input.Server, input.Tool),
		))
	}
	limited := mcp.LimitContent(content, nil)
	isError := result.IsError
	details := map[string]any{
		"server":  input.Server,
		"tool":    input.Tool,
		"isError": result.IsError,
	}
	if limited.FullOutputPath != "" {
		details["fullOutputPath"] = limited.FullOutputPath
	}
	return agentcore.AgentToolResult{
		Content:           limited.Content,
		Details:           details,
		StructuredContent: result,
		IsError:           &isError,
	}, nil
}

func marshalDocument(document map[string]any) string {
	encoded, err := json.Marshal(document)
	if err != nil {
		return fmt.Sprintf("%v", document)
	}
	return string(encoded)
}
