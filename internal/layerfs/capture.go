package layerfs

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// DefaultIgnore is the capture ignore list: only `.git/objects` (it is itself
// content-addressed, so capturing it would double-store the same bytes).
var DefaultIgnore = []string{".git/objects"}

// CaptureOptions tunes a workspace snapshot.
type CaptureOptions struct {
	// Ignore is a list of workspace-relative path prefixes to skip. A nil slice
	// uses DefaultIgnore; an explicitly empty slice captures everything.
	Ignore []string
	// Cache lets repeated captures reuse hashes for files whose size and mtime
	// did not change. Nil means every file is hashed (a first capture).
	Cache *StatCache
}

// Snapshot is one workspace capture. It is the batch fact a single fs.capture
// event carries: a tree hash, a count and the flat file list the manifest
// upserts.
type Snapshot struct {
	// Tree is the root tree object hash.
	Tree Hash
	// Files is the flat file list, sorted by path.
	Files []FileEntry
	// Count is len(Files).
	Count int
	// Bytes is the sum of file sizes.
	Bytes int64
	// NewObjects is how many file objects were newly stored.
	NewObjects int
	// ReusedObjects is how many files reused an existing object (dedupe or a
	// cache hit).
	ReusedObjects int
	// Skipped counts non-regular entries (symlinks, devices, sockets).
	Skipped int
}

// StatCache remembers (size, mtime, mode) → hash per path. Capture only hashes
// files whose stat signature changed, so a repeated capture of an unchanged
// tree stores nothing new and rehashes nothing.
type StatCache struct {
	mu    sync.Mutex
	files map[string]cachedStat
	puts  int
}

type cachedStat struct {
	size    int64
	modTime int64
	mode    uint32
	hash    Hash
}

// NewStatCache builds an empty cache.
func NewStatCache() *StatCache {
	return &StatCache{files: make(map[string]cachedStat)}
}

// PutCount reports how many PutFile calls this cache observed (test evidence).
func (c *StatCache) PutCount() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.puts
}

func (c *StatCache) lookup(rel string, info fs.FileInfo) (Hash, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cached, ok := c.files[rel]
	if !ok {
		return "", false
	}
	if cached.size != info.Size() || cached.modTime != info.ModTime().UnixNano() || cached.mode != uint32(info.Mode().Perm()) {
		return "", false
	}
	return cached.hash, true
}

func (c *StatCache) store(rel string, info fs.FileInfo, hash Hash) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.files[rel] = cachedStat{
		size:    info.Size(),
		modTime: info.ModTime().UnixNano(),
		mode:    uint32(info.Mode().Perm()),
		hash:    hash,
	}
	c.puts++
}

// Capture walks root, stores changed files and returns the workspace tree. It
// never emits an event: the file-plugin commits exactly one fs.capture for the
// returned snapshot.
func (s *Store) Capture(root string, opts CaptureOptions) (Snapshot, error) {
	if s.closed.Load() {
		return Snapshot{}, fmt.Errorf("%w: store is closed", ErrUnavailable)
	}
	absRoot, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: resolve capture root: %v", ErrInvalid, err)
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: capture root: %v", ErrIO, err)
	}
	if !info.IsDir() {
		return Snapshot{}, fmt.Errorf("%w: capture root %s is not a directory", ErrInvalid, absRoot)
	}
	ignore := opts.Ignore
	if ignore == nil {
		ignore = DefaultIgnore
	}
	ignore = normalizeIgnore(ignore)

	var (
		files      []FileEntry
		dirs       = []string{""}
		dirModes   = map[string]uint32{"": uint32(info.Mode().Perm())}
		dirEntries = map[string][]TreeEntry{}
		snapshot   Snapshot
	)
	err = filepath.WalkDir(absRoot, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == absRoot {
			return nil
		}
		rel, err := filepath.Rel(absRoot, current)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if ignored(rel, ignore) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		parent := parentDir(rel)
		name := path.Base(rel)
		switch {
		case entry.IsDir():
			dirs = append(dirs, rel)
			dirModes[rel] = uint32(info.Mode().Perm())
		case info.Mode().IsRegular():
			hash, ok := opts.Cache.lookup(rel, info)
			if ok {
				snapshot.ReusedObjects++
			} else {
				result, err := s.PutFile(current)
				if err != nil {
					return err
				}
				hash = result.Hash
				if result.Created {
					snapshot.NewObjects++
				} else {
					snapshot.ReusedObjects++
				}
				opts.Cache.store(rel, info, hash)
			}
			mode := uint32(info.Mode().Perm())
			files = append(files, FileEntry{Path: rel, Hash: hash, Mode: mode, Size: info.Size()})
			dirEntries[parent] = append(dirEntries[parent], TreeEntry{Name: name, Kind: TreeFile, Hash: hash, Mode: mode, Size: info.Size()})
			snapshot.Bytes += info.Size()
		default:
			snapshot.Skipped++
		}
		return nil
	})
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: capture %s: %v", ErrIO, absRoot, err)
	}

	// Build child directories before their parents. Depth descending is exactly
	// that order; siblings are independent. The root is depth 0, a top-level
	// directory is depth 1 (counting slashes would tie both at 0).
	sort.Slice(dirs, func(i, j int) bool {
		di, dj := dirDepth(dirs[i]), dirDepth(dirs[j])
		if di != dj {
			return di > dj
		}
		return dirs[i] < dirs[j]
	})
	for _, dir := range dirs {
		treeHash, err := s.PutTree(dirEntries[dir])
		if err != nil {
			return Snapshot{}, err
		}
		if dir == "" {
			snapshot.Tree = treeHash
			continue
		}
		parent := parentDir(dir)
		dirEntries[parent] = append(dirEntries[parent], TreeEntry{
			Name: path.Base(dir), Kind: TreeDir, Hash: treeHash, Mode: dirModes[dir],
		})
	}
	if snapshot.Tree == "" {
		return Snapshot{}, fmt.Errorf("%w: capture produced no root tree", ErrIO)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	snapshot.Files = files
	snapshot.Count = len(files)
	return snapshot, nil
}

func normalizeIgnore(ignore []string) []string {
	out := make([]string, 0, len(ignore))
	for _, prefix := range ignore {
		prefix = strings.Trim(strings.TrimSpace(filepath.ToSlash(prefix)), "/")
		if prefix == "" || prefix == "." {
			continue
		}
		out = append(out, prefix)
	}
	return out
}

func ignored(rel string, ignore []string) bool {
	for _, prefix := range ignore {
		if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
			return true
		}
	}
	return false
}

func parentDir(rel string) string {
	parent := path.Dir(rel)
	if parent == "." {
		return ""
	}
	return parent
}

func dirDepth(dir string) int {
	if dir == "" {
		return 0
	}
	return strings.Count(dir, "/") + 1
}
