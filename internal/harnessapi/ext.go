package harnessapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/placement"
	"github.com/jixoai/keel/internal/store"
)

// Extension query/control plane: plugins, triggers and sub-agents. Reads are
// tenant-scoped; writes go through the same authentication middleware as every
// other route.

func (s *Server) listPlugins(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantFrom(w, r); !ok {
		return
	}
	if s.cfg.Plugins == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("plugins are not wired"))
		return
	}
	snapshot := s.cfg.Plugins.Snapshot()
	body := map[string]any{
		"generation": snapshot.Generation,
		"plugins":    snapshot.Plugins,
		"hooks":      snapshot.Hooks,
		"tools":      snapshot.Tools,
	}
	if s.cfg.Hooks != nil {
		body["hookStats"] = s.cfg.Hooks.Stats()
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) listTriggers(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.Triggers == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("triggers are not wired"))
		return
	}
	// The engine is process-wide; the list is the caller tenant's slice. A
	// preview of another tenant's prompts/audit was the cross-tenant read this
	// route used to leak.
	writeJSON(w, http.StatusOK, map[string]any{"triggers": s.cfg.Triggers.ListForTenant(tenantID)})
}

// fireTriggerRequest is the manual/webhook fire body.
type fireTriggerRequest struct {
	IdempotencyKey string `json:"idempotencyKey"`
	Reason         string `json:"reason"`
}

func (s *Server) fireTrigger(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	triggerID := r.PathValue("id")
	if triggerID == "" {
		writeErr(w, http.StatusBadRequest, errors.New("trigger id is required"))
		return
	}
	if s.cfg.Triggers == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("triggers are not wired"))
		return
	}
	// Read the raw body once: the HMAC signature covers the exact bytes, so
	// decoding them and re-marshalling would break every valid signature.
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var body fireTriggerRequest
	if len(bytes.TrimSpace(raw)) != 0 {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	key := body.IdempotencyKey
	if key == "" {
		key = r.Header.Get("Idempotency-Key")
	}
	reason := body.Reason
	// A signed request is a webhook delivery even when the sender does not set
	// the convenience header, and a signature cannot be downgraded by a body
	// that says "manual": the signature headers are the selector.
	if r.Header.Get("X-Keel-Webhook") == "1" ||
		r.Header.Get("X-Keel-Signature") != "" ||
		r.Header.Get("X-Keel-Timestamp") != "" {
		reason = "webhook"
	} else if reason == "" {
		reason = "manual"
	}
	// The reason is provenance written into the turn payload: a closed set, so a
	// caller cannot dress a manual fire up as a schedule, and bounded.
	switch reason {
	case "manual", "schedule", "webhook":
	default:
		writeErr(w, http.StatusBadRequest, errors.New(`reason must be "manual", "schedule" or "webhook"`))
		return
	}
	if len(reason) > 32 {
		writeErr(w, http.StatusBadRequest, errors.New("reason is too long"))
		return
	}
	// A manual fire records the caller as created_by_account_id; an unattended
	// fire records the Bot's own account. The identity comes from the edge,
	// never from the body.
	var createdBy uuid.UUID
	if s.cfg.Access != nil {
		principal, err := s.cfg.Access.Principal(r.Context(), tenantID)
		if err != nil {
			writeIdentityError(w, http.StatusUnauthorized, "invalid_session", "authentication required")
			return
		}
		createdBy = principal.AccountID
	}
	result, err := s.cfg.Triggers.Fire(r.Context(), triggerID, tenantID, harnessext.FireRequest{
		IdempotencyKey:     key,
		Reason:             reason,
		WebhookTimestamp:   r.Header.Get("X-Keel-Timestamp"),
		WebhookSignature:   r.Header.Get("X-Keel-Signature"),
		WebhookBody:        raw,
		CreatedByAccountID: createdBy,
	})
	if err != nil {
		switch {
		case errors.Is(err, harnessext.ErrTriggerNotFound):
			writeErr(w, http.StatusNotFound, err)
		case errors.Is(err, harnessext.ErrTriggerDisabled):
			writeErr(w, http.StatusConflict, err)
		case errors.Is(err, harnessext.ErrTriggerBotArchived),
			errors.Is(err, store.ErrConflict):
			// The trigger exists but its Bot (or the pinned session) can no
			// longer own a new session.
			writeErr(w, http.StatusConflict, err)
		case errors.Is(err, harnessext.ErrTriggerBotMissing),
			errors.Is(err, harnessext.ErrTriggerBotTenant):
			// A fire whose Bot cannot be resolved in this tenant is forbidden;
			// 404 would leak whether the handle exists elsewhere.
			writeErr(w, http.StatusForbidden, err)
		case errors.Is(err, harnessext.ErrTriggerBotUnavailable):
			writeErr(w, http.StatusServiceUnavailable, err)
		case errors.Is(err, harnessext.ErrTriggerKeyRequired),
			errors.Is(err, harnessext.ErrInvalidTrigger):
			writeErr(w, http.StatusBadRequest, err)
		case errors.Is(err, harnessext.ErrTriggerWebhookUnauthorized):
			writeErr(w, http.StatusUnauthorized, err)
		case errors.Is(err, harnessext.ErrTriggerWebhookUnconfigured):
			// Fail closed: a webhook trigger with no HMAC secret accepts no
			// signed delivery. The tenant still cannot see the (absent) secret.
			writeErr(w, http.StatusForbidden, err)
		case errors.Is(err, harnessext.ErrTriggerBusy):
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": err.Error(), "result": result})
		case errors.Is(err, kernel.ErrQuota):
			writeErr(w, http.StatusTooManyRequests, err)
		case errors.Is(err, harnessext.ErrTriggerStartFailed):
			// The trigger ran but its turn could not start: the body carries the
			// audit result (attempts, idempotency key) so the caller can retry.
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "result": result})
		default:
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

func (s *Server) listSubagents(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.Subagents == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("sub-agents are not wired"))
		return
	}
	limit, err := queryInt(r, "limit", 50, 1, 200)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var parent *uuid.UUID
	if raw := r.URL.Query().Get("parentSessionId"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("parentSessionId must be a UUID"))
			return
		}
		parent = &parsed
	}
	records := s.cfg.Subagents.List(tenantID, parent)
	if len(records) > limit {
		records = records[:limit]
	}
	// Sub-agent records are session resources: the list is relation-filtered,
	// never every sub-agent in the tenant.
	candidates := make([]string, 0, len(records))
	for _, record := range records {
		if parsed, err := uuid.Parse(record.SessionID); err == nil {
			candidates = append(candidates, parsed.String())
		}
	}
	allowed, ok := s.visibleSessionIDs(w, r, tenantID, access.KeySessionRead, candidates)
	if !ok {
		return
	}
	filtered := records[:0]
	for _, record := range records {
		if allowed[record.SessionID] {
			filtered = append(filtered, record)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"subagents": filtered})
}

func (s *Server) getSubagent(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	sessionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("sub-agent id must be a UUID"))
		return
	}
	if s.cfg.Subagents == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("sub-agents are not wired"))
		return
	}
	record, found := s.cfg.Subagents.Get(sessionID)
	if !found || record.TenantID != tenantID.String() {
		writeErr(w, http.StatusNotFound, errors.New("sub-agent not found"))
		return
	}
	if !s.authorizeSessionResource(w, r, tenantID, sessionID, access.KeySessionRead) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subagent": record})
}

// requireSubagentBotInvoke closes the botId bypass on the HTTP sub-agent
// routes (A4.2b1): a caller naming a Bot target needs
// bot.invoke on it, exactly like the model-facing `agent` tool. A call naming
// no target spawns the parent's own Bot and needs no extra capability (the
// route's session.write/relation gate already authorized the parent).
func (s *Server) requireSubagentBotInvoke(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, botID string, selection json.RawMessage) bool {
	// The runner's childSelection() prefers a non-empty selection over the
	// top-level field, so checking only one would let the authorization and the
	// actual child binding name different Bots (the exact hole toolauth's
	// agentTargets closes on the model-facing path). Authorize the union.
	targets := uniqueSubagentTargets(strings.TrimSpace(botID), selectionBotID(selection))
	if len(targets) == 0 {
		return true
	}
	if s.cfg.Access == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("the identity surface is not wired in this deployment"))
		return false
	}
	principal, err := s.cfg.Access.Principal(r.Context(), tenantID)
	if err != nil {
		writeIdentityError(w, http.StatusUnauthorized, "invalid_session", "authentication required")
		return false
	}
	for _, target := range targets {
		decision, err := s.cfg.Access.Authorize(r.Context(), principal, access.KeyBotInvoke, access.Resource{
			TenantID: tenantID, Type: "bot", ID: target,
		})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return false
		}
		if decision.Denied() {
			writeDenied(w, decision)
			return false
		}
		// bot.invoke is high risk: every accepted target is audited like the
		// HTTP invoke route. A failed audit write refuses the call.
		if err := s.cfg.Access.AuditInvoke(r.Context(), tenantID, principal, target, uuid.Nil, "authorized"); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return false
		}
	}
	return true
}

// uniqueSubagentTargets trims and deduplicates the Bot targets the HTTP edge
// must authorize. An empty target is not a capability: it means the child runs
// the parent's own binding.
func uniqueSubagentTargets(targets ...string) []string {
	seen := make(map[string]bool, len(targets))
	out := make([]string, 0, len(targets))
	for _, target := range targets {
		target = strings.TrimSpace(target)
		if target == "" || seen[target] {
			continue
		}
		seen[target] = true
		out = append(out, target)
	}
	return out
}

// selectionBotID extracts a botId from a selection fact. A fact that names a
// team is not a bot target (the team decides its captain); it is left to the
// existing team validation.
func selectionBotID(selection json.RawMessage) string {
	if len(strings.TrimSpace(string(selection))) == 0 {
		return ""
	}
	var fact struct {
		BotID     string `json:"botId"`
		TeamBotID string `json:"teamBotId"`
	}
	if err := json.Unmarshal(selection, &fact); err != nil {
		return ""
	}
	if strings.TrimSpace(fact.TeamBotID) != "" {
		return ""
	}
	return strings.TrimSpace(fact.BotID)
}

// spawnSubagentRequest is the additive HTTP form of the spawn primitive. The
// model-facing agent tool uses the same runner.
type spawnSubagentRequest struct {
	Task           string            `json:"task"`
	Agent          string            `json:"agent"`
	BotID          string            `json:"botId"`
	Selection      json.RawMessage   `json:"selection"`
	IdempotencyKey string            `json:"idempotencyKey"`
	Budget         harnessext.Budget `json:"budget"`
	Wait           bool              `json:"wait"`
}

func (s *Server) spawnSubagent(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	parentID, ok := sessionFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.Subagents == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("sub-agents are not wired"))
		return
	}
	var body spawnSubagentRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !s.requireSubagentBotInvoke(w, r, tenantID, body.BotID, body.Selection) {
		return
	}
	record, err := s.cfg.Subagents.SpawnAsync(r.Context(), harnessext.SpawnRequest{
		TenantID:        tenantID,
		ParentSessionID: parentID,
		Task:            body.Task,
		Agent:           body.Agent,
		BotID:           body.BotID,
		Selection:       body.Selection,
		Budget:          body.Budget,
	})
	if err != nil {
		writeSubagentError(w, err)
		return
	}
	if !body.Wait {
		writeJSON(w, http.StatusAccepted, map[string]any{"subagent": record})
		return
	}
	budget := body.Budget
	if budget.TimeoutMs <= 0 {
		budget.TimeoutMs = s.cfg.SubagentWaitDefault.Milliseconds()
	}
	sessionID, err := uuid.Parse(record.SessionID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// The wait is bounded by its own budget, not by the HTTP request: a client
	// that disconnects must not cancel the sub-agent it started.
	waitCtx, cancelWait := context.WithTimeout(context.WithoutCancel(r.Context()),
		time.Duration(budget.TimeoutMs)*time.Millisecond)
	defer cancelWait()
	result, err := s.cfg.Subagents.Wait(waitCtx, tenantID, sessionID, budget, body.Agent)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if latest, found := s.cfg.Subagents.Get(sessionID); found {
		record = latest
	}
	writeJSON(w, http.StatusOK, map[string]any{"subagent": record, "result": result})
}

// writeSubagentError maps the runner's typed errors to the HTTP edge. Capacity
// is 429 + Retry-After exactly like the input/trigger paths (W2's admission
// vocabulary); a terminal resume is a normal 200 answer, not an error.
func writeSubagentError(w http.ResponseWriter, err error) {
	var capacity *placement.CapacityError
	switch {
	case errors.Is(err, harnessext.ErrParentSessionNotFound), errors.Is(err, harnessext.ErrSubagentNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, kernel.ErrQuota):
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusTooManyRequests, err)
	case errors.As(err, &capacity):
		// Ceil, exactly like the input/trigger paths: a sub-second wait still
		// advertises one second.
		seconds := int((capacity.RetryAfter + time.Second - 1) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		writeErr(w, http.StatusTooManyRequests, err)
	case errors.Is(err, kernel.ErrExternalSession):
		writeErr(w, http.StatusConflict, err)
	case errors.Is(err, harnessext.ErrForkSnapshotMissing):
		writeErr(w, http.StatusConflict, err)
	case errors.Is(err, harnessext.ErrForkUnavailable), errors.Is(err, harnessext.ErrResumeUnavailable),
		errors.Is(err, harnessext.ErrCancelUnavailable):
		writeErr(w, http.StatusServiceUnavailable, err)
	default:
		writeErr(w, http.StatusBadRequest, err)
	}
}

// forkSubagentRequest is the additive HTTP form of agent.fork. The model-facing
// tool uses the same runner.
type forkSubagentRequest struct {
	EventSeq       int64             `json:"eventSeq"`
	Task           string            `json:"task"`
	Title          string            `json:"title"`
	TaskID         string            `json:"taskId"`
	Agent          string            `json:"agent"`
	BotID          string            `json:"botId"`
	Selection      json.RawMessage   `json:"selection"`
	IdempotencyKey string            `json:"idempotencyKey"`
	Budget         harnessext.Budget `json:"budget"`
	Wait           bool              `json:"wait"`
}

func (s *Server) forkSubagent(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	parentID, ok := sessionFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.Subagents == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("sub-agents are not wired"))
		return
	}
	var body forkSubagentRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !s.requireSubagentBotInvoke(w, r, tenantID, body.BotID, body.Selection) {
		return
	}
	record, err := s.cfg.Subagents.ForkAsync(r.Context(), harnessext.ForkRequest{
		TenantID:        tenantID,
		ParentSessionID: parentID,
		EventSeq:        body.EventSeq,
		Task:            body.Task,
		Title:           body.Title,
		TaskID:          body.TaskID,
		Agent:           body.Agent,
		BotID:           body.BotID,
		Selection:       body.Selection,
		Budget:          body.Budget,
	})
	if err != nil {
		writeSubagentError(w, err)
		return
	}
	if !body.Wait {
		writeJSON(w, http.StatusAccepted, map[string]any{"subagent": record})
		return
	}
	budget := body.Budget
	if budget.TimeoutMs <= 0 {
		budget.TimeoutMs = s.cfg.SubagentWaitDefault.Milliseconds()
	}
	sessionID, err := uuid.Parse(record.SessionID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	waitCtx, cancelWait := context.WithTimeout(context.WithoutCancel(r.Context()),
		time.Duration(budget.TimeoutMs)*time.Millisecond)
	defer cancelWait()
	result, err := s.cfg.Subagents.Wait(waitCtx, tenantID, sessionID, budget, body.Agent)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if latest, found := s.cfg.Subagents.Get(sessionID); found {
		record = latest
	}
	writeJSON(w, http.StatusOK, map[string]any{"subagent": record, "result": result})
}

type resumeSubagentRequest struct {
	SessionID string          `json:"sessionId"`
	RequestID string          `json:"requestId"`
	Reason    string          `json:"reason"`
	Selection json.RawMessage `json:"selection"`
	BotID     string          `json:"botId"`
}

// resumeSubagent implements POST /v1/sessions/{id}/resume (supervision spec):
// idempotent, continues the same child, and answers a terminal run explicitly
// instead of pretending to resume it.
func (s *Server) resumeSubagent(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	parentID, ok := sessionFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.Subagents == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("sub-agents are not wired"))
		return
	}
	var body resumeSubagentRequest
	if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sessionID, err := uuid.Parse(body.SessionID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("sessionId must be a UUID"))
		return
	}
	// A botId/selection changes the binding the resumed child runs as;
	// the caller needs bot.invoke on that target exactly like a fresh spawn.
	if !s.requireSubagentBotInvoke(w, r, tenantID, body.BotID, body.Selection) {
		return
	}
	result, err := s.cfg.Subagents.Resume(r.Context(), harnessext.ResumeRequest{
		TenantID:        tenantID,
		ParentSessionID: parentID,
		SessionID:       sessionID,
		RequestID:       body.RequestID,
		Selection:       body.Selection,
		BotID:           body.BotID,
	})
	if err != nil {
		writeSubagentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resume": result})
}

type cancelSubagentRequest struct {
	Reason string `json:"reason"`
}

// cancelSubagent implements POST /v1/subagents/{id}/cancel. The parent session
// is the query parameter (the child id alone cannot authorize a stop).
func (s *Server) cancelSubagent(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	if s.cfg.Subagents == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("sub-agents are not wired"))
		return
	}
	childID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("sub-agent id must be a UUID"))
		return
	}
	parentID, err := uuid.Parse(r.URL.Query().Get("parentSessionId"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("parentSessionId must be a UUID"))
		return
	}
	// Stopping a child is a write on the parent session: the query parameter is
	// the authorizing resource, never the child id alone.
	if !s.authorizeSessionResource(w, r, tenantID, parentID, access.KeySessionWrite) {
		return
	}
	var body cancelSubagentRequest
	if r.ContentLength > 0 {
		if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	result, err := s.cfg.Subagents.Cancel(r.Context(), harnessext.CancelRequest{
		TenantID:        tenantID,
		ParentSessionID: parentID,
		SessionID:       childID,
	})
	if err != nil {
		writeSubagentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancel": result})
}
