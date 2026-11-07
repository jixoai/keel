// External-session HTTP surface: the runtime's door to AgentBackend sessions
// (DSH and any other external agent runtime).
//
// The endpoints are deliberately tenant-facing like the native session API
// (X-Tenant-ID), and they are the only way to open/query/send/cancel/close an
// external session. They never start a native run or a lease: the contract
// kernel's external lane (kernel.ExternalSessions) records the marks, and the
// ordinary event endpoints replay the external.event records the gateway
// commits. A native session is not addressable here.
//
// Endpoints:
//
//	POST /v1/external/sessions                     open (201)
//	GET  /v1/external/sessions                     list the tenant's sessions
//	GET  /v1/external/sessions/{id}                query one session's marks
//	POST /v1/external/sessions/{id}/messages       send one message (202)
//	POST /v1/external/sessions/{id}/cancel         cancel the current turn
//	POST /v1/external/sessions/{id}/close          close the session
//	GET  /v1/external/backends                     provider availability
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/externalgw"
	"github.com/jixoai/keel/internal/gateway"
	"github.com/jixoai/keel/internal/kernel"
)

// ExternalSessions is the external-gateway surface the HTTP edge needs.
// *externalgw.AgentService is the production implementation; a nil value makes
// every external route answer 503.
type ExternalSessions interface {
	Open(ctx context.Context, scope kernel.Scope, backend string, request gateway.SessionOpenRequest) (externalgw.SessionView, error)
	Get(ctx context.Context, scope kernel.Scope) (externalgw.SessionView, error)
	List(ctx context.Context, tenantID uuid.UUID) ([]externalgw.SessionView, error)
	Send(ctx context.Context, scope kernel.Scope, message json.RawMessage, requestID string) (externalgw.SendResult, error)
	Cancel(ctx context.Context, scope kernel.Scope, request externalgw.CancelRequest) error
	Close(ctx context.Context, scope kernel.Scope, reason string) error
	Backends() []externalgw.BackendStatus
}

// Default external request bounds. A client may ask for a longer one up to the
// cap; the caps keep one wedged provider from holding an HTTP connection open
// indefinitely.
const (
	defaultExternalOpenTimeout    = 30 * time.Second
	maxExternalOpenTimeout        = 2 * time.Minute
	defaultExternalSendTimeout    = 60 * time.Second
	maxExternalSendTimeout        = 5 * time.Minute
	defaultExternalControlTimeout = 10 * time.Second
	maxExternalControlTimeout     = time.Minute
)

func (s *Server) registerExternalRoutes() {
	s.mux.HandleFunc("POST /v1/external/sessions", s.externalOpen)
	s.mux.HandleFunc("GET /v1/external/sessions", s.externalList)
	s.mux.HandleFunc("GET /v1/external/sessions/{id}", s.externalGet)
	s.mux.HandleFunc("POST /v1/external/sessions/{id}/messages", s.externalSend)
	s.mux.HandleFunc("POST /v1/external/sessions/{id}/cancel", s.externalCancel)
	s.mux.HandleFunc("POST /v1/external/sessions/{id}/close", s.externalClose)
	s.mux.HandleFunc("GET /v1/external/backends", s.externalBackends)
}

// externalService returns the configured service or writes 503.
func (s *Server) externalService(w http.ResponseWriter) (ExternalSessions, bool) {
	if s.cfg.External == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("external gateway is not configured"))
		return nil, false
	}
	return s.cfg.External, true
}

// externalScope parses the tenant header and the session id path value.
func (s *Server) externalScope(w http.ResponseWriter, r *http.Request) (kernel.Scope, bool) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("X-Tenant-ID is required"))
		return kernel.Scope{}, false
	}
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("bad session id"))
		return kernel.Scope{}, false
	}
	return kernel.Scope{TenantID: tenantID, SessionID: sessionID}, true
}

func (s *Server) externalOpen(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.externalService(w)
	if !ok {
		return
	}
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("X-Tenant-ID is required"))
		return
	}
	var body struct {
		SessionID string          `json:"sessionId"`
		Backend   string          `json:"backend"`
		Reference string          `json:"reference"`
		Config    json.RawMessage `json:"config"`
		Context   json.RawMessage `json:"context"`
		TimeoutMS int64           `json:"timeoutMs"`
	}
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if body.Backend == "" {
		writeErr(w, http.StatusBadRequest, errors.New("backend is required"))
		return
	}
	sessionID := uuid.New()
	if body.SessionID != "" {
		parsed, err := uuid.Parse(body.SessionID)
		if err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("bad sessionId"))
			return
		}
		sessionID = parsed
	}
	ctx, cancel := context.WithTimeout(r.Context(), externalTimeout(body.TimeoutMS, defaultExternalOpenTimeout, maxExternalOpenTimeout))
	defer cancel()
	view, err := svc.Open(ctx, kernel.Scope{TenantID: tenantID, SessionID: sessionID}, body.Backend, gateway.SessionOpenRequest{
		Reference: body.Reference,
		Config:    body.Config,
		Context:   body.Context,
	})
	if err != nil {
		writeExternalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (s *Server) externalList(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.externalService(w)
	if !ok {
		return
	}
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("X-Tenant-ID is required"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), defaultExternalControlTimeout)
	defer cancel()
	sessions, err := svc.List(ctx, tenantID)
	if err != nil {
		writeExternalError(w, err)
		return
	}
	if sessions == nil {
		sessions = []externalgw.SessionView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (s *Server) externalGet(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.externalService(w)
	if !ok {
		return
	}
	scope, ok := s.externalScope(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), defaultExternalControlTimeout)
	defer cancel()
	view, err := svc.Get(ctx, scope)
	if err != nil {
		writeExternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) externalSend(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.externalService(w)
	if !ok {
		return
	}
	scope, ok := s.externalScope(w, r)
	if !ok {
		return
	}
	var body struct {
		Message   json.RawMessage `json:"message"`
		Text      string          `json:"text"`
		RequestID string          `json:"requestId"`
		TimeoutMS int64           `json:"timeoutMs"`
	}
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	message := body.Message
	if len(message) == 0 {
		if body.Text == "" {
			writeErr(w, http.StatusBadRequest, errors.New("message (raw JSON) or text is required"))
			return
		}
		encoded, err := json.Marshal(map[string]string{"text": body.Text})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		message = encoded
	}
	requestID := body.RequestID
	if requestID == "" {
		requestID = r.Header.Get("X-Request-ID")
	}
	ctx, cancel := context.WithTimeout(r.Context(), externalTimeout(body.TimeoutMS, defaultExternalSendTimeout, maxExternalSendTimeout))
	defer cancel()
	result, err := svc.Send(ctx, scope, message, requestID)
	if err != nil {
		writeExternalError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) externalCancel(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.externalService(w)
	if !ok {
		return
	}
	scope, ok := s.externalScope(w, r)
	if !ok {
		return
	}
	var body struct {
		TurnID    string `json:"turnId"`
		Reason    string `json:"reason"`
		TimeoutMS int64  `json:"timeoutMs"`
	}
	_ = decodeBody(w, r, s.cfg.MaxBody, &body)
	ctx, cancel := context.WithTimeout(r.Context(), externalTimeout(body.TimeoutMS, defaultExternalControlTimeout, maxExternalControlTimeout))
	defer cancel()
	if err := svc.Cancel(ctx, scope, externalgw.CancelRequest{TurnID: body.TurnID, Reason: body.Reason}); err != nil {
		writeExternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"canceled": true})
}

func (s *Server) externalClose(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.externalService(w)
	if !ok {
		return
	}
	scope, ok := s.externalScope(w, r)
	if !ok {
		return
	}
	var body struct {
		Reason    string `json:"reason"`
		TimeoutMS int64  `json:"timeoutMs"`
	}
	_ = decodeBody(w, r, s.cfg.MaxBody, &body)
	ctx, cancel := context.WithTimeout(r.Context(), externalTimeout(body.TimeoutMS, defaultExternalControlTimeout, maxExternalControlTimeout))
	defer cancel()
	if err := svc.Close(ctx, scope, body.Reason); err != nil {
		writeExternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"closed": true})
}

func (s *Server) externalBackends(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.externalService(w)
	if !ok {
		return
	}
	if _, ok := tenantFrom(r); !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("X-Tenant-ID is required"))
		return
	}
	backends := svc.Backends()
	if backends == nil {
		backends = []externalgw.BackendStatus{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"backends": backends})
}

// externalTimeout applies the default and the cap to a client-requested bound.
func externalTimeout(ms int64, def, max time.Duration) time.Duration {
	if ms <= 0 {
		return def
	}
	d := time.Duration(ms) * time.Millisecond
	if max > 0 && d > max {
		return max
	}
	return d
}

// writeExternalError maps the gateway/kernel failures onto HTTP statuses. The
// distinction matters to clients: 503 means "the backend is not there now,
// retry", 504 means "it did not answer in time", 409 means "this session state
// forbids the operation", 403 means "tenant policy", 429 means "session cap".
func writeExternalError(w http.ResponseWriter, err error) {
	var timeout *gateway.TimeoutError
	var canceled *gateway.CanceledError
	var unhealthy *gateway.UnhealthyError
	var rpc *gateway.RPCError
	var protocol *gateway.ProtocolError
	var transport *gateway.TransportError
	var httpStatus *gateway.HTTPStatusError
	var frame *gateway.FrameError
	switch {
	case errors.Is(err, kernel.ErrNotFound):
		writeErr(w, http.StatusNotFound, errors.New("external session not found"))
	case errors.Is(err, externalgw.ErrNotExternal):
		writeErr(w, http.StatusNotFound, errors.New("not an external session"))
	case errors.Is(err, externalgw.ErrBackendUnknown):
		writeErr(w, http.StatusBadRequest, err)
	case errors.Is(err, externalgw.ErrUnmeteredDenied):
		writeErr(w, http.StatusForbidden, err)
	case errors.Is(err, externalgw.ErrSessionLimit):
		writeErr(w, http.StatusTooManyRequests, err)
	case errors.Is(err, externalgw.ErrBackendUnavailable):
		writeErr(w, http.StatusServiceUnavailable, err)
	case errors.Is(err, externalgw.ErrAlreadyExternal),
		errors.Is(err, externalgw.ErrSessionClosed),
		errors.Is(err, externalgw.ErrSessionNotActive),
		errors.Is(err, kernel.ErrExternalConflict),
		errors.Is(err, kernel.ErrNotExternalSession):
		writeErr(w, http.StatusConflict, err)
	case errors.As(err, &timeout):
		writeErr(w, http.StatusGatewayTimeout, err)
	case errors.As(err, &canceled), errors.Is(err, context.Canceled):
		writeErr(w, http.StatusRequestTimeout, err)
	case errors.As(err, &unhealthy):
		writeErr(w, http.StatusServiceUnavailable, err)
	case errors.As(err, &rpc), errors.As(err, &protocol), errors.As(err, &transport),
		errors.As(err, &httpStatus), errors.As(err, &frame):
		writeErr(w, http.StatusBadGateway, err)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}
