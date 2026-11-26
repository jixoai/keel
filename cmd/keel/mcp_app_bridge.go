package main

// MCP Apps host adapters (change feat-mcp-apps-events, P3): the product API's
// outbound runtime over the live MCP manager, the durable audit writer the app
// registry and the bridge share, and the input seam that turns a confirmed
// ui/message into a normal user.message.
//
// The adapters are the only place cmd/keel connects the app contract to the
// process: internal/harnessapi owns the HTTP contract, internal/mcphost owns the
// catalogue, and neither learns about the manager's connection details.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/harnessapi"
	"github.com/jixoai/keel/internal/httpapi"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/mcp"
	"github.com/jixoai/keel/internal/mcphost"
	"github.com/jixoai/keel/internal/quota"
	"github.com/jixoai/keel/internal/servicehost"
	"github.com/jixoai/keel/internal/store"
)

// mcpAppAuditTimeout bounds one durable audit write from the registry adapter.
const mcpAppAuditTimeout = 2 * time.Second

// mcpAppRuntime is harnessapi.MCPAppRuntime over the live MCP manager. AppTool is
// the ownership gate: a tool must exist on the catalogue-derived server and
// carry "app" visibility; the manager's CallTool would otherwise accept any
// tool the model can see.
type mcpAppRuntime struct {
	manager *mcp.Manager
}

func (r mcpAppRuntime) AppTool(server, tool string) (harnessapi.MCPAppTool, bool) {
	entry, srv, found := r.manager.FindTool(server, tool)
	if !found || srv == nil || srv.Err != nil || !entry.VisibleToApp() {
		return harnessapi.MCPAppTool{}, false
	}
	destructive, _ := entry.DestructiveHint()
	return harnessapi.MCPAppTool{Name: entry.Name, Destructive: destructive}, true
}

func (r mcpAppRuntime) CallTool(ctx context.Context, server, tool string, arguments json.RawMessage) (mcp.CallToolResult, error) {
	return r.manager.CallTool(ctx, server, tool, arguments)
}

func (r mcpAppRuntime) ReadResource(ctx context.Context, server, uri string) (mcp.ReadResourceResult, error) {
	for _, srv := range r.manager.Servers() {
		if srv.Entry.Name != server {
			continue
		}
		if srv.Err != nil {
			return mcp.ReadResourceResult{}, fmt.Errorf("mcp: server %q: %w", server, srv.Err)
		}
		if srv.Client == nil {
			return mcp.ReadResourceResult{}, fmt.Errorf("mcp: server %q is not connected", server)
		}
		return srv.Client.ReadResource(ctx, uri)
	}
	return mcp.ReadResourceResult{}, fmt.Errorf("%w: %q", mcp.ErrServerNotFound, server)
}

// mcpAppRuntimeOrNil keeps a disabled MCP service nil at the interface value:
// a typed nil struct would make the product API believe the proxy is wired.
func mcpAppRuntimeOrNil(rt *servicehost.Runtime) harnessapi.MCPAppRuntime {
	manager := rt.MCPManager()
	if manager == nil {
		return nil
	}
	return mcpAppRuntime{manager: manager}
}

// mcpAppAuditor adapts the durable store to mcphost.Auditor: document fetches,
// validation failures and catalogue publishes reach the same mcp_app_audit
// table as the bridge actions. It is best-effort like every audit write.
type mcpAppAuditor struct {
	store *store.Store
	log   *slog.Logger
}

func (a mcpAppAuditor) RecordMCPApp(ctx context.Context, event mcphost.AuditEvent) {
	entry := store.MCPAppAuditEntry{
		TS:     time.Now().UTC(),
		Server: event.Server,
		AppID:  event.AppID,
		Action: event.Action,
		Result: event.Result,
		Detail: event.Detail,
	}
	if event.TenantID != uuid.Nil {
		tenantID := event.TenantID
		entry.TenantID = &tenantID
	}
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mcpAppAuditTimeout)
	defer cancel()
	if _, err := a.store.AppendMCPAppAudit(auditCtx, entry); err != nil {
		a.log.Error("mcp app registry audit write failed",
			"action", event.Action, "result", event.Result, "server", event.Server, "app", event.AppID, "err", err)
	}
}

// mcpAppInputCommit is harnessapi's ui/message write seam. A confirmed send goes
// through httpapi.CommitClientInput — the same validation, kernel commit and
// submissions bookkeeping as POST /v1/sessions/{id}/input — with delivery fixed
// to queue (a View never steers a running turn).
func mcpAppInputCommit(st *store.Store, edge kernel.Committer, edgeQuota *quota.Edge, maxUpload int64) func(ctx context.Context, tenantID, sessionID uuid.UUID, requestID, text string, attachments json.RawMessage) (harnessapi.MCPAppInput, error) {
	return func(ctx context.Context, tenantID, sessionID uuid.UUID, requestID, text string, attachments json.RawMessage) (harnessapi.MCPAppInput, error) {
		commit, err := httpapi.CommitClientInput(ctx, st, edge, httpapi.ClientInput{
			TenantID:    tenantID,
			SessionID:   sessionID,
			RequestID:   requestID,
			Text:        text,
			Delivery:    kernel.DeliveryQueue,
			Attachments: attachments,
		}, httpapi.ClientInputOptions{MaxUpload: maxUpload, Quota: edgeQuota})
		if err != nil {
			if errors.Is(err, httpapi.ErrInputArchivedSession) {
				return harnessapi.MCPAppInput{}, harnessapi.ErrSessionConflict
			}
			if errors.Is(err, httpapi.ErrInputExternalSession) {
				return harnessapi.MCPAppInput{}, harnessapi.ErrExternalSession
			}
			return harnessapi.MCPAppInput{}, err
		}
		seq := int64(0)
		if commit.Submission.Seq != nil {
			seq = *commit.Submission.Seq
		}
		return harnessapi.MCPAppInput{
			SubmissionID: commit.Submission.ID.String(),
			RequestID:    requestID,
			Status:       commit.Submission.Status,
			Seq:          seq,
			Created:      commit.Created,
		}, nil
	}
}
