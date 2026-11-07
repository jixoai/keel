package servicegraph

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func asResolveError(err error, out **ResolveError) bool { return errors.As(err, out) }

// recorder records lifecycle events in order; safe for concurrent use.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fmt.Sprintf(format, args...))
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *recorder) contains(sub string) bool { return r.index(sub) >= 0 }

func (r *recorder) count(sub string) int {
	n := 0
	for _, e := range r.list() {
		if strings.Contains(e, sub) {
			n++
		}
	}
	return n
}

func (r *recorder) index(sub string) int {
	for i, e := range r.list() {
		if strings.Contains(e, sub) {
			return i
		}
	}
	return -1
}

func (r *recorder) lastIndex(sub string) int {
	events := r.list()
	for i := len(events) - 1; i >= 0; i-- {
		if strings.Contains(events[i], sub) {
			return i
		}
	}
	return -1
}

// mustBeBefore fails when sub[0] is not recorded before sub[1]. It compares
// the LAST occurrence of each pattern so reload-order assertions observe the
// newest events (events repeat across generations).
func (r *recorder) mustBeBefore(t *testing.T, first, second string) {
	t.Helper()
	i, j := r.lastIndex(first), r.lastIndex(second)
	if i < 0 || j < 0 {
		t.Fatalf("event missing: %q=%d %q=%d in %v", first, i, second, j, r.list())
	}
	if i >= j {
		t.Fatalf("expected %q before %q in %v", first, second, r.list())
	}
}

type fakeEffect struct {
	kind string
	err  error
}

type fakeInstance struct {
	rec      *recorder
	name     string
	startErr error
	stopErr  error
	effects  []fakeEffect
	scope    *EffectScope
}

func (f *fakeInstance) Start(_ context.Context, effects *EffectScope) error {
	f.scope = effects
	f.rec.add("start:%s", f.name)
	for _, e := range f.effects {
		eff := e
		if err := effects.Register(eff.kind, func() error {
			f.rec.add("release:%s/%s", f.name, eff.kind)
			return eff.err
		}); err != nil {
			f.rec.add("register-refused:%s/%s", f.name, eff.kind)
		}
	}
	return f.startErr
}

func (f *fakeInstance) Stop(context.Context) error {
	f.rec.add("stop:%s", f.name)
	return f.stopErr
}

// instanceFactory builds a fresh fakeInstance per call.
func instanceFactory(rec *recorder, name string, effects ...string) Factory {
	return func(context.Context, Resolved) (Instance, error) {
		inst := &fakeInstance{rec: rec, name: name}
		for _, kind := range effects {
			inst.effects = append(inst.effects, fakeEffect{kind: kind})
		}
		return inst, nil
	}
}

// failingFactory builds a fakeInstance whose Start fails after registering its
// effects.
func failingFactory(rec *recorder, name string, startErr error, effects ...string) Factory {
	return func(context.Context, Resolved) (Instance, error) {
		inst := &fakeInstance{rec: rec, name: name, startErr: startErr}
		for _, kind := range effects {
			inst.effects = append(inst.effects, fakeEffect{kind: kind})
		}
		return inst, nil
	}
}

// factoryError fails at construction time, not at Start.
func factoryError(name string, err error) Factory {
	return func(context.Context, Resolved) (Instance, error) { return nil, err }
}

// fixedFactory returns the same instance every time the factory is called.
func fixedFactory(inst Instance) Factory {
	return func(context.Context, Resolved) (Instance, error) { return inst, nil }
}

// binderFactory records which capabilities were bound and whether they carried
// a live instance.
func binderFactory(rec *recorder, name string, expect []string) Factory {
	return func(_ context.Context, r Resolved) (Instance, error) {
		for _, capability := range expect {
			v, ok := r.Bind(capability)
			rec.add("bind:%s:%s=%t", name, capability, ok && v != nil)
		}
		return &fakeInstance{rec: rec, name: name}, nil
	}
}

func desc(name string, factory Factory, provides, requires, optional []string) ServiceDescriptor {
	return ServiceDescriptor{
		Name:     name,
		Version:  "v1",
		Provides: provides,
		Requires: requires,
		Optional: optional,
		Factory:  factory,
	}
}

func mustLoadout(t *testing.T, id string, revision int64, descs ...ServiceDescriptor) *Loadout {
	t.Helper()
	lo, err := NewLoadout(LoadoutConfig{ID: id, Revision: revision, Version: "test@v1", Services: descs})
	if err != nil {
		t.Fatalf("NewLoadout: %v", err)
	}
	return lo
}

func mustResolve(t *testing.T, lo *Loadout) *Graph {
	t.Helper()
	g, err := Resolve(lo)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return g
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestEngine() *Engine {
	return NewEngine(WithLogger(discardLogger()))
}

func mustApply(t *testing.T, e *Engine, lo *Loadout) *Snapshot {
	t.Helper()
	snap, err := e.Apply(context.Background(), lo)
	if err != nil {
		t.Fatalf("Apply: %v\nsnapshot:\n%s", err, snap)
	}
	if snap == nil {
		t.Fatal("Apply returned a nil snapshot")
	}
	return snap
}

func hasIssue(err error, code string) bool {
	re, ok := err.(*ResolveError)
	if !ok {
		return false
	}
	for _, issue := range re.Issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}
