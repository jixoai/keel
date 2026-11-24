package harnessapi

// The external-run projection read (change keel-oar-renderer):
//
//	GET /v1/sessions/{id}/external/projection
//
// It is the product API's read of one external session's OAR record stream: the
// same keel.oar.render-projection/1 document internal/oar folds for the CLI and
// the W12 tool-renderer slot (oar.ProjectionFromJournal / oar.ProjectionJSON,
// never a second fold model).
//
// The route is registered unconditionally. A deployment without the journal
// directory answers 501 oar_not_wired instead of an empty projection: a client
// must be able to tell "this deployment has no OAR" from "this session has no
// records yet". A missing journal file is the latter (a fresh external session)
// and answers the same-schema empty projection.
//
// Nothing here writes: no event, no task card, no ledger row. External calls are
// attributed to the card that started them by the out-of-band attribution block
// inside the records; that attribution is read back into the projection, never
// materialized as a second card.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/oar"
	"github.com/jixoai/keel/internal/store"
)

// External projection error codes. They are the route's stable typed answers
// (part of the contract; a client branches on these instead of matching text).
const (
	// ExternalProjectionNotWired reports a deployment without an OAR journal
	// directory. It is a capability state, not a session failure.
	ExternalProjectionNotWired = "oar_not_wired"
	// ExternalProjectionNotExternalSession reports a session that exists for the
	// tenant but is not an external backend session (or has no backend handle).
	ExternalProjectionNotExternalSession = "not_external_session"
	// ExternalProjectionCorrupt reports a journal that exists but cannot be read
	// as records. A corroded journal stays visible instead of being served as an
	// empty projection.
	ExternalProjectionCorrupt = "projection_corrupt"
)

// SessionExternalReader reads one session's external marks for the projection
// route: state_owner and external_ref (the backend session handle whose journal
// is read). The production implementation is *store.Store; the read is
// tenant-scoped, so a foreign session is found=false and answers the same 404
// as a missing one. Nil answers 501 oar_not_wired.
type SessionExternalReader interface {
	GetExternalSession(ctx context.Context, tenantID, sessionID uuid.UUID) (store.ExternalSession, bool, error)
}

// externalProjection implements GET /v1/sessions/{id}/external/projection.
func (s *Server) externalProjection(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	sessionID, ok := sessionFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.ExternalSessions == nil {
		writeExternalProjectionError(w, http.StatusNotImplemented, ExternalProjectionNotWired,
			"the external session surface is not wired in this deployment")
		return
	}
	// One tenant-scoped read supplies both the existence check (the same 404 the
	// other session reads answer) and the external marks. A foreign tenant's
	// session is found=false, so the answer never reveals whether it exists.
	session, found, err := s.cfg.ExternalSessions.GetExternalSession(r.Context(), tenantID, sessionID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, ErrSessionNotFound)
		return
	}
	// A native session has no external backend: the route answers the typed 404
	// instead of an empty projection (which would claim an external run exists).
	if session.StateOwner != store.StateOwnerExternal || strings.TrimSpace(session.Ref) == "" {
		writeExternalProjectionError(w, http.StatusNotFound, ExternalProjectionNotExternalSession,
			"session is not an external session")
		return
	}
	dir := strings.TrimSpace(s.cfg.OARJournalDir)
	if dir == "" {
		writeExternalProjectionError(w, http.StatusNotImplemented, ExternalProjectionNotWired,
			"the OAR journal directory is not configured in this deployment")
		return
	}
	raw, err := readExternalProjection(oar.JournalPath(dir, session.Ref))
	if err != nil {
		// The client gets a stable code and no local path; the operator gets the
		// detail in the host log.
		s.cfg.Logger.Warn("external projection read failed",
			"tenantId", tenantID.String(), "sessionId", sessionID.String(), "err", err)
		writeExternalProjectionError(w, http.StatusInternalServerError, ExternalProjectionCorrupt,
			"the OAR journal could not be read")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// readExternalProjection renders the journal at path. A missing file is an
// external session that has not produced a record yet: it answers the same
// empty projection as an empty journal (the schema is the contract, not the
// row count). Any other read/parse failure is an error the caller maps onto the
// typed 500.
func readExternalProjection(path string) ([]byte, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return oar.ProjectionJSON(oar.BuildProjection(nil))
		}
		return nil, err
	}
	projection, err := oar.ProjectionFromJournal(path)
	if err != nil {
		return nil, err
	}
	return oar.ProjectionJSON(projection)
}

// writeExternalProjectionError is the typed refusal body.
func writeExternalProjectionError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": message, "code": code})
}
