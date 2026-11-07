// ToolProvider: the caller-side view of an external, stateless tool provider.
//
// This is one of the two surfaces of the protocol and it is deliberately the
// MCP-shaped one: tools/list, tools/call, content blocks, structuredContent,
// isError are MCP's, verbatim, because the ecosystem already speaks them and
// this project reuses MCP semantics for tools instead of inventing a third
// tool protocol. It is valid only on tool_provider connections - an
// AgentBackend connection rejects it, and the agent method set is never
// expressed as tools (see agent.go).
//
// What the gateway adds around MCP's shapes is the contract: a bounded page
// walk, validated results, and the rule that a tool result goes back to the
// caller to be committed through the contract kernel - the provider never
// touches the ledger.

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
)

// maxToolListPages bounds a tools/list walk so a provider that keeps returning
// cursors cannot hang the caller (same bound as internal/mcp).
const maxToolListPages = 1_000

// ToolDefinition is one entry of a tools/list result.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ListToolsResult is one page of tools/list.
type ListToolsResult struct {
	Tools      []ToolDefinition `json:"tools"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

// ContentBlock is one block of a tool result. Type selects which fields apply:
// "text" uses Text, "image"/"audio" use Data and MimeType, "resource_link" uses
// URI and Name. Unknown kinds are passed through untouched.
type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
}

// ToolResult is a tools/call result. IsError means the tool itself reported a
// failure; it is not a transport or protocol failure.
type ToolResult struct {
	Content           []ContentBlock  `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// callToolParams is the tools/call payload.
type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ToolProvider is the caller-side contract of a stateless external tool
// provider. CallTool's result is JSON the caller may commit; the provider has
// no other channel into Keel.
type ToolProvider interface {
	ListTools(ctx context.Context) ([]ToolDefinition, error)
	CallTool(ctx context.Context, name string, arguments json.RawMessage) (ToolResult, error)
}

// ToolClient implements ToolProvider over a Conn.
type ToolClient struct {
	conn *Conn
}

// NewToolProvider returns the tool surface of a connection. Every call checks
// that the connection really is a tool_provider, so a wiring mistake fails
// fast instead of sending tools/* to an agent runtime.
func NewToolProvider(conn *Conn) *ToolClient { return &ToolClient{conn: conn} }

var _ ToolProvider = (*ToolClient)(nil)

// ListTools returns the full catalogue, following nextCursor through all pages.
func (t *ToolClient) ListTools(ctx context.Context) ([]ToolDefinition, error) {
	if err := t.requireKind("ListTools"); err != nil {
		return nil, err
	}
	var tools []ToolDefinition
	seen := make(map[string]bool)
	cursor := ""
	for page := 0; page < maxToolListPages; page++ {
		result, err := t.ListToolsPage(ctx, cursor)
		if err != nil {
			return nil, err
		}
		tools = append(tools, result.Tools...)
		if result.NextCursor == "" {
			return tools, nil
		}
		if seen[result.NextCursor] {
			return nil, fmt.Errorf("gateway: tools/list repeated cursor %q", result.NextCursor)
		}
		seen[result.NextCursor] = true
		cursor = result.NextCursor
	}
	return nil, fmt.Errorf("gateway: tools/list exceeded %d pages", maxToolListPages)
}

// ListToolsPage requests one page. An empty cursor asks for the first page and
// omits the params object entirely.
func (t *ToolClient) ListToolsPage(ctx context.Context, cursor string) (ListToolsResult, error) {
	if err := t.requireKind("ListTools"); err != nil {
		return ListToolsResult{}, err
	}
	var params json.RawMessage
	if cursor != "" {
		encoded, err := json.Marshal(struct {
			Cursor string `json:"cursor"`
		}{Cursor: cursor})
		if err != nil {
			return ListToolsResult{}, err
		}
		params = encoded
	}
	raw, err := t.conn.doRequest(ctx, MethodToolsList, params, requestOptions{})
	if err != nil {
		return ListToolsResult{}, err
	}
	return validateListToolsResult(raw)
}

// CallTool invokes one tool. A nil arguments value is sent without an
// arguments field.
func (t *ToolClient) CallTool(ctx context.Context, name string, arguments json.RawMessage) (ToolResult, error) {
	if err := t.requireKind("CallTool"); err != nil {
		return ToolResult{}, err
	}
	if name == "" {
		return ToolResult{}, fmt.Errorf("gateway: tools/call requires a tool name")
	}
	params, err := json.Marshal(callToolParams{Name: name, Arguments: arguments})
	if err != nil {
		return ToolResult{}, fmt.Errorf("gateway: encode tools/call params: %w", err)
	}
	raw, err := t.conn.doRequest(ctx, MethodToolsCall, params, requestOptions{target: name})
	if err != nil {
		return ToolResult{}, err
	}
	return validateCallToolResult(raw)
}

func (t *ToolClient) requireKind(call string) error {
	got := t.conn.Provider().Type
	if got != ProviderTypeToolProvider {
		return &ProviderTypeError{Call: call, Want: ProviderTypeToolProvider, Got: got}
	}
	return nil
}

// validateListToolsResult checks the page shape and each entry: a non-empty
// string name and an object input schema.
func validateListToolsResult(data json.RawMessage) (ListToolsResult, error) {
	invalid := &ProtocolError{Code: CodeInvalidRequest, Message: "Invalid tools/list result"}
	invalidEntry := &ProtocolError{Code: CodeInvalidRequest, Message: "Invalid entry in tools/list result"}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return ListToolsResult{}, invalid
	}
	toolsRaw, ok := fields["tools"]
	if !ok || isJSONNull(toolsRaw) {
		return ListToolsResult{}, invalid
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(toolsRaw, &entries); err != nil || entries == nil {
		return ListToolsResult{}, invalid
	}
	tools := make([]ToolDefinition, 0, len(entries))
	for _, entry := range entries {
		var entryFields map[string]json.RawMessage
		if err := json.Unmarshal(entry, &entryFields); err != nil || entryFields == nil {
			return ListToolsResult{}, invalidEntry
		}
		var name string
		if err := json.Unmarshal(entryFields["name"], &name); err != nil || name == "" {
			return ListToolsResult{}, invalidEntry
		}
		var schema map[string]json.RawMessage
		rawSchema, hasSchema := entryFields["inputSchema"]
		if !hasSchema || isJSONNull(rawSchema) || json.Unmarshal(rawSchema, &schema) != nil || schema == nil {
			return ListToolsResult{}, invalidEntry
		}
		var tool ToolDefinition
		if err := json.Unmarshal(entry, &tool); err != nil {
			return ListToolsResult{}, invalidEntry
		}
		tools = append(tools, tool)
	}
	result := ListToolsResult{Tools: tools}
	if raw, ok := fields["nextCursor"]; ok && !isJSONNull(raw) {
		var cursor string
		if err := json.Unmarshal(raw, &cursor); err != nil {
			return ListToolsResult{}, &ProtocolError{Code: CodeInvalidRequest, Message: "Invalid tools/list cursor"}
		}
		result.NextCursor = cursor
	}
	return result, nil
}

// validateCallToolResult checks content is an array when present (missing
// content defaults to an empty list) and keeps any JSON value as structured
// content.
func validateCallToolResult(data json.RawMessage) (ToolResult, error) {
	invalid := &ProtocolError{Code: CodeInvalidRequest, Message: "Invalid tools/call result"}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return ToolResult{}, invalid
	}
	result := ToolResult{Content: []ContentBlock{}}
	if raw, ok := fields["content"]; ok {
		if isJSONNull(raw) {
			return ToolResult{}, invalid
		}
		var content []ContentBlock
		if err := json.Unmarshal(raw, &content); err != nil || content == nil {
			return ToolResult{}, invalid
		}
		result.Content = content
	}
	if raw, ok := fields["structuredContent"]; ok {
		result.StructuredContent = copyRaw(raw)
	}
	if raw, ok := fields["isError"]; ok && !isJSONNull(raw) {
		if err := json.Unmarshal(raw, &result.IsError); err != nil {
			return ToolResult{}, invalid
		}
	}
	return result, nil
}
