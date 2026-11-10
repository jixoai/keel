// Package builtin carries the manifests of the plugins compiled into the keel
// binary. They are ordinary plugin packages in every respect — same schema,
// same validation, same lifecycle (an operator can disable them) — with two
// differences the host adds: they are read from the binary instead of a
// directory, and `bundled: true` marks them as shipped with keel.
//
// A builtin manifest is validated at boot; a manifest that does not validate is
// a programming error, not an operator error, so Manifests returns the failure
// instead of skipping the plugin silently.
package builtin

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"

	"github.com/jixoai/keel/internal/pluginpkg"
)

//go:embed *.yaml
var manifestFS embed.FS

// Manifests decodes every embedded manifest, in file-name order.
func Manifests() ([]*pluginpkg.Manifest, error) {
	entries, err := fs.ReadDir(manifestFS, ".")
	if err != nil {
		return nil, fmt.Errorf("builtin: read manifests: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	out := make([]*pluginpkg.Manifest, 0, len(names))
	for _, name := range names {
		raw, err := manifestFS.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("builtin: read %s: %w", name, err)
		}
		manifest, err := pluginpkg.ParseManifest(raw)
		if err != nil {
			return nil, fmt.Errorf("builtin: parse %s: %w", name, err)
		}
		manifest.Dir = "builtin:" + path.Base(name)
		out = append(out, manifest)
	}
	return out, nil
}

// Manifest returns the builtin manifest with the given id.
func Manifest(id string) (*pluginpkg.Manifest, bool) {
	manifests, err := Manifests()
	if err != nil {
		return nil, false
	}
	for _, manifest := range manifests {
		if manifest.ID == id {
			return manifest, true
		}
	}
	return nil, false
}
