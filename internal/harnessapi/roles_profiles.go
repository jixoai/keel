// Role and Profile HTTP surface (change keel-bot-identity-access, A4.2b2).
//
// Role is identity and soul only: it never grants a permission. Profile is the
// Account + Role binding a Bot runs under. Both are tenant-level resources, so
// the route key is tenant-scoped (role.read/manage, profile.read/manage) and
// cross-tenant ids answer 404 rather than leaking existence.
//
// A Profile rebind is not a plain column write: a Bot's owner relation is the
// Profile's Account, so changing the account moves every referencing Bot's
// owner tuple in the same transaction (store.UpdateProfileWithOwner).
package harnessapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/store"
)

type roleView struct {
	RoleID      string    `json:"roleId"`
	Name        string    `json:"name"`
	Instruction string    `json:"instruction,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func roleViewOf(record store.RoleRecord) roleView {
	return roleView{
		RoleID: record.ID.String(), Name: record.Name, Instruction: record.Instruction,
		CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC(),
	}
}

type profileView struct {
	ProfileID string    `json:"profileId"`
	AccountID string    `json:"accountId"`
	RoleID    string    `json:"roleId"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func profileViewOf(record store.ProfileRecord) profileView {
	return profileView{
		ProfileID: record.ID.String(), AccountID: record.AccountID.String(),
		RoleID:    record.RoleID.String(),
		CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC(),
	}
}

// --- roles ------------------------------------------------------------------

func (s *Server) listRoles(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	if _, _, ok := s.requireKey(w, r, access.KeyRoleRead); !ok {
		return
	}
	limit, err := identityLimit(r)
	if err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	tenantID, _ := tenantFrom(w, r)
	records, err := s.cfg.Identity.ListRoles(r.Context(), tenantID, limit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]roleView, 0, len(records))
	for _, record := range records {
		views = append(views, roleViewOf(record))
	}
	writeJSON(w, http.StatusOK, map[string]any{"roles": views})
}

type roleCreateRequest struct {
	Name        string `json:"name"`
	Instruction string `json:"instruction"`
}

func (s *Server) createRole(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	_, tenantID, ok := s.requireKey(w, r, access.KeyRoleManage)
	if !ok {
		return
	}
	var body roleCreateRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", "name is required")
		return
	}
	record, err := s.cfg.Identity.CreateRole(r.Context(), store.RoleRecord{
		TenantID: tenantID, ID: uuid.New(), Name: name,
		Instruction: strings.TrimSpace(body.Instruction),
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	s.identityChanged(tenantID, IdentityEventPermissions)
	writeJSON(w, http.StatusCreated, map[string]any{"role": roleViewOf(record)})
}

func (s *Server) getRole(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	if _, _, ok := s.requireKey(w, r, access.KeyRoleRead); !ok {
		return
	}
	roleID, ok := pathUUID(w, r, "id", "invalid_role_id")
	if !ok {
		return
	}
	tenantID, _ := tenantFrom(w, r)
	record, err := s.cfg.Identity.GetRole(r.Context(), tenantID, roleID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"role": roleViewOf(record)})
}

type roleUpdateRequest struct {
	Name        *string `json:"name"`
	Instruction *string `json:"instruction"`
}

func (s *Server) updateRole(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	_, tenantID, ok := s.requireKey(w, r, access.KeyRoleManage)
	if !ok {
		return
	}
	roleID, ok := pathUUID(w, r, "id", "invalid_role_id")
	if !ok {
		return
	}
	var body roleUpdateRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if body.Name == nil && body.Instruction == nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", "at least one of name or instruction is required")
		return
	}
	// Read first so a cross-tenant id answers 404 and the unchanged field is the
	// stored value, not a zero value invented here.
	current, err := s.cfg.Identity.GetRole(r.Context(), tenantID, roleID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if name == "" {
			writeIdentityError(w, http.StatusBadRequest, "invalid_request", "name cannot be empty")
			return
		}
		current.Name = name
	}
	if body.Instruction != nil {
		current.Instruction = strings.TrimSpace(*body.Instruction)
	}
	updated, err := s.cfg.Identity.UpdateRole(r.Context(), current)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	s.identityChanged(tenantID, IdentityEventPermissions, IdentityEventBots)
	writeJSON(w, http.StatusOK, map[string]any{"role": roleViewOf(updated)})
}

func (s *Server) deleteRole(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	_, tenantID, ok := s.requireKey(w, r, access.KeyRoleManage)
	if !ok {
		return
	}
	roleID, ok := pathUUID(w, r, "id", "invalid_role_id")
	if !ok {
		return
	}
	if err := s.cfg.Identity.DeleteRole(r.Context(), tenantID, roleID); err != nil {
		writeStoreError(w, err)
		return
	}
	s.identityChanged(tenantID, IdentityEventPermissions, IdentityEventBots)
	writeJSON(w, http.StatusOK, map[string]any{"roleId": roleID.String(), "deleted": true})
}

// --- profiles ---------------------------------------------------------------

func (s *Server) listProfiles(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	if _, _, ok := s.requireKey(w, r, access.KeyProfileRead); !ok {
		return
	}
	limit, err := identityLimit(r)
	if err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	tenantID, _ := tenantFrom(w, r)
	records, err := s.cfg.Identity.ListProfiles(r.Context(), tenantID, limit)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	views := make([]profileView, 0, len(records))
	for _, record := range records {
		views = append(views, profileViewOf(record))
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": views})
}

type profileCreateRequest struct {
	AccountID string `json:"accountId"`
	RoleID    string `json:"roleId"`
}

func (s *Server) createProfile(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	_, tenantID, ok := s.requireKey(w, r, access.KeyProfileManage)
	if !ok {
		return
	}
	var body profileCreateRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	accountID, ok := parseProfileAccountID(w, body.AccountID)
	if !ok {
		return
	}
	roleID, ok := parseProfileRoleID(w, body.RoleID)
	if !ok {
		return
	}
	if !s.profileReferencesExist(w, r, tenantID, accountID, roleID) {
		return
	}
	record, err := s.cfg.Identity.CreateProfile(r.Context(), store.ProfileRecord{
		TenantID: tenantID, ID: uuid.New(), AccountID: accountID, RoleID: roleID,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	s.identityChanged(tenantID, IdentityEventPermissions, IdentityEventBots)
	writeJSON(w, http.StatusCreated, map[string]any{"profile": profileViewOf(record)})
}

func (s *Server) getProfile(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	if _, _, ok := s.requireKey(w, r, access.KeyProfileRead); !ok {
		return
	}
	profileID, ok := pathUUID(w, r, "id", "invalid_profile_id")
	if !ok {
		return
	}
	tenantID, _ := tenantFrom(w, r)
	record, err := s.cfg.Identity.GetProfile(r.Context(), tenantID, profileID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"profile": profileViewOf(record)})
}

type profileUpdateRequest struct {
	AccountID *string `json:"accountId"`
	RoleID    *string `json:"roleId"`
}

func (s *Server) updateProfile(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	principal, tenantID, ok := s.requireKey(w, r, access.KeyProfileManage)
	if !ok {
		return
	}
	profileID, ok := pathUUID(w, r, "id", "invalid_profile_id")
	if !ok {
		return
	}
	var body profileUpdateRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if body.AccountID == nil && body.RoleID == nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", "at least one of accountId or roleId is required")
		return
	}
	current, err := s.cfg.Identity.GetProfile(r.Context(), tenantID, profileID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	oldAccountID := current.AccountID
	accountChanged := false
	if body.AccountID != nil {
		accountID, ok := parseProfileAccountID(w, *body.AccountID)
		if !ok {
			return
		}
		current.AccountID = accountID
		accountChanged = accountID != oldAccountID
	}
	if body.RoleID != nil {
		roleID, ok := parseProfileRoleID(w, *body.RoleID)
		if !ok {
			return
		}
		current.RoleID = roleID
	}
	if !s.profileReferencesExist(w, r, tenantID, current.AccountID, current.RoleID) {
		return
	}
	var updated store.ProfileRecord
	if accountChanged {
		updated, err = s.cfg.Identity.UpdateProfileWithOwner(r.Context(), current, oldAccountID, principal.AccountID)
	} else {
		updated, err = s.cfg.Identity.UpdateProfile(r.Context(), current)
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	s.identityChanged(tenantID, IdentityEventPermissions, IdentityEventBots)
	writeJSON(w, http.StatusOK, map[string]any{"profile": profileViewOf(updated)})
}

func (s *Server) deleteProfile(w http.ResponseWriter, r *http.Request) {
	if !s.identityReady(w) {
		return
	}
	_, tenantID, ok := s.requireKey(w, r, access.KeyProfileManage)
	if !ok {
		return
	}
	profileID, ok := pathUUID(w, r, "id", "invalid_profile_id")
	if !ok {
		return
	}
	if err := s.cfg.Identity.DeleteProfile(r.Context(), tenantID, profileID); err != nil {
		writeStoreError(w, err)
		return
	}
	s.identityChanged(tenantID, IdentityEventPermissions, IdentityEventBots)
	writeJSON(w, http.StatusOK, map[string]any{"profileId": profileID.String(), "deleted": true})
}

// --- shared validation ------------------------------------------------------

func parseProfileAccountID(w http.ResponseWriter, raw string) (uuid.UUID, bool) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil || id == uuid.Nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_account_id", "accountId must be a UUID")
		return uuid.Nil, false
	}
	return id, true
}

func parseProfileRoleID(w http.ResponseWriter, raw string) (uuid.UUID, bool) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil || id == uuid.Nil {
		writeIdentityError(w, http.StatusBadRequest, "invalid_role_id", "roleId must be a UUID")
		return uuid.Nil, false
	}
	return id, true
}

// profileReferencesExist turns a missing (or foreign-tenant) account/role into a
// field-level 400: a profile may only bind rows of its own tenant, and the
// composite FK alone would report that as a generic conflict.
func (s *Server) profileReferencesExist(w http.ResponseWriter, r *http.Request, tenantID, accountID, roleID uuid.UUID) bool {
	if _, err := s.cfg.Identity.GetAccount(r.Context(), tenantID, accountID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeIdentityError(w, http.StatusBadRequest, "invalid_account_id", "accountId does not exist in this tenant")
			return false
		}
		writeStoreError(w, err)
		return false
	}
	if _, err := s.cfg.Identity.GetRole(r.Context(), tenantID, roleID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeIdentityError(w, http.StatusBadRequest, "invalid_role_id", "roleId does not exist in this tenant")
			return false
		}
		writeStoreError(w, err)
		return false
	}
	return true
}
