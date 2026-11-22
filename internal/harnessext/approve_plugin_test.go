package harnessext

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/approval"
)

var (
	approvalTenant  = uuid.MustParse("cccccccc-1111-1111-1111-111111111111")
	approvalSession = uuid.MustParse("dddddddd-2222-2222-2222-222222222222")
)

type approvalHarness struct {
	plugin  *Plugin
	broker  *DecisionBroker
	service *approval.Service
	ctx     context.Context
}

func newApprovalHarness(t *testing.T) *approvalHarness {
	t.Helper()
	broker := NewDecisionBroker(DecisionBrokerOptions{
		Store:  NewMemoryDecisionStore(),
		Logger: testLogger(),
	})
	t.Cleanup(broker.Close)
	service := approval.NewService(approval.Options{Logger: testLogger()})
	plugin := NewApprovalPlugin(ApprovalOptions{
		Decisions:      broker,
		Provenance:     service,
		DefaultTimeout: 5 * time.Second,
		Logger:         testLogger(),
	})
	ctx := WithRunScope(context.Background(), RunScope{
		TenantID: approvalTenant, SessionID: approvalSession, RunID: "run-approval-1",
	})
	return &approvalHarness{plugin: plugin, broker: broker, service: service, ctx: ctx}
}

func (h *approvalHarness) tool(t *testing.T, name string) *ToolSpec {
	t.Helper()
	for i := range h.plugin.Tools {
		if h.plugin.Tools[i].Name == name {
			return &h.plugin.Tools[i]
		}
	}
	t.Fatalf("no tool %s", name)
	return nil
}

func (h *approvalHarness) execute(t *testing.T, name, args string) agentcore.AgentToolResult {
	t.Helper()
	result, err := h.tool(t, name).Execute(h.ctx, "call-1", json.RawMessage(args), nil)
	if err != nil {
		t.Fatalf("execute %s: %v", name, err)
	}
	return result
}

// waitForDecision polls the broker's session list until the open question the
// plugin just asked appears.
func (h *approvalHarness) waitForDecision(t *testing.T) Decision {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		decisions, err := h.broker.List(context.Background(), approvalTenant, approvalSession)
		if err != nil {
			t.Fatalf("list decisions: %v", err)
		}
		if len(decisions) > 0 {
			return decisions[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the approve tool never opened a decision")
	return Decision{}
}

func (h *approvalHarness) answer(t *testing.T, decisionID string, optionID, text string) {
	t.Helper()
	_, _, err := h.broker.Answer(context.Background(), AnswerDecisionRequest{
		TenantID:   approvalTenant,
		SessionID:  approvalSession,
		DecisionID: uuid.MustParse(decisionID),
		OptionID:   optionID,
		Text:       text,
	})
	if err != nil {
		t.Fatalf("answer decision: %v", err)
	}
}

func document(t *testing.T, result agentcore.AgentToolResult) map[string]any {
	t.Helper()
	document, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content = %#v", result.StructuredContent)
	}
	return document
}

// executeWhileAnswering runs the tool in a goroutine, waits for its question
// and answers it, mirroring the real order (the tool blocks on a human).
func (h *approvalHarness) executeWhileAnswering(t *testing.T, name, args, optionID, text string) map[string]any {
	t.Helper()
	type outcome struct {
		result agentcore.AgentToolResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := h.tool(t, name).Execute(h.ctx, "call-1", json.RawMessage(args), nil)
		done <- outcome{result: result, err: err}
	}()
	decision := h.waitForDecision(t)
	h.answer(t, decision.DecisionID, optionID, text)
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("execute: %v", out.err)
		}
		return document(t, out.result)
	case <-time.After(3 * time.Second):
		t.Fatal("the approve tool did not return after the answer")
		return nil
	}
}

func TestApproveConfirmAnsweredYes(t *testing.T) {
	h := newApprovalHarness(t)
	doc := h.executeWhileAnswering(t, ApprovalToolConfirm, `{"prompt":"Deploy to production?"}`, "yes", "")
	if doc["status"] != "answered" || doc["mode"] != "confirm" {
		t.Fatalf("document = %+v", doc)
	}
	answer, ok := doc["answer"].(map[string]any)
	if !ok || answer["optionId"] != "yes" || answer["validated"] != true {
		t.Fatalf("answer = %+v", doc["answer"])
	}
	provenance, ok := doc["provenance"].(approval.Record)
	if !ok {
		t.Fatalf("provenance = %#v", doc["provenance"])
	}
	if provenance.Validated != true || provenance.Actor != approval.UnknownActor {
		t.Fatalf("provenance = %+v (the tool answered through the raw broker, so no actor was recorded)", provenance)
	}
}

func TestApproveConfirmAnsweredNoIsAValidatedRefusal(t *testing.T) {
	h := newApprovalHarness(t)
	doc := h.executeWhileAnswering(t, ApprovalToolConfirm, `{"prompt":"Delete the workspace?"}`, "no", "")
	if doc["status"] != "answered" || doc["validated"] != true {
		t.Fatalf("document = %+v", doc)
	}
	answer := doc["answer"].(map[string]any)
	if answer["optionId"] != "no" {
		t.Fatalf("answer = %+v", answer)
	}
}

func TestApprovePromptFreeTextIsValidated(t *testing.T) {
	h := newApprovalHarness(t)
	doc := h.executeWhileAnswering(t, ApprovalToolPrompt, `{"prompt":"What is the staging URL?"}`, "", "https://staging.example.test")
	if doc["status"] != "answered" || doc["validated"] != true || doc["escape"] != false {
		t.Fatalf("document = %+v", doc)
	}
	answer := doc["answer"].(map[string]any)
	if answer["text"] != "https://staging.example.test" {
		t.Fatalf("answer = %+v", answer)
	}
}

func TestApproveChoiceOptionIsValidated(t *testing.T) {
	h := newApprovalHarness(t)
	doc := h.executeWhileAnswering(t, ApprovalToolChoice,
		`{"prompt":"Which region?","options":[{"id":"eu","label":"Europe"},{"id":"us","label":"US"}]}`, "eu", "")
	if doc["status"] != "answered" || doc["validated"] != true {
		t.Fatalf("document = %+v", doc)
	}
}

func TestApproveChoiceOtherEscapeIsNotValidated(t *testing.T) {
	h := newApprovalHarness(t)
	doc := h.executeWhileAnswering(t, ApprovalToolChoice,
		`{"prompt":"Which region?","options":[{"id":"eu","label":"Europe"},{"id":"us","label":"US"}]}`, "", "latam")
	if doc["status"] != "answered" {
		t.Fatalf("document = %+v", doc)
	}
	if doc["validated"] != false || doc["escape"] != true {
		t.Fatalf("escape provenance = validated=%v escape=%v", doc["validated"], doc["escape"])
	}
	answer := doc["answer"].(map[string]any)
	if answer["text"] != "latam" || answer["validated"] != false {
		t.Fatalf("answer = %+v", answer)
	}
}

func TestApproveChoiceRejectsEmptyOptions(t *testing.T) {
	h := newApprovalHarness(t)
	result := h.execute(t, ApprovalToolChoice, `{"prompt":"Which?","options":[]}`)
	doc := document(t, result)
	if doc["status"] != "failed" || doc["code"] != "invalid_arguments" {
		t.Fatalf("document = %+v", doc)
	}
}

func TestApproveTimeoutIsPersistedAndSafeSideIsNo(t *testing.T) {
	h := newApprovalHarness(t)
	start := time.Now()
	result := h.execute(t, ApprovalToolConfirm, `{"prompt":"Ship it?","timeoutMs":1000}`)
	elapsed := time.Since(start)
	doc := document(t, result)
	if doc["status"] != "timed_out" {
		t.Fatalf("document = %+v", doc)
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("the tool returned after %s, before the 1s deadline", elapsed)
	}
	safeSide, ok := doc["safeSide"].(map[string]any)
	if !ok || safeSide["optionId"] != "no" {
		t.Fatalf("safeSide = %+v", doc["safeSide"])
	}
	if doc["answer"] != nil {
		t.Fatalf("a timed-out question must not carry an answer: %+v", doc["answer"])
	}
	provenance := doc["provenance"].(approval.Record)
	if provenance.Status != approval.StatusTimedOut {
		t.Fatalf("provenance = %+v", provenance)
	}
	// Re-reading the question returns the same persisted timeout; it does not
	// start a second timer.
	decisions, err := h.broker.List(context.Background(), approvalTenant, approvalSession)
	if err != nil || len(decisions) != 1 || decisions[0].Status != DecisionTimedOut {
		t.Fatalf("decisions = %+v err=%v", decisions, err)
	}
}

func TestApproveRunCancellationCancelsTheQuestion(t *testing.T) {
	h := newApprovalHarness(t)
	ctx, cancel := context.WithCancel(h.ctx)
	type outcome struct {
		result agentcore.AgentToolResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := h.tool(t, ApprovalToolConfirm).Execute(ctx, "call-1", json.RawMessage(`{"prompt":"Continue?"}`), nil)
		done <- outcome{result: result, err: err}
	}()
	decision := h.waitForDecision(t)
	cancel()
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("execute: %v", out.err)
		}
		doc := document(t, out.result)
		if doc["status"] != "cancelled" {
			t.Fatalf("document = %+v", doc)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the approve tool did not return after cancellation")
	}
	settled, err := h.broker.List(context.Background(), approvalTenant, approvalSession)
	if err != nil || len(settled) != 1 || settled[0].Status != DecisionCancelled {
		t.Fatalf("decision after cancel = %+v err=%v", settled, err)
	}
	if settled[0].DecisionID != decision.DecisionID {
		t.Fatalf("wrong decision cancelled: %s != %s", settled[0].DecisionID, decision.DecisionID)
	}
}

func TestApproveRequiresRunScope(t *testing.T) {
	h := newApprovalHarness(t)
	result, err := h.tool(t, ApprovalToolConfirm).Execute(context.Background(), "call-1",
		json.RawMessage(`{"prompt":"x"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	doc := document(t, result)
	if doc["status"] != "failed" || doc["code"] != "run_scope_missing" {
		t.Fatalf("document = %+v", doc)
	}
}

func TestApproveWithoutBrokerFailsClosed(t *testing.T) {
	plugin := NewApprovalPlugin(ApprovalOptions{})
	for i := range plugin.Tools {
		ctx := WithRunScope(context.Background(), RunScope{TenantID: approvalTenant, SessionID: approvalSession})
		result, err := plugin.Tools[i].Execute(ctx, "call-1", json.RawMessage(`{"prompt":"x"}`), nil)
		if err != nil {
			t.Fatal(err)
		}
		doc := document(t, result)
		if doc["code"] != "not_wired" {
			t.Fatalf("%s = %+v", plugin.Tools[i].Name, doc)
		}
	}
}

func TestApprovePluginDeclaresOnlyInternalTools(t *testing.T) {
	plugin := NewApprovalPlugin(ApprovalOptions{})
	if names := toolNames(plugin.Tools); len(names) != 0 {
		t.Fatalf("approve plugin publishes tools = %v, want none (the harness owns the name)", names)
	}
	for _, tool := range plugin.Tools {
		if !tool.Internal {
			t.Fatalf("%s is not marked internal", tool.Name)
		}
	}
}
