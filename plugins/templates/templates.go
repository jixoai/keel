// Package templates carries the plugin package templates that `keel.creation`
// scaffolds from. The canonical copy lives in this directory (so it can be read,
// reviewed and edited as ordinary files); the same tree is embedded into the
// binary, so `keel serve` can scaffold a package without the repository at hand.
//
// A template is a set of files with `{{placeholder}}` substitutions. Rendering
// is a plain textual substitution — no templating language, no code execution —
// so a template cannot do anything the host did not write down.
//
// Spec: openspec/changes/keel-plugin-core/specs/plugin-system/spec.md
// (Requirement: 创造模式（作为内置插件 `keel.creation`）).
package templates

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

//go:embed webview-react-solidglass
var embedded embed.FS

// WebviewReactSolidglass is the shipped template: a webview plugin package with
// a React panel (built into `ui/app.js`, because the host CSP allows same-origin
// scripts only), host theme tokens, and a stdio JSON-RPC backend that serves one
// tool.
const WebviewReactSolidglass = "webview-react-solidglass"

// TemplateSuffix marks a file whose rendered name drops the suffix
// (`plugin.yaml.tmpl` renders as `plugin.yaml`).
const TemplateSuffix = ".tmpl"

// Vars are the substitutions a template accepts. Every field is a single-line
// scalar; the scaffold derives them from the tool call's arguments.
type Vars struct {
	// ID is the plugin id (`^[a-z][a-z0-9._-]{1,63}$`).
	ID string
	// Title is the display title of the plugin's UI surface.
	Title string
	// Description is the manifest's one-line description (the caller's purpose).
	Description string
	// Tool is the local tool name the backend serves and the manifest declares.
	Tool string
	// Version is the package version (semver).
	Version string
	// PublishedTool is the wire-safe name the model calls: the host joins
	// `<id>.<tool>` and normalizes it (`demo_sidecar_greet`).
	PublishedTool string
}

// File is one rendered template file.
type File struct {
	// Path is the package-relative path with forward slashes.
	Path string
	// Data is the file content.
	Data []byte
}

// Names lists the shipped template names.
func Names() []string { return []string{WebviewReactSolidglass} }

// Files lists a template's files (package-relative, sorted, `.tmpl` suffixes
// kept). It is the structural inventory `creation_validate` self-checks against.
func Files(name string) ([]string, error) {
	root, err := templateRoot(name)
	if err != nil {
		return nil, err
	}
	var files []string
	err = fs.WalkDir(root, ".", func(entry string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		files = append(files, entry)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("templates: list %s: %w", name, err)
	}
	return files, nil
}

// Render substitutes vars into every file of a template and returns the files
// with `.tmpl` suffixes dropped. A missing required value or a placeholder left
// in the rendered manifest is an error: a half-substituted package must never
// reach disk.
func Render(name string, vars Vars) ([]File, error) {
	if err := vars.validate(); err != nil {
		return nil, err
	}
	root, err := templateRoot(name)
	if err != nil {
		return nil, err
	}
	replacements := []string{
		"{{id}}", vars.ID,
		"{{version}}", vars.Version,
		"{{title}}", vars.Title,
		"{{description}}", vars.Description,
		"{{tool}}", vars.Tool,
		"{{publishedTool}}", vars.PublishedTool,
	}
	replacer := strings.NewReplacer(replacements...)
	var files []File
	err = fs.WalkDir(root, ".", func(entry string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(root, entry)
		if err != nil {
			return err
		}
		rendered := File{Path: strings.TrimSuffix(path.Clean(entry), TemplateSuffix), Data: []byte(replacer.Replace(string(data)))}
		if strings.HasSuffix(rendered.Path, ".yaml") && strings.Contains(string(rendered.Data), "{{") {
			return fmt.Errorf("templates: %s: %s still carries an unsubstituted placeholder", name, rendered.Path)
		}
		files = append(files, rendered)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("templates: render %s: %w", name, err)
	}
	return files, nil
}

// validate refuses the values a rendered manifest cannot carry safely.
func (v Vars) validate() error {
	switch {
	case v.ID == "":
		return fmt.Errorf("templates: id is required")
	case v.Tool == "":
		return fmt.Errorf("templates: tool is required")
	case v.Version == "":
		return fmt.Errorf("templates: version is required")
	case strings.ContainsAny(v.Title, "\r\n") || strings.ContainsAny(v.Description, "\r\n") ||
		strings.ContainsAny(v.Tool, "\r\n") || strings.ContainsAny(v.ID, "\r\n"):
		return fmt.Errorf("templates: every value must be a single line")
	}
	return nil
}

// templateRoot resolves a template name to its embedded subtree.
func templateRoot(name string) (fs.FS, error) {
	for _, known := range Names() {
		if known == name {
			return fs.Sub(embedded, known)
		}
	}
	return nil, fmt.Errorf("templates: unknown template %q (have: %s)", name, strings.Join(Names(), ", "))
}
