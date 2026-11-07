// Conn is one initialized conversation with one external provider: it runs the
// handshake, correlates responses with requests, enforces timeouts, propagates
// cancellation, dispatches provider notifications and tracks provider health.
//
// State ownership is strict. The connection knows nothing about Keel's event
// ledger, leases or tenants: it hands results to the caller, and the caller
// commits them through the contract kernel. A provider can therefore never
// write an event, directly or indirectly, through this package.

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Connection-layer defaults.
const (
	// DefaultRequestTimeout bounds one request when ConnOptions leaves it zero.
	DefaultRequestTimeout = 60 * time.Second
	// DefaultEventsBuffer is the inbound AgentBackend event buffer. Zero
	// disables buffering: the provider then gets backpressure the moment the
	// consumer stops reading events.
	DefaultEventsBuffer = 16
	// DefaultMaxBadFrames is how many malformed frames one provider may send
	// before the connection is marked unhealthy. Negative means unlimited.
	DefaultMaxBadFrames = 16
	// cancelNotificationTimeout bounds the best-effort cancellation
	// notification. It is deliberately short: cancellation must not itself
	// become a stall.
	cancelNotificationTimeout = 2 * time.Second
	// serverRequestTimeout bounds the answer to a provider-initiated request.
	serverRequestTimeout = 5 * time.Second
)

// ConnState is the connection lifecycle.
type ConnState string

const (
	StateIdle       ConnState = "idle"
	StateConnecting ConnState = "connecting"
	StateReady      ConnState = "ready"
	StateClosed     ConnState = "closed"
)

// HealthState is how much a provider can be trusted right now (external-gateway
// spec: failure isolation; the gateway marks a provider unhealthy and the
// caller degrades or reconnects it).
type HealthState string

const (
	// HealthOK means the connection is running clean.
	HealthOK HealthState = "ok"
	// HealthDegraded means bad frames were skipped; the connection still works.
	HealthDegraded HealthState = "degraded"
	// HealthUnhealthy means the provider failed: pending calls were failed and
	// new calls are rejected until the caller reconnects it.
	HealthUnhealthy HealthState = "unhealthy"
	// HealthClosed means the caller closed the connection.
	HealthClosed HealthState = "closed"
)

// HealthInfo is a point-in-time health report.
type HealthInfo struct {
	State     HealthState
	Reason    string
	BadFrames int
	Since     time.Time
}

// ConnOptions configures a Conn. The zero value is usable: it identifies the
// gateway as keel-gateway, accepts both provider kinds and applies the
// defaults above.
type ConnOptions struct {
	// ClientInfo identifies this gateway to the provider.
	ClientInfo Implementation
	// Capabilities is offered in initialize. When both halves are nil the
	// gateway offers tools and agent events.
	Capabilities ClientCapabilities
	// ProtocolVersion defaults to ProtocolVersion.
	ProtocolVersion string
	// RequestTimeout bounds each request. Zero uses DefaultRequestTimeout; a
	// negative value disables the timer (the caller's context still applies).
	RequestTimeout time.Duration
	// AcceptProviders restricts which provider kinds may activate. Empty
	// accepts both.
	AcceptProviders []ProviderType
	// EventsBuffer is the AgentBackend event channel capacity.
	EventsBuffer int
	// MaxBadFrames bounds tolerated malformed frames before the provider is
	// marked unhealthy. Zero uses DefaultMaxBadFrames; negative is unlimited.
	MaxBadFrames int
	// OnDiagnostic receives non-fatal protocol diagnostics. It may be called
	// from transport goroutines and must not block.
	OnDiagnostic func(Diagnostic)
	// OnCall receives one audit record per external call. Same concurrency
	// caveat as OnDiagnostic.
	OnCall func(CallRecord)
	// OnHealth is invoked when the health state changes.
	OnHealth func(HealthInfo)
}

type pendingCall struct {
	ch chan callResult
}

type callResult struct {
	result json.RawMessage
	err    error
}

// Conn is one connection to one external provider.
type Conn struct {
	opts ConnOptions

	mu        sync.Mutex
	state     ConnState
	transport Transport
	nextID    int64
	pending   map[string]*pendingCall
	closedCh  chan struct{}
	finished  bool
	terminal  error
	health    HealthInfo
	badFrames int
	badStreak int

	protocolVersion string
	provider        ProviderInfo
	serverInfo      Implementation
	capabilities    ServerCapabilities
	instructions    string
	handlers        map[string]func(json.RawMessage)

	events       chan AgentEvent
	eventsClosed bool

	receiveStarted bool
	receiveDone    chan struct{}
}

// NewConn returns a connection in the idle state.
func NewConn(opts ConnOptions) *Conn {
	if opts.ClientInfo.Name == "" {
		opts.ClientInfo = Implementation{Name: ClientName, Version: ClientVersion}
	}
	if opts.ProtocolVersion == "" {
		opts.ProtocolVersion = ProtocolVersion
	}
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = DefaultRequestTimeout
	}
	if opts.EventsBuffer == 0 {
		opts.EventsBuffer = DefaultEventsBuffer
	}
	if opts.EventsBuffer < 0 {
		// A negative buffer selects strict backpressure: an unconsumed event
		// blocks the receive loop (and therefore the provider) immediately.
		opts.EventsBuffer = 0
	}
	if opts.MaxBadFrames == 0 {
		opts.MaxBadFrames = DefaultMaxBadFrames
	}
	if opts.Capabilities.Tools == nil && opts.Capabilities.Agent == nil {
		opts.Capabilities = ClientCapabilities{
			Tools: &ToolsCapability{},
			Agent: &AgentCapability{Events: true},
		}
	}
	if len(opts.AcceptProviders) == 0 {
		opts.AcceptProviders = []ProviderType{ProviderTypeToolProvider, ProviderTypeAgentBackend}
	}
	return &Conn{
		opts:     opts,
		state:    StateIdle,
		pending:  make(map[string]*pendingCall),
		handlers: make(map[string]func(json.RawMessage)),
		closedCh: make(chan struct{}),
		events:   make(chan AgentEvent, opts.EventsBuffer),
		health:   HealthInfo{State: HealthOK, Since: time.Now()},
	}
}

// Connect starts the transport, performs the initialize handshake, validates
// the negotiation (revision, provider kind, capabilities, session marks) and
// opens the event stream for AgentBackends. On any failure the connection is
// closed and the error is returned; a connection either activates with a known
// contract or does not activate at all.
//
// The handshake completes with the initialize response: this protocol has no
// separate "initialized" notification.
func (c *Conn) Connect(ctx context.Context, transport Transport) (Handshake, error) {
	if transport == nil {
		return Handshake{}, errors.New("gateway: connect: nil transport")
	}
	c.mu.Lock()
	if c.state != StateIdle {
		state := c.state
		c.mu.Unlock()
		return Handshake{}, fmt.Errorf("gateway: cannot connect a %s connection", state)
	}
	c.state = StateConnecting
	c.transport = transport
	c.mu.Unlock()
	transport.SetDiagnosticHandler(c.transportDiagnostic)

	if err := transport.Start(ctx); err != nil {
		c.fail(&HandshakeError{Code: HandshakeTransportStart, Detail: err.Error()}, false)
		return Handshake{}, err
	}
	c.mu.Lock()
	c.receiveStarted = true
	c.receiveDone = make(chan struct{})
	c.mu.Unlock()
	go c.receiveLoop(transport)

	params, err := json.Marshal(InitializeParams{
		ProtocolVersion: c.opts.ProtocolVersion,
		ClientInfo:      c.opts.ClientInfo,
		Capabilities:    c.opts.Capabilities,
		AcceptProviders: c.opts.AcceptProviders,
	})
	if err != nil {
		c.fail(&HandshakeError{Code: HandshakeInitialize, Detail: err.Error()}, true)
		return Handshake{}, err
	}
	raw, err := c.doRequest(ctx, MethodInitialize, params, requestOptions{allowConnecting: true})
	if err != nil {
		c.fail(&HandshakeError{Code: HandshakeInitialize, Detail: err.Error()}, true)
		return Handshake{}, err
	}
	handshake, err := validateInitializeResult(raw, c.opts.AcceptProviders)
	if err != nil {
		c.fail(err, true)
		return Handshake{}, err
	}
	// A transport that echoes the negotiated revision (Streamable HTTP's
	// MCP-Protocol-Version header) learns it before the first provider call.
	if setter, ok := transport.(interface{ SetProtocolVersion(string) }); ok {
		setter.SetProtocolVersion(handshake.ProtocolVersion)
	}

	c.mu.Lock()
	if c.finished {
		terminal := c.terminal
		c.mu.Unlock()
		return Handshake{}, terminal
	}
	c.protocolVersion = handshake.ProtocolVersion
	c.provider = handshake.Provider
	c.serverInfo = handshake.ServerInfo
	c.capabilities = handshake.Capabilities
	c.instructions = handshake.Instructions
	c.state = StateReady
	c.health = HealthInfo{State: HealthOK, Since: time.Now()}
	c.mu.Unlock()

	// An AgentBackend pushes events over a server-to-client stream; transports
	// that need an explicit one (Streamable HTTP's GET stream) open it now,
	// after the handshake said the provider really is an agent backend.
	if handshake.Provider.Type == ProviderTypeAgentBackend {
		if starter, ok := transport.(EventStreamStarter); ok {
			starter.StartEventStream()
		}
	}
	return handshake, nil
}

// Close ends the connection. Pending calls fail with *ConnClosedError, the
// event channel closes, and a blocked Receive returns. It is idempotent.
func (c *Conn) Close() error {
	c.mu.Lock()
	transport := c.transport
	c.mu.Unlock()
	c.finish(&ConnClosedError{}, HealthClosed, "closed by caller")
	var err error
	if transport != nil {
		err = transport.Close()
	}
	c.mu.Lock()
	started, done := c.receiveStarted, c.receiveDone
	c.mu.Unlock()
	if started {
		<-done
	}
	c.closeEvents()
	return err
}

// State reports the lifecycle.
func (c *Conn) State() ConnState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Health reports provider health.
func (c *Conn) Health() HealthInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.health
}

// Done is closed when the connection ends for any reason.
func (c *Conn) Done() <-chan struct{} { return c.closedCh }

// Terminal is the error that ended the connection, nil while it runs.
func (c *Conn) Terminal() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.finished {
		return nil
	}
	return c.terminal
}

// ProtocolVersion is the negotiated revision, empty before Connect.
func (c *Conn) ProtocolVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.protocolVersion
}

// Provider is the provider declaration from initialize, zero before Connect.
func (c *Conn) Provider() ProviderInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.provider
}

// ServerInfo is the provider's identity from initialize.
func (c *Conn) ServerInfo() Implementation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serverInfo
}

// Capabilities is what the provider declared it supports.
func (c *Conn) Capabilities() ServerCapabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capabilities
}

// Instructions is the optional initialize instructions.
func (c *Conn) Instructions() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.instructions
}

// Events is the AgentBackend event stream. Events are delivered in order and
// backpressure the provider when the consumer falls behind; the channel closes
// when the connection ends.
func (c *Conn) Events() <-chan AgentEvent { return c.events }

// OnNotification registers a handler for a provider notification method. The
// handler runs on the connection's receive goroutine and must not block.
func (c *Conn) OnNotification(method string, handler func(params json.RawMessage)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if handler == nil {
		delete(c.handlers, method)
		return
	}
	c.handlers[method] = handler
}

// requestOptions carries per-call context that must not leak into the wire.
type requestOptions struct {
	target          string
	allowConnecting bool
}

// doRequest sends one request and waits for its response, its timeout, the
// caller's context, or the connection ending. Cancellation is propagated to
// the provider as notifications/cancelled.
func (c *Conn) doRequest(ctx context.Context, method string, params json.RawMessage, opts requestOptions) (json.RawMessage, error) {
	c.mu.Lock()
	switch {
	case c.finished:
		terminal := c.terminal
		c.mu.Unlock()
		if terminal == nil {
			terminal = &ConnClosedError{}
		}
		return nil, terminal
	case c.state != StateReady && !(opts.allowConnecting && c.state == StateConnecting):
		state := c.state
		c.mu.Unlock()
		return nil, &ConnClosedError{Reason: "connection is " + string(state)}
	}
	transport := c.transport
	c.nextID++
	id := NumberID(c.nextID)
	call := &pendingCall{ch: make(chan callResult, 1)}
	c.pending[id.Key()] = call
	c.mu.Unlock()

	started := time.Now()
	message := &Request{JSONRPC: JSONRPCVersion, ID: id, Method: method, Params: params}
	if err := transport.Send(ctx, message); err != nil {
		c.removePending(id)
		mapped := mapSendError(err, method, id, started)
		c.recordCall(method, opts.target, started, mapped)
		return nil, mapped
	}

	var timer *time.Timer
	var timeoutCh <-chan time.Time
	if c.opts.RequestTimeout > 0 {
		timer = time.NewTimer(c.opts.RequestTimeout)
		timeoutCh = timer.C
		defer timer.Stop()
	}

	select {
	case result := <-call.ch:
		c.recordCall(method, opts.target, started, result.err)
		return result.result, result.err
	case <-ctx.Done():
		c.removePending(id)
		c.sendCancel(id, "request cancelled")
		var mapped error
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			mapped = &TimeoutError{Method: method, RequestID: id.String(), Timeout: time.Since(started)}
		} else {
			mapped = &CanceledError{Method: method, RequestID: id.String(), Reason: ctx.Err().Error()}
		}
		c.recordCall(method, opts.target, started, mapped)
		return nil, mapped
	case <-timeoutCh:
		c.removePending(id)
		c.sendCancel(id, "deadline exceeded")
		mapped := &TimeoutError{Method: method, RequestID: id.String(), Timeout: c.opts.RequestTimeout}
		c.recordCall(method, opts.target, started, mapped)
		return nil, mapped
	case <-c.closedCh:
		select {
		case result := <-call.ch:
			c.recordCall(method, opts.target, started, result.err)
			return result.result, result.err
		default:
		}
		c.removePending(id)
		c.mu.Lock()
		terminal := c.terminal
		c.mu.Unlock()
		if terminal == nil {
			terminal = &ConnClosedError{}
		}
		c.recordCall(method, opts.target, started, terminal)
		return nil, terminal
	}
}

// receiveLoop dispatches provider messages until the connection dies. It is the
// only sender on the events channel, so it is also the one that closes it.
func (c *Conn) receiveLoop(transport Transport) {
	defer func() {
		close(c.receiveDone)
		c.closeEvents()
	}()
	for {
		message, err := transport.Receive(context.Background())
		if err != nil {
			c.markProviderGone(err)
			return
		}
		c.noteValidMessage()
		switch typed := message.(type) {
		case *SuccessResponse:
			c.deliver(typed.ID, callResult{result: typed.Result})
		case *ErrorResponse:
			c.deliver(typed.ID, callResult{err: &RPCError{
				Code:    typed.Error.Code,
				Message: typed.Error.Message,
				Data:    typed.Error.Data,
			}})
		case *Request:
			c.answerServerRequest(transport, typed)
		case *Notification:
			c.dispatchNotification(typed)
		}
	}
}

// deliver resolves a pending call. A response for an id that was never issued,
// or for a call that already settled, is reported and dropped.
func (c *Conn) deliver(id ID, result callResult) {
	key := id.Key()
	c.mu.Lock()
	call := c.pending[key]
	if call != nil {
		delete(c.pending, key)
	}
	c.mu.Unlock()
	if call == nil {
		c.reportDiagnostic(Diagnostic{Kind: DiagUnknownResponse, Detail: "id " + id.String()})
		return
	}
	call.ch <- result
}

// dispatchNotification routes provider notifications: event.stream events go
// to the events channel (blocking is the backpressure), everything else goes
// to a registered handler.
func (c *Conn) dispatchNotification(notification *Notification) {
	if notification.Method == MethodEventStream {
		var event AgentEvent
		if err := json.Unmarshal(notification.Params, &event); err != nil {
			c.reportDiagnostic(Diagnostic{Kind: DiagMessageInvalid, Detail: "agent event payload: " + err.Error()})
			return
		}
		if event.SessionID == "" {
			c.reportDiagnostic(Diagnostic{Kind: DiagMessageInvalid, Detail: "agent event without a session id"})
			return
		}
		select {
		case c.events <- event:
		case <-c.closedCh:
		}
		return
	}
	c.mu.Lock()
	handler := c.handlers[notification.Method]
	c.mu.Unlock()
	if handler != nil {
		handler(notification.Params)
	}
}

// answerServerRequest replies to a provider-initiated request. This protocol
// defines no server-initiated requests, so everything is "method not found"
// (the caller is not a provider).
func (c *Conn) answerServerRequest(transport Transport, request *Request) {
	c.reportDiagnostic(Diagnostic{Kind: DiagServerRequest, Detail: "method " + request.Method})
	ctx, cancel := context.WithTimeout(context.Background(), serverRequestTimeout)
	defer cancel()
	_ = transport.Send(ctx, &ErrorResponse{
		JSONRPC: JSONRPCVersion,
		ID:      request.ID,
		Error:   ErrorObject{Code: CodeMethodNotFound, Message: "Method not found: " + request.Method},
	})
}

// sendCancel propagates a cancellation to the provider, detached: the caller
// returns a definite error immediately and the notification is best-effort.
// The provider may still answer later; the response is dropped because the id
// is no longer pending.
func (c *Conn) sendCancel(id ID, reason string) {
	params, err := json.Marshal(struct {
		RequestID ID     `json:"requestId"`
		Reason    string `json:"reason,omitempty"`
	}{RequestID: id, Reason: reason})
	if err != nil {
		return
	}
	c.mu.Lock()
	finished, transport := c.finished, c.transport
	c.mu.Unlock()
	if finished || transport == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), cancelNotificationTimeout)
		defer cancel()
		_ = transport.Send(ctx, &Notification{JSONRPC: JSONRPCVersion, Method: MethodRequestCancel, Params: params})
	}()
}

// markProviderGone ends the connection after its transport died.
func (c *Conn) markProviderGone(err error) {
	reason := "provider closed the connection"
	if !terminalIsClean(err) {
		reason = err.Error()
	}
	terminal := &UnhealthyError{Reason: reason}
	if !terminalIsClean(err) {
		terminal.Err = err
	}
	c.finish(terminal, HealthUnhealthy, reason)
}

// finish ends the connection exactly once: it flips the state, fails pending
// calls, stores the terminal error and reports the final health.
func (c *Conn) finish(terminal error, health HealthState, reason string) {
	c.mu.Lock()
	if c.finished {
		c.mu.Unlock()
		return
	}
	c.finished = true
	c.state = StateClosed
	c.terminal = terminal
	info := HealthInfo{State: health, Reason: reason, BadFrames: c.badFrames, Since: time.Now()}
	c.health = info
	pending := c.pending
	c.pending = make(map[string]*pendingCall)
	close(c.closedCh)
	c.mu.Unlock()

	for _, call := range pending {
		call.ch <- callResult{err: terminal}
	}
	if c.opts.OnHealth != nil {
		c.opts.OnHealth(info)
	}
}

// fail ends the connection from a Connect path and waits for the receive loop
// to unwind so no goroutine keeps working on a dead connection.
func (c *Conn) fail(terminal error, closeTransport bool) {
	c.finish(terminal, HealthUnhealthy, terminal.Error())
	c.mu.Lock()
	transport, started, done := c.transport, c.receiveStarted, c.receiveDone
	c.mu.Unlock()
	if closeTransport && transport != nil {
		_ = transport.Close()
	}
	if started {
		<-done
	}
	c.closeEvents()
}

// closeEvents closes the event channel exactly once. It is called after the
// receive loop has stopped (its dedicated sender), so no send can race it.
func (c *Conn) closeEvents() {
	c.mu.Lock()
	if c.eventsClosed {
		c.mu.Unlock()
		return
	}
	c.eventsClosed = true
	c.mu.Unlock()
	close(c.events)
}

// transportDiagnostic counts bad frames for the unhealthy policy and forwards
// the diagnostic to the caller.
func (c *Conn) transportDiagnostic(diagnostic Diagnostic) {
	var (
		degraded  bool
		info      HealthInfo
		unhealthy bool
		limit     int
	)
	if isFrameDiagnostic(diagnostic.Kind) {
		c.mu.Lock()
		c.badFrames++
		c.badStreak++
		limit = c.opts.MaxBadFrames
		if limit > 0 && c.badStreak > limit && !c.finished {
			unhealthy = true
		} else if !c.finished && c.health.State != HealthUnhealthy {
			c.health = HealthInfo{State: HealthDegraded, Reason: diagnostic.Kind, BadFrames: c.badFrames, Since: c.health.Since}
			info = c.health
			degraded = true
		}
		c.mu.Unlock()
	}
	if c.opts.OnDiagnostic != nil {
		c.opts.OnDiagnostic(diagnostic)
	}
	if degraded && c.opts.OnHealth != nil {
		c.opts.OnHealth(info)
	}
	if unhealthy {
		reason := fmt.Sprintf("provider sent more than %d bad frames", limit)
		c.finish(&UnhealthyError{Reason: reason}, HealthUnhealthy, reason)
		c.mu.Lock()
		transport := c.transport
		c.mu.Unlock()
		if transport != nil {
			_ = transport.Close()
		}
	}
}

// noteValidMessage clears the bad-frame streak and returns the health to ok.
func (c *Conn) noteValidMessage() {
	c.mu.Lock()
	changed := false
	var info HealthInfo
	if c.badStreak > 0 {
		c.badStreak = 0
	}
	if !c.finished && c.health.State == HealthDegraded {
		c.health = HealthInfo{State: HealthOK, Since: c.health.Since, BadFrames: c.badFrames}
		info = c.health
		changed = true
	}
	c.mu.Unlock()
	if changed && c.opts.OnHealth != nil {
		c.opts.OnHealth(info)
	}
}

func (c *Conn) removePending(id ID) {
	c.mu.Lock()
	delete(c.pending, id.Key())
	c.mu.Unlock()
}

// recordCall emits one audit record for a finished call.
func (c *Conn) recordCall(method, target string, started time.Time, err error) {
	if c.opts.OnCall == nil {
		return
	}
	status, code := callStatus(err)
	c.mu.Lock()
	provider := c.serverInfo.Name
	c.mu.Unlock()
	c.opts.OnCall(CallRecord{
		Provider:  provider,
		Method:    method,
		Target:    target,
		Status:    status,
		ErrorCode: code,
		Duration:  time.Since(started),
	})
}

// reportDiagnostic forwards an internal diagnostic to the caller's handler.
func (c *Conn) reportDiagnostic(diagnostic Diagnostic) {
	if c.opts.OnDiagnostic != nil {
		c.opts.OnDiagnostic(diagnostic)
	}
}

// mapSendError converts a transport Send failure into a request error.
func mapSendError(err error, method string, id ID, started time.Time) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &TimeoutError{Method: method, RequestID: id.String(), Timeout: time.Since(started)}
	case errors.Is(err, context.Canceled):
		return &CanceledError{Method: method, RequestID: id.String(), Reason: err.Error()}
	default:
		return err
	}
}
