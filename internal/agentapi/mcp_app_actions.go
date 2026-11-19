package agentapi

// MCP Apps outbound actions (change feat-mcp-apps-events, P3.1/P3.2/P3.3/P3.4/
// P3.8): the host side of the postMessage bridge. A View never reaches an MCP
// server or a product API directly; every request lands here, where the host
//
//   - derives the server from the published catalogue (appId is the only
//     identity a caller supplies; a server name in the body is ignored),
//   - refuses a tool that is not app-visible on that server (403, audit
//     refused, no upstream request),
//   - puts destructive tools and user-visible side effects (a message, a link)
//     behind the existing decision primitive,
//   - writes the durable audit row (never the message body or a full URL), and
//   - enforces the bridge budget: 64 KiB request bodies, 200 requests/s and 4
//     concurrent requests per app.
//
// The browser-side bridge enforces the same numbers on its 64 KiB frames and
// 200 frames/s; this is the server half of the same contract (P3.8).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/mcp"
	"github.com/jixoai/keel/internal/mcphost"
	"github.com/jixoai/keel/internal/store"
)

const (
	// mcpAppRequestMaxBytes is the per-request body bound (P3.8, same as one
	// bridge frame).
	mcpAppRequestMaxBytes = 64 << 10
	// mcpAppRatePerSecond and mcpAppRateBurst are the per-app token bucket:
	// sustained 200 requests/s with a burst of one second's budget.
	mcpAppRatePerSecond = 200
	mcpAppRateBurst     = 200
	// mcpAppConcurrency is the per-app in-flight proxy request bound.
	mcpAppConcurrency = 4
	// mcpAppConfirmDefaultTimeout bounds how long the bridge holds a request
	// while the user answers a decision.
	mcpAppConfirmDefaultTimeout = 120 * time.Second
	mcpAppConfirmMaxTimeout     = 10 * time.Minute
	// mcpAppMaxOpenConfirmations bounds the unanswered app confirmations one
	// session may hold. An app that opens a confirmation and disconnects would
	// otherwise leave decisions open until their 10-minute cap; the bound keeps
	// that from becoming a durable-queue amplification.
	mcpAppMaxOpenConfirmations = 16
	// mcpAppAuditTimeout bounds one best-effort audit write.
	mcpAppAuditTimeout = 2 * time.Second
	// mcpAppAuditSampleWindow is the sampling window of a zero-cost refusal
	// audit (rate/concurrency/size/overflow): at most one row per app, action
	// and window.
	mcpAppAuditSampleWindow = time.Second
	// mcpAppAuditMaxDetail mirrors store.MCPAppAuditMaxDetail.
	mcpAppAuditMaxDetail = store.MCPAppAuditMaxDetail
	// Attachment bounds of a ui/message. They are stricter than the generic
	// input path: an app references already-uploaded blobs by id and never
	// supplies a URL of its own.
	mcpAppMaxAttachments        = 16
	mcpAppAttachmentNameMax     = 256
	mcpAppAttachmentMIMETypeMax = 128
	mcpAppAttachmentSizeMax     = 1 << 30
)

// mcpAuditSample buffers one sampled refusal row until its window closes.
type mcpAuditSample struct {
	windowStart time.Time
	entry       store.MCPAppAuditEntry
	suppressed  int
	timer       *time.Timer
	// flushed is set under Server.mcpAuditMu so the timer and an explicit flush
	// never write the same sample twice.
	flushed bool
}

// MCPAppTool is the proxy's view of one server tool.
type MCPAppTool struct {
	Name string
	// Destructive is annotations.destructiveHint, or the host's name-family
	// fallback applied by the runtime adapter.
	Destructive bool
}

// MCPAppMetrics is the fixed-cardinality metric seam of the app bridge (P4.7).
// *mcphost.Metrics implements it; the interface keeps the HTTP layer free of
// the collector implementation.
type MCPAppMetrics interface {
	BridgeFrame(method, direction string)
}

// MCPAppRuntime is the outbound execution seam of the app bridge. The host
// process implements it over the live MCP manager; tests substitute a fake.
type MCPAppRuntime interface {
	// AppTool returns one server tool if it exists and its visibility includes
	// "app". It must not answer tools visible only to the model or to another
	// server.
	AppTool(server, tool string) (MCPAppTool, bool)
	// CallTool invokes one tool of one connected server.
	CallTool(ctx context.Context, server, tool string, arguments json.RawMessage) (mcp.CallToolResult, error)
	// ReadResource invokes resources/read on one connected server.
	ReadResource(ctx context.Context, server, uri string) (mcp.ReadResourceResult, error)
}

// MCPAppInput is the outcome of committing a confirmed ui/message.
type MCPAppInput struct {
	SubmissionID string `json:"submissionId"`
	RequestID    string `json:"requestId"`
	Status       string `json:"status,omitempty"`
	Seq          int64  `json:"seq,omitempty"`
	Created      bool   `json:"created"`
}

// MCPAppAuditStore is the durable audit seam. *store.Store implements it.
type MCPAppAuditStore interface {
	AppendMCPAppAudit(ctx context.Context, entry store.MCPAppAuditEntry) (store.MCPAppAuditEntry, error)
	ListMCPAppAudit(ctx context.Context, tenantID *uuid.UUID, appID, action string, after int64, limit int) ([]store.MCPAppAuditEntry, error)
}

// mcpAppToolCallRequest is POST /v1/mcp/apps/{appId}/tools/call.
type mcpAppToolCallRequest struct {
	SessionID  string          `json:"sessionId,omitempty"`
	ToolCallID string          `json:"toolCallId,omitempty"`
	RequestID  string          `json:"requestId,omitempty"`
	Name       string          `json:"name"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	// WaitMs bounds the confirmation wait for a destructive tool (default
	// MCPAppConfirmTimeout, capped at 10 minutes).
	WaitMs int64 `json:"waitMs,omitempty"`
}

// mcpAppResourceReadRequest is POST /v1/mcp/apps/{appId}/resources/read.
type mcpAppResourceReadRequest struct {
	SessionID string `json:"sessionId,omitempty"`
	RequestID string `json:"requestId,omitempty"`
	URI       string `json:"uri"`
}

// mcpAppMessageRequest is POST /v1/mcp/apps/{appId}/ui/message.
type mcpAppMessageRequest struct {
	SessionID string `json:"sessionId"`
	RequestID string `json:"requestId,omitempty"`
	Role      string `json:"role"`
	// Content is parsed in the handler, not by encoding/json: the spec accepts
	// an array of blocks or (design §0.1 R8 tolerance) one block object, and a
	// shape error must answer JSON-RPC -32602 with a protocol-violation audit,
	// not the decoder's generic 400.
	Content     json.RawMessage `json:"content"`
	Attachments json.RawMessage `json:"attachments,omitempty"`
	Delivery    string          `json:"delivery,omitempty"`
	WaitMs      int64           `json:"waitMs,omitempty"`
}

// mcpAppMessageContent is one content block of a ui/message.
type mcpAppMessageContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// mcpAppOpenLinkRequest is POST /v1/mcp/apps/{appId}/ui/open-link.
type mcpAppOpenLinkRequest struct {
	SessionID string `json:"sessionId,omitempty"`
	RequestID string `json:"requestId,omitempty"`
	URL       string `json:"url"`
	WaitMs    int64  `json:"waitMs,omitempty"`
}

// mcpAppHostAuditRequest is the internal host-action record entry
// (POST /v1/mcp/apps/audit): the web host reports a permission decision or a
// protocol violation through the same tenant-authenticated surface, so there is
// no second, unauthenticated audit route.
type mcpAppHostAuditRequest struct {
	AppID     string `json:"appId"`
	SessionID string `json:"sessionId,omitempty"`
	Action    string `json:"action"`
	Result    string `json:"result,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// hostMCPAppAuditActions is the closed set a client may report. Everything
// else is written by the host itself (tools/call, ui/message, ...).
var hostMCPAppAuditActions = map[string]string{
	"permission.granted": "ok",
	"permission.denied":  "refused",
	"protocol.violation": "error",
}

// mcpAppFrom resolves the catalogue identity of the path's appId. The tenant
// boundary runs first; a foreign tenant, a malformed id and an unknown id are
// the same 404.
func (s *Server) mcpAppFrom(w http.ResponseWriter, r *http.Request) (uuid.UUID, mcphost.AppSpec, bool) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return uuid.Nil, mcphost.AppSpec{}, false
	}
	if s.cfg.MCPApps == nil {
		writeErr(w, http.StatusNotImplemented, errMCPAppsUnwired)
		return uuid.Nil, mcphost.AppSpec{}, false
	}
	appID := r.PathValue("appId")
	if !validAppID(appID) || !s.cfg.MCPApps.Visible(tenantID) {
		writeErr(w, http.StatusNotFound, errAppNotFound)
		return uuid.Nil, mcphost.AppSpec{}, false
	}
	app, found := s.cfg.MCPApps.App(appID)
	if !found {
		writeErr(w, http.StatusNotFound, errAppNotFound)
		return uuid.Nil, mcphost.AppSpec{}, false
	}
	return tenantID, app, true
}

// mcpAppGate applies the per-app bridge budget. It returns the release
// function; a refusal has already written the response and audit row.
func (s *Server) mcpAppGate(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, sessionID uuid.UUID, app mcphost.AppSpec, action string) (func(), bool) {
	// Every bridge exchange is one inbound frame and one outbound frame; the
	// method labels are the four fixed actions, so the series count is bounded.
	s.mcpBridgeFrame(action, "in")
	if !s.mcpAppLimits.allow(app.AppID) {
		s.recordSampledRejection(r.Context(), tenantID, sessionID, app, "bridge.rate", "refused",
			fmt.Sprintf("limit=%d/s app=%s", mcpAppRatePerSecond, app.AppID))
		writeErr(w, http.StatusTooManyRequests,
			fmt.Errorf("mcp app request rate exceeded (%d/s)", mcpAppRatePerSecond))
		s.mcpBridgeFrame(action, "out")
		return nil, false
	}
	if !s.mcpAppLimits.acquire(app.AppID) {
		s.recordSampledRejection(r.Context(), tenantID, sessionID, app, "bridge.concurrency", "refused",
			fmt.Sprintf("limit=%d app=%s", mcpAppConcurrency, app.AppID))
		writeErr(w, http.StatusTooManyRequests,
			fmt.Errorf("mcp app has %d requests in flight", mcpAppConcurrency))
		s.mcpBridgeFrame(action, "out")
		return nil, false
	}
	return func() {
		s.mcpAppLimits.release(app.AppID)
		s.mcpBridgeFrame(action, "out")
	}, true
}

// mcpBridgeFrame records one app bridge frame.
func (s *Server) mcpBridgeFrame(method, direction string) {
	if s.cfg.MCPAppMetrics != nil {
		s.cfg.MCPAppMetrics.BridgeFrame(method, direction)
	}
}

// decodeMCPAppBody decodes one bridge request with the 64 KiB bound. A body
// over the bound is a refusal with its own audit row and 413.
func (s *Server) decodeMCPAppBody(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, sessionID uuid.UUID, app mcphost.AppSpec, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, mcpAppRequestMaxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.recordSampledRejection(r.Context(), tenantID, sessionID, app, "bridge.size", "refused",
				fmt.Sprintf("limit=%d", mcpAppRequestMaxBytes))
			writeErr(w, http.StatusRequestEntityTooLarge,
				fmt.Errorf("request body exceeds %d bytes", mcpAppRequestMaxBytes))
			return false
		}
		writeErr(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

// mcpAppSessionValue resolves an optional session id without writing a
// response. The problem code is a fixed identifier: "" (valid), "missing",
// "invalid" or "not_found". An infrastructure error is returned separately.
func (s *Server) mcpAppSessionValue(ctx context.Context, tenantID uuid.UUID, raw string) (uuid.UUID, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return uuid.Nil, "missing", nil
	}
	sessionID, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, "invalid", nil
	}
	if s.cfg.SessionExists != nil {
		exists, err := s.cfg.SessionExists(ctx, tenantID, sessionID)
		if err != nil {
			return uuid.Nil, "error", err
		}
		if !exists {
			return uuid.Nil, "not_found", nil
		}
	}
	return sessionID, "", nil
}

// mcpAppSession parses the optional or required session id and verifies it
// belongs to the tenant when the deployment has a session seam. An absent
// optional session is uuid.Nil.
func (s *Server) mcpAppSession(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, raw string, required bool) (uuid.UUID, bool) {
	sessionID, problem, err := s.mcpAppSessionValue(r.Context(), tenantID, raw)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return uuid.Nil, false
	}
	switch problem {
	case "":
		return sessionID, true
	case "missing":
		if required {
			writeErr(w, http.StatusBadRequest, errors.New("sessionId is required"))
			return uuid.Nil, false
		}
		return uuid.Nil, true
	case "not_found":
		writeErr(w, http.StatusNotFound, ErrSessionNotFound)
		return uuid.Nil, false
	default:
		writeErr(w, http.StatusBadRequest, errors.New("sessionId must be a UUID"))
		return uuid.Nil, false
	}
}

// mcpAppAuditEntry builds one audit row from identifiers only.
func (s *Server) mcpAppAuditEntry(tenantID, sessionID uuid.UUID, app mcphost.AppSpec, action, result, detail string) store.MCPAppAuditEntry {
	entry := store.MCPAppAuditEntry{
		TS:     s.cfg.MCPAppNow(),
		Server: app.Server,
		AppID:  app.AppID,
		Action: action,
		Result: result,
		Detail: mcpAppBounded(detail, mcpAppAuditMaxDetail),
	}
	if tenantID != uuid.Nil {
		entry.TenantID = &tenantID
	}
	if sessionID != uuid.Nil {
		entry.SessionID = &sessionID
	}
	return entry
}

// appendMCPAppAudit writes one audit row best-effort: the primary action never
// fails because the audit store did, but the failure is logged loudly.
func (s *Server) appendMCPAppAudit(ctx context.Context, entry store.MCPAppAuditEntry) {
	if s.cfg.MCPAppAudit == nil {
		s.cfg.Logger.Warn("mcp app audit is not durable: no audit store is wired",
			"action", entry.Action, "result", entry.Result, "server", entry.Server, "app", entry.AppID)
		return
	}
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mcpAppAuditTimeout)
	defer cancel()
	if _, err := s.cfg.MCPAppAudit.AppendMCPAppAudit(auditCtx, entry); err != nil {
		s.cfg.Logger.Error("mcp app audit write failed",
			"action", entry.Action, "result", entry.Result, "server", entry.Server, "app", entry.AppID, "err", err)
	}
}

// recordMCPAppAudit writes one audit row immediately (the normal path).
func (s *Server) recordMCPAppAudit(ctx context.Context, tenantID, sessionID uuid.UUID, app mcphost.AppSpec, action, result, detail string) {
	s.appendMCPAppAudit(ctx, s.mcpAppAuditEntry(tenantID, sessionID, app, action, result, detail))
}

// recordSampledRejection writes at most one refusal row per (app, action,
// second). A rate/concurrency/size rejection is zero-cost to produce, so
// writing one row per attempt would let an attacker flood the audit table at
// line speed. The first refusal of a window is buffered and written when the
// window closes with the accumulated suppressed count; later refusals only
// increment the counter. The primary response is never delayed.
func (s *Server) recordSampledRejection(ctx context.Context, tenantID, sessionID uuid.UUID, app mcphost.AppSpec, action, result, detail string) {
	key := app.AppID + "\x00" + action
	windowStart := s.cfg.MCPAppNow().Truncate(mcpAppAuditSampleWindow)
	s.mcpAuditMu.Lock()
	sample := s.mcpAuditSamples[key]
	if sample != nil && sample.windowStart.Equal(windowStart) {
		sample.suppressed++
		s.mcpAuditMu.Unlock()
		return
	}
	entry := s.mcpAppAuditEntry(tenantID, sessionID, app, action, result, detail)
	sample = &mcpAuditSample{windowStart: windowStart, entry: entry}
	s.mcpAuditSamples[key] = sample
	// A real timer makes the row durable even if the rejection burst stops. It
	// waits slightly past the window so a scheduled callback never fires before
	// the next second's first rejection and re-samples the same window. In tests
	// the clock is fake and the window is flushed explicitly.
	sample.timer = time.AfterFunc(mcpAppAuditSampleWindow+100*time.Millisecond, func() {
		s.flushMCPAppAuditSample(key, sample)
	})
	s.mcpAuditMu.Unlock()
}

// flushMCPAppAuditSample writes one buffered rejection row exactly once. A
// sample replaced by a newer window is still written by its own timer: a map
// entry moving on must not lose the previous window's row.
func (s *Server) flushMCPAppAuditSample(key string, sample *mcpAuditSample) {
	s.mcpAuditMu.Lock()
	if sample.flushed {
		s.mcpAuditMu.Unlock()
		return
	}
	sample.flushed = true
	if s.mcpAuditSamples[key] == sample {
		delete(s.mcpAuditSamples, key)
	}
	entry := sample.entry
	entry.Detail = mcpAppBounded(entry.Detail+fmt.Sprintf(" suppressed=%d", sample.suppressed), mcpAppAuditMaxDetail)
	timer := sample.timer
	s.mcpAuditMu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	s.appendMCPAppAudit(context.Background(), entry)
}

// flushMCPAppAuditSamples flushes every buffered rejection row. Tests use it
// with a fake clock; a future shutdown path can use it for a clean stop.
func (s *Server) flushMCPAppAuditSamples() {
	s.mcpAuditMu.Lock()
	samples := make(map[string]*mcpAuditSample, len(s.mcpAuditSamples))
	for key, sample := range s.mcpAuditSamples {
		samples[key] = sample
	}
	s.mcpAuditMu.Unlock()
	for key, sample := range samples {
		s.flushMCPAppAuditSample(key, sample)
	}
}

// mcpAppToolCall proxies tools/call (P3.1). Ownership is checked against the
// catalogue-derived server and the live tool list; a refusal never reaches the
// MCP server.
func (s *Server) mcpAppToolCall(w http.ResponseWriter, r *http.Request) {
	tenantID, app, ok := s.mcpAppFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.MCPAppRuntime == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("MCP app tool proxy is not wired"))
		return
	}
	release, ok := s.mcpAppGate(w, r, tenantID, uuid.Nil, app, "tools/call")
	if !ok {
		return
	}
	defer release()
	// The session is optional for a plain tool call, but a destructive one
	// needs it to open the confirmation decision.
	var body mcpAppToolCallRequest
	if !s.decodeMCPAppBody(w, r, tenantID, uuid.Nil, app, &body) {
		return
	}
	sessionID, ok := s.mcpAppSession(w, r, tenantID, body.SessionID, false)
	if !ok {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, errors.New("name is required"))
		return
	}
	arguments := body.Arguments
	if len(arguments) == 0 || string(arguments) == "null" {
		arguments = json.RawMessage(`{}`)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &probe); err != nil || probe == nil {
		writeErr(w, http.StatusBadRequest, errors.New("arguments must be a JSON object"))
		return
	}
	tool, found := s.cfg.MCPAppRuntime.AppTool(app.Server, name)
	if !found {
		// The catalogue owns the identity: a body cannot name another server,
		// and a model-only tool is not app-callable.
		s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "tools/call", "refused",
			fmt.Sprintf("server=%s tool=%s reason=not_app_visible", app.Server, mcpAppBounded(name, 128)))
		writeErr(w, http.StatusForbidden,
			fmt.Errorf("tool %q is not callable by this app on server %q", name, app.Server))
		return
	}
	if tool.Destructive || mcpAppNameDestructive(name) {
		decision, ok := s.confirmMCPApp(w, r, tenantID, sessionID, app,
			"mcp-app.tool",
			fmt.Sprintf("App「%s」请求调用破坏性工具 %s/%s，是否允许？", mcpAppBounded(app.Title, 128), app.Server, mcpAppBounded(name, 128)),
			[]agentext.DecisionOption{
				{ID: "allow", Label: "允许"},
				{ID: "deny", Label: "拒绝"},
			}, body.WaitMs, "tools/call", fmt.Sprintf("server=%s tool=%s destructive=true", app.Server, mcpAppBounded(name, 128)))
		if !ok {
			return
		}
		if decision.Answer == nil || decision.Answer.OptionID != "allow" {
			s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "tools/call", "refused",
				fmt.Sprintf("server=%s tool=%s reason=user_denied", app.Server, mcpAppBounded(name, 128)))
			writeErr(w, http.StatusConflict, errors.New("the user denied the destructive tool call"))
			return
		}
	}
	result, err := s.cfg.MCPAppRuntime.CallTool(r.Context(), app.Server, name, arguments)
	if err != nil {
		s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "tools/call", "error",
			fmt.Sprintf("server=%s tool=%s code=%s", app.Server, mcpAppBounded(name, 128), mcpAppErrorCode(err)))
		writeErr(w, mcpAppUpstreamStatus(err), err)
		return
	}
	resultStatus := "ok"
	if result.IsError {
		// The call itself succeeded; the tool reported a failure. The audit
		// keeps that distinction honest.
		resultStatus = "error"
	}
	s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "tools/call", resultStatus,
		fmt.Sprintf("server=%s tool=%s isError=%t", app.Server, mcpAppBounded(name, 128), result.IsError))
	writeJSON(w, http.StatusOK, result)
}

// mcpAppResourceRead proxies resources/read (P3.2). The URI must use the
// app-facing scheme whitelist; the read is scoped to the catalogue's server and
// the result is validated and bounded before it reaches the View.
func (s *Server) mcpAppResourceRead(w http.ResponseWriter, r *http.Request) {
	tenantID, app, ok := s.mcpAppFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.MCPAppRuntime == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("MCP app resource proxy is not wired"))
		return
	}
	release, ok := s.mcpAppGate(w, r, tenantID, uuid.Nil, app, "resources/read")
	if !ok {
		return
	}
	defer release()
	var body mcpAppResourceReadRequest
	if !s.decodeMCPAppBody(w, r, tenantID, uuid.Nil, app, &body) {
		return
	}
	sessionID, ok := s.mcpAppSession(w, r, tenantID, body.SessionID, false)
	if !ok {
		return
	}
	uri := strings.TrimSpace(body.URI)
	if reason := mcpAppURIProblem(uri); reason != "" {
		s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "resources/read", "refused",
			fmt.Sprintf("reason=%s scheme=%s", reason, mcpAppURIScheme(uri)))
		writeErr(w, http.StatusBadRequest, fmt.Errorf("resource URI rejected: %s", reason))
		return
	}
	result, err := s.cfg.MCPAppRuntime.ReadResource(r.Context(), app.Server, uri)
	if err != nil {
		s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "resources/read", "error",
			fmt.Sprintf("server=%s scheme=%s code=%s", app.Server, mcpAppURIScheme(uri), mcpAppErrorCode(err)))
		writeErr(w, mcpAppUpstreamStatus(err), err)
		return
	}
	if err := mcpAppValidateResourceResult(result); err != nil {
		s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "resources/read", "refused",
			fmt.Sprintf("server=%s scheme=%s reason=%s", app.Server, mcpAppURIScheme(uri), mcpAppBounded(err.Error(), 160)))
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "resources/read", "ok",
		fmt.Sprintf("server=%s scheme=%s contents=%d", app.Server, mcpAppURIScheme(uri), len(result.Contents)))
	writeJSON(w, http.StatusOK, result)
}

// mcpAppMessage implements ui/message (P3.3): role gate, user confirmation,
// then one of three branches. Only "send" writes the ledger; "edit" returns the
// prefill and "discard" writes nothing.
func (s *Server) mcpAppMessage(w http.ResponseWriter, r *http.Request) {
	tenantID, app, ok := s.mcpAppFrom(w, r)
	if !ok {
		return
	}
	release, ok := s.mcpAppGate(w, r, tenantID, uuid.Nil, app, "ui/message")
	if !ok {
		return
	}
	defer release()
	var body mcpAppMessageRequest
	if !s.decodeMCPAppBody(w, r, tenantID, uuid.Nil, app, &body) {
		return
	}
	// A lenient session parse gives the protocol-violation rows an attribution
	// when the body carries a valid id, without changing the error precedence
	// (the role gate must answer -32602 even when the session is missing).
	auditSession := mcpAppParseSessionID(body.SessionID)
	// The role gate runs before any session, store or decision work: an app can
	// only speak as the user, never as the assistant or a tool.
	if body.Role != "user" {
		s.recordMCPAppAudit(r.Context(), tenantID, auditSession, app, "protocol.violation", "refused",
			"field=role reason=not_user")
		writeMCPAppRPCErr(w, http.StatusBadRequest, -32602, `role must be "user"`)
		return
	}
	// A View must not pick "guide": steering a running turn is the user's
	// choice, and the queue is the only delivery mode this path offers.
	if body.Delivery != "" && body.Delivery != "queue" {
		s.recordMCPAppAudit(r.Context(), tenantID, auditSession, app, "protocol.violation", "refused",
			"field=delivery reason=not_queue")
		writeMCPAppRPCErr(w, http.StatusBadRequest, -32602, `delivery must be "queue"`)
		return
	}
	if s.cfg.MCPAppInputCommit == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("MCP app message delivery is not wired"))
		return
	}
	if s.cfg.Decisions == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("decisions are not wired"))
		return
	}
	sessionID, ok := s.mcpAppSession(w, r, tenantID, body.SessionID, true)
	if !ok {
		return
	}
	blocks, err := parseMCPAppMessageContent(body.Content)
	if err != nil {
		s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "protocol.violation", "refused",
			"field=content reason="+mcpAppFieldErrorCode(err))
		writeMCPAppRPCErr(w, http.StatusBadRequest, -32602, err.Error())
		return
	}
	text, err := mcpAppMessageText(blocks)
	if err != nil {
		s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "protocol.violation", "refused",
			"field=content reason="+mcpAppFieldErrorCode(err))
		writeMCPAppRPCErr(w, http.StatusBadRequest, -32602, err.Error())
		return
	}
	attachments, attachmentCount, err := mcpAppValidateAttachments(body.Attachments)
	if err != nil {
		s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "protocol.violation", "refused",
			"field=attachments reason="+mcpAppFieldErrorCode(err))
		writeMCPAppRPCErr(w, http.StatusBadRequest, -32602, err.Error())
		return
	}
	preview := mcpAppBounded(strings.TrimSpace(text), 120)
	prompt := fmt.Sprintf("App「%s」请求发送一条消息：%s", mcpAppBounded(app.Title, 128), preview)
	if attachmentCount > 0 {
		prompt = fmt.Sprintf("App「%s」请求发送一条消息（含 %d 个附件）：%s",
			mcpAppBounded(app.Title, 128), attachmentCount, preview)
	}
	decision, ok := s.confirmMCPApp(w, r, tenantID, sessionID, app,
		"mcp-app.message",
		prompt,
		[]agentext.DecisionOption{
			{ID: "send", Label: "发送"},
			{ID: "edit", Label: "编辑后发送"},
			{ID: "discard", Label: "丢弃"},
		}, body.WaitMs, "ui/message",
		fmt.Sprintf("session=%s blocks=%d attachments=%d", sessionID, len(blocks), attachmentCount))
	if !ok {
		return
	}
	branch := "discard"
	if decision.Answer != nil {
		switch decision.Answer.OptionID {
		case "send":
			branch = "send"
		case "edit":
			branch = "edit"
		}
	}
	switch branch {
	case "send":
		frameID := mcpAppBounded(strings.TrimSpace(body.RequestID), 128)
		if frameID == "" {
			frameID = uuid.NewString()
		}
		// Attributable and idempotent: the same View frame replayed commits the
		// same user.message (the kernel's requestId is the idempotency key).
		requestID := "mcp-app:" + app.AppID + ":" + frameID
		commit, err := s.cfg.MCPAppInputCommit(r.Context(), tenantID, sessionID, requestID, text, attachments)
		if err != nil {
			s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "ui/message", "error",
				fmt.Sprintf("branch=send reason=commit_failed code=%s", mcpAppErrorCode(err)))
			writeMCPAppInputError(w, err)
			return
		}
		s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "ui/message", "ok",
			fmt.Sprintf("branch=send blocks=%d attachments=%d created=%t", len(blocks), attachmentCount, commit.Created))
		writeJSON(w, http.StatusOK, map[string]any{
			"result": "send",
			"message": map[string]any{
				"submissionId": commit.SubmissionID,
				"requestId":    commit.RequestID,
				"status":       commit.Status,
				"seq":          commit.Seq,
				"created":      commit.Created,
			},
		})
	case "edit":
		// Edit prefills the composer and writes nothing: the user decides
		// whether to send the edited text.
		s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "ui/message", "ok",
			fmt.Sprintf("branch=edit blocks=%d attachments=%d", len(blocks), attachmentCount))
		writeJSON(w, http.StatusOK, map[string]any{
			"result":    "edit",
			"prefilled": map[string]any{"text": text},
		})
	default:
		s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "ui/message", "ok",
			fmt.Sprintf("branch=discard blocks=%d attachments=%d", len(blocks), attachmentCount))
		writeJSON(w, http.StatusOK, map[string]any{"result": "discard"})
	}
}

// mcpAppOpenLink implements ui/open-link (P3.4): http(s) only, confirmed by the
// user, and answered with the target URL for the host front end to open with
// noopener,noreferrer. The host page never navigates and nothing is written to
// the ledger.
func (s *Server) mcpAppOpenLink(w http.ResponseWriter, r *http.Request) {
	tenantID, app, ok := s.mcpAppFrom(w, r)
	if !ok {
		return
	}
	release, ok := s.mcpAppGate(w, r, tenantID, uuid.Nil, app, "ui/open-link")
	if !ok {
		return
	}
	defer release()
	var body mcpAppOpenLinkRequest
	if !s.decodeMCPAppBody(w, r, tenantID, uuid.Nil, app, &body) {
		return
	}
	// The optional session is parsed (and looked up) before the scheme check so
	// a refusal row can be attributed to a session; the confirmation below still
	// decides whether a valid session is required.
	sessionID, sessionProblem, sessionErr := s.mcpAppSessionValue(r.Context(), tenantID, body.SessionID)
	if sessionErr != nil {
		writeErr(w, http.StatusInternalServerError, sessionErr)
		return
	}
	parsed, err := url.Parse(strings.TrimSpace(body.URL))
	scheme := ""
	if parsed != nil {
		scheme = strings.ToLower(parsed.Scheme)
	}
	reason := ""
	switch {
	case err != nil || parsed == nil:
		reason = "malformed_url"
	case scheme != "http" && scheme != "https":
		reason = "unsupported_scheme"
	case parsed.Host == "":
		reason = "missing_host"
	case parsed.User != nil:
		reason = "userinfo"
	case !mcpAppASCIIHost(parsed.Hostname()):
		reason = "non_ascii_host"
	}
	if reason != "" {
		detail := fmt.Sprintf("reason=%s scheme=%s", reason, mcpAppBounded(scheme, 32))
		if sessionID != uuid.Nil {
			detail += " session=" + sessionID.String()
		}
		if sessionProblem != "" {
			detail += " session_problem=" + sessionProblem
		}
		s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "ui/open-link", "refused", detail)
		writeMCPAppRPCErr(w, http.StatusBadRequest, -32602, "url must be an absolute http(s) URL without credentials")
		return
	}
	if s.cfg.Decisions == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("decisions are not wired"))
		return
	}
	// The scheme is acceptable; now a malformed or foreign session is a real
	// error (the confirmation needs it). A missing one is left to
	// confirmMCPApp, which answers the explicit "sessionId is required".
	switch sessionProblem {
	case "", "missing":
	case "not_found":
		writeErr(w, http.StatusNotFound, ErrSessionNotFound)
		return
	default:
		writeErr(w, http.StatusBadRequest, errors.New("sessionId must be a UUID"))
		return
	}
	display := scheme + "://" + parsed.Host + parsed.EscapedPath()
	decision, ok := s.confirmMCPApp(w, r, tenantID, sessionID, app,
		"mcp-app.link",
		fmt.Sprintf("App「%s」请求打开链接：%s", mcpAppBounded(app.Title, 128), mcpAppBounded(display, 256)),
		[]agentext.DecisionOption{
			{ID: "open", Label: "打开链接"},
			{ID: "cancel", Label: "取消"},
		}, body.WaitMs, "ui/open-link", "host="+mcpAppBounded(parsed.Host, 128))
	if !ok {
		return
	}
	if decision.Answer == nil || decision.Answer.OptionID != "open" {
		s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "ui/open-link", "refused",
			"host="+mcpAppBounded(parsed.Host, 128)+" reason=user_denied")
		writeJSON(w, http.StatusOK, map[string]any{"opened": false, "reason": "denied"})
		return
	}
	s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "ui/open-link", "ok",
		"host="+mcpAppBounded(parsed.Host, 128))
	writeJSON(w, http.StatusOK, map[string]any{"opened": true, "url": strings.TrimSpace(body.URL)})
}

// confirmMCPApp opens one decision and waits (bounded) for the user's answer.
// A timeout answers 202 with the still-open decision id and audits the refusal;
// the caller only proceeds with an answered decision.
func (s *Server) confirmMCPApp(w http.ResponseWriter, r *http.Request, tenantID, sessionID uuid.UUID, app mcphost.AppSpec, kind, prompt string, options []agentext.DecisionOption, waitMs int64, action, detail string) (agentext.Decision, bool) {
	if s.cfg.Decisions == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("decisions are not wired"))
		return agentext.Decision{}, false
	}
	if sessionID == uuid.Nil {
		s.recordMCPAppAudit(r.Context(), tenantID, uuid.Nil, app, action, "refused",
			detail+" reason=session_required_for_confirmation")
		writeErr(w, http.StatusBadRequest, errors.New("sessionId is required for a confirmation"))
		return agentext.Decision{}, false
	}
	// Bound the unanswered confirmations one session may hold. An app that
	// opens a confirmation and disconnects leaves the decision open until its
	// timeout; without this bound a 200 req/s client could stack thousands of
	// durable rows. The check is a read, not a write: normal confirmations are
	// never blocked by it.
	open, err := s.mcpAppOpenConfirmations(r.Context(), tenantID, sessionID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return agentext.Decision{}, false
	}
	if open >= mcpAppMaxOpenConfirmations {
		s.recordSampledRejection(r.Context(), tenantID, sessionID, app, "confirmation_overflow", "refused",
			fmt.Sprintf("open=%d limit=%d", open, mcpAppMaxOpenConfirmations))
		writeMCPAppRPCErr(w, http.StatusTooManyRequests, -32000, "too many pending app confirmations")
		return agentext.Decision{}, false
	}
	wait := s.cfg.MCPAppConfirmTimeout
	if waitMs > 0 {
		wait = time.Duration(waitMs) * time.Millisecond
	}
	if wait > mcpAppConfirmMaxTimeout {
		wait = mcpAppConfirmMaxTimeout
	}
	// A fresh decision per call: "破坏性工具逐次确认" must not be bypassed by an
	// app reusing its own request id.
	decision, _, err := s.cfg.Decisions.Open(r.Context(), agentext.OpenDecisionRequest{
		TenantID:  tenantID,
		SessionID: sessionID,
		RequestID: "mcp-app:" + app.AppID + ":" + kind + ":" + uuid.NewString(),
		Kind:      kind,
		Prompt:    mcpAppBounded(prompt, 1024),
		Options:   options,
		Timeout:   wait,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return agentext.Decision{}, false
	}
	decisionID, err := uuid.Parse(decision.DecisionID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, errors.New("the decision has an unusable id"))
		return agentext.Decision{}, false
	}
	waitCtx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	decision, err = s.cfg.Decisions.Wait(waitCtx, tenantID, sessionID, decisionID)
	if err != nil {
		if r.Context().Err() != nil {
			// The client went away; stop waiting and do not pretend a branch
			// ran. The decision row stays answerable.
			s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, action, "refused",
				detail+" reason=client_disconnected decision="+decisionID.String())
			return agentext.Decision{}, false
		}
		if errors.Is(err, context.DeadlineExceeded) {
			s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, action, "refused",
				detail+" reason=confirmation_timeout decision="+decisionID.String())
			writeJSON(w, http.StatusAccepted, map[string]any{
				"decisionId": decisionID.String(),
				"status":     string(decision.Status),
				"pending":    true,
			})
			return agentext.Decision{}, false
		}
		writeErr(w, http.StatusInternalServerError, err)
		return agentext.Decision{}, false
	}
	if decision.Status != agentext.DecisionAnswered {
		s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, action, "refused",
			detail+" reason=confirmation_"+string(decision.Status)+" decision="+decisionID.String())
		writeJSON(w, http.StatusAccepted, map[string]any{
			"decisionId": decisionID.String(),
			"status":     string(decision.Status),
			"pending":    false,
		})
		return agentext.Decision{}, false
	}
	return decision, true
}

// mcpAppOpenConfirmations counts the session's unanswered app confirmations
// (kind "mcp-app.*"). The broker's List expires timed-out rows before counting,
// so a decision that reached its deadline no longer blocks new ones.
func (s *Server) mcpAppOpenConfirmations(ctx context.Context, tenantID, sessionID uuid.UUID) (int, error) {
	decisions, err := s.cfg.Decisions.List(ctx, tenantID, sessionID)
	if err != nil {
		return 0, err
	}
	open := 0
	for _, decision := range decisions {
		if decision.Status == agentext.DecisionOpen && strings.HasPrefix(decision.Kind, "mcp-app.") {
			open++
		}
	}
	return open, nil
}

// mcpAppAuditList implements GET /v1/mcp/apps/audit: the tenant's app audit
// trail, keyset-paginated by audit id.
func (s *Server) mcpAppAuditList(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.MCPAppAudit == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("the MCP app audit store is not wired"))
		return
	}
	limit, err := queryInt(r, "limit", 50, 1, 200)
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
	appID := strings.TrimSpace(r.URL.Query().Get("app"))
	if appID != "" && !validAppID(appID) {
		writeErr(w, http.StatusBadRequest, errors.New("app must be an app id"))
		return
	}
	action := strings.TrimSpace(r.URL.Query().Get("action"))
	if len(action) > 64 {
		writeErr(w, http.StatusBadRequest, errors.New("action is too long"))
		return
	}
	entries, err := s.cfg.MCPAppAudit.ListMCPAppAudit(r.Context(), &tenantID, appID, action, after, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	body := map[string]any{"entries": entries, "limit": limit, "next": after}
	if len(entries) > 0 {
		body["next"] = entries[len(entries)-1].AuditID
	}
	writeJSON(w, http.StatusOK, body)
}

// mcpAppAuditRecord is the internal record entry for host actions the browser
// performs (permission grant/deny, protocol violations). It is tenant-
// authenticated like every other /v1 route and accepts only the closed host
// action set: it cannot be used to forge bridge actions.
func (s *Server) mcpAppAuditRecord(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.MCPApps == nil {
		writeErr(w, http.StatusNotImplemented, errMCPAppsUnwired)
		return
	}
	if s.cfg.MCPAppAudit == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("the MCP app audit store is not wired"))
		return
	}
	var body mcpAppHostAuditRequest
	if !s.decodeMCPAppBody(w, r, tenantID, uuid.Nil, mcphost.AppSpec{}, &body) {
		return
	}
	result, allowed := hostMCPAppAuditActions[body.Action]
	if !allowed {
		writeErr(w, http.StatusBadRequest, errors.New("action is not a host-auditable action"))
		return
	}
	// The result is part of the action's contract, not a client choice: a host
	// action cannot be recorded as "granted, refused" at will.
	_ = body.Result
	if !validAppID(body.AppID) || !s.cfg.MCPApps.Visible(tenantID) {
		writeErr(w, http.StatusNotFound, errAppNotFound)
		return
	}
	app, found := s.cfg.MCPApps.App(body.AppID)
	if !found {
		writeErr(w, http.StatusNotFound, errAppNotFound)
		return
	}
	release, ok := s.mcpAppGate(w, r, tenantID, uuid.Nil, app, "audit.record")
	if !ok {
		return
	}
	defer release()
	sessionID, ok := s.mcpAppSession(w, r, tenantID, body.SessionID, false)
	if !ok {
		return
	}
	entry := store.MCPAppAuditEntry{
		TS:     s.cfg.MCPAppNow(),
		Server: app.Server,
		AppID:  app.AppID,
		Action: body.Action,
		Result: result,
		Detail: mcpAppBounded(body.Detail, mcpAppAuditMaxDetail),
	}
	entry.TenantID = &tenantID
	if sessionID != uuid.Nil {
		entry.SessionID = &sessionID
	}
	written, err := s.cfg.MCPAppAudit.AppendMCPAppAudit(r.Context(), entry)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"entry": written})
}

// writeMCPAppRPCErr writes a JSON-RPC-shaped refusal. The browser bridge turns
// it back into an error frame with the same code (-32602 invalid params), so
// the View sees the protocol error, not an HTTP detail.
func writeMCPAppRPCErr(w http.ResponseWriter, status, code int, message string) {
	writeJSON(w, status, map[string]any{
		"jsonrpc": "2.0",
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

// writeMCPAppInputError maps the shared input commit path's refusals onto the
// bridge's wire.
func writeMCPAppInputError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrExternalSession):
		writeErr(w, http.StatusConflict, err)
	case errors.Is(err, store.ErrNoSession):
		writeErr(w, http.StatusNotFound, ErrSessionNotFound)
	default:
		writeErr(w, http.StatusBadGateway, err)
	}
}

// mcpAppFieldError is a fixed-code validation failure: the code goes to the
// audit row, the message goes to the View. Neither ever carries the offending
// value.
type mcpAppFieldError struct {
	code    string
	message string
}

func (e *mcpAppFieldError) Error() string { return e.message }

func mcpAppFieldErrorCode(err error) string {
	var fieldErr *mcpAppFieldError
	if errors.As(err, &fieldErr) {
		return fieldErr.code
	}
	return "invalid"
}

// parseMCPAppMessageContent accepts the two tolerated shapes (design §0.1 R8):
// an array of blocks, or one block object. Every block is an object with only
// "type" and "text"; any other shape is a protocol violation.
func parseMCPAppMessageContent(raw json.RawMessage) ([]mcpAppMessageContent, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, &mcpAppFieldError{code: "shape", message: "content must be an array of blocks or one block object"}
	}
	switch trimmed[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil || items == nil {
			return nil, &mcpAppFieldError{code: "shape", message: "content must be an array of blocks"}
		}
		blocks := make([]mcpAppMessageContent, 0, len(items))
		for _, item := range items {
			block, err := decodeMCPAppContentBlock(item)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, block)
		}
		return blocks, nil
	case '{':
		block, err := decodeMCPAppContentBlock(trimmed)
		if err != nil {
			return nil, err
		}
		return []mcpAppMessageContent{block}, nil
	default:
		return nil, &mcpAppFieldError{code: "shape", message: "content must be an array of blocks or one block object"}
	}
}

// decodeMCPAppContentBlock decodes one block strictly: an object with only the
// type and text members.
func decodeMCPAppContentBlock(raw json.RawMessage) (mcpAppMessageContent, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return mcpAppMessageContent{}, &mcpAppFieldError{code: "block", message: "each content block must be an object"}
	}
	for key := range fields {
		if key != "type" && key != "text" {
			return mcpAppMessageContent{}, &mcpAppFieldError{code: "block", message: "content blocks carry only type and text"}
		}
	}
	var block mcpAppMessageContent
	if rawType, ok := fields["type"]; ok {
		if err := json.Unmarshal(rawType, &block.Type); err != nil {
			return mcpAppMessageContent{}, &mcpAppFieldError{code: "type", message: "content block type must be a string"}
		}
	}
	if rawText, ok := fields["text"]; ok {
		if err := json.Unmarshal(rawText, &block.Text); err != nil {
			return mcpAppMessageContent{}, &mcpAppFieldError{code: "text", message: "content block text must be a string"}
		}
	}
	return block, nil
}

// mcpAppMessageText joins the text blocks of a ui/message and refuses every
// other block type: silently dropping a non-text block would send a message the
// app did not ask for.
func mcpAppMessageText(content []mcpAppMessageContent) (string, error) {
	if len(content) == 0 {
		return "", &mcpAppFieldError{code: "empty", message: "content must contain at least one text block"}
	}
	parts := make([]string, 0, len(content))
	for _, block := range content {
		if block.Type != "text" {
			return "", &mcpAppFieldError{code: "type", message: "only text content is accepted"}
		}
		if strings.TrimSpace(block.Text) == "" {
			return "", &mcpAppFieldError{code: "empty", message: "text content must not be empty"}
		}
		parts = append(parts, block.Text)
	}
	text := strings.Join(parts, "\n")
	if strings.TrimSpace(text) == "" {
		return "", &mcpAppFieldError{code: "empty", message: "content must not be empty"}
	}
	return text, nil
}

// mcpAppValidateAttachments applies the /input attachment shape strictly: an
// array of at most 16 entries, each {fileId (UUID), mime, name, size} and
// nothing else. An app references already-uploaded blobs; a URL or an unknown
// field is refused rather than forwarded. The raw value is returned unchanged
// so the shared input path sees exactly what the app sent.
func mcpAppValidateAttachments(raw json.RawMessage) (json.RawMessage, int, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, 0, nil
	}
	if trimmed[0] != '[' {
		return nil, 0, &mcpAppFieldError{code: "shape", message: "attachments must be an array"}
	}
	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, 0, &mcpAppFieldError{code: "shape", message: "attachments must be an array"}
	}
	if len(items) > mcpAppMaxAttachments {
		return nil, 0, &mcpAppFieldError{code: "count", message: fmt.Sprintf("at most %d attachments are accepted", mcpAppMaxAttachments)}
	}
	for _, item := range items {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil || fields == nil {
			return nil, 0, &mcpAppFieldError{code: "entry", message: "each attachment must be an object"}
		}
		for key := range fields {
			switch key {
			case "fileId", "mime", "name", "size":
			default:
				return nil, 0, &mcpAppFieldError{code: "entry", message: "attachments carry only fileId, mime, name and size"}
			}
		}
		var fileID string
		rawFileID, ok := fields["fileId"]
		if !ok || json.Unmarshal(rawFileID, &fileID) != nil {
			return nil, 0, &mcpAppFieldError{code: "file_id", message: "each attachment needs a fileId"}
		}
		id, err := uuid.Parse(fileID)
		if err != nil || id == uuid.Nil {
			return nil, 0, &mcpAppFieldError{code: "file_id", message: "attachment fileId must be a UUID"}
		}
		var name string
		if rawName, ok := fields["name"]; ok {
			if err := json.Unmarshal(rawName, &name); err != nil {
				return nil, 0, &mcpAppFieldError{code: "name", message: "attachment name must be a string"}
			}
		}
		name = strings.TrimSpace(name)
		if name == "" || len(name) > mcpAppAttachmentNameMax {
			return nil, 0, &mcpAppFieldError{code: "name", message: fmt.Sprintf("attachment name must be 1..%d bytes", mcpAppAttachmentNameMax)}
		}
		var mime string
		if rawMIME, ok := fields["mime"]; ok {
			if err := json.Unmarshal(rawMIME, &mime); err != nil {
				return nil, 0, &mcpAppFieldError{code: "mime", message: "attachment mime must be a string"}
			}
		}
		if len(mime) > mcpAppAttachmentMIMETypeMax {
			return nil, 0, &mcpAppFieldError{code: "mime", message: fmt.Sprintf("attachment mime must be at most %d bytes", mcpAppAttachmentMIMETypeMax)}
		}
		var size int64
		if rawSize, ok := fields["size"]; ok {
			if err := json.Unmarshal(rawSize, &size); err != nil {
				return nil, 0, &mcpAppFieldError{code: "size", message: "attachment size must be an integer"}
			}
		}
		if size < 0 || size > mcpAppAttachmentSizeMax {
			return nil, 0, &mcpAppFieldError{code: "size", message: "attachment size is out of range"}
		}
	}
	return append(json.RawMessage(nil), trimmed...), len(items), nil
}

// mcpAppParseSessionID parses a session id leniently for audit attribution: a
// malformed value yields uuid.Nil rather than an error. Validation still runs
// through mcpAppSession at the point the endpoint requires one.
func mcpAppParseSessionID(raw string) uuid.UUID {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return uuid.Nil
	}
	return id
}

// mcpAppURIProblem applies the resources/read scheme whitelist: only ui:// and
// http(s)://. The returned reason is a fixed code, never the URI itself.
func mcpAppURIProblem(uri string) string {
	if uri == "" {
		return "empty_uri"
	}
	parsed, err := url.Parse(uri)
	if err != nil {
		return "malformed_uri"
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "ui", "http", "https":
		if scheme != "ui" && parsed.Host == "" {
			return "missing_host"
		}
		return ""
	case "":
		return "missing_scheme"
	default:
		return "unsupported_scheme"
	}
}

// mcpAppASCIIHost rejects a host carrying non-ASCII runes: a Unicode host is a
// homograph vector and the bridge contract only accepts ASCII/DNS hosts.
func mcpAppASCIIHost(host string) bool {
	for _, r := range host {
		if r > 0x7f {
			return false
		}
	}
	return true
}

// mcpAppURIScheme reports the URI's scheme for an audit detail (never the full
// URI: it may carry a query string).
func mcpAppURIScheme(uri string) string {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme == "" {
		return "none"
	}
	return mcpAppBounded(strings.ToLower(parsed.Scheme), 32)
}

// mcpAppValidateResourceResult re-applies the client's size bound at the proxy
// boundary: a custom runtime seam must not be able to push an unbounded
// document through the bridge. Shape validation already ran in the MCP client.
func mcpAppValidateResourceResult(result mcp.ReadResourceResult) error {
	for _, content := range result.Contents {
		if len(content.Text) > mcp.AppResourceMaxBytes {
			return fmt.Errorf("resource text is %d bytes, over the %d-byte limit", len(content.Text), mcp.AppResourceMaxBytes)
		}
		if content.Blob != "" {
			decoded, err := base64.StdEncoding.DecodeString(content.Blob)
			if err != nil {
				return errors.New("resource blob is not valid base64")
			}
			if len(decoded) > mcp.AppResourceMaxBytes {
				return fmt.Errorf("resource blob is %d bytes, over the %d-byte limit", len(decoded), mcp.AppResourceMaxBytes)
			}
		}
	}
	return nil
}

// mcpAppUpstreamStatus maps an upstream failure onto the bridge's HTTP answer.
func mcpAppUpstreamStatus(err error) int {
	if errors.Is(err, mcp.ErrServerNotFound) || errors.Is(err, mcp.ErrToolNotFound) {
		return http.StatusNotFound
	}
	return http.StatusBadGateway
}

// mcpAppErrorCode renders an error's protocol code for the audit detail. It
// never includes the error message: an upstream error may echo arguments.
func mcpAppErrorCode(err error) string {
	var mcpErr *mcp.McpError
	if errors.As(err, &mcpErr) {
		return fmt.Sprintf("%d", mcpErr.Code)
	}
	switch {
	case errors.Is(err, mcp.ErrServerNotFound):
		return "server_not_found"
	case errors.Is(err, mcp.ErrToolNotFound):
		return "tool_not_found"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "error"
	}
}

// mcpAppDestructiveWords is the change-family vocabulary from
// web/src/screens/tool-families.ts (FAMILY_WORDS.changes) plus a backend-only
// destructive set (drop/truncate/rm/reset/clear/flush/wipe/nuke/erase/purge/
// destroy/revoke/unlink/format/overwrite). The frontend list is frozen for the
// display side; the host is deliberately stricter here, because a false
// positive costs one confirmation while a false negative lets an app mutate
// data without asking. A tool without a destructiveHint whose name carries one
// of these words is confirmed per call.
var mcpAppDestructiveWords = map[string]bool{
	// web/src/screens/tool-families.ts FAMILY_WORDS.changes (verbatim).
	"edit": true, "write": true, "patch": true, "apply": true, "replace": true,
	"create": true, "delete": true, "remove": true, "mkdir": true, "move": true,
	"rename": true, "git": true, "commit": true, "diff": true, "build": true,
	"test": true, "install": true, "deploy": true, "publish": true,
	// Backend-only destructive extension (strictly more severe than the frontend
	// display vocabulary).
	"drop": true, "truncate": true, "rm": true, "reset": true, "clear": true,
	"flush": true, "wipe": true, "nuke": true, "erase": true, "purge": true,
	"destroy": true, "revoke": true, "unlink": true, "format": true, "overwrite": true,
}

// mcpAppNameDestructive applies the name-family fallback.
func mcpAppNameDestructive(name string) bool {
	for _, token := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if mcpAppDestructiveWords[token] {
			return true
		}
	}
	return false
}

// mcpAppBounded removes control characters and cuts at a rune boundary.
func mcpAppBounded(value string, max int) string {
	var builder strings.Builder
	count := 0
	for _, r := range value {
		if r < 0x20 || r == 0x7f || r == '\u2028' || r == '\u2029' {
			continue
		}
		size := len(string(r))
		if count+size > max {
			break
		}
		builder.WriteRune(r)
		count += size
	}
	return builder.String()
}

// mcpAppLimiter is the per-app bridge budget (P3.8): a token bucket for the
// request rate and a counter for in-flight requests. The clock is injectable so
// the threshold tests do not sleep.
type mcpAppLimiter struct {
	now        func() time.Time
	rate       float64
	burst      float64
	concurrent int

	mu       sync.Mutex
	buckets  map[string]*mcpAppBucket
	inflight map[string]int
}

type mcpAppBucket struct {
	tokens float64
	last   time.Time
}

// newMCPAppLimiter builds the budget with the production thresholds.
func newMCPAppLimiter(now func() time.Time) *mcpAppLimiter {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &mcpAppLimiter{
		now:        now,
		rate:       mcpAppRatePerSecond,
		burst:      mcpAppRateBurst,
		concurrent: mcpAppConcurrency,
		buckets:    make(map[string]*mcpAppBucket),
		inflight:   make(map[string]int),
	}
}

// allow consumes one request token. A new app starts with a full bucket.
func (l *mcpAppLimiter) allow(appID string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	bucket := l.buckets[appID]
	if bucket == nil {
		bucket = &mcpAppBucket{tokens: l.burst, last: now}
		l.buckets[appID] = bucket
	}
	elapsed := now.Sub(bucket.last).Seconds()
	if elapsed > 0 {
		bucket.tokens = math.Min(l.burst, bucket.tokens+elapsed*l.rate)
		bucket.last = now
	}
	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens--
	return true
}

// acquire takes one in-flight slot.
func (l *mcpAppLimiter) acquire(appID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight[appID] >= l.concurrent {
		return false
	}
	l.inflight[appID]++
	return true
}

// release frees one in-flight slot.
func (l *mcpAppLimiter) release(appID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight[appID] <= 1 {
		delete(l.inflight, appID)
		return
	}
	l.inflight[appID]--
}
