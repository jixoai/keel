package modelprovider

// The provider probe (internal/modelcfg.ProviderProbe) reports the same health
// vocabulary this package owns. The probe result is the wire-independent shared
// type, so the two constant sets are duplicated by necessity; this guard keeps
// them from drifting into two spellings of the same measurement.

import (
	"testing"

	"github.com/jixoai/keel/internal/modelcfg"
)

func TestProbeStatusVocabularyMatchesHealth(t *testing.T) {
	pairs := map[string]string{
		modelcfg.ProbeStatusReady:         HealthReady,
		modelcfg.ProbeStatusNeedsAuth:     HealthNeedsAuth,
		modelcfg.ProbeStatusUnreachable:   HealthUnreachable,
		modelcfg.ProbeStatusInvalidConfig: HealthInvalidConfig,
	}
	for probeStatus, health := range pairs {
		if probeStatus != health {
			t.Fatalf("probe status %q != health state %q", probeStatus, health)
		}
	}
}
