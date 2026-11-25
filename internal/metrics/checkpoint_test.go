package metrics

import (
	"context"
	"testing"
)

// TestCheckpointMetrics pins the checkpoint families and their fixed
// cardinality: the failed counter accepts bounded reasons only (an unknown value
// folds into "other"), and no identifier becomes a label.
func TestCheckpointMetrics(t *testing.T) {
	reg := NewRegistry()
	set := NewSet(reg)
	ctx := context.Background()

	set.CheckpointCreated(9000, 1500)
	set.CheckpointCreated(9000, 1500)
	set.CheckpointRecovered()
	set.CheckpointFailed("save")
	set.CheckpointFailed("save")
	set.CheckpointFailed("invented-reason")
	set.CheckpointPruned(3)

	snapshot := reg.Snapshot(ctx)
	family := func(name string) FamilySnapshot {
		t.Helper()
		for _, candidate := range snapshot {
			if candidate.Name == name {
				return candidate
			}
		}
		t.Fatalf("family %q missing", name)
		return FamilySnapshot{}
	}
	total := func(name string) float64 {
		t.Helper()
		sum := 0.0
		for _, sample := range family(name).Samples {
			sum += sample.Value
		}
		return sum
	}
	if got := total("keel_checkpoint_created_total"); got != 2 {
		t.Fatalf("created = %v, want 2", got)
	}
	if got := total("keel_checkpoint_recovered_total"); got != 1 {
		t.Fatalf("recovered = %v, want 1", got)
	}
	if got := total("keel_checkpoint_failed_total"); got != 3 {
		t.Fatalf("failed = %v, want 3", got)
	}
	if got := total("keel_checkpoint_pruned_total"); got != 3 {
		t.Fatalf("pruned = %v, want 3", got)
	}
	// The unknown reason folded into "other": the series set is bounded by the
	// vocabulary, not by caller input.
	reasons := map[string]bool{}
	for _, sample := range family("keel_checkpoint_failed_total").Samples {
		reasons[sample.Labels["reason"]] = true
	}
	if len(reasons) != 2 || !reasons["save"] || !reasons["other"] {
		t.Fatalf("failed reasons = %v, want save and other", reasons)
	}
}
