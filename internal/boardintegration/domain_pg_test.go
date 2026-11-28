package boardintegration

import (
	"testing"

	"github.com/jixoai/keel/internal/pluginstore"
	"github.com/jixoai/keel/internal/pluginstore/conformance"
)

// TestBoardDomainConformance proves the board plugin's independent domain
// adapter participates in the shared lifecycle contract (idempotent migrate,
// ready round trip) without teaching the plugin kernel any board SQL.
func TestBoardDomainConformance(t *testing.T) {
	env := newTestEnv(t, nil)
	conformance.RunDomain(t, func(t *testing.T) pluginstore.DomainStore {
		return env.surf
	}, "board")
}
