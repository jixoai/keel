package oar

import (
	"path/filepath"
	"testing"

	"github.com/jixoai/keel/internal/pluginpkg"
)

// TestPluginPackageManifestIsValid keeps the target `plugins/keel.oar` package
// loadable by the current manifest parser. Install is deferred (the loader only
// accepts tool_provider sidecars today), but a broken manifest would silently
// rot the package before W12.
func TestPluginPackageManifestIsValid(t *testing.T) {
	dir := filepath.Join("..", "..", "plugins", "keel.oar")
	pkg := pluginpkg.LoadDir(dir, pluginpkg.Environment{
		KeelVersion:    "0.1.0",
		KnownHookEvent: func(string) bool { return true },
	})
	if !pkg.Usable() {
		t.Fatalf("plugins/keel.oar is not usable: %v (fields %+v)", pkg.Problem, pkg.Problem.Fields)
	}
	if pkg.Manifest.ID != "keel.oar" {
		t.Fatalf("id = %q", pkg.Manifest.ID)
	}
	if pkg.Manifest.Kind != pluginpkg.KindSidecar {
		t.Fatalf("kind = %q", pkg.Manifest.Kind)
	}
	if len(pkg.Manifest.Entry.Command) == 0 || pkg.Manifest.Entry.Command[0] != "keel-oar" {
		t.Fatalf("entry.command = %v", pkg.Manifest.Entry.Command)
	}
}
