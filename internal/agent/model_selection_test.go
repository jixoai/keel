// Runner model-selection context tests (P5.2 review fix): the selection fact
// that resolved an attempt is stamped on the tool context, so an out-of-band
// model call (MCP sampling) resolves the same session model.

package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/store"
)

// selectionProbe records the model-selection fact of its execution context.
type selectionProbe struct {
	mu         sync.Mutex
	selections []string
}

func (p *selectionProbe) Name() string            { return "echo" }
func (p *selectionProbe) Description() string     { return "records the attempt's model selection" }
func (p *selectionProbe) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (p *selectionProbe) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}

func (p *selectionProbe) Execute(ctx context.Context, _ string, _ json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	selection := agentcore.ToolModelSelectionFromContext(ctx)
	p.mu.Lock()
	p.selections = append(p.selections, string(selection))
	p.mu.Unlock()
	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent("probe ok")},
	}, nil
}

func (p *selectionProbe) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.selections...)
}

func TestRunnerStampsTheAttemptSelectionOnToolContext(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	ctx := context.Background()

	probe := &selectionProbe{}
	registry := agenttool.NewToolRegistry()
	if err := registry.Register(probe); err != nil {
		t.Fatalf("register probe: %v", err)
	}
	runner, kernel := newTestRunner(t, st)
	resolver := &stubResolver{resolved: ActiveModel{
		ProviderID: "p", Model: "m", APIKey: "k", Stream: FakeStream(),
	}}
	runner.Models = resolver
	runner.Assembly = staticAssembly{prompt: "probe", tools: registry}

	if _, err := st.AppendEvent(ctx, tenant, session, store.EventUserMessage, map[string]any{
		"text":      "echo one",
		"selection": map[string]any{"providerId": "p", "modelId": "m"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := execRun(t, kernel, runner, tenant, session); err != nil {
		t.Fatalf("run: %v", err)
	}

	want := map[string]string{"providerId": "p", "modelId": "m"}
	selections := probe.snapshot()
	if len(selections) != 1 {
		t.Fatalf("probe executions = %d, want 1", len(selections))
	}
	var toolSelection map[string]string
	if err := json.Unmarshal([]byte(selections[0]), &toolSelection); err != nil {
		t.Fatalf("tool context selection is not JSON: %q (%v)", selections[0], err)
	}
	if !reflect.DeepEqual(toolSelection, want) {
		t.Fatalf("tool context selection = %v, want %v", toolSelection, want)
	}
	resolved := resolver.selections()
	if len(resolved) == 0 {
		t.Fatal("the resolver was never consulted")
	}
	var resolverSelection map[string]string
	if err := json.Unmarshal([]byte(resolved[len(resolved)-1]), &resolverSelection); err != nil {
		t.Fatalf("resolver selection is not JSON: %q (%v)", resolved[len(resolved)-1], err)
	}
	if !reflect.DeepEqual(resolverSelection, want) {
		t.Fatalf("resolver selections = %v, want the attempt fact %v", resolverSelection, want)
	}
}
