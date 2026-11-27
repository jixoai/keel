package pluginpkg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// commandManifest is the valid baseline for the provides.commands tests.
func commandManifest() *Manifest {
	manifest := validManifest()
	manifest.Provides.Commands = []CommandDecl{{
		Name:        "reindex",
		Summary:     "Rebuild the search index.",
		Description: "Rebuilds the current workspace index; does not change files.",
		ArgsSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
			},
			"required": []any{"path"},
		},
		Tool:       "thing_run",
		Danger:     DangerWrite,
		Idempotent: true,
		Targets:    []SlotTarget{SlotTargetWeb, SlotTargetNative},
	}}
	return manifest
}

func TestManifestCommandsParseAndValidate(t *testing.T) {
	manifest := commandManifest()
	if err := manifest.Validate(testEnv()); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(manifest.Provides.Commands) != 1 {
		t.Fatalf("commands = %+v", manifest.Provides.Commands)
	}
	command := manifest.Provides.Commands[0]
	if command.Name != "reindex" || command.Tool != "thing_run" || command.Danger != DangerWrite || !command.Idempotent {
		t.Fatalf("command = %+v", command)
	}
	if got := command.EffectiveTargets(); len(got) != 2 || got[0] != SlotTargetWeb || got[1] != SlotTargetNative {
		t.Fatalf("targets = %v", got)
	}
	if got := map[string]any(command.EffectiveArgsSchema()); got["type"] != "object" {
		t.Fatalf("schema = %v", got)
	}
}

func TestManifestCommandDefaults(t *testing.T) {
	manifest := validManifest()
	manifest.Provides.Commands = []CommandDecl{{
		Name:    "status",
		Summary: "Show the thing status.",
		Tool:    "thing_run",
		Danger:  DangerRead,
	}}
	if err := manifest.Validate(testEnv()); err != nil {
		t.Fatalf("validate: %v", err)
	}
	command := manifest.Provides.Commands[0]
	if command.Idempotent {
		t.Fatal("idempotent must default to false")
	}
	targets := command.EffectiveTargets()
	if len(targets) != 3 || targets[0] != SlotTargetWeb || targets[1] != SlotTargetNative || targets[2] != SlotTargetTUI {
		t.Fatalf("default targets = %v", targets)
	}
	raw := string(command.ArgsSchemaJSON())
	if raw != `{"type":"object"}` {
		t.Fatalf("default schema = %s", raw)
	}
}

func TestManifestCommandFieldErrors(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Manifest)
		field  string
	}{
		{"name missing", func(m *Manifest) { m.Provides.Commands[0].Name = "" }, "provides.commands[0].name"},
		{"name uppercase", func(m *Manifest) { m.Provides.Commands[0].Name = "Reindex" }, "provides.commands[0].name"},
		{"name whitespace", func(m *Manifest) { m.Provides.Commands[0].Name = " reindex" }, "provides.commands[0].name"},
		{"name starts with digit", func(m *Manifest) { m.Provides.Commands[0].Name = "1reindex" }, "provides.commands[0].name"},
		{"name too long", func(m *Manifest) { m.Provides.Commands[0].Name = "a" + strings.Repeat("b", 32) }, "provides.commands[0].name"},
		{"name duplicate", func(m *Manifest) {
			m.Provides.Commands = append(m.Provides.Commands, m.Provides.Commands[0])
		}, "provides.commands[1].name"},
		{"reserved compact", func(m *Manifest) { m.Provides.Commands[0].Name = "compact" }, "provides.commands[0].name"},
		{"reserved learn", func(m *Manifest) { m.Provides.Commands[0].Name = "learn" }, "provides.commands[0].name"},
		{"summary missing", func(m *Manifest) { m.Provides.Commands[0].Summary = "  " }, "provides.commands[0].summary"},
		{"summary too long", func(m *Manifest) { m.Provides.Commands[0].Summary = strings.Repeat("x", 201) }, "provides.commands[0].summary"},
		{"description too long", func(m *Manifest) { m.Provides.Commands[0].Description = strings.Repeat("x", 2049) }, "provides.commands[0].description"},
		{"tool missing", func(m *Manifest) { m.Provides.Commands[0].Tool = "" }, "provides.commands[0].tool"},
		{"tool foreign", func(m *Manifest) { m.Provides.Commands[0].Tool = "other_run" }, "provides.commands[0].tool"},
		{"tool whitespace", func(m *Manifest) { m.Provides.Commands[0].Tool = " thing_run" }, "provides.commands[0].tool"},
		{"danger missing", func(m *Manifest) { m.Provides.Commands[0].Danger = "" }, "provides.commands[0].danger"},
		{"danger unknown", func(m *Manifest) { m.Provides.Commands[0].Danger = "nuclear" }, "provides.commands[0].danger"},
		{"schema explicitly empty", func(m *Manifest) {
			m.Provides.Commands[0].ArgsSchema = map[string]any{}
		}, "provides.commands[0].args_schema"},
		{"schema unsupported keyword", func(m *Manifest) {
			m.Provides.Commands[0].ArgsSchema = map[string]any{
				"type": "object", "oneOf": []any{map[string]any{"type": "object"}},
			}
		}, "provides.commands[0].args_schema.oneOf"},
		{"schema format keyword", func(m *Manifest) {
			m.Provides.Commands[0].ArgsSchema = map[string]any{
				"type": "object",
				"properties": map[string]any{
					"when": map[string]any{"type": "string", "format": "date-time"},
				},
			}
		}, "provides.commands[0].args_schema.properties.when.format"},
		{"schema additionalProperties true", func(m *Manifest) {
			m.Provides.Commands[0].ArgsSchema = map[string]any{
				"type": "object", "additionalProperties": true,
			}
		}, "provides.commands[0].args_schema.additionalProperties"},
		{"schema bad pattern", func(m *Manifest) {
			m.Provides.Commands[0].ArgsSchema = map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string", "pattern": "("},
				},
			}
		}, "provides.commands[0].args_schema.properties.name.pattern"},
		{"schema required undeclared", func(m *Manifest) {
			m.Provides.Commands[0].ArgsSchema = map[string]any{
				"type": "object", "required": []any{"missing"},
			}
		}, "provides.commands[0].args_schema.required"},
		{"schema not object", func(m *Manifest) {
			m.Provides.Commands[0].ArgsSchema = map[string]any{"type": "string"}
		}, "provides.commands[0].args_schema"},
		{"schema too large", func(m *Manifest) {
			m.Provides.Commands[0].ArgsSchema = map[string]any{
				"type":        "object",
				"description": strings.Repeat("x", MaxCommandArgsSchemaBytes),
			}
		}, "provides.commands[0].args_schema"},
		{"target unknown", func(m *Manifest) {
			m.Provides.Commands[0].Targets = []SlotTarget{"web", "android"}
		}, "provides.commands[0].targets[1]"},
		{"targets explicitly empty", func(m *Manifest) {
			m.Provides.Commands[0].Targets = []SlotTarget{}
		}, "provides.commands[0].targets"},
		{"target duplicate", func(m *Manifest) {
			m.Provides.Commands[0].Targets = []SlotTarget{"web", "web"}
		}, "provides.commands[0].targets[1]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := commandManifest()
			tc.mutate(manifest)
			err := manifest.Validate(testEnv())
			requireField(t, err, tc.field)
		})
	}
}

func TestManifestCommandUnknownFieldRejected(t *testing.T) {
	raw := []byte(`
id: vendor.thing
version: 1.2.3
kind: in-process
description: A thing.
provides:
  tools: [thing_run]
  commands:
    - name: reindex
      summary: Rebuild.
      tool: thing_run
      danger: write
      target: web
`)
	if _, err := ParseManifest(raw); err == nil {
		t.Fatal("unknown command field was accepted")
	}
}

func TestManifestCommandPublishedNames(t *testing.T) {
	manifest := commandManifest()
	published := manifest.PublishedCommands()
	if len(published) != 1 {
		t.Fatalf("published = %+v", published)
	}
	command := published[0]
	if command.Published != "vendor.thing:reindex" {
		t.Fatalf("published name = %q", command.Published)
	}
	if command.Tool != "thing_run" || command.DeclaredTool != "thing_run" || command.PublishedTool != "thing_run" {
		t.Fatalf("tool spellings = %+v", command)
	}
	pluginID, local, ok := ParseCommandPublishedName(command.Published)
	if !ok || pluginID != "vendor.thing" || local != "reindex" {
		t.Fatalf("parse = %q %q %v", pluginID, local, ok)
	}
	for _, invalid := range []string{"", "nocolon", ":reindex", "vendor.thing:", "Bad ID:reindex", "vendor.thing:Reindex"} {
		if _, _, ok := ParseCommandPublishedName(invalid); ok {
			t.Fatalf("published name %q parsed", invalid)
		}
	}
}

func TestManifestCommandPublishedNameForSidecarTool(t *testing.T) {
	manifest := commandManifest()
	manifest.Kind = KindSidecar
	manifest.Provides.UI = nil
	manifest.Entry = Entry{Command: []string{"node", "sidecar.js"}}
	manifest.Provides.Tools = []string{"vendor.thing.greet"}
	manifest.Provides.Commands[0].Tool = "vendor.thing.greet"
	if err := manifest.Validate(testEnv()); err != nil {
		t.Fatalf("validate: %v", err)
	}
	published := manifest.PublishedCommands()
	if len(published) != 1 {
		t.Fatalf("published = %+v", published)
	}
	if published[0].Tool != "greet" || published[0].PublishedTool != "vendor_thing_greet" {
		t.Fatalf("tool spellings = %+v", published[0])
	}
}

func TestReservedCommandNames(t *testing.T) {
	for _, name := range []string{"compact", "learn", "skill", "skill:release"} {
		if !ReservedCommandName(name) {
			t.Fatalf("%q must be reserved", name)
		}
	}
	for _, name := range []string{"reindex", "skills", "compactor", "learner"} {
		if ReservedCommandName(name) {
			t.Fatalf("%q must not be reserved", name)
		}
	}
}

// TestCommandManifestFixture loads the checked-in fixture, so the YAML spelling
// of provides.commands is pinned independently of the struct literal.
func TestCommandManifestFixture(t *testing.T) {
	dir := filepath.Join("testdata", "commands")
	pkg := LoadDir(dir, Environment{KeelVersion: "0.1.0"})
	if !pkg.Usable() {
		t.Fatalf("fixture package is not usable: %+v", pkg.Problem)
	}
	manifest := pkg.Manifest
	if len(manifest.Provides.Commands) != 2 {
		t.Fatalf("commands = %+v", manifest.Provides.Commands)
	}
	if manifest.Provides.Commands[0].Name != "reindex" ||
		manifest.Provides.Commands[0].Danger != DangerWrite ||
		len(manifest.Provides.Commands[0].Targets) != 3 {
		t.Fatalf("first command = %+v", manifest.Provides.Commands[0])
	}
	if manifest.Provides.Commands[1].Name != "purge" || manifest.Provides.Commands[1].Danger != DangerDestructive {
		t.Fatalf("second command = %+v", manifest.Provides.Commands[1])
	}
	if _, err := os.Stat(filepath.Join(dir, PluginFileName)); err != nil {
		t.Fatalf("fixture manifest: %v", err)
	}
}

func TestManifestReservedPluginID(t *testing.T) {
	manifest := commandManifest()
	manifest.ID = "skill"
	err := manifest.Validate(testEnv())
	requireField(t, err, "id")
	if !ReservedPluginID("skill") || ReservedPluginID("skills") {
		t.Fatal("reserved plugin id grammar is wrong")
	}
}
