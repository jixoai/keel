package layerfs

import (
	"fmt"
	"sort"
	"strings"
)

// Fork-view helpers. A forked session resolves its inherited file view from one
// tree hash (the parent's session_snapshot at the fork origin). These helpers
// read that tree as a path-indexed view without materializing it to disk and
// without copying any manifest row: the child keeps a single reference, and a
// parent deletion cannot free the blobs the child still sees.

// normalizeTreePath splits a session-relative path into validated components.
// It is deliberately stricter than filepath.Clean: a tree object never stores
// "." or ".." components, and a fork view must not invent them.
func normalizeTreePath(path string) ([]string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("%w: empty path", ErrInvalid)
	}
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return nil, fmt.Errorf("%w: path %q names the root, not a file", ErrInvalid, path)
	}
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if !validEntryName(part) {
			return nil, fmt.Errorf("%w: path component %q", ErrInvalid, part)
		}
	}
	if len(parts) > treeMaxDepth {
		return nil, fmt.Errorf("%w: path depth exceeds %d", ErrInvalid, treeMaxDepth)
	}
	return parts, nil
}

// LookupPath resolves one file inside a tree object. It walks only the
// directories on the path (O(depth) tree reads), so an inherited file view
// costs the same whether the tree holds ten files or a million.
func (s *Store) LookupPath(tree Hash, path string) (TreeEntry, bool, error) {
	parts, err := normalizeTreePath(path)
	if err != nil {
		return TreeEntry{}, false, err
	}
	current := tree
	for index, part := range parts {
		entries, err := s.Tree(current)
		if err != nil {
			return TreeEntry{}, false, err
		}
		var match *TreeEntry
		for i := range entries {
			if entries[i].Name == part {
				match = &entries[i]
				break
			}
		}
		if match == nil {
			return TreeEntry{}, false, nil
		}
		last := index == len(parts)-1
		if last {
			if match.Kind != TreeFile {
				return TreeEntry{}, false, nil
			}
			return *match, true, nil
		}
		if match.Kind != TreeDir {
			return TreeEntry{}, false, nil
		}
		current = match.Hash
	}
	return TreeEntry{}, false, nil
}

// FlattenTree returns every file under a tree object with its session-relative
// path. It is the overlay input of a fork view (the inherited half), sorted by
// path so callers can merge deterministically.
func (s *Store) FlattenTree(tree Hash) ([]FileEntry, error) {
	var out []FileEntry
	if err := s.flattenTree(tree, "", 0, &out); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (s *Store) flattenTree(tree Hash, prefix string, depth int, out *[]FileEntry) error {
	if depth > treeMaxDepth {
		return fmt.Errorf("%w: tree depth exceeds %d", ErrInvalid, treeMaxDepth)
	}
	entries, err := s.Tree(tree)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !validEntryName(entry.Name) {
			return fmt.Errorf("%w: tree entry name %q", ErrInvalid, entry.Name)
		}
		path := entry.Name
		if prefix != "" {
			path = prefix + "/" + entry.Name
		}
		switch entry.Kind {
		case TreeDir:
			if err := s.flattenTree(entry.Hash, path, depth+1, out); err != nil {
				return err
			}
		case TreeFile:
			*out = append(*out, FileEntry{Path: path, Hash: entry.Hash, Mode: entry.Mode, Size: entry.Size})
		default:
			return fmt.Errorf("%w: tree entry %q has kind %q", ErrInvalid, entry.Name, entry.Kind)
		}
	}
	return nil
}
