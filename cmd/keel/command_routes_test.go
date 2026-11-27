package main

// Route registration guard for the chat command surface: the production product
// mux exposes both routes and the permission matrix declares each one (the same
// boot gate `keel serve` runs).

import (
	"testing"

	"github.com/jixoai/keel/internal/accessgate"
	"github.com/jixoai/keel/internal/harnessapi"
)

func TestCommandRoutesAreDeclared(t *testing.T) {
	api := harnessapi.New(harnessapi.Config{})
	registered := map[string]bool{}
	for _, pattern := range api.Routes() {
		registered[pattern] = true
	}
	for _, pattern := range []string{
		"GET /v1/sessions/{id}/commands",
		"POST /v1/sessions/{id}/commands/{name}",
	} {
		if !registered[pattern] {
			t.Fatalf("route %q is not registered on the product mux", pattern)
		}
		if err := accessgate.CheckRoutes([]string{pattern}); err != nil {
			t.Fatalf("route %q has no permission declaration: %v", pattern, err)
		}
	}
}
