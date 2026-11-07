package agentloop_test

import (
	"testing"

	"github.com/jixoai/keel/internal/agentloop"
	"github.com/jixoai/keel/internal/agentloop/agentlooptest"
	"github.com/jixoai/keel/internal/loadout"
	"github.com/jixoai/keel/internal/provider"
)

// TestPiLoopPassesExportedConformanceSuite is the replacement gate's positive
// case: the default loop passes the exported PI conformance suite through the
// service interface (agentloop.Loop), not through internal/loop internals.
func TestPiLoopPassesExportedConformanceSuite(t *testing.T) {
	agentlooptest.Suite(t, func(t *testing.T, stream provider.StreamFn) agentloop.Loop {
		return agentloop.NewPiLoop(stream)
	})
}

// TestPiLoopManifestHashMatchesPin: the manifest the suite exports is exactly
// the one the default blueprint pins for pi-loop. Editing the suite without
// re-pinning fails here (and the loadout gate would refuse the loop).
func TestPiLoopManifestHashMatchesPin(t *testing.T) {
	if got := agentloop.ManifestHash(); got != loadout.PinnedPiLoopManifestHash {
		t.Fatalf("agentloop manifest hash = %s, loadout pin = %s\n"+
			"Update PinnedPiLoopManifestHash deliberately when the conformance contract changes.",
			got, loadout.PinnedPiLoopManifestHash)
	}
	if got, want := agentlooptest.ManifestHash(), agentloop.ManifestHash(); got != want {
		t.Fatalf("agentlooptest.ManifestHash = %s, agentloop.ManifestHash = %s", got, want)
	}
	if got := len(agentlooptest.Manifest()); got != len(agentloop.Manifest()) {
		t.Fatalf("agentlooptest manifest cases = %d, agentloop = %d", got, len(agentloop.Manifest()))
	}
}
