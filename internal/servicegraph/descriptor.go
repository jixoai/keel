package servicegraph

import (
	"fmt"
	"sort"
)

// Role marks a provider's place in a capability's provider chain. The zero
// value ("") is normalized to RolePrimary.
type Role string

const (
	// RolePrimary is the provider a capability resolves to when it is present.
	RolePrimary Role = "primary"
	// RoleFallback serves a capability only when the primary did not start.
	RoleFallback Role = "fallback"
)

// ServiceDescriptor describes one service that a loadout can assemble. The
// descriptor is statically enumerable: nothing here is guessed at runtime.
//
// Identity for reassembly is (Name, Version, Provides, Requires, Optional,
// Role). Version is the implementation identity — bump it whenever the
// implementation behind a factory changes, otherwise a reassembly cannot tell
// the old instance from the new one.
type ServiceDescriptor struct {
	// Name uniquely identifies the service within a loadout ("pi-loop").
	Name string
	// Version identifies the implementation ("v1").
	Version string
	// Provides lists the capabilities this service supplies ("ctx.llm").
	Provides []string
	// Requires lists capabilities that must be served before this service
	// loads; a missing required capability keeps it PENDING.
	Requires []string
	// Optional lists capabilities the service can run without; a missing one
	// activates the service degraded and is recorded in diagnostics.
	Optional []string
	// Role is this service's place in the chain of every capability it
	// provides (primary or fallback). Defaults to primary.
	Role Role
	// Effects names the cleanup scope the service registers while starting
	// (listeners, timers, registrations) — documentation for operators and
	// diagnostics, not a runtime contract.
	Effects []string
	// Factory builds the live instance. It is attached by the loadout builder
	// (see internal/loadout); a nil factory fails activation with a diagnostic.
	Factory Factory
}

// clone deep-copies the descriptor so a Loadout snapshot cannot be mutated
// through slices the caller still holds.
func (d ServiceDescriptor) clone() ServiceDescriptor {
	d.Provides = append([]string(nil), d.Provides...)
	d.Requires = append([]string(nil), d.Requires...)
	d.Optional = append([]string(nil), d.Optional...)
	d.Effects = append([]string(nil), d.Effects...)
	if d.Role == "" {
		d.Role = RolePrimary
	}
	return d
}

// identityEqual compares the reassembly identity of two descriptors. Factory
// functions cannot be compared in Go (closures share code pointers), so
// implementation changes must bump Version.
func identityEqual(a, b ServiceDescriptor) bool {
	return a.Name == b.Name &&
		a.Version == b.Version &&
		a.Role == b.Role &&
		sameStringSet(a.Provides, b.Provides) &&
		sameStringSet(a.Requires, b.Requires) &&
		sameStringSet(a.Optional, b.Optional)
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// LoadoutConfig is the constructor input for Loadout.
type LoadoutConfig struct {
	// ID names the loadout (for a tenant: "default"); recorded with every run.
	ID string
	// Revision is the configuration version. It only changes when an
	// administrator changes the configuration — not on every process start.
	Revision int64
	// Version names the assembly ("keel-default@v1"), for logs and snapshots.
	Version string
	// Summary is a configuration digest/摘要 (model, provider, flags).
	Summary string
	// Services is the static service set of this snapshot.
	Services []ServiceDescriptor
}

// Loadout is an immutable snapshot of a service set. It is validated at
// construction; there is no way to mutate it afterwards. A run pins one
// revision; reassembly builds a new Loadout instead of editing this one.
type Loadout struct {
	id       string
	revision int64
	version  string
	summary  string
	services []ServiceDescriptor
	byName   map[string]int
}

// NewLoadout validates and freezes a service set.
func NewLoadout(cfg LoadoutConfig) (*Loadout, error) {
	issues := validateConfig(cfg)
	if len(issues) > 0 {
		return nil, &ResolveError{LoadoutID: cfg.ID, Revision: cfg.Revision, Issues: issues}
	}
	lo := &Loadout{
		id:       cfg.ID,
		revision: cfg.Revision,
		version:  cfg.Version,
		summary:  cfg.Summary,
		byName:   make(map[string]int, len(cfg.Services)),
	}
	for _, d := range cfg.Services {
		lo.services = append(lo.services, d.clone())
	}
	for i, d := range lo.services {
		lo.byName[d.Name] = i
	}
	return lo, nil
}

// ID returns the loadout id.
func (l *Loadout) ID() string { return l.id }

// Revision returns the configuration revision.
func (l *Loadout) Revision() int64 { return l.revision }

// Version returns the assembly version name.
func (l *Loadout) Version() string { return l.version }

// Summary returns the configuration summary.
func (l *Loadout) Summary() string { return l.summary }

// Len returns the number of services.
func (l *Loadout) Len() int { return len(l.services) }

// Services returns a deep copy of the service set in declaration order.
func (l *Loadout) Services() []ServiceDescriptor {
	out := make([]ServiceDescriptor, 0, len(l.services))
	for _, d := range l.services {
		out = append(out, d.clone())
	}
	return out
}

// Service returns a copy of one descriptor.
func (l *Loadout) Service(name string) (ServiceDescriptor, bool) {
	i, ok := l.byName[name]
	if !ok {
		return ServiceDescriptor{}, false
	}
	return l.services[i].clone(), true
}

// validateConfig checks the structural rules of a loadout. Provider-chain
// rules (duplicate/ambiguous providers) and cycles are checked by Resolve.
func validateConfig(cfg LoadoutConfig) []ResolveIssue {
	var issues []ResolveIssue
	seen := map[string]int{}
	for i, raw := range cfg.Services {
		d := raw.clone()
		if d.Name == "" {
			issues = append(issues, ResolveIssue{
				Code:    IssueEmptyName,
				Service: fmt.Sprintf("#%d", i),
				Message: fmt.Sprintf("service descriptor at index %d has an empty name", i),
			})
			continue
		}
		if prev, ok := seen[d.Name]; ok {
			issues = append(issues, ResolveIssue{
				Code:    IssueDuplicateService,
				Service: d.Name,
				Message: fmt.Sprintf("service %q is declared twice (indices %d and %d)", d.Name, prev, i),
			})
			continue
		}
		seen[d.Name] = i

		if len(d.Provides) == 0 {
			issues = append(issues, ResolveIssue{
				Code:    IssueMissingProvides,
				Service: d.Name,
				Message: fmt.Sprintf("service %q declares no provides; every service must declare at least one provided capability", d.Name),
			})
		}
		if d.Role != RolePrimary && d.Role != RoleFallback {
			issues = append(issues, ResolveIssue{
				Code:    IssueInvalidRole,
				Service: d.Name,
				Message: fmt.Sprintf("service %q declares role %q; want %q or %q", d.Name, d.Role, RolePrimary, RoleFallback),
			})
		}
		issues = append(issues, checkCapabilityList(d.Name, "provides", d.Provides, IssueDuplicateProvides)...)
		issues = append(issues, checkCapabilityList(d.Name, "requires", d.Requires, IssueDuplicateRequires)...)
		issues = append(issues, checkCapabilityList(d.Name, "optional", d.Optional, IssueDuplicateOptional)...)

		requires := map[string]bool{}
		for _, c := range d.Requires {
			requires[c] = true
		}
		for _, c := range d.Optional {
			if requires[c] {
				issues = append(issues, ResolveIssue{
					Code:       IssueRequiresOptional,
					Service:    d.Name,
					Capability: c,
					Message:    fmt.Sprintf("service %q lists capability %q as both requires and optional", d.Name, c),
				})
			}
		}
		for _, c := range d.Provides {
			if requires[c] {
				issues = append(issues, ResolveIssue{
					Code:       IssueSelfDependency,
					Service:    d.Name,
					Capability: c,
					Message:    fmt.Sprintf("service %q requires capability %q which it provides itself", d.Name, c),
				})
			}
		}
		for _, c := range d.Optional {
			for _, p := range d.Provides {
				if p == c {
					issues = append(issues, ResolveIssue{
						Code:       IssueSelfDependency,
						Service:    d.Name,
						Capability: c,
						Message:    fmt.Sprintf("service %q consumes capability %q optionally but provides it itself", d.Name, c),
					})
					break
				}
			}
		}
	}
	return issues
}

func checkCapabilityList(service, listName string, caps []string, dupCode string) []ResolveIssue {
	var issues []ResolveIssue
	seen := map[string]bool{}
	for _, c := range caps {
		if c == "" {
			issues = append(issues, ResolveIssue{
				Code:    IssueEmptyCapability,
				Service: service,
				Message: fmt.Sprintf("service %q declares an empty capability name in %s", service, listName),
			})
			continue
		}
		if seen[c] {
			issues = append(issues, ResolveIssue{
				Code:       dupCode,
				Service:    service,
				Capability: c,
				Message:    fmt.Sprintf("service %q lists capability %q twice in %s", service, c, listName),
			})
		}
		seen[c] = true
	}
	return issues
}
