package metrics

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// parseExposition is a deliberately small Prometheus text parser: it verifies
// that every sample line is "<name>[{labels}] <value>" and returns the number
// of sample lines.
func parseExposition(t *testing.T, text string) int {
	t.Helper()
	count := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.LastIndexByte(line, ' ')
		if idx <= 0 {
			t.Fatalf("malformed sample line %q", line)
		}
		value := line[idx+1:]
		switch value {
		case "NaN", "+Inf", "-Inf":
		default:
			if _, err := strconv.ParseFloat(value, 64); err != nil {
				t.Fatalf("bad value in %q: %v", line, err)
			}
		}
		count++
	}
	return count
}

func TestTextExpositionFormat(t *testing.T) {
	reg := NewRegistry()
	reg.NewCounter("keel_test_total", "A test counter.").Add(3)
	vec := reg.NewCounterVec("keel_test_labeled_total", "A labeled counter.", "status")
	vec.With("ok").Add(2)
	vec.With("failed").Add(1)
	reg.NewGauge("keel_test_gauge", "A gauge.").Set(7.5)
	hist := reg.NewHistogram("keel_test_seconds", "A histogram.", []float64{0.1, 1})
	hist.Observe(0.05) // bucket 0.1
	hist.Observe(0.5)  // bucket 1
	hist.Observe(2)    // +Inf
	reg.NewGaugeFunc("keel_test_pending", "A scraped gauge.", func(context.Context) (float64, error) {
		return 42, nil
	})

	var buf bytes.Buffer
	if err := reg.WriteText(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	text := buf.String()
	for _, want := range []string{
		"# HELP keel_test_total A test counter.",
		"# TYPE keel_test_total counter",
		"keel_test_total 3",
		"keel_test_labeled_total{status=\"ok\"} 2",
		"keel_test_labeled_total{status=\"failed\"} 1",
		"keel_test_gauge 7.5",
		"keel_test_seconds_bucket{le=\"0.1\"} 1",
		"keel_test_seconds_bucket{le=\"1\"} 2",
		"keel_test_seconds_bucket{le=\"+Inf\"} 3",
		"keel_test_seconds_sum 2.55",
		"keel_test_seconds_count 3",
		"keel_test_pending 42",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("exposition missing %q:\n%s", want, text)
		}
	}
	if got := parseExposition(t, text); got != 10 {
		t.Fatalf("parsed %d sample lines, want 10:\n%s", got, text)
	}
}

func TestHistogramSnapshotIsCumulative(t *testing.T) {
	reg := NewRegistry()
	hist := reg.NewHistogram("keel_test_seconds", "A histogram.", []float64{1, 5})
	hist.Observe(0.5)
	hist.Observe(3)
	hist.Observe(10)
	snaps := reg.Snapshot(context.Background())
	if len(snaps) != 1 {
		t.Fatalf("snapshots = %d", len(snaps))
	}
	got := snaps[0].Samples[0]
	want := []uint64{1, 2, 3}
	for i := range want {
		if got.BucketCounts[i] != want[i] {
			t.Fatalf("bucket %d = %d, want %d", i, got.BucketCounts[i], want[i])
		}
	}
	if got.Count != 3 || got.Sum != 13.5 {
		t.Fatalf("count/sum = %d/%v", got.Count, got.Sum)
	}
}

func TestSeriesCountFixedAcrossSessions(t *testing.T) {
	reg := NewRegistry()
	set := NewSet(reg)
	BindGoRuntime(reg)
	BindQueueDepth(reg, fixedPending(0))
	BindStreams(reg, func() int64 { return 1 })
	BindPoolStats(reg, func() PoolStats { return zeroPool{} })

	countSeries := func() int {
		var buf bytes.Buffer
		if err := reg.WriteText(context.Background(), &buf); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, line := range strings.Split(buf.String(), "\n") {
			if line != "" && !strings.HasPrefix(line, "#") {
				n++
			}
		}
		return n
	}
	// Warm up every labeled series once: vectors are created lazily, so the
	// first run establishes the fixed set before we compare counts.
	set.RunStarted()
	set.RunAttempt("done", 250*time.Millisecond)
	set.RunFinished("done")
	set.RunStarted()
	set.RunAttempt("failed", 250*time.Millisecond)
	set.RunFinished("failed")
	set.RunStarted()
	set.RunAttempt("pending", 250*time.Millisecond)
	set.RunStarted()
	set.RunAttempt("finish_error", 250*time.Millisecond)
	set.FinishFailed()
	set.RunStarted()
	set.RunAttempt("abandoned", 250*time.Millisecond)
	set.LeaseExpired()
	set.EventCommitted("compaction")

	before := countSeries()
	// 1000 "sessions" produce run/tool activity. No identifier is ever passed
	// to the metric set, so the series count cannot grow.
	for i := 0; i < 1000; i++ {
		set.RunStarted()
		set.RunAttempt("done", 250*time.Millisecond)
		set.RunFinished("done")
		set.RunStarted()
		set.RunAttempt("failed", 250*time.Millisecond)
		set.RunFinished("failed")
		set.RunStarted()
		set.RunAttempt("pending", 250*time.Millisecond)
		set.LeaseExpired()
		set.EventCommitted("compaction")
	}
	after := countSeries()
	if before != after {
		t.Fatalf("series count grew from %d to %d", before, after)
	}
}

func TestLabelWhitelist(t *testing.T) {
	if err := ValidateLabelNames([]string{"status", "tool"}); err != nil {
		t.Fatalf("allowed labels rejected: %v", err)
	}
	for _, name := range []string{"session_id", "run_id", "tenant_id", "request_id", "stream_id", "email"} {
		if err := ValidateLabelNames([]string{name}); err == nil {
			t.Errorf("label %q accepted", name)
		}
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewCounterVec(%q) did not panic", name)
				}
			}()
			NewRegistry().NewCounterVec("keel_bad_total", "bad", name)
		}()
	}
	reg := NewRegistry()
	reg.NewCounter("keel_dup_total", "one")
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate family did not panic")
		}
	}()
	reg.NewCounter("keel_dup_total", "two")
}

func TestGaugeFuncErrorIsSkipped(t *testing.T) {
	reg := NewRegistry()
	var reported string
	reg.SetErrorHandler(func(name string, err error) { reported = name + ": " + err.Error() })
	reg.NewGaugeFunc("keel_broken", "Fails.", func(context.Context) (float64, error) {
		return 0, errors.New("database unavailable")
	})
	var buf bytes.Buffer
	if err := reg.WriteText(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "# TYPE keel_broken gauge") {
		t.Fatalf("family header missing:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "keel_broken 0") {
		t.Fatalf("failed gauge should not emit a sample:\n%s", buf.String())
	}
	if reported == "" {
		t.Fatal("error handler not called")
	}
}

func TestCounterNeverDecreases(t *testing.T) {
	reg := NewRegistry()
	c := reg.NewCounter("keel_test_total", "Counter.")
	c.Add(5)
	c.Add(-2)
	if c.Value() != 5 {
		t.Fatalf("value = %v, want 5", c.Value())
	}
}

func fixedPending(n int64) fixedPendingRuns { return fixedPendingRuns(n) }

type fixedPendingRuns int64

func (f fixedPendingRuns) CountPendingRuns(context.Context) (int64, error) { return int64(f), nil }

type zeroPool struct{}

func (zeroPool) AcquiredConns() int32           { return 0 }
func (zeroPool) IdleConns() int32               { return 0 }
func (zeroPool) TotalConns() int32              { return 0 }
func (zeroPool) MaxConns() int32                { return 24 }
func (zeroPool) ConstructingConns() int32       { return 0 }
func (zeroPool) AcquireCount() int64            { return 0 }
func (zeroPool) EmptyAcquireCount() int64       { return 0 }
func (zeroPool) CanceledAcquireCount() int64    { return 0 }
func (zeroPool) NewConnsCount() int64           { return 0 }
func (zeroPool) MaxLifetimeDestroyCount() int64 { return 0 }
func (zeroPool) MaxIdleDestroyCount() int64     { return 0 }

func labelsMatch(have, want map[string]string) bool {
	for key, value := range want {
		if have[key] != value {
			return false
		}
	}
	return true
}

func familyValue(snaps []FamilySnapshot, name string, labels map[string]string) (float64, bool) {
	for _, family := range snaps {
		if family.Name != name {
			continue
		}
		for _, sample := range family.Samples {
			if labelsMatch(sample.Labels, labels) {
				return sample.Value, true
			}
		}
	}
	return 0, false
}

func TestTerminalVersusAttemptSemantics(t *testing.T) {
	reg := NewRegistry()
	set := NewSet(reg)
	ctx := context.Background()

	// A failed attempt that stays pending is not a finished run.
	set.RunStarted()
	set.RunAttempt("pending", time.Second)
	snaps := reg.Snapshot(ctx)
	if _, ok := familyValue(snaps, "keel_runs_finished_total", nil); ok {
		t.Fatal("pending attempt counted as finished")
	}
	if v, ok := familyValue(snaps, "keel_run_attempts_total", map[string]string{"outcome": "pending"}); !ok || v != 1 {
		t.Fatalf("pending attempts = %v (ok=%v)", v, ok)
	}
	if v, _ := familyValue(snaps, "keel_runs_active", nil); v != 0 {
		t.Fatalf("active after settled attempt = %v", v)
	}

	// Retry succeeds: one more attempt, one terminal done, no failed terminal.
	set.RunStarted()
	set.RunAttempt("done", time.Second)
	set.RunFinished("done")
	snaps = reg.Snapshot(ctx)
	if v, ok := familyValue(snaps, "keel_runs_finished_total", map[string]string{"status": "done"}); !ok || v != 1 {
		t.Fatalf("finished done = %v (ok=%v)", v, ok)
	}
	if _, ok := familyValue(snaps, "keel_runs_finished_total", map[string]string{"status": "failed"}); ok {
		t.Fatal("retry success produced a finished failed series")
	}
	if v, _ := familyValue(snaps, "keel_runs_failed_total", nil); v != 0 {
		t.Fatalf("retry success produced failed terminal counter %v", v)
	}

	// Exhausted retries: terminal failed.
	set.RunStarted()
	set.RunAttempt("failed", time.Second)
	set.RunFinished("failed")
	snaps = reg.Snapshot(ctx)
	if v, _ := familyValue(snaps, "keel_runs_finished_total", map[string]string{"status": "failed"}); v != 1 {
		t.Fatalf("finished failed = %v", v)
	}
	if v, _ := familyValue(snaps, "keel_runs_failed_total", nil); v != 1 {
		t.Fatalf("runs failed = %v", v)
	}

	// FinishRun failure: attempt/finish-error counters only, no terminal count.
	set.RunStarted()
	set.RunAttempt("finish_error", time.Second)
	set.FinishFailed()
	snaps = reg.Snapshot(ctx)
	if v, ok := familyValue(snaps, "keel_run_finish_errors_total", nil); !ok || v != 1 {
		t.Fatalf("finish errors = %v (ok=%v)", v, ok)
	}
	if v, _ := familyValue(snaps, "keel_runs_finished_total", map[string]string{"status": "done"}); v != 1 {
		t.Fatalf("finish failure changed terminal done count: %v", v)
	}

	// Defensive: RunFinished ignores non-terminal states.
	set.RunFinished("pending")
	snaps = reg.Snapshot(ctx)
	if v, _ := familyValue(snaps, "keel_runs_finished_total", map[string]string{"status": "pending"}); v != 0 {
		t.Fatalf("RunFinished(pending) counted: %v", v)
	}
}
