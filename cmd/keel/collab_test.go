package main

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
)

func TestNewCollabModesRegistersBuiltins(t *testing.T) {
	tenant := uuid.New()
	registry, err := newCollabModes(tenant, 3, 8)
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
	second, err := newCollabModes(tenant, 3, 8)
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
		want    collab.DepthPolicy
	}{
		{setting: 3, want: collab.LimitedDepth(3)},
		{setting: 1, want: collab.LimitedDepth(1)},
		{setting: 0, want: collab.ForbiddenDepth()},
		{setting: -1, want: collab.UnlimitedDepth()},
	}
	for _, tc := range cases {
		registry, err := newCollabModes(tenant, tc.setting, 8)
		if err != nil {
			t.Fatalf("setting %d: %v", tc.setting, err)
		}
		view, ok := registry.Get(collab.ModeSubagent)
		if !ok {
			t.Fatalf("setting %d: subagent mode missing", tc.setting)
		}
		if view.Limits.Depth != tc.want {
			t.Fatalf("setting %d: descriptor depth = %+v, want %+v", tc.setting, view.Limits.Depth, tc.want)
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

func TestNewCollabModesPassesMaxChildren(t *testing.T) {
	tenant := uuid.New()
	registry, err := newCollabModes(tenant, 3, 5)
	if err != nil {
		t.Fatalf("newCollabModes: %v", err)
	}
	view, ok := registry.Get(collab.ModeSubagent)
	if !ok {
		t.Fatal("subagent mode missing")
	}
	if view.Limits.MaxChildren != 5 {
		t.Fatalf("descriptor maxChildren = %d, want the configured 5", view.Limits.MaxChildren)
	}
}
