package compaction

import (
	"testing"
	"time"
)

func boolPtr(v bool) *bool                       { return &v }
func intPtr(v int) *int                          { return &v }
func durationPtr(v time.Duration) *time.Duration { return &v }

// TestCheckpointPolicyVersionIsStableAndSpecific pins the policy identity: the
// same knobs share a version, and every knob changes it. A silent change to the
// narrowing rules must also bump the scheme prefix, so a checkpoint can never be
// mistaken for one produced under different rules.
func TestCheckpointPolicyVersionIsStableAndSpecific(t *testing.T) {
	base := DefaultCheckpointPolicy()
	version := base.Version()
	if version == "" || version == "cp1-" {
		t.Fatalf("version = %q, want a digest", version)
	}
	if again := base.Version(); again != version {
		t.Fatalf("version changed between calls: %q != %q", again, version)
	}
	mutations := map[string]func(*CheckpointPolicy){
		"enabled":         func(p *CheckpointPolicy) { p.Enabled = false },
		"thresholdTokens": func(p *CheckpointPolicy) { p.ThresholdTokens = 1 },
		"retainTokens":    func(p *CheckpointPolicy) { p.RetainTokens++ },
		"maxCheckpoints":  func(p *CheckpointPolicy) { p.MaxCheckpoints = 2 },
		"ttl":             func(p *CheckpointPolicy) { p.TTL = time.Hour },
		"manualCompact":   func(p *CheckpointPolicy) { p.ManualCompact = false },
	}
	for name, mutate := range mutations {
		p := base
		mutate(&p)
		if p.Version() == version {
			t.Fatalf("%s did not change the policy version", name)
		}
	}
}

// TestCheckpointPolicyNarrowing pins the tenant ceiling: the session may only
// disable capabilities and lower resource bounds; the spec's max-checkpoint
// scenario (tenant 3, session 10 → 3) is the canonical case.
func TestCheckpointPolicyNarrowing(t *testing.T) {
	tenant := CheckpointPolicy{
		Enabled:         true,
		ThresholdTokens: 50000,
		RetainTokens:    20000,
		MaxCheckpoints:  3,
		TTL:             24 * time.Hour,
		ManualCompact:   true,
	}

	t.Run("max checkpoints cannot exceed the tenant", func(t *testing.T) {
		effective, narrowed := NarrowCheckpointPolicy(tenant, CheckpointPolicyOverride{
			MaxCheckpoints: intPtr(10),
		})
		if effective.MaxCheckpoints != 3 {
			t.Fatalf("MaxCheckpoints = %d, want 3", effective.MaxCheckpoints)
		}
		if len(narrowed) != 1 || narrowed[0].Field != "maxCheckpoints" {
			t.Fatalf("narrowed = %+v, want one maxCheckpoints note", narrowed)
		}
	})

	t.Run("session may keep fewer and expire sooner", func(t *testing.T) {
		effective, narrowed := NarrowCheckpointPolicy(tenant, CheckpointPolicyOverride{
			MaxCheckpoints: intPtr(1),
			TTL:            durationPtr(time.Hour),
			RetainTokens:   intPtr(5000),
		})
		if effective.MaxCheckpoints != 1 || effective.TTL != time.Hour || effective.RetainTokens != 5000 {
			t.Fatalf("effective = %+v, want the session's narrower values", effective)
		}
		if len(narrowed) != 0 {
			t.Fatalf("narrowed = %+v, want none", narrowed)
		}
	})

	t.Run("session cannot enable a disabled capability", func(t *testing.T) {
		disabled := tenant
		disabled.Enabled = false
		disabled.ManualCompact = false
		effective, narrowed := NarrowCheckpointPolicy(disabled, CheckpointPolicyOverride{
			Enabled:       boolPtr(true),
			ManualCompact: boolPtr(true),
		})
		if effective.Enabled || effective.ManualCompact {
			t.Fatalf("effective = %+v, want both capabilities disabled", effective)
		}
		if len(narrowed) != 2 {
			t.Fatalf("narrowed = %+v, want two notes", narrowed)
		}
	})

	t.Run("session cannot retain or expire beyond the tenant", func(t *testing.T) {
		effective, narrowed := NarrowCheckpointPolicy(tenant, CheckpointPolicyOverride{
			RetainTokens: intPtr(999999),
			TTL:          durationPtr(72 * time.Hour),
		})
		if effective.RetainTokens != 20000 || effective.TTL != 24*time.Hour {
			t.Fatalf("effective = %+v, want tenant ceilings", effective)
		}
		if len(narrowed) != 2 {
			t.Fatalf("narrowed = %+v, want two notes", narrowed)
		}
	})

	t.Run("session cannot compact earlier than the tenant permits", func(t *testing.T) {
		effective, narrowed := NarrowCheckpointPolicy(tenant, CheckpointPolicyOverride{
			ThresholdTokens: intPtr(1000),
		})
		if effective.ThresholdTokens != 50000 {
			t.Fatalf("ThresholdTokens = %d, want 50000", effective.ThresholdTokens)
		}
		if len(narrowed) != 1 || narrowed[0].Field != "thresholdTokens" {
			t.Fatalf("narrowed = %+v, want one thresholdTokens note", narrowed)
		}
	})

	t.Run("session may drop retention entirely", func(t *testing.T) {
		effective, narrowed := NarrowCheckpointPolicy(tenant, CheckpointPolicyOverride{
			RetainTokens: intPtr(0),
		})
		if effective.RetainTokens != 0 {
			t.Fatalf("RetainTokens = %d, want 0", effective.RetainTokens)
		}
		if len(narrowed) != 0 {
			t.Fatalf("narrowed = %+v, want none", narrowed)
		}
	})

	t.Run("session cannot fall back to an earlier window-derived threshold", func(t *testing.T) {
		effective, narrowed := NarrowCheckpointPolicy(tenant, CheckpointPolicyOverride{
			ThresholdTokens: intPtr(0),
		})
		if effective.ThresholdTokens != 50000 {
			t.Fatalf("ThresholdTokens = %d, want the tenant absolute threshold", effective.ThresholdTokens)
		}
		if len(narrowed) != 1 || narrowed[0].Field != "thresholdTokens" {
			t.Fatalf("narrowed = %+v, want one thresholdTokens note", narrowed)
		}
	})

	t.Run("unset tenant bounds accept the session value", func(t *testing.T) {
		unbounded := CheckpointPolicy{Enabled: true, ManualCompact: true}
		effective, narrowed := NarrowCheckpointPolicy(unbounded, CheckpointPolicyOverride{
			MaxCheckpoints:  intPtr(4),
			TTL:             durationPtr(2 * time.Hour),
			RetainTokens:    intPtr(7000),
			ThresholdTokens: intPtr(1000),
		})
		if effective.MaxCheckpoints != 4 || effective.TTL != 2*time.Hour ||
			effective.RetainTokens != 7000 || effective.ThresholdTokens != 1000 {
			t.Fatalf("effective = %+v", effective)
		}
		if len(narrowed) != 0 {
			t.Fatalf("narrowed = %+v, want none", narrowed)
		}
	})
}

// TestCheckpointPolicyTrigger pins the threshold rule: an explicit absolute
// threshold wins; otherwise the context window minus reserve triggers, and any
// non-positive result never triggers.
func TestCheckpointPolicyTrigger(t *testing.T) {
	windowPolicy := CheckpointPolicy{Enabled: true}
	if !windowPolicy.ShouldCompact(9000, 10000, 2000) {
		t.Fatal("9000 > 10000-2000 should trigger")
	}
	if windowPolicy.ShouldCompact(8000, 10000, 2000) {
		t.Fatal("8000 is not above 10000-2000")
	}
	absolute := CheckpointPolicy{Enabled: true, ThresholdTokens: 1000}
	if !absolute.ShouldCompact(1001, 100000, 2000) {
		t.Fatal("explicit threshold should win")
	}
	if absolute.ShouldCompact(999, 100000, 2000) {
		t.Fatal("below explicit threshold must not trigger")
	}
	if (CheckpointPolicy{Enabled: false, ThresholdTokens: 1}).ShouldCompact(1000, 10000, 0) {
		t.Fatal("disabled policy must never trigger")
	}
	if windowPolicy.ShouldCompact(1, 0, 0) {
		t.Fatal("unknown window must never trigger")
	}
}

func TestCheckpointPolicyValidate(t *testing.T) {
	valid := DefaultCheckpointPolicy()
	if err := valid.Validate(); err != nil {
		t.Fatalf("default policy invalid: %v", err)
	}
	for name, policy := range map[string]CheckpointPolicy{
		"negative threshold": {ThresholdTokens: -1},
		"negative retain":    {RetainTokens: -1},
		"negative max":       {MaxCheckpoints: -1},
		"negative ttl":       {TTL: -time.Second},
	} {
		if err := policy.Validate(); err == nil {
			t.Fatalf("%s: Validate = nil, want error", name)
		}
	}
}
