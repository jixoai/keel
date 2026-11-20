// Fake MCP server for the G1/G2 acceptance scenario (change
// feat-mcp-apps-events, P6.1).
//
// It is a deterministic stdio MCP implementation with exactly the surface the
// scenario needs: a tool that declares a UI resource, an app-only tool, a
// plain model tool, a resources/read that returns `text/html;profile=mcp-app`
// with `_meta.ui` metadata, a modern `server/discover`, a legacy `initialize`,
// and `subscriptions/listen` with the three behaviours the crash matrix uses:
//
//	static  one acknowledged subscription that stays open
//	evolve  ack, then one tagged tools/list_changed and a v2 catalogue
//	flap    ack #1 then the server ends the stream; listen #2 is accepted on
//	        the wire but never acknowledged (the host is provably reconnecting)
//
// Every observable step is appended to a JSONL marker file so the harness can
// prove where a SIGKILL landed instead of inferring it from timing.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

const fakeMCPSubscriptionIDKey = "io.modelcontextprotocol/subscriptionId"

func runFakeMCPServer(args []string) int {
	fs := flag.NewFlagSet("fake-mcp-server", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	scenario := fs.String("scenario", "static", "static|evolve|flap")
	eventsPath := fs.String("events", "", "JSONL marker file (optional)")
	toolDelayMS := fs.Int("tool-delay-ms", 0, "delay before answering tools/call")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	server := &fakeMCPServer{
		scenario:  *scenario,
		toolDelay: time.Duration(*toolDelayMS) * time.Millisecond,
		tools:     1,
	}
	if *eventsPath != "" {
		file, err := os.OpenFile(*eventsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake mcp server: open marker file:", err)
			return 1
		}
		defer file.Close()
		server.events = file
	}
	server.record("start", map[string]any{
		"pid":       os.Getpid(),
		"scenario":  server.scenario,
		"toolDelay": server.toolDelay.Milliseconds(),
	})
	fmt.Fprintln(os.Stderr, "fake-mcp-server ready", server.scenario)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		server.handle(line)
	}
	server.record("stdin_closed", nil)
	return 0
}

type fakeMCPServer struct {
	scenario  string
	toolDelay time.Duration
	events    *os.File

	outMu sync.Mutex

	stateMu    sync.Mutex
	tools      int // 1 = dashboard, 2 = dashboard_v2
	evolved    bool
	listenID   json.RawMessage
	listenN    int
	listenFlap int
}

type fakeRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (s *fakeMCPServer) handle(line string) {
	var request fakeRPCRequest
	if err := json.Unmarshal([]byte(line), &request); err != nil {
		s.record("bad_message", map[string]any{"error": err.Error()})
		return
	}
	if len(request.ID) == 0 {
		// A client notification (initialized / cancelled): nothing to answer.
		if request.Method == "notifications/initialized" {
			s.record("initialized", nil)
		}
		return
	}
	switch request.Method {
	case "initialize":
		s.respond(request.ID, map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities": map[string]any{
				"tools":     map[string]any{"listChanged": true},
				"resources": map[string]any{"subscribe": true, "listChanged": true},
				"prompts":   map[string]any{"listChanged": true},
			},
			"serverInfo": map[string]any{"name": "mcp-apps-fixture", "version": "1.0.0"},
		})
	case "server/discover":
		s.respond(request.ID, map[string]any{
			"protocolVersion": "2026-07-28",
			"capabilities": map[string]any{
				"tools":     map[string]any{"listChanged": true},
				"resources": map[string]any{"subscribe": true, "listChanged": true},
				"prompts":   map[string]any{"listChanged": true},
			},
			"serverInfo": map[string]any{"name": "mcp-apps-fixture", "version": "1.0.0"},
		})
	case "tools/list":
		s.stateMu.Lock()
		version := s.tools
		s.stateMu.Unlock()
		s.record("tools_list", map[string]any{"version": version})
		s.respond(request.ID, map[string]any{"tools": fakeTools(version)})
	case "tools/call":
		s.handleToolCall(request)
	case "resources/list":
		s.stateMu.Lock()
		version := s.tools
		s.stateMu.Unlock()
		s.respond(request.ID, map[string]any{"resources": []any{map[string]any{
			"uri":      fakeResourceURI(version),
			"name":     "Dashboard",
			"mimeType": "text/html;profile=mcp-app",
		}}})
	case "resources/read":
		s.handleResourceRead(request)
	case "prompts/list":
		s.respond(request.ID, map[string]any{"prompts": []any{}})
	case "subscriptions/listen":
		s.handleListen(request)
	case "ping":
		s.respond(request.ID, map[string]any{})
	default:
		s.respondError(request.ID, -32601, "Method not found: "+request.Method)
	}
}

func fakeTools(version int) []any {
	uri := fakeResourceURI(version)
	tools := []any{
		map[string]any{
			"name":        "show_dashboard",
			"title":       "Show dashboard",
			"description": "Render the fixture dashboard.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			"_meta": map[string]any{"ui": map[string]any{
				"resourceUri": uri,
				"visibility":  []string{"model", "app"},
			}},
		},
		map[string]any{
			"name":        "refresh",
			"title":       "Refresh dashboard",
			"description": "App-only refresh action.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			"_meta": map[string]any{"ui": map[string]any{
				"resourceUri": uri,
				"visibility":  []string{"app"},
			}},
		},
		map[string]any{
			"name":        "plain",
			"description": "A tool without a UI.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
	if version >= 2 {
		// The post-list_changed catalogue must change the MODEL tool surface too,
		// not only the app document identity.
		tools = append(tools, map[string]any{
			"name":        "show_audit",
			"description": "A v2 model-only tool added by tools/list_changed.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		})
	}
	return tools
}

func fakeResourceURI(version int) string {
	if version >= 2 {
		return "ui://scenario/dashboard_v2"
	}
	return "ui://scenario/dashboard"
}

func fakeHTML(version int) string {
	label := "v1"
	if version >= 2 {
		label = "v2"
	}
	return `<!doctype html><html><head><meta charset="utf-8"><title>MCP Apps fixture ` + label +
		`</title><style>body{font-family:sans-serif}</style></head><body><h1>Fixture dashboard ` + label +
		`</h1><button id="refresh">Refresh</button><script>window.__mcpAppFixture="` + label + `";</script></body></html>`
}

func (s *fakeMCPServer) handleToolCall(request fakeRPCRequest) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(request.Params, &params); err != nil {
		s.respondError(request.ID, -32602, err.Error())
		return
	}
	s.record("tools_call_begin", map[string]any{"name": params.Name})
	if s.toolDelay > 0 {
		time.Sleep(s.toolDelay)
	}
	s.stateMu.Lock()
	version := s.tools
	s.stateMu.Unlock()
	text := ""
	switch params.Name {
	case "show_dashboard":
		text = fmt.Sprintf("dashboard-ok v%d", version)
	case "refresh":
		text = fmt.Sprintf("refresh-ok v%d", version)
	case "plain":
		text = "plain-ok"
	default:
		s.respondError(request.ID, -32602, "unknown tool "+params.Name)
		s.record("tools_call_end", map[string]any{"name": params.Name, "error": true})
		return
	}
	s.respond(request.ID, map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"structuredContent": map[string]any{
			"version": fmt.Sprintf("v%d", version),
			"tool":    params.Name,
		},
	})
	s.record("tools_call_end", map[string]any{"name": params.Name, "text": text})
}

func (s *fakeMCPServer) handleResourceRead(request fakeRPCRequest) {
	var params struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(request.Params, &params); err != nil {
		s.respondError(request.ID, -32602, err.Error())
		return
	}
	s.record("resource_read", map[string]any{"uri": params.URI})
	version := 0
	switch params.URI {
	case "ui://scenario/dashboard":
		version = 1
	case "ui://scenario/dashboard_v2":
		version = 2
	default:
		s.respondError(request.ID, -32602, "resource not found: "+params.URI)
		return
	}
	s.respond(request.ID, map[string]any{"contents": []any{map[string]any{
		"uri":      params.URI,
		"mimeType": "text/html;profile=mcp-app",
		"text":     fakeHTML(version),
		"_meta": map[string]any{"ui": map[string]any{
			"csp": map[string]any{
				"connectDomains": []string{"https://api.fixture.test"},
			},
			"permissions":   map[string]any{"clipboardWrite": true},
			"prefersBorder": true,
		}},
	}}})
}

func (s *fakeMCPServer) handleListen(request fakeRPCRequest) {
	s.stateMu.Lock()
	s.listenN++
	n := s.listenN
	s.listenID = request.ID
	s.stateMu.Unlock()

	var params struct {
		Notifications json.RawMessage `json:"notifications"`
	}
	_ = json.Unmarshal(request.Params, &params)
	accepted := map[string]any{
		"toolsListChanged":     true,
		"promptsListChanged":   true,
		"resourcesListChanged": true,
	}
	if len(params.Notifications) > 0 {
		var filter map[string]any
		if json.Unmarshal(params.Notifications, &filter) == nil {
			for _, key := range []string{"toolsListChanged", "promptsListChanged", "resourcesListChanged"} {
				if _, ok := filter[key]; ok {
					accepted[key] = filter[key]
				}
			}
			if uris, ok := filter["resourceSubscriptions"]; ok {
				accepted["resourceSubscriptions"] = uris
			}
		}
	}
	s.record("listen_begin", map[string]any{"n": n, "id": json.RawMessage(request.ID)})

	if s.scenario == "flap" && n >= 2 {
		// The host sent the reconnect; it is provably waiting for an ack that
		// never arrives (the harness kills it here).
		s.record("listen_stall", map[string]any{"n": n})
		return
	}
	if s.scenario == "reconnect" && n == 1 {
		// A delayed first ack gives the harness a deterministic window to read
		// the pre-ack catalogue generation; the stream then closes so the next
		// listen is a real reconnect, which stalls like flap.
		go func(id json.RawMessage) {
			time.Sleep(4 * time.Second)
			s.notify("notifications/subscriptions/acknowledged", map[string]any{
				"notifications": accepted,
				"_meta":         map[string]any{fakeMCPSubscriptionIDKey: id},
			})
			s.record("listen_ack", map[string]any{"n": 1})
			time.Sleep(400 * time.Millisecond)
			s.respond(id, map[string]any{"resultType": "complete"})
			s.record("listen_closed", map[string]any{"n": 1})
		}(append(json.RawMessage(nil), request.ID...))
		return
	}
	if s.scenario == "reconnect" && n >= 2 {
		s.record("listen_stall", map[string]any{"n": n})
		return
	}

	s.notify("notifications/subscriptions/acknowledged", map[string]any{
		"notifications": accepted,
		"_meta":         map[string]any{fakeMCPSubscriptionIDKey: json.RawMessage(request.ID)},
	})
	s.record("listen_ack", map[string]any{"n": n})

	switch s.scenario {
	case "evolve":
		go func(id json.RawMessage, n int) {
			time.Sleep(700 * time.Millisecond)
			s.stateMu.Lock()
			s.evolved = true
			s.tools = 2
			s.stateMu.Unlock()
			s.notify("notifications/tools/list_changed", map[string]any{
				"_meta": map[string]any{fakeMCPSubscriptionIDKey: id},
			})
			s.record("tools_list_changed_sent", map[string]any{"n": n, "version": 2})
		}(append(json.RawMessage(nil), request.ID...), n)
	case "flap":
		go func(id json.RawMessage, n int) {
			time.Sleep(250 * time.Millisecond)
			s.respond(id, map[string]any{"resultType": "complete"})
			s.record("listen_closed", map[string]any{"n": n})
		}(append(json.RawMessage(nil), request.ID...), n)
	}
}

func (s *fakeMCPServer) respond(id json.RawMessage, result any) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
}

func (s *fakeMCPServer) respondError(id json.RawMessage, code int, message string) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "error": map[string]any{
		"code": code, "message": message,
	}})
}

func (s *fakeMCPServer) notify(method string, params map[string]any) {
	s.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (s *fakeMCPServer) write(message map[string]any) {
	raw, err := json.Marshal(message)
	if err != nil {
		return
	}
	s.outMu.Lock()
	_, _ = os.Stdout.Write(append(raw, '\n'))
	s.outMu.Unlock()
}

func (s *fakeMCPServer) record(event string, fields map[string]any) {
	if s.events == nil {
		return
	}
	entry := map[string]any{
		"event": event,
		"ts":    time.Now().UTC().Format(time.RFC3339Nano),
		"pid":   os.Getpid(),
	}
	for key, value := range fields {
		entry[key] = value
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return
	}
	s.outMu.Lock()
	_, _ = s.events.Write(append(raw, '\n'))
	_ = s.events.Sync()
	s.outMu.Unlock()
}
