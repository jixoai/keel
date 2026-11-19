// Package gbashfs exposes a layerfs-backed workspace to gbash as a custom
// filesystem. It is the adapter the bash bridge mounts at a non-root mount
// point (docs/GBASH-CONTRACT.md §4.2).
//
// The package is the only layerfs package allowed to import gbash: the core
// layerfs stays path-free and gbash-free, and the adapter owns the path index
// (which files a session's sandbox sees) plus the gbash FileSystem contract.
// File bytes are never held in the index: a file node is (content hash, mode,
// size), and reads stream from the CAS. Writes spool to a temporary file and
// enter the CAS on Close, so a large file costs one chunk of memory.
//
// Mount policy: a workspace is only ever mounted at a non-root, non-reserved
// mount point. gbash writes ~263 virtual command stubs into the base filesystem
// during session initialization; mounting layerfs at "/" would pollute the
// content-addressed store with fake /bin and /usr/bin executables. The base
// filesystem is always gbash's in-memory FS, and /bin, /usr/bin and /tmp are
// explicitly mounted to memory as well.
package gbashfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	stdfs "io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	gbashfs "github.com/ewhauser/gbash/fs"

	"github.com/jixoai/keel/internal/layerfs"
)

// DefaultMountPoint is the sandbox path a workspace is mounted at.
const DefaultMountPoint = "/workspace"

// DefaultOwnerUID / DefaultOwnerGID mirror gbash's in-memory sandbox owner.
const (
	DefaultOwnerUID = 1000
	DefaultOwnerGID = 1000
)

type nodeKind uint8

const (
	nodeDir nodeKind = iota
	nodeFile
	nodeSymlink
)

type node struct {
	kind   nodeKind
	hash   layerfs.Hash
	mode   stdfs.FileMode // permission bits only; the type comes from kind
	size   int64
	atime  time.Time
	mtime  time.Time
	uid    uint32
	gid    uint32
	target string // symlink target
}

// Workspace is one session's path index over a layerfs store. It implements
// gbash's fs.FileSystem (17 methods) and adds Snapshot/Seed/WriteFile for the
// file-plugin's capture and write-through seams.
type Workspace struct {
	store  *layerfs.Store
	tmpDir string
	now    func() time.Time

	mu    sync.RWMutex
	nodes map[string]*node
	cwd   string
}

// NewWorkspace builds an empty workspace rooted at "/".
func NewWorkspace(store *layerfs.Store) (*Workspace, error) {
	if store == nil {
		return nil, errors.New("gbashfs: a layerfs store is required")
	}
	now := time.Now().UTC()
	workspace := &Workspace{
		store:  store,
		tmpDir: path.Join(store.Root(), ".tmp"),
		now:    func() time.Time { return time.Now().UTC() },
		nodes: map[string]*node{
			"/": {kind: nodeDir, mode: 0o755, atime: now, mtime: now, uid: DefaultOwnerUID, gid: DefaultOwnerGID},
		},
		cwd: "/",
	}
	if err := os.MkdirAll(workspace.tmpDir, 0o700); err != nil {
		return nil, fmt.Errorf("gbashfs: temp dir: %w", err)
	}
	return workspace, nil
}

// Store returns the backing layerfs store.
func (w *Workspace) Store() *layerfs.Store { return w.store }

// SetClock overrides the clock (tests).
func (w *Workspace) SetClock(now func() time.Time) {
	if now != nil {
		w.now = now
	}
}

// --- path resolution -------------------------------------------------------

func (w *Workspace) resolve(name string) string {
	if strings.HasPrefix(name, "/") {
		return path.Clean(name)
	}
	return path.Clean(path.Join(w.cwd, name))
}

// lookup resolves a path, following symlinks in the middle (and at the end when
// followFinal is true). The returned path is the physical path inside the
// workspace.
func (w *Workspace) lookup(name string, followFinal bool) (string, *node, error) {
	return w.lookupAbs(w.resolve(name), followFinal, 0)
}

func (w *Workspace) lookupAbs(abs string, followFinal bool, depth int) (string, *node, error) {
	if depth > 40 {
		return abs, nil, syscall.ELOOP
	}
	if abs == "/" {
		return "/", w.nodes["/"], nil
	}
	parts := strings.Split(strings.TrimPrefix(abs, "/"), "/")
	current := ""
	for index, part := range parts {
		next := current + "/" + part
		n := w.nodes[next]
		if n == nil {
			return next, nil, stdfs.ErrNotExist
		}
		final := index == len(parts)-1
		if n.kind == nodeSymlink && (!final || followFinal) {
			target := n.target
			var targetAbs string
			if strings.HasPrefix(target, "/") {
				targetAbs = path.Clean(target)
			} else {
				targetAbs = path.Clean(path.Join(current, target))
			}
			if rest := strings.Join(parts[index+1:], "/"); rest != "" {
				targetAbs = path.Join(targetAbs, rest)
			}
			return w.lookupAbs(targetAbs, followFinal, depth+1)
		}
		if !final && n.kind != nodeDir {
			return next, nil, syscall.ENOTDIR
		}
		current = next
	}
	return current, w.nodes[current], nil
}

// parentOf resolves the parent directory of a path that does not exist yet.
func (w *Workspace) parentOf(name string) (string, *node, error) {
	abs := w.resolve(name)
	if abs == "/" {
		return "/", w.nodes["/"], nil
	}
	parent := path.Dir(abs)
	_, n, err := w.lookupAbs(parent, true, 0)
	if err != nil {
		return parent, nil, err
	}
	if n.kind != nodeDir {
		return parent, nil, syscall.ENOTDIR
	}
	return parent, n, nil
}

func (w *Workspace) newDir(mode stdfs.FileMode) *node {
	now := w.now()
	return &node{kind: nodeDir, mode: mode & stdfs.ModePerm, atime: now, mtime: now, uid: DefaultOwnerUID, gid: DefaultOwnerGID}
}

func (w *Workspace) newFile(mode stdfs.FileMode) *node {
	now := w.now()
	return &node{kind: nodeFile, mode: mode & stdfs.ModePerm, atime: now, mtime: now, uid: DefaultOwnerUID, gid: DefaultOwnerGID}
}

// --- gbash fs.FileSystem ---------------------------------------------------

// Open opens a file for reading.
func (w *Workspace) Open(_ context.Context, name string) (gbashfs.File, error) {
	return w.OpenFile(context.Background(), name, os.O_RDONLY, 0)
}

// OpenFile opens (and optionally creates/truncates) a file.
func (w *Workspace) OpenFile(_ context.Context, name string, flag int, perm stdfs.FileMode) (gbashfs.File, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	abs, existing, err := w.lookup(name, true)
	if err != nil {
		if !errors.Is(err, stdfs.ErrNotExist) {
			return nil, pathError("open", w.resolve(name), err)
		}
		if flag&os.O_CREATE == 0 {
			return nil, pathError("open", w.resolve(name), stdfs.ErrNotExist)
		}
		parent := path.Dir(abs)
		if n := w.nodes[parent]; n == nil || n.kind != nodeDir {
			return nil, pathError("open", w.resolve(name), stdfs.ErrNotExist)
		}
		mode := perm & stdfs.ModePerm
		if mode == 0 {
			mode = 0o644
		}
		existing = w.newFile(mode)
		w.nodes[abs] = existing
	} else if flag&os.O_CREATE != 0 && flag&os.O_EXCL != 0 {
		return nil, pathError("open", abs, stdfs.ErrExist)
	}
	if existing.kind == nodeDir {
		return nil, pathError("open", abs, syscall.EISDIR)
	}
	if existing.kind == nodeSymlink {
		return nil, pathError("open", abs, syscall.ELOOP)
	}
	writable := flag&(os.O_WRONLY|os.O_RDWR) != 0
	if !writable {
		return w.openRead(abs, existing, flag)
	}
	return w.openWrite(abs, existing, flag, perm)
}

func (w *Workspace) openRead(abs string, n *node, flag int) (gbashfs.File, error) {
	if n.size == 0 {
		handle, err := os.CreateTemp(w.tmpDir, "read-*")
		if err != nil {
			return nil, pathError("open", abs, err)
		}
		return &fileHandle{ws: w, path: abs, file: handle, tmpPath: handle.Name(), flag: flag, mode: n.mode}, nil
	}
	object, err := w.store.Open(n.hash)
	if err != nil {
		return nil, pathError("open", abs, err)
	}
	return &fileHandle{ws: w, path: abs, file: object.File, flag: flag, mode: n.mode, size: n.size}, nil
}

func (w *Workspace) openWrite(abs string, n *node, flag int, perm stdfs.FileMode) (gbashfs.File, error) {
	handle, err := os.CreateTemp(w.tmpDir, "write-*")
	if err != nil {
		return nil, pathError("open", abs, err)
	}
	size := int64(0)
	if flag&os.O_TRUNC == 0 && n.size > 0 {
		object, err := w.store.Open(n.hash)
		if err != nil {
			handle.Close()
			os.Remove(handle.Name())
			return nil, pathError("open", abs, err)
		}
		size, err = io.Copy(handle, object)
		object.Close()
		if err != nil {
			handle.Close()
			os.Remove(handle.Name())
			return nil, pathError("open", abs, err)
		}
	}
	if flag&os.O_APPEND != 0 {
		if _, err := handle.Seek(size, io.SeekStart); err != nil {
			handle.Close()
			os.Remove(handle.Name())
			return nil, pathError("open", abs, err)
		}
	} else if _, err := handle.Seek(0, io.SeekStart); err != nil {
		handle.Close()
		os.Remove(handle.Name())
		return nil, pathError("open", abs, err)
	}
	mode := n.mode
	if perm&stdfs.ModePerm != 0 {
		mode = perm & stdfs.ModePerm
	}
	return &fileHandle{ws: w, path: abs, file: handle, tmpPath: handle.Name(), flag: flag, write: true, mode: mode, size: size}, nil
}

// Stat follows symlinks.
func (w *Workspace) Stat(_ context.Context, name string) (stdfs.FileInfo, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	abs, n, err := w.lookup(name, true)
	if err != nil {
		return nil, pathError("stat", w.resolve(name), err)
	}
	return infoFor(path.Base(abs), n), nil
}

// Lstat does not follow a final symlink.
func (w *Workspace) Lstat(_ context.Context, name string) (stdfs.FileInfo, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	abs, n, err := w.lookup(name, false)
	if err != nil {
		return nil, pathError("lstat", w.resolve(name), err)
	}
	return infoFor(path.Base(abs), n), nil
}

// ReadDir lists a directory.
func (w *Workspace) ReadDir(_ context.Context, name string) ([]stdfs.DirEntry, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	abs, n, err := w.lookup(name, true)
	if err != nil {
		return nil, pathError("readdir", w.resolve(name), err)
	}
	if n.kind != nodeDir {
		return nil, pathError("readdir", abs, syscall.ENOTDIR)
	}
	names := make([]string, 0)
	for candidate := range w.nodes {
		if candidate == abs {
			continue
		}
		if path.Dir(candidate) == abs {
			names = append(names, path.Base(candidate))
		}
	}
	sort.Strings(names)
	entries := make([]stdfs.DirEntry, 0, len(names))
	for _, child := range names {
		childPath := path.Join(abs, child)
		entries = append(entries, dirEntry{name: child, info: infoFor(child, w.nodes[childPath])})
	}
	return entries, nil
}

// Readlink returns a symlink's target.
func (w *Workspace) Readlink(_ context.Context, name string) (string, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	abs, n, err := w.lookup(name, false)
	if err != nil {
		return "", pathError("readlink", w.resolve(name), err)
	}
	if n.kind != nodeSymlink {
		return "", pathError("readlink", abs, syscall.EINVAL)
	}
	return n.target, nil
}

// Realpath resolves a path physically.
func (w *Workspace) Realpath(_ context.Context, name string) (string, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	abs, _, err := w.lookup(name, true)
	if err != nil {
		return "", pathError("realpath", w.resolve(name), err)
	}
	return abs, nil
}

// Symlink creates a symbolic link.
func (w *Workspace) Symlink(_ context.Context, target, linkName string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	abs, _, err := w.lookup(linkName, false)
	if err == nil {
		return pathError("symlink", abs, stdfs.ErrExist)
	}
	if !errors.Is(err, stdfs.ErrNotExist) {
		return pathError("symlink", w.resolve(linkName), err)
	}
	parent, _, err := w.parentOf(linkName)
	if err != nil {
		return pathError("symlink", w.resolve(linkName), err)
	}
	abs = path.Join(parent, path.Base(w.resolve(linkName)))
	now := w.now()
	w.nodes[abs] = &node{kind: nodeSymlink, mode: 0o777, size: int64(len(target)), target: target, atime: now, mtime: now, uid: DefaultOwnerUID, gid: DefaultOwnerGID}
	return nil
}

// Link creates a hard link.
func (w *Workspace) Link(_ context.Context, oldName, newName string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, old, err := w.lookup(oldName, true)
	if err != nil {
		return pathError("link", w.resolve(oldName), err)
	}
	if old.kind != nodeFile {
		return pathError("link", w.resolve(oldName), syscall.EPERM)
	}
	abs, _, err := w.lookup(newName, false)
	if err == nil {
		return pathError("link", abs, stdfs.ErrExist)
	}
	if !errors.Is(err, stdfs.ErrNotExist) {
		return pathError("link", w.resolve(newName), err)
	}
	parent, _, err := w.parentOf(newName)
	if err != nil {
		return pathError("link", w.resolve(newName), err)
	}
	abs = path.Join(parent, path.Base(w.resolve(newName)))
	copied := *old
	w.nodes[abs] = &copied
	return nil
}

// Chown changes ownership.
func (w *Workspace) Chown(_ context.Context, name string, uid, gid uint32, follow bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	abs, n, err := w.lookup(name, follow)
	if err != nil {
		return pathError("chown", w.resolve(name), err)
	}
	_ = abs
	n.uid, n.gid = uid, gid
	return nil
}

// Chmod changes permission bits.
func (w *Workspace) Chmod(_ context.Context, name string, mode stdfs.FileMode) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, n, err := w.lookup(name, true)
	if err != nil {
		return pathError("chmod", w.resolve(name), err)
	}
	n.mode = mode & stdfs.ModePerm
	return nil
}

// Chtimes changes access and modification times.
func (w *Workspace) Chtimes(_ context.Context, name string, atime, mtime time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, n, err := w.lookup(name, true)
	if err != nil {
		return pathError("chtimes", w.resolve(name), err)
	}
	n.atime, n.mtime = atime, mtime
	return nil
}

// MkdirAll creates a directory and its parents.
func (w *Workspace) MkdirAll(_ context.Context, name string, perm stdfs.FileMode) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	abs := w.resolve(name)
	if abs == "/" {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(abs, "/"), "/")
	current := ""
	for _, part := range parts {
		current += "/" + part
		n := w.nodes[current]
		if n == nil {
			mode := perm & stdfs.ModePerm
			if mode == 0 {
				mode = 0o755
			}
			w.nodes[current] = w.newDir(mode)
			continue
		}
		if n.kind != nodeDir {
			return pathError("mkdir", current, syscall.ENOTDIR)
		}
	}
	return nil
}

// Remove removes a file, symlink or directory.
func (w *Workspace) Remove(_ context.Context, name string, recursive bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	abs, n, err := w.lookup(name, false)
	if err != nil {
		return pathError("remove", w.resolve(name), err)
	}
	if abs == "/" {
		return pathError("remove", abs, syscall.EINVAL)
	}
	if n.kind == nodeDir {
		children := w.childrenLocked(abs)
		if len(children) > 0 && !recursive {
			return pathError("remove", abs, syscall.ENOTEMPTY)
		}
		for _, child := range children {
			delete(w.nodes, child)
		}
	}
	delete(w.nodes, abs)
	return nil
}

// Rename moves a path.
func (w *Workspace) Rename(_ context.Context, oldName, newName string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	oldAbs, old, err := w.lookup(oldName, false)
	if err != nil {
		return pathError("rename", w.resolve(oldName), err)
	}
	if oldAbs == "/" {
		return pathError("rename", oldAbs, syscall.EINVAL)
	}
	if oldAbs == w.resolve(newName) {
		return nil
	}
	newAbs, _, err := w.lookup(newName, false)
	if err != nil && !errors.Is(err, stdfs.ErrNotExist) {
		return pathError("rename", w.resolve(newName), err)
	}
	if err == nil {
		if strings.HasPrefix(newAbs+"/", oldAbs+"/") {
			return pathError("rename", newAbs, syscall.EINVAL)
		}
		if existing := w.nodes[newAbs]; existing != nil {
			if existing.kind == nodeDir {
				if old.kind != nodeDir {
					return pathError("rename", newAbs, syscall.EISDIR)
				}
				if len(w.childrenLocked(newAbs)) > 0 {
					return pathError("rename", newAbs, syscall.ENOTEMPTY)
				}
			}
			delete(w.nodes, newAbs)
		}
	} else {
		newAbs, _, err = w.parentOf(newName)
		if err != nil {
			return pathError("rename", w.resolve(newName), err)
		}
		// parentOf returns the parent path; append the final component.
		newAbs = path.Join(newAbs, path.Base(w.resolve(newName)))
	}
	oldPath := oldAbs
	newPath := newAbs
	for candidate, n := range w.nodes {
		if candidate != oldPath && !strings.HasPrefix(candidate, oldPath+"/") {
			continue
		}
		moved := newPath + strings.TrimPrefix(candidate, oldPath)
		delete(w.nodes, candidate)
		w.nodes[moved] = n
	}
	return nil
}

// Getwd returns the working directory.
func (w *Workspace) Getwd() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.cwd
}

// Chdir changes the working directory.
func (w *Workspace) Chdir(name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	abs, n, err := w.lookup(name, true)
	if err != nil {
		return pathError("chdir", w.resolve(name), err)
	}
	if n.kind != nodeDir {
		return pathError("chdir", abs, syscall.ENOTDIR)
	}
	w.cwd = abs
	return nil
}

// --- layerfs seams ---------------------------------------------------------

// Snapshot returns every regular file as a workspace-relative entry. It is the
// input a capture reconciles into the manifest.
func (w *Workspace) Snapshot() []layerfs.FileEntry {
	w.mu.RLock()
	defer w.mu.RUnlock()
	files := make([]layerfs.FileEntry, 0)
	for candidate, n := range w.nodes {
		if n.kind != nodeFile || candidate == "/" {
			continue
		}
		hash := n.hash
		if !hash.Valid() && n.size == 0 {
			hash = layerfs.HashBytes(nil)
		}
		files = append(files, layerfs.FileEntry{
			Path: strings.TrimPrefix(candidate, "/"),
			Hash: hash,
			Mode: uint32(n.mode.Perm()),
			Size: n.size,
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

// Seed replaces the workspace's files with entries (a session materialization).
// Parent directories are created as needed.
func (w *Workspace) Seed(entries []layerfs.FileEntry) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	w.nodes = map[string]*node{
		"/": {kind: nodeDir, mode: 0o755, atime: now, mtime: now, uid: DefaultOwnerUID, gid: DefaultOwnerGID},
	}
	for _, entry := range entries {
		raw := strings.TrimPrefix(entry.Path, "/")
		for _, part := range strings.Split(raw, "/") {
			if part == ".." {
				return fmt.Errorf("gbashfs: invalid seed path %q", entry.Path)
			}
		}
		clean := path.Clean("/" + raw)
		if clean == "/" {
			return fmt.Errorf("gbashfs: invalid seed path %q", entry.Path)
		}
		parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
		current := ""
		for _, part := range parts[:len(parts)-1] {
			current += "/" + part
			if w.nodes[current] == nil {
				w.nodes[current] = w.newDir(0o755)
			}
		}
		w.nodes[clean] = &node{
			kind: nodeFile, hash: entry.Hash, mode: stdfs.FileMode(entry.Mode) & stdfs.ModePerm,
			size: entry.Size, atime: now, mtime: now, uid: DefaultOwnerUID, gid: DefaultOwnerGID,
		}
	}
	return nil
}

// WriteFile stores r as the file's new content and updates the path index. It
// is the write-through seam: after it returns, a gbash session using this
// workspace reads the new bytes without a remount.
func (w *Workspace) WriteFile(name string, mode uint32, r io.Reader) error {
	result, err := w.store.PutStatus(r)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	abs := w.resolve(name)
	if abs == "/" {
		return fmt.Errorf("gbashfs: cannot write the root")
	}
	parts := strings.Split(strings.TrimPrefix(abs, "/"), "/")
	current := ""
	for _, part := range parts[:len(parts)-1] {
		current += "/" + part
		n := w.nodes[current]
		if n == nil {
			w.nodes[current] = w.newDir(0o755)
			continue
		}
		if n.kind != nodeDir {
			return pathError("write", abs, syscall.ENOTDIR)
		}
	}
	n := w.nodes[abs]
	if n != nil && n.kind == nodeDir {
		return pathError("write", abs, syscall.EISDIR)
	}
	if n == nil || n.kind != nodeFile {
		n = w.newFile(stdfs.FileMode(mode) & stdfs.ModePerm)
		if n.mode == 0 {
			n.mode = 0o644
		}
		w.nodes[abs] = n
	}
	n.hash = result.Hash
	n.size = result.Size
	n.mtime = w.now()
	if mode != 0 {
		n.mode = stdfs.FileMode(mode) & stdfs.ModePerm
	}
	return nil
}

// RemoveFile removes a path from the index (write-through for a delete).
func (w *Workspace) RemoveFile(name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	abs, n, err := w.lookup(name, false)
	if err != nil {
		return err
	}
	if n.kind == nodeDir {
		for _, child := range w.childrenLocked(abs) {
			delete(w.nodes, child)
		}
	}
	delete(w.nodes, abs)
	return nil
}

// childrenLocked returns every descendant path of a directory.
func (w *Workspace) childrenLocked(abs string) []string {
	children := make([]string, 0)
	prefix := abs + "/"
	if abs == "/" {
		prefix = "/"
	}
	for candidate := range w.nodes {
		if candidate != abs && strings.HasPrefix(candidate, prefix) {
			children = append(children, candidate)
		}
	}
	return children
}

func pathError(op, name string, err error) error {
	return &os.PathError{Op: op, Path: name, Err: err}
}
