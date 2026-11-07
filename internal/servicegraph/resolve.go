package servicegraph

import (
	"fmt"
	"sort"
	"strings"
)

// ProviderRef is one entry of a capability's provider chain.
type ProviderRef struct {
	Name string
	Role Role
}

// Graph is the static resolution of a loadout: provider chains per capability,
// dependency edges, standby providers, and a deterministic activation order.
// It answers static questions ("what does this service depend on", "why is it
// standby") and drives the Engine; it holds no live instances.
type Graph struct {
	loadout   *Loadout
	chains    map[string][]ProviderRef // capability → ordered provider chain
	deps      map[string][]string      // service → provider names it depends on (topological edges)
	missing   map[string][]string      // service → required capabilities with no provider
	optional  map[string][]string      // service → optional capabilities with no provider
	order     []string                 // deterministic activation order (providers before dependents)
	standby   map[string]bool          // service → is not the effective provider of any capability it provides
	byName    map[string]ServiceDescriptor
	declIndex map[string]int
}

// Resolve validates a loadout's provider graph and produces the static Graph.
//
// Rejections (all before any activation, returned as *ResolveError):
//   - a capability provided by several services without exactly one primary
//     (the rest must be declared fallback);
//   - a dependency cycle, reported with a readable path.
func Resolve(lo *Loadout) (*Graph, error) {
	if lo == nil {
		return nil, &ResolveError{Issues: []ResolveIssue{{Code: IssueDuplicateService, Message: "nil loadout"}}}
	}
	g := &Graph{
		loadout:   lo,
		chains:    map[string][]ProviderRef{},
		deps:      map[string][]string{},
		missing:   map[string][]string{},
		optional:  map[string][]string{},
		standby:   map[string]bool{},
		byName:    map[string]ServiceDescriptor{},
		declIndex: map[string]int{},
	}
	for i, d := range lo.services {
		g.byName[d.Name] = d
		g.declIndex[d.Name] = i
	}

	var issues []ResolveIssue

	// Provider chains, in capability order for determinism.
	providers := map[string][]ServiceDescriptor{}
	capabilities := make([]string, 0)
	for _, d := range lo.services {
		for _, c := range d.Provides {
			if _, ok := providers[c]; !ok {
				capabilities = append(capabilities, c)
			}
			providers[c] = append(providers[c], d)
		}
	}
	sort.Strings(capabilities)
	for _, capability := range capabilities {
		ds := providers[capability]
		if len(ds) == 1 {
			// A sole provider always serves, whatever role it declares.
			g.chains[capability] = []ProviderRef{{Name: ds[0].Name, Role: ds[0].Role}}
			continue
		}
		var primaries, fallbacks []ServiceDescriptor
		for _, d := range ds {
			if d.Role == RoleFallback {
				fallbacks = append(fallbacks, d)
			} else {
				primaries = append(primaries, d)
			}
		}
		if len(primaries) == 0 {
			issues = append(issues, ResolveIssue{
				Code:       IssueAmbiguousProvider,
				Capability: capability,
				Providers:  descriptorNames(ds),
				Message: fmt.Sprintf("capability %q has %d providers (%s) and none is primary; declare exactly one primary and mark the rest fallback",
					capability, len(ds), strings.Join(descriptorNames(ds), ", ")),
			})
			for _, d := range fallbacks {
				g.chains[capability] = append(g.chains[capability], ProviderRef{Name: d.Name, Role: RoleFallback})
			}
			continue
		}
		if len(primaries) > 1 {
			issues = append(issues, ResolveIssue{
				Code:       IssueDuplicateProvider,
				Capability: capability,
				Providers:  descriptorNames(primaries),
				Message: fmt.Sprintf("capability %q has %d primary providers (%s); only one primary is allowed, declare the rest fallback",
					capability, len(primaries), strings.Join(descriptorNames(primaries), ", ")),
			})
		}
		chain := []ProviderRef{{Name: primaries[0].Name, Role: RolePrimary}}
		for _, d := range fallbacks {
			chain = append(chain, ProviderRef{Name: d.Name, Role: RoleFallback})
		}
		g.chains[capability] = chain
	}

	// Dependency edges: a service depends on every provider in the chain of
	// each capability it requires or optionally consumes. The edge set is the
	// ordering contract for both activation and teardown.
	for _, d := range lo.services {
		seen := map[string]bool{}
		add := func(ref string) {
			if !seen[ref] {
				seen[ref] = true
				g.deps[d.Name] = append(g.deps[d.Name], ref)
			}
		}
		for _, capability := range d.Requires {
			chain := g.chains[capability]
			if len(chain) == 0 {
				g.missing[d.Name] = append(g.missing[d.Name], capability)
				continue
			}
			for _, ref := range chain {
				add(ref.Name)
			}
		}
		for _, capability := range d.Optional {
			chain := g.chains[capability]
			if len(chain) == 0 {
				g.optional[d.Name] = append(g.optional[d.Name], capability)
				continue
			}
			for _, ref := range chain {
				add(ref.Name)
			}
		}
	}

	// Cycle detection over the dependency edges, with a readable path.
	if cycle := g.findCycle(); len(cycle) > 0 {
		issues = append(issues, ResolveIssue{
			Code:    IssueCycle,
			Service: cycle[0],
			Cycle:   cycle,
			Message: fmt.Sprintf("dependency cycle: %s", strings.Join(cycle, " -> ")),
		})
	}

	if len(issues) > 0 {
		return nil, &ResolveError{LoadoutID: lo.id, Revision: lo.revision, Issues: issues}
	}

	// Standby providers: a service that is not the effective provider (chain
	// head) of any capability it provides never activates on its own.
	for _, d := range lo.services {
		isStandby := true
		for _, capability := range d.Provides {
			if chain := g.chains[capability]; len(chain) > 0 && chain[0].Name == d.Name {
				isStandby = false
				break
			}
		}
		g.standby[d.Name] = isStandby
	}

	g.order = g.topologicalOrder()
	return g, nil
}

// Loadout returns the resolved loadout.
func (g *Graph) Loadout() *Loadout { return g.loadout }

// Order returns the activation order: providers before dependents,
// deterministic for a given loadout.
func (g *Graph) Order() []string { return append([]string(nil), g.order...) }

// Chain returns the provider chain of a capability (primary first), empty when
// nothing provides it.
func (g *Graph) Chain(capability string) []ProviderRef {
	return append([]ProviderRef(nil), g.chains[capability]...)
}

// EffectiveProvider returns the provider a capability resolves to statically
// (the chain head). At runtime a fallback may be serving instead; the live
// binding is visible in the snapshot's Bindings.
func (g *Graph) EffectiveProvider(capability string) (ProviderRef, bool) {
	chain := g.chains[capability]
	if len(chain) == 0 {
		return ProviderRef{}, false
	}
	return chain[0], true
}

// Dependencies returns the provider names a service depends on (required and
// optional), in declaration order.
func (g *Graph) Dependencies(service string) []string {
	return append([]string(nil), g.deps[service]...)
}

// MissingRequires returns required capabilities of a service that no service
// provides. Such a service stays PENDING.
func (g *Graph) MissingRequires(service string) []string {
	return append([]string(nil), g.missing[service]...)
}

// MissingOptionals returns optional capabilities of a service that no service
// provides. Such a service activates degraded.
func (g *Graph) MissingOptionals(service string) []string {
	return append([]string(nil), g.optional[service]...)
}

// IsStandby reports whether a service is only a fallback: it is not the
// effective provider of any capability it provides, so it only activates when
// promoted.
func (g *Graph) IsStandby(service string) bool { return g.standby[service] }

// headName returns the effective provider name of a capability ("" if none).
func (g *Graph) headName(capability string) string {
	chain := g.chains[capability]
	if len(chain) == 0 {
		return ""
	}
	return chain[0].Name
}

// findCycle returns a readable cycle path (e.g. [a b a]) or nil.
func (g *Graph) findCycle() []string {
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var path []string
	var found []string

	var visit func(name string) bool
	visit = func(name string) bool {
		color[name] = grey
		path = append(path, name)
		for _, dep := range g.deps[name] {
			switch color[dep] {
			case grey:
				start := 0
				for i, n := range path {
					if n == dep {
						start = i
						break
					}
				}
				found = append(append([]string(nil), path[start:]...), dep)
				return true
			case white:
				if visit(dep) {
					return true
				}
			}
		}
		path = path[:len(path)-1]
		color[name] = black
		return false
	}

	for _, d := range g.loadout.services {
		if color[d.Name] == white {
			if visit(d.Name) {
				return found
			}
		}
	}
	return nil
}

// topologicalOrder runs a Kahn ordering; ties are broken by declaration
// order. Resolve has already rejected cycles, so every node is emitted.
func (g *Graph) topologicalOrder() []string {
	indegree := map[string]int{}
	dependents := map[string][]string{}
	for _, d := range g.loadout.services {
		for _, dep := range g.deps[d.Name] {
			indegree[d.Name]++
			dependents[dep] = append(dependents[dep], d.Name)
		}
	}
	var ready []string
	for _, d := range g.loadout.services {
		if indegree[d.Name] == 0 {
			ready = append(ready, d.Name)
		}
	}
	order := make([]string, 0, len(g.loadout.services))
	for len(ready) > 0 {
		name := ready[0]
		ready = ready[1:]
		order = append(order, name)
		for _, next := range dependents[name] {
			indegree[next]--
			if indegree[next] == 0 {
				ready = insertByDeclaration(g.declIndex, ready, next)
			}
		}
	}
	return order
}

func insertByDeclaration(index map[string]int, ready []string, name string) []string {
	i := sort.Search(len(ready), func(i int) bool { return index[ready[i]] > index[name] })
	ready = append(ready, "")
	copy(ready[i+1:], ready[i:])
	ready[i] = name
	return ready
}

func descriptorNames(ds []ServiceDescriptor) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Name)
	}
	return out
}
