package agentapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/httpapi"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/stream"
)

// TestPartialsSSEBypass is the A5 acceptance: token partials flow over the same
// SSE endpoint when (and only when) the client asks for them, they never carry
// an SSE id, they never enter the event table, and a reconnect with
// Last-Event-ID replays only committed events.
func TestPartialsSSEBypass(t *testing.T) {
	st := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := testLogger()
	klog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	kern := kernel.New(klog, kernel.Options{})
	hub := httpapi.NewHub(st, log)
	go hub.Run(ctx)
	partials := agentext.NewPartialHub()

	product := New(Config{
		Sessions: NewPGSessionLister(st),
		Partials: partials,
		Logger:   log,
	})
	edge := httpapi.New(httpapi.Config{
		Store:     st,
		Hub:       hub,
		Edge:      kern,
		Events:    kern,
		Scheduler: stream.NewScheduler(16),
		Admitter:  stream.NewAdmitter(100, 200),
		Metrics:   &stream.Metrics{},
		Logger:    log,
		Product:   product,
		Partials:  PartialSource(partials),
	})
	server := httptest.NewServer(edge)
	defer server.Close()

	tenantID, sessionID := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenantID, "partials"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenantID, sessionID); err != nil {
		t.Fatal(err)
	}
	commitUser := func(text string) int64 {
		payload, _ := json.Marshal(map[string]string{"text": text})
		receipt, err := kern.Commit(ctx, kernel.CommitRequest{
			Scope:     kernel.Scope{TenantID: tenantID, SessionID: sessionID},
			Kind:      kernel.KindUserMessage,
			RequestID: text,
			Payload:   payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		return receipt.Seq
	}
	firstSeq := commitUser("first")

	// The product API is mounted at /v1/ by the runtime edge.
	var capabilities Capabilities
	if code := doJSON(t, server.Client(), http.MethodGet, server.URL+"/v1/capabilities", tenantID, nil, &capabilities); code != 200 || !capabilities.Features["partials"] {
		t.Fatalf("capabilities: %d %+v", code, capabilities)
	}

	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	request, err := http.NewRequestWithContext(streamCtx, http.MethodGet,
		server.URL+"/v1/sessions/"+sessionID.String()+"/events/stream?partials=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Tenant-ID", tenantID.String())
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("stream status: %d", response.StatusCode)
	}
	frames := frameChannel(bufio.NewReader(response.Body))

	// The committed event is replayed first; then a partial arrives (published
	// in a loop so the subscription is certainly live).
	frame := readFrame(t, frames)
	if frame.event != string(kernel.KindUserMessage) || frame.id != fmt.Sprintf("%d", firstSeq) {
		t.Fatalf("first frame = %+v", frame)
	}
	publishUntil(t, partials, tenantID, sessionID)
	partial := readUntilEvent(t, frames, "partial", 5*time.Second)
	if partial.id != "" {
		t.Fatalf("a partial frame must not carry an SSE id: %+v", partial)
	}
	var partialBody agentext.Partial
	if err := json.Unmarshal([]byte(partial.data), &partialBody); err != nil {
		t.Fatalf("partial body: %v (%s)", err, partial.data)
	}
	if partialBody.Type == "" || partialBody.At.IsZero() {
		t.Fatalf("partial body = %+v", partialBody)
	}

	secondSeq := commitUser("second")
	committed := readUntilEvent(t, frames, string(kernel.KindUserMessage), 5*time.Second)
	if committed.id != fmt.Sprintf("%d", secondSeq) {
		t.Fatalf("committed frame after partial = %+v (want id %d)", committed, secondSeq)
	}
	cancelStream()

	// Partials published while disconnected are gone for good: they are not
	// stored anywhere the reconnect could read.
	for i := 0; i < 10; i++ {
		partials.Publish(tenantID, sessionID, agentext.Partial{Type: "assistant.delta", Delta: "missed"})
	}
	time.Sleep(20 * time.Millisecond)

	// Reconnect with Last-Event-ID: the first frame must be the next committed
	// event, never a replayed partial.
	reconnectCtx, cancelReconnect := context.WithCancel(ctx)
	defer cancelReconnect()
	reconnect, err := http.NewRequestWithContext(reconnectCtx, http.MethodGet,
		server.URL+"/v1/sessions/"+sessionID.String()+"/events/stream?partials=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	reconnect.Header.Set("X-Tenant-ID", tenantID.String())
	reconnect.Header.Set("Last-Event-ID", committed.id)
	again, err := server.Client().Do(reconnect)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Body.Close()
	againFrames := frameChannel(bufio.NewReader(again.Body))

	thirdSeq := commitUser("third")
	resumed, ok := nextFrame(t, againFrames, 10*time.Second)
	if !ok {
		t.Fatal("reconnect produced no frame")
	}
	if resumed.event != string(kernel.KindUserMessage) || resumed.id != fmt.Sprintf("%d", thirdSeq) {
		t.Fatalf("the first frame after reconnect must be the committed event (a partial was replayed?): %+v", resumed)
	}
	// A live partial is still delivered to the reconnected stream: the bypass is
	// live, it just has no replay.
	publishUntil(t, partials, tenantID, sessionID)
	live := readUntilEvent(t, againFrames, "partial", 5*time.Second)
	if live.id != "" {
		t.Fatalf("live partial carries an id: %+v", live)
	}

	// The event table holds exactly the committed messages: partials are not
	// persisted.
	events, err := kern.LoadEvents(ctx, kernel.Scope{TenantID: tenantID, SessionID: sessionID}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Kind == kernel.KindUserMessage {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("event table holds %d user messages, want 3", count)
	}
}

type sseFrame struct {
	id    string
	event string
	data  string
}

// frameChannel is the single reader of an SSE response. One goroutine parses
// frames and publishes them on the channel, so a caller timeout never abandons a
// read that could steal the next frame.
func frameChannel(reader *bufio.Reader) <-chan sseFrame {
	ch := make(chan sseFrame)
	go func() {
		defer close(ch)
		for {
			frame, ok := parseFrame(reader)
			if !ok {
				return
			}
			ch <- frame
		}
	}()
	return ch
}

func parseFrame(reader *bufio.Reader) (sseFrame, bool) {
	var frame sseFrame
	seen := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return sseFrame{}, false
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if seen {
				return frame, true
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		seen = true
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch key {
		case "id":
			frame.id = value
		case "event":
			frame.event = value
		case "data":
			frame.data = value
		}
	}
}

func nextFrame(t *testing.T, frames <-chan sseFrame, timeout time.Duration) (sseFrame, bool) {
	t.Helper()
	select {
	case frame, ok := <-frames:
		return frame, ok
	case <-time.After(timeout):
		return sseFrame{}, false
	}
}

func readFrame(t *testing.T, frames <-chan sseFrame) sseFrame {
	t.Helper()
	frame, ok := nextFrame(t, frames, 10*time.Second)
	if !ok {
		t.Fatal("timed out waiting for an SSE frame")
	}
	return frame
}

func readUntilEvent(t *testing.T, frames <-chan sseFrame, event string, timeout time.Duration) sseFrame {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		frame, ok := nextFrame(t, frames, time.Until(deadline))
		if !ok {
			break
		}
		if frame.event == event {
			return frame
		}
	}
	t.Fatalf("event %q did not arrive", event)
	return sseFrame{}
}

// publishUntil publishes one partial per attempt until it is delivered; the
// SSE subscription is set up after the response headers are flushed, so the
// first attempts can be lost.
func publishUntil(t *testing.T, hub *agentext.PartialHub, tenantID, sessionID uuid.UUID) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hub.Publish(tenantID, sessionID, agentext.Partial{Type: "assistant.delta", Delta: "hello"}) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no partial subscriber appeared")
}
