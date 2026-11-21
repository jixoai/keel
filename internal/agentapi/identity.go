// Identity and access HTTP surface (change keel-bot-identity-access, A4.1).
//
// This file is the shared plumbing of /v1/accounts, /v1/roles, /v1/profiles,
// /v1/permission-groups, /v1/bots and /v1/team-bots: the authorization seam,
// the structured 403 body, store-error mapping and the endpoint catalog the
// permission-coverage gate enumerates.
//
// Two rules drive the shape:
//
//   - fail closed: a deployment without the access seam answers 503 on every
//     route here; a list endpoint never falls back to "return everything".
//   - no existence leak: the key + relation check runs before the row is read,
//     so a caller without authority sees one 403 regardless of whether the
//     resource exists. A caller with authority sees the row or an honest 404.
package agentapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/store"
)

// Access is the authorization seam. *accesswire.Bridge implements it; a test
// supplies its own. Nil makes every identity route fail closed (503).
type Access interface {
	// Principal resolves the edge-authenticated identity for this request.
	Principal(ctx context.Context, tenantID uuid.UUID) (access.Principal, error)
	// Authorize decides one resource-scoped or tenant-level action and audits a
	// denial.
	Authorize(ctx context.Context, principal access.Principal, key string, resource access.Resource) (access.Decision, error)
	// Visible filters a bounded candidate set to the ids the principal may list.
	Visible(ctx context.Context, principal access.Principal, key, resourceType string, candidateIDs []string) (access.VisibleResult, error)
	// Effective is the caller's materialized permission set (whoami).
	Effective(ctx context.Context, principal access.Principal) (access.Grants, error)
	// CanGrant enforces grant ⊆ own for a proposed tuple set.
	CanGrant(ctx context.Context, principal access.Principal, proposed []access.Grant) error
	// BotOwnerAccount returns a Bot's owner account and archived flag.
	BotOwnerAccount(ctx context.Context, tenantID uuid.UUID, botID string) (uuid.UUID, bool, error)
	// Audit methods: grant/revoke, relation change, super-admin action, invoke,
	// bot-key lifecycle and handler-decided denial. The writes return an error:
	// a business write without its audit row is reported to the caller (500),
	// never silently logged as a warning (A4.2a §7).
	AuditGrant(ctx context.Context, tenantID, actor, target uuid.UUID, action, via, key, scope, result string, extra map[string]any) error
	AuditRelation(ctx context.Context, tenantID, actor uuid.UUID, action, resourceType, resourceID, relation, subjectType, subjectID string) error
	AuditSuperAdmin(ctx context.Context, principal access.Principal, action string, detail map[string]any) error
	AuditInvoke(ctx context.Context, tenantID uuid.UUID, caller access.Principal, botID string, sessionID uuid.UUID, result string) error
	AuditBotKey(ctx context.Context, tenantID, actor, accountID uuid.UUID, botID, action, keyID, prefix, label string) error
	Denied(ctx context.Context, principal access.Principal, decision access.Decision, resource access.Resource)
}

// BotSessionRequest is one bot-initiated session start. The serve layer owns
// the turn starter and the ownership write; the HTTP layer never touches the
// kernel directly.
type BotSessionRequest struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	BotID     string
	// AccountID is the Bot's owner account (from its Profile).
	AccountID uuid.UUID
	// ActorAccountID is the caller (human, bot or trigger).
	ActorAccountID uuid.UUID
	TeamBotID      string
	Mission        string
	Text           string
	RequestID      string
}

// BotSessionResult reports where the turn landed.
type BotSessionResult struct {
	SessionID uuid.UUID
	Seq       int64
	RequestID string
	Deduped   bool
}

// identityLimits bound every identity listing and candidate scan.
const (
	defaultIdentityPageLimit = 100
	maxIdentityPageLimit     = 500
)

// identityReady reports whether the identity surface is wired. A missing seam
// is a deployment configuration gap and answers 503, never "allow everything".
func (s *Server) identityReady(w http.ResponseWriter) bool {
	if s.cfg.Identity == nil || s.cfg.Access == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("the identity surface is not wired in this deployment"))
		return false
	}
	return true
}

// requireKey authorizes a tenant-level action (accounts, groups, bot create).
// The resource carries the tenant so scope "all" is still required to cross a
// tenant boundary. A relation-scoped key is delegated to requireHeldKey: a
// caller mistake must not turn a resource key into a relation-free allow.
func (s *Server) requireKey(w http.ResponseWriter, r *http.Request, key string) (access.Principal, uuid.UUID, bool) {
	spec, known := access.Lookup(key)
	if !known {
		// An unknown key is a code bug in this handler, not a client denial: the
		// route cannot name authority the registry does not have.
		writeErr(w, http.StatusInternalServerError,
			fmt.Errorf("internal error: requireKey called with unregistered key %q", key))
		return access.Principal{}, uuid.Nil, false
	}
	if spec.RequiresRelation() {
		// A resource-scoped key cannot be authorized without a resource. Calling
		// requireKey here would silently degrade it to "any held scope", which is
		// exactly the relation bypass A4.2a closes: force the explicit call.
		writeErr(w, http.StatusInternalServerError,
			fmt.Errorf("internal error: requireKey called with resource-scoped key %q; use requireResource or requireHeldKey", key))
		return access.Principal{}, uuid.Nil, false
	}
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return access.Principal{}, uuid.Nil, false
	}
	principal, err := s.cfg.Access.Principal(r.Context(), tenantID)
	if err != nil {
		writeIdentityError(w, http.StatusUnauthorized, "invalid_session", "authentication required")
		return access.Principal{}, uuid.Nil, false
	}
	decision, err := s.cfg.Access.Authorize(r.Context(), principal, key,
		access.Resource{TenantID: tenantID})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return access.Principal{}, uuid.Nil, false
	}
	if decision.Denied() {
		writeDenied(w, decision)
		return access.Principal{}, uuid.Nil, false
	}
	return principal, tenantID, true
}

// requireResource authorizes one resource action. It runs before the row is
// read, so a missing row and a forbidden row answer identically to a caller
// without authority.
func (s *Server) requireResource(w http.ResponseWriter, r *http.Request, key, resourceType, resourceID string) (access.Principal, uuid.UUID, bool) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return access.Principal{}, uuid.Nil, false
	}
	principal, err := s.cfg.Access.Principal(r.Context(), tenantID)
	if err != nil {
		writeIdentityError(w, http.StatusUnauthorized, "invalid_session", "authentication required")
		return access.Principal{}, uuid.Nil, false
	}
	decision, err := s.cfg.Access.Authorize(r.Context(), principal, key, access.Resource{
		TenantID: tenantID, Type: resourceType, ID: resourceID,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return access.Principal{}, uuid.Nil, false
	}
	if decision.Denied() {
		writeDenied(w, decision)
		return access.Principal{}, uuid.Nil, false
	}
	return principal, tenantID, true
}

// authorizeSessionResource applies a session-scoped key to one session. It is
// the handler-level companion of the route matrix for endpoints whose resource
// id is not in the path (a notification's row, a sub-agent record, a query
// parameter). It returns false after writing the refusal.
func (s *Server) authorizeSessionResource(w http.ResponseWriter, r *http.Request, tenantID, sessionID uuid.UUID, key string) bool {
	if s.cfg.Access == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("the identity surface is not wired in this deployment"))
		return false
	}
	principal, err := s.cfg.Access.Principal(r.Context(), tenantID)
	if err != nil {
		writeIdentityError(w, http.StatusUnauthorized, "invalid_session", "authentication required")
		return false
	}
	decision, err := s.cfg.Access.Authorize(r.Context(), principal, key, access.Resource{
		TenantID: tenantID, Type: "session", ID: sessionID.String(),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return false
	}
	if decision.Denied() {
		writeDenied(w, decision)
		return false
	}
	return true
}

// visibleSessionIDs filters a bounded candidate set through the engine's
// relation filter and writes a 403 if the key-level gate itself is denied.
func (s *Server) visibleSessionIDs(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, key string, candidates []string) (map[string]bool, bool) {
	if s.cfg.Access == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("the identity surface is not wired in this deployment"))
		return nil, false
	}
	principal, err := s.cfg.Access.Principal(r.Context(), tenantID)
	if err != nil {
		writeIdentityError(w, http.StatusUnauthorized, "invalid_session", "authentication required")
		return nil, false
	}
	result, err := s.cfg.Access.Visible(r.Context(), principal, key, "session", candidates)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return nil, false
	}
	if !result.Allowed() {
		writeDenied(w, result.Decision)
		return nil, false
	}
	allowed := make(map[string]bool, len(result.IDs))
	for _, id := range result.IDs {
		allowed[id] = true
	}
	return allowed, true
}

// requireHeldKey authorizes creating a new resource of a relation-scoped type.
// There is no resource row (and therefore no relation) yet, so the gate is the
// key itself at any held scope; the owner relation written after the create is
// what future single-resource decisions use. A deployment super admin holds
// every key and passes.
func (s *Server) requireHeldKey(w http.ResponseWriter, r *http.Request, key string) (access.Principal, uuid.UUID, bool) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return access.Principal{}, uuid.Nil, false
	}
	principal, err := s.cfg.Access.Principal(r.Context(), tenantID)
	if err != nil {
		writeIdentityError(w, http.StatusUnauthorized, "invalid_session", "authentication required")
		return access.Principal{}, uuid.Nil, false
	}
	effective, err := s.cfg.Access.Effective(r.Context(), principal)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return access.Principal{}, uuid.Nil, false
	}
	if _, held := effective.ScopeOf(key); !held {
		decision := access.Decision{
			Allow: false, Reason: access.ReasonMissingGrant, Key: key,
			Detail: "the principal does not hold the create key",
		}
		s.cfg.Access.Denied(r.Context(), principal, decision, access.Resource{TenantID: tenantID})
		writeDenied(w, decision)
		return access.Principal{}, uuid.Nil, false
	}
	return principal, tenantID, true
}

// writeDenied renders the structured 403. It names what is missing (key and
// stable reason) and never whether the resource exists.
func writeDenied(w http.ResponseWriter, decision access.Decision) {
	body := map[string]any{
		"error":       "forbidden",
		"code":        "forbidden",
		"reason":      decision.Reason.String(),
		"requiredKey": decision.Key,
	}
	if decision.Scope != "" {
		body["scope"] = string(decision.Scope)
	}
	writeJSON(w, http.StatusForbidden, body)
}

// writeIdentityError writes a code-carrying error body.
func writeIdentityError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": message, "code": code})
}

// writeStoreError maps the identity store's sentinels to HTTP statuses.
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrSuperAdminProtected):
		writeIdentityError(w, http.StatusConflict, "super_admin_protected", "the last super admin cannot be disabled")
	case errors.Is(err, store.ErrNotFound):
		writeIdentityError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, store.ErrConflict):
		writeIdentityError(w, http.StatusConflict, "conflict", "the write conflicts with stored state")
	case errors.Is(err, store.ErrInvalid):
		writeIdentityError(w, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

// identityLimit parses ?limit= with the identity bounds.
func identityLimit(r *http.Request) (int, error) {
	return queryInt(r, "limit", defaultIdentityPageLimit, 1, maxIdentityPageLimit)
}

// identityFlag parses a boolean query parameter ("1"/"true").
func identityFlag(r *http.Request, name string) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// pathUUID parses a uuid path value.
func pathUUID(w http.ResponseWriter, r *http.Request, name, code string) (uuid.UUID, bool) {
	id, err := uuid.Parse(strings.TrimSpace(r.PathValue(name)))
	if err != nil || id == uuid.Nil {
		writeIdentityError(w, http.StatusBadRequest, code, name+" must be a UUID")
		return uuid.Nil, false
	}
	return id, true
}

// identityEndpointCatalog is the declarative key map of this file's routes. The
// coverage test enumerates it; the routes themselves call requireKey /
// requireResource with the same constants. whoami is intentionally absent: it
// returns only the caller's own identity and grants no resource access, so it
// has no permission key.
var identityEndpointCatalog = []access.Endpoint{
	{Kind: access.EndpointHTTP, Name: "GET /v1/accounts", RequiredKey: access.KeyAccountRead},
	{Kind: access.EndpointHTTP, Name: "POST /v1/accounts", RequiredKey: access.KeyAccountManage},
	{Kind: access.EndpointHTTP, Name: "GET /v1/accounts/{id}", RequiredKey: access.KeyAccountRead},
	{Kind: access.EndpointHTTP, Name: "POST /v1/accounts/{id}/disable", RequiredKey: access.KeyAccountManage},
	{Kind: access.EndpointHTTP, Name: "POST /v1/accounts/{id}/password-reset", RequiredKey: access.KeyAccountCredentialReset},
	{Kind: access.EndpointHTTP, Name: "GET /v1/accounts/{id}/groups", RequiredKey: access.KeyAccountRead},
	{Kind: access.EndpointHTTP, Name: "POST /v1/accounts/{id}/groups", RequiredKey: access.KeyAccountPermissionGrant},
	{Kind: access.EndpointHTTP, Name: "DELETE /v1/accounts/{id}/groups/{groupId}", RequiredKey: access.KeyAccountPermissionGrant},

	{Kind: access.EndpointHTTP, Name: "GET /v1/roles", RequiredKey: access.KeyRoleRead},
	{Kind: access.EndpointHTTP, Name: "POST /v1/roles", RequiredKey: access.KeyRoleManage},
	{Kind: access.EndpointHTTP, Name: "GET /v1/roles/{id}", RequiredKey: access.KeyRoleRead},
	{Kind: access.EndpointHTTP, Name: "PATCH /v1/roles/{id}", RequiredKey: access.KeyRoleManage},
	{Kind: access.EndpointHTTP, Name: "DELETE /v1/roles/{id}", RequiredKey: access.KeyRoleManage},

	{Kind: access.EndpointHTTP, Name: "GET /v1/profiles", RequiredKey: access.KeyProfileRead},
	{Kind: access.EndpointHTTP, Name: "POST /v1/profiles", RequiredKey: access.KeyProfileManage},
	{Kind: access.EndpointHTTP, Name: "GET /v1/profiles/{id}", RequiredKey: access.KeyProfileRead},
	{Kind: access.EndpointHTTP, Name: "PATCH /v1/profiles/{id}", RequiredKey: access.KeyProfileManage},
	{Kind: access.EndpointHTTP, Name: "DELETE /v1/profiles/{id}", RequiredKey: access.KeyProfileManage},

	{Kind: access.EndpointHTTP, Name: "GET /v1/permission-groups", RequiredKey: access.KeyPermissionRead},
	{Kind: access.EndpointHTTP, Name: "POST /v1/permission-groups", RequiredKey: access.KeyPermissionManage},
	{Kind: access.EndpointHTTP, Name: "GET /v1/permission-groups/{id}", RequiredKey: access.KeyPermissionRead},
	{Kind: access.EndpointHTTP, Name: "PATCH /v1/permission-groups/{id}", RequiredKey: access.KeyPermissionManage},
	{Kind: access.EndpointHTTP, Name: "DELETE /v1/permission-groups/{id}", RequiredKey: access.KeyPermissionManage},
	{Kind: access.EndpointHTTP, Name: "POST /v1/permission-groups/{id}/members", RequiredKey: access.KeyAccountPermissionGrant},
	{Kind: access.EndpointHTTP, Name: "DELETE /v1/permission-groups/{id}/members/{accountId}", RequiredKey: access.KeyAccountPermissionGrant},
	{Kind: access.EndpointHTTP, Name: "POST /v1/permission-groups/{id}/grants", RequiredKey: access.KeyPermissionManage},
	{Kind: access.EndpointHTTP, Name: "DELETE /v1/permission-groups/{id}/grants/{key}", RequiredKey: access.KeyPermissionManage},

	{Kind: access.EndpointHTTP, Name: "GET /v1/bots", RequiredKey: access.KeyBotRead},
	{Kind: access.EndpointHTTP, Name: "POST /v1/bots", RequiredKey: access.KeyBotCreate},
	{Kind: access.EndpointHTTP, Name: "GET /v1/bots/{id}", RequiredKey: access.KeyBotRead},
	{Kind: access.EndpointHTTP, Name: "PATCH /v1/bots/{id}", RequiredKey: access.KeyBotManage},
	{Kind: access.EndpointHTTP, Name: "POST /v1/bots/{id}/archive", RequiredKey: access.KeyBotManage},
	{Kind: access.EndpointHTTP, Name: "POST /v1/bots/{id}/invoke", RequiredKey: access.KeyBotInvoke},
	{Kind: access.EndpointHTTP, Name: "GET /v1/bots/{id}/keys", RequiredKey: access.KeyBotManage},
	{Kind: access.EndpointHTTP, Name: "POST /v1/bots/{id}/keys", RequiredKey: access.KeyBotManage},
	{Kind: access.EndpointHTTP, Name: "DELETE /v1/bots/{id}/keys/{keyId}", RequiredKey: access.KeyBotManage},

	{Kind: access.EndpointHTTP, Name: "GET /v1/team-bots", RequiredKey: access.KeyTeamBotRead},
	{Kind: access.EndpointHTTP, Name: "POST /v1/team-bots", RequiredKey: access.KeyTeamBotManage},
	{Kind: access.EndpointHTTP, Name: "GET /v1/team-bots/{id}", RequiredKey: access.KeyTeamBotRead},
	{Kind: access.EndpointHTTP, Name: "PATCH /v1/team-bots/{id}", RequiredKey: access.KeyTeamBotManage},
	{Kind: access.EndpointHTTP, Name: "POST /v1/team-bots/{id}/archive", RequiredKey: access.KeyTeamBotManage},
	{Kind: access.EndpointHTTP, Name: "POST /v1/team-bots/{id}/roster", RequiredKey: access.KeyTeamBotManage},
	{Kind: access.EndpointHTTP, Name: "POST /v1/team-bots/{id}/start", RequiredKey: access.KeyTeamBotManage},
}

// IdentityEndpoints returns a copy of the declarative key map: the coverage
// gate's input and the "route declared its key" proof.
func IdentityEndpoints() []access.Endpoint {
	out := make([]access.Endpoint, len(identityEndpointCatalog))
	copy(out, identityEndpointCatalog)
	return out
}

// identityRoutes mounts the identity surface.
func (s *Server) identityRoutes() {
	s.mux.HandleFunc("GET /v1/accounts", s.listAccounts)
	s.mux.HandleFunc("POST /v1/accounts", s.createAccount)
	s.mux.HandleFunc("GET /v1/accounts/{id}", s.getAccount)
	s.mux.HandleFunc("POST /v1/accounts/{id}/disable", s.setAccountDisabled)
	s.mux.HandleFunc("POST /v1/accounts/{id}/password-reset", s.resetAccountPassword)
	s.mux.HandleFunc("GET /v1/accounts/{id}/groups", s.listAccountGroups)
	s.mux.HandleFunc("POST /v1/accounts/{id}/groups", s.addAccountGroup)
	s.mux.HandleFunc("DELETE /v1/accounts/{id}/groups/{groupId}", s.removeAccountGroup)

	s.mux.HandleFunc("GET /v1/roles", s.listRoles)
	s.mux.HandleFunc("POST /v1/roles", s.createRole)
	s.mux.HandleFunc("GET /v1/roles/{id}", s.getRole)
	s.mux.HandleFunc("PATCH /v1/roles/{id}", s.updateRole)
	s.mux.HandleFunc("DELETE /v1/roles/{id}", s.deleteRole)

	s.mux.HandleFunc("GET /v1/profiles", s.listProfiles)
	s.mux.HandleFunc("POST /v1/profiles", s.createProfile)
	s.mux.HandleFunc("GET /v1/profiles/{id}", s.getProfile)
	s.mux.HandleFunc("PATCH /v1/profiles/{id}", s.updateProfile)
	s.mux.HandleFunc("DELETE /v1/profiles/{id}", s.deleteProfile)

	s.mux.HandleFunc("GET /v1/permission-groups", s.listPermissionGroups)
	s.mux.HandleFunc("POST /v1/permission-groups", s.createPermissionGroup)
	s.mux.HandleFunc("GET /v1/permission-groups/{id}", s.getPermissionGroup)
	s.mux.HandleFunc("PATCH /v1/permission-groups/{id}", s.renamePermissionGroup)
	s.mux.HandleFunc("DELETE /v1/permission-groups/{id}", s.deletePermissionGroup)
	s.mux.HandleFunc("POST /v1/permission-groups/{id}/members", s.addPermissionGroupMember)
	s.mux.HandleFunc("DELETE /v1/permission-groups/{id}/members/{accountId}", s.removePermissionGroupMember)
	s.mux.HandleFunc("POST /v1/permission-groups/{id}/grants", s.addPermissionGroupGrant)
	s.mux.HandleFunc("DELETE /v1/permission-groups/{id}/grants/{key}", s.removePermissionGroupGrant)

	s.mux.HandleFunc("GET /v1/bots", s.listBots)
	s.mux.HandleFunc("POST /v1/bots", s.createBot)
	s.mux.HandleFunc("GET /v1/bots/{id}", s.getBot)
	s.mux.HandleFunc("PATCH /v1/bots/{id}", s.updateBot)
	s.mux.HandleFunc("POST /v1/bots/{id}/archive", s.archiveBot)
	s.mux.HandleFunc("POST /v1/bots/{id}/invoke", s.invokeBot)
	s.mux.HandleFunc("GET /v1/bots/{id}/keys", s.listBotKeys)
	s.mux.HandleFunc("POST /v1/bots/{id}/keys", s.issueBotKey)
	s.mux.HandleFunc("DELETE /v1/bots/{id}/keys/{keyId}", s.revokeBotKey)

	s.mux.HandleFunc("GET /v1/team-bots", s.listTeamBots)
	s.mux.HandleFunc("POST /v1/team-bots", s.createTeamBot)
	s.mux.HandleFunc("GET /v1/team-bots/{id}", s.getTeamBot)
	s.mux.HandleFunc("PATCH /v1/team-bots/{id}", s.updateTeamBot)
	s.mux.HandleFunc("POST /v1/team-bots/{id}/archive", s.archiveTeamBot)
	s.mux.HandleFunc("POST /v1/team-bots/{id}/roster", s.updateTeamBotRoster)
	s.mux.HandleFunc("POST /v1/team-bots/{id}/start", s.startTeamBot)
}

// timeView renders a timestamp in UTC and omits a zero value.
func timeView(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC()
}

// grantView is one (key, scope) pair on the wire.
type grantView struct {
	Key   string `json:"key"`
	Scope string `json:"scope"`
}

func grantViews(grants []store.PermissionGroupGrantRecord) []grantView {
	out := make([]grantView, 0, len(grants))
	for _, grant := range grants {
		out = append(out, grantView{Key: grant.PermissionKey, Scope: grant.Scope})
	}
	return out
}
