package gbashfs

import (
	"context"
	"fmt"
	"path"
	"strings"

	gbash "github.com/ewhauser/gbash"
	gbfs "github.com/ewhauser/gbash/fs"

	"github.com/jixoai/keel/internal/layerfs"
)

// MountOptions configures one workspace mount.
type MountOptions struct {
	// Store is the layerfs store. Required when Workspace is nil.
	Store *layerfs.Store
	// Workspace is an existing session view. Nil builds a fresh one over Store.
	Workspace *Workspace
	// Point is the sandbox mount path. Empty uses DefaultMountPoint.
	Point string
	// WorkDir is the session's initial working directory. Empty uses Point.
	WorkDir string
}

// Mount is the value the bash bridge consumes: a non-root mount point, the
// shared workspace, and the gbash filesystem configuration that isolates the
// sandbox layout from layerfs.
type Mount struct {
	Point     string
	Workspace *Workspace
	// Factory returns the same workspace for every session, so a write made by
	// one keel component is visible to the next bash execution without a
	// remount (GBASH-CONTRACT §4.2).
	Factory gbfs.Factory
	// Config is the gbash.WithFileSystem argument.
	Config gbash.FileSystemConfig
}

// NewMount builds a layerfs-backed gbash mount. The mount point is validated:
// it must be absolute, must not be "/", and must not be one of the sandbox
// layout paths (/bin, /usr/bin, /tmp).
func NewMount(opts MountOptions) (Mount, error) {
	workspace := opts.Workspace
	if workspace == nil {
		var err error
		workspace, err = NewWorkspace(opts.Store)
		if err != nil {
			return Mount{}, err
		}
	}
	point, err := ValidateMountPoint(opts.Point)
	if err != nil {
		return Mount{}, err
	}
	workDir := strings.TrimSpace(opts.WorkDir)
	if workDir == "" {
		workDir = point
	}
	if !strings.HasPrefix(workDir, "/") {
		return Mount{}, fmt.Errorf("gbashfs: working directory %q is not absolute", workDir)
	}
	workDir = path.Clean(workDir)
	if workDir != point && !strings.HasPrefix(workDir, point+"/") {
		return Mount{}, fmt.Errorf("gbashfs: working directory %q is outside the mount point %q", workDir, point)
	}
	config, err := FileSystemConfig(workspace, point, workDir)
	if err != nil {
		return Mount{}, err
	}
	return Mount{
		Point:     point,
		Workspace: workspace,
		Factory:   gbfs.FactoryFunc(func(context.Context) (gbfs.FileSystem, error) { return workspace, nil }),
		Config:    config,
	}, nil
}

// FileSystemConfig builds the gbash filesystem configuration for a workspace.
// The base and the sandbox layout paths are memory filesystems: gbash writes
// its virtual command stubs and /bin, /usr/bin, /tmp there, never into layerfs.
func FileSystemConfig(workspace *Workspace, point, workDir string) (gbash.FileSystemConfig, error) {
	if workspace == nil {
		return gbash.FileSystemConfig{}, fmt.Errorf("gbashfs: a workspace is required")
	}
	point, err := ValidateMountPoint(point)
	if err != nil {
		return gbash.FileSystemConfig{}, err
	}
	if strings.TrimSpace(workDir) == "" {
		workDir = point
	}
	return gbash.MountableFileSystem(gbash.MountableFileSystemOptions{
		Base: gbfs.Memory(),
		Mounts: []gbfs.MountConfig{
			{MountPoint: point, Factory: gbfs.FactoryFunc(func(context.Context) (gbfs.FileSystem, error) {
				return workspace, nil
			})},
			{MountPoint: "/bin", Factory: gbfs.Memory()},
			{MountPoint: "/usr/bin", Factory: gbfs.Memory()},
			{MountPoint: "/tmp", Factory: gbfs.Memory()},
		},
		WorkingDir: workDir,
	}), nil
}

// ValidateMountPoint normalizes and validates a mount point. gbash itself
// refuses "/"; the reserved paths are refused here so "which backend owns this
// path" never depends on upstream layout internals.
func ValidateMountPoint(point string) (string, error) {
	point = strings.TrimSpace(point)
	if point == "" {
		point = DefaultMountPoint
	}
	if !strings.HasPrefix(point, "/") {
		return "", fmt.Errorf("gbashfs: mount point %q is not absolute", point)
	}
	point = path.Clean(point)
	if point == "/" {
		return "", fmt.Errorf("gbashfs: mount point must not be the sandbox root")
	}
	for _, reserved := range []string{"/bin", "/usr/bin", "/tmp"} {
		if point == reserved || strings.HasPrefix(point, reserved+"/") {
			return "", fmt.Errorf("gbashfs: mount point %q is reserved by the sandbox layout", point)
		}
	}
	return point, nil
}
