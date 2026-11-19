package toolsurface

import (
	"bytes"
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
	// CatalogueSnapshot returns the catalogue frozen for one attempt (empty id =
	// live). The aggregate tool reads it so a republish never changes a running
	// attempt's tools.
	CatalogueSnapshot(attemptID string) []*mcp.Server
	CallTool(ctx context.Context, server, tool string, args json.RawMessage) (mcp.CallToolResult, error)
	CallToolWithOptions(ctx context.Context, server, tool string, args json.RawMessage, options mcp.CallOptions) (mcp.CallToolResult, error)
}

// MCPProgressPublisher publishes one notifications/progress frame of a tool
// call. It is the host's transient channel (the PartialHub); nil means this
// deployment shows no progress. Implementations must not block: progress is
// advisory and dropping a frame is always safe.
type MCPProgressPublisher func(scope agentcore.ToolRunScope, toolCallID string, progress mcp.ProgressNotification)

var mcpToolActions = []string{ActionList, ActionDesc, ActionCall}

const mcpToolDescription = "MCP tool ecosystem. Actions: list (servers and their tools); " +
	"describe {server, tool} (input schema); call {server, tool, arguments}. " +
	"Tool names are never hardcoded as mcp__<server>__<tool>; server changes do not change the tool list."

func mcpSchema() json.RawMessage {
	return schema(`, "server":{"type":"string"}, "tool":{"type":"string"}, "arguments":{"type":"object","description":"Arguments passed to the MCP tool."}`)
}

func newMCPTool(backend MCPBackend, progress MCPProgressPublisher) *actionTool {
	return &actionTool{
		name:        MCP,
		description: mcpToolDescription,
		schema:      mcpSchema(),
		actions:     mcpToolActions,
		run: func(ctx context.Context, action string, raw json.RawMessage) (agentcore.AgentToolResult, error) {
			switch action {
			case ActionList:
				return mcpList(ctx, backend)
			case ActionDesc:
				return mcpDescribe(ctx, backend, raw)
			case ActionCall:
				return mcpCall(ctx, backend, progress, raw)
			default:
				return unknownAction(MCP, action, mcpToolActions)
			}
		},
	}
}

// mcpServers reads the attempt's frozen catalogue (live outside a run).
func mcpServers(ctx context.Context, backend MCPBackend) []*mcp.Server {
	if backend == nil {
		return nil
	}
	return backend.CatalogueSnapshot(agentcore.MCPAttemptFromContext(ctx))
}

func mcpList(ctx context.Context, backend MCPBackend) (agentcore.AgentToolResult, error) {
	if backend == nil {
		return notWired(MCP, "no MCP service is configured in this deployment")
	}
	servers := mcpServers(ctx, backend)
	catalogue := make([]map[string]any, 0, len(servers))
	for _, server := range servers {
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
		// Only model-visible tools are listed. An app-only tool (visibility
		// without "model") is part of the app catalogue, not of the model's
		// surface.
		for _, tool := range server.ModelTools() {
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
		catalogue = append(catalogue, entry)
	}
	document := map[string]any{"servers": catalogue}
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(marshalDocument(document))},
		StructuredContent: document,
	}, nil
}

func mcpDescribe(ctx context.Context, backend MCPBackend, raw json.RawMessage) (agentcore.AgentToolResult, error) {
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
	for _, server := range mcpServers(ctx, backend) {
		if server.Entry.Name != input.Server {
			continue
		}
		if server.Err != nil {
			// The server exists but its connection failed: a backend failure, not
			// a missing name. `call` answers the same fact with the same code.
			return ErrorResult(CodeBackendFailed, MCP,
				fmt.Sprintf("server %q is not connected: %v", input.Server, server.Err),
				map[string]any{"server": input.Server, "tool": input.Tool})
		}
		for _, tool := range server.ModelTools() {
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

func mcpCall(ctx context.Context, backend MCPBackend, progress MCPProgressPublisher, raw json.RawMessage) (agentcore.AgentToolResult, error) {
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
	// The schema declares arguments as an object; enforce it here so a backend
	// never receives an array/string/number in its place. Missing and null both
	// mean "no arguments" and travel as the empty object.
	arguments, err := mcpCallArguments(input.Arguments)
	if err != nil {
		return invalidArguments(MCP, err.Error())
	}
	// Visibility is enforced before the backend sees the name: an app-only tool
	// must not be reachable through the model's aggregate tool.
	tool, server, err := lookupModelTool(mcpServers(ctx, backend), input.Server, input.Tool)
	if err != nil {
		return mcpCallFailure(err, input.Server, input.Tool)
	}
	result, err := backend.CallToolWithOptions(ctx, input.Server, input.Tool, arguments, mcpCallOptions(ctx, progress))
	if err != nil {
		return mcpCallFailure(err, input.Server, input.Tool)
	}
	content := mcp.ToLLMContent(result)
	if result.IsError && len(content) == 0 {
		content = append(content, agentcore.NewTextContent(
			fmt.Sprintf("MCP tool %s/%s returned an error", input.Server, input.Tool),
		))
	}
	limited := mcp.LimitContent(content, nil)
	isError := result.IsError
	details := mcp.MCPToolDetails{
		Server:         input.Server,
		Tool:           input.Tool,
		IsError:        result.IsError,
		FullOutputPath: limited.FullOutputPath,
	}
	// The app binding and the bounded structured content ride in Details: they
	// are durable (tool.result) but never enter the model context.
	if binding, ok := mcp.AppBindingFor(server, tool); ok {
		details.App = &binding
	}
	if structured, truncated := mcp.BoundStructuredContent(result.StructuredContent, mcp.StructuredContentMaxBytes); len(structured) > 0 {
		details.StructuredContent = structured
		details.StructuredContentTruncated = truncated
	}
	return agentcore.AgentToolResult{
		Content:           limited.Content,
		Details:           details,
		StructuredContent: result,
		IsError:           &isError,
	}, nil
}

// mcpCallOptions binds the call's progress surface. The token is the toolCallId
// the executor placed on the context: it is already unique per call, and the
// client further disambiguates two concurrent calls that name the same id. The
// publisher is bound to this call's (tenant, session) scope, so a frame can
// only reach the session that issued the call.
func mcpCallOptions(ctx context.Context, publisher MCPProgressPublisher) mcp.CallOptions {
	callID := agentcore.ToolCallIDFromContext(ctx)
	attemptID := agentcore.MCPAttemptFromContext(ctx)
	if callID == "" && attemptID == "" {
		return mcp.CallOptions{}
	}
	options := mcp.CallOptions{AttemptID: attemptID}
	if callID == "" {
		return options
	}
	options.ProgressToken = callID
	if publisher == nil {
		return options
	}
	if scope, ok := agentcore.ToolRunScopeFromContext(ctx); ok {
		options.OnProgress = func(progress mcp.ProgressNotification) {
			publisher(scope, callID, progress)
		}
	}
	return options
}

// lookupModelTool resolves a model-visible tool. It distinguishes the three
// facts the call path reports: no such server, the server is configured but not
// connected, and no such model-visible tool (which also covers an app-only tool
// the model must not reach).
func lookupModelTool(servers []*mcp.Server, serverName, toolName string) (mcp.Tool, *mcp.Server, error) {
	for _, server := range servers {
		if server.Entry.Name != serverName {
			continue
		}
		if server.Err != nil {
			return mcp.Tool{}, server, fmt.Errorf("mcp: server %q: %w", serverName, server.Err)
		}
		tool, ok := server.ModelTool(toolName)
		if !ok {
			return mcp.Tool{}, server, fmt.Errorf("%w: %s/%s", mcp.ErrToolNotFound, serverName, toolName)
		}
		return tool, server, nil
	}
	return mcp.Tool{}, nil, fmt.Errorf("%w: %q", mcp.ErrServerNotFound, serverName)
}

// mcpCallFailure maps a call/lookup failure onto the aggregate tool's typed
// codes: a missing name pair is not_found, anything else is backend_error.
func mcpCallFailure(err error, server, tool string) (agentcore.AgentToolResult, error) {
	switch {
	case errors.Is(err, mcp.ErrServerNotFound), errors.Is(err, mcp.ErrToolNotFound):
		return notFound(MCP, err.Error())
	default:
		return ErrorResult(CodeBackendFailed, MCP, err.Error(), map[string]any{"server": server, "tool": tool})
	}
}

// mcpCallArguments enforces the tool schema's object contract on the call
// arguments. Absent and null are the empty object ("no arguments"); anything
// else that is not a JSON object is refused with a readable message instead of
// being forwarded to the backend.
func mcpCallArguments(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage("{}"), nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil {
		return nil, fmt.Errorf("arguments must be a JSON object: %v", err)
	}
	if object == nil {
		return json.RawMessage("{}"), nil
	}
	return trimmed, nil
}

func marshalDocument(document map[string]any) string {
	encoded, err := json.Marshal(document)
	if err != nil {
		return fmt.Sprintf("%v", document)
	}
	return string(encoded)
}
