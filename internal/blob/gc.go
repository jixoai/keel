package blob

// Blob garbage collection (change keel-ga §5 blob-lifecycle).
//
// Layout and ordering (see docs/BLOB-LIFECYCLE.md for the operator view):
//
//	CAS root (KEEL_CAS_DIR, or an ephemeral temp root)
//	├── aa/bb/<64-hex>      blob objects written by internal/blob (this file's GC)
//	└── layerfs/            layerfs objects — a *different* store; never touched
//
// The CAS file name is the sha256 digest (CASPath). Several blob rows — in the
// same or different tenants — may name one digest, so a file is unlinked only
// after the metadata transaction committed AND a cross-tenant existence check
// says no row still names it.
//
// Sweep order is the spec's: the store deletes metadata (blobs) then chunks in
// one transaction; this type then unlinks CAS files. A crash between those steps
// leaves an unreachable file, never a visible blob whose bytes are missing. The
// next pass cannot find the deleted metadata again, so SweepOrphanCAS is the
// retry mechanism: it walks the two-level hex layout, ignores anything younger
// than the grace period (an in-flight write), and removes digests no blob row
// names. SweepOrphanCAS also ignores the layerfs subtree structurally (only
// two-character hex directory names are candidates).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jixoai/keel/internal/store"
)

const (
	// DefaultGCInterval is the TTL sweep period.
	DefaultGCInterval = time.Minute
	// DefaultOrphanInterval is how often the CAS orphan scan runs. It is the
	// retry path for a crash between metadata deletion and file unlink, so it
	// does not need the TTL sweep's cadence.
	DefaultOrphanInterval = 30 * time.Minute
	// DefaultOrphanGrace keeps the scan away from a file whose metadata was
	// created microseconds ago by an in-flight uploader (metadata is written
	// before the file, so a row always exists first; the grace is belt and
	// braces for a transaction not yet visible to the scan).
	DefaultOrphanGrace = time.Hour
)

// ErrNoCASDir means the deployment has no local CAS: pg_chunked blobs only, so
// there is no file work. Sweeping is still valid and is a no-op for files.
var ErrNoCASDir = errors.New("blob: CAS directory is not configured")

// Sweeper reaps TTL-expired blobs and their CAS files.
type Sweeper struct {
	Store *store.Store
	// CASDir is the blob CAS root; layerfs lives in CASDir/layerfs and is not
	// this store's responsibility.
	CASDir string
	// Interval and OrphanInterval are the two cadences; zero uses the defaults.
	Interval       time.Duration
	OrphanInterval time.Duration
	// Batch is the maximum number of blobs reaped per pass.
	Batch int
	// Now overrides the clock (tests).
	Now func() time.Time
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// NewSweeper builds a sweeper with the package defaults.
func NewSweeper(st *store.Store, casDir string, log *slog.Logger) *Sweeper {
	if log == nil {
		log = slog.Default()
	}
	return &Sweeper{Store: st, CASDir: casDir, Logger: log}
}

// Sweep reaps one batch: metadata+chunks in the store, then CAS unlink. It
// returns the number of blob rows reaped. Repeating it is harmless and reports
// zero once the queue is empty.
func (s *Sweeper) Sweep(ctx context.Context) (int, error) {
	batch := s.Batch
	if batch <= 0 {
		batch = 100
	}
	victims, err := s.Store.ReleaseExpired(ctx, batch)
	if err != nil {
		return 0, err
	}
	if len(victims) == 0 {
		return 0, nil
	}
	if strings.TrimSpace(s.CASDir) == "" {
		return len(victims), nil
	}
	var errs []error
	checked := make(map[string]struct{}, len(victims)*2)
	for _, victim := range victims {
		for _, ref := range victim.CASRefs() {
			if ref.Storage != store.StorageLocalCAS || len(ref.SHA256) != sha256.Size {
				continue
			}
			key := hex.EncodeToString(ref.SHA256)
			if _, ok := checked[key]; ok {
				continue
			}
			checked[key] = struct{}{}
			exists, err := s.Store.CASBlobExists(ctx, ref.SHA256)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if exists {
				// Content dedupe: another blob row (possibly another tenant)
				// still owns this object.
				continue
			}
			path, err := CASPath(s.CASDir, ref.SHA256)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("blob: unlink %s: %w", path, err))
				continue
			}
			s.pruneEmptyDirs(filepath.Dir(path))
		}
	}
	return len(victims), errors.Join(errs...)
}

// SweepOrphanCAS removes CAS files no blob row names. It is the idempotent
// retry of a failed unlink and also cleans files left by a crash after a
// metadata delete. Only files inside the two-level hex layout are candidates,
// so the layerfs subtree (CASDir/layerfs) and unrelated files are untouched.
func (s *Sweeper) SweepOrphanCAS(ctx context.Context, grace time.Duration) (int, error) {
	if strings.TrimSpace(s.CASDir) == "" {
		return 0, nil
	}
	if grace <= 0 {
		grace = DefaultOrphanGrace
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now()
	}
	cutoff := now.Add(-grace)
	levelOne, err := os.ReadDir(s.CASDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("blob: read CAS root: %w", err)
	}
	removed := 0
	var errs []error
	for _, first := range levelOne {
		if !first.IsDir() || !hexName(first.Name(), 2) {
			continue
		}
		firstPath := filepath.Join(s.CASDir, first.Name())
		levelTwo, err := os.ReadDir(firstPath)
		if err != nil {
			errs = append(errs, fmt.Errorf("blob: read CAS %s: %w", first.Name(), err))
			continue
		}
		for _, second := range levelTwo {
			if !second.IsDir() || !hexName(second.Name(), 2) {
				continue
			}
			dir := filepath.Join(firstPath, second.Name())
			files, err := os.ReadDir(dir)
			if err != nil {
				errs = append(errs, fmt.Errorf("blob: read CAS %s/%s: %w", first.Name(), second.Name(), err))
				continue
			}
			for _, file := range files {
				if file.IsDir() || !hexName(file.Name(), sha256.Size*2) {
					continue
				}
				if ctx.Err() != nil {
					return removed, errors.Join(append(errs, ctx.Err())...)
				}
				info, err := file.Info()
				if err != nil {
					if os.IsNotExist(err) {
						continue
					}
					errs = append(errs, err)
					continue
				}
				if info.ModTime().After(cutoff) {
					continue
				}
				sum, err := hex.DecodeString(file.Name())
				if err != nil {
					continue
				}
				exists, err := s.Store.CASBlobExists(ctx, sum)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				if exists {
					continue
				}
				path := filepath.Join(dir, file.Name())
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					errs = append(errs, fmt.Errorf("blob: unlink orphan %s: %w", path, err))
					continue
				}
				removed++
			}
			s.pruneEmptyDirs(dir)
		}
	}
	return removed, errors.Join(errs...)
}

// Run sweeps on Interval and orphan-scans on OrphanInterval until ctx ends. It
// runs one TTL sweep immediately so a short-lived process still makes progress.
func (s *Sweeper) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = DefaultGCInterval
	}
	orphanEvery := s.OrphanInterval
	if orphanEvery <= 0 {
		orphanEvery = DefaultOrphanInterval
	}
	lastOrphan := time.Time{}
	s.tick(ctx, &lastOrphan, orphanEvery)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx, &lastOrphan, orphanEvery)
		}
	}
}

func (s *Sweeper) tick(ctx context.Context, lastOrphan *time.Time, orphanEvery time.Duration) {
	n, err := s.Sweep(ctx)
	if err != nil && ctx.Err() == nil {
		s.Logger.Warn("blob GC sweep failed", "reaped", n, "err", err)
	} else if n > 0 {
		s.Logger.Info("blob GC reaped expired blobs", "count", n)
	}
	now := time.Now()
	if lastOrphan.IsZero() || now.Sub(*lastOrphan) >= orphanEvery {
		removed, err := s.SweepOrphanCAS(ctx, DefaultOrphanGrace)
		if err != nil && ctx.Err() == nil {
			s.Logger.Warn("blob GC orphan scan failed", "removed", removed, "err", err)
		} else if removed > 0 {
			s.Logger.Info("blob GC removed orphan CAS files", "count", removed)
		}
		*lastOrphan = now
	}
}

func (s *Sweeper) pruneEmptyDirs(dir string) {
	root := strings.TrimSpace(s.CASDir)
	if root == "" {
		return
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return
	}
	current, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	for current != absRoot && strings.HasPrefix(current, absRoot+string(os.PathSeparator)) {
		if err := os.Remove(current); err != nil {
			return // not empty, gone, or not ours
		}
		current = filepath.Dir(current)
	}
}

// hexName reports whether name is exactly n lowercase/uppercase hex characters.
// It is the structural guard that keeps layerfs (a word, not hex) out of the
// blob GC.
func hexName(name string, n int) bool {
	if len(name) != n {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
