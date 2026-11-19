// P5 client tests: MRTR (P5.2), legacy server requests/capabilities (P5.3) and
// era gating (P5.4). Fake servers run over the in-memory transport and over
// httptest; no test sleeps for synchronization.

package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
)

// --- P5.1 resultType parsing ---------------------------------------------

func TestResultTypeThreeStates(t *testing.T) {
	t.Run("missing resultType is complete", func(t *testing.T) {
		result, err := validateCallToolResult(json.RawMessage(`{"content":[]}`))
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		if result.ResultType != ResultTypeComplete || result.InputRequired != nil {
			t.Fatalf("result = %+v, want complete", result)
		}
	})
	t.Run("explicit complete", func(t *testing.T) {
		result, err := validateCallToolResult(json.RawMessage(`{"resultType":"complete","content":[]}`))
		if err != nil || result.ResultType != ResultTypeComplete {
			t.Fatalf("result = %+v err = %v", result, err)
		}
	})
	t.Run("input_required parses requests and state", func(t *testing.T) {
		raw := json.RawMessage(`{"resultType":"input_required","requestState":"opaque","inputRequests":{
			"login":{"method":"elicitation/create","params":{"message":"sign in"}}}}`)
		result, err := validateCallToolResult(raw)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		if result.ResultType != ResultTypeInputRequired || result.InputRequired == nil {
			t.Fatalf("result = %+v", result)
		}
		request := result.InputRequired.InputRequests["login"]
		if request.Method != "elicitation/create" {
			t.Fatalf("request = %+v", request)
		}
		if string(result.InputRequired.RequestState) != `"opaque"` {
			t.Fatalf("requestState = %s, want the raw JSON string", result.InputRequired.RequestState)
		}
	})
	t.Run("unknown resultType is a protocol error", func(t *testing.T) {
		_, err := validateCallToolResult(json.RawMessage(`{"resultType":"someday","content":[]}`))
		var rpcErr *McpError
		if !errors.As(err, &rpcErr) || rpcErr.Code != CodeInvalidRequest {
			t.Fatalf("err = %v, want invalid request", err)
		}
	})
	t.Run("input_required without requests or state is invalid", func(t *testing.T) {
		_, err := validateCallToolResult(json.RawMessage(`{"resultType":"input_required"}`))
		var rpcErr *McpError
		if !errors.As(err, &rpcErr) || rpcErr.Code != CodeInvalidRequest {
			t.Fatalf("err = %v, want invalid request", err)
		}
	})
	t.Run("input_required with only an empty requestState is valid", func(t *testing.T) {
		result, err := validateCallToolResult(json.RawMessage(`{"resultType":"input_required","requestState":""}`))
		if err != nil || result.InputRequired == nil || !result.InputRequired.HasRequestState {
			t.Fatalf("result = %+v err = %v", result, err)
		}
		if string(result.InputRequired.RequestState) != `""` {
			t.Fatalf("requestState = %s, want the empty string preserved", result.InputRequired.RequestState)
		}
	})
}

// --- P5.2 MRTR end to end ------------------------------------------------

// TestMRTRRoundTripEchoesStateAndUsesNewIDs is the hard assertion of P5.2: the
// retry carries a NEW JSON-RPC id and the server's requestState byte-for-byte.
func TestMRTRRoundTripEchoesStateAndUsesNewIDs(t *testing.T) {
	clientTransport, server := newTestServer(t)
	var mu sync.Mutex
	attempts := 0
	server.setHandler("tools/call", func(params json.RawMessage) (any, *ErrorObject) {
		mu.Lock()
		attempts++
		attempt := attempts
		mu.Unlock()
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(params, &fields); err != nil {
			return nil, &ErrorObject{Code: CodeInvalidParams, Message: err.Error()}
		}
		if attempt == 1 {
			return map[string]any{
				"resultType":   "input_required",
				"requestState": `opaque-"state-1"`,
				"inputRequests": map[string]any{
					"login": map[string]any{
						"method": "elicitation/create",
						"params": map[string]any{"message": "Sign in"},
					},
				},
			}, nil
		}
		return map[string]any{
			"resultType": "complete",
			"content":    []any{map[string]any{"type": "text", "text": "signed in"}},
		}, nil
	})

	var handlerStates []string
	client := NewClient(ClientOptions{
		Name: "p5-mrtr", Version: "1.0.0",
		InputRequests: func(_ context.Context, request InputRequiredResult, state *InputRequestState) (map[string]json.RawMessage, error) {
			handlerStates = append(handlerStates, string(request.RequestState))
			if state.Round != 1 {
				t.Errorf("handler round = %d, want 1", state.Round)
			}
			return map[string]json.RawMessage{
				"login": json.RawMessage(`{"action":"accept","content":{"user":"alice"}}`),
			}, nil
		},
	})
	if _, err := client.Connect(testContext(t), clientTransport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	result, err := client.CallTool(testContext(t), "login", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.ResultType != ResultTypeComplete || len(result.Content) != 1 || result.Content[0].Text != "signed in" {
		t.Fatalf("result = %+v", result)
	}
	if len(handlerStates) != 1 || handlerStates[0] != `"opaque-\"state-1\""` {
		t.Fatalf("handler states = %#v, want the server bytes verbatim", handlerStates)
	}

	requests := server.requests("tools/call")
	if len(requests) != 2 {
		t.Fatalf("tools/call requests = %d, want 2", len(requests))
	}
	if requests[0].ID.Key() == requests[1].ID.Key() {
		t.Fatalf("retry reused JSON-RPC id %s (MUST be a new id)", requests[0].ID)
	}
	var retry struct {
		InputResponses map[string]json.RawMessage `json:"inputResponses"`
		RequestState   json.RawMessage            `json:"requestState"`
	}
	if err := json.Unmarshal(requests[1].Params, &retry); err != nil {
		t.Fatalf("decode retry params: %v", err)
	}
	if string(retry.RequestState) != `"opaque-\"state-1\""` {
		t.Fatalf("requestState = %s, want the exact server bytes", retry.RequestState)
	}
	if login := retry.InputResponses["login"]; string(login) != `{"action":"accept","content":{"user":"alice"}}` {
		t.Fatalf("inputResponses.login = %s, want the result object directly", login)
	}
}

func TestMRTRRoundBudget(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.setHandler("tools/call", func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{
			"resultType":   "input_required",
			"requestState": "state",
			"inputRequests": map[string]any{
				"again": map[string]any{"method": "elicitation/create", "params": map[string]any{"message": "again"}},
			},
		}, nil
	})
	rounds := 0
	client := NewClient(ClientOptions{
		Name: "p5-budget", Version: "1.0.0",
		InputRequests: func(context.Context, InputRequiredResult, *InputRequestState) (map[string]json.RawMessage, error) {
			rounds++
			return map[string]json.RawMessage{"again": json.RawMessage(`{"action":"accept"}`)}, nil
		},
	})
	if _, err := client.Connect(testContext(t), clientTransport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	_, err := client.CallTool(testContext(t), "loop", nil)
	var budget *MRTRBudgetExceededError
	if !errors.As(err, &budget) {
		t.Fatalf("err = %v, want MRTRBudgetExceededError", err)
	}
	if rounds != defaultMRTRMaxRounds {
		t.Fatalf("executor rounds = %d, want %d", rounds, defaultMRTRMaxRounds)
	}
	if got := len(server.requests("tools/call")); got != defaultMRTRMaxRounds+1 {
		t.Fatalf("wire rounds = %d, want %d", got, defaultMRTRMaxRounds+1)
	}
}

func TestMRTRWithoutExecutorFailsClosed(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.setHandler("tools/call", func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{"resultType": "input_required", "requestState": "s"}, nil
	})
	client := connectTestClient(t, clientTransport)
	_, err := client.CallTool(testContext(t), "needs-input", nil)
	var unsupported *MRTRUnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want MRTRUnsupportedError", err)
	}
	if got := len(server.requests("tools/call")); got != 1 {
		t.Fatalf("wire requests = %d, want 1 (no fabricated retry)", got)
	}
}

func TestMRTRExecutorMustAnswerEveryKey(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.setHandler("tools/call", func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{
			"resultType": "input_required",
			"inputRequests": map[string]any{
				"one": map[string]any{"method": "elicitation/create", "params": map[string]any{"message": "one"}},
				"two": map[string]any{"method": "elicitation/create", "params": map[string]any{"message": "two"}},
			},
		}, nil
	})
	client := NewClient(ClientOptions{
		Name: "p5-partial", Version: "1.0.0",
		InputRequests: func(context.Context, InputRequiredResult, *InputRequestState) (map[string]json.RawMessage, error) {
			return map[string]json.RawMessage{"one": json.RawMessage(`{"action":"accept"}`)}, nil
		},
	})
	if _, err := client.Connect(testContext(t), clientTransport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	_, err := client.CallTool(testContext(t), "partial", nil)
	if err == nil || !strings.Contains(err.Error(), "no response for key") {
		t.Fatalf("err = %v, want a missing-response failure", err)
	}
	if got := len(server.requests("tools/call")); got != 1 {
		t.Fatalf("wire requests = %d, want 1 (a partial map is never sent)", got)
	}
}

// --- P5.3 legacy server requests -----------------------------------------

func toolScopeText(ctx context.Context) string {
	scope, ok := agentcore.ToolRunScopeFromContext(ctx)
	if !ok {
		return ""
	}
	return scope.TenantID + "/" + scope.SessionID
}

func TestLegacyServerRequestUsesExecutingCallScope(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.setHandler("tools/call", func(json.RawMessage) (any, *ErrorObject) {
		// Ask for sampling while the call is open, then answer the call.
		sendServerRequest(t, server.transport, NumberID(900), "sampling/createMessage", map[string]any{
			"messages":  []any{map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": "hi"}}},
			"maxTokens": 128,
		})
		if !waitFor(t, 2*time.Second, func() bool { return len(server.Messages()) > 2 }) {
			return nil, &ErrorObject{Code: CodeInternalError, Message: "no request seen"}
		}
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "call done"}}}, nil
	})
	var scopeSeen string
	client := NewClient(ClientOptions{
		Name: "p5-legacy", Version: "1.0.0",
		InputRequests: func(ctx context.Context, request InputRequiredResult, _ *InputRequestState) (map[string]json.RawMessage, error) {
			sampling := request.InputRequests["server-request"]
			if sampling.Method != "sampling/createMessage" {
				return nil, &McpError{Code: CodeMethodNotFound, Message: "unexpected method"}
			}
			scopeSeen = toolScopeText(ctx)
			return map[string]json.RawMessage{
				"server-request": json.RawMessage(`{"role":"assistant","content":{"type":"text","text":"sampled"},"model":"m","stopReason":"endTurn"}`),
			}, nil
		},
	})
	if _, err := client.Connect(testContext(t), clientTransport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	ctx := agentcore.WithToolRunScope(context.Background(), agentcore.ToolRunScope{TenantID: "tenant-1", SessionID: "session-1"})
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := client.CallTool(ctx, "ask", nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	if scopeSeen != "tenant-1/session-1" {
		t.Fatalf("sampling scope = %q, want tenant-1/session-1", scopeSeen)
	}
	var response *SuccessResponse
	ok := waitFor(t, 2*time.Second, func() bool {
		for _, message := range server.Messages() {
			if candidate, ok := message.(*SuccessResponse); ok && candidate.ID == NumberID(900) {
				response = candidate
				return true
			}
		}
		return false
	})
	if !ok || response == nil {
		t.Fatalf("server never received the sampling response; messages=%#v", server.Messages())
	}
	if !strings.Contains(string(response.Result), `"text":"sampled"`) {
		t.Fatalf("sampling response = %s", response.Result)
	}
}

func TestLegacyServerRequestOutsideCallIsRefused(t *testing.T) {
	clientTransport, server := newTestServer(t)
	client := connectTestClient(t, clientTransport)
	_ = client
	sendServerRequest(t, server.transport, NumberID(77), "sampling/createMessage", map[string]any{})
	var response *ErrorResponse
	ok := waitFor(t, 2*time.Second, func() bool {
		for _, message := range server.Messages() {
			if candidate, ok := message.(*ErrorResponse); ok && candidate.ID == NumberID(77) {
				response = candidate
				return true
			}
		}
		return false
	})
	if !ok || response == nil {
		t.Fatalf("server never received the refusal; messages=%#v", server.Messages())
	}
	if response.Error.Code != CodeMethodNotFound {
		t.Fatalf("refusal code = %d, want %d", response.Error.Code, CodeMethodNotFound)
	}
}

// TestUntaggedRequestDomainFramesSurviveAnOpenListenStream is the P4-review
// regression: progress, cancellation and server logs are request/connection
// scoped and legitimately untagged, so an open listen stream must not swallow
// them. (The P4 lane owns the canonical fix; this test pins the required
// behavior for the modern era.)
func TestUntaggedRequestDomainFramesSurviveAnOpenListenStream(t *testing.T) {
	clientTransport, server := newTestServer(t)
	server.neverRespond("subscriptions/listen")
	server.neverRespond("tools/call")
	var mu sync.Mutex
	var methods []string
	client := NewClient(ClientOptions{
		Name: "p4-regression", Version: "1.0.0",
		Notifications: func(_ context.Context, method string, _ json.RawMessage) {
			mu.Lock()
			methods = append(methods, method)
			mu.Unlock()
		},
	})
	if _, err := client.Connect(testContext(t), clientTransport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	handleCh := make(chan *ListenHandle, 1)
	errCh := make(chan error, 1)
	go func() {
		handle, err := client.Listen(testContext(t), ListenParams{Notifications: ListenFilter{ToolsListChanged: true}}, ListenOptions{})
		if err != nil {
			errCh <- err
			return
		}
		handleCh <- handle
	}()
	if !waitFor(t, 2*time.Second, func() bool { return len(server.requests("subscriptions/listen")) == 1 }) {
		t.Fatal("server never saw subscriptions/listen")
	}
	listenID := server.requests("subscriptions/listen")[0].ID
	sendNotification(t, server.transport, "notifications/subscriptions/acknowledged",
		json.RawMessage(`{"notifications":{"toolsListChanged":true},"_meta":{"`+SubscriptionIDMetaKey+`":`+listenID.String()+`}}`))
	var handle *ListenHandle
	select {
	case handle = <-handleCh:
	case err := <-errCh:
		t.Fatalf("listen: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("listen did not acknowledge")
	}
	t.Cleanup(handle.Close)

	// An untagged server log must reach the global dispatcher.
	sendNotification(t, server.transport, "notifications/message", map[string]any{"level": "info", "data": "hello"})
	if !waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, method := range methods {
			if method == "notifications/message" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("untagged notifications/message was swallowed by the listen stream: %v", methods)
	}

	// An untagged progress frame must reach the in-flight call's sink.
	progressCh := make(chan ProgressNotification, 1)
	callErr := make(chan error, 1)
	go func() {
		_, err := client.CallToolWithOptions(testContext(t), "slow", nil, CallOptions{
			ProgressToken: "regress",
			OnProgress:    func(progress ProgressNotification) { progressCh <- progress },
		})
		callErr <- err
	}()
	if !waitFor(t, 2*time.Second, func() bool { return len(server.requests("tools/call")) == 1 }) {
		t.Fatal("server never saw tools/call")
	}
	call := server.requests("tools/call")[0]
	var params struct {
		Meta struct {
			ProgressToken string `json:"progressToken"`
		} `json:"_meta"`
	}
	_ = json.Unmarshal(call.Params, &params)
	if params.Meta.ProgressToken == "" {
		t.Fatalf("tools/call has no progress token: %s", call.Params)
	}
	sendNotification(t, server.transport, "notifications/progress", map[string]any{
		"progressToken": params.Meta.ProgressToken, "progress": 0.5,
	})
	select {
	case progress := <-progressCh:
		if progress.Progress != 0.5 {
			t.Fatalf("progress = %+v", progress)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("untagged progress was swallowed by the listen stream")
	}

	// An untagged cancellation must abort the call.
	sendNotification(t, server.transport, "notifications/cancelled", map[string]any{
		"requestId": json.RawMessage(call.ID.Key()[2:]),
	})
	select {
	case err := <-callErr:
		var abort *AbortError
		if !errors.As(err, &abort) {
			t.Fatalf("call err = %v, want AbortError", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("untagged cancellation did not abort the call")
	}
}

// --- P5.4 era gating -----------------------------------------------------

func modernDiscoverResult(version string) map[string]any {
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
		"serverInfo":      map[string]any{"name": "modern-fixture", "version": "1.0.0"},
	}
}

func modernTestServer(t *testing.T, version string) (*MemoryTransport, *testServer) {
	t.Helper()
	clientTransport, server := newTestServer(t)
	server.setHandler("server/discover", func(json.RawMessage) (any, *ErrorObject) {
		return modernDiscoverResult(version), nil
	})
	server.setHandler("tools/list", func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{"tools": []any{map[string]any{
			"name": "echo", "inputSchema": map[string]any{"type": "object"},
		}}}, nil
	})
	server.setHandler("tools/call", func(json.RawMessage) (any, *ErrorObject) {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}, nil
	})
	return clientTransport, server
}

func TestClientModernEraSkipsInitialize(t *testing.T) {
	clientTransport, server := modernTestServer(t, ModernProtocolVersion)
	client := NewClient(ClientOptions{Name: "p5-modern", Version: "1.0.0", Era: EraModern})
	if _, err := client.Connect(testContext(t), clientTransport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if client.Era() != EraModern {
		t.Fatalf("era = %q", client.Era())
	}
	if got := client.ProtocolVersion(); got != ModernProtocolVersion {
		t.Fatalf("protocol version = %q", got)
	}
	for _, message := range server.Messages() {
		if request, ok := message.(*Request); ok && request.Method == "initialize" {
			t.Fatal("modern connection sent initialize")
		}
		if notification, ok := message.(*Notification); ok && notification.Method == "notifications/initialized" {
			t.Fatal("modern connection sent notifications/initialized")
		}
	}
	requests := server.requests("server/discover")
	if len(requests) != 1 {
		t.Fatalf("discover requests = %d", len(requests))
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(requests[0].Params, &fields); err != nil {
		t.Fatalf("discover params: %v", err)
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(fields["_meta"], &meta); err != nil {
		t.Fatalf("discover _meta: %v (%s)", err, requests[0].Params)
	}
	var version string
	if err := json.Unmarshal(meta[MetaProtocolVersionKey], &version); err != nil || version != ModernProtocolVersion {
		t.Fatalf("_meta protocolVersion = %s err=%v", meta[MetaProtocolVersionKey], err)
	}
	for _, key := range []string{MetaClientCapabilitiesKey, MetaClientInfoKey} {
		if len(meta[key]) == 0 {
			t.Fatalf("_meta[%s] missing: %s", key, requests[0].Params)
		}
	}
	tools, err := client.ListTools(testContext(t))
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools = %#v err=%v", tools, err)
	}
	list := server.requests("tools/list")
	if len(list) != 1 || !strings.Contains(string(list[0].Params), MetaProtocolVersionKey) {
		t.Fatalf("tools/list params = %s, want _meta", list[0].Params)
	}
}

func TestClientAutoProbeFallsBackToLegacy(t *testing.T) {
	clientTransport, server := newTestServer(t)
	client := NewClient(ClientOptions{Name: "p5-auto", Version: "1.0.0", Era: EraAuto})
	if _, err := client.Connect(testContext(t), clientTransport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if client.Era() != EraLegacy {
		t.Fatalf("era = %q, want legacy", client.Era())
	}
	if got := client.ProtocolVersion(); got != LatestProtocolVersion {
		t.Fatalf("protocol version = %q", got)
	}
	initialize := server.requests("initialize")
	if len(initialize) != 1 {
		t.Fatalf("initialize requests = %d", len(initialize))
	}
	if len(server.requests("server/discover")) != 1 {
		t.Fatalf("auto did not probe server/discover")
	}
	// The fallback initialize is the legacy byte shape: no modern _meta.
	if strings.Contains(string(initialize[0].Params), MetaProtocolVersionKey) {
		t.Fatalf("legacy initialize carried modern _meta: %s", initialize[0].Params)
	}
}

func TestClientAutoProbeSelectsModern(t *testing.T) {
	clientTransport, server := modernTestServer(t, ModernProtocolVersion)
	client := NewClient(ClientOptions{Name: "p5-auto-modern", Version: "1.0.0", Era: EraAuto})
	if _, err := client.Connect(testContext(t), clientTransport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if client.Era() != EraModern {
		t.Fatalf("era = %q, want modern", client.Era())
	}
	if len(server.requests("initialize")) != 0 {
		t.Fatal("auto probe fell through to initialize after a modern answer")
	}
	if len(server.requests("server/discover")) != 1 {
		t.Fatalf("discover requests = %d", len(server.requests("server/discover")))
	}
}

func TestEraParsingAndInvalidEnvironmentFailsClosed(t *testing.T) {
	for _, test := range []struct {
		value string
		want  ProtocolEra
		ok    bool
	}{
		{"auto", EraAuto, true},
		{"legacy", EraLegacy, true},
		{"modern", EraModern, true},
		{" MODERN ", EraModern, true},
		{"nonsense", EraLegacy, false},
	} {
		era, ok := ParseProtocolEra(test.value)
		if era != test.want || ok != test.ok {
			t.Fatalf("ParseProtocolEra(%q) = %q,%v want %q,%v", test.value, era, ok, test.want, test.ok)
		}
	}
	env := func(value string) func(string) string {
		return func(key string) string {
			if key == "KEEL_MCP_PROTOCOL_ERA" {
				return value
			}
			return ""
		}
	}
	if era := EraFromEnvironment(env("")); era != EraAuto {
		t.Fatalf("missing env era = %q, want auto", era)
	}
	if era := EraFromEnvironment(env("garbage")); era != EraLegacy {
		t.Fatalf("invalid env era = %q, want legacy (fail closed)", era)
	}
}

func TestModernCallToolMetaMergesProgressToken(t *testing.T) {
	clientTransport, server := modernTestServer(t, ModernProtocolVersion)
	client := NewClient(ClientOptions{Name: "p5-meta", Version: "1.0.0", Era: EraModern})
	if _, err := client.Connect(testContext(t), clientTransport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.CallToolWithOptions(testContext(t), "echo", nil, CallOptions{ProgressToken: "call-1"}); err != nil {
		t.Fatalf("call: %v", err)
	}
	calls := server.requests("tools/call")
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	var fields struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(calls[0].Params, &fields); err != nil {
		t.Fatalf("params: %v", err)
	}
	var token string
	if err := json.Unmarshal(fields.Meta["progressToken"], &token); err != nil || token != "call-1" {
		t.Fatalf("progressToken = %s err=%v", fields.Meta["progressToken"], err)
	}
	if len(fields.Meta[MetaProtocolVersionKey]) == 0 {
		t.Fatalf("modern _meta lost the protocol version: %s", calls[0].Params)
	}
}

func TestManagerCapabilitiesFollowTheExecutor(t *testing.T) {
	type observed struct {
		mu   sync.Mutex
		caps []map[string]json.RawMessage
	}
	seen := &observed{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var envelope struct {
			ID     json.RawMessage            `json:"id"`
			Method string                     `json:"method"`
			Params map[string]json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body, &envelope)
		var result any
		switch envelope.Method {
		case "initialize":
			var capabilities map[string]json.RawMessage
			_ = json.Unmarshal(envelope.Params["capabilities"], &capabilities)
			seen.mu.Lock()
			seen.caps = append(seen.caps, capabilities)
			seen.mu.Unlock()
			result = map[string]any{
				"protocolVersion": LatestProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "cap-fixture", "version": "1"},
			}
		case "tools/list":
			result = map[string]any{"tools": []any{}}
		default:
			result = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": envelope.ID, "result": result})
	}))
	defer server.Close()
	entry := []ServerEntry{{Name: "caps", Config: map[string]any{"url": server.URL}}}

	// Without an executor the manager must not declare sampling/elicitation.
	plain := Connect(context.Background(), entry, ConnectOptions{
		ClientName: "p5", ClientVersion: "1", Era: EraLegacy,
		Capabilities: ClientCapabilities{Sampling: map[string]json.RawMessage{}, Elicitation: map[string]json.RawMessage{}},
	})
	plain.Close()
	seen.mu.Lock()
	first := seen.caps[0]
	seen.mu.Unlock()
	if _, ok := first["sampling"]; ok {
		t.Fatalf("manager declared sampling without an executor: %v", first)
	}
	if _, ok := first["elicitation"]; ok {
		t.Fatalf("manager declared elicitation without an executor: %v", first)
	}

	// With the executor the declared capabilities ride initialize.
	wired := Connect(context.Background(), entry, ConnectOptions{
		ClientName: "p5", ClientVersion: "1", Era: EraLegacy,
		Capabilities: ClientCapabilities{Sampling: map[string]json.RawMessage{}, Elicitation: map[string]json.RawMessage{}},
		InputRequests: func(context.Context, string, InputRequiredResult, *InputRequestState) (map[string]json.RawMessage, error) {
			return nil, nil
		},
	})
	wired.Close()
	seen.mu.Lock()
	second := seen.caps[len(seen.caps)-1]
	seen.mu.Unlock()
	if _, ok := second["sampling"]; !ok {
		t.Fatalf("wired manager did not declare sampling: %v", second)
	}
	if _, ok := second["elicitation"]; !ok {
		t.Fatalf("wired manager did not declare elicitation: %v", second)
	}
	if _, ok := second["roots"]; ok {
		t.Fatalf("manager declared roots, which it always refuses: %v", second)
	}
}

// --- P5.4 HTTP wire contract --------------------------------------------

type httpRecord struct {
	Method  string
	Path    string
	Header  http.Header
	RawBody string
}

type httpRecorder struct {
	mu      sync.Mutex
	records []httpRecord
}

func (r *httpRecorder) add(rec httpRecord) {
	r.mu.Lock()
	r.records = append(r.records, rec)
	r.mu.Unlock()
}

func (r *httpRecorder) all() []httpRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]httpRecord(nil), r.records...)
}

func TestHTTPModernHeadersAndNoSession(t *testing.T) {
	recorder := &httpRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var envelope struct {
			ID     json.RawMessage            `json:"id"`
			Method string                     `json:"method"`
			Params map[string]json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body, &envelope)
		recorder.add(httpRecord{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), RawBody: string(body)})
		// Reject any header/_meta mismatch the way the spec says a server does.
		metaVersion := ""
		if raw, ok := envelope.Params["_meta"]; ok {
			var meta map[string]json.RawMessage
			_ = json.Unmarshal(raw, &meta)
			_ = json.Unmarshal(meta[MetaProtocolVersionKey], &metaVersion)
		}
		if got := r.Header.Get("MCP-Protocol-Version"); got != metaVersion {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32020,"message":"HeaderMismatch"}}`))
			return
		}
		var result any
		switch envelope.Method {
		case "server/discover":
			result = modernDiscoverResult(ModernProtocolVersion)
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}
		default:
			result = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "must-not-be-honoured")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": envelope.ID, "result": result})
	}))
	defer server.Close()
	transport := NewHTTPTransport(HTTPTransportOptions{URL: server.URL})
	client := NewClient(ClientOptions{Name: "p5-http", Version: "1.0.0", Era: EraModern})
	if _, err := client.Connect(testContext(t), transport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.ListTools(testContext(t)); err != nil {
		t.Fatalf("tools: %v", err)
	}
	if _, err := client.CallTool(testContext(t), "echo", nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	records := recorder.all()
	var discover, call *httpRecord
	for index := range records {
		if strings.Contains(records[index].RawBody, `"server/discover"`) {
			discover = &records[index]
		}
		if strings.Contains(records[index].RawBody, `"tools/call"`) {
			call = &records[index]
		}
		if records[index].Method != http.MethodPost {
			t.Fatalf("modern transport issued %s %s", records[index].Method, records[index].Path)
		}
	}
	if discover == nil || call == nil {
		t.Fatalf("records = %d, missing discover/call", len(records))
	}
	if got := discover.Header.Get("MCP-Protocol-Version"); got != ModernProtocolVersion {
		t.Fatalf("discover MCP-Protocol-Version = %q", got)
	}
	if got := discover.Header.Get("Mcp-Method"); got != "server/discover" {
		t.Fatalf("discover Mcp-Method = %q", got)
	}
	if got := call.Header.Get("Mcp-Name"); got != "echo" {
		t.Fatalf("call Mcp-Name = %q", got)
	}
	for _, record := range records {
		if got := record.Header.Get("Mcp-Session-Id"); got != "" {
			t.Fatalf("modern request sent Mcp-Session-Id %q", got)
		}
		if got := record.Header.Get("Last-Event-ID"); got != "" {
			t.Fatalf("modern request sent Last-Event-ID %q", got)
		}
	}
	if transport.SessionID() != "" {
		t.Fatalf("modern transport accepted a session id %q", transport.SessionID())
	}
}

func TestHTTPMcpNameEncodesNonASCII(t *testing.T) {
	name := "工具/call"
	encoded := encodeMCPName(name)
	if !strings.HasPrefix(encoded, "=?base64?") || !strings.HasSuffix(encoded, "?=") {
		t.Fatalf("encodeMCPName(%q) = %q", name, encoded)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(encoded, "=?base64?"), "?=")
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || string(decoded) != name {
		t.Fatalf("decoded = %q err=%v", decoded, err)
	}
	if got := encodeMCPName("plain_tool"); got != "plain_tool" {
		t.Fatalf("plain name = %q", got)
	}
	if got := encodeMCPName("=?base64?x?="); !strings.HasPrefix(got, "=?base64?") {
		t.Fatalf("sentinel-looking name = %q, must be encoded", got)
	}
}

func TestHTTPLegacyKeepsSessionAndGetBehaviour(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	var sessionHeaders []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		sessionHeaders = append(sessionHeaders, r.Header.Get("Mcp-Session-Id"))
		mu.Unlock()
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var envelope struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &envelope)
		var result any
		switch envelope.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": LatestProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "legacy-http", "version": "1"},
			}
		case "tools/list":
			result = map[string]any{"tools": []any{}}
		default:
			result = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "session-42")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": envelope.ID, "result": result})
	}))
	defer server.Close()
	transport := NewHTTPTransport(HTTPTransportOptions{URL: server.URL, DisableGetStream: true})
	client := NewClient(ClientOptions{Name: "p5-legacy-http", Version: "1.0.0", Era: EraLegacy})
	if _, err := client.Connect(testContext(t), transport); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.ListTools(testContext(t)); err != nil {
		t.Fatalf("tools: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if transport.SessionID() != "session-42" {
		t.Fatalf("session = %q", transport.SessionID())
	}
	foundSession := false
	for _, header := range sessionHeaders {
		if header == "session-42" {
			foundSession = true
		}
	}
	if !foundSession {
		t.Fatalf("legacy requests never carried the session header: %v", sessionHeaders)
	}
	if len(methods) == 0 || methods[0] != http.MethodPost {
		t.Fatalf("legacy first method = %v", methods)
	}
}
