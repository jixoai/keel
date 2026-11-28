// Package pluginruntime is the service-plugin kernel used by the board and
// superagent split (change keel-plugin-split).
//
// It is deliberately small and separate from the model-facing plugin runtime
// (harnessext): board and superagent are host services, not model extensions.
// What they share with the agent plugins is the contract this package enforces:
//
//   - a manifest-like Spec with `requires` and `provides`,
//   - topological activation (a dependency starts and publishes before its
//     dependent),
//   - PENDING instead of half-loading when a requirement is absent or failed,
//   - reverse-order unload, and a refusal to stop a plugin while an active
//     dependent still needs it,
//   - tenant-scoped persistence of the computed dependency status through
//     pluginstore.Store.
package pluginruntime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/pluginstore"
)

// State is the observable lifecycle of one service plugin.
type State string

const (
	StateActive  State = "active"
	StatePending State = "pending"
	StateFailed  State = "failed"
	StateStopped State = "stopped"
)

// Spec is the plugin's declared identity and dependency edges.
type Spec struct {
	ID       string
	Version  string
	Kind     string
	Requires []string
	Provides []string
}

// Plugin is one service plugin. Start publishes its services through Env; a
// Start error is FAILED and every dependent stays PENDING. Stop must release
// what Start acquired. Disabled skips Start and publishes nothing, so a
// dependent sees the same PENDING answer as an absent plugin.
type Plugin struct {
	Spec     Spec
	Disabled bool
	Start    func(ctx context.Context, env *Env) error
	Stop     func(ctx context.Context) error
}

// Services is the assembly-local service registry. It is only reachable from a
// plugin's Start after its requirements were satisfied, which is what makes a
// missing board a PENDING superagent rather than a nil dereference.
type Services struct {
	mu     sync.RWMutex
	values map[string]any
	owners map[string]string
}

// NewServices builds an empty registry.
func NewServices() *Services {
	return &Services{values: make(map[string]any), owners: make(map[string]string)}
}

// Provide publishes a service under a capability name. A duplicate name from a
// different plugin is refused: two owners would make the dependency graph lie.
func (s *Services) Provide(owner, name string, value any) error {
	if name == "" || value == nil {
		return errors.New("pluginruntime: a provided service needs a name and a value")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.owners[name]; ok && existing != owner {
		return fmt.Errorf("pluginruntime: service %q is already provided by %q", name, existing)
	}
	s.values[name] = value
	s.owners[name] = owner
	return nil
}

// Get reads one service.
func (s *Services) Get(name string) (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.values[name]
	return value, ok
}

// Remove withdraws a service on unload. Only the owner may remove it, so a
// plugin cannot unpublish another plugin's capability.
func (s *Services) Remove(owner, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.owners[name]; ok && existing == owner {
		delete(s.values, name)
		delete(s.owners, name)
	}
}

// GetAs is the typed form Start uses. A missing or mistyped service is a
// fail-closed error, never a zero value.
func GetAs[T any](services *Services, name string) (T, error) {
	var zero T
	if services == nil {
		return zero, fmt.Errorf("pluginruntime: no service registry for %q", name)
	}
	value, ok := services.Get(name)
	if !ok {
		return zero, fmt.Errorf("pluginruntime: required service %q is not provided", name)
	}
	typed, ok := value.(T)
	if !ok {
		return zero, fmt.Errorf("pluginruntime: service %q has type %T", name, value)
	}
	return typed, nil
}

// Env is what Start receives.
type Env struct {
	Services *Services
	Logger   *slog.Logger
	// Kernel persists the tenant-scoped dependency status. Nil keeps statuses
	// in memory (tests, a host without plugin state persistence).
	Kernel pluginstore.Store
	// TenantID is the deployment tenant the status rows belong to.
	TenantID uuid.UUID
}

// Status is the observable assembly answer.
type Status struct {
	ID       string   `json:"id"`
	Version  string   `json:"version,omitempty"`
	State    State    `json:"state"`
	Requires []string `json:"requires,omitempty"`
	Provides []string `json:"provides,omitempty"`
	Reason   string   `json:"reason,omitempty"`
}

// Options wires the runtime.
type Options struct {
	Plugins  []*Plugin
	Services *Services
	Kernel   pluginstore.Store
	TenantID uuid.UUID
	Logger   *slog.Logger
}

// Runtime owns one assembly generation.
type Runtime struct {
	mu       sync.Mutex
	opts     Options
	plugins  map[string]*Plugin
	order    []string
	statuses map[string]Status
	active   []string
	loaded   bool
}

// New builds a runtime. Duplicate ids and cycles are construction errors: the
// graph must be a DAG before any Start runs.
func New(opts Options) (*Runtime, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Services == nil {
		opts.Services = NewServices()
	}
	r := &Runtime{
		opts:     opts,
		plugins:  make(map[string]*Plugin, len(opts.Plugins)),
		statuses: make(map[string]Status, len(opts.Plugins)),
	}
	for _, plugin := range opts.Plugins {
		if plugin == nil || strings.TrimSpace(plugin.Spec.ID) == "" {
			return nil, errors.New("pluginruntime: plugin id is required")
		}
		if _, exists := r.plugins[plugin.Spec.ID]; exists {
			return nil, fmt.Errorf("pluginruntime: duplicate plugin id %q", plugin.Spec.ID)
		}
		r.plugins[plugin.Spec.ID] = plugin
		r.statuses[plugin.Spec.ID] = Status{
			ID: plugin.Spec.ID, Version: plugin.Spec.Version,
			State: StatePending, Requires: append([]string(nil), plugin.Spec.Requires...),
			Provides: append([]string(nil), plugin.Spec.Provides...),
		}
	}
	order, err := topologicalOrder(opts.Plugins)
	if err != nil {
		return nil, err
	}
	r.order = order
	return r, nil
}

// Load activates the graph in dependency order. It is idempotent for an
// already-loaded runtime.
func (r *Runtime) Load(ctx context.Context) error {
	r.mu.Lock()
	if r.loaded {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	provided := make(map[string]bool)
	activated := make([]string, 0, len(r.order))
	for _, id := range r.order {
		plugin := r.plugins[id]
		if plugin.Disabled {
			r.setStatus(id, StateStopped, "disabled")
			continue
		}
		missing := make([]string, 0)
		for _, requirement := range plugin.Spec.Requires {
			if !provided[requirement] {
				missing = append(missing, requirement)
			}
		}
		if len(missing) > 0 {
			r.setStatus(id, StatePending, "waiting for: "+strings.Join(missing, ", "))
			continue
		}
		env := &Env{Services: r.opts.Services, Logger: r.opts.Logger, Kernel: r.opts.Kernel, TenantID: r.opts.TenantID}
		if plugin.Start != nil {
			if err := plugin.Start(ctx, env); err != nil {
				r.setStatus(id, StateFailed, err.Error())
				r.opts.Logger.Warn("service plugin failed to start", "plugin", id, "err", err)
				continue
			}
		}
		for _, capability := range plugin.Spec.Provides {
			provided[capability] = true
		}
		activated = append(activated, id)
		r.setStatus(id, StateActive, "")
	}

	r.mu.Lock()
	r.active = activated
	r.loaded = true
	r.mu.Unlock()
	return nil
}

// Unload stops every active plugin in reverse activation order. A Stop error is
// recorded on the plugin but does not prevent the remaining plugins from
// stopping: leaving services alive because one failed is worse.
func (r *Runtime) Unload(ctx context.Context) error {
	r.mu.Lock()
	active := append([]string(nil), r.active...)
	r.active = nil
	r.loaded = false
	r.mu.Unlock()

	var failures []string
	for i := len(active) - 1; i >= 0; i-- {
		id := active[i]
		plugin := r.plugins[id]
		if plugin.Stop != nil {
			if err := plugin.Stop(ctx); err != nil {
				failures = append(failures, id+": "+err.Error())
				r.setStatus(id, StateFailed, "stop: "+err.Error())
				continue
			}
		}
		r.setStatus(id, StateStopped, "")
	}
	if len(failures) > 0 {
		return errors.New("pluginruntime: unload: " + strings.Join(failures, "; "))
	}
	return nil
}

// StopPlugin unloads one plugin. It refuses while an active plugin still
// requires it; board therefore cannot stop underneath an active superagent.
func (r *Runtime) StopPlugin(ctx context.Context, id string) error {
	r.mu.Lock()
	active := append([]string(nil), r.active...)
	r.mu.Unlock()
	if !contains(active, id) {
		return fmt.Errorf("pluginruntime: plugin %q is not active", id)
	}
	for _, other := range active {
		if other == id {
			continue
		}
		provided := make(map[string]bool, len(r.plugins[id].Spec.Provides))
		for _, capability := range r.plugins[id].Spec.Provides {
			provided[capability] = true
		}
		for _, requirement := range r.plugins[other].Spec.Requires {
			if requirement == id || provided[requirement] {
				return fmt.Errorf("pluginruntime: %q is required by active plugin %q", id, other)
			}
		}
	}
	plugin := r.plugins[id]
	if plugin.Stop != nil {
		if err := plugin.Stop(ctx); err != nil {
			r.setStatus(id, StateFailed, "stop: "+err.Error())
			return err
		}
	}
	r.mu.Lock()
	r.active = remove(r.active, id)
	r.mu.Unlock()
	r.setStatus(id, StateStopped, "")
	return nil
}

// Snapshot returns every status in declaration order.
func (r *Runtime) Snapshot() []Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Status, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.statuses[id])
	}
	return out
}

// Status reads one plugin's status.
func (r *Runtime) Status(id string) (Status, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	status, ok := r.statuses[id]
	return status, ok
}

// Active reports whether the plugin finished Start successfully.
func (r *Runtime) Active(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return contains(r.active, id)
}

func (r *Runtime) setStatus(id string, state State, reason string) {
	r.mu.Lock()
	status := r.statuses[id]
	status.State = state
	status.Reason = reason
	r.statuses[id] = status
	r.mu.Unlock()
	r.persist(id, status)
}

func (r *Runtime) persist(id string, status Status) {
	if r.opts.Kernel == nil || r.opts.TenantID == uuid.Nil {
		return
	}
	dependency := pluginstore.Dependency{
		PluginID: id, Requires: append([]string(nil), status.Requires...),
		State: dependencyState(status.State), Reason: status.Reason,
	}
	if _, err := r.opts.Kernel.PutDependency(context.Background(), r.opts.TenantID, dependency); err != nil {
		r.opts.Logger.Warn("service plugin status was not persisted", "plugin", id, "err", err)
	}
}

func dependencyState(state State) string {
	switch state {
	case StateActive:
		return pluginstore.DependencyReady
	case StatePending:
		return pluginstore.DependencyPending
	case StateFailed:
		return pluginstore.DependencyFailed
	default:
		return pluginstore.DependencyStopped
	}
}

// topologicalOrder is Kahn's algorithm with a declaration-order tie-break, so
// two runs over the same input activate in the same order. A requirement is
// resolved against a plugin's `provides` capability first, then against a
// plugin id, so `requires: [board]` orders the board provider first even when
// its manifest id is `keel.board`. An unknown requirement is treated as an
// external host service: it has no edge and the plugin starts PENDING unless
// the host already published that capability.
func topologicalOrder(plugins []*Plugin) ([]string, error) {
	byID := make(map[string]*Plugin, len(plugins))
	provider := make(map[string]string, len(plugins))
	index := make(map[string]int, len(plugins))
	for i, plugin := range plugins {
		byID[plugin.Spec.ID] = plugin
		index[plugin.Spec.ID] = i
		for _, capability := range plugin.Spec.Provides {
			if existing, ok := provider[capability]; ok && existing != plugin.Spec.ID {
				return nil, fmt.Errorf("pluginruntime: capability %q is provided by both %q and %q", capability, existing, plugin.Spec.ID)
			}
			provider[capability] = plugin.Spec.ID
		}
	}
	resolve := func(requirement string) (string, bool) {
		if id, ok := provider[requirement]; ok {
			return id, true
		}
		if _, ok := byID[requirement]; ok {
			return requirement, true
		}
		return "", false
	}
	indegree := make(map[string]int, len(plugins))
	dependents := make(map[string][]string, len(plugins))
	for _, plugin := range plugins {
		indegree[plugin.Spec.ID] = 0
	}
	for _, plugin := range plugins {
		for _, requirement := range plugin.Spec.Requires {
			id, ok := resolve(requirement)
			if !ok || id == plugin.Spec.ID {
				continue
			}
			indegree[plugin.Spec.ID]++
			dependents[id] = append(dependents[id], plugin.Spec.ID)
		}
	}
	ready := make([]string, 0)
	for id, degree := range indegree {
		if degree == 0 {
			ready = append(ready, id)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return index[ready[i]] < index[ready[j]] })
	order := make([]string, 0, len(plugins))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		for _, dependent := range dependents[id] {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = append(ready, dependent)
				sort.Slice(ready, func(i, j int) bool { return index[ready[i]] < index[ready[j]] })
			}
		}
	}
	if len(order) != len(plugins) {
		cycle := make([]string, 0)
		for id, degree := range indegree {
			if degree > 0 {
				cycle = append(cycle, id)
			}
		}
		sort.Strings(cycle)
		return nil, fmt.Errorf("pluginruntime: dependency cycle: %s", strings.Join(cycle, ", "))
	}
	return order, nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func remove(values []string, want string) []string {
	out := values[:0]
	for _, value := range values {
		if value != want {
			out = append(out, value)
		}
	}
	return out
}
