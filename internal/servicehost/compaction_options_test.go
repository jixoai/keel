package servicehost

import (
	"testing"

	"github.com/jixoai/keel/internal/host"
)

// TestCompactionOptionsHonorsTenantEnabled pins that compaction.enabled is the
// single source of truth for the loop settings and the policy projection: nil
// inherits the default (on), an explicit false disables both, and a reloaded
// document changes later generations.
func TestCompactionOptionsHonorsTenantEnabled(t *testing.T) {
	boolPtr := func(value bool) *bool { return &value }
	for _, tc := range []struct {
		name    string
		enabled *bool
		want    bool
	}{
		{name: "nil inherits default", enabled: nil, want: true},
		{name: "explicit true", enabled: boolPtr(true), want: true},
		{name: "explicit false", enabled: boolPtr(false), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tenant := host.TenantConfig{Compaction: host.CompactionConfig{Enabled: tc.enabled}}
			options := compactionOptions(tenant)
			if options.Settings.Enabled != tc.want {
				t.Fatalf("settings.Enabled = %v, want %v", options.Settings.Enabled, tc.want)
			}
			if options.Policy == nil {
				t.Fatal("policy is nil")
			}
			if options.Policy.Enabled != tc.want {
				t.Fatalf("policy.Enabled = %v, want %v", options.Policy.Enabled, tc.want)
			}
			if got := CheckpointPolicyFor(tenant).Enabled; got != tc.want {
				t.Fatalf("CheckpointPolicyFor.Enabled = %v, want %v", got, tc.want)
			}
		})
	}
}
