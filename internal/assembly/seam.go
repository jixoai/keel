package assembly

import (
	"sort"
	"strings"
)

// Seam is one replaceable capability boundary. The five seams are the frozen
// W0.5 list: fs, shell, subprocess, exec-surface, terminal
// (docs/PLUGIN-EXTENSIBILITY.md §6, design.md §10).
type Seam string

const (
	// SeamFS is the logical file API behind read/write/edit.
	SeamFS Seam = "fs"
	// SeamShell is the command execution surface behind bash.
	SeamShell Seam = "shell"
	// SeamSubprocess supervises child processes and sidecars.
	SeamSubprocess Seam = "subprocess"
	// SeamExecSurface materializes and captures the workspace an execution
	// surface sees (local, remote, SSH).
	SeamExecSurface Seam = "exec-surface"
	// SeamTerminal is the interactive terminal attachment surface.
	SeamTerminal Seam = "terminal"
)

// Seams returns the closed set of capability seams in declaration order.
func Seams() []Seam {
	return []Seam{SeamFS, SeamShell, SeamSubprocess, SeamExecSurface, SeamTerminal}
}

// ParseSeam validates a seam name.
func ParseSeam(raw string) (Seam, bool) {
	for _, seam := range Seams() {
		if string(seam) == raw {
			return seam, true
		}
	}
	return "", false
}

// Capability is one operation a seam provider declares. The vocabulary is
// closed per seam: an unknown name is invalid_capability, never a silent no-op.
type Capability string

const (
	// fs: the logical file API.
	CapFSRead        Capability = "read"
	CapFSWrite       Capability = "write"
	CapFSEdit        Capability = "edit"
	CapFSDelete      Capability = "delete"
	CapFSStat        Capability = "stat"
	CapFSList        Capability = "list"
	CapFSRename      Capability = "rename"
	CapFSSymlink     Capability = "symlink"
	CapFSChmod       Capability = "chmod"
	CapFSAtomicWrite Capability = "atomic-write"

	// shell: command execution.
	CapShellExec       Capability = "exec"
	CapShellEnv        Capability = "env"
	CapShellCwd        Capability = "cwd"
	CapShellTimeout    Capability = "timeout"
	CapShellBackground Capability = "background"
	CapShellJobControl Capability = "job-control"

	// subprocess: child process / sidecar supervision.
	CapSubprocessSpawn  Capability = "spawn"
	CapSubprocessWait   Capability = "wait"
	CapSubprocessSignal Capability = "signal"
	CapSubprocessKill   Capability = "kill"
	CapSubprocessStdin  Capability = "stdin"
	CapSubprocessStdout Capability = "stdout"
	CapSubprocessStderr Capability = "stderr"
	CapSubprocessPTY    Capability = "pty"

	// exec-surface: workspace materialization and capture.
	CapExecMaterialize   Capability = "materialize"
	CapExecCapture       Capability = "capture"
	CapExecSnapshot      Capability = "snapshot"
	CapExecDeleteTrack   Capability = "delete-tracking"
	CapExecSymlink       Capability = "symlink"
	CapExecExecBit       Capability = "exec-bit"
	CapExecAtomicRename  Capability = "atomic-rename"
	CapExecCaseSensitive Capability = "case-sensitive"

	// terminal: interactive terminal sessions.
	CapTerminalAttach Capability = "attach"
	CapTerminalResize Capability = "resize"
	CapTerminalScroll Capability = "scrollback"
	CapTerminalDetach Capability = "detach"
	CapTerminalReplay Capability = "replay"
)

var seamCapabilities = map[Seam][]Capability{
	SeamFS: {
		CapFSRead, CapFSWrite, CapFSEdit, CapFSDelete, CapFSStat,
		CapFSList, CapFSRename, CapFSSymlink, CapFSChmod, CapFSAtomicWrite,
	},
	SeamShell: {
		CapShellExec, CapShellEnv, CapShellCwd, CapShellTimeout,
		CapShellBackground, CapShellJobControl,
	},
	SeamSubprocess: {
		CapSubprocessSpawn, CapSubprocessWait, CapSubprocessSignal, CapSubprocessKill,
		CapSubprocessStdin, CapSubprocessStdout, CapSubprocessStderr, CapSubprocessPTY,
	},
	SeamExecSurface: {
		CapExecMaterialize, CapExecCapture, CapExecSnapshot, CapExecDeleteTrack,
		CapExecSymlink, CapExecExecBit, CapExecAtomicRename, CapExecCaseSensitive,
	},
	SeamTerminal: {
		CapTerminalAttach, CapTerminalResize, CapTerminalScroll,
		CapTerminalDetach, CapTerminalReplay,
	},
}

// KnownCapabilities returns the closed capability vocabulary of a seam in
// documentation order. An unknown seam returns nil.
func KnownCapabilities(seam Seam) []Capability {
	known, ok := seamCapabilities[seam]
	if !ok {
		return nil
	}
	out := make([]Capability, len(known))
	copy(out, known)
	return out
}

// ParseCapability validates a capability name inside a seam.
func ParseCapability(seam Seam, raw string) (Capability, bool) {
	for _, capability := range seamCapabilities[seam] {
		if string(capability) == raw {
			return capability, true
		}
	}
	return "", false
}

// Provider is one seam implementation (local, remote, ssh, ...) and the
// capabilities it declares. A provider may be registered for exactly one seam.
type Provider struct {
	// Seam is the capability boundary this provider implements.
	Seam Seam
	// Impl is the implementation id used by assembly rows.
	Impl string
	// Capabilities is the declared capability set. Unknown names are rejected
	// at registry construction.
	Capabilities []Capability
}

func (p Provider) clone() Provider {
	p.Capabilities = append([]Capability(nil), p.Capabilities...)
	return p
}

func (p Provider) has(capability Capability) bool {
	for _, declared := range p.Capabilities {
		if declared == capability {
			return true
		}
	}
	return false
}

// Requirement is one startup requirement: seam provider Impl must support every
// listed capability. The hosting process supplies these from the rows it is
// about to activate (or from the seam adapters it selected).
type Requirement struct {
	Seam         Seam
	Impl         string
	Capabilities []Capability
}

// Need builds a requirement.
func Need(seam Seam, impl string, capabilities ...Capability) Requirement {
	return Requirement{Seam: seam, Impl: impl, Capabilities: capabilities}
}

// CapabilitySupport is one cell of the capability matrix.
type CapabilitySupport struct {
	Capability Capability `json:"capability"`
	Supported  bool       `json:"supported"`
}

// MatrixEntry is one provider row of the capability matrix: every known
// capability of the seam, with whether this provider declares it.
type MatrixEntry struct {
	Seam         Seam                `json:"seam"`
	Impl         string              `json:"impl"`
	Capabilities []CapabilitySupport `json:"capabilities"`
}

// Registry is an immutable set of seam providers. Construction validates the
// whole matrix; Check answers whether a set of startup requirements is
// satisfiable.
type Registry struct {
	providers map[Seam]map[string]Provider
}

// NewRegistry validates and freezes a provider set.
func NewRegistry(providers ...Provider) (*Registry, error) {
	registry := &Registry{providers: map[Seam]map[string]Provider{}}
	var problems []CapabilityProblem
	for _, provider := range providers {
		seam, ok := ParseSeam(string(provider.Seam))
		if !ok {
			problems = append(problems, CapabilityProblem{
				Code:    ProblemUnknownSeam,
				Seam:    provider.Seam,
				Impl:    provider.Impl,
				Message: "unknown seam " + string(provider.Seam),
			})
			continue
		}
		if !seamImplPattern.MatchString(provider.Impl) {
			problems = append(problems, CapabilityProblem{
				Code:    ProblemInvalidProvider,
				Seam:    seam,
				Impl:    provider.Impl,
				Message: "invalid provider implementation id " + provider.Impl,
			})
			continue
		}
		byImpl := registry.providers[seam]
		if byImpl == nil {
			byImpl = map[string]Provider{}
			registry.providers[seam] = byImpl
		}
		if _, exists := byImpl[provider.Impl]; exists {
			problems = append(problems, CapabilityProblem{
				Code:    ProblemDuplicateProvider,
				Seam:    seam,
				Impl:    provider.Impl,
				Message: "provider " + string(seam) + "/" + provider.Impl + " is registered twice",
			})
			continue
		}
		capabilities, capabilityProblems := normalizeCapabilities(seam, provider.Capabilities)
		problems = append(problems, capabilityProblems...)
		if len(capabilityProblems) > 0 {
			continue
		}
		byImpl[provider.Impl] = Provider{Seam: seam, Impl: provider.Impl, Capabilities: capabilities}
	}
	if len(problems) > 0 {
		return nil, &InvalidCapabilityError{Problems: problems}
	}
	return registry, nil
}

func normalizeCapabilities(seam Seam, declared []Capability) ([]Capability, []CapabilityProblem) {
	seen := map[Capability]bool{}
	var problems []CapabilityProblem
	for _, capability := range declared {
		if _, ok := ParseCapability(seam, string(capability)); !ok {
			problems = append(problems, CapabilityProblem{
				Code:       ProblemUnknownCapability,
				Seam:       seam,
				Capability: capability,
				Message:    "seam " + string(seam) + " has no capability " + string(capability),
			})
			continue
		}
		if seen[capability] {
			problems = append(problems, CapabilityProblem{
				Code:       ProblemDuplicateCapability,
				Seam:       seam,
				Capability: capability,
				Message:    "capability " + string(capability) + " is declared twice for seam " + string(seam),
			})
			continue
		}
		seen[capability] = true
	}
	if len(problems) > 0 {
		return nil, problems
	}
	ordered := make([]Capability, 0, len(declared))
	for _, capability := range seamCapabilities[seam] {
		if seen[capability] {
			ordered = append(ordered, capability)
		}
	}
	return ordered, nil
}

// Lookup returns one provider.
func (r *Registry) Lookup(seam Seam, impl string) (Provider, bool) {
	if r == nil {
		return Provider{}, false
	}
	provider, ok := r.providers[seam][impl]
	if !ok {
		return Provider{}, false
	}
	return provider.clone(), true
}

// Supports reports whether a provider declares a capability.
func (r *Registry) Supports(seam Seam, impl string, capability Capability) bool {
	provider, ok := r.Lookup(seam, impl)
	if !ok {
		return false
	}
	return provider.has(capability)
}

// Providers returns the registered providers of a seam in impl order.
func (r *Registry) Providers(seam Seam) []Provider {
	if r == nil {
		return nil
	}
	byImpl := r.providers[seam]
	impls := make([]string, 0, len(byImpl))
	for impl := range byImpl {
		impls = append(impls, impl)
	}
	sort.Strings(impls)
	out := make([]Provider, 0, len(impls))
	for _, impl := range impls {
		out = append(out, byImpl[impl].clone())
	}
	return out
}

// Matrix returns the whole capability matrix in seam order, provider impl
// order and canonical capability order.
func (r *Registry) Matrix() []MatrixEntry {
	var out []MatrixEntry
	for _, seam := range Seams() {
		known := KnownCapabilities(seam)
		for _, provider := range r.Providers(seam) {
			cells := make([]CapabilitySupport, 0, len(known))
			for _, capability := range known {
				cells = append(cells, CapabilitySupport{
					Capability: capability,
					Supported:  provider.has(capability),
				})
			}
			out = append(out, MatrixEntry{Seam: seam, Impl: provider.Impl, Capabilities: cells})
		}
	}
	return out
}

// Check rejects malformed requirements (invalid_capability) and unsatisfiable
// combinations (unsupported_capability). It returns nil when every requirement
// is satisfied.
func (r *Registry) Check(requirements ...Requirement) error {
	if len(requirements) == 0 {
		return nil
	}
	if r == nil {
		return &InvalidCapabilityError{Problems: []CapabilityProblem{{
			Code:    ProblemMissingProvider,
			Message: "seam requirements were declared without a provider registry",
		}}}
	}
	var problems []CapabilityProblem
	var gaps []CapabilityGap
	for _, requirement := range requirements {
		seam, ok := ParseSeam(string(requirement.Seam))
		if !ok {
			problems = append(problems, CapabilityProblem{
				Code:    ProblemUnknownSeam,
				Seam:    requirement.Seam,
				Impl:    requirement.Impl,
				Message: "unknown seam " + string(requirement.Seam),
			})
			continue
		}
		if !seamImplPattern.MatchString(requirement.Impl) {
			problems = append(problems, CapabilityProblem{
				Code:    ProblemInvalidProvider,
				Seam:    seam,
				Impl:    requirement.Impl,
				Message: "invalid provider implementation id " + requirement.Impl,
			})
			continue
		}
		valid := true
		for _, capability := range requirement.Capabilities {
			if _, ok := ParseCapability(seam, string(capability)); !ok {
				problems = append(problems, CapabilityProblem{
					Code:       ProblemUnknownCapability,
					Seam:       seam,
					Impl:       requirement.Impl,
					Capability: capability,
					Message:    "seam " + string(seam) + " has no capability " + string(capability),
				})
				valid = false
			}
		}
		if !valid {
			continue
		}
		provider, ok := r.Lookup(seam, requirement.Impl)
		if !ok {
			gaps = mergeGap(gaps, CapabilityGap{
				Seam:            seam,
				Impl:            requirement.Impl,
				Missing:         append([]Capability(nil), requirement.Capabilities...),
				UnknownProvider: true,
			})
			continue
		}
		var missing []Capability
		for _, capability := range requirement.Capabilities {
			if !provider.has(capability) {
				missing = append(missing, capability)
			}
		}
		if len(missing) > 0 {
			gaps = mergeGap(gaps, CapabilityGap{Seam: seam, Impl: requirement.Impl, Missing: missing})
		}
	}
	if len(problems) > 0 {
		return &InvalidCapabilityError{Problems: problems}
	}
	if len(gaps) > 0 {
		return &UnsupportedCapabilityError{Gaps: gaps}
	}
	return nil
}

func mergeGap(gaps []CapabilityGap, gap CapabilityGap) []CapabilityGap {
	for i := range gaps {
		if gaps[i].Seam != gap.Seam || gaps[i].Impl != gap.Impl {
			continue
		}
		gaps[i].UnknownProvider = gaps[i].UnknownProvider || gap.UnknownProvider
		seen := map[Capability]bool{}
		for _, capability := range gaps[i].Missing {
			seen[capability] = true
		}
		for _, capability := range gap.Missing {
			if !seen[capability] {
				gaps[i].Missing = append(gaps[i].Missing, capability)
				seen[capability] = true
			}
		}
		return gaps
	}
	return append(gaps, gap)
}

func joinCapabilities(capabilities []Capability) string {
	if len(capabilities) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		parts = append(parts, string(capability))
	}
	return strings.Join(parts, ", ")
}
