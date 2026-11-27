package harnessapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/superagent"
)

var errSuperagentUnwired = errors.New("the super-agent board is not wired in this deployment")

// SuperagentService is the production surface the orchestrator exposes to its
// own workers (report/ask over HTTP) and to the board command bridge (answer,
// resume, stop). The old board's read projections (tickets/attention) and the
// operator pending recovery existed only for the retired super-agent board;
// they are gone. The supervisor itself remains the single write seam.
type SuperagentService interface {
	Report(ctx context.Context, request superagent.ReportRequest) error
	Ask(ctx context.Context, ticketID, assignmentID uuid.UUID, request collab.AttentionRequest) (collab.Attention, error)
	AnswerAttention(ctx context.Context, tenantID, attentionID uuid.UUID, optionID, text, actor string) (collab.Attention, error)
	ResumeTicket(ctx context.Context, tenantID, ticketID uuid.UUID, actor, reason string) (superagent.Ticket, error)
	StopTicket(ctx context.Context, tenantID, ticketID uuid.UUID, actor, reason string) error
}

// superagentRoutes mounts only the worker report/ask writes. The retired
// board's GET /v1/superagent/tickets and /attention, its operator answer/
// resume/stop writes, and the pending retry/dismiss routes are intentionally
// absent: a request to them is a 404 from the mux.
func (s *Server) superagentRoutes() {
	s.mux.HandleFunc("POST /v1/superagent/tickets/{id}/report", s.superagentReport)
	s.mux.HandleFunc("POST /v1/superagent/tickets/{id}/ask", s.superagentAsk)
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
	if principal.BotID != "" {
		// The product API is the human/operator surface. A Bot principal (a
		// worker Bot key or any delegation) must never reach it even when its
		// harness narrowing happens to include task.write; the model tools are
		// its only write seam.
		writeErr(w, http.StatusForbidden, errors.New("superagent: the operator API is not available to Bot principals"))
		return uuid.Nil, access.Principal{}, false
	}
	return tenantID, principal, true
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

func superagentTicketID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return uuid.Nil, false
	}
	return id, true
}

func writeSuperagentError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, superagent.ErrOperatorUnavailable), errors.Is(err, collab.ErrPermissionDenied):
		writeErr(w, http.StatusForbidden, err)
	case errors.Is(err, collab.ErrWorkItemNotFound), errors.Is(err, collab.ErrAssignmentNotFound), errors.Is(err, collab.ErrAttentionNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, collab.ErrWorkItemClosed), errors.Is(err, collab.ErrAttentionConflict), errors.Is(err, collab.ErrAttentionClosed), errors.Is(err, collab.ErrAttentionExpired):
		writeErr(w, http.StatusConflict, err)
	case errors.Is(err, collab.ErrAuditUnavailable), errors.Is(err, collab.ErrAttentionPublish),
		errors.Is(err, superagent.ErrWriterLeaseLost):
		writeErr(w, http.StatusServiceUnavailable, err)
	default:
		writeErr(w, http.StatusBadRequest, err)
	}
}
