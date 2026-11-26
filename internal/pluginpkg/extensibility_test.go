package pluginpkg

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// slotCatalogEnv is a host environment that knows the first-party keel.ui
// slots, so contributions to them are validated against the declaring plugin's
// allowed forms.
func slotCatalogEnv() Environment {
	slots := map[string]SlotDecl{
		"keel.ui.route": {
			ID: "keel.ui.route", Forms: []SlotForm{SlotFormChunk, SlotFormPackage, SlotFormIframe},
			Arity: SlotArityMulti, Targets: []SlotTarget{SlotTargetWeb, SlotTargetNative, SlotTargetTUI},
		},
		"keel.ui.settings-section": {
			ID: "keel.ui.settings-section", Forms: []SlotForm{SlotFormChunk},
			Arity: SlotArityOrdered, Targets: []SlotTarget{SlotTargetWeb, SlotTargetNative},
		},
	}
	return Environment{KnownSlot: func(id string) (SlotDecl, bool) {
		slot, ok := slots[id]
		return slot, ok
	}}
}

func validUIManifest() *Manifest {
	return &Manifest{
		ID:          "vendor.panel",
		Version:     "1.0.0",
		Kind:        KindWebview,
		Layer:       LayerUI,
		Description: "A panel with a native card and a settings section.",
		Provides: Provides{
			Slots: []SlotDecl{{
				ID:      "vendor.panel.card",
				Forms:   []SlotForm{SlotFormChunk, SlotFormData},
				Arity:   SlotArityMulti,
				Targets: []SlotTarget{SlotTargetWeb, SlotTargetNative, SlotTargetTUI},
				Schema:  map[string]any{"type": "object", "properties": map[string]any{"title": map[string]any{"type": "string"}}},
			}},
		},
		Contributes: []Contribution{
			{Slot: "vendor.panel.card", Form: SlotFormChunk, Entry: "ui/card.js"},
			{Slot: "vendor.panel.card", Form: SlotFormData, Payload: map[string]any{"title": "Card"}},
			{Slot: "keel.ui.settings-section", Form: SlotFormChunk, Entry: "ui/settings.js"},
		},
		Extensibility: &Extensibility{Doc: ExtensibilityDocFileName, Schema: ExtensibilityFileName},
	}
}

func TestManifestExtensionSurfaceAcceptsValidUIContribution(t *testing.T) {
	manifest := validUIManifest()
	if err := manifest.Validate(slotCatalogEnv()); err != nil {
		t.Fatalf("a valid UI contribution must validate: %v", err)
	}
	if got := manifest.ResolvedLayer(); got != LayerUI {
		t.Fatalf("ResolvedLayer() = %q, want %q", got, LayerUI)
	}
}

func TestReservedSlotIDsAreNamespacedAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, id := range ReservedSlotIDs() {
		if !slotIDPattern.MatchString(id) || !strings.Contains(id, ".") {
			t.Errorf("reserved slot id %q is not namespaced", id)
		}
		if seen[id] {
			t.Errorf("reserved slot id %q is declared twice", id)
		}
		seen[id] = true
	}
	for _, required := range []string{
		"keel.ui.route", "keel.ui.nav", "keel.ui.action", "keel.ui.tool-renderer",
		"keel.ui.message-part", "keel.ui.sidebar", "keel.ui.settings-section", "keel.ui.surface",
	} {
		if !seen[required] {
			t.Errorf("the frozen first-party slot set is missing %q", required)
		}
	}
}

func TestManifestLayerDerivation(t *testing.T) {
	cases := []struct {
		kind Kind
		want Layer
	}{
		{KindInProcess, LayerHarness},
		{KindSidecar, LayerProvider},
		{KindWebview, LayerUI},
	}
	for _, testCase := range cases {
		manifest := &Manifest{Kind: testCase.kind}
		if got := manifest.ResolvedLayer(); got != testCase.want {
			t.Fatalf("kind %q derives layer %q, want %q", testCase.kind, got, testCase.want)
		}
	}
}

func TestManifestExtensionSurfaceRejectsBrokenSlots(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Manifest)
		field  string
	}{
		{"bare slot id", func(m *Manifest) {
			m.Provides.Slots[0].ID = "card"
		}, "provides.slots[0].id"},
		{"foreign namespace", func(m *Manifest) {
			m.Provides.Slots[0].ID = "vendor.other.card"
		}, "provides.slots[0].id"},
		{"duplicate slot id", func(m *Manifest) {
			m.Provides.Slots = append(m.Provides.Slots, m.Provides.Slots[0])
		}, "provides.slots[1].id"},
		{"unknown form", func(m *Manifest) {
			m.Provides.Slots[0].Forms = []SlotForm{"html"}
		}, "provides.slots[0].forms[0]"},
		{"no forms", func(m *Manifest) {
			m.Provides.Slots[0].Forms = nil
		}, "provides.slots[0].forms"},
		{"duplicate form", func(m *Manifest) {
			m.Provides.Slots[0].Forms = []SlotForm{SlotFormChunk, SlotFormChunk}
		}, "provides.slots[0].forms[1]"},
		{"unknown arity", func(m *Manifest) {
			m.Provides.Slots[0].Arity = "many"
		}, "provides.slots[0].arity"},
		{"unknown target", func(m *Manifest) {
			m.Provides.Slots[0].Targets = []SlotTarget{"mobile"}
		}, "provides.slots[0].targets[0]"},
		{"no targets", func(m *Manifest) {
			m.Provides.Slots[0].Targets = nil
		}, "provides.slots[0].targets"},
		{"tui-only iframe slot", func(m *Manifest) {
			m.Provides.Slots[0].Forms = []SlotForm{SlotFormIframe}
			m.Provides.Slots[0].Targets = []SlotTarget{SlotTargetTUI}
			m.Provides.Slots[0].Schema = nil
			m.Contributes = []Contribution{{Slot: "vendor.panel.card", Form: SlotFormIframe, Entry: "ui/card.html"}}
			m.Provides.UI = &UI{Entry: "ui/index.html"}
		}, "provides.slots[0].targets"},
		{"data without schema", func(m *Manifest) {
			m.Provides.Slots[0].Schema = nil
		}, "provides.slots[0].schema"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			manifest := validUIManifest()
			testCase.mutate(manifest)
			requireField(t, manifest.Validate(slotCatalogEnv()), testCase.field)
		})
	}
}

func TestManifestExtensionSurfaceRejectsBrokenContributions(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Manifest)
		field  string
	}{
		{"unknown target slot", func(m *Manifest) {
			m.Contributes[0].Slot = "keel.ui.not-a-slot"
		}, "contributes[0].slot"},
		{"bare target slot", func(m *Manifest) {
			m.Contributes[0].Slot = "route"
		}, "contributes[0].slot"},
		{"form not allowed by slot", func(m *Manifest) {
			m.Contributes[2].Form = SlotFormIframe
			m.Provides.UI = &UI{Entry: "ui/index.html"}
		}, "contributes[2].form"},
		{"chunk without entry", func(m *Manifest) {
			m.Contributes[0].Entry = ""
		}, "contributes[0].entry"},
		{"chunk with payload", func(m *Manifest) {
			m.Contributes[0].Payload = map[string]any{"x": "y"}
		}, "contributes[0].payload"},
		{"data without payload", func(m *Manifest) {
			m.Contributes[1].Payload = nil
		}, "contributes[1].payload"},
		{"data with entry", func(m *Manifest) {
			m.Contributes[1].Entry = "ui/data.json"
		}, "contributes[1].entry"},
		{"entry escapes package", func(m *Manifest) {
			m.Contributes[0].Entry = "../escape.js"
		}, "contributes[0].entry"},
		{"duplicate contribution", func(m *Manifest) {
			m.Contributes = append(m.Contributes, m.Contributes[0])
		}, "contributes[3]"},
		{"iframe without ui entry", func(m *Manifest) {
			m.Contributes[0] = Contribution{Slot: "keel.ui.route", Form: SlotFormIframe, Entry: "ui/page.html"}
		}, "contributes[0].entry"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			manifest := validUIManifest()
			testCase.mutate(manifest)
			requireField(t, manifest.Validate(slotCatalogEnv()), testCase.field)
		})
	}
}

func TestManifestPureChunkWebviewNeedsNoUIEntry(t *testing.T) {
	manifest := validUIManifest()
	manifest.Contributes = []Contribution{{Slot: "vendor.panel.card", Form: SlotFormChunk, Entry: "ui/card.js"}}
	manifest.Provides.Slots[0].Forms = []SlotForm{SlotFormChunk}
	manifest.Provides.Slots[0].Schema = nil
	if err := manifest.Validate(slotCatalogEnv()); err != nil {
		t.Fatalf("a pure chunk webview package must validate without provides.ui: %v", err)
	}
	empty := validUIManifest()
	empty.Contributes = nil
	empty.Provides.Slots = nil
	requireField(t, empty.Validate(slotCatalogEnv()), "provides.ui")
}

func TestManifestLayerKindConstraints(t *testing.T) {
	cases := []struct {
		name   string
		layer  Layer
		kind   Kind
		field  string
		accept bool
	}{
		{"kernel in process", LayerKernel, KindInProcess, "", true},
		{"kernel sidecar", LayerKernel, KindSidecar, "layer", false},
		{"service sidecar", LayerService, KindSidecar, "", true},
		{"service in process", LayerService, KindInProcess, "", true},
		{"service webview", LayerService, KindWebview, "layer", false},
		{"provider sidecar", LayerProvider, KindSidecar, "", true},
		{"provider in process", LayerProvider, KindInProcess, "layer", false},
		{"ui webview", LayerUI, KindWebview, "", true},
		{"ui in process", LayerUI, KindInProcess, "layer", false},
		{"harness sidecar", LayerHarness, KindSidecar, "", true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			manifest := &Manifest{
				ID: "vendor.thing", Version: "1.0.0", Kind: testCase.kind,
				Layer: testCase.layer, Description: "d",
			}
			if testCase.kind == KindSidecar {
				manifest.Entry.Command = []string{"node", "sidecar.js"}
			}
			if testCase.kind == KindWebview {
				manifest.Contributes = []Contribution{{Slot: "keel.ui.route", Form: SlotFormChunk, Entry: "ui/panel.js"}}
			}
			err := manifest.Validate(Environment{})
			if testCase.accept {
				if err != nil {
					t.Fatalf("layer %q with kind %q must validate: %v", testCase.layer, testCase.kind, err)
				}
				return
			}
			requireField(t, err, testCase.field)
		})
	}
	unknown := &Manifest{ID: "vendor.thing", Version: "1.0.0", Kind: KindInProcess, Layer: "tool", Description: "d"}
	requireField(t, unknown.Validate(Environment{}), "layer")
}

func TestManifestBackends(t *testing.T) {
	manifest := &Manifest{
		ID: "vendor.thing", Version: "1.0.0", Kind: KindInProcess, Description: "d",
		Provides: Provides{Backends: []BackendDecl{{Tool: "skills", Action: "view", Impl: "view"}}},
	}
	known := Environment{KnownBackend: func(tool, action string) bool {
		return tool == "skills" && action == "view"
	}}
	if err := manifest.Validate(known); err != nil {
		t.Fatalf("a known backend action must validate: %v", err)
	}
	rejecting := Environment{KnownBackend: func(tool, action string) bool { return false }}
	requireField(t, manifest.Validate(rejecting), "provides.backends[0].action")
	manifest.Provides.Backends[0].Action = "invented"
	requireField(t, manifest.Validate(known), "provides.backends[0].action")
}

func TestManifestRequiresExtensibilityForOfficialPackages(t *testing.T) {
	manifest := validUIManifest()
	manifest.Extensibility = nil
	requireField(t, manifest.Validate(Environment{RequireExtensibility: true}), "extensibility")
	if err := manifest.Validate(Environment{}); err != nil {
		t.Fatalf("a third-party package may omit the docs: %v", err)
	}
	partial := validUIManifest()
	partial.Extensibility = &Extensibility{Doc: ExtensibilityDocFileName}
	requireField(t, partial.Validate(Environment{}), "extensibility.schema")
	escaping := validUIManifest()
	escaping.Extensibility = &Extensibility{Doc: "../EXTENSIBILITY.md", Schema: ExtensibilityFileName}
	requireField(t, escaping.Validate(Environment{}), "extensibility.doc")
}

func TestLoadDirChecksExtensionFiles(t *testing.T) {
	dir := t.TempDir()
	manifest := `id: vendor.panel
version: 1.0.0
kind: webview
layer: ui
description: A panel.
contributes:
  - { slot: keel.ui.settings-section, form: chunk, entry: ui/settings.js }
extensibility: { doc: EXTENSIBILITY.md, schema: extensibility.json }
`
	if err := os.WriteFile(filepath.Join(dir, PluginFileName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "ui"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ui", "settings.js"), []byte("export {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ExtensibilityDocFileName), []byte("# Panel\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg := LoadDir(dir, Environment{})
	if pkg.Usable() {
		t.Fatal("a manifest naming missing files must not load")
	}
	fields := fieldNamesFromProblem(pkg)
	if !containsString(fields, "extensibility.schema") {
		t.Fatalf("missing-file fields = %v", fields)
	}
	if err := os.WriteFile(filepath.Join(dir, ExtensibilityFileName), []byte(validDocJSON(t, "vendor.panel")), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg = LoadDir(dir, Environment{})
	if !pkg.Usable() {
		t.Fatalf("a complete package must load: %+v", pkg.Problem)
	}
}

func fieldNamesFromProblem(pkg Package) []string {
	var out []string
	if pkg.Problem == nil {
		return out
	}
	for _, field := range pkg.Problem.Fields {
		out = append(out, field.Field)
	}
	return out
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func validDocJSON(t *testing.T, plugin string) string {
	t.Helper()
	return `{
  "schema": "keel.plugin-extensibility/1",
  "plugin": "` + plugin + `",
  "version": "1.0.0",
  "summary": "A panel.",
  "slots": [],
  "contributes": [ { "slot": "keel.ui.settings-section", "form": "chunk", "entry": "ui/settings.js" } ],
  "configuration": [],
  "hooks": [],
  "providers": [],
  "examples": [ { "title": "Open", "description": "Open the panel." } ],
  "limits": [ { "name": "max_items", "value": "100", "description": "Bounded list." } ]
}`
}

func TestLoadExtensibilityFromPackage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ExtensibilityFileName)
	if err := os.WriteFile(path, []byte(validDocJSON(t, "vendor.panel")), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, fields, err := LoadExtensibility(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(fields) != 0 {
		t.Fatalf("fields = %v", fields)
	}
	if doc == nil || doc.Plugin != "vendor.panel" {
		t.Fatalf("doc = %+v", doc)
	}
	empty := t.TempDir()
	doc, fields, err = LoadExtensibility(empty)
	if err != nil || doc != nil || fields != nil {
		t.Fatalf("a package without the document must return (nil,nil,nil), got (%v,%v,%v)", doc, fields, err)
	}
}

func TestExtensibilityDocValidate(t *testing.T) {
	doc, err := ParseExtensibility([]byte(validDocJSON(t, "vendor.panel")))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("a complete document must validate: %v", err)
	}
}

func TestExtensibilityDocRejectsUnknownFieldsAndMissingArrays(t *testing.T) {
	if _, err := ParseExtensibility([]byte(`{"schema":"keel.plugin-extensibility/1","bogus":1}`)); err == nil {
		t.Fatal("an unknown field must be rejected")
	}
	raw := strings.Replace(validDocJSON(t, "vendor.panel"), `"slots": [],`, "", 1)
	doc, err := ParseExtensibility([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	requireField(t, doc.Validate(), "slots")
}

func TestExtensibilityDocRejectsBrokenConfiguration(t *testing.T) {
	base := func() *ExtensibilityDoc {
		doc, err := ParseExtensibility([]byte(validDocJSON(t, "vendor.panel")))
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	cases := []struct {
		name   string
		mutate func(*ExtensibilityDoc)
		field  string
	}{
		{"wrong schema", func(d *ExtensibilityDoc) { d.Schema = "keel.plugin-extensibility/2" }, "schema"},
		{"bad plugin", func(d *ExtensibilityDoc) { d.Plugin = "Vendor" }, "plugin"},
		{"bad version", func(d *ExtensibilityDoc) { d.Version = "1.0" }, "version"},
		{"empty summary", func(d *ExtensibilityDoc) { d.Summary = "  " }, "summary"},
		{"bad config key", func(d *ExtensibilityDoc) {
			d.Configuration = []ExtConfig{{Key: "timeout", Type: "string", Description: "d"}}
		}, "configuration[0].key"},
		{"bad config type", func(d *ExtensibilityDoc) {
			d.Configuration = []ExtConfig{{Key: "TIMEOUT", Type: "duration", Description: "d"}}
		}, "configuration[0].type"},
		{"duplicate config", func(d *ExtensibilityDoc) {
			d.Configuration = []ExtConfig{
				{Key: "TIMEOUT", Type: "string", Description: "d"},
				{Key: "TIMEOUT", Type: "string", Description: "d"},
			}
		}, "configuration[1].key"},
		{"bad hook", func(d *ExtensibilityDoc) {
			d.Hooks = []ExtHook{{Event: "bad event", Description: "d"}}
		}, "hooks[0].event"},
		{"bad provider port", func(d *ExtensibilityDoc) {
			d.Providers = []ExtProvider{{Port: "Image Generate", Description: "d"}}
		}, "providers[0].port"},
		{"missing example title", func(d *ExtensibilityDoc) {
			d.Examples = []ExtExample{{Description: "d"}}
		}, "examples[0].title"},
		{"bad limit name", func(d *ExtensibilityDoc) {
			d.Limits = []ExtLimit{{Name: "max items", Value: "1", Description: "d"}}
		}, "limits[0].name"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			doc := base()
			testCase.mutate(doc)
			requireField(t, doc.Validate(), testCase.field)
		})
	}
}

func TestExtensibilityDocMatchesManifest(t *testing.T) {
	manifest := validUIManifest()
	doc := &ExtensibilityDoc{
		Schema: ExtensibilitySchema, Plugin: manifest.ID, Version: manifest.Version,
		Summary: "A panel.", Slots: manifest.Provides.Slots, Contributes: manifest.Contributes,
		Configuration: []ExtConfig{}, Hooks: []ExtHook{}, Providers: []ExtProvider{},
		Examples: []ExtExample{}, Limits: []ExtLimit{},
	}
	if err := doc.ValidateAgainstManifest(manifest); err != nil {
		t.Fatalf("matching documents must validate: %v", err)
	}
	doc.Contributes = nil
	requireField(t, doc.ValidateAgainstManifest(manifest), "contributes")
	doc.Contributes = manifest.Contributes
	doc.Version = "2.0.0"
	requireField(t, doc.ValidateAgainstManifest(manifest), "version")
	doc.Version = manifest.Version
	doc.Plugin = "vendor.other"
	requireField(t, doc.ValidateAgainstManifest(manifest), "plugin")
}

// attachmentDoc returns a document whose sole contribution claims attachment
// rendering for the given MIME patterns.
func attachmentDoc(t *testing.T, mime []string) *ExtensibilityDoc {
	t.Helper()
	doc, err := ParseExtensibility([]byte(validDocJSON(t, "keel.attachment.test")))
	if err != nil {
		panic(err)
	}
	doc.Contributes = []Contribution{{
		Slot: MessagePartSlotID, Form: SlotFormChunk, Entry: "ui/pdf-preview.js",
		Part: AttachmentPartKind, Mime: mime,
	}}
	return doc
}

// TestExtensibilityDocValidatesAttachmentClaims pins the claim contract: only
// the message-part slot may carry `part`/`mime`, only `part: attachment` is
// defined, and MIME patterns are lowercase `type/subtype` or `type/*`.
func TestExtensibilityDocValidatesAttachmentClaims(t *testing.T) {
	if err := attachmentDoc(t, []string{"application/pdf", "image/*", "application/vnd.api+json", "application/x_custom"}).Validate(); err != nil {
		t.Fatalf("a valid attachment claim must validate: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*ExtensibilityDoc)
		field  string
	}{
		{"part on a foreign slot", func(d *ExtensibilityDoc) {
			d.Contributes[0].Slot = "keel.ui.settings-section"
		}, "contributes[0].part"},
		{"mime on a foreign slot", func(d *ExtensibilityDoc) {
			d.Contributes[0].Slot = "keel.ui.settings-section"
		}, "contributes[0].mime"},
		{"unknown part kind", func(d *ExtensibilityDoc) {
			d.Contributes[0].Part = "thumbnail"
		}, "contributes[0].part"},
		{"mime without the attachment claim", func(d *ExtensibilityDoc) {
			d.Contributes[0].Part = ""
		}, "contributes[0].part"},
		{"uppercase pattern", func(d *ExtensibilityDoc) {
			d.Contributes[0].Mime = []string{"Application/PDF"}
		}, "contributes[0].mime[0]"},
		{"wildcard type", func(d *ExtensibilityDoc) {
			d.Contributes[0].Mime = []string{"*/pdf"}
		}, "contributes[0].mime[0]"},
		{"empty item", func(d *ExtensibilityDoc) {
			d.Contributes[0].Mime = []string{""}
		}, "contributes[0].mime[0]"},
		{"missing subtype", func(d *ExtensibilityDoc) {
			d.Contributes[0].Mime = []string{"application/"}
		}, "contributes[0].mime[0]"},
		{"parameters are not a pattern", func(d *ExtensibilityDoc) {
			d.Contributes[0].Mime = []string{"application/pdf; charset=utf-8"}
		}, "contributes[0].mime[0]"},
		{"too many patterns", func(d *ExtensibilityDoc) {
			patterns := make([]string, MimePatternsMax+1)
			for index := range patterns {
				patterns[index] = fmt.Sprintf("application/x-%d", index)
			}
			d.Contributes[0].Mime = patterns
		}, "contributes[0].mime"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			doc := attachmentDoc(t, []string{"application/pdf"})
			testCase.mutate(doc)
			requireField(t, doc.Validate(), testCase.field)
		})
	}
}

func TestExtensibilityDocRejectsDuplicateMimeClaims(t *testing.T) {
	doc := attachmentDoc(t, []string{"application/pdf"})
	doc.Contributes = append(doc.Contributes, Contribution{
		Slot: MessagePartSlotID, Form: SlotFormChunk, Entry: "ui/other.js",
		Part: AttachmentPartKind, Mime: []string{"image/png", "application/pdf"},
	})
	requireField(t, doc.Validate(), "contributes[1].mime[1]")
}

// TestManifestRejectsAttachmentClaimsInPluginYAML pins the single source: the
// claim fields belong to extensibility.json; plugin.yaml carrying them is a
// field-level refusal (not a merge with two competing spellings).
func TestManifestRejectsAttachmentClaimsInPluginYAML(t *testing.T) {
	manifest := &Manifest{
		ID: "vendor.claims", Version: "1.0.0", Kind: KindWebview, Description: "d",
		Contributes: []Contribution{{
			Slot: MessagePartSlotID, Form: SlotFormChunk, Entry: "ui/preview.js",
			Part: AttachmentPartKind, Mime: []string{"application/pdf"},
		}},
	}
	requireField(t, manifest.Validate(Environment{}), "contributes[0].part")
	manifest.Contributes[0].Part = ""
	manifest.Contributes[0].Mime = []string{"application/pdf"}
	requireField(t, manifest.Validate(Environment{}), "contributes[0].mime")
}

// TestExtensibilityDocMatchesManifestIgnoresClaims: the document narrows the
// manifest's contribution with claims, so the base surface is what must match.
func TestExtensibilityDocMatchesManifestIgnoresClaims(t *testing.T) {
	manifest := validUIManifest()
	doc := &ExtensibilityDoc{
		Schema: ExtensibilitySchema, Plugin: manifest.ID, Version: manifest.Version,
		Summary: "A panel.", Slots: manifest.Provides.Slots,
		Contributes: []Contribution{{
			Slot: manifest.Contributes[2].Slot, Form: manifest.Contributes[2].Form,
			Entry: manifest.Contributes[2].Entry, Part: AttachmentPartKind,
			Mime: []string{"image/*"},
		}},
		Configuration: []ExtConfig{}, Hooks: []ExtHook{}, Providers: []ExtProvider{},
		Examples: []ExtExample{}, Limits: []ExtLimit{},
	}
	// The document carries one contribution while the manifest carries three:
	// that mismatch is still refused.
	requireField(t, doc.ValidateAgainstManifest(manifest), "contributes")
	doc.Contributes = contributionSurface(manifest.Contributes)
	doc.Contributes[2].Part = AttachmentPartKind
	doc.Contributes[2].Mime = []string{"image/*"}
	if err := doc.ValidateAgainstManifest(manifest); err != nil {
		t.Fatalf("claims must not break the surface comparison: %v", err)
	}
	doc.Contributes[2].Entry = "ui/other.js"
	requireField(t, doc.ValidateAgainstManifest(manifest), "contributes")
}
