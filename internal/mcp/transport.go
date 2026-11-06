// Behaviour ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/src/transports/transport.ts (MIT License); implemented independently
// in Go. Contract kept identical; see docs/UPSTREAM.md.
//
// A Transport carries JSON-RPC messages between the client and one server. The
// client owns request correlation and timeouts; transports only frame and move
// messages, so the same Client runs over a child process, an in-memory pair of
// endpoints, and (later) Streamable HTTP.

package mcp

import "context"

// Transport is one connection to an MCP server.
//
// Start prepares the connection (for stdio, spawns the child process). Send
// writes one message; Receive blocks until the next message arrives. Receive
// returns io.EOF once the connection closed cleanly (the stdio child exited, or
// the peer endpoint closed) and a *ConnectionClosedError for an aborted one.
// Close is idempotent and releases the connection; a closed transport must make
// a blocked Receive return.
//
// Send and Receive may be called from different goroutines.
type Transport interface {
	Start(ctx context.Context) error
	Send(ctx context.Context, message Message) error
	Receive(ctx context.Context) (Message, error)
	Close() error
}
