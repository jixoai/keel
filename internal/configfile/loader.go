// Package configfile is the bounded disk adapter for the W0.5 config.toml
// model: it parses the strict file through internal/assembly, rotates it
// atomically (`config.toml.new` → fsync → rename → directory fsync), and owns
// one immutable assembly.Rotation.
//
// It is an adapter, not a second configuration system: the parser, row
// vocabulary, content hash and version/anchor semantics all live in
// internal/assembly. The loader adds only the file facts (does the file exist,
// what bytes are active, when was it replaced) and the tenant-document overlay
// for the settings the file carries (today filesystem.capture.mode).
//
// Precedence is explicit and constant:
//
//	boot defaults < stored admin tenant document < config.toml settings
//
// The file is the outermost user layer. The admin JSON path keeps working: a
// tenant document still governs every setting the file does not name, and the
// next attempt sees a stored write exactly as before. A file setting wins only
// for its own key.
package configfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jixoai/keel/internal/assembly"
	"github.com/jixoai/keel/internal/capturepolicy"
	"github.com/jixoai/keel/internal/host"
)

// FileName is the active file name; the rotation temp is FileName + ".new".
const FileName = "config.toml"

// NewFileName is the atomic rotation staging name in the same directory.
const NewFileName = FileName + ".new"

// Options configures Open.
type Options struct {
	// Path is the active config.toml path. Empty is rejected: a loader without
	// a path would silently do nothing.
	Path string
	// AnchorEventSeq is the ledger sequence that anchors the version loaded at
	// startup. Zero means "no config.rotated event yet" (a fresh deployment).
	AnchorEventSeq int64
}

// Status is the active version fact exposed to the host (logs, capability
// document). It is a snapshot: later rotations cannot mutate it.
type Status struct {
	Path           string
	Exists         bool
	Revision       int64
	Hash           string
	AnchorEventSeq int64
	// CaptureMode is the file's filesystem.capture.mode value ("" when the
	// file does not set it).
	CaptureMode string
	LoadedAt    time.Time
	// Rotations counts successful rotations since Open.
	Rotations int64
	// LastRotation is the most recent rotation fact (nil before the first).
	LastRotation *RotationFact
}

// RotationFact is the durable-ish fact of one successful file rotation. The
// caller commits it to the ledger (config.rotated) with the same hash/revision;
// AnchorEventSeq is the sequence that made the version effective.
type RotationFact struct {
	Revision         int64
	Hash             string
	AnchorEventSeq   int64
	PreviousRevision int64
	PreviousHash     string
	RotatedAt        time.Time
	Bytes            int
}

// Loader owns the active config.toml version. It is safe for concurrent use:
// Pin takes a deep copy, Rotate serializes file replacement and version
// advance, and a run that already pinned a version never observes a rotation.
type Loader struct {
	path string

	mu           sync.RWMutex
	rotation     *assembly.Rotation
	exists       bool
	loadedAt     time.Time
	rotations    int64
	lastRotation *RotationFact
}

// Open loads the file at startup. A missing file is an empty configuration
// (the file is optional); an unreadable or invalid file is an error, so a
// deployment fails closed instead of running with a half-applied config.
func Open(opts Options) (*Loader, error) {
	path := strings.TrimSpace(opts.Path)
	if path == "" {
		return nil, errors.New("configfile: path is required")
	}
	config, exists, err := readConfig(path)
	if err != nil {
		return nil, err
	}
	rotation, err := assembly.NewRotation(config, opts.AnchorEventSeq)
	if err != nil {
		return nil, fmt.Errorf("configfile: %s: %w", path, err)
	}
	return &Loader{
		path:     path,
		rotation: rotation,
		exists:   exists,
		loadedAt: time.Now().UTC(),
	}, nil
}

// Path returns the active file path.
func (l *Loader) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Pin returns the immutable version a run/attempt must use. The caller may hold
// it for the whole attempt: a later rotation cannot mutate it.
func (l *Loader) Pin() assembly.Pinned {
	if l == nil {
		return assembly.Pinned{}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.rotation == nil {
		return assembly.Pinned{}
	}
	return l.rotation.Pin()
}

// Version returns the active version identity.
func (l *Loader) Version() assembly.ConfigVersion {
	if l == nil {
		return assembly.ConfigVersion{}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.rotation == nil {
		return assembly.ConfigVersion{}
	}
	return l.rotation.Current()
}

// Status returns the active version/hash, the rotation count and the last
// rotation fact.
func (l *Loader) Status() Status {
	if l == nil {
		return Status{}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	status := Status{
		Path:      l.path,
		Exists:    l.exists,
		LoadedAt:  l.loadedAt,
		Rotations: l.rotations,
	}
	if l.rotation != nil {
		version := l.rotation.Current()
		status.Revision = version.Revision
		status.Hash = version.Hash
		status.AnchorEventSeq = version.AnchorEventSeq
		pinned := l.rotation.Pin()
		if mode, ok, err := CaptureModeOf(pinned.Config); err == nil && ok {
			status.CaptureMode = string(mode)
		}
	}
	if l.lastRotation != nil {
		fact := *l.lastRotation
		status.LastRotation = &fact
	}
	return status
}

// CaptureMode returns the file's capture setting and whether it is present.
func (l *Loader) CaptureMode() (capturepolicy.CaptureMode, bool, error) {
	if l == nil {
		return "", false, nil
	}
	return CaptureModeOf(l.Pin().Config)
}

// CaptureModeOf reads filesystem.capture.mode from a parsed configuration. An
// invalid value is an error, never a silent default: the parser rejects it, and
// this check keeps a hand-constructed Config fail-closed too.
func CaptureModeOf(config assembly.Config) (capturepolicy.CaptureMode, bool, error) {
	raw, ok := config.Settings.Get(assembly.SettingFilesystemCaptureMode)
	if !ok {
		return "", false, nil
	}
	mode, err := capturepolicy.ParseCaptureMode(raw)
	if err != nil {
		return "", false, fmt.Errorf("configfile: %s: %w", assembly.SettingFilesystemCaptureMode, err)
	}
	return mode, true, nil
}

// OverlayTenant applies the pinned file settings on top of a tenant document.
// The input is not mutated; the caller gets a copy with the file's setting
// applied. The returned bool reports whether the file set the capture mode.
func OverlayTenant(pinned assembly.Pinned, tenant host.TenantConfig) (host.TenantConfig, bool, error) {
	applied := false
	mode, ok, err := CaptureModeOf(pinned.Config)
	if err != nil {
		return host.TenantConfig{}, false, err
	}
	if ok {
		tenant.Filesystem.Capture.Mode = string(mode)
		applied = true
	}
	settings := pinned.Config.Settings
	if raw, ok := settings.Get(assembly.SettingCompactionEnabled); ok {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return host.TenantConfig{}, false, fmt.Errorf("configfile: compaction.enabled: %w", err)
		}
		tenant.Compaction.Enabled = &value
		applied = true
	}
	if raw, ok := settings.Get(assembly.SettingCompactionManualCompact); ok {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return host.TenantConfig{}, false, fmt.Errorf("configfile: compaction.manualCompact: %w", err)
		}
		tenant.Compaction.ManualCompact = &value
		applied = true
	}
	if raw, ok := settings.Get(assembly.SettingCompactionThreshold); ok {
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return host.TenantConfig{}, false, fmt.Errorf("configfile: compaction.threshold: %w", err)
		}
		tenant.Compaction.Threshold = value
		applied = true
	}
	for _, field := range []struct {
		key    string
		target *int
	}{
		{assembly.SettingCompactionKeepRecentTokens, &tenant.Compaction.KeepRecentTokens},
		{assembly.SettingCompactionThresholdTokens, &tenant.Compaction.ThresholdTokens},
		{assembly.SettingCompactionMaxCheckpoints, &tenant.Compaction.MaxCheckpoints},
		{assembly.SettingCompactionTTLHours, &tenant.Compaction.TTLHours},
	} {
		raw, ok := settings.Get(field.key)
		if !ok {
			continue
		}
		value, err := strconv.Atoi(raw)
		if err != nil {
			return host.TenantConfig{}, false, fmt.Errorf("configfile: %s: %w", field.key, err)
		}
		*field.target = value
		applied = true
	}
	return tenant, applied, nil
}

// Rotate writes data as the next configuration version:
//
//  1. parse and validate the whole document (an invalid file never touches
//     the active file or the active version);
//  2. write config.toml.new, fsync it, rename it over config.toml, fsync the
//     directory;
//  3. advance the immutable version with the supplied ledger anchor.
//
// A run that pinned the previous version keeps it; the next Pin sees the new
// one. On a pre-rename failure the old file and version are untouched and the
// staging file is removed.
func (l *Loader) Rotate(data []byte, anchorEventSeq int64) (RotationFact, error) {
	if l == nil {
		return RotationFact{}, errors.New("configfile: nil loader")
	}
	config, err := parse(data, l.path)
	if err != nil {
		return RotationFact{}, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.checkAnchorLocked(anchorEventSeq); err != nil {
		return RotationFact{}, err
	}
	committed, writeErr := writeAtomic(l.path, data)
	if !committed {
		return RotationFact{}, writeErr
	}
	fact, rotateErr := l.rotateLocked(config, anchorEventSeq, len(data))
	if rotateErr != nil {
		// The rename is already visible. This is only reachable if the caller
		// bypassed checkAnchorLocked; report both facts rather than pretending
		// nothing changed on disk.
		return RotationFact{}, fmt.Errorf("configfile: %s was replaced but the version did not advance: %w", l.path, rotateErr)
	}
	if writeErr != nil {
		// The rename committed but the directory fsync did not. The new file and
		// version are active; the error is a durability warning.
		return fact, writeErr
	}
	return fact, nil
}

// Reload adopts the current file as the next version without writing it. It is
// the external-rotation path (another process replaced the file atomically):
// the caller supplies the ledger anchor of the config.rotated event it
// committed.
func (l *Loader) Reload(anchorEventSeq int64) (RotationFact, error) {
	if l == nil {
		return RotationFact{}, errors.New("configfile: nil loader")
	}
	// Read once, then parse those exact bytes. Re-reading after the parse would
	// be a TOCTOU: a concurrent atomic replacement could make the adopted
	// version describe bytes that are no longer the ones read.
	data, exists, err := readActive(l.path)
	if err != nil {
		return RotationFact{}, err
	}
	config := emptyConfig()
	if exists {
		config, err = parse(data, l.path)
		if err != nil {
			return RotationFact{}, err
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.checkAnchorLocked(anchorEventSeq); err != nil {
		return RotationFact{}, err
	}
	fact, err := l.rotateLocked(config, anchorEventSeq, len(data))
	if err != nil {
		return RotationFact{}, err
	}
	l.exists = exists
	return fact, nil
}

func (l *Loader) checkAnchorLocked(anchorEventSeq int64) error {
	if anchorEventSeq < 0 {
		return &assembly.Error{Issues: []assembly.Issue{{
			Code:    assembly.IssueConfigAnchor,
			Stage:   assembly.StageConfig,
			Field:   "anchorEventSeq",
			Message: "a configuration anchor cannot be negative",
		}}}
	}
	if l.rotation == nil {
		return errors.New("configfile: loader has no rotation state")
	}
	current := l.rotation.Current()
	if anchorEventSeq <= current.AnchorEventSeq {
		return &assembly.Error{Issues: []assembly.Issue{{
			Code:  assembly.IssueConfigAnchor,
			Stage: assembly.StageConfig,
			Field: "anchorEventSeq",
			Message: fmt.Sprintf("rotation anchor %d must be greater than the current anchor %d",
				anchorEventSeq, current.AnchorEventSeq),
		}}}
	}
	return nil
}

func (l *Loader) rotateLocked(config assembly.Config, anchorEventSeq int64, size int) (RotationFact, error) {
	previous := l.rotation.Current()
	version, err := l.rotation.Rotate(config, anchorEventSeq)
	if err != nil {
		return RotationFact{}, err
	}
	fact := RotationFact{
		Revision:         version.Revision,
		Hash:             version.Hash,
		AnchorEventSeq:   version.AnchorEventSeq,
		PreviousRevision: previous.Revision,
		PreviousHash:     previous.Hash,
		RotatedAt:        time.Now().UTC(),
		Bytes:            size,
	}
	l.rotations++
	l.lastRotation = &fact
	l.exists = true
	l.loadedAt = fact.RotatedAt
	return fact, nil
}

// readActive reads the active file once. A missing file is reported as
// (nil, false, nil): the file is optional.
func readActive(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("configfile: read %s: %w", path, err)
	}
	return data, true, nil
}

func readConfig(path string) (assembly.Config, bool, error) {
	data, exists, err := readActive(path)
	if err != nil {
		return assembly.Config{}, false, err
	}
	if !exists {
		return emptyConfig(), false, nil
	}
	config, err := parse(data, path)
	if err != nil {
		return assembly.Config{}, true, err
	}
	return config, true, nil
}

func emptyConfig() assembly.Config {
	return assembly.Config{Version: assembly.ConfigFormatVersion, Source: FileName}
}

func parse(data []byte, path string) (assembly.Config, error) {
	config, err := assembly.ParseConfig(data)
	if err != nil {
		return assembly.Config{}, fmt.Errorf("configfile: %s: %w", path, err)
	}
	if err := config.Validate(); err != nil {
		return assembly.Config{}, fmt.Errorf("configfile: %s: %w", path, err)
	}
	return config, nil
}

// writeAtomic replaces path with data using the rotation staging file. It
// reports whether the rename committed; a false result guarantees the active
// file is untouched and the staging file is gone.
func writeAtomic(path string, data []byte) (bool, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("configfile: create %s: %w", dir, err)
	}
	staging := path + ".new"
	file, err := os.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return false, fmt.Errorf("configfile: open %s: %w", staging, err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(staging)
		}
	}()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return false, fmt.Errorf("configfile: write %s: %w", staging, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return false, fmt.Errorf("configfile: fsync %s: %w", staging, err)
	}
	if err := file.Close(); err != nil {
		return false, fmt.Errorf("configfile: close %s: %w", staging, err)
	}
	if err := os.Rename(staging, path); err != nil {
		return false, fmt.Errorf("configfile: rename %s to %s: %w", staging, path, err)
	}
	cleanup = false
	if err := syncDir(dir); err != nil {
		return true, fmt.Errorf("configfile: fsync directory %s: %w", dir, err)
	}
	return true, nil
}

func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}
