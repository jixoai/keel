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
		// The example plugin declares its trigger only when a Bot is configured
		// (every trigger must fire as a Bot); the embedded manifest describes
		// that configured shape.
		{ExamplePluginID, NewExamplePlugin(ExamplePluginOptions{BotID: "keel.example"})},
		{"keel.testkit", NewTestkitPlugin()},
		{CreationPluginID, NewCreationPlugin(CreationOptions{})},
		{TasksPluginID, NewTasksPlugin(TasksOptions{})},
		{ShellPluginID, NewShellPlugin(ShellOptions{})},
		{ApprovalPluginID, NewApprovalPlugin(ApprovalOptions{})},
		{NotifyPluginID, NewNotifyPlugin(NotifyOptions{})},
		{FilePluginID, NewFilePlugin(FilePluginOptions{})},
		{ModelPluginID, NewModelPlugin()},
		{BotPluginID, NewBotPlugin()},
		{TeamBotPluginID, NewTeamBotPlugin()},
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

// TestCapabilityPluginsDeclareTheirCLI is the declaration side of the plugin
// contract: the three management surfaces are plugins, and each one declares
// its full CLI capability tree. The expected paths below are the commands that
// really exist (`cmd/keel/model.go`, `cmd/keel/bot.go`, `cmd/keel/team_bot.go`);
// a manifest that loses, renames or invents a command fails here, and the
// command-side tests exercise each declared path against a stub server, so
// neither side can drift alone.
func TestCapabilityPluginsDeclareTheirCLI(t *testing.T) {
	want := map[string][]string{
		ModelPluginID: {
			"model providers",
			"model list",
			"model show",
			"model provider list",
			"model provider show",
			"model provider add",
			"model provider update",
			"model provider remove",
			"model provider test",
			"model catalog",
		},
		BotPluginID: {
			"bot list",
			"bot show",
			"bot create",
			"bot update",
			"bot archive",
			"bot start",
			"bot keys",
			"bot key-create",
			"bot key-revoke",
		},
		TeamBotPluginID: {
			"team-bot list",
			"team-bot show",
			"team-bot create",
			"team-bot update",
			"team-bot archive",
			"team-bot roster",
			"team-bot start",
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

	// The model plugin's provider write subset declares its risk classes the
	// same way: a removal is destructive and stays out of every agent face, an
	// edit is a retryable write, and the probe declares write because it sends a
	// real (possibly billable) upstream request even though it changes no
	// configuration.
	model, _ := builtin.Manifest(ModelPluginID)
	modelByKey := map[string]pluginpkg.CLICapability{}
	for _, capability := range model.Provides.CLI {
		modelByKey[capability.Key()] = capability
	}
	if modelByKey["model provider remove"].Danger != pluginpkg.DangerDestructive {
		t.Fatalf("model provider remove danger = %s, want destructive", modelByKey["model provider remove"].Danger)
	}
	if modelByKey["model provider remove"].CallableByCaller(pluginpkg.CallerAgent) ||
		modelByKey["model provider remove"].CallableByCaller(pluginpkg.CallerTeamCaptain) {
		t.Fatal("model provider remove must not be callable by an agent or a team captain")
	}
	for _, key := range []string{"model provider add", "model provider update"} {
		if modelByKey[key].Danger != pluginpkg.DangerWrite {
			t.Fatalf("%s danger = %s, want write", key, modelByKey[key].Danger)
		}
	}
	if !modelByKey["model provider update"].Idempotent || !modelByKey["model provider test"].Idempotent {
		t.Fatal("model provider update/test must be declared idempotent")
	}
	if modelByKey["model provider test"].Danger != pluginpkg.DangerWrite {
		t.Fatalf("model provider test danger = %s, want write (it sends a real upstream request)", modelByKey["model provider test"].Danger)
	}
	for _, key := range []string{"model provider list", "model provider show", "model catalog"} {
		if modelByKey[key].Danger != pluginpkg.DangerRead {
			t.Fatalf("%s danger = %s, want read", key, modelByKey[key].Danger)
		}
	}
	if !modelByKey["model provider add"].CallableByCaller(pluginpkg.CallerUI) ||
		!modelByKey["model provider test"].CallableByCaller(pluginpkg.CallerAgent) {
		t.Fatal("model provider add/test must stay callable by the declared faces")
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
