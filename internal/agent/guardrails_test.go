package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/supervisor"
)

type recordingCommitter struct {
	mu      sync.Mutex
	commits []kernel.CommitRequest
	seq     int64
	err     error
}

func (c *recordingCommitter) Commit(_ context.Context, req kernel.CommitRequest) (kernel.Receipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return kernel.Receipt{}, c.err
	}
	c.seq++
	c.commits = append(c.commits, req)
	return kernel.Receipt{Seq: c.seq, Kind: req.Kind}, nil
}

func (c *recordingCommitter) Lookup(context.Context, kernel.Scope, kernel.KeyRef) (kernel.Receipt, bool, error) {
	return kernel.Receipt{}, false, nil
}

func (c *recordingCommitter) ofKind(kind kernel.EventKind) []kernel.CommitRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []kernel.CommitRequest
	for _, commit := range c.commits {
		if commit.Kind == kind {
			out = append(out, commit)
		}
	}
	return out
}

type fakeAgentTool struct {
	name    string
	execute func(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error)
}

func (t fakeAgentTool) Name() string            { return t.name }
func (t fakeAgentTool) Description() string     { return "fake" }
func (t fakeAgentTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t fakeAgentTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}
func (t fakeAgentTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	if t.execute != nil {
		return t.execute(ctx, id, args, onUpdate)
	}
	return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("ok")}}, nil
}

func guardForTest(t *testing.T, policy supervisor.GuardrailPolicy, ledger kernel.Committer) (*runGuard, kernel.Lease) {
	t.Helper()
	if ledger == nil {
		ledger = &recordingCommitter{}
	}
	lease := kernel.Lease{
		Scope: kernel.Scope{TenantID: uuid.New(), SessionID: uuid.New()},
		Token: kernel.OwnerToken{Owner: "guard-test", Epoch: 3},
	}
	runner := &Runner{
		Committer:      ledger,
		Logger:         discardLog(),
		ConfigHash:     "sha256:guard",
		ConfigRevision: 5,
		Guardrails:     func() supervisor.GuardrailPolicy { return policy },
	}
	guard := newRunGuard(runner, lease)
	if guard == nil {
		t.Fatalf("guard is nil for policy %+v", policy)
	}
	return guard, lease
}

func decodeGuardPayload(t *testing.T, commit kernel.CommitRequest) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(commit.Payload, &payload); err != nil {
		t.Fatalf("decode guard payload: %v", err)
	}
	return payload
}

func TestGuardrailDuplicateCallBlocksAndAudits(t *testing.T) {
	ledger := &recordingCommitter{}
	policy := supervisor.GuardrailPolicy{DuplicateCallLimit: 2}
	guard, _ := guardForTest(t, policy, ledger)
	before := guard.composeBefore(nil)
	call := agentcore.AgentToolCall{ID: "call-1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)}
	for i := 0; i < 2; i++ {
		if decision := before(context.Background(), call); decision != nil {
			t.Fatalf("call %d was blocked too early: %+v", i+1, decision)
		}
	}
	decision := before(context.Background(), call)
	if decision == nil || !decision.Block {
		t.Fatalf("third identical call was not blocked: %+v", decision)
	}
	if decision.Content == nil || !strings.Contains(ContentText(*decision.Content), "identical arguments") {
		t.Fatalf("block message = %+v", decision.Content)
	}
	facts := ledger.ofKind(kernel.KindGuardrailTripped)
	if len(facts) != 1 || !facts[0].System {
		t.Fatalf("guardrail facts = %+v", facts)
	}
	payload := decodeGuardPayload(t, facts[0])
	if payload["trigger"] != GuardrailDuplicateCall || payload["outcome"] != "blocked" ||
		payload["count"] != float64(3) || payload["limit"] != float64(2) {
		t.Fatalf("payload = %+v", payload)
	}
	if payload["configRevision"] != float64(5) || payload["attempt"] != float64(3) {
		t.Fatalf("config identity = %+v", payload)
	}
}

func TestGuardrailDuplicateResetsOnDifferentCall(t *testing.T) {
	policy := supervisor.GuardrailPolicy{DuplicateCallLimit: 1}
	guard, _ := guardForTest(t, policy, nil)
	before := guard.composeBefore(nil)
	first := agentcore.AgentToolCall{ID: "a", Name: "read", Arguments: json.RawMessage(`{"path":"a"}`)}
	second := agentcore.AgentToolCall{ID: "b", Name: "read", Arguments: json.RawMessage(`{"path":"b"}`)}
	if decision := before(context.Background(), first); decision != nil {
		t.Fatalf("first call blocked: %+v", decision)
	}
	if decision := before(context.Background(), second); decision != nil {
		t.Fatalf("different call blocked: %+v", decision)
	}
	if decision := before(context.Background(), second); decision == nil || !decision.Block {
		t.Fatal("repeat of the second call was not blocked")
	}
}

func TestGuardrailDuplicateWhitespaceIsTheSameCall(t *testing.T) {
	policy := supervisor.GuardrailPolicy{DuplicateCallLimit: 1}
	guard, _ := guardForTest(t, policy, nil)
	before := guard.composeBefore(nil)
	compact := agentcore.AgentToolCall{ID: "a", Name: "write", Arguments: json.RawMessage(`{"path":"a","text":"x"}`)}
	pretty := agentcore.AgentToolCall{ID: "b", Name: "write", Arguments: json.RawMessage("{\n  \"path\": \"a\",\n  \"text\": \"x\"\n}")}
	if decision := before(context.Background(), compact); decision != nil {
		t.Fatal("first call blocked")
	}
	if decision := before(context.Background(), pretty); decision == nil || !decision.Block {
		t.Fatal("pretty-printed repeat was not recognized")
	}
}

func TestGuardrailAccessRefusalIsNotCountedAsDuplicate(t *testing.T) {
	policy := supervisor.GuardrailPolicy{DuplicateCallLimit: 1}
	guard, _ := guardForTest(t, policy, nil)
	denied := agentcore.ContentList{agentcore.NewTextContent("denied")}
	access := func(context.Context, agentcore.AgentToolCall) *agentcore.BeforeToolCallDecision {
		return &agentcore.BeforeToolCallDecision{Block: true, Content: &denied}
	}
	before := guard.composeBefore(access)
	call := agentcore.AgentToolCall{ID: "a", Name: "bash", Arguments: json.RawMessage(`{}`)}
	for i := 0; i < 3; i++ {
		if decision := before(context.Background(), call); decision == nil {
			t.Fatal("access refusal was not returned")
		}
	}
	// The duplicate counter never moved: a refusal is not a repeated call.
	if guard.duplicateCount != 0 {
		t.Fatalf("duplicate count = %d, want 0", guard.duplicateCount)
	}
}

func TestGuardrailCallTimeoutAbortsTheCallAndAudits(t *testing.T) {
	ledger := &recordingCommitter{}
	policy := supervisor.GuardrailPolicy{CallTimeout: supervisor.Duration(10 * time.Millisecond)}
	guard, _ := guardForTest(t, policy, ledger)
	slow := fakeAgentTool{name: "slow", execute: func(ctx context.Context, _ string, _ json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		<-ctx.Done()
		return agentcore.AgentToolResult{}, ctx.Err()
	}}
	wrapped := guardedTool{inner: slow, guard: guard, timeout: 10 * time.Millisecond}
	_, err := wrapped.Execute(context.Background(), "call-1", json.RawMessage(`{}`), nil)
	if err == nil || !strings.Contains(err.Error(), "guardrail timeout") {
		t.Fatalf("timeout error = %v", err)
	}
	facts := ledger.ofKind(kernel.KindGuardrailTripped)
	if len(facts) != 1 {
		t.Fatalf("guardrail facts = %d", len(facts))
	}
	payload := decodeGuardPayload(t, facts[0])
	if payload["trigger"] != GuardrailCallTimeout || payload["outcome"] != "tool_aborted" ||
		payload["tool"] != "slow" {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestGuardrailTimeoutExemptionKeepsRegistry(t *testing.T) {
	policy := supervisor.GuardrailPolicy{
		CallTimeout:       supervisor.Duration(time.Second),
		CallTimeoutExempt: []string{"bash"},
	}
	guard, _ := guardForTest(t, policy, nil)
	registry := agenttool.NewToolRegistry()
	if err := registry.Register(fakeAgentTool{name: "bash"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(fakeAgentTool{name: "read"}); err != nil {
		t.Fatal(err)
	}
	wrapped, err := guard.wrapRegistry(registry)
	if err != nil {
		t.Fatal(err)
	}
	bashTool, _ := wrapped.Get("bash")
	if _, ok := bashTool.(guardedTool); ok {
		t.Fatal("exempt tool was wrapped with a timeout")
	}
	readTool, _ := wrapped.Get("read")
	if _, ok := readTool.(guardedTool); !ok {
		t.Fatal("non-exempt tool was not wrapped")
	}
	before, _ := registry.List(), wrapped.List()
	if before[0].Name() != wrapped.List()[0].Name() {
		t.Fatal("tool order changed")
	}
}

func TestGuardrailConsecutiveFailuresAbortsRun(t *testing.T) {
	ledger := &recordingCommitter{}
	policy := supervisor.GuardrailPolicy{ConsecutiveFailureLimit: 2}
	guard, _ := guardForTest(t, policy, ledger)
	guard.recordToolResult(true, "bash", "call-1")
	if guard.err() != nil {
		t.Fatalf("run aborted after one failure: %v", guard.err())
	}
	guard.recordToolResult(true, "bash", "call-2")
	if !errors.Is(guard.err(), ErrGuardrailAborted) {
		t.Fatalf("run error = %v, want ErrGuardrailAborted", guard.err())
	}
	payload := decodeGuardPayload(t, ledger.ofKind(kernel.KindGuardrailTripped)[0])
	if payload["trigger"] != GuardrailConsecutiveFailure || payload["outcome"] != "run_aborted" ||
		payload["count"] != float64(2) {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestGuardrailSuccessResetsFailureStreak(t *testing.T) {
	policy := supervisor.GuardrailPolicy{ConsecutiveFailureLimit: 2}
	guard, _ := guardForTest(t, policy, nil)
	guard.recordToolResult(true, "bash", "a")
	guard.recordToolResult(false, "bash", "b")
	guard.recordToolResult(true, "bash", "c")
	if guard.err() != nil {
		t.Fatalf("streak was not reset: %v", guard.err())
	}
}

func TestGuardrailTokenAndCostBudgets(t *testing.T) {
	t.Run("tokens", func(t *testing.T) {
		ledger := &recordingCommitter{}
		policy := supervisor.GuardrailPolicy{MaxTokens: 100}
		guard, _ := guardForTest(t, policy, ledger)
		guard.checkUsage(60, 50)
		if !errors.Is(guard.err(), ErrGuardrailAborted) {
			t.Fatalf("run error = %v", guard.err())
		}
		payload := decodeGuardPayload(t, ledger.ofKind(kernel.KindGuardrailTripped)[0])
		if payload["trigger"] != GuardrailTokenBudget || payload["tokens"] != float64(110) {
			t.Fatalf("payload = %+v", payload)
		}
	})
	t.Run("cost", func(t *testing.T) {
		ledger := &recordingCommitter{}
		policy := supervisor.GuardrailPolicy{
			MaxCostUSD: 0.001, InputUSDPerMTok: 10, OutputUSDPerMTok: 10,
		}
		guard, _ := guardForTest(t, policy, ledger)
		guard.checkUsage(100, 100)
		if !errors.Is(guard.err(), ErrGuardrailAborted) {
			t.Fatalf("run error = %v", guard.err())
		}
		payload := decodeGuardPayload(t, ledger.ofKind(kernel.KindGuardrailTripped)[0])
		if payload["trigger"] != GuardrailCostBudget || payload["costUSD"].(float64) <= 0.001 {
			t.Fatalf("payload = %+v", payload)
		}
	})
}

func TestGuardrailMemoryPressureAbortsOnlyItsRun(t *testing.T) {
	ledger := &recordingCommitter{}
	policy := supervisor.GuardrailPolicy{MaxMemoryMB: 10}
	first, _ := guardForTest(t, policy, ledger)
	second, _ := guardForTest(t, policy, ledger)
	first.memoryInUse = func() uint64 { return 11 * 1024 * 1024 }
	second.memoryInUse = func() uint64 { return 1 * 1024 * 1024 }

	first.checkMemory()
	second.checkMemory()
	if !errors.Is(first.err(), ErrGuardrailAborted) {
		t.Fatalf("tripped run error = %v", first.err())
	}
	if second.err() != nil {
		t.Fatalf("unrelated run was aborted: %v", second.err())
	}
	payload := decodeGuardPayload(t, ledger.ofKind(kernel.KindGuardrailTripped)[0])
	if payload["trigger"] != GuardrailMemoryPressure || payload["outcome"] != "run_aborted" {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestGuardrailDisabledPolicyKeepsRunPathUnchanged(t *testing.T) {
	runner := &Runner{
		Committer:  &recordingCommitter{},
		Logger:     discardLog(),
		Guardrails: func() supervisor.GuardrailPolicy { return supervisor.GuardrailPolicy{} },
	}
	if guard := newRunGuard(runner, kernel.Lease{Scope: kernel.Scope{TenantID: uuid.New(), SessionID: uuid.New()}}); guard != nil {
		t.Fatalf("guard = %+v, want nil for a zero policy", guard)
	}
}

func TestGuardrailCommitFailureDoesNotPanic(t *testing.T) {
	ledger := &recordingCommitter{err: errors.New("database down")}
	policy := supervisor.GuardrailPolicy{DuplicateCallLimit: 1}
	guard, _ := guardForTest(t, policy, ledger)
	before := guard.composeBefore(nil)
	call := agentcore.AgentToolCall{ID: "a", Name: "bash", Arguments: json.RawMessage(`{}`)}
	_ = before(context.Background(), call)
	decision := before(context.Background(), call)
	if decision == nil || !decision.Block {
		t.Fatal("duplicate was not blocked when the audit write failed")
	}
}

func TestCanonicalArguments(t *testing.T) {
	if got := canonicalArguments(json.RawMessage("{\n \"a\": 1\n}")); got != `{"a":1}` {
		t.Fatalf("canonicalArguments = %q", got)
	}
	if got := canonicalArguments(json.RawMessage("not json")); got != "not json" {
		t.Fatalf("canonicalArguments(invalid) = %q", got)
	}
}
