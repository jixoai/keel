package servicegraph

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// State is the lifecycle state of one service instance.
//
// PENDING → LOADING → ACTIVE → RETIRING → UNLOADING → PENDING; a failed
// activation lands in FAILED and can be retried by a later Apply.
//
// RETIRING holds an old-generation instance that in-flight runs still
// reference (see Engine.Acquire): it accepts no new runs and is unloaded in
// reverse dependency order once its references drain (or the drain deadline
// passes).
type State string

const (
	StatePending   State = "PENDING"
	StateLoading   State = "LOADING"
	StateActive    State = "ACTIVE"
	StateRetiring  State = "RETIRING"
	StateUnloading State = "UNLOADING"
	StateFailed    State = "FAILED"
)

// Instance is a live service produced by a Factory.
//
// Start acquires the service's runtime effects (listeners, timers, tool
// registrations, goroutines) and MUST register every externally visible one
// through the provided EffectScope: those registrations are the rollback
// contract. If Start returns an error, the engine releases the registered
// effects in reverse order and never calls Stop, so Start must leave nothing
// behind outside the scope.
//
// Stop releases the instance itself after its effects have been released.
type Instance interface {
	Start(ctx context.Context, effects *EffectScope) error
	Stop(ctx context.Context) error
}

// Factory builds a service instance from its resolved dependencies. It must be
// pure construction: everything acquired here must be released by Stop, and it
// must not register effects (register them in Start instead) or reach the
// contract kernel's raw write paths.
type Factory func(ctx context.Context, resolved Resolved) (Instance, error)

// Resolved carries the live dependencies handed to a Factory. Bindings maps a
// declared capability to the live instance of the provider that serves it.
type Resolved struct {
	// Service is the name of the service being constructed.
	Service string
	// Bindings maps capability name → provider instance.
	Bindings map[string]any
}

// Bind returns the provider instance bound to capability.
func (r Resolved) Bind(capability string) (any, bool) {
	v, ok := r.Bindings[capability]
	return v, ok
}

// BindAs returns the provider instance bound to capability as type T. The
// dynamic value is the provider service's live instance; dependents assert it
// to a locally declared narrow interface.
func BindAs[T any](r Resolved, capability string) (T, error) {
	var zero T
	v, ok := r.Bindings[capability]
	if !ok {
		return zero, fmt.Errorf("servicegraph: capability %q is not bound for service %q", capability, r.Service)
	}
	typed, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("servicegraph: capability %q is bound to %T, want %T", capability, v, zero)
	}
	return typed, nil
}

var (
	// ErrScopeClosed is returned when a service registers an effect after its
	// instance was unloaded; the effect must be released immediately.
	ErrScopeClosed = errors.New("servicegraph: effect scope closed")
	// ErrNilRelease is returned when an effect is registered without a
	// release function.
	ErrNilRelease = errors.New("servicegraph: effect release must not be nil")
)

// Effect describes one registered effect in diagnostics.
type Effect struct {
	Kind       string
	Generation int64
}

// EffectScope collects the effects of one service instance in one generation.
// Release runs in reverse registration order; a failing release does not stop
// the remaining ones.
type EffectScope struct {
	mu         sync.Mutex
	service    string
	generation int64
	closed     bool
	effects    []scopedEffect
}

type scopedEffect struct {
	kind    string
	release func() error
}

func newEffectScope(service string, generation int64) *EffectScope {
	return &EffectScope{service: service, generation: generation}
}

// Register adds one cleanup effect. It returns ErrScopeClosed when the scope
// is already closed; the caller must release the effect itself and abort.
func (s *EffectScope) Register(kind string, release func() error) error {
	if release == nil {
		return ErrNilRelease
	}
	if kind == "" {
		kind = "effect"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrScopeClosed
	}
	s.effects = append(s.effects, scopedEffect{kind: kind, release: release})
	return nil
}

// Effects returns a copy of the registered effects for diagnostics.
func (s *EffectScope) Effects() []Effect {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Effect, 0, len(s.effects))
	for _, e := range s.effects {
		out = append(out, Effect{Kind: e.kind, Generation: s.generation})
	}
	return out
}

// closeAndRelease closes the scope and releases every effect in reverse
// registration order, collecting all release errors.
func (s *EffectScope) closeAndRelease() []error {
	s.mu.Lock()
	s.closed = true
	entries := s.effects
	s.effects = nil
	s.mu.Unlock()
	var errs []error
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].release == nil {
			continue
		}
		if err := entries[i].release(); err != nil {
			errs = append(errs, fmt.Errorf("service %s: effect %q: %w", s.service, entries[i].kind, err))
		}
	}
	return errs
}
