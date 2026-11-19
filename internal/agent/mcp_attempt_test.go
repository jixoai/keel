// Runner attempt-scope tests (P4.5/P4.4): every attempt stamps a fresh MCP
// catalogue key and the run's (tenant, session) scope, and the executor stamps
// the tool call id. Without this the aggregate `mcp` tool could not freeze its
// catalogue per attempt or correlate progress.

package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/store"
)

// probeTool records the transient facts the runner/executor stamp on a tool
// execution context.
type probeTool struct {
	mu       sync.Mutex
	attempts []string
	calls    []string
	scopes   []agentcore.ToolRunScope
}

func (p *probeTool) Name() string            { return "echo" }
func (p *probeTool) Description() string     { return "records its execution context" }
func (p *probeTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (p *probeTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}

func (p *probeTool) Execute(ctx context.Context, id string, _ json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	scope, _ := agentcore.ToolRunScopeFromContext(ctx)
	p.mu.Lock()
	p.attempts = append(p.attempts, agentcore.MCPAttemptFromContext(ctx))
	p.calls = append(p.calls, agentcore.ToolCallIDFromContext(ctx))
	p.scopes = append(p.scopes, scope)
	p.mu.Unlock()
	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent("probe ok")},
	}, nil
}

func (p *probeTool) snapshot() (attempts, calls []string, scopes []agentcore.ToolRunScope) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.attempts...), append([]string(nil), p.calls...), append([]agentcore.ToolRunScope(nil), p.scopes...)
}

func TestRunnerStampsAttemptScopeAndCallID(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	ctx := context.Background()
	probe := &probeTool{}
	registry := agenttool.NewToolRegistry()
	if err := registry.Register(probe); err != nil {
		t.Fatalf("register probe: %v", err)
	}
	r, k := newTestRunner(t, st)
	r.Assembly = staticAssembly{prompt: "probe", tools: registry}

	if _, err := st.AppendEvent(ctx, tenant, session, store.EventUserMessage, map[string]string{"text": "echo one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := execRun(t, k, r, tenant, session); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// New input after the settled run is a second attempt.
	requeue(t, st, tenant, session)
	if _, err := st.AppendEvent(ctx, tenant, session, store.EventUserMessage, map[string]string{"text": "echo two"}); err != nil {
		t.Fatal(err)
	}
	if _, err := execRun(t, k, r, tenant, session); err != nil {
		t.Fatalf("second run: %v", err)
	}

	attempts, calls, scopes := probe.snapshot()
	if len(attempts) != 2 {
		t.Fatalf("probe executions = %d, want 2 (%v)", len(attempts), attempts)
	}
	if attempts[0] == "" || attempts[1] == "" {
		t.Fatalf("attempt ids are empty: %v", attempts)
	}
	if attempts[0] == attempts[1] {
		t.Fatalf("both attempts share the catalogue key %q", attempts[0])
	}
	for index, scope := range scopes {
		if scope.TenantID != tenant.String() || scope.SessionID != session.String() {
			t.Fatalf("scope %d = %+v, want tenant/session", index, scope)
		}
		if calls[index] == "" {
			t.Fatalf("call %d has no toolCallId", index)
		}
	}
	if calls[0] == calls[1] {
		t.Fatalf("two attempts reused the call id %q", calls[0])
	}
}
