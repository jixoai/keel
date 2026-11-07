// Package gateway implements the external-provider protocol: the transport and
// the contract Keel uses to talk to processes and services that live outside
// its own runtime (a DSH sidecar, an external tool provider).
//
// The gateway is deliberately a leaf package. It performs no ledger, lease or
// tenant work and it must stay that way: an external process can never reach
// the event log through this package (see the import guard in
// boundary_test.go). Results produced by an external provider are returned to
// the caller, which commits them through the contract kernel; the gateway
// never writes events, never creates runs and never claims leases.
//
// # Wire
//
// JSON-RPC 2.0 over one of two transports:
//
//   - stdio: newline-delimited JSON on an io.ReadWriteCloser (a child process's
//     stdin/stdout, or any in-process pipe);
//   - Streamable HTTP: one POST per message, answered with application/json or
//     a text/event-stream body, plus the server-to-client GET stream that
//     AgentBackends use to push events.
//
// The transport, the framing and the request lifecycle (ids, cancellation,
// timeouts) are reused from MCP, whose client this repository already has. The
// *method semantics* are this protocol's own, versioned by ProtocolVersion:
//
//   - the tool surface is MCP's verbatim (tools/list, tools/call) and exists
//     only on tool_provider connections, because a stateless JSON tool is what
//     MCP already defines and this project reuses MCP semantics for tools;
//   - the agent surface is a private, versioned method set for whole external
//     agent runtimes: initialize, session.open, session.resume, message.send,
//     event.stream, cancel, health, close. It is deliberately not dressed in
//     MCP method names: an external agent runtime is not an MCP server.
//
// A connection is exactly one provider kind, and the two surfaces are mutually
// exclusive; using one on the other's connection is an error, not a fallback.
//
// # Two provider kinds
//
//   - ToolProvider: stateless tools, JSON in and JSON out.
//   - AgentBackend: an external agent runtime that owns its own session state.
//     Its sessions are external sessions: they are marked state_owner=external
//     with external_backend / external_ref / external_status and must never
//     enter Keel's native run/lease flow. The gateway reports the markers; the
//     caller persists them. Model usage of such a session is marked
//     metering=external or metering=none - never silently zero.
//
// # Bounded memory
//
// Frames are read incrementally from the wire and capped (DefaultMaxFrameBytes);
// the inbound queue is bounded by both message count and payload bytes, so a
// slow consumer stops the provider instead of accumulating memory. Reading a
// frame never requires buffering the whole stream, and an oversized frame is
// aborted while the peer is still streaming it. See README.md for the exact
// framing, backpressure and safety model.
package gateway
