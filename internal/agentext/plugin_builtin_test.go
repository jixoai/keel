package agentext

import (
	"sort"
	"testing"

	"github.com/jixoai/keel/internal/pluginpkg"
	"github.com/jixoai/keel/internal/pluginpkg/builtin"
	"github.com/jixoai/keel/internal/store"
)

// TestBuiltinManifestsDescribeTheirPlugins is the contract between the embedded
// manifests and the compiled-in plugins: the manifest a client reads (tools,
// hooks, triggers) must be the surface the Go plugin actually registers. A
// mismatch is a documentation lie in the plugin list, so it fails the build.
func TestBuiltinManifestsDescribeTheirPlugins(t *testing.T) {
	manifests, err := builtin.Manifests()
	if err != nil {
		t.Fatal(err)
	}
	env := pluginpkg.Environment{KeelVersion: "0.1.0", KnownHookEvent: ValidHookEvent}
	pairs := []struct {
		id     string
		plugin *Plugin
	}{
		{ExamplePluginID, NewExamplePlugin(ExamplePluginOptions{})},
		{"keel.subagents", NewSubagentPlugin(nil)},
	}
	if len(manifests) != len(pairs) {
		t.Fatalf("embedded manifests = %d, registered builtins = %d", len(manifests), len(pairs))
	}
	for _, pair := range pairs {
		manifest, ok := builtin.Manifest(pair.id)
		if !ok {
			t.Fatalf("no embedded manifest for %s", pair.id)
		}
		if err := manifest.Validate(env); err != nil {
			t.Fatalf("manifest %s does not validate: %v", pair.id, err)
		}
		if err := manifest.CheckEngines("0.1.0"); err != nil {
			t.Fatalf("manifest %s fails the version gate: %v", pair.id, err)
		}
		if manifest.Kind != pluginpkg.KindInProcess {
			t.Fatalf("manifest %s kind = %s", pair.id, manifest.Kind)
		}
		if got, want := sortedStrings(manifest.Provides.Tools), sortedStrings(toolNames(pair.plugin.Tools)); !equalStrings(got, want) {
			t.Fatalf("manifest %s tools = %v, plugin tools = %v", pair.id, got, want)
		}
		var hooks []string
		for _, event := range hookNames(pair.plugin.Hooks) {
			hooks = append(hooks, string(event))
		}
		if got, want := sortedStrings(manifest.Provides.Hooks), sortedStrings(hooks); !equalStrings(got, want) {
			t.Fatalf("manifest %s hooks = %v, plugin hooks = %v", pair.id, got, want)
		}
		var triggers []string
		for _, trigger := range pair.plugin.Triggers {
			triggers = append(triggers, trigger.ID)
		}
		if got, want := sortedStrings(manifest.Provides.Triggers), sortedStrings(triggers); !equalStrings(got, want) {
			t.Fatalf("manifest %s triggers = %v, plugin triggers = %v", pair.id, got, want)
		}
		if manifest.ID != pair.plugin.ID {
			t.Fatalf("manifest id %s != plugin id %s", manifest.ID, pair.plugin.ID)
		}
		if manifest.Version != pair.plugin.Version {
			t.Fatalf("manifest %s version %s != plugin version %s", pair.id, manifest.Version, pair.plugin.Version)
		}
	}
}

// TestLifecycleVocabularyMatchesStore guards the one duplicated vocabulary: the
// lifecycle state strings are owned here and persisted by internal/store.
func TestLifecycleVocabularyMatchesStore(t *testing.T) {
	pairs := map[LifecycleState]string{
		LifecycleInstalled:   store.PluginStateInstalled,
		LifecycleEnabled:     store.PluginStateEnabled,
		LifecycleDisabled:    store.PluginStateDisabled,
		LifecycleFailed:      store.PluginStateFailed,
		LifecycleInvalid:     store.PluginStateInvalid,
		LifecycleUninstalled: store.PluginStateUninstalled,
	}
	for state, persisted := range pairs {
		if string(state) != persisted {
			t.Fatalf("lifecycle state %q != store constant %q", state, persisted)
		}
	}
	if len(LifecycleStates()) != len(pairs) {
		t.Fatalf("LifecycleStates() misses a state: %v", LifecycleStates())
	}
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
