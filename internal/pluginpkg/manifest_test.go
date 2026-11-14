package pluginpkg

import (
	"errors"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/modelprovider"
)

func testEnv() Environment {
	known := map[string]bool{
		"turn.started": true, "turn.finished": true, "trigger.after": true,
	}
	return Environment{
		KeelVersion:    "0.1.0",
		KnownHookEvent: func(event string) bool { return known[event] },
	}
}

// validManifest is the baseline the rejection tests mutate one field at a time.
func validManifest() *Manifest {
	return &Manifest{
		ID:          "vendor.thing",
		Version:     "1.2.3",
		Kind:        KindInProcess,
		Description: "A thing.",
		Engines:     Engines{Keel: ">=0.1.0 <1.0.0"},
		Provides: Provides{
			Tools:    []string{"thing_run"},
			Hooks:    []string{"turn.finished"},
			Triggers: []string{"vendor.thing.tick"},
			CLI: []CLICapability{{
				Path:         []string{"thing", "run"},
				Summary:      "Run the thing.",
				Description:  "Runs the thing once; no retries on failure.",
				ArgsSchema:   map[string]any{"type": "object"},
				OutputSchema: map[string]any{"type": "object"},
				Permissions:  []string{"thing.write"},
				Danger:       DangerWrite,
				CallableBy:   []Caller{CallerUI, CallerAgent},
				Examples:     []string{"keel thing run"},
			}},
			UI: &UI{Entry: "ui/index.html", Title: "Thing"},
		},
		Requires:    []string{"keel.fs"},
		Permissions: Permissions{FS: []string{"workspace-write"}, Net: []string{"api.example.com"}, Secrets: []string{"THING_TOKEN"}, Tools: []string{"thing_run"}},
		Source:      Source{Type: SourceLocal},
	}
}

func fieldNames(err error) []string {
	var out []string
	for _, field := range FieldErrors(err) {
		out = append(out, field.Field)
	}
	return out
}

func requireField(t *testing.T, err error, field string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a field error for %q, got nil", field)
	}
	for _, got := range fieldNames(err) {
		if got == field {
			return
		}
	}
	t.Fatalf("expected a field error for %q, got %v", field, fieldNames(err))
}

func TestManifestValidateAcceptsSpecExample(t *testing.T) {
	// The sample from the spec, verbatim in shape.
	raw := []byte(`
id: vendor.name
version: 1.2.3
kind: sidecar
description: one line
engines: { keel: ">=0.1.0 <0.3.0" }
provides:
  tools: [name]
  hooks: [turn.started]
  triggers: [vendor.name.tick]
requires: [keel.fs]
permissions: { fs: [workspace-write], net: [api.example.com], secrets: [TOKEN_NAME], tools: [name] }
entry: { command: [node, sidecar.js], args: [--stdio], env: [HOME] }
source: { type: local, path: /tmp/vendor.name }
`)
	manifest, err := ParseManifest(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := manifest.Validate(testEnv()); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if manifest.Kind != KindSidecar || manifest.ParsedVersion.String() != "1.2.3" {
		t.Fatalf("manifest = %+v", manifest)
	}
}

func TestManifestValidateFieldErrors(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Manifest)
		field  string
	}{
		{"id missing", func(m *Manifest) { m.ID = "" }, "id"},
		{"id uppercase", func(m *Manifest) { m.ID = "Vendor.Thing" }, "id"},
		{"id one char", func(m *Manifest) { m.ID = "a" }, "id"},
		{"id leading dot", func(m *Manifest) { m.ID = ".vendor" }, "id"},
		{"version missing", func(m *Manifest) { m.Version = "" }, "version"},
		{"version two parts", func(m *Manifest) { m.Version = "1.2" }, "version"},
		{"version leading zero", func(m *Manifest) { m.Version = "1.02.3" }, "version"},
		{"version prefixed v", func(m *Manifest) { m.Version = "v1.2.3" }, "version"},
		{"kind missing", func(m *Manifest) { m.Kind = "" }, "kind"},
		{"kind unknown", func(m *Manifest) { m.Kind = "wasm" }, "kind"},
		{"description missing", func(m *Manifest) { m.Description = "  " }, "description"},
		{"engines bad range", func(m *Manifest) { m.Engines.Keel = "not-a-range" }, "engines.keel"},
		{"tool empty", func(m *Manifest) { m.Provides.Tools = []string{""} }, "provides.tools[0]"},
		{"tool duplicated", func(m *Manifest) { m.Provides.Tools = []string{"x", "x"} }, "provides.tools[1]"},
		{"tool bad char", func(m *Manifest) { m.Provides.Tools = []string{"x y"} }, "provides.tools[0]"},
		{"hook empty", func(m *Manifest) { m.Provides.Hooks = []string{""} }, "provides.hooks[0]"},
		{"hook outside closed set", func(m *Manifest) { m.Provides.Hooks = []string{"nope.event"} }, "provides.hooks[0]"},
		{"hook duplicated", func(m *Manifest) { m.Provides.Hooks = []string{"turn.started", "turn.started"} }, "provides.hooks[1]"},
		{"trigger empty", func(m *Manifest) { m.Provides.Triggers = []string{""} }, "provides.triggers[0]"},
		{"trigger duplicated", func(m *Manifest) { m.Provides.Triggers = []string{"t", "t"} }, "provides.triggers[1]"},
		{"cli path empty", func(m *Manifest) { m.Provides.CLI[0].Path = nil }, "provides.cli[0].path"},
		{"cli danger unknown", func(m *Manifest) { m.Provides.CLI[0].Danger = "purge" }, "provides.cli[0].danger"},
		{"cli path duplicated", func(m *Manifest) { m.Provides.CLI = append(m.Provides.CLI, m.Provides.CLI[0]) }, "provides.cli[1].path"},
		{"cli description empty", func(m *Manifest) { m.Provides.CLI[0].Description = " " }, "provides.cli[0].description"},
		{"ui entry escaping", func(m *Manifest) { m.Provides.UI = &UI{Entry: "../outside.html"} }, "provides.ui.entry"},
		{"ui entry absolute", func(m *Manifest) { m.Provides.UI = &UI{Entry: "/etc/passwd"} }, "provides.ui.entry"},
		{"webview without ui", func(m *Manifest) {
			m.Kind = KindWebview
			m.Provides.UI = nil
		}, "provides.ui"},
		{"requires empty", func(m *Manifest) { m.Requires = []string{""} }, "requires[0]"},
		{"requires duplicated", func(m *Manifest) { m.Requires = []string{"a", "a"} }, "requires[1]"},
		{"requires self", func(m *Manifest) { m.Requires = []string{m.ID} }, "requires[0]"},
		{"fs permission bad", func(m *Manifest) { m.Permissions.FS = []string{"workspace write"} }, "permissions.fs[0]"},
		{"net permission bad", func(m *Manifest) { m.Permissions.Net = []string{"http://x/ y"} }, "permissions.net[0]"},
		{"secret permission bad", func(m *Manifest) { m.Permissions.Secrets = []string{"9bad"} }, "permissions.secrets[0]"},
		{"tool permission bad", func(m *Manifest) { m.Permissions.Tools = []string{"a/b"} }, "permissions.tools[0]"},
		{"in-process command", func(m *Manifest) { m.Entry.Command = []string{"node"} }, "entry.command"},
		{"sidecar without command", func(m *Manifest) { m.Kind = KindSidecar }, "entry.command"},
		{"entry env bad", func(m *Manifest) { m.Entry.Env = []string{"1BAD"} }, "entry.env[0]"},
		{"source type unknown", func(m *Manifest) { m.Source = Source{Type: "ftp"} }, "source.type"},
		{"source git without url", func(m *Manifest) { m.Source = Source{Type: SourceGit} }, "source.url"},
		{"source npm without package", func(m *Manifest) { m.Source = Source{Type: SourceNPM} }, "source.package"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := validManifest()
			if tc.name == "sidecar without command" {
				manifest.Entry = Entry{}
			}
			tc.mutate(manifest)
			err := manifest.Validate(testEnv())
			requireField(t, err, tc.field)
			var validation *ValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("error type = %T", err)
			}
			if validation.Error() == "" {
				t.Fatal("validation error renders empty")
			}
		})
	}
}

func TestManifestValidateEverythingAtOnce(t *testing.T) {
	manifest := &Manifest{Kind: "nope"}
	err := manifest.Validate(Environment{})
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, field := range []string{"id", "version", "kind", "description"} {
		requireField(t, err, field)
	}
	if len(FieldErrors(err)) != 4 {
		t.Fatalf("fields = %v", fieldNames(err))
	}
}

func TestParseManifestRejectsUnknownAndDuplicateKeys(t *testing.T) {
	if _, err := ParseManifest([]byte("id: a.b\nversion: 1.0.0\nkind: in-process\ndescription: d\nbogus: 1\n")); err == nil {
		t.Fatal("unknown field must be rejected")
	}
	if _, err := ParseManifest([]byte("id: a.b\nid: c.d\nversion: 1.0.0\nkind: in-process\ndescription: d\n")); err == nil {
		t.Fatal("duplicate key must be rejected")
	}
	if _, err := ParseManifest([]byte("id: a.b\nversion: 1.0.0\nkind: in-process\ndescription: d\n---\nid: x.y\n")); err == nil {
		t.Fatal("a second document must be rejected")
	}
}

func TestCheckEnginesGate(t *testing.T) {
	manifest := &Manifest{ID: "a.b", Version: "1.0.0", Engines: Engines{Keel: ">=0.2.0 <0.3.0"}}
	if err := manifest.CheckEngines("0.2.5"); err != nil {
		t.Fatalf("0.2.5 should be admitted: %v", err)
	}
	err := manifest.CheckEngines("0.3.0")
	requireField(t, err, "engines.keel")
	if !strings.Contains(err.Error(), ">=0.2.0 <0.3.0") || !strings.Contains(err.Error(), "0.3.0") {
		t.Fatalf("gate message must name the range and the host version: %v", err)
	}
	open := &Manifest{ID: "a.b", Version: "1.0.0"}
	if err := open.CheckEngines("99.0.0"); err != nil {
		t.Fatalf("a manifest without engines.keel is compatible with any host: %v", err)
	}
	dev := &Manifest{ID: "a.b", Version: "1.0.0", Engines: Engines{Keel: ">=0.1.0 <1.0.0"}}
	if err := dev.CheckEngines("0.2.0-dev.1"); err != nil {
		t.Fatalf("a dev build of an admitted release must pass: %v", err)
	}
	prerelease := &Manifest{ID: "a.b", Version: "1.0.0", Engines: Engines{Keel: ">=0.3.0-0 <0.3.0"}}
	if err := prerelease.CheckEngines("0.3.0-rc.1"); err != nil {
		t.Fatalf("an explicit prerelease range must admit its prerelease: %v", err)
	}
	if err := prerelease.CheckEngines("0.3.0"); err == nil {
		t.Fatal("an explicit prerelease range must not admit the release")
	}
}

func TestPluginIDGrammar(t *testing.T) {
	cases := map[string]bool{
		"keel.example":      true,
		"vendor.panel-ui_1": true,
		"a1":                true,
		"Keel":              false,
		"a":                 false,
		"-a":                false,
		"a/b":               false,
		"":                  false,
	}
	for id, want := range cases {
		if got := ValidatePluginID(id); got != want {
			t.Fatalf("ValidatePluginID(%q) = %v, want %v", id, got, want)
		}
	}
}

// TestManifestProviderExtensionSurface is the spec-shaped manifest half of the
// provider contract: `provides.provider_types`, `provides.provider` and
// `provides.feature_ports` (plus `provides.agents`/`provides.agent_teams`) are
// known fields, decode with the spec's snake_case names, and validate with
// field-level reasons. Before this, KnownFields(true) rejected every manifest
// written to the spec.
func TestManifestProviderExtensionSurface(t *testing.T) {
	raw := []byte(`
id: vendor.media
version: 1.0.0
kind: sidecar
description: Media provider plugin.
entry: { command: [node, sidecar.js] }
provides:
  provider:
    layer: provider
    feature_ports: [image.generate, video.generate]
  feature_ports: [image.generate]
  provider_types:
    - type_id: image
      display_name: Image
      icon: image
      capabilities: [image.generate]
      config_schema: { type: object }
      auth_schema:
        fields:
          - { name: apiKey, label: API key, secret: true, required: true }
      model_schema: { type: object }
      parameters:
        defaults: { steps: 30 }
      feature_ports: [image.generate]
  agents: [agent_media_lead]
  agent_teams: [team_media]
`)
	manifest, err := ParseManifest(raw)
	if err != nil {
		t.Fatalf("a spec-shaped provider manifest must parse: %v", err)
	}
	if err := manifest.Validate(testEnv()); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := len(manifest.Provides.ProviderTypes); got != 1 {
		t.Fatalf("provider_types = %d, want 1", got)
	}
	declaration := manifest.Provides.ProviderTypes[0]
	if declaration.TypeID != "image" || declaration.DisplayName != "Image" {
		t.Fatalf("declaration = %+v", declaration)
	}
	if declaration.AuthSchema == nil || len(declaration.AuthSchema.Fields) != 1 || !declaration.AuthSchema.Fields[0].Secret {
		t.Fatalf("auth_schema did not decode: %+v", declaration.AuthSchema)
	}
	if manifest.Provides.Provider == nil || manifest.Provides.Provider.Layer != ProviderPluginLayer {
		t.Fatalf("provides.provider did not decode: %+v", manifest.Provides.Provider)
	}

	// The manifest id fills the declaring plugin for assembly.
	types := manifest.ProviderTypes()
	if len(types) != 1 || types[0].PluginID != "vendor.media" {
		t.Fatalf("ProviderTypes() = %+v, want the manifest id as plugin", types)
	}
	if types[0].Parameters.Defaults["steps"] != 30 {
		t.Fatalf("parameter defaults = %v", types[0].Parameters.Defaults)
	}
}

// TestManifestProviderExtensionSurfaceRejectsBrokenDeclarations covers the
// field-level failures: a declaration missing a required schema, a provider
// plugin without its layer, a type owned by another plugin, and malformed
// feature ports / agent ids.
func TestManifestProviderExtensionSurfaceRejectsBrokenDeclarations(t *testing.T) {
	base := func() *Manifest {
		manifest := validManifest()
		manifest.Provides.Provider = &ProviderPluginDecl{Layer: ProviderPluginLayer}
		manifest.Provides.ProviderTypes = []modelprovider.ProviderType{{
			TypeID:       "image",
			DisplayName:  "Image",
			Capabilities: modelprovider.Capabilities{"image.generate"},
			ConfigSchema: modelprovider.SchemaRef{Inline: []byte(`{"type":"object"}`)},
			AuthSchema:   &modelprovider.AuthSchema{},
			ModelSchema:  modelprovider.SchemaRef{Inline: []byte(`{"type":"object"}`)},
		}}
		return manifest
	}
	if err := base().Validate(testEnv()); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*Manifest)
		field  string
	}{
		{"config schema missing", func(m *Manifest) {
			m.Provides.ProviderTypes[0].ConfigSchema = modelprovider.SchemaRef{}
		}, "provides.provider_types[0].config_schema"},
		{"auth schema missing", func(m *Manifest) {
			m.Provides.ProviderTypes[0].AuthSchema = nil
		}, "provides.provider_types[0].auth_schema"},
		{"model schema missing", func(m *Manifest) {
			m.Provides.ProviderTypes[0].ModelSchema = modelprovider.SchemaRef{}
		}, "provides.provider_types[0].model_schema"},
		{"type without provider declaration", func(m *Manifest) {
			m.Provides.Provider = nil
		}, "provides.provider"},
		{"provider without types", func(m *Manifest) {
			m.Provides.ProviderTypes = nil
		}, "provides.provider_types"},
		{"wrong layer", func(m *Manifest) {
			m.Provides.Provider.Layer = "tool"
		}, "provides.provider.layer"},
		{"foreign plugin id", func(m *Manifest) {
			m.Provides.ProviderTypes[0].PluginID = "vendor.other"
		}, "provides.provider_types[0].plugin_id"},
		{"bad feature port", func(m *Manifest) {
			m.Provides.FeaturePorts = []string{"Image Generate"}
		}, "provides.feature_ports[0]"},
		{"bad provider feature port", func(m *Manifest) {
			m.Provides.Provider.FeaturePorts = []string{"image generate"}
		}, "provides.provider.feature_ports[0]"},
		{"duplicate agent", func(m *Manifest) {
			m.Provides.Agents = []string{"agent_a", "agent_a"}
		}, "provides.agents[1]"},
		{"bad team id", func(m *Manifest) {
			m.Provides.AgentTeams = []string{"bad team"}
		}, "provides.agent_teams[0]"},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			manifest := base()
			testCase.mutate(manifest)
			requireField(t, manifest.Validate(testEnv()), testCase.field)
		})
	}
}
