package bashbridge

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"

	gbfs "github.com/ewhauser/gbash/fs"
)

// DefaultMountPoint is the sandbox path the workspace is mounted at. It must
// never be "/": gbash refuses to mount at the root, and mounting the workspace
// as the root would let gbash's sandbox layout (virtual command stubs, /bin,
// /usr/bin, /tmp) pollute the injected backend (GBASH-CONTRACT §4.2).
const DefaultMountPoint = "/workspace"

// Mount is one session's workspace mount.
//
// FS is deliberately opaque (any): the default implementation returns a gbash
// filesystem factory, and a later lane (the layerfs write-through adapter) may
// return either a gbash fs.Factory or a gbash fs.FileSystem. Keeping the value
// opaque is what lets gbash's types stay out of this package's exported
// signatures while still allowing an out-of-package backend to implement the
// seam (GBASH-CONTRACT §7.2 "类型不出包"). An implementation must import
// gbash/fs itself; no other keel package may.
type Mount struct {
	// Point is the sandbox path (absolute, not "/").
	Point string
	// FS implements gbash's fs.Factory or fs.FileSystem.
	FS any
	// Cleanup releases per-session resources (the default removes its temp dir).
	Cleanup func() error
}

// Workspace supplies the session's writable workspace at a non-root mount.
//
// The default is TempDirWorkspace: a fresh host temp directory per bridge
// session, mounted read-write and removed when the session closes. It is a
// short-lived scratch root, not content storage — the production content
// backend is replaced by the layerfs adapter in a later lane.
type Workspace interface {
	Mount(ctx context.Context) (Mount, error)
}

// TempDirWorkspace creates one host temp directory per bridge session under
// Root (os.TempDir by default).
//
// Writes go to that scratch directory and nowhere else. The directory is
// removed on session close; nothing in it survives the call, which is exactly
// why gbash's read-write host backend is acceptable here ("short-lived temp
// roots" is the case upstream's threat model allows) and unacceptable for a
// content backend (GBASH-CONTRACT §4.2).
type TempDirWorkspace struct {
	Root string
}

// Mount implements Workspace.
func (w TempDirWorkspace) Mount(_ context.Context) (Mount, error) {
	root := strings.TrimSpace(w.Root)
	if root == "" {
		root = os.TempDir()
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Mount{}, fmt.Errorf("bashbridge: workspace root: %w", err)
	}
	dir, err := os.MkdirTemp(root, "keel-bash-workspace-*")
	if err != nil {
		return Mount{}, fmt.Errorf("bashbridge: workspace temp dir: %w", err)
	}
	return Mount{
		Point: DefaultMountPoint,
		FS:    gbfs.ReadWrite(gbfs.ReadWriteOptions{Root: dir}),
		Cleanup: func() error {
			return os.RemoveAll(dir)
		},
	}, nil
}

// mountFactory converts Mount.FS into the factory a gbash FileSystemConfig
// needs, and validates the mount point.
func mountFactory(mount Mount) (gbfs.Factory, string, error) {
	point := strings.TrimSpace(mount.Point)
	if point == "" {
		point = DefaultMountPoint
	}
	if !strings.HasPrefix(point, "/") {
		return nil, "", fmt.Errorf("bashbridge: workspace mount point %q is not absolute", point)
	}
	point = path.Clean(point)
	if point == "/" {
		return nil, "", fmt.Errorf("bashbridge: workspace mount point must not be the sandbox root")
	}
	if point == "/bin" || point == "/usr/bin" || point == "/tmp" {
		return nil, "", fmt.Errorf("bashbridge: workspace mount point %q is reserved by the sandbox layout", point)
	}
	switch fsValue := mount.FS.(type) {
	case nil:
		return nil, "", fmt.Errorf("bashbridge: workspace mount %q has no filesystem", point)
	case gbfs.Factory:
		return fsValue, point, nil
	case gbfs.FileSystem:
		return gbfs.FactoryFunc(func(context.Context) (gbfs.FileSystem, error) {
			return fsValue, nil
		}), point, nil
	default:
		return nil, "", fmt.Errorf("bashbridge: workspace value %T is not a gbash filesystem or factory", mount.FS)
	}
}
