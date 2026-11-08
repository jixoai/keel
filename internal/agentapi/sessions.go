package agentapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentext"
)

// Session listing and cancellation.
const (
	defaultSessionsLimit = 50
	maxSessionsLimit     = 200
)

// ErrBadCursor is a malformed pagination cursor (400).
var ErrBadCursor = errors.New("agentapi: malformed pagination cursor")

// SessionSummary is one row of GET /v1/sessions. The six spec fields are
// sessionId, createdAt, updatedAt, lastSeq, runStatus, lastMessageAt;
// parentSessionId is additive and only present for sub-agent sessions.
type SessionSummary struct {
	SessionID string    `json:"sessionId"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	LastSeq   int64     `json:"lastSeq"`
	RunStatus string    `json:"runStatus"`
	// LastMessageAt is always present (null when the session has no events):
	// the spec makes it part of every item, not an omission.
	LastMessageAt   *time.Time `json:"lastMessageAt"`
	ParentSessionID string     `json:"parentSessionId,omitempty"`
}

// SessionPage is one keyset page. NextCursor is empty on the last page.
type SessionPage struct {
	Sessions   []SessionSummary
	NextCursor string
	HasMore    bool
}

// SessionLister is the keyset session query. The production implementation is
// PGSessionLister (internal/agentapi); tests supply a fake.
type SessionLister interface {
	ListSessionSummaries(ctx context.Context, tenantID uuid.UUID, limit int, after string) (SessionPage, error)
}

// listSessions implements GET /v1/sessions?limit=&after=.
func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.Sessions == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("session listing is not wired"))
		return
	}
	limit, err := queryInt(r, "limit", defaultSessionsLimit, 1, maxSessionsLimit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	page, err := s.cfg.Sessions.ListSessionSummaries(r.Context(), tenantID, limit, r.URL.Query().Get("after"))
	if err != nil {
		if errors.Is(err, ErrBadCursor) {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	for i := range page.Sessions {
		summary := &page.Sessions[i]
		if summary.RunStatus == "" {
			summary.RunStatus = "idle"
		}
		if s.cfg.RunStatus != nil {
			if sessionID, err := uuid.Parse(summary.SessionID); err == nil {
				if override, ok := s.cfg.RunStatus(tenantID, sessionID); ok && override != "" {
					summary.RunStatus = override
				}
			}
		}
		if summary.ParentSessionID == "" && s.cfg.Parents != nil {
			if sessionID, err := uuid.Parse(summary.SessionID); err == nil {
				if parent, ok := s.cfg.Parents.ParentOf(sessionID); ok {
					summary.ParentSessionID = parent
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions":   page.Sessions,
		"nextCursor": page.NextCursor,
		"hasMore":    page.HasMore,
	})
}

// cancelSession implements POST /v1/sessions/{id}/cancel. It is idempotent: a
// repeated call reports the same terminal state, never a second cancellation.
func (s *Server) cancelSession(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	sessionID, ok := sessionFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.Cancel == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error":  "cancel seam is not wired: the kernel needs a run-cancellation primitive (Finish with Outcome=cancelled) or the worker must register with agentext.RunControl",
			"status": "not_wired",
		})
		return
	}
	result, err := s.cfg.Cancel(r.Context(), tenantID, sessionID)
	if err != nil {
		switch {
		case errors.Is(err, ErrSessionNotFound):
			writeErr(w, http.StatusNotFound, errors.New("session not found"))
		case errors.Is(err, ErrExternalSession):
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":  "session is owned by an external backend; there is no native run to cancel",
				"status": result.Status,
				"detail": result.Detail,
			})
		case errors.Is(err, agentext.ErrCancelUnavailable):
			writeJSON(w, http.StatusNotImplemented, map[string]any{
				"error":  err.Error(),
				"status": result.Status,
				"detail": result.Detail,
			})
		default:
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	decisionsCancelled := 0
	if result.Cancelled && s.cfg.Decisions != nil {
		decisionsCancelled = s.cfg.Decisions.CancelSession(r.Context(), tenantID, sessionID, "session cancelled")
	}
	status := http.StatusOK
	if result.Status == "cancelling" {
		status = http.StatusAccepted
	}
	writeJSON(w, status, map[string]any{
		"sessionId":          sessionID,
		"status":             result.Status,
		"runStatus":          result.RunStatus,
		"cancelled":          result.Cancelled,
		"detail":             result.Detail,
		"decisionsCancelled": decisionsCancelled,
	})
}
