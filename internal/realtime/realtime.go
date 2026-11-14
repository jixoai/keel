// Package realtime bridges a browser WebSocket to a duplex realtime model
// provider (Qwen-Omni Realtime, OpenAI Realtime and compatible endpoints, plus
// the in-process demo provider enabled by KEEL_DEMO_PROVIDER).
//
// The package has two seams:
//
//   - RealtimeProvider opens the outbound leg. The built-in implementation is a
//     generic WebSocket provider configured from the KEEL_REALTIME_* environment
//     (provider.go); the demo provider (demo.go) answers from a local frame
//     queue. A plugin can implement the interface and be wired without
//     touching the bridge or the HTTP handler: Connect returns a Conn, and the
//     bridge only ever calls Read, Write and Close on it.
//   - Proxy (bridge.go) pumps opaque messages in both directions. It never
//     inspects audio: payloads are forwarded byte for byte, and the only thing
//     derived from a message is its top-level "type", for logging metadata.
//
// Realtime audio and transcripts travel inside provider JSON (base64 deltas),
// so the bridge treats every payload as data and logs only the event type and
// size.
package realtime

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Session identifies the kernel session a realtime conversation belongs to. The
// bridge hands it to the provider seam so an in-process provider can scope its
// state; the built-in WebSocket provider deliberately does not send tenant or
// session identifiers to the third-party endpoint.
type Session struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
}

// MessageType is the kind of a WebSocket message.
type MessageType uint8

const (
	// MessageText is a UTF-8 text message (the realtime event stream).
	MessageText MessageType = iota + 1
	// MessageBinary is a binary message. Provider events are JSON text today;
	// the binary case exists so a future provider that ships raw frames is not
	// forced through an artificial encoding.
	MessageBinary
)

// Message is one WebSocket message. Data is owned by the bridge for the
// duration of a pump iteration and is never mutated.
type Message struct {
	Type MessageType
	Data []byte
}

// Conn is one leg of a realtime bridge: the browser socket or the provider
// socket. Read must unblock when ctx is cancelled or the peer closes; Close
// must unblock a pending Read.
type Conn interface {
	// Read returns the next message from the peer.
	Read(ctx context.Context) (Message, error)
	// Write sends one message to the peer.
	Write(ctx context.Context, msg Message) error
	// Close closes the underlying transport with a WebSocket close code. It is
	// idempotent and returns nil when the connection was already closed.
	Close(code int, reason string) error
}

// RealtimeProvider is the extension seam for realtime backends. Connect opens
// one provider-side conversation for the session and returns the transport the
// bridge pumps. The context bounds the connect operation only: the returned
// Conn must stay usable after Connect returns and must not depend on ctx. The
// built-in WebSocket provider reads its URL, key and model allowlist from the
// environment; a future plugin can implement this interface (WebRTC, a local
// model, a vendor SDK) without editing the bridge or the HTTP handler.
type RealtimeProvider interface {
	Connect(ctx context.Context, session Session, model string) (Conn, error)
}

// Provider is a shorter alias for RealtimeProvider.
type Provider = RealtimeProvider

// CloseError is a peer-initiated WebSocket close. It carries the code and
// reason so the bridge can propagate them, but Error deliberately omits the
// reason: close reasons are peer-controlled and must never become a log field
// that could carry audio or transcript text.
type CloseError struct {
	Code   int
	Reason string
}

// Error implements error.
func (e *CloseError) Error() string {
	return fmt.Sprintf("realtime: peer closed the connection (code %d)", e.Code)
}

// SessionDefaulter is an optional provider capability: the session.update
// payload the bridge injects when the client never sends one. A provider that
// does not implement it gets the conservative OpenAI-compatible default.
type SessionDefaulter interface {
	DefaultSessionUpdate() []byte
}

// ProviderNamer is an optional provider capability used for lifecycle logs.
type ProviderNamer interface {
	ProviderName() string
}

// ModelLister is an optional provider capability: the exact set of models the
// provider will serve. A provider configured from a URL plus an environment
// allowlist does not implement it (its policy lives in the environment, and
// internal/httpapi mirrors that parsing); an in-process provider that has no
// environment at all reports its own set, so the status endpoint cannot drift.
type ModelLister interface {
	RealtimeModels() []string
}

// RealtimeModels returns the provider's own model list when it declares one.
// ok is false when the provider has no such capability, in which case the
// caller falls back to its environment-derived allowlist.
func RealtimeModels(p RealtimeProvider) (models []string, ok bool) {
	lister, ok := p.(ModelLister)
	if !ok {
		return nil, false
	}
	models = lister.RealtimeModels()
	if models == nil {
		models = []string{}
	}
	return models, true
}

// SessionUpdateDefaults returns the provider's default session.update body, or
// a conservative OpenAI-compatible default. The result is a copy: the bridge
// only reads it, but callers must not share mutable buffers.
func SessionUpdateDefaults(p RealtimeProvider) []byte {
	if d, ok := p.(SessionDefaulter); ok {
		if body := d.DefaultSessionUpdate(); len(body) > 0 {
			return append([]byte(nil), body...)
		}
	}
	return append([]byte(nil), genericDefaultSessionUpdate...)
}

// ProviderName reports a provider name for logs. It never includes credentials
// or URLs.
func ProviderName(p RealtimeProvider) string {
	if n, ok := p.(ProviderNamer); ok {
		if name := n.ProviderName(); name != "" {
			return name
		}
	}
	return "unknown"
}
