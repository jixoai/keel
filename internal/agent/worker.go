package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/jixoai/keel/internal/store"
)

// Worker polls the runs table and executes claimed sessions with a bounded
// concurrency. The bound is what keeps 1000 online sessions from turning into
// 1000 simultaneous model streams.
type Worker struct {
	Store       *store.Store
	Runner      *Runner
	Concurrency int
	Lease       time.Duration
	Poll        time.Duration
	MaxAttempts int
	Logger      *slog.Logger
}

// NewWorker builds a worker with defaults sized for a 2GB host.
func NewWorker(st *store.Store, runner *Runner, concurrency int, logger *slog.Logger) *Worker {
	if concurrency <= 0 {
		concurrency = 64
	}
	if logger == nil {
		logger = slog.Default()
	}
	lease := 30 * time.Second
	if ms, _ := strconv.Atoi(os.Getenv("KEEL_LEASE_MS")); ms > 0 {
		lease = time.Duration(ms) * time.Millisecond
	}
	return &Worker{
		Store:       st,
		Runner:      runner,
		Concurrency: concurrency,
		Lease:       lease,
		Poll:        200 * time.Millisecond,
		MaxAttempts: 3,
		Logger:      logger,
	}
}

// Run blocks until ctx is cancelled. Each claim runs in its own goroutine; the
// semaphore is the only queue (the database is the durable queue).
func (w *Worker) Run(ctx context.Context) {
	sem := make(chan struct{}, w.Concurrency)
	ticker := time.NewTicker(w.Poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		select {
		case sem <- struct{}{}:
		default:
			continue // all slots busy; do not claim more work
		}
		run, err := w.Store.ClaimRun(ctx, w.Lease)
		if err != nil {
			<-sem
			if !errors.Is(err, store.ErrNoRun) && ctx.Err() == nil {
				w.Logger.Warn("claim run", "err", err)
			}
			continue
		}
		go w.execute(ctx, sem, run)
	}
}

func (w *Worker) execute(ctx context.Context, sem chan struct{}, run store.Run) {
	defer func() { <-sem }()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Keep the lease alive while the model/tools run. A crash stops the renewal,
	// so the lease expires and another worker takes over.
	go func() {
		t := time.NewTicker(w.Lease / 3)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				if err := w.Store.RenewLease(runCtx, run.TenantID, run.SessionID, w.Lease); err != nil && runCtx.Err() == nil {
					w.Logger.Warn("renew lease", "err", err)
				}
			}
		}
	}()

	started := time.Now()
	cursor, err := w.Runner.Run(runCtx, run)
	if ctx.Err() != nil {
		// Process is shutting down: leave the lease to expire so the next
		// process resumes instead of recording a failure.
		return
	}
	if err != nil {
		w.Logger.Warn("run failed", "tenant", run.TenantID, "session", run.SessionID, "err", err)
	}
	if ferr := w.Store.FinishRun(ctx, run.TenantID, run.SessionID, cursor, err, w.MaxAttempts); ferr != nil {
		w.Logger.Error("finish run", "err", ferr)
		return
	}
	w.Logger.Info("run finished", "tenant", run.TenantID, "session", run.SessionID,
		"cursor", cursor, "attempt", run.Attempts, "elapsed", time.Since(started).String(), "err", err)
}
