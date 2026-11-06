// Behaviour ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/src/transports/in-memory.ts (MIT License); implemented independently
// in Go. Contract kept identical; see docs/UPSTREAM.md.
//
// MemoryTransport is a pair of connected endpoints for tests: what one Send
// delivers, the other Receive returns. It speaks the same Message types as the
// stdio transport, including server-initiated requests, so client behaviour is
// exercised without spawning a process.

package mcp

import (
	"context"
	"encoding/json"
	"io"
	"sync"
)

// memoryInboxSize bounds undelivered messages. Large enough that a test server
// may answer while the client is still sending.
const memoryInboxSize = 64

// MemoryTransport is one endpoint of an in-memory pair. Create the pair with
// NewMemoryTransportPair; the zero value is not usable.
type MemoryTransport struct {
	inbox chan Message
	peer  *MemoryTransport

	mu      sync.Mutex
	started bool
	closed  bool

	closeOnce sync.Once
	closedCh  chan struct{}
}

// NewMemoryTransportPair returns two connected endpoints, conventionally used
// as (client, server).
func NewMemoryTransportPair() (*MemoryTransport, *MemoryTransport) {
	client := &MemoryTransport{
		inbox:    make(chan Message, memoryInboxSize),
		closedCh: make(chan struct{}),
	}
	server := &MemoryTransport{
		inbox:    make(chan Message, memoryInboxSize),
		closedCh: make(chan struct{}),
	}
	client.peer = server
	server.peer = client
	return client, server
}

// Start marks the endpoint usable. Starting twice is an error, as is starting
// a closed endpoint.
func (t *MemoryTransport) Start(_ context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return &ConnectionClosedError{}
	}
	if t.started {
		return &McpError{Code: CodeInvalidRequest, Message: "MCP in-memory transport already started"}
	}
	t.started = true
	return nil
}

// Send delivers message to the peer. Messages are copied so neither side can
// mutate the other's view.
func (t *MemoryTransport) Send(ctx context.Context, message Message) error {
	t.mu.Lock()
	started, closed := t.started, t.closed
	peer := t.peer
	t.mu.Unlock()
	if !started || closed {
		return &ConnectionClosedError{}
	}
	if peer == nil {
		return &ConnectionClosedError{Message: "MCP in-memory transport has no peer"}
	}
	peer.mu.Lock()
	peerStarted, peerClosed := peer.started, peer.closed
	peer.mu.Unlock()
	if !peerStarted || peerClosed {
		return &ConnectionClosedError{Message: "MCP in-memory peer is not connected"}
	}
	copied, err := cloneMessage(message)
	if err != nil {
		return err
	}
	select {
	case peer.inbox <- copied:
		return nil
	case <-t.closedCh:
		return &ConnectionClosedError{}
	case <-peer.closedCh:
		return &ConnectionClosedError{Message: "MCP in-memory peer is not connected"}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Receive returns the next message delivered by the peer, io.EOF once either
// endpoint is closed, or the context error.
func (t *MemoryTransport) Receive(ctx context.Context) (Message, error) {
	// Prefer a message that is already queued: closing the pair must not drop
	// frames that arrived before Close.
	select {
	case message := <-t.inbox:
		return message, nil
	default:
	}
	select {
	case message := <-t.inbox:
		return message, nil
	case <-t.closedCh:
		select {
		case message := <-t.inbox:
			return message, nil
		default:
			return nil, io.EOF
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close closes this endpoint and its peer, as pi does. It is idempotent.
func (t *MemoryTransport) Close() error {
	t.close()
	if peer := t.peer; peer != nil {
		peer.close()
	}
	return nil
}

// close closes only this endpoint; Close propagates to the peer.
func (t *MemoryTransport) close() {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		t.mu.Unlock()
		close(t.closedCh)
	})
}

// cloneMessage round-trips a message through JSON, which both copies it and
// normalizes the identifier the way the wire does.
func cloneMessage(message Message) (Message, error) {
	data, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	return ParseMessage(data)
}
