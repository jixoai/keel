package pluginpkg

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Scan is the result of scanning a plugins root.
type Scan struct {
	// Root is the scanned root directory.
	Root string `json:"root"`
	// Packages are the usable packages, sorted by id then descending version.
	Packages []Package `json:"packages"`
	// Invalid are directories whose manifest exists but cannot be used. They are
	// isolated: the rest of the root still loads.
	Invalid []Package `json:"invalid"`
	// Skipped lists immediate subdirectories that carry no plugin.yaml (not
	// packages at all).
	Skipped []string `json:"skipped,omitempty"`
	// Errors lists directories that could not be read at all.
	Errors []string `json:"errors,omitempty"`
}

// Discover scans `<root>/*/plugin.yaml`. The canonical layout is
// `<root>/<id>/plugin.yaml`; a directory whose manifest declares a different id
// (a staged `my-plugin-1.2.3/` directory, for example) is accepted and grouped
// under the declared id, because the sha256 content hash — not the directory
// name — is the package identity.
//
// Hidden entries and non-directories are skipped. A subdirectory that is a
// symlink is refused when its target escapes the root ("拒绝符号链接外逃");
// one that stays inside is followed.
func Discover(root string, env Environment) Scan {
	scan := Scan{Root: root}
	absolute, err := filepath.Abs(root)
	if err != nil {
		scan.Errors = append(scan.Errors, "resolve root: "+err.Error())
		return scan
	}
	scan.Root = absolute
	entries, err := os.ReadDir(absolute)
	if err != nil {
		if os.IsNotExist(err) {
			// An absent root is an empty inventory, not a failure: a fresh host
			// has nothing installed.
			return scan
		}
		scan.Errors = append(scan.Errors, "read root: "+err.Error())
		return scan
	}
	resolvedRoot, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		resolvedRoot = absolute
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(absolute, name)
		info, err := os.Lstat(path)
		if err != nil {
			scan.Errors = append(scan.Errors, name+": "+err.Error())
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				scan.Invalid = append(scan.Invalid, Package{Dir: path, Problem: &Problem{Path: path, Reason: "symlink is broken: " + err.Error()}})
				continue
			}
			if !IsInside(resolvedRoot, resolved) {
				scan.Invalid = append(scan.Invalid, Package{Dir: path, Problem: &Problem{Path: path, Reason: "symlink escapes the plugins root (" + resolved + ")"}})
				continue
			}
			path = resolved
			if info, err = os.Stat(path); err != nil || !info.IsDir() {
				scan.Skipped = append(scan.Skipped, name)
				continue
			}
		} else if !info.IsDir() {
			scan.Skipped = append(scan.Skipped, name)
			continue
		}
		if _, err := os.Stat(filepath.Join(path, PluginFileName)); err != nil {
			scan.Skipped = append(scan.Skipped, name)
			continue
		}
		pkg := LoadDir(path, env)
		if pkg.Usable() {
			scan.Packages = append(scan.Packages, pkg)
		} else {
			scan.Invalid = append(scan.Invalid, pkg)
		}
	}
	SortPackages(scan.Packages)
	sort.Slice(scan.Invalid, func(i, j int) bool { return scan.Invalid[i].Dir < scan.Invalid[j].Dir })
	sort.Strings(scan.Skipped)
	sort.Strings(scan.Errors)
	return scan
}

// SortPackages orders packages by id ascending, then version descending, then
// directory, so the winner of a conflict is always the same one.
func SortPackages(packages []Package) {
	sort.Slice(packages, func(i, j int) bool {
		left, right := packages[i], packages[j]
		if left.ID() != right.ID() {
			return left.ID() < right.ID()
		}
		if comparison := left.Manifest.ParsedVersion.Compare(right.Manifest.ParsedVersion); comparison != 0 {
			return comparison > 0
		}
		return left.Dir < right.Dir
	})
}

// GroupByID groups usable packages by manifest id, each group sorted by
// descending version.
func (s Scan) GroupByID() map[string][]Package {
	groups := make(map[string][]Package)
	for _, pkg := range s.Packages {
		groups[pkg.ID()] = append(groups[pkg.ID()], pkg)
	}
	return groups
}

// Conflict is the resolution of "the same id appears more than once".
type Conflict struct {
	// ID is the contested plugin id.
	ID string `json:"id"`
	// Chosen is the package the host will use.
	Chosen Package `json:"chosen"`
	// Shadowed are the other versions, kept on disk for update/rollback.
	Shadowed []Package `json:"shadowed,omitempty"`
	// Reason explains the choice.
	Reason string `json:"reason"`
}

// ResolveConflicts applies the multi-version rule: the version the database
// says is installed/enabled wins ("取最高已启用"), otherwise the highest
// semantic version ("最新"). Every other package of that id is shadowed, never
// loaded.
func (s Scan) ResolveConflicts(installedVersion func(id string) string) (chosen []Conflict, single []Package) {
	groups := s.GroupByID()
	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		group := groups[id]
		if len(group) == 1 {
			single = append(single, group[0])
			continue
		}
		resolved := Conflict{ID: id}
		preferred := ""
		if installedVersion != nil {
			preferred = installedVersion(id)
		}
		for _, pkg := range group {
			if preferred != "" && pkg.Version() == preferred {
				resolved.Chosen = pkg
				resolved.Reason = "the installed version " + preferred + " wins over newer packages"
				break
			}
		}
		if resolved.Chosen.Manifest == nil {
			resolved.Chosen = group[0]
			resolved.Reason = "highest version wins (" + group[0].Version() + ")"
		}
		for _, pkg := range group {
			if pkg.Dir != resolved.Chosen.Dir {
				resolved.Shadowed = append(resolved.Shadowed, pkg)
			}
		}
		chosen = append(chosen, resolved)
	}
	sort.Slice(single, func(i, j int) bool { return single[i].ID() < single[j].ID() })
	return chosen, single
}

// PackageDirFor is the managed destination of a plugin id: `<root>/<id>`.
func PackageDirFor(root, id string) string { return filepath.Join(root, id) }

// ManagedDir reports whether dir is a managed plugin directory of root, i.e.
// one level below it and not a hidden working directory.
func ManagedDir(root, dir string) bool {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absoluteDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	if !IsInside(absoluteRoot, absoluteDir) || filepath.Clean(absoluteRoot) == filepath.Clean(absoluteDir) {
		return false
	}
	rel, err := filepath.Rel(absoluteRoot, absoluteDir)
	if err != nil || strings.HasPrefix(rel, ".") || strings.Contains(rel, string(filepath.Separator)) {
		return false
	}
	return true
}
