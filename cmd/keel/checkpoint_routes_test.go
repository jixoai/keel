package main

// Route registration guard for the checkpoint surface: the production product
// mux must expose the routes and the permission matrix must declare every one
// (the same boot gate `keel serve` runs).

import (
	"testing"

	"github.com/jixoai/keel/internal/accessgate"
	"github.com/jixoai/keel/internal/harnessapi"
)

func TestCheckpointRoutesAreDeclared(t *testing.T) {
	api := harnessapi.New(harnessapi.Config{})
	registered := map[string]bool{}
	for _, pattern := range api.Routes() {
		registered[pattern] = true
	}
	for _, pattern := range []string{
		"POST /v1/sessions/{id}/compact",
		"GET /v1/sessions/{id}/checkpoint-policy",
		"PUT /v1/sessions/{id}/checkpoint-policy",
	} {
		if !registered[pattern] {
			t.Fatalf("route %q is not registered on the product mux", pattern)
		}
		if err := accessgate.CheckRoutes([]string{pattern}); err != nil {
			t.Fatalf("route %q has no permission declaration: %v", pattern, err)
		}
	}
}
