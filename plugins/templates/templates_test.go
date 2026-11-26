package templates

// Template contract tests: a rendered template must be a package the REAL plugin
// validator accepts, with no placeholder left behind, and the substitutions must
// reach the manifest and the README. A template that only "looks right" is a
// template whose first use fails at install, which is exactly the failure this
// test exists to prevent.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/pluginpkg"
)

func testVars() Vars {
	return Vars{
		ID:            "demo.panel",
		Title:         "Token Panel",
		Description:   "show the session token usage",
		Tool:          "refresh",
		Version:       "0.1.0",
		PublishedTool: "demo_panel_refresh",
	}
}

func writeRendered(t *testing.T, files []File) string {
	t.Helper()
	dir := t.TempDir()
	for _, file := range files {
		target := filepath.Join(dir, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, file.Data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRenderProducesAnInstallablePackage(t *testing.T) {
	files, err := Render(WebviewReactSolidglass, testVars())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	paths := make(map[string]bool, len(files))
	for _, file := range files {
		paths[file.Path] = true
		if strings.Contains(file.Path, TemplateSuffix) {
			t.Fatalf("rendered file keeps the %s suffix: %s", TemplateSuffix, file.Path)
		}
	}
	for _, want := range []string{
		"plugin.yaml", "README.md", "EXTENSIBILITY.md", "extensibility.json",
		"ui/index.html", "ui/app.js", "ui/app.jsx", "ui/settings.js", "ui/theme.css", "ui/package.json",
		"backend/server.mjs",
	} {
		if !paths[want] {
			t.Fatalf("the template is missing %s (have: %v)", want, keys(paths))
		}
	}

	dir := writeRendered(t, files)
	pkg := pluginpkg.LoadDir(dir, pluginpkg.Environment{KeelVersion: "0.1.0"})
	if !pkg.Usable() {
		t.Fatalf("the rendered package does not validate: %+v", pkg.Problem)
	}
	if pkg.Manifest.ID != "demo.panel" || pkg.Manifest.Provides.Tools[0] != "refresh" {
		t.Fatalf("manifest = %+v", pkg.Manifest)
	}
	if pkg.Manifest.Provides.UI == nil || pkg.Manifest.Provides.UI.Title != "Token Panel" {
		t.Fatalf("ui declaration = %+v", pkg.Manifest.Provides.UI)
	}
	if pkg.Manifest.Kind != pluginpkg.KindWebview || len(pkg.Manifest.Entry.Command) == 0 {
		t.Fatalf("kind/entry = %s / %v, want a webview with a sidecar backend", pkg.Manifest.Kind, pkg.Manifest.Entry.Command)
	}
	if len(pkg.Manifest.Contributes) != 1 || pkg.Manifest.Contributes[0].Form != pluginpkg.SlotFormChunk ||
		pkg.Manifest.Contributes[0].Entry != "ui/settings.js" {
		t.Fatalf("contributes = %+v, want one Native chunk", pkg.Manifest.Contributes)
	}
	if pkg.Manifest.Extensibility == nil || pkg.Manifest.Extensibility.Schema != "extensibility.json" {
		t.Fatalf("extensibility = %+v", pkg.Manifest.Extensibility)
	}
	if got := pkg.Manifest.PublishedTools()[0].Published; got != "demo_panel_refresh" {
		t.Fatalf("published tool = %q", got)
	}
	if pkg.Hash == "" {
		t.Fatal("the rendered package has no content hash")
	}

	// The MANIFEST must not carry a placeholder (the validator would accept
	// `{{tool}}` as a tool name, but a model could never call it).
	manifest, err := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(manifest), "{{") {
		t.Fatalf("plugin.yaml carries an unsubstituted placeholder:\n%s", manifest)
	}
	readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"demo.panel", "Token Panel", "refresh", "demo_panel_refresh"} {
		if !strings.Contains(string(readme), want) {
			t.Fatalf("README does not mention %q", want)
		}
	}

	// A runtime chunk is imported as one self-contained module from
	// /plugins/<id>/ui/settings.js inside the published (linked) realm. A bare
	// `nsh` specifier cannot resolve there, so the template must read the host
	// realm's module namespace instead of importing it. This is the difference
	// between a hosted chunk that renders and a 200 that fails as ERR_CHUNK_LOAD.
	settings, err := os.ReadFile(filepath.Join(dir, "ui", "settings.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`from "nsh"`, `from 'nsh'`, `import("nsh")`, `import('nsh')`} {
		if strings.Contains(string(settings), forbidden) {
			t.Fatalf("ui/settings.js imports the host module bare (%q); a runtime chunk cannot resolve it", forbidden)
		}
	}
	for _, want := range []string{"__nsh_browser_realm", `get("nsh")`, "export function render"} {
		if !strings.Contains(string(settings), want) {
			t.Fatalf("ui/settings.js does not carry the runtime chunk contract (%q)", want)
		}
	}
}

func TestRenderRefusesUnknownTemplatesAndMissingValues(t *testing.T) {
	if _, err := Render("no-such-template", testVars()); err == nil {
		t.Fatal("an unknown template rendered")
	}
	vars := testVars()
	vars.Tool = ""
	if _, err := Render(WebviewReactSolidglass, vars); err == nil {
		t.Fatal("a template rendered without a tool name")
	}
	vars = testVars()
	vars.Description = "two\nlines"
	if _, err := Render(WebviewReactSolidglass, vars); err == nil {
		t.Fatal("a template rendered a multi-line description into plugin.yaml")
	}
}

func TestFilesListsTheWholeTemplate(t *testing.T) {
	files, err := Files(WebviewReactSolidglass)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	var hasManifestTemplate bool
	for _, file := range files {
		if file == "plugin.yaml"+TemplateSuffix {
			hasManifestTemplate = true
		}
	}
	if !hasManifestTemplate {
		t.Fatalf("Files = %v, want the manifest template", files)
	}
	if names := Names(); len(names) != 1 || names[0] != WebviewReactSolidglass {
		t.Fatalf("Names = %v", names)
	}
}

func keys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	return out
}
