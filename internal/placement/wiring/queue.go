package wiring

import (
	"context"
	"log/slog"
	"time"
)

// RunQueueSweeper is the crash-recovery loop of the placement queue. A queued
// run is normally promoted by the settlement/release path that freed its slot;
// when the process died between a settle and that promotion (or inherited the
// queue from a previous process), no in-process event will ever wake it. The
// sweeper performs one promotion pass per tenant with queued rows, immediately
// and then on every interval, until ctx is cancelled.
//
// It is deliberately separate from the admission gate: a runtime must be able to
// converge a queue it did not create, and the sweep is capacity-guarded and
// idempotent (promotion is a guarded queued -> pending write).
func RunQueueSweeper(ctx context.Context, rt *Runtime, log *slog.Logger) {
	if rt == nil || rt.Gate == nil {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	interval := rt.QueueInterval
	if interval <= 0 {
		interval = DefaultQueueInterval
	}
	sweep := func() {
		promoted, err := rt.Gate.SweepQueues(ctx)
		if err != nil {
			log.Warn("placement queue recovery sweep", "err", err)
		}
		if len(promoted) > 0 {
			log.Info("placement queue recovery promoted runs", "promoted", len(promoted))
		}
	}
	sweep()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}
