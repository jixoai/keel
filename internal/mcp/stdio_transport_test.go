// Tests ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/test/stdio.test.ts (MIT License), using a Go test binary helper
// process (TestHelperProcess) instead of the Node fixture scripts.

package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const stdioHelperEnv = "KEEL_MCP_STDIO_HELPER"

// TestHelperProcess is the MCP server child process: when the environment
// variable is set it speaks newline-delimited JSON-RPC on stdin/stdout and
// exits; under a normal `go test` run it does nothing.
func TestHelperProcess(t *testing.T) {
	if os.Getenv(stdioHelperEnv) != "1" {
		t.Skip("helper process entry point; set " + stdioHelperEnv + "=1")
	}
	os.Exit(runStdioHelper())
}

func runStdioHelper() int {
	if os.Getenv("KEEL_MCP_STDIO_NOISE") == "1" {
		// A stray non-JSON line before the protocol starts: pi reports it and
		// keeps the connection.
		fmt.Fprintln(os.Stdout, "starting stdio fixture")
	}
	fmt.Fprintln(os.Stderr, "stdio fixture ready")
	if os.Getenv("KEEL_MCP_STDIO_STUBBORN") == "1" {
		signal.Ignore(syscall.SIGTERM)
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), DefaultMaxMessageBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		message, err := ParseMessage(line)
		if err != nil {
			fmt.Fprintln(os.Stderr, "helper: bad message:", err)
			continue
		}
		request, ok := message.(*Request)
		if !ok {
			continue // notifications/initialized and other notifications
		}
		writeHelperResponse(request)
	}
	if os.Getenv("KEEL_MCP_STDIO_STUBBORN") == "1" {
		for { // ignore stdin closing; the transport has to kill us
			time.Sleep(time.Hour)
		}
	}
	return 0
}

func writeHelperResponse(request *Request) {
	respond := func(result any) {
		data, err := json.Marshal(result)
		if err != nil {
			return
		}
		message, err := json.Marshal(&SuccessResponse{JSONRPC: JSONRPCVersion, ID: request.ID, Result: data})
		if err != nil {
			return
		}
		_, _ = os.Stdout.Write(append(message, '\n'))
	}
	respondError := func(code int, text string) {
		message, err := json.Marshal(&ErrorResponse{
			JSONRPC: JSONRPCVersion,
			ID:      request.ID,
			Error:   ErrorObject{Code: code, Message: text},
		})
		if err != nil {
			return
		}
		_, _ = os.Stdout.Write(append(message, '\n'))
	}

	switch request.Method {
	case "initialize":
		respond(map[string]any{
			"protocolVersion": LatestProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
			"serverInfo":      map[string]any{"name": "stdio-fixture", "version": "1.0.0"},
		})
	case "tools/list":
		respond(map[string]any{
			"tools": []any{
				map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}},
			},
		})
	case "tools/call":
		var params struct {
			Name      string `json:"name"`
			Arguments struct {
				Text string `json:"text"`
			} `json:"arguments"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			respondError(CodeInvalidParams, err.Error())
			return
		}
		switch params.Name {
		case "echo":
			respond(map[string]any{
				"content": []any{map[string]any{"type": "text", "text": params.Arguments.Text}},
			})
		default:
			respond(map[string]any{"content": []any{}, "isError": true})
		}
	default:
		respondError(CodeMethodNotFound, "Method not found: "+request.Method)
	}
}

// lockedBuffer is a bytes.Buffer safe for the child's stderr copier and the
// test goroutine reading it.
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

func stdioHelperTransport(stderr *lockedBuffer, extraEnv ...string) *StdioTransport {
	env := append(os.Environ(), stdioHelperEnv+"=1")
	env = append(env, extraEnv...)
	return NewStdioTransport(StdioTransportOptions{
		Command:      os.Args[0],
		Args:         []string{"-test.run=^TestHelperProcess$"},
		Env:          env,
		Stderr:       stderr,
		CloseTimeout: 2 * time.Second,
	})
}

func TestStdioTransportConnectsCallsAndCloses(t *testing.T) {
	stderr := &lockedBuffer{}
	transport := stdioHelperTransport(stderr, "KEEL_MCP_STDIO_NOISE=1")
	client := NewClient(ClientOptions{Name: "stdio-test", Version: "1.0.0"})
	if _, err := client.Connect(testContext(t), transport); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if transport.PID() == 0 {
		t.Error("PID() = 0 after Start, want the child pid")
	}
	if got := client.ServerInfo(); got.Name != "stdio-fixture" || got.Version != "1.0.0" {
		t.Errorf("ServerInfo() = %+v, want stdio-fixture 1.0.0", got)
	}

	tools, err := client.ListTools(testContext(t))
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Errorf("ListTools() = %+v, want one echo tool", tools)
	}

	result, err := client.CallTool(testContext(t), "echo", json.RawMessage(`{"text":"hello"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "hello" {
		t.Errorf("CallTool content = %#v, want text hello", result.Content)
	}

	errorResult, err := client.CallTool(testContext(t), "missing", nil)
	if err != nil {
		t.Fatalf("CallTool(missing): %v", err)
	}
	if !errorResult.IsError {
		t.Error("CallTool(missing).IsError = false, want true")
	}

	if !waitFor(t, 2*time.Second, func() bool {
		return strings.Contains(stderr.String(), "stdio fixture ready")
	}) {
		t.Errorf("stderr = %q, want the fixture's ready line", stderr.String())
	}

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := client.State(); got != ClientClosed {
		t.Errorf("State() = %q, want closed", got)
	}
	select {
	case <-transport.done:
	case <-time.After(5 * time.Second):
		t.Fatal("child process did not exit after Close")
	}
}

func TestStdioTransportKillsServerIgnoringShutdown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM is not available on Windows")
	}
	transport := stdioHelperTransport(&lockedBuffer{}, "KEEL_MCP_STDIO_STUBBORN=1")
	transport.options.CloseTimeout = 100 * time.Millisecond
	client := NewClient(ClientOptions{Name: "stdio-test", Version: "1.0.0"})
	if _, err := client.Connect(testContext(t), transport); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	started := time.Now()
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("Close took %v, want less than 5s", elapsed)
	}
	select {
	case <-transport.done:
	case <-time.After(5 * time.Second):
		t.Fatal("stubborn child did not exit after Close")
	}
}

func TestStdioTransportReceivesEveryMessageInOrder(t *testing.T) {
	// One client request at a time exercises framing; interleaved requests below
	// exercise response correlation by id.
	stderr := &lockedBuffer{}
	transport := stdioHelperTransport(stderr)
	client := NewClient(ClientOptions{Name: "stdio-test", Version: "1.0.0"})
	ctx := testContext(t)
	if _, err := client.Connect(ctx, transport); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	type outcome struct {
		index  int
		result CallToolResult
		err    error
	}
	results := make(chan outcome, 4)
	for i := 0; i < 4; i++ {
		go func(index int) {
			result, err := client.CallTool(ctx, "echo", json.RawMessage(fmt.Sprintf(`{"text":"call-%d"}`, index)))
			results <- outcome{index: index, result: result, err: err}
		}(i)
	}
	seen := make(map[int]string, 4)
	for i := 0; i < 4; i++ {
		select {
		case out := <-results:
			if out.err != nil {
				t.Fatalf("concurrent CallTool %d: %v", out.index, out.err)
			}
			seen[out.index] = out.result.Content[0].Text
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for concurrent stdio calls")
		}
	}
	for i := 0; i < 4; i++ {
		if want := fmt.Sprintf("call-%d", i); seen[i] != want {
			t.Errorf("call %d returned %q, want %q (responses correlated by id)", i, seen[i], want)
		}
	}
}
