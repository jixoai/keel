package servicegraph

import (
	"fmt"
	"strings"
	"time"
)

// Severity classifies a diagnostic.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityDegraded Severity = "degraded"
	SeverityError    Severity = "error"
)

// Diagnostic codes. The message always explains the "why", so an operator can
// answer "why did this service not start" without reading code.
const (
	DiagMissingRequired = "missing-required"
	DiagProviderPending = "provider-pending"
	DiagProviderFailed  = "provider-failed"
	DiagOptionalMissing = "optional-missing"
	DiagOptionalFailed  = "optional-failed"
	DiagStandby         = "standby"
	DiagStandbyServing  = "standby-serving"
	DiagNoFactory       = "no-factory"
	DiagFactoryFailed   = "factory-failed"
	DiagStartFailed     = "start-failed"
	DiagEffectRelease   = "effect-release-failed"
	DiagStopFailed      = "stop-failed"
	DiagRetiring        = "retiring"
	DiagDrained         = "generation-drained"
	DiagDrainExpired    = "drain-deadline-exceeded"
)

// Diagnostic is one structured explanation attached to a generation.
type Diagnostic struct {
	Generation int64
	Service    string
	Code       string
	Severity   Severity
	Capability string
	WaitingOn  []string
	Message    string
	Err        string
}

// ServiceStatus is the state of one service in a Snapshot.
type ServiceStatus struct {
	Name     string
	Version  string
	State    State
	Role     Role
	Provides []string
	Requires []string
	Optional []string
	// Bindings maps capability → provider service name as resolved at load
	// time (empty for services that are not ACTIVE).
	Bindings map[string]string
	// WaitingOn lists services this one is blocked on.
	WaitingOn []string
	// Degraded lists optional capabilities running degraded.
	Degraded []string
	// Failure carries the activation error when State is FAILED.
	Failure string
	// Generation is the engine generation the live instance was loaded in
	// (0 when never loaded).
	Generation int64
	// Pins is the number of in-flight leases referencing this instance.
	Pins int
	// Effects are the registered and not yet released effects of the live
	// instance.
	Effects []Effect
}

// RetiringStatus describes one old-generation instance kept alive for
// in-flight runs.
type RetiringStatus struct {
	Generation int64
	Service    string
	// Pins is the number of in-flight leases still referencing the instance.
	Pins int
	// Deadline is when the drain deadline expires (zero when none is set); an
	// expired generation is force-unloaded and its runs must fail.
	Deadline time.Time
	Effects  []Effect
}

// Snapshot is an immutable view of the engine after one Apply (or Close).
type Snapshot struct {
	LoadoutID      string
	Revision       int64
	LoadoutVersion string
	Summary        string
	Generation     int64
	Services       []ServiceStatus
	// Retiring lists old-generation instances still held by in-flight runs.
	Retiring    []RetiringStatus
	Diagnostics []Diagnostic
}

// Service returns the status of one active-assembly service.
func (s *Snapshot) Service(name string) (ServiceStatus, bool) {
	for _, st := range s.Services {
		if st.Name == name {
			return st, true
		}
	}
	return ServiceStatus{}, false
}

// Retiring returns the retiring row of one service, if any.
func (s *Snapshot) RetiringService(name string) (RetiringStatus, bool) {
	for _, r := range s.Retiring {
		if r.Service == name {
			return r, true
		}
	}
	return RetiringStatus{}, false
}

// State returns the state of one service ("" when the service is absent). A
// generation still held by in-flight runs reports RETIRING.
func (s *Snapshot) State(name string) State {
	if st, ok := s.Service(name); ok {
		return st.State
	}
	if _, ok := s.RetiringService(name); ok {
		return StateRetiring
	}
	return ""
}

// Active reports whether a service is ACTIVE.
func (s *Snapshot) Active(name string) bool { return s.State(name) == StateActive }

// DiagnosticsFor returns the diagnostics attached to one service.
func (s *Snapshot) DiagnosticsFor(name string) []Diagnostic {
	var out []Diagnostic
	for _, d := range s.Diagnostics {
		if d.Service == name {
			out = append(out, d)
		}
	}
	return out
}

// Explain renders why a service is in its current state, following its
// waiting-on chain so "why didn't it start" is answerable in one call.
func (s *Snapshot) Explain(name string) string {
	var b strings.Builder
	s.explainInto(&b, name, 0, map[string]bool{})
	return b.String()
}

func (s *Snapshot) explainInto(b *strings.Builder, name string, depth int, seen map[string]bool) {
	indent := strings.Repeat("  ", depth)
	st, ok := s.Service(name)
	if !ok {
		if r, ok := s.RetiringService(name); ok {
			fmt.Fprintf(b, "%s%s: [%s] generation %d, %d in-flight reference(s)", indent, r.Service, StateRetiring, r.Generation, r.Pins)
			if !r.Deadline.IsZero() {
				fmt.Fprintf(b, ", drain deadline %s", r.Deadline.Format(time.RFC3339))
			}
			b.WriteString("\n")
			for _, d := range s.DiagnosticsFor(name) {
				if d.Code == DiagRetiring || d.Code == DiagDrained || d.Code == DiagDrainExpired {
					fmt.Fprintf(b, "%s  %s: %s\n", indent, d.Code, d.Message)
				}
			}
			return
		}
		fmt.Fprintf(b, "%s%s: (not in loadout)\n", indent, name)
		return
	}
	if seen[name] || depth > 4 {
		fmt.Fprintf(b, "%s%s: [%s] (already explained)\n", indent, name, st.State)
		return
	}
	seen[name] = true
	fmt.Fprintf(b, "%s%s: [%s] generation %d", indent, st.Name, st.State, st.Generation)
	if len(st.Degraded) > 0 {
		fmt.Fprintf(b, " degraded=%s", strings.Join(st.Degraded, ","))
	}
	b.WriteString("\n")
	if st.Failure != "" {
		fmt.Fprintf(b, "%s  failure: %s\n", indent, st.Failure)
	}
	for _, d := range s.DiagnosticsFor(name) {
		fmt.Fprintf(b, "%s  %s: %s\n", indent, d.Code, d.Message)
	}
	for _, w := range st.WaitingOn {
		s.explainInto(b, w, depth+1, seen)
	}
}

// String renders a compact one-line-per-service overview.
func (s *Snapshot) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "loadout %s revision %d generation %d: %d service(s)\n",
		s.LoadoutID, s.Revision, s.Generation, len(s.Services))
	for _, st := range s.Services {
		fmt.Fprintf(&b, "  %-20s %-9s gen=%d provides=%s", st.Name, st.State, st.Generation, strings.Join(st.Provides, ","))
		if st.Pins > 0 {
			fmt.Fprintf(&b, " pins=%d", st.Pins)
		}
		if len(st.Degraded) > 0 {
			fmt.Fprintf(&b, " degraded=%s", strings.Join(st.Degraded, ","))
		}
		if st.Failure != "" {
			fmt.Fprintf(&b, " failure=%s", st.Failure)
		}
		if len(st.WaitingOn) > 0 {
			fmt.Fprintf(&b, " waiting-on=%s", strings.Join(st.WaitingOn, ","))
		}
		b.WriteString("\n")
	}
	for _, r := range s.Retiring {
		fmt.Fprintf(&b, "  %-20s %-9s gen=%d pins=%d", r.Service, StateRetiring, r.Generation, r.Pins)
		if !r.Deadline.IsZero() {
			fmt.Fprintf(&b, " deadline=%s", r.Deadline.Format(time.RFC3339))
		}
		b.WriteString("\n")
	}
	for _, d := range s.Diagnostics {
		fmt.Fprintf(&b, "  diag [%s] %s: %s\n", d.Severity, d.Service, d.Message)
	}
	return b.String()
}
