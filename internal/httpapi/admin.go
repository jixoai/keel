package httpapi

// Administrator tenant API: configuration documents and the usage ledger.
// Every route is authenticated with the operator key from KEEL_ADMIN_KEY
// (constant-time comparison); the tenant id in the path is the subject, never
// taken from a request header. When no key is configured the routes answer 503
// instead of silently becoming public.

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/store"
)

const adminBearerScheme = "bearer"

func (s *Server) registerAdminRoutes() {
	s.mux.HandleFunc("PUT /v1/tenants/{id}/config", s.putTenantConfig)
	s.mux.HandleFunc("GET /v1/tenants/{id}/config", s.getTenantConfig)
	s.mux.HandleFunc("GET /v1/tenants/{id}/usage", s.getTenantUsage)
}

// adminKey resolves the operator key. Config.AdminKey wins so tests and
// embedders can inject it; otherwise the environment is read on every request
// (the key may be rotated without a restart).
func (s *Server) adminKey() string {
	if s.cfg.AdminKey != "" {
		return s.cfg.AdminKey
	}
	return os.Getenv("KEEL_ADMIN_KEY")
}

// requireAdmin enforces the Bearer key. It writes the error response and
// reports false when the request must stop.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	expected := s.adminKey()
	if expected == "" {
		writeErr(w, http.StatusServiceUnavailable, errors.New("admin api is disabled: KEEL_ADMIN_KEY is not set"))
		return false
	}
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], adminBearerScheme) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeErr(w, http.StatusUnauthorized, errors.New("admin bearer token required"))
		return false
	}
	token := strings.TrimSpace(parts[1])
	if subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeErr(w, http.StatusUnauthorized, errors.New("invalid admin token"))
		return false
	}
	return true
}

// adminTenantID parses the {id} path segment after authentication succeeded.
func adminTenantID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	tenantID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("bad tenant id: %w", err))
		return uuid.UUID{}, false
	}
	return tenantID, true
}

// putTenantConfig replaces a tenant's configuration document. The body is the
// TenantConfig object itself (not an envelope); unknown fields and invalid
// values are rejected before the write, and the stored document is the
// canonical marshaling of the validated config.
func (s *Server) putTenantConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	tenantID, ok := adminTenantID(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBody))
	if err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			writeErr(w, http.StatusRequestEntityTooLarge, err)
			return
		}
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	config, err := host.ParseTenantConfig(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := config.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	canonical, err := json.Marshal(config)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	revision, err := s.cfg.Store.SetTenantConfig(r.Context(), tenantID, canonical)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNoTenant) {
			status = http.StatusNotFound
		}
		writeErr(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenantId": tenantID,
		"revision": revision,
	})
}

// getTenantConfig returns the stored document and its revision.
func (s *Server) getTenantConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	tenantID, ok := adminTenantID(w, r)
	if !ok {
		return
	}
	config, err := s.cfg.Store.GetTenantConfig(r.Context(), tenantID)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNoTenant) {
			status = http.StatusNotFound
		}
		writeErr(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenantId": tenantID,
		"revision": config.Revision,
		"config":   config.Config,
	})
}

// tenantUsageRow is one (session, provider, model) aggregation.
type tenantUsageRow struct {
	SessionID          string `json:"sessionId"`
	Provider           string `json:"provider"`
	Model              string `json:"model"`
	Entries            int64  `json:"entries"`
	UnaccountedEntries int64  `json:"unaccountedEntries"`
	InputTokens        int64  `json:"inputTokens"`
	OutputTokens       int64  `json:"outputTokens"`
	CacheReadTokens    int64  `json:"cacheReadTokens"`
	CacheWriteTokens   int64  `json:"cacheWriteTokens"`
	ReasoningTokens    int64  `json:"reasoningTokens"`
}

// tenantUsageTotals is the tenant-wide sum over every row.
type tenantUsageTotals struct {
	Entries            int64 `json:"entries"`
	UnaccountedEntries int64 `json:"unaccountedEntries"`
	InputTokens        int64 `json:"inputTokens"`
	OutputTokens       int64 `json:"outputTokens"`
	CacheReadTokens    int64 `json:"cacheReadTokens"`
	CacheWriteTokens   int64 `json:"cacheWriteTokens"`
	ReasoningTokens    int64 `json:"reasoningTokens"`
}

// getTenantUsage aggregates the usage ledger by session/provider/model and
// returns the totals. Counts only: no price is computed or stored.
func (s *Server) getTenantUsage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	tenantID, ok := adminTenantID(w, r)
	if !ok {
		return
	}
	if _, err := s.cfg.Store.GetTenantRevision(r.Context(), tenantID); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNoTenant) {
			status = http.StatusNotFound
		}
		writeErr(w, status, err)
		return
	}
	groups, err := s.cfg.Store.SumUsage(r.Context(), store.UsageQuery{TenantID: tenantID})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	rows := make([]tenantUsageRow, 0, len(groups))
	totals := tenantUsageTotals{}
	for _, group := range groups {
		rows = append(rows, tenantUsageRow{
			SessionID:          group.SessionID.String(),
			Provider:           group.Provider,
			Model:              group.Model,
			Entries:            group.Entries,
			UnaccountedEntries: group.UnaccountedEntries,
			InputTokens:        group.InputTokens,
			OutputTokens:       group.OutputTokens,
			CacheReadTokens:    group.CacheReadTokens,
			CacheWriteTokens:   group.CacheWriteTokens,
			ReasoningTokens:    group.ReasoningTokens,
		})
		totals.Entries += group.Entries
		totals.UnaccountedEntries += group.UnaccountedEntries
		totals.InputTokens += group.InputTokens
		totals.OutputTokens += group.OutputTokens
		totals.CacheReadTokens += group.CacheReadTokens
		totals.CacheWriteTokens += group.CacheWriteTokens
		totals.ReasoningTokens += group.ReasoningTokens
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenantId": tenantID,
		"usage":    rows,
		"totals":   totals,
	})
}
