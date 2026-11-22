// Run-level guardrails (change keel-harness-v2, W12).
//
// A guardrail is a policy decision made inside one attempt: a repeated tool
// call, a call that runs too long, a streak of failing tools, a token/cost
// budget or process memory pressure. Every trip does two things and nothing
// implicit:
//
//  1. it commits a `guardrail.tripped` ledger fact (who/when/why/outcome);
//  2. it applies a deterministic outcome: the call is blocked, the call is
//     aborted, or this attempt is aborted. Other runs are never touched; the
//     guard owns only the context of the run that tripped it.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/supervisor"
)

// Guardrail trigger names. They are durable: they appear in
// guardrail.tripped payloads and in evidence/acceptance queries.
const (
	GuardrailDuplicateCall      = "duplicate_call"
	GuardrailCallTimeout        = "call_timeout"
	GuardrailConsecutiveFailure = "consecutive_failures"
	GuardrailTokenBudget        = "token_budget"
	GuardrailCostBudget         = "cost_budget"
	GuardrailMemoryPressure     = "memory_pressure"
)

// ErrGuardrailAborted is the attempt error a run-level guardrail settles with.
// It is a normal failed attempt (not a cancellation): the worker's retry policy
// and the supervisor's budget apply.
var ErrGuardrailAborted = fmt.Errorf("agent: the run was aborted by a guardrail")

type runGuard struct {
	policy supervisor.GuardrailPolicy
	commit kernel.Committer
	scope  kernel.Scope
	epoch  int64
	// configHash/configRevision identify the configuration this attempt
	// captured. A policy rotation changes later attempts only; this attempt's
	// trips keep the version it started with.
	configHash     string
	configRevision int64
	log            *slog.Logger
	now            func() time.Time
	memoryInUse    func() uint64

	mu                  sync.Mutex
	lastSignature       string
	duplicateCount      int
	consecutiveFailures int
	trips               int
	cancel              context.CancelFunc
	abortErr            error
}

// newRunGuard returns nil when every guardrail is disabled, so the run path is
// byte-identical to the pre-guardrail behavior.
func newRunGuard(r *Runner, lease kernel.Lease) *runGuard {
	if r == nil || r.Guardrails == nil {
		return nil
	}
	policy := r.Guardrails()
	if policy.DuplicateCallLimit <= 0 && policy.CallTimeout <= 0 &&
		policy.ConsecutiveFailureLimit <= 0 && policy.MaxTokens <= 0 &&
		policy.MaxCostUSD <= 0 && policy.MaxMemoryMB <= 0 {
		return nil
	}
	now := func() time.Time { return time.Now().UTC() }
	return &runGuard{
		policy:         policy,
		commit:         r.Committer,
		scope:          lease.Scope,
		epoch:          lease.Token.Epoch,
		configHash:     r.ConfigHash,
		configRevision: r.ConfigRevision,
		log:            r.Logger,
		now:            now,
		memoryInUse:    memoryInUse,
	}
}

// bindCancel installs the run-local cancel function. Only this run's context is
// cancelled; the process and other runs keep running.
func (g *runGuard) bindCancel(cancel context.CancelFunc) {
	g.mu.Lock()
	g.cancel = cancel
	g.mu.Unlock()
}

// err reports the guardrail abort, if any.
func (g *runGuard) err() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.abortErr
}

func (g *runGuard) abort(err error) {
	g.mu.Lock()
	if g.abortErr == nil {
		g.abortErr = err
	}
	cancel := g.cancel
	g.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// wrapRegistry applies the per-call timeout to the attempt's tools. Names,
// schemas, descriptions and execution modes are preserved, so the tool manifest
// hash is unchanged.
func (g *runGuard) wrapRegistry(registry *agenttool.ToolRegistry) (*agenttool.ToolRegistry, error) {
	if registry == nil {
		return nil, nil
	}
	timeout := time.Duration(g.policy.CallTimeout)
	if timeout <= 0 {
		return registry, nil
	}
	exempt := make(map[string]bool, len(g.policy.CallTimeoutExempt))
	for _, name := range g.policy.CallTimeoutExempt {
		exempt[name] = true
	}
	wrapped := agenttool.NewToolRegistry()
	for _, tool := range registry.List() {
		candidate := tool
		if !exempt[tool.Name()] {
			candidate = guardedTool{inner: tool, guard: g, timeout: timeout}
		}
		if err := wrapped.Register(candidate); err != nil {
			return nil, fmt.Errorf("agent: wrap tool %q: %w", tool.Name(), err)
		}
	}
	return wrapped, nil
}

// composeBefore chains the access guard (security) with the duplicate-call
// guard. A blocked access decision short-circuits; a duplicate is blocked only
// after authorization passed, so a refusal never counts as a repeat.
func (g *runGuard) composeBefore(access agentcore.BeforeToolCallFunc) agentcore.BeforeToolCallFunc {
	return func(ctx context.Context, call agentcore.AgentToolCall) *agentcore.BeforeToolCallDecision {
		if access != nil {
			if decision := access(ctx, call); decision != nil {
				return decision
			}
		}
		return g.beforeToolCall(ctx, call)
	}
}

func (g *runGuard) beforeToolCall(_ context.Context, call agentcore.AgentToolCall) *agentcore.BeforeToolCallDecision {
	g.mu.Lock()
	limit := g.policy.DuplicateCallLimit
	if limit <= 0 {
		g.mu.Unlock()
		return nil
	}
	signature := call.Name + "\x00" + canonicalArguments(call.Arguments)
	if signature == g.lastSignature {
		g.duplicateCount++
	} else {
		g.lastSignature = signature
		g.duplicateCount = 1
	}
	count := g.duplicateCount
	g.mu.Unlock()
	if count <= limit {
		return nil
	}
	g.trip(GuardrailDuplicateCall, "blocked", fmt.Sprintf(
		"tool %q was called with identical arguments %d times in a row (limit %d)", call.Name, count, limit),
		map[string]any{"tool": call.Name, "toolCallId": call.ID, "count": count, "limit": limit})
	message := fmt.Sprintf(
		"Tool %q was blocked: the same call with identical arguments already ran %d times in a row. "+
			"Stop repeating it; use the previous result or take a different action.", call.Name, count-1)
	content := agentcore.ContentList{agentcore.NewTextContent(message)}
	details := map[string]any{"code": "guardrail_duplicate_call", "tool": call.Name, "count": count, "limit": limit}
	var payload any = details
	return &agentcore.BeforeToolCallDecision{Block: true, Content: &content, Details: &payload}
}

// recordToolResult updates the consecutive-failure streak. A success resets it;
// reaching the limit aborts this attempt.
func (g *runGuard) recordToolResult(isError bool, tool, callID string) {
	limit := g.policy.ConsecutiveFailureLimit
	if limit <= 0 {
		return
	}
	g.mu.Lock()
	if isError {
		g.consecutiveFailures++
	} else {
		g.consecutiveFailures = 0
	}
	count := g.consecutiveFailures
	g.mu.Unlock()
	if count < limit {
		return
	}
	g.trip(GuardrailConsecutiveFailure, "run_aborted", fmt.Sprintf(
		"%d consecutive tool calls failed (limit %d)", count, limit),
		map[string]any{"tool": tool, "toolCallId": callID, "count": count, "limit": limit})
	g.abort(fmt.Errorf("%w: %d consecutive tool failures", ErrGuardrailAborted, count))
}

// checkUsage applies the token and cost budgets after an assistant message was
// committed. The answer is not retracted: the fact is durable work, and the
// attempt failure tells the supervisor to stop rather than repeat it.
func (g *runGuard) checkUsage(inputTokens, outputTokens int) {
	total := inputTokens + outputTokens
	if g.policy.MaxTokens > 0 && total > g.policy.MaxTokens {
		g.trip(GuardrailTokenBudget, "run_aborted", fmt.Sprintf(
			"the attempt used %d tokens (limit %d)", total, g.policy.MaxTokens),
			map[string]any{"tokens": total, "limit": g.policy.MaxTokens})
		g.abort(fmt.Errorf("%w: token budget exceeded (%d > %d)", ErrGuardrailAborted, total, g.policy.MaxTokens))
		return
	}
	budget := g.policy.MaxCostUSD
	if budget <= 0 {
		return
	}
	cost := float64(inputTokens)/1_000_000*g.policy.InputUSDPerMTok +
		float64(outputTokens)/1_000_000*g.policy.OutputUSDPerMTok
	if cost <= budget {
		return
	}
	g.trip(GuardrailCostBudget, "run_aborted", fmt.Sprintf(
		"the attempt is estimated at $%.4f (limit $%.4f)", cost, budget),
		map[string]any{"costUSD": cost, "limitUSD": budget,
			"inputTokens": inputTokens, "outputTokens": outputTokens})
	g.abort(fmt.Errorf("%w: cost budget exceeded ($%.4f > $%.4f)", ErrGuardrailAborted, cost, budget))
}

// checkMemory samples the process heap at a turn boundary. A trip aborts this
// attempt only: memory pressure that is really process-wide is the deployment
// layer's concern (cgroup OOM), not a licence to kill unrelated runs.
func (g *runGuard) checkMemory() {
	limitMB := g.policy.MaxMemoryMB
	if limitMB <= 0 {
		return
	}
	used := g.memoryInUse()
	limit := uint64(limitMB) * 1024 * 1024
	if used <= limit {
		return
	}
	g.trip(GuardrailMemoryPressure, "run_aborted", fmt.Sprintf(
		"process heap in use is %d MiB (limit %d MiB)", used/(1024*1024), limitMB),
		map[string]any{"heapBytes": used, "limitMB": limitMB})
	g.abort(fmt.Errorf("%w: memory pressure (%d MiB > %d MiB)", ErrGuardrailAborted, used/(1024*1024), limitMB))
}

// toolTimedOut records a per-call timeout. The call is aborted; the run
// continues so the model can react to the error result.
func (g *runGuard) toolTimedOut(tool, callID string, timeout time.Duration) {
	g.trip(GuardrailCallTimeout, "tool_aborted", fmt.Sprintf(
		"tool %q exceeded the %s call timeout", tool, timeout),
		map[string]any{"tool": tool, "toolCallId": callID, "timeout": timeout.String()})
}

// trip commits one guardrail fact. The commit uses a detached bounded context:
// a run-level abort cancels the attempt context before the fact is written, and
// the audit must not be lost to that cancellation.
func (g *runGuard) trip(trigger, outcome, detail string, fields map[string]any) {
	g.mu.Lock()
	g.trips++
	ordinal := g.trips
	g.mu.Unlock()

	payload := map[string]any{
		"actor":          "supervisor",
		"trigger":        trigger,
		"outcome":        outcome,
		"detail":         detail,
		"attempt":        g.epoch,
		"configHash":     g.configHash,
		"configRevision": g.configRevision,
		"at":             g.now().UTC().Format(time.RFC3339Nano),
	}
	for key, value := range fields {
		payload[key] = value
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		g.log.Warn("encode guardrail fact", "trigger", trigger, "err", err)
		return
	}
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()
	if _, err := g.commit.Commit(commitCtx, kernel.CommitRequest{
		Scope:        g.scope,
		Kind:         kernel.KindGuardrailTripped,
		System:       true,
		GuardrailKey: fmt.Sprintf("%s:%s:%d:%d", g.scope.SessionID, trigger, g.epoch, ordinal),
		Payload:      encoded,
	}); err != nil {
		g.log.Warn("commit guardrail fact", "trigger", trigger,
			"tenant_id", g.scope.TenantID, "session_id", g.scope.SessionID, "err", err)
	}
}

// recordRecovery commits the automatic-recovery fact when a worker claims a run
// whose previous lease expired (the crashed-process path). The ledger, not the
// process, is the recovery record: who adopted the run, when, why (the previous
// owner's lease expired), and which configuration the new attempt runs under.
//
// A commit failure is logged and does not stop the recovery: the run is already
// claimed and the successor must execute it.
func (r *Runner) recordRecovery(ctx context.Context, lease kernel.Lease) {
	if r == nil || r.Committer == nil || !lease.Reclaimed {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"actor":          "supervisor",
		"reason":         "lease_expired",
		"result":         "adopted",
		"counted":        true,
		"statusBefore":   "running",
		"owner":          lease.Token.Owner,
		"epoch":          lease.Token.Epoch,
		"configHash":     r.ConfigHash,
		"configRevision": r.ConfigRevision,
		"at":             time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		r.Logger.Warn("encode recovery fact", "session_id", lease.Scope.SessionID, "err", err)
		return
	}
	if _, err := r.Committer.Commit(ctx, kernel.CommitRequest{
		Scope:     lease.Scope,
		Kind:      kernel.KindResumeAttempt,
		System:    true,
		ResumeKey: fmt.Sprintf("supervisor:%s:adopt:%d", lease.Scope.SessionID, lease.Token.Epoch),
		Payload:   payload,
	}); err != nil {
		r.Logger.Warn("commit recovery fact", "session_id", lease.Scope.SessionID,
			"owner", lease.Token.Owner, "epoch", lease.Token.Epoch, "err", err)
	}
}

// canonicalArguments normalizes JSON whitespace so a model that pretty-prints
// the same call is recognized as a repeat. Key order is preserved: two
// different orderings of the same object are semantically equal but rare in
// provider output, and re-serializing would risk changing the call.
func canonicalArguments(raw json.RawMessage) string {
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, raw); err != nil {
		return string(raw)
	}
	return buffer.String()
}

func memoryInUse() uint64 {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapInuse
}

// guardedTool applies the per-call timeout to one tool.
type guardedTool struct {
	inner   agentcore.AgentTool
	guard   *runGuard
	timeout time.Duration
}

func (t guardedTool) Name() string            { return t.inner.Name() }
func (t guardedTool) Description() string     { return t.inner.Description() }
func (t guardedTool) Schema() json.RawMessage { return t.inner.Schema() }
func (t guardedTool) ExecutionMode() agentcore.ToolExecutionMode {
	return t.inner.ExecutionMode()
}

func (t guardedTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	callCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	result, err := t.inner.Execute(callCtx, id, args, onUpdate)
	if callCtx.Err() == context.DeadlineExceeded {
		t.guard.toolTimedOut(t.inner.Name(), id, t.timeout)
		return agentcore.AgentToolResult{}, fmt.Errorf(
			"tool %q exceeded the %s guardrail timeout", t.inner.Name(), t.timeout)
	}
	return result, err
}
