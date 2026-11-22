// Resource sharing HTTP surface (change keel-bot-identity-access, A6).
//
// ReBAC tuples existed at the engine and store layers, but there was no product
// surface to create or revoke them: cross-account sharing could only be done by
// inserting rows. This file is that surface for the three shareable resources:
//
//	GET  /v1/sessions/{id}/shares         session.share     + owner
//	POST /v1/sessions/{id}/shares         session.share     + owner
//	POST /v1/sessions/{id}/shares/revoke  session.share     + owner
//
// and the same trio for /v1/bots/{id} (bot.manage + owner) and
// /v1/team-bots/{id} (team_bot.manage + owner).
//
// The product rules are enforced here, not in the UI:
//
//   - only viewer/editor are writable; owner/team_member are system-managed and
//     a request for either is a 400;
//   - the subject must exist in the caller's tenant, resolved before the write;
//     a foreign or unknown subject is 404 (the same answer, no existence leak);
//   - no self-share: an account subject equal to the caller is a 400;
//   - a duplicate tuple is idempotent (`created:false`, 200), not a 409;
//   - revoking an absent tuple is 404;
//   - every accepted/revoked tuple appends an `access.AuditRelation` row; a
//     failed audit is a 500, exactly like the create paths (A4.2a §7);
//   - revocation is live: the engine reads relations on every request, so the
//     next request after this handler returns sees the new tuple state.
//
// The key + owner check runs before any row lookup, so an unauthorized caller
// gets one 403 regardless of whether the resource or subject exists.
package harnessapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/store"
)

// shareListLimit bounds one share listing. It matches the identity page floor:
// a resource shared to hundreds of subjects still answers in bounded memory.
const shareListLimit = 200

// shareResource names one shareable resource family and the key+relation its
// write surface requires. The registry maps each key to its required relation:
// session.share, bot.manage and team_bot.manage all require the owner relation,
// so an editor (or a plain viewer) is refused by requireResource itself.
type shareResource struct {
	kind string
	key  string
}

var (
	sessionShareResource = shareResource{kind: "session", key: access.KeySessionShare}
	botShareResource     = shareResource{kind: "bot", key: access.KeyBotManage}
	teamBotShareResource = shareResource{kind: "team_bot", key: access.KeyTeamBotManage}
)

// shareView is one listed tuple. DisplayName/Username are resolved best effort
// when the subject still exists; neither is a credential, an email or a secret.
type shareView struct {
	SubjectType string `json:"subjectType"`
	SubjectID   string `json:"subjectId"`
	Relation    string `json:"relation"`
	DisplayName string `json:"displayName,omitempty"`
	Username    string `json:"username,omitempty"`
}

type shareWriteRequest struct {
	SubjectType string `json:"subjectType"`
	SubjectID   string `json:"subjectId"`
	Relation    string `json:"relation"`
}

// shareID reads and normalizes the resource id from the path. Sessions are
// uuid resources; bots and team bots are text handles (A1 §9.1).
func shareID(w http.ResponseWriter, r *http.Request, resource shareResource) (string, bool) {
	raw := strings.TrimSpace(r.PathValue("id"))
	if raw == "" {
		writeIdentityError(w, http.StatusBadRequest, "invalid_resource_id", "resource id is required")
		return "", false
	}
	if resource.kind == "session" {
		id, err := uuid.Parse(raw)
		if err != nil || id == uuid.Nil {
			writeIdentityError(w, http.StatusBadRequest, "invalid_session_id", "session id must be a UUID")
			return "", false
		}
		return id.String(), true
	}
	return raw, true
}

// requireShareOwner authorizes one share read/write: key + owner relation from
// the engine, then an honest existence read. A caller without authority gets
// the 403 before the existence read (no existence leak).
func (s *Server) requireShareOwner(w http.ResponseWriter, r *http.Request, resource shareResource) (access.Principal, uuid.UUID, string, bool) {
	if !s.identityReady(w) {
		return access.Principal{}, uuid.Nil, "", false
	}
	resourceID, ok := shareID(w, r, resource)
	if !ok {
		return access.Principal{}, uuid.Nil, "", false
	}
	principal, tenantID, ok := s.requireResource(w, r, resource.key, resource.kind, resourceID)
	if !ok {
		return access.Principal{}, uuid.Nil, "", false
	}
	if !s.shareTargetExists(w, r, resource, tenantID, resourceID) {
		return access.Principal{}, uuid.Nil, "", false
	}
	return principal, tenantID, resourceID, true
}

// shareTargetExists turns a random id into a 404 for an authorized caller. It
// runs after the relation check, never before it.
func (s *Server) shareTargetExists(w http.ResponseWriter, r *http.Request, resource shareResource, tenantID uuid.UUID, resourceID string) bool {
	var err error
	switch resource.kind {
	case "session":
		sessionID, parseErr := uuid.Parse(resourceID)
		if parseErr != nil {
			writeIdentityError(w, http.StatusBadRequest, "invalid_session_id", "session id must be a UUID")
			return false
		}
		_, err = s.cfg.Identity.GetSessionOwnership(r.Context(), tenantID, sessionID)
	case "bot":
		_, err = s.cfg.Identity.GetBot(r.Context(), tenantID, resourceID)
	case "team_bot":
		_, err = s.cfg.Identity.GetTeamBot(r.Context(), tenantID, resourceID)
	default:
		writeErr(w, http.StatusInternalServerError, errors.New("internal error: unknown share resource "+resource.kind))
		return false
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeIdentityError(w, http.StatusNotFound, "not_found", "not found")
			return false
		}
		writeStoreError(w, err)
		return false
	}
	return true
}

// parseShareWrite validates the body. Only viewer/editor are writable: owner
// and team_member are system-managed and can never be granted through HTTP.
func parseShareWrite(w http.ResponseWriter, body shareWriteRequest) (shareWriteRequest, bool) {
	body.SubjectType = strings.TrimSpace(body.SubjectType)
	body.SubjectID = strings.TrimSpace(body.SubjectID)
	body.Relation = strings.TrimSpace(body.Relation)
	switch body.SubjectType {
	case store.RelationSubjectAccount, store.RelationSubjectGroup, store.RelationSubjectTeamBot:
	default:
		writeIdentityError(w, http.StatusBadRequest, "invalid_request",
			"subjectType must be account, group or team_bot")
		return shareWriteRequest{}, false
	}
	if body.SubjectID == "" {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", "subjectId is required")
		return shareWriteRequest{}, false
	}
	switch body.Relation {
	case store.RelationViewer, store.RelationEditor:
	case store.RelationOwner, store.RelationTeamMember:
		writeIdentityError(w, http.StatusBadRequest, "invalid_relation",
			"owner and team_member are managed by the system and cannot be granted through sharing")
		return shareWriteRequest{}, false
	default:
		writeIdentityError(w, http.StatusBadRequest, "invalid_relation",
			"relation must be viewer or editor")
		return shareWriteRequest{}, false
	}
	return body, true
}

// resolveShareSubject proves the subject exists in the tenant before the write
// and returns its display facts. A missing or foreign subject is one 404.
func (s *Server) resolveShareSubject(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, principal access.Principal, body shareWriteRequest, forWrite bool) (shareView, bool) {
	view := shareView{SubjectType: body.SubjectType, SubjectID: body.SubjectID, Relation: body.Relation}
	switch body.SubjectType {
	case store.RelationSubjectAccount:
		accountID, err := uuid.Parse(body.SubjectID)
		if err != nil || accountID == uuid.Nil {
			writeIdentityError(w, http.StatusBadRequest, "invalid_subject_id", "an account subject id must be a UUID")
			return shareView{}, false
		}
		if forWrite && accountID == principal.AccountID {
			writeIdentityError(w, http.StatusBadRequest, "self_share", "a resource cannot be shared with its own caller")
			return shareView{}, false
		}
		account, err := s.cfg.Identity.GetAccount(r.Context(), tenantID, accountID)
		if err != nil {
			return shareView{}, writeShareSubjectError(w, err)
		}
		view.DisplayName = account.DisplayName
		view.Username = account.Username
	case store.RelationSubjectGroup:
		groupID, err := uuid.Parse(body.SubjectID)
		if err != nil || groupID == uuid.Nil {
			writeIdentityError(w, http.StatusBadRequest, "invalid_subject_id", "a group subject id must be a UUID")
			return shareView{}, false
		}
		group, err := s.cfg.Identity.GetPermissionGroup(r.Context(), tenantID, groupID)
		if err != nil {
			return shareView{}, writeShareSubjectError(w, err)
		}
		view.DisplayName = group.Name
	case store.RelationSubjectTeamBot:
		team, err := s.cfg.Identity.GetTeamBot(r.Context(), tenantID, body.SubjectID)
		if err != nil {
			return shareView{}, writeShareSubjectError(w, err)
		}
		view.DisplayName = team.Name
	}
	return view, true
}

// writeShareSubjectError maps a subject lookup: missing/foreign is 404, a
// malformed id is already handled above, everything else is a store error.
func writeShareSubjectError(w http.ResponseWriter, err error) bool {
	if errors.Is(err, store.ErrNotFound) {
		writeIdentityError(w, http.StatusNotFound, "subject_not_found", "the subject does not exist in this tenant")
		return false
	}
	writeStoreError(w, err)
	return false
}

// listResourceShares is the shared implementation of the three GET routes.
func (s *Server) listResourceShares(w http.ResponseWriter, r *http.Request, resource shareResource) {
	_, tenantID, resourceID, ok := s.requireShareOwner(w, r, resource)
	if !ok {
		return
	}
	tuples, err := s.cfg.Identity.ListResourceRelations(r.Context(), tenantID, resource.kind, resourceID, maxIdentityPageLimit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]shareView, 0, len(tuples))
	cache := make(map[string]shareView, len(tuples))
	for _, tuple := range tuples {
		if tuple.Relation != store.RelationViewer && tuple.Relation != store.RelationEditor {
			continue
		}
		if len(views) >= shareListLimit {
			break
		}
		key := tuple.SubjectType + "\x00" + tuple.SubjectID
		view, cached := cache[key]
		if !cached {
			view = shareViewOf(r.Context(), s, tenantID, tuple)
			cache[key] = view
		}
		view.Relation = tuple.Relation
		views = append(views, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resourceType": resource.kind,
		"resourceId":   resourceID,
		"shares":       views,
	})
}

// shareViewOf resolves display facts best effort. A tuple whose subject was
// deleted still lists with its ids, so the owner can revoke it.
func shareViewOf(ctx context.Context, s *Server, tenantID uuid.UUID, tuple store.ResourceRelation) shareView {
	view := shareView{SubjectType: tuple.SubjectType, SubjectID: tuple.SubjectID, Relation: tuple.Relation}
	switch tuple.SubjectType {
	case store.RelationSubjectAccount:
		accountID, err := uuid.Parse(tuple.SubjectID)
		if err != nil {
			return view
		}
		if account, err := s.cfg.Identity.GetAccount(ctx, tenantID, accountID); err == nil {
			view.DisplayName = account.DisplayName
			view.Username = account.Username
		}
	case store.RelationSubjectGroup:
		groupID, err := uuid.Parse(tuple.SubjectID)
		if err != nil {
			return view
		}
		if group, err := s.cfg.Identity.GetPermissionGroup(ctx, tenantID, groupID); err == nil {
			view.DisplayName = group.Name
		}
	case store.RelationSubjectTeamBot:
		if team, err := s.cfg.Identity.GetTeamBot(ctx, tenantID, tuple.SubjectID); err == nil {
			view.DisplayName = team.Name
		}
	}
	return view
}

// addResourceShare is the shared implementation of the three POST routes.
func (s *Server) addResourceShare(w http.ResponseWriter, r *http.Request, resource shareResource) {
	principal, tenantID, resourceID, ok := s.requireShareOwner(w, r, resource)
	if !ok {
		return
	}
	var body shareWriteRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	body, ok = parseShareWrite(w, body)
	if !ok {
		return
	}
	view, ok := s.resolveShareSubject(w, r, tenantID, principal, body, true)
	if !ok {
		return
	}
	created, err := s.cfg.Identity.PutResourceRelation(r.Context(), store.ResourceRelation{
		TenantID:     tenantID,
		ResourceType: resource.kind,
		ResourceID:   resourceID,
		Relation:     body.Relation,
		SubjectType:  body.SubjectType,
		SubjectID:    body.SubjectID,
		CreatedBy:    principal.AccountID,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// The tuple is live. Its audit row is mandatory: a failed append is a 500,
	// matching the create paths (the write already committed; the caller must
	// not see an unaudited success).
	if err := s.cfg.Access.AuditRelation(r.Context(), tenantID, principal.AccountID, "share",
		resource.kind, resourceID, body.Relation, body.SubjectType, body.SubjectID); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if principal.IsSuperAdmin {
		if err := s.cfg.Access.AuditSuperAdmin(r.Context(), principal, "relation.share", map[string]any{
			"resource_type": resource.kind, "resource_id": resourceID,
			"relation": body.Relation, "subject_type": body.SubjectType, "subject_id": body.SubjectID,
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.identityChanged(tenantID, shareEventKind(resource.kind))
	writeJSON(w, status, map[string]any{"created": created, "share": view})
}

// revokeResourceShare is the shared implementation of the three revoke routes.
// POST (not DELETE) keeps the route matrix body-free; the body is the tuple.
func (s *Server) revokeResourceShare(w http.ResponseWriter, r *http.Request, resource shareResource) {
	principal, tenantID, resourceID, ok := s.requireShareOwner(w, r, resource)
	if !ok {
		return
	}
	var body shareWriteRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	body, ok = parseShareWrite(w, body)
	if !ok {
		return
	}
	// Revoke must still work after the subject row is gone: resource_relations
	// keeps the polymorphic subject_id without an FK, and a deleted group would
	// otherwise leave an unremovable tuple. The view is best effort for display;
	// the delete below is the authority on whether the tuple existed. Self-share
	// is not refused here: if such a tuple ever exists, the owner must be able to
	// clean it up.
	view := shareViewOf(r.Context(), s, tenantID, store.ResourceRelation{
		SubjectType: body.SubjectType, SubjectID: body.SubjectID, Relation: body.Relation,
	})
	if err := s.cfg.Identity.DeleteResourceRelation(r.Context(), tenantID, resource.kind, resourceID,
		body.Relation, body.SubjectType, body.SubjectID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeIdentityError(w, http.StatusNotFound, "not_found", "the share does not exist")
			return
		}
		writeStoreError(w, err)
		return
	}
	if err := s.cfg.Access.AuditRelation(r.Context(), tenantID, principal.AccountID, "revoke",
		resource.kind, resourceID, body.Relation, body.SubjectType, body.SubjectID); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if principal.IsSuperAdmin {
		if err := s.cfg.Access.AuditSuperAdmin(r.Context(), principal, "relation.revoke", map[string]any{
			"resource_type": resource.kind, "resource_id": resourceID,
			"relation": body.Relation, "subject_type": body.SubjectType, "subject_id": body.SubjectID,
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	s.identityChanged(tenantID, shareEventKind(resource.kind))
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true, "share": view})
}

// shareEventKind maps a shareable resource family to the projection its tuple
// change makes stale. A session share changes the session list (sharedWithMe),
// a Bot/TeamBot share changes its row's shared badge.
func shareEventKind(kind string) IdentityEventKind {
	switch kind {
	case "session":
		return IdentityEventSessions
	case "team_bot":
		return IdentityEventTeams
	default:
		return IdentityEventBots
	}
}

// The nine route handlers. They are deliberately one-liners: the semantics live
// in the shared functions above, so the three resource families cannot drift.

func (s *Server) listSessionShares(w http.ResponseWriter, r *http.Request) {
	s.listResourceShares(w, r, sessionShareResource)
}

func (s *Server) addSessionShare(w http.ResponseWriter, r *http.Request) {
	s.addResourceShare(w, r, sessionShareResource)
}

func (s *Server) revokeSessionShare(w http.ResponseWriter, r *http.Request) {
	s.revokeResourceShare(w, r, sessionShareResource)
}

func (s *Server) listBotShares(w http.ResponseWriter, r *http.Request) {
	s.listResourceShares(w, r, botShareResource)
}

func (s *Server) addBotShare(w http.ResponseWriter, r *http.Request) {
	s.addResourceShare(w, r, botShareResource)
}

func (s *Server) revokeBotShare(w http.ResponseWriter, r *http.Request) {
	s.revokeResourceShare(w, r, botShareResource)
}

func (s *Server) listTeamBotShares(w http.ResponseWriter, r *http.Request) {
	s.listResourceShares(w, r, teamBotShareResource)
}

func (s *Server) addTeamBotShare(w http.ResponseWriter, r *http.Request) {
	s.addResourceShare(w, r, teamBotShareResource)
}

func (s *Server) revokeTeamBotShare(w http.ResponseWriter, r *http.Request) {
	s.revokeResourceShare(w, r, teamBotShareResource)
}
