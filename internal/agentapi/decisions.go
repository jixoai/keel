package agentapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentext"
)

// Approval decisions: open (agent side or, for integration tests, over HTTP),
// list, read, answer. The decision id is the idempotency key of an answer; the
// opener's requestId is the idempotency key of the question itself.

func (s *Server) listDecisions(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	sessionID, ok := sessionFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.Decisions == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("decisions are not wired"))
		return
	}
	decisions, err := s.cfg.Decisions.List(r.Context(), tenantID, sessionID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"decisions": decisions})
}

// openDecisionRequest is the HTTP form of agentext.OpenDecisionRequest.
type openDecisionRequest struct {
	RequestID     string                    `json:"requestId"`
	Kind          string                    `json:"kind"`
	Prompt        string                    `json:"prompt"`
	Options       []agentext.DecisionOption `json:"options"`
	AllowFreeText bool                      `json:"allowFreeText"`
	TimeoutMs     int64                     `json:"timeoutMs"`
}

func (s *Server) openDecision(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	sessionID, ok := sessionFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.Decisions == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("decisions are not wired"))
		return
	}
	if s.cfg.SessionExists != nil {
		exists, err := s.cfg.SessionExists(r.Context(), tenantID, sessionID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if !exists {
			writeErr(w, http.StatusNotFound, ErrSessionNotFound)
			return
		}
	}
	var body openDecisionRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	decision, created, err := s.cfg.Decisions.Open(r.Context(), agentext.OpenDecisionRequest{
		TenantID:      tenantID,
		SessionID:     sessionID,
		RequestID:     body.RequestID,
		Kind:          body.Kind,
		Prompt:        body.Prompt,
		Options:       body.Options,
		AllowFreeText: body.AllowFreeText,
		Timeout:       time.Duration(body.TimeoutMs) * time.Millisecond,
	})
	if err != nil {
		if errors.Is(err, agentext.ErrInvalidDecision) {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"decision": decision, "created": created})
}

func (s *Server) getDecision(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	sessionID, ok := sessionFrom(w, r)
	if !ok {
		return
	}
	decisionID, ok := decisionFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.Decisions == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("decisions are not wired"))
		return
	}
	decision, found := s.cfg.Decisions.Get(r.Context(), tenantID, sessionID, decisionID)
	if !found {
		writeErr(w, http.StatusNotFound, errors.New("decision not found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"decision": decision})
}

// answerDecisionRequest is the answer body. Exactly one of optionId/text is
// normally supplied; a decision that allows free text may carry both.
type answerDecisionRequest struct {
	OptionID string `json:"optionId"`
	Text     string `json:"text"`
}

func (s *Server) answerDecision(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	sessionID, ok := sessionFrom(w, r)
	if !ok {
		return
	}
	decisionID, ok := decisionFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.Decisions == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("decisions are not wired"))
		return
	}
	var body answerDecisionRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	decision, duplicate, err := s.cfg.Decisions.Answer(r.Context(), agentext.AnswerDecisionRequest{
		TenantID:   tenantID,
		SessionID:  sessionID,
		DecisionID: decisionID,
		OptionID:   body.OptionID,
		Text:       body.Text,
	})
	if err != nil {
		switch {
		case errors.Is(err, agentext.ErrDecisionNotFound):
			writeErr(w, http.StatusNotFound, err)
		case errors.Is(err, agentext.ErrDecisionClosed),
			errors.Is(err, agentext.ErrDecisionConflict):
			writeErr(w, http.StatusConflict, err)
		case errors.Is(err, agentext.ErrInvalidDecision):
			writeErr(w, http.StatusBadRequest, err)
		default:
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"decision":  decision,
		"duplicate": duplicate,
		"status":    decision.Status,
	})
}

func decisionFrom(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	decisionID, err := uuid.Parse(r.PathValue("decisionId"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("decision id must be a UUID"))
		return uuid.Nil, false
	}
	return decisionID, true
}
