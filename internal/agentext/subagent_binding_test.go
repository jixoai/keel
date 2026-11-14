package agentext_test

// The member execution binding (change keel-agent-team-v1, RUNTIME-PLAN §c.3):
// a sub-agent spawned as an Agent library entry carries that binding into its
// own input, so the child attempt resolves the member's prompt, tool grants and
// default model exactly like any other bound run.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/kernel"
)

func TestSubagentSpawnCarriesTheMemberBinding(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	parent := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "subagent-binding"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, parent); err != nil {
		t.Fatal(err)
	}
	klog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	kern := kernel.New(klog, kernel.Options{})
	runner := agentext.NewSubagentRunner(agentext.SubagentOptions{
		Gate:   st,
		Edge:   kern,
		Events: kern,
		Runs:   storeSessions{st},
		Logger: testLogger(),
	})

	record, err := runner.SpawnAsync(ctx, agentext.SpawnRequest{
		TenantID:        tenant,
		ParentSessionID: parent,
		Task:            "review the draft",
		Agent:           "draft reviewer",
		AgentID:         "agent_member_7",
	})
	if err != nil {
		t.Fatalf("SpawnAsync: %v", err)
	}
	child, err := uuid.Parse(record.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := st.LoadEvents(ctx, tenant, child, 0, 50)
	if err != nil {
		t.Fatalf("load child events: %v", err)
	}
	if len(events) != 1 || events[0].Kind != "user.message" {
		t.Fatalf("child events = %+v, want one user.message", events)
	}
	var payload struct {
		Text      string          `json:"text"`
		Selection json.RawMessage `json:"selection"`
		Subagent  struct {
			Agent string `json:"agent"`
		} `json:"subagent"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Text != "review the draft" || payload.Subagent.Agent != "draft reviewer" {
		t.Fatalf("child payload = %+v", payload)
	}
	var selection struct {
		AgentID string `json:"agentId"`
		TeamID  string `json:"teamId"`
	}
	if err := json.Unmarshal(payload.Selection, &selection); err != nil {
		t.Fatalf("child selection %s: %v", payload.Selection, err)
	}
	if selection.AgentID != "agent_member_7" || selection.TeamID != "" {
		t.Fatalf("child selection = %+v, want agent_member_7", selection)
	}

	// A spawn without a binding keeps the child input free of a selection fact
	// (the child then runs the deployment default).
	plain, err := runner.SpawnAsync(ctx, agentext.SpawnRequest{
		TenantID:        tenant,
		ParentSessionID: parent,
		Task:            "plain child",
	})
	if err != nil {
		t.Fatalf("SpawnAsync (plain): %v", err)
	}
	plainChild, err := uuid.Parse(plain.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	plainEvents, err := st.LoadEvents(ctx, tenant, plainChild, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var plainPayload struct {
		Selection json.RawMessage `json:"selection"`
	}
	if err := json.Unmarshal(plainEvents[0].Payload, &plainPayload); err != nil {
		t.Fatal(err)
	}
	if len(plainPayload.Selection) != 0 {
		t.Fatalf("a bindingless spawn invented a selection: %s", plainPayload.Selection)
	}
}
