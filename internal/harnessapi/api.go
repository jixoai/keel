// Package harnessapi is the product-layer HarnessAPI (keel-agent): the /v1 surface
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
//     (internal/harnessapi/plugins.go: inventory, install, enable/disable/
//     uninstall/update, audit);
//   - the settings surface (internal/harnessapi/settings.go): the tenant model
//     configuration write path, secret storage and the plugin inventory —
//     separate from the workspace, mounted under /v1/settings/.
//
// Authentication is NOT implemented here: it is a service-layer concern
// (cmd/keel's cookie/passphrase middleware). This package reads the
// X-Tenant-ID the middleware (or an authenticated API client) supplies, exactly
// like the kernel does. The kernel never learns about cookies, passwords or the
// HarnessAPI's version.
package harnessapi

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
	"github.com/jixoai/keel/internal/approval"
	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/logsafe"
	"github.com/jixoai/keel/internal/notify"
	"github.com/jixoai/keel/internal/routemux"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/tasklog"
)

// Version is the HarnessAPI service version reported by /v1/capabilities.
const Version = "keel-agent/0.1.0"

// APIVersion is the URL prefix contract. New capabilities are additive; a
// breaking shape change would need /v2.
const APIVersion = "v1"

// ErrSessionNotFound is the honest 404 of the product API: the session does not
// exist for this tenant (a foreign tenant's session answers the same way).
var ErrSessionNotFound = errors.New("harnessapi: session not found")

// ErrExternalSession reports that the session is owned by an external backend:
// it has no native run to cancel (409).
var ErrExternalSession = errors.New("harnessapi: session is owned by an external backend")

// CancelFunc cancels the session's active run. The adapter lives in the hosting
// process (cmd/keel) because it needs the run seam and the session check.
type CancelFunc func(ctx context.Context, tenantID, sessionID uuid.UUID) (harnessext.CancelResult, error)

// ParentResolver supplies the parent session of a sub-agent session (the
// in-process registry of the sub-agent runner).
type ParentResolver interface {
	ParentOf(sessionID uuid.UUID) (string, bool)
}

// TeamTaskLister reads the member task cards linked to a captain session. It
// is a read-only projection over task_cards: the cards themselves live in the
// child sessions' ledgers, linked by parentTaskId. Nil answers 503.
type TeamTaskLister interface {
	ListTeamTasks(ctx context.Context, tenantID, sessionID uuid.UUID, limit int, after string) (tasklog.CardPage, error)
}

// Config wires the product API.
type Config struct {
	// Sessions lists sessions (PG-backed in production, a fake in tests). Nil
	// answers 503 on the list endpoint.
	Sessions SessionLister
	// TeamTasks reads the member task cards linked to a captain session
	// (GET /v1/sessions/{id}/team-tasks). It is a read-only projection; nil
	// answers 503 and the capability is reported false.
	TeamTasks TeamTaskLister
	// Cancel stops a session's active run. Nil answers 501 (the endpoint exists
	// but the deployment has no cancel seam).
	Cancel CancelFunc
	// Decisions is the approval primitive. Nil answers 503.
	Decisions *harnessext.DecisionBroker
	// Approvals records and reads approval provenance (who answered, over which
	// channel, validated/escape). Nil keeps the decision routes as they are and
	// omits provenance from their answers.
	Approvals *approval.Service
	// Notifications is the durable notification primitive behind the notify
	// tool and the /v1/notifications routes. Nil answers 503 there.
	Notifications *notify.Service
	// Partials is the in-process partial hub (non-persistent bypass). Nil makes
	// ?partials=1 a no-op (the flag is then reported false by capabilities).
	Partials *harnessext.PartialHub
	// Plugins is the assembled extension runtime. Nil answers 503 on
	// GET /v1/plugins.
	Plugins *harnessext.Runtime
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
	Triggers *harnessext.TriggerEngine
	// Subagents is the sub-agent runner. Nil answers 503 on the sub-agent
	// routes.
	Subagents *harnessext.SubagentRunner
	// Hooks is the hook bus (detailed subscription stats on GET /v1/plugins).
	Hooks *harnessext.HookBus
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
	// ProviderProber powers POST /v1/providers/{id}/test: one minimal upstream
	// request through the server's own credential resolution. It is optional;
	// nil answers 501. The probe never mutates configuration and never returns a
	// credential value.
	ProviderProber ProviderProber
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
	// ConfigFileStatus, when set, reports the active config.toml file version,
	// hash and rotation facts on GET /v1/capabilities. It describes the file
	// (and its capture setting), not the run's loadout-derived configHash:
	// AnchorEventSeq is 0 until a rotation event anchors the file. Nil function
	// or nil result omits the block (a deployment without the file loader).
	ConfigFileStatus func() *ConfigFileCapabilities
	// Skills is the tenant-scoped skill library behind /v1/settings/skills and
	// the `skills` aggregate tool's manage/sources actions. Nil answers 501 on
	// the settings routes (the tool keeps its constant name).
	Skills SkillLibrary
	// SessionFiles lists the open session's file manifest for the composer's
	// `$` file group. It is also the one projection behind
	// GET /v1/sessions/{id}/deliverables: a deliverable is a manifest row, not
	// a second file truth. Nil omits the group and the delivery read.
	SessionFiles SessionFileLister
	// SessionTasks lists the session's task cards for the composer's `$` task
	// group. Nil omits the group from the capability answer.
	SessionTasks SessionTaskLister
	// SessionSearch is the bounded transcript/session search behind
	// GET /v1/sessions/search. Nil answers 501 and reports the capability false
	// (a missing access surface is the separate 503 path).
	SessionSearch SessionSearcher
	// ExternalSessions reads the session's external marks (state_owner,
	// external_ref) for GET /v1/sessions/{id}/external/projection (change
	// keel-oar-renderer). Nil answers 501 oar_not_wired: without the marks there
	// is no journal handle to read.
	ExternalSessions SessionExternalReader
	// OARJournalDir is the directory of the OAR record journals
	// (session-<ref>.ndjson). Empty answers 501 oar_not_wired on the projection
	// route: a deployment without OAR must not serve an empty projection as if
	// the capability were present.
	OARJournalDir string
	// SessionExport is the bounded transcript export behind
	// GET /v1/sessions/{id}/export. Nil answers 501 and reports the capability
	// false (a missing access surface is the separate 503 path). It never dumps
	// a whole session: every page carries a row and byte bound.
	SessionExport SessionExporter
	// TaskPanel is the panel's read of one session's task memory (tree, member
	// cards, attachment references, event stream). It is a projection over
	// task_cards/task_events; nil answers 503 on GET
	// /v1/sessions/{id}/task-panel and reports the capability false.
	TaskPanel TaskPanelLister
	// FilePreview is the panel's resource preview: the session file manifest's
	// bounded read. Nil omits GET /v1/sessions/{id}/file-preview.
	FilePreview SessionFileReader
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
	// Provisioner is the tenant provisioning surface (POST /v1/admin/tenants).
	// Nil answers 501: a deployment without it has no operator path, which is a
	// configuration choice, not a bug.
	Provisioner Provisioner
	// Quota reports the tenant's limits and current usage (GET /v1/quota). Nil
	// answers 501.
	Quota QuotaReporter
	// AdminKey decides whether a request carries the deployment's operator key.
	// Nil means "KEEL_ADMIN_KEY only".
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
	// MCPAppMetrics records app bridge frames and document fetches (P4.7). Nil
	// is a no-op; the labels are bounded by the metric set, never by session or
	// app instance.
	MCPAppMetrics MCPAppMetrics
	// MCPAppRuntime executes the app bridge's outbound calls (tools/call and
	// resources/read) against the live MCP fleet. Nil answers 501 on the proxy
	// routes: a deployment without MCP connections has nothing to proxy to.
	MCPAppRuntime MCPAppRuntime
	// MCPAppAudit is the durable app audit trail (P3.7). Nil keeps the actions
	// working with a structured-log warning; the audit read route answers 503.
	// Refusal audits are best-effort either way.
	MCPAppAudit MCPAppAuditStore
	// MCPAppContexts persists ui/update-model-context values (P5.5). Nil answers
	// -32601 on the context route and reports mcp.apps.context=false: a
	// deployment without the store must not pretend the context was recorded.
	MCPAppContexts MCPAppContextStore
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
	// Identity is the identity store behind /v1/accounts, /v1/permission-groups,
	// /v1/bots and /v1/team-bots (change keel-bot-identity-access, A4.1). Nil
	// makes those routes answer 503.
	Identity *store.Store
	// Access is the authorization seam (accesswire.Bridge in production). Nil
	// makes every identity route fail closed (503): there is no "allow all"
	// fallback and no list endpoint that skips the relation filter.
	Access Access
	// IdentityEvents is the tenant-local invalidation hub behind
	// GET /v1/identity/events. Nil builds a private hub: the identity handlers
	// and the stream must share one instance, which this package does by default.
	// A host that also mutates identity rows elsewhere may inject its own. The
	// hub is process-local by design (keel serve is one process); a multi-replica
	// deployment would need a shared fan-out in front of it.
	IdentityEvents *IdentityEvents
	// IdentityEventsHeartbeat is the SSE keepalive interval. Zero uses 15s,
	// which stays under a typical 75s proxy idle timeout.
	IdentityEventsHeartbeat time.Duration
	// StartBotSession starts one bot-owned session (bot invoke and team start).
	// The serve layer owns the turn starter and the ownership write. Nil answers
	// 501 on the invoke/start routes.
	StartBotSession func(ctx context.Context, request BotSessionRequest) (BotSessionResult, error)
	// ToolExists answers whether a tool id is available to this tenant, at the
	// moment a Bot's Harness is saved. Nil skips the existence check and keeps
	// the shape rules (a deployment without a live tool surface must not refuse
	// every save). The host binds it to the assembled plugin/tool surface, so an
	// uninstalled plugin stops validating new saves without a restart.
	ToolExists func(toolID string) bool
	Logger     *slog.Logger
	MaxBody    int64
}

// Server is the HarnessAPI HTTP handler. Mount it on the runtime's mux under
// /v1/ (specific existing /v1 routes keep precedence in the outer mux).
type Server struct {
	cfg Config
	mux *routemux.Mux
	// settingsMu serializes the model-settings write path (read current →
	// validate → store → apply). Without it two concurrent writes can interleave
	// as store(B), apply(A): the row says B while the generation serves A. The
	// window is small (the store write and the apply are back to back) but the
	// inconsistency is real, and one lock is cheaper than explaining it.
	settingsMu sync.Mutex
	// mcpAppLimits is the per-app bridge budget (P3.8): 200 requests/s and 4
	// concurrent proxy requests per app.
	mcpAppLimits *mcpAppLimiter
	// mcpAuditMu guards the rejection-audit sampler. A rate/concurrency/size
	// refusal is zero-cost to produce, so its audit row is sampled to at most
	// one per (app, action, second); the row carries the suppressed count.
	mcpAuditMu      sync.Mutex
	mcpAuditSamples map[string]*mcpAuditSample
	// identityEvents is the invalidation hub the identity handlers publish to and
	// GET /v1/identity/events tails. It is always set by New.
	identityEvents *IdentityEvents
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
	events := cfg.IdentityEvents
	if events == nil {
		events = NewIdentityEvents()
	}
	s := &Server{
		cfg:             cfg,
		mux:             routemux.New(),
		mcpAppLimits:    newMCPAppLimiter(cfg.MCPAppNow),
		mcpAuditSamples: make(map[string]*mcpAuditSample),
		identityEvents:  events,
	}
	s.routes()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Routes returns every pattern registered on the product mux. The host's
// startup permission-coverage check enumerates it alongside the runtime mux.
func (s *Server) Routes() []string { return s.mux.Patterns() }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /v1/capabilities", s.capabilities)
	s.mux.HandleFunc("GET /v1/providers", s.listProviders)
	s.mux.HandleFunc("POST /v1/providers/{id}/test", s.testProvider)
	s.mux.HandleFunc("GET /v1/models", s.listModels)
	s.settingsRoutes()
	s.skillsRoutes()
	s.completionRoutes()
	s.taskPanelRoutes()
	s.sessionQueryRoutes()
	s.deliverableRoutes()
	s.mux.HandleFunc("GET /v1/sessions", s.listSessions)
	s.mux.HandleFunc("GET /v1/sessions/{id}/team-tasks", s.listTeamTasks)
	// The external run projection (change keel-oar-renderer): read-only, always
	// registered so an unwired deployment can answer 501 instead of 404.
	s.mux.HandleFunc("GET /v1/sessions/{id}/external/projection", s.externalProjection)
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
	s.mux.HandleFunc("POST /v1/sessions/{id}/fork", s.forkSubagent)
	s.mux.HandleFunc("POST /v1/sessions/{id}/resume", s.resumeSubagent)
	s.mux.HandleFunc("POST /v1/subagents/{id}/cancel", s.cancelSubagent)
	s.mux.HandleFunc("GET /v1/subagents/{id}", s.getSubagent)
	// Tenancy surface (change keel-ga §2): who am I, provisioning, quota.
	s.registerTenancyRoutes()
	// Identity surface (change keel-bot-identity-access, A4.1): accounts,
	// permission groups, bots, team bots and bot keys.
	s.identityRoutes()
	// Audit read surface (A4.4c): the 记录 page's GET /v1/audit/events.
	s.auditRoutes()
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
	// ConfigFile is the active config.toml file version and its rotation facts.
	// It is present only when the deployment loaded a file loader. It is the
	// capture-policy/file source a new attempt resolves, NOT the run's
	// loadout-derived configHash; AnchorEventSeq is 0 at boot and only becomes
	// a ledger sequence when a rotation event anchors the file.
	ConfigFile *ConfigFileCapabilities `json:"configFile,omitempty"`
}

// ConfigFileCapabilities is the config.toml discovery block. It deliberately
// omits the filesystem path (a local path is not a client fact).
type ConfigFileCapabilities struct {
	Exists         bool                `json:"exists"`
	Revision       int64               `json:"revision"`
	Hash           string              `json:"hash"`
	AnchorEventSeq int64               `json:"anchorEventSeq"`
	CaptureMode    string              `json:"captureMode,omitempty"`
	Rotations      int64               `json:"rotations"`
	LastRotation   *ConfigFileRotation `json:"lastRotation,omitempty"`
}

// ConfigFileRotation is one successful atomic rotation. AnchorEventSeq is the
// ledger sequence the caller committed for the version; PreviousRevision/Hash
// make the transition auditable.
type ConfigFileRotation struct {
	Revision         int64  `json:"revision"`
	Hash             string `json:"hash"`
	AnchorEventSeq   int64  `json:"anchorEventSeq"`
	PreviousRevision int64  `json:"previousRevision"`
	PreviousHash     string `json:"previousHash"`
	RotatedAt        string `json:"rotatedAt"`
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
		"sessions.list":   s.cfg.Sessions != nil && s.cfg.Access != nil,
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
		"team.tasks":        s.cfg.TeamTasks != nil,
		"hooks":             s.cfg.Plugins != nil,
		// Settings is its own surface: the model write path and secret storage
		// are separately discoverable, so a client can hide a section this
		// deployment does not serve.
		"settings.models":  s.modelSettingsWired(),
		"settings.secrets": s.cfg.SecretStore != nil,
		// The skill library is its own settings surface and the backend of the
		// `skills` tool's manage/sources actions. The completion flag reports the
		// data a `$` group needs (the shell decides which groups to draw).
		"settings.skills":   s.cfg.Skills != nil,
		"skills.completion": s.cfg.SessionFiles != nil || s.cfg.SessionTasks != nil,
		// task.panel: the run surface's task tree / event stream / resource
		// preview. A deployment without the task-log projection leaves the
		// panel's read out, and the surface says so instead of drawing an
		// empty tree.
		"task.panel":          s.cfg.TaskPanel != nil,
		"session.filePreview": s.cfg.FilePreview != nil,
		// The W12 product reads: bounded transcript search/export and the
		// manifest-backed delivery projection. Each flag follows the seam it
		// needs; a deployment without one leaves the client's entry out.
		"session.search":       s.cfg.SessionSearch != nil && s.cfg.Access != nil,
		"session.export":       s.cfg.SessionExport != nil && s.cfg.Access != nil,
		"session.deliverables": s.cfg.SessionFiles != nil && s.cfg.Access != nil,
		// MCP Apps (change feat-mcp-apps-events): the app catalogue and the
		// document host exist only when the host registry is wired. The sandbox
		// origin is a separate capability because it is a deployment choice:
		// false means the client uses the opaque-origin form, which is not a
		// degraded experience, just the single-origin one.
		"mcp.apps":               s.cfg.MCPApps != nil,
		"mcp.apps.sandboxOrigin": s.cfg.MCPApps != nil && s.cfg.MCPAppsSandboxOrigin != "",
		// mcp.apps.context is true only when ui/update-model-context is actually
		// persisted: the View must keep hostCapabilities.updateModelContext off
		// when this is false (spec「未接线能力不假装」).
		"mcp.apps.context": s.cfg.MCPApps != nil && s.cfg.MCPAppContexts != nil,
		// Per-action bridge capabilities. They follow the seam each action
		// actually needs, so the View offers only methods this deployment can
		// execute: tools/resources need the outbound runtime, a message needs
		// both the input write path and the decision gate, and a link needs the
		// decision gate (the host front end opens the confirmed URL itself).
		"mcp.apps.tools":     s.cfg.MCPAppRuntime != nil,
		"mcp.apps.resources": s.cfg.MCPAppRuntime != nil,
		"mcp.apps.message":   s.cfg.MCPAppInputCommit != nil && s.cfg.Decisions != nil,
		"mcp.apps.links":     s.cfg.Decisions != nil,
		// MCP events (P4): subscriptions/listen and notification routing. False
		// until the dispatcher is wired; clients then keep the legacy behavior.
		"mcp.events": s.cfg.MCPEvents,
	}
	hooks := make([]string, 0, len(harnessext.HookEvents()))
	for _, event := range harnessext.HookEvents() {
		hooks = append(hooks, string(event))
	}
	capabilities := Capabilities{
		APIVersion:     APIVersion,
		ServiceVersion: Version,
		Features:       features,
		Limits: map[string]int{
			"sessionsPageDefault":  defaultSessionsLimit,
			"sessionsPageMax":      maxSessionsLimit,
			"subagentsPageMax":     200,
			"teamTasksPageDefault": tasklog.DefaultTeamTaskLimit,
			"teamTasksPageMax":     tasklog.MaxTeamTaskLimit,
			"searchPageDefault":    defaultSearchLimit,
			"searchPageMax":        maxSearchLimit,
			"exportPageDefault":    defaultExportLimit,
			"exportPageMax":        maxExportLimit,
			"exportPageBytes":      exportPageBytes,
			"deliverablesDefault":  defaultDeliverablesLimit,
			"deliverablesMax":      maxDeliverablesLimit,
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
	if s.cfg.ConfigFileStatus != nil {
		if block := s.cfg.ConfigFileStatus(); block != nil {
			capabilities.ConfigFile = block
		}
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
