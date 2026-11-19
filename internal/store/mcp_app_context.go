package store

// MCP app model context persistence (change feat-mcp-apps-events, P5.5).
//
// `ui/update-model-context` records an overwrite-only context per (tenant,
// session, app). The next attempt reads it through the runner's
// SessionContexts seam and merges it into the prompt as <mcp_app_context>.
//
// This is deliberately not an event: app context is not a session fact, has no
// replay semantics, and the design (§3.6.3, §7-R5) explicitly rejects the
// "write a ledger kind" alternative. Nothing in this file touches events or the
// kernel; a failed write leaves the ledger untouched and the request answers an
// error instead of pretending to succeed.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MCPAppContextMaxBytes bounds each JSON document (content and structured) of
// one app context row. The document is prompt input, not a blob store.
const MCPAppContextMaxBytes = 64 << 10

// MCPAppContext is one durable app-context row.
type MCPAppContext struct {
	TenantID   uuid.UUID       `json:"tenantId"`
	SessionID  uuid.UUID       `json:"sessionId"`
	AppID      string          `json:"appId"`
	Content    json.RawMessage `json:"content,omitempty"`
	Structured json.RawMessage `json:"structuredContent,omitempty"`
	UpdatedAt  time.Time       `json:"updatedAt"`
}

// UpsertMCPAppContext overwrites one app's context for a session. The row is
// keyed on (tenant, session, app), so a second update replaces the first
// completely (the spec's "每次覆盖" semantics); it never appends.
func (s *Store) UpsertMCPAppContext(ctx context.Context, entry MCPAppContext) error {
	if entry.TenantID == uuid.Nil || entry.SessionID == uuid.Nil {
		return errors.New("store: mcp app context requires tenant and session")
	}
	appID := sanitizeAuditText(strings.TrimSpace(entry.AppID), 128)
	if appID == "" {
		return errors.New("store: mcp app context requires an app id")
	}
	content, err := boundContextDocument(entry.Content)
	if err != nil {
		return err
	}
	structured, err := boundContextDocument(entry.Structured)
	if err != nil {
		return err
	}
	if len(content) == 0 && len(structured) == 0 {
		return errors.New("store: mcp app context needs content or structuredContent")
	}
	if entry.UpdatedAt.IsZero() {
		entry.UpdatedAt = time.Now().UTC()
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO mcp_app_contexts (tenant_id, session_id, app_id, content, structured, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (tenant_id, session_id, app_id)
		 DO UPDATE SET content = EXCLUDED.content,
		               structured = EXCLUDED.structured,
		               updated_at = EXCLUDED.updated_at`,
		entry.TenantID, entry.SessionID, appID, content, structured, entry.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store: upsert mcp app context: %w", err)
	}
	return nil
}

// ListMCPAppContexts returns a session's app contexts, oldest first. The runner
// merges them into the next attempt's prompt in this order.
func (s *Store) ListMCPAppContexts(ctx context.Context, tenantID, sessionID uuid.UUID) ([]MCPAppContext, error) {
	if tenantID == uuid.Nil || sessionID == uuid.Nil {
		return nil, errors.New("store: mcp app context requires tenant and session")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT tenant_id, session_id, app_id, content, structured, updated_at
		   FROM mcp_app_contexts
		  WHERE tenant_id = $1 AND session_id = $2
		  ORDER BY updated_at, app_id`,
		tenantID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: list mcp app contexts: %w", err)
	}
	defer rows.Close()
	out := make([]MCPAppContext, 0, 4)
	for rows.Next() {
		var entry MCPAppContext
		if err := rows.Scan(&entry.TenantID, &entry.SessionID, &entry.AppID,
			&entry.Content, &entry.Structured, &entry.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan mcp app context: %w", err)
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// boundContextDocument validates one JSON document and bounds its bytes. An
// absent (nil/empty) document stays nil. Invalid JSON is a caller bug: refusing
// it keeps a broken document out of every later prompt.
func boundContextDocument(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if !json.Valid(raw) {
		return nil, errors.New("store: mcp app context document is not valid JSON")
	}
	if len(raw) > MCPAppContextMaxBytes {
		return nil, fmt.Errorf("store: mcp app context document exceeds %d bytes", MCPAppContextMaxBytes)
	}
	return append(json.RawMessage(nil), raw...), nil
}
