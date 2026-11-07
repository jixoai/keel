// Test fixtures: an in-process provider over net.Pipe that speaks the same
// framing as the gateway, so transports and the connection layer can be
// exercised without spawning processes or touching the network.

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

const testTimeout = 5 * time.Second

// diagnosticLog collects diagnostics for assertions.
type diagnosticLog struct {
	mu    sync.Mutex
	items []Diagnostic
}

func (d *diagnosticLog) add(diagnostic Diagnostic) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.items = append(d.items, diagnostic)
}

func (d *diagnosticLog) snapshot() []Diagnostic {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Diagnostic(nil), d.items...)
}

func (d *diagnosticLog) hasKind(kind string) bool {
	for _, item := range d.snapshot() {
		if item.Kind == kind {
			return true
		}
	}
	return false
}

func (d *diagnosticLog) all() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var builder strings.Builder
	for _, item := range d.items {
		builder.WriteString(item.Kind)
		builder.WriteString(":")
		builder.WriteString(item.Detail)
		builder.WriteString("\n")
	}
	return builder.String()
}

// lockedBuffer is a bytes.Buffer that a spawned child's stderr copier and the
// test goroutine can use concurrently.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// providerDecl describes how the fake provider introduces itself.
type providerDecl struct {
	kind       ProviderType
	name       string
	stateOwner StateOwner
	recovery   string
	metering   Metering
	usage      bool
	// version overrides the negotiated protocol revision (for rejection tests).
	version string
}

// fakeProvider is one end of a net.Pipe speaking the provider side.
type fakeProvider struct {
	t    *testing.T
	conn net.Conn

	mu           sync.Mutex
	handlers     map[string]func(json.RawMessage) (any, *ErrorObject)
	silent       map[string]bool
	seenRequests []*Request
	seenNotifies []*Notification

	writeMu sync.Mutex
	readErr error
}

// startFakeProvider returns the client transport and the provider end.
func startFakeProvider(t *testing.T, opts StdioTransportOptions) (*StdioTransport, *fakeProvider) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	if opts.Stream != nil {
		t.Fatal("startFakeProvider owns the stream")
	}
	opts.Stream = clientConn
	if opts.MaxFrameBytes == 0 {
		opts.MaxFrameBytes = 4 << 20
	}
	transport := NewStdioTransport(opts)
	provider := &fakeProvider{
		t:        t,
		conn:     serverConn,
		handlers: make(map[string]func(json.RawMessage) (any, *ErrorObject)),
		silent:   make(map[string]bool),
	}
	go provider.readLoop()
	t.Cleanup(func() {
		provider.close()
		_ = transport.Close()
	})
	return transport, provider
}

func (p *fakeProvider) readLoop() {
	reader := NewFrameReader(p.conn, 4<<20)
	for {
		data, err := reader.ReadFrame()
		if err != nil {
			p.mu.Lock()
			p.readErr = err
			p.mu.Unlock()
			return
		}
		message, err := ParseMessage(data)
		if err != nil {
			continue
		}
		switch typed := message.(type) {
		case *Request:
			p.mu.Lock()
			p.seenRequests = append(p.seenRequests, typed)
			handler := p.handlers[typed.Method]
			silent := p.silent[typed.Method]
			p.mu.Unlock()
			if silent {
				continue
			}
			if handler == nil {
				p.respondError(typed.ID, CodeMethodNotFound, "no handler: "+typed.Method)
				continue
			}
			result, errorObject := handler(typed.Params)
			if errorObject != nil {
				p.respondError(typed.ID, errorObject.Code, errorObject.Message)
				continue
			}
			p.respond(typed.ID, result)
		case *Notification:
			p.mu.Lock()
			p.seenNotifies = append(p.seenNotifies, typed)
			p.mu.Unlock()
		}
	}
}

func (p *fakeProvider) setHandler(method string, handler func(json.RawMessage) (any, *ErrorObject)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handlers[method] = handler
}

func (p *fakeProvider) silence(method string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.silent[method] = true
}

// declare installs the initialize handler according to decl.
func (p *fakeProvider) declare(decl providerDecl) {
	if decl.name == "" {
		decl.name = "fake-provider"
	}
	p.setHandler(MethodInitialize, func(json.RawMessage) (any, *ErrorObject) {
		capabilities := map[string]any{}
		provider := map[string]any{"type": string(decl.kind)}
		switch decl.kind {
		case ProviderTypeToolProvider:
			capabilities["tools"] = map[string]any{"listChanged": true}
		case ProviderTypeAgentBackend:
			agent := map[string]any{"events": true, "cancel": true}
			if decl.usage {
				agent["usage"] = true
			}
			capabilities["agent"] = agent
			if decl.stateOwner != "" {
				provider["stateOwner"] = string(decl.stateOwner)
			}
			if decl.recovery != "" {
				provider["recovery"] = decl.recovery
			}
			if decl.metering != "" {
				provider["metering"] = string(decl.metering)
			}
		}
		version := decl.version
		if version == "" {
			version = ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    capabilities,
			"serverInfo":      map[string]any{"name": decl.name, "version": "9.9.9"},
			"provider":        provider,
		}, nil
	})
}

func (p *fakeProvider) requests() []*Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*Request(nil), p.seenRequests...)
}

func (p *fakeProvider) notifications() []*Notification {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*Notification(nil), p.seenNotifies...)
}

// waitForRequest blocks until a request with the method arrived.
func (p *fakeProvider) waitForRequest(method string) *Request {
	p.t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		for _, request := range p.requests() {
			if request.Method == method {
				return request
			}
		}
		time.Sleep(time.Millisecond)
	}
	p.t.Fatalf("timed out waiting for request %q", method)
	return nil
}

// waitForNotification blocks until a notification with the method arrived.
func (p *fakeProvider) waitForNotification(method string) *Notification {
	p.t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		for _, notification := range p.notifications() {
			if notification.Method == method {
				return notification
			}
		}
		time.Sleep(time.Millisecond)
	}
	p.t.Fatalf("timed out waiting for notification %q", method)
	return nil
}

// The respond/send helpers run on the provider's read goroutine, so they only
// log: a failed write surfaces as a test timeout instead of a cross-goroutine
// Fatalf.

func (p *fakeProvider) respond(id ID, result any) {
	data, err := json.Marshal(result)
	if err != nil {
		p.t.Logf("fake provider: encode response: %v", err)
		return
	}
	p.write(&SuccessResponse{JSONRPC: JSONRPCVersion, ID: id, Result: data})
}

func (p *fakeProvider) respondError(id ID, code int, message string) {
	p.write(&ErrorResponse{JSONRPC: JSONRPCVersion, ID: id, Error: ErrorObject{Code: code, Message: message}})
}

func (p *fakeProvider) sendNotification(method string, params any) {
	var data json.RawMessage
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			p.t.Logf("fake provider: encode notification: %v", err)
			return
		}
		data = encoded
	}
	p.write(&Notification{JSONRPC: JSONRPCVersion, Method: method, Params: data})
}

func (p *fakeProvider) sendRequest(id ID, method string, params any) {
	var data json.RawMessage
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			p.t.Logf("fake provider: encode request: %v", err)
			return
		}
		data = encoded
	}
	p.write(&Request{JSONRPC: JSONRPCVersion, ID: id, Method: method, Params: data})
}

// writeRaw writes bytes verbatim (bad frames, half frames, stray lines).
func (p *fakeProvider) writeRaw(data []byte) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if _, err := p.conn.Write(data); err != nil {
		p.t.Logf("fake provider raw write: %v", err)
	}
}

func (p *fakeProvider) write(message Message) {
	data, err := json.Marshal(message)
	if err != nil {
		p.t.Logf("fake provider: encode message: %v", err)
		return
	}
	p.writeRaw(append(data, '\n'))
}

func (p *fakeProvider) close() { _ = p.conn.Close() }

// fixture is a connected client plus the provider behind it.
type fixture struct {
	conn      *Conn
	tool      *ToolClient
	agent     *AgentClient
	provider  *fakeProvider
	transport *StdioTransport
	diags     *diagnosticLog
}

// newFixture connects a Conn to a fake provider that declares decl.
func newFixture(t *testing.T, opts ConnOptions, decl providerDecl) *fixture {
	t.Helper()
	transport, provider := startFakeProvider(t, StdioTransportOptions{})
	provider.declare(decl)
	diags := &diagnosticLog{}
	userDiagnostic := opts.OnDiagnostic
	opts.OnDiagnostic = func(diagnostic Diagnostic) {
		diags.add(diagnostic)
		if userDiagnostic != nil {
			userDiagnostic(diagnostic)
		}
	}
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = 2 * time.Second
	}
	conn := NewConn(opts)
	if _, err := conn.Connect(context.Background(), transport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	group := &fixture{
		conn:      conn,
		provider:  provider,
		transport: transport,
		diags:     diags,
		tool:      NewToolProvider(conn),
		agent:     NewAgentBackend(conn),
	}
	t.Cleanup(func() { _ = conn.Close() })
	return group
}

// newToolFixture is the common case: a connected tool provider.
func newToolFixture(t *testing.T) *fixture {
	t.Helper()
	group := newFixture(t, ConnOptions{}, providerDecl{kind: ProviderTypeToolProvider, name: "fake-tools"})
	group.provider.setHandler(MethodToolsList, func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{
			"tools": []any{
				map[string]any{"name": "echo", "description": "echoes text", "inputSchema": map[string]any{"type": "object"}},
			},
		}, nil
	})
	group.provider.setHandler(MethodToolsCall, func(params json.RawMessage) (any, *ErrorObject) {
		var call callToolParams
		if err := json.Unmarshal(params, &call); err != nil {
			return nil, &ErrorObject{Code: CodeInvalidParams, Message: err.Error()}
		}
		if call.Name != "echo" {
			return map[string]any{"content": []any{}, "isError": true}, nil
		}
		var args struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(call.Arguments, &args)
		return map[string]any{
			"content":           []any{map[string]any{"type": "text", "text": args.Text}},
			"structuredContent": map[string]any{"text": args.Text},
		}, nil
	})
	return group
}

// newAgentFixture is the agent-backend twin.
func newAgentFixture(t *testing.T, decl providerDecl) *fixture {
	t.Helper()
	if decl.kind == "" {
		decl.kind = ProviderTypeAgentBackend
	}
	if decl.name == "" {
		decl.name = "fake-agent"
	}
	return newFixture(t, ConnOptions{}, decl)
}

// waitFor polls until condition holds.
func waitFor(t *testing.T, timeout time.Duration, condition func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return condition()
}

// decodeParams unmarshals request params into v.
func decodeParams(t *testing.T, params json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(params, v); err != nil {
		t.Fatalf("decode params: %v", err)
	}
}
