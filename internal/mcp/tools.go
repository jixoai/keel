// Behaviour ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi)
// and pi's MCP tool adaptation packages/coding-agent/src/extensions/mcp/tools.ts
// (MIT License); implemented independently in Go. Contract kept identical; see
// docs/UPSTREAM.md.
//
// This file adapts tools/list entries to agentcore.AgentTool: the model-facing
// name is mcp__<server>__<tool>, the input schema is normalized, results go
// through toLlmContent and the 20KB output limit, and the whole CallToolResult
// stays available as structured content for programmatic callers.

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jixoai/keel/internal/agentcore"
)

// MCPToolDetails is the Details payload of a tool adapted by AgentTools. It
// mirrors pi's McpToolDetails plus the isError flag: agentcore.AgentToolResult
// has no IsError field, so this is where a server-reported tool error is passed
// through without throwing away the model-facing content.
type MCPToolDetails struct {
	Server         string `json:"server"`
	Tool           string `json:"tool"`
	IsError        bool   `json:"isError,omitempty"`
	FullOutputPath string `json:"fullOutputPath,omitempty"`
}

// AgentTools lists the server's tools and adapts each one to an AgentTool.
//
// Tool names are CreateToolName(server, tool) so two servers can expose the
// same tool name without colliding and every name stays within the provider's
// length limit. The list call uses the client's default request timeout.
func (c *Client) AgentTools(server string) ([]agentcore.AgentTool, error) {
	tools, err := c.ListTools(context.Background())
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(tools))
	adapted := make([]agentcore.AgentTool, 0, len(tools))
	for _, tool := range tools {
		name := CreateToolName(server, tool.Name, func(candidate string) bool {
			return names[candidate]
		})
		names[name] = true
		schema, err := NormalizeInputSchema(tool.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("mcp: tool %q of server %q: %w", tool.Name, server, err)
		}
		adapted = append(adapted, &agentTool{
			client:      c,
			server:      server,
			tool:        tool,
			name:        name,
			description: toolDescription(server, tool),
			schema:      schema,
		})
	}
	return adapted, nil
}

// toolDescription follows pi: the trimmed description, else the title, else a
// generated label.
func toolDescription(server string, tool Tool) string {
	if description := strings.TrimSpace(tool.Description); description != "" {
		return description
	}
	if title := toolTitle(tool); title != "" {
		return title
	}
	return fmt.Sprintf("MCP tool %s from server %s", tool.Name, server)
}

// toolTitle is tool.title with annotations.title as the fallback, as in pi.
func toolTitle(tool Tool) string {
	if tool.Title != "" {
		return tool.Title
	}
	if len(tool.Annotations) > 0 {
		var annotations struct {
			Title string `json:"title"`
		}
		if err := json.Unmarshal(tool.Annotations, &annotations); err == nil {
			return annotations.Title
		}
	}
	return ""
}

// agentTool is one adapted MCP tool.
type agentTool struct {
	client      *Client
	server      string
	tool        Tool
	name        string
	description string
	schema      json.RawMessage
}

var _ agentcore.AgentTool = (*agentTool)(nil)

func (t *agentTool) Name() string            { return t.name }
func (t *agentTool) Description() string     { return t.description }
func (t *agentTool) Schema() json.RawMessage { return t.schema }

func (t *agentTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}

// Execute calls the MCP tool, converts its content for the model, truncates
// text over the 20KB limit, and returns the full CallToolResult as structured
// content. A server-reported isError is passed through in Details rather than
// as a Go error, so the model still sees the server's message (pi returns the
// result with isError set for the same reason).
func (t *agentTool) Execute(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	result, err := t.client.CallTool(ctx, t.tool.Name, args)
	if err != nil {
		return agentcore.AgentToolResult{}, err
	}
	content := ToLLMContent(result)
	if result.IsError && joinedText(content) == "" {
		content = append(content, agentcore.NewTextContent(
			fmt.Sprintf("MCP tool %s/%s returned an error", t.server, t.tool.Name),
		))
	}
	limited := LimitContent(content, nil)
	return agentcore.AgentToolResult{
		Content: limited.Content,
		Details: MCPToolDetails{
			Server:         t.server,
			Tool:           t.tool.Name,
			IsError:        result.IsError,
			FullOutputPath: limited.FullOutputPath,
		},
		StructuredContent: result,
	}, nil
}

// joinedText joins the text blocks the way pi's textOf does.
func joinedText(content []agentcore.Content) string {
	texts := make([]string, 0, len(content))
	for _, block := range content {
		switch typed := block.(type) {
		case agentcore.TextContent:
			texts = append(texts, typed.Text)
		case *agentcore.TextContent:
			texts = append(texts, typed.Text)
		}
	}
	return strings.Join(texts, "\n")
}
