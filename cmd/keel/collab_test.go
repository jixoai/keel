package main

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
)

func TestNewCollabModesRegistersBuiltins(t *testing.T) {
	tenant := uuid.New()
	registry, err := newCollabModes(tenant)
	if err != nil {
		t.Fatalf("newCollabModes: %v", err)
	}
	if registry.TenantID() != tenant {
		t.Fatalf("registry tenant = %s, want %s", registry.TenantID(), tenant)
	}
	if !registry.Enabled(collab.ModeSubagent) {
		t.Fatal("subagent mode should be enabled")
	}
	if registry.Enabled(collab.ModeSuperagent) {
		t.Fatal("superagent descriptor should ship disabled until its change lands")
	}
	view, ok := registry.Get(collab.ModeSuperagent)
	if !ok || view.Topology != collab.TopologyWorkQueue || view.WorkUnit != collab.WorkUnitTicket {
		t.Fatalf("superagent projection = %+v, ok=%v", view, ok)
	}
	views := registry.List()
	if len(views) != 2 {
		t.Fatalf("mode list = %d, want subagent+superagent", len(views))
	}
	// The assembly is deterministic: calling it twice produces the same
	// catalogue (duplicate registration would fail).
	second, err := newCollabModes(tenant)
	if err != nil {
		t.Fatalf("second newCollabModes: %v", err)
	}
	if len(second.List()) != len(views) {
		t.Fatalf("second catalogue = %d modes", len(second.List()))
	}
}
