package loadout

import (
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/agentloop"
	"github.com/jixoai/keel/internal/servicegraph"
)

// TestPinnedPiLoopManifestMatchesSuite guards the pin against suite drift: the
// literal in this package must be the manifest hash the exported suite
// actually runs.
func TestPinnedPiLoopManifestMatchesSuite(t *testing.T) {
	if got := agentloop.ManifestHash(); got != PinnedPiLoopManifestHash {
		t.Fatalf("agentloop.ManifestHash = %s, PinnedPiLoopManifestHash = %s", got, PinnedPiLoopManifestHash)
	}
}

// TestDefaultAcceptsPinnedLoopConformance: the default pi-loop, which declares
// the suite's own manifest, passes the gate.
func TestDefaultAcceptsPinnedLoopConformance(t *testing.T) {
	lo, err := Default(Config{Provide: provideAll(ServicePiLoop)})
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if lo.Len() != 1 {
		t.Fatalf("loadout services = %d, want 1", lo.Len())
	}
}

// TestDefaultRejectsTamperedLoopManifest: a loop service that claims a
// different conformance manifest is refused before the loadout exists.
func TestDefaultRejectsTamperedLoopManifest(t *testing.T) {
	tampered := append([]agentloop.Case(nil), agentloop.Manifest()...)
	tampered[0].Title += " (tampered)"
	_, err := Default(Config{
		Provide: provideAll(ServicePiLoop),
		LoopConformance: []LoopConformance{
			{Service: ServicePiLoop, Manifest: tampered},
		},
	})
	if err == nil {
		t.Fatal("Default accepted a loop with a tampered PI conformance manifest")
	}
	for _, want := range []string{ServicePiLoop, "PI conformance manifest", PinnedPiLoopManifestHash} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

// TestDefaultRejectsWrongLoopPin: an explicitly wrong pin is a rejection.
func TestDefaultRejectsWrongLoopPin(t *testing.T) {
	_, err := Default(Config{
		Provide:  provideAll(ServicePiLoop),
		LoopPins: map[string]string{ServicePiLoop: "not-the-pinned-hash"},
	})
	if err == nil || !strings.Contains(err.Error(), "does not match the pinned") {
		t.Fatalf("Default accepted a wrong conformance pin: %v", err)
	}
}

// TestDefaultRejectsDuplicateLoopDeclarations: two claims for one loop service
// are ambiguous, not mergeable.
func TestDefaultRejectsDuplicateLoopDeclarations(t *testing.T) {
	_, err := Default(Config{
		Provide: provideAll(ServicePiLoop),
		LoopConformance: []LoopConformance{
			{Service: ServicePiLoop, Manifest: agentloop.Manifest()},
			{Service: ServicePiLoop, Manifest: agentloop.Manifest()},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "more than one conformance declaration") {
		t.Fatalf("Default accepted duplicate conformance declarations: %v", err)
	}
}

// TestValidateLoopConformanceRejectsMissingManifest: a loop service with no
// declaration is rejected, never skipped (fail-closed).
func TestValidateLoopConformanceRejectsMissingManifest(t *testing.T) {
	services := []servicegraph.ServiceDescriptor{{
		Name:     "custom-loop",
		Version:  "v1",
		Provides: []string{CapLoop},
	}}
	err := ValidateLoopConformance(services, nil, nil)
	if err == nil {
		t.Fatal("ValidateLoopConformance accepted a loop service with no manifest")
	}
	if !strings.Contains(err.Error(), "custom-loop") || !strings.Contains(err.Error(), "no PI conformance manifest") {
		t.Fatalf("error = %q, want a missing-manifest rejection naming the service", err)
	}
}

// TestValidateLoopConformanceRejectsMissingPin: even a manifest that matches
// the suite cannot enter unpinned — a custom loop needs an explicit pin.
func TestValidateLoopConformanceRejectsMissingPin(t *testing.T) {
	services := []servicegraph.ServiceDescriptor{{
		Name:     "custom-loop",
		Version:  "v1",
		Provides: []string{CapLoop},
	}}
	declared := map[string][]agentloop.Case{"custom-loop": agentloop.Manifest()}
	err := ValidateLoopConformance(services, declared, nil)
	if err == nil {
		t.Fatal("ValidateLoopConformance accepted a loop service with no pin")
	}
	if !strings.Contains(err.Error(), "no pinned PI conformance manifest hash") {
		t.Fatalf("error = %q, want a missing-pin rejection", err)
	}
}

// TestValidateLoopConformanceAcceptsDeclaredLoop: the declared manifest is
// compared canonically (order independent) against the pin.
func TestValidateLoopConformanceAcceptsDeclaredLoop(t *testing.T) {
	services := []servicegraph.ServiceDescriptor{{
		Name:     "custom-loop",
		Version:  "v1",
		Provides: []string{CapLoop},
	}}
	manifest := agentloop.Manifest()
	shuffled := append([]agentloop.Case(nil), manifest...)
	for i, j := 0, len(shuffled)-1; i < j; i, j = i+1, j-1 {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	pins := map[string]string{"custom-loop": agentloop.ManifestHash()}
	if err := ValidateLoopConformance(services, map[string][]agentloop.Case{"custom-loop": shuffled}, pins); err != nil {
		t.Fatalf("ValidateLoopConformance rejected a canonically equal manifest: %v", err)
	}
}

// TestValidateLoopConformanceRejectsWrongPin: a manifest that does not match
// the pin is rejected with both hashes in the message.
func TestValidateLoopConformanceRejectsWrongPin(t *testing.T) {
	services := []servicegraph.ServiceDescriptor{{
		Name:     "custom-loop",
		Version:  "v1",
		Provides: []string{CapLoop},
	}}
	declared := map[string][]agentloop.Case{"custom-loop": agentloop.Manifest()}
	pins := map[string]string{"custom-loop": "0000deadbeef"}
	err := ValidateLoopConformance(services, declared, pins)
	if err == nil {
		t.Fatal("ValidateLoopConformance accepted a wrong pin")
	}
	for _, want := range []string{agentloop.ManifestHash(), "0000deadbeef", "does not match the pinned"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

// TestValidateLoopConformanceIgnoresNonLoopServices: the gate applies only to
// services that provide ctx.loop.
func TestValidateLoopConformanceIgnoresNonLoopServices(t *testing.T) {
	services := []servicegraph.ServiceDescriptor{
		{Name: ServiceLLM, Version: "v1", Provides: []string{CapLLM}},
		{Name: ServiceTools, Version: "v1", Provides: []string{CapTools}},
	}
	if err := ValidateLoopConformance(services, nil, nil); err != nil {
		t.Fatalf("ValidateLoopConformance rejected non-loop services: %v", err)
	}
}
