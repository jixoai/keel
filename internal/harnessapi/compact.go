// Manual checkpoint compaction (change keel-checkpoint):
//
//	POST /v1/sessions/{id}/compact
//	{"requestId": "..."}
//
// The route is a thin adapter over the hosting process's compact seam. It
// owns the permission gate (session.write), the requestId requirement and the
// identifier-only audit line; the seam owns the cut safety, the summary call,
// the compaction event and the checkpoint projection. A repeated requestId
// returns the earlier result (replayed=true) without a second compaction.
//
// The audit/log line never contains a message body or a summary; it is
// ids/tokens/outcome only.
package harnessapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/access"
)

// CompactResult is one manual-compaction outcome as answered by the API.
type CompactResult struct {
	SessionID     string `json:"sessionId"`
	RequestID     string `json:"requestId"`
	CheckpointID  int64  `json:"checkpointId"`
	Performed     bool   `json:"performed"`
	Replayed      bool   `json:"replayed"`
	TokensBefore  int    `json:"tokensBefore"`
	TokensAfter   int    `json:"tokensAfter"`
	KeptFromSeq   int64  `json:"keptFromSeq"`
	PolicyVersion string `json:"policyVersion,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// CompactSessionFunc is the manual-compaction seam. The implementation must be
// idempotent on (session, requestId), must hold the session's run lease while it
// writes the compaction event, and must leave the context untouched on any
// summary failure or cancellation.
type CompactSessionFunc func(ctx context.Context, tenantID, sessionID uuid.UUID, requestID string) (CompactResult, error)

// Errors the seam may return for a non-500 answer.
var (
	// ErrCompactDisabled reports that the effective checkpoint policy disables
	// manual compaction.
	ErrCompactDisabled = errors.New("harnessapi: manual compaction is disabled")
	// ErrCompactBusy reports that the session is executing (or another manual
	// compaction holds the run lease).
	ErrCompactBusy = errors.New("harnessapi: session is busy")
	// ErrCompactQuota reports that the tenant's run admission budget refuses
	// the claim.
	ErrCompactQuota = errors.New("harnessapi: run quota exceeded")
	// ErrCompactConflict reports a context that cannot be compacted safely now
	// (an unread input at the cut, an inherited prefix boundary, or an
	// unresolved tool call).
	ErrCompactConflict = errors.New("harnessapi: session context cannot be compacted safely")
)

const compactRequestIDMax = 200

type compactRequest struct {
	RequestID string `json:"requestId"`
}

// compactSession implements POST /v1/sessions/{id}/compact.
func (s *Server) compactSession(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	sessionID, ok := sessionFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.CompactSession == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error":  "manual compaction seam is not wired: this deployment has no live runner/provider compaction path",
			"status": "not_wired",
		})
		return
	}
	// Permission runs before the body is read: a caller without session.write
	// learns nothing about the session's context by probing this route. A
	// deployment without the access seam fails closed (503), never open.
	if !s.authorizeSessionResource(w, r, tenantID, sessionID, access.KeySessionWrite) {
		return
	}
	var body compactRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	requestID := strings.TrimSpace(body.RequestID)
	if requestID == "" {
		writeErr(w, http.StatusBadRequest, errors.New("requestId is required"))
		return
	}
	if len(requestID) > compactRequestIDMax {
		writeErr(w, http.StatusBadRequest, errors.New("requestId is too long"))
		return
	}
	result, err := s.cfg.CompactSession(r.Context(), tenantID, sessionID, requestID)
	if err != nil {
		switch {
		case errors.Is(err, ErrSessionNotFound):
			writeErr(w, http.StatusNotFound, errors.New("session not found"))
		case errors.Is(err, ErrCompactDisabled):
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":  "manual compaction is disabled by the checkpoint policy",
				"status": "disabled",
			})
		case errors.Is(err, ErrCompactBusy):
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":  "session is busy; retry after the active run settles",
				"status": "busy",
			})
		case errors.Is(err, ErrCompactQuota):
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"error":  "run admission quota exceeded",
				"status": "quota",
			})
		case errors.Is(err, ErrCompactConflict):
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":  "session context cannot be compacted safely; retry after the turn settles",
				"status": "conflict",
			})
		case errors.Is(err, ErrExternalSession):
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":  "session is owned by an external backend; there is no native context to compact",
				"status": "external",
			})
		default:
			writeErr(w, http.StatusInternalServerError, err)
		}
		s.cfg.Logger.Warn("manual compaction refused",
			"tenant_id", tenantID, "session_id", sessionID, "request_id", requestID, "err", err)
		return
	}
	result.SessionID = sessionID.String()
	result.RequestID = requestID
	// Identifier-only audit: no message text and no summary enters the log.
	s.cfg.Logger.Info("manual compaction",
		"tenant_id", tenantID, "session_id", sessionID, "request_id", requestID,
		"checkpoint_seq", result.CheckpointID, "performed", result.Performed,
		"replayed", result.Replayed, "tokens_before", result.TokensBefore,
		"tokens_after", result.TokensAfter, "policy_version", result.PolicyVersion,
		"reason", result.Reason)
	status := http.StatusOK
	if result.Performed {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}
