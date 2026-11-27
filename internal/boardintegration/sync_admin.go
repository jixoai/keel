package boardintegration

// Keel-only sync entry management routes (keel-board-v1 task 10.6).
//
// board v0.1.0 publishes GET /v1/board/sync/entries and the operational
// entries/status/inbox/outbox/threads routes, but it deliberately has no
// create/rotate route: phase 0 had no safe place for a credential. keel adds
// POST /v1/board/sync/entries and PATCH /v1/board/sync/entries/{key} at the
// mount layer. The board repository is not modified.
//
// Security: the caller needs board.manage (tenant scope) and secret.write. The
// body accepts secret NAMES only; the strict decoder rejects an unknown field,
// so a plaintext token cannot be stored by accident. Responses, logs and audit
// carry a credential STATUS, never a value.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/accesswire"
	"github.com/jixoai/keel/internal/store"

	boardsync "github.com/jixoai/keel-board/sync"
)

const syncAdminBodyLimit = 64 << 10

type syncAdmin struct {
	manager *syncManager
	bridge  *accesswire.Bridge
	tenant  uuid.UUID
	next    http.Handler
	logger  syncLogger
}

// syncLogger is the small logging seam; *slog.Logger satisfies it.
type syncLogger interface {
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// handles reports whether this request is a keel-owned sync admin route. The
// method is part of the match: board's own GET routes are left untouched.
func (a *syncAdmin) handles(r *http.Request) bool {
	if a == nil {
		return false
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch r.Method {
	case http.MethodPost:
		return path == "/v1/board/sync/entries" || path == "/v1/board/sync/threads" ||
			(strings.HasPrefix(path, "/v1/board/sources/") && strings.HasSuffix(path, "/join"))
	case http.MethodPatch:
		return strings.HasPrefix(path, "/v1/board/sync/entries/")
	case http.MethodGet:
		return path == "/v1/board/sync/credentials" ||
			(strings.HasPrefix(path, "/v1/board/sources/") && strings.HasSuffix(path, "/read"))
	default:
		return false
	}
}

func (a *syncAdmin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := a.authorize(w, r, a.needsSecretWrite(r))
	if !ok {
		return
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case r.Method == http.MethodPost && path == "/v1/board/sync/entries":
		a.create(w, r, tenantID)
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "/v1/board/sync/entries/"):
		key := strings.TrimPrefix(path, "/v1/board/sync/entries/")
		if key == "" || strings.Contains(key, "/") {
			writeAdminError(w, http.StatusNotFound, "not_found", "no such sync connection")
			return
		}
		a.update(w, r, tenantID, key)
	case r.Method == http.MethodGet && path == "/v1/board/sync/credentials":
		a.credentials(w, r, tenantID)
	case r.Method == http.MethodPost && path == "/v1/board/sync/threads":
		a.attach(w, r)
	case strings.HasPrefix(path, "/v1/board/sources/"):
		a.source(w, r, path)
	default:
		writeAdminError(w, http.StatusNotFound, "not_found", "no such route")
	}
}

func (a *syncAdmin) needsSecretWrite(r *http.Request) bool {
	path := strings.TrimSuffix(r.URL.Path, "/")
	return (r.Method == http.MethodPost && path == "/v1/board/sync/entries") ||
		(r.Method == http.MethodPatch && strings.HasPrefix(path, "/v1/board/sync/entries/"))
}

// permitConnection is the shared durable-state gate for the board's own sync
// routes. The board module checks its local sync-store row; this adds the keel
// configuration truth (enabled, same provider as this process, credential
// unchanged) so a stale replica cannot attach/read through an old provider.
func (a *syncAdmin) permitConnection(w http.ResponseWriter, r *http.Request, key string) bool {
	state, _, err := a.manager.connectionState(r.Context(), boardsync.Key(key))
	if err != nil {
		writeAdminError(w, http.StatusServiceUnavailable, "unavailable", "sync state is unavailable")
		return false
	}
	switch state {
	case connectionOK:
		return true
	case connectionDisabled, connectionMissing:
		writeAdminError(w, http.StatusNotFound, "sync_disabled", "the sync connection is disabled")
	case connectionPending:
		writeAdminError(w, http.StatusServiceUnavailable, "sync_pending_restart", "the sync connection is pending a restart")
	}
	return false
}

// attach checks the entry's durable state before the board's attach route can
// resolve a target through this process's (possibly old) registry entry.
func (a *syncAdmin) attach(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, syncAdminBodyLimit))
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid", "request body could not be read")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var request struct {
		Key string `json:"key"`
	}
	if len(bytes.TrimSpace(body)) > 0 {
		_ = json.Unmarshal(body, &request)
	}
	if key := strings.TrimSpace(request.Key); key != "" {
		if !a.permitConnection(w, r, key) {
			return
		}
	}
	a.next.ServeHTTP(w, r)
}

// source gates GET read / POST join on the same durable state, then forwards.
func (a *syncAdmin) source(w http.ResponseWriter, r *http.Request, path string) {
	rest := strings.TrimPrefix(path, "/v1/board/sources/")
	index := strings.IndexByte(rest, '/')
	if index <= 0 {
		writeAdminError(w, http.StatusNotFound, "not_found", "no such sync connection")
		return
	}
	key := rest[:index]
	if !a.permitConnection(w, r, key) {
		return
	}
	a.next.ServeHTTP(w, r)
}

// authorize resolves the edge principal and checks the two keys. It returns
// the request tenant (always the deployment tenant: a webhook namespace has no
// tenant selector) and the effective principal. A denial is audited by the
// bridge's normal path.
func (a *syncAdmin) authorize(w http.ResponseWriter, r *http.Request, secretWrite bool) (uuid.UUID, access.Principal, bool) {
	tenantID, ok := requestTenant(r)
	if !ok || tenantID == uuid.Nil {
		writeAdminError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return uuid.Nil, access.Principal{}, false
	}
	if a.tenant == uuid.Nil || tenantID != a.tenant {
		// Sync is deployment-tenant scoped: the webhook route has no tenant
		// segment, so another tenant's entries could never receive a platform
		// event. Hide the surface entirely rather than storing an inert row.
		writeAdminError(w, http.StatusNotFound, "not_found", "no such sync connection")
		return uuid.Nil, access.Principal{}, false
	}
	principal, err := a.bridge.Principal(r.Context(), tenantID)
	if err != nil {
		if errors.Is(err, accesswire.ErrNoPrincipal) {
			writeAdminError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
			return uuid.Nil, access.Principal{}, false
		}
		writeAdminError(w, http.StatusServiceUnavailable, "unavailable", "authorization is unavailable")
		return uuid.Nil, access.Principal{}, false
	}
	resource := access.Resource{TenantID: tenantID, Type: "tenant", ID: tenantID.String()}
	decision, err := a.bridge.Authorize(r.Context(), principal, access.KeyBoardManage, resource)
	if err != nil {
		writeAdminError(w, http.StatusServiceUnavailable, "unavailable", "authorization is unavailable")
		return uuid.Nil, access.Principal{}, false
	}
	if !decision.Allow {
		writeAdminError(w, http.StatusForbidden, "forbidden", "permission denied: missing board.manage")
		return uuid.Nil, access.Principal{}, false
	}
	if secretWrite {
		decision, err = a.bridge.Authorize(r.Context(), principal, access.KeySecretWrite, resource)
		if err != nil {
			writeAdminError(w, http.StatusServiceUnavailable, "unavailable", "authorization is unavailable")
			return uuid.Nil, access.Principal{}, false
		}
		if !decision.Allow {
			writeAdminError(w, http.StatusForbidden, "forbidden", "permission denied: missing secret.write")
			return uuid.Nil, access.Principal{}, false
		}
	}
	return tenantID, principal, true
}

func (a *syncAdmin) create(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) {
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	input, err := parseSyncEntryInput(body)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	view, _, err := a.manager.Create(r.Context(), input)
	if err != nil {
		writeEntryError(w, err)
		return
	}
	writeAdminJSON(w, http.StatusCreated, SyncEntryResult{Entry: view, RestartRequired: true, PendingRestart: true})
}

func (a *syncAdmin) update(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, key string) {
	body, ok := readAdminBody(w, r)
	if !ok {
		return
	}
	patch, err := parseSyncEntryPatch(body)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	view, found, restartRequired, err := a.manager.Update(r.Context(), key, patch)
	if err != nil {
		writeEntryError(w, err)
		return
	}
	if !found {
		writeAdminError(w, http.StatusNotFound, "not_found", "no such sync connection")
		return
	}
	writeAdminJSON(w, http.StatusOK, SyncEntryResult{
		Entry: view, RestartRequired: restartRequired, PendingRestart: restartRequired,
	})
}

type credentialEntryView struct {
	Key             string           `json:"key"`
	Platform        string           `json:"platform"`
	Enabled         bool             `json:"enabled"`
	Credential      CredentialStatus `json:"credential"`
	RoutesReady     bool             `json:"routesReady"`
	RouteIssue      string           `json:"routeIssue,omitempty"`
	RestartRequired bool             `json:"restartRequired,omitempty"`
}

// credentials answers GET /v1/board/sync/credentials: the configured/missing/
// invalid state of every entry, never a value. It is the keel-side status the
// board v0.1.0 entry view has no field for.
func (a *syncAdmin) credentials(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) {
	entries, err := a.manager.List(r.Context())
	if err != nil {
		a.logger.Warn("board sync credential status read failed", "tenant", tenantID, "error", err)
		writeAdminError(w, http.StatusServiceUnavailable, "unavailable", "credential status is unavailable")
		return
	}
	views := make([]credentialEntryView, 0, len(entries))
	for _, entry := range entries {
		views = append(views, credentialEntryView{
			Key: entry.Key, Platform: entry.Platform, Enabled: entry.Enabled,
			Credential: entry.Credential, RoutesReady: entry.RoutesReady,
			RouteIssue: entry.RouteIssue, RestartRequired: entry.RestartRequired,
		})
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"entries": views})
}

func readAdminBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, syncAdminBodyLimit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeAdminError(w, http.StatusRequestEntityTooLarge, "too_large", "request body is too large")
			return nil, false
		}
		writeAdminError(w, http.StatusBadRequest, "invalid", "request body could not be read")
		return nil, false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		writeAdminError(w, http.StatusBadRequest, "invalid", "request body is required")
		return nil, false
	}
	// Reject a JSON document followed by trailing bytes: a second object must
	// not be silently ignored.
	decoder := json.NewDecoder(bytes.NewReader(body))
	var probe any
	if err := decoder.Decode(&probe); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid", "request body must be one JSON object")
		return nil, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeAdminError(w, http.StatusBadRequest, "invalid", "request body must be one JSON object")
		return nil, false
	}
	return body, true
}

func writeEntryError(w http.ResponseWriter, err error) {
	if errors.Is(err, errSyncEntryUnavailable) {
		writeAdminError(w, http.StatusServiceUnavailable, "unavailable", "sync entry storage is unavailable")
		return
	}
	switch {
	case errors.Is(err, store.ErrBoardSyncEntryExists):
		writeAdminError(w, http.StatusConflict, "already_exists", "a sync connection with that key already exists")
	case errors.Is(err, store.ErrBoardSyncEntryMissing):
		writeAdminError(w, http.StatusNotFound, "not_found", "no such sync connection")
	case errors.Is(err, store.ErrBoardSyncEntryConflict):
		writeAdminError(w, http.StatusConflict, "revision_conflict", "the entry changed; re-read and retry")
	default:
		writeAdminError(w, http.StatusBadRequest, "invalid", err.Error())
	}
}

func writeAdminJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeAdminError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message, "code": code})
}
