package agentteam

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/pluginpkg"
)

// requireCommandPaths fails unless capabilities declares every path in want.
func requireCommandPaths(t *testing.T, capabilities []pluginpkg.CLICapability, want [][]string) {
	t.Helper()
	declared := pathSet(capabilities)
	for _, path := range want {
		key := strings.Join(path, " ")
		if !declared[key] {
			t.Errorf("capability tree is missing %q", "keel "+key)
		}
	}
}

// pathSet indexes capabilities by their canonical key.
func pathSet(capabilities []pluginpkg.CLICapability) map[string]bool {
	set := make(map[string]bool, len(capabilities))
	for _, capability := range capabilities {
		set[capability.Key()] = true
	}
	return set
}

// pathKeys returns the canonical keys in declaration order, for failure output.
func pathKeys(capabilities []pluginpkg.CLICapability) []string {
	keys := make([]string, len(capabilities))
	for i, capability := range capabilities {
		keys[i] = capability.Key()
	}
	return keys
}

// findCapability looks a path up in a declared tree.
func findCapability(capabilities []pluginpkg.CLICapability, path ...string) (pluginpkg.CLICapability, bool) {
	for _, capability := range capabilities {
		if slices.Equal(capability.Path, path) {
			return capability, true
		}
	}
	return pluginpkg.CLICapability{}, false
}

// withoutCapability returns a copy of capabilities with path removed.
func withoutCapability(t *testing.T, capabilities []pluginpkg.CLICapability, path []string) []pluginpkg.CLICapability {
	t.Helper()
	kept := make([]pluginpkg.CLICapability, 0, len(capabilities))
	removed := false
	for _, capability := range capabilities {
		if slices.Equal(capability.Path, path) {
			removed = true
			continue
		}
		kept = append(kept, capability)
	}
	if !removed {
		t.Fatalf("test setup: %q is not declared", "keel "+strings.Join(path, " "))
	}
	return kept
}

// containsPath reports whether paths contains path.
func containsPath(paths [][]string, path []string) bool {
	for _, candidate := range paths {
		if slices.Equal(candidate, path) {
			return true
		}
	}
	return false
}

func TestModelPluginTreeCoversSpecCommands(t *testing.T) {
	spec := ModelPluginCLISpec()
	want := [][]string{
		{"model", "provider", "list"},
		{"model", "provider", "show"},
		{"model", "provider", "add"},
		{"model", "provider", "update"},
		{"model", "provider", "remove"},
		{"model", "provider", "test"},
		{"model", "catalog"},
	}
	requireCommandPaths(t, spec.Capabilities, want)
	if got := len(spec.Capabilities); got != len(want) {
		t.Fatalf("model tree declares %d commands, want %d: %v", got, len(want), pathKeys(spec.Capabilities))
	}
	for _, path := range want {
		if !containsPath(spec.Required, path) {
			t.Errorf("model Required set does not freeze %q", "keel "+strings.Join(path, " "))
		}
	}
	requireNoError(t, spec.Validate())
}

func TestAgentPluginTreeCoversSpecCommands(t *testing.T) {
	spec := AgentPluginCLISpec()
	want := [][]string{
		{"agent", "list"},
		{"agent", "show"},
		{"agent", "create"},
		{"agent", "update"},
		{"agent", "delete"},
		{"agent", "start"},
		{"agent", "stop"},
	}
	requireCommandPaths(t, spec.Capabilities, want)
	if got := len(spec.Capabilities); got != len(want) {
		t.Fatalf("agent tree declares %d commands, want %d: %v", got, len(want), pathKeys(spec.Capabilities))
	}
	for _, path := range want {
		if !containsPath(spec.Required, path) {
			t.Errorf("agent Required set does not freeze %q", "keel "+strings.Join(path, " "))
		}
	}
	requireNoError(t, spec.Validate())
}

func TestTeamPluginTreeCoversSpecCommands(t *testing.T) {
	spec := TeamPluginCLISpec()
	want := [][]string{
		{"team", "list"},
		{"team", "show"},
		{"team", "create"},
		{"team", "update"},
		{"team", "delete"},
		{"team", "member", "add"},
		{"team", "member", "remove"},
		{"team", "member", "list"},
		{"team", "member", "save"},
		{"team", "save"},
		{"team", "start"},
	}
	requireCommandPaths(t, spec.Capabilities, want)
	if got := len(spec.Capabilities); got != len(want) {
		t.Fatalf("team tree declares %d commands, want %d: %v", got, len(want), pathKeys(spec.Capabilities))
	}
	for _, path := range want {
		if !containsPath(spec.Required, path) {
			t.Errorf("team Required set does not freeze %q", "keel "+strings.Join(path, " "))
		}
	}
	requireNoError(t, spec.Validate())
}

// TestPluginTreesValidateWithPluginpkg pins the acceptance that the data is
// checkable by pluginpkg's own CLI contract: the manifest carrying each tree is
// built here and validated with no other validator in between.
func TestPluginTreesValidateWithPluginpkg(t *testing.T) {
	specs := PluginCLISpecs()
	wantIDs := []string{"keel.model", "keel.agent", "keel.agent-team"}
	if len(specs) != len(wantIDs) {
		t.Fatalf("PluginCLISpecs returned %d specs, want %d", len(specs), len(wantIDs))
	}
	requireNoError(t, ValidatePluginCLISpecs(specs))
	for i, spec := range specs {
		if spec.PluginID != wantIDs[i] {
			t.Errorf("spec %d is %s, want %s", i, spec.PluginID, wantIDs[i])
		}
		manifest := spec.Manifest()
		requireNoError(t, manifest.Validate(pluginpkg.Environment{KeelVersion: "0.2.0"}))
		if got, want := len(manifest.CLICapabilities()), len(spec.Capabilities); got != want {
			t.Errorf("%s: manifest exposes %d capabilities, want %d", spec.PluginID, got, want)
		}
		found, ok := manifest.FindCLICapability(spec.Required[0]...)
		if !ok {
			t.Errorf("%s: manifest cannot find its first required command %v", spec.PluginID, spec.Required[0])
			continue
		}
		if found.Command() != "keel "+strings.Join(spec.Required[0], " ") {
			t.Errorf("%s: lookup returned %q", spec.PluginID, found.Command())
		}
	}
}

func TestCapabilityPathsAreGloballyUnique(t *testing.T) {
	owner := make(map[string]string, 32)
	total := 0
	for _, spec := range PluginCLISpecs() {
		for _, capability := range spec.Capabilities {
			key := capability.Key()
			if previous, exists := owner[key]; exists {
				t.Errorf("path %q is declared by %s and %s", capability.Command(), previous, spec.PluginID)
			}
			owner[key] = spec.PluginID
			total++
		}
	}
	all := CLICapabilities()
	if len(all) != total {
		t.Fatalf("union exposes %d commands, the specs declare %d", len(all), total)
	}
	seen := make(map[string]bool, len(all))
	for _, capability := range all {
		if seen[capability.Key()] {
			t.Fatalf("union repeats %q", capability.Command())
		}
		seen[capability.Key()] = true
	}
}

func TestEveryCapabilityDeclaresContractFields(t *testing.T) {
	for _, spec := range PluginCLISpecs() {
		if len(spec.Capabilities) == 0 {
			t.Fatalf("%s declares no command", spec.PluginID)
		}
		for _, capability := range spec.Capabilities {
			command := capability.Command()
			if strings.TrimSpace(capability.Summary) == "" {
				t.Errorf("%s: summary is required", command)
			}
			if strings.TrimSpace(capability.Description) == "" {
				t.Errorf("%s: description is required", command)
			}
			if len(capability.Permissions) == 0 {
				t.Errorf("%s: permissions are required", command)
			}
			for _, permission := range capability.Permissions {
				if !strings.Contains(permission, ".") {
					t.Errorf("%s: permission %q must name a domain and an action", command, permission)
				}
			}
			if _, ok := pluginpkg.ParseDanger(string(capability.Danger)); !ok {
				t.Errorf("%s: danger %q is not one of %v", command, capability.Danger, pluginpkg.Dangers())
			}
			if len(capability.CallableBy) == 0 {
				t.Errorf("%s: callable_by is required", command)
			}
			for _, caller := range capability.CallableBy {
				if _, ok := pluginpkg.ParseCaller(string(caller)); !ok {
					t.Errorf("%s: callable_by %q is not one of %v", command, caller, pluginpkg.Callers())
				}
			}
			if capability.ArgsSchema == nil || capability.OutputSchema == nil {
				t.Errorf("%s: args_schema and output_schema are required", command)
			}
			// idempotent is a bool, so "declared" means "present on the wire":
			// the node must carry the field (no omitempty) instead of leaving a
			// reader to guess whether a retry repeats the effect.
			encoded, err := json.Marshal(capability)
			requireNoError(t, err)
			if !bytes.Contains(encoded, []byte(`"idempotent":`)) {
				t.Errorf("%s: idempotent is not declared in the encoded node", command)
			}
		}
	}
}

// TestCapabilityContractTypesAndTagsAreFrozen pins the frozen shape of the
// contract the trees are written against:
//
//   - `permissions` is []string (a capability cannot require a structured
//     object, so the settings page and the dispatcher read the same list);
//   - `callable_by` is the closed pluginpkg.Caller set ui|agent|team_captain,
//     and every tree only uses values from it;
//   - the agent's team CLI switch is the Go field TeamCLI tagged `teamCli`,
//     which is what the team captain's surface is read from;
//   - the wire names (json and yaml) are the spec's, so a plugin.yaml and a
//     --json render cannot drift apart.
func TestCapabilityContractTypesAndTagsAreFrozen(t *testing.T) {
	// Compile-time pins: changing either field's Go type fails this build.
	pinFieldTypes := func(capability pluginpkg.CLICapability) {
		var _ []string = capability.Permissions
		var _ []pluginpkg.Caller = capability.CallableBy
	}
	pinFieldTypes(pluginpkg.CLICapability{})

	wantCallers := []pluginpkg.Caller{pluginpkg.CallerUI, pluginpkg.CallerAgent, pluginpkg.CallerTeamCaptain}
	if got := pluginpkg.Callers(); !slices.Equal(got, wantCallers) {
		t.Fatalf("the callable_by closed set is %v, want %v", got, wantCallers)
	}
	closed := make(map[pluginpkg.Caller]bool, len(wantCallers))
	for _, caller := range wantCallers {
		closed[caller] = true
	}
	for _, spec := range PluginCLISpecs() {
		for _, capability := range spec.Capabilities {
			for _, caller := range capability.CallableBy {
				if !closed[caller] {
					t.Errorf("%s: callable_by %q is outside %v", capability.Command(), caller, wantCallers)
				}
			}
		}
	}

	wantTags := []struct {
		field string
		name  string
	}{
		{"Path", "path"},
		{"Summary", "summary"},
		{"Description", "description"},
		{"Permissions", "permissions"},
		{"Danger", "danger"},
		{"Idempotent", "idempotent"},
		{"CallableBy", "callable_by"},
	}
	capabilityType := reflect.TypeOf(pluginpkg.CLICapability{})
	for _, want := range wantTags {
		field, ok := capabilityType.FieldByName(want.field)
		if !ok {
			t.Fatalf("CLICapability.%s is missing", want.field)
		}
		for _, tagName := range []string{"json", "yaml"} {
			got := strings.Split(field.Tag.Get(tagName), ",")[0]
			if got != want.name {
				t.Errorf("CLICapability.%s %s tag = %q, want %q", want.field, tagName, got, want.name)
			}
		}
	}

	agentType := reflect.TypeOf(Agent{})
	teamCLIField, ok := agentType.FieldByName("TeamCLI")
	if !ok {
		t.Fatal("Agent.TeamCLI is missing; the captain surface has no switch")
	}
	if got, want := teamCLIField.Type, reflect.TypeOf(TeamCLI{}); got != want {
		t.Errorf("Agent.TeamCLI has type %s, want %s", got, want)
	}
	if got, want := teamCLIField.Tag.Get("json"), "teamCli,omitempty"; got != want {
		t.Errorf("Agent.TeamCLI json tag = %q, want %q", got, want)
	}

	// The encoded node must carry every accepted field, including the boolean
	// idempotent (no omitempty: a reader never has to guess a retry's effect).
	encoded, err := json.Marshal(ModelPluginCLISpec().Capabilities[0])
	requireNoError(t, err)
	var wire map[string]any
	requireNoError(t, json.Unmarshal(encoded, &wire))
	for _, tag := range wantTags {
		if _, present := wire[tag.name]; !present {
			t.Errorf("encoded capability is missing %q: %s", tag.name, encoded)
		}
	}
	if permissions, ok := wire["permissions"].([]any); !ok || len(permissions) == 0 {
		t.Errorf("permissions does not encode as a non-empty JSON array: %v", wire["permissions"])
	}
	if callers, ok := wire["callable_by"].([]any); !ok || len(callers) == 0 {
		t.Errorf("callable_by does not encode as a non-empty JSON array: %v", wire["callable_by"])
	}
}

func TestValidateReportsMissingSubcommand(t *testing.T) {
	cases := []struct {
		name    string
		spec    PluginCLISpec
		drop    []string
		missing string
	}{
		{"model catalog dropped", ModelPluginCLISpec(), []string{"model", "catalog"}, "keel model catalog"},
		{"agent stop dropped", AgentPluginCLISpec(), []string{"agent", "stop"}, "keel agent stop"},
		{"team member add dropped", TeamPluginCLISpec(), []string{"team", "member", "add"}, "keel team member add"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			spec := testCase.spec
			spec.Capabilities = withoutCapability(t, spec.Capabilities, testCase.drop)
			err := spec.Validate()
			if err == nil {
				t.Fatalf("dropping %q validated; the required set must catch it", testCase.missing)
			}
			problems := pluginpkg.FieldErrors(err)
			if len(problems) != 1 {
				t.Fatalf("dropping one command produced %d problems, want 1: %v", len(problems), problems)
			}
			problem := problems[0]
			if problem.Field != "provides.cli" {
				t.Errorf("problem field = %q, want provides.cli", problem.Field)
			}
			wantMessage := `required command "` + testCase.missing + `" is not declared`
			if problem.Message != wantMessage {
				t.Errorf("problem message = %q, want %q", problem.Message, wantMessage)
			}
		})
	}
}

func TestValidateReportsDuplicatePath(t *testing.T) {
	spec := AgentPluginCLISpec()
	declared := spec.CLI()
	spec.Capabilities = append(declared, declared[0])
	err := spec.Validate()
	if err == nil {
		t.Fatal("a tree declaring one path twice validated")
	}
	reported := false
	for _, problem := range pluginpkg.FieldErrors(err) {
		if !strings.HasSuffix(problem.Field, ".path") {
			continue
		}
		if !strings.Contains(problem.Message, "already declared") {
			continue
		}
		if !strings.Contains(problem.Message, declared[0].Command()) {
			t.Errorf("duplicate report does not name %q: %s", declared[0].Command(), problem)
			continue
		}
		reported = true
	}
	if !reported {
		t.Fatalf("duplicate path was not reported: %v", pluginpkg.FieldErrors(err))
	}
}

func TestValidatePluginCLISpecsReportsCrossPluginPath(t *testing.T) {
	model := ModelPluginCLISpec()
	agent := AgentPluginCLISpec()
	modelCatalog, ok := findCapability(model.Capabilities, "model", "catalog")
	if !ok {
		t.Fatal("model plugin must declare keel model catalog")
	}
	agent.Capabilities = append(agent.CLI(), modelCatalog)
	// Each tree on its own is a valid manifest: only the shared namespace makes
	// the repeat a problem, which is what the union validator is for.
	requireNoError(t, agent.Validate())
	err := ValidatePluginCLISpecs([]PluginCLISpec{model, agent})
	if err == nil {
		t.Fatal("two plugins declaring one command path validated")
	}
	want := `command "keel model catalog" is declared by both keel.model and keel.agent`
	found := false
	for _, problem := range pluginpkg.FieldErrors(err) {
		if problem.Message == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("cross-plugin repeat not reported: %v", pluginpkg.FieldErrors(err))
	}
}

// TestTeamTreeMatchesTeamCLIVocabulary pins the team tree to the existing
// TeamCLI allowlist vocabulary: a declared `keel team member add` must be the
// `member.add` subcommand a team can allow or take away.
func TestTeamTreeMatchesTeamCLIVocabulary(t *testing.T) {
	known := make(map[string]bool, len(KnownTeamSubcommands()))
	for _, subcommand := range KnownTeamSubcommands() {
		known[subcommand] = true
	}
	declared := make(map[string]bool, len(KnownTeamSubcommands()))
	for _, capability := range TeamPluginCLISpec().Capabilities {
		subcommand, ok := TeamSubcommand(capability.Path)
		if !ok {
			t.Fatalf("%s: team command must start with the team segment", capability.Command())
		}
		if !known[subcommand] {
			t.Errorf("%s: %q is not a TeamCLI subcommand %v", capability.Command(), subcommand, KnownTeamSubcommands())
		}
		declared[subcommand] = true
	}
	// The reverse direction is what keeps the vocabulary honest: a subcommand a
	// team can allow but the tree does not declare would be a granted dead
	// command (teamCli {enabled, commands: [member.list]} with no such command).
	for _, subcommand := range KnownTeamSubcommands() {
		if !declared[subcommand] {
			t.Errorf("TeamCLI subcommand %q has no declared command; granting it would be a dead command", subcommand)
		}
	}
}

// TestManifestServesCallerFaces pins the caller split through pluginpkg's public
// face: the human sees everything, an agent never sees a destructive command and
// a captain sees its self-assembly surface.
func TestManifestServesCallerFaces(t *testing.T) {
	manifest := AgentPluginCLISpec().Manifest()
	every := manifest.CLICapabilities()
	if got := len(manifest.CallableCLICapabilities(pluginpkg.CallerUI)); got != len(every) {
		t.Fatalf("the ui face sees %d of %d agent commands; it must see all", got, len(every))
	}
	captain := pathSet(manifest.CallableCLICapabilities(pluginpkg.CallerTeamCaptain))
	wantCaptain := []string{"agent list", "agent show", "agent start", "agent stop"}
	if len(captain) != len(wantCaptain) {
		t.Fatalf("the captain face sees %v, want %v", captain, wantCaptain)
	}
	for _, key := range wantCaptain {
		if !captain[key] {
			t.Errorf("the captain face is missing %q", "keel "+key)
		}
	}
	agentFace := pathSet(manifest.CallableCLICapabilities(pluginpkg.CallerAgent))
	for _, key := range []string{"agent delete"} {
		if agentFace[key] {
			t.Errorf("an agent must not run %q: destructive commands stay on the human face", "keel "+key)
		}
	}
	if !agentFace["agent update"] {
		t.Error("an agent must be able to update its own configuration")
	}
	if _, ok := manifest.FindCLICapability("agent", "delete"); !ok {
		t.Fatal("the tree lost keel agent delete")
	}

	teamManifest := TeamPluginCLISpec().Manifest()
	teamCaptain := pathSet(teamManifest.CallableCLICapabilities(pluginpkg.CallerTeamCaptain))
	for _, key := range []string{"team list", "team show", "team member add", "team member remove", "team member list", "team member save", "team save", "team start"} {
		if !teamCaptain[key] {
			t.Errorf("the captain face is missing %q", "keel "+key)
		}
	}
	for _, key := range []string{"team create", "team update", "team delete"} {
		if teamCaptain[key] {
			t.Errorf("the captain face must not expose %q", "keel "+key)
		}
	}
}

// TestCLITreeRendersNestedCommands pins that the declared paths are real
// commands, not flat strings: pluginpkg renders them as nested nodes, and the
// deep node carries the capability a dispatcher runs.
func TestCLITreeRendersNestedCommands(t *testing.T) {
	combined := &pluginpkg.Manifest{Provides: pluginpkg.Provides{CLI: CLICapabilities()}}
	find := func(nodes []*pluginpkg.CLINode, segment string) *pluginpkg.CLINode {
		for _, node := range nodes {
			if node.Segment == segment {
				return node
			}
		}
		t.Fatalf("segment %q is not rendered", segment)
		return nil
	}

	team := find(combined.CLITree(), "team")
	if team.Capability != nil {
		t.Errorf("keel team is a group, not a command: %s", team.Capability.Command())
	}
	member := find(team.Children, "member")
	add := find(member.Children, "add")
	if add.Capability == nil {
		t.Fatal("keel team member add renders without a capability")
	}
	if got := add.Capability.Command(); got != "keel team member add" {
		t.Fatalf("deep node command = %q, want keel team member add", got)
	}

	model := find(combined.CLITree(), "model")
	provider := find(model.Children, "provider")
	if got := len(provider.Children); got != len(modelProviderVerbs) {
		t.Fatalf("keel model provider renders %d sub-commands, want %d", got, len(modelProviderVerbs))
	}
}

func TestCapabilityTreesReturnFreshCopies(t *testing.T) {
	first := ModelPluginCLISpec()
	first.Capabilities[0].Path[0] = "mutated"
	first.Capabilities[0].CallableBy[0] = pluginpkg.CallerTeamCaptain
	first.Capabilities[0].Permissions[0] = "mutated.permission"
	second := ModelPluginCLISpec()
	if got := second.Capabilities[0].Path[0]; got != "model" {
		t.Fatalf("the capability table is shared between calls: path[0] = %q", got)
	}
	if got := second.Capabilities[0].Permissions[0]; got != "model.read" {
		t.Fatalf("the permission list is shared between calls: %q", got)
	}
	if got := second.Capabilities[0].CallableBy[0]; got != pluginpkg.CallerUI {
		t.Fatalf("the caller list is shared between calls: %q", got)
	}

	spec := AgentPluginCLISpec()
	manifest := spec.Manifest()
	manifest.Provides.CLI[0].Path[0] = "mutated"
	manifest.Provides.CLI[0].ArgsSchema["type"] = "mutated"
	if got := spec.Capabilities[0].Path[0]; got != "agent" {
		t.Fatalf("the manifest aliases the spec path: %q", got)
	}
	if got := spec.Capabilities[0].ArgsSchema["type"]; got != "object" {
		t.Fatalf("the manifest aliases the spec schema: %v", got)
	}
}

func TestSpecWithoutRequiredSetFailsClosed(t *testing.T) {
	spec := ModelPluginCLISpec()
	spec.Required = nil
	err := spec.Validate()
	if err == nil {
		t.Fatal("a spec that freezes no command set must not validate")
	}
	if !strings.Contains(err.Error(), "coverage cannot be checked") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestModelProviderAddKeepsTheTypeVocabularyOpen pins the two CLI rules that
// must match the spec and the HTTP DTO: the provider type is not an enum (a
// plugin-declared "embedding" must be creatable without a CLI change) and
// credentialRef is optional (an empty reference means needs_auth, not a refusal
// to create).
func TestModelProviderAddKeepsTheTypeVocabularyOpen(t *testing.T) {
	spec := ModelPluginCLISpec()
	add, ok := findCapability(spec.Capabilities, "model", "provider", "add")
	if !ok {
		t.Fatal("keel model provider add is missing")
	}
	properties, ok := add.ArgsSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("args_schema.properties = %#v", add.ArgsSchema["properties"])
	}
	typeProperty, ok := properties["type"].(map[string]any)
	if !ok {
		t.Fatalf("type property = %#v", properties["type"])
	}
	if _, closed := typeProperty["enum"]; closed {
		t.Fatalf("type is a closed enum %v; provider types are plugin-declared", typeProperty["enum"])
	}
	if typeProperty["type"] != "string" {
		t.Fatalf("type property = %#v, want a free string", typeProperty)
	}

	required, _ := add.ArgsSchema["required"].([]string)
	for _, name := range required {
		if name == "credentialRef" {
			t.Fatal("credentialRef is required; an empty reference must be legal and land on needs_auth")
		}
	}
	if !slices.Contains(required, "type") || !slices.Contains(required, "name") {
		t.Fatalf("required = %v, want type and name", required)
	}

	// The list filter is open too: filtering by a plugin type cannot be limited
	// to the first batch.
	list, ok := findCapability(spec.Capabilities, "model", "provider", "list")
	if !ok {
		t.Fatal("keel model provider list is missing")
	}
	listProperties := list.ArgsSchema["properties"].(map[string]any)
	listType := listProperties["type"].(map[string]any)
	if _, closed := listType["enum"]; closed {
		t.Fatalf("list --type is a closed enum %v", listType["enum"])
	}
}

// TestCapabilityCatalogServesEveryTree pins the serving face: the catalog that
// `keel plugin capabilities --json` and GET /v1/plugins/capabilities render is
// built from the same specs the manifest validation reads, validates as an
// install would, and round-trips through its own JSON.
func TestCapabilityCatalogServesEveryTree(t *testing.T) {
	catalog := ServeCapabilityCatalog()
	if got := len(catalog.Plugins); got != 3 {
		t.Fatalf("catalog carries %d plugins, want 3", got)
	}
	requireNoError(t, catalog.Validate())

	wantIDs := []string{"keel.model", "keel.agent", "keel.agent-team"}
	for index, plugin := range catalog.Plugins {
		if plugin.PluginID != wantIDs[index] {
			t.Errorf("plugin %d = %q, want %q", index, plugin.PluginID, wantIDs[index])
		}
		if len(plugin.Commands) == 0 {
			t.Errorf("%s serves no command", plugin.PluginID)
		}
	}

	blob, err := catalog.JSON()
	requireNoError(t, err)
	var decoded CapabilityCatalog
	requireNoError(t, json.Unmarshal(blob, &decoded))
	wantCanonical, err := json.Marshal(catalog)
	requireNoError(t, err)
	gotCanonical, err := json.Marshal(decoded)
	requireNoError(t, err)
	if !bytes.Equal(wantCanonical, gotCanonical) {
		t.Fatalf("catalog JSON round trip changed the trees:\nwant %s\ngot  %s", wantCanonical, gotCanonical)
	}
	requireNoError(t, decoded.Validate())

	// The served union is the same set the CLI trees declare; a catalog cannot
	// grow a command the tree does not own.
	served := make(map[string]bool)
	for _, plugin := range catalog.Plugins {
		for _, capability := range plugin.Commands {
			if served[capability.Key()] {
				t.Fatalf("catalog serves %q twice", capability.Command())
			}
			served[capability.Key()] = true
		}
	}
	declared := pathSet(CLICapabilities())
	if len(served) != len(declared) {
		t.Fatalf("catalog serves %d commands, the trees declare %d", len(served), len(declared))
	}
	for key := range declared {
		if !served[key] {
			t.Errorf("catalog is missing %q", "keel "+key)
		}
	}
}

// TestCaptainCommandFaceFollowsEffectiveSwitch pins the P7 semantics that were
// only declarative before: the captain's command face is computed from the tree
// and the effective switch, a disabled switch removes the commands, and a
// direct call fails with a typed *PermissionError.
func TestCaptainCommandFaceFollowsEffectiveSwitch(t *testing.T) {
	spec := TeamPluginCLISpec()
	all := []string{
		TeamCLIMemberAdd, TeamCLIMemberRemove, TeamCLIMemberList, TeamCLIMemberSave,
		TeamCLISave, TeamCLIStart,
	}
	on := TeamCLI{Enabled: true, Commands: all}

	face := pathSet(CaptainTeamCapabilities(spec, on, on))
	for _, key := range []string{
		"team member add", "team member remove", "team member list",
		"team member save", "team save", "team start",
	} {
		if !face[key] {
			t.Errorf("the captain face is missing %q", "keel "+key)
		}
	}
	for _, key := range []string{"team create", "team update", "team delete"} {
		if face[key] {
			t.Errorf("the captain face exposes %q", "keel "+key)
		}
	}
	requireNoError(t, AuthorizeCaptainCommand(spec, on, on, "team", "member", "add"))

	// Off on either face removes the commands and yields the typed refusal.
	for _, off := range []struct {
		name  string
		team  TeamCLI
		agent TeamCLI
	}{
		{"team off", TeamCLI{}, on},
		{"agent off", on, TeamCLI{}},
	} {
		off := off
		t.Run(off.name, func(t *testing.T) {
			if face := CaptainTeamCapabilities(spec, off.team, off.agent); len(face) != 0 {
				t.Fatalf("a disabled switch still exposes %v", pathKeys(face))
			}
			err := AuthorizeCaptainCommand(spec, off.team, off.agent, "team", "member", "add")
			var permission *PermissionError
			if !errors.As(err, &permission) {
				t.Fatalf("error = %v, want *PermissionError", err)
			}
			if permission.Code() != "team_cli_disabled" {
				t.Fatalf("code = %q, want team_cli_disabled", permission.Code())
			}
		})
	}

	// Enabled but not allowlisted is the second typed refusal, and the command
	// is absent from the face.
	narrow := TeamCLI{Enabled: true, Commands: []string{TeamCLIMemberAdd}}
	face = pathSet(CaptainTeamCapabilities(spec, narrow, on))
	if !face["team member add"] || face["team member remove"] {
		t.Fatalf("narrow face = %v, want only member add", face)
	}
	err := AuthorizeCaptainCommand(spec, narrow, on, "team", "member", "remove")
	var permission *PermissionError
	if !errors.As(err, &permission) || permission.Code() != "team_cli_not_allowed" {
		t.Fatalf("error = %v, want team_cli_not_allowed", err)
	}

	// A command the tree never grants the captain is not_declared.
	err = AuthorizeCaptainCommand(spec, on, on, "team", "delete")
	if !errors.As(err, &permission) || permission.Code() != "team_cli_not_declared" {
		t.Fatalf("error = %v, want team_cli_not_declared", err)
	}
}
