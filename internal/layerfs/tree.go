package layerfs

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// TreeKind is the kind of one tree entry.
type TreeKind string

const (
	TreeFile TreeKind = "file"
	TreeDir  TreeKind = "dir"
)

// TreeEntry is one entry of a directory listing. Name is a single path
// component (a tree object is one directory, not a whole subtree). Hash points
// at a file object for TreeFile and at another tree object for TreeDir.
type TreeEntry struct {
	Name string
	Kind TreeKind
	Hash Hash
	Mode uint32
	Size int64
}

// FileEntry is one file of a captured workspace, with its session-relative
// path. It is what the manifest batch-upserts.
type FileEntry struct {
	Path string
	Hash Hash
	Mode uint32
	Size int64
}

const (
	treeVersion     = "keel-tree-v1"
	treeMaxBytes    = 64 << 20
	treeMaxDepth    = 256
	treeEntryMaxLen = 255
)

func validEntryName(name string) bool {
	if name == "" || len(name) > treeEntryMaxLen {
		return false
	}
	if name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, "/\\\x00\n\r") {
		return false
	}
	return true
}

// canonicalTree renders the stable byte form of a directory listing. Entries
// are sorted by name, so the same content always produces the same hash no
// matter the walk order or the file timestamps.
func canonicalTree(entries []TreeEntry) ([]byte, error) {
	seen := make(map[string]bool, len(entries))
	copied := make([]TreeEntry, len(entries))
	copy(copied, entries)
	sort.Slice(copied, func(i, j int) bool { return copied[i].Name < copied[j].Name })
	var builder bytes.Buffer
	builder.WriteString(treeVersion)
	builder.WriteByte('\n')
	for _, entry := range copied {
		if !validEntryName(entry.Name) {
			return nil, fmt.Errorf("%w: tree entry name %q", ErrInvalid, entry.Name)
		}
		if seen[entry.Name] {
			return nil, fmt.Errorf("%w: duplicate tree entry %q", ErrInvalid, entry.Name)
		}
		seen[entry.Name] = true
		if entry.Kind != TreeFile && entry.Kind != TreeDir {
			return nil, fmt.Errorf("%w: tree entry %q has kind %q", ErrInvalid, entry.Name, entry.Kind)
		}
		if !entry.Hash.Valid() {
			return nil, fmt.Errorf("%w: tree entry %q has hash %q", ErrInvalid, entry.Name, string(entry.Hash))
		}
		if entry.Size < 0 {
			return nil, fmt.Errorf("%w: tree entry %q has negative size", ErrInvalid, entry.Name)
		}
		builder.WriteString(entry.Name)
		builder.WriteByte(0)
		builder.WriteString(string(entry.Kind))
		builder.WriteByte(0)
		fmt.Fprintf(&builder, "%04o", entry.Mode&0o7777)
		builder.WriteByte(0)
		builder.WriteString(string(entry.Hash))
		builder.WriteByte(0)
		builder.WriteString(strconv.FormatInt(entry.Size, 10))
		builder.WriteByte('\n')
	}
	return builder.Bytes(), nil
}

func parseTree(raw []byte) ([]TreeEntry, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: empty tree object", ErrInvalid)
	}
	lines := strings.Split(string(raw), "\n")
	if lines[0] != treeVersion {
		return nil, fmt.Errorf("%w: tree object version %q", ErrInvalid, lines[0])
	}
	entries := make([]TreeEntry, 0, len(lines)-1)
	seen := make(map[string]bool, len(lines)-1)
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\x00")
		if len(fields) != 5 {
			return nil, fmt.Errorf("%w: malformed tree entry", ErrInvalid)
		}
		mode, err := strconv.ParseUint(fields[2], 8, 32)
		if err != nil {
			return nil, fmt.Errorf("%w: tree entry %q mode: %v", ErrInvalid, fields[0], err)
		}
		size, err := strconv.ParseInt(fields[4], 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("%w: tree entry %q size", ErrInvalid, fields[0])
		}
		hash, err := ParseHash(fields[3])
		if err != nil {
			return nil, err
		}
		entry := TreeEntry{Name: fields[0], Kind: TreeKind(fields[1]), Hash: hash, Mode: uint32(mode), Size: size}
		if !validEntryName(entry.Name) || (entry.Kind != TreeFile && entry.Kind != TreeDir) {
			return nil, fmt.Errorf("%w: malformed tree entry %q", ErrInvalid, entry.Name)
		}
		if seen[entry.Name] {
			return nil, fmt.Errorf("%w: duplicate tree entry %q", ErrInvalid, entry.Name)
		}
		seen[entry.Name] = true
		entries = append(entries, entry)
	}
	return entries, nil
}

// PutTree stores a canonical directory listing and returns its tree hash.
func (s *Store) PutTree(entries []TreeEntry) (Hash, error) {
	raw, err := canonicalTree(entries)
	if err != nil {
		return "", err
	}
	hash, _, err := s.PutBytes(raw)
	return hash, err
}

// Tree reads a tree object.
func (s *Store) Tree(hash Hash) ([]TreeEntry, error) {
	object, err := s.Open(hash)
	if err != nil {
		return nil, err
	}
	defer object.Close()
	if object.Size > treeMaxBytes {
		return nil, fmt.Errorf("%w: tree object %s is %d bytes (limit %d)", ErrInvalid, hash, object.Size, treeMaxBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(object, treeMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read tree object: %v", ErrIO, err)
	}
	return parseTree(raw)
}

// Materialize writes the tree's files under dir. It is a streaming copy: no
// file is ever held whole in memory.
func (s *Store) Materialize(treeHash Hash, dir string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("%w: materialize target is empty", ErrInvalid)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("%w: create materialize target: %v", ErrIO, err)
	}
	return s.materializeTree(treeHash, dir, 0)
}

func (s *Store) materializeTree(treeHash Hash, dir string, depth int) error {
	if depth > treeMaxDepth {
		return fmt.Errorf("%w: tree depth exceeds %d", ErrInvalid, treeMaxDepth)
	}
	entries, err := s.Tree(treeHash)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !validEntryName(entry.Name) {
			return fmt.Errorf("%w: tree entry name %q", ErrInvalid, entry.Name)
		}
		target := filepath.Join(dir, entry.Name)
		switch entry.Kind {
		case TreeDir:
			mode := os.FileMode(entry.Mode & 0o777)
			if mode == 0 {
				mode = 0o755
			}
			if err := os.MkdirAll(target, mode); err != nil {
				return fmt.Errorf("%w: create directory %s: %v", ErrIO, target, err)
			}
			if err := s.materializeTree(entry.Hash, target, depth+1); err != nil {
				return err
			}
		case TreeFile:
			if err := s.materializeFile(entry, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: tree entry %q has kind %q", ErrInvalid, entry.Name, entry.Kind)
		}
	}
	return nil
}

func (s *Store) materializeFile(entry TreeEntry, target string) error {
	object, err := s.Open(entry.Hash)
	if err != nil {
		return err
	}
	defer object.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("%w: create parent of %s: %v", ErrIO, target, err)
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("%w: create %s: %v", ErrIO, target, err)
	}
	buffer := make([]byte, s.chunkSize)
	if _, err := io.CopyBuffer(file, object, buffer); err != nil {
		file.Close()
		return fmt.Errorf("%w: write %s: %v", ErrIO, target, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("%w: close %s: %v", ErrIO, target, err)
	}
	mode := os.FileMode(entry.Mode & 0o777)
	if mode == 0 {
		mode = 0o644
	}
	if err := os.Chmod(target, mode); err != nil {
		return fmt.Errorf("%w: chmod %s: %v", ErrIO, target, err)
	}
	return nil
}
