package agentapi

// Plugin UI bridge (change keel-plugin-core, W3): the capability channel a
// plugin's webview talks to as `keel.plugin.*`.
//
// Four verbs, no more:
//
//	keel.plugin.state()            GET  /v1/plugins/{id}/bridge/state
//	keel.plugin.callTool(name, a)  POST /v1/plugins/{id}/bridge/call
//	keel.plugin.requestApproval(r) POST /v1/plugins/{id}/bridge/approve
//	keel.plugin.onEvent(fn)        GET  /v1/plugins/{id}/bridge/subscribe  (SSE)
//
// The state verb is where the host pushes the plugin's own status and the theme
// tokens down to the UI ("宿主把主题令牌与状态下发给插件 UI"), and its answer is an
// explicit projection: never the raw manifest document, never the managed
// package path, never a provenance document, never a credential. A plugin UI
// therefore cannot read a host secret through the bridge even by accident —
// and the bridge never has one to leak, because it resolves everything from the
// plugin inventory.
//
// Deliberately absent, as the spec and site/docs/harness/plugins/ui.md §5 fix:
// arbitrary HTTP, filesystem access, shell execution, other plugins' tools,
// other sessions' events, and every secret. A plugin that needs a network
// capability needs a sidecar with a declared permission.
//
// Every verb requires the plugin to be *enabled*: the same gate as the UI
// hosting route, so a plugin's page cannot keep talking to the host after an
// operator disabled it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/pluginpkg"
	"github.com/jixoai/keel/internal/store"
)

// pluginBridgeRequestPrefix marks the questions a plugin opens for itself, so a
// subscription (and the audit reader) can tell this plugin's questions from
// everyone else's.
const pluginBridgeRequestPrefix = "plugin-bridge:"

// Bounds of a plugin-opened question and of a bridge tool call.
const (
	pluginBridgePromptMax    = 1024
	pluginBridgeOptionMax    = 8
	pluginBridgeOptionIDMax  = 64
	pluginBridgeLabelMax     = 128
	pluginBridgeToolNameMax  = 128
	pluginBridgeWaitMaxMs    = int64(10 * 60 * 1000)
	pluginBridgeQueueFrames  = 64
	pluginBridgeCallAction   = "bridge.call"
	pluginBridgeKindDecision = "plugin.bridge"
)

// pluginBridgeHeartbeat is the keep-alive interval of a subscription stream.
const pluginBridgeHeartbeat = 15 * time.Second

// PluginToolInvoker invokes a tool the plugin declared ("call own tools"). It
// is the bridge's only execution seam; the host process implements it over the
// registry the model itself calls, so a bridge call cannot reach a capability
// the plugin does not own. Nil answers 501.
type PluginToolInvoker interface {
	InvokePluginTool(ctx context.Context, pluginID, tool string, args json.RawMessage) (json.RawMessage, error)
}

func (s *Server) pluginBridgeRoutes() {
	s.mux.HandleFunc("GET /v1/plugins/{id}/bridge/state", s.bridgePluginState)
	s.mux.HandleFunc("POST /v1/plugins/{id}/bridge/call", s.bridgePluginCall)
	s.mux.HandleFunc("POST /v1/plugins/{id}/bridge/approve", s.bridgePluginApprove)
	s.mux.HandleFunc("GET /v1/plugins/{id}/bridge/approvals/{decisionId}", s.bridgePluginGetApproval)
	s.mux.HandleFunc("POST /v1/plugins/{id}/bridge/approvals/{decisionId}", s.bridgePluginAnswerApproval)
	s.mux.HandleFunc("GET /v1/plugins/{id}/bridge/subscribe", s.bridgePluginSubscribe)
}

// bridgePlugin is the resolved (enabled) plugin a bridge request speaks for.
type bridgePlugin struct {
	View     agentext.PluginView
	TenantID uuid.UUID
}

// bridgePluginFrom resolves and gates the {id}: the plugin must be installed and
// enabled. When it answers false the response is already written.
func (s *Server) bridgePluginFrom(w http.ResponseWriter, r *http.Request) (bridgePlugin, bool) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return bridgePlugin{}, false
	}
	if s.cfg.PluginManager == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("plugin management is not wired"))
		return bridgePlugin{}, false
	}
	id := r.PathValue("id")
	if !pluginNameValid(id) || strings.ContainsAny(id, "/\\") {
		writeErr(w, http.StatusBadRequest, errors.New("plugin id is malformed"))
		return bridgePlugin{}, false
	}
	for _, view := range s.cfg.PluginManager.Snapshot().Plugins {
		if view.ID != id {
			continue
		}
		if view.State != agentext.LifecycleEnabled {
			// Same rule as the UI hosting route: only an enabled plugin may talk
			// to the host (and only an enabled plugin's UI is served at all).
			writeErr(w, http.StatusForbidden,
				fmt.Errorf("plugin %s is %s: only enabled plugins are bridged", id, view.State))
			return bridgePlugin{}, false
		}
		return bridgePlugin{View: view, TenantID: tenantID}, true
	}
	writeErr(w, http.StatusNotFound, errors.New("plugin is not installed"))
	return bridgePlugin{}, false
}

// pluginBridgeStateResponse is the `keel.plugin.state()` document: the plugin's
// own row plus the host theme tokens. It is an allowlist projection — every
// field is chosen here, so a future inventory field cannot flow to a plugin UI
// just because it was added to the view.
type pluginBridgeStateResponse struct {
	ID          string                  `json:"id"`
	Version     string                  `json:"version,omitempty"`
	Kind        string                  `json:"kind,omitempty"`
	Description string                  `json:"description,omitempty"`
	State       agentext.LifecycleState `json:"state"`
	Bundled     bool                    `json:"bundled"`
	Provides    pluginpkg.Provides      `json:"provides"`
	// Permissions are the plugin's own declared names (never values).
	Permissions pluginpkg.Permissions `json:"permissions,omitempty"`
	Requires    []string              `json:"requires,omitempty"`
	Tools       []string              `json:"tools,omitempty"`
	Hooks       []agentext.HookEvent  `json:"hooks,omitempty"`
	Triggers    []string              `json:"triggers,omitempty"`
	Diagnostics []string              `json:"diagnostics,omitempty"`
	Revision    int64                 `json:"revision"`
	Generation  int64                 `json:"generation"`
	// Theme is the host theme token document (web/theme.json) the UI renders
	// itself with, so a plugin inherits the host's light/dark and spacing instead
	// of shipping its own design system.
	Theme  json.RawMessage          `json:"theme,omitempty"`
	Bridge pluginBridgeCapabilities `json:"bridge"`
}

// pluginBridgeCapabilities tells the page which verbs this host answers.
type pluginBridgeCapabilities struct {
	Call      bool `json:"call"`
	Approve   bool `json:"approve"`
	Subscribe bool `json:"subscribe"`
}

func (s *Server) bridgePluginState(w http.ResponseWriter, r *http.Request) {
	bridge, ok := s.bridgePluginFrom(w, r)
	if !ok {
		return
	}
	inventory := s.cfg.PluginManager.Snapshot()
	view := bridge.View
	body := pluginBridgeStateResponse{
		ID:          view.ID,
		Version:     view.Version,
		Kind:        view.Kind,
		Description: view.Description,
		State:       view.State,
		Bundled:     view.Bundled,
		Provides:    view.Provides,
		Permissions: view.Permissions,
		Requires:    view.Requires,
		Tools:       view.Tools,
		Hooks:       view.Hooks,
		Triggers:    view.Triggers,
		Diagnostics: view.Diagnostics,
		Revision:    inventory.Revision,
		Generation:  inventory.Generation,
		Theme:       s.cfg.Theme,
		Bridge: pluginBridgeCapabilities{
			Call:      s.cfg.PluginTools != nil,
			Approve:   s.cfg.Decisions != nil,
			Subscribe: s.cfg.Hooks != nil,
		},
	}
	if body.Tools == nil {
		body.Tools = []string{}
	}
	if body.Triggers == nil {
		body.Triggers = []string{}
	}
	writeJSON(w, http.StatusOK, body)
}

// bridgePluginCallRequest is the `keel.plugin.callTool(name, args)` body.
type bridgePluginCallRequest struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

func (s *Server) bridgePluginCall(w http.ResponseWriter, r *http.Request) {
	bridge, ok := s.bridgePluginFrom(w, r)
	if !ok {
		return
	}
	var body bridgePluginCallRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	tool := strings.TrimSpace(body.Tool)
	if tool == "" || len(tool) > pluginBridgeToolNameMax {
		writeErr(w, http.StatusBadRequest, errors.New("tool is required"))
		return
	}
	// Ownership first: the call must name a tool THIS plugin declares. Another
	// plugin's tool is not a permission decision the bridge can make — it is out
	// of the bridge's vocabulary entirely.
	if !pluginDeclaresTool(bridge.View, tool) {
		s.refuseBridgeCall(r, bridge, tool, "the tool is not declared by this plugin")
		writeErr(w, http.StatusForbidden, fmt.Errorf("tool %s is not declared by plugin %s", tool, bridge.View.ID))
		return
	}
	if s.cfg.PluginTools == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("the plugin bridge has no tool invocation seam in this deployment"))
		return
	}
	args := body.Args
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	result, err := s.cfg.PluginTools.InvokePluginTool(r.Context(), bridge.View.ID, tool, args)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tool":   tool,
		"result": json.RawMessage(result),
	})
}

// pluginDeclaresTool reports whether the plugin declares (and currently
// provides) the tool. The live surface is authoritative when it exists: a
// not-assembled plugin provides nothing to call.
func pluginDeclaresTool(view agentext.PluginView, tool string) bool {
	declared := false
	for _, name := range view.Provides.Tools {
		if name == tool {
			declared = true
			break
		}
	}
	if !declared {
		return false
	}
	if view.Runtime == nil {
		// Enabled but never assembled into the runtime (a UI-only plugin, or a
		// host with no loader): nothing is executing, so there is nothing to call.
		return false
	}
	for _, name := range view.Tools {
		if name == tool {
			return true
		}
	}
	return false
}

// refuseBridgeCall audits a bridge call that named something the plugin does not
// own ("越权被拒 MUST 写审计").
func (s *Server) refuseBridgeCall(r *http.Request, bridge bridgePlugin, tool, reason string) {
	detail, err := json.Marshal(map[string]any{"reason": reason, "tool": tool})
	if err != nil {
		detail = []byte(`{}`)
	}
	entry := store.PluginAuditEntry{
		TS:       time.Now().UTC(),
		TenantID: &bridge.TenantID,
		Actor:    "plugin:" + bridge.View.ID,
		PluginID: bridge.View.ID,
		Version:  bridge.View.Version,
		Action:   pluginBridgeCallAction,
		Result:   store.PluginResultRefused,
		Detail:   detail,
	}
	if s.cfg.PluginAuditWriter == nil {
		s.cfg.Logger.Warn("bridge refusal is not audited: no audit writer is wired", "plugin", bridge.View.ID, "tool", tool)
		return
	}
	if err := s.cfg.PluginAuditWriter.RecordPluginAudit(r.Context(), entry); err != nil {
		s.cfg.Logger.Error("bridge refusal audit write failed", "plugin", bridge.View.ID, "tool", tool, "err", err)
	}
}

// --- request approval -----------------------------------------------------

// bridgePluginApproveRequest is the `keel.plugin.requestApproval(...)` body. The
// page may ask and return immediately (waitMs omitted) or await the answer for
// a bounded window, which is the shape the UI template's `await` uses.
type bridgePluginApproveRequest struct {
	RequestID     string                    `json:"requestId,omitempty"`
	Prompt        string                    `json:"prompt"`
	Options       []agentext.DecisionOption `json:"options,omitempty"`
	AllowFreeText bool                      `json:"allowFreeText,omitempty"`
	TimeoutMs     int64                     `json:"timeoutMs,omitempty"`
	WaitMs        int64                     `json:"waitMs,omitempty"`
}

func (s *Server) bridgePluginApprove(w http.ResponseWriter, r *http.Request) {
	bridge, ok := s.bridgePluginFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.Decisions == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("decisions are not wired"))
		return
	}
	var body bridgePluginApproveRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := validateBridgeApproval(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	options := body.Options
	if len(options) == 0 {
		options = []agentext.DecisionOption{
			{ID: pluginApprovalAllow, Label: "Approve"},
			{ID: pluginApprovalDeny, Label: "Deny"},
		}
	}
	sessionID := pluginBridgeSession(bridge.TenantID, bridge.View.ID)
	decision, _, err := s.cfg.Decisions.Open(r.Context(), agentext.OpenDecisionRequest{
		TenantID:      bridge.TenantID,
		SessionID:     sessionID,
		RequestID:     bridgeRequestID(bridge.View.ID, body.RequestID),
		Kind:          pluginBridgeKindDecision,
		Prompt:        body.Prompt,
		Options:       options,
		AllowFreeText: body.AllowFreeText,
		Timeout:       bridgeApprovalTimeout(body),
	})
	if err != nil {
		if errors.Is(err, agentext.ErrInvalidDecision) {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	status := http.StatusAccepted
	if body.WaitMs > 0 {
		decision = s.bridgeWaitDecision(r, bridge, decision)
		if decision.Status.Terminal() {
			status = http.StatusOK
		}
	}
	writeJSON(w, status, map[string]any{
		"decision":    decision,
		"status":      decision.Status,
		"approvalUrl": bridgeApprovalURL(bridge.View.ID, decision.DecisionID),
	})
}

// bridgeWaitDecision blocks (bounded) for the answer. The wait gets its own
// context: a page that navigates away must not cancel the question it asked.
func (s *Server) bridgeWaitDecision(r *http.Request, bridge bridgePlugin, decision agentext.Decision) agentext.Decision {
	if decision.Status.Terminal() {
		return decision
	}
	decisionID, err := uuid.Parse(decision.DecisionID)
	if err != nil {
		return decision
	}
	wait, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), bridgeWaitDuration(decision))
	defer cancel()
	answered, err := s.cfg.Decisions.Wait(wait, bridge.TenantID, pluginBridgeSession(bridge.TenantID, bridge.View.ID), decisionID)
	if err != nil {
		return decision
	}
	return answered
}

// bridgeWaitDuration bounds a synchronous wait by the question's own deadline
// (never past it) and by the hard cap.
func bridgeWaitDuration(decision agentext.Decision) time.Duration {
	remaining := time.Until(decision.Deadline)
	if remaining <= 0 || remaining > time.Duration(pluginBridgeWaitMaxMs)*time.Millisecond {
		return time.Duration(pluginBridgeWaitMaxMs) * time.Millisecond
	}
	return remaining
}

func bridgeApprovalTimeout(body bridgePluginApproveRequest) time.Duration {
	if body.TimeoutMs <= 0 {
		return 0
	}
	if body.TimeoutMs > pluginApprovalMaxTimeoutMs {
		return time.Duration(pluginApprovalMaxTimeoutMs) * time.Millisecond
	}
	return time.Duration(body.TimeoutMs) * time.Millisecond
}

// bridgeRequestID renders the question's opener key: marked with the bridge
// prefix so subscriptions can attribute it, and derived (not random) when the
// page supplied a key so a retried ask addresses the same question.
func bridgeRequestID(pluginID, requestID string) string {
	key := strings.TrimSpace(requestID)
	if key == "" || len(key) > pluginApprovalRequestIDMax {
		key = uuid.NewString()
	}
	return pluginBridgeRequestPrefix + pluginID + ":" + key
}

func validateBridgeApproval(body *bridgePluginApproveRequest) error {
	body.Prompt = strings.TrimSpace(body.Prompt)
	if body.Prompt == "" {
		return errors.New("prompt is required")
	}
	if len(body.Prompt) > pluginBridgePromptMax {
		return errors.New("prompt is too long")
	}
	if len(body.Options) > pluginBridgeOptionMax {
		return fmt.Errorf("at most %d options are allowed", pluginBridgeOptionMax)
	}
	for i := range body.Options {
		option := &body.Options[i]
		option.ID = strings.TrimSpace(option.ID)
		if option.ID == "" || len(option.ID) > pluginBridgeOptionIDMax {
			return errors.New("every option needs an id of at most 64 characters")
		}
		if len(option.Label) > pluginBridgeLabelMax || len(option.Description) > pluginBridgeLabelMax*2 {
			return errors.New("an option label is too long")
		}
	}
	if body.WaitMs < 0 || body.WaitMs > pluginBridgeWaitMaxMs {
		return fmt.Errorf("waitMs must be between 0 and %d", pluginBridgeWaitMaxMs)
	}
	return nil
}

func bridgeApprovalURL(pluginID, decisionID string) string {
	return "/v1/plugins/" + pluginID + "/bridge/approvals/" + decisionID
}

// --- reading and answering a bridge question ------------------------------

func (s *Server) bridgePluginGetApproval(w http.ResponseWriter, r *http.Request) {
	bridge, decisionID, ok := s.bridgeApprovalFrom(w, r)
	if !ok {
		return
	}
	decision, found := s.cfg.Decisions.Get(r.Context(), bridge.TenantID, pluginBridgeSession(bridge.TenantID, bridge.View.ID), decisionID)
	if !found {
		writeErr(w, http.StatusNotFound, errors.New("decision not found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"decision": decision})
}

func (s *Server) bridgePluginAnswerApproval(w http.ResponseWriter, r *http.Request) {
	bridge, decisionID, ok := s.bridgeApprovalFrom(w, r)
	if !ok {
		return
	}
	var body answerDecisionRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	decision, duplicate, err := s.cfg.Decisions.Answer(r.Context(), agentext.AnswerDecisionRequest{
		TenantID:   bridge.TenantID,
		SessionID:  pluginBridgeSession(bridge.TenantID, bridge.View.ID),
		DecisionID: decisionID,
		OptionID:   body.OptionID,
		Text:       body.Text,
	})
	if err != nil {
		switch {
		case errors.Is(err, agentext.ErrDecisionNotFound):
			writeErr(w, http.StatusNotFound, err)
		case errors.Is(err, agentext.ErrDecisionClosed), errors.Is(err, agentext.ErrDecisionConflict):
			writeErr(w, http.StatusConflict, err)
		case errors.Is(err, agentext.ErrInvalidDecision):
			writeErr(w, http.StatusBadRequest, err)
		default:
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"decision":  decision,
		"duplicate": duplicate,
		"status":    decision.Status,
	})
}

// bridgeApprovalFrom resolves the plugin gate plus the {decisionId}, so a
// plugin can never read or answer another plugin's question.
func (s *Server) bridgeApprovalFrom(w http.ResponseWriter, r *http.Request) (bridgePlugin, uuid.UUID, bool) {
	bridge, ok := s.bridgePluginFrom(w, r)
	if !ok {
		return bridgePlugin{}, uuid.Nil, false
	}
	if s.cfg.Decisions == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("decisions are not wired"))
		return bridgePlugin{}, uuid.Nil, false
	}
	decisionID, err := uuid.Parse(r.PathValue("decisionId"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("decision id must be a UUID"))
		return bridgePlugin{}, uuid.Nil, false
	}
	return bridge, decisionID, true
}

// pluginBridgeSession is the decision namespace of one (tenant, plugin) pair:
// derived, so a restart resolves the same namespace and finds the questions.
func pluginBridgeSession(tenantID uuid.UUID, pluginID string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(pluginApprovalNamespace+"|bridge|"+tenantID.String()+"|"+pluginID))
}

// --- subscribe ------------------------------------------------------------

// bridgePluginSubscribe is `keel.plugin.onEvent(fn)`: a server-sent event stream
// of the payloads that belong to THIS plugin. Attribution is explicit, not
// "everything that happens":
//
//   - trigger.before/after whose trigger id starts with "<plugin>." (the ids the
//     runtime mints for the plugin's own triggers);
//   - decision.opened/answered whose requestId carries this plugin's bridge
//     prefix (the questions the plugin itself asked).
//
// Turn, sub-agent and lifecycle payloads are not attributable to a plugin and
// are never forwarded. The event set is selectable with ?events=a,b so a page
// subscribes to exactly what it renders.
func (s *Server) bridgePluginSubscribe(w http.ResponseWriter, r *http.Request) {
	bridge, ok := s.bridgePluginFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.Hooks == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("the hook bus is not wired"))
		return
	}
	events, err := bridgeSubscribeEvents(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errors.New("this connection cannot stream"))
		return
	}
	frames := make(chan agentext.HookPayload, pluginBridgeQueueFrames)
	drops := 0
	handler := func(_ context.Context, payload agentext.HookPayload) error {
		if !bridgeEventVisible(bridge.View.ID, payload) {
			return nil
		}
		select {
		case frames <- payload:
		default:
			// A slow page must not back-pressure the hook bus (the bus's own
			// contract is non-blocking): the frame is dropped and counted.
			drops++
		}
		return nil
	}
	unsubs := make([]func(), 0, len(events))
	for _, event := range events {
		unsubscribe, err := s.cfg.Hooks.Subscribe("plugin-bridge:"+bridge.View.ID, agentext.HookOptions{
			Event:   event,
			Handler: handler,
		})
		if err != nil {
			for _, unsubscribe := range unsubs {
				unsubscribe()
			}
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		unsubs = append(unsubs, unsubscribe)
	}
	defer func() {
		for _, unsubscribe := range unsubs {
			unsubscribe()
		}
		if drops > 0 {
			s.cfg.Logger.Warn("plugin bridge stream dropped frames", "plugin", bridge.View.ID, "dropped", drops)
		}
	}()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	fmt.Fprintf(w, "event: ready\ndata: {\"plugin\":%q}\n\n", bridge.View.ID)
	flusher.Flush()

	heartbeat := time.NewTicker(pluginBridgeHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case payload := <-frames:
			raw, err := json.Marshal(payload)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", payload.Event, raw)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		}
	}
}

// bridgeSubscribeEvents resolves the requested event names (default: the whole
// closed set). An unknown name is a 400: a typo must not silently subscribe to
// nothing.
func bridgeSubscribeEvents(r *http.Request) ([]agentext.HookEvent, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("events"))
	if raw == "" {
		return agentext.HookEvents(), nil
	}
	seen := make(map[agentext.HookEvent]bool)
	out := make([]agentext.HookEvent, 0, 4)
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !agentext.ValidHookEvent(name) {
			return nil, fmt.Errorf("%q is not a hook event", name)
		}
		event := agentext.HookEvent(name)
		if seen[event] {
			continue
		}
		seen[event] = true
		out = append(out, event)
	}
	if len(out) == 0 {
		return nil, errors.New("events must name at least one hook event")
	}
	return out, nil
}

// bridgeEventVisible reports whether a payload belongs to the plugin.
func bridgeEventVisible(pluginID string, payload agentext.HookPayload) bool {
	switch payload.Event {
	case agentext.HookTriggerBefore, agentext.HookTriggerAfter:
		return strings.HasPrefix(payload.TriggerID, pluginID+".")
	case agentext.HookDecisionOpened, agentext.HookDecisionAnswered:
		return strings.HasPrefix(payload.RequestID, pluginBridgeRequestPrefix+pluginID+":")
	}
	return false
}
