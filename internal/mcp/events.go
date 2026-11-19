// MCP events (change feat-mcp-apps-events, P4). This file is the client half of
// the notification surface: the callbacks a host injects, the per-call progress
// token, and the modern `subscriptions/listen` stream.
//
// Nothing here is ledger aware: notifications are transport facts, never
// session facts. The host classifies them (internal/mcphost/events.go); the
// client only correlates them with the request that asked for them.
//
// Behaviour with no callbacks injected is unchanged from the pre-P4 client:
// notifications are dropped and server-initiated requests answer "method not
// found" (fail closed, see answerServerRequest).

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// ClientNotificationHandler receives one server notification on a Client. The
// manager adapts it to the server-aware NotificationHandler a host injects.
type ClientNotificationHandler func(ctx context.Context, method string, params json.RawMessage)

// NotificationHandler receives one server notification. server is the
// configured server name the manager connected to. It runs on the client's
// receive loop, so it must not block: a slow handler delays every message on
// the connection.
type NotificationHandler func(ctx context.Context, server, method string, params json.RawMessage)

// ClientServerRequestHandler answers a server-initiated request on a Client.
type ClientServerRequestHandler func(ctx context.Context, request *Request) (Message, error)

// ServerRequestHandler answers a server-initiated request (legacy era). A nil
// message with a nil error is treated as "not handled" and answered with
// -32601: a host must never leave a server request unanswered.
type ServerRequestHandler func(ctx context.Context, server string, request *Request) (Message, error)

// SubscriptionIDMetaKey is the `_meta` key every modern listen notification
// carries; its value is the JSON-RPC id of the `subscriptions/listen` request
// that owns the notification (2026-07-28 subscriptions pattern).
const SubscriptionIDMetaKey = "io.modelcontextprotocol/subscriptionId"

// responseStreamSourcePrefix marks transport metadata on a notification carried
// by a request's own response stream (as opposed to the connection stream).
const responseStreamSourcePrefix = "response:"

// responseStreamSource renders the Source of a frame read from id's response
// stream.
func responseStreamSource(id ID) string {
	return responseStreamSourcePrefix + id.Key()
}

// ProgressTokenMetaKey is the `_meta` key of a tools/call request carrying the
// progress token the server echoes in notifications/progress.
const ProgressTokenMetaKey = "progressToken"

// CallOptions carries the per-call facts a notification surface needs. The
// zero value keeps the pre-P4 wire shape (no `_meta` block).
type CallOptions struct {
	// ProgressToken, when non-empty, is sent as `_meta.progressToken`; the
	// server echoes it in notifications/progress so the host can correlate the
	// frame with this call. The client makes the token unique per connection
	// even when two callers ask for the same one.
	ProgressToken string
	// OnProgress receives this call's progress frames. It runs on the receive
	// loop and must not block; frames are advisory and may be dropped.
	OnProgress func(ProgressNotification)
	// AttemptID selects the catalogue snapshot of the calling attempt: a
	// tools/list_changed republish never removes a tool from under a running
	// attempt. Empty means the live catalogue.
	AttemptID string
}

// ListenFilter is the `params.notifications` object of subscriptions/listen:
// the subscription intent. The zero value opts into nothing.
type ListenFilter struct {
	ToolsListChanged      bool     `json:"toolsListChanged,omitempty"`
	PromptsListChanged    bool     `json:"promptsListChanged,omitempty"`
	ResourcesListChanged  bool     `json:"resourcesListChanged,omitempty"`
	ResourceSubscriptions []string `json:"resourceSubscriptions,omitempty"`
}

// ListenParams is the subscriptions/listen request params.
type ListenParams struct {
	Notifications ListenFilter `json:"notifications"`
}

// ListenAck is the parsed `notifications/subscriptions/acknowledged` frame.
type ListenAck struct {
	// Accepted is the server's actual subset. A category requested but absent
	// from Accepted is reported in Missing (a diagnostic, never silent).
	Accepted ListenFilter
	Missing  []string
	// SubscriptionID is the `_meta` tag the server attached (the listen
	// request's JSON-RPC id). Empty means the frame carried no tag.
	SubscriptionID string
}

// ListenEvent is one notification delivered on an established listen stream.
type ListenEvent struct {
	Method string
	Params json.RawMessage
}

// ListenOptions configures one subscriptions/listen.
type ListenOptions struct {
	// OnDiagnostic receives protocol-level notes (a frame before the ack, a
	// notification outside the accepted subset). It must not block.
	OnDiagnostic func(message string)
	// Buffer bounds the notification queue of the stream (default 64). A full
	// queue drops frames and reports one diagnostic: a directory change is
	// recovered by the next full recompute, and the wire is advisory.
	Buffer int
}

// ListenHandle is one established subscription stream. Close releases it and
// cancels the server-side subscription where the transport allows it.
type ListenHandle struct {
	Ack    ListenAck
	Events <-chan ListenEvent
	// Done closes when the stream ends; the received error is nil for a clean
	// end (the server's terminal response) and non-nil for a transport failure.
	Done <-chan error

	client    *Client
	id        ID
	events    chan ListenEvent
	done      chan error
	closeOnce sync.Once
}

// Close cancels the subscription. For stdio/memory transports it sends
// notifications/cancelled (the spec's cancellation form); for Streamable HTTP
// it closes the response stream, which is the HTTP form.
func (h *ListenHandle) Close() {
	if h == nil {
		return
	}
	h.closeOnce.Do(func() {
		if h.client == nil {
			return
		}
		h.client.removeListener(h.id)
		h.client.cancelListen(h.id)
	})
}

// listener is one open listen stream inside the client.
type listener struct {
	params  ListenParams
	options ListenOptions
	handle  *ListenHandle
	ackCh   chan struct{}

	mu       sync.Mutex
	acked    bool
	accepted ListenFilter
	ended    bool
}

func newListener(params ListenParams, options ListenOptions) *listener {
	buffer := options.Buffer
	if buffer <= 0 {
		buffer = 64
	}
	events := make(chan ListenEvent, buffer)
	done := make(chan error, 1)
	return &listener{
		params:  params,
		options: options,
		ackCh:   make(chan struct{}),
		handle:  &ListenHandle{Events: events, Done: done, events: events, done: done},
	}
}

// ack resolves the listener's acknowledgement. It fires once; later ack frames
// are duplicates and are diagnosed by the caller.
func (l *listener) ack(filter ListenFilter, subscriptionID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.acked {
		return false
	}
	l.acked = true
	l.accepted = filter
	l.handle.Ack = ListenAck{
		Accepted:       filter,
		Missing:        missingCategories(l.params.Notifications, filter),
		SubscriptionID: subscriptionID,
	}
	close(l.ackCh)
	return true
}

// isAcked reports whether the ack was seen.
func (l *listener) isAcked() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.acked
}

// filterFor returns the accepted filter once acked.
func (l *listener) filterFor() ListenFilter {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accepted
}

// deliver queues one notification. A full queue drops the frame and reports it;
// the host's recovery is the next full recompute, not replay.
func (l *listener) deliver(event ListenEvent, client *Client) {
	select {
	case l.handle.events <- event:
	default:
		client.diagnoseSampled(l.options.OnDiagnostic, "mcp listen: notification queue full, frame dropped")
	}
}

// end closes the stream once.
func (l *listener) end(err error, client *Client) {
	l.mu.Lock()
	if l.ended {
		l.mu.Unlock()
		return
	}
	l.ended = true
	l.mu.Unlock()
	client.diagnose(l.options.OnDiagnostic, "mcp listen: subscription closed")
	select {
	case l.handle.done <- err:
	default:
	}
}

// allowed reports whether a notification method belongs to the accepted filter.
// A resource update is allowed only for a URI the listener actually subscribed
// to; the modern spec delivers nothing else.
func (l *listener) allowed(method string, params json.RawMessage) bool {
	filter := l.filterFor()
	switch method {
	case "notifications/tools/list_changed":
		return filter.ToolsListChanged
	case "notifications/prompts/list_changed":
		return filter.PromptsListChanged
	case "notifications/resources/list_changed":
		return filter.ResourcesListChanged
	case "notifications/resources/updated":
		if len(filter.ResourceSubscriptions) == 0 {
			return false
		}
		uri := resourceUpdatedURI(params)
		if uri == "" {
			return false
		}
		for _, subscribed := range filter.ResourceSubscriptions {
			if subscribed == uri {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// missingCategories lists the requested categories the server did not accept.
func missingCategories(requested, accepted ListenFilter) []string {
	var missing []string
	if requested.ToolsListChanged && !accepted.ToolsListChanged {
		missing = append(missing, "toolsListChanged")
	}
	if requested.PromptsListChanged && !accepted.PromptsListChanged {
		missing = append(missing, "promptsListChanged")
	}
	if requested.ResourcesListChanged && !accepted.ResourcesListChanged {
		missing = append(missing, "resourcesListChanged")
	}
	return missing
}

// isDirectoryNotification reports whether method is a catalogue-changed frame
// (the notifications that belong to a listen subscription).
func isDirectoryNotification(method string) bool {
	switch method {
	case "notifications/tools/list_changed",
		"notifications/prompts/list_changed",
		"notifications/resources/list_changed",
		"notifications/resources/updated":
		return true
	default:
		return false
	}
}

// resourceUpdatedURI extracts the uri of notifications/resources/updated.
func resourceUpdatedURI(params json.RawMessage) string {
	var payload struct {
		URI string `json:"uri"`
	}
	if len(params) == 0 || json.Unmarshal(params, &payload) != nil {
		return ""
	}
	return payload.URI
}

// subscriptionIDOf returns the `_meta` subscription tag of a notification
// params object normalized through ID.Key (numbers 1 and 1.0 collide).
func subscriptionIDOf(params json.RawMessage) (string, bool) {
	if len(params) == 0 {
		return "", false
	}
	var envelope struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if json.Unmarshal(params, &envelope) != nil || envelope.Meta == nil {
		return "", false
	}
	raw, ok := envelope.Meta[SubscriptionIDMetaKey]
	if !ok {
		return "", false
	}
	var id ID
	if err := json.Unmarshal(raw, &id); err != nil {
		return "", false
	}
	return id.Key(), true
}

// ackFilterOf parses the acknowledged subset. The spec puts it under
// `notifications`; a server that answers with the filter at the top level is
// accepted too (the values are a closed set).
func ackFilterOf(params json.RawMessage) (ListenFilter, error) {
	if len(params) == 0 {
		return ListenFilter{}, nil
	}
	var envelope struct {
		Notifications *ListenFilter `json:"notifications"`
	}
	if err := json.Unmarshal(params, &envelope); err != nil {
		return ListenFilter{}, fmt.Errorf("mcp: invalid subscriptions/acknowledged params: %w", err)
	}
	if envelope.Notifications != nil {
		return *envelope.Notifications, nil
	}
	var direct ListenFilter
	if err := json.Unmarshal(params, &direct); err != nil {
		return ListenFilter{}, fmt.Errorf("mcp: invalid subscriptions/acknowledged params: %w", err)
	}
	return direct, nil
}
