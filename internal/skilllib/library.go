// Package skilllib is the product's skill library: the tenant-scoped registry
// behind the `skills` aggregate tool, the settings page and `keel skills`.
//
// It stores three things under one managed root:
//
//	<root>/<tenant>/.versions/<bundle-hash>/…   immutable, content-addressed versions
//	<root>/<tenant>/active/<name>/…             the materialised copy discovery reads
//	<root>/<tenant>/.registry.json              subscriptions, pins and the index
//
// The separation is the whole point of the package:
//
//   - A version directory is written once and named by the SHA-256 of its
//     canonical manifest (internal/skills/bundle.go). Equal content is stored
//     once, and a version whose bytes changed is reported (`ErrBundleMismatch`)
//     instead of being trusted.
//   - `active/` is a copy, not a link, because the discovery walk treats a
//     directory with SKILL.md as a skill root and refuses symlinks. Moving the
//     active copy is what install/pin/update do; the version's bytes never move.
//   - `.registry.json` carries the state that is not in the bytes: sources and
//     their trust, the active/pinned hash of each skill, and version history.
//     It is replaced atomically (temp + rename), so a crash never leaves half a
//     document.
//
// Trust is a property of the source a skill came from, not a request field a
// caller can upgrade: an install from an untrusted source needs the explicit
// AllowUntrusted flag, and the record keeps the source trust forever.
//
// This package never executes a skill resource. A script stored next to
// SKILL.md is bytes here; running it is the bash tool's job, under the bash
// policy (sandbox, job manager, bounded output). See backend.go.
package skilllib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/skills"
)

// Trust levels of a subscription source, plus the level of a skill authored in
// this deployment (the agent's `skills.manage create`, `/learn`, a manual
// install). `local` is not a source trust: a subscription is only trusted or
// untrusted, and installing bytes never upgrades it.
const (
	TrustTrusted   = "trusted"
	TrustUntrusted = "untrusted"
	TrustLocal     = "local"
)

// Source kinds. Only `local` is executable today; a remote kind is refused with
// a typed unsupported_source rather than accepted and never synced.
const (
	SourceLocal = "local"
)

// Conflict codes a record may carry. They are the "update conflict is visible"
// contract: the settings page renders the code and the CLI prints it.
const (
	ConflictPinned      = "pinned"
	ConflictModified    = "modified"
	ConflictSourceMiss  = "source_missing"
	ConflictNameChanged = "name_mismatch"
	ConflictMissing     = "missing_active"
)

// Typed error codes (they double as the tool surface's error code through
// ToolCode).
const (
	CodeInvalid          = "invalid_arguments"
	CodeNotFound         = "not_found"
	CodeAlreadyExists    = "already_exists"
	CodeUntrusted        = "untrusted_source"
	CodePinned           = "pinned"
	CodeNameConflict     = "name_conflict"
	CodeSourceInUse      = "source_in_use"
	CodeUnsupported      = "unsupported_source"
	CodeSourceMissing    = "source_missing"
	CodeVersionMissing   = "version_not_found"
	CodeLibraryNotWired  = "not_wired"
	CodeBundleMismatch   = "bundle_mismatch"
	CodeNoSourceRecorded = "no_source"
)

// Error is the typed failure of one library operation. Code is stable (the
// tool surface and the HTTP API both publish it); Message is operator-facing.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// ToolCode lets internal/toolsurface map a library failure onto a tool error
// code without this package importing the tool surface.
func (e *Error) ToolCode() string { return e.Code }

func errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Options configures a Library.
type Options struct {
	// Root is the managed root ($KEEL_HOME/skills by default). Each tenant gets
	// its own subtree.
	Root string
	// Now overrides the clock (version timestamps, tests).
	Now func() time.Time
	// Logger receives non-fatal diagnostics (a source that cannot be read while
	// checking updates). Nil discards them.
	Logger *slog.Logger
	// OnChange is called after every successful write (install, update, pin,
	// unsubscribe). The hosting process uses it to rediscover skills so the next
	// run's prompt sees the change.
	OnChange func()
}

// Library is the tenant-scoped skill registry. It is safe for concurrent use
// within one process; the index's atomic replace is what protects it against a
// crash, not against a second process (a deployment serving the same root from
// two processes is out of scope, like the plugin manager's root).
type Library struct {
	root     string
	now      func() time.Time
	logger   *slog.Logger
	onChange func()
	mu       sync.Mutex
}

// New builds a library rooted at opts.Root. The root is created lazily on the
// first write; a read-only deployment can point at a root it never writes.
func New(opts Options) *Library {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	return &Library{root: strings.TrimSpace(opts.Root), now: now, logger: logger, onChange: opts.OnChange}
}

// Root is the managed root (empty when the library is not configured).
func (l *Library) Root() string {
	if l == nil {
		return ""
	}
	return l.root
}

// SetOnChange installs the change callback after construction. The hosting
// process needs it because the callback closes over the runtime, which is built
// after the library; the write is guarded by the same lock the write paths hold,
// so a concurrent install never observes a torn function value.
func (l *Library) SetOnChange(fn func()) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.onChange = fn
	l.mu.Unlock()
}

// ActiveRoot is the directory servicehost prepends to the skill discovery
// roots: <root>/<tenant>/active. Every name below it is a materialised skill.
func (l *Library) ActiveRoot(tenantID uuid.UUID) string {
	if l == nil || l.root == "" || tenantID == uuid.Nil {
		return ""
	}
	return filepath.Join(l.root, tenantID.String(), "active")
}

func (l *Library) tenantRoot(tenantID uuid.UUID) string {
	return filepath.Join(l.root, tenantID.String())
}

func (l *Library) indexPath(tenantRoot string) string {
	return filepath.Join(tenantRoot, ".registry.json")
}

// Source is one subscription: where a skill's updates come from and how much
// the operator trusts that place.
type Source struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	URL       string    `json:"url"`
	Trust     string    `json:"trust"`
	CreatedAt time.Time `json:"createdAt"`
}

// Version is one immutable bundle version.
type Version struct {
	Hash      string    `json:"hash"`
	CreatedAt time.Time `json:"createdAt"`
	Files     int       `json:"files"`
	Bytes     int64     `json:"bytes"`
}

// Record is one installed skill as the settings surface publishes it.
type Record struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Trust       string `json:"trust"`
	SourceID    string `json:"sourceId,omitempty"`
	SourceKind  string `json:"sourceKind,omitempty"`
	// SourceURL is the origin update reads (a local directory today). It is
	// informational for the operator; no credential ever lives in it.
	SourceURL   string    `json:"sourceUrl,omitempty"`
	CurrentHash string    `json:"currentHash"`
	PinnedHash  string    `json:"pinnedHash,omitempty"`
	InstalledAt time.Time `json:"installedAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	Versions    []Version `json:"versions"`
	Path        string    `json:"path"`
	FileCount   int       `json:"fileCount"`
	Bytes       int64     `json:"bytes"`
	// UpdateAvailable is true when the recorded origin holds a bundle whose
	// hash differs from CurrentHash. AvailableHash names it.
	UpdateAvailable bool   `json:"updateAvailable"`
	AvailableHash   string `json:"availableHash,omitempty"`
	// Conflict is one of the Conflict* codes when the installed state cannot be
	// trusted as-is (a pin holds back an update, the active copy drifted, the
	// source changed its name or disappeared).
	Conflict    string   `json:"conflict,omitempty"`
	Diagnostics []string `json:"diagnostics,omitempty"`
}

// Snapshot is the whole tenant library as one read.
type Snapshot struct {
	Revision int64    `json:"revision"`
	Root     string   `json:"root"`
	Skills   []Record `json:"skills"`
	Sources  []Source `json:"sources"`
}

// Detail is one skill including its body and resource manifest.
type Detail struct {
	Record
	Body  string              `json:"body"`
	Files []skills.BundleFile `json:"files"`
}

// UpdateResult reports what an update did: updated, up_to_date, or pinned (the
// available hash is published and nothing changed).
type UpdateResult struct {
	Status          string `json:"status"`
	Record          Record `json:"record"`
	PreviousHash    string `json:"previousHash"`
	AvailableHash   string `json:"availableHash,omitempty"`
	PinnedHash      string `json:"pinnedHash,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable"`
}

// InstallRequest is one install. Files/Description+Body build a bundle in
// memory; Dir reads one from disk (the CLI and in-process callers); exactly one
// form is used. SourceID names a subscription and its trust; an explicit Trust
// is only honoured for a bundle with no source.
type InstallRequest struct {
	Name        string
	Description string
	Body        string
	Files       []skills.FileInput
	Dir         string
	SourceID    string
	Trust       string
	// AllowUntrusted acknowledges a bundle from an untrusted source. It is an
	// explicit operator act, never a default.
	AllowUntrusted bool
	// Pin activates and pins the installed version in one step.
	Pin bool
}

// UpdateRequest is one update. Empty Files/Dir means "read the recorded
// origin"; SourceID is only used when the skill has no origin yet.
type UpdateRequest struct {
	Files          []skills.FileInput
	Description    string
	Body           string
	Dir            string
	SourceID       string
	AllowUntrusted bool
}

// --- index ---------------------------------------------------------------

type origin struct {
	Kind string `json:"kind"`
	URL  string `json:"url"`
}

type entry struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	SourceID    string    `json:"sourceId,omitempty"`
	Origin      origin    `json:"origin,omitempty"`
	Trust       string    `json:"trust"`
	Current     string    `json:"current"`
	Pinned      string    `json:"pinned,omitempty"`
	InstalledAt time.Time `json:"installedAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	Versions    []Version `json:"versions"`
}

type index struct {
	Revision int64    `json:"revision"`
	Sources  []Source `json:"sources,omitempty"`
	Skills   []entry  `json:"skills,omitempty"`
}

func (idx *index) entry(name string) *entry {
	for i := range idx.Skills {
		if idx.Skills[i].Name == name {
			return &idx.Skills[i]
		}
	}
	return nil
}

func (idx *index) source(id string) *Source {
	for i := range idx.Sources {
		if idx.Sources[i].ID == id {
			return &idx.Sources[i]
		}
	}
	return nil
}

func (idx *index) removeEntry(name string) {
	out := idx.Skills[:0]
	for _, candidate := range idx.Skills {
		if candidate.Name != name {
			out = append(out, candidate)
		}
	}
	idx.Skills = out
}

func (l *Library) loadIndex(tenantRoot string) (index, error) {
	raw, err := os.ReadFile(l.indexPath(tenantRoot))
	if errors.Is(err, os.ErrNotExist) {
		return index{}, nil
	}
	if err != nil {
		return index{}, fmt.Errorf("skilllib: read registry: %w", err)
	}
	var idx index
	if err := json.Unmarshal(raw, &idx); err != nil {
		return index{}, fmt.Errorf("skilllib: decode registry: %w", err)
	}
	return idx, nil
}

func (l *Library) saveIndex(tenantRoot string, idx index) error {
	if err := os.MkdirAll(tenantRoot, 0o755); err != nil {
		return fmt.Errorf("skilllib: create tenant root: %w", err)
	}
	idx.Revision++
	raw, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("skilllib: encode registry: %w", err)
	}
	raw = append(raw, '\n')
	temp, err := os.CreateTemp(tenantRoot, ".registry-*")
	if err != nil {
		return fmt.Errorf("skilllib: temp registry: %w", err)
	}
	tempName := temp.Name()
	if _, err := temp.Write(raw); err != nil {
		temp.Close()
		os.Remove(tempName)
		return fmt.Errorf("skilllib: write registry: %w", err)
	}
	if err := temp.Close(); err != nil {
		os.Remove(tempName)
		return err
	}
	if err := os.Rename(tempName, l.indexPath(tenantRoot)); err != nil {
		os.Remove(tempName)
		return fmt.Errorf("skilllib: replace registry: %w", err)
	}
	return nil
}

// withIndex runs fn under the library lock and persists the index when fn says
// it changed something. OnChange fires after the successful save, inside the
// same critical section (the hosting callback re-reads the directory; it never
// calls back into this package).
func (l *Library) withIndex(tenantID uuid.UUID, fn func(*index, string) (bool, error)) error {
	if l == nil || l.root == "" {
		return errorf(CodeLibraryNotWired, "the skill library is not configured")
	}
	if tenantID == uuid.Nil {
		return errorf(CodeInvalid, "a tenant is required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	tenantRoot := l.tenantRoot(tenantID)
	idx, err := l.loadIndex(tenantRoot)
	if err != nil {
		return err
	}
	changed, err := fn(&idx, tenantRoot)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := l.saveIndex(tenantRoot, idx); err != nil {
		return err
	}
	if l.onChange != nil {
		l.onChange()
	}
	return nil
}

// --- read ----------------------------------------------------------------

// List returns the tenant library. check=true also reads each recorded origin
// and reports update availability/conflicts; check=false only verifies that the
// active copy still matches its recorded hash (a local read).
func (l *Library) List(tenantID uuid.UUID, check bool) (Snapshot, error) {
	if l == nil || l.root == "" {
		return Snapshot{}, errorf(CodeLibraryNotWired, "the skill library is not configured")
	}
	if tenantID == uuid.Nil {
		return Snapshot{}, errorf(CodeInvalid, "a tenant is required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	tenantRoot := l.tenantRoot(tenantID)
	idx, err := l.loadIndex(tenantRoot)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{Revision: idx.Revision, Root: tenantRoot, Sources: append([]Source(nil), idx.Sources...)}
	if snapshot.Sources == nil {
		snapshot.Sources = []Source{}
	}
	snapshot.Skills = make([]Record, 0, len(idx.Skills))
	for _, item := range idx.Skills {
		snapshot.Skills = append(snapshot.Skills, l.record(item, tenantRoot, check))
	}
	sort.Slice(snapshot.Skills, func(i, j int) bool { return snapshot.Skills[i].Name < snapshot.Skills[j].Name })
	return snapshot, nil
}

func (l *Library) record(item entry, tenantRoot string, check bool) Record {
	record := Record{
		Name:        item.Name,
		Description: item.Description,
		Trust:       item.Trust,
		SourceID:    item.SourceID,
		SourceKind:  item.Origin.Kind,
		SourceURL:   item.Origin.URL,
		CurrentHash: item.Current,
		PinnedHash:  item.Pinned,
		InstalledAt: item.InstalledAt,
		UpdatedAt:   item.UpdatedAt,
		Versions:    append([]Version(nil), item.Versions...),
		Path:        filepath.Join(tenantRoot, "active", item.Name),
	}
	sort.Slice(record.Versions, func(i, j int) bool { return record.Versions[i].CreatedAt.After(record.Versions[j].CreatedAt) })
	if len(record.Versions) > 0 {
		record.FileCount = record.Versions[0].Files
		record.Bytes = record.Versions[0].Bytes
	}
	// The active copy must still be the version the index names. A hand edit or
	// a half-copied tree is a conflict, never silently served.
	if active, err := skills.LoadBundle(record.Path); err != nil {
		record.Conflict = ConflictMissing
		record.Diagnostics = append(record.Diagnostics, fmt.Sprintf("active copy is unreadable: %v", err))
	} else if active.Hash != item.Current {
		record.Conflict = ConflictModified
		record.Diagnostics = append(record.Diagnostics, "the active copy no longer matches its immutable version")
	}
	if check {
		l.checkOrigin(&record, item)
	}
	return record
}

// checkOrigin compares the recorded origin with the installed version. It is
// deliberately read-only: a page refresh must not change what is installed.
func (l *Library) checkOrigin(record *Record, item entry) {
	if item.Origin.Kind == "" || item.Origin.URL == "" {
		record.Diagnostics = append(record.Diagnostics, "no source is recorded; updates are manual")
		return
	}
	bundle, err := loadOriginBundle(item.Origin, item.Name)
	if err != nil {
		record.Conflict = ConflictSourceMiss
		record.Diagnostics = append(record.Diagnostics, "the recorded source is unavailable: "+err.Error())
		return
	}
	if bundle.Skill.Name != item.Name {
		record.Conflict = ConflictNameChanged
		record.Diagnostics = append(record.Diagnostics, fmt.Sprintf(
			"the source now contains a skill named %q", bundle.Skill.Name))
		return
	}
	if bundle.Hash == item.Current {
		return
	}
	record.UpdateAvailable = true
	record.AvailableHash = bundle.Hash
	if item.Pinned != "" && item.Pinned != bundle.Hash {
		record.Conflict = ConflictPinned
		record.Diagnostics = append(record.Diagnostics,
			"a pinned version holds back the update; unpin before updating")
	}
}

// Show returns one skill with its body and resource manifest.
func (l *Library) Show(tenantID uuid.UUID, name string) (Detail, error) {
	if err := validateName(name); err != nil {
		return Detail{}, err
	}
	var detail Detail
	err := l.withIndex(tenantID, func(idx *index, tenantRoot string) (bool, error) {
		item := idx.entry(name)
		if item == nil {
			return false, errorf(CodeNotFound, "no skill named %q", name)
		}
		record := l.record(*item, tenantRoot, false)
		record.UpdateAvailable = false
		var bundle *skills.Bundle
		if active, loadErr := skills.LoadBundle(record.Path); loadErr == nil {
			bundle = active
		} else {
			// A missing active copy falls back to the immutable version: the
			// content is still readable, and the conflict says the pointer is
			// broken.
			versionDir := filepath.Join(tenantRoot, ".versions", item.Current)
			version, versionErr := skills.LoadBundle(versionDir)
			if versionErr != nil {
				return false, errorf(CodeNotFound, "skill %q has no readable content: %v", name, loadErr)
			}
			bundle = version
		}
		detail = Detail{Record: record, Body: bundle.Skill.Body, Files: bundle.Files}
		return false, nil
	})
	return detail, err
}

// --- install / update / pin / remove -------------------------------------

// Install writes a bundle as a new immutable version and makes it active.
func (l *Library) Install(tenantID uuid.UUID, req InstallRequest) (Record, error) {
	var record Record
	if req.Name != "" {
		if err := validateName(req.Name); err != nil {
			return Record{}, err
		}
	}
	err := l.withIndex(tenantID, func(idx *index, tenantRoot string) (bool, error) {
		bundle, err := l.resolveInstallBundle(idx, req)
		if err != nil {
			return false, err
		}
		name := bundle.Skill.Name
		if err := validateName(name); err != nil {
			return false, err
		}
		if req.Name != "" && req.Name != name {
			return false, errorf(CodeNameConflict,
				"the bundle declares the skill %q, not %q", name, req.Name)
		}
		var source *Source
		if req.SourceID != "" {
			source = idx.source(req.SourceID)
			if source == nil {
				return false, errorf(CodeNotFound, "no source named %q", req.SourceID)
			}
		}
		trust := req.Trust
		if source != nil {
			trust = source.Trust
		}
		if trust == "" {
			trust = TrustLocal
		}
		if trust != TrustTrusted && trust != TrustUntrusted && trust != TrustLocal {
			return false, errorf(CodeInvalid, "trust must be %q, %q or %q", TrustTrusted, TrustUntrusted, TrustLocal)
		}
		if trust == TrustUntrusted && !req.AllowUntrusted {
			return false, errorf(CodeUntrusted,
				"source %q is untrusted; acknowledge the risk with allowUntrusted to install it", trustedLabel(source, req.SourceID))
		}

		item := idx.entry(name)
		if item != nil {
			if item.Pinned != "" && item.Pinned != bundle.Hash {
				return false, errorf(CodePinned,
					"skill %q is pinned at %s; unpin before installing %s", name, short(item.Pinned), short(bundle.Hash))
			}
			if item.SourceID != "" && req.SourceID != "" && item.SourceID != req.SourceID {
				return false, errorf(CodeNameConflict,
					"skill %q is already installed from source %q", name, item.SourceID)
			}
		}
		if err := l.writeVersion(tenantRoot, bundle); err != nil {
			return false, err
		}
		if err := skills.ReplaceBundle(filepath.Join(tenantRoot, "active", name), bundle); err != nil {
			return false, err
		}
		now := l.now()
		if item == nil {
			idx.Skills = append(idx.Skills, entry{
				Name:        name,
				Description: bundle.Skill.Description,
				SourceID:    req.SourceID,
				Origin:      originFor(source),
				Trust:       trust,
				Current:     bundle.Hash,
				InstalledAt: now,
				UpdatedAt:   now,
				Versions:    []Version{versionOf(bundle, now)},
			})
			item = idx.entry(name)
		} else {
			item.Description = bundle.Skill.Description
			if req.SourceID != "" {
				item.Trust = trust
				item.SourceID = req.SourceID
				item.Origin = originFor(source)
			} else if item.SourceID == "" {
				// A manual install/relearn with no source: the request's trust is
				// the record's trust. An existing sourced skill keeps its source
				// trust — installing bytes cannot upgrade a source.
				item.Trust = trust
				if item.Origin.Kind == "" && req.Dir != "" {
					// A manual install from a directory can be updated later from
					// the same directory; the origin is recorded explicitly.
					absolute, absErr := filepath.Abs(req.Dir)
					if absErr == nil {
						item.Origin = origin{Kind: SourceLocal, URL: absolute}
					}
				}
			}
			item.Current = bundle.Hash
			item.UpdatedAt = now
			item.Versions = appendVersion(item.Versions, versionOf(bundle, now))
		}
		if req.Pin {
			item.Pinned = bundle.Hash
		}
		record = l.record(*item, tenantRoot, false)
		return true, nil
	})
	return record, err
}

// Update advances a skill to its source's current bundle. A pinned skill whose
// source moved is a typed `pinned` failure: the pin is the operator's promise,
// not a suggestion.
func (l *Library) Update(tenantID uuid.UUID, name string, req UpdateRequest) (UpdateResult, error) {
	if err := validateName(name); err != nil {
		return UpdateResult{}, err
	}
	var result UpdateResult
	err := l.withIndex(tenantID, func(idx *index, tenantRoot string) (bool, error) {
		item := idx.entry(name)
		if item == nil {
			return false, errorf(CodeNotFound, "no skill named %q", name)
		}
		bundle, err := l.resolveUpdateBundle(*item, idx, req)
		if err != nil {
			return false, err
		}
		if bundle.Skill.Name != name {
			return false, errorf(CodeNameConflict, "the source now contains a skill named %q", bundle.Skill.Name)
		}
		result = UpdateResult{Record: l.record(*item, tenantRoot, false), PreviousHash: item.Current}
		if bundle.Hash == item.Current {
			result.Status = "up_to_date"
			result.Record = l.record(*item, tenantRoot, false)
			return false, nil
		}
		result.AvailableHash = bundle.Hash
		result.UpdateAvailable = true
		if item.Pinned != "" && item.Pinned != bundle.Hash {
			result.Status = "pinned"
			result.PinnedHash = item.Pinned
			return false, errorf(CodePinned,
				"skill %q is pinned at %s; unpin before updating to %s", name, short(item.Pinned), short(bundle.Hash))
		}
		if item.Trust == TrustUntrusted && !req.AllowUntrusted {
			return false, errorf(CodeUntrusted,
				"skill %q came from an untrusted source; acknowledge the update with allowUntrusted", name)
		}
		if err := l.writeVersion(tenantRoot, bundle); err != nil {
			return false, err
		}
		if err := skills.ReplaceBundle(filepath.Join(tenantRoot, "active", name), bundle); err != nil {
			return false, err
		}
		now := l.now()
		item.Current = bundle.Hash
		item.Description = bundle.Skill.Description
		item.UpdatedAt = now
		item.Versions = appendVersion(item.Versions, versionOf(bundle, now))
		if req.SourceID != "" && item.SourceID == "" {
			source := idx.source(req.SourceID)
			if source == nil {
				return false, errorf(CodeNotFound, "no source named %q", req.SourceID)
			}
			item.SourceID = req.SourceID
			item.Origin = originFor(source)
			item.Trust = source.Trust
		}
		result.Status = "updated"
		result.Record = l.record(*item, tenantRoot, false)
		return true, nil
	})
	return result, err
}

// Pin activates an existing version and holds it. The version must already be
// in this skill's history; pinning is not a second install path.
func (l *Library) Pin(tenantID uuid.UUID, name, hash string) (Record, error) {
	if err := validateName(name); err != nil {
		return Record{}, err
	}
	var record Record
	err := l.withIndex(tenantID, func(idx *index, tenantRoot string) (bool, error) {
		item := idx.entry(name)
		if item == nil {
			return false, errorf(CodeNotFound, "no skill named %q", name)
		}
		if hash == "" {
			hash = item.Current
		}
		if !hasVersion(item.Versions, hash) {
			return false, errorf(CodeVersionMissing, "skill %q has no version %s", name, short(hash))
		}
		if item.Pinned == hash && item.Current == hash {
			record = l.record(*item, tenantRoot, false)
			return false, nil
		}
		versionDir := filepath.Join(tenantRoot, ".versions", hash)
		bundle, err := skills.LoadBundle(versionDir)
		if err != nil {
			return false, errorf(CodeBundleMismatch, "version %s is unreadable: %v", short(hash), err)
		}
		if err := skills.ReplaceBundle(filepath.Join(tenantRoot, "active", name), bundle); err != nil {
			return false, err
		}
		item.Current = hash
		item.Pinned = hash
		item.UpdatedAt = l.now()
		record = l.record(*item, tenantRoot, false)
		return true, nil
	})
	return record, err
}

// Unpin releases the hold; the active version stays whatever it was.
func (l *Library) Unpin(tenantID uuid.UUID, name string) (Record, error) {
	if err := validateName(name); err != nil {
		return Record{}, err
	}
	var record Record
	err := l.withIndex(tenantID, func(idx *index, tenantRoot string) (bool, error) {
		item := idx.entry(name)
		if item == nil {
			return false, errorf(CodeNotFound, "no skill named %q", name)
		}
		if item.Pinned == "" {
			record = l.record(*item, tenantRoot, false)
			return false, nil
		}
		item.Pinned = ""
		item.UpdatedAt = l.now()
		record = l.record(*item, tenantRoot, false)
		return true, nil
	})
	return record, err
}

// Remove deletes an installed skill and its active copy. Version objects still
// referenced by another skill stay; unreferenced ones are collected.
func (l *Library) Remove(tenantID uuid.UUID, name string) (bool, error) {
	if err := validateName(name); err != nil {
		return false, err
	}
	removed := false
	err := l.withIndex(tenantID, func(idx *index, tenantRoot string) (bool, error) {
		if idx.entry(name) == nil {
			return false, errorf(CodeNotFound, "no skill named %q", name)
		}
		if err := os.RemoveAll(filepath.Join(tenantRoot, "active", name)); err != nil {
			return false, err
		}
		idx.removeEntry(name)
		l.collectVersions(tenantRoot, idx)
		removed = true
		return true, nil
	})
	return removed, err
}

// --- sources -------------------------------------------------------------

// Sources lists the tenant subscriptions.
func (l *Library) Sources(tenantID uuid.UUID) ([]Source, error) {
	var sources []Source
	err := l.withIndex(tenantID, func(idx *index, _ string) (bool, error) {
		sources = append([]Source(nil), idx.Sources...)
		if sources == nil {
			sources = []Source{}
		}
		return false, nil
	})
	return sources, err
}

// Subscribe adds a local source. Remote kinds are refused with
// unsupported_source: accepting a source this build cannot sync would make
// "check for updates" a lie.
func (l *Library) Subscribe(tenantID uuid.UUID, source Source) (Source, error) {
	if source.Trust == "" {
		source.Trust = TrustUntrusted
	}
	if err := validateSource(source); err != nil {
		return Source{}, err
	}
	var stored Source
	err := l.withIndex(tenantID, func(idx *index, _ string) (bool, error) {
		if existing := idx.source(source.ID); existing != nil {
			if existing.Kind == source.Kind && existing.URL == source.URL && existing.Trust == source.Trust {
				stored = *existing
				return false, nil
			}
			return false, errorf(CodeAlreadyExists, "source %q already exists", source.ID)
		}
		source.CreatedAt = l.now()
		idx.Sources = append(idx.Sources, source)
		stored = source
		return true, nil
	})
	return stored, err
}

// Unsubscribe removes a source. A source still referenced by an installed skill
// is refused: removing it would make the update origin disappear silently.
func (l *Library) Unsubscribe(tenantID uuid.UUID, id string) (bool, error) {
	if strings.TrimSpace(id) == "" {
		return false, errorf(CodeInvalid, "a source id is required")
	}
	removed := false
	err := l.withIndex(tenantID, func(idx *index, _ string) (bool, error) {
		if idx.source(id) == nil {
			return false, errorf(CodeNotFound, "no source named %q", id)
		}
		for _, item := range idx.Skills {
			if item.SourceID == id {
				return false, errorf(CodeSourceInUse,
					"source %q is still the origin of skill %q; remove that skill first", id, item.Name)
			}
		}
		out := idx.Sources[:0]
		for _, source := range idx.Sources {
			if source.ID != id {
				out = append(out, source)
			}
		}
		idx.Sources = out
		removed = true
		return true, nil
	})
	return removed, err
}

// --- bundle resolution ---------------------------------------------------

func (l *Library) resolveInstallBundle(idx *index, req InstallRequest) (*skills.Bundle, error) {
	if len(req.Files) > 0 {
		bundle, err := skills.BundleFromFilesNamed(req.Files, req.Name)
		if err != nil {
			return nil, bundleError(err)
		}
		return bundle, nil
	}
	if req.Dir != "" {
		return loadDirBundle(req.Dir, req.Name)
	}
	if req.SourceID != "" {
		source := idx.source(req.SourceID)
		if source == nil {
			return nil, errorf(CodeNotFound, "no source named %q", req.SourceID)
		}
		if req.Name == "" {
			return nil, errorf(CodeInvalid, "installing from source %q needs the skill name", req.SourceID)
		}
		return loadOriginBundle(originFor(source), req.Name)
	}
	if req.Name == "" {
		return nil, errorf(CodeInvalid, "an install needs files, a directory, a source, or a name with a description and body")
	}
	if req.Description == "" {
		return nil, errorf(CodeInvalid, "an install needs a description")
	}
	document, err := skills.ComposeSkillDocument(req.Name, req.Description, req.Body, nil)
	if err != nil {
		return nil, bundleError(err)
	}
	bundle, err := skills.BundleFromFilesNamed([]skills.FileInput{{Path: skills.SkillFileName, Content: document}}, req.Name)
	if err != nil {
		return nil, bundleError(err)
	}
	return bundle, nil
}

func (l *Library) resolveUpdateBundle(item entry, idx *index, req UpdateRequest) (*skills.Bundle, error) {
	if len(req.Files) > 0 {
		bundle, err := skills.BundleFromFilesNamed(req.Files, item.Name)
		if err != nil {
			return nil, bundleError(err)
		}
		return bundle, nil
	}
	if req.Dir != "" {
		return loadDirBundle(req.Dir, item.Name)
	}
	if req.Description != "" {
		document, err := skills.ComposeSkillDocument(item.Name, req.Description, req.Body, nil)
		if err != nil {
			return nil, bundleError(err)
		}
		bundle, err := skills.BundleFromFilesNamed([]skills.FileInput{{Path: skills.SkillFileName, Content: document}}, item.Name)
		if err != nil {
			return nil, bundleError(err)
		}
		return bundle, nil
	}
	if item.Origin.Kind != "" && item.Origin.URL != "" {
		return loadOriginBundle(item.Origin, item.Name)
	}
	if req.SourceID != "" {
		source := idx.source(req.SourceID)
		if source == nil {
			return nil, errorf(CodeNotFound, "no source named %q", req.SourceID)
		}
		return loadOriginBundle(originFor(source), item.Name)
	}
	return nil, errorf(CodeNoSourceRecorded,
		"skill %q has no source to update from; pass files or a directory", item.Name)
}

// loadDirBundle reads a directory that is either one skill (SKILL.md at its
// root) or a directory of skills (one subdirectory named after the skill).
func loadDirBundle(dir, name string) (*skills.Bundle, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, errorf(CodeInvalid, "resolve %q: %v", dir, err)
	}
	if fileExists(filepath.Join(abs, skills.SkillFileName)) {
		bundle, err := skills.LoadBundle(abs)
		if err != nil {
			return nil, bundleError(err)
		}
		if name != "" && bundle.Skill.Name != name {
			return nil, errorf(CodeNameConflict,
				"the bundle at %s declares the skill %q, not %q", dir, bundle.Skill.Name, name)
		}
		return bundle, nil
	}
	if name != "" {
		candidate := filepath.Join(abs, name)
		if fileExists(filepath.Join(candidate, skills.SkillFileName)) {
			bundle, err := skills.LoadBundle(candidate)
			if err != nil {
				return nil, bundleError(err)
			}
			return bundle, nil
		}
	}
	return nil, errorf(CodeInvalid, "%s holds no %s (checked the directory and its %q subdirectory)",
		dir, skills.SkillFileName, name)
}

func loadOriginBundle(source origin, name string) (*skills.Bundle, error) {
	if source.Kind != SourceLocal {
		return nil, errorf(CodeUnsupported, "source kind %q is not supported", source.Kind)
	}
	if source.URL == "" {
		return nil, errorf(CodeNoSourceRecorded, "the source has no URL")
	}
	abs, err := filepath.Abs(source.URL)
	if err != nil {
		return nil, errorf(CodeSourceMissing, "resolve %q: %v", source.URL, err)
	}
	if fileExists(filepath.Join(abs, skills.SkillFileName)) {
		bundle, err := skills.LoadBundle(abs)
		if err != nil {
			return nil, bundleError(err)
		}
		return bundle, nil
	}
	candidate := filepath.Join(abs, name)
	if fileExists(filepath.Join(candidate, skills.SkillFileName)) {
		bundle, err := skills.LoadBundle(candidate)
		if err != nil {
			return nil, bundleError(err)
		}
		return bundle, nil
	}
	return nil, errorf(CodeSourceMissing, "source %s holds no skill %q", source.URL, name)
}

// --- internals -----------------------------------------------------------

func (l *Library) writeVersion(tenantRoot string, bundle *skills.Bundle) error {
	dir := filepath.Join(tenantRoot, ".versions", bundle.Hash)
	if err := skills.WriteBundle(dir, bundle); err != nil {
		if errors.Is(err, skills.ErrBundleMismatch) {
			return errorf(CodeBundleMismatch, "%v", err)
		}
		return bundleError(err)
	}
	return nil
}

func (l *Library) collectVersions(tenantRoot string, idx *index) {
	referenced := make(map[string]struct{})
	for _, item := range idx.Skills {
		for _, version := range item.Versions {
			referenced[version.Hash] = struct{}{}
		}
	}
	entries, err := os.ReadDir(filepath.Join(tenantRoot, ".versions"))
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, keep := referenced[entry.Name()]; keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(tenantRoot, ".versions", entry.Name())); err != nil {
			l.logger.Warn("skilllib: collect version", "version", entry.Name(), "error", err)
		}
	}
}

func originFor(source *Source) origin {
	if source == nil {
		return origin{}
	}
	return origin{Kind: source.Kind, URL: source.URL}
}

func versionOf(bundle *skills.Bundle, at time.Time) Version {
	var bytes int64
	for _, file := range bundle.Files {
		bytes += file.Size
	}
	return Version{Hash: bundle.Hash, CreatedAt: at, Files: len(bundle.Files), Bytes: bytes}
}

func appendVersion(versions []Version, version Version) []Version {
	for _, existing := range versions {
		if existing.Hash == version.Hash {
			return versions
		}
	}
	return append(versions, version)
}

func hasVersion(versions []Version, hash string) bool {
	for _, version := range versions {
		if version.Hash == hash {
			return true
		}
	}
	return false
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func bundleError(err error) *Error {
	if typed, ok := err.(*Error); ok {
		return typed
	}
	return errorf(CodeInvalid, "%v", err)
}

var sourceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
var skillNamePattern = regexp.MustCompile(`^[a-z0-9-]+$`)

func validateName(name string) error {
	if name == "" {
		return errorf(CodeInvalid, "a skill name is required")
	}
	if len(name) > skills.MaxNameLength || !skillNamePattern.MatchString(name) {
		return errorf(CodeInvalid,
			"skill name %q must be lowercase a-z, 0-9 and hyphens (max %d characters)", name, skills.MaxNameLength)
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") || strings.Contains(name, "--") {
		return errorf(CodeInvalid, "skill name %q must not start/end with a hyphen or contain consecutive hyphens", name)
	}
	return nil
}

func validateSource(source Source) error {
	if !sourceIDPattern.MatchString(source.ID) {
		return errorf(CodeInvalid, "source id must match [A-Za-z0-9][A-Za-z0-9._-]{0,63}")
	}
	if source.Kind != SourceLocal {
		return errorf(CodeUnsupported,
			"source kind %q is not supported by this build (only %q)", source.Kind, SourceLocal)
	}
	if strings.TrimSpace(source.URL) == "" {
		return errorf(CodeInvalid, "a source URL is required")
	}
	abs, err := filepath.Abs(source.URL)
	if err != nil {
		return errorf(CodeInvalid, "resolve source %q: %v", source.URL, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return errorf(CodeSourceMissing, "source directory %q is unavailable: %v", source.URL, err)
	}
	if !info.IsDir() {
		return errorf(CodeInvalid, "source %q is not a directory", source.URL)
	}
	if source.Trust == "" {
		source.Trust = TrustUntrusted
	}
	if source.Trust != TrustTrusted && source.Trust != TrustUntrusted {
		return errorf(CodeInvalid, "source trust must be %q or %q", TrustTrusted, TrustUntrusted)
	}
	return nil
}

func trustedLabel(source *Source, id string) string {
	if source != nil {
		return source.ID
	}
	return id
}

func short(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

// Bundle returns the installed skill's current bytes (the active copy, with the
// immutable version as fallback). It is the read behind patch/write_file: the
// agent edits what is installed, not a stale copy it brought along.
func (l *Library) Bundle(tenantID uuid.UUID, name string) (*skills.Bundle, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	var bundle *skills.Bundle
	err := l.withIndex(tenantID, func(idx *index, tenantRoot string) (bool, error) {
		item := idx.entry(name)
		if item == nil {
			return false, errorf(CodeNotFound, "no skill named %q", name)
		}
		active, err := skills.LoadBundle(filepath.Join(tenantRoot, "active", name))
		if err == nil {
			bundle = active
			return false, nil
		}
		version, versionErr := skills.LoadBundle(filepath.Join(tenantRoot, ".versions", item.Current))
		if versionErr != nil {
			return false, errorf(CodeNotFound, "skill %q has no readable content: %v", name, err)
		}
		bundle = version
		return false, nil
	})
	return bundle, err
}

// SkillsForDiscovery returns the tenant's active skills as discovery records.
// It is what the toolsurface backend merges into `skills ls`/`view`, so a newly
// installed skill is visible in the same turn even if the run's system prompt
// was assembled earlier.
func (l *Library) SkillsForDiscovery(ctx context.Context, tenantID uuid.UUID) ([]skills.Skill, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snapshot, err := l.List(tenantID, false)
	if err != nil {
		return nil, err
	}
	out := make([]skills.Skill, 0, len(snapshot.Skills))
	for _, record := range snapshot.Skills {
		bundle, err := skills.LoadBundle(record.Path)
		if err != nil {
			l.logger.Warn("skilllib: skip unreadable active skill", "skill", record.Name, "error", err)
			continue
		}
		out = append(out, *bundle.WithPath(record.Path).Skill)
	}
	return out, nil
}
