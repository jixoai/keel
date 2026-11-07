// Package servicegraph implements the Keel service graph: statically
// enumerable service descriptors, immutable loadout snapshots, dependency
// resolution, and the lifecycle that activates, reassembles, and tears down
// service instances.
//
// The graph is deliberately independent of the rest of the runtime (cmd,
// httpapi, agent): hosting processes bind concrete implementations through
// Factory functions, and services receive narrow interfaces through their
// resolved bindings — never a raw event-store write handle. See README.md.
//
// Reassembly is run-aware: an in-flight run acquires a Lease on the current
// generation, and instances that a new assembly would replace enter RETIRING
// instead of being unloaded under the run's feet. They are released when the
// last reference drains, or when the configured drain deadline expires (the
// holder must then fail its run). In-flight runs pin a loadout revision;
// cross-process upgrades are the runtime handoff protocol's job, not this
// engine's.
package servicegraph

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	// ErrClosed is returned by Apply/Acquire after the engine has been closed.
	ErrClosed = errors.New("servicegraph: engine closed")
	// ErrNotAssembled is returned by Acquire before any loadout was applied.
	ErrNotAssembled = errors.New("servicegraph: no loadout applied")
)

// Option configures an Engine.
type Option func(*Engine)

// WithLogger attaches a logger. Every Apply logs one structured line per
// generation.
func WithLogger(l *slog.Logger) Option { return func(e *Engine) { e.log = l } }

// WithRetireTimeout sets the drain deadline for RETIRING generations. When a
// retiring generation still has references after the timeout,
// CollectExpired (also invoked by Apply and Acquire) force-unloads it and
// invalidates the outstanding leases: the caller must fail those runs. Zero
// (the default) waits for references to drain without a deadline.
func WithRetireTimeout(d time.Duration) Option { return func(e *Engine) { e.retireTimeout = d } }

// WithClock overrides the clock used for drain deadlines (tests).
func WithClock(now func() time.Time) Option {
	return func(e *Engine) {
		if now != nil {
			e.now = now
		}
	}
}

// Engine assembles and reassembles one process-level service graph.
//
// Apply is the only entry point: it validates the loadout (rejecting cycles
// and duplicate providers before anything starts), unloads services that were
// removed or whose resolution changed — dependents before providers — and then
// activates what is left in dependency order. Services that in-flight runs
// still reference enter RETIRING instead of unloading immediately. A service
// activation failure is isolated: already-started services stay ACTIVE,
// dependents of the failed service remain PENDING with diagnostics.
//
// Every accepted Apply increments the engine generation; instances record the
// generation they were loaded in. Reassembly in-process is for boot and
// controlled flows — live tenant upgrades go through the runtime handoff
// protocol (new process, drain, epoch+1).
type Engine struct {
	mu         sync.Mutex
	log        *slog.Logger
	loadout    *Loadout
	graph      *Graph
	generation int64
	services   map[string]*serviceState
	diags      []Diagnostic
	closed     bool

	leases        map[*Lease]bool
	retiring      map[int64]*retiringEntry
	retiringOrder []int64 // generations with live retiring entries, in retire order
	retireTimeout time.Duration
	now           func() time.Time
}

type serviceState struct {
	desc             ServiceDescriptor
	state            State
	instance         Instance
	scope            *EffectScope
	bindings         map[string]string
	loadedGeneration int64
	failureCode      string
	failure          string
	waiting          []string
	degraded         []string
	lastAttempt      int64
	// pins counts in-flight leases that reference this instance. A pinned
	// instance is never unloaded in place; it retires first.
	pins int
}

// retiringEntry groups the instances of one generation that were replaced
// while in-flight references existed.
type retiringEntry struct {
	generation int64
	deadline   time.Time // zero when no drain deadline is configured
	states     map[string]*serviceState
	order      []string // reverse dependency order for release
}

// NewEngine builds an empty engine; nothing is loaded until Apply.
func NewEngine(opts ...Option) *Engine {
	e := &Engine{
		services: map[string]*serviceState{},
		leases:   map[*Lease]bool{},
		retiring: map[int64]*retiringEntry{},
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Apply resolves, validates, and assembles a loadout.
//
// On a rejected loadout (*ResolveError) nothing changes: the previous
// generation stays live. Otherwise the reassembly is committed; a non-nil
// *ActivationError reports services that failed to start (their dependents are
// PENDING, everything else is live). Instances still referenced by leases move
// to RETIRING rather than unloading.
func (e *Engine) Apply(ctx context.Context, lo *Loadout) (*Snapshot, error) {
	if lo == nil {
		return nil, errors.New("servicegraph: apply nil loadout")
	}
	graph, err := Resolve(lo)
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, ErrClosed
	}

	e.generation++
	gen := e.generation
	e.diags = e.diags[:0]
	e.collectExpiredLocked(ctx)

	// 1. Retire or unload removed/dirty services, dependents before providers.
	for _, name := range e.teardownOrderLocked(graph) {
		st := e.services[name]
		if st == nil {
			continue
		}
		if st.instance != nil && st.pins > 0 {
			e.retireLocked(gen, name, st)
			continue
		}
		e.unloadLocked(ctx, name)
	}

	// 2. Adopt the new graph.
	for name := range e.services {
		if _, ok := graph.loadout.Service(name); !ok {
			delete(e.services, name)
		}
	}
	for _, d := range graph.loadout.services {
		st, ok := e.services[d.Name]
		if !ok {
			st = &serviceState{}
			e.services[d.Name] = st
		}
		st.desc = d
		if st.state == "" {
			st.state = StatePending
		}
	}
	e.loadout, e.graph = graph.loadout, graph

	// 3. Activate in dependency order.
	var failures []ServiceFailure
	for _, name := range graph.order {
		st := e.services[name]
		if st.state == StateActive {
			continue
		}
		if st.state == StateFailed {
			// Retry on a new generation; the descriptor is unchanged but the
			// environment may have recovered.
			st.state = StatePending
			st.failureCode, st.failure = "", ""
		}
		if graph.standby[name] {
			st.state = StatePending
			st.waiting, st.degraded = nil, nil
			continue
		}
		st.waiting, st.degraded = nil, nil
		if !e.ensure(ctx, graph, name, map[string]bool{}) && st.state == StateFailed {
			failures = append(failures, ServiceFailure{Service: name, Code: st.failureCode, Err: st.failure})
		}
	}

	// 4. Explain standby providers after activation, once we know which of
	// them were promoted to serve.
	for _, name := range graph.order {
		if !graph.standby[name] {
			continue
		}
		st := e.services[name]
		primary := graph.primaryOf(name)
		primaryState := StatePending
		if pst, ok := e.services[primary]; ok {
			primaryState = pst.state
		}
		switch st.state {
		case StateActive:
			e.addDiag(gen, Diagnostic{
				Service:  name,
				Code:     DiagStandbyServing,
				Severity: SeverityInfo,
				Message:  fmt.Sprintf("%s serves as the active provider (declared fallback; primary %s, state %s)", name, primary, primaryState),
			})
		case StatePending:
			if st.lastAttempt == 0 {
				e.addDiag(gen, Diagnostic{
					Service:  name,
					Code:     DiagStandby,
					Severity: SeverityInfo,
					Message:  fmt.Sprintf("standby fallback: %s is not the active provider, %s is", name, primary),
				})
			}
		}
	}

	if e.log != nil {
		e.log.Info("loadout applied",
			"loadout", graph.loadout.id,
			"revision", graph.loadout.revision,
			"generation", gen,
			"services", len(graph.order),
			"active", e.countActiveLocked(),
			"failed", len(failures),
			"retiring_generations", len(e.retiringOrder))
	}
	snap := e.snapshotLocked()
	if len(failures) > 0 {
		return snap, &ActivationError{Generation: gen, Failures: failures}
	}
	return snap, nil
}

// Acquire pins the current generation for one in-flight run: every ACTIVE
// instance is referenced, and a later Apply that replaces any of them moves
// the old instance to RETIRING instead of unloading it. New runs always pin
// the newest generation; a retiring generation accepts no new runs.
//
// Release the lease when the run finishes.
func (e *Engine) Acquire() (*Lease, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, ErrClosed
	}
	if e.loadout == nil {
		return nil, ErrNotAssembled
	}
	e.collectExpiredLocked(context.Background())
	states := make(map[string]*serviceState, len(e.services))
	for name, st := range e.services {
		if st.instance != nil {
			states[name] = st
			st.pins++
		}
	}
	lease := &Lease{
		e:          e,
		generation: e.generation,
		loadoutID:  e.loadout.id,
		revision:   e.loadout.revision,
		states:     states,
	}
	e.leases[lease] = true
	return lease, nil
}

// CollectExpired force-unloads retiring generations whose drain deadline has
// passed, invalidating the references still outstanding: the in-flight runs
// holding them must be failed by the caller. It returns the number of
// instances force-unloaded. Apply and Acquire call it too, so a hosting loop
// that keeps applying or acquiring honors deadlines without its own timer.
func (e *Engine) CollectExpired(ctx context.Context) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.collectExpiredLocked(ctx)
}

// Close unloads every service in reverse dependency order and releases its
// effects, including generations still RETIRING (their reference holders are
// invalidated and must fail). It is idempotent; Apply/Acquire afterwards
// return ErrClosed.
func (e *Engine) Close(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	var errs []error
	if e.graph != nil {
		order := append([]string(nil), e.graph.order...)
		for i := len(order) - 1; i >= 0; i-- {
			errs = append(errs, e.unloadLocked(ctx, order[i])...)
		}
	}
	for _, gen := range e.retiringOrder {
		entry := e.retiring[gen]
		if entry == nil {
			continue
		}
		for _, name := range entry.order {
			st := entry.states[name]
			if st == nil {
				continue
			}
			errs = append(errs, e.unloadStateLocked(ctx, name, st)...)
		}
		delete(e.retiring, gen)
	}
	e.retiringOrder = nil
	for lease := range e.leases {
		lease.broken = true
	}
	e.closed = true
	e.diags = e.diags[:0]
	return errors.Join(errs...)
}

// Snapshot returns the current structured view: states, bindings, degraded
// capabilities, failures, effects, retiring generations, and diagnostics.
func (e *Engine) Snapshot() *Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshotLocked()
}

// Generation returns the current engine generation.
func (e *Engine) Generation() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.generation
}

// Lookup returns the live instance of an ACTIVE service. Runs should resolve
// instances through their Lease instead, so a reassembly cannot swap the
// instance under them.
func (e *Engine) Lookup(name string) (Instance, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.services[name]
	if !ok || st.instance == nil || st.state != StateActive {
		return nil, false
	}
	return st.instance, true
}

// ensure activates one service on demand: dependencies first, then the
// factory, then Start. It returns true when the service is ACTIVE.
func (e *Engine) ensure(ctx context.Context, g *Graph, name string, stack map[string]bool) bool {
	st := e.services[name]
	switch st.state {
	case StateActive:
		return true
	case StateFailed, StateLoading, StateUnloading:
		return false
	}
	if st.lastAttempt == e.generation {
		// Already attempted in this generation; the result stands.
		return false
	}
	if stack[name] {
		// Defense in depth: Resolve rejects cycles before activation.
		return false
	}
	stack[name] = true
	defer delete(stack, name)

	st.lastAttempt = e.generation
	st.state = StateLoading
	st.waiting, st.degraded = nil, nil

	bindings := make(map[string]string, len(st.desc.Requires)+len(st.desc.Optional))
	required := append([]string(nil), st.desc.Requires...)
	sort.Strings(required)
	for _, capability := range required {
		bound, blockedOn := e.resolveDependency(ctx, g, st, capability, g.chains[capability], bindings, stack, true)
		if !bound {
			st.state = StatePending
			if blockedOn != "" {
				st.waiting = append(st.waiting, blockedOn)
			}
			return false
		}
	}
	optional := append([]string(nil), st.desc.Optional...)
	sort.Strings(optional)
	for _, capability := range optional {
		e.resolveDependency(ctx, g, st, capability, g.chains[capability], bindings, stack, false)
	}

	if st.desc.Factory == nil {
		st.state = StateFailed
		st.failureCode = DiagNoFactory
		st.failure = fmt.Sprintf("service %q has no factory", name)
		e.addDiag(e.generation, Diagnostic{
			Service:  name,
			Code:     DiagNoFactory,
			Severity: SeverityError,
			Message:  fmt.Sprintf("service %q declares no factory; the loadout builder must attach one", name),
		})
		return false
	}

	anyBindings := make(map[string]any, len(bindings))
	for capability, provider := range bindings {
		anyBindings[capability] = e.services[provider].instance
	}
	instance, err := st.desc.Factory(ctx, Resolved{Service: name, Bindings: anyBindings})
	if err != nil {
		st.state = StateFailed
		st.failureCode = DiagFactoryFailed
		st.failure = err.Error()
		e.addDiag(e.generation, Diagnostic{
			Service: name,
			Code:    DiagFactoryFailed,
			Err:     err.Error(),
			Message: fmt.Sprintf("factory for %q failed: %v", name, err),
		})
		return false
	}
	if instance == nil {
		st.state = StateFailed
		st.failureCode = DiagFactoryFailed
		st.failure = "factory returned a nil instance"
		e.addDiag(e.generation, Diagnostic{
			Service:  name,
			Code:     DiagFactoryFailed,
			Severity: SeverityError,
			Message:  fmt.Sprintf("factory for %q returned a nil instance", name),
		})
		return false
	}

	scope := newEffectScope(name, e.generation)
	if err := instance.Start(ctx, scope); err != nil {
		releaseErrs := scope.closeAndRelease()
		st.state = StateFailed
		st.failureCode = DiagStartFailed
		st.failure = err.Error()
		e.addDiag(e.generation, Diagnostic{
			Service: name,
			Code:    DiagStartFailed,
			Err:     err.Error(),
			Message: fmt.Sprintf("service %q failed to start: %v; its registered effects were released", name, err),
		})
		for _, releaseErr := range releaseErrs {
			e.addDiag(e.generation, Diagnostic{
				Service:  name,
				Code:     DiagEffectRelease,
				Severity: SeverityDegraded,
				Err:      releaseErr.Error(),
				Message:  fmt.Sprintf("releasing failed-start effects of %q: %v", name, releaseErr),
			})
		}
		return false
	}

	st.instance = instance
	st.scope = scope
	st.bindings = bindings
	st.loadedGeneration = e.generation
	st.state = StateActive
	st.waiting = nil
	return true
}

// resolveDependency binds one capability to a live provider.
//
// Required: a missing provider records missing-required; a merely PENDING
// provider stops resolution (no fallback takeover while the primary may still
// come up); a FAILED provider promotes the next fallback.
//
// Optional: any failure degrades the service and records it, never blocks.
func (e *Engine) resolveDependency(ctx context.Context, g *Graph, st *serviceState, capability string, chain []ProviderRef, bindings map[string]string, stack map[string]bool, required bool) (bool, string) {
	gen := e.generation
	if len(chain) == 0 {
		if required {
			e.addDiag(gen, Diagnostic{
				Service:    st.desc.Name,
				Code:       DiagMissingRequired,
				Severity:   SeverityError,
				Capability: capability,
				Message:    fmt.Sprintf("no provider for required capability %q in loadout %q revision %d", capability, e.loadout.id, e.loadout.revision),
			})
			return false, ""
		}
		e.addDiag(gen, Diagnostic{
			Service:    st.desc.Name,
			Code:       DiagOptionalMissing,
			Severity:   SeverityDegraded,
			Capability: capability,
			Message:    fmt.Sprintf("optional capability %q is not provided; %s runs without it", capability, st.desc.Name),
		})
		st.degraded = append(st.degraded, capability)
		return false, ""
	}

	lastState := StatePending
	for _, ref := range chain {
		if e.ensure(ctx, g, ref.Name, stack) {
			bindings[capability] = ref.Name
			return true, ""
		}
		providerState := e.services[ref.Name].state
		if providerState == StateFailed {
			lastState = StateFailed
			continue // try the next fallback
		}
		lastState = providerState
		if required {
			e.addDiag(gen, Diagnostic{
				Service:    st.desc.Name,
				Code:       DiagProviderPending,
				Severity:   SeverityError,
				Capability: capability,
				WaitingOn:  []string{ref.Name},
				Message:    fmt.Sprintf("waiting for capability %q: provider %q is %s", capability, ref.Name, providerState),
			})
			return false, ref.Name
		}
		break // optional dependency: degrade instead of waiting
	}

	if required {
		names := providerNames(chain)
		e.addDiag(gen, Diagnostic{
			Service:    st.desc.Name,
			Code:       DiagProviderFailed,
			Severity:   SeverityError,
			Capability: capability,
			WaitingOn:  names,
			Message:    fmt.Sprintf("capability %q is unavailable: every provider failed (%s)", capability, strings.Join(names, ", ")),
		})
		if len(names) > 0 {
			return false, names[0]
		}
		return false, ""
	}
	e.addDiag(gen, Diagnostic{
		Service:    st.desc.Name,
		Code:       DiagOptionalFailed,
		Severity:   SeverityDegraded,
		Capability: capability,
		WaitingOn:  providerNames(chain),
		Message:    fmt.Sprintf("optional capability %q is %s; %s runs without it", capability, lastState, st.desc.Name),
	})
	st.degraded = append(st.degraded, capability)
	return false, ""
}

// teardownOrderLocked computes the services that must leave the active set
// before the new graph is adopted, in reverse dependency order (dependents
// first).
func (e *Engine) teardownOrderLocked(next *Graph) []string {
	if e.graph == nil {
		return nil
	}
	dirty := map[string]bool{}
	for name, st := range e.services {
		nd, ok := next.loadout.Service(name)
		if !ok {
			dirty[name] = true // removed
			continue
		}
		if !identityEqual(st.desc, nd) {
			dirty[name] = true // implementation or declaration changed
			continue
		}
		if !sameCapabilityResolution(e.graph, next, st.desc) {
			dirty[name] = true // an effective provider changed
		}
	}
	// Propagate through live bindings: a dependent bound to a torn-down
	// provider (e.g. a promoted fallback) must leave the active set too.
	for {
		grew := false
		for name, st := range e.services {
			if dirty[name] {
				continue
			}
			for _, target := range st.bindings {
				if dirty[target] {
					dirty[name] = true
					grew = true
					break
				}
			}
		}
		if !grew {
			break
		}
	}
	var out []string
	for _, name := range e.graph.order {
		if dirty[name] {
			out = append(out, name)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// sameCapabilityResolution reports whether the effective provider of every
// capability a service consumes is the same in both graphs.
func sameCapabilityResolution(oldGraph, newGraph *Graph, d ServiceDescriptor) bool {
	caps := append(append([]string(nil), d.Requires...), d.Optional...)
	for _, capability := range caps {
		if oldGraph.headName(capability) != newGraph.headName(capability) {
			return false
		}
	}
	return true
}

// retireLocked moves a pinned instance out of the active set into its
// generation's retiring entry. It is not unloaded until every reference
// drains (or the drain deadline passes).
func (e *Engine) retireLocked(gen int64, name string, st *serviceState) {
	entry := e.retiring[st.loadedGeneration]
	if entry == nil {
		entry = &retiringEntry{generation: st.loadedGeneration, states: map[string]*serviceState{}}
		if e.retireTimeout > 0 {
			entry.deadline = e.now().Add(e.retireTimeout)
		}
		e.retiring[st.loadedGeneration] = entry
		e.retiringOrder = append(e.retiringOrder, st.loadedGeneration)
	}
	entry.states[name] = st
	entry.order = append(entry.order, name)
	st.state = StateRetiring
	st.lastAttempt = 0
	delete(e.services, name)
	e.addDiag(gen, Diagnostic{
		Service:  name,
		Code:     DiagRetiring,
		Severity: SeverityInfo,
		Message: fmt.Sprintf("generation %d instance kept in RETIRING: it no longer accepts new runs but %d in-flight reference(s) still use it; new runs use generation %d",
			st.loadedGeneration, st.pins, gen),
	})
}

// drainLocked unloads retiring entries whose references have all drained, in
// the recorded reverse dependency order.
func (e *Engine) drainLocked(ctx context.Context) []error {
	var errs []error
	kept := e.retiringOrder[:0]
	for _, gen := range e.retiringOrder {
		entry := e.retiring[gen]
		if entry == nil {
			continue
		}
		drained := true
		for _, st := range entry.states {
			if st.pins > 0 {
				drained = false
				break
			}
		}
		if !drained {
			kept = append(kept, gen)
			continue
		}
		for _, name := range entry.order {
			st := entry.states[name]
			if st == nil {
				continue
			}
			errs = append(errs, e.unloadStateLocked(ctx, name, st)...)
			e.addDiag(e.generation, Diagnostic{
				Service:  name,
				Code:     DiagDrained,
				Severity: SeverityInfo,
				Message:  fmt.Sprintf("generation %d reference(s) drained; %s unloaded in reverse order", entry.generation, name),
			})
		}
		delete(e.retiring, gen)
	}
	e.retiringOrder = kept
	return errs
}

// collectExpiredLocked force-unloads retiring entries past their drain
// deadline and invalidates the leases still holding them.
func (e *Engine) collectExpiredLocked(ctx context.Context) (int, error) {
	if e.retireTimeout <= 0 || len(e.retiringOrder) == 0 {
		return 0, nil
	}
	now := e.now()
	collected := 0
	var errs []error
	kept := e.retiringOrder[:0]
	for _, gen := range e.retiringOrder {
		entry := e.retiring[gen]
		if entry == nil {
			continue
		}
		if entry.deadline.IsZero() || now.Before(entry.deadline) {
			kept = append(kept, gen)
			continue
		}
		outstanding := 0
		for _, st := range entry.states {
			outstanding += st.pins
		}
		for lease := range e.leases {
			if lease.broken {
				continue
			}
			for name, st := range entry.states {
				if lease.states[name] == st {
					lease.broken = true
					break
				}
			}
		}
		for _, name := range entry.order {
			st := entry.states[name]
			if st == nil {
				continue
			}
			errs = append(errs, e.unloadStateLocked(ctx, name, st)...)
			collected++
			e.addDiag(e.generation, Diagnostic{
				Service:  name,
				Code:     DiagDrainExpired,
				Severity: SeverityDegraded,
				Message: fmt.Sprintf("drain deadline exceeded for generation %d with %d outstanding reference(s); %s force-unloaded — the in-flight runs holding it must fail",
					entry.generation, outstanding, name),
			})
		}
		delete(e.retiring, gen)
	}
	e.retiringOrder = kept
	return collected, errors.Join(errs...)
}

// unloadLocked unloads one active service (a no-op when it never loaded).
func (e *Engine) unloadLocked(ctx context.Context, name string) []error {
	st := e.services[name]
	if st == nil {
		return nil
	}
	st.lastAttempt = 0
	if st.instance == nil {
		st.state = StatePending
		st.failureCode, st.failure = "", ""
		st.waiting, st.degraded = nil, nil
		return nil
	}
	return e.unloadStateLocked(ctx, name, st)
}

// unloadStateLocked releases one instance: effects in reverse registration
// order, then the instance itself. It never fails the caller; problems become
// diagnostics.
func (e *Engine) unloadStateLocked(ctx context.Context, name string, st *serviceState) []error {
	st.state = StateUnloading
	var errs []error
	if st.scope != nil {
		for _, err := range st.scope.closeAndRelease() {
			errs = append(errs, err)
			e.addDiag(e.generation, Diagnostic{
				Service:  name,
				Code:     DiagEffectRelease,
				Severity: SeverityDegraded,
				Err:      err.Error(),
				Message:  fmt.Sprintf("releasing effects of %q: %v", name, err),
			})
		}
	}
	if st.instance != nil {
		if err := st.instance.Stop(ctx); err != nil {
			errs = append(errs, err)
			e.addDiag(e.generation, Diagnostic{
				Service:  name,
				Code:     DiagStopFailed,
				Severity: SeverityDegraded,
				Err:      err.Error(),
				Message:  fmt.Sprintf("stopping %q reported an error: %v", name, err),
			})
		}
	}
	st.instance, st.scope, st.bindings = nil, nil, nil
	st.loadedGeneration = 0
	st.state = StatePending
	st.failureCode, st.failure = "", ""
	st.waiting, st.degraded = nil, nil
	return errs
}

func (e *Engine) addDiag(gen int64, d Diagnostic) {
	if d.Generation == 0 {
		d.Generation = gen
	}
	if d.Severity == "" {
		d.Severity = SeverityError
	}
	e.diags = append(e.diags, d)
}

func (e *Engine) snapshotLocked() *Snapshot {
	snap := &Snapshot{Generation: e.generation, Diagnostics: append([]Diagnostic(nil), e.diags...)}
	if e.loadout != nil {
		snap.LoadoutID = e.loadout.id
		snap.Revision = e.loadout.revision
		snap.LoadoutVersion = e.loadout.version
		snap.Summary = e.loadout.summary
	}
	if e.graph == nil {
		return snap
	}
	for name, st := range e.services {
		status := ServiceStatus{
			Name:       name,
			Version:    st.desc.Version,
			State:      st.state,
			Role:       st.desc.Role,
			Provides:   append([]string(nil), st.desc.Provides...),
			Requires:   append([]string(nil), st.desc.Requires...),
			Optional:   append([]string(nil), st.desc.Optional...),
			WaitingOn:  append([]string(nil), st.waiting...),
			Degraded:   append([]string(nil), st.degraded...),
			Failure:    st.failure,
			Generation: st.loadedGeneration,
			Pins:       st.pins,
		}
		if st.bindings != nil {
			status.Bindings = make(map[string]string, len(st.bindings))
			for capability, provider := range st.bindings {
				status.Bindings[capability] = provider
			}
		}
		if st.scope != nil {
			status.Effects = st.scope.Effects()
		}
		snap.Services = append(snap.Services, status)
	}
	sort.Slice(snap.Services, func(i, j int) bool { return snap.Services[i].Name < snap.Services[j].Name })
	for _, gen := range e.retiringOrder {
		entry := e.retiring[gen]
		if entry == nil {
			continue
		}
		for _, name := range entry.order {
			st := entry.states[name]
			if st == nil {
				continue
			}
			row := RetiringStatus{
				Generation: entry.generation,
				Service:    name,
				Pins:       st.pins,
				Deadline:   entry.deadline,
			}
			if st.scope != nil {
				row.Effects = st.scope.Effects()
			}
			snap.Retiring = append(snap.Retiring, row)
		}
	}
	sort.Slice(snap.Retiring, func(i, j int) bool {
		if snap.Retiring[i].Generation != snap.Retiring[j].Generation {
			return snap.Retiring[i].Generation < snap.Retiring[j].Generation
		}
		return snap.Retiring[i].Service < snap.Retiring[j].Service
	})
	return snap
}

func (e *Engine) countActiveLocked() int {
	n := 0
	for _, st := range e.services {
		if st.state == StateActive {
			n++
		}
	}
	return n
}

func providerNames(chain []ProviderRef) []string {
	out := make([]string, 0, len(chain))
	for _, ref := range chain {
		out = append(out, ref.Name)
	}
	return out
}

// primaryOf returns the effective provider (chain head) of the first
// capability a service provides; used for standby diagnostics.
func (g *Graph) primaryOf(service string) string {
	for _, capability := range g.byName[service].Provides {
		if head := g.headName(capability); head != "" {
			return head
		}
	}
	return ""
}
