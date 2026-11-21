// Tenant provisioning and quota surface (change keel-ga §2 tenancy-quotas).
//
// Three concerns live here:
//
//   - who am I (GET /v1/auth/whoami): the resolved tenant, key prefix and role
//     of the credential the request presented. A client that holds a key uses
//     this to learn its tenant without being told it separately.
//   - provisioning (POST /v1/admin/tenants, POST /v1/admin/tenants/{id}/keys):
//     the operator path that creates a tenant and mints a key. The plaintext key
//     appears exactly once, in the creation answer; it is never stored, never
//     logged, and no route returns it again.
//   - quota visibility (GET /v1/quota): the tenant's own limits and current
//     usage, so a client can back off before it is refused rather than
//     discovering the limit as a 429.
//
// Every provisioning route requires an admin credential (an api_keys row with
// role='admin', or the deployment's KEEL_ADMIN_KEY). A tenant key presenting
// itself as an operator is a 403, not a 401: the credential is valid, its
// authority is not sufficient.
package agentapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/auth"
	"github.com/jixoai/keel/internal/quota"
)

// Provisioner is the persistence seam of the provisioning surface. It is
// implemented over the store; tests supply a fake.
type Provisioner interface {
	// CreateTenant inserts a tenant (idempotent by id) and reports whether it
	// was created by this call.
	CreateTenant(ctx context.Context, tenantID uuid.UUID, name string) (bool, error)
	// MintAPIKey persists a key hash and returns the row's identity. The
	// plaintext is never passed here: the caller keeps it for the one-time
	// answer.
	MintAPIKey(ctx context.Context, tenantID, keyID uuid.UUID, hash []byte, prefix, role, label string) error
	// TenantExists reports whether the tenant row exists.
	TenantExists(ctx context.Context, tenantID uuid.UUID) (bool, error)
	// ListAPIKeys returns a tenant's keys (prefixes only).
	ListAPIKeys(ctx context.Context, tenantID uuid.UUID) ([]APIKeyView, error)
	// RevokeAPIKey revokes one key and reports whether it existed.
	RevokeAPIKey(ctx context.Context, tenantID, keyID uuid.UUID) (bool, error)
}

// APIKeyView is the display projection of a key: no hash, no plaintext.
type APIKeyView struct {
	KeyID     string     `json:"keyId"`
	Prefix    string     `json:"prefix"`
	Role      string     `json:"role"`
	Label     string     `json:"label"`
	CreatedAt time.Time  `json:"createdAt"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
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
	s.mux.HandleFunc("GET /v1/admin/tenants/{id}/keys", s.listAPIKeys)
	s.mux.HandleFunc("POST /v1/admin/tenants/{id}/keys", s.createAPIKey)
	s.mux.HandleFunc("DELETE /v1/admin/tenants/{id}/keys/{keyId}", s.revokeAPIKey)
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

// requireAdminCredential enforces the operator boundary for provisioning.
//
// Two credentials are accepted: an api_keys row with role='admin' (the
// middleware has already resolved it) or the deployment's KEEL_ADMIN_KEY in a
// Bearer header (the pre-existing operator path the runtime's admin API uses).
// A valid tenant key is a 403: the caller is authenticated, just not
// authorized.
func (s *Server) requireAdminCredential(w http.ResponseWriter, r *http.Request) bool {
	if identity, ok := auth.IdentityFrom(r.Context()); ok {
		if identity.Admin() {
			return true
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="keel-admin"`)
		writeErr(w, http.StatusForbidden, errors.New("an admin credential is required for provisioning"))
		return false
	}
	// The middleware strips Authorization only for key-shaped credentials, so a
	// raw Bearer here is the deployment's operator key.
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

// provisionTenant creates a tenant and, unless told otherwise, its first key.
// The answer carries the plaintext key exactly once.
func (s *Server) provisionTenant(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminCredential(w, r) {
		return
	}
	if s.cfg.Provisioner == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("tenant provisioning is not wired"))
		return
	}
	var body struct {
		Name      string `json:"name"`
		TenantID  string `json:"tenantId"`
		Role      string `json:"role"`
		Label     string `json:"label"`
		NoAPIKey  bool   `json:"noApiKey"`
		QuotaKeep struct {
			Streams      int   `json:"streams"`
			BytesPerSec  int64 `json:"bytesPerSec"`
			StorageBytes int64 `json:"storageBytes"`
		} `json:"quota"`
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
	answer := map[string]any{"tenantId": tenantID.String(), "name": name, "created": created}
	if body.NoAPIKey {
		writeJSON(w, http.StatusCreated, answer)
		return
	}
	key, err := s.mintKey(r.Context(), tenantID, body.Role, body.Label)
	if err != nil {
		// The tenant exists but its key failed: report both facts rather than
		// pretending the whole call failed (a retry can mint the key alone).
		answer["error"] = "tenant created, api key could not be minted"
		writeJSON(w, http.StatusInternalServerError, answer)
		return
	}
	answer["apiKey"] = key
	writeJSON(w, http.StatusCreated, answer)
}

// createAPIKey mints another key for an existing tenant.
func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminCredential(w, r) {
		return
	}
	if s.cfg.Provisioner == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("tenant provisioning is not wired"))
		return
	}
	tenantID, ok := s.pathTenantID(w, r)
	if !ok {
		return
	}
	exists, err := s.cfg.Provisioner.TenantExists(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !exists {
		writeErr(w, http.StatusNotFound, errors.New("tenant not found"))
		return
	}
	var body struct {
		Role  string `json:"role"`
		Label string `json:"label"`
	}
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	key, err := s.mintKey(r.Context(), tenantID, body.Role, body.Label)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"tenantId": tenantID.String(), "apiKey": key})
}

// mintKey generates, persists and describes a key. The plaintext is in the
// returned map only, for the caller's single answer.
func (s *Server) mintKey(ctx context.Context, tenantID uuid.UUID, role, label string) (map[string]any, error) {
	minted, err := auth.MintKey(role)
	if err != nil {
		return nil, err
	}
	keyID := uuid.New()
	if err := s.cfg.Provisioner.MintAPIKey(ctx, tenantID, keyID, minted.Hash, minted.Prefix, minted.Role, strings.TrimSpace(label)); err != nil {
		return nil, err
	}
	return map[string]any{
		"keyId":  keyID.String(),
		"prefix": minted.Prefix,
		"role":   minted.Role,
		"label":  strings.TrimSpace(label),
		// The one and only appearance of the plaintext.
		"key": minted.Plaintext,
		// The client must store it now: nothing can show it again.
		"notice": "store this key now; it cannot be shown again",
	}, nil
}

func (s *Server) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminCredential(w, r) {
		return
	}
	if s.cfg.Provisioner == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("tenant provisioning is not wired"))
		return
	}
	tenantID, ok := s.pathTenantID(w, r)
	if !ok {
		return
	}
	keys, err := s.cfg.Provisioner.ListAPIKeys(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if keys == nil {
		keys = []APIKeyView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenantId": tenantID.String(), "keys": keys})
}

func (s *Server) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminCredential(w, r) {
		return
	}
	if s.cfg.Provisioner == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("tenant provisioning is not wired"))
		return
	}
	tenantID, ok := s.pathTenantID(w, r)
	if !ok {
		return
	}
	keyID, err := uuid.Parse(r.PathValue("keyId"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("keyId must be a UUID"))
		return
	}
	found, err := s.cfg.Provisioner.RevokeAPIKey(r.Context(), tenantID, keyID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, errors.New("api key not found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenantId": tenantID.String(), "keyId": keyID.String(), "revoked": true})
}

func (s *Server) pathTenantID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	tenantID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("tenant id must be a UUID"))
		return uuid.Nil, false
	}
	return tenantID, true
}
