// Tenant provisioning and quota surface (change keel-ga §2 tenancy-quotas).
//
// Three concerns live here:
//
//   - who am I (GET /v1/auth/whoami): the resolved tenant, account and (for a
//     bot key) Bot identity of the credential the request presented.
//   - provisioning (POST /v1/admin/tenants): the operator path that creates a
//     tenant and seeds its built-in permission groups. No tenant-wide credential
//     is minted: accounts log in with passwords and Bots use bot-bound keys.
//   - quota visibility (GET /v1/quota): the tenant's own limits and current
//     usage, so a client can back off before it is refused rather than
//     discovering the limit as a 429.
//
// Every provisioning route requires the deployment's operator credential
// (KEEL_ADMIN_KEY or the wired AdminKey seam).
package agentapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/quota"
)

// Provisioner is the persistence seam of the provisioning surface. It is
// implemented over the store; tests supply a fake. Tenant credentials are no
// longer minted here: Bot credentials live at /v1/bots/{id}/keys and human
// accounts use passwords (L18).
type Provisioner interface {
	// CreateTenant inserts a tenant (idempotent by id) and reports whether it
	// was created by this call.
	CreateTenant(ctx context.Context, tenantID uuid.UUID, name string) (bool, error)
	// TenantExists reports whether the tenant row exists.
	TenantExists(ctx context.Context, tenantID uuid.UUID) (bool, error)
}

// QuotaReporter answers GET /v1/quota: the tenant's configured limits and its
// current consumption. Nil answers 501 (a deployment without edge quotas).
type QuotaReporter interface {
	// TenantQuota returns the configured limits.
	TenantQuota(ctx context.Context, tenantID uuid.UUID) (quota.TenantQuota, error)
	// StoredBytes returns the bytes currently stored by the tenant.
	StoredBytes(ctx context.Context, tenantID uuid.UUID) (int64, error)
	// ActiveStreams returns the process's currently open streams for the tenant.
	ActiveStreams(tenantID uuid.UUID) int
}

// registerTenancyRoutes mounts the tenancy surface.
func (s *Server) registerTenancyRoutes() {
	s.mux.HandleFunc("GET /v1/auth/whoami", s.whoAmI)
	s.mux.HandleFunc("GET /v1/quota", s.getQuota)
	s.mux.HandleFunc("POST /v1/admin/tenants", s.provisionTenant)
}

// whoAmI reports the caller's identity and effective permissions. It returns
// the Account, the Bot when the credential is a bot key, and the materialized
// (key -> scope) grant set. The old tenant role is gone: authority is the
// permission registry, not an api_keys role (A4.1).
func (s *Server) whoAmI(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.Access == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("the identity surface is not wired in this deployment"))
		return
	}
	principal, err := s.cfg.Access.Principal(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, errors.New("authentication required"))
		return
	}
	grants, err := s.cfg.Access.Effective(r.Context(), principal)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	permissions := make(map[string]string, len(grants))
	for key, scope := range grants {
		permissions[key] = string(scope)
	}
	account := map[string]any{
		"id":           principal.AccountID.String(),
		"isSuperAdmin": principal.IsSuperAdmin,
	}
	if edge, ok := auth.PrincipalFromContext(r.Context()); ok {
		account["username"] = edge.Username
		account["displayName"] = edge.DisplayName
	}
	if s.cfg.Identity != nil {
		if record, err := s.cfg.Identity.GetAccount(r.Context(), tenantID, principal.AccountID); err == nil {
			account["username"] = record.Username
			account["displayName"] = record.DisplayName
			account["kind"] = record.Kind
			account["disabled"] = record.DisabledAt != nil
		}
	}
	answer := map[string]any{
		"tenantId":    tenantID.String(),
		"account":     account,
		"permissions": permissions,
	}
	if principal.BotID != "" {
		answer["bot"] = map[string]any{"id": principal.BotID}
	}
	if edge, ok := auth.PrincipalFromContext(r.Context()); ok && edge.IsBot() {
		answer["keyId"] = edge.KeyID.String()
		answer["keyPrefix"] = edge.KeyPrefix
	}
	writeJSON(w, http.StatusOK, answer)
}

// getQuota reports the tenant's limits and current usage. Every value is an
// integer count: there is no price, amount or currency anywhere in this
// answer (prices live in contracts).
func (s *Server) getQuota(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.Quota == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("quota reporting is not wired"))
		return
	}
	limits, err := s.cfg.Quota.TenantQuota(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	stored, err := s.cfg.Quota.StoredBytes(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenantId": tenantID.String(),
		"limits": map[string]int64{
			"streams":      int64(limits.Streams),
			"bytesPerSec":  limits.BytesPerSec,
			"storageBytes": limits.StorageBytes,
		},
		"usage": map[string]int64{
			"streams":      int64(s.cfg.Quota.ActiveStreams(tenantID)),
			"storageBytes": stored,
		},
	})
}

// requireAdminCredential enforces the operator boundary for provisioning. The
// retired api_keys admin role no longer exists; the deployment's KEEL_ADMIN_KEY
// in a Bearer header (or the wired AdminKey seam) is the operator credential.
func (s *Server) requireAdminCredential(w http.ResponseWriter, r *http.Request) bool {
	// A raw Bearer here is the deployment's operator key.
	if adminKey := strings.TrimSpace(os.Getenv("KEEL_ADMIN_KEY")); adminKey != "" {
		header := strings.TrimSpace(r.Header.Get("Authorization"))
		if scheme, token, found := strings.Cut(header, " "); found && strings.EqualFold(scheme, "bearer") {
			if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), []byte(adminKey)) == 1 {
				return true
			}
		}
	}
	if s.cfg.AdminKey != nil && s.cfg.AdminKey(r) {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="keel-admin"`)
	writeErr(w, http.StatusUnauthorized, errors.New("an admin credential is required for provisioning"))
	return false
}

// provisionTenant creates a tenant (and seeds its built-in permission groups
// through the Provisioner). No tenant-wide credential is minted: accounts log
// in with passwords and Bots use bot-bound keys.
func (s *Server) provisionTenant(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminCredential(w, r) {
		return
	}
	if s.cfg.Provisioner == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("tenant provisioning is not wired"))
		return
	}
	var body struct {
		Name     string `json:"name"`
		TenantID string `json:"tenantId"`
	}
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, errors.New("name is required"))
		return
	}
	tenantID := uuid.New()
	if raw := strings.TrimSpace(body.TenantID); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("tenantId must be a UUID"))
			return
		}
		tenantID = parsed
	}
	created, err := s.cfg.Provisioner.CreateTenant(r.Context(), tenantID, name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"tenantId": tenantID.String(), "name": name, "created": created,
	})
}
