package store

// MCP App audit persistence (change feat-mcp-apps-events, P3.7).
//
// One row per bridged action or refusal. The table is written from two places:
// the app registry (document fetch/validation/cache outcomes) and the product
// API's app bridge (tools/call, resources/read, ui/message, ui/open-link,
// permission decisions, rate/size refusals, protocol violations). Both funnel
// through AppendMCPAppAudit so the bounds and the column shape cannot drift.
//
// The audit row is deliberately identifier-only. It carries tenant/session,
// server/app identity, the action and result, and a bounded detail. Message
// bodies, credentials, tokens and full URL query strings must never be passed
// here; the callers build the detail from a fixed vocabulary and the store
// strips control characters and cuts it at a rune boundary as the last line of
// defence.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MCPAppAuditMaxDetail bounds the detail summary of one audit row.
const MCPAppAuditMaxDetail = 512

// MCPAppAuditEntry is one durable app audit row.
type MCPAppAuditEntry struct {
	AuditID   int64      `json:"auditId"`
	TS        time.Time  `json:"ts"`
	TenantID  *uuid.UUID `json:"tenantId,omitempty"`
	SessionID *uuid.UUID `json:"sessionId,omitempty"`
	Server    string     `json:"server,omitempty"`
	AppID     string     `json:"appId,omitempty"`
	Action    string     `json:"action"`
	Result    string     `json:"result"`
	// Detail is a bounded summary of identifiers and counters. It must not
	// carry user content.
	Detail string `json:"detail,omitempty"`
}

// AppendMCPAppAudit writes one audit row and returns it with its cursor.
func (s *Store) AppendMCPAppAudit(ctx context.Context, entry MCPAppAuditEntry) (MCPAppAuditEntry, error) {
	if entry.Action == "" || entry.Result == "" {
		return MCPAppAuditEntry{}, errors.New("store: mcp app audit action and result are required")
	}
	if entry.TS.IsZero() {
		entry.TS = time.Now().UTC()
	}
	entry.Server = sanitizeAuditText(entry.Server, 128)
	entry.AppID = sanitizeAuditText(entry.AppID, 128)
	entry.Action = sanitizeAuditText(entry.Action, 64)
	entry.Result = sanitizeAuditText(entry.Result, 32)
	entry.Detail = sanitizeAuditText(entry.Detail, MCPAppAuditMaxDetail)
	row := s.pool.QueryRow(ctx,
		`INSERT INTO mcp_app_audit (ts, tenant_id, session_id, server, app_id, action, result, detail)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING audit_id`,
		entry.TS, entry.TenantID, entry.SessionID, entry.Server, entry.AppID, entry.Action, entry.Result, entry.Detail)
	if err := row.Scan(&entry.AuditID); err != nil {
		return MCPAppAuditEntry{}, fmt.Errorf("store: append mcp app audit: %w", err)
	}
	return entry, nil
}

// ListMCPAppAudit returns audit rows in chronological order (audit_id > after),
// scoped to a tenant and optionally narrowed to one app and one action. A nil
// tenant reads the deployment-wide trail; the product API always passes the
// caller's tenant. NULL-tenant rows (a process-scoped registry) are visible to
// every tenant, matching the plugin audit reader.
func (s *Store) ListMCPAppAudit(ctx context.Context, tenantID *uuid.UUID, appID, action string, after int64, limit int) ([]MCPAppAuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT audit_id, ts, tenant_id, session_id, server, app_id, action, result, detail
		 FROM mcp_app_audit
		 WHERE audit_id > $1
		   AND ($2::uuid IS NULL OR tenant_id IS NULL OR tenant_id = $2)
		   AND ($3 = '' OR app_id = $3)
		   AND ($4 = '' OR action = $4)
		 ORDER BY audit_id
		 LIMIT $5`,
		after, tenantID, sanitizeAuditText(appID, 128), sanitizeAuditText(action, 64), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list mcp app audit: %w", err)
	}
	defer rows.Close()
	out := make([]MCPAppAuditEntry, 0, limit)
	for rows.Next() {
		var entry MCPAppAuditEntry
		if err := rows.Scan(&entry.AuditID, &entry.TS, &entry.TenantID, &entry.SessionID,
			&entry.Server, &entry.AppID, &entry.Action, &entry.Result, &entry.Detail); err != nil {
			return nil, fmt.Errorf("store: scan mcp app audit: %w", err)
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// sanitizeAuditText removes control characters and cuts at a rune boundary.
// It is not a substitute for the callers' discipline (no bodies, no query
// strings): it only bounds what a bug could write.
func sanitizeAuditText(value string, max int) string {
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
