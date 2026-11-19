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
	"strings"
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
	// Notifications, when set, receives every server notification. Nil keeps the
	// pre-P4 behaviour: notifications are dropped (fail closed; no events
	// surface is wired).
	Notifications ClientNotificationHandler
	// ServerRequests, when set, answers server-initiated requests. Nil answers
	// "method not found", exactly as before P4.
	ServerRequests ClientServerRequestHandler
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

	// listeners are the open subscriptions/listen streams keyed by the listen
	// request id. Progress sinks are keyed by the token sent on the wire; at
	// most one in-flight call owns a token, so a frame cannot reach a second
	// session.
	listeners map[string]*listener
	progress  map[string]func(ProgressNotification)
	// progressSeq disambiguates two concurrent calls that asked for the same
	// token (the caller only names a preference; uniqueness is the client's).
	progressSeq int64

	// Diagnostic sampling: a hostile or broken server can flood protocol
	// violations (untagged listens, a full notification queue). The client
	// reports at most one such warning per second and folds the rest into
	// "(suppressed=N)" (the P3 audit-sampling shape).
	diagMu         sync.Mutex
	diagWindow     time.Time
	diagSuppressed int
	now            func() time.Time
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
		options:   options,
		state:     ClientIdle,
		pending:   make(map[string]*pendingCall),
		closedCh:  make(chan struct{}),
		listeners: make(map[string]*listener),
		progress:  make(map[string]func(ProgressNotification)),
		now:       time.Now,
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
	return c.CallToolWithOptions(ctx, name, args, CallOptions{})
}

// CallToolWithOptions invokes one tool with the per-call notification surface
// (a progress token and its sink). The zero options produce exactly the wire
// shape CallTool always produced.
func (c *Client) CallToolWithOptions(ctx context.Context, name string, args json.RawMessage, options CallOptions) (CallToolResult, error) {
	token := ""
	if options.ProgressToken != "" {
		token = c.registerProgress(options.ProgressToken, options.OnProgress)
		defer c.unregisterProgress(token)
	}
	params, err := encodeCallToolParams(name, args, token)
	if err != nil {
		return CallToolResult{}, fmt.Errorf("mcp: encode tools/call params: %w", err)
	}
	raw, err := c.request(ctx, "tools/call", params, false)
	if err != nil {
		return CallToolResult{}, err
	}
	return validateCallToolResult(raw)
}

// encodeCallToolParams builds the tools/call params object. `_meta` appears only
// when a progress token was requested: the pre-P4 bytes stay identical.
func encodeCallToolParams(name string, args json.RawMessage, progressToken string) (json.RawMessage, error) {
	if progressToken == "" {
		return json.Marshal(struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments,omitempty"`
		}{Name: name, Arguments: args})
	}
	return json.Marshal(struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
		Meta      struct {
			ProgressToken string `json:"progressToken"`
		} `json:"_meta"`
	}{Name: name, Arguments: args, Meta: struct {
		ProgressToken string `json:"progressToken"`
	}{ProgressToken: progressToken}})
}

// registerProgress installs the per-call sink and returns the unique token put
// on the wire. Two concurrent calls that ask for the same token get distinct
// tokens: a progress frame belongs to exactly one call (no cross-session leak).
func (c *Client) registerProgress(token string, sink func(ProgressNotification)) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	unique := token
	for {
		if _, taken := c.progress["s:"+unique]; !taken {
			break
		}
		c.progressSeq++
		unique = fmt.Sprintf("%s#%d", token, c.progressSeq)
	}
	if sink != nil {
		c.progress["s:"+unique] = sink
	}
	return unique
}

func (c *Client) unregisterProgress(token string) {
	if token == "" {
		return
	}
	c.mu.Lock()
	delete(c.progress, "s:"+token)
	c.mu.Unlock()
}

// deliverProgress routes one notifications/progress frame to the call that owns
// its token. An unknown token is a late/foreign frame and is dropped: progress
// is advisory and dropping it is always safe.
func (c *Client) deliverProgress(params json.RawMessage) {
	var notification ProgressNotification
	if len(params) == 0 || json.Unmarshal(params, &notification) != nil {
		return
	}
	key := "n:" + string(notification.ProgressToken)
	var token string
	if json.Unmarshal(notification.ProgressToken, &token) == nil {
		key = "s:" + token
	}
	c.mu.Lock()
	sink := c.progress[key]
	c.mu.Unlock()
	if sink != nil {
		sink(notification)
	}
}

// Listen opens a modern subscriptions/listen stream. It returns once the
// server's first message acknowledged the subscription (or fails when the ack
// is missing); subsequent notifications arrive on the handle's Events channel,
// filtered to the server's accepted subset and tagged with this subscription.
func (c *Client) Listen(ctx context.Context, params ListenParams, options ListenOptions) (*ListenHandle, error) {
	c.mu.Lock()
	if c.state != ClientConnected {
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
	listener := newListener(params, options)
	listener.handle.client = c
	listener.handle.id = id
	c.listeners[id.Key()] = listener
	c.mu.Unlock()

	encoded, err := json.Marshal(params)
	if err != nil {
		c.removeListener(id)
		return nil, fmt.Errorf("mcp: encode subscriptions/listen params: %w", err)
	}
	if err := transport.Send(ctx, &Request{JSONRPC: JSONRPCVersion, ID: id, Method: "subscriptions/listen", Params: encoded}); err != nil {
		c.removeListener(id)
		return nil, err
	}

	var timer *time.Timer
	var timeoutCh <-chan time.Time
	if c.options.RequestTimeout > 0 {
		timer = time.NewTimer(c.options.RequestTimeout)
		timeoutCh = timer.C
		defer timer.Stop()
	}
	for {
		select {
		case <-listener.ackCh:
			return listener.handle, nil
		case err := <-listener.handle.done:
			// The stream ended. If the ack was already processed the subscription
			// existed: return the handle and let the caller read the terminal
			// state from Done (a server may answer complete immediately).
			if listener.isAcked() {
				return listener.handle, nil
			}
			c.removeListener(id)
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("mcp: subscriptions/listen ended before the acknowledgement")
		case <-ctx.Done():
			c.removeListener(id)
			c.cancelListen(id)
			return nil, ctx.Err()
		case <-timeoutCh:
			c.removeListener(id)
			c.cancelListen(id)
			return nil, &TimeoutError{Timeout: c.options.RequestTimeout}
		case <-c.closedCh:
			c.removeListener(id)
			return nil, &ConnectionClosedError{}
		}
	}
}

// removeListener drops one stream from the routing table.
func (c *Client) removeListener(id ID) {
	c.mu.Lock()
	delete(c.listeners, id.Key())
	c.mu.Unlock()
}

// cancelListen ends the server-side subscription. Streamable HTTP closes the
// response stream (the HTTP form); other transports send notifications/cancelled
// (the stdio form).
func (c *Client) cancelListen(id ID) {
	c.mu.Lock()
	transport := c.transport
	c.mu.Unlock()
	if transport == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), serverRequestTimeout)
	defer cancel()
	if closer, ok := transport.(interface{ CancelRequest(ID) }); ok {
		closer.CancelRequest(id)
		return
	}
	params, err := json.Marshal(CancelledNotification{RequestID: mustMarshalID(id), Reason: "client closed the subscription"})
	if err != nil {
		return
	}
	_ = transport.Send(ctx, &Notification{JSONRPC: JSONRPCVersion, Method: "notifications/cancelled", Params: params})
}

// mustMarshalID renders an identifier for a cancellation payload.
func mustMarshalID(id ID) json.RawMessage {
	raw, err := json.Marshal(id)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return raw
}

// routeToListener handles one notification that belongs to an open listen
// stream. It reports whether the notification was consumed here (the global
// dispatcher must not see it twice).
func (c *Client) routeToListener(notification *Notification) bool {
	key, tagged := subscriptionIDOf(notification.Params)
	c.mu.Lock()
	var routed *listener
	if tagged {
		routed = c.listeners[key]
	}
	active := len(c.listeners)
	// Which stream carried this frame? The transport sets Source for frames read
	// from a request's own response stream; a listen request's response stream is
	// the one whose id is in the listeners table.
	var listenSource *listener
	if strings.HasPrefix(notification.Source, responseStreamSourcePrefix) {
		listenSource = c.listeners[strings.TrimPrefix(notification.Source, responseStreamSourcePrefix)]
	}
	c.mu.Unlock()
	if !tagged {
		if listenSource != nil {
			// Modern listen streams MUST tag every notification. An untagged frame
			// on that stream is a protocol violation.
			c.diagnoseSampled(listenSource.options.OnDiagnostic,
				"mcp listen: untagged notification on a listen stream: "+notification.Method)
			return true
		}
		if notification.Source == "" && active > 0 && isDirectoryNotification(notification.Method) {
			// A source-less transport (stdio, memory) cannot distinguish the
			// listen stream from the connection stream. Directory changes are
			// listen-domain facts; while a listen subscription is active an
			// untagged one is diagnosed. Request-domain frames (progress,
			// cancelled, message) still pass to the dispatcher.
			c.mu.Lock()
			for _, candidate := range c.listeners {
				c.diagnoseSampled(candidate.options.OnDiagnostic,
					"mcp listen: untagged directory notification while a listen stream is active: "+notification.Method)
				break
			}
			c.mu.Unlock()
			return true
		}
		// Untagged frames on the connection stream (stdio, legacy GET) or on
		// another request's response stream are request/connection-domain frames
		// (progress, cancelled, message). They MUST reach the dispatcher even
		// while a listen stream is open.
		return false
	}
	if routed == nil {
		// A frame tagged with a subscription this connection no longer has
		// (closed or superseded). It is not a global event: dropping it is the
		// dedupe contract, and it must never reach another subscription or the
		// dispatcher as a change.
		return true
	}
	if notification.Method == "notifications/subscriptions/acknowledged" {
		filter, err := ackFilterOf(notification.Params)
		if err != nil {
			c.diagnose(routed.options.OnDiagnostic, err.Error())
			return true
		}
		if !routed.ack(filter, key) {
			c.diagnose(routed.options.OnDiagnostic, "mcp listen: duplicate acknowledgement")
		}
		return true
	}
	if !routed.isAcked() {
		// The spec forbids notifications before the ack; the subscription is not
		// established yet, so the frame is a protocol violation, not a change.
		c.diagnoseSampled(routed.options.OnDiagnostic, "mcp listen: notification before acknowledgement: "+notification.Method)
		return true
	}
	if !routed.allowed(notification.Method, notification.Params) {
		c.diagnoseSampled(routed.options.OnDiagnostic, "mcp listen: notification outside the accepted subscription: "+notification.Method)
		return true
	}
	routed.deliver(ListenEvent{Method: notification.Method, Params: notification.Params}, c)
	return true
}

// finishListener resolves a terminal response for a listen request id.
func (c *Client) finishListener(id ID, err error) bool {
	c.mu.Lock()
	listener := c.listeners[id.Key()]
	if listener == nil {
		c.mu.Unlock()
		return false
	}
	delete(c.listeners, id.Key())
	c.mu.Unlock()
	listener.end(err, c)
	return true
}

func (c *Client) diagnose(sink func(string), message string) {
	if sink != nil {
		sink(message)
	}
}

// diagnoseSampled reports at most one warning per connection per second and
// folds the suppressed count into the next message. Protocol violations are
// server-controlled volume, so an unbounded log line per frame would let a
// hostile server flood the host log.
func (c *Client) diagnoseSampled(sink func(string), message string) {
	if sink == nil {
		return
	}
	now := c.now()
	c.diagMu.Lock()
	if c.diagWindow.IsZero() || now.Sub(c.diagWindow) >= time.Second {
		suppressed := c.diagSuppressed
		c.diagWindow = now
		c.diagSuppressed = 0
		c.diagMu.Unlock()
		sink(fmt.Sprintf("%s (suppressed=%d)", message, suppressed))
		return
	}
	c.diagSuppressed++
	c.diagMu.Unlock()
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
			if c.finishListener(typed.ID, nil) {
				continue
			}
			c.deliver(typed.ID, callResult{result: typed.Result})
		case *ErrorResponse:
			serverErr := &McpError{
				Code:    typed.Error.Code,
				Message: typed.Error.Message,
				Data:    typed.Error.Data,
			}
			if c.finishListener(typed.ID, serverErr) {
				continue
			}
			c.deliver(typed.ID, callResult{err: serverErr})
		case *Request:
			c.answerServerRequest(transport, typed)
		case *Notification:
			c.handleNotification(typed)
		}
		// Unknown message kinds cannot come out of ParseMessage.
	}
}

// handleNotification routes one server notification. Listen frames are consumed
// by their subscription; everything else reaches the injected dispatcher. With
// no dispatcher injected the pre-P4 behaviour is kept: notifications are
// dropped and no state changes.
func (c *Client) handleNotification(notification *Notification) {
	if c.routeToListener(notification) {
		return
	}
	if c.options.Notifications == nil {
		return
	}
	switch notification.Method {
	case "notifications/progress":
		c.deliverProgress(notification.Params)
	case "notifications/cancelled":
		c.abortCancelled(notification.Params)
	}
	c.options.Notifications(context.Background(), notification.Method, notification.Params)
}

// abortCancelled aborts the pending request a notifications/cancelled names.
// The call's result path then records the failure (the eventual tool.result),
// which is the only durable fact a cancellation produces.
func (c *Client) abortCancelled(params json.RawMessage) {
	var notification CancelledNotification
	if len(params) == 0 || json.Unmarshal(params, &notification) != nil {
		return
	}
	var id ID
	if json.Unmarshal(notification.RequestID, &id) != nil {
		return
	}
	reason := "the server cancelled the request"
	if notification.Reason != "" {
		reason += ": " + notification.Reason
	}
	c.deliver(id, callResult{err: &AbortError{Reason: reason}})
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

// answerServerRequest replies with the injected handler's message, or "method
// not found" when no handler is wired. This batch supports no server-initiated
// requests by default (roots, sampling, elicitation, ping are P5 work), and the
// spec requires a response rather than silence.
func (c *Client) answerServerRequest(transport Transport, request *Request) {
	ctx, cancel := context.WithTimeout(context.Background(), serverRequestTimeout)
	defer cancel()
	if handler := c.options.ServerRequests; handler != nil {
		response, err := handler(ctx, request)
		if err == nil && response != nil {
			_ = transport.Send(ctx, response)
			return
		}
		// An unhandled or failed server request is answered, never dropped: an
		// unanswered request blocks the server. -32601 is the honest answer for
		// "this host does not implement it".
	}
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
	listeners := c.listeners
	c.listeners = make(map[string]*listener)
	c.progress = make(map[string]func(ProgressNotification))
	c.closeOnce.Do(func() { close(c.closedCh) })
	c.mu.Unlock()
	for _, listener := range listeners {
		listener.end(&ConnectionClosedError{}, c)
	}
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
		// MCP Apps metadata is optional. A malformed block stays a diagnostic on
		// the tool; it never invalidates the catalogue entry.
		tool.UI, tool.UIDiagnostics = parseToolUI(tool.Meta)
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
	result.TTLMs, result.CacheScope = parseCacheHints(fields)
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
