package harnessext

// W12 extension declarations: the reader tolerates the manifest's documented
// shape, the validator fails closed on a bad one, and the plugin manager
// carries the declaration through install → inventory → uninstall.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/pluginpkg"
	"github.com/jixoai/keel/internal/store"
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

// attachmentPackage writes a webview package whose plugin.yaml declares the
// message-part contribution and whose extensibility.json carries the claim.
func attachmentPackage(t *testing.T, id, claim string) string {
	t.Helper()
	return attachmentPackageVersion(t, id, "1.0.0", claim)
}

func attachmentPackageVersion(t *testing.T, id, version, claim string) string {
	t.Helper()
	manifest := `id: ` + id + `
version: ` + version + `
kind: webview
description: attachment preview fixture
engines:
  keel: ">=0.1.0 <1.0.0"
provides:
  ui:
    entry: ui/index.html
contributes:
  - { slot: keel.ui.message-part, form: chunk, entry: ui/panel.js }
extensibility:
  doc: EXTENSIBILITY.md
  schema: extensibility.json
`
	doc := fmt.Sprintf(`{
  "schema": "keel.plugin-extensibility/1",
  "plugin": %q,
  "version": %q,
  "summary": "attachment preview fixture",
  "slots": [],
  "contributes": [%s],
  "configuration": [],
  "hooks": [],
  "providers": [],
  "examples": [],
  "limits": []
}`, id, version, claim)
	return writePluginPackage(t, filepath.Join(t.TempDir(), strings.ReplaceAll(id, ".", "-")), map[string]string{
		"plugin.yaml":        manifest,
		"ui/index.html":      "<html>panel</html>",
		"ui/panel.js":        "export function render() {}",
		"EXTENSIBILITY.md":   "# fixture\n",
		"extensibility.json": doc,
	})
}

// TestDecodeExtensionMergesAttachmentClaims pins the merge rule: part/mime
// attach to the contribution plugin.yaml declared by (slot, form, entry); they
// never create a second entry, and the projection reaches the inventory.
func TestDecodeExtensionMergesAttachmentClaims(t *testing.T) {
	dir := attachmentPackage(t, "vendor.preview", `{"slot": "keel.ui.message-part", "form": "chunk", "entry": "ui/panel.js",
     "part": "attachment", "mime": ["application/pdf", "image/*"]}`)
	pkg := pluginpkg.LoadDir(dir, pluginpkg.Environment{KeelVersion: testKeelVersion()})
	if !pkg.Usable() {
		t.Fatalf("fixture package is not usable: %+v", pkg.Problem)
	}
	decl := DecodeExtension(pkg.Dir, pkg.Manifest)
	if len(decl.Problems) > 0 {
		t.Fatalf("problems = %+v", decl.Problems)
	}
	if len(decl.Contributes) != 1 {
		t.Fatalf("claims created a duplicate contribution: %+v", decl.Contributes)
	}
	contribution := decl.Contributes[0]
	if contribution.Part != pluginpkg.AttachmentPartKind || strings.Join(contribution.Mime, ",") != "application/pdf,image/*" {
		t.Fatalf("merged contribution = %+v", contribution)
	}
	claims := AttachmentClaims(decl.Contributes)
	if len(claims) != 1 || claims[0].Slot != pluginpkg.MessagePartSlotID ||
		claims[0].Form != "chunk" || len(claims[0].Mime) != 2 {
		t.Fatalf("claims = %+v", claims)
	}
}

func TestDecodeExtensionRejectsABadAttachmentClaim(t *testing.T) {
	dir := attachmentPackage(t, "vendor.badclaim", `{"slot": "keel.ui.message-part", "form": "chunk", "entry": "ui/panel.js",
     "part": "attachment", "mime": ["Application/PDF"]}`)
	pkg := pluginpkg.LoadDir(dir, pluginpkg.Environment{KeelVersion: testKeelVersion()})
	if pkg.Usable() {
		t.Fatal("an uppercase MIME pattern must not validate")
	}
	if pkg.Problem == nil || len(pkg.Problem.Fields) == 0 ||
		!strings.Contains(pkg.Problem.Fields[0].Field, "mime") {
		t.Fatalf("problem = %+v", pkg.Problem)
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
		{
			name: "attachment claim on a foreign slot",
			decl: ExtensionDeclaration{Contributes: []ExtensionContribution{{
				Slot: "keel.ui.action", Form: "chunk", Entry: "x.js", Part: "attachment", Mime: []string{"application/pdf"},
			}}},
			field: "contributes[0].part",
		},
		{
			name: "mime without the attachment claim",
			decl: ExtensionDeclaration{Contributes: []ExtensionContribution{{
				Slot: "keel.ui.message-part", Form: "chunk", Entry: "x.js", Mime: []string{"application/pdf"},
			}}},
			field: "contributes[0].part",
		},
		{
			name: "invalid mime pattern",
			decl: ExtensionDeclaration{Contributes: []ExtensionContribution{{
				Slot: "keel.ui.message-part", Form: "chunk", Entry: "x.js", Part: "attachment", Mime: []string{"Image/PNG"},
			}}},
			field: "contributes[0].mime[0]",
		},
		{
			name: "duplicate mime pattern across contributions",
			decl: ExtensionDeclaration{Contributes: []ExtensionContribution{
				{Slot: "keel.ui.message-part", Form: "chunk", Entry: "a.js", Part: "attachment", Mime: []string{"application/pdf"}},
				{Slot: "keel.ui.message-part", Form: "chunk", Entry: "b.js", Part: "attachment", Mime: []string{"application/pdf"}},
			}},
			field: "contributes[1].mime[0]",
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

// TestManagerViewCarriesAttachmentClaims pins the GET /v1/plugins projection:
// the claim fields ride the same contribution the client already reads, so the
// web client can select a renderer by part + MIME without a second endpoint.
func TestManagerViewCarriesAttachmentClaims(t *testing.T) {
	root := t.TempDir()
	harness := newManagerHarness(t, root, nil)
	ctx := context.Background()
	if err := harness.manager.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	source := attachmentPackage(t, "vendor.preview", `{"slot": "keel.ui.message-part", "form": "chunk", "entry": "ui/panel.js",
     "part": "attachment", "mime": ["application/pdf", "image/*"]}`)
	if _, err := harness.manager.Install(ctx, InstallRequest{Path: source}, AuditContext{Actor: "tester"}); err != nil {
		t.Fatal(err)
	}
	views := harness.manager.Snapshot().Plugins
	if len(views) != 1 || len(views[0].Contributes) != 1 {
		t.Fatalf("inventory = %+v", views)
	}
	contribution := views[0].Contributes[0]
	if contribution.Part != pluginpkg.AttachmentPartKind || len(contribution.Mime) != 2 {
		t.Fatalf("contribution claims = %+v", contribution)
	}
	raw, err := json.Marshal(views[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{`"part":"attachment"`, `"mime":["application/pdf","image/*"]`} {
		if !strings.Contains(string(raw), needle) {
			t.Fatalf("view JSON lacks %s: %s", needle, raw)
		}
	}
}

// TestAttachmentPreviewLifecycleWithdrawsAndAudits is the backend half of the
// lifecycle requirement: install an attachment preview plugin, see the claim in
// the inventory, enable/disable/uninstall, and every lifecycle audit row names
// the claim (slot/form/MIME) while the contribution disappears with the plugin.
func TestAttachmentPreviewLifecycleWithdrawsAndAudits(t *testing.T) {
	root := t.TempDir()
	harness := newManagerHarness(t, root, nil)
	ctx := context.Background()
	if err := harness.manager.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	source := attachmentPackage(t, "vendor.preview", `{"slot": "keel.ui.message-part", "form": "chunk", "entry": "ui/panel.js",
     "part": "attachment", "mime": ["application/pdf", "image/*"]}`)
	view, err := harness.manager.Install(ctx, InstallRequest{Path: source}, AuditContext{Actor: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Contributes) != 1 || view.Contributes[0].Part != pluginpkg.AttachmentPartKind {
		t.Fatalf("installed view = %+v", view.Contributes)
	}
	if _, err := harness.manager.Enable(ctx, "vendor.preview", AuditContext{Actor: "tester"}); err != nil {
		t.Fatal(err)
	}
	enabled := harness.manager.Snapshot()
	if len(enabled.Enabled) != 1 || enabled.Enabled[0] != "vendor.preview" {
		t.Fatalf("enabled after enable = %+v", enabled.Enabled)
	}
	generation := enabled.Generation
	if _, err := harness.manager.Disable(ctx, "vendor.preview", AuditContext{Actor: "tester"}); err != nil {
		t.Fatal(err)
	}
	disabled := harness.manager.Snapshot()
	if len(disabled.Enabled) != 0 {
		t.Fatalf("enabled after disable = %+v", disabled.Enabled)
	}
	if disabled.Generation <= generation {
		t.Fatalf("disable did not advance the runtime generation: %d -> %d", generation, disabled.Generation)
	}
	if len(disabled.Plugins) != 1 || disabled.Plugins[0].State != LifecycleDisabled {
		t.Fatalf("plugin row after disable = %+v", disabled.Plugins)
	}
	if _, err := harness.manager.Uninstall(ctx, "vendor.preview", AuditContext{Actor: "tester"}); err != nil {
		t.Fatal(err)
	}
	if plugins := harness.manager.Snapshot().Plugins; len(plugins) != 0 {
		t.Fatalf("uninstall left residue: %+v", plugins)
	}
	if _, err := os.Stat(pluginpkg.PackageDirFor(root, "vendor.preview")); !os.IsNotExist(err) {
		t.Fatalf("managed directory survived uninstall: %v", err)
	}

	rows, err := harness.manager.Audit(ctx, nil, "vendor.preview", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if row.Result != store.PluginResultOK {
			continue
		}
		switch row.Action {
		case store.PluginActionInstall, store.PluginActionEnable, store.PluginActionDisable, store.PluginActionUninstall:
		default:
			continue
		}
		seen[row.Action] = true
		var detail struct {
			Claims []AttachmentClaim `json:"attachmentClaims"`
		}
		if err := json.Unmarshal(row.Detail, &detail); err != nil || len(detail.Claims) != 1 {
			t.Fatalf("%s audit detail = %s (%v)", row.Action, row.Detail, err)
		}
		if detail.Claims[0].Slot != pluginpkg.MessagePartSlotID || detail.Claims[0].Form != "chunk" ||
			len(detail.Claims[0].Mime) != 2 {
			t.Fatalf("%s claim detail = %+v", row.Action, detail.Claims[0])
		}
	}
	for _, action := range []string{store.PluginActionInstall, store.PluginActionEnable, store.PluginActionDisable, store.PluginActionUninstall} {
		if !seen[action] {
			t.Fatalf("no %s audit row: %+v", action, rows)
		}
	}
}

// TestDecodeExtensionIgnoresFixedDocumentWithoutPointer pins the claim source:
// a file merely named extensibility.json is not an approval surface unless
// plugin.yaml points at it. Its contributions still exist, but part/mime are
// empty (no renderer claim).
func TestDecodeExtensionIgnoresFixedDocumentWithoutPointer(t *testing.T) {
	dir := writePluginPackage(t, filepath.Join(t.TempDir(), "fixed"), map[string]string{
		"plugin.yaml":        webviewManifest("vendor.fixed", "1.0.0", nil, false),
		"ui/index.html":      "<html></html>",
		"ui/panel.js":        "export function render() {}",
		"extensibility.json": `{"contributes":[{"slot":"keel.ui.message-part","form":"chunk","entry":"ui/panel.js","part":"attachment","mime":["application/pdf"]}]}`,
	})
	decl := DecodeExtension(dir, nil)
	if len(decl.Contributes) != 1 {
		t.Fatalf("contributions = %+v", decl.Contributes)
	}
	if decl.Contributes[0].Part != "" || len(decl.Contributes[0].Mime) != 0 {
		t.Fatalf("a fixed-name document without a manifest pointer became a claim: %+v", decl.Contributes[0])
	}
}

// TestDecodeExtensionReadsTheDeclaredDocumentPath pins the other half: the
// manifest's pointer may name a path that is not extensibility.json.
func TestDecodeExtensionReadsTheDeclaredDocumentPath(t *testing.T) {
	manifest := `id: vendor.customdoc
version: 1.0.0
kind: webview
description: custom document path
engines:
  keel: ">=0.1.0 <1.0.0"
provides:
  ui:
    entry: ui/index.html
contributes:
  - { slot: keel.ui.message-part, form: chunk, entry: ui/panel.js }
extensibility:
  doc: EXTENSIBILITY.md
  schema: ext/claims.json
`
	doc := `{
  "schema": "keel.plugin-extensibility/1",
  "plugin": "vendor.customdoc",
  "version": "1.0.0",
  "summary": "custom path",
  "slots": [],
  "contributes": [
    {"slot": "keel.ui.message-part", "form": "chunk", "entry": "ui/panel.js",
     "part": "attachment", "mime": ["application/pdf"]}
  ],
  "configuration": [],
  "hooks": [],
  "providers": [],
  "examples": [],
  "limits": []
}`
	dir := writePluginPackage(t, filepath.Join(t.TempDir(), "custom"), map[string]string{
		"plugin.yaml":      manifest,
		"EXTENSIBILITY.md": "# custom\n",
		"ext/claims.json":  doc,
		"ui/index.html":    "<html></html>",
		"ui/panel.js":      "export function render() {}",
	})
	pkg := pluginpkg.LoadDir(dir, pluginpkg.Environment{KeelVersion: testKeelVersion()})
	if !pkg.Usable() {
		t.Fatalf("package is not usable: %+v", pkg.Problem)
	}
	decl := DecodeExtension(pkg.Dir, pkg.Manifest)
	if len(decl.Problems) > 0 {
		t.Fatalf("problems = %+v", decl.Problems)
	}
	if len(decl.Contributes) != 1 || decl.Contributes[0].Part != pluginpkg.AttachmentPartKind ||
		len(decl.Contributes[0].Mime) != 1 {
		t.Fatalf("declared document claims = %+v", decl.Contributes)
	}
	if decl.Doc == nil || !decl.Doc.Usable || decl.Doc.Schema != "ext/claims.json" {
		t.Fatalf("doc pointer = %+v", decl.Doc)
	}
}

// TestAttachmentPreviewUpdateAuditsNewClaims: an update replaces the installed
// declaration, so the update audit row names the NEW package's claims (not the
// old surface it just removed).
func TestAttachmentPreviewUpdateAuditsNewClaims(t *testing.T) {
	root := t.TempDir()
	harness := newManagerHarness(t, root, nil)
	ctx := context.Background()
	if err := harness.manager.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	v1 := attachmentPackageVersion(t, "vendor.preview", "1.0.0",
		`{"slot": "keel.ui.message-part", "form": "chunk", "entry": "ui/panel.js", "part": "attachment", "mime": ["application/pdf"]}`)
	if _, err := harness.manager.Install(ctx, InstallRequest{Path: v1}, AuditContext{Actor: "tester"}); err != nil {
		t.Fatal(err)
	}
	v2 := attachmentPackageVersion(t, "vendor.preview", "2.0.0",
		`{"slot": "keel.ui.message-part", "form": "chunk", "entry": "ui/panel.js", "part": "attachment", "mime": ["image/*"]}`)
	if _, err := harness.manager.Update(ctx, "vendor.preview", UpdateRequest{Path: v2}, AuditContext{Actor: "tester"}); err != nil {
		t.Fatal(err)
	}
	rows, err := harness.manager.Audit(ctx, nil, "vendor.preview", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var updateDetail struct {
		Claims []AttachmentClaim `json:"attachmentClaims"`
		From   string            `json:"from"`
		To     string            `json:"to"`
	}
	found := false
	for _, row := range rows {
		if row.Action != store.PluginActionUpdate || row.Result != store.PluginResultOK {
			continue
		}
		found = true
		if err := json.Unmarshal(row.Detail, &updateDetail); err != nil {
			t.Fatalf("update detail = %s: %v", row.Detail, err)
		}
	}
	if !found {
		t.Fatalf("no update audit row: %+v", rows)
	}
	if updateDetail.From != "1.0.0" || updateDetail.To != "2.0.0" {
		t.Fatalf("update detail versions = %+v", updateDetail)
	}
	if len(updateDetail.Claims) != 1 || len(updateDetail.Claims[0].Mime) != 1 ||
		updateDetail.Claims[0].Mime[0] != "image/*" {
		t.Fatalf("update claims = %+v, want the new package's image/* claim", updateDetail.Claims)
	}
}

// TestValidateExtensionRejectsClaimsWithoutDocument: a claim requires the
// machine-readable document; without it the declaration is not a renderer.
func TestValidateExtensionRejectsClaimsWithoutDocument(t *testing.T) {
	problems := ValidateExtension("vendor.claim", "webview", ExtensionDeclaration{
		Contributes: []ExtensionContribution{{
			Slot: pluginpkg.MessagePartSlotID, Form: "chunk", Entry: "ui/panel.js",
			Part: pluginpkg.AttachmentPartKind, Mime: []string{"application/pdf"},
		}},
	})
	found := false
	for _, problem := range problems {
		if problem.Field == "contributes[0].part" && strings.Contains(problem.Message, ExtensionFileName) {
			found = true
		}
	}
	if !found {
		t.Fatalf("problems = %+v, want the missing-document refusal", problems)
	}
}
