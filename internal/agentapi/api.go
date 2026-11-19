// Package agentapi is the product-layer AgentAPI (keel-agent): the /v1 surface
// an AI UI (web or TUI) consumes.
//
// It is mounted next to the runtime's existing HTTP edge (internal/httpapi) on
// the same server, so a browser never needs CORS and the SSE endpoint keeps its
// single transport. The package owns:
//
//   - session listing (keyset pagination) and cancellation;
//   - approval decisions (open/answer/wait, idempotent, durable rows);
//   - the token-level partial transport (?partials=1), wired into the runtime's
//     SSE handler through a narrow seam;
//   - capability discovery and the extension query/control plane
//     (plugins/triggers/sub-agents), including the plugin lifecycle surface
//     (internal/agentapi/plugins.go: inventory, install, enable/disable/
//     uninstall/update, audit);
//   - the settings surface (internal/agentapi/settings.go): the tenant model
//     configuration write path, secret storage and the plugin inventory —
//     separate from the workspace, mounted under /v1/settings/.
//
// Authentication is NOT implemented here: it is a service-layer concern
// (cmd/keel's cookie/passphrase middleware). This package reads the
// X-Tenant-ID the middleware (or an authenticated API client) supplies, exactly
// like the kernel does. The kernel never learns about cookies, passwords or the
// AgentAPI's version.
package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/approval"
	"github.com/jixoai/keel/internal/logsafe"
	"github.com/jixoai/keel/internal/notify"
)

// Version is the AgentAPI service version reported by /v1/capabilities.
const Version = "keel-agent/0.1.0"

// APIVersion is the URL prefix contract. New capabilities are additive; a
// breaking shape change would need /v2.
const APIVersion = "v1"

// ErrSessionNotFound is the honest 404 of the product API: the session does not
// exist for this tenant (a foreign tenant's session answers the same way).
var ErrSessionNotFound = errors.New("agentapi: session not found")

// ErrExternalSession reports that the session is owned by an external backend:
// it has no native run to cancel (409).
var ErrExternalSession = errors.New("agentapi: session is owned by an external backend")

// CancelFunc cancels the session's active run. The adapter lives in the hosting
// process (cmd/keel) because it needs the run seam and the session check.
type CancelFunc func(ctx context.Context, tenantID, sessionID uuid.UUID) (agentext.CancelResult, error)

// ParentResolver supplies the parent session of a sub-agent session (the
// in-process registry of the sub-agent runner).
type ParentResolver interface {
	ParentOf(sessionID uuid.UUID) (string, bool)
}

// Config wires the product API.
type Config struct {
	// Sessions lists sessions (PG-backed in production, a fake in tests). Nil
	// answers 503 on the list endpoint.
	Sessions SessionLister
	// Cancel stops a session's active run. Nil answers 501 (the endpoint exists
	// but the deployment has no cancel seam).
	Cancel CancelFunc
	// Decisions is the approval primitive. Nil answers 503.
	Decisions *agentext.DecisionBroker
	// Approvals records and reads approval provenance (who answered, over which
	// channel, validated/escape). Nil keeps the decision routes as they are and
	// omits provenance from their answers.
	Approvals *approval.Service
	// Notifications is the durable notification primitive behind the notify
	// tool and the /v1/notifications routes. Nil answers 503 there.
	Notifications *notify.Service
	// Partials is the in-process partial hub (non-persistent bypass). Nil makes
	// ?partials=1 a no-op (the flag is then reported false by capabilities).
	Partials *agentext.PartialHub
	// Plugins is the assembled extension runtime. Nil answers 503 on
	// GET /v1/plugins.
	Plugins *agentext.Runtime
	// PluginManager is the plugin lifecycle surface (inventory, install,
	// enable/disable/uninstall/update, audit). When set, GET /v1/plugins answers
	// the managed inventory; when nil, it answers the assembled snapshot (a host
	// that only compiles plugins in) and the write routes answer 503.
	PluginManager PluginManager
	// PluginAuditWriter records audit rows for lifecycle writes that never
	// reached the plugin manager (approval refusals). Nil logs the refusal
	// instead: the action is refused either way, but only a wired writer makes
	// it queryable through GET /v1/plugins/audit as `refused`.
	PluginAuditWriter PluginAuditWriter
	// PluginTools is the plugin UI bridge's tool invocation seam: calling a tool
	// the plugin declared. Nil answers 501 on bridge calls.
	PluginTools PluginToolInvoker
	// Theme is the host theme token document pushed to plugin UIs through the
	// bridge state verb ("宿主把主题令牌下发给插件 UI"). The host process reads
	// it from the web build (web/theme.json); nil omits it from the answer.
	Theme json.RawMessage
	// Triggers is the trigger engine. Nil answers 503 on the trigger routes.
	Triggers *agentext.TriggerEngine
	// Subagents is the sub-agent runner. Nil answers 503 on the sub-agent
	// routes.
	Subagents *agentext.SubagentRunner
	// Hooks is the hook bus (detailed subscription stats on GET /v1/plugins).
	Hooks *agentext.HookBus
	// Parents optionally resolves sub-agent parent links; defaults to Subagents.
	Parents ParentResolver
	// Models, when set, exposes the resolved provider configuration as the
	// settings/registry projection (GET /v1/providers, GET /v1/models). The
	// implementation lives in the hosting process, so this package never
	// depends on the service assembly. Nil answers 501 on both endpoints.
	Models ModelCatalog
	// ModelsLive, when set, resolves the model catalog of the current runtime
	// generation per request instead of once at boot, so a settings write is
	// visible to the next /v1/providers and /v1/models call without a restart. A
	// nil result (the current generation carries no provider configuration at
	// all) keeps the 501 answer Models: nil gives.
	ModelsLive func() ModelCatalog
	// ModelSettingsStore and ModelSettingsRuntime are the model settings write
	// path (GET/PUT/DELETE /v1/settings/models): the override store and the live
	// runtime that validates and applies it. Both must be set for the routes to
	// be live; either nil answers 501.
	ModelSettingsStore   ModelSettingsStore
	ModelSettingsRuntime ModelSettingsRuntime
	// ProviderReferences is the registry read behind provider deletion
	// protection: the agents/teams whose model bindings name a provider the
	// write would remove. Nil skips the check (a deployment without the
	// agent/team registry).
	ProviderReferences ProviderReferences
	// SecretStore is the tenant secret storage (GET/PUT/DELETE
	// /v1/settings/secrets). Nil means this deployment has no secret storage:
	// the list reports unavailable with SecretStoreReason and writes answer 503.
	SecretStore SecretStore
	// SecretStoreReason explains a missing secret store to an operator. It is
	// rendered verbatim and must never contain a value.
	SecretStoreReason string
	// SessionExists validates that a session belongs to the tenant before a
	// product endpoint writes to it (a decision opened on a missing session
	// would be an orphan row). Nil skips the check (tests without a store).
	SessionExists func(ctx context.Context, tenantID, sessionID uuid.UUID) (bool, error)
	// ArchiveSession backs DELETE /v1/sessions/{id}: a soft delete that takes
	// the session out of the list while its event ledger stays (usage/audit
	// truth is never deleted). It reports false for a missing or foreign
	// session, which the route answers 404. Nil answers 503.
	ArchiveSession func(ctx context.Context, tenantID, sessionID uuid.UUID) (bool, error)
	// RunStatus optionally overrides the durable run status (a cancelled run
	// settles as failed in the kernel policy table; the product snapshot
	// remembers the cancellation).
	RunStatus func(tenantID, sessionID uuid.UUID) (string, bool)
	// Provisioner is the tenant provisioning surface (POST /v1/admin/tenants
	// and the per-tenant key routes). Nil answers 501: a deployment without it
	// has no operator path, which is a configuration choice, not a bug.
	Provisioner Provisioner
	// Quota reports the tenant's limits and current usage (GET /v1/quota). Nil
	// answers 501.
	Quota QuotaReporter
	// AdminKey decides whether a request carries the deployment's operator key.
	// The provisioner routes accept it in addition to an api_keys admin
	// credential. Nil means "KEEL_ADMIN_KEY only".
	AdminKey func(r *http.Request) bool
	// SubagentWaitDefault bounds the synchronous wait of a sub-agent spawn over
	// HTTP when the request does not set a budget (default 60s).
	SubagentWaitDefault time.Duration
	// MCPApps is the host's MCP Apps read model (the app catalogue and the
	// document store). Nil answers 501 on the /v1/mcp/apps* routes and reports
	// mcp.apps=false: a deployment without the registry does not serve apps.
	MCPApps MCPAppsSurface
	// MCPAppsSandboxOrigin is the configured second origin of the View sandbox
	// (KEEL_MCP_APP_ORIGIN). Empty means the capability bit is false and the
	// client stays on the opaque-origin form (spec「View 沙箱与跨端承载」).
	MCPAppsSandboxOrigin string
	// MCPAppsTheme is the host theme token document (web/theme.json). The app
	// catalogue serves its cssVars mapped onto the apps standard variables, so
	// the View inherits the host's palettes.
	MCPAppsTheme json.RawMessage
	// MCPEvents reports the MCP events capability (subscriptions/listen,
	// notification routing). P4 wires it; until then it is false and clients
	// use the legacy behavior.
	MCPEvents bool
	// MCPAppRuntime executes the app bridge's outbound calls (tools/call and
	// resources/read) against the live MCP fleet. Nil answers 501 on the proxy
	// routes: a deployment without MCP connections has nothing to proxy to.
	MCPAppRuntime MCPAppRuntime
	// MCPAppAudit is the durable app audit trail (P3.7). Nil keeps the actions
	// working with a structured-log warning; the audit read route answers 503.
	// Refusal audits are best-effort either way.
	MCPAppAudit MCPAppAuditStore
	// MCPAppInputCommit writes a confirmed ui/message as a normal user.message
	// through the runtime's shared input path (delivery=queue). Nil answers 501
	// on ui/message: a deployment without the write path must not pretend the
	// message was sent.
	MCPAppInputCommit func(ctx context.Context, tenantID, sessionID uuid.UUID, requestID, text string, attachments json.RawMessage) (MCPAppInput, error)
	// MCPAppConfirmTimeout bounds the wait of one app confirmation decision
	// (default 120s, capped at 10 minutes). The app bridge holds the request
	// while the user answers; a timeout answers 202 with the open decision id so
	// the bridge can retry the same logical call.
	MCPAppConfirmTimeout time.Duration
	// MCPAppNow overrides the limiter clock (tests use a fake clock). Nil uses
	// time.Now.
	MCPAppNow func() time.Time
	Logger    *slog.Logger
	MaxBody   int64
}

// Server is the AgentAPI HTTP handler. Mount it on the runtime's mux under
// /v1/ (specific existing /v1 routes keep precedence in the outer mux).
type Server struct {
	cfg Config
	mux *http.ServeMux
	// settingsMu serializes the model-settings write path (read current →
	// validate → store → apply). Without it two concurrent writes can interleave
	// as store(B), apply(A): the row says B while the generation serves A. The
	// window is small (the store write and the apply are back to back) but the
	// inconsistency is real, and one lock is cheaper than explaining it.
	settingsMu sync.Mutex
	// mcpAppLimits is the per-app bridge budget (P3.8): 200 requests/s and 4
	// concurrent proxy requests per app.
	mcpAppLimits *mcpAppLimiter
}

// New builds the API and its routes.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = 1 << 20
	}
	if cfg.SubagentWaitDefault <= 0 {
		cfg.SubagentWaitDefault = 60 * time.Second
	}
	if cfg.Parents == nil && cfg.Subagents != nil {
		cfg.Parents = cfg.Subagents
	}
	if cfg.MCPAppConfirmTimeout <= 0 {
		cfg.MCPAppConfirmTimeout = mcpAppConfirmDefaultTimeout
	}
	if cfg.MCPAppConfirmTimeout > mcpAppConfirmMaxTimeout {
		cfg.MCPAppConfirmTimeout = mcpAppConfirmMaxTimeout
	}
	if cfg.MCPAppNow == nil {
		cfg.MCPAppNow = func() time.Time { return time.Now().UTC() }
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux(), mcpAppLimits: newMCPAppLimiter(cfg.MCPAppNow)}
	s.routes()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /v1/capabilities", s.capabilities)
	s.mux.HandleFunc("GET /v1/providers", s.listProviders)
	s.mux.HandleFunc("GET /v1/models", s.listModels)
	s.settingsRoutes()
	s.mux.HandleFunc("GET /v1/sessions", s.listSessions)
	s.mux.HandleFunc("DELETE /v1/sessions/{id}", s.archiveSession)
	s.mux.HandleFunc("POST /v1/sessions/{id}/cancel", s.cancelSession)
	s.mux.HandleFunc("GET /v1/sessions/{id}/decisions", s.listDecisions)
	s.mux.HandleFunc("POST /v1/sessions/{id}/decisions", s.openDecision)
	s.mux.HandleFunc("GET /v1/sessions/{id}/decisions/{decisionId}", s.getDecision)
	s.mux.HandleFunc("POST /v1/sessions/{id}/decisions/{decisionId}", s.answerDecision)
	s.approvalRoutes()
	s.notificationRoutes()
	s.pluginRoutes()
	// MCP Apps (P2.4): the fleet projection, the app catalogue and the
	// document host. The httpapi edge keeps the more specific /v1/mcp routes
	// (sign-in, callback) and forwards the rest here.
	s.mcpAppRoutes()
	s.mux.HandleFunc("GET /v1/triggers", s.listTriggers)
	s.mux.HandleFunc("POST /v1/triggers/{id}/fire", s.fireTrigger)
	s.mux.HandleFunc("GET /v1/subagents", s.listSubagents)
	s.mux.HandleFunc("POST /v1/sessions/{id}/subagents", s.spawnSubagent)
	s.mux.HandleFunc("GET /v1/subagents/{id}", s.getSubagent)
	// Tenancy surface (change keel-ga §2): who am I, provisioning, quota.
	s.registerTenancyRoutes()
}

// --- shared helpers ------------------------------------------------------

// tenantFrom reads X-Tenant-ID. The auth middleware injects it; a direct API
// caller supplies it. An absent or malformed header is 401, never an implicit
// default tenant.
func tenantFrom(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	raw := strings.TrimSpace(r.Header.Get("X-Tenant-ID"))
	if raw == "" {
		writeErr(w, http.StatusUnauthorized, errors.New("X-Tenant-ID is required"))
		return uuid.Nil, false
	}
	tenantID, err := uuid.Parse(raw)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, fmt.Errorf("X-Tenant-ID is not a UUID: %w", err))
		return uuid.Nil, false
	}
	return tenantID, true
}

func sessionFrom(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("bad session id: %w", err))
		return uuid.Nil, false
	}
	return sessionID, true
}

func decodeBody(w http.ResponseWriter, r *http.Request, limit int64, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		if errors.Is(err, http.ErrBodyReadAfterClose) {
			return errors.New("body closed")
		}
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// writeErr is the product API's error body. Like httpapi's, it masks
// credential-shaped substrings: the text is composed somewhere else, and this is
// the one place every agent API error passes through.
func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": logsafe.Redact(err.Error())})
}

// --- capabilities --------------------------------------------------------

// Capabilities is the discovery document. Clients downgrade on false flags
// instead of guessing at the server version.
type Capabilities struct {
	APIVersion     string          `json:"apiVersion"`
	ServiceVersion string          `json:"serviceVersion"`
	Features       map[string]bool `json:"features"`
	Limits         map[string]int  `json:"limits"`
	Hooks          []string        `json:"hooks"`
	// Plugins is the enabled plugin ids of the current inventory, and
	// PluginRevision is its change identity: an enable/disable/install moves it,
	// so a client can see hot plugin changes without polling the full inventory.
	Plugins        []string `json:"plugins,omitempty"`
	PluginRevision int64    `json:"pluginRevision,omitempty"`
	// MCPApps is the MCP Apps block (P2.5): the deployed sandbox-origin
	// enhancement, the catalogue generation and the app count. It is present
	// only when the app surface is wired; the feature flags below are the
	// stable booleans clients gate on.
	MCPApps *MCPAppsCapabilities `json:"mcpApps,omitempty"`
}

// MCPAppsCapabilities is the MCP Apps discovery block.
type MCPAppsCapabilities struct {
	// SandboxOrigin is the configured second origin of the View sandbox
	// (empty when the deployment serves the single-origin opaque-origin form).
	SandboxOrigin string `json:"sandboxOrigin,omitempty"`
	Generation    int64  `json:"generation"`
	Apps          int    `json:"apps"`
}

func (s *Server) capabilities(w http.ResponseWriter, _ *http.Request) {
	features := map[string]bool{
		"sessions.list":   s.cfg.Sessions != nil,
		"sessions.cancel": s.cfg.Cancel != nil,
		"decisions":       s.cfg.Decisions != nil,
		"partials":        s.cfg.Partials != nil,
		"plugins":         s.cfg.Plugins != nil,
		"plugins.manage":  s.cfg.PluginManager != nil,
		"plugins.audit":   s.cfg.PluginManager != nil,
		// plugins.approvals gates a capability this deployment may not serve: the
		// manager without the decision primitive refuses every lifecycle write
		// (fail closed), so a client must be able to see that before offering the
		// control. plugins.bridge is the plugin-UI capability channel.
		"plugins.approvals": s.cfg.PluginManager != nil && s.cfg.Decisions != nil,
		"plugins.bridge":    s.cfg.PluginManager != nil,
		"triggers":          s.cfg.Triggers != nil,
		"subagents":         s.cfg.Subagents != nil,
		"hooks":             s.cfg.Plugins != nil,
		// Settings is its own surface: the model write path and secret storage
		// are separately discoverable, so a client can hide a section this
		// deployment does not serve.
		"settings.models":  s.modelSettingsWired(),
		"settings.secrets": s.cfg.SecretStore != nil,
		// MCP Apps (change feat-mcp-apps-events): the app catalogue and the
		// document host exist only when the host registry is wired. The sandbox
		// origin is a separate capability because it is a deployment choice:
		// false means the client uses the opaque-origin form, which is not a
		// degraded experience, just the single-origin one.
		"mcp.apps":               s.cfg.MCPApps != nil,
		"mcp.apps.sandboxOrigin": s.cfg.MCPApps != nil && s.cfg.MCPAppsSandboxOrigin != "",
		// MCP events (P4): subscriptions/listen and notification routing. False
		// until the dispatcher is wired; clients then keep the legacy behavior.
		"mcp.events": s.cfg.MCPEvents,
	}
	hooks := make([]string, 0, len(agentext.HookEvents()))
	for _, event := range agentext.HookEvents() {
		hooks = append(hooks, string(event))
	}
	capabilities := Capabilities{
		APIVersion:     APIVersion,
		ServiceVersion: Version,
		Features:       features,
		Limits: map[string]int{
			"sessionsPageDefault": defaultSessionsLimit,
			"sessionsPageMax":     maxSessionsLimit,
			"subagentsPageMax":    200,
		},
		Hooks: hooks,
	}
	if s.cfg.PluginManager != nil {
		inventory := s.cfg.PluginManager.Snapshot()
		capabilities.Plugins = inventory.Enabled
		capabilities.PluginRevision = inventory.Revision
		if capabilities.Plugins == nil {
			capabilities.Plugins = []string{}
		}
	}
	if s.cfg.MCPApps != nil {
		block := &MCPAppsCapabilities{SandboxOrigin: s.cfg.MCPAppsSandboxOrigin}
		if surface, published := s.cfg.MCPApps.Surface(); published {
			block.Generation = surface.Generation
			block.Apps = len(surface.Apps)
		}
		capabilities.MCPApps = block
	}
	writeJSON(w, http.StatusOK, capabilities)
}

// queryInt parses an optional bounded integer query parameter.
func queryInt(r *http.Request, name string, def, min, max int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	if value < min || value > max {
		return 0, fmt.Errorf("%s must be between %d and %d", name, min, max)
	}
	return value, nil
}
