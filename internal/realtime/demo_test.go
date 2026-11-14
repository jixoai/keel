package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/demo"
)

// readDemoFrame reads one provider frame from a demo conn, failing the test
// instead of blocking forever if the conn stalls.
func readDemoFrame(t *testing.T, conn Conn) Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read demo frame: %v", err)
	}
	return msg
}

// requireNoDemoFrame asserts that the conn has nothing left to deliver. A demo
// turn is queued whole, so "no frame" is a positive fact, not a race: anything
// the demo meant to send is already in the queue.
func requireNoDemoFrame(t *testing.T, conn Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	msg, err := conn.Read(ctx)
	if err == nil {
		t.Fatalf("unexpected extra frame: %s", msg.Data)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read after the expected frames = %v, want a timeout", err)
	}
}

func writeDemoEvent(t *testing.T, conn Conn, payload string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, Message{Type: MessageText, Data: []byte(payload)}); err != nil {
		t.Fatalf("write %s: %v", payload, err)
	}
}

func TestDemoProviderValidatesItsOneModel(t *testing.T) {
	provider := NewDemoProvider()
	for _, model := range []string{"", "demo-realtime", " demo-realtime "} {
		if err := provider.ValidateModel(model); err != nil {
			t.Fatalf("ValidateModel(%q) = %v, want nil", model, err)
		}
	}
	for _, model := range []string{"qwen-omni-realtime", "demo-llm", "demo-realtime-x"} {
		err := provider.ValidateModel(model)
		if !errors.Is(err, ErrModelNotAllowed) {
			t.Fatalf("ValidateModel(%q) = %v, want ErrModelNotAllowed", model, err)
		}
	}
	// The same seam the HTTP layer calls must reject before Connect dials.
	if _, err := provider.Connect(context.Background(), Session{}, "nope"); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("Connect(nope) = %v, want ErrModelNotAllowed", err)
	}
}

func TestDemoProviderReportsItsCapabilities(t *testing.T) {
	provider := NewDemoProvider()
	if got := ProviderName(provider); got != string(KindDemo) {
		t.Fatalf("ProviderName = %q, want %q", got, KindDemo)
	}
	models, ok := RealtimeModels(provider)
	if !ok {
		t.Fatal("the demo provider must report its own model list")
	}
	if len(models) != 1 || models[0] != demo.ModelRealtime {
		t.Fatalf("RealtimeModels = %v, want [%s]", models, demo.ModelRealtime)
	}
	// A provider without the capability must not be mistaken for an empty one.
	if _, ok := RealtimeModels(bareProvider{}); ok {
		t.Fatal("bareProvider reported a model list")
	}
}

func TestDemoProviderSessionDefaultShape(t *testing.T) {
	var body struct {
		Type    string `json:"type"`
		Session struct {
			Modalities    []string `json:"modalities"`
			TurnDetection struct {
				Type string `json:"type"`
			} `json:"turn_detection"`
			Audio struct {
				Input struct {
					Format struct {
						Type       string `json:"type"`
						SampleRate int    `json:"sample_rate"`
						Channels   int    `json:"channels"`
					} `json:"format"`
				} `json:"input"`
				Output struct {
					Format struct {
						Type       string `json:"type"`
						SampleRate int    `json:"sample_rate"`
					} `json:"format"`
				} `json:"output"`
			} `json:"audio"`
			Transcription struct {
				Model string `json:"model"`
			} `json:"input_audio_transcription"`
		} `json:"session"`
	}
	if err := json.Unmarshal(NewDemoProvider().DefaultSessionUpdate(), &body); err != nil {
		t.Fatalf("demo session default is not valid JSON: %v", err)
	}
	if body.Type != "session.update" {
		t.Fatalf("type = %q, want session.update", body.Type)
	}
	if body.Session.TurnDetection.Type != "server_vad" {
		t.Fatalf("turn_detection = %q, want server_vad", body.Session.TurnDetection.Type)
	}
	if got := body.Session.Audio.Input.Format; got.Type != "pcm" || got.SampleRate != 16000 || got.Channels != 1 {
		t.Fatalf("input format = %+v, want pcm/16000/1 (the browser host's non-OpenAI default)", got)
	}
	if got := body.Session.Audio.Output.Format; got.Type != "pcm" || got.SampleRate != 24000 {
		t.Fatalf("output format = %+v, want pcm/24000 (the browser host's output default)", got)
	}
	if got := body.Session.Transcription.Model; got != demo.ModelSTT {
		t.Fatalf("input_audio_transcription.model = %q, want %q", got, demo.ModelSTT)
	}
}

// TestDemoTurnFrameSequence pins the whole answer to one response.create: the
// created event, the streaming deltas, the final transcript and the terminal
// done, in exactly that order, with the fields the shipped client reads.
func TestDemoTurnFrameSequence(t *testing.T) {
	provider := NewDemoProvider()
	conn, err := provider.Connect(context.Background(), Session{}, demo.ModelRealtime)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close(1000, "done")

	// A provider announces the session when the socket opens, so a client leaves
	// "connecting" without waiting for its grace timer.
	first := readDemoFrame(t, conn)
	if got := eventType(first); got != "session.created" {
		t.Fatalf("first frame = %q, want session.created", got)
	}

	writeDemoEvent(t, conn, `{"type":"session.update","session":{"modalities":["text","audio"]}}`)
	if got := eventType(readDemoFrame(t, conn)); got != "session.updated" {
		t.Fatalf("after session.update the demo sent %q, want session.updated", got)
	}

	writeDemoEvent(t, conn, `{"type":"response.create"}`)

	wantOrder := []string{"response.created"}
	deltas := demo.ReplyChunks()
	for range deltas {
		wantOrder = append(wantOrder, "response.audio_transcript.delta")
	}
	wantOrder = append(wantOrder, "response.audio_transcript.done", "response.done")

	var (
		order []string
		text  strings.Builder
	)
	for len(order) < len(wantOrder) {
		msg := readDemoFrame(t, conn)
		typ := eventType(msg)
		if typ == "" {
			t.Fatalf("demo frame is not a JSON event: %s", msg.Data)
		}
		order = append(order, typ)
		if typ == "response.audio_transcript.delta" {
			if keys := jsonKeys(t, msg.Data); !equalKeys(keys, "delta", "type") {
				t.Fatalf("delta frame carries fields %v, want exactly type+delta", keys)
			}
			var event struct {
				Delta string `json:"delta"`
			}
			if err := json.Unmarshal(msg.Data, &event); err != nil {
				t.Fatalf("delta frame is not JSON: %v", err)
			}
			text.WriteString(event.Delta)
		}
		if typ == "response.audio_transcript.done" {
			if keys := jsonKeys(t, msg.Data); !equalKeys(keys, "transcript", "type") {
				t.Fatalf("transcript.done carries fields %v, want exactly type+transcript", keys)
			}
			var event struct {
				Transcript string `json:"transcript"`
			}
			if err := json.Unmarshal(msg.Data, &event); err != nil {
				t.Fatalf("transcript.done is not JSON: %v", err)
			}
			if event.Transcript != text.String() {
				t.Fatalf("transcript.done = %q, want the concatenated deltas %q", event.Transcript, text.String())
			}
			if event.Transcript != demo.ReplyText {
				t.Fatalf("transcript.done text = %q, want the demo reply", event.Transcript)
			}
		}
		if typ == "response.done" {
			if keys := jsonKeys(t, msg.Data); !equalKeys(keys, "response", "type") {
				t.Fatalf("response.done carries fields %v, want exactly type+response", keys)
			}
			var event struct {
				Response struct {
					Status string `json:"status"`
				} `json:"response"`
			}
			if err := json.Unmarshal(msg.Data, &event); err != nil {
				t.Fatalf("response.done is not JSON: %v", err)
			}
			if event.Response.Status != "completed" {
				t.Fatalf("response.done status = %q, want completed", event.Response.Status)
			}
		}
	}
	if got := strings.Join(order, ","); got != strings.Join(wantOrder, ",") {
		t.Fatalf("frame order = %v, want %v", order, wantOrder)
	}
	requireNoDemoFrame(t, conn)
}

// TestDemoFirstAppendStartsOneTurn pins the microphone trigger: the first
// input_audio_buffer.append opens a turn, and the chunks that follow it (the
// microphone streams continuously) do not start another.
func TestDemoFirstAppendStartsOneTurn(t *testing.T) {
	provider := NewDemoProvider()
	conn, err := provider.Connect(context.Background(), Session{}, "")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close(1000, "done")
	if got := eventType(readDemoFrame(t, conn)); got != "session.created" {
		t.Fatalf("first frame = %q, want session.created", got)
	}

	writeDemoEvent(t, conn, `{"type":"input_audio_buffer.append","audio":"AAAA"}`)
	if got := eventType(readDemoFrame(t, conn)); got != "response.created" {
		t.Fatalf("first append started %q, want response.created", got)
	}
	// Drain the rest of the turn.
	for {
		msg := readDemoFrame(t, conn)
		if eventType(msg) == "response.done" {
			break
		}
	}
	// A second append is the same utterance still streaming: no second reply.
	writeDemoEvent(t, conn, `{"type":"input_audio_buffer.append","audio":"BBBB"}`)
	requireNoDemoFrame(t, conn)
}

// TestDemoCancelDropsTheRestOfTheTurn pins interrupt behavior: a cancel while
// the turn is still queued discards every undelivered frame and ends the turn
// with the cancelled done.
func TestDemoCancelDropsTheRestOfTheTurn(t *testing.T) {
	provider := NewDemoProvider()
	conn, err := provider.Connect(context.Background(), Session{}, demo.ModelRealtime)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close(1000, "done")
	if got := eventType(readDemoFrame(t, conn)); got != "session.created" {
		t.Fatalf("first frame = %q, want session.created", got)
	}

	writeDemoEvent(t, conn, `{"type":"response.create"}`)
	// Nothing is read, so the whole turn is still queued when the cancel lands.
	writeDemoEvent(t, conn, `{"type":"response.cancel"}`)

	msg := readDemoFrame(t, conn)
	if got := eventType(msg); got != "response.done" {
		t.Fatalf("frame after cancel = %q (%s), want response.done", got, msg.Data)
	}
	var event struct {
		Response struct {
			Status string `json:"status"`
		} `json:"response"`
	}
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		t.Fatalf("response.done is not JSON: %v", err)
	}
	if event.Response.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", event.Response.Status)
	}
	// The created event and every delta of that turn were dropped: no later
	// frame (a late delta, a second done) may follow the cancelled done.
	requireNoDemoFrame(t, conn)
}

// TestDemoCancelWithNothingInFlightIsSilent pins the other cancel path: the
// client sends a bare cancel when the user presses "打断" even if no response
// is active, and a demo must not invent an error row for it.
func TestDemoCancelWithNothingInFlightIsSilent(t *testing.T) {
	conn := newDemoConn()
	defer conn.Close(1000, "done")
	if got := eventType(readDemoFrame(t, conn)); got != "session.created" {
		t.Fatalf("first frame = %q, want session.created", got)
	}
	writeDemoEvent(t, conn, `{"type":"response.cancel"}`)
	requireNoDemoFrame(t, conn)
}

// TestDemoIgnoresUnrelatedClientEvents pins the pass-through rule: events the
// demo protocol does not answer (conversation items, tool chatter, non-JSON
// frames) never produce a frame.
func TestDemoIgnoresUnrelatedClientEvents(t *testing.T) {
	conn := newDemoConn()
	defer conn.Close(1000, "done")
	if got := eventType(readDemoFrame(t, conn)); got != "session.created" {
		t.Fatalf("first frame = %q, want session.created", got)
	}
	for _, payload := range []string{
		`{"type":"conversation.item.create","item":{"type":"message"}}`,
		`{"type":"input_audio_buffer.commit"}`,
		`not json at all`,
	} {
		writeDemoEvent(t, conn, payload)
		requireNoDemoFrame(t, conn)
	}
}

func TestDemoConnCloseUnblocksAndIsIdempotent(t *testing.T) {
	conn := newDemoConn()
	if got := eventType(readDemoFrame(t, conn)); got != "session.created" {
		t.Fatalf("first frame = %q, want session.created", got)
	}
	if err := conn.Close(1001, "bye"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := conn.Close(1000, "again"); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := conn.Read(ctx); err == nil {
		t.Fatal("Read after Close succeeded")
	} else {
		var closeErr *CloseError
		if !errors.As(err, &closeErr) || closeErr.Code != 1001 {
			t.Fatalf("Read after Close = %v, want CloseError 1001", err)
		}
	}
	// Writing into a closed conn is the bridge's error path, not a silent drop.
	if err := conn.Write(ctx, Message{Type: MessageText, Data: []byte(`{"type":"response.create"}`)}); err == nil {
		t.Fatal("Write after Close succeeded")
	}
}

// TestDemoTurnQueuesWithoutPacing pins the "no sleep" contract: the whole turn is
// readable the instant response.create is written. A pacing timer would show up
// as a read that has to wait for a later frame.
func TestDemoTurnQueuesWithoutPacing(t *testing.T) {
	conn := newDemoConn()
	defer conn.Close(1000, "done")
	_ = readDemoFrame(t, conn) // session.created
	writeDemoEvent(t, conn, `{"type":"response.create"}`)

	want := 2 + len(demo.ReplyChunks()) + 1 // created + deltas + transcript.done + done
	start := time.Now()
	for i := 0; i < want; i++ {
		msg := readDemoFrame(t, conn)
		if msg.Type != MessageText {
			t.Fatalf("frame %d is not a text event", i)
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("reading a whole demo turn took %v; the demo must not pace frames", elapsed)
	}
	requireNoDemoFrame(t, conn)
}

// TestFromLookupSelectionOrder pins how the demo provider and a real WebSocket
// provider share the environment. The order is documented on FromLookup; each
// case here is one line of that documentation.
func TestFromLookupSelectionOrder(t *testing.T) {
	const wssURL = "wss://workspace.example/api-ws/v1/realtime?model=qwen-omni-test"
	tests := []struct {
		name     string
		env      map[string]string
		wantKind Kind
		wantErr  error
	}{
		{
			name:    "neither configured",
			env:     map[string]string{},
			wantErr: ErrNotConfigured,
		},
		{
			name:     "demo only",
			env:      map[string]string{demo.Env: "1"},
			wantKind: KindDemo,
		},
		{
			name:     "demo enabled by an affirmative word",
			env:      map[string]string{demo.Env: "true"},
			wantKind: KindDemo,
		},
		{
			name:    "demo disabled by a typo",
			env:     map[string]string{demo.Env: "yes-please"},
			wantErr: ErrNotConfigured,
		},
		{
			name:     "vendor only",
			env:      map[string]string{envWSURL: wssURL, envProvider: "qwen"},
			wantKind: KindQwen,
		},
		{
			name:     "vendor url without an explicit family",
			env:      map[string]string{envWSURL: wssURL},
			wantKind: KindQwen,
		},
		{
			// The vendor endpoint outranks an enabled demo: turning the demo on
			// must not silently replace a working voice service.
			name:     "both configured, no family: vendor wins",
			env:      map[string]string{demo.Env: "1", envWSURL: wssURL},
			wantKind: KindQwen,
		},
		{
			// An explicit family wins over the demo flag even with a URL present.
			name:     "both configured, explicit qwen: vendor wins",
			env:      map[string]string{demo.Env: "1", envWSURL: wssURL, envProvider: "qwen"},
			wantKind: KindQwen,
		},
		{
			// An explicit demo wins over a configured URL.
			name:     "both configured, explicit demo: demo wins",
			env:      map[string]string{demo.Env: "1", envWSURL: wssURL, envProvider: "demo"},
			wantKind: KindDemo,
		},
		{
			// Pinning the demo provider with the switch off is a configuration
			// error, never a silent fallback to the vendor URL.
			name:    "explicit demo without the switch",
			env:     map[string]string{envWSURL: wssURL, envProvider: "demo"},
			wantErr: nil, // asserted separately: a non-nil error that is not ErrNotConfigured
		},
		{
			name:    "explicit family without a url",
			env:     map[string]string{envProvider: "openai"},
			wantErr: ErrNotConfigured,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, err := FromLookup(lookupFrom(tt.env))
			if tt.name == "explicit demo without the switch" {
				if err == nil {
					t.Fatalf("FromLookup = %T, want a configuration error", provider)
				}
				if errors.Is(err, ErrNotConfigured) {
					t.Fatalf("err = %v, want a diagnostic (not the silent not-configured state)", err)
				}
				if !strings.Contains(err.Error(), demo.Env) {
					t.Fatalf("err = %v, want it to name %s", err, demo.Env)
				}
				return
			}
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("FromLookup: %v", err)
			}
			if got := Kind(ProviderName(provider)); got != tt.wantKind {
				t.Fatalf("provider = %q, want %q", got, tt.wantKind)
			}
		})
	}
}

// TestFromLookupExplicitDemoStillHonorsModelPolicy is the "demo + vendor"
// overlap that matters in practice: an explicit demo never falls back to the
// vendor URL or its allowlist, and the demo still rejects another provider's
// model.
func TestFromLookupExplicitDemoRejectsVendorModels(t *testing.T) {
	provider, err := FromLookup(lookupFrom(map[string]string{
		demo.Env:        "1",
		envWSURL:        "wss://workspace.example/api-ws/v1/realtime?model=qwen-omni-test",
		envProvider:     "demo",
		envModels:       "qwen-omni-test,qwen-omni-other",
		envAPIKey:       "vendor-key",
		envModel:        "qwen-omni-test",
		envDashScopeKey: "",
	}))
	if err != nil {
		t.Fatalf("FromLookup: %v", err)
	}
	if _, ok := provider.(*DemoProvider); !ok {
		t.Fatalf("provider = %T, want *DemoProvider", provider)
	}
	validator, ok := provider.(interface{ ValidateModel(string) error })
	if !ok {
		t.Fatal("the demo provider must expose ValidateModel")
	}
	if err := validator.ValidateModel("qwen-omni-test"); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("ValidateModel(vendor model) = %v, want ErrModelNotAllowed", err)
	}
	if models, _ := RealtimeModels(provider); len(models) != 1 || models[0] != demo.ModelRealtime {
		t.Fatalf("RealtimeModels = %v, want [%s]", models, demo.ModelRealtime)
	}
}

// jsonKeys returns the top-level keys of a JSON object.
func jsonKeys(t *testing.T, data []byte) []string {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatalf("not a JSON object: %v (%s)", err, data)
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	return keys
}

// equalKeys reports whether keys holds exactly the wanted names (order-free).
func equalKeys(keys []string, want ...string) bool {
	if len(keys) != len(want) {
		return false
	}
	seen := make(map[string]int, len(keys))
	for _, key := range keys {
		seen[key]++
	}
	for _, key := range want {
		seen[key]--
		if seen[key] < 0 {
			return false
		}
	}
	for _, count := range seen {
		if count != 0 {
			return false
		}
	}
	return true
}
