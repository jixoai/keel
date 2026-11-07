// stdio transport tests: round trips over in-process pipes, request
// correlation, cancellation, timeouts, slow consumers (backpressure), bad
// frames, crashes and the spawned-process lifecycle.
//
// Nothing here spawns a real external tool: the only process is the test
// binary re-executed as a helper (TestGatewayHelperProcess), the same pattern
// internal/mcp's stdio tests use.

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
	"testing"
	"time"
)

func TestStdioRoundTripTools(t *testing.T) {
	group := newToolFixture(t)
	ctx := context.Background()

	tools, err := group.tool.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("ListTools = %+v, want one echo tool", tools)
	}
	result, err := group.tool.CallTool(ctx, "echo", json.RawMessage(`{"text":"hello"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "hello" {
		t.Fatalf("CallTool content = %+v, want text hello", result.Content)
	}
	if !strings.Contains(string(result.StructuredContent), "hello") {
		t.Fatalf("structuredContent = %s, want it to carry the text", result.StructuredContent)
	}
	if got := group.conn.Provider().Type; got != ProviderTypeToolProvider {
		t.Errorf("provider type = %q, want tool_provider", got)
	}
	if got := group.conn.ServerInfo().Name; got != "fake-tools" {
		t.Errorf("serverInfo.name = %q, want fake-tools", got)
	}
	if got := group.conn.ProtocolVersion(); got != ProtocolVersion {
		t.Errorf("protocol version = %q, want %q", got, ProtocolVersion)
	}
	errorResult, err := group.tool.CallTool(ctx, "missing", nil)
	if err != nil {
		t.Fatalf("CallTool(missing): %v", err)
	}
	if !errorResult.IsError {
		t.Error("CallTool(missing).IsError = false, want true")
	}
	if stats := group.transport.Stats(); stats.Sent < 3 || stats.Received < 3 {
		t.Errorf("stats = %+v, want at least three sent and received", stats)
	}
}

// TestStdioCorrelatesOutOfOrderResponses answers three concurrent calls in
// reverse order and checks each caller got its own result.
func TestStdioCorrelatesOutOfOrderResponses(t *testing.T) {
	group := newToolFixture(t)
	group.provider.silence(MethodToolsCall)

	type outcome struct {
		index int
		text  string
		err   error
	}
	results := make(chan outcome, 3)
	for index := 0; index < 3; index++ {
		go func(index int) {
			result, err := group.tool.CallTool(context.Background(), "echo",
				json.RawMessage(fmt.Sprintf(`{"text":"call-%d"}`, index)))
			text := ""
			if err == nil && len(result.Content) > 0 {
				text = result.Content[0].Text
			}
			results <- outcome{index: index, text: text, err: err}
		}(index)
	}
	var calls []*Request
	deadline := time.Now().Add(testTimeout)
	for len(calls) < 3 && time.Now().Before(deadline) {
		calls = nil
		for _, request := range group.provider.requests() {
			if request.Method == MethodToolsCall {
				calls = append(calls, request)
			}
		}
		time.Sleep(time.Millisecond)
	}
	if len(calls) != 3 {
		t.Fatalf("saw %d tools/call requests, want 3", len(calls))
	}
	// Answer newest first: correlation must be by id, not by arrival order.
	for index := len(calls) - 1; index >= 0; index-- {
		call := calls[index]
		var params callToolParams
		decodeParams(t, call.Params, &params)
		var args struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(params.Arguments, &args)
		group.provider.respond(call.ID, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": args.Text}},
		})
	}
	seen := make(map[int]string, 3)
	for index := 0; index < 3; index++ {
		select {
		case out := <-results:
			if out.err != nil {
				t.Fatalf("call %d: %v", out.index, out.err)
			}
			seen[out.index] = out.text
		case <-time.After(testTimeout):
			t.Fatal("timed out waiting for concurrent calls")
		}
	}
	for index := 0; index < 3; index++ {
		want := fmt.Sprintf("call-%d", index)
		if seen[index] != want {
			t.Errorf("call %d = %q, want %q", index, seen[index], want)
		}
	}
}

func TestStdioBadFrameIsSkippedThenResponseArrives(t *testing.T) {
	group := newToolFixture(t)
	group.provider.silence(MethodToolsCall)

	done := make(chan struct{})
	var callErr error
	var callResult ToolResult
	go func() {
		callResult, callErr = group.tool.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"ok"}`))
		close(done)
	}()
	request := group.provider.waitForRequest(MethodToolsCall)

	// A stray log line, an empty line and a truncated frame before the real
	// response: the connection must survive all three.
	group.provider.writeRaw([]byte("INFO provider starting up\n"))
	group.provider.writeRaw([]byte("\n"))
	group.provider.writeRaw([]byte(`{"jsonrpc":"2.0","id":99,"result":{"oops"`))
	group.provider.writeRaw([]byte("\n"))
	group.provider.respond(request.ID, map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "ok"}},
	})

	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("call did not settle after bad frames")
	}
	if callErr != nil {
		t.Fatalf("call failed: %v", callErr)
	}
	if len(callResult.Content) != 1 || callResult.Content[0].Text != "ok" {
		t.Fatalf("result = %+v, want text ok", callResult)
	}
	if !group.diags.hasKind(DiagMessageInvalid) {
		t.Errorf("diagnostics = %s, want an invalid-message diagnostic", group.diags.all())
	}
	if state := group.conn.Health().State; state != HealthOK && state != HealthDegraded {
		t.Errorf("health = %q, want ok or degraded after a skippable frame", state)
	}
}

// TestStdioBackpressureStallsTheProvider is the spec's "慢消费者" scenario: a
// slow consumer must stop the provider, and the queue must stay inside its
// configured bounds.
func TestStdioBackpressureStallsTheProvider(t *testing.T) {
	const (
		maxQueued      = 2
		maxQueuedBytes = 1 << 16
		produced       = 12
	)
	transport, provider := startFakeProvider(t, StdioTransportOptions{
		MaxQueuedMessages: maxQueued,
		MaxQueuedBytes:    maxQueuedBytes,
	})
	if err := transport.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The producer writes as fast as the pipe allows; it will block once the
	// consumer stops reading.
	go func() {
		for index := 0; index < produced; index++ {
			provider.sendNotification("stream.tick", map[string]any{"index": index})
		}
	}()
	time.Sleep(150 * time.Millisecond)

	stats := transport.Stats()
	if stats.PeakQueued > maxQueued {
		t.Errorf("peak queued = %d, want <= %d", stats.PeakQueued, maxQueued)
	}
	if stats.PeakQueuedBytes > maxQueuedBytes {
		t.Errorf("peak queued bytes = %d, want <= %d", stats.PeakQueuedBytes, maxQueuedBytes)
	}
	if stats.Queued > maxQueued {
		t.Errorf("queued = %d, want <= %d", stats.Queued, maxQueued)
	}

	// Draining must yield every produced frame, in order, with no loss.
	for index := 0; index < produced; index++ {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		message, err := transport.Receive(ctx)
		cancel()
		if err != nil {
			t.Fatalf("Receive %d: %v", index, err)
		}
		notification, ok := message.(*Notification)
		if !ok {
			t.Fatalf("message %d is %T, want *Notification", index, message)
		}
		var params struct {
			Index int `json:"index"`
		}
		decodeParams(t, notification.Params, &params)
		if params.Index != index {
			t.Fatalf("message %d carried index %d: frames were lost or reordered", index, params.Index)
		}
	}
}

func TestStdioPeerCrashFailsPendingCall(t *testing.T) {
	group := newToolFixture(t)
	group.provider.silence(MethodToolsCall)

	failed := make(chan error, 1)
	go func() {
		_, err := group.tool.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"lost"}`))
		failed <- err
	}()
	group.provider.waitForRequest(MethodToolsCall)
	// The provider dies mid-call.
	group.provider.close()

	select {
	case err := <-failed:
		var unhealthy *UnhealthyError
		if !errors.As(err, &unhealthy) {
			t.Fatalf("error = %v, want *UnhealthyError", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("pending call did not fail after the provider died")
	}
	if state := group.conn.Health().State; state != HealthUnhealthy {
		t.Errorf("health = %q, want unhealthy", state)
	}
	select {
	case <-group.conn.Done():
	case <-time.After(testTimeout):
		t.Fatal("connection did not end after the provider died")
	}
	// A later call fails fast instead of hanging.
	if _, err := group.tool.ListTools(context.Background()); err == nil {
		t.Fatal("ListTools succeeded on an unhealthy connection")
	}
}

func TestStdioSendRefusesOversizeFrame(t *testing.T) {
	transport, _ := startFakeProvider(t, StdioTransportOptions{MaxFrameBytes: 64})
	if err := transport.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	big, err := json.Marshal(&Notification{JSONRPC: JSONRPCVersion, Method: "x", Params: json.RawMessage(`"` + strings.Repeat("y", 128) + `"`)})
	if err != nil {
		t.Fatal(err)
	}
	message, err := ParseMessage(big)
	if err != nil {
		t.Fatal(err)
	}
	sendErr := transport.Send(context.Background(), message)
	var frameErr *FrameError
	if !errors.As(sendErr, &frameErr) || frameErr.Direction != "outbound" {
		t.Fatalf("Send error = %v, want outbound *FrameError", sendErr)
	}
}

func TestStdioCloseDrainsTerminalAndIsIdempotent(t *testing.T) {
	transport, provider := startFakeProvider(t, StdioTransportOptions{})
	if err := transport.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	provider.sendNotification("one.last", nil)
	message, err := transport.Receive(context.Background())
	if err != nil || message == nil {
		t.Fatalf("Receive = %v, %v; want the queued message", message, err)
	}
	if err := transport.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := transport.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if _, err := transport.Receive(ctx); err == nil {
		t.Fatal("Receive after Close returned nil error")
	}
	if err := transport.Send(context.Background(), &Notification{JSONRPC: JSONRPCVersion, Method: "late"}); err == nil {
		t.Fatal("Send after Close returned nil error")
	}
}

// TestStdioHalfFrameAtEOFReportsAndEnds: a truncated frame produced no message,
// is diagnosed, and then the stream ends - a pending call fails instead of
// hanging on bytes that will never complete.
func TestStdioHalfFrameAtEOFReportsAndEnds(t *testing.T) {
	transport, provider := startFakeProvider(t, StdioTransportOptions{})
	diags := &diagnosticLog{}
	transport.SetDiagnosticHandler(diags.add)
	if err := transport.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	provider.writeRaw([]byte(`{"jsonrpc":"2.0","id":1`))
	provider.close()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if _, err := transport.Receive(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("Receive = %v, want io.EOF after a truncated final frame", err)
	}
	if !diags.hasKind(DiagFrameMalformed) {
		t.Errorf("diagnostics = %s, want a malformed-frame diagnostic", diags.all())
	}
}

// Helper process: the test binary re-executes itself and speaks the protocol
// on stdin/stdout, exactly like the DSH sidecar will. Extra modes support the
// environment-hygiene and process-tree tests.
const (
	gatewayHelperEnv      = "KEEL_GATEWAY_STDIO_HELPER"
	gatewayHelperEnvDump  = "KEEL_GATEWAY_HELPER_ENV_DUMP"
	gatewayHelperOrphan   = "KEEL_GATEWAY_HELPER_ORPHAN"
	gatewayOrphanChildEnv = "KEEL_GATEWAY_ORPHAN_CHILD"
)

func TestGatewayHelperProcess(t *testing.T) {
	if os.Getenv(gatewayHelperEnv) != "1" {
		t.Skip("helper process entry point; set " + gatewayHelperEnv + "=1")
	}
	os.Exit(runGatewayHelper())
}

// TestGatewayOrphanChild is the grandchild of the test process: it does not
// speak the protocol and never reads stdin, so only a process-group kill can
// reap it. It stays alive until signalled.
func TestGatewayOrphanChild(t *testing.T) {
	if os.Getenv(gatewayOrphanChildEnv) != "1" {
		t.Skip("orphan child entry point; set " + gatewayOrphanChildEnv + "=1")
	}
	fmt.Fprintf(os.Stderr, "orphan alive pid %d\n", os.Getpid())
	for {
		time.Sleep(time.Hour)
	}
}

func runGatewayHelper() int {
	if os.Getenv(gatewayHelperEnvDump) == "1" {
		for _, entry := range os.Environ() {
			fmt.Fprintln(os.Stderr, "ENV "+entry)
		}
		return 0
	}
	if os.Getenv(gatewayHelperOrphan) == "1" {
		child := exec.Command(os.Args[0], "-test.run=^TestGatewayOrphanChild$")
		child.Env = append(os.Environ(), gatewayOrphanChildEnv+"=1")
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "orphan start failed:", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "orphan child pid %d\n", child.Process.Pid)
	}
	reader := NewFrameReader(os.Stdin, 1<<20)
	writer := NewFrameWriter(os.Stdout, 1<<20)
	crash := os.Getenv("KEEL_GATEWAY_HELPER_CRASH") == "1"
	for {
		data, err := reader.ReadFrame()
		if err != nil {
			return 0
		}
		message, err := ParseMessage(data)
		if err != nil {
			continue
		}
		request, ok := message.(*Request)
		if !ok {
			continue
		}
		if crash && request.Method == MethodToolsCall {
			// Die mid-call: the client must surface this as a provider crash.
			return 7
		}
		var result any
		switch request.Method {
		case MethodInitialize:
			result = map[string]any{
				"protocolVersion": ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "helper-provider", "version": "1.0.0"},
				"provider":        map[string]any{"type": string(ProviderTypeToolProvider)},
			}
		case MethodToolsList:
			result = map[string]any{
				"tools": []any{map[string]any{"name": "ping", "inputSchema": map[string]any{"type": "object"}}},
			}
		case MethodToolsCall:
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "pong"}}}
		default:
			response, _ := json.Marshal(&ErrorResponse{
				JSONRPC: JSONRPCVersion,
				ID:      request.ID,
				Error:   ErrorObject{Code: CodeMethodNotFound, Message: "Method not found: " + request.Method},
			})
			if err := writer.WriteFrame(response); err != nil {
				return 1
			}
			continue
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return 1
		}
		response, err := json.Marshal(&SuccessResponse{JSONRPC: JSONRPCVersion, ID: request.ID, Result: encoded})
		if err != nil {
			return 1
		}
		if err := writer.WriteFrame(response); err != nil {
			return 1
		}
	}
}

// helperEnv is the explicit environment a spawned helper gets: the allowlisted
// basics plus the helper markers, never the raw host environment.
func helperEnv(extra ...string) []string {
	env := append(MinimalEnv(os.Environ()), gatewayHelperEnv+"=1")
	return append(env, extra...)
}

func helperSpawnOptions(extraEnv ...string) SpawnOptions {
	return SpawnOptions{
		Command:    os.Args[0],
		Args:       []string{"-test.run=^TestGatewayHelperProcess$"},
		Env:        helperEnv(extraEnv...),
		CloseGrace: 2 * time.Second,
	}
}

func TestSpawnStdioHelperProcessRoundTrip(t *testing.T) {
	transport, err := SpawnStdio(helperSpawnOptions())
	if err != nil {
		t.Fatalf("SpawnStdio: %v", err)
	}
	conn := NewConn(ConnOptions{RequestTimeout: 5 * time.Second})
	t.Cleanup(func() { _ = conn.Close() })

	if _, err := conn.Connect(context.Background(), transport); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if transport.PID() == 0 {
		t.Error("PID() = 0 after SpawnStdio")
	}
	if got := conn.ServerInfo().Name; got != "helper-provider" {
		t.Errorf("serverInfo.name = %q, want helper-provider", got)
	}
	tools, err := NewToolProvider(conn).ListTools(context.Background())
	if err != nil || len(tools) != 1 || tools[0].Name != "ping" {
		t.Fatalf("ListTools = %+v, %v", tools, err)
	}
	result, err := NewToolProvider(conn).CallTool(context.Background(), "ping", nil)
	if err != nil || len(result.Content) != 1 || result.Content[0].Text != "pong" {
		t.Fatalf("CallTool = %+v, %v", result, err)
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-transport.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("helper process did not exit after Close")
	}
}

// TestSpawnStdioReportsProviderCrash: the process exit status is what the
// caller needs to record an external session as backend-unavailable.
func TestSpawnStdioReportsProviderCrash(t *testing.T) {
	transport, err := SpawnStdio(helperSpawnOptions("KEEL_GATEWAY_HELPER_CRASH=1"))
	if err != nil {
		t.Fatalf("SpawnStdio: %v", err)
	}
	conn := NewConn(ConnOptions{RequestTimeout: 5 * time.Second})
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Connect(context.Background(), transport); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	_, callErr := NewToolProvider(conn).CallTool(context.Background(), "ping", nil)
	if callErr == nil {
		t.Fatal("CallTool succeeded although the provider crashed")
	}
	var unhealthy *UnhealthyError
	if !errors.As(callErr, &unhealthy) {
		t.Fatalf("error = %v, want *UnhealthyError", callErr)
	}
	if !waitFor(t, testTimeout, func() bool { return transport.ExitState() != nil }) {
		t.Fatalf("no exit status recorded for the crashed provider: %v", callErr)
	}
	if state := transport.ExitState(); state.ExitCode() != 7 {
		t.Errorf("exit code = %d, want 7", state.ExitCode())
	}
	if !strings.Contains(unhealthy.Reason, "exited") && !strings.Contains(unhealthy.Reason, "closed") {
		t.Errorf("reason = %q, want it to mention the provider going away", unhealthy.Reason)
	}
}
