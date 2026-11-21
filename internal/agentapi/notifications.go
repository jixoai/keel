package agentapi

// Notification routes (change keel-harness-v2, W2.5).
//
// The model creates notifications through the `notify` tool; these routes are
// how a client lists them, acks one consumer, and how the capability URLs the
// push mints are consumed. The open/pixel routes deliberately do NOT require
// X-Tenant-ID: the unguessable single-use token is the authority, and the row
// supplies its tenant. A tenant header, when present, is checked against the
// row (defence in depth).

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/notify"
)

func (s *Server) notificationRoutes() {
	s.mux.HandleFunc("GET /v1/notifications", s.listNotifications)
	s.mux.HandleFunc("POST /v1/notifications/{id}/ack", s.ackNotification)
	s.mux.HandleFunc("GET /v1/notifications/{id}/open", s.openNotificationURL)
	s.mux.HandleFunc("GET /v1/notifications/{id}/pixel", s.trackNotificationDelivery)
}

func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.Notifications == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("notifications are not wired"))
		return
	}
	var sessionID *uuid.UUID
	if raw := strings.TrimSpace(r.URL.Query().Get("sessionId")); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("sessionId must be a UUID"))
			return
		}
		sessionID = &parsed
	}
	limit := 20
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > 100 {
			writeErr(w, http.StatusBadRequest, errors.New("limit must be between 1 and 100"))
			return
		}
		limit = parsed
	}
	if sessionID != nil {
		if !s.authorizeSessionResource(w, r, tenantID, *sessionID, access.KeySessionRead) {
			return
		}
	}
	views, err := s.cfg.Notifications.List(r.Context(), tenantID, sessionID, limit)
	if err != nil {
		writeNotificationError(w, err)
		return
	}
	// A tenant-wide list is still a session-visibility list: keep only the rows
	// whose session the caller may read. Notifications without a session are
	// shown to nobody but a super admin (a notification is never a tenant-public
	// document).
	if sessionID == nil {
		candidates := make([]string, 0, len(views))
		for _, view := range views {
			if view.SessionID != "" {
				candidates = append(candidates, view.SessionID)
			}
		}
		allowed, ok := s.visibleSessionIDs(w, r, tenantID, access.KeySessionRead, candidates)
		if !ok {
			return
		}
		principal, err := s.cfg.Access.Principal(r.Context(), tenantID)
		if err != nil {
			writeIdentityError(w, http.StatusUnauthorized, "invalid_session", "authentication required")
			return
		}
		filtered := views[:0]
		for _, view := range views {
			if view.SessionID == "" {
				if principal.IsSuperAdmin {
					filtered = append(filtered, view)
				}
				continue
			}
			if allowed[view.SessionID] {
				filtered = append(filtered, view)
			}
		}
		views = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(views), "notifications": views})
}

func (s *Server) ackNotification(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.Notifications == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("notifications are not wired"))
		return
	}
	notificationID, ok := notificationIDFrom(w, r)
	if !ok {
		return
	}
	var body struct {
		ConsumerID string `json:"consumerId"`
		Actor      string `json:"actor,omitempty"`
	}
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// The notification row names the session it belongs to; ack is a session
	// write on that session. Reading the row first is also what makes an ack of
	// another account's notification a relation denial instead of a tenant-level
	// write. A sessionless notification is super-admin only.
	current, err := s.cfg.Notifications.Get(r.Context(), tenantID, notificationID)
	if err != nil {
		writeNotificationError(w, err)
		return
	}
	if current.SessionID != "" {
		sessionID, err := uuid.Parse(current.SessionID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, errors.New("notification has a malformed session id"))
			return
		}
		// Acking a notification is consumption state, not session content: the
		// read relation is the right authority (a read-only viewer may ack).
		if !s.authorizeSessionResource(w, r, tenantID, sessionID, access.KeySessionRead) {
			return
		}
	} else {
		if s.cfg.Access == nil {
			writeErr(w, http.StatusServiceUnavailable, errors.New("the identity surface is not wired in this deployment"))
			return
		}
		principal, err := s.cfg.Access.Principal(r.Context(), tenantID)
		if err != nil {
			writeIdentityError(w, http.StatusUnauthorized, "invalid_session", "authentication required")
			return
		}
		if !principal.IsSuperAdmin {
			s.cfg.Access.Denied(r.Context(), principal, access.Decision{
				Allow: false, Reason: access.ReasonMissingRelation, Key: access.KeySessionWrite,
				Detail: "a sessionless notification can only be acknowledged by a super admin",
			}, access.Resource{TenantID: tenantID, Type: "notification", ID: notificationID.String()})
			writeIdentityError(w, http.StatusForbidden, "forbidden",
				"a sessionless notification can only be acknowledged by a super admin")
			return
		}
	}
	actor := strings.TrimSpace(r.Header.Get("X-Keel-Actor"))
	if actor == "" {
		actor = body.Actor
	}
	view, duplicate, err := s.cfg.Notifications.Ack(r.Context(), tenantID, notificationID, body.ConsumerID, actor, notify.ChannelAPI)
	if err != nil {
		writeNotificationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notification": view, "duplicate": duplicate})
}

func (s *Server) openNotificationURL(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Notifications == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("notifications are not wired"))
		return
	}
	notificationID, ok := notificationIDFrom(w, r)
	if !ok {
		return
	}
	tenantHint, ok := notificationTenantHint(w, r)
	if !ok {
		return
	}
	view, err := s.cfg.Notifications.OpenURL(r.Context(), notificationID, r.URL.Query().Get("token"), tenantHint)
	if err != nil {
		writeNotificationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notification": view, "consumed": true})
}

func (s *Server) trackNotificationDelivery(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Notifications == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("notifications are not wired"))
		return
	}
	notificationID, ok := notificationIDFrom(w, r)
	if !ok {
		return
	}
	tenantHint, ok := notificationTenantHint(w, r)
	if !ok {
		return
	}
	view, err := s.cfg.Notifications.TrackDelivery(r.Context(), notificationID, r.URL.Query().Get("token"), tenantHint)
	if err != nil {
		writeNotificationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notification": view, "tracked": true})
}

func notificationIDFrom(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("notification id must be a UUID"))
		return uuid.Nil, false
	}
	return id, true
}

// notificationTenantHint reads an optional X-Tenant-ID on the token routes.
// Absence is valid; a malformed header is a client error.
func notificationTenantHint(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	raw := strings.TrimSpace(r.Header.Get("X-Tenant-ID"))
	if raw == "" {
		return uuid.Nil, true
	}
	tenantID, err := uuid.Parse(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("X-Tenant-ID is not a UUID"))
		return uuid.Nil, false
	}
	return tenantID, true
}

// writeNotificationError maps the notify package's typed errors onto HTTP.
func writeNotificationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, notify.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, notify.ErrTokenInvalid):
		// Do not distinguish a wrong token from a missing notification: both
		// answer 404, so the route is not an oracle.
		writeErr(w, http.StatusNotFound, errors.New("notification or token not found"))
	case errors.Is(err, notify.ErrTokenExpired), errors.Is(err, notify.ErrTokenUsed), errors.Is(err, notify.ErrConsumed):
		writeErr(w, http.StatusGone, err)
	case errors.Is(err, notify.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err)
	case errors.Is(err, notify.ErrNotWired):
		writeErr(w, http.StatusServiceUnavailable, err)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}
