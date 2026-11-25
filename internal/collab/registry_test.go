package collab_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
)

func testMode(limits collab.Limits) collab.ModeDescriptor {
	return collab.ModeDescriptor{
		ID:          "test.mode",
		Title:       "Test mode",
		Description: "test descriptor",
		Roles: []collab.RoleSpec{
			{Name: "main", Min: 1, Max: 1},
			{Name: "worker", Min: 0, Max: 0},
		},
		Topology:     collab.TopologyTree,
		WorkUnit:     collab.WorkUnitDelegation,
		Inputs:       []string{"task"},
		Outputs:      []string{"summary"},
		Capabilities: []collab.Capability{collab.CapabilityAttention},
		Limits:       limits,
		UI:           collab.UIEntry{Label: "Test", Route: "/test"},
	}
}

func newRegistry(t *testing.T, ceiling collab.Limits) *collab.ModeRegistry {
	t.Helper()
	return newRegistryFor(t, uuid.New(), ceiling)
}

func newRegistryFor(t *testing.T, tenantID uuid.UUID, ceiling collab.Limits) *collab.ModeRegistry {
	t.Helper()
	registry, err := collab.NewModeRegistry(tenantID, ceiling)
	if err != nil {
		t.Fatalf("NewModeRegistry: %v", err)
	}
	return registry
}

func TestRegistryRegistersBuiltinModesAndProjects(t *testing.T) {
	registry := newRegistry(t, collab.Limits{
		MaxDepth: 8, MaxChildren: 32, MaxConcurrent: 64,
		MaxTurns: 500, MaxTokens: 5_000_000, MaxDurationMs: 3_600_000,
	})
	for _, descriptor := range collab.BuiltinModes() {
		if err := registry.Register(descriptor); err != nil {
			t.Fatalf("Register(%s): %v", descriptor.ID, err)
		}
	}
	if err := registry.SetEnabled(collab.ModeSuperagent, false); err != nil {
		t.Fatalf("SetEnabled(superagent): %v", err)
	}

	views := registry.List()
	if len(views) != 2 {
		t.Fatalf("List() = %d modes, want 2", len(views))
	}
	if views[0].ID != collab.ModeSubagent || views[1].ID != collab.ModeSuperagent {
		t.Fatalf("List() order = %s,%s; want subagent,superagent", views[0].ID, views[1].ID)
	}
	sub := views[0]
	if !sub.Enabled {
		t.Fatal("subagent should start enabled")
	}
	if sub.TenantID != registry.TenantID() || sub.TenantID == uuid.Nil {
		t.Fatalf("projection tenant = %s, registry = %s", sub.TenantID, registry.TenantID())
	}
	if sub.Topology != collab.TopologyTree || sub.WorkUnit != collab.WorkUnitDelegation {
		t.Fatalf("subagent projection = %+v", sub)
	}
	if len(sub.Roles) == 0 || len(sub.Capabilities) == 0 {
		t.Fatalf("subagent projection lost roles/capabilities: %+v", sub)
	}
	if views[1].Enabled {
		t.Fatal("superagent descriptor should be disabled by assembly")
	}

	// The tenant ceiling is a hard upper bound, even for a builtin descriptor.
	narrow := newRegistry(t, collab.Limits{MaxDepth: 2})
	if err := narrow.Register(collab.SubagentMode()); err != nil {
		t.Fatalf("Register(subagent): %v", err)
	}
	view, ok := narrow.Get(collab.ModeSubagent)
	if !ok {
		t.Fatal("Get(subagent) missing")
	}
	if view.Limits.MaxDepth != 2 {
		t.Fatalf("narrowed MaxDepth = %d, want 2", view.Limits.MaxDepth)
	}
}

func TestSubagentModeDefaultDepthIsThree(t *testing.T) {
	// The sub-agent change fixes the default at 3 (main depth 0, children up
	// to depth 3). The core descriptor must not drift to a wider arbitrary
	// number: keel-collab-subagent reads this value as the default.
	if collab.DefaultSubagentMaxDepth != 3 {
		t.Fatalf("DefaultSubagentMaxDepth = %d, want 3", collab.DefaultSubagentMaxDepth)
	}
	descriptor := collab.SubagentMode()
	if descriptor.Limits.MaxDepth != collab.DefaultSubagentMaxDepth {
		t.Fatalf("SubagentMode MaxDepth = %d, want %d", descriptor.Limits.MaxDepth, collab.DefaultSubagentMaxDepth)
	}
	if descriptor.Limits.MaxChildren != collab.DefaultSubagentMaxChildren {
		t.Fatalf("SubagentMode MaxChildren = %d, want %d", descriptor.Limits.MaxChildren, collab.DefaultSubagentMaxChildren)
	}
}

func TestRegistryRejectsDuplicateAndUnknown(t *testing.T) {
	registry := newRegistry(t, collab.Limits{})
	if err := registry.Register(testMode(collab.Limits{})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.Register(testMode(collab.Limits{})); !errors.Is(err, collab.ErrDuplicateMode) {
		t.Fatalf("duplicate Register = %v, want ErrDuplicateMode", err)
	}
	if err := registry.SetEnabled("missing", true); !errors.Is(err, collab.ErrUnknownMode) {
		t.Fatalf("SetEnabled(unknown) = %v, want ErrUnknownMode", err)
	}
	if _, err := registry.RequireEnabled("missing"); !errors.Is(err, collab.ErrUnknownMode) {
		t.Fatalf("RequireEnabled(unknown) = %v, want ErrUnknownMode", err)
	}
	if registry.Enabled("missing") {
		t.Fatal("unknown mode reported enabled")
	}
	if _, ok := registry.Descriptor("missing"); ok {
		t.Fatal("unknown mode has a descriptor")
	}
}

func TestRegistryRejectsInvalidDescriptors(t *testing.T) {
	cases := map[string]collab.ModeDescriptor{
		"empty id": {
			Roles: []collab.RoleSpec{{Name: "main"}}, Topology: collab.TopologyTree, WorkUnit: collab.WorkUnitDelegation,
		},
		"bad id characters": func() collab.ModeDescriptor {
			descriptor := testMode(collab.Limits{})
			descriptor.ID = "Not Valid"
			return descriptor
		}(),
		"bad topology": func() collab.ModeDescriptor {
			descriptor := testMode(collab.Limits{})
			descriptor.Topology = "galaxy"
			return descriptor
		}(),
		"bad work unit": func() collab.ModeDescriptor {
			descriptor := testMode(collab.Limits{})
			descriptor.WorkUnit = "errant"
			return descriptor
		}(),
		"no roles": func() collab.ModeDescriptor {
			descriptor := testMode(collab.Limits{})
			descriptor.Roles = nil
			return descriptor
		}(),
		"duplicate role": func() collab.ModeDescriptor {
			descriptor := testMode(collab.Limits{})
			descriptor.Roles = []collab.RoleSpec{{Name: "main"}, {Name: "main"}}
			return descriptor
		}(),
		"bad capability": func() collab.ModeDescriptor {
			descriptor := testMode(collab.Limits{})
			descriptor.Capabilities = []collab.Capability{"telepathy"}
			return descriptor
		}(),
		"negative limit": func() collab.ModeDescriptor {
			descriptor := testMode(collab.Limits{MaxDepth: -1})
			return descriptor
		}(),
	}
	for name, descriptor := range cases {
		t.Run(name, func(t *testing.T) {
			registry := newRegistry(t, collab.Limits{})
			if err := registry.Register(descriptor); !errors.Is(err, collab.ErrInvalidMode) {
				t.Fatalf("Register = %v, want ErrInvalidMode", err)
			}
		})
	}
}

func TestRegistryDisableKeepsDescriptor(t *testing.T) {
	registry := newRegistry(t, collab.Limits{})
	if err := registry.Register(testMode(collab.Limits{MaxDepth: 3})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.SetEnabled("test.mode", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if _, err := registry.RequireEnabled("test.mode"); !errors.Is(err, collab.ErrModeDisabled) {
		t.Fatalf("RequireEnabled = %v, want ErrModeDisabled", err)
	}
	descriptor, ok := registry.Descriptor("test.mode")
	if !ok || descriptor.ID != "test.mode" {
		t.Fatalf("disabled descriptor = %+v, ok=%v", descriptor, ok)
	}
	view, ok := registry.Get("test.mode")
	if !ok || view.Enabled {
		t.Fatalf("disabled view = %+v, ok=%v", view, ok)
	}
	if err := registry.SetEnabled("test.mode", true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if _, err := registry.RequireEnabled("test.mode"); err != nil {
		t.Fatalf("RequireEnabled after re-enable: %v", err)
	}
}

func TestRegistryProjectionIsACopy(t *testing.T) {
	registry := newRegistry(t, collab.Limits{})
	if err := registry.Register(testMode(collab.Limits{MaxDepth: 3})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	view, _ := registry.Get("test.mode")
	view.Roles[0].Name = "mutated"
	view.Capabilities[0] = "mutated"
	view.Inputs = append(view.Inputs, "mutated")
	again, _ := registry.Get("test.mode")
	if again.Roles[0].Name != "main" || again.Capabilities[0] != collab.CapabilityAttention || len(again.Inputs) != 1 {
		t.Fatalf("registry projection aliases internal slices: %+v", again)
	}
}

func TestRegistryNilCeilingIsZero(t *testing.T) {
	var registry *collab.ModeRegistry
	if err := registry.Register(testMode(collab.Limits{})); !errors.Is(err, collab.ErrInvalidMode) {
		t.Fatalf("nil registry Register = %v", err)
	}
	if registry.List() != nil {
		t.Fatal("nil registry List should be nil")
	}
}
