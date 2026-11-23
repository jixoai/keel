package assembly

import (
	"fmt"
	"strings"
)

// Issue codes reported when a layer document is rejected. They are stable
// strings so tests and operators can name a failure without matching prose.
const (
	// Structure of the four-stage stack.
	IssueMissingBase    = "missing-base"
	IssueStageOrder     = "stage-order"
	IssueStageDuplicate = "stage-duplicate"
	IssueStageUnknown   = "stage-unknown"
	IssueMode           = "mode"

	// Row identity and document shape.
	IssueRowID         = "row-id"
	IssueRowDuplicate  = "row-duplicate"
	IssueRowUnknown    = "row-unknown"
	IssueRowFinal      = "row-final"
	IssueRowOrder      = "row-order"
	IssueRowAbsentVals = "row-absent-values"
	IssueRowBaseAbsent = "row-base-absent"
	IssueRowLayer      = "row-layer"
	IssueRowImpl       = "row-impl"
	IssueRowParams     = "row-params"
	IssueRowRequires   = "row-requires"
	IssueRowCycle      = "row-cycle"

	// The config.toml stage and its rotation identity.
	IssueConfigVersion    = "config-version"
	IssueConfigTable      = "config-table"
	IssueConfigKey        = "config-key"
	IssueConfigValue      = "config-value"
	IssueConfigDuplicate  = "config-duplicate"
	IssueConfigAbsentImpl = "config-absent-impl"
	IssueConfigEmptyRow   = "config-empty-row"
	IssueConfigParamsAbs  = "config-params-on-absent"
	IssueConfigAnchor     = "config-anchor"
)

// Code is the stable code returned by capability rejection errors. The spec
// names unsupported_capability literally; invalid_capability covers a
// requirement or declaration that is not part of the closed vocabulary.
const (
	CodeUnsupportedCapability = "unsupported_capability"
	CodeInvalidCapability     = "invalid_capability"
)

// Issue is one reason a layer document cannot be assembled.
type Issue struct {
	// Code is a stable machine-readable reason (see the Issue* constants).
	Code string
	// Stage is the assembly stage the issue belongs to ("" for stack-level
	// issues such as a missing base).
	Stage Stage
	// Row names the offending row id when one is known.
	Row RowID
	// Field is a document path such as "base[3].id" or "config.toml:7".
	Field string
	// Message explains the problem in one sentence.
	Message string
}

// Error reports one or more rejected layer documents. Build validates the
// whole stack before anything is assembled, so a caller never sees a
// half-overlaid plan.
type Error struct {
	Issues []Issue
}

func (e *Error) Error() string {
	if e == nil {
		return "assembly: no error"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "assembly: %d issue(s)", len(e.Issues))
	for _, issue := range e.Issues {
		b.WriteString("\n  - ")
		b.WriteString(issue.Code)
		b.WriteString(": ")
		b.WriteString(issue.Message)
		if issue.Field != "" {
			fmt.Fprintf(&b, " [%s]", issue.Field)
		}
	}
	return b.String()
}

// HasCode reports whether any issue uses code.
func (e *Error) HasCode(code string) bool {
	if e == nil {
		return false
	}
	for _, issue := range e.Issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}

// Codes returns the distinct issue codes in first-seen order.
func (e *Error) Codes() []string {
	if e == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, issue := range e.Issues {
		if seen[issue.Code] {
			continue
		}
		seen[issue.Code] = true
		out = append(out, issue.Code)
	}
	return out
}

// CapabilityProblem is one malformed capability declaration or requirement.
// Unlike a capability gap (a valid requirement the chosen provider cannot
// satisfy), a problem means the vocabulary itself is wrong and no provider
// could satisfy it.
type CapabilityProblem struct {
	// Code is a stable machine-readable reason (see the CapabilityProblem*
	// constants).
	Code string
	// Seam is the seam the declaration belongs to, when known.
	Seam Seam
	// Impl is the provider implementation id, when known.
	Impl string
	// Capability is the offending capability, when known.
	Capability Capability
	// Message explains the problem in one sentence.
	Message string
}

// Stable capability-problem codes.
const (
	ProblemUnknownSeam         = "unknown-seam"
	ProblemUnknownCapability   = "unknown-capability"
	ProblemInvalidProvider     = "invalid-provider"
	ProblemDuplicateProvider   = "duplicate-provider"
	ProblemDuplicateCapability = "duplicate-capability"
	ProblemMissingProvider     = "missing-provider"
	ProblemMissingCapability   = "missing-capability"
)

// InvalidCapabilityError rejects a capability declaration or requirement that
// is outside the closed vocabulary. Code() returns invalid_capability.
type InvalidCapabilityError struct {
	Problems []CapabilityProblem
}

func (e *InvalidCapabilityError) Error() string {
	if e == nil {
		return "assembly: no error"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "assembly: %s: %d problem(s)", CodeInvalidCapability, len(e.Problems))
	for _, problem := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(problem.Code)
		b.WriteString(": ")
		b.WriteString(problem.Message)
	}
	return b.String()
}

// Code returns the stable machine-readable error code.
func (*InvalidCapabilityError) Code() string { return CodeInvalidCapability }

// CapabilityGap is one requirement the selected provider cannot satisfy. An
// unknown provider is a gap with UnknownProvider set and every required
// capability missing: the startup decision is the same (reject), while the
// message tells the operator whether to install a provider or pick another.
type CapabilityGap struct {
	// Seam and Impl identify the selected provider seam.
	Seam Seam
	Impl string
	// Missing lists the required capabilities the provider does not declare.
	Missing []Capability
	// UnknownProvider is true when no provider with this id is registered.
	UnknownProvider bool
}

// UnsupportedCapabilityError rejects an assembly before startup because a
// selected seam provider cannot provide a required capability. Code() returns
// unsupported_capability.
type UnsupportedCapabilityError struct {
	Gaps []CapabilityGap
}

func (e *UnsupportedCapabilityError) Error() string {
	if e == nil {
		return "assembly: no error"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "assembly: %s: %d unsupported combination(s)", CodeUnsupportedCapability, len(e.Gaps))
	for _, gap := range e.Gaps {
		b.WriteString("\n  - ")
		b.WriteString(string(gap.Seam))
		b.WriteString("/")
		b.WriteString(gap.Impl)
		if gap.UnknownProvider {
			b.WriteString(": provider is not registered")
			continue
		}
		b.WriteString(": missing ")
		b.WriteString(joinCapabilities(gap.Missing))
	}
	return b.String()
}

// Code returns the stable machine-readable error code.
func (*UnsupportedCapabilityError) Code() string { return CodeUnsupportedCapability }

// field renders the document path of row index i in doc.
func field(doc Document, i int) string {
	name := doc.Name
	if name == "" {
		name = string(doc.Stage)
	}
	return fmt.Sprintf("%s[%d]", name, i)
}
