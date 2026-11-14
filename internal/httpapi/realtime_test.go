package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/kernel"
)

// The realtime tests run without KEEL_TEST_DSN: they exercise the bridge
// against a fake provider WebSocket server and a Server whose kernel seams are
// unwired.

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type providerRequest struct {
	URL    string
	Header http.Header
}

type fakeRealtimeProvider struct {
	server   *httptest.Server
	conns    chan *websocket.Conn
	received chan []byte
	closed   chan int
	requests chan providerRequest
	accepted atomic.Int32

	mu  sync.Mutex
	all []*websocket.Conn
}

func newFakeRealtimeProvider(t *testing.T) *fakeRealtimeProvider {
	t.Helper()
	fp := &fakeRealtimeProvider{
		conns:    make(chan *websocket.Conn, 4),
		received: make(chan []byte, 64),
		closed:   make(chan int, 8),
		requests: make(chan providerRequest, 4),
	}
	fp.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fp.requests <- providerRequest{URL: r.URL.String(), Header: r.Header.Clone()}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		fp.accepted.Add(1)
		fp.mu.Lock()
		fp.all = append(fp.all, c)
		fp.mu.Unlock()
		fp.conns <- c
		for {
			_, data, err := c.Read(context.Background())
			if err != nil {
				code := websocket.CloseStatus(err)
				if code == -1 {
					code = websocket.StatusAbnormalClosure
				}
				fp.closed <- int(code)
				return
			}
			fp.received <- data
		}
	}))
	t.Cleanup(func() {
		fp.mu.Lock()
		for _, c := range fp.all {
			_ = c.Close(websocket.StatusGoingAway, "test done")
		}
		fp.mu.Unlock()
		fp.server.Close()
	})
	return fp
}

func (fp *fakeRealtimeProvider) wsURL() string {
	return "ws" + strings.TrimPrefix(fp.server.URL, "http")
}

func (fp *fakeRealtimeProvider) next(t *testing.T, ctx context.Context, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case data := <-ch:
		return data
	case <-ctx.Done():
		t.Fatalf("timed out waiting for a provider message: %v", ctx.Err())
		return nil
	}
}

func (fp *fakeRealtimeProvider) waitConn(t *testing.T, ctx context.Context) *websocket.Conn {
	t.Helper()
	select {
	case c := <-fp.conns:
		return c
	case <-ctx.Done():
		t.Fatalf("timed out waiting for the provider connection: %v", ctx.Err())
		return nil
	}
}

func (fp *fakeRealtimeProvider) waitRequest(t *testing.T, ctx context.Context) providerRequest {
	t.Helper()
	select {
	case req := <-fp.requests:
		return req
	case <-ctx.Done():
		t.Fatalf("timed out waiting for the provider request: %v", ctx.Err())
		return providerRequest{}
	}
}

func (fp *fakeRealtimeProvider) waitClosed(t *testing.T, ctx context.Context) int {
	t.Helper()
	select {
	case code := <-fp.closed:
		return code
	case <-ctx.Done():
		t.Fatalf("timed out waiting for the provider close: %v", ctx.Err())
		return 0
	}
}

func setFakeProviderEnv(t *testing.T, fp *fakeRealtimeProvider) {
	t.Helper()
	t.Setenv("KEEL_REALTIME_WS_URL", fp.wsURL()+"/api-ws/v1/realtime")
	t.Setenv("KEEL_REALTIME_PROVIDER", "qwen")
	t.Setenv("KEEL_REALTIME_API_KEY", "server-side-key")
	t.Setenv("KEEL_REALTIME_MODEL", "qwen-omni-test")
	t.Setenv("KEEL_REALTIME_MODELS", "")
	t.Setenv("DASHSCOPE_API_KEY", "")
}

// emptyEventReader is the permissive kernel read seam the bridge tests use: a
// nil Events seam is a 503 (the tenant boundary must never fail open), so the
// tests that exercise the bridge itself wire a reader that finds the session.
type emptyEventReader struct{}

func (emptyEventReader) LoadEvents(context.Context, kernel.Scope, int64, int) ([]kernel.Event, error) {
	return nil, nil
}

func (emptyEventReader) UnresolvedToolCalls(context.Context, kernel.Scope) ([]kernel.ToolCallRef, error) {
	return nil, nil
}

func newRealtimeTestServer(t *testing.T, logger *slog.Logger) *httptest.Server {
	t.Helper()
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	// The tenant boundary requires a wired reader; a server without one answers
	// 503 before the upgrade (see TestRealtimeUnwiredTenantCheckAnswers503).
	hs := httptest.NewServer(New(Config{Logger: logger, Events: emptyEventReader{}}))
	t.Cleanup(hs.Close)
	return hs
}

func dialRealtime(t *testing.T, hs *httptest.Server, tenant, session, extraQuery string) (*websocket.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	u := "ws" + strings.TrimPrefix(hs.URL, "http") + "/v1/realtime?session_id=" + session
	if extraQuery != "" {
		u += "&" + extraQuery
	}
	conn, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Tenant-ID": []string{tenant}},
	})
	if err != nil {
		cancel()
		t.Fatalf("dial realtime: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "test done") })
	return conn, ctx
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not met within 5s")
}

func TestRealtimeBridgeProxiesEvents(t *testing.T) {
	fp := newFakeRealtimeProvider(t)
	setFakeProviderEnv(t, fp)
	logs := &syncBuffer{}
	hs := newRealtimeTestServer(t, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	tenant := uuid.NewString()
	session := uuid.NewString()
	client, ctx := dialRealtime(t, hs, tenant, session,
		"model=qwen-omni-test&ws_url=ws://evil.example/realtime&api_key=evil-key")

	provConn := fp.waitConn(t, ctx)
	request := fp.waitRequest(t, ctx)
	if got := request.Header.Get("Authorization"); got != "Bearer server-side-key" {
		t.Fatalf("provider Authorization = %q, want the server-side key", got)
	}
	if strings.Contains(request.URL, "evil") || strings.Contains(request.URL, "api_key") {
		t.Fatalf("client-supplied provider config reached the provider URL: %q", request.URL)
	}
	if got := request.Header.Get("X-Tenant-ID"); got != "" {
		t.Fatalf("tenant header leaked to the provider: %q", got)
	}
	if got := request.URL; !strings.Contains(got, "model=qwen-omni-test") {
		t.Fatalf("provider URL = %q, want the configured model", got)
	}
	if got := fp.accepted.Load(); got != 1 {
		t.Fatalf("provider connections = %d, want exactly one per client", got)
	}

	audio := base64.StdEncoding.EncodeToString([]byte("audio-payload-marker"))
	appendEvent := `{"type":"input_audio_buffer.append","audio":"` + audio + `"}`
	if err := client.Write(ctx, websocket.MessageText, []byte(appendEvent)); err != nil {
		t.Fatalf("client write: %v", err)
	}

	// The bridge injects the Qwen-shaped session.update before the first client
	// event because the client did not send one.
	injected := fp.next(t, ctx, fp.received)
	var sessionUpdate struct {
		Type    string `json:"type"`
		Session struct {
			Modalities    []string `json:"modalities"`
			TurnDetection struct {
				Type string `json:"type"`
			} `json:"turn_detection"`
			Audio struct {
				Input struct {
					Format struct {
						SampleRate int `json:"sample_rate"`
					} `json:"format"`
				} `json:"input"`
				Output struct {
					Format struct {
						SampleRate int `json:"sample_rate"`
					} `json:"format"`
				} `json:"output"`
			} `json:"audio"`
		} `json:"session"`
	}
	if err := json.Unmarshal(injected, &sessionUpdate); err != nil {
		t.Fatalf("injected session.update is invalid JSON: %v", err)
	}
	if sessionUpdate.Type != "session.update" {
		t.Fatalf("injected type = %q", sessionUpdate.Type)
	}
	if len(sessionUpdate.Session.Modalities) != 2 {
		t.Fatalf("injected modalities = %v", sessionUpdate.Session.Modalities)
	}
	if got := sessionUpdate.Session.Audio.Input.Format.SampleRate; got != 16000 {
		t.Fatalf("injected input rate = %d, want 16000", got)
	}
	if got := sessionUpdate.Session.Audio.Output.Format.SampleRate; got != 24000 {
		t.Fatalf("injected output rate = %d, want 24000", got)
	}
	if got := sessionUpdate.Session.TurnDetection.Type; got != "server_vad" {
		t.Fatalf("injected turn detection = %q", got)
	}

	forwarded := fp.next(t, ctx, fp.received)
	if string(forwarded) != appendEvent {
		t.Fatalf("provider received %s, want the client event unchanged", forwarded)
	}

	// A second client event must not trigger another session.update.
	second := `{"type":"input_audio_buffer.commit"}`
	if err := client.Write(ctx, websocket.MessageText, []byte(second)); err != nil {
		t.Fatalf("client write: %v", err)
	}
	if got := fp.next(t, ctx, fp.received); string(got) != second {
		t.Fatalf("provider received %s, want %s (no second injection)", got, second)
	}

	// Provider -> client events are forwarded byte for byte.
	delta := `{"type":"response.audio.delta","delta":"` + audio + `"}`
	if err := provConn.Write(ctx, websocket.MessageText, []byte(delta)); err != nil {
		t.Fatalf("provider write: %v", err)
	}
	_, got, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(got) != delta {
		t.Fatalf("client received %s, want %s", got, delta)
	}

	// A client close propagates to the provider.
	if err := client.Close(websocket.StatusNormalClosure, "bye"); err != nil {
		t.Fatalf("client close: %v", err)
	}
	if code := fp.waitClosed(t, ctx); code != 1000 {
		t.Fatalf("provider close code = %d, want 1000", code)
	}

	// The bridge logs event type and size only, never payloads.
	waitFor(t, func() bool { return strings.Contains(logs.String(), "realtime bridge closed") })
	text := logs.String()
	if strings.Contains(text, audio) {
		t.Fatalf("audio payload appeared in logs:\n%s", text)
	}
	if !strings.Contains(text, "response.audio.delta") {
		t.Fatalf("event type metadata missing from logs:\n%s", text)
	}
}

func TestRealtimeNotConfigured(t *testing.T) {
	t.Setenv("KEEL_REALTIME_WS_URL", "")
	t.Setenv("KEEL_REALTIME_PROVIDER", "")
	t.Setenv("KEEL_REALTIME_API_KEY", "")
	t.Setenv("DASHSCOPE_API_KEY", "")
	hs := newRealtimeTestServer(t, nil)

	req, err := http.NewRequest(http.MethodGet, hs.URL+"/v1/realtime?session_id="+uuid.NewString(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", uuid.NewString())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(body)); got != `{"error":"realtime provider is not configured"}` {
		t.Fatalf("body = %s", got)
	}
}

func TestRealtimeRequiresTenantAndSession(t *testing.T) {
	hs := newRealtimeTestServer(t, nil)

	t.Run("missing tenant", func(t *testing.T) {
		resp, err := http.Get(hs.URL + "/v1/realtime?session_id=" + uuid.NewString())
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("bad session id", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, hs.URL+"/v1/realtime?session_id=not-a-uuid", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Tenant-ID", uuid.NewString())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})
}

type notFoundReader struct{}

func (notFoundReader) LoadEvents(context.Context, kernel.Scope, int64, int) ([]kernel.Event, error) {
	return nil, kernel.ErrNotFound
}

func (notFoundReader) UnresolvedToolCalls(context.Context, kernel.Scope) ([]kernel.ToolCallRef, error) {
	return nil, nil
}

func TestRealtimeForeignSessionIs404(t *testing.T) {
	t.Setenv("KEEL_REALTIME_WS_URL", "wss://example.test/api-ws/v1/realtime")
	t.Setenv("KEEL_REALTIME_PROVIDER", "qwen")
	t.Setenv("KEEL_REALTIME_API_KEY", "key")
	hs := httptest.NewServer(New(Config{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Events: notFoundReader{},
	}))
	t.Cleanup(hs.Close)

	req, err := http.NewRequest(http.MethodGet, hs.URL+"/v1/realtime?session_id="+uuid.NewString(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", uuid.NewString())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestRealtimeModelNotAllowed(t *testing.T) {
	t.Setenv("KEEL_REALTIME_WS_URL", "wss://example.test/api-ws/v1/realtime")
	t.Setenv("KEEL_REALTIME_PROVIDER", "qwen")
	t.Setenv("KEEL_REALTIME_API_KEY", "key")
	t.Setenv("KEEL_REALTIME_MODEL", "allowed-model")
	t.Setenv("KEEL_REALTIME_MODELS", "allowed-model")
	hs := newRealtimeTestServer(t, nil)

	req, err := http.NewRequest(http.MethodGet,
		hs.URL+"/v1/realtime?session_id="+uuid.NewString()+"&model=other-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", uuid.NewString())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "realtime model is not allowed") {
		t.Fatalf("body = %s", body)
	}
}

func TestRealtimeForwardsProviderErrorAndClose(t *testing.T) {
	fp := newFakeRealtimeProvider(t)
	setFakeProviderEnv(t, fp)
	hs := newRealtimeTestServer(t, nil)

	client, ctx := dialRealtime(t, hs, uuid.NewString(), uuid.NewString(), "")
	provConn := fp.waitConn(t, ctx)

	errorEvent := `{"type":"error","error":{"type":"invalid_request_error","message":"bad audio"}}`
	if err := provConn.Write(ctx, websocket.MessageText, []byte(errorEvent)); err != nil {
		t.Fatalf("provider write: %v", err)
	}
	if err := provConn.Close(websocket.StatusInternalError, "provider failure"); err != nil {
		t.Fatalf("provider close: %v", err)
	}

	_, got, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("client read error event: %v", err)
	}
	if string(got) != errorEvent {
		t.Fatalf("client received %s, want the provider error event verbatim", got)
	}
	if _, _, err := client.Read(ctx); err == nil {
		t.Fatal("client read after provider error succeeded, want a close")
	} else if code := websocket.CloseStatus(err); code != 1011 {
		t.Fatalf("client close code = %d, want 1011 (%v)", code, err)
	}
}

func TestRealtimeProviderUnavailable(t *testing.T) {
	t.Setenv("KEEL_REALTIME_WS_URL", "ws://127.0.0.1:1/api-ws/v1/realtime")
	t.Setenv("KEEL_REALTIME_PROVIDER", "qwen")
	t.Setenv("KEEL_REALTIME_API_KEY", "key")
	t.Setenv("KEEL_REALTIME_MODEL", "qwen-omni-test")
	hs := newRealtimeTestServer(t, nil)

	client, ctx := dialRealtime(t, hs, uuid.NewString(), uuid.NewString(), "")
	_, got, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("client read bridge error: %v", err)
	}
	if !strings.Contains(string(got), `"provider_unavailable"`) {
		t.Fatalf("bridge error = %s, want provider_unavailable", got)
	}
	if _, _, err := client.Read(ctx); err == nil {
		t.Fatal("client read after bridge error succeeded, want a close")
	} else if code := websocket.CloseStatus(err); code != 1011 {
		t.Fatalf("client close code = %d, want 1011 (%v)", code, err)
	}
}

func TestRealtimeRejectsOversizedMessages(t *testing.T) {
	fp := newFakeRealtimeProvider(t)
	setFakeProviderEnv(t, fp)
	hs := newRealtimeTestServer(t, nil)

	client, ctx := dialRealtime(t, hs, uuid.NewString(), uuid.NewString(), "")
	_ = fp.waitConn(t, ctx)

	oversized := bytes.Repeat([]byte("a"), (1<<20)+1)
	if err := client.Write(ctx, websocket.MessageText, oversized); err != nil {
		// The write may fail once the server closes mid-frame; that is still a
		// rejection, but the close assertion below is the real check.
		t.Logf("oversized write returned %v", err)
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, _, err := client.Read(readCtx); err == nil {
		t.Fatal("client read succeeded after an oversized message, want a close")
	}
}

// TestRealtimeUnwiredTenantCheckAnswers503 pins the fail-open fix: without the
// kernel read seam the bridge cannot verify the session belongs to the tenant,
// so it must refuse the upgrade (503) instead of opening a live bridge.
func TestRealtimeUnwiredTenantCheckAnswers503(t *testing.T) {
	t.Setenv("KEEL_REALTIME_WS_URL", "wss://example.test/api-ws/v1/realtime")
	t.Setenv("KEEL_REALTIME_PROVIDER", "qwen")
	t.Setenv("KEEL_REALTIME_API_KEY", "key")
	t.Setenv("KEEL_REALTIME_MODEL", "")
	t.Setenv("KEEL_REALTIME_MODELS", "")
	hs := httptest.NewServer(New(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}))
	t.Cleanup(hs.Close)

	req, err := http.NewRequest(http.MethodGet, hs.URL+"/v1/realtime?session_id="+uuid.NewString(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", uuid.NewString())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "contract kernel is not wired") {
		t.Fatalf("body = %s, want the unwired diagnostic", body)
	}
}

// TestRealtimeStatusConfigured pins the preflight report: configured, the
// provider family, the model allowlist and the sample rates the session default
// carries — and no provider URL or credential anywhere in the body.
func TestRealtimeStatusConfigured(t *testing.T) {
	t.Setenv("KEEL_REALTIME_WS_URL", "wss://workspace.example/api-ws/v1/realtime")
	t.Setenv("KEEL_REALTIME_PROVIDER", "qwen")
	t.Setenv("KEEL_REALTIME_API_KEY", "status-secret-key")
	t.Setenv("KEEL_REALTIME_MODEL", "qwen-omni-a")
	t.Setenv("KEEL_REALTIME_MODELS", "qwen-omni-a,qwen-omni-b")
	t.Setenv("DASHSCOPE_API_KEY", "")
	hs := newRealtimeTestServer(t, nil)

	req, err := http.NewRequest(http.MethodGet, hs.URL+"/v1/realtime/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", uuid.NewString())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d %s", resp.StatusCode, body)
	}
	for _, leak := range []string{"status-secret-key", "workspace.example", "wss://", "api-ws", "Authorization"} {
		if strings.Contains(string(body), leak) {
			t.Fatalf("status body leaked %q: %s", leak, body)
		}
	}
	var status realtimeStatusResponse
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("status body is not JSON: %v (%s)", err, body)
	}
	if !status.Configured {
		t.Fatalf("configured = false, want true")
	}
	if status.Provider != "qwen" || status.Kind != "qwen" {
		t.Fatalf("provider/kind = %q/%q, want qwen/qwen", status.Provider, status.Kind)
	}
	if !reflect.DeepEqual(status.Models, []string{"qwen-omni-a", "qwen-omni-b"}) {
		t.Fatalf("models = %v, want the configured allowlist", status.Models)
	}
	if status.InputSampleRate != 16000 || status.OutputSampleRate != 24000 {
		t.Fatalf("sample rates = %d/%d, want 16000/24000", status.InputSampleRate, status.OutputSampleRate)
	}
}

// TestRealtimeStatusUnconfigured pins the normal "voice is not configured"
// state: a 200 body with configured=false, empty arrays (never null) and no
// credential or URL leakage, so the UI can render the true state up front.
func TestRealtimeStatusUnconfigured(t *testing.T) {
	t.Setenv("KEEL_REALTIME_WS_URL", "")
	t.Setenv("KEEL_REALTIME_PROVIDER", "")
	t.Setenv("KEEL_REALTIME_API_KEY", "unused-secret-key")
	t.Setenv("KEEL_REALTIME_MODEL", "")
	t.Setenv("KEEL_REALTIME_MODELS", "")
	t.Setenv("DASHSCOPE_API_KEY", "")
	hs := newRealtimeTestServer(t, nil)

	req, err := http.NewRequest(http.MethodGet, hs.URL+"/v1/realtime/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", uuid.NewString())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "unused-secret-key") {
		t.Fatalf("status body leaked the key: %s", body)
	}
	var status realtimeStatusResponse
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("status body is not JSON: %v (%s)", err, body)
	}
	if status.Configured {
		t.Fatal("configured = true, want false")
	}
	if status.Provider != "" || status.Kind != "" {
		t.Fatalf("provider/kind = %q/%q, want empty", status.Provider, status.Kind)
	}
	if status.Models == nil || len(status.Models) != 0 {
		t.Fatalf("models = %#v, want an empty non-nil list", status.Models)
	}
	if status.InputSampleRate != 0 || status.OutputSampleRate != 0 {
		t.Fatalf("sample rates = %d/%d, want 0/0", status.InputSampleRate, status.OutputSampleRate)
	}
	if !strings.Contains(string(body), `"models":[]`) {
		t.Fatalf("models must render as []: %s", body)
	}

	// The preflight sits behind the same tenant header as the bridge.
	bare, err := http.Get(hs.URL + "/v1/realtime/status")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer bare.Body.Close()
	if bare.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status without X-Tenant-ID = %d, want 401", bare.StatusCode)
	}
}
