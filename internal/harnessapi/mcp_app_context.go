package harnessapi

// ui/update-model-context (change feat-mcp-apps-events, P5.5).
//
// An app's View may record a context document (content and/or structuredContent)
// for its session. The semantics are overwrite-only: each update replaces the
// previous value of that app, and the next attempt merges it into the prompt as
// <mcp_app_context>.
//
// The context is deliberately NOT written to the event ledger: it is not a
// session fact, has no replay semantics and is re-read from its own table by
// every attempt (design §3.6.3, §7-R5). A deployment without the store answers
// -32601 and reports mcp.apps.context=false; it never pretends to have recorded
// the context.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/store"
)

// MCPAppContextStore is the durable app-context seam. *store.Store implements
// it; tests substitute a fake.
type MCPAppContextStore interface {
	UpsertMCPAppContext(ctx context.Context, entry store.MCPAppContext) error
}

// mcpAppContextRequest is POST /v1/mcp/apps/{appId}/context.
type mcpAppContextRequest struct {
	SessionID         string          `json:"sessionId"`
	RequestID         string          `json:"requestId,omitempty"`
	Content           json.RawMessage `json:"content,omitempty"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
}

// mcpAppUpdateContext records one app's model context.
func (s *Server) mcpAppUpdateContext(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantFrom(w, r)
	if !ok {
		return
	}
	// The unwired answer comes before any app lookup: a capability this
	// deployment does not serve must not be probeable through 404s, and the
	// View must get the explicit -32601 it downgrades on.
	if s.cfg.MCPAppContexts == nil {
		writeMCPAppRPCErr(w, http.StatusNotImplemented, -32601,
			"ui/update-model-context is not implemented in this deployment")
		return
	}
	if s.cfg.MCPApps == nil {
		writeErr(w, http.StatusNotImplemented, errMCPAppsUnwired)
		return
	}
	appID := r.PathValue("appId")
	if !validAppID(appID) || !s.cfg.MCPApps.Visible(tenantID) {
		writeErr(w, http.StatusNotFound, errAppNotFound)
		return
	}
	app, found := s.cfg.MCPApps.App(appID)
	if !found {
		writeErr(w, http.StatusNotFound, errAppNotFound)
		return
	}
	// The context route is a bridge action like any other: it consumes the same
	// per-app rate/concurrency budget, so a View cannot bypass the gate through
	// this endpoint.
	release, ok := s.mcpAppGate(w, r, tenantID, uuid.Nil, app, "ui/update-model-context")
	if !ok {
		return
	}
	defer release()
	// The body carries the session id; decode before the session gate so a body
	// size violation keeps its 413 path.
	var body mcpAppContextRequest
	if !s.decodeMCPAppBody(w, r, tenantID, uuid.Nil, app, &body) {
		return
	}
	var sessionID uuid.UUID
	if body.SessionID != "" {
		var ok bool
		sessionID, ok = s.mcpAppSession(w, r, tenantID, body.SessionID, true)
		if !ok {
			return
		}
	}
	if sessionID == uuid.Nil {
		writeMCPAppRPCErr(w, http.StatusBadRequest, -32602, "sessionId is required")
		return
	}
	if !s.authorizeMCPAppSession(w, r, tenantID, sessionID, access.KeySessionWrite) {
		return
	}
	content, structured, err := validateMCPAppContext(body)
	if err != nil {
		s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "ui/update-model-context", "refused",
			"reason=invalid_context")
		writeMCPAppRPCErr(w, http.StatusBadRequest, -32602, err.Error())
		return
	}
	entry := store.MCPAppContext{
		TenantID:   tenantID,
		SessionID:  sessionID,
		AppID:      app.AppID,
		Content:    content,
		Structured: structured,
	}
	if err := s.cfg.MCPAppContexts.UpsertMCPAppContext(r.Context(), entry); err != nil {
		s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "ui/update-model-context", "error",
			"reason=store_error")
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.recordMCPAppAudit(r.Context(), tenantID, sessionID, app, "ui/update-model-context", "ok",
		contextAuditDetail(content, structured))
	writeJSON(w, http.StatusOK, map[string]any{
		"jsonrpc": "2.0",
		"result":  map[string]any{},
	})
}

// validateMCPAppContext bounds and validates the two documents. Content must be
// the block array/object shape (or a JSON string?), structuredContent any JSON;
// both are bounded by the store's constant. At least one must be present.
func validateMCPAppContext(body mcpAppContextRequest) (json.RawMessage, json.RawMessage, error) {
	content := trimContextDocument(body.Content)
	structured := trimContextDocument(body.StructuredContent)
	if len(content) == 0 && len(structured) == 0 {
		return nil, nil, errors.New("content or structuredContent is required")
	}
	for _, document := range []json.RawMessage{content, structured} {
		if len(document) == 0 {
			continue
		}
		if !json.Valid(document) {
			return nil, nil, errors.New("context documents must be valid JSON")
		}
		if len(document) > store.MCPAppContextMaxBytes {
			return nil, nil, errors.New("context documents exceed the size bound")
		}
	}
	return content, structured, nil
}

func trimContextDocument(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	trimmed := string(raw)
	if trimmed == "null" {
		return nil
	}
	return raw
}

// contextAuditDetail is identifier-only: sizes, never the document body.
func contextAuditDetail(content, structured json.RawMessage) string {
	return "contentBytes=" + strconv.Itoa(len(content)) + " structuredBytes=" + strconv.Itoa(len(structured))
}
