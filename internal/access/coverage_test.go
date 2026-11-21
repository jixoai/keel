package access

import (
	"strings"
	"testing"
)

func TestCheckCoveragePassesCompleteList(t *testing.T) {
	endpoints := []Endpoint{
		{Kind: EndpointHTTP, Name: "POST /v1/auth/login", Public: true},
		{Kind: EndpointHTTP, Name: "GET /v1/auth/session", Public: true},
	}
	for _, spec := range All() {
		endpoints = append(endpoints, Endpoint{
			Kind:        EndpointHTTP,
			Name:        "route " + spec.Key,
			RequiredKey: spec.Key,
		})
	}
	report := CheckCoverage(endpoints)
	if !report.OK() {
		t.Fatalf("coverage failed: %v", report.Err())
	}
	if report.Checked != len(endpoints) {
		t.Fatalf("Checked = %d, want %d", report.Checked, len(endpoints))
	}
	if report.Public != 2 {
		t.Fatalf("Public = %d, want 2", report.Public)
	}
	if len(report.UnusedKeys) != 0 {
		t.Fatalf("UnusedKeys = %v, want none", report.UnusedKeys)
	}
	if err := report.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}
}

func TestCheckCoverageMissingKey(t *testing.T) {
	report := CheckCoverage([]Endpoint{
		{Kind: EndpointHTTP, Name: "GET /v1/accounts"},
	})
	if report.OK() {
		t.Fatal("missing key passed coverage")
	}
	if len(report.Issues) != 1 {
		t.Fatalf("Issues = %v, want one", report.Issues)
	}
	issue := report.Issues[0]
	if issue.Reason != CoverageMissingKey || issue.Name != "GET /v1/accounts" {
		t.Fatalf("issue = %+v", issue)
	}
	if err := report.Err(); err == nil || !strings.Contains(err.Error(), "missing_key") {
		t.Fatalf("Err() = %v, want a missing_key failure", err)
	}
}

func TestCheckCoverageUnknownKey(t *testing.T) {
	report := CheckCoverage([]Endpoint{
		{Kind: EndpointTool, Name: "bash", RequiredKey: "tool.bash"},
	})
	if report.OK() {
		t.Fatal("unknown key passed coverage")
	}
	if report.Issues[0].Reason != CoverageUnknownKey {
		t.Fatalf("issue = %+v", report.Issues[0])
	}
	if len(report.UnusedKeys) != len(Keys()) {
		t.Fatalf("an unknown key should not mark any registry key used: %v", report.UnusedKeys)
	}
}

func TestCheckCoveragePublicEndpoint(t *testing.T) {
	report := CheckCoverage([]Endpoint{
		{Kind: EndpointHTTP, Name: "POST /v1/auth/login", Public: true},
	})
	if !report.OK() {
		t.Fatalf("public endpoint failed coverage: %v", report.Err())
	}
	if report.Public != 1 {
		t.Fatalf("Public = %d, want 1", report.Public)
	}

	bad := CheckCoverage([]Endpoint{
		{Kind: EndpointHTTP, Name: "POST /v1/auth/login", Public: true, RequiredKey: KeyAccountRead},
	})
	if bad.OK() || bad.Issues[0].Reason != CoveragePublicWithKey {
		t.Fatalf("public endpoint with key = %+v, want public_with_key", bad.Issues)
	}
}

func TestCheckCoverageInvalidKindAndName(t *testing.T) {
	report := CheckCoverage([]Endpoint{
		{Kind: "grpc", Name: "rpc", RequiredKey: KeySessionRead},
		{Kind: EndpointHTTP, Name: "  ", RequiredKey: KeySessionRead},
	})
	if len(report.Issues) != 2 {
		t.Fatalf("Issues = %v, want two", report.Issues)
	}
	if report.Issues[0].Reason != CoverageInvalidKind {
		t.Fatalf("first issue = %+v", report.Issues[0])
	}
	if report.Issues[1].Reason != CoverageMissingName {
		t.Fatalf("second issue = %+v", report.Issues[1])
	}
}

func TestCheckCoverageUnusedKeysAreWarnings(t *testing.T) {
	report := CheckCoverage([]Endpoint{
		{Kind: EndpointHTTP, Name: "GET /v1/sessions", RequiredKey: KeySessionRead},
	})
	if !report.OK() {
		t.Fatalf("unused keys should not fail the gate: %v", report.Err())
	}
	if len(report.UnusedKeys) != len(Keys())-1 {
		t.Fatalf("UnusedKeys has %d entries, want %d", len(report.UnusedKeys), len(Keys())-1)
	}
	for _, key := range report.UnusedKeys {
		if key == KeySessionRead {
			t.Fatal("a used key was reported unused")
		}
	}
}

func TestCheckCoverageEmptyInput(t *testing.T) {
	report := CheckCoverage(nil)
	if !report.OK() || report.Checked != 0 {
		t.Fatalf("empty report = %+v", report)
	}
	if len(report.UnusedKeys) != len(Keys()) {
		t.Fatalf("every key should be unused with no endpoints: %v", report.UnusedKeys)
	}
}

func TestKnownEndpointKinds(t *testing.T) {
	for _, kind := range KnownEndpointKinds() {
		if !ValidEndpointKind(kind) {
			t.Fatalf("KnownEndpointKinds returned invalid %q", kind)
		}
	}
	if ValidEndpointKind("grpc") {
		t.Fatal("grpc accepted as an endpoint kind")
	}
}
