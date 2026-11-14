// Decision endpoint: POST /v1/decisions/ask.
//
// This is the agent/UI-facing ask surface over the structured decision model.
// It adds no second provider client: request and response shapes, validation,
// credential handling, error sanitization and the provider call all live in
// internal/mediagw (the media gateway already exposes the same capability at
// POST /v1/media/decision). The edge here only adapts the public ask shape:
//
//	{"input": <string|messages>, "questions": [...], "model": "<optional>"}
//	-> {"answers": [...], "model": "...", "usage": {...}}
//
// The provider credential never reaches a client: it is read from
// KEEL_MEDIA_API_KEY / KEEL_API_KEY and only written into the upstream
// Authorization header by mediagw. An unconfigured deployment answers 503; an
// unknown model or a malformed question answers 400.

package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"github.com/jixoai/keel/internal/mediagw"
)

const (
	// decisionModelEnv is this endpoint's decision allowlist. It is deliberately
	// independent of the media gateway's KEEL_MEDIA_MODEL_DECISION: the ask
	// surface names one allowed model, defaults to the token-plan decision model,
	// and refuses anything else instead of silently re-routing it.
	decisionModelEnv = "KEEL_DECISION_MODEL"
	// decisionMaxBody caps one ask body. The input may be a message array, so it
	// shares the media body bound instead of the smaller default.
	decisionMaxBody = 1 << 20
)

// decisionAPI is the thin edge over the media gateway's decision operation.
type decisionAPI struct {
	gw *mediagw.Gateway
}

// registerDecisionRoutes wires POST /v1/decisions/ask. The gateway is built once
// from the process environment, like the media routes; a missing base URL or
// credential leaves it unconfigured, which answers 503 per request.
func (s *Server) registerDecisionRoutes() {
	cfg := mediagw.ConfigFromEnv(nil)
	cfg.ModelDecision = mediagw.DefaultModelDecision
	if model := strings.TrimSpace(os.Getenv(decisionModelEnv)); model != "" {
		cfg.ModelDecision = model
	}
	api := &decisionAPI{gw: mediagw.New(cfg)}
	s.mux.HandleFunc("POST /v1/decisions/ask", api.ask)
}

// ask handles POST /v1/decisions/ask. The tenant header follows the rest of
// /v1: the auth middleware injects it, and a cross-tenant caller never reaches
// the provider.
func (a *decisionAPI) ask(w http.ResponseWriter, r *http.Request) {
	if !mediaTenant(w, r) {
		return
	}
	var body struct {
		Input     json.RawMessage    `json:"input"`
		Questions []mediagw.Question `json:"questions"`
		Model     string             `json:"model"`
	}
	if err := decodeBody(w, r, decisionMaxBody, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	result, err := a.gw.Decision(r.Context(), mediagw.DecisionRequest{
		Input:     body.Input,
		Questions: body.Questions,
		Model:     body.Model,
	})
	if err != nil {
		writeMediaError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
