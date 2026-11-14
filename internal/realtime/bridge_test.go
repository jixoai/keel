package realtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeConn is an in-memory Conn: reads come from reads, writes go to writes,
// and Close unblocks both like a WebSocket close frame would.
type fakeConn struct {
	reads  chan Message
	writes chan Message

	closed    chan struct{}
	closeOnce sync.Once
	closeCode atomic.Int32
}

func newFakeConn() *fakeConn {
	return &fakeConn{
		reads:  make(chan Message, 16),
		writes: make(chan Message, 16),
		closed: make(chan struct{}),
	}
}

func (c *fakeConn) Read(ctx context.Context) (Message, error) {
	select {
	case <-ctx.Done():
		return Message{}, ctx.Err()
	case <-c.closed:
		return Message{}, &CloseError{Code: c.code()}
	case msg := <-c.reads:
		return msg, nil
	}
}

func (c *fakeConn) Write(ctx context.Context, msg Message) error {
	select {
	case <-c.closed:
		return &CloseError{Code: c.code()}
	case <-ctx.Done():
		return ctx.Err()
	case c.writes <- msg:
		return nil
	}
}

func (c *fakeConn) Close(code int, _ string) error {
	c.closeOnce.Do(func() {
		c.closeCode.Store(int32(code))
		close(c.closed)
	})
	return nil
}

func (c *fakeConn) code() int { return int(c.closeCode.Load()) }

func (c *fakeConn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for a message")
		var zero T
		return zero
	}
}

func TestProxyInjectsDefaultSessionAndPassesEvents(t *testing.T) {
	client, provider := newFakeConn(), newFakeConn()
	defaultSession := []byte(`{"type":"session.update","session":{"modalities":["text","audio"]}}`)
	audio := base64.StdEncoding.EncodeToString([]byte("not-a-real-audio-frame"))
	appendEvent := []byte(`{"type":"input_audio_buffer.append","audio":"` + audio + `"}`)
	deltaEvent := []byte(`{"type":"response.audio.delta","delta":"` + audio + `"}`)

	var mu sync.Mutex
	var events []Event
	done := make(chan ProxyResult, 1)
	go func() {
		done <- Proxy(context.Background(), client, provider, ProxyOptions{
			DefaultSessionUpdate: defaultSession,
			OnEvent: func(ev Event) {
				mu.Lock()
				events = append(events, ev)
				mu.Unlock()
			},
		})
	}()

	client.reads <- Message{Type: MessageText, Data: appendEvent}

	injected := receive(t, provider.writes)
	if got := eventType(injected); got != "session.update" {
		t.Fatalf("first provider message type = %q, want session.update", got)
	}
	if !bytes.Equal(injected.Data, defaultSession) {
		t.Fatalf("injected session.update = %s, want %s", injected.Data, defaultSession)
	}
	forwarded := receive(t, provider.writes)
	if !bytes.Equal(forwarded.Data, appendEvent) {
		t.Fatalf("forwarded client event = %s, want %s", forwarded.Data, appendEvent)
	}

	provider.reads <- Message{Type: MessageText, Data: deltaEvent}
	gotDelta := receive(t, client.writes)
	if !bytes.Equal(gotDelta.Data, deltaEvent) {
		t.Fatalf("client received %s, want %s", gotDelta.Data, deltaEvent)
	}
	if gotDelta.Type != MessageText {
		t.Fatalf("client message type = %v, want text", gotDelta.Type)
	}

	// Closing the client ends the bridge and propagates the code to the provider.
	if err := client.Close(1000, "bye"); err != nil {
		t.Fatalf("client close: %v", err)
	}
	result := receive(t, done)
	if result.EndedBy != EndedByClient || result.Code != 1000 {
		t.Fatalf("result = %+v, want client/1000", result)
	}
	if got := provider.code(); got != 1000 {
		t.Fatalf("provider close code = %d, want 1000", got)
	}
	if !client.isClosed() || !provider.isClosed() {
		t.Fatal("both legs must be closed when the bridge returns")
	}

	mu.Lock()
	defer mu.Unlock()
	var injectedSeen bool
	for _, ev := range events {
		if ev.Injected && ev.Type == "session.update" {
			injectedSeen = true
		}
		if ev.Bytes == 0 {
			t.Fatalf("event metadata must carry a size: %+v", ev)
		}
		if strings.Contains(ev.Type, audio) {
			t.Fatalf("event metadata leaked a payload: %+v", ev)
		}
	}
	if !injectedSeen {
		t.Fatalf("injected session.update was not reported to OnEvent: %+v", events)
	}
}

func TestProxyForwardsClientSessionUpdateWithoutInjecting(t *testing.T) {
	client, provider := newFakeConn(), newFakeConn()
	clientSession := []byte(`{"type":"session.update","session":{"modalities":["audio"],"instructions":"client"}}`)
	appendEvent := []byte(`{"type":"input_audio_buffer.append","audio":"AAAA"}`)

	var mu sync.Mutex
	var events []Event
	done := make(chan ProxyResult, 1)
	go func() {
		done <- Proxy(context.Background(), client, provider, ProxyOptions{
			DefaultSessionUpdate: []byte(`{"type":"session.update","session":{"instructions":"default"}}`),
			OnEvent: func(ev Event) {
				mu.Lock()
				events = append(events, ev)
				mu.Unlock()
			},
		})
	}()

	client.reads <- Message{Type: MessageText, Data: clientSession}
	first := receive(t, provider.writes)
	if !bytes.Equal(first.Data, clientSession) {
		t.Fatalf("provider first message = %s, want the client session.update", first.Data)
	}
	client.reads <- Message{Type: MessageText, Data: appendEvent}
	second := receive(t, provider.writes)
	if !bytes.Equal(second.Data, appendEvent) {
		t.Fatalf("provider second message = %s, want the append event", second.Data)
	}

	if err := provider.Close(1000, "done"); err != nil {
		t.Fatalf("provider close: %v", err)
	}
	result := receive(t, done)
	if result.EndedBy != EndedByProvider || result.Code != 1000 {
		t.Fatalf("result = %+v, want provider/1000", result)
	}
	if got := client.code(); got != 1000 {
		t.Fatalf("client close code = %d, want 1000", got)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, ev := range events {
		if ev.Injected {
			t.Fatalf("bridge injected a session.update even though the client sent one: %+v", ev)
		}
	}
}

func TestProxyClosesBothLegsWithProviderErrorCode(t *testing.T) {
	client, provider := newFakeConn(), newFakeConn()
	provider.Close(1011, "provider blew up")

	result := Proxy(context.Background(), client, provider, ProxyOptions{})
	if result.EndedBy != EndedByProvider {
		t.Fatalf("EndedBy = %q, want %q", result.EndedBy, EndedByProvider)
	}
	if result.Code != 1011 {
		t.Fatalf("close code = %d, want 1011", result.Code)
	}
	if got := client.code(); got != 1011 {
		t.Fatalf("client close code = %d, want 1011", got)
	}
}

func TestProxyHonorsContextCancellation(t *testing.T) {
	client, provider := newFakeConn(), newFakeConn()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan ProxyResult, 1)
	go func() { done <- Proxy(ctx, client, provider, ProxyOptions{}) }()

	cancel()
	result := receive(t, done)
	if result.EndedBy != EndedByContext {
		t.Fatalf("EndedBy = %q, want %q", result.EndedBy, EndedByContext)
	}
	if result.Code != 1001 {
		t.Fatalf("close code = %d, want 1001", result.Code)
	}
	if got := client.code(); got != 1001 {
		t.Fatalf("client close code = %d, want 1001", got)
	}
	if got := provider.code(); got != 1001 {
		t.Fatalf("provider close code = %d, want 1001", got)
	}
}

func TestProxyRejectsOversizedMessages(t *testing.T) {
	client, provider := newFakeConn(), newFakeConn()
	done := make(chan ProxyResult, 1)
	go func() {
		done <- Proxy(context.Background(), client, provider, ProxyOptions{MaxMessageSize: 8})
	}()

	client.reads <- Message{Type: MessageText, Data: bytes.Repeat([]byte("a"), 9)}
	result := receive(t, done)
	if result.EndedBy != EndedByClient {
		t.Fatalf("EndedBy = %q, want client", result.EndedBy)
	}
	if result.Code != 1009 {
		t.Fatalf("close code = %d, want 1009", result.Code)
	}
	if got := provider.code(); got != 1009 {
		t.Fatalf("provider close code = %d, want 1009", got)
	}
}

func TestCloseErrorDoesNotCarryReason(t *testing.T) {
	err := &CloseError{Code: 1008, Reason: "secret transcript text"}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("CloseError.Error leaked the reason: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "1008") {
		t.Fatalf("CloseError.Error = %q, want the code", err.Error())
	}
}

func TestEventType(t *testing.T) {
	tests := []struct {
		name string
		msg  Message
		want string
	}{
		{"text event", Message{Type: MessageText, Data: []byte(`{"type":"response.audio.delta","delta":"AAAA"}`)}, "response.audio.delta"},
		{"nested first", Message{Type: MessageText, Data: []byte(`{"session":{"type":"nested"},"type":"session.created"}`)}, "session.created"},
		{"missing type", Message{Type: MessageText, Data: []byte(`{"delta":"AAAA"}`)}, ""},
		{"invalid json", Message{Type: MessageText, Data: []byte(`not json`)}, ""},
		{"binary", Message{Type: MessageBinary, Data: []byte(`{"type":"x"}`)}, ""},
		{"empty", Message{Type: MessageText}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := eventType(tt.msg); got != tt.want {
				t.Fatalf("eventType() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCloseCode(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
	}{
		{"nil", nil, 1000},
		{"close", &CloseError{Code: 1001}, 1001},
		{"reserved close", &CloseError{Code: 1006}, 1011},
		{"no status close", &CloseError{Code: 1005}, 1000},
		{"message too large", ErrMessageTooLarge, 1009},
		{"canceled", context.Canceled, 1000},
		{"other", errors.New("boom"), 1011},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _ := closeCode(tt.err)
			if code != tt.wantCode {
				t.Fatalf("closeCode(%v) = %d, want %d", tt.err, code, tt.wantCode)
			}
		})
	}
}
