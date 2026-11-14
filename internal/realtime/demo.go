package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/jixoai/keel/internal/demo"
)

// The demo realtime provider: an in-process channel that speaks the provider event
// family the bridge and the client already understand.
//
// # Why a provider and not an HTTP stub
//
// The bridge (Proxy) pumps opaque messages between the browser socket and a Conn. A
// demo that answered the WebSocket directly would be a second bridge, and the
// client would talk to something the production path never exercises. Making the
// demo a RealtimeProvider instead means the real bridge, the real session defaults,
// the real close propagation and the real browser socket are all in the path — only
// the far leg is local.
//
// # What it answers
//
//   - session.created        when the channel opens, so a client leaves "connecting"
//     without waiting for its grace timer
//   - session.update         → session.updated (the client re-sends its config)
//   - response.create        → one assistant turn (see demoResponseEvents)
//   - input_audio_buffer.append → one assistant turn, for the *first* chunk only
//   - response.cancel        → a cancelled response.done, discarding the turn
//   - anything else          → no reply (a provider ignores events it does not act
//     on)
//
// The microphone streams continuously and the demo never reads audio, so it cannot
// segment utterances: answering every chunk would produce a reply per 100 ms of
// speech. It answers the first chunk — which is what makes pressing "talk" in a
// review produce an answer — and leaves later turns to `response.create`, which a
// client sends once it has a real transcript.
//
// # Whole-turn queue, no pacing
//
// A turn is queued whole and delivered as fast as the reader pulls it. The demo has
// no model to wait for, so a timer would only make a review slower and a test
// flaky; the client's own streaming projection is what animates the arrival. The
// queue is also what makes interruption honest: a cancel that lands before the
// client read anything discards the entire turn, and a cancel after delivery starts
// still ends the turn with the cancelled done.

// demoDefaultModel is the model a demo voice session runs.
const demoDefaultModel = demo.ModelRealtime

// DemoProvider is the in-process realtime provider. It satisfies RealtimeProvider,
// SessionDefaulter, ProviderNamer and ModelLister, so the bridge and the status
// endpoint treat it like any other provider.
type DemoProvider struct{}

// NewDemoProvider builds the in-process realtime provider.
func NewDemoProvider() *DemoProvider { return &DemoProvider{} }

// Connect implements RealtimeProvider. It validates the model against the demo
// allowlist and returns an in-memory Conn. The session identifiers are not used:
// the demo keeps no per-user state, so it cannot accumulate one session's data into
// another.
func (p *DemoProvider) Connect(ctx context.Context, _ Session, model string) (Conn, error) {
	if err := p.ValidateModel(model); err != nil {
		return nil, err
	}
	return newDemoConn(), nil
}

// ValidateModel reports whether a client-supplied model may be used. The demo
// serves exactly one model; an empty request resolves to it, and surrounding
// whitespace is tolerated (a query string is user input).
func (p *DemoProvider) ValidateModel(model string) error {
	requested := strings.TrimSpace(model)
	if requested == "" || requested == demoDefaultModel {
		return nil
	}
	return fmt.Errorf("%w: %q", ErrModelNotAllowed, requested)
}

// ProviderName implements ProviderNamer.
func (p *DemoProvider) ProviderName() string { return string(KindDemo) }

// RealtimeModels implements ModelLister: the status endpoint reports exactly what
// this provider serves, without re-deriving a policy from the environment (the demo
// has no environment).
func (p *DemoProvider) RealtimeModels() []string { return []string{demoDefaultModel} }

// DefaultSessionUpdate implements SessionDefaulter with the shape the browser host
// actually consumes: the nested session.audio input/output formats (pcm, 16 kHz in,
// 24 kHz out, mono), server VAD and the demo STT model for input transcription. The
// nested (Qwen-style) shape is deliberate: the bundled host reads those fields, and
// a flat OpenAI default would leave its input format undetected.
func (p *DemoProvider) DefaultSessionUpdate() []byte {
	return append([]byte(nil), demoSessionUpdate...)
}

// demoSessionUpdate is the session default the demo channel declares.
var demoSessionUpdate = []byte(`{"type":"session.update","session":{"modalities":["text","audio"],"turn_detection":{"type":"server_vad"},"audio":{"input":{"format":{"type":"pcm","sample_rate":16000,"sample_format":"s16le","channels":1,"packing":"interleaved","channel_layout":"mono"}},"output":{"voice":"demo","format":{"type":"pcm","sample_rate":24000}}},"input_audio_transcription":{"model":"` + demo.ModelSTT + `"}}}`)

// demoQueueDepth bounds one turn's queue. One turn is a created event plus one delta
// per chunk plus the transcript.done and the terminal done; the bound is a safety
// net for a producer bug, not a content contract (see push: exceeding it drops the
// frame rather than growing the queue).
const demoQueueDepth = 64

// demoConn is the in-memory provider leg. The bridge reads provider→client messages
// and writes client→provider messages on two different goroutines, so the queue is
// mutex-guarded and a signal channel wakes a blocked reader:
//
//   - a buffered `notify` channel of capacity 1 means "there is something to read";
//     a reader that drains the queue loops back before parking, so a push between
//     drain and park is never lost;
//   - the queue is a slice, not a channel, because a cancel has to discard frames
//     it never delivered — a channel cannot be emptied without consuming it.
type demoConn struct {
	mu     sync.Mutex
	queue  []Message
	notify chan struct{}

	closed    chan struct{}
	closeOnce sync.Once
	closeCode int

	// answeredAppend reports whether the first microphone chunk of this session has
	// already been answered (see the package comment).
	answeredAppend bool
}

func newDemoConn() *demoConn {
	conn := &demoConn{
		queue:  make([]Message, 0, demoQueueDepth),
		notify: make(chan struct{}, 1),
		closed: make(chan struct{}),
	}
	// Announce the session as soon as the channel opens.
	conn.push(demoEvent(map[string]any{
		"type": "session.created",
		"session": map[string]any{
			"id":     "sess-demo-0001",
			"model":  demoDefaultModel,
			"status": "active",
		},
	}))
	return conn
}

// handle applies one client message. It runs on the caller's goroutine (the
// bridge's client→provider pump), so the state it touches is only this conn's.
func (c *demoConn) handle(message Message) {
	if message.Type != MessageText {
		return
	}
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(message.Data, &event); err != nil {
		return
	}
	switch event.Type {
	case "session.update":
		c.push(demoEvent(map[string]any{
			"type": "session.updated",
			"session": map[string]any{
				"id":     "sess-demo-0001",
				"model":  demoDefaultModel,
				"status": "active",
			},
		}))
	case "response.create":
		c.queueTurn()
	case "input_audio_buffer.append":
		// The microphone streams many chunks per utterance and the demo cannot tell
		// them apart (it does not read audio), so it answers the first one only.
		if !c.answeredAppend {
			c.answeredAppend = true
			c.queueTurn()
		}
	case "response.cancel":
		c.cancelTurn()
	}
}

// queueTurn appends one assistant turn to the queue.
func (c *demoConn) queueTurn() {
	for _, frame := range demoResponseEvents() {
		c.push(frame)
	}
}

// cancelTurn discards every undelivered frame and ends the turn with the cancelled
// done. With nothing queued the cancel is silent: pressing "打断" when nothing is
// running must not invent an error row.
func (c *demoConn) cancelTurn() {
	c.mu.Lock()
	pending := len(c.queue)
	c.queue = c.queue[:0]
	c.mu.Unlock()
	if pending == 0 {
		return
	}
	c.push(demoEvent(map[string]any{
		"type":     "response.done",
		"response": map[string]any{"id": demo.ResponseID, "status": "cancelled"},
	}))
}

// push enqueues one frame and wakes a parked reader. A closed conn drops the frame:
// the peer is gone and the bridge is unwinding.
//
// The depth bound drops the frame rather than blocking the producer: a demo whose
// caller never reads must not pin the bridge's write pump.
func (c *demoConn) push(message Message) {
	select {
	case <-c.closed:
		return
	default:
	}
	c.mu.Lock()
	if len(c.queue) < demoQueueDepth {
		c.queue = append(c.queue, message)
	}
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

// pop removes the next queued frame.
func (c *demoConn) pop() (Message, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.queue) == 0 {
		return Message{}, false
	}
	message := c.queue[0]
	c.queue = c.queue[1:]
	return message, true
}

// demoResponseEvents is one complete assistant turn: the created marker, one
// transcript delta per chunk, the final transcript and the terminal done. The order
// and the field sets match a real provider's, so the client walks the same states
// (thinking → streaming → final).
func demoResponseEvents() []Message {
	deltas := demo.ReplyChunks()
	events := make([]Message, 0, len(deltas)+3)
	events = append(events, demoEvent(map[string]any{
		"type":     "response.created",
		"response": map[string]any{"id": demo.ResponseID, "status": "in_progress"},
	}))
	var transcript string
	for _, delta := range deltas {
		transcript += delta
		events = append(events, demoEvent(map[string]any{
			"type":  "response.audio_transcript.delta",
			"delta": delta,
		}))
	}
	events = append(events, demoEvent(map[string]any{
		"type":       "response.audio_transcript.done",
		"transcript": transcript,
	}))
	events = append(events, demoEvent(map[string]any{
		"type":     "response.done",
		"response": map[string]any{"id": demo.ResponseID, "status": "completed"},
	}))
	return events
}

// demoEvent marshals one provider event. A failure here is a programming defect (the
// documents are closed shapes) and would silently drop a turn event, so it panics.
func demoEvent(payload map[string]any) Message {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic("realtime: marshal demo event: " + err.Error())
	}
	return Message{Type: MessageText, Data: raw}
}

// Read implements Conn.
func (c *demoConn) Read(ctx context.Context) (Message, error) {
	for {
		if message, ok := c.pop(); ok {
			return message, nil
		}
		select {
		case <-ctx.Done():
			return Message{}, ctx.Err()
		case <-c.closed:
			return Message{}, &CloseError{Code: c.closeCodeOrDefault()}
		case <-c.notify:
			// A wake-up with nothing to read means another reader won the frame (or a
			// cancel dropped it): loop back and park again.
		}
	}
}

// Write implements Conn. It applies the message immediately and reports the close to
// a caller writing into a closing conn, so the bridge stops pumping.
func (c *demoConn) Write(ctx context.Context, message Message) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return &CloseError{Code: c.closeCodeOrDefault()}
	default:
	}
	c.handle(message)
	return nil
}

// Close implements Conn. It is idempotent and unblocks both directions.
func (c *demoConn) Close(code int, _ string) error {
	c.closeOnce.Do(func() {
		c.closeCode = code
		close(c.closed)
	})
	return nil
}

// closeCodeOrDefault reports the close code a reader sees. A zero code means the
// conn was never closed with one; 1000 (normal) is the honest default.
func (c *demoConn) closeCodeOrDefault() int {
	if c.closeCode == 0 {
		return 1000
	}
	return c.closeCode
}
