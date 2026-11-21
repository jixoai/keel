package access

import (
	"fmt"
	"strings"
)

// EndpointKind is the kind of product entrance a coverage check enumerates.
// A4's CI gate feeds every HTTP route, tool manifest entry, CLI subcommand and
// UI action through CheckCoverage.
type EndpointKind string

// Endpoint kinds.
const (
	EndpointHTTP EndpointKind = "http"
	EndpointTool EndpointKind = "tool"
	EndpointCLI  EndpointKind = "cli"
	EndpointUI   EndpointKind = "ui"
)

var endpointKinds = []EndpointKind{EndpointHTTP, EndpointTool, EndpointCLI, EndpointUI}

// ValidEndpointKind reports whether kind is one of the enumerated kinds.
func ValidEndpointKind(kind EndpointKind) bool {
	for _, candidate := range endpointKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// KnownEndpointKinds returns a copy of the endpoint-kind vocabulary.
func KnownEndpointKinds() []EndpointKind {
	return append([]EndpointKind(nil), endpointKinds...)
}

// Endpoint describes one product entrance for the permission coverage gate.
// Every non-public endpoint MUST declare a required key; public endpoints are
// explicitly exempt and MUST NOT declare one, so an accidentally unguarded
// route cannot pass by being silently classified as public.
type Endpoint struct {
	// Kind is http, tool, cli or ui.
	Kind EndpointKind
	// Name identifies the entrance ("GET /v1/accounts", "bash", "keel bot list").
	Name string
	// RequiredKey is the permission key the entrance declares.
	RequiredKey string
	// Public marks an entrance that is intentionally unauthenticated
	// (login, bootstrap, health). It must leave RequiredKey empty.
	Public bool
}

// CoverageReason is the machine-readable coverage failure.
type CoverageReason string

const (
	// CoverageMissingKey means a non-public endpoint declared no key.
	CoverageMissingKey CoverageReason = "missing_key"
	// CoverageUnknownKey means the declared key is not in the registry.
	CoverageUnknownKey CoverageReason = "unknown_key"
	// CoverageMissingName means the endpoint has no name to report.
	CoverageMissingName CoverageReason = "missing_name"
	// CoverageInvalidKind means the endpoint kind is not enumerated.
	CoverageInvalidKind CoverageReason = "invalid_kind"
	// CoveragePublicWithKey means a public endpoint declared a key, which hides
	// an authorization mistake behind an exemption.
	CoveragePublicWithKey CoverageReason = "public_with_key"
	// CoverageTooFewEndpoints means the enumerated list is shorter than the
	// known-entrance floor. An empty or truncated catalog must fail the gate
	// instead of passing vacuously ("zero routes checked, zero problems").
	CoverageTooFewEndpoints CoverageReason = "too_few_endpoints"
)

// CoverageIssue is one failed endpoint.
type CoverageIssue struct {
	Kind        EndpointKind
	Name        string
	RequiredKey string
	Reason      CoverageReason
	Detail      string
}

// String renders the issue for a CI log.
func (i CoverageIssue) String() string {
	kind := i.Kind
	if kind == "" {
		kind = "endpoint"
	}
	name := i.Name
	if name == "" {
		name = "<unnamed>"
	}
	return fmt.Sprintf("%s %q: %s: %s", kind, name, i.Reason, i.Detail)
}

// CoverageReport is the pure result of CheckCoverage. Issues must make a CI
// gate fail; UnusedKeys are warnings only (design §3.1: registry 中无入口使用
// 的 key 只警告).
type CoverageReport struct {
	// Checked is the number of endpoints examined.
	Checked int
	// Public is the number of explicitly public endpoints.
	Public int
	// Issues are missing/unknown/invalid declarations.
	Issues []CoverageIssue
	// UnusedKeys are registered keys no endpoint declared, sorted.
	UnusedKeys []string
}

// OK reports whether the gate passes. Unused keys do not fail it.
func (r CoverageReport) OK() bool { return len(r.Issues) == 0 }

// Err returns nil when the gate passes, or a summarized error naming the first
// few failures.
func (r CoverageReport) Err() error {
	if len(r.Issues) == 0 {
		return nil
	}
	const limit = 5
	parts := make([]string, 0, limit)
	for index, issue := range r.Issues {
		if index == limit {
			break
		}
		parts = append(parts, issue.String())
	}
	suffix := ""
	if len(r.Issues) > limit {
		suffix = fmt.Sprintf(" (and %d more)", len(r.Issues)-limit)
	}
	return fmt.Errorf("access: permission coverage failed: %s%s", strings.Join(parts, "; "), suffix)
}

// CheckCoverage reports missing and unknown permission keys across an endpoint
// list. It is a pure function: A4 can run it in CI over a generated snapshot
// and in tests over a hand-written list.
func CheckCoverage(endpoints []Endpoint) CoverageReport {
	report := CoverageReport{}
	used := make(map[string]bool, len(endpoints))
	for _, endpoint := range endpoints {
		report.Checked++
		if !ValidEndpointKind(endpoint.Kind) {
			report.Issues = append(report.Issues, CoverageIssue{
				Kind:        endpoint.Kind,
				Name:        endpoint.Name,
				RequiredKey: endpoint.RequiredKey,
				Reason:      CoverageInvalidKind,
				Detail:      fmt.Sprintf("unknown endpoint kind %q (want http, tool, cli or ui)", endpoint.Kind),
			})
			continue
		}
		if strings.TrimSpace(endpoint.Name) == "" {
			report.Issues = append(report.Issues, CoverageIssue{
				Kind:        endpoint.Kind,
				Name:        endpoint.Name,
				RequiredKey: endpoint.RequiredKey,
				Reason:      CoverageMissingName,
				Detail:      "endpoint has no name",
			})
			continue
		}
		if endpoint.Public {
			report.Public++
			if endpoint.RequiredKey != "" {
				report.Issues = append(report.Issues, CoverageIssue{
					Kind:        endpoint.Kind,
					Name:        endpoint.Name,
					RequiredKey: endpoint.RequiredKey,
					Reason:      CoveragePublicWithKey,
					Detail:      fmt.Sprintf("public endpoint declared key %q", endpoint.RequiredKey),
				})
				continue
			}
			continue
		}
		if endpoint.RequiredKey == "" {
			report.Issues = append(report.Issues, CoverageIssue{
				Kind:   endpoint.Kind,
				Name:   endpoint.Name,
				Reason: CoverageMissingKey,
				Detail: "non-public endpoint declares no required permission key",
			})
			continue
		}
		if _, ok := Lookup(endpoint.RequiredKey); !ok {
			report.Issues = append(report.Issues, CoverageIssue{
				Kind:        endpoint.Kind,
				Name:        endpoint.Name,
				RequiredKey: endpoint.RequiredKey,
				Reason:      CoverageUnknownKey,
				Detail:      fmt.Sprintf("permission key %q is not in the registry", endpoint.RequiredKey),
			})
			continue
		}
		used[endpoint.RequiredKey] = true
	}
	for _, key := range Keys() {
		if !used[key] {
			report.UnusedKeys = append(report.UnusedKeys, key)
		}
	}
	return report
}

// MinHTTPEndpoints is the floor of the A4.1 product HTTP surface: accounts,
// permission groups, bots, team bots, bot keys and the identity summary. The
// coverage gate refuses a catalog below it, so a generator that silently
// returns nothing (or loses a route family) fails CI instead of passing
// vacuously.
const MinHTTPEndpoints = 24

// CheckCoverageFloor is CheckCoverage plus the non-empty/known-floor
// assertion. It is the gate form A4's CI runs; CheckCoverage stays the pure
// per-endpoint check.
func CheckCoverageFloor(endpoints []Endpoint, minimum int) CoverageReport {
	report := CheckCoverage(endpoints)
	if minimum <= 0 {
		minimum = MinHTTPEndpoints
	}
	if report.Checked < minimum {
		report.Issues = append([]CoverageIssue{{
			Kind:   EndpointHTTP,
			Name:   "<catalog>",
			Reason: CoverageTooFewEndpoints,
			Detail: fmt.Sprintf("enumerated %d endpoints, want at least %d", report.Checked, minimum),
		}}, report.Issues...)
	}
	return report
}
