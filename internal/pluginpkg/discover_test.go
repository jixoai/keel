package pluginpkg

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDiscoverScansRootAndIsolatesInvalid(t *testing.T) {
	root := t.TempDir()
	writePackage(t, root, "vendor.one", map[string]string{"plugin.yaml": manifestYAML("vendor.one", "1.0.0", "")})
	// A directory named <id>-<version> is accepted: the manifest declares the id.
	writePackage(t, root, "vendor.two-2.0.0", map[string]string{"plugin.yaml": manifestYAML("vendor.two", "2.0.0", "")})
	// Corrupt manifests are isolated, never fatal.
	writePackage(t, root, "vendor.broken", map[string]string{"plugin.yaml": "id: nope\n"})
	// A hidden directory is not a plugin root.
	writePackage(t, root, ".staging", map[string]string{"plugin.yaml": manifestYAML("vendor.hidden", "1.0.0", "")})
	// A directory without a manifest is skipped, not an error.
	if err := os.MkdirAll(filepath.Join(root, "not-a-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A stray file is skipped too.
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	scan := Discover(root, testEnv())
	if len(scan.Errors) != 0 {
		t.Fatalf("errors = %v", scan.Errors)
	}
	if len(scan.Packages) != 2 {
		t.Fatalf("packages = %+v", scan.Packages)
	}
	if len(scan.Invalid) != 1 || scan.Invalid[0].Problem == nil {
		t.Fatalf("invalid = %+v", scan.Invalid)
	}
	if !strings.Contains(scan.Invalid[0].Problem.Reason, "version") {
		t.Fatalf("isolation reason = %q", scan.Invalid[0].Problem.Reason)
	}
	ids := []string{scan.Packages[0].ID(), scan.Packages[1].ID()}
	if ids[0] != "vendor.one" || ids[1] != "vendor.two" {
		t.Fatalf("ids = %v", ids)
	}
	if len(scan.Skipped) != 2 {
		t.Fatalf("skipped = %v", scan.Skipped)
	}
	if scan.Root == "" || !filepath.IsAbs(scan.Root) {
		t.Fatalf("root = %q", scan.Root)
	}
}

func TestDiscoverMissingRootIsEmpty(t *testing.T) {
	scan := Discover(filepath.Join(t.TempDir(), "nope"), testEnv())
	if len(scan.Packages) != 0 || len(scan.Invalid) != 0 || len(scan.Errors) != 0 {
		t.Fatalf("scan = %+v", scan)
	}
}

func TestDiscoverRefusesEscapingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	root := t.TempDir()
	outside := writePackage(t, t.TempDir(), "vendor.elsewhere", map[string]string{
		"plugin.yaml": manifestYAML("vendor.elsewhere", "1.0.0", ""),
	})
	if err := os.Symlink(outside, filepath.Join(root, "vendor.link")); err != nil {
		t.Fatal(err)
	}
	scan := Discover(root, testEnv())
	if len(scan.Invalid) != 1 || !strings.Contains(scan.Invalid[0].Problem.Reason, "escapes the plugins root") {
		t.Fatalf("invalid = %+v", scan.Invalid)
	}
	if len(scan.Packages) != 0 {
		t.Fatalf("packages = %+v", scan.Packages)
	}
	// A symlink that stays inside the root is followed.
	inside := writePackage(t, root, "vendor.inside", map[string]string{"plugin.yaml": manifestYAML("vendor.inside", "1.0.0", "")})
	if err := os.Symlink(inside, filepath.Join(root, "vendor.alias")); err != nil {
		t.Fatal(err)
	}
	scan = Discover(root, testEnv())
	grouped := scan.GroupByID()
	if len(grouped["vendor.inside"]) != 2 {
		t.Fatalf("an in-root symlink must be discovered: %+v", grouped)
	}
}

func TestResolveConflictsPicksInstalledThenHighest(t *testing.T) {
	root := t.TempDir()
	writePackage(t, root, "vendor.multi", map[string]string{"plugin.yaml": manifestYAML("vendor.multi", "1.0.0", "")})
	writePackage(t, root, "vendor.multi-2.0.0", map[string]string{"plugin.yaml": manifestYAML("vendor.multi", "2.0.0", "")})
	writePackage(t, root, "vendor.multi-1.5.0", map[string]string{"plugin.yaml": manifestYAML("vendor.multi", "1.5.0", "")})
	writePackage(t, root, "vendor.solo", map[string]string{"plugin.yaml": manifestYAML("vendor.solo", "0.9.0", "")})

	scan := Discover(root, testEnv())
	conflicts, single := scan.ResolveConflicts(nil)
	if len(single) != 1 || single[0].ID() != "vendor.solo" {
		t.Fatalf("single = %+v", single)
	}
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %+v", conflicts)
	}
	resolved := conflicts[0]
	if resolved.Chosen.Version() != "2.0.0" {
		t.Fatalf("highest version must win, got %s", resolved.Chosen.Version())
	}
	if len(resolved.Shadowed) != 2 {
		t.Fatalf("shadowed = %+v", resolved.Shadowed)
	}
	if !strings.Contains(resolved.Reason, "highest") {
		t.Fatalf("reason = %q", resolved.Reason)
	}

	// The installed version wins over a newer package on disk.
	conflicts, _ = scan.ResolveConflicts(func(id string) string {
		if id == "vendor.multi" {
			return "1.5.0"
		}
		return ""
	})
	if conflicts[0].Chosen.Version() != "1.5.0" {
		t.Fatalf("installed version must win, got %s", conflicts[0].Chosen.Version())
	}
	if len(conflicts[0].Shadowed) != 2 {
		t.Fatalf("shadowed = %+v", conflicts[0].Shadowed)
	}
}

func TestManagedDirAndPackageDirFor(t *testing.T) {
	root := t.TempDir()
	if got := PackageDirFor(root, "vendor.one"); got != filepath.Join(root, "vendor.one") {
		t.Fatalf("PackageDirFor = %q", got)
	}
	if !ManagedDir(root, filepath.Join(root, "vendor.one")) {
		t.Fatal("a one-level subdirectory is managed")
	}
	if ManagedDir(root, root) {
		t.Fatal("the root itself is not a managed plugin directory")
	}
	if ManagedDir(root, filepath.Join(root, ".staging", "vendor.one")) {
		t.Fatal("a hidden working directory is not managed")
	}
	if ManagedDir(root, filepath.Dir(root)) {
		t.Fatal("a path outside the root is not managed")
	}
}
