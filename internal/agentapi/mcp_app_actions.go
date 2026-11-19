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
	// mcpAppAuditTimeout bounds one best-effort audit write.
	mcpAppAuditTimeout = 2 * time.Second
	// mcpAppAuditMaxDetail mirrors store.MCPAppAuditMaxDetail.
	mcpAppAuditMaxDetail = store.MCPAppAuditMaxDetail
)

// MCPAppTool is the proxy's view of one server tool.
type MCPAppTool struct {
	Name string
	// Destructive is annotations.destructiveHint, or the host's name-family
	// fallback applied by the runtime adapter.
	Destructive bool
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
	SessionID   string                 `json:"sessionId"`
	RequestID   string                 `json:"requestId,omitempty"`
	Role        string                 `json:"role"`
	Content     []mcpAppMessageContent `json:"content"`
	Attachments json.RawMessage        `json:"attachments,omitempty"`
	Delivery    string                 `json:"delivery,omitempty"`
	WaitMs      int64                  `json:"waitMs,omitempty"`
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
	if !s.mcpAppLimits.allow(app.AppID) {
		s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "bridge.rate", "refused",
			fmt.Sprintf("limit=%d/s app=%s", mcpAppRatePerSecond, app.AppID))
		writeErr(w, http.StatusTooManyRequests,
			fmt.Errorf("mcp app request rate exceeded (%d/s)", mcpAppRatePerSecond))
		return nil, false
	}
	if !s.mcpAppLimits.acquire(app.AppID) {
		s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "bridge.concurrency", "refused",
			fmt.Sprintf("limit=%d app=%s", mcpAppConcurrency, app.AppID))
		writeErr(w, http.StatusTooManyRequests,
			fmt.Errorf("mcp app has %d requests in flight", mcpAppConcurrency))
		return nil, false
	}
	return func() { s.mcpAppLimits.release(app.AppID) }, true
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
			s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "bridge.size", "refused",
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

// mcpAppSession parses the optional or required session id and verifies it
// belongs to the tenant when the deployment has a session seam. An absent
// optional session is uuid.Nil.
func (s *Server) mcpAppSession(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, raw string, required bool) (uuid.UUID, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if required {
			writeErr(w, http.StatusBadRequest, errors.New("sessionId is required"))
			return uuid.Nil, false
		}
		return uuid.Nil, true
	}
	sessionID, err := uuid.Parse(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("sessionId must be a UUID"))
		return uuid.Nil, false
	}
	if s.cfg.SessionExists != nil {
		exists, err := s.cfg.SessionExists(r.Context(), tenantID, sessionID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return uuid.Nil, false
		}
		if !exists {
			writeErr(w, http.StatusNotFound, ErrSessionNotFound)
			return uuid.Nil, false
		}
	}
	return sessionID, true
}

// recordMCPAppAudit writes one audit row best-effort: the primary action never
// fails because the audit store did, but the failure is logged loudly. A
// refusal audit uses the same path (it is attempted even when the request
// context is already cancelled).
func (s *Server) recordMCPAppAudit(ctx context.Context, tenantID, sessionID uuid.UUID, app mcphost.AppSpec, action, result, detail string) {
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
	if s.cfg.MCPAppAudit == nil {
		s.cfg.Logger.Warn("mcp app audit is not durable: no audit store is wired",
			"action", action, "result", result, "server", app.Server, "app", app.AppID)
		return
	}
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mcpAppAuditTimeout)
	defer cancel()
	if _, err := s.cfg.MCPAppAudit.AppendMCPAppAudit(auditCtx, entry); err != nil {
		s.cfg.Logger.Error("mcp app audit write failed",
			"action", action, "result", result, "server", app.Server, "app", app.AppID, "err", err)
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
	// The role gate runs before any session, store or decision work: an app can
	// only speak as the user, never as the assistant or a tool.
	if body.Role != "user" {
		writeMCPAppRPCErr(w, http.StatusBadRequest, -32602, `role must be "user"`)
		return
	}
	// A View must not pick "guide": steering a running turn is the user's
	// choice, and the queue is the only delivery mode this path offers.
	if body.Delivery != "" && body.Delivery != "queue" {
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
	text, err := mcpAppMessageText(body.Content)
	if err != nil {
		writeMCPAppRPCErr(w, http.StatusBadRequest, -32602, err.Error())
		return
	}
	preview := mcpAppBounded(strings.TrimSpace(text), 120)
	decision, ok := s.confirmMCPApp(w, r, tenantID, sessionID, app,
		"mcp-app.message",
		fmt.Sprintf("App「%s」请求发送一条消息：%s", mcpAppBounded(app.Title, 128), preview),
		[]agentext.DecisionOption{
			{ID: "send", Label: "发送"},
			{ID: "edit", Label: "编辑后发送"},
			{ID: "discard", Label: "丢弃"},
		}, body.WaitMs, "ui/message",
		fmt.Sprintf("session=%s blocks=%d", sessionID, len(body.Content)))
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
		commit, err := s.cfg.MCPAppInputCommit(r.Context(), tenantID, sessionID, requestID, text, body.Attachments)
		if err != nil {
			s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "ui/message", "error",
				fmt.Sprintf("branch=send reason=commit_failed code=%s", mcpAppErrorCode(err)))
			writeMCPAppInputError(w, err)
			return
		}
		s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "ui/message", "ok",
			fmt.Sprintf("branch=send blocks=%d created=%t", len(body.Content), commit.Created))
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
			fmt.Sprintf("branch=edit blocks=%d", len(body.Content)))
		writeJSON(w, http.StatusOK, map[string]any{
			"result":    "edit",
			"prefilled": map[string]any{"text": text},
		})
	default:
		s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "ui/message", "ok",
			fmt.Sprintf("branch=discard blocks=%d", len(body.Content)))
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
	parsed, err := url.Parse(strings.TrimSpace(body.URL))
	scheme := ""
	if parsed != nil {
		scheme = strings.ToLower(parsed.Scheme)
	}
	if err != nil || parsed == nil || parsed.Host == "" || (scheme != "http" && scheme != "https") {
		s.recordMCPAppAudit(r.Context(), tenantID, uuid.Nil, app, "ui/open-link", "refused",
			fmt.Sprintf("reason=unsupported_scheme scheme=%s", mcpAppBounded(scheme, 32)))
		writeMCPAppRPCErr(w, http.StatusBadRequest, -32602, "url must be an absolute http(s) URL")
		return
	}
	if s.cfg.Decisions == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("decisions are not wired"))
		return
	}
	sessionID, ok := s.mcpAppSession(w, r, tenantID, body.SessionID, false)
	if !ok {
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

// mcpAppMessageText joins the text blocks of a ui/message and refuses every
// other block type: silently dropping a non-text block would send a message the
// app did not ask for.
func mcpAppMessageText(content []mcpAppMessageContent) (string, error) {
	if len(content) == 0 {
		return "", errors.New("content must contain at least one text block")
	}
	parts := make([]string, 0, len(content))
	for _, block := range content {
		if block.Type != "text" {
			return "", fmt.Errorf("unsupported content type %q (only text is accepted)", mcpAppBounded(block.Type, 32))
		}
		if strings.TrimSpace(block.Text) == "" {
			return "", errors.New("text content must not be empty")
		}
		parts = append(parts, block.Text)
	}
	text := strings.Join(parts, "\n")
	if strings.TrimSpace(text) == "" {
		return "", errors.New("content must not be empty")
	}
	return text, nil
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
// web/src/screens/tool-families.ts (FAMILY_WORDS.changes). A tool without a
// destructiveHint whose name carries one of these words is treated as
// destructive and confirmed per call.
var mcpAppDestructiveWords = map[string]bool{
	"edit": true, "write": true, "patch": true, "apply": true, "replace": true,
	"create": true, "delete": true, "remove": true, "mkdir": true, "move": true,
	"rename": true, "git": true, "commit": true, "diff": true, "build": true,
	"test": true, "install": true, "deploy": true, "publish": true,
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
