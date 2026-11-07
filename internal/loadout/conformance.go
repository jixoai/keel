package loadout

import (
	"fmt"

	"github.com/jixoai/keel/internal/agentloop"
	"github.com/jixoai/keel/internal/servicegraph"
)

// PinnedPiLoopManifestHash is the frozen PI conformance manifest hash the
// default blueprint requires from its loop service.
//
// It is a literal on purpose: the manifest lives in agentloop, and a test fails
// when the two drift. Changing the PI conformance contract (adding, removing,
// renaming or retitling a case) therefore requires a deliberate re-pin here and
// a version bump of the loop service — a replacement loop cannot silently ride
// a weakened suite into a loadout.
const PinnedPiLoopManifestHash = "da8d2c4b3a401c4e0fc57037547b6cfa694d74c96fd11ba83a7896f91efb8ae5"

// LoopConformance declares the PI conformance manifest a loop service's
// implementation was verified against. The manifest is the exported suite's
// canonical case list (agentlooptest.Manifest); the declaration is what makes
// "this loop passed the pinned suite" a static, checkable claim.
//
// A declaration is a claim, not a proof: it is the hosting process's statement
// that it ran the suite. The gate's job is to make the claim mandatory, pinned
// and hash-checked, so an undeclared or drifted loop cannot enter by omission.
type LoopConformance struct {
	// Service is the loop service name (ServicePiLoop for the default loop).
	Service string
	// Manifest is the PI conformance manifest the implementation passed.
	Manifest []agentloop.Case
}

// defaultLoopManifests is the blueprint's own conformance declaration: the
// default pi-loop is verified against the suite this repository ships.
var defaultLoopManifests = map[string][]agentloop.Case{
	ServicePiLoop: agentloop.Manifest(),
}

// defaultLoopPins is the blueprint's frozen pin per loop service.
var defaultLoopPins = map[string]string{
	ServicePiLoop: PinnedPiLoopManifestHash,
}

// ValidateLoopConformance is the fail-closed replacement gate: every service in
// the loadout that provides ctx.loop must present a PI conformance manifest
// whose canonical hash equals that service's pin. A loop with no manifest, no
// pin, or a mismatched manifest is rejected before the loadout exists.
//
// declared and pins override the blueprint defaults per service name; a missing
// entry falls back to the blueprint declaration/pin. Custom loop services have
// no blueprint entry, so the hosting process must supply both — omission is
// rejection, never a silent pass.
func ValidateLoopConformance(services []servicegraph.ServiceDescriptor, declared map[string][]agentloop.Case, pins map[string]string) error {
	for _, d := range services {
		if !providesCapability(d, CapLoop) {
			continue
		}
		cases := declared[d.Name]
		if len(cases) == 0 {
			cases = defaultLoopManifests[d.Name]
		}
		if len(cases) == 0 {
			return fmt.Errorf("loadout: loop service %q rejected: no PI conformance manifest declared (fail-closed); run the exported agentlooptest.Suite and declare its manifest", d.Name)
		}
		pin := pins[d.Name]
		if pin == "" {
			pin = defaultLoopPins[d.Name]
		}
		if pin == "" {
			return fmt.Errorf("loadout: loop service %q rejected: no pinned PI conformance manifest hash (fail-closed)", d.Name)
		}
		actual := agentloop.HashCases(cases)
		if actual != pin {
			return fmt.Errorf("loadout: loop service %q rejected: declared PI conformance manifest hash %s does not match the pinned %s; a loop that has not passed the pinned PI conformance suite cannot enter a loadout", d.Name, actual, pin)
		}
	}
	return nil
}

// validateConfigLoopConformance adapts Config's declarations to the gate and
// rejects duplicate declarations (two claims for one loop service are
// ambiguous, not mergeable).
func validateConfigLoopConformance(services []servicegraph.ServiceDescriptor, declarations []LoopConformance, pins map[string]string) error {
	declared := make(map[string][]agentloop.Case, len(declarations))
	for _, lc := range declarations {
		if lc.Service == "" {
			return fmt.Errorf("loadout: loop conformance declaration has no service name")
		}
		if _, dup := declared[lc.Service]; dup {
			return fmt.Errorf("loadout: loop service %q has more than one conformance declaration", lc.Service)
		}
		declared[lc.Service] = lc.Manifest
	}
	return ValidateLoopConformance(services, declared, pins)
}

func providesCapability(d servicegraph.ServiceDescriptor, capability string) bool {
	for _, provided := range d.Provides {
		if provided == capability {
			return true
		}
	}
	return false
}
