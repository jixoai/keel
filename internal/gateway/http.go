// HTTPTransport: the Streamable HTTP half of the gateway transport pair.
//
// One POST per message; the answer is either application/json (a message or a
// batch of messages) or text/event-stream, consumed in the background so
// streaming responses and progress notifications arrive as they happen. After
// notifications/initialized the transport also opens the server-to-client GET
// stream, which is how an AgentBackend pushes events over HTTP; it reconnects
// with bounded backoff and resumes with Last-Event-ID.
//
// Memory is bounded the same way as on stdio: JSON bodies are read through a
// limit, SSE events are capped, and every message goes into the bounded queue,
// so a stalled consumer stops reading the body and the server's write blocks.

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Headers shared with MCP's Streamable HTTP transport.
const (
	headerSessionID       = "Mcp-Session-Id"
	headerProtocolVersion = "MCP-Protocol-Version"
	headerLastEventID     = "Last-Event-ID"
)

// HTTP transport defaults.
const (
	defaultStreamRetries  = 5
	defaultStreamMinDelay = 250 * time.Millisecond
	defaultStreamMaxDelay = 10 * time.Second
	maxHTTPErrorBodyBytes = 8 * 1024
	errorMessageBodyChars = 500
)

// HTTPTransportOptions configures a Streamable HTTP connection.
type HTTPTransportOptions struct {
	// URL is the provider endpoint. Required.
	URL string
	// Headers are sent with every request (authorization, tenant hints of the
	// provider's own, ...). Secrets belong in a provider-specific header
	// resolver, never hard-coded; see README.md.
	Headers map[string]string
	// Client is used for every request. Nil uses http.DefaultClient.
	Client *http.Client
	// MaxFrameBytes bounds one JSON body, one SSE event and one outbound message.
	MaxFrameBytes int
	// MaxQueuedMessages and MaxQueuedBytes bound the inbound queue.
	MaxQueuedMessages int
	MaxQueuedBytes    int64
	// DisableEventStream keeps the server-to-client GET stream closed.
	DisableEventStream bool
	// StreamRetries bounds GET stream reconnect attempts before giving up.
	StreamRetries int
}

// HTTPTransport is a Transport over Streamable HTTP.
type HTTPTransport struct {
	opts    HTTPTransportOptions
	url     string
	client  *http.Client
	queue   *messageQueue
	maxSize int

	mu              sync.Mutex
	started         bool
	closed          bool
	handler         func(Diagnostic)
	baseCtx         context.Context
	cancel          context.CancelFunc
	sessionID       string
	protocolVersion string
	streamStarted   bool
	lastEventID     string

	closeOnce sync.Once
	closedCh  chan struct{}

	sent     atomic.Int64
	received atomic.Int64
	skipped  atomic.Int64
}

// NewHTTPTransport returns the transport; Start begins use.
func NewHTTPTransport(opts HTTPTransportOptions) *HTTPTransport {
	if opts.Client == nil {
		opts.Client = http.DefaultClient
	}
	if opts.MaxFrameBytes <= 0 {
		opts.MaxFrameBytes = DefaultMaxFrameBytes
	}
	return &HTTPTransport{
		opts:     opts,
		url:      opts.URL,
		client:   opts.Client,
		queue:    newMessageQueue(opts.MaxQueuedMessages, opts.MaxQueuedBytes),
		maxSize:  opts.MaxFrameBytes,
		closedCh: make(chan struct{}),
	}
}

// Start prepares the transport. It performs no I/O: the first exchange happens
// on the first Send.
func (t *HTTPTransport) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.opts.URL == "" {
		return errors.New("gateway: HTTP transport requires a URL")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return &TransportError{Reason: "http transport is closed", Err: ErrTransportClosed}
	}
	if t.started {
		return errors.New("gateway: HTTP transport already started")
	}
	t.started = true
	t.baseCtx, t.cancel = context.WithCancel(context.Background())
	return nil
}

// SetProtocolVersion sets the revision sent in the MCP-Protocol-Version
// header; the connection layer calls it right after initialize.
func (t *HTTPTransport) SetProtocolVersion(version string) {
	t.mu.Lock()
	t.protocolVersion = version
	t.mu.Unlock()
}

// SessionID is the session id the server assigned, if any.
func (t *HTTPTransport) SessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

// Send POSTs one message. Requests are answered either in the response body or
// on a stream consumed in the background; notifications return once the server
// acknowledged them.
func (t *HTTPTransport) Send(ctx context.Context, message Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.mu.Lock()
	if !t.started || t.closed {
		t.mu.Unlock()
		return &TransportError{Reason: "http transport is not connected", Err: ErrTransportClosed}
	}
	baseCtx := t.baseCtx
	t.mu.Unlock()

	body, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("gateway: encode JSON-RPC message: %w", err)
	}
	if len(body) > t.maxSize {
		return &FrameError{Kind: FrameTooLarge, Direction: "outbound", Size: len(body), Limit: t.maxSize}
	}

	// The request context must end when either the caller gives up or the
	// transport closes, so Close cannot leave a POST hanging. A background
	// stream consumer inherits the cancellation instead (handedOff).
	requestCtx, cancelRequest := context.WithCancel(ctx)
	handedOff := false
	defer func() {
		if !handedOff {
			cancelRequest()
		}
	}()
	go func() {
		select {
		case <-baseCtx.Done():
			cancelRequest()
		case <-requestCtx.Done():
		}
	}()

	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return &TransportError{Reason: "http request", Err: err}
	}
	request.Header.Set("content-type", "application/json")
	request.Header.Set("accept", "application/json, text/event-stream")
	t.applyHeaders(request)
	t.sent.Add(1)

	response, err := t.client.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return &TransportError{Reason: "http send", Err: err}
	}
	t.mu.Lock()
	if sid := response.Header.Get(headerSessionID); sid != "" {
		t.sessionID = sid
	}
	if version := response.Header.Get(headerProtocolVersion); version != "" {
		t.protocolVersion = version
	}
	closed := t.closed
	t.mu.Unlock()
	if closed {
		response.Body.Close()
		return &TransportError{Reason: "http transport closed during send", Err: ErrTransportClosed}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		excerpt := readBoundedExcerpt(response.Body, maxHTTPErrorBodyBytes)
		response.Body.Close()
		return &HTTPStatusError{Status: response.StatusCode, Body: excerpt}
	}

	if _, isRequest := message.(*Request); !isRequest {
		// A notification's response body carries nothing; drain a bounded
		// amount so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 8*1024))
		response.Body.Close()
		return nil
	}
	if response.StatusCode == http.StatusAccepted || response.StatusCode == http.StatusNoContent {
		response.Body.Close()
		// Accepted without a body: the response, if any, arrives on the stream.
		return nil
	}
	switch contentType(response) {
	case "application/json":
		defer response.Body.Close()
		data, truncated, err := readBounded(response.Body, t.maxSize)
		if err != nil {
			return &TransportError{Reason: "http response body", Err: err}
		}
		if truncated {
			return &FrameError{
				Kind:   FrameTooLarge,
				Size:   t.maxSize,
				Limit:  t.maxSize,
				Detail: fmt.Sprintf("JSON response over %d bytes", t.maxSize),
			}
		}
		messages, sizes, err := decodeJSONMessages(data)
		if err != nil {
			return err
		}
		for index, item := range messages {
			if !t.queue.push(item, sizes[index]) {
				return &TransportError{Reason: "http transport closed while queueing the response", Err: ErrTransportClosed}
			}
			t.received.Add(1)
		}
		return nil
	case "text/event-stream":
		// The stream may stay open after the response: consume it in the
		// background so Send returns and the caller can settle its request.
		// The consumer owns the request context from here: canceling it on
		// return would abort the body read.
		requestID := message.(*Request).ID
		handedOff = true
		go func() {
			defer cancelRequest()
			t.consumeResponseStream(response.Body, requestID)
		}()
		return nil
	default:
		response.Body.Close()
		return &HTTPStatusError{
			Status: response.StatusCode,
			Body:   fmt.Sprintf("unsupported response content type %q", orMissing(contentType(response))),
		}
	}
}

// Receive returns the next message pushed by any response or the event stream.
func (t *HTTPTransport) Receive(ctx context.Context) (Message, error) {
	return receiveMessage(ctx, t.queue)
}

// Close aborts in-flight streams, deletes the server session (best effort) and
// releases the connection. It is idempotent.
func (t *HTTPTransport) Close() error {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		sessionID := t.sessionID
		started := t.started
		cancel := t.cancel
		t.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		close(t.closedCh)
		t.queue.close(&TransportError{Reason: "http transport closed", Err: ErrTransportClosed})
		if started && sessionID != "" {
			ctx, cancelDelete := context.WithTimeout(context.Background(), time.Second)
			request, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.url, nil)
			if err == nil {
				t.applyHeaders(request)
				if response, err := t.client.Do(request); err == nil {
					_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
					response.Body.Close()
				}
			}
			cancelDelete()
		}
	})
	return nil
}

// Stats is the transport counters snapshot.
func (t *HTTPTransport) Stats() TransportStats {
	queued, bytes, peakItems, peakBytes := t.queue.stats()
	return TransportStats{
		Sent:            t.sent.Load(),
		Received:        t.received.Load(),
		Skipped:         t.skipped.Load(),
		Queued:          int64(queued),
		QueuedBytes:     bytes,
		PeakQueued:      peakItems,
		PeakQueuedBytes: peakBytes,
	}
}

// SetDiagnosticHandler installs the diagnostic sink.
func (t *HTTPTransport) SetDiagnosticHandler(handler func(Diagnostic)) {
	t.mu.Lock()
	t.handler = handler
	t.mu.Unlock()
}

// consumeResponseStream reads the SSE body answering one request. If the
// stream ends before the response arrived, the request fails with a definite
// error instead of hanging until its timeout.
func (t *HTTPTransport) consumeResponseStream(body io.ReadCloser, requestID ID) {
	defer body.Close()
	answered := false
	reader := newSSEReader(body, t.maxSize)
	for {
		event, err := reader.ReadEvent()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.report(Diagnostic{Kind: DiagStreamError, Detail: err.Error()})
			}
			if !answered && !t.isClosed() {
				reason := "response stream ended without a response"
				if !errors.Is(err, io.EOF) {
					reason += ": " + err.Error()
				}
				_ = t.queue.push(&ErrorResponse{
					JSONRPC: JSONRPCVersion,
					ID:      requestID,
					Error:   ErrorObject{Code: CodeInternalError, Message: reason},
				}, 0)
			}
			return
		}
		if !t.deliverEvent(event, &answered, requestID) {
			return
		}
	}
}

// deliverEvent parses and queues one SSE event; it reports false when the
// transport closed and consumption must stop.
func (t *HTTPTransport) deliverEvent(event sseEvent, answered *bool, requestID ID) bool {
	if event.ID != "" {
		t.mu.Lock()
		t.lastEventID = event.ID
		t.mu.Unlock()
	}
	if event.Event != "" && event.Event != "message" {
		return true
	}
	if strings.TrimSpace(event.Data) == "" {
		return true
	}
	message, err := ParseMessage([]byte(event.Data))
	if err != nil {
		t.report(Diagnostic{Kind: DiagMessageInvalid, Detail: fmt.Sprintf("dropped a %d byte SSE event: %v", len(event.Data), err)})
		t.skipped.Add(1)
		return true
	}
	if answered != nil && responseMatches(message, requestID) {
		*answered = true
	}
	if !t.queue.push(message, len(event.Data)) {
		return false
	}
	t.received.Add(1)
	return true
}

// StartEventStream opens the server-to-client GET stream (once). The
// connection layer calls it after the handshake, when the provider is known to
// be an AgentBackend that pushes events.
func (t *HTTPTransport) StartEventStream() { t.startEventStream() }

// startEventStream opens the server-to-client GET stream once.
func (t *HTTPTransport) startEventStream() {
	t.mu.Lock()
	if t.opts.DisableEventStream || t.streamStarted || t.closed {
		t.mu.Unlock()
		return
	}
	t.streamStarted = true
	t.mu.Unlock()
	go t.runEventStream()
}

// runEventStream keeps the GET stream open, reconnecting with bounded backoff.
func (t *HTTPTransport) runEventStream() {
	retries := t.opts.StreamRetries
	if retries <= 0 {
		retries = defaultStreamRetries
	}
	attempt := 0
	for !t.isClosed() {
		openedAt := time.Now()
		body, err := t.openEventStream()
		if err != nil {
			if t.isClosed() {
				return
			}
			t.report(Diagnostic{Kind: DiagStreamError, Detail: err.Error()})
		} else if body == nil {
			return // the server offers no event stream (405)
		} else {
			received, err := t.consumeEventStream(body)
			_ = body.Close()
			if t.isClosed() {
				return
			}
			if err != nil && !errors.Is(err, io.EOF) {
				t.report(Diagnostic{Kind: DiagStreamError, Detail: err.Error()})
			}
			if received || time.Since(openedAt) >= defaultStreamMaxDelay {
				attempt = 0
			}
		}
		attempt++
		if attempt > retries {
			return
		}
		if !t.sleep(streamDelay(attempt)) {
			return
		}
	}
}

// consumeEventStream drains one GET stream; it reports whether any event was
// delivered.
func (t *HTTPTransport) consumeEventStream(body io.ReadCloser) (bool, error) {
	reader := newSSEReader(body, t.maxSize)
	received := false
	for {
		event, err := reader.ReadEvent()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return received, nil
			}
			return received, err
		}
		if !t.deliverEvent(event, nil, ID{}) {
			return received, nil
		}
		received = true
	}
}

// openEventStream opens the GET stream; (nil, nil) means the server has none.
func (t *HTTPTransport) openEventStream() (io.ReadCloser, error) {
	t.mu.Lock()
	baseCtx, lastEventID := t.baseCtx, t.lastEventID
	t.mu.Unlock()
	request, err := http.NewRequestWithContext(baseCtx, http.MethodGet, t.url, nil)
	if err != nil {
		return nil, &TransportError{Reason: "http event stream request", Err: err}
	}
	request.Header.Set("accept", "text/event-stream")
	if lastEventID != "" {
		request.Header.Set(headerLastEventID, lastEventID)
	}
	t.applyHeaders(request)
	response, err := t.client.Do(request)
	if err != nil {
		return nil, &TransportError{Reason: "http event stream", Err: err}
	}
	if response.StatusCode == http.StatusMethodNotAllowed {
		response.Body.Close()
		return nil, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		excerpt := readBoundedExcerpt(response.Body, maxHTTPErrorBodyBytes)
		response.Body.Close()
		return nil, &HTTPStatusError{Status: response.StatusCode, Body: excerpt}
	}
	t.mu.Lock()
	if sid := response.Header.Get(headerSessionID); sid != "" {
		t.sessionID = sid
	}
	t.mu.Unlock()
	if contentType(response) != "text/event-stream" {
		response.Body.Close()
		return nil, &HTTPStatusError{
			Status: response.StatusCode,
			Body:   fmt.Sprintf("unsupported event stream content type %q", orMissing(contentType(response))),
		}
	}
	return response.Body, nil
}

func (t *HTTPTransport) applyHeaders(request *http.Request) {
	for name, value := range t.opts.Headers {
		request.Header.Set(name, value)
	}
	t.mu.Lock()
	sessionID, version := t.sessionID, t.protocolVersion
	t.mu.Unlock()
	if sessionID != "" {
		request.Header.Set(headerSessionID, sessionID)
	}
	if version != "" {
		request.Header.Set(headerProtocolVersion, version)
	}
}

func (t *HTTPTransport) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

func (t *HTTPTransport) report(diagnostic Diagnostic) {
	t.mu.Lock()
	handler := t.handler
	t.mu.Unlock()
	if handler != nil {
		handler(diagnostic)
	}
}

// sleep waits for d; false means the transport closed meanwhile.
func (t *HTTPTransport) sleep(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return !t.isClosed()
	case <-t.closedCh:
		return false
	}
}

// streamDelay is exponential backoff for stream reconnects.
func streamDelay(attempt int) time.Duration {
	delay := float64(defaultStreamMinDelay) * math.Pow(2, float64(attempt-1))
	if delay > float64(defaultStreamMaxDelay) {
		return defaultStreamMaxDelay
	}
	return time.Duration(delay)
}

// decodeJSONMessages decodes a response body into messages, accepting a single
// object or a batch array, and reports each message's wire size.
func decodeJSONMessages(data []byte) ([]Message, []int, error) {
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}))
	if len(trimmed) == 0 {
		return nil, nil, &ProtocolError{Code: CodeParseError, Message: "empty JSON response body"}
	}
	items := []json.RawMessage{trimmed}
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, nil, &ProtocolError{Code: CodeParseError, Message: "invalid JSON response body: " + err.Error()}
		}
	}
	messages := make([]Message, 0, len(items))
	sizes := make([]int, 0, len(items))
	for _, item := range items {
		message, err := ParseMessage(item)
		if err != nil {
			return nil, nil, err
		}
		messages = append(messages, message)
		sizes = append(sizes, len(item))
	}
	return messages, sizes, nil
}

// responseMatches reports whether message is the response for requestID.
func responseMatches(message Message, requestID ID) bool {
	if requestID.IsZero() {
		return false
	}
	switch typed := message.(type) {
	case *SuccessResponse:
		return typed.ID.Key() == requestID.Key()
	case *ErrorResponse:
		return typed.ID.Key() == requestID.Key()
	default:
		return false
	}
}

// readBounded reads at most max bytes and reports whether more were available.
func readBounded(reader io.Reader, max int) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(reader, int64(max)+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > max {
		return data[:max], true, nil
	}
	return data, false, nil
}

// readBoundedExcerpt keeps only the bounded excerpt an error message reports.
func readBoundedExcerpt(reader io.Reader, max int) string {
	data, _, _ := readBounded(reader, max)
	text := strings.TrimSpace(string(data))
	if len(text) > errorMessageBodyChars {
		text = text[:errorMessageBodyChars-3] + "..."
	}
	return text
}

// contentType is the lower-cased media type without parameters.
func contentType(response *http.Response) string {
	value := response.Header.Get("content-type")
	if index := strings.IndexByte(value, ';'); index >= 0 {
		value = value[:index]
	}
	return strings.ToLower(strings.TrimSpace(value))
}

func orMissing(value string) string {
	if value == "" {
		return "missing"
	}
	return value
}
