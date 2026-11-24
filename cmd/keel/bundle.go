// The self-contained distribution: one platform binary carries the PostgreSQL
// runtime, the packaged web client and the release README. A source checkout
// builds with only assets/README.txt; the release script fills the rest.
//
// The runtime is extracted once per version under $KEEL_HOME/runtime/<version>
// (or used in place from a .app's Contents/Resources), never from /tmp: the
// data directory and the server binaries must survive a reboot and belong to
// the user.
package main

import (
	"archive/tar"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ulikunitz/xz"
)

//go:embed assets
var embeddedAssets embed.FS

const (
	embeddedPGRoot = "assets/pg.txz"
	embeddedWeb    = "assets/web"
	embeddedIcon   = "assets/icon.png"
	embeddedReadme = "assets/README.md"
	runtimeMarker  = ".ready"
)

func hasEmbeddedPG() bool {
	f, err := embeddedAssets.Open(embeddedPGRoot)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func keelHome() string {
	if home := strings.TrimSpace(os.Getenv("KEEL_HOME")); home != "" {
		return home
	}
	if dir, err := os.UserHomeDir(); err == nil {
		return filepath.Join(dir, ".keel")
	}
	return ""
}

func runtimeRoot() string {
	home := keelHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "runtime", version)
}

// installedResources returns the .app Resources directory when the running
// binary is the one inside a generated macOS app.
func installedResources() (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	macOS := filepath.Dir(exe)
	if filepath.Base(macOS) != "MacOS" {
		return "", false
	}
	resources := filepath.Join(filepath.Dir(filepath.Dir(macOS)), "Resources")
	if isDir(filepath.Join(resources, "pg")) && isDir(filepath.Join(resources, "web")) {
		return resources, true
	}
	return "", false
}

func executableDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// pgSearchDirs is the order `pgTool` tries before PATH: an explicit
// KEEL_PG_DIR, the installed app's Resources, a pg/ beside the binary, then the
// extracted runtime of this version.
func pgSearchDirs() []string {
	var dirs []string
	if env := strings.TrimSpace(os.Getenv("KEEL_PG_DIR")); env != "" {
		dirs = append(dirs, filepath.Join(env, "bin"), env)
	}
	if res, ok := installedResources(); ok {
		dirs = append(dirs, filepath.Join(res, "pg", "bin"))
	}
	if dir := executableDir(); dir != "" {
		dirs = append(dirs, filepath.Join(dir, "pg", "bin"))
	}
	if root := runtimeRoot(); root != "" {
		dirs = append(dirs, filepath.Join(root, "pg", "bin"))
	}
	return dirs
}

func bundledWebDir() string {
	if res, ok := installedResources(); ok {
		return filepath.Join(res, "web")
	}
	if dir := executableDir(); dir != "" && isDir(filepath.Join(dir, "web")) {
		return filepath.Join(dir, "web")
	}
	if root := runtimeRoot(); root != "" && isDir(filepath.Join(root, "web")) {
		return filepath.Join(root, "web")
	}
	return ""
}

// defaultWebDir is the serve flag's default: an explicit env var, the installed
// app's web, the runtime's web, then the repository's development build.
func defaultWebDir() string {
	if dir := strings.TrimSpace(os.Getenv("KEEL_SERVE_WEB_DIR")); dir != "" {
		return dir
	}
	if dir := bundledWebDir(); dir != "" {
		return dir
	}
	return defaultServeWebDir
}

func assetSignature() string {
	f, err := embeddedAssets.Open(embeddedPGRoot)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s %d", version, info.Size())
}

// ensureRuntime makes the embedded assets available and returns the directory
// that holds `pg/` and `web/`. It is idempotent and keyed by the binary's
// version + asset size, so rebuilding over a running install re-extracts once.
func ensureRuntime(stderr io.Writer) (string, error) {
	if res, ok := installedResources(); ok {
		return res, nil
	}
	root := runtimeRoot()
	if root == "" {
		return "", errors.New("cannot resolve KEEL_HOME for the runtime")
	}
	marker := filepath.Join(root, runtimeMarker)
	want := assetSignature()
	if want != "" {
		if got, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(got)) == want {
			return root, nil
		}
	}
	if !hasEmbeddedPG() {
		return "", errors.New("this build has no bundled PostgreSQL; run `keel db start` with the local PostgreSQL tools on PATH")
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return "", err
	}
	tmp := root + fmt.Sprintf(".tmp-%d", os.Getpid())
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return "", err
	}
	if err := extractPostgres(filepath.Join(tmp, "pg")); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	if err := copyEmbeddedDir(embeddedWeb, filepath.Join(tmp, "web")); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	if err := copyEmbeddedFile(embeddedIcon, filepath.Join(tmp, "icon.png")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	if err := copyEmbeddedFile(embeddedReadme, filepath.Join(tmp, "README.md")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	if want != "" {
		if err := os.WriteFile(filepath.Join(tmp, runtimeMarker), []byte(want+"\n"), 0o600); err != nil {
			_ = os.RemoveAll(tmp)
			return "", err
		}
	}
	_ = os.RemoveAll(root)
	if err := os.Rename(tmp, root); err != nil {
		return "", err
	}
	if stderr != nil {
		fmt.Fprintf(stderr, "keel: runtime extracted to %s\n", root)
	}
	return root, nil
}

func extractPostgres(dst string) error {
	f, err := embeddedAssets.Open(embeddedPGRoot)
	if err != nil {
		return err
	}
	defer f.Close()
	xr, err := xz.NewReader(f)
	if err != nil {
		return fmt.Errorf("read bundled PostgreSQL: %w", err)
	}
	return extractTar(xr, dst)
}

func extractTar(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read bundled PostgreSQL archive: %w", err)
		}
		name := filepath.Clean(hdr.Name)
		if name == "." || name == "" {
			continue
		}
		if filepath.IsAbs(name) || strings.HasPrefix(name, ".."+string(os.PathSeparator)) || name == ".." {
			return fmt.Errorf("bundled PostgreSQL archive has an unsafe path %q", hdr.Name)
		}
		target := filepath.Join(dst, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, hdr.FileInfo().Mode().Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Link(filepath.Join(dst, filepath.Clean(hdr.Linkname)), target); err != nil {
				return err
			}
		default:
			// Character/block devices and FIFOs are not part of a PostgreSQL
			// runtime; skipping them is safer than trying to create them.
		}
	}
}

func copyEmbeddedDir(root, dst string) error {
	return fs.WalkDir(embeddedAssets, root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(path, root+"/")
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		data, err := embeddedAssets.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

func copyEmbeddedFile(name, dst string) error {
	data, err := embeddedAssets.ReadFile(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

// copyTree copies a directory tree (used when an installed app is made from a
// runtime that was already extracted).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_ = os.Remove(target)
			return os.Symlink(link, target)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}
