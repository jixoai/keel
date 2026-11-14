package servicehost

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/demo"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/modelcfg"
	"github.com/jixoai/keel/internal/provider"
)

// The assembly point these tests pin is the driver factory in models.go:
//
//	buildModelsService → modelcfg.DemoCatalogFromEnv()   (catalog layer)
//	                   → modelsService.stream(active)    (driver layer)
//
// A review deployment is only useful if a selection of (demo, demo-llm) actually
// reaches the in-process driver through that path — a catalog entry alone would
// list in the picker and then fail on the first turn.
//
// The tests drive the real assembly (buildModelsService + Resolve + StreamCompletion)
// with the switch set and unset, so they cover the environment boundary and the
// routing branch together.

// demoModelDocument is a tenant document that names the demo LLM as its default,
// the way a review deployment is configured.
func demoModelDocument() json.RawMessage {
	return json.RawMessage(`{
	  "schemaVersion": 1,
	  "selection": {"providerId": "` + demo.ProviderID + `", "modelId": "` + demo.ModelLLM + `"}
	}`)
}

// TestDemoSelectionRoutesToTheInProcessDriver is the end-to-end assembly check: with
// the demo provider enabled, resolving (demo, demo-llm) yields a StreamFn that
// answers the fixed demo text with no endpoint and no credential.
func TestDemoSelectionRoutesToTheInProcessDriver(t *testing.T) {
	t.Setenv(demo.Env, "1")

	service, err := buildModelsService(
		host.TenantConfig{Models: demoModelDocument()},
		// No lookup and no flat key: any HTTP driver would fail the
		// missing-credential gate, so a green answer proves the in-process driver
		// was chosen.
		LLMOptions{LookupEnv: func(string) (string, bool) { return "", false }},
		nil,
	)
	if err != nil {
		t.Fatalf("buildModelsService: %v", err)
	}

	active, err := service.Resolve(context.Background(), kernel.Scope{},
		json.RawMessage(`{"providerId":"`+demo.ProviderID+`","modelId":"`+demo.ModelLLM+`"}`))
	if err != nil {
		t.Fatalf("Resolve(demo/%s): %v", demo.ModelLLM, err)
	}
	if active.ProviderID != demo.ProviderID || active.Model != demo.ModelLLM {
		t.Fatalf("identity = %s/%s, want %s/%s", active.ProviderID, active.Model, demo.ProviderID, demo.ModelLLM)
	}
	if active.APIKey != "" {
		t.Fatal("the demo provider must resolve no credential")
	}
	if active.Stream == nil {
		t.Fatal("Resolve must produce a stream function")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := active.Stream(ctx, active.Model, provider.LlmContext{}, provider.StreamConfig{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// The demo stream is unbuffered, so the producer blocks until the consumer
	// drains it: the events must be read, not skipped.
	var assembled string
	for event := range stream.Events() {
		switch typed := event.(type) {
		case provider.StreamTextEvent:
			assembled = agentcore.ContentToText(typed.Partial.Content)
		case provider.StreamDoneEvent:
			assembled = agentcore.ContentToText(typed.Message.Content)
		case provider.StreamErrorEvent:
			t.Fatalf("the demo stream failed: %v", typed.Err)
		}
	}
	if assembled != demo.ReplyText {
		t.Fatalf("assembled reply = %q, want the fixed demo text %q", assembled, demo.ReplyText)
	}

	message, err := stream.Result(ctx)
	if err != nil {
		t.Fatalf("stream result: %v", err)
	}
	if got := agentcore.ContentToText(message.Content); got != demo.ReplyText {
		t.Fatalf("terminal reply = %q, want the fixed demo text %q", got, demo.ReplyText)
	}
	if message.StopReason != agentcore.StopReasonEndTurn {
		t.Fatalf("stopReason = %q, want %q", message.StopReason, agentcore.StopReasonEndTurn)
	}
}

// TestDemoSelectionIsListedInThePickerProjection pins the /v1/models half of the
// same assembly: the enabled service offers demo-llm with providerType llm and
// selectable true, which is what the picker reads.
func TestDemoSelectionIsListedInThePickerProjection(t *testing.T) {
	t.Setenv(demo.Env, "1")

	service, err := buildModelsService(host.TenantConfig{}, LLMOptions{}, nil)
	if err != nil {
		t.Fatalf("buildModelsService: %v", err)
	}

	found := false
	for _, model := range service.ModelViews() {
		if model.ProviderID != demo.ProviderID || model.ModelID != demo.ModelLLM {
			continue
		}
		found = true
		if model.ProviderType != modelcfg.ProviderTypeLLM {
			t.Fatalf("providerType = %q, want %q", model.ProviderType, modelcfg.ProviderTypeLLM)
		}
		if !model.Selectable {
			t.Fatalf("demo-llm must be selectable, issues: %v", model.Issues)
		}
	}
	if !found {
		t.Fatal("the enabled service must list demo-llm in the picker projection")
	}
}

// TestDemoAbsentFromTheProjectionWithoutTheSwitch is the zero-impact half at the
// assembly boundary: a production process offers no demo model and cannot resolve
// one.
func TestDemoAbsentFromTheProjectionWithoutTheSwitch(t *testing.T) {
	t.Setenv(demo.Env, "")

	service, err := buildModelsService(host.TenantConfig{}, LLMOptions{}, nil)
	if err != nil {
		t.Fatalf("buildModelsService: %v", err)
	}

	for _, model := range service.ModelViews() {
		if demo.FamilyInModelIDs(model.ModelID) {
			t.Fatalf("a disabled demo provider must contribute no model, found %s/%s",
				model.ProviderID, model.ModelID)
		}
	}
	for _, view := range service.ProviderViews() {
		if demo.IsProvider(view.ProviderID) {
			t.Fatalf("a disabled demo provider must not appear in the settings projection: %+v", view)
		}
	}
	if _, err := service.Resolve(context.Background(), kernel.Scope{},
		json.RawMessage(`{"providerId":"`+demo.ProviderID+`","modelId":"`+demo.ModelLLM+`"}`)); err == nil {
		t.Fatal("resolving a demo model without the switch must fail closed")
	}
}

// TestDemoDecisionModelIsListedButNotSelectable pins the decision family's
// projection boundary at the assembly level: /v1/models carries demo-decision
// (the settings page reads that list, so the capability must be visible with its
// issue) but the model is not selectable, so the picker's
// `selectable !== false` filter keeps the user out of the generic "no dedicated
// panel" placeholder the selection would open.
func TestDemoDecisionModelIsListedButNotSelectable(t *testing.T) {
	t.Setenv(demo.Env, "1")

	service, err := buildModelsService(host.TenantConfig{Models: demoModelDocument()}, LLMOptions{}, nil)
	if err != nil {
		t.Fatalf("buildModelsService: %v", err)
	}

	var decision *modelcfg.ModelView
	for _, model := range service.ModelViews() {
		if model.ProviderID == demo.ProviderID && model.ModelID == demo.ModelDecision {
			copied := model
			decision = &copied
		}
	}
	if decision == nil {
		t.Fatal("demo-decision must stay in the /v1/models projection (the settings page lists it)")
	}
	if decision.Selectable {
		t.Fatalf("demo-decision must not be selectable, issues: %v", decision.Issues)
	}
	if !decision.Executable {
		t.Fatalf("demo-decision is a working capability endpoint, so it must stay executable: %v", decision.Issues)
	}
	hasProviderTypeIssue := false
	for _, issue := range decision.Issues {
		if issue.Field == "providerType" && issue.Message != "" {
			hasProviderTypeIssue = true
		}
	}
	if !hasProviderTypeIssue {
		t.Fatalf("demo-decision must explain why it cannot be chosen: %v", decision.Issues)
	}

	// The settings view (GET /v1/providers) keeps the model too, with the same
	// non-selectable flag: that is where the capability stays discoverable.
	settingsListed := false
	for _, view := range service.ProviderViews() {
		if view.ProviderID != demo.ProviderID {
			continue
		}
		for _, model := range view.Models {
			if model.ModelID == demo.ModelDecision {
				settingsListed = true
				if model.Selectable {
					t.Fatal("the settings view must not present demo-decision as selectable")
				}
			}
		}
	}
	if !settingsListed {
		t.Fatal("the settings view must keep demo-decision")
	}

	// The picker still offers the conversation families: the new guard must not
	// have shrunk the list to nothing.
	if found := func() bool {
		for _, model := range service.ModelViews() {
			if model.ProviderID == demo.ProviderID && model.ModelID == demo.ModelLLM && model.Selectable {
				return true
			}
		}
		return false
	}(); !found {
		t.Fatal("demo-llm must stay selectable: conversation families are unaffected")
	}
	if got, want := service.ModelsSelectable(), len(service.ModelViews())-1; got != want {
		t.Fatalf("ModelsSelectable = %d, want %d (the decision model is the only non-selectable demo row)", got, want)
	}
}

// TestDemoProviderBehaviouralNameIsOrdinary pins that the resolved provider reads
// like any other: the picker groups it by its name and the driver reports the demo
// provider id, with no separate "demo mode" surface.
func TestDemoProviderBehaviouralNameIsOrdinary(t *testing.T) {
	t.Setenv(demo.Env, "1")

	service, err := buildModelsService(host.TenantConfig{}, LLMOptions{}, nil)
	if err != nil {
		t.Fatalf("buildModelsService: %v", err)
	}
	var view *struct {
		name       string
		visibility string
	}
	for _, candidate := range service.ProviderViews() {
		if !demo.IsProvider(candidate.ProviderID) {
			continue
		}
		copied := struct {
			name       string
			visibility string
		}{name: candidate.Name, visibility: candidate.Visibility}
		view = &copied
	}
	if view == nil {
		t.Fatal("the demo provider must be visible in the settings projection")
	}
	if view.name != demo.ProviderName {
		t.Fatalf("provider name = %q, want %q", view.name, demo.ProviderName)
	}
	if view.visibility != "visible" {
		t.Fatalf("provider visibility = %q, want visible", view.visibility)
	}
}
