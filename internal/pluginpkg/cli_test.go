package pluginpkg

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// cliManifest is the baseline capability tree: one read capability declaring
// every field of the contract, and two write capabilities so caller faces and
// duplicate detection have something to separate.
func cliManifest() *Manifest {
	manifest := validManifest()
	manifest.Provides.CLI = []CLICapability{
		{
			Path:        []string{"model", "provider", "list"},
			Summary:     "列出模型 Provider",
			Description: "按类型/状态过滤；不返回凭据明文。",
			ArgsSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type": map[string]any{"enum": []any{"llm", "image", "video", "decision", "tts", "stt"}},
				},
			},
			OutputSchema: map[string]any{
				"type":  "array",
				"items": map[string]any{"$ref": "#/definitions/model_provider_summary"},
			},
			Permissions: []string{"model.read"},
			Danger:      DangerRead,
			Idempotent:  true,
			CallableBy:  []Caller{CallerUI, CallerAgent, CallerTeamCaptain},
			Examples:    []string{"keel model provider list --type image"},
		},
		{
			Path:         []string{"model", "provider", "add"},
			Summary:      "新增模型 Provider",
			Description:  "从凭据引用创建 Provider；凭据只接受引用。",
			ArgsSchema:   map[string]any{"type": "object"},
			OutputSchema: map[string]any{"type": "object"},
			Permissions:  []string{"model.write"},
			Danger:       DangerWrite,
			Idempotent:   false,
			CallableBy:   []Caller{CallerUI, CallerAgent},
			Examples:     []string{"keel model provider add --type image --credential-ref vault://img"},
		},
		{
			Path:         []string{"team", "member", "add"},
			Summary:      "新增团队成员",
			Description:  "把 Agent 加入当前团队配置；不修改 model/agent provider 配置。",
			ArgsSchema:   map[string]any{"type": "object"},
			OutputSchema: map[string]any{"type": "object"},
			Permissions:  []string{"team.write"},
			Danger:       DangerWrite,
			CallableBy:   []Caller{CallerUI, CallerTeamCaptain},
			Examples:     []string{"keel team member add agent_design_lead"},
		},
	}
	return manifest
}

// requireSameCLITree compares two capability trees through their canonical JSON
// encoding, so YAML-decoded ints and JSON-decoded float64 do not read as a
// difference while a real field difference does.
func requireSameCLITree(t *testing.T, want, got []CLICapability) {
	t.Helper()
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	if !bytes.Equal(wantJSON, gotJSON) {
		t.Fatalf("capability tree changed:\nwant %s\ngot  %s", wantJSON, gotJSON)
	}
}

func TestCLICapabilityDeclaresEveryContractField(t *testing.T) {
	// The design example, verbatim in shape: the contract must accept it as
	// written (openspec/changes/keel-agent-team-v1/design.md §2).
	raw := []byte(`
id: vendor.model
version: 1.2.3
kind: in-process
description: Model plugin.
provides:
  cli:
    - path: [model, provider, list]
      summary: 列出模型 Provider
      description: 按类型/状态过滤；不返回凭据明文。
      args_schema:
        type: object
        properties:
          type:
            enum: [llm, image, video, decision, tts, stt]
      output_schema:
        type: array
        items: { $ref: "#/definitions/model_provider_summary" }
      permissions: [model.read]
      danger: read
      idempotent: true
      callable_by: [ui, agent, team_captain]
      examples: ["keel model provider list --type image"]
    - path: [model, provider, add]
      summary: 新增模型 Provider
      description: 从凭据引用创建 Provider；凭据只接受引用。
      args_schema:
        type: object
        properties:
          type: { type: string }
          name: { type: string }
      output_schema:
        type: object
      permissions: [model.write]
      danger: write
      idempotent: false
      callable_by: [ui, agent]
      examples: ["keel model provider add --type image --credential-ref vault://img"]
`)
	manifest, err := ParseManifest(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := manifest.Validate(testEnv()); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(manifest.Provides.CLI) != 2 {
		t.Fatalf("capabilities = %d", len(manifest.Provides.CLI))
	}

	list := manifest.Provides.CLI[0]
	if !samePath(list.Path, []string{"model", "provider", "list"}) {
		t.Fatalf("path = %v", list.Path)
	}
	if list.Summary != "列出模型 Provider" {
		t.Fatalf("summary = %q", list.Summary)
	}
	if list.Description != "按类型/状态过滤；不返回凭据明文。" {
		t.Fatalf("description = %q", list.Description)
	}
	if got := list.Command(); got != "keel model provider list" {
		t.Fatalf("command = %q", got)
	}
	if list.ArgsSchema["type"] != "object" {
		t.Fatalf("args_schema = %#v", list.ArgsSchema)
	}
	properties, ok := list.ArgsSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("args_schema.properties = %#v", list.ArgsSchema["properties"])
	}
	typeArg, ok := properties["type"].(map[string]any)
	if !ok {
		t.Fatalf("args_schema.properties.type = %#v", properties["type"])
	}
	enumValues, ok := typeArg["enum"].([]any)
	if !ok || len(enumValues) != 6 || enumValues[0] != "llm" {
		t.Fatalf("args_schema.properties.type.enum = %#v", typeArg["enum"])
	}
	if list.OutputSchema["type"] != "array" {
		t.Fatalf("output_schema = %#v", list.OutputSchema)
	}
	items, ok := list.OutputSchema["items"].(map[string]any)
	if !ok || items["$ref"] != "#/definitions/model_provider_summary" {
		t.Fatalf("output_schema.items = %#v", list.OutputSchema["items"])
	}
	if len(list.Permissions) != 1 || list.Permissions[0] != "model.read" {
		t.Fatalf("permissions = %v", list.Permissions)
	}
	if list.Danger != DangerRead || list.Danger.RequiresConfirmation() {
		t.Fatalf("danger = %q", list.Danger)
	}
	if !list.Idempotent {
		t.Fatal("idempotent = false")
	}
	if len(list.CallableBy) != 3 || !list.CallableByCaller(CallerTeamCaptain) {
		t.Fatalf("callable_by = %v", list.CallableBy)
	}
	if len(list.Examples) != 1 || list.Examples[0] != "keel model provider list --type image" {
		t.Fatalf("examples = %v", list.Examples)
	}

	// The wire names are the contract: `keel plugin capabilities --json` and the
	// settings page read exactly these keys.
	encoded, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}
	for _, key := range []string{
		`"path"`, `"summary"`, `"description"`, `"args_schema"`, `"output_schema"`,
		`"permissions"`, `"danger"`, `"idempotent"`, `"callable_by"`, `"examples"`,
	} {
		if !bytes.Contains(encoded, []byte(key)) {
			t.Fatalf("capability JSON %s is missing %s", encoded, key)
		}
	}

	add := manifest.Provides.CLI[1]
	if add.Danger != DangerWrite || !add.Danger.RequiresConfirmation() {
		t.Fatalf("add danger = %q", add.Danger)
	}
	if add.Idempotent {
		t.Fatal("add idempotent = true")
	}
	if add.CallableByCaller(CallerTeamCaptain) {
		t.Fatal("add is not callable by a team captain")
	}
}

// TestParseManifestKnownFieldsAcceptsCLI is the KnownFields(true) half: the new
// tree is a known field, and a typo inside a node is still refused.
func TestParseManifestKnownFieldsAcceptsCLI(t *testing.T) {
	accepted := []byte(`
id: vendor.model
version: 1.2.3
kind: in-process
description: Model plugin.
provides:
  cli:
    - path: [model, provider, list]
      summary: list providers
      description: lists them
      permissions: [model.read]
      danger: read
      idempotent: true
      callable_by: [ui]
`)
	manifest, err := ParseManifest(accepted)
	if err != nil {
		t.Fatalf("provides.cli must be a known field: %v", err)
	}
	if len(manifest.Provides.CLI) != 1 {
		t.Fatalf("capabilities = %d", len(manifest.Provides.CLI))
	}

	rejected := []byte(`
id: vendor.model
version: 1.2.3
kind: in-process
description: Model plugin.
provides:
  cli:
    - path: [model, provider, list]
      summary: list providers
      description: lists them
      callableby: [ui]
`)
	if _, err := ParseManifest(rejected); err == nil {
		t.Fatal("a misspelled field inside a capability must still be rejected")
	}
}

func TestCLICapabilityRoundTripPreservesYAMLAndJSON(t *testing.T) {
	source := cliManifest()
	if err := source.Validate(testEnv()); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	yamlBytes, err := yaml.Marshal(source)
	if err != nil {
		t.Fatalf("yaml marshal: %v", err)
	}
	yamlParsed, err := ParseManifest(yamlBytes)
	if err != nil {
		t.Fatalf("yaml roundtrip parse: %v", err)
	}
	requireSameCLITree(t, source.Provides.CLI, yamlParsed.Provides.CLI)
	if yamlParsed.CLICapabilities()[1].Command() != "keel model provider list" {
		t.Fatalf("yaml roundtrip command = %q", yamlParsed.CLICapabilities()[1].Command())
	}

	jsonBytes, err := json.Marshal(source)
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}
	var jsonParsed Manifest
	if err := json.Unmarshal(jsonBytes, &jsonParsed); err != nil {
		t.Fatalf("json roundtrip unmarshal: %v", err)
	}
	requireSameCLITree(t, source.Provides.CLI, jsonParsed.Provides.CLI)
}

func TestCLICapabilityValidationFieldPaths(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Manifest)
		field  string
	}{
		{"path missing", func(m *Manifest) { m.Provides.CLI[0].Path = nil }, "provides.cli[0].path"},
		{"path empty list", func(m *Manifest) { m.Provides.CLI[0].Path = []string{} }, "provides.cli[0].path"},
		{"segment empty", func(m *Manifest) { m.Provides.CLI[0].Path = []string{"model", ""} }, "provides.cli[0].path[1]"},
		{"segment whitespace", func(m *Manifest) { m.Provides.CLI[0].Path = []string{"model", "  "} }, "provides.cli[0].path[1]"},
		{"segment uppercase", func(m *Manifest) { m.Provides.CLI[0].Path = []string{"Model"} }, "provides.cli[0].path[0]"},
		{"segment underscore", func(m *Manifest) { m.Provides.CLI[0].Path = []string{"model_provider"} }, "provides.cli[0].path[0]"},
		{"path duplicated", func(m *Manifest) {
			m.Provides.CLI[1].Path = append([]string(nil), m.Provides.CLI[0].Path...)
		}, "provides.cli[1].path"},
		{"summary empty", func(m *Manifest) { m.Provides.CLI[0].Summary = "  " }, "provides.cli[0].summary"},
		{"summary too long", func(m *Manifest) { m.Provides.CLI[0].Summary = strings.Repeat("x", maxCLISummaryRunes+1) }, "provides.cli[0].summary"},
		{"description empty", func(m *Manifest) { m.Provides.CLI[0].Description = "" }, "provides.cli[0].description"},
		{"args_schema missing", func(m *Manifest) { m.Provides.CLI[0].ArgsSchema = nil }, "provides.cli[0].args_schema"},
		{"output_schema missing", func(m *Manifest) { m.Provides.CLI[0].OutputSchema = nil }, "provides.cli[0].output_schema"},
		{"permissions missing", func(m *Manifest) { m.Provides.CLI[0].Permissions = nil }, "provides.cli[0].permissions"},
		{"danger missing", func(m *Manifest) { m.Provides.CLI[0].Danger = "" }, "provides.cli[0].danger"},
		{"danger unknown", func(m *Manifest) { m.Provides.CLI[0].Danger = "purge" }, "provides.cli[0].danger"},
		{"callable_by missing", func(m *Manifest) { m.Provides.CLI[0].CallableBy = nil }, "provides.cli[0].callable_by"},
		{"callable_by unknown", func(m *Manifest) { m.Provides.CLI[0].CallableBy = []Caller{CallerUI, "cron"} }, "provides.cli[0].callable_by[1]"},
		{"callable_by duplicated", func(m *Manifest) { m.Provides.CLI[0].CallableBy = []Caller{CallerUI, CallerUI} }, "provides.cli[0].callable_by[1]"},
		{"permission empty", func(m *Manifest) { m.Provides.CLI[0].Permissions = []string{""} }, "provides.cli[0].permissions[0]"},
		{"permission bad grammar", func(m *Manifest) { m.Provides.CLI[0].Permissions = []string{"model read"} }, "provides.cli[0].permissions[0]"},
		{"example empty", func(m *Manifest) { m.Provides.CLI[0].Examples = []string{" "} }, "provides.cli[0].examples[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := cliManifest()
			tc.mutate(manifest)
			err := manifest.Validate(testEnv())
			requireField(t, err, tc.field)
		})
	}
}

func TestCLICapabilityLookupCallerFacesAndCopies(t *testing.T) {
	manifest := cliManifest()
	capabilities := manifest.CLICapabilities()
	if len(capabilities) != 3 {
		t.Fatalf("capabilities = %d", len(capabilities))
	}
	wantOrder := []string{"model provider add", "model provider list", "team member add"}
	for i, want := range wantOrder {
		if got := capabilities[i].Key(); got != want {
			t.Fatalf("capability[%d] = %q, want %q", i, got, want)
		}
	}

	found, ok := manifest.FindCLICapability("model", "provider", "list")
	if !ok || found.Command() != "keel model provider list" {
		t.Fatalf("FindCLICapability = %v, %v", found.Command(), ok)
	}
	if _, ok := manifest.FindCLICapability("model", "provider"); ok {
		t.Fatal("a prefix is not a capability")
	}

	agentFacing := manifest.CallableCLICapabilities(CallerAgent)
	if len(agentFacing) != 2 {
		t.Fatalf("agent capabilities = %v", agentFacing)
	}
	for _, capability := range agentFacing {
		if capability.Key() == "team member add" {
			t.Fatal("team member add is not on the agent face")
		}
	}
	captainFacing := manifest.CallableCLICapabilities(CallerTeamCaptain)
	if len(captainFacing) != 2 || captainFacing[1].Key() != "team member add" {
		t.Fatalf("captain capabilities = %v", captainFacing)
	}

	capabilities[0].Path[0] = "mutated"
	fresh, ok := manifest.FindCLICapability("model", "provider", "add")
	if !ok || fresh.Path[0] != "model" {
		t.Fatal("returned capabilities must not alias the manifest")
	}
}

func TestCLITreeRendersGroupsAndCapabilities(t *testing.T) {
	manifest := cliManifest()
	tree := manifest.CLITree()
	if len(tree) != 2 {
		t.Fatalf("roots = %d", len(tree))
	}
	model := tree[0]
	if model.Segment != "model" || model.Command != "keel model" {
		t.Fatalf("model node = %+v", model)
	}
	if model.Capability != nil {
		t.Fatal("model is a group, not a declared capability")
	}
	if len(model.Children) != 1 || model.Children[0].Segment != "provider" {
		t.Fatalf("model children = %v", model.Children)
	}
	provider := model.Children[0]
	if provider.Command != "keel model provider" || len(provider.Children) != 2 {
		t.Fatalf("provider node = %+v", provider)
	}
	if provider.Children[0].Segment != "add" || provider.Children[1].Segment != "list" {
		t.Fatalf("provider children = %v", provider.Children)
	}
	if provider.Children[1].Capability == nil || provider.Children[1].Capability.Danger != DangerRead {
		t.Fatal("list must carry its declaration")
	}

	team := tree[1]
	if team.Segment != "team" || len(team.Children) != 1 {
		t.Fatalf("team node = %+v", team)
	}
	member := team.Children[0]
	if member.Segment != "member" || len(member.Children) != 1 || member.Children[0].Segment != "add" {
		t.Fatalf("member node = %+v", member)
	}

	// A group may itself be a declared capability.
	grouped := &Manifest{Provides: Provides{CLI: []CLICapability{
		{Path: []string{"model", "provider"}, Summary: "provider group", Description: "group"},
		{Path: []string{"model", "provider", "list"}, Summary: "list", Description: "list"},
	}}}
	groupedTree := grouped.CLITree()
	if len(groupedTree) != 1 || len(groupedTree[0].Children) != 1 {
		t.Fatalf("grouped tree = %+v", groupedTree)
	}
	if groupedTree[0].Children[0].Capability == nil {
		t.Fatal("a group that is also a command must carry its declaration")
	}
}

func TestDangerAndCallerClosedSets(t *testing.T) {
	if DangerRead.RequiresConfirmation() {
		t.Fatal("read must not ask for confirmation")
	}
	if !DangerWrite.RequiresConfirmation() || !DangerDestructive.RequiresConfirmation() {
		t.Fatal("write and destructive must ask for confirmation")
	}
	if _, ok := ParseDanger("read"); !ok {
		t.Fatal("read must parse")
	}
	if _, ok := ParseDanger("purge"); ok {
		t.Fatal("purge must not parse")
	}
	if _, ok := ParseCaller("team_captain"); !ok {
		t.Fatal("team_captain must parse")
	}
	if _, ok := ParseCaller("cron"); ok {
		t.Fatal("cron must not parse")
	}
}
