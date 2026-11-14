package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/demo"
	"github.com/jixoai/keel/internal/realtime"
)

// The demo realtime provider is ordinary server configuration: enabling
// KEEL_DEMO_PROVIDER makes the same /v1/realtime/status and /v1/realtime paths
// answer as if a vendor were wired, and disabling it leaves the deployment in
// the documented "not configured" state. These tests pin both directions, plus
// one full WS turn through the real bridge.

// setDemoProviderEnv turns the demo provider on and removes every vendor
// setting, so the selection order lands on the demo provider for a reason
// (nothing else is configured), not by accident.
func setDemoProviderEnv(t *testing.T) {
	t.Helper()
	t.Setenv(demo.Env, "1")
	clearVendorRealtimeEnv(t)
}

// setNoRealtimeProviderEnv is the production-shaped environment: no demo switch
// and no vendor endpoint.
func setNoRealtimeProviderEnv(t *testing.T) {
	t.Helper()
	t.Setenv(demo.Env, "")
	clearVendorRealtimeEnv(t)
}

func clearVendorRealtimeEnv(t *testing.T) {
	t.Helper()
	t.Setenv("KEEL_REALTIME_WS_URL", "")
	t.Setenv("KEEL_REALTIME_PROVIDER", "")
	t.Setenv("KEEL_REALTIME_API_KEY", "")
	t.Setenv("KEEL_REALTIME_MODEL", "")
	t.Setenv("KEEL_REALTIME_MODELS", "")
	t.Setenv("DASHSCOPE_API_KEY", "")
}

// getRealtimeStatus performs the preflight request and decodes the body.
func getRealtimeStatus(t *testing.T, hs *httptest.Server) (realtimeStatusResponse, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, hs.URL+"/v1/realtime/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", uuid.NewString())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("status request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d %s", resp.StatusCode, body)
	}
	var status realtimeStatusResponse
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("status body is not JSON: %v (%s)", err, body)
	}
	return status, body
}

// TestRealtimeStatusDemoProvider pins the preflight report a review deployment
// sees: the provider names itself "demo", serves exactly demo-realtime and
// declares the same sample rates the browser host uses for that kind.
func TestRealtimeStatusDemoProvider(t *testing.T) {
	setDemoProviderEnv(t)
	hs := newRealtimeTestServer(t, nil)

	status, body := getRealtimeStatus(t, hs)
	if !status.Configured {
		t.Fatalf("configured = false, want true: %s", body)
	}
	if status.Provider != "demo" || status.Kind != "demo" {
		t.Fatalf("provider/kind = %q/%q, want demo/demo", status.Provider, status.Kind)
	}
	if !reflect.DeepEqual(status.Models, []string{demo.ModelRealtime}) {
		t.Fatalf("models = %v, want [%s]", status.Models, demo.ModelRealtime)
	}
	// 16 kHz in / 24 kHz out: the browser host's defaults for a non-OpenAI
	// provider kind (web/host.js providerRates) and the rates the demo session
	// document declares, so the two cannot disagree.
	if status.InputSampleRate != 16000 || status.OutputSampleRate != 24000 {
		t.Fatalf("sample rates = %d/%d, want 16000/24000", status.InputSampleRate, status.OutputSampleRate)
	}
	// The demo has no endpoint or credential, but the status must stay a
	// provider-shaped document regardless: no URL, no key words.
	for _, leak := range []string{"wss://", "ws://", "KEEL_", "Authorization"} {
		if strings.Contains(string(body), leak) {
			t.Fatalf("status body leaked %q: %s", leak, body)
		}
	}
}

// TestRealtimeStatusDemoOff pins the production shape: without the switch the
// preflight reports "not configured" and the bridge refuses to upgrade, so no
// client ever opens a socket that would close with 1006.
func TestRealtimeStatusDemoOff(t *testing.T) {
	setNoRealtimeProviderEnv(t)
	hs := newRealtimeTestServer(t, nil)

	status, body := getRealtimeStatus(t, hs)
	if status.Configured {
		t.Fatalf("configured = true, want false: %s", body)
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

	// The same environment must not upgrade a WebSocket: the client's preflight
	// said "unconfigured", and the bridge has to agree.
	req, err := http.NewRequest(http.MethodGet, hs.URL+"/v1/realtime?session_id="+uuid.NewString(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Tenant-ID", uuid.NewString())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("bridge request: %v", err)
	}
	defer resp.Body.Close()
	bridgeBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("bridge status = %d, want 503 (%s)", resp.StatusCode, bridgeBody)
	}
	if got := strings.TrimSpace(string(bridgeBody)); got != `{"error":"realtime provider is not configured"}` {
		t.Fatalf("bridge body = %s", got)
	}
}

// TestRealtimeDemoTurnEndToEnd drives one conversation through the real handler:
// the browser socket, the tenant boundary, the bridge pumps and the demo
// provider. It asserts the event names the shipped client projects.
func TestRealtimeDemoTurnEndToEnd(t *testing.T) {
	setDemoProviderEnv(t)
	hs := newRealtimeTestServer(t, nil)

	client, ctx := dialRealtime(t, hs, uuid.NewString(), uuid.NewString(), "model="+demo.ModelRealtime)

	// A real provider announces the session as soon as the socket opens; the
	// demo does the same, so the host leaves "connecting" immediately.
	first := readEvent(t, ctx, client)
	if first.Type != "session.created" {
		t.Fatalf("first event = %q, want session.created", first.Type)
	}

	// The host's own session.update, answered by session.updated.
	writeEvent(t, ctx, client, `{"type":"session.update","session":{"modalities":["text","audio"]}}`)
	if got := readEvent(t, ctx, client).Type; got != "session.updated" {
		t.Fatalf("event after session.update = %q, want session.updated", got)
	}

	writeEvent(t, ctx, client, `{"type":"response.create"}`)
	if got := readEvent(t, ctx, client).Type; got != "response.created" {
		t.Fatalf("event after response.create = %q, want response.created", got)
	}

	var (
		transcript strings.Builder
		frames     []string
		ea         = demo.ReplyChunks()
	)
	for {
		event := readEvent(t, ctx, client)
		frames = append(frames, event.Type)
		switch event.Type {
		case "response.audio_transcript.delta":
			transcript.WriteString(event.Delta)
		case "response.audio_transcript.done":
			if event.Transcript != transcript.String() {
				t.Fatalf("transcript.done = %q, want the concatenated deltas %q", event.Transcript, transcript.String())
			}
			if event.Transcript != demo.ReplyText {
				t.Fatalf("transcript.done text = %q, want the demo reply", event.Transcript)
			}
		case "response.done":
			if event.Response.Status != "completed" {
				t.Fatalf("response.done status = %q, want completed", event.Response.Status)
			}
			// The turn is exactly created + one delta per chunk + done, with no
			// invented event in between.
			want := 1 + len(ea) + 1
			if len(frames) != want {
				t.Fatalf("turn carried %d events (%v), want %d", len(frames), frames, want)
			}
			return
		default:
			t.Fatalf("unexpected event in the demo turn: %q", event.Type)
		}
	}
}

// TestRealtimeDemoModelRejectedWhenVendorIsSelected pins the request boundary:
// with a vendor provider selected, a demo model is a plain 400 — the demo model
// is only servable when the demo provider is the selected one.
func TestRealtimeDemoModelRejectedWhenVendorIsSelected(t *testing.T) {
	fp := newFakeRealtimeProvider(t)
	setFakeProviderEnv(t, fp)
	t.Setenv(demo.Env, "")
	hs := newRealtimeTestServer(t, nil)

	req, err := http.NewRequest(http.MethodGet,
		hs.URL+"/v1/realtime?session_id="+uuid.NewString()+"&model="+demo.ModelRealtime, nil)
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
	if got := fp.accepted.Load(); got != 0 {
		t.Fatalf("the vendor provider was dialed %d times for a rejected model", got)
	}
}

// realtimeEvent is the subset of a provider event these tests assert on.
type realtimeEvent struct {
	Type       string `json:"type"`
	Delta      string `json:"delta"`
	Transcript string `json:"transcript"`
	Response   struct {
		Status string `json:"status"`
	} `json:"response"`
}

func readEvent(t *testing.T, ctx context.Context, conn *websocket.Conn) realtimeEvent {
	t.Helper()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	var event realtimeEvent
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("event is not JSON: %v (%s)", err, data)
	}
	if event.Type == "" {
		t.Fatalf("event has no type: %s", data)
	}
	return event
}

func writeEvent(t *testing.T, ctx context.Context, conn *websocket.Conn, payload string) {
	t.Helper()
	if err := conn.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
		t.Fatalf("client write %s: %v", payload, err)
	}
}

// TestRealtimeDemoProviderIsSelectableByNameAndKind is a compile-level guard on
// the wiring the status endpoint promises: the demo provider the HTTP layer
// builds from a demo-enabled environment is the realtime.DemoProvider and
// reports the kind the browser host switches on.
func TestRealtimeDemoProviderIsSelectableByNameAndKind(t *testing.T) {
	provider, err := realtime.FromLookup(func(key string) string {
		if key == demo.Env {
			return "1"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	if got := realtime.ProviderName(provider); got != "demo" {
		t.Fatalf("ProviderName = %q, want demo", got)
	}
	if _, ok := provider.(*realtime.DemoProvider); !ok {
		t.Fatalf("provider = %T, want *realtime.DemoProvider", provider)
	}
}
