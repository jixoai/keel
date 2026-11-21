package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/placement"
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
	if _, ok := tenantFrom(w, r); !ok {
		return
	}
	if s.cfg.Triggers == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("triggers are not wired"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"triggers": s.cfg.Triggers.List()})
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
	result, err := s.cfg.Triggers.Fire(r.Context(), triggerID, tenantID, agentext.FireRequest{
		IdempotencyKey:   key,
		Reason:           reason,
		WebhookTimestamp: r.Header.Get("X-Keel-Timestamp"),
		WebhookSignature: r.Header.Get("X-Keel-Signature"),
		WebhookBody:      raw,
	})
	if err != nil {
		switch {
		case errors.Is(err, agentext.ErrTriggerNotFound):
			writeErr(w, http.StatusNotFound, err)
		case errors.Is(err, agentext.ErrTriggerDisabled):
			writeErr(w, http.StatusConflict, err)
		case errors.Is(err, agentext.ErrTriggerKeyRequired),
			errors.Is(err, agentext.ErrInvalidTrigger):
			writeErr(w, http.StatusBadRequest, err)
		case errors.Is(err, agentext.ErrTriggerWebhookUnauthorized):
			writeErr(w, http.StatusUnauthorized, err)
		case errors.Is(err, agentext.ErrTriggerWebhookUnconfigured):
			// Fail closed: a webhook trigger with no HMAC secret accepts no
			// signed delivery. The tenant still cannot see the (absent) secret.
			writeErr(w, http.StatusForbidden, err)
		case errors.Is(err, agentext.ErrTriggerBusy):
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": err.Error(), "result": result})
		case errors.Is(err, kernel.ErrQuota):
			writeErr(w, http.StatusTooManyRequests, err)
		case errors.Is(err, agentext.ErrTriggerStartFailed):
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
	writeJSON(w, http.StatusOK, map[string]any{"subagents": records})
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
	writeJSON(w, http.StatusOK, map[string]any{"subagent": record})
}

// spawnSubagentRequest is the additive HTTP form of the spawn primitive. The
// model-facing agent tool uses the same runner.
type spawnSubagentRequest struct {
	Task           string          `json:"task"`
	Agent          string          `json:"agent"`
	AgentID        string          `json:"agentId"`
	Selection      json.RawMessage `json:"selection"`
	IdempotencyKey string          `json:"idempotencyKey"`
	Budget         agentext.Budget `json:"budget"`
	Wait           bool            `json:"wait"`
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
	record, err := s.cfg.Subagents.SpawnAsync(r.Context(), agentext.SpawnRequest{
		TenantID:        tenantID,
		ParentSessionID: parentID,
		Task:            body.Task,
		Agent:           body.Agent,
		AgentID:         body.AgentID,
		Selection:       body.Selection,
		IdempotencyKey:  body.IdempotencyKey,
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
	case errors.Is(err, agentext.ErrParentSessionNotFound), errors.Is(err, agentext.ErrSubagentNotFound):
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
	case errors.Is(err, agentext.ErrForkSnapshotMissing):
		writeErr(w, http.StatusConflict, err)
	case errors.Is(err, agentext.ErrForkUnavailable), errors.Is(err, agentext.ErrResumeUnavailable),
		errors.Is(err, agentext.ErrCancelUnavailable):
		writeErr(w, http.StatusServiceUnavailable, err)
	default:
		writeErr(w, http.StatusBadRequest, err)
	}
}

// forkSubagentRequest is the additive HTTP form of agent.fork. The model-facing
// tool uses the same runner.
type forkSubagentRequest struct {
	EventSeq       int64           `json:"eventSeq"`
	Task           string          `json:"task"`
	Title          string          `json:"title"`
	TaskID         string          `json:"taskId"`
	Agent          string          `json:"agent"`
	AgentID        string          `json:"agentId"`
	Selection      json.RawMessage `json:"selection"`
	IdempotencyKey string          `json:"idempotencyKey"`
	Budget         agentext.Budget `json:"budget"`
	Wait           bool            `json:"wait"`
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
	record, err := s.cfg.Subagents.ForkAsync(r.Context(), agentext.ForkRequest{
		TenantID:        tenantID,
		ParentSessionID: parentID,
		EventSeq:        body.EventSeq,
		Task:            body.Task,
		Title:           body.Title,
		TaskID:          body.TaskID,
		Agent:           body.Agent,
		AgentID:         body.AgentID,
		Selection:       body.Selection,
		IdempotencyKey:  body.IdempotencyKey,
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
	AgentID   string          `json:"agentId"`
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
	result, err := s.cfg.Subagents.Resume(r.Context(), agentext.ResumeRequest{
		TenantID:        tenantID,
		ParentSessionID: parentID,
		SessionID:       sessionID,
		RequestID:       body.RequestID,
		Reason:          body.Reason,
		Selection:       body.Selection,
		AgentID:         body.AgentID,
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
	var body cancelSubagentRequest
	if r.ContentLength > 0 {
		if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	result, err := s.cfg.Subagents.Cancel(r.Context(), agentext.CancelRequest{
		TenantID:        tenantID,
		ParentSessionID: parentID,
		SessionID:       childID,
		Reason:          body.Reason,
	})
	if err != nil {
		writeSubagentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancel": result})
}
