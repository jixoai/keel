// Accounts HTTP surface (change keel-bot-identity-access, A4.1).
//
// Account is the login and permission subject. This surface creates human, bot
// and service accounts, disables/enables them, resets credentials and assigns
// permission groups. It never creates or promotes a super admin: is_super_admin
// is a deployment bootstrap fact, so a request that asks for it is refused
// explicitly instead of being silently ignored (and the store write never sets
// it here).
package agentapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/botteam"
	"github.com/jixoai/keel/internal/store"
)

// accountView is the wire projection: no password hash, no salt, no session.
type accountView struct {
	AccountID    string     `json:"accountId"`
	Username     string     `json:"username"`
	DisplayName  string     `json:"displayName"`
	Kind         string     `json:"kind"`
	IsSuperAdmin bool       `json:"isSuperAdmin"`
	DisabledAt   *time.Time `json:"disabledAt"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

func accountViewOf(record store.AccountRecord) accountView {
	return accountView{
		AccountID: record.ID.String(), Username: record.Username,
		DisplayName: record.DisplayName, Kind: record.Kind,
		IsSuperAdmin: record.IsSuperAdmin, DisabledAt: record.DisabledAt,
		CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC(),
	}
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	if _, _, ok := s.requireKey(w, r, access.KeyAccountRead); !ok {
		return
	}
	limit, err := identityLimit(r)
	if err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	tenantID, _ := tenantFrom(w, r)
	records, err := s.cfg.Identity.ListAccounts(r.Context(), tenantID, limit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]accountView, 0, len(records))
	for _, record := range records {
		views = append(views, accountViewOf(record))
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": views})
}

type createAccountRequest struct {
	Username     string `json:"username"`
	DisplayName  string `json:"displayName"`
	Kind         string `json:"kind"`
	Password     string `json:"password"`
	IsSuperAdmin bool   `json:"isSuperAdmin"`
}

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	principal, tenantID, ok := s.requireKey(w, r, access.KeyAccountManage)
	if !ok {
		return
	}
	var body createAccountRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// The super-admin flag is reserved to the bootstrap/CLI path. Refuse rather
	// than ignore: a caller asking for it believes it succeeded.
	if body.IsSuperAdmin {
		s.cfg.Access.Denied(r.Context(), principal, access.Decision{
			Allow: false, Reason: access.ReasonMissingGrant, Key: access.KeyAccountManage,
			Detail: "is_super_admin is reserved for bootstrap/CLI",
		}, access.Resource{TenantID: tenantID, Type: "account"})
		writeIdentityError(w, http.StatusForbidden, "super_admin_reserved",
			"is_super_admin is reserved for the bootstrap/CLI path and cannot be set through the account API")
		return
	}
	kind := strings.TrimSpace(body.Kind)
	if kind == "" {
		kind = string(botteam.AccountKindHuman)
	}
	record := store.AccountRecord{
		TenantID: tenantID, ID: uuid.New(),
		Username: strings.TrimSpace(body.Username), DisplayName: strings.TrimSpace(body.DisplayName),
		Kind: kind,
	}
	if record.DisplayName == "" {
		record.DisplayName = record.Username
	}
	domain := botteam.Account{
		ID: record.ID, Username: record.Username, DisplayName: record.DisplayName,
		Kind: botteam.AccountKind(kind),
	}
	if err := domain.Validate(); err != nil {
		writeValidationError(w, err)
		return
	}
	password := body.Password
	if kind == string(botteam.AccountKindHuman) {
		if password == "" {
			writeIdentityError(w, http.StatusBadRequest, "invalid_request", "a human account requires a password")
			return
		}
	}
	if kind == string(botteam.AccountKindBot) && password != "" {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", "a bot account uses a bot-bound API key, not a password")
		return
	}
	if password != "" {
		hash, err := auth.HashPassword(password)
		if err != nil {
			writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		record.PasswordHash = hash
	}
	created, err := s.cfg.Identity.CreateAccountAudited(r.Context(), record, principal.AccountID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if principal.IsSuperAdmin {
		if err := s.cfg.Access.AuditSuperAdmin(r.Context(), principal, "account.create", map[string]any{
			"target_account_id": created.ID.String(), "kind": created.Kind,
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"account": accountViewOf(created)})
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	if _, _, ok := s.requireKey(w, r, access.KeyAccountRead); !ok {
		return
	}
	accountID, ok := pathUUID(w, r, "id", "invalid_account_id")
	if !ok {
		return
	}
	tenantID, _ := tenantFrom(w, r)
	record, err := s.cfg.Identity.GetAccount(r.Context(), tenantID, accountID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	groups, err := s.cfg.Identity.ListPermissionGroupsForAccount(r.Context(), tenantID, accountID, maxIdentityPageLimit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	groupViews := make([]map[string]any, 0, len(groups))
	for _, group := range groups {
		groupViews = append(groupViews, map[string]any{
			"groupId": group.ID.String(), "name": group.Name, "builtin": group.Builtin,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account": accountViewOf(record),
		"groups":  groupViews,
	})
}

type accountStateRequest struct {
	Disabled *bool  `json:"disabled"`
	Reason   string `json:"reason"`
}

func (s *Server) setAccountDisabled(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	principal, tenantID, ok := s.requireKey(w, r, access.KeyAccountManage)
	if !ok {
		return
	}
	accountID, ok := pathUUID(w, r, "id", "invalid_account_id")
	if !ok {
		return
	}
	var body accountStateRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if body.Disabled == nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", "disabled is required")
		return
	}
	record, err := s.cfg.Identity.DisableAccountAudited(r.Context(), tenantID, accountID,
		principal.AccountID, *body.Disabled, strings.TrimSpace(body.Reason))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if principal.IsSuperAdmin {
		if err := s.cfg.Access.AuditSuperAdmin(r.Context(), principal, "account.disable", map[string]any{
			"target_account_id": accountID.String(), "disabled": *body.Disabled,
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": accountViewOf(record)})
}

type passwordResetRequest struct {
	Password string `json:"password"`
}

func (s *Server) resetAccountPassword(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	principal, tenantID, ok := s.requireKey(w, r, access.KeyAccountCredentialReset)
	if !ok {
		return
	}
	accountID, ok := pathUUID(w, r, "id", "invalid_account_id")
	if !ok {
		return
	}
	var body passwordResetRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if body.Password == "" {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", "password is required")
		return
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	target, err := s.cfg.Identity.GetAccount(r.Context(), tenantID, accountID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// A super admin credential is deployment-level: an ordinary administrator
	// (even a tenant admin) must not reset it and take the account over. The
	// local CLI stays the recovery path.
	if target.IsSuperAdmin && !principal.IsSuperAdmin {
		s.cfg.Access.Denied(r.Context(), principal, access.Decision{
			Allow: false, Reason: access.ReasonMissingGrant, Key: access.KeyAccountCredentialReset,
			Detail: "only a super admin can reset a super admin credential",
		}, access.Resource{TenantID: tenantID, Type: "account", ID: accountID.String()})
		writeIdentityError(w, http.StatusForbidden, "super_admin_protected",
			"a super admin credential can only be reset by a super admin (or the local CLI)")
		return
	}
	// The reset is the CLI reset semantics over HTTP: a password.reset event (not
	// password.changed) revokes every live session and makes the next login
	// require a password change.
	if _, err := s.cfg.Identity.ResetAccountPasswordByID(r.Context(), tenantID, accountID, hash, principal.AccountID); err != nil {
		writeStoreError(w, err)
		return
	}
	if principal.IsSuperAdmin {
		if err := s.cfg.Access.AuditSuperAdmin(r.Context(), principal, "account.password_reset", map[string]any{
			"target_account_id": accountID.String(),
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accountId": accountID.String(), "passwordChanged": true,
		"mustChangePassword": true, "sessionsRevoked": true,
	})
}

func (s *Server) listAccountGroups(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	if _, _, ok := s.requireKey(w, r, access.KeyAccountRead); !ok {
		return
	}
	accountID, ok := pathUUID(w, r, "id", "invalid_account_id")
	if !ok {
		return
	}
	tenantID, _ := tenantFrom(w, r)
	if _, err := s.cfg.Identity.GetAccount(r.Context(), tenantID, accountID); err != nil {
		writeStoreError(w, err)
		return
	}
	groups, err := s.cfg.Identity.ListPermissionGroupsForAccount(r.Context(), tenantID, accountID, maxIdentityPageLimit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]map[string]any, 0, len(groups))
	for _, group := range groups {
		views = append(views, map[string]any{
			"groupId": group.ID.String(), "name": group.Name, "builtin": group.Builtin,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"accountId": accountID.String(), "groups": views})
}

type accountGroupRequest struct {
	GroupID string `json:"groupId"`
}

func (s *Server) addAccountGroup(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	principal, tenantID, ok := s.requireKey(w, r, access.KeyAccountPermissionGrant)
	if !ok {
		return
	}
	accountID, ok := pathUUID(w, r, "id", "invalid_account_id")
	if !ok {
		return
	}
	var body accountGroupRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	groupID, err := uuid.Parse(strings.TrimSpace(body.GroupID))
	if err != nil || groupID == uuid.Nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_group_id", "groupId must be a UUID")
		return
	}
	if _, err := s.cfg.Identity.GetAccount(r.Context(), tenantID, accountID); err != nil {
		writeStoreError(w, err)
		return
	}
	group, err := s.cfg.Identity.GetPermissionGroup(r.Context(), tenantID, groupID)
	if err != nil {
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
	if err := s.cfg.Access.AuditGrant(r.Context(), tenantID, principal.AccountID, accountID,
		"grant", "group_membership", "", "", "ok", map[string]any{
			"group_id": groupID.String(), "group": group.Name, "created": created,
		}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accountId": accountID.String(), "groupId": groupID.String(), "added": created,
	})
}

func (s *Server) removeAccountGroup(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	principal, tenantID, ok := s.requireKey(w, r, access.KeyAccountPermissionGrant)
	if !ok {
		return
	}
	accountID, ok := pathUUID(w, r, "id", "invalid_account_id")
	if !ok {
		return
	}
	groupID, ok := pathUUID(w, r, "groupId", "invalid_group_id")
	if !ok {
		return
	}
	if err := s.cfg.Identity.RemovePermissionGroupMember(r.Context(), tenantID, groupID, accountID); err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.cfg.Access.AuditGrant(r.Context(), tenantID, principal.AccountID, accountID,
		"revoke", "group_membership", "", "", "ok", map[string]any{"group_id": groupID.String()}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accountId": accountID.String(), "groupId": groupID.String(), "removed": true,
	})
}

// checkGroupGrantable enforces grant ⊆ own for every grant a group carries
// before an account is put in it. A group with no grants is harmless.
func (s *Server) checkGroupGrantable(w http.ResponseWriter, r *http.Request, principal access.Principal, tenantID uuid.UUID, group store.PermissionGroupRecord) error {
	records, err := s.cfg.Identity.ListPermissionGroupGrants(r.Context(), tenantID, group.ID, maxIdentityPageLimit)
	if err != nil {
		writeStoreError(w, err)
		return err
	}
	proposed := make([]access.Grant, 0, len(records))
	for _, record := range records {
		key := strings.TrimSpace(record.PermissionKey)
		scope, err := access.ParseScope(record.Scope)
		if err != nil {
			writeIdentityError(w, http.StatusInternalServerError, "invalid_grant", err.Error())
			return err
		}
		proposed = append(proposed, access.Grant{Key: key, Scope: scope})
	}
	if err := s.cfg.Access.CanGrant(r.Context(), principal, proposed); err != nil {
		var rejection *access.GrantRejection
		reason := access.ReasonMissingGrant
		key := ""
		if errors.As(err, &rejection) {
			reason = rejection.Reason
			key = rejection.Key
		}
		s.cfg.Access.Denied(r.Context(), principal, access.Decision{
			Allow: false, Reason: reason, Key: key, Detail: err.Error(),
		}, access.Resource{TenantID: tenantID, Type: "permission_group", ID: group.ID.String()})
		body := map[string]any{
			"error":  "forbidden",
			"code":   "grant_exceeds_own",
			"reason": reason.String(),
		}
		if key != "" {
			body["key"] = key
		}
		writeJSON(w, http.StatusForbidden, body)
		return err
	}
	return nil
}

// writeValidationError renders botteam's field-level failures as a 400 with the
// stable codes, so a client can fix every field in one round.
func writeValidationError(w http.ResponseWriter, err error) {
	fields := botteam.FieldErrors(err)
	if fields == nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error":  err.Error(),
		"code":   "invalid_request",
		"fields": fields,
	})
}
