// MCP destructive-tool approval on the model path.
//
// The toolsurface gate refuses a destructive MCP tool when no approval seam is
// wired (fail closed). This file supplies the production seam: a destructive
// call opens a durable decision through the same primitive the `approve` tool
// and the HTTP decision routes use, waits for a human answer, and refuses the
// call unless that answer selected "Allow".
//
// The decision kind carries the approval prefix, so the HTTP answer path
// records provenance (who answered, from which channel) exactly like a
// model-issued approval question. The request id is derived from the attempt
// scope and the provider's toolCallId: a retry of the same call re-addresses the
// same decision, while a second distinct call always asks again.

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/approval"
	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/toolsurface"
)

const (
	// mcpApprovalTimeout bounds one human decision. It matches the decision
	// primitive's default (5 minutes) and is far shorter than a run lease.
	mcpApprovalTimeout = 5 * time.Minute
	// mcpApprovalAllowOption is the only answer that lets the call execute.
	mcpApprovalAllowOption = "allow"
)

// errMCPApprovalUnavailable keeps the fail-closed contract when the product
// layer has not been assembled yet (a run that starts during boot).
var errMCPApprovalUnavailable = errors.New("mcp: human approval is not wired")

// mcpApprovalDeps are the product services the gate needs. They are installed
// after Boot, so the gate is safe to use before they exist (it refuses).
type mcpApprovalDeps struct {
	decisions  *harnessext.DecisionBroker
	provenance *approval.Service
	log        *slog.Logger
}

// mcpApprovalGate is the toolsurface approval seam. A mutex rather than a bare
// pointer keeps the post-Boot bind race-free against a run that is already
// executing.
type mcpApprovalGate struct {
	mu   sync.Mutex
	deps *mcpApprovalDeps
}

// bind installs the product services. It is idempotent; a later call replaces
// the previous deps (a rebooted product layer).
func (g *mcpApprovalGate) bind(decisions *harnessext.DecisionBroker, provenance *approval.Service, log *slog.Logger) {
	if g == nil || decisions == nil {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deps = &mcpApprovalDeps{decisions: decisions, provenance: provenance, log: log}
}

func (g *mcpApprovalGate) snapshot() *mcpApprovalDeps {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.deps
}

// Approve implements toolsurface.MCPApprovalFunc. It never executes the tool:
// nil means a human allowed it, any error is the typed refusal toolsurface
// turns into mcp_approval_denied.
func (g *mcpApprovalGate) Approve(ctx context.Context, request toolsurface.MCPApprovalRequest) error {
	deps := g.snapshot()
	if deps == nil {
		return errMCPApprovalUnavailable
	}
	scope, ok := agentcore.ToolRunScopeFromContext(ctx)
	if !ok {
		return errors.New("mcp: a destructive tool call needs the ambient run scope")
	}
	tenantID, err := uuid.Parse(scope.TenantID)
	if err != nil {
		return fmt.Errorf("mcp: invalid run tenant: %w", err)
	}
	sessionID, err := uuid.Parse(scope.SessionID)
	if err != nil {
		return fmt.Errorf("mcp: invalid run session: %w", err)
	}
	requestID := mcpApprovalRequestID(ctx, scope)
	kind := approval.KindPrefix + approval.ModeConfirm
	prompt := fmt.Sprintf("Allow the agent to run the destructive MCP tool %s/%s?", request.Server, request.Tool)
	decision, _, err := deps.decisions.Open(ctx, harnessext.OpenDecisionRequest{
		TenantID:  tenantID,
		SessionID: sessionID,
		RequestID: requestID,
		Kind:      kind,
		Prompt:    prompt,
		Options: []harnessext.DecisionOption{
			{ID: mcpApprovalAllowOption, Label: "Allow"},
			{ID: "deny", Label: "Deny"},
		},
		Timeout: mcpApprovalTimeout,
	})
	if err != nil {
		return fmt.Errorf("mcp: open approval decision: %w", err)
	}
	// Provenance is an audit projection; a write failure must not turn an
	// answered approval into a refusal of its own.
	if deps.provenance != nil {
		if _, _, err := deps.provenance.Open(ctx, approval.Record{
			TenantID:   tenantID.String(),
			SessionID:  sessionID.String(),
			DecisionID: decision.DecisionID,
			Kind:       kind,
			Mode:       approval.ModeConfirm,
			Actor:      "agent",
			Channel:    approval.ChannelTool,
			Context: map[string]string{
				"server": request.Server,
				"tool":   request.Tool,
			},
		}); err != nil {
			deps.log.Warn("mcp approval provenance (open) failed",
				"decision_id", decision.DecisionID, "err", err)
		}
	}
	decisionID, err := uuid.Parse(decision.DecisionID)
	if err != nil {
		return fmt.Errorf("mcp: invalid approval decision id: %w", err)
	}
	settled, err := deps.decisions.Wait(ctx, tenantID, sessionID, decisionID)
	if err != nil {
		if ctx.Err() != nil {
			// The run stopped waiting; cancel the question so the UI does not
			// keep an unanswerable prompt. The wait context is over, so the
			// cancel runs detached.
			cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			deps.decisions.CancelSession(cancelCtx, tenantID, sessionID, "run cancelled while waiting for MCP approval")
			cancel()
		}
		return fmt.Errorf("mcp: approval decision: %w", err)
	}
	if settled.Status != harnessext.DecisionAnswered || settled.Answer == nil {
		return fmt.Errorf("mcp: destructive tool %s/%s was not approved (%s)", request.Server, request.Tool, settled.Status)
	}
	if settled.Answer.OptionID != mcpApprovalAllowOption {
		return fmt.Errorf("mcp: destructive tool %s/%s was denied by the approver", request.Server, request.Tool)
	}
	return nil
}

// mcpApprovalRequestID names one approval question. The provider's toolCallId
// makes a retry of the same call re-address the same decision; without it a
// fresh id keeps two calls from sharing an answer.
func mcpApprovalRequestID(ctx context.Context, scope agentcore.ToolRunScope) string {
	if callID := agentcore.ToolCallIDFromContext(ctx); callID != "" {
		return "mcp-approval:" + scope.TenantID + ":" + scope.SessionID + ":" + callID
	}
	return "mcp-approval:" + uuid.NewString()
}
