package agentapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/kernel"
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
	var body fireTriggerRequest
	if r.ContentLength != 0 {
		if err := decodeBody(w, r, s.cfg.MaxBody, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	key := body.IdempotencyKey
	if key == "" {
		key = r.Header.Get("Idempotency-Key")
	}
	reason := body.Reason
	if reason == "" {
		if r.Header.Get("X-Keel-Webhook") == "1" {
			reason = "webhook"
		} else {
			reason = "manual"
		}
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
		IdempotencyKey: key,
		Reason:         reason,
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
// model-facing tool (spawn_subagent) uses the same runner.
type spawnSubagentRequest struct {
	Task           string          `json:"task"`
	Agent          string          `json:"agent"`
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
		IdempotencyKey:  body.IdempotencyKey,
		Budget:          body.Budget,
	})
	if err != nil {
		switch {
		case errors.Is(err, agentext.ErrParentSessionNotFound):
			writeErr(w, http.StatusNotFound, err)
		case errors.Is(err, kernel.ErrQuota):
			writeErr(w, http.StatusTooManyRequests, err)
		case errors.Is(err, kernel.ErrExternalSession):
			writeErr(w, http.StatusConflict, err)
		default:
			writeErr(w, http.StatusBadRequest, err)
		}
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
