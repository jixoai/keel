package pluginpkg

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOfficialPluginsPassTheReleaseGate runs the official-package gate over the
// real plugins/ tree: every plugin.yaml loads with RequireExtensibility, every
// declared document exists, and extensibility.json agrees with the manifest.
// A regression in either half fails here instead of at release time.
func TestOfficialPluginsPassTheReleaseGate(t *testing.T) {
	root := filepath.Join("..", "..", "plugins")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Skipf("plugins root is not available: %v", err)
	}
	found := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, PluginFileName)); err != nil {
			continue
		}
		found++
		pkg := LoadDir(dir, Environment{KeelVersion: "0.1.0", RequireExtensibility: true})
		if !pkg.Usable() {
			t.Errorf("%s: %v", dir, pkg.Problem)
			continue
		}
		if pkg.Manifest.Extensibility == nil {
			t.Errorf("%s: official package has no extensibility declaration", dir)
		}
	}
	if found == 0 {
		t.Fatal("no official plugin packages found; the gate would pass vacuously")
	}
}
