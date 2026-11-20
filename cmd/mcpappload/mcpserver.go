// mcpserver mode: a real Streamable HTTP MCP server for the G5 fixture.
//
// It speaks the modern (2026-07-28) dialect the host's listen supervisor uses:
// server/discover, stateless POSTs with Mcp-Method/Mcp-Name, tools/list with
// `_meta.ui.resourceUri`, resources/read returning a text/html;profile=mcp-app
// document, tools/call, and subscriptions/listen as an SSE stream whose first
// frame is the tagged acknowledgement.
//
// The control surface is what makes the chaos phase deterministic:
//
//	POST /control/crash          SIGKILL this process (Docker restarts it)
//	POST /control/drop           close every established listen stream
//	POST /control/bump           revision++ and broadcast resources/list_changed
//	POST /control/mode?listen=reject|accept
//	GET  /control/status         revision, mode, listen attempt timeline, counters
//
// The document revision is persisted to a file before the process serves, so a
// SIGKILL restart publishes new content: the host's post-reconnect recompute is
// observable as a catalogue hash change.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const subscriptionIDMetaKey = "io.modelcontextprotocol/subscriptionId"

type fakeMCP struct {
	mu           sync.Mutex
	apps         int
	docBytes     int
	revision     int64
	statePath    string
	rejectListen bool
	listeners    map[string]*fakeListen
	attempts     []listenAttempt
	docs         map[string]string
	startedAt    time.Time
	calls        int64
	reads        int64
	drops        int64
	crashes      int64
}

type fakeListen struct {
	key  string
	id   json.RawMessage
	ch   chan []byte
	done chan struct{}
	once sync.Once
}

func (l *fakeListen) close() {
	l.once.Do(func() { close(l.done) })
}

type listenAttempt struct {
	At      time.Time `json:"at"`
	UnixMs  int64     `json:"unixMs"`
	Outcome string    `json:"outcome"`
	ID      string    `json:"id,omitempty"`
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func runMCPServer(args []string) error {
	fs := flag.NewFlagSet("mcpserver", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:13689", "listen address")
	apps := fs.Int("apps", 20, "number of app resources")
	docBytes := fs.Int("doc-bytes", 1536*1024, "document size in bytes")
	state := fs.String("state", "", "revision state file (persists across SIGKILL restarts)")
	name := fs.String("name", "perf-fleet", "MCP server name in serverInfo")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *apps <= 0 || *apps > 200 {
		return fmt.Errorf("-apps must be in 1..200")
	}
	if *docBytes < 1024 || *docBytes > 2<<20 {
		return fmt.Errorf("-doc-bytes must be in 1024..2097152")
	}
	server := &fakeMCP{
		apps:      *apps,
		docBytes:  *docBytes,
		statePath: *state,
		listeners: make(map[string]*fakeListen),
		docs:      make(map[string]string),
		startedAt: time.Now(),
	}
	server.revision = bumpRevisionFile(*state)
	if *name == "" {
		*name = "perf-fleet"
	}
	serverName = *name

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/", server)
	httpServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	fmt.Fprintf(os.Stderr, "mcpappload mcpserver: %s apps=%d docBytes=%d revision=%d\n",
		ln.Addr(), server.apps, server.docBytes, server.revision)
	return httpServer.Serve(ln)
}

var serverName = "perf-fleet"

// bumpRevisionFile increments the persisted revision and returns it. A missing
// or unreadable file starts at 1; a corrupt value is replaced rather than
// blocking the fixture.
func bumpRevisionFile(path string) int64 {
	var current int64
	if path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			if parsed, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); err == nil && parsed > 0 {
				current = parsed
			}
		}
	}
	current++
	if path == "" {
		return current
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(strconv.FormatInt(current, 10)), 0o644); err == nil {
			_ = os.Rename(tmp, path)
		}
	}
	return current
}

func (f *fakeMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	case r.URL.Path == "/mcp":
		f.handleMCP(w, r)
	case strings.HasPrefix(r.URL.Path, "/control/"):
		f.handleControl(w, r)
	default:
		http.NotFound(w, r)
	}
}

// --- control -------------------------------------------------------------

func (f *fakeMCP) handleControl(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/control/status":
		f.mu.Lock()
		attempts := append([]listenAttempt(nil), f.attempts...)
		status := struct {
			StartedAt       string          `json:"startedAt"`
			StartedAtUnixMs int64           `json:"startedAtUnixMs"`
			Revision        int64           `json:"revision"`
			Mode            string          `json:"mode"`
			Apps            int             `json:"apps"`
			DocBytes        int             `json:"docBytes"`
			ActiveListeners int             `json:"activeListeners"`
			ListenAttempts  []listenAttempt `json:"listenAttempts"`
			Drops           int64           `json:"drops"`
			Crashes         int64           `json:"crashes"`
			Calls           int64           `json:"calls"`
			Reads           int64           `json:"reads"`
		}{
			StartedAt:       f.startedAt.UTC().Format(time.RFC3339Nano),
			StartedAtUnixMs: f.startedAt.UnixMilli(),
			Revision:        f.revision,
			Mode:            f.mode(),
			Apps:            f.apps,
			DocBytes:        f.docBytes,
			ActiveListeners: len(f.listeners),
			ListenAttempts:  attempts,
			Drops:           f.drops,
			Crashes:         f.crashes,
			Calls:           f.calls,
			Reads:           f.reads,
		}
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, status)
	case "/control/bump":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		revision := f.bump()
		f.broadcast("notifications/resources/list_changed")
		writeJSON(w, http.StatusOK, map[string]any{"revision": revision})
	case "/control/drop":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		f.mu.Lock()
		f.drops++
		listeners := make([]*fakeListen, 0, len(f.listeners))
		for _, listener := range f.listeners {
			listeners = append(listeners, listener)
		}
		f.mu.Unlock()
		for _, listener := range listeners {
			listener.close()
		}
		writeJSON(w, http.StatusOK, map[string]any{"dropped": len(listeners)})
	case "/control/crash":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		f.mu.Lock()
		f.crashes++
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "crashing\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// Give the response a moment to leave the socket, then die by SIGKILL:
		// Docker's restart policy replaces the process, and the persisted
		// revision makes the new content observable.
		time.AfterFunc(100*time.Millisecond, func() {
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		})
	case "/control/mode":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		mode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("listen")))
		switch mode {
		case "reject", "accept":
		default:
			http.Error(w, "listen must be reject|accept", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.rejectListen = mode == "reject"
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"mode": mode})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeMCP) mode() string {
	if f.rejectListen {
		return "reject"
	}
	return "accept"
}

func (f *fakeMCP) bump() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revision++
	f.docs = make(map[string]string)
	revision := f.revision
	if f.statePath != "" {
		tmp := f.statePath + ".tmp"
		if err := os.WriteFile(tmp, []byte(strconv.FormatInt(revision, 10)), 0o644); err == nil {
			_ = os.Rename(tmp, f.statePath)
		}
	}
	return revision
}

func (f *fakeMCP) broadcast(method string) {
	f.mu.Lock()
	listeners := make([]*fakeListen, 0, len(f.listeners))
	for _, listener := range f.listeners {
		listeners = append(listeners, listener)
	}
	f.mu.Unlock()
	for _, listener := range listeners {
		frame := f.notificationFrame(listener, method, nil)
		select {
		case listener.ch <- frame:
		default:
			// A slow listener is dropped, not blocked: the reconnect recompute
			// is the recovery path.
			listener.close()
		}
	}
}

// --- MCP ----------------------------------------------------------------

func (f *fakeMCP) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var request rpcRequest
	if err := json.Unmarshal(body, &request); err != nil || request.Method == "" {
		http.Error(w, "invalid JSON-RPC request", http.StatusBadRequest)
		return
	}
	if header := strings.TrimSpace(r.Header.Get("Mcp-Method")); header != "" && header != request.Method {
		writeRPCError(w, request.ID, http.StatusBadRequest, -32600,
			fmt.Sprintf("Mcp-Method %q does not match body method %q", header, request.Method))
		return
	}
	if name := strings.TrimSpace(r.Header.Get("Mcp-Name")); name != "" {
		want := mcpNameFor(request.Method, request.Params)
		if want != "" && name != want {
			writeRPCError(w, request.ID, http.StatusBadRequest, -32600,
				fmt.Sprintf("Mcp-Name %q does not match %q", name, want))
			return
		}
	}
	if len(request.ID) == 0 {
		// Notification: the host never awaits a body.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch request.Method {
	case "server/discover", "initialize":
		writeRPCResult(w, request.ID, map[string]any{
			"protocolVersion": "2026-07-28",
			"capabilities": map[string]any{
				"tools":     map[string]any{"listChanged": true},
				"resources": map[string]any{"subscribe": true, "listChanged": true},
				"prompts":   map[string]any{"listChanged": true},
			},
			"serverInfo": map[string]any{"name": serverName, "version": "1.0.0-perf"},
		})
	case "tools/list":
		writeRPCResult(w, request.ID, map[string]any{"tools": f.tools()})
	case "resources/list":
		writeRPCResult(w, request.ID, map[string]any{"resources": f.resources()})
	case "prompts/list":
		writeRPCResult(w, request.ID, map[string]any{"prompts": []any{}})
	case "resources/read":
		f.handleResourceRead(w, request)
	case "tools/call":
		f.handleToolCall(w, request)
	case "subscriptions/listen":
		f.handleListen(w, r, request)
	default:
		writeRPCError(w, request.ID, http.StatusOK, -32601, "method not found: "+request.Method)
	}
}

func (f *fakeMCP) tools() []any {
	tools := make([]any, 0, f.apps)
	for i := 0; i < f.apps; i++ {
		uri := fmt.Sprintf("ui://perf/app-%d", i)
		tools = append(tools, map[string]any{
			"name":        fmt.Sprintf("ping_%d", i),
			"title":       fmt.Sprintf("Perf app %d", i),
			"description": "G5 perf fixture tool",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"i":       map[string]any{"type": "number"},
					"delayMs": map[string]any{"type": "number"},
				},
			},
			"_meta": map[string]any{
				"ui": map[string]any{
					"resourceUri": uri,
					"visibility":  []string{"app"},
				},
			},
		})
	}
	return tools
}

func (f *fakeMCP) resources() []any {
	out := make([]any, 0, f.apps)
	for i := 0; i < f.apps; i++ {
		out = append(out, map[string]any{
			"uri":      fmt.Sprintf("ui://perf/app-%d", i),
			"name":     fmt.Sprintf("Perf app %d", i),
			"mimeType": "text/html;profile=mcp-app",
		})
	}
	return out
}

func (f *fakeMCP) handleResourceRead(w http.ResponseWriter, request rpcRequest) {
	var params struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(request.Params, &params); err != nil || params.URI == "" {
		writeRPCError(w, request.ID, http.StatusOK, -32602, "resources/read requires uri")
		return
	}
	var index int
	if _, err := fmt.Sscanf(params.URI, "ui://perf/app-%d", &index); err != nil || index < 0 || index >= f.apps {
		writeRPCError(w, request.ID, http.StatusOK, -32602, "unknown app resource: "+params.URI)
		return
	}
	doc := f.document(f.currentRevision(), index)
	f.mu.Lock()
	f.reads++
	f.mu.Unlock()
	writeRPCResult(w, request.ID, map[string]any{
		"contents": []any{map[string]any{
			"uri":      params.URI,
			"mimeType": "text/html;profile=mcp-app",
			"text":     doc,
		}},
	})
}

func (f *fakeMCP) handleToolCall(w http.ResponseWriter, request rpcRequest) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(request.Params, &params); err != nil || params.Name == "" {
		writeRPCError(w, request.ID, http.StatusOK, -32602, "tools/call requires name")
		return
	}
	var index int
	if _, err := fmt.Sscanf(params.Name, "ping_%d", &index); err != nil || index < 0 || index >= f.apps {
		writeRPCError(w, request.ID, http.StatusOK, -32602, "unknown tool: "+params.Name)
		return
	}
	var args struct {
		DelayMS float64 `json:"delayMs"`
	}
	if len(params.Arguments) > 0 {
		_ = json.Unmarshal(params.Arguments, &args)
	}
	if args.DelayMS > 0 {
		delay := time.Duration(args.DelayMS * float64(time.Millisecond))
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		time.Sleep(delay)
	}
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	writeRPCResult(w, request.ID, map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "ok " + params.Name}},
	})
}

func (f *fakeMCP) handleListen(w http.ResponseWriter, r *http.Request, request rpcRequest) {
	f.mu.Lock()
	reject := f.rejectListen
	f.mu.Unlock()
	if reject {
		f.recordAttempt("rejected", request.ID)
		writeRPCError(w, request.ID, http.StatusServiceUnavailable, -32603, "listen rejected by fixture mode")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		f.recordAttempt("rejected", request.ID)
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	accepted := listenFilter(request.Params)
	key := string(request.ID)
	listener := &fakeListen{
		key:  key,
		id:   append(json.RawMessage(nil), request.ID...),
		ch:   make(chan []byte, 32),
		done: make(chan struct{}),
	}
	f.mu.Lock()
	if previous := f.listeners[key]; previous != nil {
		previous.close()
	}
	f.listeners[key] = listener
	f.mu.Unlock()
	f.recordAttempt("accepted", request.ID)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ack := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/subscriptions/acknowledged",
		"params": map[string]any{
			"notifications": accepted,
			"_meta":         map[string]any{subscriptionIDMetaKey: listener.id},
		},
	}
	if raw, err := json.Marshal(ack); err == nil {
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", raw)
		flusher.Flush()
	}

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	defer func() {
		f.mu.Lock()
		if f.listeners[key] == listener {
			delete(f.listeners, key)
		}
		f.mu.Unlock()
		listener.close()
	}()
	for {
		select {
		case frame := <-listener.ch:
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", frame)
			flusher.Flush()
		case <-keepalive.C:
			_, _ = io.WriteString(w, ": keepalive\n\n")
			flusher.Flush()
		case <-listener.done:
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (f *fakeMCP) recordAttempt(outcome string, id json.RawMessage) {
	attempt := listenAttempt{
		At:      time.Now().UTC(),
		UnixMs:  time.Now().UnixMilli(),
		Outcome: outcome,
		ID:      string(id),
	}
	f.mu.Lock()
	f.attempts = append(f.attempts, attempt)
	if len(f.attempts) > 2000 {
		f.attempts = append([]listenAttempt(nil), f.attempts[len(f.attempts)-2000:]...)
	}
	f.mu.Unlock()
}

func (f *fakeMCP) notificationFrame(listener *fakeListen, method string, extra map[string]any) []byte {
	params := map[string]any{
		"_meta": map[string]any{subscriptionIDMetaKey: listener.id},
	}
	for key, value := range extra {
		params[key] = value
	}
	frame, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
	return frame
}

func listenFilter(params json.RawMessage) map[string]any {
	accepted := map[string]any{
		"toolsListChanged":     true,
		"promptsListChanged":   true,
		"resourcesListChanged": true,
	}
	if len(params) == 0 {
		return accepted
	}
	var envelope struct {
		Notifications map[string]any `json:"notifications"`
	}
	if json.Unmarshal(params, &envelope) != nil || envelope.Notifications == nil {
		return accepted
	}
	for key, value := range envelope.Notifications {
		switch key {
		case "toolsListChanged", "promptsListChanged", "resourcesListChanged":
			accepted[key] = value
		case "resourceSubscriptions":
			accepted[key] = value
		}
	}
	return accepted
}

func mcpNameFor(method string, params json.RawMessage) string {
	var source string
	switch method {
	case "tools/call", "prompts/get":
		source = "name"
	case "resources/read":
		source = "uri"
	default:
		return ""
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(params, &fields) != nil {
		return ""
	}
	var value string
	if json.Unmarshal(fields[source], &value) != nil {
		return ""
	}
	return value
}

func (f *fakeMCP) currentRevision() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revision
}

func (f *fakeMCP) document(revision int64, app int) string {
	key := strconv.FormatInt(revision, 10) + ":" + strconv.Itoa(app)
	f.mu.Lock()
	if doc, ok := f.docs[key]; ok {
		f.mu.Unlock()
		return doc
	}
	f.mu.Unlock()

	prefix := fmt.Sprintf("<!doctype html><html><head><meta charset=\"utf-8\"><title>Perf app %d</title></head><body><h1>Perf app %d</h1><!-- revision=%d app=%d -->", app, app, revision, app)
	suffix := "</body></html>"
	pad := f.docBytes - len(prefix) - len(suffix)
	if pad < 0 {
		pad = 0
	}
	doc := prefix + strings.Repeat("x", pad) + suffix

	f.mu.Lock()
	if f.docs == nil {
		f.docs = make(map[string]string)
	}
	f.docs[key] = doc
	f.mu.Unlock()
	return doc
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, http.StatusOK, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, status, code int, message string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	writeJSON(w, status, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   rpcError{Code: code, Message: message},
	})
}
