package agentext

import (
	"os"
	"path/filepath"
	"testing"
)

// A directory whose manifest never parses and whose name is not a legal id must
// not write a junk row: it is listed as invalid from disk on every reconcile.
func TestPluginManagerJunkDirectoryIsNotPersisted(t *testing.T) {
	root := t.TempDir()
	writePluginPackage(t, filepath.Join(root, "My Broken Plugin (copy)"), map[string]string{"plugin.yaml": "not: [a, manifest\n"})
	harness := newManagerHarness(t, root, nil)
	if err := harness.manager.Boot(t.Context()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, view := range harness.manager.Snapshot().Plugins {
		if view.State == LifecycleInvalid {
			found = true
		}
	}
	if !found {
		t.Fatal("the broken package must be listed as invalid")
	}
	rows, err := harness.store.ListPlugins(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("a junk directory must not write a row: %+v", rows)
	}
	if _, statErr := os.Stat(filepath.Join(root, "My Broken Plugin (copy)")); statErr != nil {
		t.Fatal("the package directory must be left alone")
	}
}
