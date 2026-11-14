package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
)

// Direction names one leg of the bridge. The value is safe to log.
type Direction string

const (
	// ClientToProvider is the browser -> provider leg.
	ClientToProvider Direction = "client_to_provider"
	// ProviderToClient is the provider -> browser leg.
	ProviderToClient Direction = "provider_to_client"
)

// EndedBy values reported in ProxyResult.
const (
	EndedByClient   = "client"
	EndedByProvider = "provider"
	EndedByContext  = "context"
)

// Event is the metadata of one forwarded message. It never carries payload
// bytes: Type is the top-level JSON "type" (empty for non-JSON or binary
// messages) and Bytes is the payload length. This is the only thing the bridge
// exposes to logging.
type Event struct {
	Direction Direction
	Type      string
	Bytes     int
	// Injected marks the session.update the bridge synthesized because the
	// client never sent one.
	Injected bool
}

// ProxyOptions tunes one bridge run.
type ProxyOptions struct {
	// DefaultSessionUpdate is sent to the provider before the first client
	// message when the client never sent a session.update of its own. Empty
	// disables the injection.
	DefaultSessionUpdate []byte
	// OnEvent observes forwarded message metadata. It must not block: it runs on
	// the pump goroutine.
	OnEvent func(Event)
	// MaxMessageSize overrides DefaultMaxMessageSize.
	MaxMessageSize int
}

// ProxyResult describes how a bridge run ended.
type ProxyResult struct {
	// EndedBy is the side that ended the session first: EndedByClient,
	// EndedByProvider or EndedByContext.
	EndedBy string
	// Code is the WebSocket close code propagated to both peers.
	Code int
	// Err is the first failure of that side (nil is not used: a clean close
	// surfaces as a CloseError with code 1000).
	Err error
}

// ErrMessageTooLarge is returned by a pump when a single message exceeds the
// configured bound. The bridge closes both legs with 1009 (message too big).
var ErrMessageTooLarge = errors.New("realtime: message too large")

// closeGrace bounds how long Proxy waits for the second pump after closing both
// connections. The WebSocket transport unblocks on Close, so this is a safety
// net, not the normal path.
const closeGrace = 5 * time.Second

type pumpResult struct {
	failedSide string
	err        error
}

// Proxy pumps messages in both directions until either side ends, then closes
// both connections with the peer's close code and returns. It owns neither
// connection before the call nor after it returns: both are closed.
func Proxy(ctx context.Context, client, provider Conn, opts ProxyOptions) ProxyResult {
	// The pumps do not read with the caller's context: cancelling the context a
	// WebSocket read waits on tears the socket down without a close frame, which
	// would turn every orderly shutdown into an abnormal 1006 for the peer. The
	// caller's cancellation is observed by the select below; Close unblocks the
	// reads, and pumpCancel is the safety net for a misbehaving Conn.
	pumpCtx, pumpCancel := context.WithCancel(context.Background())
	defer pumpCancel()

	results := make(chan pumpResult, 2)
	go func() {
		results <- pump(pumpCtx, client, provider, ClientToProvider, opts)
	}()
	go func() {
		results <- pump(pumpCtx, provider, client, ProviderToClient, opts)
	}()

	received := 0
	var res ProxyResult
	select {
	case first := <-results:
		received = 1
		res.EndedBy, res.Err = first.failedSide, first.err
	case <-ctx.Done():
		res.EndedBy, res.Err = EndedByContext, ctx.Err()
	}
	// A pump can observe the caller's cancellation before the select does; the
	// session still ended because the caller cancelled, not because a peer left.
	if ctx.Err() != nil {
		res.EndedBy, res.Err = EndedByContext, ctx.Err()
	}

	code, reason := closeCode(res.Err)
	if res.EndedBy == EndedByContext {
		code, reason = 1001, "server shutting down"
	}
	res.Code = code

	// Close both legs first so the pending reads unblock with a proper close
	// frame, then stop the pumps. The two handshakes run concurrently: a dead
	// peer must not delay closing the other (and the provider) leg.
	var closes sync.WaitGroup
	closes.Add(2)
	go func() {
		defer closes.Done()
		_ = client.Close(code, reason)
	}()
	go func() {
		defer closes.Done()
		_ = provider.Close(code, reason)
	}()
	closes.Wait()
	pumpCancel()

	// Drain the second pump. Every Conn implementation the bridge ships unblocks
	// on Close; the timer only guards a misbehaving extension.
	timer := time.NewTimer(closeGrace)
	defer timer.Stop()
	for received < 2 {
		select {
		case <-results:
			received++
		case <-timer.C:
			return res
		}
	}
	return res
}

// pump forwards messages from src to dst until a read or write fails.
func pump(ctx context.Context, src, dst Conn, dir Direction, opts ProxyOptions) pumpResult {
	limit := opts.MaxMessageSize
	if limit <= 0 {
		limit = DefaultMaxMessageSize
	}
	sawSessionUpdate := dir != ClientToProvider
	for {
		msg, err := src.Read(ctx)
		if err != nil {
			return pumpResult{failedSide: sourceSide(dir), err: err}
		}
		if len(msg.Data) > limit {
			return pumpResult{failedSide: sourceSide(dir), err: ErrMessageTooLarge}
		}
		if dir == ClientToProvider && !sawSessionUpdate {
			sawSessionUpdate = true
			if !isSessionUpdate(msg) && len(opts.DefaultSessionUpdate) > 0 {
				injected := Message{Type: MessageText, Data: opts.DefaultSessionUpdate}
				if err := dst.Write(ctx, injected); err != nil {
					return pumpResult{failedSide: destinationSide(dir), err: err}
				}
				observe(opts, Event{Direction: dir, Type: "session.update", Bytes: len(injected.Data), Injected: true})
			}
		}
		if err := dst.Write(ctx, msg); err != nil {
			return pumpResult{failedSide: destinationSide(dir), err: err}
		}
		observe(opts, Event{Direction: dir, Type: eventType(msg), Bytes: len(msg.Data)})
	}
}

func sourceSide(dir Direction) string {
	if dir == ClientToProvider {
		return EndedByClient
	}
	return EndedByProvider
}

func destinationSide(dir Direction) string {
	if dir == ClientToProvider {
		return EndedByProvider
	}
	return EndedByClient
}

func observe(opts ProxyOptions, ev Event) {
	if opts.OnEvent != nil {
		opts.OnEvent(ev)
	}
}

// eventType extracts the top-level "type" of a JSON event. Decoding into a
// one-field struct skips the remaining values without copying them (in
// particular the base64 audio payload), so this stays cheap enough to run on
// every message.
func eventType(msg Message) string {
	if msg.Type != MessageText || len(msg.Data) == 0 {
		return ""
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(msg.Data, &head); err != nil {
		return ""
	}
	return head.Type
}

func isSessionUpdate(msg Message) bool { return eventType(msg) == "session.update" }

// closeCode maps a pump failure to the close code both peers receive. A peer
// close is propagated; a transport failure becomes 1011; a clean end of stream
// (or the caller's cancellation) becomes 1000/1001.
func closeCode(err error) (int, string) {
	if err == nil {
		return 1000, ""
	}
	var ce *CloseError
	if errors.As(err, &ce) {
		switch {
		case ce.Code == 1005:
			// No status received is a normal close without a code; it must never
			// be echoed on the wire, so answer with 1000.
			return 1000, ""
		case sendableCloseCode(ce.Code):
			return ce.Code, ce.Reason
		default:
			return 1011, ""
		}
	}
	switch {
	case errors.Is(err, ErrMessageTooLarge):
		return 1009, "message too big"
	case errors.Is(err, context.Canceled), errors.Is(err, io.EOF):
		return 1000, ""
	default:
		return 1011, "connection failed"
	}
}

// sendableCloseCode reports whether code may appear in a close frame. 1004,
// 1005, 1006 and 1015 are reserved and must never be sent on the wire.
func sendableCloseCode(code int) bool {
	switch code {
	case 1004, 1005, 1006, 1015:
		return false
	}
	return code >= 1000 && code <= 4999
}
