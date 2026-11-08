package httpapi

// Token-level partial bypass (keel-agent A5).
//
// A partial frame is NOT an event: it is never stored, never replayed and never
// carries an SSE id, so it cannot move a client's Last-Event-ID cursor. The
// runtime's SSE handler subscribes only when the client asks for
// `?partials=1`; the producer is an in-process hub (internal/agentext) and the
// committed event stream stays authoritative for everything a client resumes
// from.

import "github.com/google/uuid"

// PartialFrame is one transport frame of the bypass. Event names are stable
// strings (for example "partial"); Data is the JSON document.
type PartialFrame struct {
	Event string
	Data  []byte
}

// PartialSource is the subscription seam of the SSE handler. A session with no
// subscribers costs nothing: the source is consulted only for a request that
// asked for partials.
type PartialSource interface {
	// SubscribePartials returns a channel and a cancel function. The channel is
	// closed by the source when it stops; a slow consumer drops frames instead
	// of blocking the producer (the source owns the back-pressure policy).
	SubscribePartials(tenantID, sessionID uuid.UUID) (<-chan PartialFrame, func())
}
