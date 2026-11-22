package harnessext

// W12 extension declarations: the reader tolerates the manifest's documented
// shape, the validator fails closed on a bad one, and the plugin manager
// carries the declaration through install → inventory → uninstall.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/pluginpkg"
)

func writeExtensionPackage(t *testing.T, id string, extension string) string {
	t.Helper()
	files := map[string]string{
		"plugin.yaml":        webviewManifest(id, "1.0.0", nil, false),
		"ui/index.html":      "<html>panel</html>",
		"ui/panel.js":        "export function render() {}",
		"extensibility.json": extension,
		"EXTENSIBILITY.md":   "# fixture\n",
	}
	return writePluginPackage(t, filepath.Join(t.TempDir(), strings.ReplaceAll(id, ".", "-")), files)
}

const fixtureExtension = `{
  "layer": "ui",
  "slots": [
    {"id": "vendor.panel.widget", "forms": ["chunk", "data"], "arity": "multi", "targets": ["web", "native", "tui"],
     "schema": {"type": "object", "required": ["label"]}}
  ],
  "contributes": [
    {"id": "refresh", "slot": "keel.ui.action", "form": "chunk", "entry": "ui/panel.js",
     "title": "Refresh", "capabilities": ["state", "call"]},
    {"id": "nav", "slot": "keel.ui.nav", "form": "data", "payload": {"label": "Panel"}, "capabilities": ["state"]}
  ],
  "extensibility": {"doc": "EXTENSIBILITY.md", "schema": "extensibility.json"}
}`

func TestDecodeExtensionReadsTheDocumentedShape(t *testing.T) {
	dir := writeExtensionPackage(t, "vendor.panel", fixtureExtension)
	pkg := pluginpkg.LoadDir(dir, pluginpkg.Environment{KeelVersion: testKeelVersion(), KnownHookEvent: ValidHookEvent})
	if !pkg.Usable() {
		t.Fatalf("fixture package is not usable: %+v", pkg.Problem)
	}
	decl := DecodeExtension(pkg.Dir, pkg.Manifest)
	if len(decl.Problems) > 0 {
		t.Fatalf("problems = %+v", decl.Problems)
	}
	if decl.Layer != "ui" {
		t.Fatalf("layer = %q", decl.Layer)
	}
	if len(decl.Slots) != 1 || decl.Slots[0].Plugin != "vendor.panel" {
		t.Fatalf("slots = %+v", decl.Slots)
	}
	if len(decl.Slots[0].Schema) == 0 {
		t.Fatal("the slot schema did not survive the read")
	}
	if len(decl.Contributes) != 2 {
		t.Fatalf("contributions = %+v", decl.Contributes)
	}
	if decl.Contributes[0].Capabilities[0] != "state" || decl.Contributes[0].Entry != "ui/panel.js" {
		t.Fatalf("contribution = %+v", decl.Contributes[0])
	}
	if decl.Contributes[1].Payload == nil {
		t.Fatal("the data payload did not survive the read")
	}
	if decl.Doc == nil || !decl.Doc.Usable || decl.Doc.Schema != ExtensionFileName {
		t.Fatalf("extensibility doc = %+v", decl.Doc)
	}
}

func TestDecodeExtensionKeepsAnonymousContributionsDistinct(t *testing.T) {
	doc := `{"contributes":[
	  {"slot":"keel.ui.action","form":"chunk","entry":"ui/a.js"},
	  {"slot":"keel.ui.action","form":"chunk","entry":"ui/b.js"}
	]}`
	dir := writeExtensionPackage(t, "vendor.anonymous", doc)
	decl := DecodeExtension(dir, nil)
	if len(decl.Problems) > 0 {
		t.Fatalf("problems = %+v", decl.Problems)
	}
	if len(decl.Contributes) != 2 {
		t.Fatalf("anonymous contributions were merged: %+v", decl.Contributes)
	}
}

func TestDecodeExtensionMissingDocumentIsNotAnInstallError(t *testing.T) {
	dir := writePluginPackage(t, filepath.Join(t.TempDir(), "panel"), map[string]string{
		"plugin.yaml":   webviewManifest("vendor.plain", "1.0.0", nil, false),
		"ui/index.html": "<html></html>",
	})
	decl := DecodeExtension(dir, nil)
	if len(decl.Problems) > 0 {
		t.Fatalf("a third-party package without a document must stay installable: %+v", decl.Problems)
	}
	if decl.Doc == nil || decl.Doc.Usable {
		t.Fatalf("doc = %+v", decl.Doc)
	}
}

func TestValidateExtensionFailsClosed(t *testing.T) {
	cases := []struct {
		name  string
		decl  ExtensionDeclaration
		field string
	}{
		{
			name:  "slot id outside the declaring namespace",
			decl:  ExtensionDeclaration{Slots: []ExtensionSlot{{ID: "other.widget", Forms: []string{"chunk"}, Targets: []string{"web"}}}},
			field: "provides.slots[0].id",
		},
		{
			name:  "unknown form",
			decl:  ExtensionDeclaration{Contributes: []ExtensionContribution{{Slot: "keel.ui.action", Form: "webview"}}},
			field: "contributes[0].form",
		},
		{
			name:  "chunk without entry",
			decl:  ExtensionDeclaration{Contributes: []ExtensionContribution{{Slot: "keel.ui.action", Form: "chunk"}}},
			field: "contributes[0].entry",
		},
		{
			name:  "entry escapes the package",
			decl:  ExtensionDeclaration{Contributes: []ExtensionContribution{{Slot: "keel.ui.action", Form: "chunk", Entry: "../x.js"}}},
			field: "contributes[0].entry",
		},
		{
			name:  "data without payload",
			decl:  ExtensionDeclaration{Contributes: []ExtensionContribution{{Slot: "keel.ui.nav", Form: "data"}}},
			field: "contributes[0].payload",
		},
		{
			name:  "undeclared bridge capability",
			decl:  ExtensionDeclaration{Contributes: []ExtensionContribution{{Slot: "keel.ui.action", Form: "chunk", Entry: "x.js", Capabilities: []string{"admin"}}}},
			field: "contributes[0].capabilities[0]",
		},
		{
			name:  "javascript iframe",
			decl:  ExtensionDeclaration{Contributes: []ExtensionContribution{{Slot: "keel.ui.surface", Form: "iframe", Entry: "javascript:alert(1)"}}},
			field: "contributes[0].entry",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := ValidateExtension("vendor.panel", "webview", tc.decl)
			if len(problems) == 0 {
				t.Fatalf("expected a %s problem", tc.field)
			}
			found := false
			for _, problem := range problems {
				if problem.Field == tc.field {
					found = true
				}
			}
			if !found {
				t.Fatalf("problems = %+v, want field %s", problems, tc.field)
			}
		})
	}
}

func TestManagerInstallCarriesContributionAndUninstallLeavesNone(t *testing.T) {
	root := t.TempDir()
	harness := newManagerHarness(t, root, nil)
	ctx := context.Background()
	if err := harness.manager.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	// The checked-in fixture package: the same contribution shape the web
	// fixture (web/src/plugins/fixtures/hello-chunk.tsx) renders.
	source, err := filepath.Abs(filepath.Join("testdata", "w12-ui-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	view, err := harness.manager.Install(ctx, InstallRequest{Path: source}, AuditContext{Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if view.Layer != "ui" {
		t.Fatalf("layer = %q", view.Layer)
	}
	if len(view.Contributes) != 6 {
		t.Fatalf("view surface = %+v", view)
	}
	if view.Extensibility == nil || !view.Extensibility.Usable {
		t.Fatalf("extensibility = %+v", view.Extensibility)
	}
	if _, err := harness.manager.Enable(ctx, "keel.w12.fixture", AuditContext{Actor: "tester"}); err != nil {
		t.Fatal(err)
	}
	inventory := harness.manager.Snapshot()
	if len(inventory.Plugins) != 1 || len(inventory.Plugins[0].Contributes) != 6 {
		t.Fatalf("inventory = %+v", inventory.Plugins)
	}
	if _, err := harness.manager.Disable(ctx, "keel.w12.fixture", AuditContext{Actor: "tester"}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.manager.Uninstall(ctx, "keel.w12.fixture", AuditContext{Actor: "tester"}); err != nil {
		t.Fatal(err)
	}
	if plugins := harness.manager.Snapshot().Plugins; len(plugins) != 0 {
		t.Fatalf("uninstall left residue: %+v", plugins)
	}
	if _, err := os.Stat(pluginpkg.PackageDirFor(root, "keel.w12.fixture")); !os.IsNotExist(err) {
		t.Fatalf("managed directory survived uninstall: %v", err)
	}
}

func TestManagerInstallRefusesABadExtensionDeclaration(t *testing.T) {
	root := t.TempDir()
	harness := newManagerHarness(t, root, nil)
	ctx := context.Background()
	if err := harness.manager.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	bad := `{"contributes":[{"slot":"keel.ui.action","form":"chunk","entry":"../escape.js"}]}`
	source := writeExtensionPackage(t, "vendor.bad", bad)
	_, err := harness.manager.Install(ctx, InstallRequest{Path: source}, AuditContext{Actor: "tester"})
	if !errors.Is(err, ErrPluginInvalid) {
		t.Fatalf("install err = %v", err)
	}
	var pluginErr *PluginError
	if !errors.As(err, &pluginErr) || len(pluginErr.Fields) == 0 {
		t.Fatalf("a bad contribution must be a field-level refusal: %+v", err)
	}
	if harness.manager.Snapshot().Plugins != nil && len(harness.manager.Snapshot().Plugins) != 0 {
		t.Fatalf("a refused install left a row: %+v", harness.manager.Snapshot().Plugins)
	}
}

// The view's JSON must carry the declaration to the client without the raw
// package path or provenance (the same allowlist rule as the bridge).
func TestPluginViewJSONCarriesTheDeclaration(t *testing.T) {
	root := t.TempDir()
	harness := newManagerHarness(t, root, nil)
	ctx := context.Background()
	if err := harness.manager.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	source := writeExtensionPackage(t, "vendor.panel", fixtureExtension)
	if _, err := harness.manager.Install(ctx, InstallRequest{Path: source}, AuditContext{Actor: "tester"}); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(harness.manager.Snapshot().Plugins[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{`"contributes"`, `"slots"`, `"extensibility"`} {
		if !strings.Contains(string(raw), needle) {
			t.Fatalf("view JSON has no %s: %s", needle, raw)
		}
	}
}

var _ = os.ErrNotExist
