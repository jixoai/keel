package toolsurface

import (
	"testing"

	"github.com/jixoai/keel/internal/access"
)

func TestPermissionCatalogIsComplete(t *testing.T) {
	if err := ValidatePermissionCatalog(); err != nil {
		t.Fatalf("ValidatePermissionCatalog: %v", err)
	}
	if got := len(ToolPermissionManifest()); got == 0 {
		t.Fatal("permission catalog is empty")
	}
	// The eleven builtin tools, the five model tools built by mediawiring and
	// the two super-agent worker tools (product extras).
	for _, tool := range append(BuiltinNames(), ImageGenerate, VideoGenerate, SpeechTTS, SpeechSTT, DecisionAsk, Report, Ask) {
		found := false
		for _, entry := range ToolPermissionManifest() {
			if entry.Tool == tool {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("tool %q has no permission declaration", tool)
		}
	}
}

func TestRequiredKeyMapping(t *testing.T) {
	cases := []struct {
		tool   string
		action string
		want   string
	}{
		{Bash, ActionRun, access.KeyShellExec},
		{Bash, ActionJobs, access.KeyShellExec},
		{Read, "", access.KeyFileRead},
		{Write, "", access.KeyFileWrite},
		{Edit, "", access.KeyFileWrite},
		{MCP, ActionList, access.KeyMCPUse},
		{MCP, ActionCall, access.KeyMCPUse},
		{Skills, ActionLs, access.KeySkillRead},
		{Skills, ActionManage, access.KeySkillManage},
		{Skills, ActionSources, access.KeySkillManage},
		{Tasks, ActionCreate, access.KeyTaskWrite},
		{Tasks, ActionClose, access.KeyTaskWrite},
		{Tasks, ActionList, access.KeyTaskRead},
		{Tasks, ActionShow, access.KeyTaskRead},
		{Agent, ActionSpawn, access.KeyBotInvoke},
		{Agent, ActionFork, access.KeyBotInvoke},
		{Agent, ActionResume, access.KeyBotInvoke},
		{Agent, ActionCancel, access.KeyBotInvoke},
		{Agent, ActionList, access.KeyBotInvoke},
		{Agent, ActionStatus, access.KeyBotInvoke},
		{Approve, ActionConfirm, access.KeyApprovalWrite},
		{Approve, ActionPrompt, access.KeyApprovalRead},
		{Approve, ActionChoice, access.KeyApprovalRead},
		{Notify, ActionPush, access.KeyNotificationWrite},
		{Notify, ActionAck, access.KeyNotificationWrite},
		{Notify, ActionList, access.KeyNotificationRead},
		{Creation, ActionInstall, access.KeyPluginLifecycleInstall},
		{Creation, ActionCatalog, access.KeyPluginInvoke},
		{Creation, ActionScaffold, access.KeyPluginInvoke},
		{Creation, ActionValidate, access.KeyPluginInvoke},
		{Creation, ActionList, access.KeyPluginInvoke},
		{Creation, ActionRemove, access.KeyPluginLifecycleUninstall},
		{ImageGenerate, "", access.KeyMediaInvoke},
		{VideoGenerate, "", access.KeyMediaInvoke},
		{SpeechTTS, "", access.KeyMediaInvoke},
		{SpeechSTT, "", access.KeyMediaInvoke},
		{DecisionAsk, "", access.KeyDecisionAsk},
		{Report, "", access.KeyTaskWrite},
		{Ask, "", access.KeyTaskWrite},
		// An unknown or omitted action on a known aggregate tool falls back to
		// the tool's base key so the tool's own unknown_action error reaches the
		// model.
		{Bash, "bogus", access.KeyShellExec},
		{Tasks, "bogus", access.KeyTaskWrite},
		{Agent, "", access.KeyBotInvoke},
		{Tasks, "", access.KeyTaskWrite},
	}
	for _, test := range cases {
		key, ok := RequiredKey(test.tool, test.action)
		if !ok {
			t.Fatalf("RequiredKey(%q, %q) has no declaration", test.tool, test.action)
		}
		if key != test.want {
			t.Fatalf("RequiredKey(%q, %q) = %q, want %q", test.tool, test.action, key, test.want)
		}
	}

	// A plugin-published tool is not in the static map; the authorizer gates it
	// with plugin.invoke.
	if _, ok := RequiredKey("vendor_tool", ""); ok {
		t.Fatal("an unknown tool was declared in the static permission catalog")
	}
	if key, ok := RequiredKey("echo", ""); ok {
		t.Fatalf("plugin tool echo declared key %q; it must use plugin.invoke", key)
	}
}

func TestActionOfOnlyParsesAggregateTools(t *testing.T) {
	if got := ActionOf(Bash, []byte(`{"action":"run"}`)); got != ActionRun {
		t.Fatalf("bash action = %q, want run", got)
	}
	if got := ActionOf(Read, []byte(`{"action":"run"}`)); got != "" {
		t.Fatalf("read action = %q, want empty (single-action tool)", got)
	}
	if got := ActionOf(Bash, []byte(`not json`)); got != "" {
		t.Fatalf("malformed arguments action = %q, want empty", got)
	}
}

func TestPermissionEndpointsPassTheCoverageGate(t *testing.T) {
	endpoints := PermissionEndpoints()
	if len(endpoints) != len(ToolPermissionManifest()) {
		t.Fatalf("endpoints = %d, declarations = %d", len(endpoints), len(ToolPermissionManifest()))
	}
	report := access.CheckCoverage(endpoints)
	if !report.OK() {
		t.Fatalf("tool permission coverage failed: %v", report.Err())
	}
	if report.Checked != len(endpoints) {
		t.Fatalf("checked = %d, want %d", report.Checked, len(endpoints))
	}
	for _, endpoint := range endpoints {
		if endpoint.Kind != access.EndpointTool || endpoint.RequiredKey == "" || endpoint.Name == "" {
			t.Fatalf("malformed tool endpoint: %+v", endpoint)
		}
	}
}

// TestBotOperatorHoldsTheWholeToolSurface pins the operator preset against the
// runtime surface: it is the preset that runs the full builtin tool set, so a
// new tool key without a preset grant would silently disable that tool for every
// operator Bot.
func TestBotOperatorHoldsTheWholeToolSurface(t *testing.T) {
	preset, ok := access.LookupBuiltinGroup("bot-operator")
	if !ok {
		t.Fatal("bot-operator preset missing")
	}
	held := make(map[string]bool, len(preset.Grants))
	for _, grant := range preset.Grants {
		held[grant.Key] = true
	}
	for _, entry := range ToolPermissionManifest() {
		if !held[entry.Key] {
			t.Fatalf("bot-operator does not hold %q (tool %q action %q)", entry.Key, entry.Tool, entry.Action)
		}
	}
}

func TestValidatePermissionCatalogRejects(t *testing.T) {
	known := map[string][]string{
		"alpha": {"one", "two"},
		"beta":  {""},
	}
	good := []ToolPermission{
		{Tool: "alpha", Action: "one", Key: access.KeySessionRead},
		{Tool: "alpha", Action: "two", Key: access.KeySessionWrite},
		{Tool: "beta", Key: access.KeySessionRead},
	}
	if err := validatePermissionCatalog(good, known); err != nil {
		t.Fatalf("well-formed catalog rejected: %v", err)
	}
	cases := map[string][]ToolPermission{
		"empty": nil,
		"unknown tool": {
			{Tool: "gamma", Action: "one", Key: access.KeySessionRead},
		},
		"unknown action": {
			{Tool: "alpha", Action: "one", Key: access.KeySessionRead},
			{Tool: "alpha", Action: "two", Key: access.KeySessionWrite},
			{Tool: "alpha", Action: "three", Key: access.KeySessionRead},
			{Tool: "beta", Key: access.KeySessionRead},
		},
		"unknown key": {
			{Tool: "alpha", Action: "one", Key: "session.fly"},
			{Tool: "alpha", Action: "two", Key: access.KeySessionWrite},
			{Tool: "beta", Key: access.KeySessionRead},
		},
		"missing action": {
			{Tool: "alpha", Action: "one", Key: access.KeySessionRead},
			{Tool: "beta", Key: access.KeySessionRead},
		},
		"duplicate": {
			{Tool: "alpha", Action: "one", Key: access.KeySessionRead},
			{Tool: "alpha", Action: "one", Key: access.KeySessionWrite},
			{Tool: "alpha", Action: "two", Key: access.KeySessionWrite},
			{Tool: "beta", Key: access.KeySessionRead},
		},
	}
	for name, specs := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validatePermissionCatalog(specs, known); err == nil {
				t.Fatalf("%s: invalid catalog passed", name)
			}
		})
	}
}

func TestToolPermissionManifestReturnsACopy(t *testing.T) {
	first := ToolPermissionManifest()
	first[0].Key = "session.fly"
	second := ToolPermissionManifest()
	if second[0].Key == "session.fly" {
		t.Fatal("ToolPermissionManifest exposed the catalog")
	}
}
