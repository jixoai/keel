// Behaviour ported from @earendil-works/pi-mcp (https://github.com/earendil-works/pi),
// packages/mcp/src/transports/streamable-http.ts and src/auth-provider.ts
// (MIT License); implemented independently in Go. Contract kept identical; see
// docs/UPSTREAM.md.
//
// The Streamable HTTP transport POSTs each JSON-RPC message and reads the reply
// either as application/json or as an SSE stream, keeps the MCP session and
// protocol version headers, opens the server-to-client GET stream after
// initialization, resumes streams with Last-Event-ID, and hands 401 /
// insufficient-scope 403 responses to an AuthProvider once.

package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// maxHTTPErrorBodyBytes is the excerpt kept from a failed response body.
	maxHTTPErrorBodyBytes = 8 * 1024
	// errorMessageBodyChars bounds the body excerpt in the error message.
	errorMessageBodyChars = 500
	defaultReconnectDelay = time.Second
	defaultReconnectMax   = 30 * time.Second
	defaultReconnectTries = 5
)

// AuthProvider supplies bearer tokens for a Streamable HTTP MCP server and may
// refresh them after a 401 (or a 403 asking for more scope).
type AuthProvider interface {
	// Token returns the token for the next request. Returning "" sends none.
	Token(ctx context.Context) (string, error)
	// OnUnauthorized is called once with the rejected response. The request is
	// retried with whatever credentials Token returns afterwards.
	OnUnauthorized(ctx context.Context, event UnauthorizedEvent) error
}

// UnauthorizedEvent describes an authorization failure handed to an
// AuthProvider.
type UnauthorizedEvent struct {
	Status          int
	WWWAuthenticate string
	Body            string
	ServerURL       string
	// Token is the access token the rejected request carried, if any.
	Token string
	// Client is the HTTP client of the rejected request, so OAuth discovery and
	// token requests share its transport.
	Client *http.Client
}

// HTTPError is a non-2xx response from an MCP server.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	text := strings.TrimSpace(e.Body)
	if len(text) > errorMessageBodyChars {
		text = text[:errorMessageBodyChars-3] + "..."
	}
	if text == "" {
		return fmt.Sprintf("MCP HTTP request failed with status %d", e.Status)
	}
	return fmt.Sprintf("MCP HTTP request failed with status %d: %s", e.Status, text)
}

// AuthRequiredError is a 401, or a 403 whose bearer challenge reports
// insufficient_scope (step-up authorization).
type AuthRequiredError struct {
	HTTPError
	WWWAuthenticate string
}

// SessionExpiredError is a 404 after a session was established.
type SessionExpiredError struct {
	HTTPError
}

// SSEEvent is one dispatched server-sent event.
type SSEEvent struct {
	Event string
	Data  string
	ID    string
}

// SSEOptions configures ConsumeSSEStream.
type SSEOptions struct {
	MaxEventBytes int
	OnEvent       func(SSEEvent)
	// OnID receives every id field, including events without data.
	OnID func(string)
	// OnRetry receives every valid retry field.
	OnRetry func(time.Duration)
}

// ConsumeSSEStream parses an SSE stream. Lines may end with CR, LF, or CRLF
// (also split across reads); comments are ignored; data lines join with "\n".
func ConsumeSSEStream(r io.Reader, options SSEOptions) error {
	maxBytes := options.MaxEventBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxMessageBytes
	}
	reader := bufio.NewReaderSize(r, 32*1024)
	var (
		line          []byte
		eventName     string
		eventID       string
		dataLines     []string
		dataBytes     int
		skipLeadingLF bool
		lineBytes     int
	)
	fail := func() error { return fmt.Errorf("MCP SSE event exceeds %d bytes", maxBytes) }
	dispatch := func() {
		if len(dataLines) == 0 {
			eventName, eventID = "", ""
			return
		}
		event := SSEEvent{Data: strings.Join(dataLines, "\n")}
		if eventName != "" {
			event.Event = eventName
		}
		if eventID != "" {
			event.ID = eventID
		}
		if options.OnEvent != nil {
			options.OnEvent(event)
		}
		eventName, eventID = "", ""
		dataLines = dataLines[:0]
		dataBytes = 0
	}
	processLine := func(raw []byte) error {
		if len(raw) == 0 {
			dispatch()
			return nil
		}
		if raw[0] == ':' {
			return nil
		}
		field, value := string(raw), ""
		if colon := bytes.IndexByte(raw, ':'); colon >= 0 {
			field = string(raw[:colon])
			value = string(raw[colon+1:])
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			dataBytes += len(value)
			if len(dataLines) > 0 {
				dataBytes++
			}
			if dataBytes > maxBytes {
				return fail()
			}
			dataLines = append(dataLines, value)
		case "event":
			eventName = value
		case "id":
			if !strings.ContainsRune(value, 0) {
				eventID = value
				if options.OnID != nil {
					options.OnID(value)
				}
			}
		case "retry":
			if value != "" && strings.Trim(value, "0123456789") == "" {
				if ms, err := strconv.ParseInt(value, 10, 64); err == nil && options.OnRetry != nil {
					options.OnRetry(time.Duration(ms) * time.Millisecond)
				}
			}
		}
		return nil
	}

	for {
		b, err := reader.ReadByte()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return err
			}
			// Deliver the unterminated tail line, then the pending event.
			if skipLeadingLF {
				if len(line) > 0 && line[0] == '\n' {
					line = line[1:]
				}
				skipLeadingLF = false
			}
			if len(line) > 0 {
				if err := processLine(line); err != nil {
					return err
				}
			}
			dispatch()
			return nil
		}
		if skipLeadingLF {
			skipLeadingLF = false
			if b == '\n' {
				continue
			}
		}
		if b == '\r' || b == '\n' {
			if err := processLine(line); err != nil {
				return err
			}
			line = line[:0]
			lineBytes = 0
			if b == '\r' {
				next, peekErr := reader.Peek(1)
				if peekErr == nil && len(next) == 1 && next[0] == '\n' {
					_, _ = reader.ReadByte()
				} else if peekErr != nil && errors.Is(peekErr, io.EOF) {
					// The CR ended the line; nothing follows.
				} else {
					skipLeadingLF = true
				}
			}
			continue
		}
		line = append(line, b)
		lineBytes++
		if lineBytes > maxBytes {
			return fail()
		}
	}
}

// ReconnectOptions configures stream reconnection backoff.
type ReconnectOptions struct {
	InitialDelay time.Duration
	MaxDelay     time.Duration
	MaxRetries   int
}

// HTTPTransportOptions configures a Streamable HTTP transport.
type HTTPTransportOptions struct {
	URL     string
	Headers map[string]string
	// Client is used for every request. Nil uses http.DefaultClient.
	Client *http.Client
	// DisableGetStream keeps the server-to-client GET stream closed.
	DisableGetStream bool
	// MaxMessageBytes bounds JSON responses and SSE events.
	MaxMessageBytes int
	AuthProvider    AuthProvider
	Reconnect       ReconnectOptions
}

// HTTPTransport is the Streamable HTTP MCP transport.
type HTTPTransport struct {
	options  HTTPTransportOptions
	url      string
	client   *http.Client
	maxBytes int

	mu               sync.Mutex
	started          bool
	closed           bool
	terminal         error
	queue            []Message
	wake             chan struct{}
	closedCh         chan struct{}
	closeOnce        sync.Once
	baseCtx          context.Context
	cancel           context.CancelFunc
	sessionID        string
	protocolVersion  string
	lastToken        string
	getStreamStarted bool
	receiveErr       error
}

// NewHTTPTransport creates the transport; Start begins use.
func NewHTTPTransport(options HTTPTransportOptions) *HTTPTransport {
	client := options.Client
	if client == nil {
		client = http.DefaultClient
	}
	maxBytes := options.MaxMessageBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxMessageBytes
	}
	return &HTTPTransport{
		options:  options,
		url:      options.URL,
		client:   client,
		maxBytes: maxBytes,
		wake:     make(chan struct{}, 1),
		closedCh: make(chan struct{}),
	}
}

func (t *HTTPTransport) Start(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return &ConnectionClosedError{}
	}
	if t.started {
		return fmt.Errorf("mcp: HTTP transport already started")
	}
	t.started = true
	t.baseCtx, t.cancel = context.WithCancel(context.Background())
	return nil
}

// SetProtocolVersion sets the MCP-Protocol-Version header sent with requests.
func (t *HTTPTransport) SetProtocolVersion(version string) {
	t.mu.Lock()
	t.protocolVersion = version
	t.mu.Unlock()
}

// SessionID is the Mcp-Session-Id the server assigned, if any.
func (t *HTTPTransport) SessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

// Send POSTs one message and, for a request, reads its reply from the JSON body
// or the SSE response stream.
func (t *HTTPTransport) Send(ctx context.Context, message Message) error {
	t.mu.Lock()
	started, closed := t.started, t.closed
	baseCtx := t.baseCtx
	t.mu.Unlock()
	if !started || closed {
		return &ConnectionClosedError{}
	}
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	headers := map[string]string{
		"accept":       "application/json, text/event-stream",
		"content-type": "application/json",
	}
	response, err := t.authorizedFetch(baseCtx, ctx, http.MethodPost, headers, body)
	if err != nil {
		return err
	}
	t.mu.Lock()
	if sid := response.Header.Get("mcp-session-id"); sid != "" {
		t.sessionID = sid
	}
	protocol := t.protocolVersion
	t.mu.Unlock()
	if protocol != "" {
		// Nothing to do: buildHeaders already read the current version for the
		// next request. The server may have set one on this response; capture it.
		if version := response.Header.Get("mcp-protocol-version"); version != "" {
			t.SetProtocolVersion(version)
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		err := t.responseError(response, bodyExcerpt(response))
		response.Body.Close()
		return err
	}

	request, isRequest := message.(*Request)
	if !isRequest {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
		response.Body.Close()
		if notification, ok := message.(*Notification); ok && notification.Method == "notifications/initialized" {
			t.startGetStream()
		}
		return nil
	}
	if response.StatusCode == http.StatusAccepted || response.StatusCode == http.StatusNoContent {
		response.Body.Close()
		return &HTTPError{Status: response.StatusCode, Body: fmt.Sprintf("MCP server accepted request %s without a response", request.Method)}
	}
	mediaType := contentType(response)
	switch mediaType {
	case "application/json":
		defer response.Body.Close()
		data, truncated, err := readLimited(response.Body, t.maxBytes)
		if err != nil {
			return err
		}
		if truncated {
			return fmt.Errorf("MCP JSON response exceeds %d bytes", t.maxBytes)
		}
		data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
		var decoded any
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Errorf("mcp: parse JSON response: %w", err)
		}
		items, ok := decoded.([]any)
		if !ok {
			items = []any{decoded}
		}
		for _, item := range items {
			encoded, err := json.Marshal(item)
			if err != nil {
				return err
			}
			parsed, err := ParseMessage(encoded)
			if err != nil {
				return err
			}
			t.push(parsed)
		}
		return nil
	case "text/event-stream":
		// The stream may stay open after the response: consume it in the
		// background so Send returns and the client can receive the message.
		go func() {
			defer response.Body.Close()
			t.consumeResponseStream(response.Body, request.ID, t.maxBytes)
		}()
		return nil
	default:
		response.Body.Close()
		return &HTTPError{Status: response.StatusCode, Body: fmt.Sprintf("Unsupported MCP response content type: %s", orMissing(mediaType))}
	}
}

// Close aborts streams and best-effort deletes the server session. It is
// idempotent and never asks the auth provider for a new token.
func (t *HTTPTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	token := t.lastToken
	sessionID := t.sessionID
	started := t.started
	if t.cancel != nil {
		t.cancel()
	}
	t.terminal = &ConnectionClosedError{}
	t.closeOnce.Do(func() { close(t.closedCh) })
	t.mu.Unlock()
	t.signalWake()

	if started && sessionID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.url, nil)
		if err == nil {
			t.applyHeaders(req, map[string]string{}, token)
			if response, err := t.client.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
				response.Body.Close()
			}
		}
		cancel()
	}
	return nil
}

// Receive returns the next message pushed by any response or GET stream.
func (t *HTTPTransport) Receive(ctx context.Context) (Message, error) {
	for {
		t.mu.Lock()
		if len(t.queue) > 0 {
			message := t.queue[0]
			t.queue = t.queue[1:]
			t.mu.Unlock()
			return message, nil
		}
		terminal := t.terminal
		t.mu.Unlock()
		if terminal != nil {
			return nil, terminal
		}
		select {
		case <-t.wake:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.closedCh:
		}
	}
}

func (t *HTTPTransport) push(message Message) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.queue = append(t.queue, message)
	t.mu.Unlock()
	t.signalWake()
}

func (t *HTTPTransport) signalWake() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (t *HTTPTransport) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// authorizedFetch sends a request with the current token. A 401, or a 403 whose
// challenge reports insufficient_scope, is handed to the auth provider once and
// the request is retried.
func (t *HTTPTransport) authorizedFetch(baseCtx, requestCtx context.Context, method string, headers map[string]string, body []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		var token string
		if provider := t.options.AuthProvider; provider != nil {
			value, err := provider.Token(requestCtx)
			if err != nil {
				return nil, err
			}
			token = value
		}
		t.mu.Lock()
		t.lastToken = token
		t.mu.Unlock()
		req, err := http.NewRequestWithContext(baseCtx, method, t.url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		t.applyHeaders(req, headers, token)
		if requestCtx.Err() != nil {
			return nil, &AbortError{Reason: requestCtx.Err().Error()}
		}
		response, err := t.client.Do(req)
		if err != nil {
			return nil, err
		}
		provider := t.options.AuthProvider
		if attempt > 0 || provider == nil || !needsAuthorization(response) {
			return response, nil
		}
		excerpt, _, _ := readLimited(response.Body, maxHTTPErrorBodyBytes)
		response.Body.Close()
		event := UnauthorizedEvent{
			Status:          response.StatusCode,
			WWWAuthenticate: response.Header.Get("www-authenticate"),
			Body:            string(excerpt),
			ServerURL:       t.url,
			Token:           token,
			Client:          t.client,
		}
		if err := provider.OnUnauthorized(requestCtx, event); err != nil {
			return nil, err
		}
	}
}

func (t *HTTPTransport) applyHeaders(req *http.Request, extra map[string]string, token string) {
	for name, value := range t.options.Headers {
		req.Header.Set(name, value)
	}
	for name, value := range extra {
		req.Header.Set(name, value)
	}
	t.mu.Lock()
	sessionID, protocol := t.sessionID, t.protocolVersion
	t.mu.Unlock()
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	if protocol != "" {
		req.Header.Set("MCP-Protocol-Version", protocol)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func (t *HTTPTransport) responseError(response *http.Response, body string) error {
	if response.StatusCode == http.StatusUnauthorized {
		return &AuthRequiredError{
			HTTPError:       HTTPError{Status: response.StatusCode, Body: body},
			WWWAuthenticate: response.Header.Get("www-authenticate"),
		}
	}
	t.mu.Lock()
	hasSession := t.sessionID != ""
	t.mu.Unlock()
	if response.StatusCode == http.StatusNotFound && hasSession {
		return &SessionExpiredError{HTTPError: HTTPError{Status: response.StatusCode, Body: body}}
	}
	return &HTTPError{Status: response.StatusCode, Body: body}
}

// consumeResponseStream reads the SSE stream answering one request. When it ends
// before that response arrived and the server assigned event IDs, it resumes the
// stream with GET and Last-Event-ID.
func (t *HTTPTransport) consumeResponseStream(body io.Reader, requestID ID, maxBytes int) {
	cursor := &streamCursor{}
	answered := false
	stream := body
	var failure error
	for attempt := 0; ; {
		if stream != nil {
			failure = ConsumeSSEStream(stream, SSEOptions{
				MaxEventBytes: maxBytes,
				OnID:          func(id string) { cursor.lastEventID = id },
				OnRetry:       func(delay time.Duration) { cursor.retry = delay },
				OnEvent: func(event SSEEvent) {
					cursor.received = true
					if strings.TrimSpace(event.Data) == "" || (event.Event != "" && event.Event != "message") {
						return
					}
					message, err := parseSSEMessage(event.Data)
					if err != nil {
						return
					}
					if response, ok := message.(*SuccessResponse); ok && response.ID.Key() == requestID.Key() {
						answered = true
					}
					if response, ok := message.(*ErrorResponse); ok && response.ID.Key() == requestID.Key() {
						answered = true
					}
					t.push(message)
				},
			})
		}
		if answered || t.isClosed() {
			return
		}
		if failure != nil && !isRetryable(failure) {
			break
		}
		if cursor.lastEventID == "" || attempt >= t.maxRetries() {
			break
		}
		if cursor.received {
			attempt = 0
		}
		cursor.received = false
		if !t.sleep(t.retryDelay(attempt, cursor.retry)) {
			return
		}
		attempt++
		opened, err := t.openSSEStream(cursor.lastEventID)
		if err != nil {
			failure = err
			if !isRetryable(err) {
				break
			}
			stream = nil
			continue
		}
		stream = opened
	}
	if t.isClosed() {
		return
	}
	reason := "stream ended without a response"
	if failure != nil {
		reason = failure.Error()
	}
	t.push(&ErrorResponse{
		JSONRPC: JSONRPCVersion,
		ID:      requestID,
		Error: ErrorObject{
			Code:    CodeInternalError,
			Message: "MCP response stream failed: " + reason,
		},
	})
}

type streamCursor struct {
	lastEventID string
	retry       time.Duration
	received    bool
}

func (t *HTTPTransport) startGetStream() {
	t.mu.Lock()
	if t.options.DisableGetStream || t.getStreamStarted || t.closed {
		t.mu.Unlock()
		return
	}
	t.getStreamStarted = true
	t.mu.Unlock()
	go t.runGetStream()
}

// runGetStream keeps the server-to-client stream open, reconnecting with
// backoff when it drops.
func (t *HTTPTransport) runGetStream() {
	cursor := &streamCursor{}
	for attempt := 0; !t.isClosed(); {
		openedAt := time.Now()
		stream, err := t.openSSEStream(cursor.lastEventID)
		if err != nil {
			if t.isClosed() {
				return
			}
			if !isRetryable(err) {
				return
			}
		} else if stream == nil {
			return // the server offers no GET stream (405)
		} else {
			err = ConsumeSSEStream(stream, SSEOptions{
				MaxEventBytes: t.maxBytes,
				OnID:          func(id string) { cursor.lastEventID = id },
				OnRetry:       func(delay time.Duration) { cursor.retry = delay },
				OnEvent: func(event SSEEvent) {
					cursor.received = true
					if strings.TrimSpace(event.Data) == "" || (event.Event != "" && event.Event != "message") {
						return
					}
					message, err := parseSSEMessage(event.Data)
					if err != nil {
						return
					}
					t.push(message)
				},
			})
			stream.Close()
			if err == nil && (cursor.received || time.Since(openedAt) > t.maxDelay()) {
				attempt = 0
			}
		}
		cursor.received = false
		if attempt >= t.maxRetries() {
			return
		}
		if !t.sleep(t.retryDelay(attempt, cursor.retry)) {
			return
		}
		attempt++
	}
}

// openSSEStream opens a GET SSE stream. It returns (nil, nil) for a 405.
func (t *HTTPTransport) openSSEStream(lastEventID string) (io.ReadCloser, error) {
	headers := map[string]string{"accept": "text/event-stream"}
	if lastEventID != "" {
		headers["last-event-id"] = lastEventID
	}
	response, err := t.authorizedFetch(t.baseCtx, t.baseCtx, http.MethodGet, headers, nil)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusMethodNotAllowed {
		response.Body.Close()
		return nil, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body := bodyExcerpt(response)
		response.Body.Close()
		return nil, t.responseError(response, body)
	}
	t.mu.Lock()
	if sid := response.Header.Get("mcp-session-id"); sid != "" {
		t.sessionID = sid
	}
	t.mu.Unlock()
	if contentType(response) != "text/event-stream" {
		response.Body.Close()
		return nil, &HTTPError{Status: response.StatusCode, Body: fmt.Sprintf("Unsupported MCP GET response content type: %s", orMissing(contentType(response)))}
	}
	return response.Body, nil
}

func (t *HTTPTransport) retryDelay(attempt int, serverDelay time.Duration) time.Duration {
	if serverDelay > 0 {
		return serverDelay
	}
	initial := t.options.Reconnect.InitialDelay
	if initial <= 0 {
		initial = defaultReconnectDelay
	}
	delay := float64(initial) * math.Pow(2, float64(attempt))
	if max := t.maxDelay(); delay > float64(max) {
		return max
	}
	return time.Duration(delay)
}

func (t *HTTPTransport) maxDelay() time.Duration {
	if t.options.Reconnect.MaxDelay > 0 {
		return t.options.Reconnect.MaxDelay
	}
	return defaultReconnectMax
}

func (t *HTTPTransport) maxRetries() int {
	if t.options.Reconnect.MaxRetries > 0 {
		return t.options.Reconnect.MaxRetries
	}
	return defaultReconnectTries
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

func parseSSEMessage(data string) (Message, error) {
	return ParseMessage([]byte(data))
}

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

func needsAuthorization(response *http.Response) bool {
	if response.StatusCode == http.StatusUnauthorized {
		return true
	}
	if response.StatusCode != http.StatusForbidden {
		return false
	}
	challenge := response.Header.Get("www-authenticate")
	return strings.Contains(strings.ToLower(challenge), `error="insufficient_scope"`) ||
		strings.Contains(strings.ToLower(challenge), "error=insufficient_scope")
}

func isRetryable(err error) bool {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Status == http.StatusRequestTimeout || httpErr.Status == http.StatusTooManyRequests || httpErr.Status >= 500
	}
	// Network failures (including a connection dropped mid-body) are retryable.
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

// readLimited reads at most max bytes and reports whether the body was longer.
func readLimited(reader io.Reader, max int) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(reader, int64(max)+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > max {
		return data[:max], true, nil
	}
	return data, false, nil
}

// bodyExcerpt keeps only the excerpt an error message reports and discards the
// rest, so a huge error body is never buffered.
func bodyExcerpt(response *http.Response) string {
	data, _, _ := readLimited(response.Body, maxHTTPErrorBodyBytes)
	return string(data)
}
