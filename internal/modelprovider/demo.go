package modelprovider

import (
	"strings"

	"github.com/jixoai/keel/internal/demo"
)

// The demo provider, as registry vocabulary.
//
// # Why it lives here and not only in internal/demo
//
// This package owns the provider *type* vocabulary that plugins and the settings
// UI speak (llm/realtime/image/video/decision/tts/stt). The demo provider serves
// all of them from one provider id, so the fact "this provider declares six
// families" has to be expressible in this vocabulary: a settings screen that
// lists provider types reads TypeSet, and a review deployment must describe its
// demo provider there like any other.
//
// # Why there is no driver here
//
// This package is the pure registry contract: it performs no IO and imports no
// runtime package (see the package documentation, enforced by
// internal/architecture). The executable demo driver -- the thing that emits
// assistant deltas for a run -- lives in internal/provider, where the transport
// and agentcore contracts already are. Keeping the split means the registry
// stays testable without a runtime and the driver stays where a Provider may
// legitimately produce events.
//
// # Switch
//
// Default off. The enabling environment variable and its parsing live in
// internal/demo (demo.Enabled); nothing in this file reads the environment.

const (
	// DemoProviderID is the demo provider's identity in the registry, re-exported
	// so callers holding only this package do not need the demo package for one
	// string.
	DemoProviderID = demo.ProviderID

	// DemoProviderDisplayName is the display name of the demo provider group.
	DemoProviderDisplayName = demo.ProviderName
)

// DemoProviderTypeIDs lists the provider families the demo provider serves, in
// registration order.
func DemoProviderTypeIDs() []string {
	return []string{TypeLLM, TypeImage, TypeDecision, TypeRealtime, TypeTTS, TypeSTT}
}

// DemoModelFamily maps a demo model id onto the provider-family constant of this
// package. ok is false for any other id, including a vendor model's.
//
// The mapping goes through demo.Family so the strings cannot drift from the
// catalog the resolver registers: this function is a translation into this
// package's constants, not a second source of truth.
func DemoModelFamily(modelID string) (string, bool) {
	family, ok := demo.Family(modelID)
	if !ok {
		return "", false
	}
	switch family {
	case demo.FamilyLLM:
		return TypeLLM, true
	case demo.FamilyImage:
		return TypeImage, true
	case demo.FamilyDecision:
		return TypeDecision, true
	case demo.FamilyRealtime:
		return TypeRealtime, true
	case demo.FamilyTTS:
		return TypeTTS, true
	case demo.FamilySTT:
		return TypeSTT, true
	default:
		return "", false
	}
}

// IsDemoProviderID reports whether an id names the demo provider.
func IsDemoProviderID(providerID string) bool {
	return strings.EqualFold(strings.TrimSpace(providerID), demo.ProviderID)
}

// DemoModelIDs returns the demo provider's model ids in registration order.
func DemoModelIDs() []string { return demo.ModelIDs() }

// DemoModelDescriptors returns one ModelDescriptor per demo model, each declaring
// the capability flags its family implies. A caller building a provider instance
// (the settings view of a review deployment) gets the same facts the executable
// catalog carries.
func DemoModelDescriptors() []ModelDescriptor {
	descriptors := make([]ModelDescriptor, 0, len(demo.ModelIDs()))
	for _, modelID := range demo.ModelIDs() {
		family, ok := DemoModelFamily(modelID)
		if !ok {
			continue
		}
		capabilities := Capabilities{CapabilityStreaming}
		switch family {
		case TypeLLM:
			capabilities = append(capabilities, CapabilityReasoning, CapabilityToolCall, CapabilityJSONSchemaOutput)
		case TypeRealtime:
			capabilities = append(capabilities,
				CapabilityVAD, CapabilityInterruption, CapabilityTranscriptDelta, CapabilityTextFallback)
		}
		descriptors = append(descriptors, ModelDescriptor{
			ModelID:       modelID,
			DisplayName:   modelID,
			ContextWindow: demoContextWindowTokens,
			MaxOutput:     demoMaxOutputTokens,
			Capabilities:  capabilities,
		})
	}
	return descriptors
}

// The demo models' declared budgets, mirrored from the executable catalog so a
// descriptor and a resolved model report the same numbers.
const (
	demoContextWindowTokens = 128000
	demoMaxOutputTokens     = 8192
)
