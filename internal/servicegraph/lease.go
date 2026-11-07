package servicegraph

import (
	"context"
	"errors"
)

// Lease pins one generation of the assembly for one in-flight run.
//
// Safer resolution: while a run holds a lease, a reassembly that replaces an
// instance moves it to RETIRING instead of unloading it, and Lease.Lookup
// keeps returning the pinned instances. New runs acquire the newest
// generation; a retiring generation accepts no new runs.
//
// Lease is an in-process reference handle. Store-level leases (owner/epoch
// fencing in the contract kernel) are a different mechanism and are not
// affected by it.
type Lease struct {
	e          *Engine
	generation int64
	loadoutID  string
	revision   int64
	states     map[string]*serviceState
	done       bool
	broken     bool
}

// Generation returns the assembly generation this lease pinned.
func (l *Lease) Generation() int64 { return l.generation }

// LoadoutID returns the loadout id at acquire time (for run metadata).
func (l *Lease) LoadoutID() string { return l.loadoutID }

// Revision returns the loadout revision at acquire time; a run pins this
// revision, and a later configuration change never mutates it.
func (l *Lease) Revision() int64 { return l.revision }

// Lookup returns the instance this generation served for a service. It keeps
// working after the instance moved to RETIRING; it fails once the lease is
// released, force-collected, or the engine is closed.
func (l *Lease) Lookup(name string) (Instance, bool) {
	if l == nil || l.e == nil {
		return nil, false
	}
	e := l.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if l.done || l.broken {
		return nil, false
	}
	st, ok := l.states[name]
	if !ok || st.instance == nil {
		return nil, false
	}
	return st.instance, true
}

// Broken reports whether the lease was invalidated by a forced drain (the
// run holding it must fail) or by engine close.
func (l *Lease) Broken() bool {
	if l == nil || l.e == nil {
		return true
	}
	e := l.e
	e.mu.Lock()
	defer e.mu.Unlock()
	return l.broken
}

// Release drops the reference. When the last reference to a retiring
// generation drains, its instances are unloaded in reverse order. Release is
// idempotent.
func (l *Lease) Release(ctx context.Context) error {
	if l == nil || l.e == nil {
		return nil
	}
	e := l.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if l.done {
		return nil
	}
	l.done = true
	for _, st := range l.states {
		if st.pins > 0 {
			st.pins--
		}
	}
	delete(e.leases, l)
	if e.closed {
		return nil
	}
	return errors.Join(e.drainLocked(ctx)...)
}
