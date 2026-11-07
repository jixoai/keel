// The transport contract and the pieces every transport shares.

package gateway

import (
	"context"
	"errors"
	"io"
)

// Transport carries JSON-RPC messages between the gateway and exactly one
// external provider.
//
// Start prepares the connection (for stdio over a child process, starts
// reading and writing). Send enqueues one message into a bounded outbound
// queue: it blocks while that queue is full, returns as soon as the context
// ends, and a writer goroutine performs the write. A provider that stops
// reading therefore stops the senders instead of growing a buffer, and it
// cannot wedge them either - a write that cannot complete within the write
// budget ends the connection (watchdog) rather than holding a lock forever.
// Receive blocks until the next message arrives and returns io.EOF once the
// connection ended cleanly, or a *TransportError / *FrameError (fatal) when it
// broke. Close is idempotent, releases the connection, and makes a blocked
// Receive or Send return promptly.
//
// Send and Receive may be called from different goroutines. Transports never
// expose ledger, lease or tenant data: they move bytes between this process and
// an external provider and nothing else.
type Transport interface {
	Start(ctx context.Context) error
	Send(ctx context.Context, message Message) error
	Receive(ctx context.Context) (Message, error)
	Close() error
	// Stats is a snapshot of counters used by health and tests.
	Stats() TransportStats
	// SetDiagnosticHandler installs the sink for non-fatal diagnostics. The
	// connection layer sets its own handler; callers receive diagnostics
	// through ConnOptions.OnDiagnostic. Handlers may be called from the
	// transport's reader goroutine and must not block.
	SetDiagnosticHandler(handler func(Diagnostic))
}

// TransportStats is a snapshot of one transport's counters.
type TransportStats struct {
	// Sent and Received count messages successfully written and delivered.
	Sent     int64
	Received int64
	// Skipped counts bad frames dropped while keeping the connection.
	Skipped int64
	// Queued and QueuedBytes are the inbound queue depth right now; Peak* are
	// the high-water marks since Start. Tests and health checks assert these
	// stay under the configured bounds.
	Queued          int64
	QueuedBytes     int64
	PeakQueued      int64
	PeakQueuedBytes int64
	// OutboundQueued, OutboundQueuedBytes and PeakOutbound are the same
	// counters for the writer-side queue (stdio transports; HTTP writes
	// synchronously and reports zero).
	OutboundQueued      int64
	OutboundQueuedBytes int64
	PeakOutbound        int64
}

// Transport budgets. They are the numbers the memory bound is built from:
// per frame, per queue, and per connection.
const (
	// DefaultMaxFrameBytes bounds one frame in either direction. 16 MiB matches
	// internal/mcp's default message bound, so a provider that works through one
	// works through the other; it is per frame, so the worst case per connection
	// is this plus one queue's worth.
	DefaultMaxFrameBytes = 16 << 20
	// DefaultMaxQueuedBytes bounds the inbound queue by payload bytes: one
	// maximum frame. Beyond that the reader blocks and the provider's write
	// blocks - memory stays flat under a slow consumer.
	DefaultMaxQueuedBytes = int64(16 << 20)
	// DefaultMaxQueuedMessages bounds the inbound queue by message count, which
	// is what actually bites with many small frames.
	DefaultMaxQueuedMessages = 64
)

// EventStreamStarter is implemented by transports that need an explicit
// server-to-client stream (Streamable HTTP's GET stream). The connection layer
// starts it once the handshake says the provider is an AgentBackend. Transports
// that carry server-to-client messages on their main channel (stdio) do not
// implement it.
type EventStreamStarter interface {
	StartEventStream()
}

// Diagnostic is one non-fatal protocol event a transport observed. Detail is
// built from sizes and error text only: it never contains raw frame bytes or
// message payloads, because diagnostics end up in logs.
type Diagnostic struct {
	Kind   string
	Detail string
}

// Diagnostic kinds.
const (
	// DiagFrameMalformed is a frame that is not valid JSON.
	DiagFrameMalformed = "frame.malformed"
	// DiagFrameTooLarge is a frame skipped for exceeding the limit.
	DiagFrameTooLarge = "frame.too_large"
	// DiagFrameDesync is lost framing (fatal for the connection).
	DiagFrameDesync = "frame.desync"
	// DiagMessageInvalid is well-formed JSON that is not a JSON-RPC message.
	DiagMessageInvalid = "message.invalid"
	// DiagReadError is a source read failure.
	DiagReadError = "read.error"
	// DiagWriteError is a sink write failure.
	DiagWriteError = "write.error"
	// DiagUnknownResponse is a response for an id that was never issued.
	DiagUnknownResponse = "response.unknown"
	// DiagServerRequest is a provider-initiated request the gateway answered
	// with "method not found".
	DiagServerRequest = "server.request"
	// DiagStreamError is a failure while consuming or reconnecting an HTTP stream.
	DiagStreamError = "stream.error"
	// DiagProcessExit is a spawned provider process exiting.
	DiagProcessExit = "process.exit"
)

// isFrameDiagnostic reports whether a diagnostic counts as a bad frame for the
// unhealthy-provider policy.
func isFrameDiagnostic(kind string) bool {
	switch kind {
	case DiagFrameMalformed, DiagFrameTooLarge, DiagFrameDesync, DiagMessageInvalid:
		return true
	default:
		return false
	}
}

// receiveMessage is the Receive implementation every transport shares: drain
// the queue first, report the terminal error exactly once the queue is empty,
// and wait for either a push or the caller's context.
func receiveMessage(ctx context.Context, q *messageQueue) (Message, error) {
	for {
		if message, ok := q.pop(); ok {
			return message, nil
		}
		if closed, terminal := q.state(); closed {
			// Close and the last push are ordered by the queue mutex, so an
			// empty queue here means no further message can arrive.
			if message, ok := q.pop(); ok {
				return message, nil
			}
			return nil, terminal
		}
		select {
		case <-q.wake:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// terminalIsClean reports whether a receive error means "the provider closed
// the connection" rather than a failure worth reporting as a read error.
func terminalIsClean(err error) bool {
	return errors.Is(err, io.EOF)
}
