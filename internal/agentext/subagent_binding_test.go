package agentext_test

// The member execution binding (change keel-agent-team-v1, RUNTIME-PLAN §c.3):
// a sub-agent spawned as an Agent library entry carries that binding into its
// own input, so the child attempt resolves the member's prompt, tool grants and
// default model exactly like any other bound run.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/kernel"
)

// selectionValidator stands in for the input edge's submit-time gate: it records
// every selection the runner submits and answers the configured verdict. A nil
// err accepts.
type selectionValidator struct {
	seen []json.RawMessage
	err  error
}

func (v *selectionValidator) validate(raw json.RawMessage) error {
	v.seen = append(v.seen, append(json.RawMessage(nil), raw...))
	return v.err
}

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
	validator := &selectionValidator{}
	runner := agentext.NewSubagentRunner(agentext.SubagentOptions{
		Gate:              st,
		Edge:              kern,
		Events:            kern,
		Runs:              storeSessions{st},
		Logger:            testLogger(),
		ValidateSelection: validator.validate,
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

	// The binding the spawn stored is exactly the selection the submit-time gate
	// saw, and a bindingless spawn submits nothing to it.
	if len(validator.seen) != 1 {
		t.Fatalf("validator calls = %d, want 1 (only the bound spawn)", len(validator.seen))
	}
	var validated struct {
		AgentID string `json:"agentId"`
	}
	if err := json.Unmarshal(validator.seen[0], &validated); err != nil {
		t.Fatalf("validated selection %s: %v", validator.seen[0], err)
	}
	if validated.AgentID != "agent_member_7" {
		t.Fatalf("validated selection = %+v, want agentId agent_member_7", validated)
	}
}

// A selection the submit-time gate refuses must stop the spawn before any child
// session, user.message or registry record exists (agent-team: MUST refuse at
// submission, MUST NOT store history and fail every attempt later).
func TestSubagentSpawnRefusesAnInvalidSelectionBeforeCommit(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	parent := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "subagent-refusal"); err != nil {
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
	validator := &selectionValidator{err: errors.New(`agent "agent_ghost" is archived`)}
	runner := agentext.NewSubagentRunner(agentext.SubagentOptions{
		Gate:              st,
		Edge:              kern,
		Events:            kern,
		Runs:              storeSessions{st},
		Logger:            testLogger(),
		ValidateSelection: validator.validate,
	})
	const key = "refused-selection"
	_, err = runner.SpawnAsync(ctx, agentext.SpawnRequest{
		TenantID:        tenant,
		ParentSessionID: parent,
		Task:            "review the draft",
		Selection:       json.RawMessage(`{"agentId":"agent_ghost"}`),
		IdempotencyKey:  key,
	})
	if !errors.Is(err, agentext.ErrInvalidSpawn) {
		t.Fatalf("invalid selection spawn = %v, want ErrInvalidSpawn", err)
	}
	if len(validator.seen) != 1 {
		t.Fatalf("validator calls = %d, want 1", len(validator.seen))
	}
	childID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("keel.subagent.v1|"+
		tenant.String()+"|"+parent.String()+"|"+key))
	if _, exists, err := st.SessionOwner(ctx, tenant, childID); err != nil {
		t.Fatal(err)
	} else if exists {
		t.Fatal("an invalid selection created the child session")
	}
	events, err := st.LoadEvents(ctx, tenant, childID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("an invalid selection wrote child events: %+v", events)
	}
	if records := runner.List(tenant, &parent); len(records) != 0 {
		t.Fatalf("an invalid selection registered a sub-agent: %+v", records)
	}

	// The same request with a selection the gate accepts runs exactly as before.
	validator.err = nil
	record, err := runner.SpawnAsync(ctx, agentext.SpawnRequest{
		TenantID:        tenant,
		ParentSessionID: parent,
		Task:            "review the draft",
		Selection:       json.RawMessage(`{"agentId":"agent_ghost"}`),
		IdempotencyKey:  key,
	})
	if err != nil {
		t.Fatalf("accepted selection spawn: %v", err)
	}
	if record.SessionID != childID.String() {
		t.Fatalf("child session = %s, want %s", record.SessionID, childID)
	}
}
