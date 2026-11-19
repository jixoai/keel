package fileplugin

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Surface is the materialized execution space's write-through seam. The
// file-plugin writes through to it after the durable write commits, so a
// subsequent bash call sees the new bytes without a remount. A nil surface
// means no execution space is attached (read/write still work on layerfs).
type Surface interface {
	// WriteFile stores r at the session-relative path, creating parent
	// directories as needed.
	WriteFile(path string, mode uint32, r io.Reader) error
}

// DirSurface is the local-directory execution space (tests and the local
// adapter). Writes are atomic: a temp file in the destination directory is
// renamed over the target.
type DirSurface struct {
	Root string
}

// WriteFile implements Surface.
func (d *DirSurface) WriteFile(path string, mode uint32, r io.Reader) error {
	if strings.TrimSpace(d.Root) == "" {
		return fmt.Errorf("fileplugin: dir surface has no root")
	}
	rel, err := NormalizePath(path)
	if err != nil {
		return err
	}
	target := filepath.Join(d.Root, filepath.FromSlash(rel))
	rootAbs, err := filepath.Abs(d.Root)
	if err != nil {
		return err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("fileplugin: path %q escapes the execution space", path)
	}
	if err := os.MkdirAll(filepath.Dir(targetAbs), 0o755); err != nil {
		return fmt.Errorf("fileplugin: create execution-space parent: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(targetAbs), ".keel-write-*")
	if err != nil {
		return fmt.Errorf("fileplugin: create execution-space temp: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := io.Copy(temp, r); err != nil {
		temp.Close()
		return fmt.Errorf("fileplugin: write execution-space file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("fileplugin: sync execution-space file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("fileplugin: close execution-space file: %w", err)
	}
	perm := os.FileMode(mode & 0o777)
	if perm == 0 {
		perm = 0o644
	}
	if err := os.Chmod(tempPath, perm); err != nil {
		return fmt.Errorf("fileplugin: chmod execution-space file: %w", err)
	}
	if err := os.Rename(tempPath, targetAbs); err != nil {
		return fmt.Errorf("fileplugin: commit execution-space file: %w", err)
	}
	return nil
}

// NormalizePath maps a model-supplied path onto the session-relative namespace.
// `/workspace/a` and `/a` both mean `a`; `..` is refused. The session's
// workspace is the only root a tool path can name.
func NormalizePath(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", Errorf(CodeInvalid, "path is required")
	}
	if strings.ContainsRune(value, 0) {
		return "", Errorf(CodeInvalid, "path contains a NUL byte")
	}
	value = strings.ReplaceAll(value, "\\", "/")
	value = strings.TrimPrefix(value, "/workspace/")
	value = strings.TrimPrefix(value, "/")
	clean := filepath.ToSlash(filepath.Clean(value))
	if clean == "." || clean == "" {
		return "", Errorf(CodeInvalid, "path %q does not name a file", raw)
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", Errorf(CodeInvalid, "path %q escapes the workspace", raw)
	}
	return clean, nil
}
