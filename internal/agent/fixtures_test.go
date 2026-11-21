package agent

import (
	"testing"

	"github.com/jixoai/keel/internal/testkit"
)

// registerTestkitTools installs the deterministic development tools into a
// runner's fallback registry. Production runners start empty (the model-visible
// surface is the host assembly); tests that drive the fake provider's echo
// round-trip register the tool explicitly, exactly like the extension E2E.
func registerTestkitTools(t *testing.T, runner *Runner) {
	t.Helper()
	// These tests drive the deterministic echo/clock tools without the identity
	// surface; the allow-all fake is the explicit opt-out the production
	// fail-closed default requires (A4.2b1).
	runner.ToolAccess = AllowAllToolAccess{}
	if err := runner.Tools.Register(testkit.ClockTool{}); err != nil {
		t.Fatalf("register testkit clock: %v", err)
	}
	if err := runner.Tools.Register(testkit.EchoTool{}); err != nil {
		t.Fatalf("register testkit echo: %v", err)
	}
}
