package agentapi

// Durable approval for plugin lifecycle writes (change keel-plugin-core, P2).
//
// Installing, enabling or upgrading a plugin adopts (or schedules) code the
// operator did not write, so the spec makes each of those transitions a durable
// question instead of a default: "安装/启用/升级 MUST 经 durable 审批（拒绝则
// 不生效）". The question IS a decision — the primitive the agent-side approvals
// already use (internal/agentext/decisions.go): a durable row with an answer
// journal, an idempotent answer and a deadline.
//
// The protocol is two-phase, because a durable question must be answerable at
// human speed while one HTTP request cannot stay open indefinitely:
//
//	POST /v1/plugins/install {"path": "..."}                      → 202 {"decision": ...}
//	POST /v1/plugins/approvals/{decisionId} {"optionId": "allow"}  (the human answers)
//	POST /v1/plugins/install {"path": "...", "decisionId": "..."}  → 201 (or 409 refused)
//
// The decision is bound to the request it authorizes: its id is derived from
// (tenant, action, requestId-or-target), so a decision opened for one action,
// one plugin or one package cannot authorize another. A retry with an answered
// decision proceeds; a refusal (deny, or a deadline that passed) never reaches
// the manager, and it is recorded in the same audit trail as a lifecycle action
// with result `refused` — an operator can tell "the approver said no" from "the
// action failed" (P2's audit requirement).
//
// The decision rows live in a session namespace derived from the tenant
// (pluginApprovalSession): the decision primitive is session-keyed and a plugin
// write arrives from a settings page, not from inside a chat session.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/pluginpkg"
	"github.com/jixoai/keel/internal/store"
)

// The closed answer vocabulary of the approval question.
const (
	pluginApprovalAllow = "allow"
	pluginApprovalDeny  = "deny"
	// pluginApprovalNamespace seeds the decision-session and the decision ids.
	// Both are derived, never random: a restarted process resolves the same
	// namespace and finds the question it asked before the crash.
	pluginApprovalNamespace = "keel.plugin.approval.v1"
	// pluginApprovalTargetMax bounds the action target (a plugin id or a package
	// path) that is embedded in the derived request id and rendered in the
	// prompt. A longer target is a client error, not a truncation: a truncated
	// target would let two packages share one question.
	pluginApprovalTargetMax = 256
	// pluginApprovalRequestIDMax bounds a client-supplied opener key.
	pluginApprovalRequestIDMax = 128
)

// Approval window bounds. The broker caps the final deadline at 24h anyway; the
// request-level bound keeps a typo ("timeoutMs": 999999999999) a 400 instead of
// a silently clamped value.
const (
	pluginApprovalMinTimeoutMs = int64(1_000)
	pluginApprovalMaxTimeoutMs = int64(24 * 60 * 60 * 1_000)
)

// PluginAuditWriter records one audit row for an action the manager never saw.
// Approval refusals are the case in point: the row must exist (result
// `refused`) and the manager cannot write it because it was never called.
// cmd/keel adapts the store (store.Store.AppendPluginAudit); a host without a
// writer logs the refusal instead of losing it silently.
type PluginAuditWriter interface {
	RecordPluginAudit(ctx context.Context, entry store.PluginAuditEntry) error
}

// pluginApprovalRequest is the approval-related part of a lifecycle write body.
// It is embedded flat into the install/enable/update bodies.
type pluginApprovalRequest struct {
	// RequestID is the opener's idempotency key for the question: a retry that
	// repeats it (with the same action and target) addresses the same row.
	RequestID string `json:"requestId,omitempty"`
	// DecisionID is the answered decision that authorizes this write. Passing it
	// is how a client completes the loop: open (202) → answer → write with the id.
	DecisionID string `json:"decisionId,omitempty"`
	// ApprovalTimeoutMs bounds the question's deadline (default: the decision
	// broker's). Bounded below and above; a value outside is a 400.
	ApprovalTimeoutMs int64 `json:"approvalTimeoutMs,omitempty"`
}

// pluginApprovalAsk is the resolved question of one lifecycle write.
type pluginApprovalAsk struct {
	// Action is the lifecycle action (store.PluginAction*).
	Action string
	// Target is what the action names: a plugin id, or the package path for an
	// install. It is bound into the decision id.
	Target string
	// Label is the human-facing target (id plus version when known).
	Label string
	// PluginID is the id an audit row uses. For an install it is the package
	// directory name, because the manifest has not been read yet.
	PluginID string
	// Version is the currently installed version, when known.
	Version string
}

// pluginWriteContext is the single entrance of every approval-gated lifecycle
// write (install, enable, update): the manager, the audit attribution and the
// durable approval gate in one place, so no route reaches the manager while
// skipping the question. When it returns false the response is already written.
func (s *Server) pluginWriteContext(w http.ResponseWriter, r *http.Request, ask pluginApprovalAsk, approval pluginApprovalRequest) (PluginManager, agentext.AuditContext, bool) {
	manager, audit, ok := s.pluginCallContext(w, r)
	if !ok {
		return nil, agentext.AuditContext{}, false
	}
	if !s.gatePluginApproval(w, r, audit, ask, approval) {
		return nil, agentext.AuditContext{}, false
	}
	return manager, audit, true
}

// gatePluginApproval applies the durable approval rule. When it answers false
// the response is already written and the action must not proceed.
func (s *Server) gatePluginApproval(w http.ResponseWriter, r *http.Request, audit agentext.AuditContext, ask pluginApprovalAsk, req pluginApprovalRequest) bool {
	if s.cfg.Decisions == nil {
		// Fail closed: a deployment without the decision primitive cannot ask the
		// durable question, so it must not perform the action.
		writeErr(w, http.StatusServiceUnavailable,
			errors.New("plugin lifecycle writes require the durable approval primitive, but decisions are not wired in this deployment"))
		return false
	}
	requestID, err := pluginApprovalRequestID(ask, req)
	if err != nil {
		// A request that cannot even name its target never reaches an approver:
		// it is the manager's own field-level error, rendered by the same writer
		// the manager's failures use, so the client sees one error vocabulary.
		var askErr *agentext.PluginError
		if errors.As(err, &askErr) {
			s.writePluginError(w, askErr)
			return false
		}
		writeErr(w, http.StatusBadRequest, err)
		return false
	}
	timeout, err := pluginApprovalTimeout(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return false
	}
	tenantID := *audit.TenantID
	sessionID := pluginApprovalSession(tenantID)
	expectedID := agentext.DecisionIDFor(tenantID, sessionID, requestID)

	if req.DecisionID != "" {
		given, err := uuid.Parse(strings.TrimSpace(req.DecisionID))
		if err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("decisionId must be a UUID"))
			return false
		}
		if given != expectedID {
			// A decision opened for another action, another plugin or another
			// package never authorizes this write — the id is the binding. The
			// client holds the requestId it asked with (or asked without one), so
			// the message names both ways out.
			writeErr(w, http.StatusBadRequest, errors.New(
				"decisionId does not authorize this request: repeat it with the same requestId it was asked with "+
					"(or with no requestId at all), and drop decisionId to ask again"))
			return false
		}
		decision, found := s.cfg.Decisions.Get(r.Context(), tenantID, sessionID, expectedID)
		if !found {
			writeErr(w, http.StatusNotFound, errors.New("approval decision not found"))
			return false
		}
		switch {
		case isPluginApprovalGrant(decision):
			return true
		case decision.Status == agentext.DecisionOpen:
			writeJSON(w, http.StatusConflict, pluginApprovalPendingBody(decision))
			return false
		default:
			reason := "the approval decision is " + string(decision.Status)
			if decision.Answer != nil && decision.Answer.OptionID != "" {
				reason = "the approver answered " + decision.Answer.OptionID
			}
			s.refusePluginAction(r.Context(), audit, ask, decision, reason)
			writeJSON(w, http.StatusConflict, pluginApprovalRefusedBody(decision, reason))
			return false
		}
	}

	decision, _, err := s.cfg.Decisions.Open(r.Context(), agentext.OpenDecisionRequest{
		TenantID:  tenantID,
		SessionID: sessionID,
		RequestID: requestID,
		Kind:      pluginApprovalKind(ask.Action),
		Prompt:    pluginApprovalPrompt(ask),
		Options: []agentext.DecisionOption{
			{ID: pluginApprovalAllow, Label: "Approve"},
			{ID: pluginApprovalDeny, Label: "Deny"},
		},
		Timeout: timeout,
	})
	if err != nil {
		if errors.Is(err, agentext.ErrInvalidDecision) {
			writeErr(w, http.StatusBadRequest, err)
			return false
		}
		writeErr(w, http.StatusInternalServerError, err)
		return false
	}
	writeJSON(w, http.StatusAccepted, pluginApprovalPendingBody(decision))
	return false
}

// isPluginApprovalGrant reports whether an answered decision allows the action.
func isPluginApprovalGrant(decision agentext.Decision) bool {
	return decision.Status == agentext.DecisionAnswered &&
		decision.Answer != nil && decision.Answer.OptionID == pluginApprovalAllow
}

// pluginApprovalPendingBody is the 202/409 answer while the question is open.
func pluginApprovalPendingBody(decision agentext.Decision) map[string]any {
	return map[string]any{
		"approvalRequired": true,
		"decision":         decision,
		"answerUrl":        pluginApprovalAnswerURL(decision.DecisionID),
	}
}

// pluginApprovalRefusedBody is the 409 answer of a refusal. The hint matters:
// the question's id is derived from the action and the target, so a terminal
// answer keeps refusing the same request — asking again is an explicit new
// question (a new requestId), never an accidental one.
func pluginApprovalRefusedBody(decision agentext.Decision, reason string) map[string]any {
	return map[string]any{
		"error":    "plugin action refused: " + reason,
		"refused":  true,
		"decision": decision,
		"hint":     "the question is terminal; repeat the request with a new requestId (and no decisionId) to ask again",
	}
}

func pluginApprovalAnswerURL(decisionID string) string {
	return "/v1/plugin-approvals/" + decisionID
}

// pluginApprovalRequestID derives the opener key of the question. It is
// deterministic per (action, target-or-client-key) so a retried request without
// a decisionId re-addresses the same row instead of duplicating the question.
func pluginApprovalRequestID(ask pluginApprovalAsk, req pluginApprovalRequest) (string, error) {
	if ask.Target == "" {
		// Only an install can arrive without a target (an enable/update names a
		// plugin in its path); the field error is the manager's own shape.
		failure := agentext.NewPluginError(ask.Action, ask.PluginID, agentext.ErrPluginPath, "path is required")
		failure.Fields = []pluginpkg.FieldError{{Field: "path", Message: "path is required"}}
		return "", failure
	}
	if len(ask.Target) > pluginApprovalTargetMax {
		return "", errors.New("the target is too long to be approved")
	}
	key := strings.TrimSpace(req.RequestID)
	if key == "" {
		key = ask.Target
	} else if len(key) > pluginApprovalRequestIDMax {
		return "", errors.New("requestId is too long")
	}
	return ask.Action + ":" + key, nil
}

// pluginApprovalTimeout resolves the question's deadline.
func pluginApprovalTimeout(req pluginApprovalRequest) (time.Duration, error) {
	if req.ApprovalTimeoutMs == 0 {
		return 0, nil // the broker's default applies
	}
	if req.ApprovalTimeoutMs < pluginApprovalMinTimeoutMs || req.ApprovalTimeoutMs > pluginApprovalMaxTimeoutMs {
		return 0, fmt.Errorf("approvalTimeoutMs must be between %d and %d", pluginApprovalMinTimeoutMs, pluginApprovalMaxTimeoutMs)
	}
	return time.Duration(req.ApprovalTimeoutMs) * time.Millisecond, nil
}

// pluginApprovalKind names the question's kind: "plugin.install" and friends, so
// a decision list can tell a plugin approval from an agent approval.
func pluginApprovalKind(action string) string { return "plugin." + action }

// pluginApprovalPrompt is the human-facing question.
func pluginApprovalPrompt(ask pluginApprovalAsk) string {
	label := ask.Label
	if label == "" {
		label = ask.Target
	}
	switch ask.Action {
	case store.PluginActionInstall:
		return fmt.Sprintf("Install the plugin package %s? Installing adopts its code into this deployment and lets it run.", label)
	case store.PluginActionEnable:
		return fmt.Sprintf("Enable plugin %s? Enabling runs what the plugin declares (tools, hooks, triggers, UI).", label)
	case store.PluginActionUpdate:
		return fmt.Sprintf("Update plugin %s to a new package? The running version is replaced.", label)
	}
	return fmt.Sprintf("Approve plugin %s of %s?", ask.Action, label)
}

// installApprovalAsk builds the ask of an install: the package path is the
// target (the manifest cannot be read here — reading it is the manager's job),
// and the audit row is attributed to the package directory name. An empty path
// keeps an empty target, which the gate reports as the manager's own field
// error instead of asking anyone to approve "the current directory".
func installApprovalAsk(path string) pluginApprovalAsk {
	target := strings.TrimSpace(path)
	if target == "" {
		return pluginApprovalAsk{
			Action:   store.PluginActionInstall,
			PluginID: "unknown",
		}
	}
	if absolute, err := filepath.Abs(target); err == nil && absolute != "" {
		target = filepath.Clean(absolute)
	}
	id := filepath.Base(filepath.FromSlash(target))
	return pluginApprovalAsk{
		Action:   store.PluginActionInstall,
		Target:   target,
		Label:    target,
		PluginID: id,
	}
}

// pluginIDApprovalAsk builds the ask of an enable or update.
func (s *Server) pluginIDApprovalAsk(action, id string) pluginApprovalAsk {
	version := ""
	if s.cfg.PluginManager != nil {
		for _, view := range s.cfg.PluginManager.Snapshot().Plugins {
			if view.ID == id {
				version = view.Version
				break
			}
		}
	}
	label := id
	if version != "" {
		label = id + " " + version
	}
	return pluginApprovalAsk{Action: action, Target: id, Label: label, PluginID: id, Version: version}
}

// pluginApprovalSession is the session namespace plugin approvals live in.
// Derived from the tenant, never random: a restarted process resolves it again.
func pluginApprovalSession(tenantID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(pluginApprovalNamespace+"|"+tenantID.String()))
}

// refusePluginAction records a refusal in the audit trail. The action was
// refused, not attempted: result `refused`, with the decision that refused it.
func (s *Server) refusePluginAction(ctx context.Context, audit agentext.AuditContext, ask pluginApprovalAsk, decision agentext.Decision, reason string) {
	detail := map[string]any{
		"reason":         reason,
		"decisionId":     decision.DecisionID,
		"decisionStatus": string(decision.Status),
	}
	if decision.RequestID != "" {
		detail["requestId"] = decision.RequestID
	}
	if decision.Answer != nil && decision.Answer.OptionID != "" {
		detail["optionId"] = decision.Answer.OptionID
	}
	if ask.Target != "" {
		detail["target"] = ask.Target
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		raw = []byte(`{}`)
	}
	pluginID := ask.PluginID
	if pluginID == "" {
		pluginID = "unknown"
	}
	entry := store.PluginAuditEntry{
		TS:       time.Now().UTC(),
		TenantID: audit.TenantID,
		Actor:    audit.Actor,
		PluginID: pluginID,
		Version:  ask.Version,
		Action:   ask.Action,
		Result:   store.PluginResultRefused,
		Detail:   raw,
	}
	if s.cfg.PluginAuditWriter == nil {
		s.cfg.Logger.Warn("plugin refusal is not audited: no audit writer is wired",
			"plugin", pluginID, "action", ask.Action, "reason", reason)
		return
	}
	if err := s.cfg.PluginAuditWriter.RecordPluginAudit(ctx, entry); err != nil {
		s.cfg.Logger.Error("plugin refusal audit write failed", "plugin", pluginID, "action", ask.Action, "err", err)
	}
}

// --- approval routes ------------------------------------------------------
//
// The routes live at /v1/plugin-approvals/{decisionId}, NOT under
// /v1/plugins/approvals/: a 4-segment "/v1/plugins/approvals/{decisionId}"
// pattern and the existing "/v1/plugins/{id}/update" pattern overlap (at
// /v1/plugins/approvals/update) with neither more specific than the other, and
// Go's ServeMux panics on exactly that conflict. The collection is
// tenant-scoped rather than session-scoped because a plugin write arrives from
// a settings page; the session-scoped decisions of a run keep living under
// /v1/sessions/{id}/decisions/{decisionId}.

func (s *Server) pluginApprovalRoutes() {
	s.mux.HandleFunc("GET /v1/plugin-approvals/{decisionId}", s.getPluginApproval)
	s.mux.HandleFunc("POST /v1/plugin-approvals/{decisionId}", s.answerPluginApproval)
}

func (s *Server) getPluginApproval(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.Decisions == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("decisions are not wired"))
		return
	}
	decisionID, ok := pluginApprovalIDFrom(w, r)
	if !ok {
		return
	}
	decision, found := s.cfg.Decisions.Get(r.Context(), tenantID, pluginApprovalSession(tenantID), decisionID)
	if !found {
		writeErr(w, http.StatusNotFound, errors.New("approval decision not found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"decision": decision})
}

func (s *Server) answerPluginApproval(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.Decisions == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("decisions are not wired"))
		return
	}
	decisionID, ok := pluginApprovalIDFrom(w, r)
	if !ok {
		return
	}
	var body answerDecisionRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	decision, duplicate, err := s.cfg.Decisions.Answer(r.Context(), agentext.AnswerDecisionRequest{
		TenantID:   tenantID,
		SessionID:  pluginApprovalSession(tenantID),
		DecisionID: decisionID,
		OptionID:   body.OptionID,
		Text:       body.Text,
	})
	if err != nil {
		switch {
		case errors.Is(err, agentext.ErrDecisionNotFound):
			writeErr(w, http.StatusNotFound, err)
		case errors.Is(err, agentext.ErrDecisionClosed),
			errors.Is(err, agentext.ErrDecisionConflict):
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

func pluginApprovalIDFrom(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	decisionID, err := uuid.Parse(r.PathValue("decisionId"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("decision id must be a UUID"))
		return uuid.Nil, false
	}
	return decisionID, true
}
