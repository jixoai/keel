package main

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
)

func TestNewCollabModesRegistersBuiltins(t *testing.T) {
	tenant := uuid.New()
	registry, err := newCollabModes(tenant, 3)
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
	second, err := newCollabModes(tenant, 3)
	if err != nil {
		t.Fatalf("second newCollabModes: %v", err)
	}
	if len(second.List()) != len(views) {
		t.Fatalf("second catalogue = %d modes", len(second.List()))
	}
}

func TestNewCollabModesMapsDepthSettings(t *testing.T) {
	tenant := uuid.New()
	cases := []struct {
		setting int
		want    int
	}{
		{setting: 3, want: 3},
		{setting: 1, want: 1},
		{setting: 0, want: 0},
		{setting: -1, want: 0}, // unlimited: the mode layer owns it, the core sees no cap
	}
	for _, tc := range cases {
		registry, err := newCollabModes(tenant, tc.setting)
		if err != nil {
			t.Fatalf("setting %d: %v", tc.setting, err)
		}
		view, ok := registry.Get(collab.ModeSubagent)
		if !ok {
			t.Fatalf("setting %d: subagent mode missing", tc.setting)
		}
		if view.Limits.MaxDepth != tc.want {
			t.Fatalf("setting %d: descriptor maxDepth = %d, want %d", tc.setting, view.Limits.MaxDepth, tc.want)
		}
	}
}

func TestParseSubagentMaxDepth(t *testing.T) {
	if value, err := parseSubagentMaxDepth(""); err != nil || value != nil {
		t.Fatalf("empty = %v, %v; want nil default", value, err)
	}
	for _, raw := range []string{"-1", "0", "3"} {
		value, err := parseSubagentMaxDepth(raw)
		if err != nil || value == nil {
			t.Fatalf("%q = %v, %v", raw, value, err)
		}
	}
	for _, raw := range []string{"-2", "abc", "1.5"} {
		if _, err := parseSubagentMaxDepth(raw); err == nil {
			t.Fatalf("%q should be rejected", raw)
		}
	}
}
