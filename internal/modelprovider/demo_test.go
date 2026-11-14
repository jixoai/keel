package modelprovider

import (
	"testing"

	"github.com/jixoai/keel/internal/demo"
)

// TestDemoProviderVocabulary pins the registry view of the demo provider: one id
// serving six families, with the model→family mapping agreeing with the shared
// demo vocabulary.
func TestDemoProviderVocabulary(t *testing.T) {
	if DemoProviderID != demo.ProviderID || DemoProviderDisplayName != demo.ProviderName {
		t.Fatalf("demo identity = %q/%q, want %q/%q",
			DemoProviderID, DemoProviderDisplayName, demo.ProviderID, demo.ProviderName)
	}
	if !IsDemoProviderID(" demo ") || !IsDemoProviderID("DEMO") {
		t.Fatal("IsDemoProviderID must accept the demo id regardless of case and padding")
	}
	if IsDemoProviderID("openai") || IsDemoProviderID("") {
		t.Fatal("IsDemoProviderID must refuse any other id")
	}

	families := DemoProviderTypeIDs()
	if len(families) != len(demo.ModelIDs()) {
		t.Fatalf("demo families = %d, want one per model (%d)", len(families), len(demo.ModelIDs()))
	}
	for _, family := range families {
		if !IsBuiltinTypeID(family) {
			t.Fatalf("demo family %q must be a built-in type id", family)
		}
	}

	if got, want := DemoModelIDs(), demo.ModelIDs(); len(got) != len(want) {
		t.Fatalf("DemoModelIDs = %v, want %v", got, want)
	}
	for _, modelID := range DemoModelIDs() {
		family, ok := DemoModelFamily(modelID)
		if !ok {
			t.Fatalf("%s must map to a family", modelID)
		}
		if !IsBuiltinTypeID(family) {
			t.Fatalf("%s family %q must be a built-in type id", modelID, family)
		}
	}
	if _, ok := DemoModelFamily("gpt-5"); ok {
		t.Fatal("a vendor model must not map to a demo family")
	}
}

// TestDemoModelDescriptors pins the descriptor facts the settings view renders:
// one descriptor per model, a positive budget and the capability flags the family
// implies.
func TestDemoModelDescriptors(t *testing.T) {
	descriptors := DemoModelDescriptors()
	if len(descriptors) != len(demo.ModelIDs()) {
		t.Fatalf("descriptors = %d, want %d", len(descriptors), len(demo.ModelIDs()))
	}
	byID := make(map[string]ModelDescriptor, len(descriptors))
	for _, descriptor := range descriptors {
		byID[descriptor.ModelID] = descriptor
		if descriptor.ContextWindow <= 0 || descriptor.MaxOutput <= 0 {
			t.Fatalf("%s budgets = %d/%d, want positive values",
				descriptor.ModelID, descriptor.ContextWindow, descriptor.MaxOutput)
		}
		if !descriptor.Capabilities.Has(CapabilityStreaming) {
			t.Fatalf("%s must declare streaming", descriptor.ModelID)
		}
	}
	llm, ok := byID[demo.ModelLLM]
	if !ok {
		t.Fatalf("the demo LLM descriptor is missing: %+v", descriptors)
	}
	for _, capability := range []string{CapabilityToolCall, CapabilityReasoning, CapabilityJSONSchemaOutput} {
		if !llm.Capabilities.Has(capability) {
			t.Fatalf("the demo LLM must declare %q: %v", capability, llm.Capabilities)
		}
	}
	realtime, ok := byID[demo.ModelRealtime]
	if !ok {
		t.Fatalf("the demo realtime descriptor is missing: %+v", descriptors)
	}
	for _, capability := range []string{CapabilityVAD, CapabilityInterruption, CapabilityTranscriptDelta, CapabilityTextFallback} {
		if !realtime.Capabilities.Has(capability) {
			t.Fatalf("the demo realtime model must declare %q: %v", capability, realtime.Capabilities)
		}
	}
	// A media family must not claim the tool capability: nothing routes tools to
	// an image model.
	if image, ok := byID[demo.ModelImage]; ok && image.Capabilities.Has(CapabilityToolCall) {
		t.Fatalf("the demo image model must not declare tool-call: %v", image.Capabilities)
	}
}
