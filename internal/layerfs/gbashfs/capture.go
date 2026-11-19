package gbashfs

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/jixoai/keel/internal/layerfs"
)

// Capture snapshots the workspace into layerfs objects and returns the batch
// fact one fs.capture event carries. It is the execution space's capture seam:
// unlike layerfs.Store.Capture it never walks a host directory, because the
// workspace is an in-memory path index over the CAS (bash writes enter the CAS
// at file close, not at capture time).
//
// The snapshot carries every regular file as a (path, hash, mode, size) entry
// and every directory as tree objects, using the same canonical tree form as
// layerfs.Store.Capture. Symlinks are counted as skipped: the layerfs tree model
// has no symlink kind, exactly like a directory capture.
//
// It emits nothing; the caller commits exactly one fs.capture for the returned
// snapshot (fileplugin.CaptureWorkspace).
func (w *Workspace) Capture(ctx context.Context) (layerfs.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return layerfs.Snapshot{}, err
	}
	w.mu.RLock()
	nodes := make(map[string]node, len(w.nodes))
	for candidate, n := range w.nodes {
		nodes[candidate] = *n
	}
	w.mu.RUnlock()

	var (
		snapshot   layerfs.Snapshot
		files      []layerfs.FileEntry
		dirs       = []string{""}
		dirModes   = map[string]uint32{}
		dirEntries = map[string][]layerfs.TreeEntry{}
	)
	if root, ok := nodes["/"]; ok && root.kind == nodeDir {
		dirModes[""] = uint32(root.mode.Perm())
	} else {
		dirModes[""] = 0o755
	}
	for candidate, n := range nodes {
		if candidate == "/" {
			continue
		}
		rel := strings.TrimPrefix(candidate, "/")
		if captureIgnored(rel) {
			continue
		}
		name := path.Base(rel)
		parent := captureParentDir(rel)
		switch n.kind {
		case nodeDir:
			dirs = append(dirs, rel)
			dirModes[rel] = uint32(n.mode.Perm())
		case nodeFile:
			hash := n.hash
			if !hash.Valid() {
				if n.size != 0 {
					return layerfs.Snapshot{}, fmt.Errorf("gbashfs: capture: file %q has no content hash", candidate)
				}
				hash = layerfs.HashBytes(nil)
			}
			mode := uint32(n.mode.Perm())
			files = append(files, layerfs.FileEntry{Path: rel, Hash: hash, Mode: mode, Size: n.size})
			dirEntries[parent] = append(dirEntries[parent], layerfs.TreeEntry{
				Name: name, Kind: layerfs.TreeFile, Hash: hash, Mode: mode, Size: n.size,
			})
			snapshot.Bytes += n.size
			// File bytes enter the CAS when the handle closes, before capture
			// runs; the capture itself stores no file object. The count is honest
			// for this snapshot operation (tree objects are not counted, matching
			// layerfs.Store.Capture).
			snapshot.ReusedObjects++
		case nodeSymlink:
			snapshot.Skipped++
		}
	}

	// Build child directories before their parents (depth descending), exactly
	// like layerfs.Store.Capture: the root is depth 0, a top-level directory is
	// depth 1.
	sort.Slice(dirs, func(i, j int) bool {
		di, dj := captureDirDepth(dirs[i]), captureDirDepth(dirs[j])
		if di != dj {
			return di > dj
		}
		return dirs[i] < dirs[j]
	})
	for _, dir := range dirs {
		treeHash, err := w.store.PutTree(dirEntries[dir])
		if err != nil {
			return layerfs.Snapshot{}, fmt.Errorf("gbashfs: capture: store tree %q: %w", dir, err)
		}
		if dir == "" {
			snapshot.Tree = treeHash
			continue
		}
		parent := captureParentDir(dir)
		dirEntries[parent] = append(dirEntries[parent], layerfs.TreeEntry{
			Name: path.Base(dir), Kind: layerfs.TreeDir, Hash: treeHash, Mode: dirModes[dir],
		})
	}
	if !snapshot.Tree.Valid() {
		return layerfs.Snapshot{}, fmt.Errorf("gbashfs: capture produced no root tree")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	snapshot.Files = files
	snapshot.Count = len(files)
	return snapshot, nil
}

func captureParentDir(rel string) string {
	parent := path.Dir(rel)
	if parent == "." {
		return ""
	}
	return parent
}

// captureIgnored applies layerfs' default ignore list to the workspace index.
// The default is only .git/objects (it is content-addressed itself), exactly
// like a directory capture.
func captureIgnored(rel string) bool {
	for _, prefix := range layerfs.DefaultIgnore {
		if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
			return true
		}
	}
	return false
}

func captureDirDepth(dir string) int {
	if dir == "" {
		return 0
	}
	return strings.Count(dir, "/") + 1
}
