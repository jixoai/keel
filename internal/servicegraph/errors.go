package servicegraph

import (
	"fmt"
	"strings"
)

// Issue codes reported when a loadout is rejected before anything starts.
const (
	IssueEmptyName         = "empty-name"
	IssueDuplicateService  = "duplicate-service"
	IssueMissingProvides   = "missing-provides"
	IssueDuplicateProvides = "duplicate-provides"
	IssueDuplicateRequires = "duplicate-requires"
	IssueDuplicateOptional = "duplicate-optional"
	IssueEmptyCapability   = "empty-capability"
	IssueInvalidRole       = "invalid-role"
	IssueSelfDependency    = "self-dependency"
	IssueRequiresOptional  = "requires-and-optional"
	IssueDuplicateProvider = "duplicate-provider"
	IssueAmbiguousProvider = "ambiguous-provider"
	IssueCycle             = "cycle"
)

// ResolveIssue is one reason a loadout cannot be assembled.
type ResolveIssue struct {
	Code       string
	Service    string
	Capability string
	Providers  []string
	Cycle      []string
	Message    string
}

// ResolveError reports a rejected loadout. Rejection happens before any
// service is activated: the engine keeps its previous generation untouched.
type ResolveError struct {
	LoadoutID string
	Revision  int64
	Issues    []ResolveIssue
}

func (e *ResolveError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "servicegraph: loadout %q revision %d rejected (%d issue(s))", e.LoadoutID, e.Revision, len(e.Issues))
	for _, issue := range e.Issues {
		b.WriteString("\n  - ")
		b.WriteString(issue.Code)
		b.WriteString(": ")
		b.WriteString(issue.Message)
	}
	return b.String()
}

// ServiceFailure records one service that failed to activate.
type ServiceFailure struct {
	Service string
	Code    string
	Err     string
}

// ActivationError reports services that failed during an Apply. The rest of
// the loadout is committed: independent services stay ACTIVE, dependents of a
// failed service remain PENDING with diagnostics explaining why.
type ActivationError struct {
	Generation int64
	Failures   []ServiceFailure
}

func (e *ActivationError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "servicegraph: generation %d: %d service(s) failed to activate", e.Generation, len(e.Failures))
	for _, f := range e.Failures {
		fmt.Fprintf(&b, "\n  - %s (%s): %s", f.Service, f.Code, f.Err)
	}
	return b.String()
}
