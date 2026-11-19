package layerfs

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jixoai/keel/internal/blob"
)

// DefaultChunkSize matches internal/blob's 64KiB chunk: large enough to keep
// syscall overhead down, small enough that a stream never holds more than one
// chunk per operation.
const DefaultChunkSize = 64 << 10

// DefaultRetention is the GC window for unreferenced objects. A write commits
// its ledger event after the object exists; the window must comfortably exceed
// that window (and a process crash between Put and commit).
const DefaultRetention = 24 * time.Hour

// Options configures a Store. The zero value is usable.
type Options struct {
	// TempDir is where Put spools its temporary file. Empty uses <root>/.tmp.
	TempDir string
	// ChunkSize is the fixed streaming buffer. Non-positive uses 64KiB.
	ChunkSize int
	// QuotaBytes caps stored bytes (0 = unlimited). The check happens at the
	// commit point of Put, after the bytes are hashed: an over-quota upload
	// leaves no visible object.
	QuotaBytes int64
	// Retention is the minimum age of an unreferenced object before GC may
	// delete it. Non-positive uses DefaultRetention.
	Retention time.Duration
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Store is a content-addressed object store over a local directory. It is safe
// for concurrent use.
type Store struct {
	root      string
	tempDir   string
	chunkSize int
	quota     int64
	retention time.Duration
	now       func() time.Time

	// casMu serializes the commit point of Put against GC's sweep, so a GC pass
	// can never delete an object between its rename and the usage accounting.
	casMu sync.RWMutex
	usage atomic.Int64
	refs  *refIndex
	// closed is checked by every public operation.
	closed atomic.Bool
}

// PutResult is what a successful Put reports.
type PutResult struct {
	Hash    Hash
	Size    int64
	Created bool // false means the object already existed (dedupe)
}

// Open opens (and creates) a CAS root.
func Open(root string, opts Options) (*Store, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("%w: a CAS root is required", ErrInvalid)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve CAS root: %v", ErrIO, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("%w: create CAS root: %v", ErrIO, err)
	}
	tempDir := opts.TempDir
	if tempDir == "" {
		tempDir = filepath.Join(abs, ".tmp")
	}
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return nil, fmt.Errorf("%w: create CAS temp dir: %v", ErrIO, err)
	}
	refs, err := loadRefIndex(filepath.Join(abs, "refs.log"))
	if err != nil {
		return nil, err
	}
	store := &Store{
		root:      abs,
		tempDir:   tempDir,
		chunkSize: opts.ChunkSize,
		quota:     opts.QuotaBytes,
		retention: opts.Retention,
		now:       opts.Now,
		refs:      refs,
	}
	if store.chunkSize <= 0 {
		store.chunkSize = DefaultChunkSize
	}
	if store.retention <= 0 {
		store.retention = DefaultRetention
	}
	if store.now == nil {
		store.now = func() time.Time { return time.Now().UTC() }
	}
	usage, err := scanUsage(abs)
	if err != nil {
		_ = refs.close()
		return nil, err
	}
	store.usage.Store(usage)
	return store, nil
}

// scanUsage sums the sizes of every object file in the two-level CAS layout.
// It skips the temp directory and the refs journal.
func scanUsage(root string) (int64, error) {
	var total int64
	levelOne, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("%w: scan CAS root: %v", ErrIO, err)
	}
	for _, first := range levelOne {
		if !first.IsDir() || len(first.Name()) != 2 {
			continue
		}
		levelTwo, err := os.ReadDir(filepath.Join(root, first.Name()))
		if err != nil {
			return 0, fmt.Errorf("%w: scan CAS: %v", ErrIO, err)
		}
		for _, second := range levelTwo {
			if !second.IsDir() || len(second.Name()) != 2 {
				continue
			}
			entries, err := os.ReadDir(filepath.Join(root, first.Name(), second.Name()))
			if err != nil {
				return 0, fmt.Errorf("%w: scan CAS: %v", ErrIO, err)
			}
			for _, entry := range entries {
				if entry.IsDir() || len(entry.Name()) != sha256.Size*2 {
					continue
				}
				info, err := entry.Info()
				if err != nil {
					return 0, fmt.Errorf("%w: scan CAS: %v", ErrIO, err)
				}
				total += info.Size()
			}
		}
	}
	return total, nil
}

// Close releases the journal handle. It is safe to call twice.
func (s *Store) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	return s.refs.close()
}

// Root returns the absolute CAS root.
func (s *Store) Root() string { return s.root }

// ChunkSize returns the fixed streaming buffer size.
func (s *Store) ChunkSize() int { return s.chunkSize }

// UsageBytes returns the bytes currently stored.
func (s *Store) UsageBytes() int64 { return s.usage.Load() }

// QuotaBytes returns the configured quota (0 = unlimited).
func (s *Store) QuotaBytes() int64 { return s.quota }

// Put streams r into the CAS and returns its hash. The digest is computed while
// spooling, so the data is read exactly once and never held whole in memory.
func (s *Store) Put(r io.Reader) (Hash, int64, error) {
	result, err := s.PutStatus(r)
	return result.Hash, result.Size, err
}

// PutStatus is Put with the dedupe outcome.
func (s *Store) PutStatus(r io.Reader) (PutResult, error) {
	if s.closed.Load() {
		return PutResult{}, fmt.Errorf("%w: store is closed", ErrUnavailable)
	}
	if r == nil {
		return PutResult{}, fmt.Errorf("%w: nil reader", ErrInvalid)
	}
	tmp, err := os.CreateTemp(s.tempDir, "put-*")
	if err != nil {
		return PutResult{}, fmt.Errorf("%w: create temp object: %v", ErrIO, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	hasher := sha256.New()
	buffer := make([]byte, s.chunkSize)
	size, err := io.CopyBuffer(io.MultiWriter(tmp, hasher), r, buffer)
	if err != nil {
		tmp.Close()
		return PutResult{}, fmt.Errorf("%w: spool object: %v", ErrIO, err)
	}
	if err := tmp.Close(); err != nil {
		return PutResult{}, fmt.Errorf("%w: close temp object: %v", ErrIO, err)
	}
	sum := hasher.Sum(nil)
	hash := Hash(fmt.Sprintf("%x", sum))
	path, err := blob.CASPath(s.root, sum)
	if err != nil {
		return PutResult{}, fmt.Errorf("%w: cas path: %v", ErrIO, err)
	}

	s.casMu.Lock()
	defer s.casMu.Unlock()
	if info, err := os.Stat(path); err == nil {
		if info.Size() != size {
			return PutResult{}, fmt.Errorf("%w: object %s exists with size %d, upload is %d", ErrIO, hash, info.Size(), size)
		}
		return PutResult{Hash: hash, Size: size, Created: false}, nil
	} else if !os.IsNotExist(err) {
		return PutResult{}, fmt.Errorf("%w: stat object: %v", ErrIO, err)
	}
	if s.quota > 0 {
		usage := s.usage.Load()
		if usage+size > s.quota {
			return PutResult{}, fmt.Errorf("%w: usage=%d bytes, upload=%d bytes, quota=%d bytes",
				ErrQuotaExceeded, usage, size, s.quota)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return PutResult{}, fmt.Errorf("%w: create cas dir: %v", ErrIO, err)
	}
	// fsync only a new object: a deduped upload must not pay for durability it
	// does not change (a 100k-file capture hashes mostly existing content).
	if err := syncFile(tmpPath); err != nil {
		return PutResult{}, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		// The temp file lives under the same root, so a cross-device rename is
		// not expected; fall back to an exclusive copy for unusual mounts.
		if copyErr := copyExclusive(tmpPath, path); copyErr != nil {
			return PutResult{}, fmt.Errorf("%w: commit object: %v", ErrIO, err)
		}
	}
	s.usage.Add(size)
	return PutResult{Hash: hash, Size: size, Created: true}, nil
}

func syncFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: open temp object for sync: %v", ErrIO, err)
	}
	defer file.Close()
	if err := file.Sync(); err != nil {
		return fmt.Errorf("%w: sync temp object: %v", ErrIO, err)
	}
	return nil
}

func copyExclusive(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// PutBytes stores raw bytes.
func (s *Store) PutBytes(raw []byte) (Hash, int64, error) {
	return s.Put(bytes.NewReader(raw))
}

// PutFile streams a file into the CAS. It hashes first and checks for an
// existing object before spooling, so capturing a tree whose files already
// exist costs one read per file and no temporary file or fsync.
func (s *Store) PutFile(path string) (PutResult, error) {
	info, err := os.Stat(path)
	if err != nil {
		return PutResult{}, fmt.Errorf("%w: stat %s: %v", ErrIO, path, err)
	}
	hasher := sha256.New()
	file, err := os.Open(path)
	if err != nil {
		return PutResult{}, fmt.Errorf("%w: open %s: %v", ErrIO, path, err)
	}
	buffer := make([]byte, s.chunkSize)
	if _, err := io.CopyBuffer(hasher, file, buffer); err != nil {
		file.Close()
		return PutResult{}, fmt.Errorf("%w: hash %s: %v", ErrIO, path, err)
	}
	if err := file.Close(); err != nil {
		return PutResult{}, fmt.Errorf("%w: close %s: %v", ErrIO, path, err)
	}
	sum := hasher.Sum(nil)
	hash := Hash(fmt.Sprintf("%x", sum))
	finalPath, err := blob.CASPath(s.root, sum)
	if err != nil {
		return PutResult{}, fmt.Errorf("%w: cas path: %v", ErrIO, err)
	}
	s.casMu.Lock()
	defer s.casMu.Unlock()
	if existing, err := os.Stat(finalPath); err == nil {
		if existing.Size() != info.Size() {
			return PutResult{}, fmt.Errorf("%w: object %s exists with size %d, file is %d", ErrIO, hash, existing.Size(), info.Size())
		}
		return PutResult{Hash: hash, Size: info.Size(), Created: false}, nil
	} else if !os.IsNotExist(err) {
		return PutResult{}, fmt.Errorf("%w: stat object: %v", ErrIO, err)
	}
	if s.quota > 0 {
		usage := s.usage.Load()
		if usage+info.Size() > s.quota {
			return PutResult{}, fmt.Errorf("%w: usage=%d bytes, upload=%d bytes, quota=%d bytes",
				ErrQuotaExceeded, usage, info.Size(), s.quota)
		}
	}
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return PutResult{}, fmt.Errorf("%w: create cas dir: %v", ErrIO, err)
	}
	tmp, err := os.CreateTemp(s.tempDir, "put-*")
	if err != nil {
		return PutResult{}, fmt.Errorf("%w: create temp object: %v", ErrIO, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	source, err := os.Open(path)
	if err != nil {
		tmp.Close()
		return PutResult{}, fmt.Errorf("%w: reopen %s: %v", ErrIO, path, err)
	}
	if _, err := io.CopyBuffer(tmp, source, buffer); err != nil {
		source.Close()
		tmp.Close()
		return PutResult{}, fmt.Errorf("%w: spool %s: %v", ErrIO, path, err)
	}
	source.Close()
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return PutResult{}, fmt.Errorf("%w: sync temp object: %v", ErrIO, err)
	}
	if err := tmp.Close(); err != nil {
		return PutResult{}, fmt.Errorf("%w: close temp object: %v", ErrIO, err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		if copyErr := copyExclusive(tmpPath, finalPath); copyErr != nil {
			return PutResult{}, fmt.Errorf("%w: commit object: %v", ErrIO, err)
		}
	}
	s.usage.Add(info.Size())
	return PutResult{Hash: hash, Size: info.Size(), Created: true}, nil
}

// Object is an open read handle over one file object.
type Object struct {
	*os.File
	Hash Hash
	Size int64
}

// Open opens a file object for streaming reads.
func (s *Store) Open(hash Hash) (*Object, error) {
	if !hash.Valid() {
		return nil, fmt.Errorf("%w: object hash %q", ErrInvalid, string(hash))
	}
	sha, err := hash.Bytes()
	if err != nil {
		return nil, err
	}
	path, err := blob.CASPath(s.root, sha)
	if err != nil {
		return nil, fmt.Errorf("%w: cas path: %v", ErrIO, err)
	}
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, hash)
		}
		return nil, fmt.Errorf("%w: open object: %v", ErrIO, err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("%w: stat object: %v", ErrIO, err)
	}
	return &Object{File: file, Hash: hash, Size: info.Size()}, nil
}

// ObjectInfo is the metadata Stat reports.
type ObjectInfo struct {
	Hash    Hash
	Size    int64
	ModTime time.Time
}

// Stat reports one object's metadata.
func (s *Store) Stat(hash Hash) (ObjectInfo, error) {
	object, err := s.Open(hash)
	if err != nil {
		return ObjectInfo{}, err
	}
	defer object.Close()
	info, err := object.File.Stat()
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("%w: stat object: %v", ErrIO, err)
	}
	return ObjectInfo{Hash: hash, Size: info.Size(), ModTime: info.ModTime()}, nil
}

// Has reports whether the object exists.
func (s *Store) Has(hash Hash) bool {
	info, err := s.Stat(hash)
	return err == nil && info.Size >= 0
}

// ReadRange streams length bytes starting at offset to w. A non-positive
// length, or a range past EOF, is clamped to the object size.
func (s *Store) ReadRange(hash Hash, offset, length int64, w io.Writer) (int64, error) {
	object, err := s.Open(hash)
	if err != nil {
		return 0, err
	}
	defer object.Close()
	if offset < 0 || offset > object.Size {
		return 0, fmt.Errorf("%w: offset %d outside object %s (size %d)", ErrInvalid, offset, hash, object.Size)
	}
	if length <= 0 || offset+length > object.Size {
		length = object.Size - offset
	}
	if length == 0 {
		return 0, nil
	}
	if _, err := object.Seek(offset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("%w: seek object: %v", ErrIO, err)
	}
	buffer := make([]byte, s.chunkSize)
	written, err := io.CopyBuffer(w, io.LimitReader(object, length), buffer)
	if err != nil {
		return written, fmt.Errorf("%w: read object: %v", ErrIO, err)
	}
	return written, nil
}

// ObjectPath returns the CAS path of an object (tests and operational audits).
func (s *Store) ObjectPath(hash Hash) (string, error) {
	sha, err := hash.Bytes()
	if err != nil {
		return "", err
	}
	return blob.CASPath(s.root, sha)
}

// Retain records one event reference to an object.
func (s *Store) Retain(hash Hash, ref Ref) (bool, error) {
	return s.refs.retain(hash, ref)
}

// RetainBatch records many event references with one journal flush per chunk.
func (s *Store) RetainBatch(entries []RefEntry) (int, error) {
	return s.refs.retainBatch(entries)
}

// Release removes one event reference.
func (s *Store) Release(hash Hash, ref Ref) (bool, error) {
	return s.refs.release(hash, ref)
}

// ReleaseSession removes every reference owned by a session. Objects shared
// with a fork keep their other references.
func (s *Store) ReleaseSession(session string) (int, error) {
	return s.refs.releaseSession(session)
}

// RefCount reports the number of distinct event references.
func (s *Store) RefCount(hash Hash) int { return s.refs.count(hash) }

// Refs lists an object's references.
func (s *Store) Refs(hash Hash) []Ref { return s.refs.refs(hash) }

// Collect deletes unreferenced objects older than the retention window. It is
// safe against concurrent Put: the commit point and the sweep share casMu.
func (s *Store) Collect(retention time.Duration) (int, error) {
	if s.closed.Load() {
		return 0, fmt.Errorf("%w: store is closed", ErrUnavailable)
	}
	if retention <= 0 {
		retention = s.retention
	}
	cutoff := s.now().Add(-retention)

	s.casMu.Lock()
	defer s.casMu.Unlock()
	removed := 0
	levelOne, err := os.ReadDir(s.root)
	if err != nil {
		return 0, fmt.Errorf("%w: scan CAS root: %v", ErrIO, err)
	}
	for _, first := range levelOne {
		if !first.IsDir() || len(first.Name()) != 2 {
			continue
		}
		firstPath := filepath.Join(s.root, first.Name())
		levelTwo, err := os.ReadDir(firstPath)
		if err != nil {
			return removed, fmt.Errorf("%w: scan CAS: %v", ErrIO, err)
		}
		for _, second := range levelTwo {
			if !second.IsDir() || len(second.Name()) != 2 {
				continue
			}
			secondPath := filepath.Join(firstPath, second.Name())
			entries, err := os.ReadDir(secondPath)
			if err != nil {
				return removed, fmt.Errorf("%w: scan CAS: %v", ErrIO, err)
			}
			for _, entry := range entries {
				if entry.IsDir() || len(entry.Name()) != sha256.Size*2 {
					continue
				}
				hash := Hash(entry.Name())
				if !hash.Valid() || s.refs.count(hash) > 0 {
					continue
				}
				info, err := entry.Info()
				if err != nil {
					continue
				}
				if info.ModTime().After(cutoff) {
					continue
				}
				if err := os.Remove(filepath.Join(secondPath, entry.Name())); err != nil && !os.IsNotExist(err) {
					return removed, fmt.Errorf("%w: delete object: %v", ErrIO, err)
				}
				s.usage.Add(-info.Size())
				removed++
			}
			removeIfEmpty(secondPath)
		}
		removeIfEmpty(firstPath)
	}
	if err := s.refs.compact(); err != nil {
		return removed, err
	}
	return removed, nil
}

func removeIfEmpty(path string) {
	entries, err := os.ReadDir(path)
	if err == nil && len(entries) == 0 {
		_ = os.Remove(path)
	}
}
