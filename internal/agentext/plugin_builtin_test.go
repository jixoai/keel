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
		{CreationPluginID, NewCreationPlugin(CreationOptions{})},
		{ModelPluginID, NewModelPlugin()},
		{AgentPluginID, NewAgentPlugin()},
		{TeamPluginID, NewTeamPlugin()},
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

// TestCapabilityPluginsDeclareTheirCLI is the declaration side of the
// agent-team plugin contract (change keel-agent-team-v1, plugin-capabilities):
// the three management surfaces are plugins, and each one declares its full
// CLI capability tree. The expected paths below are the commands that really
// exist (`cmd/keel/model.go`, `cmd/keel/agentteam.go`); a manifest that loses,
// renames or invents a command fails here, and the command-side tests
// (cmd/keel/model_test.go, cmd/keel/agentteam_test.go) exercise each declared
// path against a stub server, so neither side can drift alone.
func TestCapabilityPluginsDeclareTheirCLI(t *testing.T) {
	want := map[string][]string{
		ModelPluginID: {
			"model providers",
			"model list",
			"model show",
		},
		AgentPluginID: {
			"agent list",
			"agent show",
			"agent create",
			"agent update",
			"agent delete",
		},
		TeamPluginID: {
			"team list",
			"team show",
			"team create",
			"team update",
			"team delete",
			"team member add",
			"team member remove",
		},
	}
	for id, paths := range want {
		manifest, ok := builtin.Manifest(id)
		if !ok {
			t.Fatalf("no embedded manifest for %s", id)
		}
		got := make([]string, 0, len(manifest.Provides.CLI))
		for _, capability := range manifest.Provides.CLI {
			got = append(got, capability.Key())
			if len(capability.Permissions) == 0 {
				t.Fatalf("%s declares %q without a permission", id, capability.Key())
			}
			if len(capability.CallableBy) == 0 {
				t.Fatalf("%s declares %q without callable_by", id, capability.Key())
			}
			if len(capability.Examples) == 0 {
				t.Fatalf("%s declares %q without an example", id, capability.Key())
			}
		}
		if !equalStrings(sortedStrings(got), sortedStrings(paths)) {
			t.Fatalf("%s declares %v, want %v", id, got, paths)
		}
	}

	// The captain's team CLI is the one caller the server enforces (403 when the
	// team switch is off): the declaration must carry it exactly where the
	// enforcement lives.
	team, _ := builtin.Manifest(TeamPluginID)
	byKey := map[string]pluginpkg.CLICapability{}
	for _, capability := range team.Provides.CLI {
		byKey[capability.Key()] = capability
	}
	for _, key := range []string{"team member add", "team member remove"} {
		capability := byKey[key]
		if !capability.CallableByCaller(pluginpkg.CallerTeamCaptain) {
			t.Fatalf("%s must be callable by team_captain", key)
		}
	}
	if byKey["team member remove"].Danger != pluginpkg.DangerDestructive {
		t.Fatalf("team member remove danger = %s, want destructive", byKey["team member remove"].Danger)
	}
	if byKey["team create"].Danger != pluginpkg.DangerWrite || byKey["team delete"].Danger != pluginpkg.DangerDestructive {
		t.Fatalf("team create/delete danger classes are wrong")
	}
	for _, key := range []string{"team list", "team show"} {
		if !byKey[key].Idempotent {
			t.Fatalf("%s must be declared idempotent", key)
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
