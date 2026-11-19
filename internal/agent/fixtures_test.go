package agent

import (
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/testkit"
)

// Install the deterministic development tools into runners built without a host
// assembly. This is a test-only registration: production leaves the fallback
// registry empty and uses the host's constant tool surface.
func init() {
	runnerToolFixture = func(reg *agenttool.ToolRegistry) {
		if err := reg.Register(testkit.ClockTool{}); err != nil {
			panic(err)
		}
		if err := reg.Register(testkit.EchoTool{}); err != nil {
			panic(err)
		}
	}
}
