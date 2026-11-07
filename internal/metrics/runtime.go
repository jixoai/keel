// Runtime metric set: the fixed, always-on instruments the Keel runtime
// exports. Every label value is bounded by configuration or by a small fixed
// vocabulary; session, run, tenant, and request IDs never appear in labels.

package metrics

import (
	"context"
	"runtime"
	"time"
)

// DefaultDurationBuckets covers short tool-only runs through long multi-turn
// runs. Unit: seconds.
var DefaultDurationBuckets = []float64{
	0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300,
}

// Set is the runtime's fixed metric set. It is safe for concurrent use.
//
// Terminology: one claimed run may be executed several times (attempts) when
// it is retried after a failure or when new input arrived. Attempt metrics count
// every execution; finished metrics count only the terminal state (done/failed)
// that FinishRun committed to the database.
type Set struct {
	runsStarted    *Counter
	runsFinished   *CounterVec
	runsFailed     *Counter
	runAttempts    *CounterVec
	runAttemptTime *Histogram
	runsActive     *Gauge
	leaseExpired   *Counter
	finishErrors   *Counter
	compactions    *Counter
	draining       *Gauge
}

// NewSet registers the runtime metric families on reg.
func NewSet(reg *Registry) *Set {
	return &Set{
		runsStarted: reg.NewCounter("keel_runs_started_total",
			"Agent run attempts claimed by this process."),
		runsFinished: reg.NewCounterVec("keel_runs_finished_total",
			"Agent runs whose terminal state was committed (status=done|failed).", "status"),
		runsFailed: reg.NewCounter("keel_runs_failed_total",
			"Agent runs whose terminal state is failed."),
		runAttempts: reg.NewCounterVec("keel_run_attempts_total",
			"Agent run attempts settled by this process, by outcome (done|failed|pending|finish_error|abandoned|other).", "outcome"),
		runAttemptTime: reg.NewHistogram("keel_run_attempt_duration_seconds",
			"Wall-clock duration of one agent run attempt.", DefaultDurationBuckets),
		runsActive: reg.NewGauge("keel_runs_active",
			"Agent run attempts currently executing in this process."),
		leaseExpired: reg.NewCounter("keel_leases_expired_total",
			"Runs reclaimed after their lease expired."),
		finishErrors: reg.NewCounter("keel_run_finish_errors_total",
			"Run attempts whose terminal state could not be persisted."),
		compactions: reg.NewCounter("keel_compactions_total",
			"Compaction events committed to the event log."),
		draining: reg.NewGauge("keel_draining",
			"1 while the process is draining, else 0."),
	}
}

// RunStarted counts a claimed attempt and marks it active.
func (s *Set) RunStarted() {
	s.runsStarted.Inc()
	s.runsActive.Inc()
}

// RunAttempt records one settled attempt and its duration. outcome is the state
// committed by FinishRun (done/failed/pending) or a local outcome
// (finish_error/abandoned). It always releases the active-attempt gauge.
func (s *Set) RunAttempt(outcome string, duration time.Duration) {
	switch outcome {
	case "done", "failed", "pending", "finish_error", "abandoned":
	default:
		outcome = "other"
	}
	s.runAttempts.With(outcome).Inc()
	s.runAttemptTime.Observe(duration.Seconds())
	s.runsActive.Dec()
}

// RunFinished records a committed terminal state. status must be done or
// failed; pending retries are attempts, not finished runs.
func (s *Set) RunFinished(status string) {
	switch status {
	case "done":
		s.runsFinished.With("done").Inc()
	case "failed":
		s.runsFinished.With("failed").Inc()
		s.runsFailed.Inc()
	}
}

// FinishFailed records an attempt whose terminal state could not be persisted.
func (s *Set) FinishFailed() { s.finishErrors.Inc() }

// LeaseExpired counts a run reclaimed from an expired lease.
func (s *Set) LeaseExpired() { s.leaseExpired.Inc() }

// EventCommitted is usable as store.EventObserver: it counts committed
// compaction events.
func (s *Set) EventCommitted(kind string) {
	if kind == "compaction" {
		s.compactions.Inc()
	}
}

// SetDraining toggles the drain gauge.
func (s *Set) SetDraining(v bool) {
	if v {
		s.draining.Set(1)
		return
	}
	s.draining.Set(0)
}

// BindGoRuntime registers the process/runtime gauges every server exports.
func BindGoRuntime(reg *Registry) {
	reg.NewGaugeFunc("go_goroutines", "Number of goroutines that currently exist.", func(context.Context) (float64, error) {
		return float64(runtime.NumGoroutine()), nil
	})
	reg.NewGaugeFunc("go_memstats_heap_alloc_bytes", "Bytes of allocated heap objects.", func(context.Context) (float64, error) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return float64(m.HeapAlloc), nil
	})
	reg.NewGaugeFunc("go_memstats_heap_sys_bytes", "Bytes of heap memory obtained from the OS.", func(context.Context) (float64, error) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return float64(m.HeapSys), nil
	})
}

// PendingRuns is the queue-depth source. store.Store implements it.
type PendingRuns interface {
	CountPendingRuns(ctx context.Context) (int64, error)
}

// BindQueueDepth registers the durable run queue depth gauge.
func BindQueueDepth(reg *Registry, src PendingRuns) {
	reg.NewGaugeFunc("keel_runs_pending", "Durable runs waiting to be claimed.", func(ctx context.Context) (float64, error) {
		ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		n, err := src.CountPendingRuns(ctx)
		return float64(n), err
	})
}

// BindStreams registers the active-stream gauge. active is expected to be an
// atomic read from the stream layer, so the scrape never blocks.
func BindStreams(reg *Registry, active func() int64) {
	reg.NewGaugeFunc("keel_streams_active", "SSE and file streams currently open.", func(context.Context) (float64, error) {
		return float64(active()), nil
	})
}

// PoolStats is the optional pgxpool surface. *pgxpool.Stat satisfies it; the
// metrics package stays free of a pgx dependency.
type PoolStats interface {
	AcquiredConns() int32
	IdleConns() int32
	TotalConns() int32
	MaxConns() int32
	ConstructingConns() int32
	AcquireCount() int64
	EmptyAcquireCount() int64
	CanceledAcquireCount() int64
	NewConnsCount() int64
	MaxLifetimeDestroyCount() int64
	MaxIdleDestroyCount() int64
}

// BindPoolStats registers no-op-safe pgxpool gauges and counters. stat is
// evaluated at scrape time; a nil pool disables nothing, it simply reports the
// zero values.
func BindPoolStats(reg *Registry, stat func() PoolStats) {
	gauge := func(name, help string, read func(PoolStats) float64) {
		reg.NewGaugeFunc(name, help, func(context.Context) (float64, error) {
			return read(stat()), nil
		})
	}
	counter := func(name, help string, read func(PoolStats) float64) {
		reg.NewCounterFunc(name, help, func(context.Context) (float64, error) {
			return read(stat()), nil
		})
	}
	gauge("pgxpool_acquired_conns", "Connections currently acquired from the pool.", func(s PoolStats) float64 { return float64(s.AcquiredConns()) })
	gauge("pgxpool_idle_conns", "Idle connections in the pool.", func(s PoolStats) float64 { return float64(s.IdleConns()) })
	gauge("pgxpool_total_conns", "Total connections in the pool.", func(s PoolStats) float64 { return float64(s.TotalConns()) })
	gauge("pgxpool_max_conns", "Maximum connections the pool may open.", func(s PoolStats) float64 { return float64(s.MaxConns()) })
	gauge("pgxpool_constructing_conns", "Connections currently being constructed.", func(s PoolStats) float64 { return float64(s.ConstructingConns()) })
	counter("pgxpool_acquire_count_total", "Successful connection acquisitions.", func(s PoolStats) float64 { return float64(s.AcquireCount()) })
	counter("pgxpool_empty_acquire_total", "Acquisitions that found the pool empty.", func(s PoolStats) float64 { return float64(s.EmptyAcquireCount()) })
	counter("pgxpool_canceled_acquire_total", "Acquisitions canceled by the caller.", func(s PoolStats) float64 { return float64(s.CanceledAcquireCount()) })
	counter("pgxpool_new_conns_total", "New connections opened by the pool.", func(s PoolStats) float64 { return float64(s.NewConnsCount()) })
	counter("pgxpool_max_lifetime_destroy_total", "Connections destroyed after max lifetime.", func(s PoolStats) float64 { return float64(s.MaxLifetimeDestroyCount()) })
	counter("pgxpool_max_idle_destroy_total", "Connections destroyed after max idle time.", func(s PoolStats) float64 { return float64(s.MaxIdleDestroyCount()) })
}
