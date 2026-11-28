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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
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
	// Every approval question is bound to this exact call: scope, attempt,
	// provider toolCallId, and a digest of the arguments. A decision answered in
	// one attempt can never authorize the same tool name in a later attempt,
	// because the attempt id is part of the decision's request id.
	requestID, argumentsDigest, argumentsSummary, redacted := mcpApprovalIdentity(ctx, scope, request)
	kind := approval.KindPrefix + approval.ModeConfirm
	prompt := fmt.Sprintf("Allow the agent to run the destructive MCP tool %s/%s?\nArguments: %s",
		request.Server, request.Tool, argumentsSummary)
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
				"server":            request.Server,
				"tool":              request.Tool,
				"arguments":         argumentsSummary,
				"argumentsDigest":   argumentsDigest,
				"argumentsRedacted": fmt.Sprintf("%t", redacted),
				"attemptId":         agentcore.MCPAttemptFromContext(ctx),
				"toolCallId":        agentcore.ToolCallIDFromContext(ctx),
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

// mcpApprovalIdentity names one approval question and the bounded argument
// view a human sees. The request id is bound to the attempt, the provider
// toolCallId and a digest of the exact arguments, so an answered decision can
// never authorize a later attempt or a different payload.
func mcpApprovalIdentity(ctx context.Context, scope agentcore.ToolRunScope, request toolsurface.MCPApprovalRequest) (requestID, digest, summary string, redacted bool) {
	digest = mcpArgumentsDigest(request.Arguments)
	summary, redacted = mcpArgumentSummary(request.Arguments)
	attempt := strings.TrimSpace(agentcore.MCPAttemptFromContext(ctx))
	if attempt == "" {
		// A call outside an assembled attempt (a test or an alternate host)
		// still asks per call; it simply has no attempt identity to bind to.
		attempt = "attemptless"
	}
	callID := strings.TrimSpace(agentcore.ToolCallIDFromContext(ctx))
	if callID == "" {
		callID = uuid.NewString()
	}
	requestID = "mcp-approval:v2:" + scope.TenantID + ":" + scope.SessionID + ":" +
		attempt + ":" + callID + ":" + digest
	return requestID, digest, summary, redacted
}

// mcpApprovalSummaryLimit bounds the argument text in a decision prompt.
const mcpApprovalSummaryLimit = 480

func mcpArgumentsDigest(raw json.RawMessage) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// mcpArgumentSummary renders a bounded, redacted view of the call arguments.
// The second result reports that at least one value was redacted, so the audit
// row can say so without storing the secret.
func mcpArgumentSummary(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "{}", false
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "<unparseable arguments; digest only>", true
	}
	redacted := false
	value = redactMCPValue(value, 0, &redacted)
	encoded, err := json.Marshal(value)
	if err != nil {
		return "<unprintable arguments; digest only>", true
	}
	summary := string(encoded)
	if runes := []rune(summary); len(runes) > mcpApprovalSummaryLimit {
		summary = string(runes[:mcpApprovalSummaryLimit]) + "…(truncated)"
	}
	return summary, redacted
}

// mcpSensitiveArgumentKey is deliberately broad: a false positive hides a
// harmless string from the approval prompt, a false negative leaks a secret.
var mcpSensitiveArgumentKeys = []string{
	"token", "secret", "password", "passwd", "api_key", "apikey",
	"authorization", "cookie", "credential", "private_key", "access_key",
	"bearer", "session_key", "client_secret",
}

func redactMCPValue(value any, depth int, redacted *bool) any {
	if depth > 6 {
		*redacted = true
		return "[…]"
	}
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		// Deterministic order is load-bearing: the summary is part of the
		// decision prompt, and Open refuses a repeated requestId with a
		// different prompt. A random 32-key slice would make a same-attempt
		// retry conflict.
		sort.Strings(keys)
		count := 0
		for _, key := range keys {
			if count >= 32 {
				out["…"] = "[more fields]"
				*redacted = true
				break
			}
			count++
			if mcpSensitiveKey(key) {
				out[key] = "[redacted]"
				*redacted = true
				continue
			}
			out[key] = redactMCPValue(typed[key], depth+1, redacted)
		}
		return out
	case []any:
		limit := len(typed)
		if limit > 20 {
			limit = 20
			*redacted = true
		}
		out := make([]any, 0, limit+1)
		for _, item := range typed[:limit] {
			out = append(out, redactMCPValue(item, depth+1, redacted))
		}
		if len(typed) > limit {
			out = append(out, "[more items]")
		}
		return out
	case string:
		if runes := []rune(typed); len(runes) > 120 {
			*redacted = true
			return string(runes[:120]) + "…"
		}
		return typed
	default:
		return value
	}
}

func mcpSensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	for _, token := range mcpSensitiveArgumentKeys {
		if strings.Contains(normalized, token) {
			return true
		}
	}
	return false
}
