// Write-path tests: a provider that stops reading its stdin must not be able
// to wedge senders, hold a write mutex forever, or accumulate cancel
// notifications.

package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// wedgedStream is a stream whose writes block until released. It stands in for
// a provider that stopped reading its stdin. When deadlines are enabled,
// SetWriteDeadline is honored the way os.File and net.Conn honor it.
type wedgedStream struct {
	deadlines bool
	// blockAfter lets the first N writes succeed before the stream wedges,
	// which is how a provider that reads the handshake and then stops reading
	// behaves.
	blockAfter int

	mu        sync.Mutex
	deadline  time.Time
	written   [][]byte
	released  chan struct{}
	closedCh  chan struct{}
	closeOnce sync.Once
}

func newWedgedStream(deadlines bool) *wedgedStream {
	return &wedgedStream{
		deadlines: deadlines,
		released:  make(chan struct{}),
		closedCh:  make(chan struct{}),
	}
}

func (s *wedgedStream) Read([]byte) (int, error) {
	<-s.closedCh
	return 0, io.EOF
}

func (s *wedgedStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	if s.blockAfter > 0 && len(s.written) < s.blockAfter {
		s.written = append(s.written, append([]byte(nil), p...))
		s.mu.Unlock()
		return len(p), nil
	}
	when := s.deadline
	s.mu.Unlock()
	var deadline <-chan time.Time
	if s.deadlines && !when.IsZero() {
		timer := time.NewTimer(time.Until(when))
		defer timer.Stop()
		deadline = timer.C
	}
	select {
	case <-s.released:
		s.mu.Lock()
		s.written = append(s.written, append([]byte(nil), p...))
		s.mu.Unlock()
		return len(p), nil
	case <-deadline:
		return 0, os.ErrDeadlineExceeded
	case <-s.closedCh:
		return 0, io.ErrClosedPipe
	}
}

func (s *wedgedStream) SetWriteDeadline(deadline time.Time) error {
	if !s.deadlines {
		return errors.New("gateway: this stream does not support write deadlines")
	}
	s.mu.Lock()
	s.deadline = deadline
	s.mu.Unlock()
	return nil
}

func (s *wedgedStream) Close() error {
	s.closeOnce.Do(func() { close(s.closedCh) })
	return nil
}

func (s *wedgedStream) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.released:
	default:
		close(s.released)
	}
}

func (s *wedgedStream) framesWritten() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.written)
}

// TestStdioSendUnblocksOnCancelWhilePeerStopsReading: the outbound queue is
// bounded and its push is context-aware, so a canceled send returns instead of
// queueing up behind a wedged write.
func TestStdioSendUnblocksOnCancelWhilePeerStopsReading(t *testing.T) {
	stream := newWedgedStream(true)
	transport := NewStdioTransport(StdioTransportOptions{
		Stream:            stream,
		MaxQueuedMessages: 2,
		MaxQueuedBytes:    1 << 20,
		WriteTimeout:      -1, // isolate the queue's behavior from the watchdog
	})
	if err := transport.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = transport.Close() })

	note := func(method string) Message {
		return &Notification{JSONRPC: JSONRPCVersion, Method: method}
	}
	if err := transport.Send(context.Background(), note("first")); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	// The writer takes the first frame and blocks inside Write. Wait for it to
	// leave the queue, then fill the queue up.
	if !waitFor(t, testTimeout, func() bool { return transport.Stats().OutboundQueued == 0 }) {
		t.Fatal("the writer never picked the first frame up")
	}
	for index := 0; index < 2; index++ {
		if err := transport.Send(context.Background(), note(fmt.Sprintf("queued-%d", index))); err != nil {
			t.Fatalf("queued Send %d: %v", index, err)
		}
	}
	if stats := transport.Stats(); stats.OutboundQueued != 2 {
		t.Fatalf("outbound queued = %d, want 2 (the writer is stuck in Write)", stats.OutboundQueued)
	}

	// A sender now has to wait for room; its context must win.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := transport.Send(ctx, note("blocked"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Send error = %v, want the context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("Send blocked for %v, want it bounded by the context", elapsed)
	}
	// The canceled frame was never enqueued, and the queue stayed inside its
	// bound.
	if stats := transport.Stats(); stats.OutboundQueued > 2 || stats.PeakOutbound > 2 {
		t.Errorf("outbound queue = %d (peak %d), want <= 2", stats.OutboundQueued, stats.PeakOutbound)
	}

	// A detached cancel-notification style send (a short context of its own)
	// also returns promptly instead of piling up.
	cancelCtx, cancelDone := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelDone()
	if err := transport.Send(cancelCtx, note(MethodRequestCancel)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel-style Send error = %v, want the context deadline", err)
	}

	// Once the peer starts reading, the queued frames drain in order.
	stream.release()
	if !waitFor(t, testTimeout, func() bool { return transport.Stats().Sent == 3 }) {
		t.Fatalf("written frames = %d, want the three queued frames", transport.Stats().Sent)
	}
}

// TestStdioCancelStyleSendDoesNotPileUp: the exact shape the connection layer
// uses for notifications/cancelled - a detached send with its own short
// context - must return within that context when the provider stopped reading,
// and the never-sent frame must not appear on the wire later.
func TestStdioCancelStyleSendDoesNotPileUp(t *testing.T) {
	stream := newWedgedStream(true)
	stream.blockAfter = 1 // the handshake write lands; everything after wedges
	transport := NewStdioTransport(StdioTransportOptions{
		Stream:            stream,
		MaxQueuedMessages: 1,
		MaxQueuedBytes:    1 << 20,
		WriteTimeout:      -1, // no watchdog: prove the queue alone unblocks
	})
	if err := transport.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = transport.Close() })

	note := func(method string) Message {
		return &Notification{JSONRPC: JSONRPCVersion, Method: method}
	}
	// First frame: written. Second: the writer blocks inside it.
	if err := transport.Send(context.Background(), note("request")); err != nil {
		t.Fatalf("Send(request): %v", err)
	}
	if err := transport.Send(context.Background(), note("second")); err != nil {
		t.Fatalf("Send(second): %v", err)
	}
	if !waitFor(t, testTimeout, func() bool { return transport.Stats().Sent >= 1 }) {
		t.Fatal("the first frame was never written")
	}
	// Fill the queue, then send the cancel-style notification.
	if err := transport.Send(context.Background(), note("queued")); err != nil {
		t.Fatalf("Send(queued): %v", err)
	}
	started := time.Now()
	cancelCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := transport.Send(cancelCtx, note(MethodRequestCancel))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel-style Send error = %v, want its own context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("cancel-style Send blocked for %v, want it bounded by its context", elapsed)
	}

	// Once the peer reads again, exactly three frames exist: the canceled one
	// was never queued.
	stream.release()
	if !waitFor(t, testTimeout, func() bool { return transport.Stats().Sent == 3 }) {
		t.Fatalf("written frames = %d, want 3", transport.Stats().Sent)
	}
	if got := stream.framesWritten(); got != 3 {
		t.Fatalf("frames on the wire = %d, want 3 (the canceled cancel notification must not be sent)", got)
	}
}

// TestStdioWriteWatchdogMarksStalledLinkUnhealthy: a write that cannot complete
// within the budget ends the link (death instead of an eternal stall), for both
// deadline-capable streams and streams without deadline support.
func TestStdioWriteWatchdogMarksStalledLinkUnhealthy(t *testing.T) {
	for _, deadlines := range []bool{true, false} {
		t.Run(fmt.Sprintf("deadlines=%v", deadlines), func(t *testing.T) {
			stream := newWedgedStream(deadlines)
			diags := &diagnosticLog{}
			transport := NewStdioTransport(StdioTransportOptions{
				Stream:       stream,
				WriteTimeout: 60 * time.Millisecond,
			})
			transport.SetDiagnosticHandler(diags.add)
			if err := transport.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() { _ = transport.Close() })

			sendErr := transport.Send(context.Background(), &Notification{JSONRPC: JSONRPCVersion, Method: "stuck"})
			if sendErr != nil {
				t.Fatalf("Send: %v", sendErr)
			}
			ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
			defer cancel()
			_, err := transport.Receive(ctx)
			var transportErr *TransportError
			if !errors.As(err, &transportErr) {
				t.Fatalf("terminal error = %v, want *TransportError", err)
			}
			if !strings.Contains(err.Error(), "write") {
				t.Errorf("terminal error = %v, want it to name the write failure", err)
			}
			if !diags.hasKind(DiagWriteError) {
				t.Errorf("diagnostics = %s, want a write diagnostic", diags.all())
			}
			select {
			case <-transport.WriterDone():
			case <-time.After(testTimeout):
				t.Fatal("the writer goroutine did not exit after the stall")
			}
			// Sends fail fast afterwards: the link is known dead.
			if err := transport.Send(context.Background(), &Notification{JSONRPC: JSONRPCVersion, Method: "late"}); err == nil {
				t.Error("Send after the link was marked dead succeeded")
			}
			if stream.framesWritten() != 0 {
				t.Errorf("%d frames were written to a stream that never accepted one", stream.framesWritten())
			}
		})
	}
}

// TestStdioConnForwardsWriteDeadlines: the stream a spawned provider gets
// (os.File pipes) supports deadlines, so the watchdog never has to fall back to
// closing the stream there.
func TestStdioConnForwardsWriteDeadlines(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer reader.Close()
	defer writer.Close()
	conn := &stdioConn{reader: reader, writer: writer}
	if err := conn.SetWriteDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("SetWriteDeadline was not forwarded to the pipe: %v", err)
	}
	// Nobody reads the pipe, and the payload is larger than its buffer: the
	// write must fail with the deadline instead of blocking forever.
	started := time.Now()
	if _, err := conn.Write(make([]byte, 512*1024)); err == nil {
		t.Fatal("a write to a full, unread pipe succeeded")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("the write took %v, want the deadline to fire", elapsed)
	}
}

// TestConnFailsFastWhenProviderStopsReading is the end-to-end shape: Connect
// against a provider that never reads its stdin must fail within the write
// watchdog, not hang until the request timeout.
func TestConnFailsFastWhenProviderStopsReading(t *testing.T) {
	stream := newWedgedStream(true)
	transport := NewStdioTransport(StdioTransportOptions{Stream: stream, WriteTimeout: 60 * time.Millisecond})
	t.Cleanup(func() { _ = transport.Close() })
	conn := NewConn(ConnOptions{RequestTimeout: 30 * time.Second})
	t.Cleanup(func() { _ = conn.Close() })

	started := time.Now()
	if _, err := conn.Connect(context.Background(), transport); err == nil {
		t.Fatal("Connect succeeded although the provider never answered")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("Connect took %v, want it bounded by the write watchdog", elapsed)
	}
	if state := conn.Health().State; state != HealthUnhealthy {
		t.Errorf("health = %q, want unhealthy", state)
	}
}
