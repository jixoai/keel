package agentapi

// Approval provenance routes and the answer hook (change keel-harness-v2,
// W2.5). The decision primitive already answers the question durably; this
// file records *who* answered it and *whether* the answer was a declared
// option, and exposes that audit next to the decision.
//
// The answer hook runs inside POST /v1/sessions/{id}/decisions/{decisionId}:
// the web client's existing path therefore records provenance without a second
// route or a second answer path. A provenance write failure returns 500 after
// the decision was answered; retrying the same answer is idempotent (the
// decision broker reports duplicate=true) and the retry writes the record, so
// no answer can stay unaudited without the client being told.

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/approval"
)

func (s *Server) approvalRoutes() {
	s.mux.HandleFunc("GET /v1/sessions/{id}/approvals", s.listApprovals)
	s.mux.HandleFunc("GET /v1/sessions/{id}/approvals/{decisionId}", s.getApproval)
}

// isApprovalDecision reports whether the decision is one the approve plugin
// asked. Other question kinds (clarify, plan) keep their existing behaviour.
func isApprovalDecision(decision agentext.Decision) bool {
	return strings.HasPrefix(decision.Kind, approval.KindPrefix)
}

// recordApprovalAnswer writes the provenance of an answered decision. It is
// called with the decision the broker just returned (the durable answer).
func (s *Server) recordApprovalAnswer(r *http.Request, decision agentext.Decision, actor, text string) (approval.Record, error) {
	optionIDs := make([]string, 0, len(decision.Options))
	for _, option := range decision.Options {
		optionIDs = append(optionIDs, option.ID)
	}
	optionID, answerText := "", text
	answeredAt := time.Now().UTC()
	if decision.Answer != nil {
		optionID = decision.Answer.OptionID
		answerText = decision.Answer.Text
		answeredAt = decision.Answer.At
	}
	if strings.TrimSpace(actor) == "" {
		actor = approval.UnknownActor
	}
	mode := approval.Mode(decision.Kind)
	validated, escape := approval.Compute(mode, optionIDs, optionID, answerText)
	record := approval.Record{
		TenantID:    decision.TenantID,
		SessionID:   decision.SessionID,
		DecisionID:  decision.DecisionID,
		Kind:        decision.Kind,
		Mode:        mode,
		Status:      approval.StatusAnswered,
		OptionID:    optionID,
		Text:        answerText,
		Validated:   validated,
		Escape:      escape,
		Actor:       actor,
		Channel:     approval.ChannelHTTP,
		AnsweredAt:  &answeredAt,
		OpenedAt:    &decision.CreatedAt,
		DeliveredAt: &decision.CreatedAt,
		Context: map[string]string{
			"remoteAddr": r.RemoteAddr,
			"userAgent":  r.UserAgent(),
			"requestId":  decision.RequestID,
		},
	}
	stored, _, err := s.cfg.Approvals.RecordAnswer(r.Context(), record)
	if err != nil {
		return approval.Record{}, err
	}
	return stored, nil
}

func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) {
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
	rows := make([]map[string]any, 0, len(decisions))
	provenance := map[string]approval.Record{}
	if s.cfg.Approvals != nil {
		records, err := s.cfg.Approvals.List(r.Context(), tenantID, sessionID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		for _, record := range records {
			provenance[record.DecisionID] = record
		}
	}
	for _, decision := range decisions {
		row := map[string]any{"decision": decision}
		if record, ok := provenance[decision.DecisionID]; ok {
			row["provenance"] = record
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": rows, "count": len(rows)})
}

func (s *Server) getApproval(w http.ResponseWriter, r *http.Request) {
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
	body := map[string]any{"decision": decision}
	if s.cfg.Approvals != nil {
		if record, ok, err := s.cfg.Approvals.Get(r.Context(), tenantID, sessionID, decisionID); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		} else if ok {
			body["provenance"] = record
		}
	}
	writeJSON(w, http.StatusOK, body)
}
