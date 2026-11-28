package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/approval"
	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/toolsurface"
)

func newMCPApprovalTestGate(t *testing.T) (*mcpApprovalGate, *harnessext.DecisionBroker, *approval.Service, agentcore.ToolRunScope) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broker := harnessext.NewDecisionBroker(harnessext.DecisionBrokerOptions{
		Store:          harnessext.NewMemoryDecisionStore(),
		DefaultTimeout: time.Minute,
		Logger:         logger,
	})
	t.Cleanup(broker.Close)
	provenance := approval.NewService(approval.Options{Store: approval.NewMemoryStore(), Logger: logger})
	gate := &mcpApprovalGate{}
	gate.bind(broker, provenance, logger)
	scope := agentcore.ToolRunScope{TenantID: uuid.NewString(), SessionID: uuid.NewString()}
	return gate, broker, provenance, scope
}

// answerMCPApproval polls the decision journal for the open question and
// answers it. A missing question is reported through the test's deadline, never
// silently ignored.
func answerMCPApproval(t *testing.T, broker *harnessext.DecisionBroker, scope agentcore.ToolRunScope, optionID string) {
	t.Helper()
	tenantID, _ := uuid.Parse(scope.TenantID)
	sessionID, _ := uuid.Parse(scope.SessionID)
	ctx := context.Background()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		decisions, err := broker.List(ctx, tenantID, sessionID)
		if err == nil {
			for _, decision := range decisions {
				if decision.Status != harnessext.DecisionOpen {
					continue
				}
				decisionID, err := uuid.Parse(decision.DecisionID)
				if err != nil {
					continue
				}
				if _, _, err := broker.Answer(ctx, harnessext.AnswerDecisionRequest{
					TenantID: tenantID, SessionID: sessionID, DecisionID: decisionID,
					OptionID: optionID, Actor: "reviewer",
				}); err != nil {
					t.Errorf("answer approval: %v", err)
				}
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("no open MCP approval decision appeared")
}

// TestMCPApprovalAllowsOnlyAfterHumanAllow pins the production approval path:
// the gate opens a durable decision, waits, and executes only when the human
// selected Allow.
func TestMCPApprovalAllowsOnlyAfterHumanAllow(t *testing.T) {
	gate, broker, provenance, scope := newMCPApprovalTestGate(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = agentcore.WithToolRunScope(ctx, scope)
	ctx = agentcore.WithToolCallID(ctx, "call-allow-1")

	answered := make(chan struct{})
	go func() {
		defer close(answered)
		answerMCPApproval(t, broker, scope, mcpApprovalAllowOption)
	}()
	err := gate.Approve(ctx, toolsurface.MCPApprovalRequest{Server: "filesystem", Tool: "delete_file", Destructive: true})
	<-answered
	if err != nil {
		t.Fatalf("approved call = %v, want nil", err)
	}
	decisions, err := broker.List(context.Background(), mustUUID(t, scope.TenantID), mustUUID(t, scope.SessionID))
	if err != nil || len(decisions) != 1 {
		t.Fatalf("decisions = (%d, %v), want one", len(decisions), err)
	}
	if decisions[0].Status != harnessext.DecisionAnswered || decisions[0].Answer == nil ||
		decisions[0].Answer.OptionID != mcpApprovalAllowOption {
		t.Fatalf("decision = %+v, want an answered Allow", decisions[0])
	}
	if !strings.Contains(decisions[0].Prompt, "filesystem/delete_file") {
		t.Fatalf("prompt = %q, want the server/tool named", decisions[0].Prompt)
	}
	if _, found, err := provenance.Get(context.Background(),
		mustUUID(t, scope.TenantID), mustUUID(t, scope.SessionID), mustUUID(t, decisions[0].DecisionID)); err != nil || !found {
		t.Fatalf("provenance row = (found=%v, err=%v), want an open audit row", found, err)
	}
}

// TestMCPApprovalDeniesWithoutAllow pins the refusal half: any answer other
// than Allow (and any timeout) is an error, so the tool never executes.
func TestMCPApprovalDeniesWithoutAllow(t *testing.T) {
	gate, broker, _, scope := newMCPApprovalTestGate(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = agentcore.WithToolRunScope(ctx, scope)
	ctx = agentcore.WithToolCallID(ctx, "call-deny-1")

	answered := make(chan struct{})
	go func() {
		defer close(answered)
		answerMCPApproval(t, broker, scope, "deny")
	}()
	err := gate.Approve(ctx, toolsurface.MCPApprovalRequest{Server: "filesystem", Tool: "delete_file", Destructive: true})
	<-answered
	if err == nil {
		t.Fatal("denied call returned nil")
	}
}

// TestMCPApprovalFailsClosedWithoutSeam pins that an unbound gate refuses.
func TestMCPApprovalFailsClosedWithoutSeam(t *testing.T) {
	gate := &mcpApprovalGate{}
	ctx := agentcore.WithToolRunScope(context.Background(), agentcore.ToolRunScope{
		TenantID: uuid.NewString(), SessionID: uuid.NewString(),
	})
	if err := gate.Approve(ctx, toolsurface.MCPApprovalRequest{Server: "s", Tool: "delete", Destructive: true}); err == nil {
		t.Fatal("unbound gate approved a destructive call")
	}
}

// TestMCPApprovalRequestIDBindsAttemptCallAndArguments pins the replay guard:
// the decision id is bound to the attempt, the provider call and the exact
// argument digest. A retry inside one attempt reuses the question; a new
// attempt or a different payload asks again.
func TestMCPApprovalRequestIDBindsAttemptCallAndArguments(t *testing.T) {
	scope := agentcore.ToolRunScope{TenantID: uuid.NewString(), SessionID: uuid.NewString()}
	request := toolsurface.MCPApprovalRequest{
		Server: "filesystem", Tool: "delete_file", Arguments: json.RawMessage(`{"path":"/tmp/a"}`),
	}
	ctxA := agentcore.WithToolCallID(agentcore.WithMCPAttempt(context.Background(), "attempt-a"), "call-1")
	ctxB := agentcore.WithToolCallID(agentcore.WithMCPAttempt(context.Background(), "attempt-b"), "call-1")

	first, firstDigest, _, _ := mcpApprovalIdentity(ctxA, scope, request)
	again, againDigest, _, _ := mcpApprovalIdentity(ctxA, scope, request)
	otherAttempt, _, _, _ := mcpApprovalIdentity(ctxB, scope, request)
	changed := request
	changed.Arguments = json.RawMessage(`{"path":"/tmp/b"}`)
	otherArgs, changedDigest, _, _ := mcpApprovalIdentity(ctxA, scope, changed)

	if first != again || firstDigest != againDigest {
		t.Fatalf("same attempt/call/args = %q / %q, want one decision", first, again)
	}
	if first == otherAttempt {
		t.Fatalf("a new attempt reused the answered decision id %q", first)
	}
	if first == otherArgs || firstDigest == changedDigest {
		t.Fatalf("changed arguments reused the decision id/digest")
	}
	if empty, _, _, _ := mcpApprovalIdentity(context.Background(), scope, request); empty == "" {
		t.Fatal("a call without attempt/call ids produced an empty request id")
	}
}

// TestMCPApprovalDoesNotReplayAcrossAttempts is the end-to-end replay guard:
// an answer to attempt A does not stop attempt B's identical call from asking
// again. The second wait is left unanswered and must time out instead of
// returning an auto-approval.
func TestMCPApprovalDoesNotReplayAcrossAttempts(t *testing.T) {
	gate, broker, _, scope := newMCPApprovalTestGate(t)
	request := toolsurface.MCPApprovalRequest{
		Server: "filesystem", Tool: "delete_file", Destructive: true,
		Arguments: json.RawMessage(`{"path":"/tmp/a"}`),
	}
	ctxA, cancelA := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelA()
	ctxA = agentcore.WithToolRunScope(ctxA, scope)
	ctxA = agentcore.WithToolCallID(agentcore.WithMCPAttempt(ctxA, "attempt-a"), "call-1")
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		answerMCPApproval(t, broker, scope, mcpApprovalAllowOption)
	}()
	if err := gate.Approve(ctxA, request); err != nil {
		t.Fatalf("attempt A approval: %v", err)
	}
	<-answered

	ctxB, cancelB := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancelB()
	ctxB = agentcore.WithToolRunScope(ctxB, scope)
	ctxB = agentcore.WithToolCallID(agentcore.WithMCPAttempt(ctxB, "attempt-b"), "call-1")
	if err := gate.Approve(ctxB, request); err == nil {
		t.Fatal("attempt B reused attempt A's answer instead of asking again")
	}
	decisions, err := broker.List(context.Background(), mustUUID(t, scope.TenantID), mustUUID(t, scope.SessionID))
	if err != nil || len(decisions) != 2 {
		t.Fatalf("decisions = (%d, %v), want one per attempt", len(decisions), err)
	}
}

// TestMCPApprovalPromptAndProvenanceCarryBoundedRedactedArguments pins what a
// human and the audit row can see: a bounded argument summary with sensitive
// values redacted, the digest of the exact payload, and the attempt/call
// identity. The secret must never appear in either surface.
func TestMCPApprovalPromptAndProvenanceCarryBoundedRedactedArguments(t *testing.T) {
	gate, broker, provenance, scope := newMCPApprovalTestGate(t)
	arguments := json.RawMessage(`{"path":"/tmp/a","token":"super-secret-value","note":"` + strings.Repeat("x", 400) + `"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = agentcore.WithToolRunScope(ctx, scope)
	ctx = agentcore.WithToolCallID(agentcore.WithMCPAttempt(ctx, "attempt-visible"), "call-visible")
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		answerMCPApproval(t, broker, scope, mcpApprovalAllowOption)
	}()
	if err := gate.Approve(ctx, toolsurface.MCPApprovalRequest{
		Server: "filesystem", Tool: "delete_file", Destructive: true, Arguments: arguments,
	}); err != nil {
		t.Fatalf("approval: %v", err)
	}
	<-answered
	decisions, err := broker.List(context.Background(), mustUUID(t, scope.TenantID), mustUUID(t, scope.SessionID))
	if err != nil || len(decisions) != 1 {
		t.Fatalf("decisions = (%d, %v), want one", len(decisions), err)
	}
	prompt := decisions[0].Prompt
	if !strings.Contains(prompt, `"path":"/tmp/a"`) && !strings.Contains(prompt, `"/tmp/a"`) {
		t.Fatalf("prompt = %q, want the benign argument", prompt)
	}
	if !strings.Contains(prompt, "[redacted]") {
		t.Fatalf("prompt = %q, want the token redacted", prompt)
	}
	if strings.Contains(prompt, "super-secret-value") || strings.Contains(prompt, strings.Repeat("x", 200)) {
		t.Fatalf("prompt leaked a secret or the unbounded argument: %q", prompt)
	}
	if len([]rune(prompt)) > 800 {
		t.Fatalf("prompt is %d runes, want a bounded summary", len([]rune(prompt)))
	}
	record, found, err := provenance.Get(context.Background(),
		mustUUID(t, scope.TenantID), mustUUID(t, scope.SessionID), mustUUID(t, decisions[0].DecisionID))
	if err != nil || !found {
		t.Fatalf("provenance = (found=%v, err=%v)", found, err)
	}
	if record.Context["argumentsDigest"] != mcpArgumentsDigest(arguments) {
		t.Fatalf("argumentsDigest = %q, want the exact-payload digest", record.Context["argumentsDigest"])
	}
	if record.Context["argumentsRedacted"] != "true" {
		t.Fatalf("argumentsRedacted = %q, want true", record.Context["argumentsRedacted"])
	}
	if record.Context["attemptId"] != "attempt-visible" || record.Context["toolCallId"] != "call-visible" {
		t.Fatalf("provenance identity = %+v", record.Context)
	}
	auditArguments := record.Context["arguments"]
	if !strings.Contains(auditArguments, "/tmp/a") || !strings.Contains(auditArguments, "[redacted]") ||
		strings.Contains(auditArguments, "super-secret-value") {
		t.Fatalf("audit arguments = %q, want bounded/redacted payload", auditArguments)
	}
}

func mustUUID(t *testing.T, value string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
