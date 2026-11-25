// Session checkpoint policy surface (change keel-checkpoint):
//
//	GET /v1/sessions/{id}/checkpoint-policy   effective policy projection
//	PUT /v1/sessions/{id}/checkpoint-policy   write a narrowing override
//
// The session can only narrow the tenant policy; the response always carries
// the effective policy plus the fields the tenant clamped. The projection has
// no conversation content and no message body ever enters the audit log.
package harnessapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/sessionpolicy"
)

// SessionCheckpointPolicyService is the session policy read/write seam. A nil
// seam answers 501 (the route exists, the deployment has no policy store).
type SessionCheckpointPolicyService interface {
	Get(ctx context.Context, tenantID, sessionID uuid.UUID) (sessionpolicy.View, error)
	Put(ctx context.Context, tenantID, sessionID uuid.UUID, document compaction.CheckpointPolicyOverrideDocument) (sessionpolicy.View, error)
}

// getCheckpointPolicy implements GET /v1/sessions/{id}/checkpoint-policy.
func (s *Server) getCheckpointPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	sessionID, ok := sessionFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.CheckpointPolicies == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error":  "session checkpoint policy is not wired in this deployment",
			"status": "not_wired",
		})
		return
	}
	if s.cfg.Access != nil && !s.authorizeSessionResource(w, r, tenantID, sessionID, access.KeySessionRead) {
		return
	}
	view, err := s.cfg.CheckpointPolicies.Get(r.Context(), tenantID, sessionID)
	if err != nil {
		s.writeCheckpointPolicyError(w, tenantID, sessionID, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// putCheckpointPolicy implements PUT /v1/sessions/{id}/checkpoint-policy.
func (s *Server) putCheckpointPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	sessionID, ok := sessionFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.CheckpointPolicies == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error":  "session checkpoint policy is not wired in this deployment",
			"status": "not_wired",
		})
		return
	}
	if s.cfg.Access != nil && !s.authorizeSessionResource(w, r, tenantID, sessionID, access.KeySessionWrite) {
		return
	}
	var document compaction.CheckpointPolicyOverrideDocument
	if err := decodeBody(w, r, s.cfg.MaxBody, &document); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	view, err := s.cfg.CheckpointPolicies.Put(r.Context(), tenantID, sessionID, document)
	if err != nil {
		s.writeCheckpointPolicyError(w, tenantID, sessionID, err)
		return
	}
	// Identifier-only audit: fields the tenant clamped, never values or bodies.
	fields := make([]string, 0, len(view.Narrowed))
	for _, narrowed := range view.Narrowed {
		fields = append(fields, narrowed.Field)
	}
	s.cfg.Logger.Info("session checkpoint policy written",
		"tenant_id", tenantID, "session_id", sessionID,
		"revision", view.Revision, "source", view.Source, "narrowed", fields)
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) writeCheckpointPolicyError(w http.ResponseWriter, tenantID, sessionID uuid.UUID, err error) {
	switch {
	case errors.Is(err, sessionpolicy.ErrSessionNotFound):
		writeErr(w, http.StatusNotFound, errors.New("session not found"))
	case errors.Is(err, sessionpolicy.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
	s.cfg.Logger.Warn("session checkpoint policy refused",
		"tenant_id", tenantID, "session_id", sessionID, "err", err)
}
