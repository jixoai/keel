// Behaviour ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/src/client.ts (MIT License); implemented independently in Go.
// Contract kept identical; see docs/UPSTREAM.md.
//
// Client is the JSON-RPC endpoint of an MCP connection: it starts a Transport,
// negotiates initialize/notifications/initialized, correlates responses by
// request id, enforces request timeouts, and exposes tools/list and tools/call.
// Server-initiated requests are answered with "method not found"; responses
// with an id this client never issued are ignored. Streamable HTTP, OAuth, and
// mcp.json configuration are deliberately out of scope for this batch.

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// DefaultRequestTimeout is how long a request waits for its response when
// ClientOptions.RequestTimeout is zero (the task baseline; pi defaults to 30s).
const DefaultRequestTimeout = 60 * time.Second

// maxListPages bounds pagination so a server that keeps returning cursors
// cannot hang the client forever, like pi's MAX_LIST_PAGES.
const maxListPages = 1_000

// serverRequestTimeout bounds the reply to a server-initiated request.
const serverRequestTimeout = 5 * time.Second

// ClientState is the connection lifecycle, named as in pi.
type ClientState string

const (
	ClientIdle       ClientState = "idle"
	ClientConnecting ClientState = "connecting"
	ClientConnected  ClientState = "connected"
	ClientClosed     ClientState = "closed"
)

// ClientOptions configures a Client. Name and Version identify it to the
// server (clientInfo).
type ClientOptions struct {
	Name    string
	Version string
	Title   string
	// Capabilities are sent in initialize. Roots is not implied: unlike pi this
	// batch has no roots handler, so callers must opt in explicitly.
	Capabilities ClientCapabilities
	// ProtocolVersion defaults to LatestProtocolVersion.
	ProtocolVersion string
	// RequestTimeout bounds each request. Zero uses DefaultRequestTimeout; a
	// negative value disables the timer (the context still applies).
	RequestTimeout time.Duration
}

// pendingCall is one in-flight request. The channel has capacity one so the
// receive loop never blocks delivering its response.
type pendingCall struct {
	ch chan callResult
}

type callResult struct {
	result json.RawMessage
	err    error
}

// Client is a connected (or connectable) MCP client. It is safe for concurrent
// use: requests may be issued from several goroutines.
type Client struct {
	options ClientOptions

	mu        sync.Mutex
	state     ClientState
	transport Transport
	nextID    int64
	pending   map[string]*pendingCall
	closedCh  chan struct{}
	closeOnce sync.Once

	protocolVersion    string
	serverInfo         Implementation
	serverCapabilities ServerCapabilities
	instructions       string
}

// NewClient returns a client in the idle state.
func NewClient(options ClientOptions) *Client {
	if options.ProtocolVersion == "" {
		options.ProtocolVersion = LatestProtocolVersion
	}
	if options.RequestTimeout == 0 {
		options.RequestTimeout = DefaultRequestTimeout
	}
	return &Client{
		options:  options,
		state:    ClientIdle,
		pending:  make(map[string]*pendingCall),
		closedCh: make(chan struct{}),
	}
}

// State reports the connection lifecycle.
func (c *Client) State() ClientState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// ProtocolVersion is the version the server selected, empty before Connect.
func (c *Client) ProtocolVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.protocolVersion
}

// ServerInfo is the server's identity from initialize, zero before Connect.
func (c *Client) ServerInfo() Implementation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serverInfo
}

// ServerCapabilities is what the server said it supports, zero before Connect.
func (c *Client) ServerCapabilities() ServerCapabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serverCapabilities
}

// Instructions is the optional initialize instructions, empty if absent.
func (c *Client) Instructions() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.instructions
}

// Connect starts transport, performs initialize and sends
// notifications/initialized. On any failure the client is closed.
func (c *Client) Connect(ctx context.Context, transport Transport) (*InitializeResult, error) {
	if transport == nil {
		return nil, fmt.Errorf("mcp: connect: nil transport")
	}
	c.mu.Lock()
	if c.state != ClientIdle {
		state := c.state
		c.mu.Unlock()
		return nil, fmt.Errorf("mcp: cannot connect client in %s state", state)
	}
	c.state = ClientConnecting
	c.transport = transport
	c.mu.Unlock()

	if err := transport.Start(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	go c.receiveLoop(transport)

	params, err := json.Marshal(struct {
		ProtocolVersion string             `json:"protocolVersion"`
		Capabilities    ClientCapabilities `json:"capabilities"`
		ClientInfo      Implementation     `json:"clientInfo"`
	}{
		ProtocolVersion: c.options.ProtocolVersion,
		Capabilities:    c.options.Capabilities,
		ClientInfo: Implementation{
			Name:    c.options.Name,
			Version: c.options.Version,
			Title:   c.options.Title,
		},
	})
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("mcp: encode initialize: %w", err)
	}
	raw, err := c.request(ctx, "initialize", params, true)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	result, err := validateInitializeResult(raw)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	if !isSupportedProtocolVersion(result.ProtocolVersion) {
		_ = c.Close()
		return nil, fmt.Errorf("mcp: server selected unsupported protocol version %s", result.ProtocolVersion)
	}
	// Transports that echo the version (Streamable HTTP's MCP-Protocol-Version
	// header) learn it here, before the initialized notification opens the GET
	// stream, like pi's transport.setProtocolVersion.
	if setter, ok := transport.(interface{ SetProtocolVersion(string) }); ok {
		setter.SetProtocolVersion(result.ProtocolVersion)
	}

	c.mu.Lock()
	c.protocolVersion = result.ProtocolVersion
	c.serverInfo = result.ServerInfo
	c.serverCapabilities = result.Capabilities
	c.instructions = result.Instructions
	c.mu.Unlock()

	if err := c.notify(ctx, "notifications/initialized", nil, true); err != nil {
		_ = c.Close()
		return nil, err
	}

	c.mu.Lock()
	if c.state != ClientConnecting {
		c.mu.Unlock()
		return nil, &ConnectionClosedError{Message: "MCP client closed during initialize"}
	}
	c.state = ClientConnected
	c.mu.Unlock()
	return &result, nil
}

// ListTools returns every tool, following nextCursor through all pages, like
// pi's listTools. Servers that end pagination with null or "" are accepted.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var tools []Tool
	seen := make(map[string]bool)
	cursor := ""
	for page := 0; page < maxListPages; page++ {
		result, err := c.ListToolsPage(ctx, cursor)
		if err != nil {
			return nil, err
		}
		tools = append(tools, result.Tools...)
		if result.NextCursor == "" {
			return tools, nil
		}
		if seen[result.NextCursor] {
			return nil, fmt.Errorf("mcp: tools/list returned duplicate cursor: %s", result.NextCursor)
		}
		seen[result.NextCursor] = true
		cursor = result.NextCursor
	}
	return nil, fmt.Errorf("mcp: tools/list exceeded %d pages", maxListPages)
}

// ListToolsPage requests one page of tools starting at cursor; an empty cursor
// asks for the first page and omits the params object entirely, like pi.
func (c *Client) ListToolsPage(ctx context.Context, cursor string) (ListToolsResult, error) {
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
	raw, err := c.request(ctx, "tools/list", params, false)
	if err != nil {
		return ListToolsResult{}, err
	}
	return validateListToolsResult(raw)
}

// CallTool invokes one tool. A nil or empty args is sent without an arguments
// field, like pi's callTool(name, undefined).
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (CallToolResult, error) {
	params, err := json.Marshal(struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}{Name: name, Arguments: args})
	if err != nil {
		return CallToolResult{}, fmt.Errorf("mcp: encode tools/call params: %w", err)
	}
	raw, err := c.request(ctx, "tools/call", params, false)
	if err != nil {
		return CallToolResult{}, err
	}
	return validateCallToolResult(raw)
}

// Close rejects in-flight requests, closes the transport, and marks the client
// closed. It is idempotent.
func (c *Client) Close() error {
	c.mu.Lock()
	transport := c.transport
	c.transport = nil
	c.mu.Unlock()
	c.markClosed(&ConnectionClosedError{})
	if transport != nil {
		return transport.Close()
	}
	return nil
}

// request sends one request and waits for its response, its timeout, the
// context, or the connection closing.
func (c *Client) request(ctx context.Context, method string, params json.RawMessage, allowConnecting bool) (json.RawMessage, error) {
	c.mu.Lock()
	if c.state != ClientConnected && !(allowConnecting && c.state == ClientConnecting) {
		state := c.state
		c.mu.Unlock()
		return nil, &ConnectionClosedError{Message: "MCP client is " + string(state)}
	}
	transport := c.transport
	if transport == nil {
		c.mu.Unlock()
		return nil, &ConnectionClosedError{}
	}
	c.nextID++
	id := NumberID(c.nextID)
	call := &pendingCall{ch: make(chan callResult, 1)}
	c.pending[id.Key()] = call
	c.mu.Unlock()

	message := &Request{JSONRPC: JSONRPCVersion, ID: id, Method: method, Params: params}
	if err := transport.Send(ctx, message); err != nil {
		c.removePending(id)
		return nil, err
	}

	var timer *time.Timer
	var timeoutCh <-chan time.Time
	if c.options.RequestTimeout > 0 {
		timer = time.NewTimer(c.options.RequestTimeout)
		timeoutCh = timer.C
		defer timer.Stop()
	}

	select {
	case result := <-call.ch:
		return result.result, result.err
	case <-ctx.Done():
		c.removePending(id)
		return nil, ctx.Err()
	case <-timeoutCh:
		c.removePending(id)
		return nil, &TimeoutError{Timeout: c.options.RequestTimeout}
	case <-c.closedCh:
		select {
		case result := <-call.ch:
			return result.result, result.err
		default:
		}
		c.removePending(id)
		return nil, &ConnectionClosedError{}
	}
}

// notify sends a notification without waiting for a response.
func (c *Client) notify(ctx context.Context, method string, params json.RawMessage, allowConnecting bool) error {
	c.mu.Lock()
	if c.state != ClientConnected && !(allowConnecting && c.state == ClientConnecting) {
		state := c.state
		c.mu.Unlock()
		return &ConnectionClosedError{Message: "MCP client is " + string(state)}
	}
	transport := c.transport
	c.mu.Unlock()
	if transport == nil {
		return &ConnectionClosedError{}
	}
	return transport.Send(ctx, &Notification{JSONRPC: JSONRPCVersion, Method: method, Params: params})
}

// receiveLoop dispatches server messages until the transport closes.
func (c *Client) receiveLoop(transport Transport) {
	for {
		message, err := transport.Receive(context.Background())
		if err != nil {
			c.markClosed(&ConnectionClosedError{})
			return
		}
		switch typed := message.(type) {
		case *SuccessResponse:
			c.deliver(typed.ID, callResult{result: typed.Result})
		case *ErrorResponse:
			c.deliver(typed.ID, callResult{err: &McpError{
				Code:    typed.Error.Code,
				Message: typed.Error.Message,
				Data:    typed.Error.Data,
			}})
		case *Request:
			c.answerServerRequest(transport, typed)
		}
		// Notifications are not consumed by this batch; unknown message kinds
		// cannot come out of ParseMessage.
	}
}

// deliver resolves a pending call. A response whose id this client never
// issued is ignored, as are responses for calls that already timed out.
func (c *Client) deliver(id ID, result callResult) {
	key := id.Key()
	c.mu.Lock()
	call := c.pending[key]
	if call != nil {
		delete(c.pending, key)
	}
	c.mu.Unlock()
	if call == nil {
		return
	}
	call.ch <- result
}

// answerServerRequest replies "method not found": this batch supports no
// server-initiated requests (roots, sampling, elicitation, ping are future
// work), and the spec requires a response rather than silence.
func (c *Client) answerServerRequest(transport Transport, request *Request) {
	ctx, cancel := context.WithTimeout(context.Background(), serverRequestTimeout)
	defer cancel()
	_ = transport.Send(ctx, &ErrorResponse{
		JSONRPC: JSONRPCVersion,
		ID:      request.ID,
		Error: ErrorObject{
			Code:    CodeMethodNotFound,
			Message: "Method not found: " + request.Method,
		},
	})
}

func (c *Client) removePending(id ID) {
	c.mu.Lock()
	delete(c.pending, id.Key())
	c.mu.Unlock()
}

// markClosed rejects every pending call and flips the state. It fires once.
func (c *Client) markClosed(err error) {
	c.mu.Lock()
	if c.state == ClientClosed {
		c.mu.Unlock()
		return
	}
	c.state = ClientClosed
	pending := c.pending
	c.pending = make(map[string]*pendingCall)
	c.closeOnce.Do(func() { close(c.closedCh) })
	c.mu.Unlock()
	for _, call := range pending {
		call.ch <- callResult{err: err}
	}
}

// validateInitializeResult checks the required initialize fields, then decodes
// them. pi rejects a missing/invalid protocolVersion, capabilities, serverInfo,
// name, version, or a non-string instructions.
func validateInitializeResult(data json.RawMessage) (InitializeResult, error) {
	invalid := &McpError{Code: CodeInvalidRequest, Message: "Invalid MCP initialize result"}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return InitializeResult{}, invalid
	}
	var version string
	if err := json.Unmarshal(fields["protocolVersion"], &version); err != nil {
		return InitializeResult{}, invalid
	}
	var capabilities map[string]json.RawMessage
	if raw, ok := fields["capabilities"]; !ok || isJSONNull(raw) || json.Unmarshal(raw, &capabilities) != nil || capabilities == nil {
		return InitializeResult{}, invalid
	}
	var serverInfo map[string]json.RawMessage
	if raw, ok := fields["serverInfo"]; !ok || isJSONNull(raw) || json.Unmarshal(raw, &serverInfo) != nil || serverInfo == nil {
		return InitializeResult{}, invalid
	}
	var name, serverVersion string
	if err := json.Unmarshal(serverInfo["name"], &name); err != nil {
		return InitializeResult{}, invalid
	}
	if err := json.Unmarshal(serverInfo["version"], &serverVersion); err != nil {
		return InitializeResult{}, invalid
	}
	if raw, ok := fields["instructions"]; ok {
		if isJSONNull(raw) {
			return InitializeResult{}, invalid
		}
		var instructions string
		if err := json.Unmarshal(raw, &instructions); err != nil {
			return InitializeResult{}, invalid
		}
	}
	var result InitializeResult
	if err := json.Unmarshal(data, &result); err != nil {
		return InitializeResult{}, invalid
	}
	return result, nil
}

// validateListToolsResult checks the page shape and each tool: a string name
// and an object inputSchema, as pi's isTool requires.
func validateListToolsResult(data json.RawMessage) (ListToolsResult, error) {
	invalid := &McpError{Code: CodeInvalidRequest, Message: "Invalid MCP tools/list result"}
	invalidEntry := &McpError{Code: CodeInvalidRequest, Message: "Invalid entry in MCP tools/list result"}
	invalidCursor := &McpError{Code: CodeInvalidRequest, Message: "Invalid MCP tools/list cursor"}

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
	tools := make([]Tool, 0, len(entries))
	for _, entry := range entries {
		var entryFields map[string]json.RawMessage
		if err := json.Unmarshal(entry, &entryFields); err != nil || entryFields == nil {
			return ListToolsResult{}, invalidEntry
		}
		var name string
		if err := json.Unmarshal(entryFields["name"], &name); err != nil {
			return ListToolsResult{}, invalidEntry
		}
		var schema map[string]json.RawMessage
		rawSchema, hasSchema := entryFields["inputSchema"]
		if !hasSchema || isJSONNull(rawSchema) || json.Unmarshal(rawSchema, &schema) != nil || schema == nil {
			return ListToolsResult{}, invalidEntry
		}
		var tool Tool
		if err := json.Unmarshal(entry, &tool); err != nil {
			return ListToolsResult{}, invalidEntry
		}
		tools = append(tools, tool)
	}
	result := ListToolsResult{Tools: tools}
	if raw, ok := fields["nextCursor"]; ok && !isJSONNull(raw) {
		var cursor string
		if err := json.Unmarshal(raw, &cursor); err != nil {
			return ListToolsResult{}, invalidCursor
		}
		// Some servers end pagination with "" instead of omitting the cursor.
		result.NextCursor = cursor
	}
	return result, nil
}

// validateCallToolResult checks content is an array when present (missing
// content defaults to an empty list, as the SDK does) and keeps any JSON value
// as structuredContent. isError passes through.
func validateCallToolResult(data json.RawMessage) (CallToolResult, error) {
	invalid := &McpError{Code: CodeInvalidRequest, Message: "Invalid MCP tools/call result"}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return CallToolResult{}, invalid
	}
	result := CallToolResult{Content: []ContentBlock{}}
	if raw, ok := fields["content"]; ok {
		if isJSONNull(raw) {
			return CallToolResult{}, invalid
		}
		var content []ContentBlock
		if err := json.Unmarshal(raw, &content); err != nil || content == nil {
			return CallToolResult{}, invalid
		}
		result.Content = content
	}
	if raw, ok := fields["structuredContent"]; ok {
		result.StructuredContent = append(json.RawMessage(nil), raw...)
	}
	if raw, ok := fields["isError"]; ok && !isJSONNull(raw) {
		if err := json.Unmarshal(raw, &result.IsError); err != nil {
			return CallToolResult{}, invalid
		}
	}
	return result, nil
}
