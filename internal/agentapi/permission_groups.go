// Permission groups HTTP surface (change keel-bot-identity-access, A4.1).
//
// A group is a set of (key, scope) grants. Membership is additive and there is
// no deny and no nesting (L5). Two invariants shape the writes:
//
//   - grant ⊆ own: a group grant and a membership (which confers every grant of
//     the group) are checked against the actor's own effective set. The super
//     admin short circuit lives in the engine.
//   - built-in groups are read-only: their name and grant set are the product's
//     preset. Membership stays editable — that is how an account receives the
//     preset. Customization is "save as a copy" (A4.4/L5), not mutating the
//     preset.
package agentapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/store"
)

type permissionGroupView struct {
	GroupID   string `json:"groupId"`
	Name      string `json:"name"`
	Builtin   bool   `json:"builtin"`
	CreatedAt any    `json:"createdAt"`
	UpdatedAt any    `json:"updatedAt"`
}

func permissionGroupViewOf(record store.PermissionGroupRecord) permissionGroupView {
	return permissionGroupView{
		GroupID: record.ID.String(), Name: record.Name, Builtin: record.Builtin,
		CreatedAt: timeView(record.CreatedAt), UpdatedAt: timeView(record.UpdatedAt),
	}
}

func (s *Server) listPermissionGroups(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	if _, _, ok := s.requireKey(w, r, access.KeyPermissionRead); !ok {
		return
	}
	limit, err := identityLimit(r)
	if err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	tenantID, _ := tenantFrom(w, r)
	records, err := s.cfg.Identity.ListPermissionGroups(r.Context(), tenantID, limit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]permissionGroupView, 0, len(records))
	for _, record := range records {
		views = append(views, permissionGroupViewOf(record))
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": views})
}

type permissionGroupRequest struct {
	Name string `json:"name"`
}

func (s *Server) createPermissionGroup(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	principal, tenantID, ok := s.requireKey(w, r, access.KeyPermissionManage)
	if !ok {
		return
	}
	var body permissionGroupRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", "name is required")
		return
	}
	record, err := s.cfg.Identity.CreatePermissionGroup(r.Context(), store.PermissionGroupRecord{
		TenantID: tenantID, ID: uuid.New(), Name: name,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if principal.IsSuperAdmin {
		s.cfg.Access.AuditSuperAdmin(r.Context(), principal, "permission_group.create", map[string]any{
			"group_id": record.ID.String(), "name": record.Name,
		})
	}
	writeJSON(w, http.StatusCreated, map[string]any{"group": permissionGroupViewOf(record)})
}

func (s *Server) getPermissionGroup(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	if _, _, ok := s.requireKey(w, r, access.KeyPermissionRead); !ok {
		return
	}
	groupID, ok := pathUUID(w, r, "id", "invalid_group_id")
	if !ok {
		return
	}
	tenantID, _ := tenantFrom(w, r)
	group, err := s.cfg.Identity.GetPermissionGroup(r.Context(), tenantID, groupID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	grants, err := s.cfg.Identity.ListPermissionGroupGrants(r.Context(), tenantID, groupID, maxIdentityPageLimit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	members, err := s.cfg.Identity.ListPermissionGroupMembers(r.Context(), tenantID, groupID, maxIdentityPageLimit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	memberViews := make([]map[string]any, 0, len(members))
	for _, member := range members {
		memberViews = append(memberViews, map[string]any{
			"accountId": member.AccountID.String(), "addedBy": member.AddedBy.String(),
			"createdAt": member.CreatedAt.UTC(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group":   permissionGroupViewOf(group),
		"grants":  grantViews(grants),
		"members": memberViews,
	})
}

func (s *Server) renamePermissionGroup(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	principal, tenantID, ok := s.requireKey(w, r, access.KeyPermissionManage)
	if !ok {
		return
	}
	groupID, ok := pathUUID(w, r, "id", "invalid_group_id")
	if !ok {
		return
	}
	group, err := s.cfg.Identity.GetPermissionGroup(r.Context(), tenantID, groupID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if builtinGroupGuard(w, group) {
		return
	}
	var body permissionGroupRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", "name is required")
		return
	}
	updated, err := s.cfg.Identity.RenamePermissionGroup(r.Context(), tenantID, groupID, name)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if principal.IsSuperAdmin {
		s.cfg.Access.AuditSuperAdmin(r.Context(), principal, "permission_group.rename", map[string]any{
			"group_id": groupID.String(), "name": updated.Name,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"group": permissionGroupViewOf(updated)})
}

func (s *Server) deletePermissionGroup(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	principal, tenantID, ok := s.requireKey(w, r, access.KeyPermissionManage)
	if !ok {
		return
	}
	groupID, ok := pathUUID(w, r, "id", "invalid_group_id")
	if !ok {
		return
	}
	group, err := s.cfg.Identity.GetPermissionGroup(r.Context(), tenantID, groupID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if builtinGroupGuard(w, group) {
		return
	}
	if err := s.cfg.Identity.DeletePermissionGroup(r.Context(), tenantID, groupID); err != nil {
		writeStoreError(w, err)
		return
	}
	if principal.IsSuperAdmin {
		s.cfg.Access.AuditSuperAdmin(r.Context(), principal, "permission_group.delete", map[string]any{
			"group_id": groupID.String(), "name": group.Name,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"groupId": groupID.String(), "deleted": true})
}

type groupMemberRequest struct {
	AccountID string `json:"accountId"`
}

func (s *Server) addPermissionGroupMember(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	principal, tenantID, ok := s.requireKey(w, r, access.KeyAccountPermissionGrant)
	if !ok {
		return
	}
	groupID, ok := pathUUID(w, r, "id", "invalid_group_id")
	if !ok {
		return
	}
	var body groupMemberRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	accountID, err := uuid.Parse(strings.TrimSpace(body.AccountID))
	if err != nil || accountID == uuid.Nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_account_id", "accountId must be a UUID")
		return
	}
	group, err := s.cfg.Identity.GetPermissionGroup(r.Context(), tenantID, groupID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if _, err := s.cfg.Identity.GetAccount(r.Context(), tenantID, accountID); err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.checkGroupGrantable(w, r, principal, tenantID, group); err != nil {
		return
	}
	created, err := s.cfg.Identity.AddPermissionGroupMember(r.Context(), tenantID, groupID, accountID, principal.AccountID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	s.cfg.Access.AuditGrant(r.Context(), tenantID, principal.AccountID, accountID,
		"grant", "group_membership", "", "", "ok", map[string]any{
			"group_id": groupID.String(), "group": group.Name, "created": created,
		})
	writeJSON(w, http.StatusOK, map[string]any{
		"groupId": groupID.String(), "accountId": accountID.String(), "added": created,
	})
}

func (s *Server) removePermissionGroupMember(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	principal, tenantID, ok := s.requireKey(w, r, access.KeyAccountPermissionGrant)
	if !ok {
		return
	}
	groupID, ok := pathUUID(w, r, "id", "invalid_group_id")
	if !ok {
		return
	}
	accountID, ok := pathUUID(w, r, "accountId", "invalid_account_id")
	if !ok {
		return
	}
	if err := s.cfg.Identity.RemovePermissionGroupMember(r.Context(), tenantID, groupID, accountID); err != nil {
		writeStoreError(w, err)
		return
	}
	s.cfg.Access.AuditGrant(r.Context(), tenantID, principal.AccountID, accountID,
		"revoke", "group_membership", "", "", "ok", map[string]any{"group_id": groupID.String()})
	writeJSON(w, http.StatusOK, map[string]any{
		"groupId": groupID.String(), "accountId": accountID.String(), "removed": true,
	})
}

type groupGrantRequest struct {
	Key   string `json:"key"`
	Scope string `json:"scope"`
}

func (s *Server) addPermissionGroupGrant(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	principal, tenantID, ok := s.requireKey(w, r, access.KeyPermissionManage)
	if !ok {
		return
	}
	groupID, ok := pathUUID(w, r, "id", "invalid_group_id")
	if !ok {
		return
	}
	group, err := s.cfg.Identity.GetPermissionGroup(r.Context(), tenantID, groupID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if builtinGroupGuard(w, group) {
		return
	}
	var body groupGrantRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	key := strings.TrimSpace(body.Key)
	scope, err := access.ParseScope(strings.TrimSpace(body.Scope))
	if err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_scope", err.Error())
		return
	}
	grant := access.Grant{Key: key, Scope: scope}
	if err := access.ValidateGrant(grant); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_grant", err.Error())
		return
	}
	if err := s.cfg.Access.CanGrant(r.Context(), principal, []access.Grant{grant}); err != nil {
		var rejection *access.GrantRejection
		reason := access.ReasonMissingGrant
		if errors.As(err, &rejection) {
			reason = rejection.Reason
		}
		s.cfg.Access.Denied(r.Context(), principal, access.Decision{
			Allow: false, Reason: reason, Key: key, Scope: scope, Detail: err.Error(),
		}, access.Resource{TenantID: tenantID, Type: "permission_group", ID: groupID.String()})
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "forbidden", "code": "grant_exceeds_own", "reason": reason.String(), "key": key,
		})
		return
	}
	created, err := s.cfg.Identity.AddPermissionGroupGrant(r.Context(), tenantID, groupID, key, string(scope))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	s.cfg.Access.AuditGrant(r.Context(), tenantID, principal.AccountID, groupID,
		"grant", "group_grant", key, string(scope), "ok", map[string]any{"created": created})
	writeJSON(w, http.StatusOK, map[string]any{
		"groupId": groupID.String(), "key": key, "scope": string(scope), "added": created,
	})
}

func (s *Server) removePermissionGroupGrant(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	principal, tenantID, ok := s.requireKey(w, r, access.KeyPermissionManage)
	if !ok {
		return
	}
	groupID, ok := pathUUID(w, r, "id", "invalid_group_id")
	if !ok {
		return
	}
	group, err := s.cfg.Identity.GetPermissionGroup(r.Context(), tenantID, groupID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if builtinGroupGuard(w, group) {
		return
	}
	key := strings.TrimSpace(r.PathValue("key"))
	scope, err := access.ParseScope(strings.TrimSpace(r.URL.Query().Get("scope")))
	if err != nil || key == "" {
		writeIdentityError(w, http.StatusBadRequest, "invalid_grant", "key and scope are required")
		return
	}
	if err := s.cfg.Identity.RemovePermissionGroupGrant(r.Context(), tenantID, groupID, key, string(scope)); err != nil {
		writeStoreError(w, err)
		return
	}
	s.cfg.Access.AuditGrant(r.Context(), tenantID, principal.AccountID, groupID,
		"revoke", "group_grant", key, string(scope), "ok", nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"groupId": groupID.String(), "key": key, "scope": string(scope), "removed": true,
	})
}

// builtinGroupGuard answers 409 for a write to a built-in preset. Membership is
// not guarded: receiving a preset is the point of a preset.
func builtinGroupGuard(w http.ResponseWriter, group store.PermissionGroupRecord) bool {
	if !group.Builtin {
		return false
	}
	writeIdentityError(w, http.StatusConflict, "builtin_group_read_only",
		"a built-in permission group is read-only; save a copy to customize it")
	return true
}
