// Behaviour ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/src/transports/stdio.ts and lines.ts (MIT License); implemented
// independently in Go. Contract kept identical; see docs/UPSTREAM.md.
//
// StdioTransport runs an MCP server as a child process and frames the
// connection the way the spec defines it: newline-delimited JSON-RPC on stdin
// and stdout, with stderr passed through for diagnostics. The child exiting
// closes the transport, so a crashed server fails pending calls instead of
// hanging them.

package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"time"
)

// DefaultMaxMessageBytes bounds one newline-delimited message, like pi's
// DEFAULT_MAX_MESSAGE_BYTES.
const DefaultMaxMessageBytes = 16 * 1024 * 1024

// DefaultCloseTimeout is how long Close waits for the server after SIGTERM
// before killing it, like pi's DEFAULT_CLOSE_TIMEOUT_MS.
const DefaultCloseTimeout = 2 * time.Second

// stdinCloseGrace is how long the server gets to exit on its own after stdin
// closes, before SIGTERM.
const stdinCloseGrace = 500 * time.Millisecond

// StdioTransportOptions configures a stdio server process. Only Command is
// required.
type StdioTransportOptions struct {
	Command string
	Args    []string
	// Dir is the working directory, empty for the current one.
	Dir string
	// Env replaces the child environment when non-nil; nil inherits os.Environ.
	Env []string
	// Stderr receives the child's stderr. Nil means os.Stderr (pass-through).
	Stderr io.Writer
	// MaxMessageBytes bounds one stdout line. Zero uses DefaultMaxMessageBytes.
	MaxMessageBytes int
	// CloseTimeout is how long Close waits after SIGTERM before SIGKILL. Zero
	// uses DefaultCloseTimeout; a negative value kills immediately.
	CloseTimeout time.Duration
	// OnError receives framing and process errors that do not close the
	// connection by themselves (for example a stray log line on stdout). Nil
	// discards them.
	OnError func(error)
}

// StdioTransport is a Transport over a child process's stdin/stdout.
type StdioTransport struct {
	options StdioTransportOptions

	mu      sync.Mutex
	started bool
	closed  bool
	cmd     *exec.Cmd
	stdin   io.WriteCloser

	writeMu sync.Mutex

	incoming chan Message
	// closedCh is closed by Close and wakes a blocked Receive.
	closedCh chan struct{}
	// done is closed when the child process exits (cmd.Wait returned).
	done chan struct{}

	closeOnce sync.Once
}

// NewStdioTransport returns a transport that spawns options.Command on Start.
func NewStdioTransport(options StdioTransportOptions) *StdioTransport {
	if options.MaxMessageBytes <= 0 {
		options.MaxMessageBytes = DefaultMaxMessageBytes
	}
	if options.CloseTimeout == 0 {
		options.CloseTimeout = DefaultCloseTimeout
	}
	options.Args = append([]string(nil), options.Args...)
	options.Env = append([]string(nil), options.Env...)
	if options.Stderr == nil {
		options.Stderr = os.Stderr
	}
	return &StdioTransport{
		options:  options,
		incoming: make(chan Message, 16),
		closedCh: make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// PID is the child process id, or 0 before Start.
func (t *StdioTransport) PID() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cmd == nil || t.cmd.Process == nil {
		return 0
	}
	return t.cmd.Process.Pid
}

// Start spawns the server and begins reading its stdout.
func (t *StdioTransport) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return &ConnectionClosedError{}
	}
	if t.started {
		t.mu.Unlock()
		return &McpError{Code: CodeInvalidRequest, Message: "MCP stdio transport already started"}
	}
	t.started = true
	t.mu.Unlock()

	command := exec.Command(t.options.Command, t.options.Args...)
	command.Dir = t.options.Dir
	if t.options.Env != nil {
		command.Env = t.options.Env
	}
	command.Stderr = t.options.Stderr
	stdin, err := command.StdinPipe()
	if err != nil {
		return fmt.Errorf("mcp: stdio stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("mcp: stdio stdout: %w", err)
	}
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return fmt.Errorf("mcp: start %q: %w", t.options.Command, err)
	}

	t.mu.Lock()
	t.cmd = command
	t.stdin = stdin
	t.mu.Unlock()

	go t.waitProcess()
	go t.readStdout(stdout)
	return nil
}

// waitProcess reaps the child and signals exit to Close.
func (t *StdioTransport) waitProcess() {
	_ = t.cmd.Wait()
	close(t.done)
}

// readStdout frames stdout as newline-delimited JSON-RPC messages. A malformed
// line is reported and skipped, matching pi: servers sometimes write a stray
// log line, and one bad line must not take the connection down.
func (t *StdioTransport) readStdout(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), t.options.MaxMessageBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		message, err := ParseMessage(line)
		if err != nil {
			t.reportError(err)
			continue
		}
		select {
		case t.incoming <- message:
		case <-t.closedCh:
			close(t.incoming)
			return
		}
	}
	if err := scanner.Err(); err != nil {
		t.reportError(fmt.Errorf("mcp: stdio read: %w", err))
	}
	close(t.incoming)
}

func (t *StdioTransport) reportError(err error) {
	if t.options.OnError != nil {
		t.options.OnError(err)
	}
}

// Send writes one message as a JSON line.
func (t *StdioTransport) Send(ctx context.Context, message Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("mcp: encode JSON-RPC message: %w", err)
	}
	data = append(data, '\n')

	t.mu.Lock()
	started, closed, stdin := t.started, t.closed, t.stdin
	t.mu.Unlock()
	if !started || closed || stdin == nil {
		return &ConnectionClosedError{}
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if _, err := stdin.Write(data); err != nil {
		return &ConnectionClosedError{Message: "MCP stdio write: " + err.Error()}
	}
	return nil
}

// Receive returns the next message, io.EOF once the transport closed, or the
// context error.
func (t *StdioTransport) Receive(ctx context.Context) (Message, error) {
	select {
	case message, ok := <-t.incoming:
		if !ok {
			return nil, io.EOF
		}
		return message, nil
	default:
	}
	select {
	case message, ok := <-t.incoming:
		if !ok {
			return nil, io.EOF
		}
		return message, nil
	case <-t.closedCh:
		select {
		case message, ok := <-t.incoming:
			if ok {
				return message, nil
			}
		default:
		}
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close shuts the server down the way the spec suggests: close stdin, let the
// server exit, then SIGTERM, then SIGKILL. It is idempotent.
func (t *StdioTransport) Close() error {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		started := t.started
		stdin := t.stdin
		cmd := t.cmd
		t.mu.Unlock()
		close(t.closedCh)

		if !started || cmd == nil || cmd.Process == nil {
			return
		}
		if stdin != nil {
			_ = stdin.Close()
		}
		grace := stdinCloseGrace
		if timeout := t.options.CloseTimeout; timeout < 0 {
			grace = 0
		} else if timeout < grace {
			grace = timeout
		}
		if t.waitDone(grace) {
			return
		}
		if runtime.GOOS != "windows" {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		if t.waitDone(t.options.CloseTimeout) {
			return
		}
		_ = cmd.Process.Kill()
		t.waitDone(time.Second)
	})
	return nil
}

// waitDone reports whether the child exited within timeout.
func (t *StdioTransport) waitDone(timeout time.Duration) bool {
	if timeout < 0 {
		timeout = 0
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-t.done:
		return true
	case <-timer.C:
		return false
	}
}
