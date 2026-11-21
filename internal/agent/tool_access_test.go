package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/kernel"
)

// countingTool records whether the executor reached Execute.
type countingTool struct {
	name  string
	calls int
}

func (t *countingTool) Name() string            { return t.name }
func (t *countingTool) Description() string     { return "counting tool" }
func (t *countingTool) Schema() json.RawMessage { return nil }
func (t *countingTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}
func (t *countingTool) Execute(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	t.calls++
	return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("ok")}}, nil
}

func TestToolAccessNilSeamFailsClosed(t *testing.T) {
	runner := &Runner{}
	guard := runner.toolAccessGuard(kernel.Scope{})
	decision := guard(context.Background(), agentcore.AgentToolCall{ID: "c1", Name: "bash", Arguments: json.RawMessage(`{"action":"run"}`)})
	if decision == nil || !decision.Block {
		t.Fatalf("nil seam did not block: %+v", decision)
	}
	details, ok := (*decision.Details).(map[string]any)
	if !ok {
		t.Fatalf("details = %#v, want a map", *decision.Details)
	}
	if details["code"] != ToolAccessUnavailable {
		t.Fatalf("code = %v, want %s", details["code"], ToolAccessUnavailable)
	}
}

func TestToolAccessAllowAllFakeIsExplicit(t *testing.T) {
	runner := &Runner{ToolAccess: AllowAllToolAccess{}}
	guard := runner.toolAccessGuard(kernel.Scope{})
	if decision := guard(context.Background(), agentcore.AgentToolCall{ID: "c1", Name: "bash"}); decision != nil {
		t.Fatalf("allow-all fake blocked: %+v", decision)
	}
}

func TestToolAccessDeniedIsStructuredAndDoesNotLeakResource(t *testing.T) {
	decision := toolAccessDecision(agentcore.AgentToolCall{ID: "c1", Name: "agent"}, &ToolAccessError{
		Code:   ToolAccessPermissionDenied,
		Key:    "bot.invoke",
		Reason: "missing_relation",
		Detail: "no relation satisfies viewer for bot secret-member",
	})
	if decision == nil || !decision.Block {
		t.Fatal("denial did not block")
	}
	text := agentcore.ContentToText(*decision.Content)
	if !strings.Contains(text, "bot.invoke") || !strings.Contains(text, "missing_relation") {
		t.Fatalf("denial text = %q, want key and reason", text)
	}
	if strings.Contains(text, "secret-member") {
		t.Fatalf("denial text leaked the resource id: %q", text)
	}
	details, ok := (*decision.Details).(map[string]any)
	if !ok {
		t.Fatalf("details = %#v, want a map", *decision.Details)
	}
	if details["code"] != ToolAccessPermissionDenied || details["requiredKey"] != "bot.invoke" || details["reason"] != "missing_relation" {
		t.Fatalf("details = %+v", details)
	}
	if _, leaked := details["resourceId"]; leaked {
		t.Fatalf("details leaked a resource id: %+v", details)
	}
}

func TestToolAccessNoPrincipalMessage(t *testing.T) {
	decision := toolAccessDecision(agentcore.AgentToolCall{ID: "c1", Name: "read"}, &ToolAccessError{
		Code:   ToolAccessNoPrincipal,
		Reason: "no_session_principal",
	})
	text := agentcore.ContentToText(*decision.Content)
	if !strings.Contains(text, "no resolvable account or bot identity") {
		t.Fatalf("message = %q", text)
	}
}

// TestToolAccessBlockRunsBeforeExecute proves the guard is enforced by the
// executor, not only by the helper: a denied call never reaches the tool and
// the model receives an IsError result.
func TestToolAccessBlockRunsBeforeExecute(t *testing.T) {
	tool := &countingTool{name: "bash"}
	registry := agenttool.NewToolRegistry()
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{ToolAccess: denyingToolAccess{}}
	results, _ := agenttool.ExecuteToolCalls(context.Background(), agenttool.BatchConfig{
		ToolExecutorConfig: agenttool.ToolExecutorConfig{
			Registry:       registry,
			BeforeToolCall: runner.toolAccessGuard(kernel.Scope{}),
		},
	}, []agentcore.AgentToolCall{{ID: "c1", Name: "bash", Arguments: json.RawMessage(`{"action":"run"}`)}}, nil)
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("results = %+v, want one error result", results)
	}
	if tool.calls != 0 {
		t.Fatalf("the tool executed %d times behind a denial", tool.calls)
	}
	if text := agentcore.ContentToText(results[0].Content); !strings.Contains(text, "shell.exec") {
		t.Fatalf("result text = %q, want the required key", text)
	}
}

type denyingToolAccess struct{}

func (denyingToolAccess) AuthorizeToolCall(context.Context, kernel.Scope, agentcore.AgentToolCall) error {
	return &ToolAccessError{Code: ToolAccessPermissionDenied, Key: "shell.exec", Reason: "missing_grant"}
}
