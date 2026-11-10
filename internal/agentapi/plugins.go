package agentapi

// Plugin lifecycle API (change keel-plugin-core): the inventory
// (GET /v1/plugins), installation from a local package (POST /v1/plugins/install),
// the state machine (POST /v1/plugins/{id}/enable|disable|uninstall|update) and
// the audit trail (GET /v1/plugins/audit).
//
// The package does not implement any of it: every action is delegated to the
// plugin manager (internal/agentext), which owns the state machine, the package
// store and the runtime assembly. This file is the HTTP shape: status codes,
// structured field-level errors and audit attribution (who asked).
//
// Status-code semantics:
//
//	400 the request or the package it names cannot be used (bad path, invalid
//	    manifest) — the client can fix it and retry;
//	404 the plugin id is not installed;
//	409 the state machine refuses the transition (already enabled, not enabled,
//	    enabled dependents, version gate, kind without a loader), or a durable
//	    approval was refused/left open;
//	202 install/enable/update answered with the durable approval question
//	    (plugin_approval.go): the action runs only once that decision is answered
//	    with `allow` and the request is repeated with its decisionId;
//	503 no plugin manager is wired in this deployment, or lifecycle writes have
//	    no decision primitive to ask through (they fail closed).

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/pluginpkg"
	"github.com/jixoai/keel/internal/store"
)

// PluginManager is the lifecycle seam. *agentext.Manager satisfies it; tests
// substitute a fake.
type PluginManager interface {
	// Snapshot renders the inventory and the effective tool surface.
	Snapshot() agentext.PluginInventory
	Install(ctx context.Context, req agentext.InstallRequest, audit agentext.AuditContext) (agentext.PluginView, error)
	Enable(ctx context.Context, id string, audit agentext.AuditContext) (agentext.PluginView, error)
	Disable(ctx context.Context, id string, audit agentext.AuditContext) (agentext.PluginView, error)
	Uninstall(ctx context.Context, id string, audit agentext.AuditContext) (agentext.PluginView, error)
	Update(ctx context.Context, id string, req agentext.UpdateRequest, audit agentext.AuditContext) (agentext.PluginView, error)
	Audit(ctx context.Context, tenantID *uuid.UUID, pluginID string, after int64, limit int) ([]store.PluginAuditEntry, error)
}

// pluginAuditMaxLimit bounds GET /v1/plugins/audit.
const pluginAuditMaxLimit = 200

func (s *Server) pluginRoutes() {
	s.mux.HandleFunc("GET /v1/plugins", s.pluginsInventory)
	s.mux.HandleFunc("POST /v1/plugins/install", s.installPlugin)
	s.mux.HandleFunc("GET /v1/plugins/audit", s.pluginAudit)
	s.mux.HandleFunc("POST /v1/plugins/{id}/enable", s.enablePlugin)
	s.mux.HandleFunc("POST /v1/plugins/{id}/disable", s.disablePlugin)
	s.mux.HandleFunc("POST /v1/plugins/{id}/uninstall", s.uninstallPlugin)
	s.mux.HandleFunc("POST /v1/plugins/{id}/update", s.updatePlugin)
	s.pluginApprovalRoutes()
	s.pluginBridgeRoutes()
}

// pluginInventoryResponse is the GET /v1/plugins body.
type pluginInventoryResponse struct {
	Root       string                   `json:"root,omitempty"`
	Revision   int64                    `json:"revision"`
	Generation int64                    `json:"generation"`
	Plugins    []agentext.PluginView    `json:"plugins"`
	Tools      []string                 `json:"tools"`
	Enabled    []string                 `json:"enabled"`
	Hooks      agentext.StatsSummary    `json:"hooks"`
	HookStats  []agentext.HookStat      `json:"hookStats,omitempty"`
	Triggers   []agentext.TriggerStatus `json:"triggers,omitempty"`
	Lifecycle  string                   `json:"lifecycle"`
	Limits     map[string]int           `json:"limits,omitempty"`
}

// pluginsInventory implements GET /v1/plugins. Without a manager it answers the
// assembled snapshot exactly as before (a host that only compiles plugins in).
func (s *Server) pluginsInventory(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantFrom(w, r); !ok {
		return
	}
	if s.cfg.PluginManager == nil {
		s.listPlugins(w, r)
		return
	}
	inventory := s.cfg.PluginManager.Snapshot()
	body := pluginInventoryResponse{
		Root:       inventory.Root,
		Revision:   inventory.Revision,
		Generation: inventory.Generation,
		Plugins:    inventory.Plugins,
		Tools:      inventory.Tools,
		Enabled:    inventory.Enabled,
		Hooks:      inventory.Hooks,
		Lifecycle:  "dynamic",
		Limits:     map[string]int{"auditPageMax": pluginAuditMaxLimit},
	}
	if s.cfg.Triggers != nil {
		body.Triggers = s.cfg.Triggers.List()
	}
	if s.cfg.Hooks != nil {
		body.HookStats = s.cfg.Hooks.Stats()
	}
	if body.Plugins == nil {
		body.Plugins = []agentext.PluginView{}
	}
	if body.Tools == nil {
		body.Tools = []string{}
	}
	if body.Enabled == nil {
		body.Enabled = []string{}
	}
	writeJSON(w, http.StatusOK, body)
}

// installPluginRequest is the POST /v1/plugins/install body.
type installPluginRequest struct {
	// Path is a local package directory. Its content is validated, hashed and
	// copied into the managed plugins root.
	Path string `json:"path"`
	// The durable approval of this install (see plugin_approval.go): without a
	// decisionId the call answers 202 with the question it opened.
	pluginApprovalRequest
}

func (s *Server) installPlugin(w http.ResponseWriter, r *http.Request) {
	var body installPluginRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	manager, audit, ok := s.pluginWriteContext(w, r, installApprovalAsk(body.Path), body.pluginApprovalRequest)
	if !ok {
		return
	}
	view, err := manager.Install(r.Context(), agentext.InstallRequest{Path: body.Path}, audit)
	if err != nil {
		s.writePluginError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"plugin": view})
}

// enablePluginRequest is the POST /v1/plugins/{id}/enable body: only the
// durable approval travels here.
type enablePluginRequest struct {
	pluginApprovalRequest
}

func (s *Server) enablePlugin(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pluginIDFrom(w, r)
	if !ok {
		return
	}
	var body enablePluginRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	manager, audit, ok := s.pluginWriteContext(w, r, s.pluginIDApprovalAsk(store.PluginActionEnable, id), body.pluginApprovalRequest)
	if !ok {
		return
	}
	view, err := manager.Enable(r.Context(), id, audit)
	if err != nil {
		s.writePluginError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugin": view})
}

func (s *Server) disablePlugin(w http.ResponseWriter, r *http.Request) {
	manager, audit, ok := s.pluginCallContext(w, r)
	if !ok {
		return
	}
	id, ok := s.pluginIDFrom(w, r)
	if !ok {
		return
	}
	view, err := manager.Disable(r.Context(), id, audit)
	if err != nil {
		s.writePluginError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugin": view})
}

func (s *Server) uninstallPlugin(w http.ResponseWriter, r *http.Request) {
	manager, audit, ok := s.pluginCallContext(w, r)
	if !ok {
		return
	}
	id, ok := s.pluginIDFrom(w, r)
	if !ok {
		return
	}
	view, err := manager.Uninstall(r.Context(), id, audit)
	if err != nil {
		s.writePluginError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugin": view})
}

// updatePluginRequest is the POST /v1/plugins/{id}/update body. An empty path
// picks the highest installed version different from the current one from the
// plugins root (the multi-version layout).
type updatePluginRequest struct {
	Path string `json:"path"`
	// The durable approval of this update (see plugin_approval.go).
	pluginApprovalRequest
}

func (s *Server) updatePlugin(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pluginIDFrom(w, r)
	if !ok {
		return
	}
	var body updatePluginRequest
	if r.ContentLength != 0 {
		if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	manager, audit, ok := s.pluginWriteContext(w, r, s.pluginIDApprovalAsk(store.PluginActionUpdate, id), body.pluginApprovalRequest)
	if !ok {
		return
	}
	view, err := manager.Update(r.Context(), id, agentext.UpdateRequest{Path: body.Path}, audit)
	if err != nil {
		s.writePluginError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugin": view})
}

// pluginAudit implements GET /v1/plugins/audit: the tenant's lifecycle trail in
// chronological order, keyset-paginated by `after` (an audit id).
func (s *Server) pluginAudit(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.PluginManager == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("plugin management is not wired"))
		return
	}
	limit, err := queryInt(r, "limit", 50, 1, pluginAuditMaxLimit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	after := int64(0)
	if raw := strings.TrimSpace(r.URL.Query().Get("after")); raw != "" {
		parsed, err := parseAuditCursor(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		after = parsed
	}
	pluginID := strings.TrimSpace(r.URL.Query().Get("plugin"))
	if pluginID != "" && !pluginNameValid(pluginID) {
		writeErr(w, http.StatusBadRequest, errors.New("plugin must be a plugin id"))
		return
	}
	entries, err := s.cfg.PluginManager.Audit(r.Context(), &tenantID, pluginID, after, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	body := map[string]any{"entries": entries, "limit": limit}
	if len(entries) > 0 {
		body["next"] = entries[len(entries)-1].AuditID
	} else {
		body["next"] = after
	}
	writeJSON(w, http.StatusOK, body)
}

// --- helpers --------------------------------------------------------------

// pluginCallContext resolves the manager and the audit attribution (tenant plus
// actor) shared by every lifecycle write.
func (s *Server) pluginCallContext(w http.ResponseWriter, r *http.Request) (PluginManager, agentext.AuditContext, bool) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return nil, agentext.AuditContext{}, false
	}
	if s.cfg.PluginManager == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("plugin management is not wired"))
		return nil, agentext.AuditContext{}, false
	}
	actor := strings.TrimSpace(r.Header.Get("X-Keel-Actor"))
	if actor == "" {
		actor = "tenant:" + tenantID.String()
	}
	if len(actor) > 128 {
		actor = actor[:128]
	}
	return s.cfg.PluginManager, agentext.AuditContext{TenantID: &tenantID, Actor: actor}, true
}

// pluginIDFrom reads and validates the {id} path value.
func (s *Server) pluginIDFrom(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	// The same grammar the manifest enforces: refusing a malformed id here keeps
	// the 404/400 split clean (a malformed id cannot name an installed plugin).
	if !pluginNameValid(id) || strings.ContainsAny(id, "/\\") {
		writeErr(w, http.StatusBadRequest, errors.New("plugin id is malformed"))
		return "", false
	}
	return id, true
}

// writePluginError renders a manager failure as a structured, statused body.
func (s *Server) writePluginError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var pluginErr *agentext.PluginError
	errors.As(err, &pluginErr)
	switch {
	case errors.Is(err, agentext.ErrPluginNotFound):
		status = http.StatusNotFound
	case errors.Is(err, agentext.ErrPluginExists),
		errors.Is(err, agentext.ErrPluginState),
		errors.Is(err, agentext.ErrPluginDependency),
		errors.Is(err, agentext.ErrPluginKind),
		errors.Is(err, agentext.ErrPluginBundled),
		errors.Is(err, agentext.ErrPluginGate):
		status = http.StatusConflict
	case errors.Is(err, agentext.ErrPluginInvalid), errors.Is(err, agentext.ErrPluginPath):
		status = http.StatusBadRequest
		if pluginErr != nil && pluginErr.Op != "" && pluginErr.Op != "install" && pluginErr.Op != "update" {
			// The request was fine; the package it names is not usable in its
			// current state.
			status = http.StatusConflict
		}
	}
	body := map[string]any{"error": err.Error()}
	if pluginErr != nil {
		if pluginErr.Op != "" {
			body["op"] = pluginErr.Op
		}
		if pluginErr.ID != "" {
			body["pluginId"] = pluginErr.ID
		}
		if len(pluginErr.Fields) > 0 {
			body["fields"] = pluginErr.Fields
		}
		if len(pluginErr.Missing) > 0 {
			body["missing"] = pluginErr.Missing
		}
		if len(pluginErr.Dependents) > 0 {
			body["dependents"] = pluginErr.Dependents
		}
		if len(pluginErr.Diagnostics) > 0 {
			body["diagnostics"] = pluginErr.Diagnostics
		}
	}
	writeJSON(w, status, body)
}

// pluginNameValid reports whether a string matches the plugin-id grammar (the
// manifest's, from internal/pluginpkg) so a malformed path is a 400 before the
// manager is consulted.
func pluginNameValid(value string) bool { return pluginpkg.ValidatePluginID(value) }

// parseAuditCursor validates the `after` cursor.
func parseAuditCursor(raw string) (int64, error) {
	var value int64
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, errors.New("after must be an audit id")
		}
		value = value*10 + int64(raw[i]-'0')
	}
	return value, nil
}
