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
//     (plugins/triggers/sub-agents);
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
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentext"
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
	// Partials is the in-process partial hub (non-persistent bypass). Nil makes
	// ?partials=1 a no-op (the flag is then reported false by capabilities).
	Partials *agentext.PartialHub
	// Plugins is the assembled extension runtime. Nil answers 503 on
	// GET /v1/plugins.
	Plugins *agentext.Runtime
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
	// RunStatus optionally overrides the durable run status (a cancelled run
	// settles as failed in the kernel policy table; the product snapshot
	// remembers the cancellation).
	RunStatus func(tenantID, sessionID uuid.UUID) (string, bool)
	// SubagentWaitDefault bounds the synchronous wait of a sub-agent spawn over
	// HTTP when the request does not set a budget (default 60s).
	SubagentWaitDefault time.Duration
	Logger              *slog.Logger
	MaxBody             int64
}

// Server is the AgentAPI HTTP handler. Mount it on the runtime's mux under
// /v1/ (specific existing /v1 routes keep precedence in the outer mux).
type Server struct {
	cfg Config
	mux *http.ServeMux
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
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
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
	s.mux.HandleFunc("POST /v1/sessions/{id}/cancel", s.cancelSession)
	s.mux.HandleFunc("GET /v1/sessions/{id}/decisions", s.listDecisions)
	s.mux.HandleFunc("POST /v1/sessions/{id}/decisions", s.openDecision)
	s.mux.HandleFunc("GET /v1/sessions/{id}/decisions/{decisionId}", s.getDecision)
	s.mux.HandleFunc("POST /v1/sessions/{id}/decisions/{decisionId}", s.answerDecision)
	s.mux.HandleFunc("GET /v1/plugins", s.listPlugins)
	s.mux.HandleFunc("GET /v1/triggers", s.listTriggers)
	s.mux.HandleFunc("POST /v1/triggers/{id}/fire", s.fireTrigger)
	s.mux.HandleFunc("GET /v1/subagents", s.listSubagents)
	s.mux.HandleFunc("POST /v1/sessions/{id}/subagents", s.spawnSubagent)
	s.mux.HandleFunc("GET /v1/subagents/{id}", s.getSubagent)
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

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": err.Error()})
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
}

func (s *Server) capabilities(w http.ResponseWriter, _ *http.Request) {
	features := map[string]bool{
		"sessions.list":   s.cfg.Sessions != nil,
		"sessions.cancel": s.cfg.Cancel != nil,
		"decisions":       s.cfg.Decisions != nil,
		"partials":        s.cfg.Partials != nil,
		"plugins":         s.cfg.Plugins != nil,
		"triggers":        s.cfg.Triggers != nil,
		"subagents":       s.cfg.Subagents != nil,
		"hooks":           s.cfg.Plugins != nil,
		// Settings is its own surface: the model write path and secret storage
		// are separately discoverable, so a client can hide a section this
		// deployment does not serve.
		"settings.models":  s.modelSettingsWired(),
		"settings.secrets": s.cfg.SecretStore != nil,
	}
	hooks := make([]string, 0, len(agentext.HookEvents()))
	for _, event := range agentext.HookEvents() {
		hooks = append(hooks, string(event))
	}
	writeJSON(w, http.StatusOK, Capabilities{
		APIVersion:     APIVersion,
		ServiceVersion: Version,
		Features:       features,
		Limits: map[string]int{
			"sessionsPageDefault": defaultSessionsLimit,
			"sessionsPageMax":     maxSessionsLimit,
			"subagentsPageMax":    200,
		},
		Hooks: hooks,
	})
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
