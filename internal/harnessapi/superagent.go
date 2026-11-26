package harnessapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/superagent"
)

var errSuperagentUnwired = errors.New("the super-agent board is not wired in this deployment")

// SuperagentReader is the read-only projection of the super-agent queue.
type SuperagentReader interface {
	Board(ctx context.Context, tenantID uuid.UUID, filter superagent.BoardFilter) (superagent.BoardView, error)
	Inbox(ctx context.Context, tenantID uuid.UUID) (superagent.AttentionInbox, error)
}

// SuperagentService is the production write surface the orchestrator exposes to
// its own workers and to an authorized operator console. It is not a second
// collaboration engine: every mutation goes through the supervisor's guarded
// methods and the core Host.
type SuperagentService interface {
	SuperagentReader
	Report(ctx context.Context, request superagent.ReportRequest) error
	Ask(ctx context.Context, ticketID, assignmentID uuid.UUID, request collab.AttentionRequest) (collab.Attention, error)
	AnswerAttention(ctx context.Context, tenantID, attentionID uuid.UUID, optionID, text, actor string) (collab.Attention, error)
	ResumeTicket(ctx context.Context, tenantID, ticketID uuid.UUID, actor, reason string) (superagent.Ticket, error)
	StopTicket(ctx context.Context, tenantID, ticketID uuid.UUID, actor, reason string) error
}

// superagentRoutes mounts the read projections and the minimal write loop:
// worker report/ask, operator answer/resume/stop. Ticket creation remains a
// Source event, never an HTTP create route.
func (s *Server) superagentRoutes() {
	s.mux.HandleFunc("GET /v1/superagent/tickets", s.listSuperagentTickets)
	s.mux.HandleFunc("GET /v1/superagent/attention", s.listSuperagentAttention)
	s.mux.HandleFunc("POST /v1/superagent/tickets/{id}/report", s.superagentReport)
	s.mux.HandleFunc("POST /v1/superagent/tickets/{id}/ask", s.superagentAsk)
	s.mux.HandleFunc("POST /v1/superagent/attention/{id}/answer", s.superagentAnswer)
	s.mux.HandleFunc("POST /v1/superagent/tickets/{id}/resume", s.superagentResume)
	s.mux.HandleFunc("POST /v1/superagent/tickets/{id}/stop", s.superagentStop)
}

func (s *Server) superagentReady(w http.ResponseWriter, r *http.Request, key string) (uuid.UUID, access.Principal, bool) {
	if s.cfg.Superagent == nil || s.cfg.Access == nil {
		writeErr(w, http.StatusServiceUnavailable, errSuperagentUnwired)
		return uuid.Nil, access.Principal{}, false
	}
	principal, tenantID, ok := s.requireHeldKey(w, r, key)
	if !ok {
		return uuid.Nil, access.Principal{}, false
	}
	return tenantID, principal, true
}

func (s *Server) listSuperagentTickets(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := s.superagentReady(w, r, access.KeyTaskRead)
	if !ok {
		return
	}
	filter := superagent.BoardFilter{}
	if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
		filter.Status = collab.WorkItemStatus(raw)
	}
	if _, present := r.URL.Query()["limit"]; present {
		limit, err := queryInt(r, "limit", 100, 1, 500)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		filter.Limit = limit
	}
	board, err := s.cfg.Superagent.Board(r.Context(), tenantID, filter)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, board)
}

func (s *Server) listSuperagentAttention(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := s.superagentReady(w, r, access.KeyTaskRead)
	if !ok {
		return
	}
	inbox, err := s.cfg.Superagent.Inbox(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, inbox)
}

type superagentReportBody struct {
	AssignmentID uuid.UUID              `json:"assignmentId"`
	SessionID    uuid.UUID              `json:"sessionId"`
	State        superagent.ReportState `json:"state"`
	Summary      string                 `json:"summary,omitempty"`
	AttentionID  uuid.UUID              `json:"attentionId,omitempty"`
	Result       json.RawMessage        `json:"result,omitempty"`
	RequestID    string                 `json:"requestId,omitempty"`
}

func (s *Server) superagentReport(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := s.superagentReady(w, r, access.KeyTaskWrite)
	if !ok {
		return
	}
	ticketID, ok := superagentTicketID(w, r)
	if !ok {
		return
	}
	var body superagentReportBody
	if err := decodeBody(w, r, 1<<20, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if body.SessionID == uuid.Nil {
		writeErr(w, http.StatusBadRequest, errors.New("sessionId is required for a worker report"))
		return
	}
	err := s.cfg.Superagent.Report(r.Context(), superagent.ReportRequest{
		TenantID: tenantID, WorkItemID: ticketID, AssignmentID: body.AssignmentID,
		CallerSessionID: body.SessionID, State: body.State, Summary: body.Summary,
		AttentionID: body.AttentionID, Result: body.Result, RequestID: body.RequestID,
	})
	if err != nil {
		writeSuperagentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reported": true})
}

type superagentAskBody struct {
	AssignmentID  uuid.UUID                `json:"assignmentId"`
	SessionID     uuid.UUID                `json:"sessionId"`
	RequestID     string                   `json:"requestId,omitempty"`
	Kind          string                   `json:"kind,omitempty"`
	Prompt        string                   `json:"prompt"`
	Options       []collab.AttentionOption `json:"options,omitempty"`
	AllowFreeText bool                     `json:"allowFreeText"`
	Route         string                   `json:"route,omitempty"`
	Channel       string                   `json:"channel,omitempty"`
	TimeoutMs     int64                    `json:"timeoutMs,omitempty"`
}

func (s *Server) superagentAsk(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.superagentReady(w, r, access.KeyTaskWrite)
	if !ok {
		return
	}
	ticketID, ok := superagentTicketID(w, r)
	if !ok {
		return
	}
	var body superagentAskBody
	if err := decodeBody(w, r, 1<<20, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if body.SessionID == uuid.Nil {
		writeErr(w, http.StatusBadRequest, errors.New("sessionId is required for a worker attention"))
		return
	}
	attention, err := s.cfg.Superagent.Ask(r.Context(), ticketID, body.AssignmentID, collab.AttentionRequest{
		CallerSessionID: body.SessionID, RequestID: body.RequestID, Kind: body.Kind,
		Prompt: body.Prompt, Options: body.Options, AllowFreeText: body.AllowFreeText,
		Route: collab.AttentionRoute(body.Route), Channel: body.Channel,
		Timeout: time.Duration(body.TimeoutMs) * time.Millisecond,
	})
	if err != nil {
		writeSuperagentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"attention": attention})
}

type superagentAnswerBody struct {
	OptionID string `json:"optionId,omitempty"`
	Text     string `json:"text,omitempty"`
}

func (s *Server) superagentAnswer(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := s.superagentReady(w, r, access.KeyTaskWrite)
	if !ok {
		return
	}
	attentionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var body superagentAnswerBody
	if err := decodeBody(w, r, 1<<20, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	attention, err := s.cfg.Superagent.AnswerAttention(r.Context(), tenantID, attentionID, body.OptionID, body.Text, operatorActor(principal))
	if err != nil {
		writeSuperagentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"attention": attention})
}

type superagentOperatorBody struct {
	Reason string `json:"reason,omitempty"`
}

func (s *Server) superagentResume(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := s.superagentReady(w, r, access.KeyTaskWrite)
	if !ok {
		return
	}
	ticketID, ok := superagentTicketID(w, r)
	if !ok {
		return
	}
	var body superagentOperatorBody
	if err := decodeBody(w, r, 1<<20, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ticket, err := s.cfg.Superagent.ResumeTicket(r.Context(), tenantID, ticketID, operatorActor(principal), body.Reason)
	if err != nil {
		writeSuperagentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ticket": ticket})
}

func (s *Server) superagentStop(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := s.superagentReady(w, r, access.KeyTaskWrite)
	if !ok {
		return
	}
	ticketID, ok := superagentTicketID(w, r)
	if !ok {
		return
	}
	var body superagentOperatorBody
	if err := decodeBody(w, r, 1<<20, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.cfg.Superagent.StopTicket(r.Context(), tenantID, ticketID, operatorActor(principal), body.Reason); err != nil {
		writeSuperagentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stopped": true})
}

func superagentTicketID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return uuid.Nil, false
	}
	return id, true
}

func operatorActor(principal access.Principal) string {
	if principal.BotID != "" {
		if principal.AccountID != uuid.Nil {
			return principal.BotID + "/" + principal.AccountID.String()
		}
		return principal.BotID
	}
	if principal.AccountID != uuid.Nil {
		return principal.AccountID.String()
	}
	return "operator"
}

func writeSuperagentError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, superagent.ErrOperatorUnavailable), errors.Is(err, collab.ErrPermissionDenied):
		writeErr(w, http.StatusForbidden, err)
	case errors.Is(err, collab.ErrWorkItemNotFound), errors.Is(err, collab.ErrAssignmentNotFound), errors.Is(err, collab.ErrAttentionNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, collab.ErrWorkItemClosed), errors.Is(err, collab.ErrAttentionConflict), errors.Is(err, collab.ErrAttentionClosed), errors.Is(err, collab.ErrAttentionExpired):
		writeErr(w, http.StatusConflict, err)
	case errors.Is(err, collab.ErrAuditUnavailable), errors.Is(err, collab.ErrAttentionPublish):
		writeErr(w, http.StatusServiceUnavailable, err)
	default:
		writeErr(w, http.StatusBadRequest, err)
	}
}
