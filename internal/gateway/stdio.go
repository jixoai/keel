// StdioTransport: newline-delimited JSON-RPC over an io.ReadWriteCloser.
//
// That stream is a child process's stdin/stdout (see SpawnStdio) or any
// in-process pipe; tests use net.Pipe or io.Pipe. Reading is incremental and
// capped (frame.go), and the reader goroutine pushes into the bounded queue
// (queue.go), so a slow consumer stops pulling bytes off the wire and the
// provider's next write blocks: backpressure end to end.
//
// A provider that dies closes the stream; the transport reports it as the
// queue's terminal error and the connection layer marks the provider unhealthy
// (external-gateway spec: an external crash must not take Keel down).

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultProcessCloseGrace bounds each step of shutting a spawned provider
// down (wait for a clean exit, then SIGTERM, then SIGKILL).
const DefaultProcessCloseGrace = 2 * time.Second

// stdinCloseGrace is how long a spawned provider gets to exit on its own after
// its stdin closed, before SIGTERM.
const stdinCloseGrace = 500 * time.Millisecond

// processExitGrace is how long the read loop waits for a spawned process's exit
// status after its stdout reached EOF.
const processExitGrace = 100 * time.Millisecond

// StdioTransportOptions configures a stdio connection.
type StdioTransportOptions struct {
	// Stream carries the connection. Frames are newline-delimited JSON in both
	// directions; Close closes both ends.
	Stream io.ReadWriteCloser
	// MaxFrameBytes bounds one frame in either direction. Zero uses
	// DefaultMaxFrameBytes.
	MaxFrameBytes int
	// MaxQueuedMessages and MaxQueuedBytes bound the inbound queue. Outbound
	// frames use the same bounds. Zero uses the defaults.
	MaxQueuedMessages int
	MaxQueuedBytes    int64
	// WriteTimeout bounds how long one frame may take to reach the peer. A
	// provider that stops reading its stdin must not wedge the writer: the
	// transport prefers the stream's write deadline and falls back to a timer
	// that closes the stream, so the link is marked unhealthy instead of
	// stalling forever. Zero uses DefaultWriteTimeout; negative disables the
	// watchdog. The outbound queue still keeps canceled sends from piling up.
	WriteTimeout time.Duration
	// CloseGrace bounds each shutdown step of a spawned process. Zero uses
	// DefaultProcessCloseGrace; negative kills immediately.
	CloseGrace time.Duration
}

// DefaultWriteTimeout bounds one stdio frame write.
const DefaultWriteTimeout = 10 * time.Second

// StdioTransport is a Transport over a byte stream.
type StdioTransport struct {
	opts        StdioTransportOptions
	queue       *messageQueue
	outbound    *outboundQueue
	frameWriter *FrameWriter

	mu      sync.Mutex
	started bool
	closed  bool
	handler func(Diagnostic)

	closeOnce  sync.Once
	closedCh   chan struct{}
	done       chan struct{}
	writerDone chan struct{}

	process     *os.Process
	processDone chan struct{}
	exitState   atomic.Pointer[os.ProcessState]

	sent     atomic.Int64
	received atomic.Int64
	skipped  atomic.Int64
}

// NewStdioTransport returns a transport over opts.Stream. Start begins reading
// and writing.
func NewStdioTransport(opts StdioTransportOptions) *StdioTransport {
	if opts.MaxFrameBytes <= 0 {
		opts.MaxFrameBytes = DefaultMaxFrameBytes
	}
	if opts.WriteTimeout == 0 {
		opts.WriteTimeout = DefaultWriteTimeout
	}
	return &StdioTransport{
		opts:        opts,
		queue:       newMessageQueue(opts.MaxQueuedMessages, opts.MaxQueuedBytes),
		outbound:    newOutboundQueue(opts.MaxQueuedMessages, opts.MaxQueuedBytes),
		frameWriter: NewFrameWriter(opts.Stream, opts.MaxFrameBytes),
		closedCh:    make(chan struct{}),
		done:        make(chan struct{}),
		writerDone:  make(chan struct{}),
	}
}

// Start begins reading frames and running the writer. It must be called once.
func (t *StdioTransport) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return &TransportError{Reason: "stdio transport is closed", Err: ErrTransportClosed}
	}
	if t.started {
		return errors.New("gateway: stdio transport already started")
	}
	if t.opts.Stream == nil {
		return errors.New("gateway: stdio transport requires a stream")
	}
	t.started = true
	go t.readLoop()
	go t.writeLoop()
	return nil
}

// Send enqueues one message as a JSON line. The frame limit is enforced before
// anything is queued, and the bounded outbound queue is what keeps a provider
// that never reads its stdin from piling senders up: push blocks only while the
// queue is full and returns as soon as the context ends.
func (t *StdioTransport) Send(ctx context.Context, message Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("gateway: encode JSON-RPC message: %w", err)
	}
	if len(data) > t.opts.MaxFrameBytes {
		return &FrameError{Kind: FrameTooLarge, Direction: "outbound", Size: len(data), Limit: t.opts.MaxFrameBytes}
	}
	t.mu.Lock()
	started, closed := t.started, t.closed
	t.mu.Unlock()
	if !started || closed {
		return &TransportError{Reason: "stdio transport is not connected", Err: ErrTransportClosed}
	}
	return t.outbound.push(ctx, data)
}

// writeLoop is the only goroutine that writes to the stream. A write error (or
// a write that cannot complete within the write budget) ends the link: the
// queues close with the error, the stream is closed to unblock the read side,
// and every pending call fails instead of hanging.
func (t *StdioTransport) writeLoop() {
	defer close(t.writerDone)
	for {
		frame, err := t.outbound.pop(context.Background())
		if err != nil {
			return // the queue (and thus the link) closed
		}
		if err := t.writeFrame(frame); err != nil {
			t.report(Diagnostic{Kind: DiagWriteError, Detail: err.Error()})
			terminal := &TransportError{Reason: "stdio write", Err: err}
			t.outbound.close(terminal)
			t.queue.close(terminal)
			t.closeStream()
			return
		}
		t.sent.Add(1)
	}
}

// writeFrame writes one frame under the write watchdog. When the stream
// supports write deadlines (pipes from os.Pipe and net.Conn do) the deadline
// makes a blocked write return with a timeout error; otherwise a timer closes
// the stream, which unblocks the write. Either way the writer cannot be wedged
// forever by a peer that stopped reading.
func (t *StdioTransport) writeFrame(frame []byte) error {
	if setter, ok := t.opts.Stream.(interface{ SetWriteDeadline(time.Time) error }); ok && t.opts.WriteTimeout > 0 {
		if err := setter.SetWriteDeadline(time.Now().Add(t.opts.WriteTimeout)); err != nil {
			return fmt.Errorf("stdio write deadline: %w", err)
		}
		defer func() { _ = setter.SetWriteDeadline(time.Time{}) }()
		return t.frameWriter.WriteFrame(frame)
	}
	if t.opts.WriteTimeout > 0 {
		var inFlight atomic.Bool
		inFlight.Store(true)
		timer := time.AfterFunc(t.opts.WriteTimeout, func() {
			if !inFlight.Load() {
				return
			}
			// The stream cannot carry a deadline, so closing it is the only way
			// to unblock this write.
			t.report(Diagnostic{Kind: DiagWriteError, Detail: "stdio write stalled; closing the stream"})
			terminal := &TransportError{Reason: "stdio write stalled", Err: os.ErrDeadlineExceeded}
			t.outbound.close(terminal)
			t.queue.close(terminal)
			t.closeStream()
		})
		defer func() {
			inFlight.Store(false)
			timer.Stop()
		}()
	}
	return t.frameWriter.WriteFrame(frame)
}

// Receive returns the next message, io.EOF once the provider closed the stream
// cleanly, or the terminal *TransportError / *FrameError.
func (t *StdioTransport) Receive(ctx context.Context) (Message, error) {
	return receiveMessage(ctx, t.queue)
}

// Close releases the stream and, for a spawned process, terminates its whole
// process group: stdin close, a grace period, SIGTERM to the group, then
// SIGKILL. It is idempotent.
func (t *StdioTransport) Close() error {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		t.mu.Unlock()
		close(t.closedCh)
		terminal := &TransportError{Reason: "stdio transport closed", Err: ErrTransportClosed}
		t.outbound.close(terminal)
		t.queue.close(terminal)
		t.closeStream()
		t.terminateProcess()
	})
	return nil
}

// closeStream closes the underlying stream, which unblocks a blocked read or
// write on it. Idempotent via the stream's own Close contract.
func (t *StdioTransport) closeStream() {
	if t.opts.Stream != nil {
		_ = t.opts.Stream.Close()
	}
}

// Done is closed when the read loop ended.
func (t *StdioTransport) Done() <-chan struct{} { return t.done }

// WriterDone is closed when the writer goroutine ended.
func (t *StdioTransport) WriterDone() <-chan struct{} { return t.writerDone }

// PID is the spawned process's id, or 0 for an in-process stream.
func (t *StdioTransport) PID() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.process == nil {
		return 0
	}
	return t.process.Pid
}

// ExitState is the spawned process's wait status, nil while it runs or when no
// process is managed.
func (t *StdioTransport) ExitState() *os.ProcessState { return t.exitState.Load() }

// Stats is the transport counters snapshot.
func (t *StdioTransport) Stats() TransportStats {
	queued, bytes, peakItems, peakBytes := t.queue.stats()
	outQueued, outBytes, outPeak := t.outbound.stats()
	return TransportStats{
		Sent:                t.sent.Load(),
		Received:            t.received.Load(),
		Skipped:             t.skipped.Load(),
		Queued:              int64(queued),
		QueuedBytes:         bytes,
		PeakQueued:          peakItems,
		PeakQueuedBytes:     peakBytes,
		OutboundQueued:      outQueued,
		OutboundQueuedBytes: outBytes,
		PeakOutbound:        outPeak,
	}
}

// SetDiagnosticHandler installs the diagnostic sink.
func (t *StdioTransport) SetDiagnosticHandler(handler func(Diagnostic)) {
	t.mu.Lock()
	t.handler = handler
	t.mu.Unlock()
}

// readLoop frames the stream, drops bad frames with a diagnostic, and pushes
// good ones into the queue. It is the queue's single producer.
func (t *StdioTransport) readLoop() {
	defer close(t.done)
	reader := NewFrameReader(t.opts.Stream, t.opts.MaxFrameBytes)
	for {
		data, err := reader.ReadFrame()
		if err != nil {
			var frameErr *FrameError
			if errors.As(err, &frameErr) {
				t.report(Diagnostic{Kind: diagnosticKindForFrame(frameErr), Detail: frameErr.Error()})
				t.skipped.Add(1)
				if frameErr.Fatal {
					t.queue.close(frameErr)
					return
				}
				continue
			}
			if errors.Is(err, io.EOF) {
				t.queue.close(t.endTerminal())
				return
			}
			t.report(Diagnostic{Kind: DiagReadError, Detail: err.Error()})
			t.queue.close(&TransportError{Reason: "stdio read", Err: err})
			return
		}
		message, perr := ParseMessage(data)
		if perr != nil {
			// One bad message must not take the connection down: report it
			// (without the bytes) and keep framing.
			t.report(Diagnostic{
				Kind:   DiagMessageInvalid,
				Detail: fmt.Sprintf("dropped a %d byte frame: %v", len(data), perr),
			})
			t.skipped.Add(1)
			continue
		}
		if !t.queue.push(message, len(data)) {
			return // closed; the message is dropped deliberately
		}
		t.received.Add(1)
	}
}

// endTerminal is the terminal error for a stream that reached EOF: a spawned
// process's exit status when there is one, plain io.EOF otherwise.
func (t *StdioTransport) endTerminal() error {
	if t.process == nil {
		return io.EOF
	}
	timer := time.NewTimer(processExitGrace)
	defer timer.Stop()
	select {
	case <-t.processDone:
		if state := t.exitState.Load(); state != nil {
			t.report(Diagnostic{Kind: DiagProcessExit, Detail: state.String()})
			return &TransportError{Reason: "provider process exited: " + state.String()}
		}
		return io.EOF
	case <-timer.C:
		return io.EOF
	}
}

func (t *StdioTransport) report(diagnostic Diagnostic) {
	t.mu.Lock()
	handler := t.handler
	t.mu.Unlock()
	if handler != nil {
		handler(diagnostic)
	}
}

// terminateProcess runs the shutdown ladder for a spawned provider: it must
// take the whole process group down, not just the direct child, or a Node/DSH
// runtime's own children survive as orphans.
func (t *StdioTransport) terminateProcess() {
	t.mu.Lock()
	process := t.process
	t.mu.Unlock()
	if process == nil {
		return
	}
	grace := t.opts.CloseGrace
	if grace == 0 {
		grace = DefaultProcessCloseGrace
	}
	if grace < 0 {
		_ = killProcessTree(process)
		t.waitProcess(time.Second)
		return
	}
	first := stdinCloseGrace
	if first > grace {
		first = grace
	}
	// The direct child may exit on stdin close while its children keep running,
	// so every step checks the whole group, not just the child.
	if t.waitProcess(first) && !processGroupExists(process.Pid) {
		return
	}
	if err := terminateProcessTree(process); err != nil {
		t.report(Diagnostic{Kind: DiagProcessExit, Detail: "process group signal: " + err.Error()})
		_ = process.Signal(os.Interrupt)
	}
	if t.waitProcess(grace) && !processGroupExists(process.Pid) {
		return
	}
	_ = killProcessTree(process)
	t.waitProcess(time.Second)
	if processGroupExists(process.Pid) {
		t.report(Diagnostic{Kind: DiagProcessExit, Detail: "provider process group still alive after SIGKILL"})
	}
}

// waitProcess reports whether the spawned process exited within timeout.
func (t *StdioTransport) waitProcess(timeout time.Duration) bool {
	if t.processDone == nil {
		return true
	}
	if timeout < 0 {
		timeout = 0
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-t.processDone:
		return true
	case <-timer.C:
		return false
	}
}

// diagnosticKindForFrame maps a frame error to its diagnostic kind.
func diagnosticKindForFrame(err *FrameError) string {
	switch err.Kind {
	case FrameTooLarge:
		return DiagFrameTooLarge
	case FrameDesync:
		return DiagFrameDesync
	default:
		return DiagFrameMalformed
	}
}

// SpawnOptions configures SpawnStdio.
type SpawnOptions struct {
	Command string
	Args    []string
	// Dir is the child's working directory; empty means the current one.
	Dir string
	// Env is the child's complete environment and MUST be explicit (non-nil).
	// A nil Env would hand the provider the entire host environment - database
	// URLs, model API keys, tunnel tokens - so SpawnStdio refuses it: start
	// from MinimalEnv(os.Environ()) and append exactly the variables the
	// provider genuinely needs. An empty slice means "no environment".
	Env []string
	// Stderr receives the child's stderr. Nil means os.Stderr.
	Stderr io.Writer

	MaxFrameBytes     int
	MaxQueuedMessages int
	MaxQueuedBytes    int64
	WriteTimeout      time.Duration
	CloseGrace        time.Duration
}

// SpawnStdio starts command and returns a transport over its stdin/stdout. The
// child's stderr passes through (diagnostics must not corrupt the protocol
// stream). The child becomes its own process-group leader, and Close takes the
// whole group down. The caller must Close the transport.
func SpawnStdio(opts SpawnOptions) (*StdioTransport, error) {
	if opts.Command == "" {
		return nil, errors.New("gateway: SpawnStdio requires a command")
	}
	if opts.Env == nil {
		return nil, errors.New("gateway: SpawnStdio requires an explicit environment (Env is nil): " +
			"build one with MinimalEnv(os.Environ()) plus the provider's own variables; " +
			"inheriting the host environment would leak Keel's secrets into the provider")
	}
	for _, entry := range opts.Env {
		if !strings.Contains(entry, "=") {
			return nil, fmt.Errorf("gateway: SpawnStdio: environment entry %q is not NAME=value", entry)
		}
	}
	command := exec.Command(opts.Command, opts.Args...)
	command.Dir = opts.Dir
	command.Env = opts.Env
	setProcessGroup(command)
	if opts.Stderr != nil {
		command.Stderr = opts.Stderr
	} else {
		command.Stderr = os.Stderr
	}
	// The pipes are created here rather than with StdinPipe/StdoutPipe, and
	// reaped with Wait rather than with os.Process.Wait: this way the parent
	// owns exactly two file descriptors (the ones it reads and writes), the
	// child owns the other two, and Wait does not close a descriptor the reader
	// goroutine may still be blocked on.
	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("gateway: spawn %q: stdin pipe: %w", opts.Command, err)
	}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		_ = stdinRead.Close()
		_ = stdinWrite.Close()
		return nil, fmt.Errorf("gateway: spawn %q: stdout pipe: %w", opts.Command, err)
	}
	command.Stdin = stdinRead
	command.Stdout = stdoutWrite
	if err := command.Start(); err != nil {
		_ = stdinRead.Close()
		_ = stdinWrite.Close()
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()
		return nil, fmt.Errorf("gateway: spawn %q: %w", opts.Command, err)
	}
	// The child owns its ends now; only the parent's ends stay open here.
	_ = stdinRead.Close()
	_ = stdoutWrite.Close()

	transport := NewStdioTransport(StdioTransportOptions{
		Stream: &stdioConn{
			reader:  stdoutRead,
			writer:  stdinWrite,
			closers: []io.Closer{stdinWrite, stdoutRead},
		},
		MaxFrameBytes:     opts.MaxFrameBytes,
		MaxQueuedMessages: opts.MaxQueuedMessages,
		MaxQueuedBytes:    opts.MaxQueuedBytes,
		WriteTimeout:      opts.WriteTimeout,
		CloseGrace:        opts.CloseGrace,
	})
	transport.process = command.Process
	transport.processDone = make(chan struct{})
	go func() {
		// Wait reaps the child; ProcessState carries the exit status.
		_ = command.Wait()
		if state := command.ProcessState; state != nil {
			transport.exitState.Store(state)
		}
		close(transport.processDone)
	}()
	return transport, nil
}

// stdioConn adapts a child's separate stdin/stdout pipes to io.ReadWriteCloser.
type stdioConn struct {
	reader  io.Reader
	writer  io.Writer
	closers []io.Closer
}

func (c *stdioConn) Read(p []byte) (int, error)  { return c.reader.Read(p) }
func (c *stdioConn) Write(p []byte) (int, error) { return c.writer.Write(p) }

// SetWriteDeadline forwards to the underlying write end when it supports
// deadlines (os.File pipes do), so a spawned provider gets the watchdog's
// precise path instead of the close-the-stream fallback.
func (c *stdioConn) SetWriteDeadline(deadline time.Time) error {
	if setter, ok := c.writer.(interface{ SetWriteDeadline(time.Time) error }); ok {
		return setter.SetWriteDeadline(deadline)
	}
	return errors.New("gateway: this stream does not support write deadlines")
}

func (c *stdioConn) Close() error {
	var first error
	for _, closer := range c.closers {
		if err := closer.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
