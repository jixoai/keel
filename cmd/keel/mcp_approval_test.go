package main

import (
	"context"
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

// TestMCPApprovalRequestIDIsPerCall pins idempotency: the same provider call
// re-addresses one decision, a different call asks again.
func TestMCPApprovalRequestIDIsPerCall(t *testing.T) {
	scope := agentcore.ToolRunScope{TenantID: uuid.NewString(), SessionID: uuid.NewString()}
	first := mcpApprovalRequestID(agentcore.WithToolCallID(context.Background(), "call-1"), scope)
	again := mcpApprovalRequestID(agentcore.WithToolCallID(context.Background(), "call-1"), scope)
	other := mcpApprovalRequestID(agentcore.WithToolCallID(context.Background(), "call-2"), scope)
	if first != again {
		t.Fatalf("same call ids = %q / %q, want one decision", first, again)
	}
	if first == other {
		t.Fatalf("different calls share request id %q", first)
	}
	if mcpApprovalRequestID(context.Background(), scope) == "" {
		t.Fatal("a call without a provider id produced an empty request id")
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
