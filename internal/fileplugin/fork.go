package fileplugin

import (
	"context"
	"sort"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/layerfs"
	"github.com/jixoai/keel/internal/sessionfork"
)

// ForkBaseSource resolves the fork origin of a session. *store.Store implements
// it; a deployment without the fork columns leaves it nil and every session
// behaves as an unforked session.
//
// The file view of a fork is an overlay, not a copy: the inherited half is read
// from the parent's snapshot tree object by hash, and only the child's own
// writes live in file_manifest. That is what makes fork O(1) and keeps a parent
// deletion from freeing a blob the child still references.
type ForkBaseSource interface {
	SessionFork(ctx context.Context, tenantID, sessionID uuid.UUID) (sessionfork.Fact, bool, error)
}

// forkViewCacheSize bounds the in-process fork views. A view is derived state
// (one tree hash + its flattened entries), so dropping one only costs a re-read.
const forkViewCacheSize = 128

type forkViewKey struct {
	tenantID  uuid.UUID
	sessionID uuid.UUID
}

type forkView struct {
	treeHash string
	origin   int64
	byPath   map[string]Entry
	entries  []Entry
}

// forkBaseEntries returns the inherited entries of a session, or nil when the
// session is not a fork (or the fork had no files). It never copies manifest
// rows: the entries are decoded from the parent's tree object and cached. The
// fact is immutable once recorded (the store refuses a re-point), so a cached
// view — including a negative one — cannot go stale for a live session.
func (s *Service) forkBaseEntries(ctx context.Context, tenantID, sessionID uuid.UUID) ([]Entry, error) {
	if s.forks == nil {
		return nil, nil
	}
	key := forkViewKey{tenantID: tenantID, sessionID: sessionID}
	s.forkMu.Lock()
	if view := s.forkViews[key]; view != nil {
		entries := view.entries
		s.forkMu.Unlock()
		return entries, nil
	}
	s.forkMu.Unlock()

	fact, found, err := s.forks.SessionFork(ctx, tenantID, sessionID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	if !found || fact.TreeHash == "" {
		s.cacheForkView(key, &forkView{})
		return nil, nil
	}

	tree, err := parseForkTree(fact.TreeHash)
	if err != nil {
		return nil, err
	}
	files, err := s.store.FlattenTree(tree)
	if err != nil {
		return nil, mapLayerfsError(err)
	}
	view := &forkView{
		treeHash: fact.TreeHash,
		origin:   fact.OriginSeq,
		byPath:   make(map[string]Entry, len(files)),
		entries:  make([]Entry, 0, len(files)),
	}
	for _, file := range files {
		entry := Entry{
			Path: file.Path, Hash: file.Hash, Mode: file.Mode, Size: file.Size,
			// The inherited view's sequence is the fork origin: it is the log
			// position at which the child came to see these bytes.
			UpdatedSeq: fact.OriginSeq,
		}
		view.byPath[file.Path] = entry
		view.entries = append(view.entries, entry)
	}
	sort.Slice(view.entries, func(i, j int) bool { return view.entries[i].Path < view.entries[j].Path })
	s.cacheForkView(key, view)
	return view.entries, nil
}

// cacheForkView stores one derived view, bounded.
func (s *Service) cacheForkView(key forkViewKey, view *forkView) {
	s.forkMu.Lock()
	defer s.forkMu.Unlock()
	if s.forkViews == nil {
		s.forkViews = make(map[forkViewKey]*forkView)
	}
	if len(s.forkViews) >= forkViewCacheSize {
		// Bounded cache: dropping any one view is safe (it is re-read on demand).
		for drop := range s.forkViews {
			delete(s.forkViews, drop)
			break
		}
	}
	s.forkViews[key] = view
}

// effectiveGet resolves one path through the session's overlay: the child's
// manifest row wins, then the inherited fork view.
func (s *Service) effectiveGet(ctx context.Context, tenantID, sessionID uuid.UUID, path string) (Entry, bool, error) {
	entry, ok, err := s.manifest.Get(ctx, tenantID, sessionID, path)
	if err != nil {
		return Entry{}, false, mapStoreError(err)
	}
	if ok {
		return entry, true, nil
	}
	entries, err := s.forkBaseEntries(ctx, tenantID, sessionID)
	if err != nil {
		return Entry{}, false, err
	}
	for _, inherited := range entries {
		if inherited.Path == path {
			return inherited, true, nil
		}
	}
	return Entry{}, false, nil
}

// effectiveList returns the session's merged view: child manifest rows over the
// inherited fork view, sorted by path.
func (s *Service) effectiveList(ctx context.Context, tenantID, sessionID uuid.UUID) ([]Entry, error) {
	own, err := s.manifest.List(ctx, tenantID, sessionID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	inherited, err := s.forkBaseEntries(ctx, tenantID, sessionID)
	if err != nil {
		return nil, err
	}
	if len(inherited) == 0 {
		return own, nil
	}
	merged := make(map[string]Entry, len(own)+len(inherited))
	for _, entry := range inherited {
		merged[entry.Path] = entry
	}
	for _, entry := range own {
		merged[entry.Path] = entry
	}
	out := make([]Entry, 0, len(merged))
	for _, entry := range merged {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// forkBaseMap is the overlay half of a capture reconciliation: paths the child
// inherited unchanged must not be re-upserted as its own manifest rows.
func (s *Service) forkBaseMap(ctx context.Context, tenantID, sessionID uuid.UUID) (map[string]Entry, error) {
	entries, err := s.forkBaseEntries(ctx, tenantID, sessionID)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	out := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		out[entry.Path] = entry
	}
	return out, nil
}

// parseForkTree parses the stored tree hash. A malformed hash is a corrupt
// projection, not a "no files" answer.
func parseForkTree(raw string) (layerfs.Hash, error) {
	return layerfs.ParseHash(raw)
}
