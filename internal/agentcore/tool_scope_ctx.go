// Tool-run scope context (change feat-mcp-apps-events, P4.5).
//
// The runner stamps the attempt's tenant/session on the context tools execute
// under; the tool executor stamps the toolCallId. A tool that can correlate
// out-of-band frames with its call (the aggregate `mcp` tool and MCP
// notifications/progress) reads both here. Nothing in the ledger depends on
// these values: they are transient transport facts.

package agentcore

import "context"

// ToolRunScope is the (tenant, session) pair of one attempt.
type ToolRunScope struct {
	TenantID  string
	SessionID string
}

// IsZero reports whether either id is missing.
func (s ToolRunScope) IsZero() bool {
	return s.TenantID == "" || s.SessionID == ""
}

type toolRunScopeKey struct{}

// WithToolRunScope returns a context carrying the attempt's scope.
func WithToolRunScope(ctx context.Context, scope ToolRunScope) context.Context {
	if scope.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, toolRunScopeKey{}, scope)
}

// ToolRunScopeFromContext returns the attempt scope, if one was stamped.
func ToolRunScopeFromContext(ctx context.Context) (ToolRunScope, bool) {
	scope, ok := ctx.Value(toolRunScopeKey{}).(ToolRunScope)
	return scope, ok && !scope.IsZero()
}

type toolCallIDKey struct{}

// WithToolCallID returns a context naming the tool call being executed. The id
// is the provider's toolCallId: it is the correlation key the progress token
// echoes and the UI keys a tool row on.
func WithToolCallID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, toolCallIDKey{}, id)
}

// ToolCallIDFromContext returns the executing call's id, empty when absent.
func ToolCallIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(toolCallIDKey{}).(string)
	return id
}

type mcpAttemptKey struct{}

// WithMCPAttempt returns a context naming this attempt. The MCP catalogue
// snapshot is keyed on it: the first MCP action of an attempt freezes the
// catalogue, every later action of the same attempt sees that frozen value, and
// the next attempt (a new key) sees the fresh one. This is the run-boundary
// rule of spec mcp-events「热更的 attempt 边界与失败保面」.
func WithMCPAttempt(ctx context.Context, attemptID string) context.Context {
	if attemptID == "" {
		return ctx
	}
	return context.WithValue(ctx, mcpAttemptKey{}, attemptID)
}

// MCPAttemptFromContext returns the attempt key, empty when absent.
func MCPAttemptFromContext(ctx context.Context) string {
	id, _ := ctx.Value(mcpAttemptKey{}).(string)
	return id
}
