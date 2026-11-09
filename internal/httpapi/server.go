// Package httpapi is the runtime's HTTP surface: sessions, events (JSON + SSE),
// file upload/download, and health/metrics. It is intentionally thin — all
// durable facts live in PostgreSQL, and the only in-memory structures are the
// SSE wakeup hub and the bounded caches in the stream package.
package httpapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/blob"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/stream"
)

// Config wires the server's dependencies.
type Config struct {
	Store     *store.Store
	Hub       *Hub
	Blobs     *blob.Reader
	Uploader  *blob.Uploader
	Scheduler *stream.Scheduler
	Admitter  *stream.Admitter
	Limiter   *stream.Limiter
	Metrics   *stream.Metrics
	Logger    *slog.Logger
	CASDir    string
	// Edge is the contract kernel's commit seam for client input (edge-scoped,
	// idempotent). It is satisfied by *kernel.Kernel; the server never holds a
	// raw event-table write handle. POST /v1/sessions/{id}/events and
	// /v1/sessions/{id}/input answer 503 when it is not wired.
	Edge kernel.Committer
	// Events is the contract kernel's read seam. It carries the tenant
	// boundary, so GET /v1/sessions/{id}/events answers 404 for a foreign or
	// missing session instead of 200 + an empty list.
	Events kernel.Reader
	// ValidateSelection, when set, validates a selection object before it is
	// persisted with the input. The core shell stores the fact; the product layer
	// owns the vocabulary, so an unwired validator accepts nothing but absent
	// fields. `keel serve` binds the seam to the model service of the CURRENT
	// generation (resolved per call, never captured at boot), so a settings write
	// changes what the edge accepts without a restart.
	ValidateSelection func(json.RawMessage) error
	// Loadouts is the read-only run-loadout query (the loadout revision and
	// fencing epoch a run finished under). It is a separate endpoint on
	// purpose: adding fields to the run-status body would change an existing
	// response shape. Nil answers 503.
	Loadouts LoadoutReader
	// AdminKey authenticates the tenant admin API. Empty falls back to
	// KEEL_ADMIN_KEY at request time; when neither is set the admin API is
	// disabled (503) rather than open.
	AdminKey string
	// MetricsExporter backs GET /metrics (Prometheus text). Nil returns 503.
	MetricsExporter MetricsExporter
	// Ready backs GET /v1/ready. Nil means the process is ready as soon as it
	// is listening.
	Ready ReadyChecker
	// Drain coordinates SIGTERM drainage. Nil disables drain behavior. A
	// *ModeDrainer additionally distinguishes shutdown from handoff drains
	// (see runtime.go).
	Drain Drainer
	// Control backs the runtime admin routes (GET /v1/runtime/status,
	// POST /v1/runtime/drain). Nil answers 503 on those routes.
	Control RuntimeControl
	// MCPOAuth enables the MCP sign-in endpoints; nil answers 503.
	MCPOAuth *MCPOAuth
	// External is the external-gateway surface (AgentBackend sessions). Nil
	// answers 503 on every /v1/external route; a tenant document that declares
	// no external backend also leaves it nil.
	External ExternalSessions
	// Product is the product-layer AgentAPI (keel-agent, internal/agentapi)
	// mounted under /v1/. Specific routes registered here keep precedence; the
	// subtree handler only sees paths this server does not own. Nil disables it.
	Product http.Handler
	// Partials is the token-level bypass seam for the SSE handler. Nil keeps
	// ?partials=1 a no-op.
	Partials PartialSource
	// MaxBody caps JSON request bodies (uploads use MaxUpload).
	MaxBody   int64
	MaxUpload int64
}

// errKernelUnwired is the explicit failure of an edge write endpoint when the
// contract kernel was not injected. The legacy store write path is not a
// fallback: events must be committed through the kernel (contract-kernel).
var errKernelUnwired = errors.New("contract kernel is not wired for the edge endpoints")

// LoadoutReader answers which service loadout a run pinned. It is the kernel's
// read-only run-pin query (satisfied by *kernel.Kernel).
type LoadoutReader interface {
	RunPin(ctx context.Context, scope kernel.Scope) (kernel.RunPin, error)
}

// Server is the HTTP handler.
type Server struct {
	cfg Config
	mux *http.ServeMux
}

// New builds the server and its routes.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = &stream.Metrics{}
	}
	if cfg.MaxBody == 0 {
		cfg.MaxBody = 1 << 20
	}
	if cfg.MaxUpload == 0 {
		cfg.MaxUpload = 64 << 20
	}
	if cfg.Edge == nil || cfg.Events == nil {
		cfg.Logger.Warn("httpapi: contract kernel is not wired",
			"detail", "POST /v1/sessions/{id}/events and /input answer 503; GET /v1/sessions/{id}/events answers 503")
	}
	if cfg.CASDir != "" && cfg.Blobs != nil {
		cfg.Blobs.CASDir = cfg.CASDir
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.routes()
	return s
}

// ServeHTTP implements http.Handler. While draining, new API requests receive
// 503 + Retry-After; probes and /metrics stay reachable so the orchestrator can
// observe the drain.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.serveLogged(w, r) }

var sseExitLogs int64

func (s *Server) routes() {
	s.mux.HandleFunc("GET /v1/health", s.health)
	s.mux.HandleFunc("GET /v1/ready", s.ready)
	s.mux.HandleFunc("GET /v1/metrics", s.metrics)
	s.mux.HandleFunc("GET /metrics", s.prometheusMetrics)
	s.mux.HandleFunc("POST /v1/tenants", s.createTenant)
	s.registerAdminRoutes()
	s.registerRuntimeRoutes()
	s.registerExternalRoutes()
	s.mux.HandleFunc("POST /v1/sessions", s.createSession)
	s.mux.HandleFunc("POST /v1/sessions/{id}/input", s.input)
	s.mux.HandleFunc("POST /v1/sessions/{id}/run", s.triggerRun)
	s.mux.HandleFunc("GET /v1/sessions/{id}/run", s.runStatus)
	s.mux.HandleFunc("GET /v1/sessions/{id}/run/loadout", s.runLoadout)
	s.mux.HandleFunc("POST /v1/sessions/{id}/events", s.appendEvent)
	s.mux.HandleFunc("GET /v1/sessions/{id}/events", s.listEvents)
	s.mux.HandleFunc("GET /v1/sessions/{id}/events/stream", s.streamEvents)
	s.mux.HandleFunc("POST /v1/files", s.upload)
	s.mux.HandleFunc("GET /v1/files/{id}/meta", s.fileMeta)
	s.mux.HandleFunc("GET /v1/files/{id}", s.file)
	s.mux.HandleFunc("POST /v1/mcp/servers/{name}/signin", s.mcpSignIn)
	s.mux.HandleFunc("/v1/mcp/callback", s.mcpCallback)
	// Product-layer AgentAPI (keel-agent): its own mux owns the remaining /v1
	// patterns. The more specific patterns above win, so the existing edge
	// contract is unchanged.
	if s.cfg.Product != nil {
		s.mux.Handle("/v1/", s.cfg.Product)
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	writeJSON(w, 200, map[string]any{
		"ok":        true,
		"pid":       pid,
		"bootId":    bootID,
		"goroutine": runtime.NumGoroutine(),
		"heapMB":    float64(m.HeapAlloc) / 1e6,
		"rssMB":     float64(m.Sys) / 1e6,
	})
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	inflight, control, classes, rejected := s.cfg.Scheduler.Snapshot()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	writeJSON(w, 200, map[string]any{
		"stream":    s.cfg.Metrics.Snapshot(),
		"inflight":  inflight,
		"control":   control,
		"classes":   map[string]int{"small": classes[0], "medium": classes[1], "large": classes[2]},
		"rejected":  rejected,
		"goroutine": runtime.NumGoroutine(),
		"heapMB":    float64(m.HeapAlloc) / 1e6,
		"sysMB":     float64(m.Sys) / 1e6,
	})
}

// tenantFrom reads the X-Tenant-ID header.
func tenantFrom(r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	return id, err == nil
}

// createTenant provisions a tenant. It is an operator action, not a tenant
// self-service endpoint: an unauthenticated caller must not be able to mint a
// tenant id that later owns credentials and events. Authentication reuses the
// admin API semantics (503 when KEEL_ADMIN_KEY is unset, 401 otherwise).
func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	id := uuid.New()
	if raw := r.Header.Get("X-Tenant-ID"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			writeErr(w, 400, fmt.Errorf("bad tenant id: %w", err))
			return
		}
		id = parsed
	}
	if err := s.cfg.Store.CreateTenant(r.Context(), id, body.Name); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 201, map[string]any{"tenantId": id, "name": body.Name})
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, 401, errors.New("X-Tenant-ID is required"))
		return
	}
	var body struct {
		SessionID string `json:"sessionId"`
	}
	_ = decodeBody(w, r, s.cfg.MaxBody, &body)
	id := uuid.New()
	if body.SessionID != "" {
		parsed, err := uuid.Parse(body.SessionID)
		if err != nil {
			writeErr(w, 400, fmt.Errorf("bad sessionId: %w", err))
			return
		}
		id = parsed
	}
	if err := s.cfg.Store.CreateSession(r.Context(), tenantID, id); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 201, map[string]any{"sessionId": id})
}

func (s *Server) input(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, 401, errors.New("X-Tenant-ID is required"))
		return
	}
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, fmt.Errorf("bad session id: %w", err))
		return
	}
	var body struct {
		Text      string `json:"text"`
		RequestID string `json:"requestId"`
		// Delivery selects how the loop consumes this input: "queue" (default)
		// waits for the next run, "guide" is injected into the running turn at a
		// tool boundary.
		Delivery string `json:"delivery"`
		// Selection names the provider/model this input asks its answer from. The
		// shell only stores the fact (verbatim); the product vocabulary is
		// validated through Config.ValidateSelection before the commit.
		Selection json.RawMessage `json:"selection"`
	}
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeErr(w, 400, errors.New("text is required"))
		return
	}
	// A selection the server cannot vet is refused before any store IO or
	// commit: the kernel would also reject a non-object, but only after the
	// request entered the commit path.
	if len(body.Selection) > 0 {
		if err := s.vetSelection(body.Selection); err != nil {
			writeErr(w, 400, err)
			return
		}
	}
	delivery := body.Delivery
	switch delivery {
	case "":
		// Absent means queue (the pre-delivery contract). The field is not
		// injected: a client that never sends delivery must keep a byte-identical
		// payload, or its idempotent retry after a crash becomes a key conflict.
	case kernel.DeliveryQueue:
	case kernel.DeliveryGuide:
		// The downgrade rule: a guide input steers the turn that is running, so
		// it only stays a guide while an attempt is actually executing. Without
		// a live attempt there is nothing to steer and it is committed as queue
		// input — visible in the event, never silent.
		run, err := s.cfg.Store.GetRun(r.Context(), tenantID, sessionID)
		if err != nil || run.Status != kernel.RunStatusRunning {
			delivery = kernel.DeliveryQueue
		}
	default:
		writeErr(w, 400, fmt.Errorf("delivery must be %q or %q", kernel.DeliveryQueue, kernel.DeliveryGuide))
		return
	}
	requestID := body.RequestID
	if requestID == "" {
		requestID = r.Header.Get("X-Request-ID")
	}
	if requestID == "" {
		requestID = uuid.NewString()
	}
	scope := kernel.Scope{TenantID: tenantID, SessionID: sessionID}
	fields := map[string]any{"text": body.Text, "requestId": requestID}
	if delivery != "" {
		fields["delivery"] = delivery
	}
	if len(body.Selection) > 0 {
		// Verbatim: a RawMessage passes the object through the payload marshal
		// without re-encoding its fields, so the value the client sent is the
		// value in the log.
		fields["selection"] = json.RawMessage(body.Selection)
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if s.cfg.Edge == nil {
		writeErr(w, http.StatusServiceUnavailable, errKernelUnwired)
		return
	}
	// Class pre-check before the commit: an external session has no native
	// run/lease flow, so /input answers 409 instead of appending a message and
	// then failing to queue a run (M4's "commit then 500"). The kernel's edge
	// seam still records messages for external sessions (the external gateway's
	// SSE replay depends on that contract), so the refusal belongs to the
	// native client API, before anything is written.
	owner, exists, err := s.cfg.Store.SessionOwner(r.Context(), tenantID, sessionID)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if !exists {
		writeErr(w, http.StatusNotFound, errors.New("session not found"))
		return
	}
	if owner == store.StateOwnerExternal {
		writeErr(w, http.StatusConflict, errors.New("session is owned by an external backend; native input is not supported"))
		return
	}
	// Edge-scoped commit: user.message carries the requestId as its idempotency
	// key and never a fencing token (client input keeps landing during a drain;
	// the next runtime picks it up).
	receipt, err := s.cfg.Edge.Commit(r.Context(), kernel.CommitRequest{
		Scope:      scope,
		Kind:       kernel.KindUserMessage,
		RequestID:  requestID,
		Payload:    payload,
		NativeEdge: true,
	})
	if err != nil {
		writeEdgeError(w, err)
		return
	}
	// The committed event is the truth; the submissions row is client-facing
	// bookkeeping kept for the response contract. A row that predates a crash
	// between commit and bookkeeping is repaired here. The commit itself already
	// queued the run in its own transaction (kernel edge commit), so a failure
	// in this bookkeeping can no longer strand the input.
	sub, created, err := s.cfg.Store.Submit(r.Context(), tenantID, requestID, sessionID, uuid.New())
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if sub.Seq == nil {
		if err := s.cfg.Store.SetSubmissionStatus(r.Context(), tenantID, sessionID, requestID, "committed", receipt.Seq); err != nil {
			writeErr(w, 500, err)
			return
		}
		sub.Seq = &receipt.Seq
		// The response reports the state the bookkeeping now holds, not the
		// transient "pending" the row was inserted with.
		sub.Status = "committed"
	}
	// Enqueue on replays too: if the first enqueue failed after the event was
	// committed, the client's retry repairs it without creating a second event.
	if err := s.cfg.Store.EnqueueRun(r.Context(), tenantID, sessionID); err != nil {
		writeEnqueueError(w, err)
		return
	}
	status := 200
	if created {
		status = 201
	}
	writeJSON(w, status, map[string]any{
		"submissionId": sub.ID,
		"requestId":    requestID,
		"status":       sub.Status,
		"seq":          sub.Seq,
		"created":      created,
	})
}

// vetSelection checks a client-supplied selection before the input is
// committed. Object shape is the shell's own contract (the kernel persists the
// fact without interpreting it); the provider/model vocabulary belongs to the
// product layer, so a deployment without a wired validator refuses every
// selection rather than storing one it cannot honour.
func (s *Server) vetSelection(selection json.RawMessage) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(selection, &obj); err != nil || obj == nil {
		return errors.New("selection must be a JSON object")
	}
	if s.cfg.ValidateSelection == nil {
		return errors.New("selection is not supported by this server")
	}
	return s.cfg.ValidateSelection(selection)
}

// triggerRun enqueues a run without appending a user event. It is useful for
// retrying a failed run after the operator fixed the provider configuration.
// A missing (or foreign) session is a 404 and never creates a run row: an
// orphan runs row can never be claimed and would inflate the queue depth
// forever.
func (s *Server) triggerRun(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, 401, errors.New("X-Tenant-ID is required"))
		return
	}
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, fmt.Errorf("bad session id: %w", err))
		return
	}
	if err := s.cfg.Store.EnqueueRun(r.Context(), tenantID, sessionID); err != nil {
		writeEnqueueError(w, err)
		return
	}
	writeJSON(w, 202, map[string]any{"enqueued": true})
}

// runStatus reports the session's durable run state.
func (s *Server) runStatus(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, 401, errors.New("X-Tenant-ID is required"))
		return
	}
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, fmt.Errorf("bad session id: %w", err))
		return
	}
	run, err := s.cfg.Store.GetRun(r.Context(), tenantID, sessionID)
	if err != nil {
		status := 500
		if errors.Is(err, store.ErrNoRun) {
			status = 404
		}
		writeErr(w, status, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"status":     run.Status,
		"attempts":   run.Attempts,
		"cursorSeq":  run.CursorSeq,
		"leaseUntil": run.LeaseUntil,
		"lastError":  run.LastError,
	})
}

// runLoadout answers the service loadout a run pinned: the immutable snapshot
// id/revision and the graph generation that served it, plus the fencing epoch
// the run finished under. It is a read-only debug/audit endpoint; the
// run-status response keeps its existing shape. A run that settled before the
// runtime adopted loadout metadata answers zeros (it did not record a pin).
func (s *Server) runLoadout(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, 401, errors.New("X-Tenant-ID is required"))
		return
	}
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, fmt.Errorf("bad session id: %w", err))
		return
	}
	if s.cfg.Loadouts == nil {
		writeErr(w, http.StatusServiceUnavailable, errKernelUnwired)
		return
	}
	pin, err := s.cfg.Loadouts.RunPin(r.Context(), kernel.Scope{TenantID: tenantID, SessionID: sessionID})
	if err != nil {
		status := 500
		if errors.Is(err, kernel.ErrNotFound) || errors.Is(err, kernel.ErrNoRunnable) {
			status = 404
		}
		writeErr(w, status, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"loadoutId":  pin.LoadoutID,
		"revision":   pin.LoadoutRevision,
		"generation": pin.LoadoutGeneration,
		"ownerEpoch": pin.LoadoutOwnerEpoch,
	})
}

// appendEvent accepts one client event. The edge whitelist is exactly the
// kernel's edge-scoped kinds (user.message): any other kind is a 400, and a
// foreign or missing session is a 404 that does not leak existence. The
// requestId is the idempotency key: it is taken from the payload, then the
// X-Request-ID header, and generated when neither is present (a generated key
// cannot deduplicate a retry, so clients that retry must send one).
func (s *Server) appendEvent(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, 401, errors.New("X-Tenant-ID is required"))
		return
	}
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, fmt.Errorf("bad session id: %w", err))
		return
	}
	var body struct {
		Kind    string          `json:"kind"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	if body.Kind == "" {
		writeErr(w, 400, errors.New("kind is required"))
		return
	}
	if class, known := kernel.KindClass(kernel.EventKind(body.Kind)); !known || class != kernel.ClassEdge {
		writeErr(w, 400, fmt.Errorf("kind %q is not accepted from clients; the edge whitelist is %s",
			body.Kind, edgeKindList()))
		return
	}
	if len(body.Payload) == 0 {
		body.Payload = json.RawMessage(`{}`)
	}
	requestID := r.Header.Get("X-Request-ID")
	if requestID == "" {
		var meta struct {
			RequestID string `json:"requestId"`
		}
		if err := json.Unmarshal(body.Payload, &meta); err == nil {
			requestID = meta.RequestID
		}
	}
	if requestID == "" {
		requestID = uuid.NewString()
	}
	if s.cfg.Edge == nil {
		writeErr(w, http.StatusServiceUnavailable, errKernelUnwired)
		return
	}
	receipt, err := s.cfg.Edge.Commit(r.Context(), kernel.CommitRequest{
		Scope:      kernel.Scope{TenantID: tenantID, SessionID: sessionID},
		Kind:       kernel.EventKind(body.Kind),
		RequestID:  requestID,
		Payload:    body.Payload,
		NativeEdge: true,
	})
	if err != nil {
		writeEdgeError(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"seq": receipt.Seq, "deduped": receipt.Deduped})
}

// listEvents reads committed events through the kernel's read seam: a foreign
// or missing session is a 404 instead of a 200 with an empty list.
func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, 401, errors.New("X-Tenant-ID is required"))
		return
	}
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, fmt.Errorf("bad session id: %w", err))
		return
	}
	after := parseAfter("", r.URL.Query().Get("after"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if s.cfg.Events == nil {
		writeErr(w, 503, errKernelUnwired)
		return
	}
	events, err := s.cfg.Events.LoadEvents(r.Context(),
		kernel.Scope{TenantID: tenantID, SessionID: sessionID}, after, limit)
	if err != nil {
		writeEdgeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"events": storeEvents(events)})
}

// writeEdgeError maps a kernel rejection onto the edge status. NotFound is the
// 404 that does not leak existence; a rejected request shape is the client's
// 400; a native edge write to an external session is 409; quota is 429;
// everything else is a 500.
func writeEdgeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errKernelUnwired):
		writeErr(w, http.StatusServiceUnavailable, err)
	case errors.Is(err, kernel.ErrNotFound):
		writeErr(w, http.StatusNotFound, errors.New("session not found"))
	case errors.Is(err, kernel.ErrExternalSession):
		writeErr(w, http.StatusConflict, errors.New("session is owned by an external backend; native input is not supported"))
	case errors.Is(err, kernel.ErrUnknownKind),
		errors.Is(err, kernel.ErrKeyKindMismatch),
		errors.Is(err, kernel.ErrInvalidRequest),
		errors.Is(err, kernel.ErrMissingToken):
		writeErr(w, http.StatusBadRequest, err)
	case errors.Is(err, kernel.ErrQuota):
		writeErr(w, http.StatusTooManyRequests, err)
	case errors.Is(err, kernel.ErrKeyConflict),
		errors.Is(err, kernel.ErrExternalSession):
		writeErr(w, http.StatusConflict, err)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

// writeEnqueueError maps the native queue seam's refusals: a missing (or
// foreign) session is 404; an external session is 409 because the session
// exists but must never enter the native run/lease flow.
func writeEnqueueError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNoSession):
		writeErr(w, http.StatusNotFound, errors.New("session not found"))
	case errors.Is(err, store.ErrExternalSession):
		writeErr(w, http.StatusConflict, errors.New("session is owned by an external backend; native runs are not supported"))
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

// edgeKindList renders the client-writable whitelist for a 400 message.
func edgeKindList() string {
	kinds := kernel.EdgeKinds()
	out := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		out = append(out, string(kind))
	}
	return strings.Join(out, ", ")
}

// storeEvents maps kernel events onto the wire shape (the JSON keys the
// event-list endpoint has always used).
func storeEvents(events []kernel.Event) []store.Event {
	out := make([]store.Event, 0, len(events))
	for _, ev := range events {
		out = append(out, store.Event{Seq: ev.Seq, Kind: string(ev.Kind), Payload: ev.Payload})
	}
	return out
}

// streamEvents is the SSE endpoint. It subscribes to the hub for wakeups and
// runs a keyset query after its cursor; Last-Event-ID resumes exactly where the
// client left off.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, 401, errors.New("X-Tenant-ID is required"))
		return
	}
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, fmt.Errorf("bad session id: %w", err))
		return
	}
	after := parseAfter(r.Header.Get("Last-Event-ID"), r.URL.Query().Get("after"))
	// The stream carries the same tenant boundary as GET /events: a foreign or
	// missing session is a 404 before any SSE headers are written. Otherwise the
	// stream would answer 200 with an empty body and never end, which both
	// leaks the difference and holds a stream slot forever.
	if s.cfg.Events == nil {
		writeErr(w, http.StatusServiceUnavailable, errKernelUnwired)
		return
	}
	if _, err := s.cfg.Events.LoadEvents(r.Context(),
		kernel.Scope{TenantID: tenantID, SessionID: sessionID}, 0, 1); err != nil {
		writeEdgeError(w, err)
		return
	}
	if !s.cfg.Admitter.Allow() {
		delay := s.cfg.Admitter.Delay() / 1000
		if delay < 1 {
			delay = 1
		}
		w.Header().Set("Retry-After", strconv.FormatInt(delay, 10))
		writeErr(w, http.StatusTooManyRequests, errors.New("too many new streams"))
		return
	}
	release, ok := s.cfg.Scheduler.AcquireControl()
	if !ok {
		s.cfg.Metrics.AdmissionReject.Add(1)
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusTooManyRequests, errors.New("control stream capacity exhausted"))
		return
	}
	defer release()
	s.cfg.Metrics.StreamsActive.Add(1)
	defer s.cfg.Metrics.StreamsActive.Add(-1)
	s.cfg.Metrics.StreamsTotal.Add(1)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, errors.New("streaming unsupported"))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher.Flush()

	rc := http.NewResponseController(w)
	wake, cancel := s.cfg.Hub.Subscribe(tenantID, sessionID)
	defer cancel()
	// Token-level partials are an opt-in bypass: no id line, no persistence, no
	// effect on Last-Event-ID. A nil source (or no ?partials=1) keeps the
	// historical behavior byte for byte.
	var partialCh <-chan PartialFrame
	if r.URL.Query().Get("partials") == "1" && s.cfg.Partials != nil {
		ch, cancelPartials := s.cfg.Partials.SubscribePartials(tenantID, sessionID)
		defer cancelPartials()
		partialCh = ch
	}
	var drainDone <-chan struct{}
	if s.cfg.Drain != nil {
		drainDone = s.cfg.Drain.Done()
	}

	exit := func(reason string, err error) {
		ctxErr := ""
		if r.Context().Err() != nil {
			ctxErr = r.Context().Err().Error()
		}
		if atomic.AddInt64(&sseExitLogs, 1) <= 50 {
			s.cfg.Logger.Warn("sse exit", "reason", reason, "err", err, "ctx", ctxErr, "after", after,
				"request_id", RequestIDFrom(r.Context()),
				"tenant_id", tenantID, "session_id", sessionID)
		}
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	// NOTIFY is the primary wakeup; the fallback is a safety net for a dropped
	// notification. Five seconds keeps 1000 sessions at 200 QPS instead of 1000.
	fallback := time.NewTimer(5 * time.Second)
	defer fallback.Stop()

	for {
		events, err := s.cfg.Store.LoadEvents(r.Context(), tenantID, sessionID, after, 200)
		if err != nil {
			if r.Context().Err() != nil {
				exit("load-client-gone", err)
				return
			}
			// A transient pool/statement timeout must not tear down the stream:
			// the client keeps its cursor and the next attempt resumes there.
			s.cfg.Logger.Warn("sse load retrying", "err", err)
			select {
			case <-r.Context().Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
			continue
		}
		for _, ev := range events {
			frame := fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Kind, ev.Payload)
			if err := setWriteDeadline(rc, 10*time.Second); err != nil {
				exit("deadline", err)
				return
			}
			if _, err := w.Write([]byte(frame)); err != nil {
				exit("write", err)
				return
			}
			after = ev.Seq
		}
		if len(events) > 0 {
			flusher.Flush()
			if !fallback.Stop() {
				select {
				case <-fallback.C:
				default:
				}
			}
			fallback.Reset(5 * time.Second)
		}
		select {
		case <-r.Context().Done():
			exit("done", r.Context().Err())
			return
		case <-drainDone:
			// The durable log is the truth: flush what the client has not seen,
			// then tell it to reconnect elsewhere and close at drain rate.
			if err := s.drainStream(r.Context(), w, flusher, rc, tenantID, sessionID, &after); err != nil {
				exit("drain", err)
			}
			return
		case <-wake:
		case partial, ok := <-partialCh:
			if !ok {
				partialCh = nil
				continue
			}
			if err := setWriteDeadline(rc, 10*time.Second); err != nil {
				exit("partial-deadline", err)
				return
			}
			// No `id:` line: a partial must never move the client's resume
			// cursor. A reconnect replays only committed events.
			frame := "event: " + partial.Event + "\ndata: " + string(partial.Data) + "\n\n"
			if _, err := w.Write([]byte(frame)); err != nil {
				exit("partial-write", err)
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if err := setWriteDeadline(rc, 10*time.Second); err != nil {
				exit("heartbeat-deadline", err)
				return
			}
			if _, err := w.Write([]byte(": heartbeat\n\n")); err != nil {
				exit("heartbeat-write", err)
				return
			}
			flusher.Flush()
		case <-fallback.C:
			// Safety net: a dropped NOTIFY costs at most one fallback interval.
		}
	}
}

// setWriteDeadline bounds one SSE write. ErrNotSupported means a middleware
// wrapper hides the underlying writer; that is a deployment bug, but SSE must
// keep working, so it is tolerated here (the file path fails loudly instead).
func setWriteDeadline(rc *http.ResponseController, d time.Duration) error {
	err := rc.SetWriteDeadline(time.Now().Add(d))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

// upload accepts a raw body (Content-Type = mime) or multipart/form-data with a
// single "file" part, spools it, and returns the file id/url.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, 401, errors.New("X-Tenant-ID is required"))
		return
	}
	release, ok := s.cfg.Scheduler.Acquire(stream.ClassOf(r.ContentLength))
	if !ok {
		s.cfg.Metrics.AdmissionReject.Add(1)
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusTooManyRequests, errors.New("upload capacity exhausted"))
		return
	}
	defer release()

	mime := r.Header.Get("Content-Type")
	if strings.HasPrefix(mime, "multipart/form-data") {
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			writeErr(w, 400, err)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		defer file.Close()
		if mime == "" || strings.HasPrefix(mime, "multipart/") {
			mime = header.Header.Get("Content-Type")
		}
		meta, err := s.cfg.Uploader.Put(r.Context(), tenantID, mime, file)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		s.writeFileMeta(w, meta)
		return
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	meta, err := s.cfg.Uploader.Put(r.Context(), tenantID, mime, http.MaxBytesReader(w, r.Body, s.cfg.MaxUpload))
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	s.writeFileMeta(w, meta)
}

func (s *Server) writeFileMeta(w http.ResponseWriter, meta store.BlobMeta) {
	writeJSON(w, 201, map[string]any{
		"fileId":  meta.BlobID,
		"url":     "/v1/files/" + meta.BlobID.String(),
		"size":    meta.Size,
		"mime":    meta.Mime,
		"sha256":  hex.EncodeToString(meta.SHA256),
		"storage": meta.Storage,
		"chunks":  meta.Chunks,
	})
}

func (s *Server) fileMeta(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, 401, errors.New("X-Tenant-ID is required"))
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, fmt.Errorf("bad file id: %w", err))
		return
	}
	meta, err := s.cfg.Store.BlobMetaOf(r.Context(), tenantID, id)
	if err != nil {
		status := 500
		if errors.Is(err, store.ErrNoBlob) {
			status = 404
		}
		writeErr(w, status, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"fileId": meta.BlobID, "size": meta.Size, "mime": meta.Mime,
		"sha256": hex.EncodeToString(meta.SHA256), "storage": meta.Storage,
		"chunks": meta.Chunks, "complete": meta.Complete,
		"wireLength": blob.WireLen(meta.Size),
	})
}

// file streams a blob as raw bytes or base64. Raw supports standard Range;
// base64 uses the client's wire offset (multiple of 4) so a resumed stream
// continues the same base64 sequence.
func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(r)
	if !ok {
		writeErr(w, 401, errors.New("X-Tenant-ID is required"))
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, fmt.Errorf("bad file id: %w", err))
		return
	}
	meta, err := s.cfg.Store.BlobMetaOf(r.Context(), tenantID, id)
	if err != nil {
		status := 500
		if errors.Is(err, store.ErrNoBlob) {
			status = 404
		}
		writeErr(w, status, err)
		return
	}
	asBase64 := r.URL.Query().Get("format") == "base64"

	if !s.cfg.Admitter.Allow() {
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusTooManyRequests, errors.New("too many new streams"))
		return
	}
	class := stream.ClassOf(meta.Size)
	if asBase64 {
		class = stream.ClassOf(blob.WireLen(meta.Size))
	}
	release, ok := s.cfg.Scheduler.Acquire(class)
	if !ok {
		s.cfg.Metrics.AdmissionReject.Add(1)
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusTooManyRequests, errors.New("stream capacity exhausted"))
		return
	}
	defer release()
	s.cfg.Metrics.StreamsActive.Add(1)
	defer s.cfg.Metrics.StreamsActive.Add(-1)
	s.cfg.Metrics.StreamsTotal.Add(1)

	h := w.Header()
	h.Set("Accept-Ranges", "bytes")
	h.Set("Cache-Control", "private, max-age=3600")
	if meta.Mime != "" {
		h.Set("Content-Type", meta.Mime)
	}

	if asBase64 {
		total := blob.WireLen(meta.Size)
		off := parseAfter(r.Header.Get("Last-Event-ID"), r.URL.Query().Get("offset"))
		if off > total || off%4 != 0 {
			writeErr(w, 416, fmt.Errorf("invalid wire offset %d", off))
			return
		}
		h.Set("Content-Length", strconv.FormatInt(total-off, 10))
		status := 200
		if off > 0 {
			status = http.StatusPartialContent
			h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, total-1, total))
			s.cfg.Metrics.Resumes.Add(1)
		}
		w.WriteHeader(status)
		guard := stream.NewGuard(w, stream.GuardConfig{Limiter: s.cfg.Limiter})
		n, err := s.cfg.Blobs.StreamBase64(r.Context(), tenantID, id, meta, guard, off)
		guard.Close()
		s.cfg.Metrics.BytesSent.Add(n)
		s.cfg.Metrics.ChunksFetched.Add((meta.Size + store.ChunkSize - 1) / store.ChunkSize)
		if err != nil {
			// Headers are gone; abort the connection without a second response.
			s.cfg.Logger.Error("file base64 stream failed", "file", id, "err", err)
			panic(http.ErrAbortHandler)
		}
		return
	}

	start, end := int64(0), meta.Size-1
	status := 200
	if rng := r.Header.Get("Range"); rng != "" {
		parsedStart, parsedEnd, ok := parseRange(rng, meta.Size)
		if !ok {
			h.Set("Content-Range", fmt.Sprintf("bytes */%d", meta.Size))
			writeErr(w, 416, errors.New("invalid range"))
			return
		}
		start, end = parsedStart, parsedEnd
		status = http.StatusPartialContent
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, meta.Size))
		s.cfg.Metrics.Resumes.Add(1)
	}
	h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(status)
	guard := stream.NewGuard(w, stream.GuardConfig{Limiter: s.cfg.Limiter})
	n, err := s.cfg.Blobs.StreamRaw(r.Context(), tenantID, id, meta, guard, start, end-start+1)
	guard.Close()
	s.cfg.Metrics.BytesSent.Add(n)
	if err != nil {
		s.cfg.Logger.Error("file raw stream failed", "file", id, "err", err)
		panic(http.ErrAbortHandler)
	}
}

// parseRange parses "bytes=start-end" (single range only).
func parseRange(v string, size int64) (int64, int64, bool) {
	if !strings.HasPrefix(v, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(v, "bytes=")
	if strings.Contains(spec, ",") {
		return 0, 0, false
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	if parts[0] == "" {
		n, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return 0, 0, false
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, true
}

func decodeBody(w http.ResponseWriter, r *http.Request, limit int64, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
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
		w.WriteHeader(500)
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
