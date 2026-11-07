// AgentBackend: the caller-side surface of an external agent runtime.
//
// This is the interface a DSH sidecar is reached through, and it is this
// protocol's own versioned method set - initialize, session.open,
// session.resume, message.send, event.stream, cancel, health, close - carried
// over the MCP-style transport and framing. It is deliberately NOT expressed
// as MCP methods: an external agent runtime is not an MCP server, and the
// protocol must not promise MCP guarantees (resources, prompts, sampling) that
// nobody implements.
//
// # External sessions
//
// A session of an AgentBackend is an *external session*: the backend owns its
// state. The gateway reports the marks Keel persists:
//
//	state_owner=external, external_backend=<name>, external_ref=<backend session id>,
//	external_status=<opening|active|backend-unavailable|closed>, metering=<external|none>
//
// External sessions MUST NOT enter Keel's native run/lease flow (a message to
// the backend is not a run; Keel records what it chooses to record through the
// contract kernel, and Keel's SSE only replays Keel-recorded events - the
// gateway never injects backend events into it). A backend that becomes
// unavailable is a queryable terminal state: Status reports
// backend-unavailable and Health reports the same, and no native recovery is
// claimed for those sessions.
//
// # Usage
//
// The gateway never synthesizes a usage record. A session is marked
// metering=external only when the backend declares it reports token usage (or
// says so explicitly in session.open), and metering=none otherwise; absence of
// usage is never recorded as zero usage. Which quota policy applies to an
// external provider is a loadout/tenant decision, not a gateway one.

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// External session statuses (the external_status mark).
const (
	// ExternalStatusOpening means the session is being opened or resumed.
	ExternalStatusOpening = "opening"
	// ExternalStatusActive means the backend is serving the session.
	ExternalStatusActive = "active"
	// ExternalStatusUnavailable means the backend (or its connection) is gone.
	// It is a queryable terminal state, not a transient error.
	ExternalStatusUnavailable = "backend-unavailable"
	// ExternalStatusClosed means the caller closed the session.
	ExternalStatusClosed = "closed"
)

// Backend health statuses reported by the health method.
const (
	BackendStatusOK          = "ok"
	BackendStatusDegraded    = "degraded"
	BackendStatusUnavailable = "unavailable"
)

// Usage is the token usage a backend reports for one session or turn. A Usage
// value only ever comes from the backend; the gateway never fabricates one,
// and a missing value means "not measurable" (metering=none), never zero.
type Usage struct {
	// Metering is the usage mark: external (backend-accounted) or none.
	Metering Metering `json:"metering"`
	Model    string   `json:"model,omitempty"`
	// InputTokens, OutputTokens and TotalTokens are zero when the backend does
	// not report them; they are omitted from the wire when zero.
	InputTokens  int64 `json:"inputTokens,omitempty"`
	OutputTokens int64 `json:"outputTokens,omitempty"`
	TotalTokens  int64 `json:"totalTokens,omitempty"`
}

// ExternalSession is a session of an external agent backend plus the marks
// Keel persists for it. SessionID is the backend's handle; it is opaque to
// Keel and is meaningless outside the backend that issued it.
type ExternalSession struct {
	SessionID string `json:"sessionId"`
	// Reference is the caller's opaque correlation token, echoed back.
	Reference string `json:"reference,omitempty"`
	// Backend is the backend name from initialize (external_backend).
	Backend string `json:"backend,omitempty"`
	// StateOwner is keel or external; for an AgentBackend it is external
	// unless the backend explicitly claims otherwise.
	StateOwner StateOwner `json:"stateOwner,omitempty"`
	// Status is the external_status mark.
	Status string `json:"status,omitempty"`
	// Metering is the session's usage mark.
	Metering Metering `json:"metering,omitempty"`
	// Recovery is the backend's declared recovery mode for this session.
	Recovery string `json:"recovery,omitempty"`
	// Resumed reports whether a session.resume actually resumed the session.
	Resumed bool `json:"resumed,omitempty"`
	// LastSeq is the backend's event cursor at open/resume time.
	LastSeq int64 `json:"lastSeq,omitempty"`
}

// CompatMode reports whether the session must be treated as an external
// (compatibility) session, i.e. whether Keel-native recovery must not be
// claimed for it.
func (s ExternalSession) CompatMode() bool { return s.StateOwner != StateOwnerKeel }

// Markers are the fields Keel persists when the session becomes visible in its
// own store (external-gateway spec: 外部会话数据模型).
func (s ExternalSession) Markers() map[string]string {
	owner := s.StateOwner
	if owner == "" {
		owner = StateOwnerExternal
	}
	status := s.Status
	if status == "" {
		status = ExternalStatusActive
	}
	metering := s.Metering
	if metering == "" {
		metering = MeteringNone
	}
	return map[string]string{
		"state_owner":      string(owner),
		"external_backend": s.Backend,
		"external_ref":     s.SessionID,
		"external_status":  status,
		"metering":         string(metering),
	}
}

// SessionOpenRequest opens a session. Reference is an opaque correlation token
// chosen by the caller; Config and Context carry whatever the backend needs to
// start (they are backend-defined JSON).
type SessionOpenRequest struct {
	Reference string          `json:"reference,omitempty"`
	Config    json.RawMessage `json:"config,omitempty"`
	Context   json.RawMessage `json:"context,omitempty"`
}

// SessionResumeRequest asks a backend to resume an external session it already
// knows, optionally from an event cursor.
type SessionResumeRequest struct {
	SessionID string `json:"sessionId"`
	FromSeq   int64  `json:"fromSeq,omitempty"`
	Reference string `json:"reference,omitempty"`
}

// MessageSendRequest hands one input message to a session.
type MessageSendRequest struct {
	SessionID string          `json:"sessionId"`
	Message   json.RawMessage `json:"message,omitempty"`
}

// MessageSendResult acknowledges a send.
type MessageSendResult struct {
	// TurnID identifies the turn the message started, when the backend has one.
	TurnID string `json:"turnId,omitempty"`
}

// EventStreamRequest subscribes to a session's event stream. Sending it is the
// "event.stream" request; every event then arrives as an "event.stream"
// notification (see README.md).
type EventStreamRequest struct {
	SessionID string `json:"sessionId"`
	// FromSeq asks the backend to replay from an event cursor (0 = live only,
	// backend-defined).
	FromSeq int64 `json:"fromSeq,omitempty"`
}

// EventStreamResult acknowledges a subscription.
type EventStreamResult struct {
	StreamID string `json:"streamId,omitempty"`
	Cursor   int64  `json:"cursor,omitempty"`
}

// CancelRequest cancels a session's current turn.
type CancelRequest struct {
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// HealthResult is a backend's answer to health: its availability plus whatever
// version and detail it wants to expose.
type HealthResult struct {
	// Status is one of the BackendStatus constants.
	Status string `json:"status"`
	// Version is the backend's own version string, when it reports one.
	Version string `json:"version,omitempty"`
	// Detail is free-form diagnostic text (already safe to log: the gateway
	// never logs raw payloads, and backends must redact their own secrets).
	Detail string `json:"detail,omitempty"`
}

// CloseRequest closes a session, or requests a graceful backend shutdown when
// it carries no session id.
type CloseRequest struct {
	SessionID string `json:"sessionId,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// AgentEvent is one event pushed by a backend on event.stream. Type and Data
// are backend-defined; the gateway passes them through unmodified, in order.
type AgentEvent struct {
	SessionID string          `json:"sessionId"`
	TurnID    string          `json:"turnId,omitempty"`
	Sequence  int64           `json:"seq,omitempty"`
	Type      string          `json:"type,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	// Usage is present only when the backend reports token usage for this
	// event. Absent means not measurable, never zero.
	Usage *Usage `json:"usage,omitempty"`
}

// AgentBackend is the caller-side contract of an external agent runtime: the
// protocol's agent method set plus the local status query.
type AgentBackend interface {
	// Open opens a session and reports its marks.
	Open(ctx context.Context, request SessionOpenRequest) (ExternalSession, error)
	// Resume resumes an existing session, reporting whether it really resumed.
	Resume(ctx context.Context, request SessionResumeRequest) (ExternalSession, error)
	// Send hands one input message to a session.
	Send(ctx context.Context, request MessageSendRequest) (MessageSendResult, error)
	// EventStream subscribes to a session's event stream; events are then
	// delivered on Events().
	EventStream(ctx context.Context, request EventStreamRequest) (EventStreamResult, error)
	// Cancel cancels a session's current turn.
	Cancel(ctx context.Context, request CancelRequest) error
	// Health probes the backend.
	Health(ctx context.Context) (HealthResult, error)
	// Close closes one session, or asks the backend to shut down when the
	// request carries no session id.
	Close(ctx context.Context, request CloseRequest) error
	// Events is the ordered event stream of every session on this connection.
	Events() <-chan AgentEvent
	// Status reports a session's external_status, including the terminal
	// backend-unavailable state after the backend or connection died.
	Status(sessionID string) string
}

// AgentClient implements AgentBackend over a Conn.
type AgentClient struct {
	conn *Conn

	mu       sync.Mutex
	sessions map[string]ExternalSession
}

// NewAgentBackend returns the agent surface of a connection. Every call checks
// that the connection really is an agent_backend, so a wiring mistake fails
// fast instead of sending agent methods to a tool provider.
func NewAgentBackend(conn *Conn) *AgentClient {
	return &AgentClient{conn: conn, sessions: make(map[string]ExternalSession)}
}

var _ AgentBackend = (*AgentClient)(nil)

// Open opens a session.
func (a *AgentClient) Open(ctx context.Context, request SessionOpenRequest) (ExternalSession, error) {
	if err := a.requireKind("Open"); err != nil {
		return ExternalSession{}, err
	}
	params, err := json.Marshal(request)
	if err != nil {
		return ExternalSession{}, fmt.Errorf("gateway: encode session.open params: %w", err)
	}
	raw, err := a.conn.doRequest(ctx, MethodSessionOpen, params, requestOptions{})
	if err != nil {
		return ExternalSession{}, err
	}
	session, err := a.normalizeSession(raw)
	if err != nil {
		return ExternalSession{}, err
	}
	if session.Status == "" {
		session.Status = ExternalStatusActive
	}
	session.Reference = firstNonEmpty(session.Reference, request.Reference)
	a.remember(session)
	return session, nil
}

// Resume resumes an existing session. A backend that answers resumed=false
// yields the terminal backend-unavailable status: the session is not gone from
// Keel's records, it is only unreachable.
func (a *AgentClient) Resume(ctx context.Context, request SessionResumeRequest) (ExternalSession, error) {
	if err := a.requireKind("Resume"); err != nil {
		return ExternalSession{}, err
	}
	if request.SessionID == "" {
		return ExternalSession{}, fmt.Errorf("gateway: session.resume requires a session id")
	}
	params, err := json.Marshal(request)
	if err != nil {
		return ExternalSession{}, fmt.Errorf("gateway: encode session.resume params: %w", err)
	}
	raw, err := a.conn.doRequest(ctx, MethodSessionResume, params, requestOptions{target: request.SessionID})
	if err != nil {
		return ExternalSession{}, err
	}
	session, err := a.normalizeSession(raw)
	if err != nil {
		return ExternalSession{}, err
	}
	if session.SessionID == "" {
		session.SessionID = request.SessionID
	}
	if session.Status == "" {
		if session.Resumed {
			session.Status = ExternalStatusActive
		} else {
			session.Status = ExternalStatusUnavailable
		}
	}
	session.Reference = firstNonEmpty(session.Reference, request.Reference)
	a.remember(session)
	return session, nil
}

// Send hands one input message to a session.
func (a *AgentClient) Send(ctx context.Context, request MessageSendRequest) (MessageSendResult, error) {
	if err := a.requireKind("Send"); err != nil {
		return MessageSendResult{}, err
	}
	if request.SessionID == "" {
		return MessageSendResult{}, fmt.Errorf("gateway: message.send requires a session id")
	}
	params, err := json.Marshal(request)
	if err != nil {
		return MessageSendResult{}, fmt.Errorf("gateway: encode message.send params: %w", err)
	}
	raw, err := a.conn.doRequest(ctx, MethodMessageSend, params, requestOptions{target: request.SessionID})
	if err != nil {
		return MessageSendResult{}, err
	}
	var result MessageSendResult
	if len(raw) > 0 && !isJSONNull(raw) {
		if err := json.Unmarshal(raw, &result); err != nil {
			return MessageSendResult{}, &ProtocolError{Code: CodeInvalidRequest, Message: "Invalid message.send result"}
		}
	}
	return result, nil
}

// EventStream subscribes to a session's events.
func (a *AgentClient) EventStream(ctx context.Context, request EventStreamRequest) (EventStreamResult, error) {
	if err := a.requireKind("EventStream"); err != nil {
		return EventStreamResult{}, err
	}
	if request.SessionID == "" {
		return EventStreamResult{}, fmt.Errorf("gateway: event.stream requires a session id")
	}
	params, err := json.Marshal(request)
	if err != nil {
		return EventStreamResult{}, fmt.Errorf("gateway: encode event.stream params: %w", err)
	}
	raw, err := a.conn.doRequest(ctx, MethodEventStream, params, requestOptions{target: request.SessionID})
	if err != nil {
		return EventStreamResult{}, err
	}
	var result EventStreamResult
	if len(raw) > 0 && !isJSONNull(raw) {
		if err := json.Unmarshal(raw, &result); err != nil {
			return EventStreamResult{}, &ProtocolError{Code: CodeInvalidRequest, Message: "Invalid event.stream result"}
		}
	}
	return result, nil
}

// Cancel cancels a session's current turn.
func (a *AgentClient) Cancel(ctx context.Context, request CancelRequest) error {
	if err := a.requireKind("Cancel"); err != nil {
		return err
	}
	if request.SessionID == "" {
		return fmt.Errorf("gateway: cancel requires a session id")
	}
	params, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("gateway: encode cancel params: %w", err)
	}
	_, err = a.conn.doRequest(ctx, MethodCancel, params, requestOptions{target: request.SessionID})
	return err
}

// Health probes the backend.
func (a *AgentClient) Health(ctx context.Context) (HealthResult, error) {
	if err := a.requireKind("Health"); err != nil {
		return HealthResult{}, err
	}
	raw, err := a.conn.doRequest(ctx, MethodHealth, nil, requestOptions{})
	if err != nil {
		return HealthResult{}, err
	}
	var result HealthResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return HealthResult{}, &ProtocolError{Code: CodeInvalidRequest, Message: "Invalid health result"}
	}
	switch result.Status {
	case BackendStatusOK, BackendStatusDegraded, BackendStatusUnavailable:
	default:
		return HealthResult{}, &ProtocolError{Code: CodeInvalidRequest, Message: "Invalid health status"}
	}
	return result, nil
}

// Close closes one session, or asks the backend to shut down when the request
// carries no session id.
func (a *AgentClient) Close(ctx context.Context, request CloseRequest) error {
	if err := a.requireKind("Close"); err != nil {
		return err
	}
	params, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("gateway: encode close params: %w", err)
	}
	target := request.SessionID
	_, err = a.conn.doRequest(ctx, MethodClose, params, requestOptions{target: target})
	if err != nil {
		return err
	}
	if request.SessionID != "" {
		a.mu.Lock()
		session, ok := a.sessions[request.SessionID]
		if ok {
			session.Status = ExternalStatusClosed
			a.sessions[request.SessionID] = session
		}
		a.mu.Unlock()
	}
	return nil
}

// Events is the ordered event stream of this connection.
func (a *AgentClient) Events() <-chan AgentEvent { return a.conn.Events() }

// Status reports a session's external_status. It is a queryable terminal
// state: an explicitly closed session stays closed; every other session of a
// backend whose connection died reports backend-unavailable. An unknown
// session reports "".
func (a *AgentClient) Status(sessionID string) string {
	a.mu.Lock()
	session, known := a.sessions[sessionID]
	a.mu.Unlock()
	if known && session.Status == ExternalStatusClosed {
		return ExternalStatusClosed
	}
	health := a.conn.Health().State
	if health == HealthUnhealthy || health == HealthClosed {
		return ExternalStatusUnavailable
	}
	if !known {
		return ""
	}
	return session.Status
}

// Sessions returns a snapshot of the sessions this client knows about.
func (a *AgentClient) Sessions() map[string]ExternalSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]ExternalSession, len(a.sessions))
	for id, session := range a.sessions {
		out[id] = session
	}
	return out
}

// remember records the marks of a session for Status queries.
func (a *AgentClient) remember(session ExternalSession) {
	if session.SessionID == "" {
		return
	}
	a.mu.Lock()
	a.sessions[session.SessionID] = session
	a.mu.Unlock()
}

// normalizeSession fills the marks a backend may leave implicit from the
// negotiated provider declaration and validates the rest.
func (a *AgentClient) normalizeSession(raw json.RawMessage) (ExternalSession, error) {
	var session ExternalSession
	if err := json.Unmarshal(raw, &session); err != nil {
		return ExternalSession{}, &ProtocolError{Code: CodeInvalidRequest, Message: "Invalid session result"}
	}
	if session.SessionID == "" {
		return ExternalSession{}, &ProtocolError{Code: CodeInvalidRequest, Message: "Invalid session result: missing sessionId"}
	}
	provider := a.conn.Provider()
	if session.Backend == "" {
		session.Backend = a.conn.ServerInfo().Name
	}
	if session.StateOwner == "" {
		session.StateOwner = provider.StateOwner
		if session.StateOwner == "" {
			session.StateOwner = StateOwnerExternal
		}
	}
	if !session.StateOwner.Valid() {
		return ExternalSession{}, &ProtocolError{Code: CodeInvalidRequest, Message: "Invalid session result: unknown stateOwner"}
	}
	if session.Metering == "" {
		session.Metering = provider.Metering
		if session.Metering == "" {
			session.Metering = MeteringNone
		}
	}
	if !session.Metering.Valid() {
		return ExternalSession{}, &ProtocolError{Code: CodeInvalidRequest, Message: "Invalid session result: unknown metering mark"}
	}
	if session.Recovery == "" {
		session.Recovery = provider.Recovery
	}
	return session, nil
}

func (a *AgentClient) requireKind(call string) error {
	got := a.conn.Provider().Type
	if got != ProviderTypeAgentBackend {
		return &ProviderTypeError{Call: call, Want: ProviderTypeAgentBackend, Got: got}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
