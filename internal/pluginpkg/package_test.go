package pluginpkg

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writePackage materializes a plugin package on disk and returns its directory.
func writePackage(t *testing.T, root, dir string, files map[string]string) string {
	t.Helper()
	path := filepath.Join(root, dir)
	for name, content := range files {
		target := filepath.Join(path, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func manifestYAML(id, version string, extra string) string {
	return "id: " + id + "\nversion: " + version + "\nkind: in-process\ndescription: d\n" + extra
}

func TestLoadDirValidAndHashIdentity(t *testing.T) {
	root := t.TempDir()
	dir := writePackage(t, root, "vendor.thing", map[string]string{
		"plugin.yaml":  manifestYAML("vendor.thing", "1.0.0", "provides:\n  tools: [thing_run]\n"),
		"lib/notes.md": "hello",
	})
	pkg := LoadDir(dir, testEnv())
	if !pkg.Usable() {
		t.Fatalf("package invalid: %v", pkg.Problem)
	}
	if pkg.Hash == "" || len(pkg.Hash) != 64 {
		t.Fatalf("hash = %q", pkg.Hash)
	}
	// The hash is content identity: identical trees hash identically, and a
	// single byte change moves it.
	clone := writePackage(t, t.TempDir(), "copy", map[string]string{
		"plugin.yaml":  manifestYAML("vendor.thing", "1.0.0", "provides:\n  tools: [thing_run]\n"),
		"lib/notes.md": "hello",
	})
	other := LoadDir(clone, testEnv())
	if other.Hash != pkg.Hash {
		t.Fatalf("identical trees hash differently: %s vs %s", pkg.Hash, other.Hash)
	}
	if err := os.WriteFile(filepath.Join(dir, "lib", "notes.md"), []byte("hello!"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed := LoadDir(dir, testEnv())
	if changed.Hash == pkg.Hash {
		t.Fatal("a content change must change the hash")
	}
	// A manifest change is a content change too.
	if err := os.WriteFile(filepath.Join(dir, "lib", "notes.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifestYAML("vendor.thing", "1.0.1", "")), 0o644); err != nil {
		t.Fatal(err)
	}
	if bumped := LoadDir(dir, testEnv()); bumped.Hash == pkg.Hash {
		t.Fatal("a manifest change must change the hash")
	}
}

func TestLoadDirIsolatesBrokenManifest(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "no.manifest")
	if err := os.MkdirAll(missing, 0o755); err != nil {
		t.Fatal(err)
	}
	pkg := LoadDir(missing, testEnv())
	if pkg.Problem == nil || !strings.Contains(pkg.Problem.Reason, "plugin.yaml is missing") {
		t.Fatalf("problem = %v", pkg.Problem)
	}

	bad := writePackage(t, root, "bad.manifest", map[string]string{"plugin.yaml": "id: BAD\nversion: nope\n"})
	pkg = LoadDir(bad, testEnv())
	if pkg.Problem == nil {
		t.Fatal("expected a problem")
	}
	if len(pkg.Problem.Fields) < 3 {
		t.Fatalf("field errors = %+v", pkg.Problem.Fields)
	}
	// The declared id stays readable for diagnostics, but an unusable package has
	// no content identity: nothing may be installed from it.
	if pkg.ID() != "BAD" || pkg.Hash != "" || pkg.Usable() {
		t.Fatalf("unusable package must not expose a hash: %+v", pkg)
	}
}

func TestLoadDirChecksUITree(t *testing.T) {
	root := t.TempDir()
	dir := writePackage(t, root, "vendor.web", map[string]string{
		"plugin.yaml": "id: vendor.web\nversion: 1.0.0\nkind: webview\ndescription: d\nprovides:\n  ui:\n    entry: ui/index.html\n",
	})
	pkg := LoadDir(dir, testEnv())
	if pkg.Problem == nil {
		t.Fatal("a webview package without its entry file must be refused")
	}
	if !strings.Contains(pkg.Problem.Fields[0].Field, "provides.ui.entry") {
		t.Fatalf("fields = %+v", pkg.Problem.Fields)
	}
	writePackage(t, root, "vendor.web", map[string]string{"ui/index.html": "<html></html>"})
	pkg = LoadDir(dir, testEnv())
	if !pkg.Usable() {
		t.Fatalf("problem = %v", pkg.Problem)
	}
}

func TestLoadDirRefusesEscapingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := writePackage(t, root, "vendor.link", map[string]string{"plugin.yaml": manifestYAML("vendor.link", "1.0.0", "")})
	if err := os.Symlink(secret, filepath.Join(dir, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	pkg := LoadDir(dir, testEnv())
	if pkg.Problem == nil || !strings.Contains(pkg.Problem.Reason, "escapes the plugin package") {
		t.Fatalf("problem = %v", pkg.Problem)
	}
}

func TestCopyTreeMaterializesSymlinksAndCapsSize(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	root := t.TempDir()
	src := writePackage(t, root, "src", map[string]string{
		"plugin.yaml":   manifestYAML("vendor.copy", "1.0.0", ""),
		"ui/index.html": "<html></html>",
	})
	if err := os.Symlink(filepath.Join(src, "ui", "index.html"), filepath.Join(src, "ui", "alias.html")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "managed")
	if err := CopyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(dst, "ui", "alias.html"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the managed copy must not contain symlinks")
	}
	if data, err := os.ReadFile(filepath.Join(dst, "ui", "alias.html")); err != nil || string(data) != "<html></html>" {
		t.Fatalf("copied content = %q, %v", data, err)
	}
	hashSrc, err := HashTree(src)
	if err != nil {
		t.Fatal(err)
	}
	hashDst, err := HashTree(dst)
	if err != nil {
		t.Fatal(err)
	}
	if hashSrc != hashDst {
		t.Fatalf("a materialized copy must hash like its source: %s vs %s", hashSrc, hashDst)
	}

	big := t.TempDir()
	if err := os.WriteFile(filepath.Join(big, "plugin.yaml"), []byte(manifestYAML("vendor.big", "1.0.0", "")), 0o644); err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, MaxPackageFileBytes+1)
	if err := os.WriteFile(filepath.Join(big, "blob.bin"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CopyTree(big, filepath.Join(t.TempDir(), "big")); err == nil {
		t.Fatal("a package over the file cap must be refused")
	}
}

func TestRemoveTreeStaysInsideRoot(t *testing.T) {
	root := t.TempDir()
	dir := writePackage(t, root, "vendor.gone", map[string]string{"plugin.yaml": manifestYAML("vendor.gone", "1.0.0", "")})
	if err := RemoveTree(root, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("directory should be gone")
	}
	outside := t.TempDir()
	if err := RemoveTree(root, outside); err == nil {
		t.Fatal("removing anything outside the root must be refused")
	}
	if err := RemoveTree(root, root); err == nil {
		t.Fatal("removing the root itself must be refused")
	}
}
